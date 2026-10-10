package credentialstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRequiresVolumeAndKeyFile proves a Store never comes up with missing configuration, because
// either default would turn a misconfigured deployment into a silently empty store (D6, D8).
func TestOpenRequiresVolumeAndKeyFile(t *testing.T) {
	t.Run("blank volume", func(t *testing.T) {
		keys := map[string][]byte{"primary": keyMaterial(t)}
		keyFile := writeKeyringFile(t, filepath.Join(t.TempDir(), "kek.json"), "primary", keys)
		store, err := Open(Config{Volume: "   ", KeyFile: keyFile})
		if err == nil {
			t.Fatal("Open accepted a blank volume")
		}
		if store != nil {
			t.Fatal("Open returned a store alongside the error")
		}
	})
	t.Run("blank key file", func(t *testing.T) {
		volume := filepath.Join(t.TempDir(), "volume")
		store, err := Open(Config{Volume: volume, KeyFile: ""})
		if !errors.Is(err, ErrKeyMissing) {
			t.Fatalf("Open error = %v, want errors.Is(err, ErrKeyMissing)", err)
		}
		if store != nil {
			t.Fatal("Open returned a store alongside the error")
		}
	})
}

// TestOpenRefusesMissingKeyFileAsErrKeyMissing proves "no KEK" is an identifiable startup refusal,
// never an empty store that would report "cannot decrypt" as "never existed" (D8).
func TestOpenRefusesMissingKeyFileAsErrKeyMissing(t *testing.T) {
	base := t.TempDir()
	volume := filepath.Join(base, "volume")
	store, err := Open(Config{Volume: volume, KeyFile: filepath.Join(base, "absent-kek.json")})
	if !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrKeyMissing)", err)
	}
	if store != nil {
		t.Fatal("Open returned a store alongside the error")
	}
	assertNoRecordFiles(t, volume)
}

// TestLoadKeyringRefusesMalformedConfigurations proves every way a deployment can get the keyring
// wrong is one refusal reported as ErrKeyMissing, so a broken keyring can never present as a usable
// empty store (D8).
func TestLoadKeyringRefusesMalformedConfigurations(t *testing.T) {
	goodKey := base64.StdEncoding.EncodeToString(keyMaterial(t))
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, keyBytes-1))
	cases := []struct {
		name     string
		contents string
	}{
		{"not json", "this is not a keyring"},
		{"no keys", `{"primary":"a","keys":[]}`},
		{"primary absent", `{"primary":"b","keys":[{"id":"a","key":"` + goodKey + `"}]}`},
		{"empty id", `{"primary":"","keys":[{"id":"  ","key":"` + goodKey + `"}]}`},
		{"bad base64", `{"primary":"a","keys":[{"id":"a","key":"!!!"}]}`},
		{"short key", `{"primary":"a","keys":[{"id":"a","key":"` + shortKey + `"}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kek.json")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write keyring: %v", err)
			}
			if _, err := LoadKeyring(path); !errors.Is(err, ErrKeyMissing) {
				t.Fatalf("LoadKeyring(%s) error = %v, want errors.Is(err, ErrKeyMissing)", test.name, err)
			}
		})
	}
	t.Run("empty path", func(t *testing.T) {
		if _, err := LoadKeyring("  "); !errors.Is(err, ErrKeyMissing) {
			t.Fatalf("LoadKeyring(blank) error = %v, want ErrKeyMissing", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadKeyring(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, ErrKeyMissing) {
			t.Fatalf("LoadKeyring(missing) error = %v, want ErrKeyMissing", err)
		}
	})
}

// TestOpenRefusesNonPrivateVolume proves the startup gate refuses a volume other local accounts can
// read, since that volume already hands an attacker the ciphertext and the wrapping metadata (D2).
func TestOpenRefusesNonPrivateVolume(t *testing.T) {
	keys := map[string][]byte{"primary": keyMaterial(t)}
	keyFile := writeKeyringFile(t, filepath.Join(t.TempDir(), "kek.json"), "primary", keys)
	cases := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"world readable", 0o755, true},
		{"group readable", 0o750, true},
		{"group writable", 0o070, true},
		{"private", 0o700, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			volume := filepath.Join(t.TempDir(), "volume")
			if err := os.Mkdir(volume, 0o700); err != nil {
				t.Fatalf("create volume: %v", err)
			}
			if err := os.Chmod(volume, test.mode); err != nil {
				t.Fatalf("chmod volume: %v", err)
			}
			_, err := Open(Config{Volume: volume, KeyFile: keyFile})
			if test.wantErr && err == nil {
				t.Fatalf("Open accepted volume mode %#o", test.mode)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Open rejected a private volume mode %#o: %v", test.mode, err)
			}
		})
	}
}

// TestVolumeExportNeverYieldsToken proves exporting the Store's volume alone cannot yield the token:
// it holds only ciphertext and wrapping metadata, not the plaintext or any encoding of it (D2, D8).
func TestVolumeExportNeverYieldsToken(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(1)
	const token = "ghp_VeryDistinctiveTokenValue_1234567890"
	if _, _, err := fixture.store.Write(context.Background(), id, token); err != nil {
		t.Fatalf("Write: %v", err)
	}
	exported := readVolumeBytes(t, fixture.volume)
	if len(exported) == 0 {
		t.Fatal("volume export is empty; the assertion would be vacuous")
	}
	if bytes.Contains(exported, []byte(token)) {
		t.Fatal("raw token appears in the volume export")
	}
	for _, encoded := range tokenEncodings(token) {
		if bytes.Contains(exported, []byte(encoded)) {
			t.Fatalf("encoded token %q appears in the volume export", encoded)
		}
	}
	for _, entry := range fixture.keys {
		encoded := base64.StdEncoding.EncodeToString(entry)
		if bytes.Contains(exported, []byte(encoded)) {
			t.Fatal("key-encryption key material appears in the volume export")
		}
		if bytes.Contains(exported, entry) {
			t.Fatal("raw key-encryption key material appears in the volume export")
		}
	}
}

// TestKeyIdentifierIsPersistedAndRotationKeepsOldKeyUsable proves the wrapping names the KEK that
// produced it, so a keyring that still holds the old key opens existing material while a keyring
// holding only an unrelated key cannot (D2, D8).
func TestKeyIdentifierIsPersistedAndRotationKeepsOldKeyUsable(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(2)
	const token = "ghp_original_material"
	if _, _, err := fixture.store.Write(context.Background(), id, token); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stored := readRecord(t, fixture.store, id)
	if stored.Current.KeyID != fixture.primary {
		t.Fatalf("persisted wrapping key id = %q, want the primary %q", stored.Current.KeyID, fixture.primary)
	}
	if fixture.store.KeyID() != fixture.primary {
		t.Fatalf("Store.KeyID() = %q, want %q", fixture.store.KeyID(), fixture.primary)
	}

	rotatedKeys := map[string][]byte{
		"kek-next":      keyMaterial(t),
		fixture.primary: fixture.keys[fixture.primary],
	}
	rotatedFile := writeKeyringFile(t, filepath.Join(t.TempDir(), "rotated.json"), "kek-next", rotatedKeys)
	rotated := openStoreForTest(t, fixture.volume, rotatedFile, DefaultRetention, nil)
	if rotated.KeyID() != "kek-next" {
		t.Fatalf("rotated Store.KeyID() = %q, want kek-next", rotated.KeyID())
	}
	if got := openTokenOn(t, rotated, id, &stored.Current); got != token {
		t.Fatalf("material written under the old KEK opened as %q after rotation, want %q", got, token)
	}

	unrelatedKeys := map[string][]byte{"kek-unrelated": keyMaterial(t)}
	unrelatedFile := writeKeyringFile(t, filepath.Join(t.TempDir(), "unrelated.json"), "kek-unrelated", unrelatedKeys)
	unrelated := openStoreForTest(t, fixture.volume, unrelatedFile, DefaultRetention, nil)
	if _, err := unrelated.keys.openToken(id, &stored.Current); err == nil {
		t.Fatal("material decrypted under a key-encryption key that did not wrap it")
	}
}

// TestOpeningVolumeWithForeignKeyringFailsClosed proves opening the same volume with a different
// keyring fails instead of returning plaintext, so a leaked volume is not decrementable without the
// KEK that produced its wrappings (D2, D8).
func TestOpeningVolumeWithForeignKeyringFailsClosed(t *testing.T) {
	fixture := newStoreFixture(t)
	id := testCredentialID(3)
	const token = "ghp_foreign_keyring_must_fail"
	if _, _, err := fixture.store.Write(context.Background(), id, token); err != nil {
		t.Fatalf("Write: %v", err)
	}
	foreignKeys := map[string][]byte{"kek-foreign": keyMaterial(t)}
	foreignFile := writeKeyringFile(t, filepath.Join(t.TempDir(), "foreign.json"), "kek-foreign", foreignKeys)
	foreign := openStoreForTest(t, fixture.volume, foreignFile, DefaultRetention, nil)
	stored := readRecord(t, foreign, id)
	opened, err := foreign.keys.openToken(id, &stored.Current)
	if err == nil {
		t.Fatalf("a foreign keyring decrypted the volume and returned %q", opened)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("the decryption failure leaked the token")
	}
}
