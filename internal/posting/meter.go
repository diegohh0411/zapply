package posting

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Meter bills each provider call to the member the scrape is for. Every call
// is a charge on the member's ledger: Open holds its most expensive outcome
// against their plan, and Close settles it at the credits the provider used.
type Meter interface {
	// Open reserves cost picodollars for one call to provider. It returns why
	// the member cannot pay ("" when they can); a free call (cost 0) is
	// recorded whatever their plan.
	Open(ctx context.Context, id uuid.UUID, provider, host string, creditPrice, cost int64, deadline time.Time) (reason string, err error)
	// Close settles the call at credits provider credits.
	Close(ctx context.Context, id uuid.UUID, credits int) error
}

// maxCredits is the most one scrape can use: Firecrawl's automatic proxy
// retries a blocked page through its stealth proxy, which costs 5 credits.
func maxCredits(kind string) int {
	if kind == KindFirecrawl {
		return 5
	}
	return 1
}

// creditsUsed is what a provider charged for a call that ended in err: what
// it reported, one credit for a page it read, nothing for a page it answered
// it could not read, and the most it could have charged when it did not
// answer (the call may have run).
func creditsUsed(kind string, page Page, err error) int {
	var unreadable *UnreadableError
	switch {
	case err == nil && page.Credits > 0:
		return page.Credits
	case err == nil:
		return 1
	case errors.As(err, &unreadable):
		return 0
	case unanswered(err):
		return maxCredits(kind)
	}
	return 0
}

// unanswered reports whether err left the provider's answer unknown.
func unanswered(err error) bool {
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// BudgetError is a scrape the member cannot pay for. Reason is the ledger's:
// membership_required, membership_suspended, request_exceeds_plan or
// usage_limit.
type BudgetError struct{ Reason string }

func (e *BudgetError) Error() string { return "scrape not affordable: " + e.Reason }

// unreadable says why no provider could be paid for, in the member's terms.
func (e *BudgetError) unreadable() *UnreadableError {
	switch e.Reason {
	case "membership_required":
		return &UnreadableError{Reason: "reading pages needs a plan; ask the owner to add you to one"}
	case "membership_suspended":
		return &UnreadableError{Reason: "your membership is suspended"}
	case "request_exceeds_plan":
		return &UnreadableError{Reason: "reading this page costs more than your plan allows"}
	}
	return &UnreadableError{Reason: "your plan's usage limit is reached"}
}
