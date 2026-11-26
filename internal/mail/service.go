package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	// RetryAt is when a rate-limited sync resumes by itself; nil when none is held.
	RetryAt *time.Time `json:"retry_at"`
	// MailsDone of MailsTotal messages are classified. The total counts mail still
	// waiting for the model and mail found but not yet read, so it grows during a
	// backlog. MailsRelevant is how many of the done ones concern an application.
	MailsDone     int `json:"mails_done"`
	MailsTotal    int `json:"mails_total"`
	MailsRelevant int `json:"mails_relevant"`
}

type Item struct {
	ID        uuid.UUID  `json:"id"`
	AccountID uuid.UUID  `json:"account_id"`
	Category  string     `json:"category"`
	JobID     *uuid.UUID `json:"job_id"`
	// JobManual is true when the member chose the job (or chose none).
	JobManual    bool          `json:"job_manual"`
	From         string        `json:"from"`
	Subject      string        `json:"subject"`
	Snippet      string        `json:"snippet"`
	ReceivedAt   time.Time     `json:"received_at"`
	ClassifiedAt time.Time     `json:"classified_at"`
	GmailURL     string        `json:"gmail_url"`
	Status       *StatusChange `json:"status_change"`
}

// StatusChange is what a message proposed for its job's status, and what
// became of it. Outcome is no_change, review, applied, dismissed or undone.
type StatusChange struct {
	Outcome    string     `json:"outcome"`
	ProposedID *uuid.UUID `json:"proposed_status_id"`
	PreviousID *uuid.UUID `json:"previous_status_id"`
	Confidence *float64   `json:"confidence"`
	Auto       bool       `json:"auto"`
}

const itemColumns = `id,account_id,category,job_id,job_manual,COALESCE(from_address,''),COALESCE(subject,''),COALESCE(snippet,''),received_at,classified_at,
 (SELECT address FROM mail_account WHERE id=account_id),COALESCE(NULLIF(thread_id,''),provider_message_id),
 status_outcome,suggested_status_id,previous_status_id,status_confidence,status_auto`

func scanItem(row pgx.Row) (Item, error) {
	var m Item
	var address, thread string
	var outcome *string
	var c StatusChange
	if e := row.Scan(&m.ID, &m.AccountID, &m.Category, &m.JobID, &m.JobManual, &m.From, &m.Subject, &m.Snippet, &m.ReceivedAt, &m.ClassifiedAt, &address, &thread,
		&outcome, &c.ProposedID, &c.PreviousID, &c.Confidence, &c.Auto); e != nil {
		return m, e
	}
	m.GmailURL = gmailURL(address, thread)
	if outcome != nil {
		c.Outcome = *outcome
		m.Status = &c
	}
	return m, nil
}

func (s *Service) item(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (Item, error) {
	return scanItem(tx.QueryRow(ctx, `SELECT `+itemColumns+` FROM mail_message WHERE user_id=$1 AND id=$2`, u, id))
}

// gmailURL opens a thread in the mailbox it was read from. authuser picks the
// right account when the browser is signed in to several.
func gmailURL(address, threadID string) string {
	return "https://mail.google.com/mail/?authuser=" + url.QueryEscape(address) + "#all/" + url.PathEscape(threadID)
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

// ErrBadState means the OAuth state is unknown, used, expired, or was started by
// someone else.
var ErrBadState = errors.New("unknown or expired authorization state")

// CompleteConnect finishes a flow for the signed-in member: it trades the code
// for tokens, stores the refresh token encrypted, and queues the first sync. The
// state must be one this member started, so a consent link forwarded to someone
// else cannot attach that person's mailbox to this account.
func (s *Service) CompleteConnect(ctx context.Context, u uuid.UUID, state, code string) (Account, error) {
	if !s.Configured() {
		return Account{}, ErrNotConfigured
	}
	var raw []byte
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT platform.take_mail_oauth($1)", digest(state)).Scan(&raw)
	})
	if err != nil || raw == nil {
		return Account{}, errors.Join(ErrBadState, err)
	}
	var flow struct {
		Verifier string `json:"verifier"`
	}
	if err := json.Unmarshal(raw, &flow); err != nil {
		return Account{}, err
	}
	tokens, err := s.provider.Exchange(ctx, code, flow.Verifier)
	if err != nil {
		return Account{}, err
	}
	profile, err := s.provider.Profile(ctx, tokens.Access)
	if err != nil {
		return Account{}, err
	}
	var out Account
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var id uuid.UUID
		e := tx.QueryRow(ctx, "SELECT id FROM mail_account WHERE user_id=$1 AND provider=$2 AND address=$3", u, s.provider.Name(), profile.Address).Scan(&id)
		switch {
		case errors.Is(e, pgx.ErrNoRows):
			id = uuid.Must(uuid.NewV7())
			if _, e = tx.Exec(ctx, "INSERT INTO mail_account(id,user_id,provider,address) VALUES($1,$2,$3,$4)", id, u, s.provider.Name(), profile.Address); e != nil {
				return e
			}
		case e != nil:
			return e
		default:
			// Reconnecting keeps the cursor and the messages already read.
			if _, e = tx.Exec(ctx, "UPDATE mail_account SET status='active',last_error='',version=version+1,updated_at=now() WHERE id=$1 AND user_id=$2", id, u); e != nil {
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
		if e = emit(ctx, tx, u, "mail.account.updated", id); e != nil {
			return e
		}
		if e = s.enqueue(ctx, tx, SyncArgs{UserID: u, AccountID: id, Tick: "connect:" + uuid.NewString()}); e != nil {
			return e
		}
		out, e = s.account(ctx, tx, u, id)
		return e
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		// The address belongs to another zapply user. Don't keep a stolen-looking token.
		_ = s.provider.Revoke(ctx, tokens.Refresh)
		return Account{}, &library.ConflictError{Message: "that Gmail address is already connected to another zapply account"}
	}
	if err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	s.tokens[out.ID] = tokens
	s.mu.Unlock()
	return out, nil
}

func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	if s.queue == nil {
		return errors.New("mail queue unavailable")
	}
	return s.queue.InsertTx(ctx, tx, args)
}

const accountColumns = `id,provider,address,status,last_synced_at,last_error,version,created_at`

func (s *Service) Accounts(ctx context.Context, u uuid.UUID) ([]Account, error) {
	out := []Account{}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+accountColumns+` FROM mail_account WHERE user_id=$1 ORDER BY created_at,id`, u)
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
		if e = rows.Err(); e != nil {
			return e
		}
		rows.Close()
		for i := range out {
			if e = s.progress(ctx, tx, u, &out[i]); e != nil {
				return e
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) account(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (Account, error) {
	var a Account
	e := tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM mail_account WHERE user_id=$1 AND id=$2 FOR UPDATE`, u, id).
		Scan(&a.ID, &a.Provider, &a.Address, &a.Status, &a.LastSyncedAt, &a.LastError, &a.Version, &a.CreatedAt)
	if e != nil {
		return a, translate(e)
	}
	return a, s.progress(ctx, tx, u, &a)
}

// progress fills what a member watches while a mailbox is read: how many
// messages are classified out of all it has found so far, and when a held sync
// resumes. Ids still waiting in the sync's list count once, unless already stored.
func (s *Service) progress(ctx context.Context, tx pgx.Tx, u uuid.UUID, a *Account) error {
	var waiting, queued int
	e := tx.QueryRow(ctx, `SELECT a.sync_retry_at,
 (SELECT count(*) FILTER (WHERE m.state<>'pending') FROM mail_message m WHERE m.user_id=a.user_id AND m.account_id=a.id),
 (SELECT count(*) FILTER (WHERE m.state='pending') FROM mail_message m WHERE m.user_id=a.user_id AND m.account_id=a.id),
 (SELECT count(*) FILTER (WHERE m.state='classified' AND m.category<>'other') FROM mail_message m WHERE m.user_id=a.user_id AND m.account_id=a.id),
 (SELECT count(*) FROM jsonb_array_elements_text(COALESCE(a.sync_pending->'ids','[]')) i
   WHERE NOT EXISTS(SELECT FROM mail_message m WHERE m.user_id=a.user_id AND m.account_id=a.id AND m.provider_message_id=i))
 FROM mail_account a WHERE a.user_id=$1 AND a.id=$2`, u, a.ID).Scan(&a.RetryAt, &a.MailsDone, &waiting, &a.MailsRelevant, &queued)
	if e != nil {
		return e
	}
	a.MailsTotal = a.MailsDone + waiting + queued
	if a.RetryAt != nil && !a.RetryAt.After(time.Now()) {
		a.RetryAt = nil
	}
	return nil
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
		// Pausing or resuming starts clean: an old failure or backoff is not about now.
		if e = tx.QueryRow(ctx, `UPDATE mail_account SET status=$3,last_error='',sync_retry_at=NULL,sync_quota_failures=0,version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2 RETURNING version`, u, id, want).Scan(&a.Version); e != nil {
			return e
		}
		a.Status, a.LastError, a.RetryAt = want, "", nil
		out = a
		if e = emit(ctx, tx, u, "mail.account.updated", id); e != nil {
			return e
		}
		if paused {
			return nil
		}
		return s.wake(ctx, tx, u, id, "resume")
	})
	return out, err
}

// wake queues a sync of the mailbox and a retry of every message still waiting
// for the model, which a pause or a long failure may have left idle.
func (s *Service) wake(ctx context.Context, tx pgx.Tx, u, id uuid.UUID, why string) error {
	rows, e := tx.Query(ctx, `SELECT id FROM mail_message WHERE user_id=$1 AND account_id=$2 AND state='pending'`, u, id)
	if e != nil {
		return e
	}
	var waiting []uuid.UUID
	for rows.Next() {
		var m uuid.UUID
		if e = rows.Scan(&m); e != nil {
			rows.Close()
			return e
		}
		waiting = append(waiting, m)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, m := range waiting {
		if e = s.enqueue(ctx, tx, ClassifyArgs{UserID: u, MessageID: m, Wake: why + ":" + uuid.NewString()}); e != nil {
			return e
		}
	}
	return s.enqueue(ctx, tx, SyncArgs{UserID: u, AccountID: id, Tick: why + ":" + uuid.NewString()})
}

// SyncNow checks the mailbox now instead of waiting for the next notification
// or scan. A sync held back by Gmail's rate limit still waits for its time, which
// the account reports as RetryAt.
func (s *Service) SyncNow(ctx context.Context, u, id uuid.UUID) (Account, error) {
	var out Account
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		a, e := s.account(ctx, tx, u, id)
		if e != nil {
			return e
		}
		out = a
		if a.Status != "active" {
			return &library.ConflictError{Message: "the account is " + a.Status + " and cannot be synced"}
		}
		return s.wake(ctx, tx, u, id, "manual")
	})
	return out, err
}

// Rescan re-reads the last `days` days of the mailbox, beyond the usual window.
// Mail already read is skipped, so only mail never seen reaches the model.
func (s *Service) Rescan(ctx context.Context, u, id uuid.UUID, days int) (Account, error) {
	if days < RescanMinDays || days > RescanMaxDays {
		return Account{}, &library.ValidationError{Fields: []library.FieldError{{Field: "days", Message: fmt.Sprintf("must be between %d and %d", RescanMinDays, RescanMaxDays)}}}
	}
	var out Account
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		a, e := s.account(ctx, tx, u, id)
		if e != nil {
			return e
		}
		out = a
		if a.Status != "active" {
			return &library.ConflictError{Message: "the account is " + a.Status + " and cannot be rescanned"}
		}
		return s.enqueue(ctx, tx, SyncArgs{UserID: u, AccountID: id, Tick: "rescan:" + uuid.NewString(), Days: days})
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
		rows, e := tx.Query(ctx, `SELECT `+itemColumns+`
 FROM mail_message WHERE user_id=$1 AND state='classified' AND category<>'other'
 AND ($2='' OR category=$2) AND ($3::uuid IS NULL OR job_id=$3)
 AND ($4::uuid IS NULL OR (received_at,id) < (SELECT received_at,id FROM mail_message WHERE user_id=$1 AND id=$4))
 ORDER BY received_at DESC,id DESC LIMIT $5`, u, category, job, before, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			m, e := scanItem(rows)
			if e != nil {
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

// ItemsByID loads relevant messages by id inside the caller's tenant
// transaction, for lists (such as notifications) that point at mail.
func ItemsByID(ctx context.Context, tx pgx.Tx, u uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Item, error) {
	out := map[uuid.UUID]Item{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+itemColumns+` FROM mail_message WHERE user_id=$1 AND id=ANY($2) AND state='classified' AND category<>'other'`, u, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out[m.ID] = m
	}
	return out, rows.Err()
}
