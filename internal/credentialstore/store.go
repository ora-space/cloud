package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// SupersededRetention is how long a superseded version stays redeemable after a newer version is
// accepted. It covers authorizations Cloud issued against the old version before the rotation
// (cloneauth.MaxLifetime is far shorter); after it the ciphertext is destroyed.
const SupersededRetention = time.Hour

// claimRetention is how long redemption and submission claims are kept. A claim only has to
// outlive the authorization or grant it spent (at most cloneauth.MaxLifetime plus clock skew);
// a day leaves a wide margin and keeps the volume bounded.
const claimRetention = 24 * time.Hour

// Store outcomes the HTTP layer maps to stable codes. They carry no secret material.
var (
	errNotFound        = errors.New("credential not found")
	errUnavailable     = errors.New("credential version unavailable")
	errRevoked         = errors.New("credential revoked")
	errVersionConflict = errors.New("expected store version does not follow the current version")
	errReplayed        = errors.New("submission grant already used")
	errAlreadyRedeemed = errors.New("authorization already redeemed")
	errTenantMismatch  = errors.New("credential belongs to another tenant")
)

// StoreConfig configures OpenStore. Every field is required.
type StoreConfig struct {
	// Root is the Store's private volume (mode 0700). It is created if absent.
	Root string
	// Keyring is the deployment's KEK set; see LoadKeyring.
	Keyring *Keyring
	// Now is the Store's own clock. Authorization expiry and version retention are judged by it,
	// never by a caller-supplied time.
	Now func() time.Time
}

// Store holds the sealed credentials on one volume. Its write operations are serialized by mu;
// redemption holds the read side across "check availability, claim, decrypt" so a completed
// revocation can never be followed by a delivery from a record read before it.
type Store struct {
	volume *volume
	keys   *Keyring
	now    func() time.Time
	mu     sync.RWMutex
}

// OpenStore validates the configuration, prepares the volume and returns a ready Store.
func OpenStore(cfg StoreConfig) (*Store, error) {
	if cfg.Keyring == nil {
		return nil, fmt.Errorf("%w: no keyring was provided", ErrKeyringUnavailable)
	}
	if cfg.Now == nil {
		return nil, errors.New("credential store requires a clock")
	}
	v, err := openVolume(cfg.Root)
	if err != nil {
		return nil, err
	}
	return &Store{volume: v, keys: cfg.Keyring, now: cfg.Now}, nil
}

// KeyID reports the primary KEK identifier. It is safe to log.
func (s *Store) KeyID() string { return s.keys.PrimaryID() }

// Metadata is the management view of one credential. It never carries plaintext or ciphertext.
type Metadata struct {
	CredentialID string `json:"credentialId"`
	// CurrentVersion is the newest accepted version, 0 when none exists.
	CurrentVersion int64 `json:"currentVersion"`
	// SubmissionID is the submission grant that wrote CurrentVersion, "" when none exists.
	SubmissionID string `json:"submissionId"`
	Revoked      bool   `json:"revoked"`
}

// submit writes the version a verified submission grant names.
//
// Order: refuse a revoked or foreign-tenant record, refuse a reused grant, check that the grant's
// expected version is exactly current+1, claim the grant exclusively, then write the record. A
// crash after the claim leaves the grant spent and no version written; the browser asks Cloud for
// a new grant, which is safe because no version was half-written.
func (s *Store) submit(ctx context.Context, grant *cloneauth.Submission, secret Secret) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stored, found, err := s.volume.readRecord(grant.CredentialID)
	if err != nil {
		return 0, err
	}
	if !found {
		stored = &record{CredentialID: grant.CredentialID}
	}
	if stored.RevokedAt != nil {
		return 0, errRevoked
	}
	if stored.TenantID != "" && stored.TenantID != grant.TenantID {
		return 0, errTenantMismatch
	}
	used, err := s.volume.claimExists(submissionsDir, grant.SubmissionID)
	if err != nil {
		return 0, err
	}
	if used {
		return 0, errReplayed
	}
	if grant.ExpectedStoreVersion != stored.CurrentVersion+1 {
		return 0, errVersionConflict
	}
	next := grant.ExpectedStoreVersion
	sealedSecret, err := s.keys.seal(grant.CredentialID, next, secret)
	if err != nil {
		return 0, fmt.Errorf("seal secret: %w", err)
	}
	now := s.now().UTC()
	err = s.volume.createClaim(submissionsDir, &claim{
		ID: grant.SubmissionID, CredentialID: grant.CredentialID, Version: next,
		Purpose: string(cloneauth.SubmissionPurpose), ClaimedAt: now,
	})
	if errors.Is(err, errClaimed) {
		return 0, errReplayed
	}
	if err != nil {
		return 0, err
	}
	for i := range stored.Versions {
		if stored.Versions[i].SupersededAt == nil {
			stored.Versions[i].SupersededAt = &now
		}
	}
	stored.Versions = append(s.retained(stored.Versions, now), version{
		Version: next, SubmissionID: grant.SubmissionID, Attribution: grant.Attribution,
		OwnerUserID: grant.OwnerUserID, RepositoryScope: grant.RepositoryScope, StoredAt: now,
		Sealed: sealedSecret,
	})
	stored.TenantID = grant.TenantID
	stored.CurrentVersion = next
	stored.CurrentSubmissionID = grant.SubmissionID
	if err := s.volume.writeRecord(stored); err != nil {
		return 0, err
	}
	return next, nil
}

// redeem delivers the secret one verified execution authorization names, at most once.
//
// The redemption claim is created durably before decryption, so a crash or a decryption failure
// after it can only lose a delivery, never allow a second one.
func (s *Store) redeem(ctx context.Context, authorization *cloneauth.Authorization) (Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	stored, found, err := s.volume.readRecord(authorization.CredentialID)
	if err != nil {
		return Secret{}, err
	}
	if !found || (stored.CurrentVersion == 0 && stored.RevokedAt == nil) {
		return Secret{}, errNotFound
	}
	if stored.RevokedAt != nil {
		return Secret{}, errUnavailable
	}
	now := s.now().UTC()
	var target *version
	for i := range stored.Versions {
		if stored.Versions[i].Version == authorization.CredentialVersion && s.live(&stored.Versions[i], now) {
			target = &stored.Versions[i]
		}
	}
	if target == nil {
		return Secret{}, errUnavailable
	}
	err = s.volume.createClaim(redemptionsDir, &claim{
		ID: authorization.AuthorizationID, CredentialID: authorization.CredentialID,
		Version: target.Version, Purpose: string(authorization.Purpose), ClaimedAt: now,
	})
	if errors.Is(err, errClaimed) {
		return Secret{}, errAlreadyRedeemed
	}
	if err != nil {
		return Secret{}, err
	}
	secret, err := s.keys.open(authorization.CredentialID, target.Version, &target.Sealed)
	if err != nil {
		return Secret{}, fmt.Errorf("decrypt credential version: %w", err)
	}
	return secret, nil
}

// metadata reports the management view. An unknown id is not an error: Cloud asks about ids it
// created and the browser may not have submitted yet.
func (s *Store) metadata(ctx context.Context, credentialID string) (Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	stored, found, err := s.volume.readRecord(credentialID)
	if err != nil {
		return Metadata{}, err
	}
	if !found {
		return Metadata{CredentialID: credentialID}, nil
	}
	return Metadata{
		CredentialID:   credentialID,
		CurrentVersion: stored.CurrentVersion,
		SubmissionID:   stored.CurrentSubmissionID,
		Revoked:        stored.RevokedAt != nil,
	}, nil
}

// revoke writes the tombstone and destroys every sealed version in the same atomic record
// replacement. It is idempotent, and revoking an unknown id still leaves a tombstone so a late
// submission cannot create the credential afterwards.
func (s *Store) revoke(ctx context.Context, credentialID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	stored, found, err := s.volume.readRecord(credentialID)
	if err != nil {
		return err
	}
	if !found {
		stored = &record{CredentialID: credentialID}
	}
	if stored.RevokedAt != nil && len(stored.Versions) == 0 {
		return nil
	}
	now := s.now().UTC()
	if stored.RevokedAt == nil {
		stored.RevokedAt = &now
	}
	stored.Versions = nil
	return s.volume.writeRecord(stored)
}

// rewrap moves every DEK under the primary KEK and reports how many were rewrapped. Each record
// is rewritten atomically, so an interruption leaves every record readable under the old or the
// new KEK, and a retry continues where it stopped.
func (s *Store) rewrap(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.volume.listRecords()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		stored, found, err := s.volume.readRecord(id)
		if err != nil {
			return total, err
		}
		if !found {
			continue
		}
		changed := 0
		for i := range stored.Versions {
			moved, err := s.keys.rewrap(&stored.Versions[i].Sealed)
			if err != nil {
				return total, fmt.Errorf("rewrap credential %s: %w", id, err)
			}
			if moved {
				changed++
			}
		}
		if changed == 0 {
			continue
		}
		if err := s.volume.writeRecord(stored); err != nil {
			return total, err
		}
		total += changed
	}
	return total, nil
}

// SweepResult counts what one Sweep destroyed.
type SweepResult struct {
	// Versions is the number of superseded versions whose ciphertext was destroyed.
	Versions int
	// Claims is the number of expired redemption and submission claims removed.
	Claims int
}

// Sweep destroys superseded versions past SupersededRetention and claims past their retention.
// Redemption already refuses an expired version without waiting for a sweep; the sweep is what
// removes its ciphertext from the volume. The lock is taken per record so the sweep never blocks
// redemption for longer than one record rewrite.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var result SweepResult
	ids, err := s.volume.listRecords()
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		removed, err := s.sweepRecord(id)
		result.Versions += removed
		if err != nil {
			return result, err
		}
	}
	cutoff := s.now().UTC().Add(-claimRetention)
	for _, dir := range []string{redemptionsDir, submissionsDir} {
		removed, err := s.volume.sweepClaims(dir, cutoff)
		result.Claims += removed
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) sweepRecord(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, found, err := s.volume.readRecord(id)
	if err != nil || !found {
		return 0, err
	}
	kept := s.retained(stored.Versions, s.now().UTC())
	removed := len(stored.Versions) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	stored.Versions = kept
	return removed, s.volume.writeRecord(stored)
}

// live reports whether a version may still be redeemed: the current version always, a superseded
// one only strictly inside its retention window.
func (*Store) live(v *version, now time.Time) bool {
	return v.SupersededAt == nil || now.Before(v.SupersededAt.Add(SupersededRetention))
}

// retained returns the versions that are still live, reusing the input's backing array.
func (s *Store) retained(versions []version, now time.Time) []version {
	kept := versions[:0]
	for i := range versions {
		if s.live(&versions[i], now) {
			kept = append(kept, versions[i])
		}
	}
	return kept
}
