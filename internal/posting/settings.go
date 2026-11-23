package posting

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Scraper provider kinds.
const (
	// KindFirecrawl is a Firecrawl v2 API, hosted or self-hosted.
	KindFirecrawl = "firecrawl"
	// KindCrawl4AI is a self-hosted Crawl4AI Docker server.
	KindCrawl4AI = "crawl4ai"
)

// Bounds on scraper settings, matching the database's checks.
const (
	maxProviders = 10
	maxRules     = 100
)

// ProviderConfig is one scraper in the chain. KeyID names the system key sent
// to the API ("" sends none) and must be a key of the provider's own kind;
// BaseURL is the API's address ("" is Firecrawl's hosted API; Crawl4AI has no
// hosted API and needs one).
type ProviderConfig struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
}

// Rule replaces the default order for a host and its subdomains. Providers are
// positions in Settings.Providers, in the order to try them.
type Rule struct {
	Host      string `json:"host"`
	Providers []int  `json:"providers"`
}

// Settings is the owner's scraper configuration: providers tried in order,
// and rules that pick other providers (or another order) for some sites.
type Settings struct {
	Providers []ProviderConfig `json:"providers"`
	Rules     []Rule           `json:"rules"`
}

// Order returns the positions of the providers to try for host: those of the
// first rule that covers it, else every provider in order.
func (s Settings) Order(host string) []int {
	host = normalizeHost(host)
	for _, r := range s.Rules {
		if host == r.Host || strings.HasSuffix(host, "."+r.Host) {
			return r.Providers
		}
	}
	all := make([]int, len(s.Providers))
	for i := range all {
		all[i] = i
	}
	return all
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// SettingsError is a setting the owner can fix. Field names the offending
// part, as the API reports it.
type SettingsError struct{ Field, Message string }

func (e *SettingsError) Error() string { return e.Field + ": " + e.Message }

var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Validate returns a *SettingsError for the first setting that cannot work.
func (s Settings) Validate() error {
	if len(s.Providers) > maxProviders {
		return &SettingsError{"providers", fmt.Sprintf("use at most %d providers", maxProviders)}
	}
	if len(s.Rules) > maxRules {
		return &SettingsError{"rules", fmt.Sprintf("use at most %d site rules", maxRules)}
	}
	for i, p := range s.Providers {
		field := fmt.Sprintf("providers[%d]", i)
		if p.Kind != KindFirecrawl && p.Kind != KindCrawl4AI {
			return &SettingsError{field + ".kind", "select a supported scraper"}
		}
		if p.Kind == KindCrawl4AI && p.BaseURL == "" {
			return &SettingsError{field + ".base_url", "Crawl4AI has no hosted API: enter your server's address"}
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(p.BaseURL, " \t\r\n") || len(p.BaseURL) > 2048 {
				return &SettingsError{field + ".base_url", "must be an http(s) URL"}
			}
		}
	}
	seen := map[string]bool{}
	for i, r := range s.Rules {
		field := fmt.Sprintf("rules[%d]", i)
		if len(r.Host) > 253 || !hostname.MatchString(r.Host) {
			return &SettingsError{field + ".host", "must be a lowercase hostname such as linkedin.com, without a scheme, port or path"}
		}
		if seen[r.Host] {
			return &SettingsError{field + ".host", "another rule already covers this site"}
		}
		seen[r.Host] = true
		if len(r.Providers) == 0 || len(r.Providers) > maxProviders {
			return &SettingsError{field + ".providers", "choose at least one provider"}
		}
		for j, n := range r.Providers {
			if n < 0 || n >= len(s.Providers) {
				return &SettingsError{field + ".providers", "choose providers from the list"}
			}
			if slices.Contains(r.Providers[:j], n) {
				return &SettingsError{field + ".providers", "a provider can be listed once per rule"}
			}
		}
	}
	return nil
}
