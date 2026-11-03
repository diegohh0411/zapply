// Package mail connects a member's mailbox, syncs new inbox messages and
// classifies each one once. Mailbox access sits behind Provider so Gmail is
// the first of several possible backends; design in docs/parts/10-mail-plan.md.
package mail

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrRevoked means the member withdrew access or the refresh token expired.
	// The account needs a new consent; retrying cannot help.
	ErrRevoked = errors.New("mail authorization revoked")
	// ErrHistoryExpired means the sync cursor is too old for incremental sync.
	ErrHistoryExpired = errors.New("mail history cursor expired")
	// ErrMessageGone means the message was deleted before it could be read.
	ErrMessageGone = errors.New("mail message not found")
	// ErrRateLimited is a temporary provider quota or rate limit.
	ErrRateLimited = errors.New("mail provider rate limited")
)

type Tokens struct {
	Refresh string
	Access  string
	Expires time.Time
}

type Profile struct {
	Address   string
	HistoryID string
}

type Watch struct {
	HistoryID string
	Expires   time.Time
}

// Message is what a provider returns for one message: headers and a preview.
// Bodies are never requested.
type Message struct {
	ID         string
	ThreadID   string
	ReceivedAt time.Time
	From       string
	Subject    string
	Snippet    string
}

type HistoryPage struct {
	MessageIDs []string
	Next       string
	// HistoryID is the mailbox's current position. Store it only after the last
	// page (Next empty); it is ahead of anything an earlier page covered.
	HistoryID string
	// LastID is the newest change record on this page. Stopping before the last
	// page, resume from here.
	LastID string
}

type Provider interface {
	Name() string
	AuthURL(state, challenge string) string
	Exchange(ctx context.Context, code, verifier string) (Tokens, error)
	// Refresh returns a fresh access token, or ErrRevoked.
	Refresh(ctx context.Context, refresh string) (Tokens, error)
	Profile(ctx context.Context, access string) (Profile, error)
	// Watch asks the provider to push change notifications. ok is false when
	// push is not configured; the reconcile scan then carries all syncing.
	Watch(ctx context.Context, access string) (w Watch, ok bool, err error)
	Stop(ctx context.Context, access string) error
	Revoke(ctx context.Context, token string) error
	History(ctx context.Context, access, start, page string) (HistoryPage, error)
	// Recent lists inbox message IDs from the last `days` days, newest first.
	Recent(ctx context.Context, access string, days, limit int) ([]string, error)
	Get(ctx context.Context, access, id string) (Message, error)
}
