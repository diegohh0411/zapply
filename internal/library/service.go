package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// Service implements the library use cases. Every method runs in one tenant
// transaction and records its domain events in the outbox.
type Service struct {
	store *store.Store
}

func NewService(st *store.Store) *Service { return &Service{store: st} }

var (
	ErrNotFound        = errors.New("not found")
	ErrVersionMismatch = errors.New("version mismatch")
)

// ValidationError lists every invalid field of a request (HTTP 422).
type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "invalid request"
	}
	return fmt.Sprintf("invalid request: %s %s", e.Fields[0].Field, e.Fields[0].Message)
}

func invalid(field, msg string) error {
	return &ValidationError{Fields: []FieldError{{field, msg}}}
}

// ConflictError is a request that clashes with current state (HTTP 409).
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

// tx runs fn in a tenant transaction with typed queries.
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
	case "ZA001": // evidence rules (see migration 00002)
		return &ValidationError{Fields: []FieldError{{"", pg.Message}}}
	case "23505":
		return &ConflictError{Message: "already exists"}
	case "23503":
		return invalid("", "references a missing record")
	case "23514":
		return invalid("", "violates "+pg.ConstraintName)
	}
	return err
}

func emit(ctx context.Context, q *db.Queries, uid uuid.UUID, topic string, id uuid.UUID, version int) error {
	body := map[string]any{"id": id}
	if version > 0 {
		body["version"] = version
	}
	payload, _ := json.Marshal(body)
	eid, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return q.InsertOutbox(ctx, db.InsertOutboxParams{ID: eid, UserID: uid, Topic: topic, Payload: payload})
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
