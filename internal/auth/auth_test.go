package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
)

const secret = "0123456789abcdef0123456789abcdef"

func devToken(t *testing.T, claims jwt.RegisteredClaims, method jwt.SigningMethod, key any) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Issuer:    config.DevIssuer,
		Subject:   "alice",
		Audience:  jwt.ClaimStrings{"authenticated"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
}

func TestDevVerifier(t *testing.T) {
	v := auth.NewDev(secret, "authenticated")

	tok, err := auth.MintDev(secret, "authenticated", "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if got.Principal != (auth.Principal{Issuer: config.DevIssuer, Subject: "alice"}) {
		t.Fatalf("principal = %+v", got.Principal)
	}
	if d := time.Until(got.ExpiresAt); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("expires in %v, want about 1h", d)
	}

	cases := map[string]func(*jwt.RegisteredClaims){
		"expired":       func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) },
		"no expiry":     func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil },
		"wrong issuer":  func(c *jwt.RegisteredClaims) { c.Issuer = "evil" },
		"wrong aud":     func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other"} },
		"empty subject": func(c *jwt.RegisteredClaims) { c.Subject = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validClaims()
			mutate(&c)
			_, err := v.Verify(context.Background(), devToken(t, c, jwt.SigningMethodHS256, []byte(secret)))
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}

	t.Run("wrong secret", func(t *testing.T) {
		tok := devToken(t, validClaims(), jwt.SigningMethodHS256, []byte("another-secret-another-secret-xx"))
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("accepted token with wrong secret")
		}
	})
	t.Run("alg none", func(t *testing.T) {
		tok := devToken(t, validClaims(), jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("accepted unsigned token")
		}
	})
}

func writeJWKS(w http.ResponseWriter, key *rsa.PrivateKey) {
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
		"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
}

func TestJWKSVerifier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJWKS(w, key)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, err := auth.NewJWKS(ctx, srv.URL, "https://idp.example", "authenticated")
	if err != nil {
		t.Fatal(err)
	}

	c := validClaims()
	c.Issuer = "https://idp.example"
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, raw); err != nil {
		t.Fatalf("valid RS256 token rejected: %v", err)
	}

	// An HS256 token must not verify against the JWKS verifier, whatever the key.
	hs := devToken(t, c, jwt.SigningMethodHS256, []byte(secret))
	if _, err := v.Verify(ctx, hs); err == nil {
		t.Fatal("JWKS verifier accepted HS256")
	}
}

func TestJWKSUnavailableAtStartup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := auth.NewJWKS(ctx, srv.URL, "https://idp.example", "authenticated"); err == nil {
		t.Fatal("NewJWKS succeeded with no keys")
	}
}

// An unknown kid refetches the JWKS; a stalled fetch must not outlive the request.
func TestJWKSVerifyHonoursRequestContext(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	defer close(stall)
	var served atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !served.Swap(true) {
			writeJWKS(w, key)
			return
		}
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, err := auth.NewJWKS(ctx, srv.URL, "https://idp.example", "authenticated")
	if err != nil {
		t.Fatal(err)
	}
	c := validClaims()
	c.Issuer = "https://idp.example"
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "rotated"
	raw, _ := tok.SignedString(key)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reqCancel()
	start := time.Now()
	if _, err := v.Verify(reqCtx, raw); err == nil {
		t.Fatal("unknown kid verified")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Verify blocked %v after the request was cancelled", d)
	}
}

type fixedResolver uuid.UUID

func (f fixedResolver) ResolveUser(context.Context, auth.Principal) (uuid.UUID, error) {
	return uuid.UUID(f), nil
}

func TestMiddleware(t *testing.T) {
	uid := uuid.New()
	h := auth.Middleware(auth.NewDev(secret, "authenticated"), fixedResolver(uid))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(auth.UserID(r.Context()).String()))
		}))

	tok, _ := auth.MintDev(secret, "authenticated", "alice", time.Hour)
	cases := []struct {
		name, header string
		want         int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + tok, http.StatusUnauthorized},
		{"garbage", "Bearer nope", http.StatusUnauthorized},
		{"valid", "Bearer " + tok, http.StatusOK},
		{"lowercase scheme", "bearer " + tok, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/me", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusOK && w.Body.String() != uid.String() {
				t.Fatalf("user = %q", w.Body.String())
			}
			if tc.want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate")
			}
		})
	}
}
