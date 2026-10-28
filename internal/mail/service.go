package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
)

// ErrNotConfigured means no OAuth client or encryption key ring is set up.
var ErrNotConfigured = errors.New("mail is not configured")

// Categories a message can have. Everything but "other" is relevant and kept.
var Categories = []string{"received", "assessment", "interview", "rejection", "offer", "other"}

type Service struct {
	store     *store.Store
	queue     *worker.Queue
	keys      *secrets.Ring
	provider  Provider
	push      bool
	reconcile time.Duration
	log       *slog.Logger

	mu     sync.Mutex
	tokens map[uuid.UUID]Tokens
}

// New returns a service. A nil provider (or key ring) leaves mail unconfigured:
// the read endpoints still answer and connecting fails with ErrNotConfigured.
// push says whether the provider delivers change notifications.
func New(st *store.Store, q *worker.Queue, keys *secrets.Ring, p Provider, push bool, reconcile time.Duration, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: st, queue: q, keys: keys, provider: p, push: push, reconcile: reconcile, log: log, tokens: map[uuid.UUID]Tokens{}}
}

// Store exposes the service's store to tests in other packages.
func (s *Service) Store() *store.Store { return s.store }

func (s *Service) Configured() bool { return s.provider != nil && s.keys != nil }

type Account struct {
	ID           uuid.UUID  `json:"id"`
	Provider     string     `json:"provider"`
	Address      string     `json:"address"`
	Status       string     `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	LastError    string     `json:"last_error"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Item struct {
	ID           uuid.UUID  `json:"id"`
	AccountID    uuid.UUID  `json:"account_id"`
	Category     string     `json:"category"`
	JobID        *uuid.UUID `json:"job_id"`
	From         string     `json:"from"`
	Subject      string     `json:"subject"`
	Snippet      string     `json:"snippet"`
	ReceivedAt   time.Time  `json:"received_at"`
	ClassifiedAt time.Time  `json:"classified_at"`
}

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return library.ErrNotFound
	}
	return err
}

func emit(ctx context.Context, tx pgx.Tx, u uuid.UUID, topic string, id uuid.UUID) error {
	payload, _ := json.Marshal(map[string]any{"id": id})
	_, err := tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, topic, json.RawMessage(payload))
	return err
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// StartConnect begins the consent flow and returns the URL to send the browser to.
func (s *Service) StartConnect(ctx context.Context, u uuid.UUID) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	state, err := random()
	if err != nil {
		return "", err
	}
	verifier, err := random()
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "SELECT platform.start_mail_oauth($1,$2)", digest(state), verifier)
		return e
	})
	if err != nil {
		return "", err
	}
	return s.provider.AuthURL(state, base64.RawURLEncoding.EncodeToString(challenge[:])), nil
}

// ErrBadState means the OAuth state is unknown, used or expired.
var ErrBadState = errors.New("unknown or expired authorization state")

// CompleteConnect finishes the flow the callback received: it trades the code
// for tokens, stores the refresh token encrypted, and queues the first sync.
func (s *Service) CompleteConnect(ctx context.Context, state, code string) (uuid.UUID, error) {
	if !s.Configured() {
		return uuid.Nil, ErrNotConfigured
	}
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, "SELECT platform.take_mail_oauth($1)", digest(state)).Scan(&raw); err != nil || raw == nil {
		return uuid.Nil, ErrBadState
	}
	var flow struct {
		UserID   uuid.UUID `json:"user_id"`
		Verifier string    `json:"verifier"`
	}
	if err := json.Unmarshal(raw, &flow); err != nil {
		return uuid.Nil, err
	}
	tokens, err := s.provider.Exchange(ctx, code, flow.Verifier)
	if err != nil {
		return uuid.Nil, err
	}
	profile, err := s.provider.Profile(ctx, tokens.Access)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.store.InTenant(ctx, flow.UserID, func(tx pgx.Tx) error {
		e := tx.QueryRow(ctx, "SELECT id FROM mail_account WHERE user_id=$1 AND provider=$2 AND address=$3", flow.UserID, s.provider.Name(), profile.Address).Scan(&id)
		switch {
		case errors.Is(e, pgx.ErrNoRows):
			id = uuid.Must(uuid.NewV7())
			if _, e = tx.Exec(ctx, "INSERT INTO mail_account(id,user_id,provider,address) VALUES($1,$2,$3,$4)", id, flow.UserID, s.provider.Name(), profile.Address); e != nil {
				return e
			}
		case e != nil:
			return e
		default:
			// Reconnecting keeps the cursor and the messages already read.
			if _, e = tx.Exec(ctx, "UPDATE mail_account SET status='active',last_error='',version=version+1,updated_at=now() WHERE id=$1 AND user_id=$2", id, flow.UserID); e != nil {
				return e
			}
		}
		ct, version, e := s.keys.Encrypt(id.String(), tokens.Refresh)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "SELECT platform.save_mail_credential($1,$2,$3)", id, ct, version); e != nil {
			return e
		}
		if e = emit(ctx, tx, flow.UserID, "mail.account.updated", id); e != nil {
			return e
		}
		return s.enqueue(ctx, tx, SyncArgs{UserID: flow.UserID, AccountID: id, Tick: "connect:" + uuid.NewString()})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		// The address belongs to another zapply user. Don't keep a stolen-looking token.
		_ = s.provider.Revoke(ctx, tokens.Refresh)
		return uuid.Nil, &library.ConflictError{Message: "that Gmail address is already connected to another zapply account"}
	}
	if err != nil {
		return uuid.Nil, err
	}
	s.mu.Lock()
	s.tokens[id] = tokens
	s.mu.Unlock()
	return id, nil
}

func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	if s.queue == nil {
		return errors.New("mail queue unavailable")
	}
	return s.queue.InsertTx(ctx, tx, args)
}

func (s *Service) Accounts(ctx context.Context, u uuid.UUID) ([]Account, error) {
	out := []Account{}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT id,provider,address,status,last_synced_at,last_error,version,created_at FROM mail_account WHERE user_id=$1 ORDER BY created_at,id`, u)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a Account
			if e = rows.Scan(&a.ID, &a.Provider, &a.Address, &a.Status, &a.LastSyncedAt, &a.LastError, &a.Version, &a.CreatedAt); e != nil {
				return e
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) account(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (Account, error) {
	var a Account
	e := tx.QueryRow(ctx, `SELECT id,provider,address,status,last_synced_at,last_error,version,created_at FROM mail_account WHERE user_id=$1 AND id=$2 FOR UPDATE`, u, id).
		Scan(&a.ID, &a.Provider, &a.Address, &a.Status, &a.LastSyncedAt, &a.LastError, &a.Version, &a.CreatedAt)
	return a, translate(e)
}

// SetPaused pauses or resumes classification for an account. A paused account
// is not read at all, so nothing is fetched or sent to the model meanwhile.
func (s *Service) SetPaused(ctx context.Context, u, id uuid.UUID, paused bool) (Account, error) {
	var out Account
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		a, e := s.account(ctx, tx, u, id)
		if e != nil {
			return e
		}
		want, from := "active", "paused"
		if paused {
			want, from = "paused", "active"
		}
		if a.Status == want {
			out = a
			return nil
		}
		if a.Status != from {
			return &library.ConflictError{Message: "the account is " + a.Status + " and cannot be " + want}
		}
		if e = tx.QueryRow(ctx, `UPDATE mail_account SET status=$3,version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2 RETURNING version`, u, id, want).Scan(&a.Version); e != nil {
			return e
		}
		a.Status = want
		out = a
		if e = emit(ctx, tx, u, "mail.account.updated", id); e != nil {
			return e
		}
		if !paused {
			return s.enqueue(ctx, tx, SyncArgs{UserID: u, AccountID: id, Tick: "resume:" + uuid.NewString()})
		}
		return nil
	})
	return out, err
}

// Disconnect stops reading the mailbox. The worker then stops the watch and
// revokes the token; messages already classified stay until the member deletes
// their zapply account.
func (s *Service) Disconnect(ctx context.Context, u, id uuid.UUID) error {
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		a, e := s.account(ctx, tx, u, id)
		if e != nil {
			return e
		}
		if a.Status == "disconnected" {
			return nil
		}
		if _, e = tx.Exec(ctx, `UPDATE mail_account SET status='disconnected',version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2`, u, id); e != nil {
			return e
		}
		if e = emit(ctx, tx, u, "mail.account.updated", id); e != nil {
			return e
		}
		return s.enqueue(ctx, tx, TeardownArgs{UserID: u, AccountID: id, Tick: uuid.NewString()})
	})
}

// Messages lists relevant classified mail, newest first.
func (s *Service) Messages(ctx context.Context, u uuid.UUID, category string, job, before *uuid.UUID, limit int) ([]Item, error) {
	out := []Item{}
	limit = min(100, max(1, limit))
	if category != "" {
		valid := false
		for _, c := range Categories[:len(Categories)-1] {
			valid = valid || c == category
		}
		if !valid {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "category", Message: "must be received, assessment, interview, rejection or offer"}}}
		}
	}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT id,account_id,category,job_id,COALESCE(from_address,''),COALESCE(subject,''),COALESCE(snippet,''),received_at,classified_at
 FROM mail_message WHERE user_id=$1 AND state='classified' AND category<>'other'
 AND ($2='' OR category=$2) AND ($3::uuid IS NULL OR job_id=$3)
 AND ($4::uuid IS NULL OR (received_at,id) < (SELECT received_at,id FROM mail_message WHERE user_id=$1 AND id=$4))
 ORDER BY received_at DESC,id DESC LIMIT $5`, u, category, job, before, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var m Item
			if e = rows.Scan(&m.ID, &m.AccountID, &m.Category, &m.JobID, &m.From, &m.Subject, &m.Snippet, &m.ReceivedAt, &m.ClassifiedAt); e != nil {
				return e
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// HandlePush turns a provider notification into a sync of that mailbox. Unknown
// or inactive mailboxes are ignored, so the push endpoint can always acknowledge.
func (s *Service) HandlePush(ctx context.Context, address, cursor string) error {
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, "SELECT platform.mail_account_for_address($1,$2)", "google", address).Scan(&raw); err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	var a struct {
		UserID    uuid.UUID `json:"user_id"`
		AccountID uuid.UUID `json:"account_id"`
		Status    string    `json:"status"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if a.Status != "active" {
		return nil
	}
	return s.store.InTenant(ctx, a.UserID, func(tx pgx.Tx) error {
		return s.enqueue(ctx, tx, SyncArgs{UserID: a.UserID, AccountID: a.AccountID, Tick: "push:" + cursor})
	})
}
