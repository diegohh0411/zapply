package preferences_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/preferences"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
)

// model answers the area question: mostly software, partly data.
type model struct{ calls atomic.Int32 }

func (m *model) Quote(context.Context) (modelapi.Quote, error) {
	return modelapi.Quote{}, errors.New("unused")
}
func (m *model) Generate(context.Context, string, modelapi.Quote) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("unused")
}
func (m *model) Lookup(context.Context, string, string) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("unused")
}
func sheet(name string) billing.Sheet {
	return billing.Sheet{Provider: "test", Model: name, InputRate: 1000, Context: 32000, InputPerQuestion: true, MaxRequestBytes: extraction.RequestBudget,
		Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 16}, FetchedAt: time.Now()}
}
func (m *model) DecisionSheet(_ context.Context, name string) (billing.Sheet, error) {
	return sheet(name), nil
}
func (m *model) DecisionLookupSupported(string) bool { return false }
func (m *model) ExtractionSheet(_ context.Context, name string) (billing.Sheet, error) {
	return sheet(name), nil
}
func (m *model) Extract(context.Context, string, extraction.Request, modelapi.Quote) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("unused")
}
func (m *model) Decide(_ context.Context, _ string, r systemone.Request, _ modelapi.Quote) (modelapi.Result, error) {
	m.calls.Add(1)
	out := map[string]systemone.Answer{}
	for key, q := range r.Questions {
		var criteria map[string]json.RawMessage
		_ = json.Unmarshal(q.Criteria, &criteria)
		probs := map[string]float64{}
		for k := range criteria {
			probs[k] = 0.05 / float64(len(criteria)-2)
		}
		probs["engineering_software"], probs["data_analytics"] = 0.65, 0.3
		c := 0.8
		out[key] = systemone.Answer{Type: "choice", Choice: "engineering_software", Probabilities: probs, Confidence: &c}
	}
	raw, _ := json.Marshal(out)
	amount := int64(2_580_000)
	return modelapi.Result{GenerationID: "gen-" + uuid.NewString(), Text: string(raw), Amount: &amount, Usage: json.RawMessage(`{}`)}, nil
}

type env struct {
	db      *testdb.DB
	lib     *library.Service
	prefs   *preferences.Service
	worker  *preferences.Service
	runtime *worker.Runtime
	model   *model
	member  uuid.UUID
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	u, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwner(ctx, d.AdminURL, u, true); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	ms := membership.New(d.Store, ring)
	plan := uuid.New()
	if _, err := ms.SavePlan(ctx, u, plan, "Test", "1", "2", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.SaveMembership(ctx, u, u, plan, "active", 0); err != nil {
		t.Fatal(err)
	}
	key, err := ms.AddKey(ctx, u, "Primary", "provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.ActivateKey(ctx, u, key, true, 1); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, d.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ws.Close)
	q, err := worker.NewQueue(d.Store)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{db: d, lib: library.NewService(d.Store), prefs: preferences.New(d.Store, q, log), worker: preferences.New(ws, q, log), model: &model{}, member: u}
	e.runtime = &worker.Runtime{Store: ws, Provider: e.model, Keys: ring, Log: log}
	e.runtime.OnTerminal = func(ctx context.Context, tx pgx.Tx, op worker.Operation) error {
		handled, err := e.worker.OnTerminal(ctx, tx, op)
		if !handled && err == nil {
			t.Errorf("operation %s not claimed", op.ID)
		}
		return err
	}
	return e
}

func (e *env) setModel(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := e.db.Store.InTenant(ctx, e.member, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.save_posting_models('liquid/d1.2',NULL)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) evidence(t *testing.T, in library.EvidenceInput) {
	t.Helper()
	if _, _, err := e.lib.CreateEvidence(context.Background(), e.member, in, nil); err != nil {
		t.Fatal(err)
	}
}

// runAreas processes the open area question, if any.
func (e *env) runAreas(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	var op *uuid.UUID
	if err := e.db.Store.InTenant(ctx, e.member, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT area_operation FROM job_preference`).Scan(&op)
	}); err != nil {
		t.Fatal(err)
	}
	if op == nil {
		return 0
	}
	if err := e.runtime.Process(ctx, worker.Args{UserID: e.member, OperationID: *op}); err != nil {
		t.Fatal(err)
	}
	return 1
}

func TestDeriveEditAndRederive(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.lib.PutProfile(ctx, e.member, 0, library.Profile{Location: "Guadalajara, Mexico", Headline: "Backend engineer", AutoMatch: true}); err != nil {
		t.Fatal(err)
	}
	e.evidence(t, library.EvidenceInput{Kind: "job", Category: "paid", Title: "Software Engineer", Start: "2019-01", End: "2022-12", Skills: []string{"Go", "SQL"}})
	e.evidence(t, library.EvidenceInput{Kind: "job", Category: "paid", Title: "Senior Software Engineer", Start: "2023-01", IsCurrent: true})

	// Without a model the rules still derive; areas wait for the member.
	p, err := e.prefs.Get(ctx, e.member)
	if err != nil {
		t.Fatal(err)
	}
	if p.SeniorityMin == nil || *p.SeniorityMin != "senior" || *p.SeniorityMax != "staff" || !slices.Equal(p.Countries, []string{"MX"}) ||
		!p.RemoteOK || !slices.Equal(p.ExcludedEmployment, []string{"internship"}) || len(p.Areas) != 0 || p.Deriving {
		t.Fatalf("derived = %+v", p)
	}
	if p.Sources["seniority"] != "derived" {
		t.Fatalf("sources = %v", p.Sources)
	}

	// With a model, a library change asks the area question, charged to the member.
	e.setModel(t)
	e.evidence(t, library.EvidenceInput{Kind: "freelance", Category: "paid", Title: "Data consultant", Start: "2018-01", End: "2018-12"})
	if p, err = e.prefs.Get(ctx, e.member); err != nil || !p.Deriving {
		t.Fatalf("deriving = %+v, %v", p, err)
	}
	if e.runAreas(t) != 1 {
		t.Fatal("no area question")
	}
	if p, err = e.prefs.Get(ctx, e.member); err != nil || !slices.Equal(p.Areas, []string{"engineering_software", "data_analytics"}) || p.Deriving {
		t.Fatalf("areas = %+v, %v", p, err)
	}
	var spent int
	if err := e.db.Store.InTenant(ctx, e.member, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM model_operation WHERE user_id=$1 AND kind='decision' AND status='completed'`, e.member).Scan(&spent)
	}); err != nil || spent != 1 {
		t.Fatalf("member operations = %d, %v", spent, err)
	}

	// The member's choices survive re-derivation; the rest follows the library.
	areas := []string{"product_design"}
	p, err = e.prefs.Save(ctx, e.member, p.Version, preferences.Update{Areas: &areas, Seniority: &[2]string{"senior", "staff"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.prefs.Save(ctx, e.member, p.Version-1, preferences.Update{Areas: &areas}); !errors.Is(err, library.ErrVersionMismatch) {
		t.Fatalf("stale save = %v", err)
	}
	if _, err := e.lib.PutProfile(ctx, e.member, 1, library.Profile{Location: "Madrid, Spain", AutoMatch: true}); err != nil {
		t.Fatal(err)
	}
	if p, err = e.prefs.Get(ctx, e.member); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Areas, areas) || *p.SeniorityMin != "senior" || !slices.Equal(p.Countries, []string{"ES"}) || p.Sources["areas"] != "member" {
		t.Fatalf("after rederive = %+v", p)
	}
	// The profile lost its headline, so the area question was asked again.
	e.runAreas(t)

	// Reset puts a group back on the derivation.
	if p, err = e.prefs.Reset(ctx, e.member, p.Version, []string{"areas"}); err != nil {
		t.Fatal(err)
	}
	if p.Sources["areas"] != "derived" || !slices.Equal(p.Areas, p.Suggested.Areas) || len(p.Areas) == 0 {
		t.Fatalf("after reset = %+v", p)
	}

	// Invalid input is rejected.
	bad := []string{"astronaut"}
	var ve *library.ValidationError
	if _, err := e.prefs.Save(ctx, e.member, p.Version, preferences.Update{Areas: &bad}); !errors.As(err, &ve) {
		t.Fatalf("bad area = %v", err)
	}
	if _, err := e.prefs.Save(ctx, e.member, p.Version, preferences.Update{Seniority: &[2]string{"staff", "entry"}}); !errors.As(err, &ve) {
		t.Fatalf("inverted range = %v", err)
	}
}

func TestUnchangedLibraryAsksOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.setModel(t)
	e.evidence(t, library.EvidenceInput{Kind: "job", Category: "paid", Title: "Accountant", Start: "2020-01", IsCurrent: true})
	if _, err := e.prefs.Get(ctx, e.member); err != nil {
		t.Fatal(err)
	}
	e.runAreas(t)
	// A change the area question does not read (volunteering) asks nothing.
	e.evidence(t, library.EvidenceInput{Kind: "volunteering", Category: "unpaid", Title: "Food bank", Start: "2021-01", End: "2021-06"})
	if err := e.worker.Recover(ctx, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5500 * time.Millisecond) // the recovery waits for a quiet period
	if err := e.worker.Recover(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := e.runAreas(t); n != 0 || e.model.calls.Load() != 1 {
		t.Fatalf("asked again: open=%d calls=%d", n, e.model.calls.Load())
	}
	p, err := e.prefs.Get(ctx, e.member)
	if err != nil || p.DerivedAt == nil {
		t.Fatalf("recovered = %+v, %v", p, err)
	}
}
