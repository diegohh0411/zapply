// Package google implements mail.Provider over Google OAuth and the Gmail REST
// API, with plain net/http so every request is visible.
package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/mail"
)

// Scope is the one scope we request. The Gmail docs promise the snippet only in
// the full message format, which gmail.metadata refuses (docs/parts/10-mail-plan.md).
const Scope = "https://www.googleapis.com/auth/gmail.readonly"

type Config struct {
	ClientID, ClientSecret, RedirectURL string
	// Topic is the Pub/Sub topic Gmail publishes to; empty turns push off.
	Topic string
}

type Client struct {
	cfg  Config
	http *http.Client
	// Endpoints are fields so tests can point them at a local server.
	authURL, tokenURL, revokeURL, apiURL string
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second},
		authURL: "https://accounts.google.com/o/oauth2/v2/auth", tokenURL: "https://oauth2.googleapis.com/token",
		revokeURL: "https://oauth2.googleapis.com/revoke", apiURL: "https://gmail.googleapis.com/gmail/v1/users/me"}
}

// WithEndpoints points the client at other servers (tests).
func (c *Client) WithEndpoints(auth, token, revoke, api string) *Client {
	c.authURL, c.tokenURL, c.revokeURL, c.apiURL = auth, token, revoke, api
	return c
}

func (c *Client) Name() string { return "google" }

func (c *Client) AuthURL(state, challenge string) string {
	q := url.Values{
		"client_id": {c.cfg.ClientID}, "redirect_uri": {c.cfg.RedirectURL}, "response_type": {"code"},
		"scope": {Scope}, "access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"false"},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	return c.authURL + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (c *Client) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer res.Body.Close()
	var out tokenResponse
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	_ = json.Unmarshal(body, &out)
	if res.StatusCode >= 400 {
		if out.Error == "invalid_grant" {
			return out, mail.ErrRevoked
		}
		return out, fmt.Errorf("google token endpoint: %d %s", res.StatusCode, out.Error)
	}
	if out.AccessToken == "" {
		return out, errors.New("google token endpoint returned no access token")
	}
	return out, nil
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (mail.Tokens, error) {
	r, err := c.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {c.cfg.RedirectURL}})
	if err != nil {
		return mail.Tokens{}, err
	}
	// Reading mail is the whole point; a consent screen the member narrowed is no use.
	if !strings.Contains(" "+r.Scope+" ", " "+Scope+" ") {
		return mail.Tokens{}, errors.New("gmail read access was not granted")
	}
	if r.RefreshToken == "" {
		return mail.Tokens{}, errors.New("google returned no refresh token")
	}
	return mail.Tokens{Refresh: r.RefreshToken, Access: r.AccessToken, Expires: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}, nil
}

func (c *Client) Refresh(ctx context.Context, refresh string) (mail.Tokens, error) {
	r, err := c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		return mail.Tokens{}, err
	}
	return mail.Tokens{Refresh: refresh, Access: r.AccessToken, Expires: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}, nil
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	// 400 means the token is already invalid, which is what we wanted.
	if res.StatusCode >= 500 {
		return fmt.Errorf("google revoke: %d", res.StatusCode)
	}
	return nil
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return "gmail api: " + strconv.Itoa(e.Status) + " " + e.Body }

func (c *Client) call(ctx context.Context, access, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	u := c.apiURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		// Error bodies can echo request fields; keep only a short prefix.
		return &apiError{res.StatusCode, truncate(string(raw), 200)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func status(err error) int {
	var e *apiError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) Profile(ctx context.Context, access string) (mail.Profile, error) {
	var p struct {
		EmailAddress string `json:"emailAddress"`
		HistoryID    string `json:"historyId"`
	}
	if err := c.call(ctx, access, http.MethodGet, "/profile", nil, nil, &p); err != nil {
		return mail.Profile{}, err
	}
	return mail.Profile{Address: strings.ToLower(p.EmailAddress), HistoryID: p.HistoryID}, nil
}

func (c *Client) Watch(ctx context.Context, access string) (mail.Watch, bool, error) {
	if c.cfg.Topic == "" {
		return mail.Watch{}, false, nil
	}
	var w struct {
		HistoryID  string `json:"historyId"`
		Expiration string `json:"expiration"`
	}
	in := map[string]any{"topicName": c.cfg.Topic, "labelIds": []string{"INBOX"}, "labelFilterBehavior": "INCLUDE"}
	if err := c.call(ctx, access, http.MethodPost, "/watch", nil, in, &w); err != nil {
		return mail.Watch{}, false, err
	}
	ms, err := strconv.ParseInt(w.Expiration, 10, 64)
	if err != nil {
		return mail.Watch{}, false, errors.New("gmail watch returned no expiration")
	}
	return mail.Watch{HistoryID: w.HistoryID, Expires: time.UnixMilli(ms)}, true, nil
}

func (c *Client) Stop(ctx context.Context, access string) error {
	return c.call(ctx, access, http.MethodPost, "/stop", nil, nil, nil)
}

func (c *Client) History(ctx context.Context, access, start, page string) (mail.HistoryPage, error) {
	q := url.Values{"startHistoryId": {start}, "historyTypes": {"messageAdded"}, "labelId": {"INBOX"}, "maxResults": {"100"}}
	if page != "" {
		q.Set("pageToken", page)
	}
	var r struct {
		History []struct {
			ID            string `json:"id"`
			MessagesAdded []struct {
				Message struct {
					ID       string   `json:"id"`
					LabelIDs []string `json:"labelIds"`
				} `json:"message"`
			} `json:"messagesAdded"`
		} `json:"history"`
		NextPageToken string `json:"nextPageToken"`
		HistoryID     string `json:"historyId"`
	}
	if err := c.call(ctx, access, http.MethodGet, "/history", q, nil, &r); err != nil {
		if status(err) == http.StatusNotFound {
			return mail.HistoryPage{}, mail.ErrHistoryExpired
		}
		return mail.HistoryPage{}, err
	}
	out := mail.HistoryPage{Next: r.NextPageToken, HistoryID: r.HistoryID}
	for _, h := range r.History {
		out.LastID = h.ID
		for _, a := range h.MessagesAdded {
			if hasLabel(a.Message.LabelIDs, "INBOX") {
				out.MessageIDs = append(out.MessageIDs, a.Message.ID)
			}
		}
	}
	return out, nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func (c *Client) Recent(ctx context.Context, access string, days, limit int) ([]string, error) {
	var ids []string
	page := ""
	for len(ids) < limit {
		q := url.Values{"labelIds": {"INBOX"}, "q": {fmt.Sprintf("newer_than:%dd", days)}, "maxResults": {strconv.Itoa(min(100, limit-len(ids)))}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var r struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.call(ctx, access, http.MethodGet, "/messages", q, nil, &r); err != nil {
			return nil, err
		}
		for _, m := range r.Messages {
			ids = append(ids, m.ID)
		}
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// Get reads one message in the full format with a field mask that keeps only
// the headers and the snippet, so no body part leaves Google.
func (c *Client) Get(ctx context.Context, access, id string) (mail.Message, error) {
	q := url.Values{"format": {"full"}, "fields": {"id,threadId,internalDate,snippet,payload/headers"}}
	var r struct {
		ID           string `json:"id"`
		ThreadID     string `json:"threadId"`
		InternalDate string `json:"internalDate"`
		Snippet      string `json:"snippet"`
		Payload      struct {
			Headers []struct{ Name, Value string } `json:"headers"`
		} `json:"payload"`
	}
	if err := c.call(ctx, access, http.MethodGet, "/messages/"+url.PathEscape(id), q, nil, &r); err != nil {
		if status(err) == http.StatusNotFound {
			return mail.Message{}, mail.ErrMessageGone
		}
		return mail.Message{}, err
	}
	m := mail.Message{ID: r.ID, ThreadID: r.ThreadID, Snippet: truncate(html.UnescapeString(r.Snippet), 500), ReceivedAt: time.Now().UTC()}
	if ms, err := strconv.ParseInt(r.InternalDate, 10, 64); err == nil {
		m.ReceivedAt = time.UnixMilli(ms).UTC()
	}
	dec := new(mime.WordDecoder)
	for _, h := range r.Payload.Headers {
		v, err := dec.DecodeHeader(h.Value)
		if err != nil {
			v = h.Value
		}
		switch strings.ToLower(h.Name) {
		case "from":
			m.From = truncate(v, 300)
		case "subject":
			m.Subject = truncate(v, 300)
		}
	}
	return m, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // do not split a UTF-8 sequence
		n--
	}
	return s[:n]
}
