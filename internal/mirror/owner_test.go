//go:build unix

package mirror

// One mirror root, two identities. The credential executor publishes into it as its own uid and the
// Git Server reads, reclaims and records both of those as root, and the deployment fixes the volume's
// ownership once before either starts. Everything this package creates therefore has to belong to
// that owner rather than to whoever happened to write it — a root-owned 0600 record or lock file is
// one the executor cannot open, which turns the reclaimer's own bookkeeping into a store the
// publisher can no longer use.
//
// The two behavioral cases below are the two ways that can go: the process can give the file the
// owner, or it cannot, and the second must fail the write rather than leave the file behind. Which of
// the two runs here depends on the identity the suite is run under, because the property being
// asserted is about privilege: as root the adoption can be observed against a foreign owner, and as
// anybody else it can only be observed as a refusal. The file is Unix-only because the property is:
// there is no uid to adopt, and no chown to fail, elsewhere.
//
// The privileged half therefore skips under the ordinary suite, which runs as an unprivileged user,
// and it is the half that reproduces the defect. Run it under an identity that can chown:
//
//	CGO_ENABLED=0 go test -c -o /tmp/mirror.test ./internal/mirror/
//	docker run --rm --user 0:0 --network none -v /tmp/mirror.test:/mirror.test:ro \
//	    alpine:3.22 /mirror.test -test.run 'StoreFilesCarryTheVolumeOwner' -test.v

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// fileOwner reports the uid owning a path, failing the test if the platform cannot say.
func fileOwner(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	uid, ok := volumeOwner(info)
	if !ok {
		t.Fatalf("this platform reports no file owner for %s", path)
	}
	return uid
}

// TestStoreFilesCarryTheVolumeOwner is the defect that was measured: the reclaimer ran as root over a
// volume the deployment had fixed to the executor's uid, wrote a record as root, and every later
// publication failed with a permission error naming a file the executor had never touched.
//
// It needs privilege, because the only way to tell adoption from accident is to make the volume's
// owner somebody this process is not — which is exactly what the deployment does when it fixes the
// volume to the executor's uid and then runs the Git Server as root.
func TestStoreFilesCarryTheVolumeOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("observing adoption against a foreign volume owner needs a privileged process")
	}
	root := t.TempDir()
	// A uid no process in the test is, so a file that keeps the writing process's identity is visible
	// as such rather than coinciding with the owner.
	foreign := os.Geteuid() + 4242
	if err := os.Chown(root, foreign, -1); err != nil {
		t.Fatalf("give the volume an owner: %v", err)
	}
	store, _ := newTestStore(t, func(cfg *Config) { cfg.Root = root })
	if store.ownerUID != foreign {
		t.Fatalf("the store adopted uid %d, want the volume's %d", store.ownerUID, foreign)
	}

	key := testKey(testCommitA)
	record := Record{Key: key, State: StatePreparing, Generation: 1, Ref: testRef}
	if err := store.writeRecord(&record); err != nil {
		t.Fatalf("write a record: %v", err)
	}
	if got := fileOwner(t, store.recordPath(key.Digest())); got != foreign {
		t.Fatalf("the record is owned by uid %d, want the volume's %d", got, foreign)
	}

	// The lock file matters as much as the record and fails differently: a lock file the other
	// identity cannot open is not a lock somebody holds, it is every later publication of that key
	// failing at the lock.
	lock, err := store.keyLock(key.Digest())
	if err != nil {
		t.Fatalf("open the key lock: %v", err)
	}
	if got := fileOwner(t, filepath.Join(store.locksDir(), key.Digest()+".lock")); got != foreign {
		t.Fatalf("the lock file is owned by uid %d, want the volume's %d", got, foreign)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close the key lock: %v", err)
	}

	// A directory the store had to create is the same statement: the writer has to be able to enter
	// it, and a directory only root can open is a store the executor cannot use at all.
	if err := os.RemoveAll(store.recordsDir()); err != nil {
		t.Fatalf("remove the records directory: %v", err)
	}
	if err := store.ensureDir(store.recordsDir()); err != nil {
		t.Fatalf("recreate the records directory: %v", err)
	}
	if got := fileOwner(t, store.recordsDir()); got != foreign {
		t.Fatalf("the recreated directory is owned by uid %d, want the volume's %d", got, foreign)
	}
}

// TestStoreRefusesAFileTheVolumeOwnerCouldNotRead is the other half, and it is a refusal rather than
// a best effort: if the store cannot give a file the volume's owner, the write must fail. Silently
// keeping the file would leave exactly the state this exists to prevent — a store that works for the
// process that wrote it and is unusable for the other one, discovered much later and somewhere else.
func TestStoreRefusesAFileTheVolumeOwnerCouldNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process cannot fail to change a file's owner, so the refusal is unreachable")
	}
	store, _ := newTestStore(t, nil)
	// The volume is owned by a uid this process is not and cannot become, which is what a wrongly
	// prepared volume looks like to the process that has to write into it.
	store.ownerUID = os.Geteuid() + 4242

	key := testKey(testCommitA)
	record := Record{Key: key, State: StatePreparing, Generation: 1, Ref: testRef}
	err := store.writeRecord(&record)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("writing a record into a volume owned by another uid returned %v, want ErrUnavailable", err)
	}
	if _, statErr := os.Stat(store.recordPath(key.Digest())); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the refused record was left behind: %v", statErr)
	}

	if _, err := store.keyLock(key.Digest()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("opening a key lock in a volume owned by another uid returned %v, want ErrUnavailable", err)
	}
}

// TestVolumeOwnerReportsTheOwnerOfTheVolume pins the reading the rest of this file depends on, so a
// change that made `volumeOwner` stop recognizing a Unix stat would fail here rather than silently
// switching adoption off for every store in the deployment.
func TestVolumeOwnerReportsTheOwnerOfTheVolume(t *testing.T) {
	store, _ := newTestStore(t, nil)
	info, err := os.Stat(store.Root())
	if err != nil {
		t.Fatalf("stat the root: %v", err)
	}
	uid, ok := volumeOwner(info)
	if !ok {
		t.Fatal("a Unix stat was not recognized as reporting a file owner")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("the root's Sys() is %T, not a Unix stat", info.Sys())
	}
	if uid != int(stat.Uid) {
		t.Fatalf("volumeOwner reported uid %d, want the directory's %d", uid, stat.Uid)
	}
}
