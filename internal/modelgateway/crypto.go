// Package modelgateway owns encrypted personal model credentials and runtime-scoped model forwarding.
// Cloud and Controller receive references only; usable upstream keys never leave this service.
package modelgateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"strconv"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// Cipher encrypts immutable credential records with a deployment-owned key and an explicit key identity.
// It deliberately has no String or serialization method exposing the key material.
type Cipher struct {
	aead           cipher.AEAD
	keyID          string
	fingerprintKey []byte
}

// LoadCipher reads the 256-bit master key from its dedicated read-only deployment file.
func LoadCipher(path, keyID string) (*Cipher, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("model credential master key unavailable")
	}
	defer clear(key)
	return NewCipher(key, keyID)
}

// NewCipher constructs an independent credential cipher; callers own the original key buffer.
func NewCipher(key []byte, keyID string) (*Cipher, error) {
	if len(key) != 32 || keyID == "" {
		return nil, errors.New("a 256-bit model credential key and key identity are required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("ora-model-credential-idempotency-v1"))
	return &Cipher{aead: aead, keyID: keyID, fingerprintKey: mac.Sum(nil)}, nil
}

// CredentialID binds a retry to its owner, connection, version, operation and secret without
// persisting a plaintext hash. The keyed fingerprint cannot be tested from a database dump.
func (c *Cipher) CredentialID(identity *core.Claims, connection string, version int64, operation, secret string) string {
	mac := hmac.New(sha256.New, c.fingerprintKey)
	for _, field := range []string{identity.Source, identity.Subject, connection, strconv.FormatInt(version, 10), operation, secret} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(field)) + ":" + field))
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, mac.Sum(nil)).String()
}

// Seal uses a fresh nonce and binds the ciphertext to its immutable credential identity.
func (c *Cipher) Seal(id string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("model credential encryption unavailable")
	}
	return c.aead.Seal(nonce, nonce, plaintext, []byte("ora-model-credential-v1:"+c.keyID+":"+id)), nil
}

// Open authenticates the complete record before returning a request-local plaintext buffer.
// The caller must clear the returned buffer and must never log a decryption error's inputs.
func (c *Cipher) Open(id, keyID string, ciphertext []byte) ([]byte, error) {
	if keyID != c.keyID || len(ciphertext) < c.aead.NonceSize()+c.aead.Overhead() {
		return nil, errors.New("model credential unavailable")
	}
	n := c.aead.NonceSize()
	plaintext, err := c.aead.Open(nil, ciphertext[:n], ciphertext[n:], []byte("ora-model-credential-v1:"+keyID+":"+id))
	if err != nil {
		return nil, errors.New("model credential unavailable")
	}
	return plaintext, nil
}
