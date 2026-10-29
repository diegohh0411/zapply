package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/mail"
)

func server(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Config{ClientID: "id", ClientSecret: "secret", RedirectURL: "https://app.test/oauth/google/callback", Topic: "projects/p/topics/t"}).
		WithEndpoints(srv.URL+"/auth", srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/gmail")
}

func TestAuthURLUsesPKCEAndTheNarrowScope(t *testing.T) {
	u, err := url.Parse(New(Config{ClientID: "id", RedirectURL: "https://app.test/cb"}).AuthURL("st", "chal"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != Scope || q.Get("access_type") != "offline" || q.Get("prompt") != "select_account consent" || q.Get("code_challenge") != "chal" ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") != "st" || q.Get("client_id") != "id" {
		t.Fatalf("auth query %v", q)
	}
}

func TestExchangeRequiresTheMailScopeAndARefreshToken(t *testing.T) {
	for name, body := range map[string]string{
		"narrowed":   `{"access_token":"a","refresh_token":"r","expires_in":3600,"scope":"openid email"}`,
		"no refresh": `{"access_token":"a","expires_in":3600,"scope":"` + Scope + `"}`,
	} {
		c := server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := c.Exchange(context.Background(), "code", "verifier"); err == nil {
			t.Fatalf("%s grant accepted", name)
		}
	}
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "verifier" || r.Form.Get("client_secret") != "secret" {
			t.Errorf("token form %v", r.Form)
		}
		io.WriteString(w, `{"access_token":"a","refresh_token":"r","expires_in":3600,"scope":"`+Scope+`"}`)
	})
	tok, err := c.Exchange(context.Background(), "code", "verifier")
	if err != nil || tok.Refresh != "r" || tok.Access != "a" || tok.Expires.IsZero() {
		t.Fatalf("exchange %+v %v", tok, err)
	}
}

func TestRefreshMapsInvalidGrantToRevoked(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	})
	if _, err := c.Refresh(context.Background(), "r"); !errors.Is(err, mail.ErrRevoked) {
		t.Fatalf("got %v", err)
	}
	c = server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := c.Refresh(context.Background(), "r"); err == nil || errors.Is(err, mail.ErrRevoked) {
		t.Fatalf("a server error must be retried, not treated as revoked: %v", err)
	}
}

func TestGetAsksOnlyForHeadersAndSnippet(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("format") != "full" || q.Get("fields") != "id,threadId,internalDate,snippet,payload/headers" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s %v", r.URL, r.Header)
		}
		io.WriteString(w, `{"id":"m1","threadId":"t1","internalDate":"1700000000000","snippet":"Don&#39;t miss it &amp; more",
 "payload":{"headers":[{"name":"From","value":"=?UTF-8?B?w4FuZ2Vs?= <a@b.test>"},{"name":"Subject","value":"Interview"},{"name":"X-Other","value":"ignored"}]}}`)
	})
	m, err := c.Get(context.Background(), "tok", "m1")
	if err != nil || m.ID != "m1" || m.ThreadID != "t1" || m.Subject != "Interview" || m.From != "Ángel <a@b.test>" ||
		m.Snippet != "Don't miss it & more" || m.ReceivedAt.Unix() != 1700000000 {
		t.Fatalf("message %+v %v", m, err)
	}
	c = server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err = c.Get(context.Background(), "tok", "gone"); !errors.Is(err, mail.ErrMessageGone) {
		t.Fatalf("got %v", err)
	}
}

func TestQuotaErrorsAreDistinguishedFromOtherForbiddenResponses(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		limited bool
	}{
		{403, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, true},
		{403, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, true},
		{429, `{}`, true},
		{403, `{"error":{"errors":[{"reason":"accessNotConfigured"}]}}`, false},
	} {
		c := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := c.Profile(context.Background(), "token")
		if err == nil || errors.Is(err, mail.ErrRateLimited) != tc.limited {
			t.Fatalf("status %d body %s: %v", tc.status, tc.body, err)
		}
	}
}

func TestHistoryReadsInboxAdditionsAndReportsExpiry(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("startHistoryId") == "old" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if q.Get("historyTypes") != "messageAdded" || q.Get("labelId") != "INBOX" {
			t.Errorf("query %v", q)
		}
		io.WriteString(w, `{"history":[{"messagesAdded":[{"message":{"id":"a","labelIds":["INBOX"]}},{"message":{"id":"b","labelIds":["SENT"]}}]}],"nextPageToken":"p2","historyId":"99"}`)
	})
	p, err := c.History(context.Background(), "tok", "10", "")
	if err != nil || len(p.MessageIDs) != 1 || p.MessageIDs[0] != "a" || p.Next != "p2" || p.HistoryID != "99" {
		t.Fatalf("history %+v %v", p, err)
	}
	if _, err = c.History(context.Background(), "tok", "old", ""); !errors.Is(err, mail.ErrHistoryExpired) {
		t.Fatalf("got %v", err)
	}
}

func TestWatchNeedsATopicAndWatchesTheInbox(t *testing.T) {
	var body map[string]any
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"historyId":"5","expiration":"1700000000000"}`)
	})
	w, ok, err := c.Watch(context.Background(), "tok")
	if err != nil || !ok || w.HistoryID != "5" || w.Expires.Unix() != 1700000000 || body["topicName"] != "projects/p/topics/t" {
		t.Fatalf("watch %+v %v %v %v", w, ok, err, body)
	}
	if _, ok, err = New(Config{}).Watch(context.Background(), "tok"); ok || err != nil {
		t.Fatalf("watch without a topic: %v %v", ok, err)
	}
}

func TestRecentPagesUntilTheLimit(t *testing.T) {
	var pages int
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if !strings.Contains(r.URL.Query().Get("q"), "newer_than:30d") || r.URL.Query().Get("labelIds") != "INBOX" {
			t.Errorf("query %v", r.URL.Query())
		}
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"messages":[{"id":"1"},{"id":"2"}],"nextPageToken":"n"}`)
			return
		}
		io.WriteString(w, `{"messages":[{"id":"3"},{"id":"4"}]}`)
	})
	ids, err := c.Recent(context.Background(), "tok", 30, 3)
	if err != nil || len(ids) != 3 || pages != 2 {
		t.Fatalf("ids %v pages %d %v", ids, pages, err)
	}
}

func TestTruncateKeepsUTF8Whole(t *testing.T) {
	if got := truncate("ñññ", 3); got != "ñ" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
