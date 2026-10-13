package credentialstore

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// Resolve failures. D7 requires the wire codes to tell unauthorized, revoked, version mismatch and
// store-unavailable apart without revealing anything about the secret, so each outcome the Store can
// reach has its own sentinel and the transport maps them to fixed codes.
var (
	// ErrAuthorizationUnverifiable reports that this deployment configured no verification key, so the
	// Store cannot decide whether any authorization is genuine. It is the fail-closed default: a Store
	// with no key refuses every redemption rather than trusting the caller.
	ErrAuthorizationUnverifiable = errors.New("authorization verification is not configured")
	// ErrAuthorizationInvalid reports a token that is not a Cloud-issued authorization. Malformed
	// tokens and tokens whose signature does not verify share this outcome on purpose: telling a
	// caller which of the two it sent would help it iterate on a forgery.
	ErrAuthorizationInvalid = errors.New("authorization is not a valid Cloud-issued authorization")
	// ErrAuthorizationExpired reports an authorization whose lifetime has closed.
	ErrAuthorizationExpired = errors.New("authorization has expired")
	// ErrAuthorizationConsumed reports an authorization that was already spent. It is a conflict
	// rather than a refusal because the first redemption may well have succeeded.
	ErrAuthorizationConsumed = errors.New("authorization has already been consumed")
	// ErrAuthorizationBinding reports an authorization that does not match the credential it names or
	// the runtime presenting it.
	ErrAuthorizationBinding = errors.New("authorization does not match the presented binding")
	// ErrAuthorizationPurpose reports an authorization minted for other work than the path it was
	// presented to. It is separate from ErrAuthorizationBinding because the two call for different
	// responses: a binding mismatch is a caller that presented the wrong values, while a purpose
	// mismatch is a caller presenting an authorization it was never meant to hold — and the operation
	// is signed, so a mismatch means either a bug in the issuer or an attempt at confusion.
	ErrAuthorizationPurpose = errors.New("authorization was issued for a different purpose")
	// ErrVersionUnavailable reports that the bound credential version is not readable here. D4 bounds
	// how long a rotated-away version stays readable, so this is the "version mismatch" outcome D7
	// requires to be distinguishable from "revoked".
	ErrVersionUnavailable = errors.New("credential version is not available")
)

// ResolveRequest is what the credential-executing identity presents, both on the wire and to the
// resolver: the transport decodes this type directly rather than into a second, identical shape.
//
// The runtime fields are the presenting workload's own declaration of which Node instance and
// generation it is. The Store compares them against the authorization's binding, so an authorization
// issued for one generation cannot be spent under another. This is a consistency and fencing check,
// not an attestation of the presenter's identity: the deployment's fixed three-identity table (D3)
// gives every Node the same `broker-resolver` certificate, so nothing in the mutual-TLS handshake
// distinguishes one Node instance from another. Making that distinction cryptographic would need
// per-Node identities, which is a decision the identity table does not currently take.
type ResolveRequest struct {
	// Authorization is the bearer token Cloud issued. It is a secret: it is never logged, audited, or
	// persisted, and the transport clears it before any of those happen.
	Authorization string `json:"authorization"`
	// NodeID, NodeInstanceID, NodeIncarnationID, RuntimeGeneration and ControlEpoch must equal the
	// values the authorization was bound to.
	NodeID            string `json:"nodeId"`
	NodeInstanceID    string `json:"nodeInstanceId"`
	NodeIncarnationID string `json:"nodeIncarnationId"`
	RuntimeGeneration int64  `json:"runtimeGeneration"`
	ControlEpoch      int64  `json:"controlEpoch"`
}

// MirrorResolveRequest is what the credential-executing identity presents to redeem a
// `mirror_prepare` authorization.
//
// It carries the bearer token and nothing else, and that emptiness is the design. A clone redemption
// takes a runtime declaration because a clone runs on a Node whose identity the Store cannot see
// through the deployment's single `broker-resolver` certificate (see ResolveRequest). A mirror
// preparation is executed by C, which runs on no Node at all, so there is no runtime to declare — and
// declaring one would put the party that spends the authorization back in charge of saying who it is.
// Who may spend it comes from two places the request cannot reach: the operation, which is signed, and
// the caller's identity, which the transport resolves from the verified client certificate.
type MirrorResolveRequest struct {
	// Authorization is the bearer token Cloud issued. It is a secret: it is never logged, audited, or
	// persisted, and the transport clears it before any of those happen.
	Authorization string `json:"authorization"`
}

// ResolvedCredential is one delivered secret.
//
// The token leaves the Store only through this value and only after the consumption record is
// durable, so a caller that receives one can be sure the authorization behind it can never be spent
// again.
type ResolvedCredential struct {
	// Token is the user-managed HTTPS Git token. It is the only plaintext secret this package ever
	// returns.
	Token string
	// Version is the credential generation that was delivered, so the caller's audit trail can name
	// it without re-deriving it.
	Version int64
}

// The two outcomes D10 requires a consumption record to distinguish. The record is created with
// `consumed` and only rewritten once the secret is in hand, so a crash between the two leaves the
// conservative reading: spent, not delivered.
const (
	consumptionConsumed  = "consumed_not_delivered"
	consumptionDelivered = "delivered"
)

// consumptionRecord is the Store's own evidence that one authorization was spent (D10).
//
// It lives in the Store's volume rather than in Cloud's database, the Node's database, or a process
// journal: the decision it records is the Store's, and a party that could edit the record could
// un-spend an authorization. It carries no secret and nothing from which the secret can be
// recovered — only which authorization was spent, for which purpose, when, by which runtime or
// identity, and with what outcome.
type consumptionRecord struct {
	AuthorizationID string `json:"authorizationId"`
	CredentialID    string `json:"credentialId"`
	// CredentialVersion is recorded so the audit trail names the material, not just the reference.
	CredentialVersion int64 `json:"credentialVersion"`
	// Operation is the purpose the authorization was minted for. It is recorded rather than inferred
	// from which fields are populated, because the two purposes leave different traces and an operator
	// reading the record should not have to work out which one it is holding.
	Operation  string    `json:"operation"`
	ConsumedAt time.Time `json:"consumedAt"`
	// NodeID, NodeInstanceID, NodeIncarnationID, RuntimeGeneration and ControlEpoch are the runtime a
	// clone declared. They are empty for a mirror preparation, which runs on no Node.
	NodeID            string `json:"nodeId,omitempty"`
	NodeInstanceID    string `json:"nodeInstanceId,omitempty"`
	NodeIncarnationID string `json:"nodeIncarnationId,omitempty"`
	RuntimeGeneration int64  `json:"runtimeGeneration,omitempty"`
	ControlEpoch      int64  `json:"controlEpoch,omitempty"`
	// ExecutorIdentity is the identity the transport proved from the client certificate for a mirror
	// preparation. It is empty for a clone, whose binding carries a Node runtime instead: recording
	// which of the two identified the caller is the point, because they are established differently.
	ExecutorIdentity string `json:"executorIdentity,omitempty"`
	// ExpiresAt is the authorization's own expiry. It is retained so an operator can tell a spent
	// authorization from a live one without the token, and so a future sweep can drop records that
	// can no longer be replayed.
	ExpiresAt time.Time `json:"expiresAt"`
	Result    string    `json:"result"`
}

// Resolver holds the resolve capability: the verification keys, and the Store state needed to deliver
// one secret.
//
// It is a separate type from Store on purpose. D3 splits the capabilities so that one compromised
// certificate cannot both replace every secret and read every secret back, and the original design
// made that split a type boundary rather than caller discipline. Keeping Resolve off Store preserves
// it: the ingress path holds a *Store, which has no way to read a secret, and the resolving path holds
// a *Resolver, which has no way to write, rotate, revoke, or enumerate anything.
type Resolver struct {
	store *Store
	// keys is the deployment's verification key set. It is read once, when the Store opens, so a
	// redemption never depends on the key file still being readable: an operator who unmounts or
	// corrupts it does not thereby turn the capability off, and an operator who wants a key withdrawn
	// withdraws it by restarting the Store with the file edited. MaxLifetime bounds what a withdrawn
	// key could still have signed, so the restart is not on any hot path.
	keys []ed25519.PublicKey
}

// Resolver returns the resolve capability, or false when this deployment configured no verification
// key. A false result is not an error: the capability is simply absent, and every redemption is then
// refused rather than trusted.
func (s *Store) Resolver() (*Resolver, bool) {
	if len(s.authorizationKeys) == 0 {
		return nil, false
	}
	return &Resolver{store: s, keys: s.authorizationKeys}, true
}

// Resolve delivers the token one clone authorization names, consuming that authorization on the way.
//
// The order of operations is the whole point of this method, and D3 and D10 fix it:
//
//  1. Authenticate the authorization against the Store's own verification key, and check it against
//     the Store's own clock. The Store does not take the caller's word that either already happened.
//  2. Check every binding the Store can check without spending anything: the purpose it was minted
//     for, the credential it names, the version, the runtime presenting it. A failure here leaves the
//     authorization unspent, which is what D5 means by "a failure that did not obtain the secret is
//     always a new authorization" — here it need not even be new.
//  3. Spend the authorization by creating its consumption record, atomically and durably.
//  4. Only then open the secret.
//
// Reversing 3 and 4 would create the one state the decisions forbid: a secret that left the Store
// while the record that proves it was spent is missing, which is exactly a replayable authorization.
//
// The purpose check is first because it is the one refusal that is about the caller rather than about
// the request's contents. A `mirror_prepare` authorization presented here would otherwise be spent on
// a clone: its binding carries no Node runtime at all, so a caller presenting empty runtime values
// would match it field for field, and the single use a preparation is given would be consumed on
// work it was never issued for.
//
//nolint:gocritic // Value parameter on purpose: the runtime declaration checked against the binding has to be the same snapshot that is written into the consumption record.
func (r *Resolver) Resolve(ctx context.Context, credentialID string, request ResolveRequest) (ResolvedCredential, error) {
	store := r.store
	if !validCredentialID(credentialID) {
		return ResolvedCredential{}, fmt.Errorf("%w: credential id is not a canonical identifier", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return ResolvedCredential{}, err
	}
	now := store.now().UTC()
	binding, err := r.verify(request.Authorization, now)
	if err != nil {
		return ResolvedCredential{}, err
	}
	if binding.Operation != cloneauth.OperationClone {
		return ResolvedCredential{}, ErrAuthorizationPurpose
	}
	// Every binding this request can contradict. The credential id is checked against the path rather
	// than the body, so an authorization for one credential can never be spent on another even if the
	// caller rewrites its own request.
	if binding.CredentialID != credentialID ||
		binding.NodeID != request.NodeID ||
		binding.NodeInstanceID != request.NodeInstanceID ||
		binding.NodeIncarnationID != request.NodeIncarnationID ||
		binding.RuntimeGeneration != request.RuntimeGeneration ||
		binding.ControlEpoch != request.ControlEpoch {
		return ResolvedCredential{}, ErrAuthorizationBinding
	}
	return r.deliver(credentialID, binding, now, &consumptionRecord{
		Operation:         cloneauth.OperationClone,
		NodeID:            request.NodeID,
		NodeInstanceID:    request.NodeInstanceID,
		NodeIncarnationID: request.NodeIncarnationID,
		RuntimeGeneration: request.RuntimeGeneration,
		ControlEpoch:      request.ControlEpoch,
	})
}

// ResolveMirror delivers the token one `mirror_prepare` authorization names, consuming that
// authorization on the way.
//
// The steps are Resolve's, and the differences are the point:
//
//  1. The purpose must be `mirror_prepare`. A clone authorization presented here is refused rather
//     than spent, so neither route can consume what the other was given.
//  2. The caller's identity is compared against the binding's `executorIdentity`. `presented` is not a
//     value from the request: the transport resolves it from the verified client certificate, so the
//     party that may spend this authorization is the one the deployment issued a certificate to.
//     Nothing in the body can name it, which is why the request carries only the token.
//  3. The requested version is checked against the Store's own record and the credential's revocation
//     state, exactly as for a clone.
//
// # What this method cannot decide, stated plainly
//
// The Store holds no copy of Cloud's rows and never queries Cloud, so it cannot decide whether the
// Project Operation is still current, whether `preparationGeneration` is still the live attempt, or
// whether the Project's credential binding is still in force. Those are Cloud's facts, and Cloud
// re-checks all of them — against live state, not against this outcome — when it accepts a published
// mirror and when it decides a Workspace may leave the `node` step. Revocation therefore takes effect
// at the next of those checks and at the authorization's own expiry, whichever comes first; it is not
// instantaneous here, and this method must not be described as if it were. What the Store does decide,
// on its own and with its own clock, is that this token was signed by Cloud for this purpose, is
// unspent, is unexpired, names a version it can still read and has not revoked, and is being presented
// by the identity Cloud named. That is the whole of what it claims.
func (r *Resolver) ResolveMirror(ctx context.Context, credentialID, presented string, request MirrorResolveRequest) (ResolvedCredential, error) {
	store := r.store
	if !validCredentialID(credentialID) {
		return ResolvedCredential{}, fmt.Errorf("%w: credential id is not a canonical identifier", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return ResolvedCredential{}, err
	}
	now := store.now().UTC()
	binding, err := r.verify(request.Authorization, now)
	if err != nil {
		return ResolvedCredential{}, err
	}
	if binding.Operation != cloneauth.OperationMirrorPrepare {
		return ResolvedCredential{}, ErrAuthorizationPurpose
	}
	// An empty `presented` is refused rather than compared: a transport that could not establish an
	// identity must not be able to match a binding whose identity happens to be unset, and refusing
	// here means the comparison never depends on that field being populated for the wrong reason.
	if strings.TrimSpace(presented) == "" {
		return ResolvedCredential{}, fmt.Errorf("%w: the caller's service identity was not established", ErrAuthorizationBinding)
	}
	if binding.CredentialID != credentialID || binding.ExecutorIdentity != presented {
		return ResolvedCredential{}, ErrAuthorizationBinding
	}
	return r.deliver(credentialID, binding, now, &consumptionRecord{
		Operation:        cloneauth.OperationMirrorPrepare,
		ExecutorIdentity: presented,
	})
}

// deliver performs the steps both purposes share once the purpose and the binding have been
// established: read the credential, refuse a revoked one or an unavailable version, spend the
// authorization durably, and only then open the secret.
//
// It exists so the order D3 and D10 fix has one implementation rather than two. The two entry points
// differ in what they check before spending — which is where the purpose separation lives — and not in
// what they do afterwards, and a second copy of this sequence is precisely where "validate, then
// spend, then deliver" would eventually be written down in the wrong order.
//
// `record` arrives with only the purpose-specific fields set; everything the record shares is filled
// in here, from the same `binding` and `now` the checks above used.
//
//nolint:gocritic // The binding is passed by value on purpose: the values this spends against are the ones the caller already verified and must not be reachable for writing from here.
func (r *Resolver) deliver(credentialID string, binding cloneauth.Binding, now time.Time, record *consumptionRecord) (ResolvedCredential, error) {
	store := r.store
	stored, found, err := store.files.read(credentialID)
	if err != nil {
		return ResolvedCredential{}, err
	}
	if !found {
		return ResolvedCredential{}, ErrNotFound
	}
	if stored.revoked() {
		return ResolvedCredential{}, ErrRevoked
	}
	material, available := stored.version(binding.CredentialVersion, now)
	if !available {
		return ResolvedCredential{}, ErrVersionUnavailable
	}
	record.AuthorizationID = binding.AuthorizationID
	record.CredentialID = credentialID
	record.CredentialVersion = binding.CredentialVersion
	record.ConsumedAt = now
	record.ExpiresAt = binding.ExpiresAt
	record.Result = consumptionConsumed
	if err := store.claimAuthorization(record); err != nil {
		return ResolvedCredential{}, err
	}
	token, err := store.keys.openToken(credentialID, material)
	if err != nil {
		// The authorization stays spent and the record keeps saying "not delivered". That is the
		// truthful outcome, and the caller must obtain a new authorization rather than retrying this
		// one (D5).
		return ResolvedCredential{}, err
	}
	record.Result = consumptionDelivered
	if err := store.markAuthorizationDelivered(record); err != nil {
		// The secret is already in memory, but returning it would leave a delivered secret whose
		// record still reads "not delivered". Burning the authorization is the conservative outcome
		// and matches D5: a caller that did not obtain the secret asks for a new authorization.
		return ResolvedCredential{}, err
	}
	return ResolvedCredential{Token: token, Version: binding.CredentialVersion}, nil
}

// verify authenticates one token and translates the package's failures into the Store's own
// sentinels, so the transport never has to know how authorizations are encoded.
func (r *Resolver) verify(token string, now time.Time) (cloneauth.Binding, error) {
	binding, err := cloneauth.Verify(r.keys, token, now)
	switch {
	case err == nil:
		return binding, nil
	case errors.Is(err, cloneauth.ErrExpired):
		return cloneauth.Binding{}, ErrAuthorizationExpired
	case errors.Is(err, cloneauth.ErrSignature), errors.Is(err, cloneauth.ErrMalformed):
		return cloneauth.Binding{}, ErrAuthorizationInvalid
	default:
		// A binding that fails validation is signed but not usable; report it as invalid rather than
		// surfacing an internal error, because the caller cannot act on the difference.
		return cloneauth.Binding{}, ErrAuthorizationInvalid
	}
}

// claimAuthorization spends one authorization.
//
// The single-use decision is the filesystem's: `O_CREATE|O_EXCL` lets exactly one caller create this
// file, so two concurrent redemptions of one authorization cannot both proceed, and the answer is a
// directory entry rather than process state — it survives a restart with no extra bookkeeping and no
// schema. The file is flushed and its directory entry is synced before this returns, because D10
// requires the record to be durable before the secret leaves the Store.
func (s *Store) claimAuthorization(record *consumptionRecord) error {
	path, err := s.files.authorizationPath(record.AuthorizationID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return errors.New("consumption record could not be encoded")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return ErrAuthorizationConsumed
	}
	if err != nil {
		return fmt.Errorf("record authorization consumption: %w", err)
	}
	if err := writeAndFlush(file, raw); err != nil {
		// Nothing was delivered, so leaving this authorization unspent is the correct outcome, and
		// removing the unreadable stub keeps the record directory free of entries no reader can parse.
		// A concurrent caller that already saw the file refuses and simply asks for a new
		// authorization; no secret is delivered twice either way.
		_ = os.Remove(path)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// markAuthorizationDelivered rewrites a consumption record once the secret is in hand.
//
// This is the second half of D10's three-phase table. A failure here leaves the record reading
// "consumed, not delivered", which is the conservative of the two readings, so the caller refuses
// rather than reporting a delivery the Store cannot evidence.
func (s *Store) markAuthorizationDelivered(record *consumptionRecord) error {
	path, err := s.files.authorizationPath(record.AuthorizationID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return errors.New("consumption record could not be encoded")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".consumption-*")
	if err != nil {
		return fmt.Errorf("stage consumption record: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := writeAndFlush(temp, raw); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish consumption record: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

// writeAndFlush writes a record and forces it to the volume. It closes the file on every path so a
// failed write cannot leak a descriptor.
func writeAndFlush(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return fmt.Errorf("write consumption record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush consumption record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close consumption record: %w", err)
	}
	return nil
}

// syncDirectory flushes one directory entry.
//
// Flushing a file only guarantees its contents; the name that makes it findable is a separate entry in
// the directory, and on a crash the entry can be lost after the contents were written. The credential
// records deliberately stop at the file's own flush — they are rewritten on every rotation and a lost
// entry costs only a retry — but a consumption record is the evidence that an authorization is spent,
// so its directory is flushed too.
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open consumption record directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("flush consumption record directory: %w", err)
	}
	return nil
}
