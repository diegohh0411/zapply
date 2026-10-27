// Package secrets encrypts system credentials; master keys stay outside Postgres.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type Ring struct {
	keys   map[string]cipher.AEAD
	active string
}

// New accepts a JSON map of version -> base64-encoded 32-byte key.
func New(raw, active string) (*Ring, error) {
	if raw == "" && active == "" {
		return nil, nil
	}
	var encoded map[string]string
	if json.Unmarshal([]byte(raw), &encoded) != nil {
		return nil, errors.New("invalid encryption key ring JSON")
	}
	r := &Ring{keys: make(map[string]cipher.AEAD), active: active}
	for version, key := range encoded {
		b, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(b) != 32 || version == "" {
			return nil, errors.New("encryption keys must be versioned 32-byte base64 values")
		}
		block, err := aes.NewCipher(b)
		if err != nil {
			return nil, err
		}
		a, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		r.keys[version] = a
	}
	if r.keys[active] == nil {
		return nil, errors.New("active encryption key version is missing")
	}
	return r, nil
}

func (r *Ring) Encrypt(id, plaintext string) ([]byte, string, error) {
	if r == nil {
		return nil, "", errors.New("encryption is not configured")
	}
	a := r.keys[r.active]
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	return a.Seal(nonce, nonce, []byte(plaintext), []byte("openrouter:"+id+":"+r.active)), r.active, nil
}
func (r *Ring) Decrypt(id, version string, encrypted []byte) (string, error) {
	if r == nil || r.keys[version] == nil {
		return "", errors.New("encryption key version is unavailable")
	}
	a := r.keys[version]
	if len(encrypted) < a.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	b, err := a.Open(nil, encrypted[:a.NonceSize()], encrypted[a.NonceSize():], []byte("openrouter:"+id+":"+version))
	if err != nil {
		return "", errors.New("credential authentication failed")
	}
	return string(b), nil
}
func Fingerprint(value string) string {
	s := sha256.Sum256([]byte(value))
	return hex.EncodeToString(s[:6])
}
