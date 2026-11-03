// Package secret encrypts credentials at rest with AES-256-GCM.
//
// The key lives in a separate file (secret.key), so a copy of the database
// alone does not reveal switch passwords. Every ciphertext is bound to
// associated data (for devices: ID, host and port), so it cannot be moved to
// another device or host.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Key is a 256-bit AES key.
type Key struct {
	aead cipher.AEAD
}

// LoadOrCreate reads the hex-encoded key at path, creating it with mode 0600
// if it does not exist.
func LoadOrCreate(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := Generate(path); err != nil {
			return nil, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read secret key: %w", err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("secret key %s: want 64 hex characters", path)
	}
	return New(raw)
}

// Generate writes a new random key to path. It refuses to overwrite.
func Generate(path string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create secret key: %w", err)
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(raw) + "\n")
	return err
}

// New builds a key from 32 raw bytes.
func New(raw []byte) (*Key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead}, nil
}

// Encrypt returns nonce || ciphertext. aad is authenticated but not stored;
// Decrypt must be called with the same value.
func (k *Key) Encrypt(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, plain, aad), nil
}

// ErrDecrypt is returned when a ciphertext cannot be opened: wrong key,
// corrupted data, or associated data that differs from encryption time.
var ErrDecrypt = errors.New("decrypt credentials: wrong secret.key, changed device address or corrupted data")

// Decrypt reverses Encrypt.
func (k *Key) Decrypt(data, aad []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("ciphertext too short")
	}
	plain, err := k.aead.Open(nil, data[:n], data[n:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
