package credentialstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The Store's recovery path is its volume: an export of the directory it writes and a restore of that
// export in its place. The decisions the Store owns — which authorization was spent, which credential
// was revoked — are recorded in that directory and nowhere else, so a restore is the one operation
// that could silently undo either of them. This file exercises the drill rather than the mechanism:
// it exports the volume, empties it, puts the export back, and then asks the recovered Store the two
// questions a replay would ask.
//
// What this proves, and what it does not, is worth stating before the assertions. It proves the
// volume carries the decisions taken before the export, so neither a spent authorization nor a revoked
// credential comes back with the backup. It does not — and cannot, from inside the Store — cover a
// decision Cloud took *after* the export: an older backup restored over a newer revocation leaves the
// Store with material it has no reason to refuse. The authority for revocation is Cloud's metadata
// (`credential_refs.availability`), which is what refuses to mint an authorization in the first place;
// `docs/` and the credential ADRs place that boundary there deliberately, and this test does not claim
// otherwise.

// TestARestoreDoesNotReviveAConsumedAuthorizationOrARevokedCredential drives the three answers a
// recovered Store can give, so the two refusals are read against a delivery rather than against a
// Store that came back broken.
//
// The three credentials are written before the export and differ only in what happened to them
// afterwards: one authorization was spent, one credential was revoked, and the third was left alone.
// The third is the control that keeps the other two from passing for the wrong reason — a restore that
// produced an unreadable volume, a keyring the recovered Store could not open, or a clock that moved
// would refuse everything, and the control would fail.
func TestARestoreDoesNotReviveAConsumedAuthorizationOrARevokedCredential(t *testing.T) {
	fixture := newResolveFixture(t)
	spentOn := testCredentialID(71)
	revokedBefore := testCredentialID(72)
	untouched := testCredentialID(73)
	for id, token := range map[string]string{
		spentOn:       "ghp_consumed_before_the_restore",
		revokedBefore: "ghp_revoked_before_the_restore",
		untouched:     "ghp_untouched_by_the_restore",
	} {
		storeCredential(t, fixture.store, id, token)
	}

	// Each authorization carries its own id, because the id is what names the consumption record: two
	// grants sharing one id would make the second refusal a duplicate of the first rather than a
	// separate answer.
	authorize := func(credentialID string, sequence int) string {
		t.Helper()
		binding := fixture.binding(credentialID, 1)
		binding.AuthorizationID = testAuthorizationID(sequence)
		return fixture.issue(t, binding)
	}
	spentAuthorization := authorize(spentOn, 71)

	// The two decisions, taken before the export so the export is what has to carry them.
	if _, err := fixture.resolver.Resolve(context.Background(), spentOn, testResolverIdentity, fixture.request(spentAuthorization)); err != nil {
		t.Fatalf("Resolve before the restore: %v", err)
	}
	if err := fixture.store.Revoke(context.Background(), revokedBefore); err != nil {
		t.Fatalf("Revoke before the restore: %v", err)
	}

	snapshot := snapshotVolume(t, fixture.volume)
	restoreVolume(t, snapshot, fixture.volume)

	// The recovered Store is opened the way the deployment opens one — same volume, same keyring, same
	// verification key, same clock — so the only thing that changed is that its directory now holds the
	// export instead of the files it wrote.
	recovered, err := Open(Config{
		Volume:                     fixture.volume,
		KeyFile:                    fixture.keyFile,
		Retention:                  DefaultRetention,
		Now:                        fixture.clock.Now,
		AuthorizationPublicKeyFile: fixture.publicFile,
	})
	if err != nil {
		t.Fatalf("Open the restored volume: %v", err)
	}
	resolver, ok := recovered.Resolver()
	if !ok {
		t.Fatal("the restored Store reported no resolve capability although its verification key is still configured")
	}

	// The replay. This is the outcome the obligation names as forbidden: a backup that left the
	// consumption records out of the export would let one authorization be spent twice, and the token
	// it names would leave the Store a second time.
	if _, err := resolver.Resolve(context.Background(), spentOn, testResolverIdentity, fixture.request(spentAuthorization)); !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("replaying a spent authorization after a restore: got %v, want %v", err, ErrAuthorizationConsumed)
	}
	// The revocation. A fresh authorization is presented, so nothing about the replay above can be the
	// reason for this refusal, and the credential is not spent — only withdrawn.
	fresh := authorize(revokedBefore, 72)
	if _, err := resolver.Resolve(context.Background(), revokedBefore, testResolverIdentity, fixture.request(fresh)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("resolving a revoked credential after a restore: got %v, want %v", err, ErrRevoked)
	}
	// The control: a credential that was neither spent nor revoked still delivers, and its own
	// authorization is spent by this call rather than having been spent before.
	live := authorize(untouched, 73)
	token, err := resolver.Resolve(context.Background(), untouched, testResolverIdentity, fixture.request(live))
	if err != nil {
		t.Fatalf("a credential the backup recorded as usable must still resolve: %v", err)
	}
	if token.Token != "ghp_untouched_by_the_restore" {
		t.Fatalf("the restored Store delivered %q, want the material the export held", token.Token)
	}
}

// snapshotVolume copies a Store's volume to a directory outside it and returns that directory: the
// export a backup takes, with the volume's whole tree and nothing else.
func snapshotVolume(t *testing.T, volume string) string {
	t.Helper()
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	copyVolumeTree(t, volume, snapshot)
	return snapshot
}

// restoreVolume puts an export back where the Store reads from, after removing the directory the Store
// wrote, so what the recovered Store opens is the export rather than a mixture of the two.
func restoreVolume(t *testing.T, snapshot, volume string) {
	t.Helper()
	if err := os.RemoveAll(volume); err != nil {
		t.Fatalf("empty the volume before restoring: %v", err)
	}
	copyVolumeTree(t, snapshot, volume)
}

// copyVolumeTree copies one directory tree over another, creating the destination. The volume holds
// records, consumption records and staging files, so the copy is of every file rather than of the
// entries one test happens to name.
func copyVolumeTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatalf("create %s: %v", to, err)
	}
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
}
