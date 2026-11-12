package library_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func setup(t *testing.T) (*library.Service, uuid.UUID, context.Context) {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	uid, err := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return library.NewService(db.Store), uid, ctx
}

func ptr[T any](v T) *T { return &v }

func fieldErr(t *testing.T, err error, field string) {
	t.Helper()
	var v *library.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("err = %v, want ValidationError on %s", err, field)
	}
	for _, f := range v.Fields {
		if f.Field == field {
			return
		}
	}
	t.Fatalf("validation errors %+v lack field %q", v.Fields, field)
}

func TestEvidenceLifecycle(t *testing.T) {
	s, uid, ctx := setup(t)

	org, err := s.CreateOrganization(ctx, uid, library.OrganizationInput{Name: "Acme  Corp", Type: "company"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "job", Category: library.Paid, OrganizationID: &org.ID, Title: "Engineer",
		Start: "2021-03", IsCurrent: true, Details: map[string]any{"work_mode": "remote"},
		Skills: []string{"Go", "go", "PostgreSQL"},
		Highlights: []library.HighlightInput{
			{Text: "Cut deploy time", MetricValue: ptr(40.0), MetricUnit: "%", Skills: []string{"CI"}},
			{Text: "Mentored two engineers"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if job.Form != library.Engagement || len(job.Skills) != 2 || len(job.Highlights) != 2 || job.Version != 1 {
		t.Fatalf("created job = %+v", job)
	}
	if job.Start.String() != "2021-03" || !job.AutoInclude {
		t.Fatalf("start %v auto %v", job.Start, job.AutoInclude)
	}

	project, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "project", ParentID: &job.ID, Title: "Billing rewrite", Start: "2022",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if project.Category != library.Paid {
		t.Fatalf("child category = %s, want inherited paid", project.Category)
	}

	// Update keeps a highlight id, drops the other, adds one.
	keep := job.Highlights[0]
	in := library.EvidenceInput{
		Kind: "job", Category: library.Paid, OrganizationID: &org.ID, Title: "Senior Engineer",
		Start: "2021-03", IsCurrent: true, Skills: []string{"Go"},
		Highlights: []library.HighlightInput{
			{ID: &keep.ID, Text: keep.Text, MetricValue: keep.MetricValue, MetricUnit: "%", Skills: []string{"CI"}},
			{Text: "Led on-call rotation"},
		},
	}
	updated, err := s.UpdateEvidence(ctx, uid, job.ID, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.Highlights[0].ID != keep.ID || string(updated.Highlights[0].ContentHash) != string(keep.ContentHash) {
		t.Fatalf("updated = %+v", updated)
	}
	if len(updated.Children) != 1 {
		t.Fatalf("children = %+v", updated.Children)
	}
	if _, err := s.UpdateEvidence(ctx, uid, job.ID, 1, in); !errors.Is(err, library.ErrVersionMismatch) {
		t.Fatalf("stale update err = %v", err)
	}

	// Renaming the organization changes the job's hash but not its version.
	if _, err := s.UpdateOrganization(ctx, uid, org.ID, library.OrganizationInput{Name: "Acme Inc", Type: "company"}); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetEvidence(ctx, uid, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.ContentHash) == string(updated.ContentHash) || after.Version != updated.Version {
		t.Fatal("organization rename did not rehash, or bumped the version")
	}

	// Move the project to a degree: it adopts the education category.
	degree, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "degree_program", Category: library.Education, Title: "BSc Computer Science", Start: "2015", End: "2019",
		Details: map[string]any{"degree_type": "bachelor", "status": "completed"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.MoveEvidence(ctx, uid, project.ID, library.MoveInput{ParentID: &degree.ID})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Category != library.Education || *moved.ParentID != degree.ID {
		t.Fatalf("moved = %+v", moved)
	}

	if err := s.DeleteEvidence(ctx, uid, degree.ID, false); err == nil {
		t.Fatal("deleted parent with children without cascade")
	}
	if err := s.DeleteEvidence(ctx, uid, degree.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvidence(ctx, uid, project.ID); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("child survived cascade: %v", err)
	}

	list, next, err := s.ListEvidences(ctx, uid, library.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || next != "" {
		t.Fatalf("list = %d items, next %q", len(list), next)
	}
}

// A kind can change on update, together with its category, when the new
// kind allows that category. Details of the old kind are not carried over.
func TestChangeKindOnUpdate(t *testing.T) {
	s, uid, ctx := setup(t)

	research, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{
		Kind: "job", Category: library.Paid, Title: "NLP research", Start: "2026-08",
		Details: map[string]any{"work_mode": "remote"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Paid → Unpaid as an internship: the category and kind both change.
	moved, err := s.UpdateEvidence(ctx, uid, research.ID, 1, library.EvidenceInput{
		Kind: "internship", Category: library.Unpaid, Title: "NLP research", Start: "2026-08",
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Kind != "internship" || moved.Form != library.Engagement || moved.Category != library.Unpaid || moved.Version != 2 {
		t.Fatalf("moved = %+v", moved)
	}
	if len(moved.Details) != 0 {
		t.Fatalf("details of the old kind survived: %v", moved.Details)
	}

	// A category the new kind does not allow is still refused.
	_, err = s.UpdateEvidence(ctx, uid, research.ID, 2, library.EvidenceInput{
		Kind: "volunteering", Category: library.Paid, Title: "NLP research", Start: "2026-08",
	})
	var verr *library.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("volunteering in paid: err = %v, want validation error", err)
	}
}

func TestEvidenceValidation(t *testing.T) {
	s, uid, ctx := setup(t)
	_, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "job", Category: library.Unpaid, Title: "x", Start: "2020"}, nil)
	fieldErr(t, err, "category")
	_, _, err = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "assignment", Category: library.Paid, Title: "x", Start: "2020"}, nil)
	fieldErr(t, err, "parent_id")
	_, _, err = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "job", Category: library.Paid, Title: "x", Start: "2020", End: "2019"}, nil)
	fieldErr(t, err, "end")
	_, _, err = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "license", Category: library.Education, Title: "RN", Details: map[string]any{"nope": 1}}, nil)
	fieldErr(t, err, "details.issuer")
	fieldErr(t, err, "details.nope")

	cert, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "certification", Category: library.Education, Title: "CKA", Details: map[string]any{"issuer": "CNCF"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "award", ParentID: &cert.ID, Title: "x", Details: map[string]any{"issuer": "y"}}, nil)
	fieldErr(t, err, "parent_id")

	brk, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "career_break", Category: library.Unpaid, Title: "Career break", Start: "2020-01", End: "2020-12"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if brk.AutoInclude {
		t.Fatal("career break auto-included by default")
	}
}

// The evidence_kind table and the Go catalog must agree.
func TestKindTableMatchesCatalog(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	rows, err := db.Admin(t).Query(ctx, `SELECT kind, form, categories::text[], child_only FROM evidence_kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var kind, form string
		var cats []string
		var childOnly bool
		if err := rows.Scan(&kind, &form, &cats, &childOnly); err != nil {
			t.Fatal(err)
		}
		k, ok := library.LookupKind(kind)
		if !ok {
			t.Errorf("table kind %q missing from catalog", kind)
			continue
		}
		seen++
		var want []string
		for _, c := range k.Categories {
			want = append(want, string(c))
		}
		slices.Sort(want)
		slices.Sort(cats)
		if string(k.Form) != form || k.ChildOnly != childOnly || !slices.Equal(want, cats) {
			t.Errorf("%s: table (%s %v %v) != catalog (%s %v %v)", kind, form, cats, childOnly, k.Form, want, k.ChildOnly)
		}
	}
	if seen != len(library.Kinds) {
		t.Errorf("table has %d of %d catalog kinds", seen, len(library.Kinds))
	}
}

func TestWritesEmitOutboxEvents(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	uid, _ := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: uuid.NewString()})
	s := library.NewService(db.Store)

	e, _, err := s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "volunteering", Category: library.Unpaid, Title: "Food bank", Start: "2019"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetArchived(ctx, uid, e.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEvidence(ctx, uid, e.ID, false); err != nil {
		t.Fatal(err)
	}
	// A failed write leaves no event behind.
	_, _, _ = s.CreateEvidence(ctx, uid, library.EvidenceInput{Kind: "volunteering", Category: library.Paid, Title: "x", Start: "2019"}, nil)

	rows, err := db.Admin(t).Query(ctx, `SELECT topic FROM outbox WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		t.Fatal(err)
	}
	var topics []string
	for rows.Next() {
		var tp string
		_ = rows.Scan(&tp)
		topics = append(topics, tp)
	}
	want := []string{"evidence.created", "evidence.archived", "evidence.deleted"}
	if !slices.Equal(topics, want) {
		t.Fatalf("outbox topics = %v, want %v", topics, want)
	}
}

func TestIdempotentCreateUnderConcurrency(t *testing.T) {
	s, uid, ctx := setup(t)
	in := library.EvidenceInput{Kind: "volunteering", Category: library.Unpaid, Title: "Food bank", Start: "2019"}
	idem := &library.Idempotency{Key: "same", RequestHash: library.RequestHash([]byte("body"))}

	const n = 8
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var e library.Evidence
			e, _, errs[i] = s.CreateEvidence(ctx, uid, in, idem)
			ids[i] = e.ID
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("concurrent retries created different evidence: %v", ids)
		}
	}
	list, _, _ := s.ListEvidences(ctx, uid, library.ListFilter{})
	if len(list) != 1 {
		t.Fatalf("%d evidences, want 1", len(list))
	}
}
