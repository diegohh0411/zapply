// Package edgetoken issues and hashes edge worker bearer tokens. A token is 32
// random bytes, so a plain SHA-256 is enough to store: there is nothing to
// brute-force, and the database never sees the token itself.
package edgetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Prefix marks an edge worker token, so a leaked one is recognisable.
const Prefix = "zedge_"

// New returns a fresh token and its hash.
func New() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = Prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token), nil
}

// Hash is what the database stores for a token.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Bearer extracts the token of an "Authorization: Bearer <token>" header.
func Bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > 200 {
		return "", false
	}
	return token, true
}
