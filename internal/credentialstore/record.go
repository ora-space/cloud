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

// material is one sealed version of a token plus everything needed to open it again.
//
// Every field except Fingerprint is ciphertext or wrapping metadata; none of it is a secret on its
// own, which is what lets the store's volume be backed up without carrying the KEK (D8). Reaching
// the token requires the KEK as well.
type material struct {
	// KeyID names the KEK that wrapped DEKNonce/WrappedDEK, so a rotation can still open old
	// wrappings (D2, D8).
	KeyID string `json:"keyId"`
	// DEKNonce and WrappedDEK are the AES-256-GCM wrapping of the per-secret data key.
	DEKNonce   string `json:"dekNonce"`
	WrappedDEK string `json:"wrappedDek"`
	// Nonce and Ciphertext are the AES-256-GCM sealing of the token under the data key.
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	// Fingerprint is the keyed digest that lets a replayed write of the same token be recognized as
	// a replay rather than stored twice. It is not a verifier: it cannot be tested without the KEK.
	Fingerprint string `json:"fingerprint"`
}

// retained is a superseded version kept for a bounded grace period.
//
// D4 requires rotation to write the new version first and delete the old one only after the
// metadata pointer has advanced, so an execution already in flight can still settle against the
// version it was admitted under.
type retained struct {
	Version     int64     `json:"version"`
	Material    material  `json:"material"`
	RetainUntil time.Time `json:"retainUntil"`
}

// record is the Store's whole per-credential state. It is keyed by Cloud's `credential_refs.id`,
// which D4 names as the idempotency key for the cross-store create.
type record struct {
	CredentialID string `json:"credentialId"`
	// SecretRef is generated once, by this Store, and never changes: Cloud persists it as the
	// locating identifier for this credential's material (D1).
	SecretRef string `json:"secretRef"`
	// Version counts material generations written by this Store.
	Version   int64      `json:"version"`
	Current   material   `json:"current"`
	Previous  []retained `json:"previous,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	// RevokedAt is the tombstone that keeps a revoked credential from being written again. D4 fixes
	// revocation authority in Cloud's metadata, so this tombstone is defense in depth: it stops a
	// replay from resurrecting a secret Cloud has already reported as unusable.
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// revoked reports whether the record is a tombstone.
func (r *record) revoked() bool { return r.RevokedAt != nil }

// clearMaterial drops the sealed bytes of one version. Revocation destroys the stored secret rather
// than only marking it, so a later reader cannot recover it even with the KEK.
func (r *record) clearMaterial() {
	r.Current = material{}
	for i := range r.Previous {
		r.Previous[i].Material = material{}
	}
	r.Previous = nil
}

// fileStore is the Store's own persistent data store: one JSON record per credential inside the
// Store's private volume.
//
// A filesystem store — rather than a table in Cloud's database — is what D1 asks for: the Store
// shares no database instance, connection, credential, or volume with Cloud, and it must be
// replaceable by an external secret manager without Cloud or Node observing the change. The record
// contents are already ciphertext, so the volume can be snapshotted under the deployment's own
// retention policy.
type fileStore struct {
	root string
}

// credentialDirectory is the single subdirectory holding every record. Keeping records one level
// down means the volume root can also hold the store's other, non-secret state without colliding
// with a credential id.
const credentialDirectory = "credentials"

// openFileStore prepares the record directory. The directory is created only if it is absent; an
// existing directory is validated by the caller so a wrong mount point cannot silently become a new
// empty store.
func openFileStore(root string) (*fileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("credential store volume is not configured")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve credential store volume: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absolute, credentialDirectory), 0o700); err != nil {
		return nil, fmt.Errorf("prepare credential store volume: %w", err)
	}
	return &fileStore{root: absolute}, nil
}

// path returns the record path for one credential id.
//
// The id reaches this store from Cloud's ingress only, but it is still treated as untrusted input at
// the boundary: only a single, non-traversing path segment is accepted so a crafted id can never
// address a file outside the record directory.
func (f *fileStore) path(credentialID string) (string, error) {
	if !validCredentialID(credentialID) {
		return "", errors.New("credential id is not a single safe path segment")
	}
	dir := filepath.Join(f.root, credentialDirectory)
	name := filepath.Join(dir, credentialID+".json")
	if filepath.Dir(name) != dir {
		return "", errors.New("credential id escapes the record directory")
	}
	return name, nil
}

// validCredentialID accepts the canonical lowercase UUID form Cloud generates and nothing else.
func validCredentialID(id string) bool {
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

// read loads one record. A missing record is reported as ok=false with no error so callers can tell
// "not present" from "unreadable": the first is a normal first write, the second must fail closed.
func (f *fileStore) read(credentialID string) (*record, bool, error) {
	path, err := f.path(credentialID)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(path)
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

// write replaces a record atomically.
//
// A partial record would be worse than a missing one — a truncated wrapping reads as "corrupt
// ciphertext" and D6 forbids treating that as anything but a hard failure — so the new bytes land in
// a sibling temporary file, are flushed, and are renamed over the target. The temporary file is
// always removed, on every path.
func (f *fileStore) write(stored *record) error {
	path, err := f.path(stored.CredentialID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return errors.New("credential record could not be encoded")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return fmt.Errorf("stage credential record: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("restrict credential record: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write credential record: %w", err)
	}
	// The rename below is only durable once the file's own bytes have reached the volume.
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("flush credential record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close credential record: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish credential record: %w", err)
	}
	return nil
}
