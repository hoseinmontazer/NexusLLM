package secretstore

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return key
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	s, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secret := "sk-or-v1-abcdef0123456789"

	ciphertext, err := s.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(ciphertext, secret) {
		t.Fatalf("ciphertext must not contain the plaintext secret")
	}

	plaintext, err := s.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plaintext != secret {
		t.Fatalf("round trip mismatch: got %q, want %q", plaintext, secret)
	}
}

func TestEncrypt_NondeterministicNonce(t *testing.T) {
	s, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, err := s.Encrypt("same-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := s.Encrypt("same-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if a == b {
		t.Fatalf("two encryptions of the same plaintext must not produce identical ciphertext (nonce reuse)")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	s1, _ := New(testKey(t))
	s2, _ := New(testKey(t))

	ciphertext, err := s1.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := s2.Decrypt(ciphertext); err != ErrInvalidCiphertext {
		t.Fatalf("Decrypt with wrong key: got err=%v, want ErrInvalidCiphertext", err)
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	s, _ := New(testKey(t))
	ciphertext, err := s.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := ciphertext[:len(ciphertext)-4] + "AAAA"
	if _, err := s.Decrypt(tampered); err != ErrInvalidCiphertext {
		t.Fatalf("Decrypt tampered ciphertext: got err=%v, want ErrInvalidCiphertext", err)
	}
}

func TestDecrypt_GarbageInputFails(t *testing.T) {
	s, _ := New(testKey(t))
	if _, err := s.Decrypt("not-valid-base64-!!!"); err != ErrInvalidCiphertext {
		t.Fatalf("Decrypt garbage: got err=%v, want ErrInvalidCiphertext", err)
	}
	if _, err := s.Decrypt("dG9vc2hvcnQ="); err != ErrInvalidCiphertext { // "tooshort" b64
		t.Fatalf("Decrypt too-short blob: got err=%v, want ErrInvalidCiphertext", err)
	}
}

func TestNew_RejectsWrongKeySize(t *testing.T) {
	if _, err := New([]byte("too-short")); err == nil {
		t.Fatalf("expected error for non-32-byte key")
	}
}

func TestNewFromBase64Key(t *testing.T) {
	if _, err := NewFromBase64Key("not base64!!"); err == nil {
		t.Fatalf("expected error for invalid base64")
	}
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 10))
	if _, err := NewFromBase64Key(shortKey); err == nil {
		t.Fatalf("expected error for wrong-length decoded key")
	}

	key := testKey(t)
	b64 := base64.StdEncoding.EncodeToString(key)
	s, err := NewFromBase64Key(b64)
	if err != nil {
		t.Fatalf("NewFromBase64Key with valid key: %v", err)
	}
	ciphertext, err := s.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if plaintext, err := s.Decrypt(ciphertext); err != nil || plaintext != "secret" {
		t.Fatalf("round trip via NewFromBase64Key failed: plaintext=%q err=%v", plaintext, err)
	}
}
