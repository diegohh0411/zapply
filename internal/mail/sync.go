package mail

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

const (
	// backfillDays and backfillMax bound the first read of a mailbox.
	backfillDays = 30
	backfillMax  = 200
	// syncBatch caps the messages one run reads; a larger backlog is picked up
	// by the next sync because the cursor only moves past what was read.
	syncBatch = 500
	leaseFor  = 5 * time.Minute
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
	msg := "Sync failed; will retry."
	if cause != nil && !errors.Is(cause, context.Canceled) {
		msg = "Sync failed: " + truncate(cause.Error(), 150)
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
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var lease *time.Time
		e := tx.QueryRow(ctx, `SELECT address,status,history_id,sync_lease_until FROM mail_account WHERE user_id=$1 AND id=$2 FOR UPDATE`, u, id).Scan(&acc.Address, &acc.Status, &acc.HistoryID, &lease)
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
		return river.JobSnooze(5 * time.Second)
	}
	err = s.sync(ctx, u, id, acc)
	// Release the lease even when the run was cancelled; a crash leaves it to expire.
	release := context.WithoutCancel(ctx)
	_ = s.store.InTenant(release, u, func(tx pgx.Tx) error {
		_, e := tx.Exec(release, `UPDATE mail_account SET sync_lease_until=NULL WHERE user_id=$1 AND id=$2`, u, id)
		return e
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRevoked):
		return nil // the member must reconnect; retrying cannot help
	default:
		s.fail(ctx, u, id, err)
		return err
	}
}

func (s *Service) sync(ctx context.Context, u, id uuid.UUID, acc accountState) error {
	access, err := s.access(ctx, u, id)
	if err != nil {
		return err
	}
	var cursor string
	var ids []string
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
			if p.HistoryID != "" {
				cursor = p.HistoryID
			}
			if p.Next == "" || len(ids) >= syncBatch {
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
	ids = unique(ids)
	known, err := s.known(ctx, u, id, ids)
	if err != nil {
		return err
	}
	msgs := make([]Message, 0, len(ids))
	for _, mid := range ids {
		if known[mid] {
			continue
		}
		m, e := s.provider.Get(ctx, access, mid)
		if errors.Is(e, ErrMessageGone) {
			continue
		}
		if e != nil {
			return e
		}
		// Mail the member sent to themselves is not an application update.
		if strings.Contains(strings.ToLower(m.From), "<"+acc.Address+">") || strings.EqualFold(strings.TrimSpace(m.From), acc.Address) {
			continue
		}
		msgs = append(msgs, m)
	}
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		for _, m := range msgs {
			mid := uuid.Must(uuid.NewV7())
			var inserted uuid.UUID
			e := tx.QueryRow(ctx, `INSERT INTO mail_message(id,user_id,account_id,provider_message_id,thread_id,received_at,from_address,subject,snippet)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(account_id,provider_message_id) DO NOTHING RETURNING id`,
				mid, u, id, m.ID, m.ThreadID, m.ReceivedAt, m.From, m.Subject, m.Snippet).Scan(&inserted)
			if errors.Is(e, pgx.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			if e = s.enqueue(ctx, tx, ClassifyArgs{UserID: u, MessageID: inserted}); e != nil {
				return e
			}
		}
		var expires *time.Time
		if watch != nil {
			expires = &watch.Expires
		}
		tag, e := tx.Exec(ctx, `UPDATE mail_account SET history_id=$3,last_synced_at=now(),last_error='',watch_expires_at=COALESCE($4,watch_expires_at),version=version+1,updated_at=now()
 WHERE user_id=$1 AND id=$2 AND status='active'`, u, id, cursor, expires)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		return emit(ctx, tx, u, "mail.account.updated", id)
	})
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
