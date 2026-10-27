package match

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Service) Advance(ctx context.Context, u, id uuid.UUID) error {
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var job uuid.UUID
		e := tx.QueryRow(ctx, `SELECT job_id FROM match_run WHERE user_id=$1 AND id=$2`, u, id).Scan(&job)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT platform.lock_match_job($2::uuid) WHERE $1::uuid=app_user_id()`, u, job); e != nil {
			return e
		}
		r, e := loadRun(ctx, tx, u, id, true)
		if e != nil {
			return e
		}
		if terminal(r.Status) {
			return nil
		}
		if r.Model == nil {
			if e = tx.QueryRow(ctx, `SELECT platform.selected_decision_model()`).Scan(&r.Model); e != nil {
				return e
			}
			if r.Model == nil {
				return s.status(ctx, tx, u, r, "blocked", "model_not_configured", nil)
			}
			if _, e = tx.Exec(ctx, `UPDATE match_run SET model=$3 WHERE user_id=$1 AND id=$2 AND model IS NULL`, u, id, r.Model); e != nil {
				return e
			}
		}
		if e = s.ingest(ctx, tx, u, id); e != nil {
			return e
		}
		for stage := 0; stage < 3; stage++ {
			a, e := answers(ctx, tx, u, id)
			if e != nil {
				return e
			}
			requirements, _ := Requirements(r.Snapshot, a)
			var questions []decide.Pending
			if r.Stage == "read" {
				questions, e = ReadQuestions(r.Snapshot)
			} else {
				questions, e = ScoreQuestions(r.Snapshot, requirements)
			}
			if e != nil {
				return e
			}
			for _, q := range questions {
				_, e = tx.Exec(ctx, `INSERT INTO match_run_question(user_id,run_id,stage,subject,state_hash,question_hash,state,question) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, u, id, r.Stage, q.Subject, q.StateHash, q.QuestionHash, q.State, systemone.Raw(q.Question))
				if e != nil {
					return e
				}
			}
			// Recover answers paid by another/cancelled run before planning any work.
			pending := []decide.Pending{}
			rows, e := tx.Query(ctx, `SELECT subject,state,question,state_hash,question_hash FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND stage=$3 AND status='pending' AND operation_id IS NULL ORDER BY subject`, u, id, r.Stage)
			if e != nil {
				return e
			}
			for rows.Next() {
				var p decide.Pending
				if e = rows.Scan(&p.Subject, &p.State, &p.Question, &p.StateHash, &p.QuestionHash); e != nil {
					rows.Close()
					return e
				}
				pending = append(pending, p)
			}
			rows.Close()
			if e = rows.Err(); e != nil {
				return e
			}
			misses := []decide.Pending{}
			for _, p := range pending {
				answer, e := decide.Lookup(ctx, tx, u, *r.Model, p)
				if e != nil {
					return e
				}
				if answer == nil {
					misses = append(misses, p)
					continue
				}
				if _, e = tx.Exec(ctx, `UPDATE match_run_question SET status='cached',answer=$5 WHERE user_id=$1 AND run_id=$2 AND stage=$3 AND subject=$4`, u, id, r.Stage, p.Subject, systemone.Raw(answer)); e != nil {
					return e
				}
			}
			var op worker.Operation
			var raw []byte
			e = tx.QueryRow(ctx, `SELECT to_jsonb(o) FROM match_run_operation m JOIN model_operation o ON o.user_id=m.user_id AND o.id=m.operation_id WHERE m.user_id=$1 AND m.run_id=$2 AND NOT m.ingested ORDER BY o.created_at LIMIT 1`, u, id).Scan(&raw)
			if e == nil {
				if e = json.Unmarshal(raw, &op); e != nil {
					return e
				}
				status := "running"
				if op.Status == "blocked" || op.Status == "needs_review" {
					status = op.Status
				}
				return s.status(ctx, tx, u, r, status, op.BlockedReason, op.NextEligibleAt)
			}
			if !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
			if len(misses) > 0 {
				maxBytes, maxQuestions := systemone.RequestBudget, 0
				var quoteRaw []byte
				qe := tx.QueryRow(ctx, `SELECT quote FROM platform.model_quote WHERE model=$1`, *r.Model).Scan(&quoteRaw)
				if qe != nil && !errors.Is(qe, pgx.ErrNoRows) {
					return qe
				}
				if qe == nil {
					var q modelapi.Quote
					if e = json.Unmarshal(quoteRaw, &q); e != nil {
						return e
					}
					if q.Decision && q.MaxRequestBytes > 0 {
						maxBytes = min(maxBytes, q.MaxRequestBytes)
						maxQuestions = q.MaxQuestions
					}
				}
				packed, e := decide.Pack(*r.Model, misses, maxBytes, maxQuestions)
				if e != nil {
					return s.status(ctx, tx, u, r, "failed", "question_too_large", nil)
				}
				p := packed[0]
				opID := uuid.Must(uuid.NewV7())
				hash, e := systemone.Hash(p.Request)
				if e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'decision',$3,$4)`, opID, u, "match:"+id.String()+":"+opID.String(), hash); e != nil {
					return e
				}
				if e = decide.Store(ctx, tx, u, opID, p); e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO match_run_operation(user_id,run_id,operation_id) VALUES($1,$2,$3)`, u, id, opID); e != nil {
					return e
				}
				for key, item := range p.Items {
					if _, e = tx.Exec(ctx, `UPDATE match_run_question SET operation_id=$5,request_key=$6,tries=tries+1 WHERE user_id=$1 AND run_id=$2 AND stage=$3 AND subject=$4`, u, id, r.Stage, item.Subject, opID, key); e != nil {
						return e
					}
				}
				if e = s.queue.InsertTx(ctx, tx, worker.Args{UserID: u, OperationID: opID}); e != nil {
					return e
				}
				return s.status(ctx, tx, u, r, "running", "", nil)
			}
			if r.Stage == "read" {
				a, e = answers(ctx, tx, u, id)
				if e != nil {
					return e
				}
				requirements, _ = Requirements(r.Snapshot, a)
				for _, req := range requirements {
					if _, e = tx.Exec(ctx, `INSERT INTO match_run_requirement(user_id,run_id,text_hash,facts) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, u, id, req.TextHash, systemone.Raw(req)); e != nil {
						return e
					}
				}
				r.Stage = "score"
				if _, e = tx.Exec(ctx, `UPDATE match_run SET stage='score' WHERE user_id=$1 AND id=$2`, u, id); e != nil {
					return e
				}
				continue
			}
			r.Stage = "finalize"
			return s.finalize(ctx, tx, u, r, true)
		}
		return nil
	})
}
func (s *Service) ingest(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) error {
	type completed struct {
		ID                     uuid.UUID
		Status, Result, Reason string
	}
	ops := []completed{}
	rows, e := tx.Query(ctx, `SELECT o.id,o.status,o.result,o.blocked_reason FROM match_run_operation m JOIN model_operation o ON o.user_id=m.user_id AND o.id=m.operation_id WHERE m.user_id=$1 AND m.run_id=$2 AND NOT m.ingested AND o.status IN ('completed','failed','cancelled') ORDER BY o.created_at`, u, id)
	if e != nil {
		return e
	}
	for rows.Next() {
		var op completed
		if e = rows.Scan(&op.ID, &op.Status, &op.Result, &op.Reason); e != nil {
			rows.Close()
			return e
		}
		ops = append(ops, op)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, op := range ops {
		valid := map[string]systemone.Answer{}
		if op.Status == "completed" {
			valid, e = decide.Ingest(ctx, tx, u, op.ID, op.Result)
			if e != nil {
				return e
			}
		}
		rows, e = tx.Query(ctx, `SELECT stage,subject,request_key,tries FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND operation_id=$3`, u, id, op.ID)
		if e != nil {
			return e
		}
		type item struct {
			stage, subject, key string
			tries               int
		}
		items := []item{}
		for rows.Next() {
			var it item
			if e = rows.Scan(&it.stage, &it.subject, &it.key, &it.tries); e != nil {
				rows.Close()
				return e
			}
			items = append(items, it)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		for _, it := range items {
			if a, ok := valid[it.key]; ok {
				_, e = tx.Exec(ctx, `UPDATE match_run_question SET status='answered',answer=$5 WHERE user_id=$1 AND run_id=$2 AND stage=$3 AND subject=$4`, u, id, it.stage, it.subject, systemone.Raw(a))
			} else {
				status := "pending"
				if op.Reason == "request_too_large" {
					it.tries = max(0, it.tries-1)
				}
				if it.tries >= 2 {
					status = "unanswered"
				}
				_, e = tx.Exec(ctx, `UPDATE match_run_question SET status=$5,operation_id=NULL,request_key=NULL,tries=$6 WHERE user_id=$1 AND run_id=$2 AND stage=$3 AND subject=$4`, u, id, it.stage, it.subject, status, it.tries)
			}
			if e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE match_run_operation SET ingested=true WHERE user_id=$1 AND run_id=$2 AND operation_id=$3`, u, id, op.ID); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) status(ctx context.Context, tx pgx.Tx, u uuid.UUID, r Run, status, reason string, next *time.Time) error {
	_, e := tx.Exec(ctx, `UPDATE match_run SET status=$3,blocked_reason=$4,next_eligible_at=$5,version=version+1,
 total=(SELECT count(*) FROM match_run_question WHERE user_id=$1 AND run_id=$2),
 answered=(SELECT count(*) FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND status IN ('answered','cached')),
 cached=(SELECT count(*) FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND status='cached'),
 unanswered=(SELECT count(*) FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND status='unanswered'),
 finished_at=CASE WHEN $3='failed' THEN now() ELSE finished_at END
 WHERE user_id=$1 AND id=$2`, u, r.ID, status, reason, next)
	if e != nil {
		return e
	}
	return emitRun(ctx, tx, u, r.ID, r.JobID)
}
func (s *Service) finalize(ctx context.Context, tx pgx.Tx, u uuid.UUID, r Run, followup bool) error {
	a, e := answers(ctx, tx, u, r.ID)
	if e != nil {
		return e
	}
	o, e := overrides(ctx, tx, u, r.JobID)
	if e != nil {
		return e
	}
	asOf := r.CreatedAt
	if r.FinishedAt != nil {
		asOf = *r.FinishedAt
	} else {
		asOf = time.Now().UTC()
	}
	requirements, incomplete := Requirements(r.Snapshot, a, o)
	result := Compute(r.Snapshot, requirements, a, o, asOf, incomplete)
	status := "completed"
	if result.Partial {
		status = "partial"
	}
	if e = s.status(ctx, tx, u, r, status, "", nil); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE match_run SET stage='finalize',result=$3,score=$4,must_met=$5,must_total=$6,finished_at=$7 WHERE user_id=$1 AND id=$2`, u, r.ID, systemone.Raw(result), result.Score, result.MustMet, result.MustTotal, asOf); e != nil {
		return e
	}
	if followup && r.RerunRequested {
		_, e = s.create(ctx, tx, u, r.JobID, "auto:"+uuid.NewString(), "auto")
		return e
	}
	return nil
}
