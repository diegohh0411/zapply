package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// ScraperSettings returns the scraper settings as platform.scraper_settings()
// shapes them. It needs no signed-in user: posting pages are read for every
// member by the same configured chain.
func (s *Store) ScraperSettings(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT platform.scraper_settings()`).Scan(&raw)
	return raw, err
}

// ScraperKey returns a Firecrawl system key sealed as the key ring stores it.
// ok is false when no Firecrawl key has that ID.
func (s *Store) ScraperKey(ctx context.Context, id uuid.UUID) (ciphertext []byte, version string, ok bool, err error) {
	rows, err := s.pool.Query(ctx, `SELECT ciphertext,encryption_version FROM platform.scraper_key($1)`, id)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&ciphertext, &version); err != nil {
			return nil, "", false, err
		}
		ok = true
	}
	return ciphertext, version, ok, rows.Err()
}

// SealedKey is a system key as the database stores it.
type SealedKey struct {
	ID          uuid.UUID
	Ciphertext  []byte
	Version     string
	Fingerprint string
}

// SeedScraperSettings imports an environment's scraper providers (and the
// Firecrawl key they use, when key is not nil) as the saved settings. It does
// nothing and reports false once settings have been saved.
func (s *Store) SeedScraperSettings(ctx context.Context, providers json.RawMessage, key *SealedKey) (bool, error) {
	var id *uuid.UUID
	var cipher []byte
	var version, fingerprint *string
	if key != nil {
		id, cipher, version, fingerprint = &key.ID, key.Ciphertext, &key.Version, &key.Fingerprint
	}
	var seeded bool
	err := s.pool.QueryRow(ctx, `SELECT platform.seed_scraper_settings($1::jsonb,$2,$3,$4,$5)`, string(providers), id, cipher, version, fingerprint).Scan(&seeded)
	return seeded, err
}
