package mail_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/mail"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fakeMail is an in-memory mailbox standing in for Gmail.
type fakeMail struct {
	mu        sync.Mutex
	address   string
	revoked   bool
	messages  map[string]mail.Message
	recent    []string
	added     []string
	calls     map[string]int
	stopped   int
	revokes   []string
	pageSize  int
	rateAfter int
}

func newFakeMail(address string) *fakeMail {
	return &fakeMail{address: address, messages: map[string]mail.Message{}, calls: map[string]int{}}
}
func (f *fakeMail) add(id, from, subject, snippet string) {
	f.addAt(id, from, subject, snippet, time.Now().UTC())
}
func (f *fakeMail) addAt(id, from, subject, snippet string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = mail.Message{ID: id, ThreadID: "t" + id, ReceivedAt: at, From: from, Subject: subject, Snippet: snippet}
	f.added = append(f.added, id)
}
func (f *fakeMail) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}
func (f *fakeMail) hit(name string) { f.calls[name]++ }

func (f *fakeMail) Name() string { return "google" }
func (f *fakeMail) AuthURL(state, challenge string) string {
	return "https://consent.example/?state=" + state
}
func (f *fakeMail) Exchange(context.Context, string, string) (mail.Tokens, error) {
	return mail.Tokens{Refresh: "refresh-secret", Access: "access-1", Expires: time.Now().Add(time.Hour)}, nil
}
func (f *fakeMail) Refresh(_ context.Context, refresh string) (mail.Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("refresh")
	if f.revoked {
		return mail.Tokens{}, mail.ErrRevoked
	}
	if refresh != "refresh-secret" {
		return mail.Tokens{}, errors.New("wrong refresh token")
	}
	return mail.Tokens{Refresh: refresh, Access: "access-2", Expires: time.Now().Add(time.Hour)}, nil
}
func (f *fakeMail) Profile(context.Context, string) (mail.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("profile")
	return mail.Profile{Address: f.address, HistoryID: itoa(len(f.added))}, nil
}
func (f *fakeMail) Watch(context.Context, string) (mail.Watch, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("watch")
	return mail.Watch{HistoryID: "c0", Expires: time.Now().Add(7 * 24 * time.Hour)}, true, nil
}
func (f *fakeMail) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	return nil
}
func (f *fakeMail) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes = append(f.revokes, token)
	return nil
}
func (f *fakeMail) History(_ context.Context, _, start, page string) (mail.HistoryPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("history")
	if start == "expired" {
		return mail.HistoryPage{}, mail.ErrHistoryExpired
	}
	from := atoi(start)
	offset := atoi(page)
	rest := f.added[from+offset:]
	out := mail.HistoryPage{HistoryID: itoa(len(f.added))}
	if f.pageSize > 0 && len(rest) > f.pageSize {
		rest = rest[:f.pageSize]
		out.Next = itoa(offset + f.pageSize)
	}
	out.MessageIDs = append([]string(nil), rest...)
	out.LastID = itoa(from + offset + len(rest))
	return out, nil
}
func (f *fakeMail) Recent(context.Context, string, int, int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("recent")
	return append([]string(nil), f.recent...), nil
}
func (f *fakeMail) Get(_ context.Context, _, id string) (mail.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hit("get")
	if f.rateAfter > 0 && f.calls["get"] == f.rateAfter {
		return mail.Message{}, mail.ErrRateLimited
	}
	m, ok := f.messages[id]
	if !ok {
		return mail.Message{}, mail.ErrMessageGone
	}
	return m, nil
}
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
func atoi(s string) int {
	var n int
	if s != "" {
		_ = json.Unmarshal([]byte(s), &n)
	}
	return n
}

// model answers mail questions from the subject line.
type model struct{ calls int }

func (p *model) Quote(context.Context) (modelapi.Quote, error) {
	return modelapi.Quote{}, errors.New("chat")
}
func (p *model) Generate(context.Context, string, modelapi.Quote) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("chat")
}
func (p *model) Lookup(context.Context, string, string) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("lookup unsupported")
}
func (p *model) DecisionLookupSupported(string) bool { return false }
func (p *model) DecisionSheet(_ context.Context, name string) (billing.Sheet, error) {
	return billing.Sheet{Provider: "fake", Model: name, InputRate: 1, Context: 100000, MaxRequestBytes: systemone.RequestBudget,
		Prior: billing.Tokenization{TokensPerKB: 1000}, FetchedAt: time.Now()}, nil
}
func (p *model) Decide(_ context.Context, key string, r systemone.Request, _ modelapi.Quote) (modelapi.Result, error) {
	p.calls++
	var state struct{ Email struct{ Subject string } }
	_ = json.Unmarshal(r.State, &state)
	subject := strings.ToLower(state.Email.Subject)
	answers := map[string]systemone.Answer{}
	conf := .9
	for k, q := range r.Questions {
		var options map[string]string
		_ = json.Unmarshal(q.Criteria, &options)
		chosen := "other"
		if _, isCategory := options["received"]; isCategory {
			for _, c := range []string{"interview", "rejection", "offer", "assessment", "received"} {
				if strings.Contains(subject, c) {
					chosen = c
					break
				}
			}
		} else {
			chosen = "none"
			for id, label := range options {
				if id != "none" && strings.Contains(subject, strings.ToLower(strings.Split(label, " at ")[1])) {
					chosen = id
				}
			}
		}
		a := systemone.Answer{Type: "choice", Choice: chosen, Confidence: &conf, Probabilities: map[string]float64{}}
		for o := range options {
			a.Probabilities[o] = 0
		}
		a.Probabilities[chosen] = 1
		answers[k] = a
	}
	amount := int64(100)
	return modelapi.Result{GenerationID: "gen-" + uuid.NewString(), Text: string(systemone.Raw(answers)), Amount: &amount, Usage: systemone.Raw(map[string]int{"input_tokens": 5})}, nil
}

type fixture struct {
	db      *testdb.DB
	u       uuid.UUID
	fake    *fakeMail
	api     *mail.Service // the API process
	ws      *mail.Service // the worker process
	runtime *worker.Runtime
	model   *model
	job     jobs.Job
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
	aq, e := worker.NewQueue(d.Store)
	if e != nil {
		t.Fatal(e)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := newFakeMail("me@example.com")
	ws := mail.New(workerStore, wq, ring, fake, true, time.Hour, log)
	apiSvc := mail.New(d.Store, aq, ring, fake, true, time.Hour, log)
	m := &model{}
	runtime := &worker.Runtime{Store: workerStore, Keys: ring, Provider: m, Log: log,
		OnTerminal: func(ctx context.Context, tx pgx.Tx, op worker.Operation) error {
			_, err := ws.OnTerminal(ctx, tx, op)
			return err
		}}
	job, e := jobs.NewService(d.Store).CreateJob(ctx, u, jobs.JobInput{Title: "Engineer", Company: "Acme"})
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{d, u, fake, apiSvc, ws, runtime, m, job}
}

// connect runs the consent flow against the fake provider.
func (f *fixture) connect(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	url, e := f.api.StartConnect(ctx, f.u)
	if e != nil {
		t.Fatal(e)
	}
	state := url[strings.Index(url, "state=")+len("state="):]
	a, e := f.api.CompleteConnect(ctx, f.u, state, "code")
	if e != nil {
		t.Fatal(e)
	}
	return a.ID
}

// drain finishes every open mail request, as the worker would.
func (f *fixture) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var ops []uuid.UUID
	e := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT m.operation_id FROM mail_operation m JOIN model_operation o ON o.user_id=m.user_id AND o.id=m.operation_id WHERE NOT m.ingested AND o.status IN ('pending','blocked')`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if e = rows.Scan(&id); e != nil {
				return e
			}
			ops = append(ops, id)
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

func (f *fixture) messageIDs(t *testing.T) map[string]uuid.UUID {
	t.Helper()
	out := map[string]uuid.UUID{}
	rows, e := f.db.Admin(t).Query(context.Background(), `SELECT provider_message_id,id FROM mail_message`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var id uuid.UUID
		if e = rows.Scan(&p, &id); e != nil {
			t.Fatal(e)
		}
		out[p] = id
	}
	return out
}

type row struct {
	State, Category        string
	From, Subject, Snippet *string
	Job                    *uuid.UUID
}

func (f *fixture) row(t *testing.T, id uuid.UUID) row {
	t.Helper()
	var r row
	var cat *string
	e := f.db.Admin(t).QueryRow(context.Background(), `SELECT state,category,from_address,subject,snippet,job_id FROM mail_message WHERE id=$1`, id).Scan(&r.State, &cat, &r.From, &r.Subject, &r.Snippet, &r.Job)
	if e != nil {
		t.Fatal(e)
	}
	if cat != nil {
		r.Category = *cat
	}
	return r
}

// classifyAll runs classification until every message settles.
func (f *fixture) classifyAll(t *testing.T, ids map[string]uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for round := 0; round < 6; round++ {
		for _, id := range ids {
			// Over the member's open-request limit a message waits for the next round.
			if e := f.ws.Classify(ctx, f.u, id); e != nil && !strings.Contains(e.Error(), "JobSnooze") {
				t.Fatal(e)
			}
		}
		f.drain(t)
	}
}

func TestSyncClassifyRetentionAndScrub(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.add("m1", "Recruiter <jobs@acme.test>", "Interview with Acme", "Can you talk Tuesday?")
	f.fake.add("m2", "News <news@letters.test>", "Weekly newsletter", "Ten tips")
	f.fake.add("m3", "HR <hr@other.test>", "Your application: rejection notice", "We went another way")
	f.fake.add("m4", "Me <me@example.com>", "Note to self", "buy milk")
	f.fake.recent = []string{"m1", "m2", "m3", "m4"}

	// The first sync backfills; the self-sent note is skipped.
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	ids := f.messageIDs(t)
	if len(ids) != 3 || f.fake.count("recent") != 1 {
		t.Fatalf("messages %v, recent calls %d", ids, f.fake.count("recent"))
	}
	var cursor *string
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT history_id FROM mail_account WHERE id=$1`, acct).Scan(&cursor); e != nil || cursor == nil || *cursor != "4" {
		t.Fatalf("cursor %v %v", cursor, e)
	}
	// A repeat sync with nothing new reads nothing and adds nothing.
	gets := f.fake.count("get")
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	if len(f.messageIDs(t)) != 3 || f.fake.count("get") != gets || f.fake.count("history") != 1 {
		t.Fatalf("repeat sync read mail again: gets %d history %d", f.fake.count("get"), f.fake.count("history"))
	}
	// A new message arrives; incremental sync picks up only it.
	f.fake.add("m5", "Acme <talent@acme.test>", "Offer from Acme", "We are delighted")
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	ids = f.messageIDs(t)
	if len(ids) != 4 || f.fake.count("get") != gets+1 {
		t.Fatalf("incremental sync: %d messages, %d gets", len(ids), f.fake.count("get"))
	}

	f.classifyAll(t, ids)

	for _, c := range []struct {
		id, category string
		kept, linked bool
	}{{"m1", "interview", true, true}, {"m2", "other", false, false}, {"m3", "rejection", true, false}, {"m5", "offer", true, true}} {
		r := f.row(t, ids[c.id])
		if r.State != "classified" || r.Category != c.category {
			t.Fatalf("%s: %+v", c.id, r)
		}
		if kept := r.From != nil && r.Subject != nil && r.Snippet != nil; kept != c.kept {
			t.Fatalf("%s kept content = %v, want %v", c.id, kept, c.kept)
		}
		if (r.Job != nil && *r.Job == f.job.ID) != c.linked {
			t.Fatalf("%s job link = %v, want linked %v", c.id, r.Job, c.linked)
		}
	}
	// Every request body that carried mail content has been blanked.
	var leaked int
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM model_request r JOIN mail_operation m ON m.operation_id=r.operation_id WHERE r.body::text ~* '(weekly newsletter|interview with acme|we went another way|ten tips|talk tuesday|jobs@acme)'`).Scan(&leaked); e != nil || leaked != 0 {
		t.Fatalf("mail content left in %d model requests (%v)", leaked, e)
	}
	var unscrubbed int
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM model_request r JOIN mail_operation m ON m.operation_id=r.operation_id WHERE r.body->'state' <> '{}'::jsonb`).Scan(&unscrubbed); e != nil || unscrubbed != 0 {
		t.Fatalf("%d finished mail requests still hold a state (%v)", unscrubbed, e)
	}
	// The relevant list shows only relevant mail.
	items, e := f.api.Messages(ctx, f.u, "", nil, nil, 50)
	if e != nil || len(items) != 3 {
		t.Fatalf("messages %v %v", items, e)
	}
	byJob, e := f.api.Messages(ctx, f.u, "", &f.job.ID, nil, 50)
	if e != nil || len(byJob) != 2 {
		t.Fatalf("by job %v %v", byJob, e)
	}
	// Replaying a finished request's completion is harmless.
	var op worker.Operation
	var raw []byte
	if e = f.db.Admin(t).QueryRow(ctx, `SELECT to_jsonb(o) FROM model_operation o JOIN mail_operation m ON m.operation_id=o.id LIMIT 1`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(raw, &op)
	if e = f.ws.Store().InTenant(ctx, f.u, func(tx pgx.Tx) error {
		handled, err := f.ws.OnTerminal(ctx, tx, op)
		if !handled {
			t.Error("replay not handled")
		}
		return err
	}); e != nil {
		t.Fatal(e)
	}
	calls := f.model.calls
	f.classifyAll(t, ids)
	if f.model.calls != calls {
		t.Fatal("classified messages were sent to the model again")
	}
}

func TestDatabaseEnforcesRetention(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.add("m1", "a@b.test", "Hello", "preview")
	f.fake.recent = []string{"m1"}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	id := f.messageIDs(t)["m1"]
	_, e := f.db.Admin(t).Exec(ctx, `UPDATE mail_message SET category='other',state='classified',classified_at=now() WHERE id=$1`, id)
	if e == nil {
		t.Fatal("an 'other' message kept its sender, subject and snippet")
	}
	_, e = f.db.Admin(t).Exec(ctx, `UPDATE mail_message SET state='failed' WHERE id=$1`, id)
	if e == nil {
		t.Fatal("a failed message kept its content")
	}
}

func TestCredentialsAreSealedAndTenantsIsolated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	// The runtime role cannot read the token table, and what is stored is not the token.
	if _, e := f.db.Store.Pool().Exec(ctx, `SELECT * FROM platform.mail_credential`); e == nil {
		t.Fatal("the app role can read refresh tokens")
	}
	var ct []byte
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT ciphertext FROM platform.mail_credential WHERE account_id=$1`, acct).Scan(&ct); e != nil || bytes.Contains(ct, []byte("refresh-secret")) {
		t.Fatalf("token stored in the clear (%v)", e)
	}
	// Another member sees nothing and cannot touch the account.
	other, e := f.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "other"})
	if e != nil {
		t.Fatal(e)
	}
	if list, e := f.api.Accounts(ctx, other); e != nil || len(list) != 0 {
		t.Fatalf("other member sees %v %v", list, e)
	}
	if _, e = f.api.SetPaused(ctx, other, acct, true); !errors.Is(e, library.ErrNotFound) {
		t.Fatalf("other member paused the account: %v", e)
	}
	if e = f.api.Disconnect(ctx, other, acct); !errors.Is(e, library.ErrNotFound) {
		t.Fatalf("other member disconnected the account: %v", e)
	}
	// The same Gmail address cannot be connected twice.
	url, e := f.api.StartConnect(ctx, other)
	if e != nil {
		t.Fatal(e)
	}
	state := url[strings.Index(url, "state=")+len("state="):]
	// Someone else's consent link cannot attach a mailbox to this account, and
	// trying does not use the state up.
	if _, e = f.api.CompleteConnect(ctx, f.u, state, "code"); !errors.Is(e, mail.ErrBadState) {
		t.Fatalf("a forwarded consent link was accepted: %v", e)
	}
	_, e = f.api.CompleteConnect(ctx, other, state, "code")
	var conflict *library.ConflictError
	if !errors.As(e, &conflict) || len(f.fake.revokes) != 1 {
		t.Fatalf("second connect: %v, revokes %v", e, f.fake.revokes)
	}
	// An OAuth state works once.
	if _, e = f.api.CompleteConnect(ctx, other, state, "code"); !errors.Is(e, mail.ErrBadState) {
		t.Fatalf("state reused: %v", e)
	}
}

func TestPausedAccountIsNotRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	if _, e := f.api.SetPaused(ctx, f.u, acct, true); e != nil {
		t.Fatal(e)
	}
	profiles := f.fake.count("profile")
	if e := f.ws.Sync(ctx, f.u, acct); e != nil || f.fake.count("profile") != profiles || f.fake.count("history") != 0 {
		t.Fatalf("paused account was read (%v)", e)
	}
	a, e := f.api.SetPaused(ctx, f.u, acct, false)
	if e != nil || a.Status != "active" {
		t.Fatalf("resume: %v %+v", e, a)
	}
}

func TestRevokedAccessNeedsReconnect(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	// Drop the cached token so the worker must refresh.
	f.fake.revoked = true
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatalf("a revoked grant should not be retried: %v", e)
	}
	var status, msg string
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT status,last_error FROM mail_account WHERE id=$1`, acct).Scan(&status, &msg); e != nil || status != "needs_reconnect" || msg == "" {
		t.Fatalf("status %q %q %v", status, msg, e)
	}
	// Reconnecting restores it and keeps the account.
	f.fake.revoked = false
	if again := f.connect(t); again != acct {
		t.Fatalf("reconnect made a new account %s != %s", again, acct)
	}
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT status FROM mail_account WHERE id=$1`, acct).Scan(&status); e != nil || status != "active" {
		t.Fatalf("status after reconnect %q %v", status, e)
	}
	// Disconnecting stops the watch, revokes the grant and deletes the token.
	if e := f.api.Disconnect(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	if e := f.ws.Teardown(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM platform.mail_credential WHERE account_id=$1`, acct).Scan(&n); e != nil || n != 0 {
		t.Fatalf("credential kept after disconnect (%d, %v)", n, e)
	}
	if f.fake.stopped != 1 || len(f.fake.revokes) == 0 || f.fake.revokes[len(f.fake.revokes)-1] != "refresh-secret" {
		t.Fatalf("stop %d, revokes %v", f.fake.stopped, f.fake.revokes)
	}
}

func TestPushQueuesSyncAndExpiredCursorResyncs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	jobsOf := func(kind string) int {
		var n int
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind=$1`, kind).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	before := jobsOf("mail_sync")
	if e := f.api.HandlePush(ctx, "nobody@example.com", "10"); e != nil || jobsOf("mail_sync") != before {
		t.Fatalf("unknown mailbox queued work (%v)", e)
	}
	if e := f.api.HandlePush(ctx, "ME@example.com", "10"); e != nil || jobsOf("mail_sync") != before+1 {
		t.Fatalf("push did not queue a sync (%v)", e)
	}
	if e := f.api.HandlePush(ctx, "me@example.com", "10"); e != nil || jobsOf("mail_sync") != before+1 {
		t.Fatalf("a repeated notification queued a second sync (%v)", e)
	}
	// A cursor Gmail no longer has falls back to a bounded re-list.
	if _, e := f.db.Admin(t).Exec(ctx, `UPDATE mail_account SET history_id='expired' WHERE id=$1`, acct); e != nil {
		t.Fatal(e)
	}
	f.fake.add("m1", "x@y.test", "Received your application", "thanks")
	f.fake.recent = []string{"m1"}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	if f.fake.count("recent") == 0 || len(f.messageIDs(t)) != 1 {
		t.Fatalf("expired cursor: recent %d, messages %d", f.fake.count("recent"), len(f.messageIDs(t)))
	}
}

func TestKeyRotationReencryptsMailTokens(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	key := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	rotated, e := secrets.New(`{"v1":"`+key(7)+`","v2":"`+key(9)+`"}`, "v2")
	if e != nil {
		t.Fatal(e)
	}
	if e = store.ReencryptCredentials(ctx, f.db.AdminURL, rotated); e != nil {
		t.Fatal(e)
	}
	var version string
	if e = f.db.Admin(t).QueryRow(ctx, `SELECT encryption_version FROM platform.mail_credential WHERE account_id=$1`, acct).Scan(&version); e != nil || version != "v2" {
		t.Fatalf("token still sealed with %q (%v)", version, e)
	}
	// A worker that has only the new key can still read the mailbox.
	onlyNew, e := secrets.New(`{"v2":"`+key(9)+`"}`, "v2")
	if e != nil {
		t.Fatal(e)
	}
	workerStore, e := store.Open(ctx, f.db.WorkerURL)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(workerStore.Close)
	q, e := worker.NewQueue(workerStore)
	if e != nil {
		t.Fatal(e)
	}
	fresh := mail.New(workerStore, q, onlyNew, f.fake, true, time.Hour, nil)
	refreshes := f.fake.count("refresh")
	if e = fresh.Sync(ctx, f.u, acct); e != nil || f.fake.count("refresh") != refreshes+1 {
		t.Fatalf("sync with the rotated ring: %v, refreshes %d -> %d", e, refreshes, f.fake.count("refresh"))
	}
}

func TestRecoverQueuesDueWorkOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.add("m1", "a@b.test", "Hello", "preview")
	f.fake.recent = []string{"m1"}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	runtime := &worker.Runtime{Store: f.ws.Store(), AddWorkers: f.ws.AddWorkers}
	client, e := runtime.Client(1)
	if e != nil {
		t.Fatal(e)
	}
	count := func(kind string) int {
		var n int
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind=$1`, kind).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Synced a moment ago: nothing to reconcile. The watch was set by the sync.
	if e = f.ws.Recover(ctx, client); e != nil {
		t.Fatal(e)
	}
	if count("mail_watch") != 0 || count("mail_classify") != 1 {
		t.Fatalf("recover queued work for a fresh account: watch %d classify %d", count("mail_watch"), count("mail_classify"))
	}
	// An account quiet for longer than the interval is synced; an expiring watch is renewed.
	if _, e = f.db.Admin(t).Exec(ctx, `UPDATE mail_account SET last_synced_at=now()-interval '3 hours',watch_expires_at=now()+interval '1 hour' WHERE id=$1`, acct); e != nil {
		t.Fatal(e)
	}
	syncs := count("mail_sync")
	for i := 0; i < 2; i++ { // the second pass must not add duplicates
		if e = f.ws.Recover(ctx, client); e != nil {
			t.Fatal(e)
		}
	}
	if count("mail_sync") != syncs+1 || count("mail_watch") != 1 {
		t.Fatalf("recover: syncs %d -> %d, watches %d", syncs, count("mail_sync"), count("mail_watch"))
	}
	// A waiting message whose job was lost is woken once per interval.
	if _, e = f.db.Admin(t).Exec(ctx, `UPDATE platform.mail_work_index SET updated_at=now()-interval '1 hour'`); e != nil {
		t.Fatal(e)
	}
	classify := count("mail_classify")
	for i := 0; i < 2; i++ {
		if e = f.ws.Recover(ctx, client); e != nil {
			t.Fatal(e)
		}
	}
	if count("mail_classify") != classify+1 {
		t.Fatalf("classify jobs %d -> %d", classify, count("mail_classify"))
	}
	// Without push there are no watches to renew.
	var watches int
	nopush := mail.New(f.ws.Store(), nil, nil, f.fake, false, time.Hour, nil)
	if _, e = f.db.Admin(t).Exec(ctx, `DELETE FROM river.river_job WHERE kind='mail_watch'`); e != nil {
		t.Fatal(e)
	}
	if e = nopush.Recover(ctx, client); e != nil {
		t.Fatal(e)
	}
	if watches = count("mail_watch"); watches != 0 {
		t.Fatalf("watch queued with push off: %d", watches)
	}
}

func TestSyncResumesAfterQuotaError(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.add("first", "a@example.test", "one", "preview")
	f.fake.add("second", "b@example.test", "two", "preview")
	f.fake.recent = []string{"first", "second"}
	f.fake.rateAfter = 2
	if e := f.ws.Sync(ctx, f.u, acct); e == nil || !strings.Contains(e.Error(), "JobSnooze") {
		t.Fatalf("expected retry, got %v", e)
	}
	if n := len(f.messageIDs(t)); n != 1 {
		t.Fatalf("saved %d messages before quota error", n)
	}
	var message string
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT last_error FROM mail_account WHERE id=$1`, acct).Scan(&message); e != nil || !strings.Contains(message, "limiting") {
		t.Fatalf("status %q: %v", message, e)
	}
	if _, e := f.db.Admin(t).Exec(ctx, `UPDATE mail_account SET sync_retry_at=now()-interval '1 second' WHERE id=$1`, acct); e != nil {
		t.Fatal(e)
	}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	if n := len(f.messageIDs(t)); n != 2 || f.fake.count("get") != 3 {
		t.Fatalf("saved %d messages, made %d reads", n, f.fake.count("get"))
	}
}

func TestBacklogIsReadInBatchesWithoutSkipping(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	if e := f.ws.Sync(ctx, f.u, acct); e != nil { // bootstrap: nothing yet
		t.Fatal(e)
	}
	for i := 0; i < 250; i++ {
		f.fake.add("b"+itoa(i), "x@y.test", "Update "+itoa(i), "preview")
	}
	f.fake.pageSize = 60
	cursor := func() string {
		var c string
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT history_id FROM mail_account WHERE id=$1`, acct).Scan(&c); e != nil {
			t.Fatal(e)
		}
		return c
	}
	syncs := func() int {
		var n int
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind='mail_sync' AND args->>'tick' LIKE 'continue:%'`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Exhaust the shared budget repeatedly, then allow the next window.
	for i := 0; i < 10 && len(f.messageIDs(t)) < 250; i++ {
		e := f.ws.Sync(ctx, f.u, acct)
		if e != nil && !strings.Contains(e.Error(), "JobSnooze") {
			t.Fatal(e)
		}
		if _, e = f.db.Admin(t).Exec(ctx, `UPDATE mail_account SET sync_window_start=now()-interval '66 seconds' WHERE id=$1`, acct); e != nil {
			t.Fatal(e)
		}
	}
	if n := len(f.messageIDs(t)); n != 250 || cursor() != "250" || syncs() == 0 {
		t.Fatalf("after catching up: %d messages, cursor %s, follow-ups %d", n, cursor(), syncs())
	}

}

func TestUnstorableMailDoesNotStopTheSync(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	f.fake.add("bad\x00id", "x@y.test", "cannot store this id", "preview")
	f.fake.add("nul", "x@y.test", "has a\x00NUL", "bad \xff bytes")
	f.fake.add("long", "x@y.test", strings.Repeat("s", 1500), "a row the database refuses")
	f.fake.add("fine", "x@y.test", "Interview", "ok")
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatalf("one bad message stopped the sync: %v", e)
	}
	ids := f.messageIDs(t)
	if len(ids) != 2 || ids["fine"] == uuid.Nil || ids["nul"] == uuid.Nil {
		t.Fatalf("stored %v", ids)
	}
	r := f.row(t, ids["nul"])
	if r.Subject == nil || *r.Subject != "has aNUL" || r.Snippet == nil || *r.Snippet != "bad  bytes" {
		t.Fatalf("not cleaned: %+v", r)
	}
	var cursor string
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT history_id FROM mail_account WHERE id=$1`, acct).Scan(&cursor); e != nil || cursor != "4" {
		t.Fatalf("cursor %q %v: the bad message would be fetched again", cursor, e)
	}
}

func TestMailOlderThanThirtyDaysIsNeverRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.addAt("old", "x@y.test", "Interview long ago", "p", time.Now().AddDate(0, 0, -45))
	f.fake.addAt("new", "x@y.test", "Interview this week", "p", time.Now().AddDate(0, 0, -3))
	f.fake.recent = []string{"new", "old"}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	if ids := f.messageIDs(t); len(ids) != 1 || ids["new"] == uuid.Nil {
		t.Fatalf("stored %v", ids)
	}
}

func TestPausedAndDisconnectedMailboxesSendNothingToTheModel(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	f.fake.add("m1", "a@b.test", "Interview with Acme", "p")
	f.fake.add("m2", "a@b.test", "Offer from Acme", "p")
	f.fake.recent = []string{"m1", "m2"}
	if e := f.ws.Sync(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	ids := f.messageIDs(t)
	ops := func() int {
		var n int
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM mail_operation`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	classify := func() int {
		var n int
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind='mail_classify'`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Paused after reading: the waiting mail is not classified.
	if _, e := f.api.SetPaused(ctx, f.u, acct, true); e != nil {
		t.Fatal(e)
	}
	for _, id := range ids {
		if e := f.ws.Classify(ctx, f.u, id); e != nil {
			t.Fatal(e)
		}
	}
	f.drain(t)
	if ops() != 0 || f.model.calls != 0 {
		t.Fatalf("paused mailbox sent %d requests, model called %d times", ops(), f.model.calls)
	}
	// Resuming wakes the waiting messages.
	before := classify()
	if _, e := f.api.SetPaused(ctx, f.u, acct, false); e != nil {
		t.Fatal(e)
	}
	if classify() != before+2 {
		t.Fatalf("resume queued %d classify jobs, want 2", classify()-before)
	}
	// Disconnected: waiting mail is dropped, content included.
	if e := f.api.Disconnect(ctx, f.u, acct); e != nil {
		t.Fatal(e)
	}
	for _, id := range ids {
		if e := f.ws.Classify(ctx, f.u, id); e != nil {
			t.Fatal(e)
		}
		if r := f.row(t, id); r.State != "failed" || r.Subject != nil || r.Snippet != nil || r.From != nil {
			t.Fatalf("disconnected mailbox kept mail: %+v", r)
		}
	}
	if ops() != 0 || f.model.calls != 0 {
		t.Fatal("a disconnected mailbox reached the model")
	}
}

func TestOnlyTheWorkerWritesMessages(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for table, denied := range map[string][]string{
		"mail_message":   {"INSERT", "UPDATE", "DELETE"},
		"mail_operation": {"INSERT", "UPDATE", "DELETE"},
		"mail_account":   {"DELETE"},
	} {
		for _, priv := range denied {
			var ok bool
			if e := f.db.Admin(t).QueryRow(ctx, `SELECT has_table_privilege('zapply_app',$1,$2)`, table, priv).Scan(&ok); e != nil || ok {
				t.Errorf("zapply_app can %s %s (%v)", priv, table, e)
			}
		}
	}
	for _, c := range []struct{ table, priv string }{{"mail_message", "INSERT"}, {"mail_account", "UPDATE"}} {
		var ok bool
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT has_table_privilege('zapply_worker',$1,$2)`, c.table, c.priv).Scan(&ok); e != nil || !ok {
			t.Errorf("zapply_worker cannot %s %s (%v)", c.priv, c.table, e)
		}
	}
}
