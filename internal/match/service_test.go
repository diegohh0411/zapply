package match_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type provider struct {
	calls        int
	discard      bool
	uncertain    bool
	maxQuestions int
}

func (p *provider) Quote(context.Context) (modelapi.Quote, error) {
	return modelapi.Quote{}, errors.New("chat called")
}
func (p *provider) Generate(context.Context, string, modelapi.Quote) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("chat called")
}
func (p *provider) Lookup(context.Context, string, string) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("lookup unsupported")
}
func (p *provider) DecisionLookupSupported(string) bool { return false }
func (p *provider) QuoteDecision(_ context.Context, r systemone.Request) (modelapi.Quote, error) {
	q := modelapi.Quote{Model: r.Model, Reserved: 1000, FetchedAt: time.Now(), Decision: true, InputRate: "0.000000000001", RequestRate: "0", MaxInput: 1000, MaxRequestBytes: systemone.RequestBudget, BoundReference: "fake provider fixture", MaxQuestions: p.maxQuestions}
	if p.maxQuestions > 0 && len(r.Questions) > p.maxQuestions {
		return q, &modelapi.ProviderError{Code: "request_too_large", Uncharged: true}
	}
	return q, nil
}
func (p *provider) Decide(_ context.Context, key string, r systemone.Request, _ modelapi.Quote) (modelapi.Result, error) {
	p.calls++
	if key != "test-provider-secret" {
		return modelapi.Result{}, errors.New("wrong key")
	}
	if p.uncertain {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "transport_uncertain"}
	}
	answers := map[string]systemone.Answer{}
	confidence := .8
	for k, q := range r.Questions {
		a := systemone.Answer{Type: q.Type, Confidence: &confidence}
		switch q.Type {
		case "noul":
			n := .9
			a.Noul = &n
		case "choice":
			var options map[string]string
			json.Unmarshal(q.Criteria, &options)
			chosen := "not_specified"
			if _, ok := options["must"]; ok {
				chosen = "must"
			} else if _, ok := options["skill"]; ok {
				chosen = "skill"
			}
			a.Choice = chosen
			a.Probabilities = map[string]float64{}
			for key := range options {
				a.Probabilities[key] = 0
			}
			a.Probabilities[chosen] = 1
		case "score":
			var criteria []json.RawMessage
			json.Unmarshal(q.Criteria, &criteria)
			score := 3.
			a.Score = &score
			a.Legend = map[string]json.RawMessage{}
			a.Probabilities = map[string]float64{"0": 0, "1": 0, "2": 0, "3": 1}
			for i, c := range criteria {
				a.Legend[string(rune('0'+i))] = c
			}
		}
		if !p.discard {
			answers[k] = a
		}
	}
	amount := int64(500)
	return modelapi.Result{GenerationID: "gen-" + uuid.NewString(), Text: string(systemone.Raw(answers)), Amount: &amount, Usage: systemone.Raw(map[string]int{"input_tokens": 10})}, nil
}

type fixture struct {
	db       *testdb.DB
	u        uuid.UUID
	s        *match.Service
	js       *jobs.Service
	lib      *library.Service
	runtime  *worker.Runtime
	p        *provider
	job      jobs.Job
	evidence library.Evidence
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	u, e := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.SetOwner(ctx, d.AdminURL, u, true); e != nil {
		t.Fatal(e)
	}
	ring, e := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "v1")
	if e != nil {
		t.Fatal(e)
	}
	member := membership.New(d.Store, ring)
	plan := uuid.New()
	if _, e = member.SavePlan(ctx, u, plan, "Test", "1", "2", 0); e != nil {
		t.Fatal(e)
	}
	if _, e = member.SaveMembership(ctx, u, u, plan, "active", 0); e != nil {
		t.Fatal(e)
	}
	key, e := member.AddKey(ctx, u, "Primary", "test-provider-secret")
	if e != nil {
		t.Fatal(e)
	}
	if e = member.ActivateKey(ctx, u, key, true, 1); e != nil {
		t.Fatal(e)
	}
	if e = d.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT platform.save_model_settings('test/decision',1)`)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	workerStore, e := store.Open(ctx, d.WorkerURL)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(workerStore.Close)
	wq, e := worker.NewQueue(workerStore)
	if e != nil {
		t.Fatal(e)
	}
	ws := match.New(workerStore, wq)
	p := &provider{}
	runtime := &worker.Runtime{Store: workerStore, Keys: ring, Provider: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OnTerminal: ws.OnTerminal}
	js := jobs.NewService(d.Store)
	job, e := js.CreateJob(ctx, u, jobs.JobInput{Title: "Engineer", Company: "Acme", Description: "Experience in Go"})
	if e != nil {
		t.Fatal(e)
	}
	lib := library.NewService(d.Store)
	evidence, _, e := lib.CreateEvidence(ctx, u, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: "Built Go services", Highlights: []library.HighlightInput{{Text: "Built and operated Go services"}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{d, u, ws, js, lib, runtime, p, job, evidence}
}
func (f *fixture) start(t *testing.T) match.Run {
	t.Helper()
	q, e := worker.NewQueue(f.db.Store)
	if e != nil {
		t.Fatal(e)
	}
	r, e := match.New(f.db.Store, q).Start(context.Background(), f.u, f.job.ID, uuid.NewString())
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (f *fixture) run(t *testing.T, id uuid.UUID) match.Run {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if e := f.s.Advance(ctx, f.u, id); e != nil {
			t.Fatal(e)
		}
		r, e := f.s.Detail(ctx, f.u, id)
		if e != nil {
			t.Fatal(e)
		}
		if r.Status == "completed" || r.Status == "partial" || r.Status == "cancelled" {
			return r
		}
		var ops []uuid.UUID
		e = f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
			rows, e := tx.Query(ctx, `SELECT o.id FROM match_run_operation m JOIN model_operation o ON o.user_id=m.user_id AND o.id=m.operation_id WHERE m.user_id=$1 AND m.run_id=$2 AND NOT m.ingested AND o.status IN ('pending','blocked')`, f.u, id)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var op uuid.UUID
				if e = rows.Scan(&op); e != nil {
					return e
				}
				ops = append(ops, op)
			}
			return rows.Err()
		})
		if e != nil {
			t.Fatal(e)
		}
		for _, op := range ops {
			if e = f.runtime.Process(ctx, worker.Args{UserID: f.u, OperationID: op}); e != nil && !strings.Contains(e.Error(), "snooze") {
				t.Fatal(e)
			}
		}
	}
	t.Fatal("run did not finish")
	return match.Run{}
}
func TestColdWarmRunsCorrectionsAndIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := f.start(t)
	r = f.run(t, r.ID)
	if r.Status != "completed" || r.Score == nil || *r.Score != 100 || len(r.Result.Selection) != 1 || f.p.calls != 2 {
		t.Fatalf("cold run: %#v, calls %d", r, f.p.calls)
	}
	// Completion hook can be lost: Advance itself ingests terminal operations.
	f.runtime.OnTerminal = nil
	r = f.start(t)
	r = f.run(t, r.ID)
	if f.p.calls != 2 || r.Cached != r.Total {
		t.Fatalf("warm run called model: %d %#v", f.p.calls, r)
	}
	ignore := "ignore"
	if e := f.s.Override(ctx, f.u, f.job.ID, r.Result.Requirements[0].TextHash, &ignore); e != nil {
		t.Fatal(e)
	}
	r, e := f.s.Detail(ctx, f.u, r.ID)
	if e != nil || r.MustTotal != 0 || r.Score != nil {
		t.Fatalf("correction %v %#v", e, r)
	}
	other, e := f.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "other"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Detail(ctx, other, r.ID); !errors.Is(e, library.ErrNotFound) {
		t.Fatalf("tenant leak %v", e)
	}
	if e = f.s.Cancel(ctx, other, r.ID); !errors.Is(e, library.ErrNotFound) {
		t.Fatalf("cross tenant cancel %v", e)
	}
	// A library edit marks the immutable historical run stale.
	_, e = f.lib.UpdateEvidence(ctx, f.u, f.evidence.ID, f.evidence.Version, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: "Updated Go service"})
	if e != nil {
		t.Fatal(e)
	}
	r, e = f.s.Detail(ctx, f.u, r.ID)
	if e != nil || !r.Stale || r.Changed != 1 {
		t.Fatalf("stale %v %#v", e, r)
	}
}
func TestDiscardRetryThenPartialAndCancel(t *testing.T) {
	f := setup(t)
	f.p.discard = true
	r := f.start(t)
	r = f.run(t, r.ID)
	if r.Status != "partial" || r.Score != nil || f.p.calls != 2 {
		t.Fatalf("discard %#v calls %d", r, f.p.calls)
	}
	r = f.start(t)
	if e := f.s.Cancel(context.Background(), f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	if e := f.s.Advance(context.Background(), f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	if f.p.calls != 2 {
		t.Fatal("cancel dispatched")
	}
}
func TestAutoRunsSupersedeAndUnconfigured(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.js.Matcher = f.s
	job, e := f.js.CreateJob(ctx, f.u, jobs.JobInput{Title: "Automatic", Description: "Go"})
	if e != nil {
		t.Fatal(e)
	}
	runs, e := f.s.List(ctx, f.u, job.ID, nil, 10)
	if e != nil || len(runs) != 1 {
		t.Fatalf("auto create %v %#v", e, runs)
	}
	first := runs[0].ID
	job, e = f.js.UpdateJob(ctx, f.u, job.ID, job.Version, jobs.JobInput{Title: "Changed", Description: "Go"})
	if e != nil {
		t.Fatal(e)
	}
	runs, e = f.s.List(ctx, f.u, job.ID, nil, 10)
	if e != nil || len(runs) != 2 || runs[1].Status != "cancelled" {
		t.Fatalf("supersede %v %#v", e, runs)
	}
	if e = f.s.Advance(ctx, f.u, first); e != nil {
		t.Fatal(e)
	}
	if f.p.calls != 0 {
		t.Fatal("superseded dispatch")
	}
	if e = f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error { _, e := tx.Exec(ctx, `SELECT platform.save_model_settings(NULL,2)`); return e }); e != nil {
		t.Fatal(e)
	}
	r := f.start(t)
	if e = f.s.Advance(ctx, f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	r, e = f.s.Detail(ctx, f.u, r.ID)
	if e != nil || r.Status != "blocked" || r.BlockedReason != "model_not_configured" {
		t.Fatalf("unconfigured %v %#v", e, r)
	}
	if e = f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT platform.save_model_settings('test/decision',3)`)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	r = f.run(t, r.ID)
	if r.Model == nil || *r.Model != "test/decision" {
		t.Fatal("model not bound")
	}
}

func TestUncertainAccountingResumesWithNewOperation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.p.uncertain = true
	r := f.start(t)
	if e := f.s.Advance(ctx, f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	var op uuid.UUID
	if e := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT operation_id FROM match_run_operation WHERE user_id=$1 AND run_id=$2`, f.u, r.ID).Scan(&op)
	}); e != nil {
		t.Fatal(e)
	}
	_ = f.runtime.Process(ctx, worker.Args{UserID: f.u, OperationID: op})
	if e := f.s.Advance(ctx, f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	r, e := f.s.Detail(ctx, f.u, r.ID)
	if e != nil || r.Status != "needs_review" || f.p.calls != 1 {
		t.Fatalf("uncertain %v %#v calls %d", e, r, f.p.calls)
	}
	var attempt uuid.UUID
	if e = f.db.Admin(t).QueryRow(ctx, `UPDATE platform.model_attempt SET deadline_at=now()-interval '1 minute' WHERE operation_id=$1 RETURNING id`, op).Scan(&attempt); e != nil {
		t.Fatal(e)
	}
	member := membership.New(f.db.Store, f.runtime.Keys)
	if e = member.Resolve(ctx, f.u, attempt, "0", nil, "Fake provider confirms uncharged", 2); e != nil {
		t.Fatal(e)
	}
	if e = f.runtime.Process(ctx, worker.Args{UserID: f.u, OperationID: op}); e != nil {
		t.Fatal(e)
	}
	f.p.uncertain = false
	r = f.run(t, r.ID)
	if r.Status != "completed" || f.p.calls != 3 {
		t.Fatalf("resume %#v calls %d", r, f.p.calls)
	}
	// Reprocessing the old operation never sends it again.
	if e = f.runtime.Process(ctx, worker.Args{UserID: f.u, OperationID: op}); e != nil {
		t.Fatal(e)
	}
	if f.p.calls != 3 {
		t.Fatal("uncertain operation resent")
	}
}
func TestEstimateCacheAndModelSnapshots(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	before, e := f.s.Estimate(ctx, f.u, f.job.ID)
	if e != nil || before.Available {
		t.Fatalf("missing quote estimate %v %#v", e, before)
	}
	r := f.start(t)
	r = f.run(t, r.ID)
	estimate, e := f.s.Estimate(ctx, f.u, f.job.ID)
	if e != nil || !estimate.Available || estimate.Requests != 0 || estimate.CostUSD == nil || *estimate.CostUSD != "0.000000000000" {
		t.Fatalf("warm estimate %v %#v", e, estimate)
	}
	old := f.start(t)
	if e = f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT platform.save_model_settings('test/new-model',2)`)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	old = f.run(t, old.ID)
	if *old.Model != "test/decision" {
		t.Fatal("model changed on bound run")
	}
	fresh := f.start(t)
	if fresh.Model == nil || *fresh.Model != "test/new-model" {
		t.Fatal("new run did not select changed model")
	}
}

func TestCancelRacesReservation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := f.start(t)
	if e := f.s.Advance(ctx, f.u, r.ID); e != nil {
		t.Fatal(e)
	}
	var op uuid.UUID
	if e := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT operation_id FROM match_run_operation WHERE user_id=$1 AND run_id=$2`, f.u, r.ID).Scan(&op)
	}); e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	var cancelled bool
	var reservation struct {
		Reason string `json:"reason"`
	}
	go func() {
		<-start
		done <- f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT platform.cancel_operation($1)`, op).Scan(&cancelled)
		})
	}()
	go func() {
		<-start
		done <- f.runtime.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
			var raw []byte
			e := tx.QueryRow(ctx, `SELECT platform.reserve($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), op, "test/decision", "request-hash", json.RawMessage(`{}`), int64(1000), time.Now().Add(time.Minute)).Scan(&raw)
			if e != nil {
				return e
			}
			return json.Unmarshal(raw, &reservation)
		})
	}()
	close(start)
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if cancelled == (reservation.Reason == "") {
		t.Fatalf("cancel %v, reserve reason %q: expected exactly one winner", cancelled, reservation.Reason)
	}
}
func TestVerifiedPackingLimitsSplitBeforeDispatch(t *testing.T) {
	f := setup(t)
	f.p.maxQuestions = 1
	r := f.start(t)
	r = f.run(t, r.ID)
	if r.Status != "completed" || f.p.calls != r.Total || r.Unanswered != 0 {
		t.Fatalf("packing limits not applied: %#v calls %d", r, f.p.calls)
	}
}

func TestDisplayFlagsReuseAnswersAndNewSkillsOnlyAskNewQuestion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := f.start(t)
	f.run(t, r.ID)
	calls := f.p.calls
	off := false
	h := f.evidence.Highlights[0]
	_, e := f.lib.UpdateEvidence(ctx, f.u, f.evidence.ID, f.evidence.Version, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: f.evidence.Title, AutoInclude: &off, Highlights: []library.HighlightInput{{ID: &h.ID, Text: h.Text, AutoInclude: &off}}})
	if e != nil {
		t.Fatal(e)
	}
	r = f.start(t)
	r = f.run(t, r.ID)
	if f.p.calls != calls || r.Cached != r.Total || len(r.Result.Selection) != 0 {
		t.Fatalf("display edit lost cache: calls %d -> %d", calls, f.p.calls)
	}
	_, e = f.lib.CreateSkill(ctx, f.u, library.SkillInput{Name: "Go", Type: "hard"})
	if e != nil {
		t.Fatal(e)
	}
	r = f.start(t)
	r = f.run(t, r.ID)
	if f.p.calls != calls+1 || r.Total-r.Cached != 1 {
		t.Fatalf("new skill invalidated existing questions: %#v calls %d", r, f.p.calls)
	}
}
