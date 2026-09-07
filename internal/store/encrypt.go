package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// SecretCrypto encrypts credential payloads at rest (AES-256-GCM). The master
// key is deployment-provided (KMS / env injection); plaintext secrets never
// leave this package's boundary except to the BMC driver (docs/02-architecture.md §4).
type SecretCrypto struct {
	aead cipher.AEAD
}

// NewSecretCrypto builds the crypto from a base64-encoded 32-byte master key.
func NewSecretCrypto(masterKeyB64 string) (*SecretCrypto, error) {
	raw, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("store: master key must be base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("store: master key must decode to 32 bytes, got %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("store: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: gcm: %w", err)
	}
	return &SecretCrypto{aead: aead}, nil
}

// Encrypt seals plaintext into nonce‖ciphertext.
func (c *SecretCrypto) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens nonce‖ciphertext.
func (c *SecretCrypto) Decrypt(sealed []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("store: ciphertext too short")
	}
	pt, err := c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("store: decrypt: %w", err)
	}
	return pt, nil
}
