package credentialstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// Sentinel failures the Store distinguishes. D7 requires the wire reason codes to tell
// "unauthorized", "revoked", "version mismatch" and "store unavailable" apart without revealing
// anything about the secret, so the transport maps these values to stable codes rather than
// forwarding error text.
var (
	// ErrNotFound reports that no material exists for the given credential id.
	ErrNotFound = errors.New("credential material not found")
	// ErrRevoked reports that the credential was revoked. A revoked credential is a tombstone: D4
	// makes Cloud's metadata authoritative for revocation, and this refuses to undo it.
	ErrRevoked = errors.New("credential material was revoked")
	// ErrConflict reports that the credential already holds different material, so accepting the
	// request would silently replace a secret the caller did not name.
	ErrConflict = errors.New("credential material already exists")
	// ErrInvalidRequest reports input the Store refuses at its own boundary.
	ErrInvalidRequest = errors.New("invalid credential store request")
)

// maxSecretBytes bounds a token the Store will accept. The bound exists so a malformed or hostile
// ingress request cannot turn the Store's volume into an unbounded write target; real HTTPS Git
// tokens are far shorter.
const maxSecretBytes = 4096

// secretRefPrefix marks a Store-generated reference. The prefix is not a security property — the
// entropy below is — it only makes the identifier recognizable as this Store's in a Cloud row.
const secretRefPrefix = "cs_"

// secretRefBytes is the entropy behind one reference. 24 random bytes give ~192 bits, which D1
// requires so a reference cannot be guessed or enumerated even if one leaks.
const secretRefBytes = 24

// Config configures one Store. Both Volume and KeyFile are required: there is no fallback volume and
// no generated key, because either would turn a misconfigured deployment into a silent empty store.
type Config struct {
	// Volume is the Store's own persistent directory. It must not be Cloud's database, another
	// service's volume, or a path shared with the Node's state (D1).
	Volume string
	// KeyFile is the deployment-delivered keyring described by LoadKeyring. The Store never writes
	// it (D2).
	KeyFile string
	// Retention is how long a superseded version stays readable after a rotation, so an execution
	// already admitted under the old version can still settle (D4). Zero selects the default.
	Retention time.Duration
	// Now supplies the clock. It is injected so tests can drive retention and ordering without
	// sleeping.
	Now func() time.Time
}

// DefaultRetention is the grace period a rotated-away version stays readable. It is deliberately
// longer than a clone command timeout so an in-flight execution settles against the version it was
// admitted under instead of failing midway.
const DefaultRetention = time.Hour

// Store holds user-managed Git tokens inside the Store's own volume.
//
// The type exposes exactly the ingress-writer capability set D3 allows: write a new secret, rotate,
// and revoke. There is deliberately no resolve method and no listing method here — D3 gives the
// resolving identity neither write nor enumerate power and gives this identity neither resolve nor
// enumerate power, and a Store that offered them from one type would make the separation a matter of
// caller discipline instead of a boundary.
type Store struct {
	files     *fileStore
	keys      *Keyring
	retention time.Duration
	now       func() time.Time
	// mu serializes the read-modify-write of one record. Writes are rare and short, so one lock over
	// the whole store keeps every cross-store idempotency decision atomic without a lock hierarchy.
	mu sync.Mutex
}

// Open prepares the Store's volume and loads the deployment's key-encryption keys.
//
// Open is the startup gate D6 and D8 require: a missing or unusable KEK, an unreadable or
// non-private volume, or a missing configuration all fail here, before the service accepts any
// request. Returning a ready Store or an error — never a partially initialized one — is what keeps
// "cannot decrypt" from ever presenting as "no credentials yet".
func Open(cfg Config) (*Store, error) {
	files, err := openFileStore(cfg.Volume)
	if err != nil {
		return nil, err
	}
	if err := requirePrivateVolume(files.root); err != nil {
		return nil, err
	}
	keys, err := LoadKeyring(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	retention := cfg.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Store{files: files, keys: keys, retention: retention, now: now}, nil
}

// Volume reports the directory this Store owns. It is safe to log: it names a location, never a
// secret.
func (s *Store) Volume() string { return s.files.root }

// KeyID reports the identifier of the KEK every new wrapping uses. It is safe to log and to store
// beside a ciphertext.
func (s *Store) KeyID() string { return s.keys.PrimaryID() }

// requirePrivateVolume refuses a volume other local accounts can read.
//
// The records are ciphertext, but the volume also carries wrapping metadata and the key identifier,
// and D2's whole argument is that the KEK is the only thing standing between that volume and the
// tokens. A world-readable mount would hand an attacker the offline material for free, so the
// deployment must mount it privately and the Store verifies that rather than assuming it.
func requirePrivateVolume(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("credential store volume does not exist")
		}
		return fmt.Errorf("inspect credential store volume: %w", err)
	}
	if !info.IsDir() {
		return errors.New("credential store volume is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("credential store volume is readable by other local accounts")
	}
	return nil
}

// Write stores a new token for one credential id and returns the reference the Store generated.
//
// The credential id is the idempotency key D4 fixes for the cross-store create: Cloud generates
// `credential_refs.id` first and reuses it on every retry, so a write that already exists must
// return the original reference instead of creating a second secret. Material that differs under
// the same id is refused — that is a caller bug or a substitution attempt, never a retry.
func (s *Store) Write(ctx context.Context, credentialID, token string) (secretRef string, version int64, err error) {
	if err := validateRequest(credentialID, token); err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	stored, found, err := s.files.read(credentialID)
	if err != nil {
		return "", 0, err
	}
	if found {
		if stored.revoked() {
			return "", 0, ErrRevoked
		}
		if stored.Current.Fingerprint != s.keys.fingerprint(token) {
			return "", 0, ErrConflict
		}
		return stored.SecretRef, stored.Version, nil
	}
	sealed, err := s.keys.sealToken(credentialID, token)
	if err != nil {
		return "", 0, err
	}
	now := s.now().UTC()
	created := &record{
		CredentialID: credentialID,
		SecretRef:    newSecretRef(),
		Version:      1,
		Current:      sealed,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.files.write(created); err != nil {
		return "", 0, err
	}
	return created.SecretRef, created.Version, nil
}

// Rotate replaces the material of an existing credential.
//
// Ordering is the point of this method. D4 requires the new version to be written before Cloud
// advances its metadata pointer, so the previous version is kept — with an expiry, not deleted — and
// a rotation that fails after this call still leaves a readable secret rather than a dangling
// reference. Revocation is the mirror image and is ordered the other way round in Revoke.
//
// Re-supplying the current token is a no-op. That is what makes a retried rotate safe: the caller
// cannot tell a retry from a genuine re-rotation of identical material, and treating it as a
// rotation would advance a version nothing depends on while burning the retention window of a
// version an in-flight execution may still need.
func (s *Store) Rotate(ctx context.Context, credentialID, token string) (int64, error) {
	if err := validateRequest(credentialID, token); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stored, found, err := s.files.read(credentialID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrNotFound
	}
	if stored.revoked() {
		return 0, ErrRevoked
	}
	if stored.Current.Fingerprint == s.keys.fingerprint(token) {
		return stored.Version, nil
	}
	sealed, err := s.keys.sealToken(credentialID, token)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	stored.Previous = append(s.prune(stored.Previous, now), retained{
		Version:     stored.Version,
		Material:    stored.Current,
		RetainUntil: now.Add(s.retention),
	})
	stored.Version++
	stored.Current = sealed
	stored.UpdatedAt = now
	if err := s.files.write(stored); err != nil {
		return 0, err
	}
	return stored.Version, nil
}

// Revoke destroys the stored material and leaves a tombstone.
//
// D4 fixes revocation authority in Cloud's metadata and requires the Cloud-side change to happen
// first, then a best-effort delete here; a failure here must never make the credential usable again.
// So this method is idempotent and treats "already absent" as success: the caller's desired end state
// is "no material", and reaching it twice is not an error. The tombstone is what stops a later
// replay of the original create from resurrecting a credential Cloud has already reported as gone.
func (s *Store) Revoke(ctx context.Context, credentialID string) error {
	if !validCredentialID(credentialID) {
		return fmt.Errorf("%w: credential id is not a canonical identifier", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	stored, found, err := s.files.read(credentialID)
	if err != nil {
		return err
	}
	if !found || stored.revoked() {
		return nil
	}
	now := s.now().UTC()
	stored.clearMaterial()
	stored.RevokedAt = &now
	stored.UpdatedAt = now
	return s.files.write(stored)
}

// prune drops superseded versions whose retention window has closed. D4 bounds how long an old
// version stays readable; keeping it past that window would widen the exposure the rotation was
// meant to narrow.
//
// The window is enforced per record on the writes that already load it. Sweeping records nobody
// writes again — and the orphan TTL for secrets Cloud never referenced — is still an open
// deployment question in the ADR, so this method deliberately does not grow into a background job.
func (s *Store) prune(previous []retained, now time.Time) []retained {
	kept := previous[:0]
	for i := range previous {
		if previous[i].RetainUntil.After(now) {
			kept = append(kept, previous[i])
		}
	}
	return kept
}

// validateRequest applies the Store's own boundary checks, independent of whatever the ingress path
// already validated. D3 makes the store trust the ingress-writer identity for user-level
// authorization, but that is not a reason to trust its input shape.
func validateRequest(credentialID, token string) error {
	if !validCredentialID(credentialID) {
		return fmt.Errorf("%w: credential id is not a canonical identifier", ErrInvalidRequest)
	}
	if token == "" || len(token) > maxSecretBytes {
		return fmt.Errorf("%w: token length is not acceptable", ErrInvalidRequest)
	}
	if strings.ContainsRune(token, 0) {
		return fmt.Errorf("%w: token contains a NUL byte", ErrInvalidRequest)
	}
	return nil
}

// newSecretRef draws a fresh opaque reference. D1 places generation here, in the store, so neither
// the browser, the end user, nor the deployer can choose or predict one.
func newSecretRef() string {
	raw := make([]byte, secretRefBytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing means the process has no usable entropy source; continuing would risk
		// a predictable reference, which is exactly what D1 forbids.
		panic(fmt.Sprintf("credential store cannot draw a secret reference: %v", err))
	}
	return secretRefPrefix + base64.RawURLEncoding.EncodeToString(raw)
}
