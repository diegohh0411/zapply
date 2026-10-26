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

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
)

// Principal is a verified token identity.
type Principal struct {
	Issuer  string
	Subject string
}

// Verifier validates a raw JWT.
type Verifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

var ErrInvalidToken = errors.New("invalid token")

type jwtVerifier struct {
	keyfunc jwt.Keyfunc
	opts    []jwt.ParserOption
}

func (v *jwtVerifier) Verify(_ context.Context, raw string) (Principal, error) {
	var claims jwt.RegisteredClaims
	if _, err := jwt.ParseWithClaims(raw, &claims, v.keyfunc, v.opts...); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return Principal{}, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	return Principal{Issuer: claims.Issuer, Subject: claims.Subject}, nil
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

// NewJWKS verifies asymmetric tokens against a provider's JWKS, refreshed in
// the background until ctx is cancelled.
func NewJWKS(ctx context.Context, url, issuer, audience string) (Verifier, error) {
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{url})
	if err != nil {
		return nil, fmt.Errorf("load JWKS: %w", err)
	}
	return &jwtVerifier{
		keyfunc: kf.Keyfunc,
		opts:    parserOptions(issuer, audience, []string{"RS256", "ES256", "EdDSA"}),
	}, nil
}

// NewDev verifies HS256 tokens signed with a shared development secret.
func NewDev(secret, audience string) Verifier {
	key := []byte(secret)
	return &jwtVerifier{
		keyfunc: func(*jwt.Token) (any, error) { return key, nil },
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

func (m MultiVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	err := ErrInvalidToken
	for _, v := range m {
		p, verr := v.Verify(ctx, raw)
		if verr == nil {
			return p, nil
		}
		err = verr
	}
	return Principal{}, err
}

// Resolver maps a principal to an internal user ID, creating the user on first sight.
type Resolver interface {
	ResolveUser(ctx context.Context, p Principal) (uuid.UUID, error)
}

type userKey struct{}

// UserID returns the authenticated user. It panics if the Middleware did not
// run, which is a routing bug rather than a client error.
func UserID(ctx context.Context) uuid.UUID {
	id, ok := ctx.Value(userKey{}).(uuid.UUID)
	if !ok {
		panic("auth: no user in context")
	}
	return id
}

// WithUser stores a user ID in ctx. Exposed for tests and internal callers.
func WithUser(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userKey{}, id)
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
			p, err := v.Verify(req.Context(), raw)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				httpx.WriteProblem(w, req, http.StatusUnauthorized, "invalid token")
				return
			}
			uid, err := r.ResolveUser(req.Context(), p)
			if err != nil {
				logx.From(req.Context()).Error("resolve user", "err", err)
				httpx.WriteProblem(w, req, http.StatusServiceUnavailable, "")
				return
			}
			ctx := WithUser(req.Context(), uid)
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
