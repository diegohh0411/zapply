// Package notify is the member's notification inbox: things to look at, read
// or unread. Rows are made by the database (see migration 00033); this package
// lists them, counts the unread ones and marks them read.
package notify

import (
	"context"
	"encoding/json"
	"time"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/mail"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Kinds of notification. Each kind carries its own payload field on Item.
var Kinds = []string{"mail"}

type Item struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	OccurredAt time.Time  `json:"occurred_at"`
	ReadAt     *time.Time `json:"read_at"`
	ArchivedAt *time.Time `json:"archived_at"`
	Mail       *mail.Item `json:"mail"`
}

type Summary struct {
	Unread int `json:"unread"`
}

// Filter narrows a list. View is inbox (not archived, the default), unread
// (unread and not archived) or archived.
type Filter struct {
	Kind   string
	View   string
	Before *uuid.UUID
	Limit  int
}

type Service struct{ store *store.Store }

func New(st *store.Store) *Service { return &Service{store: st} }

func invalid(field, msg string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: msg}}}
}

// List returns notifications newest first.
func (s *Service) List(ctx context.Context, u uuid.UUID, f Filter) ([]Item, error) {
	f.Limit = min(100, max(1, f.Limit))
	if f.Kind != "" && f.Kind != "mail" {
		return nil, invalid("kind", "must be mail")
	}
	switch f.View {
	case "":
		f.View = "inbox"
	case "inbox", "unread", "archived":
	default:
		return nil, invalid("view", "must be inbox, unread or archived")
	}
	out := []Item{}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT n.id,n.kind,n.occurred_at,n.read_at,n.archived_at,n.mail_message_id FROM notification n
 WHERE n.user_id=$1 AND ($2='' OR n.kind=$2)
 AND (CASE $3 WHEN 'archived' THEN n.archived_at IS NOT NULL WHEN 'unread' THEN n.archived_at IS NULL AND n.read_at IS NULL ELSE n.archived_at IS NULL END)
 AND ($4::uuid IS NULL OR (n.occurred_at,n.id) < (SELECT occurred_at,id FROM notification WHERE user_id=$1 AND id=$4))
 ORDER BY n.occurred_at DESC,n.id DESC LIMIT $5`, u, f.Kind, f.View, f.Before, f.Limit)
		if e != nil {
			return e
		}
		var mailIDs []uuid.UUID
		refs := map[uuid.UUID]uuid.UUID{}
		for rows.Next() {
			var it Item
			var ref *uuid.UUID
			if e = rows.Scan(&it.ID, &it.Kind, &it.OccurredAt, &it.ReadAt, &it.ArchivedAt, &ref); e != nil {
				rows.Close()
				return e
			}
			if ref != nil {
				mailIDs = append(mailIDs, *ref)
				refs[it.ID] = *ref
			}
			out = append(out, it)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		msgs, e := mail.ItemsByID(ctx, tx, u, mailIDs)
		if e != nil {
			return e
		}
		for i := range out {
			if m, ok := msgs[refs[out[i].ID]]; ok {
				out[i].Mail = &m
			}
		}
		return nil
	})
	return out, err
}

const unreadCount = `SELECT count(*) FROM notification WHERE user_id=$1 AND read_at IS NULL AND archived_at IS NULL`

// Summary counts unread notifications in the inbox.
func (s *Service) Summary(ctx context.Context, u uuid.UUID) (Summary, error) {
	var sum Summary
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, unreadCount, u).Scan(&sum.Unread)
	})
	return sum, err
}

func checkIDs(ids []uuid.UUID) error {
	if len(ids) > 500 {
		return invalid("ids", "at most 500 at a time")
	}
	return nil
}

// change runs one update and, when it changed anything, tells open pages.
func (s *Service) change(ctx context.Context, u uuid.UUID, query string, args ...any) (Summary, error) {
	var sum Summary
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, query, append([]any{u}, args...)...)
		if e != nil {
			return e
		}
		if tag.RowsAffected() > 0 {
			payload, _ := json.Marshal(map[string]any{"id": u})
			if _, e = tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, "notification.updated", json.RawMessage(payload)); e != nil {
				return e
			}
		}
		return tx.QueryRow(ctx, unreadCount, u).Scan(&sum.Unread)
	})
	return sum, err
}

// MarkRead marks the named notifications read (or unread), or every inbox
// notification read when all is set.
func (s *Service) MarkRead(ctx context.Context, u uuid.UUID, ids []uuid.UUID, all, unread bool) (Summary, error) {
	if e := checkIDs(ids); e != nil {
		return Summary{}, e
	}
	switch {
	case unread && (all || len(ids) == 0):
		return Summary{}, invalid("ids", "name the notifications to mark unread")
	case unread:
		return s.change(ctx, u, `UPDATE notification SET read_at=NULL WHERE user_id=$1 AND read_at IS NOT NULL AND id=ANY($2)`, ids)
	case all:
		return s.change(ctx, u, `UPDATE notification SET read_at=now() WHERE user_id=$1 AND read_at IS NULL AND archived_at IS NULL`)
	case len(ids) == 0:
		return Summary{}, invalid("ids", "name at least one notification, or set all")
	}
	return s.change(ctx, u, `UPDATE notification SET read_at=now() WHERE user_id=$1 AND read_at IS NULL AND id=ANY($2)`, ids)
}

// Archive moves the named notifications (or every read one, with read) out of
// the inbox and marks them read; restore moves the named ones back.
func (s *Service) Archive(ctx context.Context, u uuid.UUID, ids []uuid.UUID, read, restore bool) (Summary, error) {
	if e := checkIDs(ids); e != nil {
		return Summary{}, e
	}
	switch {
	case restore && len(ids) == 0:
		return Summary{}, invalid("ids", "name the notifications to restore")
	case restore:
		return s.change(ctx, u, `UPDATE notification SET archived_at=NULL WHERE user_id=$1 AND archived_at IS NOT NULL AND id=ANY($2)`, ids)
	case read:
		return s.change(ctx, u, `UPDATE notification SET archived_at=now() WHERE user_id=$1 AND archived_at IS NULL AND read_at IS NOT NULL`)
	case len(ids) == 0:
		return Summary{}, invalid("ids", "name at least one notification, or set read")
	}
	return s.change(ctx, u, `UPDATE notification SET archived_at=now(),read_at=COALESCE(read_at,now()) WHERE user_id=$1 AND archived_at IS NULL AND id=ANY($2)`, ids)
}
