// Package credentialstore implements the independent Credential Service that holds user-managed
// personal HTTPS Git tokens for Ora Cloud.
//
// The package owns the security boundary described by
// `specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md`:
//
//   - The Store is a separate process with its own persistent volume and its own data store. It
//     shares no database instance, connection, credential, or volume with Cloud's business database,
//     so exporting Cloud's database never yields a token (D1).
//   - A token is sealed with AES-256-GCM under a per-secret data key; only the data key is wrapped by
//     the key-encryption key. Cloud's process never holds decryption capability, and the KEK is
//     delivered by the deployment as a file, never generated or persisted here (D2).
//   - `secret_ref` is an opaque, high-entropy identifier this Store generates on a successful write.
//     Browsers, end users, and deployers may never supply one (D1).
//   - Only mutually authenticated service identities are accepted. The three fixed identities have
//     non-overlapping capabilities (D3).
//   - A missing or unusable KEK is a startup refusal, never an empty store (D6, D8).
package credentialstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// keyBytes is the AES-256 key length every key in the keyring must have.
const keyBytes = 32

// ErrKeyMissing reports that the deployment did not deliver a usable key-encryption key. It is
// deliberately distinct from a corrupt keyring: D8 requires "no KEK" to be a startup refusal
// rather than an empty store, because an empty store would report "cannot decrypt" as "never
// existed".
var ErrKeyMissing = errors.New("key-encryption key is not available")

// Keyring holds the key-encryption keys the deployment delivered, one of which is primary.
//
// The ring holds more than one key because D8 requires every wrapping to carry the identifier of the
// key that produced it: during a KEK rotation both the new and the old key must be able to open
// existing wrappings. The Store never writes the ring back — KEK material belongs to the deployment,
// not to the Store's runtime or its volume.
type Keyring struct {
	primary string
	keys    map[string][]byte
}

// keyringFile is the on-volume-free JSON shape the deployment mounts for `--kek-file`.
//
// It is a set rather than a single key so KEK rotation is a deployment change with no Store-side
// data migration. `primary` names the key every new wrapping uses; the rest stay usable for
// unwrapping.
type keyringFile struct {
	Primary string         `json:"primary"`
	Keys    []keyringEntry `json:"keys"`
}

type keyringEntry struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// LoadKeyring reads the KEK set from a mounted file.
//
// Every failure is reported as ErrKeyMissing (wrapped) so callers cannot accidentally treat a
// misconfigured deployment as "no credentials yet": D8 makes the absence of a usable KEK a startup
// refusal. The error never contains key material.
func LoadKeyring(path string) (*Keyring, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: no key-encryption key file is configured", ErrKeyMissing)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read the configured key-encryption key file", ErrKeyMissing)
	}
	var file keyringFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%w: the key-encryption key file is not a valid keyring", ErrKeyMissing)
	}
	if len(file.Keys) == 0 {
		return nil, fmt.Errorf("%w: the keyring declares no keys", ErrKeyMissing)
	}
	keys := make(map[string][]byte, len(file.Keys))
	for _, entry := range file.Keys {
		if strings.TrimSpace(entry.ID) == "" {
			return nil, fmt.Errorf("%w: a keyring entry has no key identifier", ErrKeyMissing)
		}
		material, err := base64.StdEncoding.DecodeString(entry.Key)
		if err != nil || len(material) != keyBytes {
			return nil, fmt.Errorf("%w: keyring entry %q is not a %d-byte base64 key", ErrKeyMissing, entry.ID, keyBytes)
		}
		keys[entry.ID] = material
	}
	if _, ok := keys[file.Primary]; !ok {
		return nil, fmt.Errorf("%w: the primary key %q is not present in the keyring", ErrKeyMissing, file.Primary)
	}
	return &Keyring{primary: file.Primary, keys: keys}, nil
}

// PrimaryID names the key every new wrapping uses. It is safe to log and to store beside a
// ciphertext; it identifies a key without revealing it.
func (k *Keyring) PrimaryID() string { return k.primary }

// newDEK draws one data key for one secret. D2 requires a per-secret DEK so that a single DEK
// compromise cannot decrypt the whole store.
func (k *Keyring) newDEK() ([]byte, error) {
	dek := make([]byte, keyBytes)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("draw data key: %w", err)
	}
	return dek, nil
}

// wrap seals a DEK under the primary KEK. The returned key id is persisted next to the wrapping so a
// later unwrap can pick the right key after a KEK rotation.
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

// unwrap opens a DEK with the KEK named in the wrapping. The key id is authenticated as additional
// data, so a wrapping cannot be relabeled to a different key.
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

// aead builds the AES-256-GCM primitive for one key. A key named in a persisted wrapping but absent
// from the ring means the deployment dropped a KEK that still has live wrappings: that is a refusal,
// not a fallback to another key.
func (k *Keyring) aead(keyID string) (cipher.AEAD, error) {
	material, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("key-encryption key %q is not available", keyID)
	}
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, fmt.Errorf("key-encryption key %q is unusable", keyID)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("key-encryption key %q is unusable", keyID)
	}
	return gcm, nil
}

// fingerprint derives the stable, non-reversible identity of a token under the primary KEK.
//
// The Store needs to answer "is this the same secret material I already hold?" so a retried write or
// rotate is a no-op instead of a second copy (D4). A plain hash would be an offline-checkable
// verifier for a low-entropy token, so the digest is keyed with the KEK: an attacker holding the
// Store's volume still cannot test candidate tokens without the KEK.
func (k *Keyring) fingerprint(token string) string {
	mac := hmac.New(sha256.New, k.keys[k.primary])
	mac.Write([]byte(k.primary))
	mac.Write([]byte{0})
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sealToken encrypts a token under a fresh DEK and wraps that DEK under the primary KEK.
//
// The credential id is authenticated as additional data so a sealed blob cannot be moved to another
// credential's record without failing authentication.
func (k *Keyring) sealToken(credentialID, token string) (material, error) {
	dek, err := k.newDEK()
	if err != nil {
		return material{}, err
	}
	defer clear(dek)
	block, err := aes.NewCipher(dek)
	if err != nil {
		return material{}, errors.New("data key is unusable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return material{}, errors.New("data key is unusable")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return material{}, fmt.Errorf("draw token nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), []byte(credentialID))
	keyID, wrapNonce, wrapped, err := k.wrap(dek)
	if err != nil {
		return material{}, err
	}
	return material{
		KeyID:       keyID,
		DEKNonce:    encodeBase64(wrapNonce),
		WrappedDEK:  encodeBase64(wrapped),
		Nonce:       encodeBase64(nonce),
		Ciphertext:  encodeBase64(sealed),
		Fingerprint: k.fingerprint(token),
	}, nil
}

// openToken reverses sealToken. D6 requires a failed integrity check to end as an explicit failure:
// this method never returns partial plaintext and the caller must not retry the decryption.
//
// The material is taken by pointer because one sealed version is 96 bytes and callers already hold it
// in place; nothing here writes to it.
func (k *Keyring) openToken(credentialID string, m *material) (string, error) {
	dek, err := k.unwrap(m.KeyID, decodeBase64(m.DEKNonce), decodeBase64(m.WrappedDEK))
	if err != nil {
		return "", err
	}
	defer clear(dek)
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", errors.New("data key is unusable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("data key is unusable")
	}
	nonce := decodeBase64(m.Nonce)
	if len(nonce) != gcm.NonceSize() {
		return "", errors.New("sealed token has a malformed nonce")
	}
	plain, err := gcm.Open(nil, nonce, decodeBase64(m.Ciphertext), []byte(credentialID))
	if err != nil {
		return "", errors.New("sealed token failed authentication")
	}
	return string(plain), nil
}

// encode and decode keep persisted key material in one textual form. Recording is base64 rather than
// raw bytes so a record stays valid JSON and a diff of two records stays readable.
func encodeBase64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func decodeBase64(text string) []byte {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil
	}
	return raw
}
