// Package secretstore provides at-rest encryption for reversible secrets
// (upstream provider API credentials) that NexusLLM must decrypt later to use
// on an outbound request — as opposed to API-key hashing (internal/auth),
// which is one-way and never needs to be reversed.
//
// This is the first reversible-secret storage in NexusLLM. Existing plaintext
// columns (providers.api_key, model_endpoints.upstream_api_key) predate this
// package and are documented as a known gap (migration 040) — they are not
// migrated here. New credential storage (provider_credentials.secret_ciphertext,
// migration 062) always goes through Store.Encrypt/Decrypt.
//
// The encryption key MUST come from environment/secret configuration
// (NEXUS_CREDENTIAL_ENCRYPTION_KEY), never from the database — a compromised
// DB dump alone must never be enough to recover plaintext credentials.
package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KeyEnvVar is the environment variable holding the base64-encoded 32-byte
// (AES-256) encryption key.
const KeyEnvVar = "NEXUS_CREDENTIAL_ENCRYPTION_KEY"

// ErrNoKey is returned by New when the environment variable is unset — callers
// that need to encrypt/decrypt provider credentials must treat this as fatal
// configuration, not silently store plaintext.
var ErrNoKey = errors.New("secretstore: " + KeyEnvVar + " is not set")

// ErrInvalidCiphertext is returned by Decrypt when the stored value is
// malformed or fails authentication (wrong key, corrupted data, or tampering).
var ErrInvalidCiphertext = errors.New("secretstore: invalid or tampered ciphertext")

// Store encrypts and decrypts secrets with AES-256-GCM. Safe for concurrent
// use — the underlying cipher.AEAD is stateless per call.
type Store struct {
	aead cipher.AEAD
}

// New constructs a Store from a raw 32-byte AES-256 key.
func New(key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secretstore: key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretstore: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretstore: %w", err)
	}
	return &Store{aead: aead}, nil
}

// NewFromBase64Key decodes a base64-encoded 32-byte key (the format expected
// in NEXUS_CREDENTIAL_ENCRYPTION_KEY) and constructs a Store.
func NewFromBase64Key(b64Key string) (*Store, error) {
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("secretstore: key is not valid base64: %w", err)
	}
	return New(key)
}

// Encrypt returns a base64-encoded (nonce || ciphertext || tag) blob safe to
// store in a TEXT column. Never returns the plaintext in the output — callers
// must not log the argument either.
func (s *Store) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secretstore: generating nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Returns ErrInvalidCiphertext on any authentication
// or format failure — never a partial/garbage plaintext.
func (s *Store) Decrypt(blob string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", ErrInvalidCiphertext
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plaintext), nil
}
