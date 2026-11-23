package posting

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeScraper records its calls and answers with page or err.
type fakeScraper struct {
	name  string
	page  Page
	err   error
	calls *[]string
}

func (f fakeScraper) Scrape(_ context.Context, url string) (Page, error) {
	*f.calls = append(*f.calls, f.name)
	return f.page, f.err
}

// fakeSource serves settings that tests change between scrapes.
type fakeSource struct {
	mu       sync.Mutex
	settings Settings
	err      error
	loads    int
	keys     map[string]string
}

func (f *fakeSource) Settings(context.Context) (Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	return f.settings, f.err
}

func (f *fakeSource) Key(_ context.Context, id string) (string, error) {
	if k, ok := f.keys[id]; ok {
		return k, nil
	}
	return "", errors.New("key not found")
}

func (f *fakeSource) set(s Settings, err error) {
	f.mu.Lock()
	f.settings, f.err = s, err
	f.mu.Unlock()
}

// testChain builds a chain whose providers are fakes named by their base URL,
// each failing with errs[name] when one is given.
func testChain(src SettingsSource, errs map[string]error) (*Chain, *[]string) {
	var calls []string
	c := NewChain(src)
	c.build = func(_ context.Context, cfg ProviderConfig) (Scraper, error) {
		return fakeScraper{name: cfg.BaseURL, page: Page{Markdown: "from " + cfg.BaseURL}, err: errs[cfg.BaseURL], calls: &calls}, nil
	}
	return c, &calls
}

// providers names n providers a, b, c... through their base URL.
func providers(names ...string) []ProviderConfig {
	out := make([]ProviderConfig, len(names))
	for i, n := range names {
		out[i] = ProviderConfig{Kind: KindFirecrawl, BaseURL: n}
	}
	return out
}

func TestChainFallsBackInOrder(t *testing.T) {
	unreadable := &UnreadableError{Reason: "blocked"}
	src := &fakeSource{settings: Settings{Providers: providers("a", "b", "c")}}
	c, calls := testChain(src, map[string]error{"a": unreadable, "b": errors.New("boom")})
	page, err := c.Scrape(context.Background(), "https://example.com/job")
	if err != nil || page.Markdown != "from c" {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestChainStopsAtFirstSuccess(t *testing.T) {
	src := &fakeSource{settings: Settings{Providers: providers("a", "b")}}
	c, calls := testChain(src, nil)
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestChainReturnsLastErrorWhenAllFail(t *testing.T) {
	first, last := errors.New("first"), &UnreadableError{Reason: "last"}
	src := &fakeSource{settings: Settings{Providers: providers("a", "b")}}
	c, _ := testChain(src, map[string]error{"a": first, "b": last})
	_, err := c.Scrape(context.Background(), "https://example.com/job")
	var u *UnreadableError
	if !errors.As(err, &u) || u != last {
		t.Fatalf("err = %v, want the last provider's", err)
	}
}

func TestChainWithoutProvidersIsNotConfigured(t *testing.T) {
	src := &fakeSource{}
	c, calls := testChain(src, nil)
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if len(*calls) != 0 {
		t.Errorf("calls = %v", *calls)
	}
}

func TestChainRulesPickProvidersByHost(t *testing.T) {
	src := &fakeSource{settings: Settings{
		Providers: providers("a", "b", "c"),
		Rules: []Rule{
			{Host: "linkedin.com", Providers: []int{2, 1}},
			{Host: "www.linkedin.com", Providers: []int{0}}, // shadowed by the first match
			{Host: "boards.greenhouse.io", Providers: []int{1}},
		},
	}}
	for _, tc := range []struct {
		url  string
		want []string
	}{
		{"https://linkedin.com/jobs/1", []string{"c"}},
		{"https://www.linkedin.com/jobs/view/1", []string{"c"}},
		{"https://LinkedIn.com:8443/jobs/1", []string{"c"}},
		{"https://boards.greenhouse.io/acme/jobs/1", []string{"b"}},
		{"https://notlinkedin.com/jobs/1", []string{"a"}},
		{"https://linkedin.com.evil.test/jobs/1", []string{"a"}},
		{"https://greenhouse.io/jobs/1", []string{"a"}},
		{"https://example.com/jobs/1", []string{"a"}},
	} {
		c, calls := testChain(src, nil)
		if _, err := c.Scrape(context.Background(), tc.url); err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if !reflect.DeepEqual(*calls, tc.want) {
			t.Errorf("%s: calls = %v, want %v", tc.url, *calls, tc.want)
		}
	}
}

func TestChainRuleOrderIsTheFallbackOrder(t *testing.T) {
	src := &fakeSource{settings: Settings{
		Providers: providers("a", "b", "c"),
		Rules:     []Rule{{Host: "linkedin.com", Providers: []int{2, 0}}},
	}}
	c, calls := testChain(src, map[string]error{"c": errors.New("down")})
	if page, err := c.Scrape(context.Background(), "https://linkedin.com/jobs/1"); err != nil || page.Markdown != "from a" {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	if want := []string{"c", "a"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestChainCachesSettingsUntilTheTTL(t *testing.T) {
	src := &fakeSource{settings: Settings{Providers: providers("a")}}
	c, _ := testChain(src, nil)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	scrape := func() (Page, error) { return c.Scrape(context.Background(), "https://example.com/job") }

	if page, err := scrape(); err != nil || page.Markdown != "from a" {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	src.set(Settings{Providers: providers("b")}, nil)
	now = now.Add(settingsTTL - time.Second)
	if page, _ := scrape(); page.Markdown != "from a" {
		t.Errorf("settings reloaded before the TTL: %q", page.Markdown)
	}
	now = now.Add(2 * time.Second)
	if page, _ := scrape(); page.Markdown != "from b" {
		t.Errorf("settings not reloaded after the TTL: %q", page.Markdown)
	}
	if src.loads != 2 {
		t.Errorf("loads = %d, want 2", src.loads)
	}
}

func TestChainKeepsLastSettingsWhenReloadFails(t *testing.T) {
	src := &fakeSource{settings: Settings{Providers: providers("a")}}
	c, _ := testChain(src, nil)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil {
		t.Fatal(err)
	}
	src.set(Settings{}, errors.New("database down"))
	now = now.Add(time.Minute)
	if page, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil || page.Markdown != "from a" {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
}

func TestChainFailsWhenSettingsNeverLoaded(t *testing.T) {
	src := &fakeSource{err: errors.New("database down")}
	c, _ := testChain(src, nil)
	_, err := c.Scrape(context.Background(), "https://example.com/job")
	if err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("err = %v", err)
	}
	var u *UnreadableError
	if errors.As(err, &u) || errors.Is(err, ErrNotConfigured) {
		t.Errorf("a settings failure is a server error: %v", err)
	}
}

func TestChainStopsWhenTheContextEnds(t *testing.T) {
	src := &fakeSource{settings: Settings{Providers: providers("a", "b")}}
	c, calls := testChain(src, map[string]error{"a": context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Scrape(ctx, "https://example.com/job"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"a"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestChainUnbuildableProviderFallsThrough(t *testing.T) {
	src := &fakeSource{settings: Settings{Providers: []ProviderConfig{
		{Kind: KindFirecrawl, BaseURL: "https://one.test", KeyID: "gone"},
		{Kind: KindFirecrawl, BaseURL: "https://two.test", KeyID: "k"},
	}}, keys: map[string]string{"k": "secret"}}
	c := NewChain(src)
	var gotKey string
	_, providers, err := c.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if providers[0].err == nil || providers[1].err != nil {
		t.Fatalf("provider errors = %v, %v", providers[0].err, providers[1].err)
	}
	if f, ok := providers[1].scraper.(*Firecrawl); !ok || f.baseURL != "https://two.test" {
		t.Fatalf("scraper = %#v", providers[1].scraper)
	} else {
		gotKey = f.key
	}
	if gotKey != "secret" {
		t.Errorf("key = %q", gotKey)
	}
}

func TestSettingsValidate(t *testing.T) {
	ok := Settings{
		Providers: []ProviderConfig{{Kind: KindFirecrawl}, {Kind: KindFirecrawl, BaseURL: "http://127.0.0.1:3002"}},
		Rules:     []Rule{{Host: "linkedin.com", Providers: []int{1, 0}}, {Host: "boards.greenhouse.io", Providers: []int{0}}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid settings: %v", err)
	}
	if err := (Settings{}).Validate(); err != nil {
		t.Fatalf("empty settings: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Settings)
		field  string
	}{
		"unknown kind":   {func(s *Settings) { s.Providers[0].Kind = "other" }, "providers[0].kind"},
		"relative url":   {func(s *Settings) { s.Providers[1].BaseURL = "127.0.0.1:3002" }, "providers[1].base_url"},
		"ftp url":        {func(s *Settings) { s.Providers[1].BaseURL = "ftp://example.com" }, "providers[1].base_url"},
		"scheme in host": {func(s *Settings) { s.Rules[0].Host = "https://linkedin.com" }, "rules[0].host"},
		"uppercase host": {func(s *Settings) { s.Rules[0].Host = "LinkedIn.com" }, "rules[0].host"},
		"host with path": {func(s *Settings) { s.Rules[0].Host = "linkedin.com/jobs" }, "rules[0].host"},
		"host with port": {func(s *Settings) { s.Rules[0].Host = "linkedin.com:443" }, "rules[0].host"},
		"empty host":     {func(s *Settings) { s.Rules[0].Host = "" }, "rules[0].host"},
		"duplicate host": {func(s *Settings) { s.Rules[1].Host = "linkedin.com" }, "rules[1].host"},
		"no providers":   {func(s *Settings) { s.Rules[0].Providers = nil }, "rules[0].providers"},
		"unknown ref":    {func(s *Settings) { s.Rules[0].Providers = []int{2} }, "rules[0].providers"},
		"negative ref":   {func(s *Settings) { s.Rules[0].Providers = []int{-1} }, "rules[0].providers"},
		"repeated ref":   {func(s *Settings) { s.Rules[0].Providers = []int{0, 0} }, "rules[0].providers"},
	} {
		s := Settings{Providers: append([]ProviderConfig(nil), ok.Providers...), Rules: nil}
		for _, r := range ok.Rules {
			s.Rules = append(s.Rules, Rule{Host: r.Host, Providers: append([]int(nil), r.Providers...)})
		}
		tc.mutate(&s)
		var e *SettingsError
		if err := s.Validate(); !errors.As(err, &e) || e.Field != tc.field {
			t.Errorf("%s: err = %v, want field %s", name, err, tc.field)
		}
	}
}
