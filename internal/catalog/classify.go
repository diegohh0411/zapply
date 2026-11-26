package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/worker"
)

// Stages.
const (
	StageDecision   = "decision"
	StageExtraction = "extraction"
)

const (
	// maxInFlight bounds the catalog's open operations, under the ledger's
	// eight concurrent calls per member, so a backfill never parks thousands
	// of blocked operations.
	maxInFlight = 6
	// stageAttempts is how often a stage is tried before it is given up.
	stageAttempts = 2
)

// SweepArgs wakes the classifier. There is at most one waiting at a time.
type SweepArgs struct{}

func (SweepArgs) Kind() string { return "posting_classify" }

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	c *Catalog
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	_, err := w.c.Sweep(ctx)
	return err
}

func (c *Catalog) AddWorkers(w *river.Workers) { river.AddWorker(w, &sweepWorker{c: c}) }

// Recover runs every minute with the worker's recovery: a sweep picks up
// whatever a missed wake left waiting.
func (c *Catalog) Recover(ctx context.Context, _ *river.Client[pgx.Tx]) error {
	_, err := c.Sweep(ctx)
	return err
}

type postingRow struct {
	ID            uuid.UUID
	Stage         string
	Company       string
	Title         string
	Location      string
	Remote        bool
	Description   string
	SeniorityHint string
}

// Sweep sends the next postings' stages to the models, newest postings
// first, as long as the catalog has operations to spare. A stage whose model
// the owner has not chosen waits. It returns how many stages it started.
func (c *Catalog) Sweep(ctx context.Context) (int, error) {
	var decisionModel, extractionModel *string
	if err := c.store.Pool().QueryRow(ctx, `SELECT decision,extraction FROM platform.posting_models()`).Scan(&decisionModel, &extractionModel); err != nil {
		return 0, err
	}
	stages := []string{}
	if decisionModel != nil {
		stages = append(stages, StageDecision)
	}
	if extractionModel != nil {
		stages = append(stages, StageExtraction)
	}
	if len(stages) == 0 {
		return 0, nil
	}
	if c.queue == nil {
		return 0, errors.New("catalog queue unavailable")
	}
	started := 0
	err := c.store.InTenant(ctx, SystemMember, func(tx pgx.Tx) error {
		started = 0
		// One sweeper at a time, so two cannot both count free slots.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('posting_classify',0))`); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.posting_stage WHERE operation_id IS NOT NULL AND NOT done`).Scan(&open); err != nil {
			return err
		}
		if open >= maxInFlight {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT s.posting_id,s.stage,p.company,p.title,p.location,p.remote,COALESCE(p.description,''),COALESCE(p.seniority_hint,'')
 FROM platform.posting_stage s JOIN platform.posting p ON p.id=s.posting_id
 WHERE NOT s.done AND s.operation_id IS NULL AND s.stage=ANY($1) ORDER BY p.first_seen_at DESC,s.posting_id,s.stage LIMIT $2 FOR UPDATE OF s SKIP LOCKED`,
			stages, maxInFlight-open)
		if err != nil {
			return err
		}
		var todo []postingRow
		for rows.Next() {
			var p postingRow
			if err := rows.Scan(&p.ID, &p.Stage, &p.Company, &p.Title, &p.Location, &p.Remote, &p.Description, &p.SeniorityHint); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range todo {
			var err error
			if p.Stage == StageDecision {
				err = c.dispatchDecision(ctx, tx, *decisionModel, p)
			} else {
				err = c.dispatchExtraction(ctx, tx, *extractionModel, p)
			}
			if err != nil {
				return fmt.Errorf("posting %s %s: %w", p.ID, p.Stage, err)
			}
			started++
		}
		return nil
	})
	return started, err
}

// dispatchDecision asks the decision questions about one posting in one
// request. Answers already cached for the same model, posting and questions
// are used without a call.
func (c *Catalog) dispatchDecision(ctx context.Context, tx pgx.Tx, model string, p postingRow) error {
	state := postingState(p)
	qs := decisionQuestions()
	pending := make([]decide.Pending, len(qs))
	for i, q := range qs {
		// Subjects sort in question order, so Pack keys them q1, q2, …
		item, err := decide.New(fmt.Sprintf("q%d", i+1), state, q)
		if err != nil {
			return err
		}
		pending[i] = item
	}
	cached, err := decide.LookupAll(ctx, tx, SystemMember, model, pending)
	if err != nil {
		return err
	}
	answers := map[string]systemone.Answer{}
	for i, a := range cached {
		if a != nil {
			answers[pending[i].Subject] = *a
		}
	}
	if len(answers) == len(qs) {
		l, ok := decisionLabels(answers)
		if !ok {
			return giveUpStage(ctx, tx, p.ID, StageDecision)
		}
		return writeDecision(ctx, tx, p.ID, model, l)
	}
	packed, err := decide.Pack(model, pending, 0, len(qs))
	if err != nil || len(packed) != 1 {
		return errors.Join(errors.New("posting questions do not fit one request"), err)
	}
	hash, err := systemone.Hash(packed[0].Request)
	if err != nil {
		return err
	}
	op := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'decision',$3,$4)`,
		op, SystemMember, "posting:"+p.ID.String()+":"+op.String(), hash); err != nil {
		return err
	}
	if err := decide.Store(ctx, tx, SystemMember, op, packed[0]); err != nil {
		return err
	}
	return c.started(ctx, tx, p.ID, StageDecision, op)
}

// dispatchExtraction asks for the extracted labels of one posting.
func (c *Catalog) dispatchExtraction(ctx context.Context, tx pgx.Tx, model string, p postingRow) error {
	r := extractionRequest(model, p)
	if extraction.Validate(r) != nil {
		// A request that can never validate (a posting with no text, a model
		// that answers no lists) would fail the same way every sweep.
		return giveUpStage(ctx, tx, p.ID, StageExtraction)
	}
	op, err := c.queue.EnqueueExtraction(ctx, tx, SystemMember, "posting:"+p.ID.String(), r)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE platform.posting_stage SET operation_id=$3,attempts=attempts+1 WHERE posting_id=$1 AND stage=$2`, p.ID, StageExtraction, op)
	return err
}

// started records a stage's operation and queues it.
func (c *Catalog) started(ctx context.Context, tx pgx.Tx, posting uuid.UUID, stage string, op uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE platform.posting_stage SET operation_id=$3,attempts=attempts+1 WHERE posting_id=$1 AND stage=$2`, posting, stage, op); err != nil {
		return err
	}
	return c.queue.InsertTx(ctx, tx, worker.Args{UserID: SystemMember, OperationID: op})
}

// OnTerminal records a finished classification, inside the transaction
// that finished it. It handles every operation of the catalog's member, so
// no other feature reads them; it reports false for everyone else's.
func (c *Catalog) OnTerminal(ctx context.Context, tx pgx.Tx, op worker.Operation) (bool, error) {
	if op.UserID != SystemMember {
		return false, nil
	}
	var posting uuid.UUID
	var stage string
	var attempts int
	var done bool
	err := tx.QueryRow(ctx, `SELECT posting_id,stage,attempts,done FROM platform.posting_stage WHERE operation_id=$1 FOR UPDATE`, op.ID).Scan(&posting, &stage, &attempts, &done)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && done) {
		return true, nil // a replay, or a stage already retried
	}
	if err != nil {
		return true, err
	}
	ok := false
	if op.Status == "completed" {
		switch stage {
		case StageDecision:
			var model string
			if err := tx.QueryRow(ctx, `SELECT body->>'model' FROM model_request WHERE user_id=$1 AND operation_id=$2`, SystemMember, op.ID).Scan(&model); err != nil {
				return true, err
			}
			answers, err := decide.Ingest(ctx, tx, SystemMember, op.ID, op.Result)
			if err != nil {
				return true, err
			}
			var l Labels
			if l, ok = decisionLabels(answers); ok {
				if err := writeDecision(ctx, tx, posting, model, l); err != nil {
					return true, err
				}
			}
		case StageExtraction:
			var body []byte
			if err := tx.QueryRow(ctx, `SELECT body FROM model_request WHERE user_id=$1 AND operation_id=$2`, SystemMember, op.ID).Scan(&body); err != nil {
				return true, err
			}
			var r extraction.Request
			if err := json.Unmarshal(body, &r); err != nil {
				return true, err
			}
			// An extraction that found nothing still answered: a posting may
			// name no country or language.
			ok = true
			if err := writeExtraction(ctx, tx, posting, r.Model, extractionLabels(r, op.Result)); err != nil {
				return true, err
			}
		}
	}
	if !ok {
		if attempts >= stageAttempts {
			if err := giveUpStage(ctx, tx, posting, stage); err != nil {
				return true, err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE platform.posting_stage SET operation_id=NULL WHERE posting_id=$1 AND stage=$2`, posting, stage); err != nil {
			return true, err
		}
	}
	if c.queue == nil {
		return true, nil
	}
	return true, c.queue.InsertTx(ctx, tx, SweepArgs{})
}

func giveUpStage(ctx context.Context, tx pgx.Tx, posting uuid.UUID, stage string) error {
	_, err := tx.Exec(ctx, `UPDATE platform.posting_stage SET done=true,failed=true WHERE posting_id=$1 AND stage=$2`, posting, stage)
	return err
}

// writeDecision stores a decision's labels and finishes its stage.
func writeDecision(ctx context.Context, tx pgx.Tx, posting uuid.UUID, model string, l Labels) error {
	conf, _ := json.Marshal(l.Confidence)
	if _, err := tx.Exec(ctx, `INSERT INTO platform.posting_label(posting_id,version,seniority,min_years,areas,remote,employment_type,confidence,decision_model,decided_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
 ON CONFLICT(posting_id) DO UPDATE SET version=EXCLUDED.version,seniority=EXCLUDED.seniority,min_years=EXCLUDED.min_years,areas=EXCLUDED.areas,
  remote=EXCLUDED.remote,employment_type=EXCLUDED.employment_type,confidence=posting_label.confidence||EXCLUDED.confidence,
  decision_model=EXCLUDED.decision_model,decided_at=now(),updated_at=now()`,
		posting, LabelVersion, l.Seniority, l.MinYears, l.Areas, l.Remote, l.EmploymentType, json.RawMessage(conf), model); err != nil {
		return err
	}
	return finishStage(ctx, tx, posting, StageDecision)
}

// writeExtraction stores an extraction's labels and finishes its stage.
func writeExtraction(ctx context.Context, tx pgx.Tx, posting uuid.UUID, model string, l Labels) error {
	conf, _ := json.Marshal(l.Confidence)
	countries, languages := l.Countries, l.Languages
	if countries == nil {
		countries = []string{}
	}
	if languages == nil {
		languages = []string{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.posting_label(posting_id,version,specialty,countries,languages,confidence,extraction_model,extracted_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,now())
 ON CONFLICT(posting_id) DO UPDATE SET version=EXCLUDED.version,specialty=EXCLUDED.specialty,countries=EXCLUDED.countries,languages=EXCLUDED.languages,
  confidence=posting_label.confidence||EXCLUDED.confidence,extraction_model=EXCLUDED.extraction_model,extracted_at=now(),updated_at=now()`,
		posting, LabelVersion, l.Specialty, countries, languages, json.RawMessage(conf), model); err != nil {
		return err
	}
	return finishStage(ctx, tx, posting, StageExtraction)
}

func finishStage(ctx context.Context, tx pgx.Tx, posting uuid.UUID, stage string) error {
	_, err := tx.Exec(ctx, `UPDATE platform.posting_stage SET done=true,failed=false WHERE posting_id=$1 AND stage=$2`, posting, stage)
	return err
}
