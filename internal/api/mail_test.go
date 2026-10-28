package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// pushVerifier accepts two fixed tokens, standing in for Google's signed OIDC tokens.
type pushVerifier struct{}

func (pushVerifier) Verify(_ context.Context, token string) (auth.Token, error) {
	switch token {
	case "pubsub":
		return auth.Token{Principal: auth.Principal{Issuer: "https://accounts.google.com", Subject: "1", Email: "push@zapply.iam.gserviceaccount.com"}}, nil
	case "stranger":
		return auth.Token{Principal: auth.Principal{Issuer: "https://accounts.google.com", Subject: "2", Email: "someone@else.iam.gserviceaccount.com"}}, nil
	}
	return auth.Token{}, errors.New("bad token")
}

func mailServer(t *testing.T, google config.Google) (alice, bob *client, base string) {
	t.Helper()
	db := testdb.New(t)
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store: db.Store, Keys: ring, Google: google, PushVerifier: pushVerifier{},
		Verifier: auth.NewDev(secret, "authenticated"),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	mk := func(sub string) *client {
		tok, _ := auth.MintDev(secret, "authenticated", sub, time.Hour)
		return &client{t: t, base: srv.URL, token: tok, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	return mk("alice"), mk("bob"), srv.URL
}

func post(t *testing.T, url, auth, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func notification(address string) string {
	data, _ := json.Marshal(map[string]string{"emailAddress": address, "historyId": "123"})
	body, _ := json.Marshal(map[string]any{"message": map[string]string{"data": base64.StdEncoding.EncodeToString(data)}})
	return string(body)
}

func TestMailIsOffUntilConfigured(t *testing.T) {
	alice, _, base := mailServer(t, config.Google{})
	list := alice.must(200, "GET", "/v1/mail/accounts", nil)
	if list.Body["configured"] != false || len(list.Body["items"].([]any)) != 0 {
		t.Fatalf("accounts %v", list.Body)
	}
	alice.must(503, "POST", "/v1/mail/accounts/google/start", nil)
	if res := post(t, base+"/hooks/google/pubsub", "pubsub", notification("a@b.test")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("push on an unconfigured server = %d", res.StatusCode)
	}
	res, err := alice.http.Get(base + "/oauth/google/callback?state=x&code=y")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/mail?oauth_code=y&oauth_state=x" {
		t.Fatalf("callback = %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	alice.must(503, "POST", "/v1/mail/accounts/google/complete", map[string]any{"state": "x", "code": "y"})
}

func TestMailConnectAndPush(t *testing.T) {
	google := config.Google{ClientID: "client", ClientSecret: "secret", PublicURL: "https://zapply.example",
		PubSubTopic: "projects/p/topics/t", PubSubServiceAccount: "push@zapply.iam.gserviceaccount.com", ReconcileInterval: time.Hour}
	alice, bob, base := mailServer(t, google)

	start := alice.must(200, "POST", "/v1/mail/accounts/google/start", nil)
	url, _ := start.Body["url"].(string)
	for _, want := range []string{"https://accounts.google.com/", "client_id=client", "gmail.readonly", "code_challenge_method=S256",
		"redirect_uri=https%3A%2F%2Fzapply.example%2Foauth%2Fgoogle%2Fcallback", "access_type=offline"} {
		if !strings.Contains(url, want) {
			t.Fatalf("consent URL lacks %q: %s", want, url)
		}
	}
	list := alice.must(200, "GET", "/v1/mail/accounts", nil)
	if list.Body["configured"] != true || len(list.Body["items"].([]any)) != 0 {
		t.Fatalf("accounts %v", list.Body)
	}
	bob.must(404, "PATCH", "/v1/mail/accounts/00000000-0000-0000-0000-000000000000", map[string]any{"paused": true})
	bob.must(404, "DELETE", "/v1/mail/accounts/00000000-0000-0000-0000-000000000000", nil)
	alice.must(422, "GET", "/v1/mail/messages?category=other", nil)
	if msgs := alice.must(200, "GET", "/v1/mail/messages?category=interview", nil); len(msgs.Body["items"].([]any)) != 0 {
		t.Fatalf("messages %v", msgs.Body)
	}

	// The callback only hands the result to the app, which finishes the flow as
	// the signed-in member.
	for query, want := range map[string]string{
		"state=nope&code=c&scope=x": "/mail?oauth_code=c&oauth_state=nope",
		"error=access_denied":       "/mail?error=denied",
		"state=only":                "/mail?error=expired",
	} {
		res, err := alice.http.Get(base + "/oauth/google/callback?" + query)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("callback %s = %d %s, want %s", query, res.StatusCode, res.Header.Get("Location"), want)
		}
	}
	// Finishing needs a state this member started: an unknown one, or one started
	// by someone else, is refused.
	done := alice.must(422, "POST", "/v1/mail/accounts/google/complete", map[string]any{"state": "nope", "code": "c"})
	if !hasFieldError(done, "state") {
		t.Fatalf("complete: %v", done.Body)
	}
	bobStart := bob.must(200, "POST", "/v1/mail/accounts/google/start", nil)
	bobURL, _ := bobStart.Body["url"].(string)
	forwarded := bobURL[strings.Index(bobURL, "state=")+len("state="):]
	if i := strings.Index(forwarded, "&"); i >= 0 {
		forwarded = forwarded[:i]
	}
	alice.must(422, "POST", "/v1/mail/accounts/google/complete", map[string]any{"state": forwarded, "code": "c"})
	alice.must(422, "POST", "/v1/mail/accounts/google/complete", map[string]any{"code": "c"})

	// Pub/Sub pushes are accepted only from the subscription's service account.
	endpoint := base + "/hooks/google/pubsub"
	for name, c := range map[string]struct {
		token, body string
		want        int
	}{
		"no token":        {"", notification("a@b.test"), http.StatusUnauthorized},
		"bad token":       {"forged", notification("a@b.test"), http.StatusUnauthorized},
		"wrong account":   {"stranger", notification("a@b.test"), http.StatusUnauthorized},
		"unknown mailbox": {"pubsub", notification("nobody@b.test"), http.StatusNoContent},
		"malformed":       {"pubsub", `{"message":{"data":"!!"}}`, http.StatusNoContent},
		"not json":        {"pubsub", `nope`, http.StatusNoContent},
	} {
		if res := post(t, endpoint, c.token, c.body); res.StatusCode != c.want {
			t.Errorf("%s: push = %d, want %d", name, res.StatusCode, c.want)
		}
	}
}
