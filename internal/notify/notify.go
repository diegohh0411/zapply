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
	Mail       *mail.Item `json:"mail"`
}

type Summary struct {
	Unread int `json:"unread"`
}

// Filter narrows a list. Category applies to mail notifications.
type Filter struct {
	Kind     string
	Category string
	Unread   bool
	Before   *uuid.UUID
	Limit    int
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
	if f.Category != "" {
		ok := false
		for _, c := range mail.Categories[:len(mail.Categories)-1] {
			ok = ok || c == f.Category
		}
		if !ok {
			return nil, invalid("category", "must be received, assessment, interview, rejection or offer")
		}
	}
	out := []Item{}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT n.id,n.kind,n.occurred_at,n.read_at,n.mail_message_id FROM notification n
 LEFT JOIN mail_message m ON m.user_id=n.user_id AND m.id=n.mail_message_id
 WHERE n.user_id=$1 AND ($2='' OR n.kind=$2) AND ($3='' OR m.category=$3) AND (NOT $4 OR n.read_at IS NULL)
 AND ($5::uuid IS NULL OR (n.occurred_at,n.id) < (SELECT occurred_at,id FROM notification WHERE user_id=$1 AND id=$5))
 ORDER BY n.occurred_at DESC,n.id DESC LIMIT $6`, u, f.Kind, f.Category, f.Unread, f.Before, f.Limit)
		if e != nil {
			return e
		}
		var mailIDs []uuid.UUID
		refs := map[uuid.UUID]uuid.UUID{}
		for rows.Next() {
			var it Item
			var ref *uuid.UUID
			if e = rows.Scan(&it.ID, &it.Kind, &it.OccurredAt, &it.ReadAt, &ref); e != nil {
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

// Summary counts unread notifications.
func (s *Service) Summary(ctx context.Context, u uuid.UUID) (Summary, error) {
	var sum Summary
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification WHERE user_id=$1 AND read_at IS NULL`, u).Scan(&sum.Unread)
	})
	return sum, err
}

// MarkRead marks the given notifications read, or every one when all is set,
// and returns the new summary. Open pages hear notification.updated.
func (s *Service) MarkRead(ctx context.Context, u uuid.UUID, ids []uuid.UUID, all bool) (Summary, error) {
	if !all && len(ids) == 0 {
		return Summary{}, invalid("ids", "name at least one notification, or set all")
	}
	if len(ids) > 500 {
		return Summary{}, invalid("ids", "at most 500 at a time")
	}
	var sum Summary
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE notification SET read_at=now() WHERE user_id=$1 AND read_at IS NULL AND ($2 OR id=ANY($3))`, u, all, ids)
		if e != nil {
			return e
		}
		if tag.RowsAffected() > 0 {
			payload, _ := json.Marshal(map[string]any{"id": u})
			if _, e = tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, "notification.updated", json.RawMessage(payload)); e != nil {
				return e
			}
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification WHERE user_id=$1 AND read_at IS NULL`, u).Scan(&sum.Unread)
	})
	return sum, err
}
