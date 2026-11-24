// Package membership implements synchronous platform administration and usage reads.
package membership

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/library"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/store/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Service struct {
	store *store.Store
	ring  *secrets.Ring
}

var inputAmount = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,12})?$`)

func New(st *store.Store, ring *secrets.Ring) *Service { return &Service{st, ring} }

type Error struct {
	Status       int
	Code, Detail string
}

func (e *Error) Error() string { return e.Detail }

func Translate(err error) error {
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "ZA403":
		return &Error{403, "owner_required", p.Message}
	case "ZA404":
		return library.ErrNotFound
	case "ZA412":
		return library.ErrVersionMismatch
	case "ZA409", "23505":
		return &Error{409, "conflict", "the resource conflicts with existing state"}
	case "ZA422", "23514", "23503", "22003", "22001":
		return invalid("", "invalid or missing referenced value")
	}
	return err
}
func invalid(field, detail string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: detail}}}
}

func (s *Service) read(ctx context.Context, u uuid.UUID, sql string, args ...any) (json.RawMessage, error) {
	var result json.RawMessage
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&result) })
	return result, Translate(err)
}
func (s *Service) exec(ctx context.Context, u uuid.UUID, sql string, args ...any) error {
	return Translate(s.store.InTenant(ctx, u, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }))
}
func (s *Service) Role(ctx context.Context, u uuid.UUID) (string, error) {
	var role string
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error { var err error; role, err = db.New(tx).GetPlatformRole(ctx); return err })
	return role, err
}
func (s *Service) AssertOwner(ctx context.Context, u uuid.UUID) error {
	return s.exec(ctx, u, "SELECT platform.assert_owner()")
}
func (s *Service) Usage(ctx context.Context, u uuid.UUID) (json.RawMessage, error) {
	return s.readQueries(ctx, u, func(q *db.Queries) (json.RawMessage, error) { return q.GetPlatformUsage(ctx) })
}
func (s *Service) Plans(ctx context.Context, u uuid.UUID) (json.RawMessage, error) {
	return s.readQueries(ctx, u, func(q *db.Queries) (json.RawMessage, error) { return q.ListPlatformPlans(ctx) })
}
func (s *Service) Members(ctx context.Context, u uuid.UUID, after *uuid.UUID, limit int) (json.RawMessage, error) {
	return s.read(ctx, u, "SELECT platform.members($1,$2)", after, limit)
}
func (s *Service) Entries(ctx context.Context, u uuid.UUID, before *int64, limit int) (json.RawMessage, error) {
	return s.read(ctx, u, "SELECT platform.usage_entries($1,$2)", before, limit)
}
func (s *Service) Keys(ctx context.Context, u uuid.UUID) (json.RawMessage, error) {
	return s.readQueries(ctx, u, func(q *db.Queries) (json.RawMessage, error) { return q.ListSystemKeys(ctx) })
}
func (s *Service) readQueries(ctx context.Context, u uuid.UUID, fn func(*db.Queries) (json.RawMessage, error)) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error { var err error; raw, err = fn(db.New(tx)); return err })
	return raw, Translate(err)
}
func (s *Service) Attempts(ctx context.Context, u uuid.UUID, before *uuid.UUID, limit int) (json.RawMessage, error) {
	return s.read(ctx, u, "SELECT platform.admin_attempts($1,$2)", before, limit)
}

// SavePlan creates (version 0) or updates a plan. maxSearch nil keeps a plan's
// search watchers, or gives a new plan none.
func (s *Service) SavePlan(ctx context.Context, u, id uuid.UUID, name, week, month string, maxSearch *int, version int) (json.RawMessage, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.ContainsRune(name, 0) {
		return nil, invalid("name", "must be between 1 and 100 characters")
	}
	w, err := modelapi.ParseUSD(week)
	if err != nil || w <= 0 || !inputAmount.MatchString(week) {
		return nil, invalid("cap_7d_usd", "must be a positive USD amount")
	}
	m, err := modelapi.ParseUSD(month)
	if err != nil || m <= 0 || !inputAmount.MatchString(month) {
		return nil, invalid("cap_30d_usd", "must be a positive USD amount")
	}
	if maxSearch != nil && (*maxSearch < 0 || *maxSearch > 100) {
		return nil, invalid("max_search_watchers", "must be between 0 and 100")
	}
	return s.read(ctx, u, "SELECT platform.save_plan($1,$2,$3,$4,$5,$6)", id, name, w, m, maxSearch, version)
}
func (s *Service) SaveMembership(ctx context.Context, u, member, plan uuid.UUID, status string, version int) (json.RawMessage, error) {
	if status != "active" && status != "suspended" {
		return nil, invalid("status", "must be active or suspended")
	}
	return s.read(ctx, u, "SELECT platform.save_membership($1,$2,$3,$4)", member, plan, status, version)
}

// AddKey stores an OpenRouter key, inactive.
func (s *Service) AddKey(ctx context.Context, u uuid.UUID, label, key string) (uuid.UUID, error) {
	return s.AddKeyOfKind(ctx, u, label, key, "openrouter")
}

// AddKeyOfKind stores a provider key of kind "openrouter", "firecrawl",
// "crawl4ai" or "theirstack", inactive.
func (s *Service) AddKeyOfKind(ctx context.Context, u uuid.UUID, label, key, kind string) (uuid.UUID, error) {
	// Authorize before encryption/configuration errors to avoid disclosing system state.
	if err := s.AssertOwner(ctx, u); err != nil {
		return uuid.Nil, err
	}
	if kind != "openrouter" && kind != "firecrawl" && kind != "crawl4ai" && kind != "theirstack" {
		return uuid.Nil, invalid("kind", "must be openrouter, firecrawl, crawl4ai or theirstack")
	}
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" || utf8.RuneCountInString(label) > 100 || strings.ContainsRune(label, 0) {
		return uuid.Nil, invalid("label", "must be between 1 and 100 characters")
	}
	if len(key) < 8 || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n\x00") {
		return uuid.Nil, invalid("key", "invalid provider key")
	}
	id := uuid.Must(uuid.NewV7())
	encrypted, version, err := s.ring.Encrypt(id.String(), key)
	if err != nil {
		return uuid.Nil, &Error{503, "encryption_unavailable", "system key encryption is not configured"}
	}
	err = s.exec(ctx, u, "SELECT platform.add_key($1,$2,$3,$4,$5,$6)", id, label, encrypted, version, secrets.Fingerprint(key), kind)
	return id, err
}

// SearchSettings returns the job search provider's settings (owner only).
func (s *Service) SearchSettings(ctx context.Context, u uuid.UUID) (json.RawMessage, error) {
	return s.read(ctx, u, "SELECT platform.search_settings()")
}

// SaveSearchSettings sets the TheirStack key, its credit price (USD, "" for
// none) and how many postings one poll may return.
func (s *Service) SaveSearchSettings(ctx context.Context, u uuid.UUID, key *uuid.UUID, creditUSD string, maxResults, version int) (json.RawMessage, error) {
	var price *int64
	if creditUSD != "" {
		n, err := modelapi.ParseUSD(creditUSD)
		if err != nil || n <= 0 || !creditAmount.MatchString(creditUSD) {
			return nil, invalid("credit_usd", "must be a positive USD amount such as 0.0069")
		}
		price = &n
	}
	if maxResults < 1 || maxResults > 100 {
		return nil, invalid("max_results", "must be between 1 and 100")
	}
	return s.read(ctx, u, "SELECT platform.save_search_settings($1,$2,$3,$4)", key, price, maxResults, version)
}

var creditAmount = regexp.MustCompile(`^[0-9]{1,3}(?:\.[0-9]{1,12})?$`)

func (s *Service) ActivateKey(ctx context.Context, u, id uuid.UUID, active bool, version int) error {
	return s.exec(ctx, u, "SELECT platform.activate_key($1,$2,$3)", id, active, version)
}
func (s *Service) Resolve(ctx context.Context, u, id uuid.UUID, amount string, generation *string, reason string, version int) error {
	n, err := modelapi.ParseUSD(amount)
	if err != nil || !inputAmount.MatchString(amount) {
		return invalid("amount_usd", "must be a non-negative USD amount")
	}
	if utf8.RuneCountInString(strings.TrimSpace(reason)) < 3 || utf8.RuneCountInString(reason) > 1000 || strings.ContainsRune(reason, 0) {
		return invalid("reason", "provide 3 to 1000 characters of reconciliation evidence")
	}
	if generation != nil && (len(*generation) > 128 || !strings.HasPrefix(*generation, "gen-")) {
		return invalid("generation_id", "invalid generation ID")
	}
	return s.exec(ctx, u, "SELECT platform.resolve_attempt($1,$2,$3,$4,$5)", id, n, generation, reason, version)
}

// SearchBalance reads the team's TheirStack credit balance with the key the
// search settings name (owner only). No key gives a nil balance.
func (s *Service) SearchBalance(ctx context.Context, u uuid.UUID, ts *jobsearch.TheirStack) (*jobsearch.Balance, error) {
	var id uuid.UUID
	var encrypted []byte
	var version string
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT key_id,ciphertext,encryption_version FROM platform.search_key()").Scan(&id, &encrypted, &version)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err)
	}
	if s.ring == nil {
		return nil, errors.New("key ring is unavailable")
	}
	key, err := s.ring.Decrypt(id.String(), version, encrypted)
	if err != nil {
		return nil, err
	}
	b, err := ts.Balance(ctx, key)
	if err != nil {
		return nil, err
	}
	return &b, nil
}
