package posting

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// readPage repeats a line from its body in the footer, so an answer that ends
// the description on that line names a sentence the page holds twice.
var readPage = "Apply now\n\nBuild models that run on device every day.\n" +
	"Our team owns the data pipeline and the APIs behind it.\n" +
	strings.Repeat("The team reviews each change together and pairs on the hard parts of the platform. ", 3) + "\n" +
	"We ship a fast API for partners.\n\nApply now\n\nSimilar: Our team owns the data pipeline and the APIs behind it."

const (
	readStart  = "Build models that run on device every day"
	readRepeat = "Our team owns the data pipeline and the APIs behind it"
	readUnique = "We ship a fast API for partners"
)

// readAnswer is the extraction's answer naming the description's first and
// last sentence.
func readAnswer(end string) string {
	a, _ := json.Marshal(readStart)
	z, _ := json.Marshal(end)
	return `{"description_start":{"type":"text","value":` + string(a) + `,"confidence":0.9},"description_end":{"type":"text","value":` + string(z) + `,"confidence":0.9}}`
}

// readHarness runs a posting read the way the API and the worker do: the
// read starts inside the API's tenant transaction, and each attempt finishes
// inside the worker's.
type readHarness struct {
	t        *testing.T
	ctx      context.Context
	app, ws  *store.Store
	start    *Reads
	terminal *Reads
	user     uuid.UUID
}

func newReadHarness(t *testing.T) *readHarness {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	u, err := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "reads"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, db.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ws.Close)
	appQueue, err := worker.NewQueue(db.Store)
	if err != nil {
		t.Fatal(err)
	}
	workerQueue, err := worker.NewQueue(ws)
	if err != nil {
		t.Fatal(err)
	}
	return &readHarness{t: t, ctx: ctx, app: db.Store, ws: ws, start: NewReads(appQueue), terminal: NewReads(workerQueue), user: u}
}

func (h *readHarness) begin() uuid.UUID {
	h.t.Helper()
	var id uuid.UUID
	if err := h.app.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		var err error
		id, err = h.start.Start(h.ctx, tx, h.user, "liquid/d1.2", readPage)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// finish completes an attempt with result, running the terminal hook in the
// same transaction, as the worker's settlement does.
func (h *readHarness) finish(id uuid.UUID, result string) {
	h.t.Helper()
	if err := h.ws.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		handled, err := h.terminal.OnTerminal(h.ctx, tx, worker.Operation{ID: id, Kind: "extraction", UserID: h.user, Status: "completed", Result: result})
		if err == nil && !handled {
			err = errors.New("a read's attempt was not handled")
		}
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *readHarness) latest(id uuid.UUID) uuid.UUID {
	h.t.Helper()
	var got uuid.UUID
	if err := h.app.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		var err error
		got, err = LatestAttempt(h.ctx, tx, h.user, id)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return got
}

// row is an attempt's stored problems and the attempt that replaced it.
func (h *readHarness) row(id uuid.UUID) ([]string, *uuid.UUID) {
	h.t.Helper()
	var problems []string
	var next *uuid.UUID
	if err := h.app.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT problems,next_operation_id FROM posting_read WHERE operation_id=$1`, id).Scan(&problems, &next)
	}); err != nil {
		h.t.Fatal(err)
	}
	return problems, next
}

func (h *readHarness) request(id uuid.UUID) string {
	h.t.Helper()
	var body []byte
	if err := h.app.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT body FROM model_request WHERE operation_id=$1`, id).Scan(&body)
	}); err != nil {
		h.t.Fatal(err)
	}
	return string(body)
}

func (h *readHarness) attempts() int {
	h.t.Helper()
	var n int
	if err := h.app.InTenant(h.ctx, h.user, func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM model_operation WHERE kind='extraction'`).Scan(&n)
	}); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestReadRetriesAnAnswerThePageDoesNotConfirm(t *testing.T) {
	h := newReadHarness(t)
	first := h.begin()
	h.finish(first, readAnswer(readRepeat))

	second := h.latest(first)
	if second == first {
		t.Fatal("an end marker repeated in the page was not retried")
	}
	problems, next := h.row(first)
	if next == nil || *next != second || len(problems) != 1 || !strings.Contains(problems[0], "appears 2 times") {
		t.Fatalf("first attempt: next %v, problems %q", next, problems)
	}
	if body := h.request(second); !strings.Contains(body, "appears 2 times") {
		t.Errorf("retry does not carry the rejection: %s", body)
	}

	h.finish(second, readAnswer(readUnique))
	if got := h.latest(first); got != second {
		t.Errorf("a confirmed answer was retried again: %v", got)
	}
	if problems, next := h.row(second); next != nil || len(problems) != 0 {
		t.Errorf("confirmed attempt: next %v, problems %q", next, problems)
	}
	if n := h.attempts(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestReadStopsAfterThreeAttempts(t *testing.T) {
	h := newReadHarness(t)
	first := h.begin()
	h.finish(first, readAnswer(readRepeat))
	second := h.latest(first)
	h.finish(second, readAnswer(readRepeat))
	third := h.latest(first)
	if third == first || third == second {
		t.Fatalf("no third attempt: %v", third)
	}
	h.finish(third, readAnswer("Apply now"))

	if got := h.latest(first); got != third {
		t.Errorf("a fourth attempt was queued: latest %v, want %v", got, third)
	}
	if problems, next := h.row(third); next != nil || len(problems) != 1 || !strings.Contains(problems[0], "has 8 letters") {
		t.Errorf("last attempt: next %v, problems %q", next, problems)
	}
	if n := h.attempts(); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}

	// A replayed terminal event for an attempt already replaced queues nothing.
	h.finish(first, readAnswer(readRepeat))
	if n := h.attempts(); n != 3 {
		t.Errorf("replay queued an attempt: %d operations", n)
	}
}
