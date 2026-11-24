package posting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
)

// DBSource reads the owner's scraper settings from the database, and opens the
// Firecrawl keys they name with the system key ring.
type DBSource struct {
	store *store.Store
	keys  *secrets.Ring
}

// NewDBSource returns a settings source over st. keys opens Firecrawl keys;
// it may be nil when none is configured.
func NewDBSource(st *store.Store, keys *secrets.Ring) *DBSource {
	return &DBSource{store: st, keys: keys}
}

// Settings returns the saved settings.
func (d *DBSource) Settings(ctx context.Context) (Settings, error) {
	raw, err := d.store.ScraperSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("decode scraper settings: %w", err)
	}
	return s, nil
}

// Key returns the plaintext of the scraper (Firecrawl or Crawl4AI) system key id.
func (d *DBSource) Key(ctx context.Context, id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", errors.New("invalid key ID")
	}
	cipher, version, ok, err := d.store.ScraperKey(ctx, u)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("key not found")
	}
	return d.keys.Decrypt(u.String(), version, cipher)
}

// SeedFromEnv imports an environment's scraper (the first boot of a deployment
// that predates owner-managed settings) as the saved settings: one provider,
// and its key as a Firecrawl system key. It does nothing once settings have
// been saved, or when baseURL and key are both empty.
func SeedFromEnv(ctx context.Context, st *store.Store, ring *secrets.Ring, baseURL, key string) (bool, error) {
	if baseURL == "" && key == "" {
		return false, nil
	}
	cfg := ProviderConfig{Kind: KindFirecrawl, BaseURL: baseURL}
	var sealed *store.SealedKey
	if key != "" {
		id, err := uuid.NewV7()
		if err != nil {
			return false, err
		}
		cipher, version, err := ring.Encrypt(id.String(), key)
		if err != nil {
			return false, fmt.Errorf("seal Firecrawl key: %w", err)
		}
		cfg.KeyID = id.String()
		sealed = &store.SealedKey{ID: id, Ciphertext: cipher, Version: version, Fingerprint: secrets.Fingerprint(key)}
	}
	providers, err := json.Marshal([]ProviderConfig{cfg})
	if err != nil {
		return false, err
	}
	return st.SeedScraperSettings(ctx, providers, sealed)
}

// Open and Close make the source the chain's Meter: scrapes are billed to the
// signed-in member.
func (d *DBSource) Open(ctx context.Context, id uuid.UUID, provider, host string, creditPrice, cost int64, deadline time.Time) (string, error) {
	u, ok := auth.UserIDFrom(ctx)
	if !ok || u == uuid.Nil {
		return "", ErrNoMember
	}
	return d.store.OpenScrapeCharge(ctx, u, id, provider, host, creditPrice, cost, deadline)
}

func (d *DBSource) Close(ctx context.Context, id uuid.UUID, credits int) error {
	u, ok := auth.UserIDFrom(ctx)
	if !ok {
		return ErrNoMember
	}
	return d.store.CloseScrapeCharge(ctx, u, id, credits)
}

var _ Meter = (*DBSource)(nil)

// EnqueueScrapeJob and ScrapeJobResult make the source the edge job queue.
func (d *DBSource) EnqueueScrapeJob(ctx context.Context, url, host string, ttlSeconds int) (uuid.UUID, bool, error) {
	return d.store.EnqueueScrapeJob(ctx, url, host, ttlSeconds)
}

func (d *DBSource) ScrapeJobResult(ctx context.Context, id uuid.UUID, cancel bool) (store.ScrapeOutcome, error) {
	return d.store.ScrapeJobResult(ctx, id, cancel)
}

var _ EdgeQueue = (*DBSource)(nil)
