package library_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// Regression tests for the PR #2 review findings.

func noStaleHashes(t *testing.T, s *library.Service, uid uuid.UUID) {
	t.Helper()
	stale, err := s.StaleHashes(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Fatalf("stale content hashes on %v", stale)
	}
}

// A rename must not overwrite a concurrent document write with a hash of
// the content it read before that write committed.
func TestRehashWaitsForConcurrentWrite(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	uid, _ := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: uuid.NewString()})
	s := library.NewService(db.Store)

	org, err := s.CreateOrganization(ctx, uid, library.OrganizationInput{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "job", Category: library.Paid, OrganizationID: &org.ID, Title: "Engineer", Start: "2020",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A writer holds the evidence lock (as a PUT does) and changes content.
	locked, release, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		written <- db.Store.InTenant(ctx, uid, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM evidence WHERE id = $1 FOR UPDATE`, job.ID); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := tx.Exec(ctx, `UPDATE evidence SET title = 'Staff Engineer' WHERE id = $1`, job.ID)
			return err
		})
	}()
	<-locked
	renamed := make(chan error, 1)
	go func() {
		_, err := s.UpdateOrganization(ctx, uid, org.ID, library.OrganizationInput{Name: "Acme Inc"})
		renamed <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the rename reach the lock
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}
	noStaleHashes(t, s, uid)
}

func TestIdempotentReplayReturnsOriginal(t *testing.T) {
	s, uid, ctx := setup(t)
	in := library.EvidenceInput{Kind: "volunteering", Category: library.Unpaid, Title: "Original", Start: "2019"}
	idem := &library.Idempotency{Key: "k", RequestHash: library.RequestHash([]byte("body"))}
	first, _, err := s.CreateEvidence(ctx, uid, in, idem)
	if err != nil {
		t.Fatal(err)
	}
	edit := in
	edit.Title = "Edited"
	if _, err := s.UpdateEvidence(ctx, uid, first.ID, 1, edit); err != nil {
		t.Fatal(err)
	}
	again, replayed, err := s.CreateEvidence(ctx, uid, in, idem)
	if err != nil || !replayed || again.Title != "Original" || again.Version != 1 || again.ID != first.ID {
		t.Fatalf("replay = %+v replayed=%v err=%v", again, replayed, err)
	}
	if err := s.DeleteEvidence(ctx, uid, first.ID, false); err != nil {
		t.Fatal(err)
	}
	if again, _, err = s.CreateEvidence(ctx, uid, in, idem); err != nil || again.Title != "Original" {
		t.Fatalf("replay after delete = %+v, %v", again, err)
	}
}

func titles(t *testing.T, s *library.Service, uid uuid.UUID, c library.Category) string {
	t.Helper()
	list, _, err := s.ListEvidences(context.Background(), uid, library.ListFilter{Category: c, TopLevel: true})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, e := range list {
		out += e.Title
	}
	return out
}

func TestCategoryChangeGetsAFreshPosition(t *testing.T) {
	s, uid, ctx := setup(t)
	mk := func(title string, c library.Category) library.Evidence {
		e, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "project", Category: c, Title: title}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	mk("A", library.Paid)
	b := mk("B", library.Unpaid) // same first key as A, in another scope
	if _, err := s.UpdateEvidence(ctx, uid, b.ID, 1, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: "B"}); err != nil {
		t.Fatal(err)
	}
	c := mk("C", library.Paid)
	if _, err := s.MoveEvidence(ctx, uid, c.ID, library.MoveInput{BeforeID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, uid, library.Paid); got != "ACB" {
		t.Fatalf("order = %s, want ACB", got)
	}
}

func TestMoveRepairsTiedPositions(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	uid, _ := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: uuid.NewString()})
	s := library.NewService(db.Store)
	var ids []uuid.UUID
	for _, title := range []string{"A", "B", "C"} {
		e, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: title}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	// Legacy tie between A and B.
	if _, err := db.Admin(t).Exec(ctx, `UPDATE evidence SET position = 'V' WHERE id = ANY ($1)`, ids[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveEvidence(ctx, uid, ids[2], library.MoveInput{BeforeID: &ids[1]}); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, uid, library.Paid); got != "ACB" {
		t.Fatalf("order = %s, want ACB", got)
	}
}

func outboxTopics(t *testing.T, db *testdb.DB, uid uuid.UUID) []string {
	t.Helper()
	rows, err := db.Admin(t).Query(context.Background(), `SELECT topic FROM outbox WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		t.Fatal(err)
	}
	topics, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return topics
}

func TestImplicitChangesEmitEvents(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	uid, _ := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: uuid.NewString()})
	s := library.NewService(db.Store)

	intern, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "internship", Category: library.Paid, Title: "Intern", Start: "2018", Skills: []string{"Go"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "project", ParentID: &intern.ID, Title: "Tool"}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"skill.created", "evidence.created", "evidence.created"}
	if got := outboxTopics(t, db, uid); !slices.Equal(got, want) {
		t.Fatalf("after creates: %v, want %v", got, want)
	}

	// Recategorizing the parent updates (and announces) the child too.
	if _, err := s.UpdateEvidence(ctx, uid, intern.ID, 1, library.EvidenceInput{
		Kind: "internship", Category: library.Unpaid, Title: "Intern", Start: "2018", Skills: []string{"Go"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := outboxTopics(t, db, uid)[3:]; !slices.Equal(got, []string{"evidence.updated", "evidence.updated"}) {
		t.Fatalf("after recategorize: %v", got)
	}

	// Renaming a linked skill changes the document: new version and event.
	skills, _ := s.ListSkills(ctx, uid)
	before, _ := s.GetEvidence(ctx, uid, intern.ID)
	if _, err := s.UpdateSkill(ctx, uid, skills[0].ID, library.SkillInput{Name: "Golang"}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetEvidence(ctx, uid, intern.ID)
	if after.Version != before.Version+1 || after.Skills[0] != "Golang" {
		t.Fatalf("after skill rename: version %d→%d skills %v", before.Version, after.Version, after.Skills)
	}
	got := outboxTopics(t, db, uid)
	if got[len(got)-2] != "evidence.updated" || got[len(got)-1] != "skill.updated" {
		t.Fatalf("after skill rename: %v", got)
	}
	noStaleHashes(t, s, uid)
}

func TestNegativeZeroHashesStably(t *testing.T) {
	s, uid, ctx := setup(t)
	negZero := 0.0
	negZero = -negZero
	_, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "course", Category: library.Education, Title: "Intro", Start: "2020",
		Details:    map[string]any{"hours": negZero},
		Highlights: []library.HighlightInput{{Text: "Done", MetricValue: &negZero}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	noStaleHashes(t, s, uid)
}

func TestOversizedListsFailFast(t *testing.T) {
	s, uid, ctx := setup(t)
	hs := make([]library.HighlightInput, 20000)
	_, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: "x", Highlights: hs}, nil)
	fieldErr(t, err, "highlights")
	if n := len(err.(*library.ValidationError).Fields); n > 51 {
		t.Fatalf("%d field errors for one oversized list", n)
	}

	details := map[string]any{}
	for i := range 500 {
		details[uuid.NewString()] = i
	}
	_, _, err = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "project", Category: library.Paid, Title: "x", Details: details}, nil)
	if n := len(err.(*library.ValidationError).Fields); n != 51 {
		t.Fatalf("%d field errors, want capped at 51", n)
	}
}

func TestListRejectsBadFilters(t *testing.T) {
	s, uid, ctx := setup(t)
	_, _, err := s.ListEvidences(ctx, uid, library.ListFilter{Category: "bogus"})
	fieldErr(t, err, "category")
	_, _, err = s.ListEvidences(ctx, uid, library.ListFilter{Form: "bogus"})
	fieldErr(t, err, "form")
	// A well-formed cursor with an invalid category.
	_, _, err = s.ListEvidences(ctx, uid, library.ListFilter{Cursor: "eyJjIjoiYm9ndXMiLCJwIjoiViIsImkiOiIwMWExNzYxNi1jMGZiLTdjNDAtYmIzNi0yYTNmZWM5MjIwOWEifQ"})
	fieldErr(t, err, "cursor")
}
