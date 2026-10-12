package credentialstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCredentialID builds a canonical lowercase UUID shaped exactly like Cloud's
// `credential_refs.id`, which the Store's validation and path rule both require.
func testCredentialID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

// keyMaterial draws fresh 32-byte key-encryption key material that never leaves the process or the
// test's temporary directory.
func keyMaterial(t *testing.T) []byte {
	t.Helper()
	material := make([]byte, keyBytes)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("draw key material: %v", err)
	}
	return material
}

// writeKeyringFile serializes a keyring the way a deployment mounts one. It lives under a temporary
// path so generated key material can never reach a durable fixture.
func writeKeyringFile(t *testing.T, path, primary string, keys map[string][]byte) string {
	t.Helper()
	file := keyringFile{Primary: primary}
	for id, material := range keys {
		file.Keys = append(file.Keys, keyringEntry{ID: id, Key: base64.StdEncoding.EncodeToString(material)})
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encode keyring: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write keyring: %v", err)
	}
	return path
}

// testClock lets a test drive retention and ordering without sleeping, so the tests prove the
// Store's own time comparisons rather than the wall clock's progress.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
func testStart() time.Time                   { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }

// storeFixture is one Store wired to a temporary volume, a temporary keyring, and an injected clock.
type storeFixture struct {
	store   *Store
	clock   *testClock
	volume  string
	keyFile string
	keys    map[string][]byte
	primary string
}

// openStoreForTest opens a Store, failing the test rather than returning a nil store, so every test
// body works with a ready value.
func openStoreForTest(t *testing.T, volume, keyFile string, retention time.Duration, clock *testClock) *Store {
	t.Helper()
	cfg := Config{Volume: volume, KeyFile: keyFile, Retention: retention}
	if clock != nil {
		cfg.Now = clock.Now
	}
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(volume=%q, keyFile=%q): %v", volume, keyFile, err)
	}
	return store
}

// newStoreFixture builds the common single-key fixture at the default retention and a fixed clock.
func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	return newStoreFixtureWith(t, "kek-primary", DefaultRetention)
}

// newStoreFixtureWith builds a fixture whose keyring has one key named `primary` and whose retention
// window the caller chooses.
func newStoreFixtureWith(t *testing.T, primary string, retention time.Duration) *storeFixture {
	t.Helper()
	base := t.TempDir()
	volume := filepath.Join(base, "volume")
	keys := map[string][]byte{primary: keyMaterial(t)}
	keyFile := writeKeyringFile(t, filepath.Join(base, "kek.json"), primary, keys)
	clock := &testClock{now: testStart()}
	store := openStoreForTest(t, volume, keyFile, retention, clock)
	return &storeFixture{store: store, clock: clock, volume: volume, keyFile: keyFile, keys: keys, primary: primary}
}

// readRecord loads a record through the Store's own read path so a test asserts persistence rather
// than an in-memory copy.
func readRecord(t *testing.T, store *Store, credentialID string) *record {
	t.Helper()
	stored, found, err := store.files.read(credentialID)
	if err != nil {
		t.Fatalf("read record %s: %v", credentialID, err)
	}
	if !found {
		t.Fatalf("record %s not present", credentialID)
	}
	return stored
}

// openTokenOn unwraps and decrypts one sealed version, failing the test when it cannot.
func openTokenOn(t *testing.T, store *Store, credentialID string, m *material) string {
	t.Helper()
	token, err := store.keys.openToken(credentialID, m)
	if err != nil {
		t.Fatalf("open sealed material for %s: %v", credentialID, err)
	}
	return token
}

// readVolumeBytes concatenates every file the Store wrote under its volume, standing in for the
// "export the volume" an attacker or a backup would produce.
func readVolumeBytes(t *testing.T, volume string) []byte {
	t.Helper()
	var all []byte
	err := filepath.WalkDir(volume, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return appendRecordBytes(&all, raw)
	})
	if err != nil {
		t.Fatalf("walk volume: %v", err)
	}
	return all
}

func appendRecordBytes(all *[]byte, raw []byte) error {
	*all = append(*all, raw...)
	*all = append(*all, '\n')
	return nil
}

// assertNoRecordFiles proves a refused Open wrote nothing durable: the volume either does not exist
// or holds no files at all.
func assertNoRecordFiles(t *testing.T, volume string) {
	t.Helper()
	err := filepath.WalkDir(volume, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			t.Errorf("refused Open left file %s in the volume", path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk volume: %v", err)
	}
}

// tokenEncodings lists the textual forms a naive encoding of the token could take, so the export
// assertion covers more than the raw bytes.
func tokenEncodings(token string) []string {
	raw := []byte(token)
	return []string{
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
		hex.EncodeToString(raw),
	}
}

// TestWriteGeneratesOpaqueUniqueReference proves the Store — not the browser, user, or deployer —
// generates secret_ref, and that it is opaque and unique per credential (D1, D4).
func TestWriteGeneratesOpaqueUniqueReference(t *testing.T) {
	fixture := newStoreFixture(t)
	const token = "ghp_shared_token"
	first, version, err := fixture.store.Write(context.Background(), testCredentialID(4), token)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(first, secretRefPrefix) {
		t.Fatalf("secret_ref %q lacks the %q prefix", first, secretRefPrefix)
	}
	if want := len(secretRefPrefix) + base64.RawURLEncoding.EncodedLen(secretRefBytes); len(first) != want {
		t.Fatalf("secret_ref %q has length %d, want %d for %d bytes of entropy", first, len(first), want, secretRefBytes)
	}
	if strings.Contains(first, token) || first == token {
		t.Fatal("secret_ref is derived from the token")
	}
	if version != 1 {
		t.Fatalf("first write version = %d, want 1", version)
	}
	second, _, err := fixture.store.Write(context.Background(), testCredentialID(5), token)
	if err != nil {
		t.Fatalf("Write second credential: %v", err)
	}
	if second == first {
		t.Fatal("two credentials were given the same secret_ref")
	}
}

// TestWriteIsIdempotentPerCredentialID proves the credential id is the cross-store idempotency key:
// a retry returns the original reference, while different material under the same id is refused as a
// substitution rather than silently replacing it (D4).
func TestWriteIsIdempotentPerCredentialID(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(6)
	first, firstVersion, err := fixture.store.Write(context.Background(), id, "token-one")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	retry, retryVersion, err := fixture.store.Write(context.Background(), id, "token-one")
	if err != nil {
		t.Fatalf("repeated Write: %v", err)
	}
	if retry != first || retryVersion != firstVersion {
		t.Fatalf("retry returned (%q,%d), want the original (%q,%d)", retry, retryVersion, first, firstVersion)
	}
	if _, _, err := fixture.store.Write(context.Background(), id, "token-two"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Write with different material error = %v, want ErrConflict", err)
	}
}

// TestWriteRejectsInvalidRequests proves the Store applies its own input boundary — empty,
// over-long, NUL-bearing, or non-canonical — independent of whatever the ingress already checked
// (D4).
func TestWriteRejectsInvalidRequests(t *testing.T) {
	t.Run("non-canonical id", func(t *testing.T) {
		// The id is Cloud's UUID; rejecting anything else here keeps it a single safe path segment and
		// a stable idempotency key rather than attacker-shaped input.
		cases := []struct {
			name       string
			credential string
		}{
			{"empty id", ""},
			{"short id", "abc"},
			{"uppercase id", "00000000-0000-4000-8000-00000000000A"},
			{"non-hex id", "zzzzzzzz-0000-4000-8000-000000000007"},
			{"path traversal id", "../../etc/passwd"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				fixture := newStoreFixture(t)
				ctx := context.Background()
				if _, _, err := fixture.store.Write(ctx, test.credential, "token"); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Write(%s) error = %v, want ErrInvalidRequest", test.name, err)
				}
				if _, err := fixture.store.Rotate(ctx, test.credential, "token"); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Rotate(%s) error = %v, want ErrInvalidRequest", test.name, err)
				}
				if err := fixture.store.Revoke(ctx, test.credential); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Revoke(%s) error = %v, want ErrInvalidRequest", test.name, err)
				}
			})
		}
	})
	t.Run("unacceptable token", func(t *testing.T) {
		// Revoke takes no token, so only the two sealing operations are checked here.
		cases := []struct {
			name  string
			token string
		}{
			{"empty token", ""},
			{"over max token", strings.Repeat("a", maxSecretBytes+1)},
			{"nul in token", "abc\x00def"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				fixture := newStoreFixture(t)
				id := testCredentialID(7)
				ctx := context.Background()
				if _, _, err := fixture.store.Write(ctx, id, test.token); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Write(%s) error = %v, want ErrInvalidRequest", test.name, err)
				}
				if _, err := fixture.store.Rotate(ctx, id, test.token); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Rotate(%s) error = %v, want ErrInvalidRequest", test.name, err)
				}
			})
		}
	})
	t.Run("boundary token accepted", func(t *testing.T) {
		fixture := newStoreFixture(t)
		token := strings.Repeat("a", maxSecretBytes)
		if _, _, err := fixture.store.Write(context.Background(), testCredentialID(8), token); err != nil {
			t.Fatalf("Write with a token of exactly %d bytes failed: %v", maxSecretBytes, err)
		}
	})
}

// TestRotateAdvancesVersionAndRetainsPrevious proves rotation writes the new version and keeps the
// superseded one readable for a bounded grace period, so an execution admitted under the old version
// can still settle (D4).
func TestRotateAdvancesVersionAndRetainsPrevious(t *testing.T) {
	fixture := newStoreFixtureWith(t, "kek-primary", time.Hour)
	id := testCredentialID(9)
	const original, rotated = "ghp_version_one", "ghp_version_two"
	if _, _, err := fixture.store.Write(context.Background(), id, original); err != nil {
		t.Fatalf("Write: %v", err)
	}
	version, err := fixture.store.Rotate(context.Background(), id, rotated)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if version != 2 {
		t.Fatalf("version after one rotation = %d, want 2", version)
	}
	stored := readRecord(t, fixture.store, id)
	if len(stored.Previous) != 1 || stored.Previous[0].Version != 1 {
		t.Fatalf("previous versions = %+v, want exactly version 1", stored.Previous)
	}
	if got := openTokenOn(t, fixture.store, id, &stored.Current); got != rotated {
		t.Fatalf("current version opened as %q, want %q", got, rotated)
	}
	if got := openTokenOn(t, fixture.store, id, &stored.Previous[0].Material); got != original {
		t.Fatalf("retained version opened as %q, want %q", got, original)
	}
	if !stored.Previous[0].RetainUntil.After(fixture.clock.now) {
		t.Fatalf("previous version retention %v is not after now %v", stored.Previous[0].RetainUntil, fixture.clock.now)
	}
}

// TestRotateIdenticalTokenIsNoOp proves re-rotating identical material does not advance a version or
// consume the retention window of a version an in-flight execution may still need (D4).
func TestRotateIdenticalTokenIsNoOp(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(10)
	if _, _, err := fixture.store.Write(context.Background(), id, "same-token"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	version, err := fixture.store.Rotate(context.Background(), id, "same-token")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if version != 1 {
		t.Fatalf("no-op rotate advanced to version %d, want 1", version)
	}
	if stored := readRecord(t, fixture.store, id); len(stored.Previous) != 0 {
		t.Fatalf("no-op rotate created %d superseded versions, want 0", len(stored.Previous))
	}
}

// TestRotateMissingIsNotFound proves rotating a credential the Store never held is an explicit
// not-found rather than an implicit create (D4).
func TestRotateMissingIsNotFound(t *testing.T) {
	fixture := newStoreFixture(t)
	if _, err := fixture.store.Rotate(context.Background(), testCredentialID(11), "token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rotate(missing) error = %v, want ErrNotFound", err)
	}
}

// TestRotationPrunesExpiredPreviousVersion proves a superseded version is dropped once its retention
// window closes while the current version stays readable, bounding how long old material stays
// recoverable (D4).
func TestRotationPrunesExpiredPreviousVersion(t *testing.T) {
	fixture := newStoreFixtureWith(t, "kek-primary", 30*time.Minute)
	id := testCredentialID(12)
	ctx := context.Background()
	if _, _, err := fixture.store.Write(ctx, id, "v1-token"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := fixture.store.Rotate(ctx, id, "v2-token"); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	fixture.clock.Advance(30*time.Minute + time.Second)
	if _, err := fixture.store.Rotate(ctx, id, "v3-token"); err != nil {
		t.Fatalf("second Rotate: %v", err)
	}
	stored := readRecord(t, fixture.store, id)
	if len(stored.Previous) != 1 || stored.Previous[0].Version != 2 {
		t.Fatalf("retained versions = %+v, want only version 2 after the window closed", stored.Previous)
	}
	for _, entry := range stored.Previous {
		if entry.Version == 1 {
			t.Fatal("expired version 1 is still retained")
		}
	}
	if got := openTokenOn(t, fixture.store, id, &stored.Current); got != "v3-token" {
		t.Fatalf("current version opened as %q, want v3-token", got)
	}
}

// TestRevokeIsTerminalTombstone proves revocation clears the material and leaves a tombstone that
// refuses every later write or rotation, so a replayed create cannot resurrect the credential (D4).
func TestRevokeIsTerminalTombstone(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(13)
	ctx := context.Background()
	if _, _, err := fixture.store.Write(ctx, id, "token"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := fixture.store.Revoke(ctx, id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := fixture.store.Revoke(ctx, id); err != nil {
		t.Fatalf("repeated Revoke = %v, want idempotent success", err)
	}
	stored := readRecord(t, fixture.store, id)
	if !stored.revoked() {
		t.Fatal("record is not a tombstone after Revoke")
	}
	if stored.Current != (material{}) {
		t.Fatalf("revoked record kept material %+v", stored.Current)
	}
	if _, err := fixture.store.Rotate(ctx, id, "next-token"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Rotate after Revoke error = %v, want ErrRevoked", err)
	}
	if _, _, err := fixture.store.Write(ctx, id, "token"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("replayed Write after Revoke error = %v, want ErrRevoked", err)
	}
	if _, _, err := fixture.store.Write(ctx, id, "different-token"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Write with new material after Revoke error = %v, want ErrRevoked", err)
	}
}

// TestRevokedIsDistinguishableFromNeverExisted proves a tombstone keeps "revoked" distinguishable
// from "never existed", which D7 requires the wire reason codes to tell apart (D4, D7).
func TestRevokedIsDistinguishableFromNeverExisted(t *testing.T) {
	fixture := newStoreFixture(t)
	ctx := context.Background()
	existing := testCredentialID(14)
	if _, _, err := fixture.store.Write(ctx, existing, "token"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := fixture.store.Revoke(ctx, existing); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := fixture.store.Rotate(ctx, existing, "next"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Rotate(revoked) error = %v, want ErrRevoked", err)
	}

	absent := testCredentialID(15)
	if err := fixture.store.Revoke(ctx, absent); err != nil {
		t.Fatalf("Revoke(never existed) = %v, want idempotent success", err)
	}
	if _, err := fixture.store.Rotate(ctx, absent, "next"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rotate(never existed) error = %v, want ErrNotFound, distinct from ErrRevoked", err)
	}
}
