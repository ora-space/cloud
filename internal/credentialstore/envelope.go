package credentialstore

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Secret is one stored Git credential. It exists only in process memory: it is sealed before it
// touches the volume and is handed out only in a redemption response.
type Secret struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// String redacts the secret so an accidental %v or %s in a log statement cannot print it.
func (Secret) String() string { return "credentialstore.Secret{[redacted]}" }

// GoString redacts the secret for %#v as well.
func (Secret) GoString() string { return "credentialstore.Secret{[redacted]}" }

// sealed is one envelope-encrypted secret version as persisted. None of its fields is a secret on
// its own; reaching the plaintext needs the KEK named by KEKID.
type sealed struct {
	// KEKID names the KEK that wrapped the DEK; it is the AAD of the wrapping.
	KEKID      string `json:"kekId"`
	WrapNonce  string `json:"wrapNonce"`
	WrappedDEK string `json:"wrappedDek"`
	// Nonce and Ciphertext are the AES-256-GCM sealing of the secret JSON under the DEK, with
	// AAD = credentialId + "\x00" + version so a ciphertext cannot be replayed under another
	// credential or version.
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// secretAAD binds a ciphertext to exactly one credential version.
func secretAAD(credentialID string, version int64) []byte {
	return []byte(credentialID + "\x00" + strconv.FormatInt(version, 10))
}

// seal encrypts secret under a fresh DEK and wraps the DEK under the primary KEK. A per-version DEK
// means one leaked DEK opens one version only.
func (k *Keyring) seal(credentialID string, version int64, secret Secret) (sealed, error) {
	plain, err := json.Marshal(secret)
	if err != nil {
		return sealed{}, errors.New("encode secret")
	}
	defer clear(plain)
	dek := make([]byte, keyBytes)
	if _, err := rand.Read(dek); err != nil {
		return sealed{}, fmt.Errorf("draw data key: %w", err)
	}
	defer clear(dek)
	gcm, err := newGCM(dek)
	if err != nil {
		return sealed{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return sealed{}, fmt.Errorf("draw secret nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plain, secretAAD(credentialID, version))
	keyID, wrapNonce, wrapped, err := k.wrap(dek)
	if err != nil {
		return sealed{}, err
	}
	return sealed{
		KEKID:      keyID,
		WrapNonce:  encode(wrapNonce),
		WrappedDEK: encode(wrapped),
		Nonce:      encode(nonce),
		Ciphertext: encode(ciphertext),
	}, nil
}

// open reverses seal. Any integrity failure is a hard error with a fixed message: no partial
// plaintext is ever returned, and the caller must not retry with another key.
func (k *Keyring) open(credentialID string, version int64, s *sealed) (Secret, error) {
	wrapNonce, err1 := decode(s.WrapNonce)
	wrapped, err2 := decode(s.WrappedDEK)
	nonce, err3 := decode(s.Nonce)
	ciphertext, err4 := decode(s.Ciphertext)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return Secret{}, errors.New("sealed secret is malformed")
	}
	dek, err := k.unwrap(s.KEKID, wrapNonce, wrapped)
	if err != nil {
		return Secret{}, err
	}
	defer clear(dek)
	gcm, err := newGCM(dek)
	if err != nil {
		return Secret{}, err
	}
	if len(nonce) != gcm.NonceSize() {
		return Secret{}, errors.New("sealed secret has a malformed nonce")
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, secretAAD(credentialID, version))
	if err != nil {
		return Secret{}, errors.New("sealed secret failed authentication")
	}
	defer clear(plain)
	var secret Secret
	if err := json.Unmarshal(plain, &secret); err != nil {
		return Secret{}, errors.New("sealed secret is not a valid document")
	}
	return secret, nil
}

// rewrap moves one DEK under the primary KEK without touching the secret ciphertext. It reports
// false when the DEK is already wrapped by the primary.
func (k *Keyring) rewrap(s *sealed) (bool, error) {
	if s.KEKID == k.primary {
		return false, nil
	}
	wrapNonce, err1 := decode(s.WrapNonce)
	wrapped, err2 := decode(s.WrappedDEK)
	if errors.Join(err1, err2) != nil {
		return false, errors.New("wrapped data key is malformed")
	}
	dek, err := k.unwrap(s.KEKID, wrapNonce, wrapped)
	if err != nil {
		return false, err
	}
	defer clear(dek)
	keyID, nonce, rewrapped, err := k.wrap(dek)
	if err != nil {
		return false, err
	}
	s.KEKID, s.WrapNonce, s.WrappedDEK = keyID, encode(nonce), encode(rewrapped)
	return true, nil
}

func encode(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func decode(text string) ([]byte, error) { return base64.StdEncoding.DecodeString(text) }
