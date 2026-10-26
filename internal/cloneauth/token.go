// Package cloneauth signs and verifies the one-time authorizations of the external Git line.
//
// Two kinds of grant share one token format and nothing else:
//
//   - an Authorization lets the Repo Executor start one sync, publish or probe task and redeem the
//     credential that task names at the Credential Store;
//   - a Submission lets a browser write one new credential version into the Credential Store.
//
// A token is `base64url(payload) "." base64url(signature)` (RawURLEncoding), where the signature is
// Ed25519 over the exact payload bytes. Signing what is transmitted rather than a re-encoded form means
// verification never depends on a canonical JSON encoding, so no field ordering or escaping difference
// can make two payloads verify as one.
//
// Only Cloud holds the private keys; verifiers hold a set of public keys so a deployment can rotate the
// signing key with an overlap window. Execution and submission grants use distinct key pairs, and
// each payload type is decoded strictly (unknown fields rejected) and carries its own purpose, so a
// token minted for one never verifies as the other even if a deployment reused a key.
package cloneauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// MaxClockSkew is how far in the future a grant's IssuedAt may lie relative to the verifier's clock.
// Issuer and verifiers run on different hosts; a larger tolerance would let a grant be used before
// the instant its issuer claims to have made it.
const MaxClockSkew = time.Minute

// maxTokenLength bounds the input Verify decodes. Every caller reads the token from a bounded request
// body, but the bound here keeps the package safe on its own.
const maxTokenLength = 16 << 10

// Sentinel failures. Callers map them to stable reason codes with errors.Is; none carries payload or
// key content.
var (
	// ErrMalformed reports a token that is not two base64url parts or whose payload is not strictly
	// the expected JSON object.
	ErrMalformed = errors.New("cloneauth: token is malformed")
	// ErrSignature reports a well-formed token that verifies under none of the configured keys.
	ErrSignature = errors.New("cloneauth: signature is not valid")
	// ErrExpired reports a grant whose ExpiresAt is not after the verifier's clock.
	ErrExpired = errors.New("cloneauth: grant has expired")
	// ErrInvalid reports a grant whose fields break the rules of its purpose, whose lifetime is out of
	// range, or whose IssuedAt lies beyond MaxClockSkew in the verifier's future.
	ErrInvalid = errors.New("cloneauth: grant is invalid")
	// ErrKey reports a missing or unusable signing or verification key.
	ErrKey = errors.New("cloneauth: key is not usable")
)

// sign marshals payload and returns the token. The caller has validated payload already.
func sign(key ed25519.PrivateKey, payload any) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: signing key is not an Ed25519 private key", ErrKey)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("cloneauth: encode payload: %w", err)
	}
	signature := ed25519.Sign(key, encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// open authenticates token under any of keys and strictly decodes its payload into target.
//
// The signature is checked before the payload is parsed, so an unauthenticated party never reaches the
// JSON decoder or the field rules.
func open(token string, keys []ed25519.PublicKey, target any) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: no verification key is configured", ErrKey)
	}
	for _, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: verification key is not an Ed25519 public key", ErrKey)
		}
	}
	payload, signature, err := split(token)
	if err != nil {
		return err
	}
	authenticated := false
	for _, key := range keys {
		if ed25519.Verify(key, payload, signature) {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return ErrSignature
	}
	return decodeStrict(payload, target)
}

// split separates a token into payload and signature bytes. Exactly one separator is accepted, so the
// meaning of a token never depends on how its parts were counted.
func split(token string) (payload, signature []byte, err error) {
	if len(token) > maxTokenLength {
		return nil, nil, fmt.Errorf("%w: token is too long", ErrMalformed)
	}
	encodedPayload, encodedSignature, found := strings.Cut(token, ".")
	if !found || encodedPayload == "" || strings.Contains(encodedSignature, ".") {
		return nil, nil, ErrMalformed
	}
	payload, err = base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: payload is not base64url", ErrMalformed)
	}
	signature, err = base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, nil, fmt.Errorf("%w: signature is not an Ed25519 signature", ErrMalformed)
	}
	return payload, signature, nil
}

// decodeStrict decodes exactly one JSON object with no unknown fields and no trailing value. The
// unknown-field rule is also what keeps an execution payload from decoding as a submission and back.
func decodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the payload", ErrMalformed)
	}
	return nil
}

// checkLifetime enforces the shared time rules: both instants set and UTC, and a lifetime in
// (0, maxLifetime].
func checkLifetime(issuedAt, expiresAt time.Time, maxLifetime time.Duration) error {
	if issuedAt.IsZero() || expiresAt.IsZero() {
		return fmt.Errorf("%w: issuedAt and expiresAt are required", ErrInvalid)
	}
	if issuedAt.Location() != time.UTC || expiresAt.Location() != time.UTC {
		return fmt.Errorf("%w: issuedAt and expiresAt must be UTC", ErrInvalid)
	}
	lifetime := expiresAt.Sub(issuedAt)
	if lifetime <= 0 {
		return fmt.Errorf("%w: expiresAt is not after issuedAt", ErrInvalid)
	}
	if lifetime > maxLifetime {
		return fmt.Errorf("%w: lifetime exceeds %s", ErrInvalid, maxLifetime)
	}
	return nil
}

// checkWindow applies the verifier's clock: a grant issued beyond the skew in the future is invalid,
// and one whose expiry is not after now has expired.
func checkWindow(issuedAt, expiresAt, now time.Time) error {
	if issuedAt.After(now.Add(MaxClockSkew)) {
		return fmt.Errorf("%w: issuedAt is in the future", ErrInvalid)
	}
	if !now.Before(expiresAt) {
		return ErrExpired
	}
	return nil
}
