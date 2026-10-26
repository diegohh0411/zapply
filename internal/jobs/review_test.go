package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/google/uuid"
)

func TestRegressionDeleteStatusStealsConcurrentMove(t *testing.T) {
	d := testdb.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	uid, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "review", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s := jobs.NewService(d.Store)
	sts, err := s.ListStatuses(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := sts[0].ID, sts[1].ID, sts[2].ID
	j, err := s.CreateJob(ctx, uid, jobs.JobInput{Title: "concurrent move", StatusID: &a})
	if err != nil {
		t.Fatal(err)
	}
	blocker := d.Admin(t)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, "SELECT id FROM job_status WHERE id=$1 FOR UPDATE", b)
	if err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteStatus(ctx, uid, a, &b) }()
	observer := d.Admin(t)
	for {
		var blocked bool
		err := observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM job_status%FOR UPDATE%')`).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	moved, err := s.MoveJob(ctx, uid, j.ID, jobs.JobMove{StatusID: &c})
	if err != nil {
		t.Fatal(err)
	}
	if moved.StatusID != c {
		t.Fatal("move did not reach C")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, uid, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := s.ListActivities(ctx, uid, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 2 || acts[0].ToStatus != sts[2].Name {
		t.Fatalf("unexpected activity after deleting the old column: %+v", acts)
	}
	if got.StatusID != c {
		t.Fatalf("delete moved a job that had already left its column")
	}
}

func TestRegressionURLPreservesEscapedPath(t *testing.T) {
	a, _ := jobs.NormalizeURL("https://jobs.example/a%2Fb")
	b, _ := jobs.NormalizeURL("https://jobs.example/a/b")
	t.Logf("encoded slash=%q literal slash=%q", a, b)
	if a == b {
		t.Fatal("distinct paths collide")
	}
}

func TestRegressionNULReturnsValidation(t *testing.T) {
	d := testdb.New(t)
	h := api.Handler(api.Deps{Store: d.Store, Verifier: auth.NewDev("review-secret", "authenticated"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	tok, _ := auth.MintDev("review-secret", "authenticated", "alice", time.Hour)
	body, _ := json.Marshal(map[string]any{"title": "engineer\x00test"})
	r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	t.Logf("POST /jobs with NUL: status=%d body=%s", w.Code, w.Body)
	if w.Code != 422 {
		t.Fatalf("got %d, want field validation", w.Code)
	}
}

func TestRegressionLongURL(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	uid, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "review", Subject: "longurl"})
	if err != nil {
		t.Fatal(err)
	}
	var path strings.Builder
	for i := 0; i < 110; i++ {
		fmt.Fprintf(&path, "%s", uuid.NewString())
	}
	raw := "https://jobs.example/" + path.String()[:1975]
	_, err = jobs.NewService(d.Store).CreateJob(ctx, uid, jobs.JobInput{Title: "longurl", URL: raw})
	t.Logf("URL length=%d, create err=%v", len(raw), err)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegressionUnicodeURLIndexLimit(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	uid, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "review", Subject: "unicodeurl"})
	if err != nil {
		t.Fatal(err)
	}
	var path strings.Builder
	for i := 0; i < 1900; i++ {
		path.WriteRune(rune(0x4e00 + (i*7919)%20992))
	}
	raw := "https://jobs.example/" + path.String()
	norm, _ := jobs.NormalizeURL(raw)
	s := jobs.NewService(d.Store)
	created, err := s.CreateJob(ctx, uid, jobs.JobInput{Title: "valid Unicode URL", URL: raw})
	t.Logf("input runes=%d normalized bytes=%d err=%v", len([]rune(raw)), len(norm), err)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateJob(ctx, uid, jobs.JobInput{Title: "duplicate", URL: raw + "#apply"})
	var duplicate *jobs.DuplicateError
	if !errors.As(err, &duplicate) || duplicate.ExistingID != created.ID {
		t.Fatalf("long URL duplicate = %v; want existing id %s", err, created.ID)
	}
	if _, err = s.CreateJob(ctx, uid, jobs.JobInput{Title: "distinct", URL: raw + "x"}); err != nil {
		t.Fatalf("distinct long URL: %v", err)
	}

}

func TestUpgradeRepairsEscapedURLKeys(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	uid, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "review", Subject: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	s := jobs.NewService(d.Store)
	encoded := "https://jobs.example/a%2Fb"
	literal := "https://jobs.example/a/b"
	created, err := s.CreateJob(ctx, uid, jobs.JobInput{Title: "encoded posting", URL: encoded})
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a database at version 5 with a key written by the old normalizer.
	admin := d.Admin(t)
	if _, err := admin.Exec(ctx, "UPDATE job SET normalized_url=$2 WHERE id=$1", created.ID, literal); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM goose_db_version WHERE version_id=6"); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, d.AdminURL, log); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateJob(ctx, uid, jobs.JobInput{Title: "same posting", URL: encoded})
	var duplicate *jobs.DuplicateError
	if !errors.As(err, &duplicate) || duplicate.ExistingID != created.ID {
		t.Fatalf("encoded URL duplicate after upgrade: %v", err)
	}
	if _, err := s.CreateJob(ctx, uid, jobs.JobInput{Title: "different posting", URL: literal}); err != nil {
		t.Fatalf("literal path after upgrade: %v", err)
	}
	// Idempotent migration must not reinterpret or damage the repaired keys.
	if err := store.Migrate(ctx, d.AdminURL, log); err != nil {
		t.Fatal(err)
	}
	var forced bool
	if err := admin.QueryRow(ctx, "SELECT relforcerowsecurity FROM pg_class WHERE oid='job'::regclass").Scan(&forced); err != nil || !forced {
		t.Fatalf("forced RLS after backfill: %v, %v", forced, err)
	}
}
