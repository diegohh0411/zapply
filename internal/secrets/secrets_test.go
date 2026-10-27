package secrets

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestCredentialBindingAndRotation(t *testing.T) {
	a := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	b := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	r, err := New(`{"v1":"`+a+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	cipher, version, err := r.Encrypt("record-a", "secret-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("secret-provider-key")) {
		t.Fatal("plaintext in ciphertext")
	}
	rotated, err := New(`{"v1":"`+a+`","v2":"`+b+`"}`, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if text, err := rotated.Decrypt("record-a", version, cipher); err != nil || text != "secret-provider-key" {
		t.Fatalf("rotation: %q %v", text, err)
	}
	if _, err := rotated.Decrypt("record-b", version, cipher); err == nil {
		t.Fatal("credential could be moved to another record")
	}
	cipher[len(cipher)-1] ^= 1
	if _, err := rotated.Decrypt("record-a", version, cipher); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
	var absent *Ring
	if _, _, err := absent.Encrypt("x", "y"); err == nil {
		t.Fatal("unconfigured ring accepted")
	}
}
