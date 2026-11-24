package posting

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/diegohh0411/zapply/internal/logx"
)

// SettingsSource supplies the owner's scraper settings and the keys they name.
type SettingsSource interface {
	// Settings returns the saved settings; none saved yet is the zero Settings.
	Settings(ctx context.Context) (Settings, error)
	// Key returns the plaintext of the scraper system key id.
	Key(ctx context.Context, id string) (string, error)
}

// settingsTTL is how long a Chain keeps the settings it loaded, so an owner's
// edits apply within seconds without a query per scrape.
const settingsTTL = 10 * time.Second

// Chain scrapes through the providers the owner configured: the first rule
// that covers the page's host picks them, else all of them, in order. A page
// one provider cannot read is tried on the next.
type Chain struct {
	src   SettingsSource
	ttl   time.Duration
	now   func() time.Time
	build func(ctx context.Context, c ProviderConfig) (Scraper, error)

	mu       sync.Mutex
	loaded   time.Time
	settings Settings
	scrapers []provider
	ready    bool
}

// provider is a configured scraper, or why it could not be built.
type provider struct {
	kind    string
	scraper Scraper
	err     error
}

// NewChain returns a Chain over src.
func NewChain(src SettingsSource) *Chain {
	c := &Chain{src: src, ttl: settingsTTL, now: time.Now}
	c.build = c.buildProvider
	return c
}

func (c *Chain) buildProvider(ctx context.Context, cfg ProviderConfig) (Scraper, error) {
	switch cfg.Kind {
	case KindFirecrawl:
		var key string
		if cfg.KeyID != "" {
			var err error
			if key, err = c.src.Key(ctx, cfg.KeyID); err != nil {
				return nil, fmt.Errorf("firecrawl key: %w", err)
			}
		}
		return NewFirecrawl(cfg.BaseURL, key), nil
	case KindCrawl4AI:
		var token string
		if cfg.KeyID != "" {
			var err error
			if token, err = c.src.Key(ctx, cfg.KeyID); err != nil {
				return nil, fmt.Errorf("crawl4ai key: %w", err)
			}
		}
		return NewCrawl4AI(cfg.BaseURL, token), nil
	case KindEdge:
		q, ok := c.src.(EdgeQueue)
		if !ok {
			return nil, errors.New("edge workers are not available")
		}
		return NewEdge(q), nil
	}
	return nil, fmt.Errorf("unknown scraper %q", cfg.Kind)
}

// current returns the providers and settings, reloading them once they are
// older than the TTL. A failed reload keeps serving the last good settings.
func (c *Chain) current(ctx context.Context) (Settings, []provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready && c.now().Sub(c.loaded) < c.ttl {
		return c.settings, c.scrapers, nil
	}
	s, err := c.src.Settings(ctx)
	if err != nil {
		if c.ready {
			logx.From(ctx).WarnContext(ctx, "scraper settings not reloaded", "error", err)
			c.loaded = c.now()
			return c.settings, c.scrapers, nil
		}
		return Settings{}, nil, fmt.Errorf("load scraper settings: %w", err)
	}
	built := make([]provider, len(s.Providers))
	for i, cfg := range s.Providers {
		built[i].kind = cfg.Kind
		built[i].scraper, built[i].err = c.build(ctx, cfg)
	}
	c.settings, c.scrapers, c.loaded, c.ready = s, built, c.now(), true
	return s, built, nil
}

// Scrape reads rawURL through the providers configured for its host. It
// returns the last provider's error when all fail, and ErrNotConfigured when
// there is none to try.
func (c *Chain) Scrape(ctx context.Context, rawURL string) (Page, error) {
	settings, providers, err := c.current(ctx)
	if err != nil {
		return Page{}, err
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	order := settings.Order(host)
	if len(order) == 0 {
		return Page{}, ErrNotConfigured
	}
	var last error
	for n, i := range order {
		if i < 0 || i >= len(providers) {
			continue
		}
		p := providers[i]
		var page Page
		if p.err != nil {
			last = p.err
		} else if page, last = p.scraper.Scrape(ctx, rawURL); last == nil {
			return page, nil
		}
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		logx.From(ctx).WarnContext(ctx, "scraper failed", "provider", p.kind, "position", i, "host", host, "fallback", n+1 < len(order), "error", last)
	}
	return Page{}, last
}

var _ Scraper = (*Chain)(nil)
