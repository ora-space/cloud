// Package cloneauth issues and verifies the single-use authorization that lets the
// credential-executing identity resolve exactly one Git token for exactly one clone.
//
// The approved decisions fix what an authorization binds, how long it lives, and where it is
// consumed, but they deliberately leave its cryptographic form open:
//
//   - `20261010-user-managed-git-secret-store` D3 has the Store itself perform the two-factor
//     resolution and D10 makes the Store persist the consumption record, so the Store must be able to
//     decide on its own that an authorization is genuine.
//   - D10 also forbids the issuer from pre-registering anything in the Store ("Cloud 只做绑定与签发,
//     不做任何'已用'标记,也不预留消费记录"), so authenticity cannot come from a record the issuer
//     planted at issuance time.
//
// The only form that satisfies both is an authorization the issuer signs and the Store verifies with
// a public key. Without it, anything holding the `broker-resolver` certificate could mint an
// authorization for any tenant's credential — the multi-tenant isolation the Node decision
// (`20261010-controlled-node-git-authentication-isolation` D14-B item 11) requires would rest on
// caller discipline alone.
//
// The signature is Ed25519 over the exact transmitted payload bytes. Signing what is transmitted
// rather than a re-encoded form means verification never depends on a canonical JSON encoding, so
// there is no field ordering or escaping difference that could make a genuine authorization fail or,
// worse, two different payloads verify as one.
//
// # Key ownership
//
// The private half belongs to Cloud, and only to Cloud: it is the ability to authorize, and D10's
// consumption record is only meaningful while exactly one party can mint what it records. It is
// mounted as a file reference rather than a configuration value, like every other deployment key.
// The Store holds the public half only, so a compromised Store cannot mint.
//
// Rotation is the deployment's, and this package only makes it possible: the Store's verification
// file may carry several public keys, so an overlap window exists in which authorizations signed by
// either the outgoing or the incoming key are accepted. Revocation is the same mechanism in reverse —
// removing a key from that file and restarting the Store — and MaxLifetime is what makes it
// sufficient, because no authorization signed by a withdrawn key can still be live once the window
// has passed. Neither operation is performed by this package: there is no in-band key distribution
// and no key identifier in the token, so a deployment that rotates keys is expected to do so by
// editing the mounted file, not by asking the Store to trust something new over the wire.
package cloneauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// OperationClone is the only operation an authorization permits today. It is carried in the payload
// and checked by the Store so an authorization minted for a future operation cannot be replayed
// against the clone path, or the reverse.
const OperationClone = "clone"

// MaxLifetime bounds any authorization this package will issue.
//
// D5 requires an authorization to live no longer than one clone command timeout. This package cannot
// see that timeout, so it enforces a ceiling instead: the caller picks the real value, and a bug or a
// misconfiguration cannot turn a single-use authorization into a long-lived bearer token.
//
// It also bounds the damage a compromised signing key can do, which is what makes the rotation
// procedure in this package's README sufficient: every authorization an attacker mints with a stolen
// key stops being acceptable within this window, whether or not the deployment has noticed the theft.
const MaxLifetime = 15 * time.Minute

// Sentinel failures. Issue and Verify report these so a caller can map them to stable reason codes
// without inspecting error text, and so no failure can accidentally carry payload content.
var (
	// ErrIncomplete reports a binding that is missing a field the decisions require.
	ErrIncomplete = errors.New("authorization binding is incomplete")
	// ErrMalformed reports a token that is not a well-formed authorization.
	ErrMalformed = errors.New("authorization is malformed")
	// ErrSignature reports a token whose signature does not verify under any of the configured keys. It
	// is deliberately distinct from ErrMalformed: a well-formed token that fails verification is the
	// signature of a forgery attempt rather than of a bug.
	ErrSignature = errors.New("authorization signature is not valid")
	// ErrExpired reports an authorization whose lifetime has closed.
	ErrExpired = errors.New("authorization has expired")
	// ErrLifetime reports a requested lifetime above MaxLifetime.
	ErrLifetime = errors.New("authorization lifetime exceeds the permitted maximum")
	// ErrKey reports a missing or unusable signing or verification key.
	ErrKey = errors.New("authorization key is not available")
)

// Binding is everything one authorization is bound to.
//
// The set is not a convenience envelope: each field is a value the Store can check against its own
// state or against what the presenting identity declares, so that an authorization captured for one
// clone cannot be spent on another. Tenant, owner, project and workspace are the scope decisions D4
// fixes; credential id and version pin the exact material; repository pins the target; the Node
// instance, incarnation, runtime generation and control epoch are the fencing values that must match
// the runtime the clone was admitted under.
type Binding struct {
	// AuthorizationID is the identifier the Store records consumption under. Cloud mints it once and
	// it must be a canonical UUID, because the Store turns it into a file name.
	AuthorizationID string `json:"authorizationId"`
	// Operation is the single action this authorization permits.
	Operation string `json:"operation"`
	// TenantID and OwnerUserID are the scope the credential belongs to. They are carried so the Store
	// can record and compare them; the Store never uses them to look a credential up, because the
	// credential id is already tenant-scoped on Cloud's side.
	TenantID    string `json:"tenantId"`
	OwnerUserID string `json:"ownerUserId"`
	// ProjectID, WorkspaceID and ExecutionID name the clone this authorization was issued for.
	ProjectID   string `json:"projectId"`
	WorkspaceID string `json:"workspaceId"`
	ExecutionID string `json:"executionId"`
	// CredentialID and CredentialVersion name the exact material. The version is checked against the
	// Store's own record, so an authorization for a rotated-away version cannot be used to fetch the
	// version that replaced it.
	CredentialID      string `json:"credentialId"`
	CredentialVersion int64  `json:"credentialVersion"`
	// Repository is the normalized repository identity the credential is scoped to.
	Repository string `json:"repository"`
	// NodeID, NodeInstanceID, NodeIncarnationID, RuntimeGeneration and ControlEpoch are the fencing
	// values of the runtime the clone was admitted under.
	NodeID            string `json:"nodeId"`
	NodeInstanceID    string `json:"nodeInstanceId"`
	NodeIncarnationID string `json:"nodeIncarnationId"`
	RuntimeGeneration int64  `json:"runtimeGeneration"`
	ControlEpoch      int64  `json:"controlEpoch"`
	// IssuedAt and ExpiresAt bound the lifetime. Both are set by the issuer from its own clock.
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// validate rejects a binding the decisions do not permit. It runs on both issue and verify: on issue
// it stops a caller from minting a malformed authorization, and on verify it stops a signed but
// malformed payload from reaching the Store's checks.
//
//nolint:gocritic // Value receiver on purpose: the snapshot that is validated has to be the snapshot that is signed or checked, so no caller can change the binding between the two.
func (b Binding) validate() error {
	for _, required := range []struct {
		name  string
		value string
	}{
		{"authorization id", b.AuthorizationID},
		{"operation", b.Operation},
		{"tenant id", b.TenantID},
		{"owner user id", b.OwnerUserID},
		{"project id", b.ProjectID},
		{"workspace id", b.WorkspaceID},
		{"execution id", b.ExecutionID},
		{"credential id", b.CredentialID},
		{"repository", b.Repository},
		{"node id", b.NodeID},
		{"node instance id", b.NodeInstanceID},
		{"node incarnation id", b.NodeIncarnationID},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("%w: %s is missing", ErrIncomplete, required.name)
		}
	}
	if b.Operation != OperationClone {
		return fmt.Errorf("%w: operation %q is not %q", ErrIncomplete, b.Operation, OperationClone)
	}
	if b.CredentialVersion < 1 {
		return fmt.Errorf("%w: credential version must be positive", ErrIncomplete)
	}
	if b.RuntimeGeneration < 1 {
		return fmt.Errorf("%w: runtime generation must be positive", ErrIncomplete)
	}
	// The control epoch may be zero, which means "no runtime control had been established when this
	// was issued". That is the normal state for a Workspace clone: Cloud dispatches it as soon as the
	// Node is ready, which is before any holder has claimed the runtime, so the epoch it can honestly
	// bind is the one the control row currently carries — zero. The value is still compared for
	// equality on both sides, so an authorization issued at epoch zero cannot be spent under an epoch
	// that has since advanced.
	if b.ControlEpoch < 0 {
		return fmt.Errorf("%w: control epoch must not be negative", ErrIncomplete)
	}
	if b.IssuedAt.IsZero() || b.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: issued and expiry instants are both required", ErrIncomplete)
	}
	if !b.ExpiresAt.After(b.IssuedAt) {
		return fmt.Errorf("%w: expiry is not after issuance", ErrIncomplete)
	}
	if b.ExpiresAt.Sub(b.IssuedAt) > MaxLifetime {
		return fmt.Errorf("%w: lifetime exceeds %s", ErrLifetime, MaxLifetime)
	}
	return nil
}

// Issue signs one binding and returns the bearer authorization.
//
// The caller supplies the clock through Binding.IssuedAt and Binding.ExpiresAt rather than this
// package reading the wall clock, so issuance is deterministic in tests and the deployment's time
// source stays in one place.
//
//nolint:gocritic // Value parameter on purpose: the binding that is validated and the binding whose bytes are signed must be one snapshot.
func Issue(key ed25519.PrivateKey, binding Binding) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: signing key is not an Ed25519 private key", ErrKey)
	}
	if err := binding.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return "", errors.New("authorization binding could not be encoded")
	}
	signature := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify authenticates one authorization against the deployment's mounted key set and returns its
// binding.
//
// `keys` is a set rather than one key so a deployment can rotate its signing key without an outage:
// during the overlap the Store accepts authorizations signed by either the outgoing or the incoming
// key, so no clone in flight is refused merely because the key changed underneath it. The set is
// ordered only for determinism; a token that verifies under any member is accepted, and a token that
// verifies under none is ErrSignature regardless of which member was tried first.
//
// `now` is the verifier's own clock. The Store passes its own rather than trusting the payload's
// self-reported instants, because an expired authorization is only meaningfully expired relative to
// the party that decides whether to deliver a secret. Clock skew between issuer and Store therefore
// resolves in the Store's favor, which is the direction that fails closed.
func Verify(keys []ed25519.PublicKey, token string, now time.Time) (Binding, error) {
	if len(keys) == 0 {
		return Binding{}, fmt.Errorf("%w: no verification key is configured", ErrKey)
	}
	for _, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return Binding{}, fmt.Errorf("%w: verification key is not an Ed25519 public key", ErrKey)
		}
	}
	payload, signature, err := split(token)
	if err != nil {
		return Binding{}, err
	}
	authenticated := false
	for _, key := range keys {
		if ed25519.Verify(key, payload, signature) {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return Binding{}, ErrSignature
	}
	var binding Binding
	if err := json.Unmarshal(payload, &binding); err != nil {
		return Binding{}, ErrMalformed
	}
	if err := binding.validate(); err != nil {
		return Binding{}, err
	}
	if !now.Before(binding.ExpiresAt) {
		return Binding{}, ErrExpired
	}
	return binding, nil
}

// split separates one token into the signed bytes and the signature.
//
// Exactly one separator is accepted. A token with a second separator is refused rather than having
// one of its parts ignored, so no caller can present a token whose meaning depends on how the parts
// were counted.
func split(token string) (payload, signature []byte, err error) {
	encodedPayload, encodedSignature, found := strings.Cut(token, ".")
	if !found || encodedPayload == "" || encodedSignature == "" {
		return nil, nil, ErrMalformed
	}
	if strings.ContainsRune(encodedSignature, '.') {
		return nil, nil, ErrMalformed
	}
	payload, err = base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil || len(payload) == 0 {
		return nil, nil, ErrMalformed
	}
	signature, err = base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, nil, ErrMalformed
	}
	return payload, signature, nil
}

// LoadSigningKey reads the issuer's Ed25519 private key from a mounted PEM file.
//
// Key material arrives as a file reference and never as a configuration value, matching the rest of
// the deployment. PKCS#8 is what `openssl genpkey -algorithm ED25519` writes, so the deployment
// generates this key with the same tooling it already uses for the Credential Service's own keys.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	block, err := readKeyBlock(path)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: signing key is not a PKCS#8 private key", ErrKey)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: signing key is not an Ed25519 private key", ErrKey)
	}
	return key, nil
}

// LoadVerificationKeys reads the Store's Ed25519 public keys from a mounted PEM file.
//
// The Store holds only public halves. A Store whose volume and process are both compromised still
// cannot mint an authorization, which is the property that keeps D10's consumption record meaningful.
//
// The file may hold more than one key, which is how the deployment rotates without an outage: append
// the incoming public key, restart the Store, switch the issuer's signing key, wait out MaxLifetime,
// then remove the outgoing key and restart again. Every block must be a usable Ed25519 public key —
// a file that mixed a good key with an unusable one is refused at startup rather than silently
// verifying against a subset, because the failure that hides is "the key the operator just added is
// not actually in force", which would only surface after the issuer had already switched.
func LoadVerificationKeys(path string) ([]ed25519.PublicKey, error) {
	blocks, err := readKeyBlocks(path)
	if err != nil {
		return nil, err
	}
	keys := make([]ed25519.PublicKey, 0, len(blocks))
	for _, block := range blocks {
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: verification key is not a PKIX public key", ErrKey)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: verification key is not an Ed25519 public key", ErrKey)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// readKeyBlock reads exactly one PEM block, for key material that has no rotation story and therefore
// no reason to be a set. Two blocks in a signing key file would be ambiguous, so it is refused rather
// than resolved by position.
func readKeyBlock(path string) (*pem.Block, error) {
	blocks, err := readKeyBlocks(path)
	if err != nil {
		return nil, err
	}
	if len(blocks) != 1 {
		return nil, fmt.Errorf("%w: the key file must hold exactly one PEM block", ErrKey)
	}
	return blocks[0], nil
}

// readKeyBlocks reads every PEM block in one file. Errors name the problem without echoing the file's
// contents, so a misconfigured path can be reported without any risk of printing key material.
func readKeyBlocks(path string) ([]*pem.Block, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: no key file is configured", ErrKey)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read the configured key file", ErrKey)
	}
	var blocks []*pem.Block
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: the configured key file holds no PEM block", ErrKey)
	}
	return blocks, nil
}
