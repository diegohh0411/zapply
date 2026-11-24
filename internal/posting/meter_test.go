package posting

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// meteredSource is a settings source that bills scrapes: it refuses those it
// is told to and records what each charge settled at.
type meteredSource struct {
	fakeSource
	refuse  map[string]string // "<kind>:<cost>" → reason
	opened  []int64           // reserved costs, in order
	settled map[uuid.UUID]int
	hosts   map[uuid.UUID]string
	noUser  bool
}

func (m *meteredSource) Open(_ context.Context, id uuid.UUID, provider, host string, price, cost int64, _ time.Time) (string, error) {
	if m.noUser {
		return "", ErrNoMember
	}
	m.opened = append(m.opened, cost)
	m.hosts[id] = host
	return m.refuse[provider+":"+itoa(cost)], nil
}

func (m *meteredSource) Close(_ context.Context, id uuid.UUID, credits int) error {
	m.settled[id] = credits
	return nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestChainBillsEachProviderCall(t *testing.T) {
	src := &meteredSource{settled: map[uuid.UUID]int{}, hosts: map[uuid.UUID]string{}, refuse: map[string]string{}}
	src.settings = Settings{Providers: []ProviderConfig{
		{Kind: KindFirecrawl, BaseURL: "a", CreditUSD: "0.002"},
		{Kind: KindCrawl4AI, BaseURL: "b"},
	}}
	c, calls := testChain(src, map[string]error{"a": &UnreadableError{Reason: "blocked"}})
	if _, err := c.Scrape(context.Background(), "https://jobs.example.com/1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v", *calls)
	}
	// Firecrawl reserves its stealth retry (5 credits); Crawl4AI is free.
	if want := []int64{5 * 2_000_000_000, 0}; !reflect.DeepEqual(src.opened, want) {
		t.Fatalf("reserved %v, want %v", src.opened, want)
	}
	got := []int{}
	for _, n := range src.settled {
		got = append(got, n)
	}
	if len(got) != 2 || got[0]+got[1] != 1 {
		t.Fatalf("settled %v: the unreadable page is free, the read one 1 credit", src.settled)
	}
	for _, h := range src.hosts {
		if h != "jobs.example.com" {
			t.Fatalf("host = %q", h)
		}
	}
}

func TestChainSkipsWhatTheMemberCannotPay(t *testing.T) {
	src := &meteredSource{settled: map[uuid.UUID]int{}, hosts: map[uuid.UUID]string{}, refuse: map[string]string{"firecrawl:5000000000": "usage_limit"}}
	src.settings = Settings{Providers: []ProviderConfig{{Kind: KindFirecrawl, BaseURL: "a", CreditUSD: "0.001"}}}
	c, calls := testChain(src, nil)
	_, err := c.Scrape(context.Background(), "https://example.com/job")
	var u *UnreadableError
	if !errors.As(err, &u) || u.Reason != "your plan's usage limit is reached" || len(*calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, *calls)
	}
	// A free provider after it still reads the page.
	src.settings.Providers = append(src.settings.Providers, ProviderConfig{Kind: KindEdge})
	c, calls = testChain(src, nil)
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil || len(*calls) != 1 {
		t.Fatalf("err = %v, calls = %v", err, *calls)
	}
}

func TestChainWithoutMemberIsNotBilled(t *testing.T) {
	src := &meteredSource{noUser: true, settled: map[uuid.UUID]int{}, hosts: map[uuid.UUID]string{}}
	src.settings = Settings{Providers: []ProviderConfig{{Kind: KindFirecrawl, BaseURL: "a", CreditUSD: "1"}}}
	c, _ := testChain(src, nil)
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil || len(src.settled) != 0 {
		t.Fatalf("err = %v, settled = %v", err, src.settled)
	}
}

func TestCreditsUsed(t *testing.T) {
	transport := &url.Error{Op: "Post", URL: "https://api.firecrawl.dev", Err: errors.New("reset")}
	for _, tc := range []struct {
		name string
		page Page
		err  error
		want int
	}{
		{"reported", Page{Credits: 5}, nil, 5},
		{"read", Page{}, nil, 1},
		{"unreadable", Page{}, &UnreadableError{Reason: "x"}, 0},
		{"no answer", Page{}, transport, 5},
		{"refused", Page{}, errors.New("scrape: status 402"), 0},
	} {
		if got := creditsUsed(KindFirecrawl, tc.page, tc.err); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSettingsValidateCreditPrice(t *testing.T) {
	for _, tc := range []struct {
		p  ProviderConfig
		ok bool
	}{
		{ProviderConfig{Kind: KindFirecrawl, CreditUSD: "0.0053"}, true},
		{ProviderConfig{Kind: KindFirecrawl, CreditUSD: "-1"}, false},
		{ProviderConfig{Kind: KindFirecrawl, CreditUSD: "1e3"}, false},
		{ProviderConfig{Kind: KindEdge, CreditUSD: "0.01"}, false},
	} {
		err := Settings{Providers: []ProviderConfig{tc.p}}.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.p, err)
		}
	}
	if p := (ProviderConfig{CreditUSD: "0.002"}).CreditPrice(); p != 2_000_000_000 {
		t.Errorf("price = %d", p)
	}
}
