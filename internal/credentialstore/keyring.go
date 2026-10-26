// Package credentialstore implements the independent Credential Store that holds user-managed
// HTTPS Git tokens for Ora Cloud (identity-access ADR "Git credentials in an independent
// Credential Store", D2 and D3).
//
// The Store is its own process with its own volume. Browsers submit a token directly to its
// ingress with a one-time submission grant Cloud signed; Cloud only reads metadata and revokes
// through the management interface; the Repo Executor redeems plaintext exactly once per
// execution authorization; a KEK operator rewraps data keys after a key rotation. Each of those
// callers is a distinct identity on a distinct listener, and no identity holds two powers.
//
// Secrets are sealed with envelope encryption: a fresh 32-byte data key (DEK) per version encrypts
// the secret with AES-256-GCM, and the DEK is wrapped by a key-encryption key (KEK) from a
// deployment-mounted keyring. Plaintext never reaches a log line, an error string or a management
// response.
package credentialstore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// keyBytes is the AES-256 key length of every KEK and DEK.
const keyBytes = 32

// maxKeyringBytes bounds the keyring file so a wrong mount cannot make startup read an
// arbitrarily large file into memory.
const maxKeyringBytes = 1 << 20

// ErrKeyringUnavailable reports that the deployment did not deliver a usable keyring. The
// command refuses to start on it: an empty store would present "cannot decrypt" as "never
// existed". The wrapped detail never contains key material.
var ErrKeyringUnavailable = errors.New("credential store keyring is unavailable")

// Keyring holds the deployment's key-encryption keys. Exactly one is primary and wraps every new
// DEK; the others stay available for unwrapping until a rewrap has moved every DEK to the
// primary. The Store never writes the keyring: KEK material belongs to the deployment.
type Keyring struct {
	primary string
	keys    map[string][]byte
}

// keyringFile is the mounted JSON shape: {"primary":"<kekId>","keys":{"<kekId>":"<base64>"}}.
type keyringFile struct {
	Primary string            `json:"primary"`
	Keys    map[string]string `json:"keys"`
}

// LoadKeyring reads and validates the mounted keyring file. Every failure wraps
// ErrKeyringUnavailable so the caller treats it as a startup refusal.
func LoadKeyring(path string) (*Keyring, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: no keyring file is configured", ErrKeyringUnavailable)
	}
	file, err := os.Open(path) // #nosec G304 -- the path is the operator-configured keyring mount.
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open the keyring file", ErrKeyringUnavailable)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyringBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read the keyring file", ErrKeyringUnavailable)
	}
	if len(raw) > maxKeyringBytes {
		return nil, fmt.Errorf("%w: the keyring file is too large", ErrKeyringUnavailable)
	}
	return ParseKeyring(raw)
}

// ParseKeyring validates a keyring document. It is separate from LoadKeyring so tests and tools
// can build a keyring without a file. The input slice is not retained.
func ParseKeyring(raw []byte) (*Keyring, error) {
	var file keyringFile
	if err := decodeStrict(bytes.NewReader(raw), &file); err != nil {
		return nil, fmt.Errorf("%w: the keyring is not a single valid document", ErrKeyringUnavailable)
	}
	if len(file.Keys) == 0 {
		return nil, fmt.Errorf("%w: the keyring declares no keys", ErrKeyringUnavailable)
	}
	keys := make(map[string][]byte, len(file.Keys))
	for id, encoded := range file.Keys {
		if !validKeyID(id) {
			return nil, fmt.Errorf("%w: a keyring entry has an invalid key identifier", ErrKeyringUnavailable)
		}
		material, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(material) != keyBytes {
			return nil, fmt.Errorf("%w: keyring entry %q is not a %d-byte base64 key", ErrKeyringUnavailable, id, keyBytes)
		}
		keys[id] = material
	}
	if _, ok := keys[file.Primary]; !ok {
		return nil, fmt.Errorf("%w: the primary key is not present in the keyring", ErrKeyringUnavailable)
	}
	return &Keyring{primary: file.Primary, keys: keys}, nil
}

// validKeyID accepts short printable identifiers. The id is persisted beside every wrapping and
// authenticated as AAD, so it must be stable and unambiguous, never a free-form string.
func validKeyID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// PrimaryID names the KEK that wraps new DEKs. It identifies a key without revealing it, so it is
// safe to log.
func (k *Keyring) PrimaryID() string { return k.primary }

// wrap seals a DEK under the primary KEK with AAD = kekId, so a wrapping cannot be relabeled to
// another key.
func (k *Keyring) wrap(dek []byte) (keyID string, nonce, wrapped []byte, err error) {
	gcm, err := k.aead(k.primary)
	if err != nil {
		return "", nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, nil, fmt.Errorf("draw wrapping nonce: %w", err)
	}
	return k.primary, nonce, gcm.Seal(nil, nonce, dek, []byte(k.primary)), nil
}

// unwrap opens a DEK with the KEK the wrapping names. A KEK absent from the ring is a refusal,
// never a fallback to another key.
func (k *Keyring) unwrap(keyID string, nonce, wrapped []byte) ([]byte, error) {
	gcm, err := k.aead(keyID)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("wrapped data key has a malformed nonce")
	}
	dek, err := gcm.Open(nil, nonce, wrapped, []byte(keyID))
	if err != nil {
		return nil, errors.New("wrapped data key failed authentication")
	}
	return dek, nil
}

func (k *Keyring) aead(keyID string) (cipher.AEAD, error) {
	material, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("key-encryption key %q is not in the keyring", keyID)
	}
	return newGCM(material)
}

// newGCM builds AES-256-GCM for one key. Errors are fixed strings so key bytes never reach them.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("encryption key is unusable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("encryption key is unusable")
	}
	return gcm, nil
}
