package mail_test

import (
	"context"
	"errors"
	"testing"

	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/mail"
	"github.com/google/uuid"
)

// receive syncs one new message and classifies it to the end.
func (f *fixture) receive(t *testing.T, acct uuid.UUID, id, subject string) uuid.UUID {
	t.Helper()
	f.fake.add(id, "Talent <talent@acme.test>", subject, "Hello")
	f.fake.recent = append(f.fake.recent, id) // read by the first, backfilling sync
	if e := f.ws.Sync(context.Background(), f.u, acct); e != nil {
		t.Fatal(e)
	}
	ids := f.messageIDs(t)
	f.classifyAll(t, map[string]uuid.UUID{id: ids[id]})
	return ids[id]
}

func (f *fixture) statusName(t *testing.T) string {
	t.Helper()
	var name string
	if e := f.db.Admin(t).QueryRow(context.Background(), `SELECT st.name FROM job j JOIN job_status st ON st.id=j.status_id WHERE j.id=$1`, f.job.ID).Scan(&name); e != nil {
		t.Fatal(e)
	}
	return name
}

func (f *fixture) outcome(t *testing.T, id uuid.UUID) mail.StatusChange {
	t.Helper()
	items, e := f.api.Messages(context.Background(), f.u, "", nil, nil, 100)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range items {
		if m.ID == id {
			if m.Status == nil {
				t.Fatalf("message %s has no status change", id)
			}
			return *m.Status
		}
	}
	t.Fatalf("message %s not listed", id)
	return mail.StatusChange{}
}

func TestMailProposesAndAppliesStatusChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acct := f.connect(t)
	if f.statusName(t) != "Bookmarked" {
		t.Fatalf("job starts in %s", f.statusName(t))
	}

	// A confident move forward is applied, with a timeline entry. The job and
	// its status are asked in one request, after the category.
	calls := f.model.calls
	m1 := f.receive(t, acct, "m1", "Interview with Acme")
	if n := f.model.calls - calls; n != 2 {
		t.Fatalf("a linked message took %d model calls, want 2", n)
	}
	if c := f.outcome(t, m1); c.Outcome != "applied" || !c.Auto || c.PreviousID == nil {
		t.Fatalf("m1 %+v", c)
	}
	if f.statusName(t) != "Interviewing" {
		t.Fatalf("job in %s, want Interviewing", f.statusName(t))
	}
	var moves int
	if e := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM job_activity WHERE job_id=$1 AND type='status_changed' AND from_status='Bookmarked' AND to_status='Interviewing'`, f.job.ID).Scan(&moves); e != nil || moves != 1 {
		t.Fatalf("activities %d %v", moves, e)
	}
	// Undo puts it back, once.
	if m, e := f.api.Resolve(ctx, f.u, m1, "undo"); e != nil || m.Status.Outcome != "undone" {
		t.Fatalf("undo %+v %v", m.Status, e)
	}
	if f.statusName(t) != "Bookmarked" {
		t.Fatalf("after undo job in %s", f.statusName(t))
	}
	var conflict *library.ConflictError
	if _, e := f.api.Resolve(ctx, f.u, m1, "undo"); !errors.As(e, &conflict) {
		t.Fatalf("second undo: %v", e)
	}

	// An unsure proposal waits for the member.
	f.model.statusP = .6
	m2 := f.receive(t, acct, "m2", "Offer from Acme")
	if c := f.outcome(t, m2); c.Outcome != "review" || c.Auto {
		t.Fatalf("m2 %+v", c)
	}
	if f.statusName(t) != "Bookmarked" {
		t.Fatal("an unsure proposal moved the job")
	}
	if m, e := f.api.Resolve(ctx, f.u, m2, "apply"); e != nil || m.Status.Outcome != "applied" || m.Status.Auto {
		t.Fatalf("apply %+v %v", m.Status, e)
	}
	if f.statusName(t) != "Offer" {
		t.Fatalf("after apply job in %s", f.statusName(t))
	}

	// A confident move backward still waits, and can be dismissed.
	f.model.statusP = 1
	m3 := f.receive(t, acct, "m3", "Interview follow-up at Acme")
	if c := f.outcome(t, m3); c.Outcome != "review" {
		t.Fatalf("backward move %+v", c)
	}
	if m, e := f.api.Resolve(ctx, f.u, m3, "dismiss"); e != nil || m.Status.Outcome != "dismissed" || f.statusName(t) != "Offer" {
		t.Fatalf("dismiss %+v %v", m.Status, e)
	}

	// Mail that repeats the current status changes nothing.
	m4 := f.receive(t, acct, "m4", "Acme application received")
	if c := f.outcome(t, m4); c.Outcome != "no_change" {
		t.Fatalf("m4 %+v", c)
	}

	// Undo refuses once the member has moved the job themselves.
	sts, e := jobs.NewService(f.db.Store).ListStatuses(ctx, f.u)
	if e != nil {
		t.Fatal(e)
	}
	var rejected uuid.UUID
	for _, s := range sts {
		if s.Name == "Rejected" {
			rejected = s.ID
		}
	}
	if _, e = jobs.NewService(f.db.Store).MoveJob(ctx, f.u, f.job.ID, jobs.JobMove{StatusID: &rejected}); e != nil {
		t.Fatal(e)
	}
	if _, e = f.api.Resolve(ctx, f.u, m2, "undo"); !errors.As(e, &conflict) || f.statusName(t) != "Rejected" {
		t.Fatalf("undo after a manual move: %v, job in %s", e, f.statusName(t))
	}
}

func TestWorkerMovesJobsThroughColumnGrantsOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, c := range []struct {
		table, column string
		want          bool
	}{
		{"job", "status_id", true}, {"job", "position", true}, {"job", "title", false},
		{"job", "description", false}, {"job_status", "updated_at", true}, {"job_status", "name", false},
	} {
		var ok bool
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT has_column_privilege('zapply_worker',$1,$2,'UPDATE')`, c.table, c.column).Scan(&ok); e != nil || ok != c.want {
			t.Errorf("zapply_worker UPDATE %s.%s = %v, want %v (%v)", c.table, c.column, ok, c.want, e)
		}
	}
	for _, table := range []string{"job", "job_status"} {
		var ok bool
		if e := f.db.Admin(t).QueryRow(ctx, `SELECT has_table_privilege('zapply_worker',$1,'UPDATE')`, table).Scan(&ok); e != nil || ok {
			t.Errorf("zapply_worker can UPDATE all of %s (%v)", table, e)
		}
	}
}
