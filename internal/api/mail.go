package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/mail"
)

const (
	googleCerts  = "https://www.googleapis.com/oauth2/v3/certs"
	googleIssuer = "https://accounts.google.com"
)

func mailValue[T any](v any, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	raw, e := json.Marshal(v)
	return decodePlatform[T](raw, e)
}

func (s *server) ListMailAccounts(ctx context.Context, _ gen.ListMailAccountsRequestObject) (gen.ListMailAccountsResponseObject, error) {
	items, err := s.mail.Accounts(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	return mailValue[gen.ListMailAccounts200JSONResponse](map[string]any{"configured": s.mail.Configured(), "items": items}, nil)
}

func (s *server) StartGoogleConnect(ctx context.Context, _ gen.StartGoogleConnectRequestObject) (gen.StartGoogleConnectResponseObject, error) {
	url, err := s.mail.StartConnect(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	return gen.StartGoogleConnect200JSONResponse{Url: url}, nil
}

func (s *server) UpdateMailAccount(ctx context.Context, req gen.UpdateMailAccountRequestObject) (gen.UpdateMailAccountResponseObject, error) {
	a, err := s.mail.SetPaused(ctx, uid(ctx), req.Id, req.Body.Paused)
	return mailValue[gen.UpdateMailAccount200JSONResponse](a, err)
}

func (s *server) SyncMailAccount(ctx context.Context, req gen.SyncMailAccountRequestObject) (gen.SyncMailAccountResponseObject, error) {
	a, err := s.mail.SyncNow(ctx, uid(ctx), req.Id)
	return mailValue[gen.SyncMailAccount200JSONResponse](a, err)
}

func (s *server) RescanMailAccount(ctx context.Context, req gen.RescanMailAccountRequestObject) (gen.RescanMailAccountResponseObject, error) {
	a, err := s.mail.Rescan(ctx, uid(ctx), req.Id, req.Body.Days)
	return mailValue[gen.RescanMailAccount200JSONResponse](a, err)
}

func (s *server) DisconnectMailAccount(ctx context.Context, req gen.DisconnectMailAccountRequestObject) (gen.DisconnectMailAccountResponseObject, error) {
	if err := s.mail.Disconnect(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DisconnectMailAccount204Response{}, nil
}

func (s *server) ListMailMessages(ctx context.Context, req gen.ListMailMessagesRequestObject) (gen.ListMailMessagesResponseObject, error) {
	category := ""
	if req.Params.Category != nil {
		category = string(*req.Params.Category)
	}
	limit := pageLimit(req.Params.Limit)
	items, err := s.mail.Messages(ctx, uid(ctx), category, req.Params.JobId, req.Params.Before, limit)
	if err != nil {
		return nil, err
	}
	page := map[string]any{"items": items}
	if len(items) == limit {
		page["next_cursor"] = items[len(items)-1].ID
	}
	return mailValue[gen.ListMailMessages200JSONResponse](page, nil)
}

// CompleteGoogleConnect finishes the consent flow for the signed-in member. The
// state must be one this member started, which is what stops a consent link sent
// to someone else from attaching their mailbox here.
func (s *server) CompleteGoogleConnect(ctx context.Context, req gen.CompleteGoogleConnectRequestObject) (gen.CompleteGoogleConnectResponseObject, error) {
	a, err := s.mail.CompleteConnect(ctx, uid(ctx), req.Body.State, req.Body.Code)
	if errors.Is(err, mail.ErrBadState) {
		return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "state", Message: "this sign-in link expired or belongs to another account; start again"}}}
	}
	return mailValue[gen.CompleteGoogleConnect200JSONResponse](a, err)
}

// googleCallback receives the browser after Google's consent screen. It has no
// bearer token, so it only hands the result to the app, which finishes the flow
// as the signed-in member (CompleteGoogleConnect).
func (s *server) googleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back := url.Values{}
	switch {
	case q.Get("error") != "":
		back.Set("error", "denied")
	case q.Get("state") == "" || q.Get("code") == "":
		back.Set("error", "expired")
	default:
		back.Set("oauth_state", q.Get("state"))
		back.Set("oauth_code", q.Get("code"))
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/settings/connections?"+back.Encode(), http.StatusSeeOther)
}

// lazyVerifier builds the Google OIDC verifier on first use, so the API starts
// even when Google's key endpoint is unreachable.
type lazyVerifier struct {
	audience string
	mu       sync.Mutex
	v        auth.Verifier
}

func (l *lazyVerifier) Verify(ctx context.Context, token string) (auth.Token, error) {
	l.mu.Lock()
	if l.v == nil {
		v, err := auth.NewJWKS(context.WithoutCancel(ctx), googleCerts, googleIssuer, l.audience)
		if err != nil {
			l.mu.Unlock()
			return auth.Token{}, err
		}
		l.v = v
	}
	v := l.v
	l.mu.Unlock()
	return v.Verify(ctx, token)
}

// googlePush receives Gmail change notifications from a Pub/Sub push
// subscription. Pub/Sub signs each request with the subscription's service
// account; anything else is rejected. A 2xx acknowledges the message.
func (s *server) googlePush(w http.ResponseWriter, r *http.Request) {
	g := s.deps.Google
	if !s.mail.Configured() || g.PubSubTopic == "" || s.pushVerifier == nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "")
		return
	}
	t, err := s.pushVerifier.Verify(r.Context(), token)
	if err != nil || !strings.EqualFold(t.Principal.Email, g.PubSubServiceAccount) {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "")
		return
	}
	var envelope struct {
		Message struct {
			Data string `json:"data"`
		} `json:"message"`
	}
	var note struct {
		EmailAddress string      `json:"emailAddress"`
		HistoryID    json.Number `json:"historyId"`
	}
	data, err := base64.StdEncoding.DecodeString(envelope.Message.Data)
	if json.Unmarshal(body, &envelope) == nil {
		data, err = base64.StdEncoding.DecodeString(envelope.Message.Data)
		if err != nil {
			data, err = base64.URLEncoding.DecodeString(envelope.Message.Data)
		}
	}
	if err != nil || json.Unmarshal(data, &note) != nil || note.EmailAddress == "" {
		// Malformed messages will never become valid; acknowledge to drop them.
		logx.From(r.Context()).Warn("dropped malformed gmail notification")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.mail.HandlePush(r.Context(), strings.ToLower(note.EmailAddress), note.HistoryID.String()); err != nil {
		logx.From(r.Context()).Error("gmail notification failed", "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) ResolveMailStatusChange(ctx context.Context, req gen.ResolveMailStatusChangeRequestObject) (gen.ResolveMailStatusChangeResponseObject, error) {
	m, err := s.mail.Resolve(ctx, uid(ctx), req.Id, string(req.Body.Action))
	return mailValue[gen.ResolveMailStatusChange200JSONResponse](m, err)
}

func (s *server) RelinkMailMessage(ctx context.Context, req gen.RelinkMailMessageRequestObject) (gen.RelinkMailMessageResponseObject, error) {
	m, err := s.mail.Relink(ctx, uid(ctx), req.Id, req.Body.JobId)
	return mailValue[gen.RelinkMailMessage200JSONResponse](m, err)
}
