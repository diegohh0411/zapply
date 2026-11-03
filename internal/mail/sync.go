package mail

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
)

const (
	// backfillDays and backfillMax bound the first read of a mailbox.
	backfillDays = 30
	backfillMax  = 200
	// syncBatch caps the messages one run reads, so a run finishes well inside the
	// job timeout. A larger backlog is picked up by a follow-up sync, because the
	// cursor only moves past what was read.
	syncBatch = 100
	// maxAgeDays is how far back mail is ever read, on first connect, after a
	// lapse or in a backlog.
	maxAgeDays = backfillDays
	leaseFor   = 5 * time.Minute
)

// SyncArgs is unique per trigger (Tick): River keeps a running job unique, so a
// notification arriving mid-sync would otherwise be dropped. The lease below
// keeps two syncs of one account from running at once.
type SyncArgs struct {
	UserID    uuid.UUID `json:"user_id" river:"unique"`
	AccountID uuid.UUID `json:"account_id" river:"unique"`
	Tick      string    `json:"tick" river:"unique"`
}

func (SyncArgs) Kind() string { return "mail_sync" }

type WatchArgs struct {
	UserID    uuid.UUID `json:"user_id" river:"unique"`
	AccountID uuid.UUID `json:"account_id" river:"unique"`
	Tick      string    `json:"tick" river:"unique"`
}

func (WatchArgs) Kind() string { return "mail_watch" }

type TeardownArgs struct {
	UserID    uuid.UUID `json:"user_id" river:"unique"`
	AccountID uuid.UUID `json:"account_id" river:"unique"`
	Tick      string    `json:"tick" river:"unique"`
}

func (TeardownArgs) Kind() string { return "mail_teardown" }

type accountState struct {
	Address   string
	Status    string
	HistoryID *string
	Pending   []byte
}

// access returns a usable access token for the account, refreshing when needed.
// A revoked or missing credential marks the account and returns ErrRevoked.
func (s *Service) access(ctx context.Context, u, id uuid.UUID) (string, error) {
	s.mu.Lock()
	t, ok := s.tokens[id]
	s.mu.Unlock()
	if ok && time.Until(t.Expires) > time.Minute {
		return t.Access, nil
	}
	var ct []byte
	var version string
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT ciphertext,encryption_version FROM platform.load_mail_credential($1)", id).Scan(&ct, &version)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", s.revoked(ctx, u, id)
	}
	if err != nil {
		return "", err
	}
	refresh, err := s.keys.Decrypt(id.String(), version, ct)
	if err != nil {
		return "", err
	}
	t, err = s.provider.Refresh(ctx, refresh)
	if errors.Is(err, ErrRevoked) {
		return "", s.revoked(ctx, u, id)
	}
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[id] = t
	s.mu.Unlock()
	return t.Access, nil
}

// revoked records that the member must consent again and returns ErrRevoked.
func (s *Service) revoked(ctx context.Context, u, id uuid.UUID) error {
	s.mu.Lock()
	delete(s.tokens, id)
	s.mu.Unlock()
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE mail_account SET status='needs_reconnect',last_error='Gmail access was revoked or expired. Reconnect to resume.',sync_lease_until=NULL,version=version+1,updated_at=now()
 WHERE user_id=$1 AND id=$2 AND status IN ('active','paused')`, u, id)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		return emit(ctx, tx, u, "mail.account.updated", id)
	})
	return errors.Join(ErrRevoked, err)
}

func (s *Service) fail(ctx context.Context, u, id uuid.UUID, cause error) {
	msg := "Sync failed. zapply will keep retrying."
	if cause != nil && !errors.Is(cause, context.Canceled) {
		// The cause can be a provider's raw JSON: the log keeps it, the member gets the short line.
		s.log.Warn("mail sync failed", "account_id", id, "err", truncate(cause.Error(), 500))
	}
	ctx = context.WithoutCancel(ctx)
	_ = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE mail_account SET last_error=$3,updated_at=now() WHERE user_id=$1 AND id=$2 AND status='active'`, u, id, msg)
		return e
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// Sync reads new inbox messages for one account and queues each for classification.
func (s *Service) Sync(ctx context.Context, u, id uuid.UUID) error {
	if !s.Configured() {
		return nil
	}
	var acc accountState
	var held bool
	var wait time.Duration
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var lease, retryAt *time.Time
		e := tx.QueryRow(ctx, `SELECT address,status,history_id,sync_lease_until,sync_pending,sync_retry_at FROM mail_account WHERE user_id=$1 AND id=$2 FOR UPDATE`, u, id).Scan(&acc.Address, &acc.Status, &acc.HistoryID, &lease, &acc.Pending, &retryAt)
		if errors.Is(e, pgx.ErrNoRows) {
			acc.Status = ""
			return nil
		}
		if e != nil {
			return e
		}
		if acc.Status != "active" {
			return nil
		}
		if retryAt != nil && retryAt.After(time.Now()) {
			held = true
			wait = time.Until(*retryAt)
			return nil
		}
		if lease != nil && lease.After(time.Now()) {
			held = true
			return nil
		}
		_, e = tx.Exec(ctx, `UPDATE mail_account SET sync_lease_until=$3 WHERE user_id=$1 AND id=$2`, u, id, time.Now().Add(leaseFor))
		return e
	})
	if err != nil {
		return err
	}
	if acc.Status != "active" {
		return nil
	}
	if held {
		return river.JobSnooze(max(wait, 5*time.Second))
	}
	err = s.sync(ctx, u, id, acc)
	var delay time.Duration
	if errors.Is(err, ErrRateLimited) {
		var e error
		delay, e = s.quotaBackoff(ctx, u, id)
		if e != nil {
			err = e
		}
	}
	// Release the lease even when the run was cancelled; a crash leaves it to expire.
	release := context.WithoutCancel(ctx)
	_ = s.store.InTenant(release, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(release, `UPDATE mail_account SET sync_lease_until=NULL WHERE user_id=$1 AND id=$2`, u, id)
		return e
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRateLimited):
		return river.JobSnooze(delay)
	case errors.Is(err, errPaced):
		return river.JobSnooze(65 * time.Second)
	case errors.Is(err, ErrRevoked):
		return nil // the member must reconnect; retrying cannot help
	default:
		s.fail(ctx, u, id, err)
		return err
	}
}

type pendingSync struct {
	IDs       []string `json:"ids"`
	Cursor    string   `json:"cursor"`
	More      bool     `json:"more"`
	Bootstrap bool     `json:"bootstrap"`
}

var errPaced = errors.New("mail sync request budget reached")

// reserveRead maintains a shared, durable per-account budget, even across workers
// and restarts. 40 full reads cost 2400 units, leaving room for other calls.
func (s *Service) reserveRead(ctx context.Context, u, id uuid.UUID) (bool, error) {
	var ok bool
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE mail_account SET
   sync_window_reads=CASE WHEN sync_window_start IS NULL OR sync_window_start <= now()-interval '65 seconds' THEN 1 ELSE sync_window_reads+1 END,
   sync_window_start=CASE WHEN sync_window_start IS NULL OR sync_window_start <= now()-interval '65 seconds' THEN now() ELSE sync_window_start END
   WHERE user_id=$1 AND id=$2 AND status='active' AND (sync_window_start IS NULL OR sync_window_start <= now()-interval '65 seconds' OR sync_window_reads < 40)
   RETURNING true`, u, id).Scan(&ok)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

func (s *Service) quotaBackoff(ctx context.Context, u, id uuid.UUID) (time.Duration, error) {
	var failures int
	var delay time.Duration
	ctx = context.WithoutCancel(ctx)
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `SELECT sync_quota_failures FROM mail_account WHERE user_id=$1 AND id=$2 AND status='active' FOR UPDATE`, u, id).Scan(&failures); e != nil {
			return e
		}
		failures = min(failures+1, 6)
		delay = time.Minute*time.Duration(1<<(failures-1)) + time.Duration(rand.IntN(15))*time.Second
		_, e := tx.Exec(ctx, `UPDATE mail_account SET sync_quota_failures=$3, sync_retry_at=now()+$4 * interval '1 second',
		 last_error='Gmail is limiting requests. Sync will resume automatically.', updated_at=now() WHERE user_id=$1 AND id=$2`, u, id, failures, delay.Seconds())
		return e
	})
	return delay, err
}

func (s *Service) sync(ctx context.Context, u, id uuid.UUID, acc accountState) error {
	access, err := s.access(ctx, u, id)
	if err != nil {
		return err
	}
	var pending pendingSync
	if len(acc.Pending) != 0 {
		if err := json.Unmarshal(acc.Pending, &pending); err != nil {
			return err
		}
	} else {
		var cursor string
		var ids []string
		var more bool
		var watch *Watch
		bootstrap := acc.HistoryID == nil
		if !bootstrap {
			page := ""
			cursor = *acc.HistoryID
			for {
				p, e := s.provider.History(ctx, access, *acc.HistoryID, page)
				if errors.Is(e, ErrHistoryExpired) {
					bootstrap = true
					break
				}
				if e != nil {
					return e
				}
				ids = append(ids, p.MessageIDs...)
				if p.Next == "" {
					if p.HistoryID != "" {
						cursor = p.HistoryID
					}
					break
				}
				// Not the last page: the mailbox's current position would skip what is
				// left, so only advance over the changes actually read.
				cursor = newer(cursor, p.LastID)
				if len(ids) >= syncBatch {
					more = true
					break
				}
				page = p.Next
			}
		}
		if bootstrap {
			// Take the cursor before listing so nothing arriving meanwhile is missed;
			// a message seen twice is deduplicated on insert.
			profile, e := s.provider.Profile(ctx, access)
			if e != nil {
				return e
			}
			cursor = profile.HistoryID
			if ids, e = s.provider.Recent(ctx, access, backfillDays, backfillMax); e != nil {
				return e
			}
		}
		if s.push {
			if w, ok, e := s.provider.Watch(ctx, access); e != nil {
				s.log.Warn("mail watch failed", "account_id", id, "err", e)
			} else if ok {
				watch = &w
			}
		}

		pending = pendingSync{IDs: unique(storable(ids)), Cursor: cursor, More: more, Bootstrap: bootstrap}
		raw, e := json.Marshal(pending)
		if e != nil {
			return e
		}
		var expires *time.Time
		if watch != nil {
			expires = &watch.Expires
		}
		if e = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE mail_account SET sync_pending=$3,watch_expires_at=COALESCE($4,watch_expires_at) WHERE user_id=$1 AND id=$2 AND status='active'`, u, id, string(raw), expires)
			return e
		}); e != nil {
			return e
		}
	}
	for len(pending.IDs) > 0 {
		mid := pending.IDs[0]
		known, e := s.known(ctx, u, id, []string{mid})
		if e != nil {
			return e
		}
		var m Message
		save := false
		if !known[mid] {
			ok, e := s.reserveRead(ctx, u, id)
			if e != nil {
				return e
			}
			if !ok {
				return errPaced
			}
			m, e = s.provider.Get(ctx, access, mid)
			if e != nil && !errors.Is(e, ErrMessageGone) {
				return e
			}
			if e == nil && !strings.Contains(strings.ToLower(m.From), "<"+acc.Address+">") && !strings.EqualFold(strings.TrimSpace(m.From), acc.Address) && !m.ReceivedAt.Before(time.Now().AddDate(0, 0, -maxAgeDays)) {
				save = true
			}
		}
		pending.IDs = pending.IDs[1:]
		raw, e := json.Marshal(pending)
		if e != nil {
			return e
		}
		e = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
			if save {
				if e := s.store1(ctx, tx, u, id, m); e != nil {
					return e
				}
			}
			// Progress proves Gmail is answering, so an earlier failure or backoff no
			// longer describes the mailbox.
			_, e := tx.Exec(ctx, `UPDATE mail_account SET sync_pending=$3,last_error='',sync_retry_at=NULL,sync_quota_failures=0 WHERE user_id=$1 AND id=$2 AND status='active'`, u, id, string(raw))
			if e != nil || !save {
				return e
			}
			return emit(ctx, tx, u, "mail.account.updated", id) // one more mail found
		})
		if e != nil {
			return e
		}
	}
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE mail_account SET history_id=$3,last_synced_at=now(),last_error='',sync_pending=NULL,sync_retry_at=NULL,sync_quota_failures=0,version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2 AND status='active'`, u, id, pending.Cursor)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		if e = emit(ctx, tx, u, "mail.account.updated", id); e != nil {
			return e
		}
		if pending.More || pending.Bootstrap {
			return s.enqueue(ctx, tx, SyncArgs{UserID: u, AccountID: id, Tick: "continue:" + pending.Cursor})
		}
		return nil
	})
}

// store1 saves one message and queues its classification. A message Postgres
// cannot store (a NUL byte, an invalid sequence) is skipped on its own, inside a
// savepoint: letting it fail the batch would roll the cursor back and make every
// retry fail on the same message.
func (s *Service) store1(ctx context.Context, tx pgx.Tx, u, account uuid.UUID, m Message) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	var inserted uuid.UUID
	err = sp.QueryRow(ctx, `INSERT INTO mail_message(id,user_id,account_id,provider_message_id,thread_id,received_at,from_address,subject,snippet)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(account_id,provider_message_id) DO NOTHING RETURNING id`,
		uuid.Must(uuid.NewV7()), u, account, m.ID, clean(m.ThreadID), m.ReceivedAt, clean(m.From), clean(m.Subject), clean(m.Snippet)).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return sp.Commit(ctx) // already stored
	}
	if err == nil {
		err = s.enqueue(ctx, sp, ClassifyArgs{UserID: u, MessageID: inserted})
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "22") || strings.HasPrefix(pg.Code, "23")) {
			s.log.Warn("skipped a message the database would not store", "account_id", account, "code", pg.Code)
			return nil
		}
		return err
	}
	return sp.Commit(ctx)
}

// clean removes what text columns cannot hold. Mail headers are attacker
// controlled, and a MIME encoded word can decode to a NUL or invalid UTF-8.
func clean(s string) string { return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "") }

// newer returns the later of two history IDs, which are unsigned numbers.
func newer(a, b string) string {
	x, ea := strconv.ParseUint(a, 10, 64)
	y, eb := strconv.ParseUint(b, 10, 64)
	if eb != nil {
		return a
	}
	if ea != nil || y > x {
		return b
	}
	return a
}

// storable drops IDs Postgres cannot hold in a text column. Real provider IDs
// are plain ASCII; anything else is not worth a failed query.
func storable(in []string) []string {
	out := in[:0]
	for _, id := range in {
		if id != "" && utf8.ValidString(id) && !strings.ContainsRune(id, 0) {
			out = append(out, id)
		}
	}
	return out
}

func unique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// known marks provider message IDs already stored, so they are not read again.
func (s *Service) known(ctx context.Context, u, id uuid.UUID, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT provider_message_id FROM mail_message WHERE user_id=$1 AND account_id=$2 AND provider_message_id=ANY($3)`, u, id, ids)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			if e = rows.Scan(&m); e != nil {
				return e
			}
			out[m] = true
		}
		return rows.Err()
	})
	return out, err
}

// RenewWatch asks the provider to keep pushing notifications for the account.
func (s *Service) RenewWatch(ctx context.Context, u, id uuid.UUID) error {
	if !s.Configured() || !s.push {
		return nil
	}
	var status string
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM mail_account WHERE user_id=$1 AND id=$2`, u, id).Scan(&status)
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return nil
	}
	if err != nil {
		return err
	}
	access, err := s.access(ctx, u, id)
	if errors.Is(err, ErrRevoked) {
		return nil
	}
	if err != nil {
		return err
	}
	w, ok, err := s.provider.Watch(ctx, access)
	if err != nil || !ok {
		return err
	}
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE mail_account SET watch_expires_at=$3,updated_at=now() WHERE user_id=$1 AND id=$2`, u, id, w.Expires)
		return e
	})
}

// Teardown stops a disconnected account's watch, revokes its token and deletes it.
// It does nothing if the member has reconnected in the meantime.
func (s *Service) Teardown(ctx context.Context, u, id uuid.UUID) error {
	if !s.Configured() {
		return nil
	}
	var status string
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM mail_account WHERE user_id=$1 AND id=$2`, u, id).Scan(&status)
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "disconnected") {
		return nil
	}
	if err != nil {
		return err
	}
	var ct []byte
	var version string
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT ciphertext,encryption_version FROM platform.load_mail_credential($1)", id).Scan(&ct, &version)
	})
	if err == nil {
		refresh, e := s.keys.Decrypt(id.String(), version, ct)
		if e != nil {
			return e
		}
		if t, e := s.provider.Refresh(ctx, refresh); e == nil {
			_ = s.provider.Stop(ctx, t.Access) // best effort: the watch expires on its own
		}
		// A reconnect since the first check made this token the live one: leave it.
		var again string
		if e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM mail_account WHERE user_id=$1 AND id=$2`, u, id).Scan(&again)
		}); e != nil || again != "disconnected" {
			return e
		}
		if e := s.provider.Revoke(ctx, refresh); e != nil {
			return e
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	s.mu.Lock()
	delete(s.tokens, id)
	s.mu.Unlock()
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		// Re-check inside the delete: a reconnect after the read must keep its token.
		_, e := tx.Exec(ctx, `SELECT platform.delete_mail_credential(id) FROM mail_account WHERE user_id=$1 AND id=$2 AND status='disconnected'`, u, id)
		return e
	})
}
