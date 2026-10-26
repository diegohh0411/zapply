// Package jobs tracks job postings and the applications to them: per-user
// statuses (board columns), jobs, their activity timeline and saved views.
// Design: docs/parts/04-jobs.md.
//
// Errors are the library's (ValidationError, ConflictError, ErrNotFound,
// ErrVersionMismatch), so the API maps both domains the same way.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// Service implements the tracking use cases. Every method runs in one
// tenant transaction and records its domain events in the outbox.
type Service struct {
	store *store.Store
}

func NewService(st *store.Store) *Service { return &Service{store: st} }

// DuplicateError is a job whose posting URL the user already saved (409).
type DuplicateError struct{ ExistingID uuid.UUID }

func (e *DuplicateError) Error() string { return "a job with this URL already exists" }

func (s *Service) tx(ctx context.Context, uid uuid.UUID, fn func(q *db.Queries) error) error {
	err := s.store.InTenant(ctx, uid, func(tx pgx.Tx) error { return fn(db.New(tx)) })
	return translate(err)
}

// translate maps database rule violations that slipped past validation.
func translate(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23505":
		return &library.ConflictError{Message: "already exists"}
	case "23503":
		return invalid("", "references a missing record")
	case "23514":
		return invalid("", "violates "+pg.ConstraintName)
	}
	return err
}

func emit(ctx context.Context, q *db.Queries, uid uuid.UUID, topic string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	return q.InsertOutbox(ctx, db.InsertOutboxParams{ID: newID(), UserID: uid, Topic: topic, Payload: raw})
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return library.ErrNotFound
	}
	return err
}

func invalid(field, msg string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: msg}}}
}

// fieldErrors collects validation failures for one request.
type fieldErrors []library.FieldError

func (f *fieldErrors) add(field, msg string) {
	*f = append(*f, library.FieldError{Field: field, Message: msg})
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return &library.ValidationError{Fields: f}
}

// text trims s and checks its length (and presence, if required).
func (f *fieldErrors) text(field string, s *string, max int, required bool) {
	*s = strings.TrimSpace(*s)
	switch {
	case strings.ContainsRune(*s, '\x00'):
		f.add(field, "must not contain NUL characters")
	case required && *s == "":
		f.add(field, "is required")
	case utf8.RuneCountInString(*s) > max:
		f.add(field, fmt.Sprintf("must be at most %d characters", max))
	}
}

// oneOf checks an optional enum value; "" means unset.
func (f *fieldErrors) oneOf(field, v string, allowed []string) {
	if v == "" {
		return
	}
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	f.add(field, "must be one of "+strings.Join(allowed, ", "))
}

func ptrIf[T ~string](v string) *T {
	if v == "" {
		return nil
	}
	t := T(v)
	return &t
}

func str[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}
