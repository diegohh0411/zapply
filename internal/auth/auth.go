// Package auth verifies bearer JWTs and resolves them to a zapply user.
//
// Tokens identify a principal by (issuer, subject). The store maps that pair to
// an internal user ID, so the identity provider can change without touching
// tenant data.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
)

// Principal is a verified token identity.
type Principal struct {
	Issuer  string
	Subject string
	Email   string
}

type jwtClaims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
}

// Token is a verified JWT: who it identifies and when it stops being valid.
// Principal alone is the identity, so it stays usable as a cache key.
type Token struct {
	Principal Principal
	ExpiresAt time.Time
}

// Verifier validates a raw JWT.
type Verifier interface {
	Verify(ctx context.Context, token string) (Token, error)
}

var ErrInvalidToken = errors.New("invalid token")

type jwtVerifier struct {
	// keyfunc is per request so key lookups (which may refresh the JWKS)
	// stop when the request is cancelled.
	keyfunc func(context.Context) jwt.Keyfunc
	opts    []jwt.ParserOption
}

func (v *jwtVerifier) Verify(ctx context.Context, raw string) (Token, error) {
	var claims jwtClaims
	if _, err := jwt.ParseWithClaims(raw, &claims, v.keyfunc(ctx), v.opts...); err != nil {
		return Token{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return Token{}, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	// exp is required by the parser options.
	return Token{
		Principal: Principal{Issuer: claims.Issuer, Subject: claims.Subject, Email: claims.Email},
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

func parserOptions(issuer, audience string, methods []string) []jwt.ParserOption {
	return []jwt.ParserOption{
		jwt.WithValidMethods(methods),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second),
	}
}

// NewJWKS verifies asymmetric tokens against a provider's JWKS, refreshed
// hourly in the background until ctx is cancelled. The first fetch must
// succeed: starting with no keys would pass readiness yet reject every login.
func NewJWKS(ctx context.Context, url, issuer, audience string) (Verifier, error) {
	remote, err := jwkset.NewStorageFromHTTP(url, jwkset.HTTPClientStorageOptions{
		Ctx:             ctx,
		HTTPTimeout:     10 * time.Second,
		RefreshInterval: time.Hour,
		RefreshErrorHandler: func(ctx context.Context, err error) {
			logx.From(ctx).Warn("JWKS refresh failed; keeping cached keys", "url", url, "err", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("load JWKS from %s: %w", url, err)
	}
	client, err := jwkset.NewHTTPClient(jwkset.HTTPClientOptions{
		HTTPURLs: map[string]jwkset.Storage{url: remote},
		// An unknown kid (key rotation) triggers at most one refetch a minute;
		// over the limit, verification fails at once instead of waiting.
		RefreshUnknownKID: rate.NewLimiter(rate.Every(time.Minute), 1),
		RateLimitWaitMax:  time.Second,
	})
	if err != nil {
		return nil, err
	}
	kf, err := keyfunc.New(keyfunc.Options{Ctx: ctx, Storage: client})
	if err != nil {
		return nil, err
	}
	return &jwtVerifier{
		keyfunc: kf.KeyfuncCtx,
		opts:    parserOptions(issuer, audience, []string{"RS256", "ES256", "EdDSA"}),
	}, nil
}

// NewDev verifies HS256 tokens signed with a shared development secret.
func NewDev(secret, audience string) Verifier {
	key := []byte(secret)
	static := func(*jwt.Token) (any, error) { return key, nil }
	return &jwtVerifier{
		keyfunc: func(context.Context) jwt.Keyfunc { return static },
		opts:    parserOptions(config.DevIssuer, audience, []string{"HS256"}),
	}
}

// MintDev signs a development token for subject.
func MintDev(secret, audience, subject string, ttl time.Duration) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    config.DevIssuer,
		Subject:   subject,
		Audience:  jwt.ClaimStrings{audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}).SignedString([]byte(secret))
}

// MultiVerifier tries each verifier in order and returns the first success.
type MultiVerifier []Verifier

func (m MultiVerifier) Verify(ctx context.Context, raw string) (Token, error) {
	err := ErrInvalidToken
	for _, v := range m {
		tok, verr := v.Verify(ctx, raw)
		if verr == nil {
			return tok, nil
		}
		err = verr
	}
	return Token{}, err
}

// Resolver maps a principal to an internal user ID, creating the user on first sight.
type Resolver interface {
	ResolveUser(ctx context.Context, p Principal) (uuid.UUID, error)
}

type (
	userKey    struct{}
	expiresKey struct{}
)

// UserID returns the authenticated user. It panics if the Middleware did not
// run, which is a routing bug rather than a client error.
func UserID(ctx context.Context) uuid.UUID {
	id, ok := ctx.Value(userKey{}).(uuid.UUID)
	if !ok {
		panic("auth: no user in context")
	}
	return id
}

// UserIDFrom returns the authenticated user, if a request's context names one.
func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userKey{}).(uuid.UUID)
	return id, ok
}

// WithUser stores a user ID in ctx. Exposed for tests and internal callers.
func WithUser(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

// ExpiresAt returns when the request's token expires, or the zero time if the
// Middleware did not run. Long-lived responses (SSE) end there.
func ExpiresAt(ctx context.Context) time.Time {
	t, _ := ctx.Value(expiresKey{}).(time.Time)
	return t
}

// WithExpiry stores a token expiry in ctx. Exposed for tests.
func WithExpiry(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, expiresKey{}, t)
}

// Middleware requires a valid bearer token and puts the user ID in the context.
func Middleware(v Verifier, r Resolver) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			raw, ok := bearer(req)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				httpx.WriteProblem(w, req, http.StatusUnauthorized, "missing bearer token")
				return
			}
			tok, err := v.Verify(req.Context(), raw)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				httpx.WriteProblem(w, req, http.StatusUnauthorized, "invalid token")
				return
			}
			uid, err := r.ResolveUser(req.Context(), tok.Principal)
			if err != nil {
				logx.From(req.Context()).Error("resolve user", "err", err)
				httpx.WriteProblem(w, req, http.StatusServiceUnavailable, "")
				return
			}
			ctx := WithExpiry(WithUser(req.Context(), uid), tok.ExpiresAt)
			ctx = logx.With(ctx, logx.From(ctx).With("user_id", uid))
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
