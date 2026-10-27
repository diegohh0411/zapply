package worker_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/llm"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type fake struct {
	calls            atomic.Int32
	amount           int64
	uncertain        bool
	missingCost      bool
	text             string
	entered, release chan struct{}
}

func (f *fake) Quote(context.Context) (llm.Quote, error) {
	return llm.Quote{Model: "test/model", Endpoint: "test", Reserved: f.amount, FetchedAt: time.Now()}, nil
}
func (f *fake) Generate(ctx context.Context, key string, _ llm.Quote) (llm.Result, error) {
	f.calls.Add(1)
	if key != "test-provider-secret" {
		return llm.Result{}, errors.New("wrong credential")
	}
	if f.entered != nil {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return llm.Result{}, ctx.Err()
		}
	}
	if f.uncertain {
		return llm.Result{}, &llm.ProviderError{Code: "transport_uncertain"}
	}
	if f.missingCost {
		return llm.Result{GenerationID: "gen-missing-cost", Text: "OK"}, nil
	}
	amount := f.amount
	text := "OK"
	if f.text != "" {
		text = f.text
	}
	return llm.Result{GenerationID: "gen-" + uuid.NewString(), Text: text, Amount: &amount, Usage: json.RawMessage(`{"prompt_tokens":4}`)}, nil
}
func (f *fake) Lookup(_ context.Context, key string, _ string) (llm.Result, error) {
	if key != "test-provider-secret" {
		return llm.Result{}, errors.New("lookup used replacement credential")
	}
	amount := f.amount
	return llm.Result{Amount: &amount}, nil
}

type fixture struct {
	db   *testdb.DB
	u    uuid.UUID
	s    *membership.Service
	q    *worker.Queue
	r    *worker.Runtime
	p    *fake
	plan uuid.UUID
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	u, err := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwner(ctx, db.AdminURL, u, true); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	s := membership.New(db.Store, ring)
	plan := uuid.Must(uuid.NewV7())
	if _, err := s.SavePlan(ctx, u, plan, "Member", "1", "2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveMembership(ctx, u, u, plan, "active", 0); err != nil {
		t.Fatal(err)
	}
	key, err := s.AddKey(ctx, u, "Primary", "test-provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateKey(ctx, u, key, true, 1); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, db.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	q, err := worker.NewQueue(db.Store)
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := llm.ParseUSD("0.6")
	p := &fake{amount: amount}
	r := &worker.Runtime{Store: st, Provider: p, Keys: ring, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &fixture{db, u, s, q, r, p, plan}
}
func (f *fixture) operation(t *testing.T) worker.Args {
	t.Helper()
	op, err := f.q.Enqueue(context.Background(), f.u, uuid.NewString(), "test-request")
	if err != nil {
		t.Fatal(err)
	}
	return worker.Args{UserID: f.u, OperationID: op.ID}
}
func (f *fixture) state(t *testing.T, args worker.Args) worker.Operation {
	t.Helper()
	op, err := f.q.Get(context.Background(), f.u, args.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func (f *fixture) usage(t *testing.T) map[string]any {
	t.Helper()
	raw, err := f.s.Usage(context.Background(), f.u)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConcurrentReservationsAndReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first, second := f.operation(t), f.operation(t)
	f.p.entered = make(chan struct{})
	f.p.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- f.r.Process(ctx, first) }()
	select {
	case <-f.p.entered:
	case err := <-done:
		t.Fatalf("first dispatch stopped before provider call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider dispatch timed out")
	}
	if err := f.r.Process(ctx, second); err == nil {
		t.Fatal("simultaneous call did not pause")
	}
	if f.p.calls.Load() != 1 {
		t.Fatal("multiple paid calls dispatched")
	}
	close(f.p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.p.entered = nil
	if err := f.r.Process(ctx, first); err != nil {
		t.Fatal(err)
	}
	if f.p.calls.Load() != 1 {
		t.Fatal("completed operation was billed twice")
	}
	if err := f.r.Process(ctx, second); err == nil {
		t.Fatal("weekly limit did not block")
	}
	if op := f.state(t, second); op.BlockedReason != "usage_limit" || op.NextEligibleAt == nil {
		t.Fatalf("blocked state: %+v", op)
	}
	got := f.usage(t)["reserved_usd"]
	if n, err := llm.ParseUSD(got.(string)); err != nil || n != 0 {
		t.Fatalf("hold not released: %v", got)
	}
	// A plan change preserves the first charge and makes a second call eligible.
	if _, err := f.s.SavePlan(ctx, f.u, f.plan, "Member", "2", "2", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Process(ctx, second); err != nil {
		t.Fatal(err)
	}
	if f.p.calls.Load() != 2 {
		t.Fatalf("calls=%d", f.p.calls.Load())
	}
}

func TestRollingWindowsAndSuspension(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := f.operation(t)
	if err := f.r.Process(ctx, first); err != nil {
		t.Fatal(err)
	}
	admin := f.db.Admin(t)
	if _, err := admin.Exec(ctx, "UPDATE platform.llm_usage SET accounted_at=now()-interval '8 days'"); err != nil {
		t.Fatal(err)
	}
	second := f.operation(t)
	if err := f.r.Process(ctx, second); err != nil {
		t.Fatal(err)
	}
	// Two charges count in the 30-day window, even though only one is in 7 days.
	if _, err := f.s.SavePlan(ctx, f.u, f.plan, "Member", "2", "1.5", 1); err != nil {
		t.Fatal(err)
	}
	third := f.operation(t)
	if err := f.r.Process(ctx, third); err == nil {
		t.Fatal("monthly limit did not block")
	}
	if op := f.state(t, third); op.BlockedReason != "usage_limit" {
		t.Fatalf("monthly block: %+v", op)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.llm_usage SET accounted_at=now()-interval '31 days'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SaveMembership(ctx, f.u, f.u, f.plan, "suspended", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Process(ctx, third); err == nil {
		t.Fatal("suspended membership dispatched")
	}
	if op := f.state(t, third); op.BlockedReason != "membership_suspended" {
		t.Fatalf("suspension: %+v", op)
	}
	if _, err := f.s.SaveMembership(ctx, f.u, f.u, f.plan, "active", 2); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Process(ctx, third); err != nil {
		t.Fatal(err)
	}
	if f.p.calls.Load() != 3 {
		t.Fatalf("calls=%d", f.p.calls.Load())
	}
}

func TestUncertainOutcomeNeverResends(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.p.uncertain = true
	args := f.operation(t)
	if err := f.r.Process(ctx, args); err == nil {
		t.Fatal("uncertain outcome not paused")
	}
	for range 3 {
		_ = f.r.Process(ctx, args)
	}
	if f.p.calls.Load() != 1 {
		t.Fatal("uncertain paid call repeated")
	}
	if f.state(t, args).Status != "needs_review" {
		t.Fatal("uncertain outcome was hidden")
	}
	// Neither elapsed windows nor expired worker leases release an uncertain hold.
	if _, err := f.db.Admin(t).Exec(ctx, "UPDATE platform.llm_attempt SET dispatched_at=now()-interval '40 days',deadline_at=now()-interval '1 day'"); err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.Attempts(ctx, f.u, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID        uuid.UUID `json:"id"`
		Version   int       `json:"version"`
		ErrorCode string    `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ErrorCode != "transport_uncertain" {
		t.Fatalf("attempts=%s", raw)
	}
	// A reconciliation pass while the owner checks the provider must not make
	// their resolution stale.
	if err := f.r.Process(ctx, args); err == nil {
		t.Fatal("unresolved attempt not paused")
	}
	if n, _ := llm.ParseUSD(f.usage(t)["reserved_usd"].(string)); n != f.p.amount {
		t.Fatalf("uncertain hold=%d", n)
	}
	if err := f.s.Resolve(ctx, f.u, rows[0].ID, "0.4", nil, "Confirmed in provider activity", rows[0].Version); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Resolve(ctx, f.u, rows[0].ID, "0.4", nil, "Repeated resolution", rows[0].Version); err == nil {
		t.Fatal("duplicate owner resolution accepted")
	}
	if err := f.r.Process(ctx, args); err != nil {
		t.Fatal(err)
	}
	if f.p.calls.Load() != 1 || f.state(t, args).Status != "failed" {
		t.Fatal("resolved lost result was regenerated")
	}
}

func TestOwnerPermissionsAndTenantIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Values returned by a read must be accepted unchanged by a subsequent edit.
	raw, err := f.s.Plans(ctx, f.u)
	if err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		ID      uuid.UUID `json:"id"`
		Name    string    `json:"name"`
		Week    string    `json:"cap_7d_usd"`
		Month   string    `json:"cap_30d_usd"`
		Version int       `json:"version"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plans: %s %v", raw, err)
	}
	p := plans[0]
	if _, err := f.s.SavePlan(ctx, f.u, p.ID, p.Name, p.Week, p.Month, p.Version); err != nil {
		t.Fatalf("plan money did not round trip: %v", err)
	}
	other, err := f.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Plans(ctx, other); err == nil {
		t.Fatal("member listed platform plans")
	}
	if _, err := f.s.Keys(ctx, other); err == nil {
		t.Fatal("member listed system keys")
	}
	if _, err := f.s.Attempts(ctx, other, nil, 50); err == nil {
		t.Fatal("member listed other usage")
	}
	args := f.operation(t)
	if err := f.db.Store.InTenant(ctx, other, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM llm_operation WHERE id=$1", args.OperationID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("cross-tenant operation visible")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT * FROM platform.system_key", "SELECT * FROM platform.llm_usage", "SELECT platform.settle(NULL,0,NULL,'{}','','provider_response')"} {
		if err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, query); return err }); err == nil {
			t.Fatalf("API role executed %s", query)
		}
	}
	if err := store.SetOwner(ctx, f.db.AdminURL, f.u, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Plans(ctx, f.u); err == nil {
		t.Fatal("revoked owner retained permissions")
	}
}

func TestQueueRollbackAndIdempotency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	key := uuid.NewString()
	a, err := f.q.Enqueue(ctx, f.u, key, "request")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.q.Enqueue(ctx, f.u, key, "request")
	if err != nil || a.ID != b.ID {
		t.Fatalf("idempotency: %+v %v", b, err)
	}
	var jobs int
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*) FROM river.river_job").Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d %v", jobs, err)
	}
	// Concurrent callers with one idempotency key must return one operation.
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := f.q.Enqueue(ctx, f.u, key, "request")
			if err == nil && op.ID != a.ID {
				err = errors.New("duplicate operation")
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDurableDispatchCrashAndLateCorrection(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	args := f.operation(t)
	attempt := uuid.Must(uuid.NewV7())
	q, _ := json.Marshal(llm.Quote{Model: "test/model", Reserved: f.p.amount})
	if err := f.r.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		var raw []byte
		return tx.QueryRow(ctx, "SELECT platform.reserve($1,$2,$3,$4,$5,$6,$7)", attempt, args.OperationID, "test/model", "request", json.RawMessage(q), f.p.amount, time.Now().Add(time.Minute)).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate process death after committing dispatch, before knowing if HTTP ran.
	if _, err := f.db.Admin(t).Exec(ctx, "UPDATE platform.llm_attempt SET deadline_at=now()-interval '1 second' WHERE id=$1", attempt); err != nil {
		t.Fatal(err)
	}
	_ = f.r.Process(ctx, args)
	if f.p.calls.Load() != 0 || f.state(t, args).Status != "needs_review" {
		t.Fatal("crashed dispatch was repeated")
	}
	amount, _ := llm.ParseUSD("0.6")
	settle := func(cost int64) {
		t.Helper()
		if err := f.r.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT platform.settle($1,$2,$3,$4,$5,$6)", attempt, cost, "gen-crash", json.RawMessage(`{}`), "OK", "generation_lookup")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	settle(amount)
	settle(amount)
	corrected, _ := llm.ParseUSD("0.4")
	settle(corrected)
	settle(corrected)
	var entries int
	var total int64
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*),sum(amount) FROM platform.llm_usage WHERE attempt_id=$1", attempt).Scan(&entries, &total); err != nil {
		t.Fatal(err)
	}
	if entries != 2 || total != corrected {
		t.Fatalf("corrections were not idempotent: entries=%d total=%d", entries, total)
	}
}

func TestRiverExecutesAndRecoversAbandonedOperations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	args := f.operation(t)
	// A discarded queue job must not strand its nonterminal domain operation.
	if _, err := f.db.Admin(t).Exec(ctx, "UPDATE river.river_job SET state='discarded',finalized_at=now()"); err != nil {
		t.Fatal(err)
	}
	client, err := f.r.Client(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Recover(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	deadline := time.After(10 * time.Second)
	for {
		if f.state(t, args).Status == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("River operation did not finish")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if f.p.calls.Load() != 1 {
		t.Fatalf("paid calls=%d", f.p.calls.Load())
	}
	var events int
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*) FROM outbox WHERE topic='llm_operation.changed'").Scan(&events); err != nil || events == 0 {
		t.Fatalf("completion event=%d %v", events, err)
	}
}

func TestQueueFailureRollsBackOperation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.db.Admin(t).Exec(ctx, "REVOKE INSERT ON river.river_job FROM zapply_app"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Enqueue(ctx, f.u, "rollback-test", "request"); err == nil {
		t.Fatal("queue failure was hidden")
	}
	var count int
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*) FROM llm_operation WHERE idempotency_key='rollback-test'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphaned operation count=%d %v", count, err)
	}
}

func TestProviderAndMasterKeyRotationPreserveAccounting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	args := f.operation(t)
	f.p.missingCost = true
	if err := f.r.Process(ctx, args); err == nil {
		t.Fatal("missing cost did not retain reservation")
	}
	replacement, err := f.s.AddKey(ctx, f.u, "Replacement", "replacement-provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ActivateKey(ctx, f.u, replacement, true, 1); err != nil {
		t.Fatal(err)
	}
	old := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	fresh := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	rotated, err := secrets.New(`{"v1":"`+old+`","v2":"`+fresh+`"}`, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReencryptCredentials(ctx, f.db.AdminURL, rotated); err != nil {
		t.Fatal(err)
	}
	// All stored credentials are now readable without retaining the old master key.
	onlyNew, err := secrets.New(`{"v2":"`+fresh+`"}`, "v2")
	if err != nil {
		t.Fatal(err)
	}
	f.r.Keys = onlyNew
	if err := f.r.Process(ctx, args); err != nil {
		t.Fatal(err)
	}
	if op := f.state(t, args); op.Status != "completed" || op.Result != "OK" || f.p.calls.Load() != 1 {
		t.Fatalf("rotation lost pending accounting/result: %+v", op)
	}
	var oldKeys int
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*) FROM platform.system_key WHERE encryption_version<>'v2'").Scan(&oldKeys); err != nil || oldKeys != 0 {
		t.Fatalf("old encryption versions=%d %v", oldKeys, err)
	}
}

func TestChargedOutputWithNULIsSaved(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.p.text = "O\x00K"
	args := f.operation(t)
	if err := f.r.Process(ctx, args); err != nil {
		t.Fatal(err)
	}
	if op := f.state(t, args); op.Status != "completed" || op.Result != "OK" {
		t.Fatalf("charged output lost: %+v", op)
	}
}

func TestKeyFailureStopsRetrying(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.r.Keys = nil
	args := f.operation(t)
	for range 10 {
		_ = f.r.Process(ctx, args)
	}
	if f.p.calls.Load() != 0 || f.state(t, args).Status != "failed" {
		t.Fatalf("status=%s calls=%d", f.state(t, args).Status, f.p.calls.Load())
	}
	var n int
	if err := f.db.Admin(t).QueryRow(ctx, "SELECT count(*) FROM platform.llm_attempt").Scan(&n); err != nil || n != 5 {
		t.Fatalf("attempts=%d err=%v", n, err)
	}
}
