package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OpenScrapeCharge opens the ledger charge for one scraper call made for
// member u (platform.open_scrape_charge). It returns why the member cannot pay
// for it, or "" once the charge is open.
func (s *Store) OpenScrapeCharge(ctx context.Context, u, id uuid.UUID, provider, host string, creditPrice, cost int64, deadline time.Time) (string, error) {
	var reason string
	err := s.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.open_scrape_charge($1,$2,$3,$4,$5,$6)`, id, provider, host, creditPrice, cost, deadline).Scan(&reason)
	})
	return reason, err
}

// CloseScrapeCharge settles member u's scrape charge at the credits used.
func (s *Store) CloseScrapeCharge(ctx context.Context, u, id uuid.UUID, credits int) error {
	return s.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.close_scrape_charge($1,$2)`, id, credits)
		return err
	})
}
