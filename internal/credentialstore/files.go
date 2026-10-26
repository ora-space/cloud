package credentialstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The volume layout is a durable compatibility boundary:
//
//	<root>/credentials/<credentialId>.json   one record per credential (sealed versions, metadata)
//	<root>/redemptions/<authorizationId>     one claim per redeemed execution authorization
//	<root>/submissions/<submissionId>        one claim per accepted submission grant
const (
	credentialsDir = "credentials"
	redemptionsDir = "redemptions"
	submissionsDir = "submissions"
)

// errClaimed reports that an exclusive claim file already exists: the grant or authorization was
// already used.
var errClaimed = errors.New("claim already exists")

// record is the whole per-credential state.
type record struct {
	CredentialID string `json:"credentialId"`
	// TenantID is fixed by the first accepted submission. A later grant for the same credential id
	// from another tenant is refused, so a credential id cannot be hijacked across tenants.
	TenantID string `json:"tenantId,omitempty"`
	// CurrentVersion is the newest accepted version, 0 when none was ever written. It survives
	// revocation so management reads still report what existed.
	CurrentVersion int64 `json:"currentVersion"`
	// CurrentSubmissionID names the grant that wrote CurrentVersion; Cloud compares it on confirm.
	CurrentSubmissionID string `json:"currentSubmissionId,omitempty"`
	// RevokedAt is the tombstone. A revoked record carries no ciphertext and accepts no version.
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	// Versions holds the current version and superseded versions inside their retention window.
	Versions []version `json:"versions,omitempty"`
}

// version is one sealed generation of a credential.
type version struct {
	Version         int64     `json:"version"`
	SubmissionID    string    `json:"submissionId"`
	Attribution     string    `json:"attribution"`
	OwnerUserID     string    `json:"ownerUserId"`
	RepositoryScope string    `json:"repositoryScope"`
	StoredAt        time.Time `json:"storedAt"`
	// SupersededAt is set when a newer version is accepted; the version stays redeemable for the
	// retention window after it, for authorizations Cloud issued before the rotation.
	SupersededAt *time.Time `json:"supersededAt,omitempty"`
	Sealed       sealed     `json:"sealed"`
}

// claim is the content of a redemption or submission claim file. It is evidence only; the file's
// existence is what makes the claim exclusive.
type claim struct {
	ID           string    `json:"id"`
	CredentialID string    `json:"credentialId"`
	Version      int64     `json:"version"`
	Purpose      string    `json:"purpose"`
	ClaimedAt    time.Time `json:"claimedAt"`
}

// volume is the Store's private directory tree.
type volume struct {
	root string
}

// openVolume prepares the tree and refuses a root other local accounts can read: the records are
// ciphertext, but the KEK is then the only barrier, and a readable volume hands out offline
// material for free.
func openVolume(root string) (*volume, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, errors.New("credential store root must be an absolute path")
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("prepare credential store root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect credential store root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("credential store root is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("credential store root is accessible to other local accounts")
	}
	for _, dir := range []string{credentialsDir, redemptionsDir, submissionsDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return nil, fmt.Errorf("prepare credential store %s directory: %w", dir, err)
		}
	}
	return &volume{root: root}, nil
}

// path joins one validated identifier under dir. Identifiers are canonical UUIDs, so they are
// always a single safe segment; the check is repeated here so no caller can bypass it.
func (v *volume) path(dir, id, suffix string) (string, error) {
	if !validUUID(id) {
		return "", errors.New("identifier is not a canonical UUID")
	}
	return filepath.Join(v.root, dir, id+suffix), nil
}

// readRecord loads one record; found=false with a nil error means no record exists.
func (v *volume) readRecord(credentialID string) (*record, bool, error) {
	path, err := v.path(credentialsDir, credentialID, ".json")
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is built from a validated UUID under the store root.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read credential record: %w", err)
	}
	var stored record
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, false, errors.New("credential record is unreadable")
	}
	if stored.CredentialID != credentialID {
		return nil, false, errors.New("credential record does not match its address")
	}
	return &stored, true, nil
}

// writeRecord replaces a record atomically: temp file, fsync, rename, directory fsync. A record
// that returned is one a restart still sees, and a crash never leaves a truncated record. The
// rename also unlinks the previous inode, which is how revocation and the retention sweep destroy
// ciphertext.
func (v *volume) writeRecord(stored *record) error {
	path, err := v.path(credentialsDir, stored.CredentialID, ".json")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return errors.New("credential record could not be encoded")
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("stage credential record: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := writeSynced(temp, raw); err != nil {
		return fmt.Errorf("write credential record: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish credential record: %w", err)
	}
	return syncDir(dir)
}

// listRecords returns every credential id with a record.
func (v *volume) listRecords() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(v.root, credentialsDir))
	if err != nil {
		return nil, fmt.Errorf("list credential records: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && entry.Type().IsRegular() && validUUID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// createClaim creates dir/<id> exclusively and durably. O_CREATE|O_EXCL makes the filesystem the
// single-use authority, which unlike an in-memory set survives a restart; the file and its
// directory entry are flushed before the caller acts on the claim.
func (v *volume) createClaim(dir string, c *claim) error {
	path, err := v.path(dir, c.ID, "")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return errors.New("claim could not be encoded")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- validated UUID under the store root.
	if errors.Is(err, fs.ErrExist) {
		return errClaimed
	}
	if err != nil {
		return fmt.Errorf("create claim: %w", err)
	}
	if err := writeSynced(file, raw); err != nil {
		return fmt.Errorf("write claim: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// claimExists reports whether dir/<id> exists.
func (v *volume) claimExists(dir, id string) (bool, error) {
	path, err := v.path(dir, id, "")
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect claim: %w", err)
	}
	return true, nil
}

// sweepClaims removes claims older than cutoff. A claim only has to outlive the grant or
// authorization it spent; once that has expired, Verify refuses a replay on its own.
func (v *volume) sweepClaims(dir string, cutoff time.Time) (int, error) {
	base := filepath.Join(v.root, dir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}
	removed := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !validUUID(entry.Name()) {
			continue
		}
		path := filepath.Join(base, entry.Name())
		raw, err := os.ReadFile(path) // #nosec G304 -- validated UUID under the store root.
		if err != nil {
			return removed, fmt.Errorf("read claim: %w", err)
		}
		var c claim
		// An unreadable claim is kept: deleting it could reopen a replay window.
		if json.Unmarshal(raw, &c) != nil || c.ClaimedAt.IsZero() || !c.ClaimedAt.Before(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("remove claim: %w", err)
		}
		removed++
	}
	if removed > 0 {
		return removed, syncDir(base)
	}
	return removed, nil
}

// writeSynced writes, flushes and closes file; the file is closed on every path.
func writeSynced(file *os.File, raw []byte) error {
	_, err := file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// syncDir flushes a directory so a rename or create inside it is durable.
func syncDir(dir string) error {
	handle, err := os.Open(dir) // #nosec G304 -- one of the store's own directories.
	if err != nil {
		return fmt.Errorf("open directory for flushing: %w", err)
	}
	err = handle.Sync()
	return errors.Join(err, handle.Close())
}

// validUUID accepts only the canonical lowercase UUID spelling, because the same value must be a
// stable identifier and a safe file name.
func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
