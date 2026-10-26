package credentialstore

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

func TestSealOpenRoundTrip(t *testing.T) {
	ring := mustKeyring(t, "kek-1", map[string][]byte{"kek-1": randomKey(t)})
	secret := Secret{Username: "x-access-token", Token: canaryToken}
	box, err := ring.seal(testCredentialID, 3, secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if box.KEKID != "kek-1" {
		t.Fatalf("sealed under %q, want the primary kek-1", box.KEKID)
	}
	for _, field := range []string{box.Ciphertext, box.WrappedDEK} {
		if strings.Contains(field, canaryToken) {
			t.Fatal("sealed material contains the plaintext token")
		}
	}
	got, err := ring.open(testCredentialID, 3, &box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != secret {
		t.Fatalf("open = %#v, want the sealed secret", got.Username)
	}
}

func TestOpenRejectsWrongBinding(t *testing.T) {
	ring := mustKeyring(t, "kek-1", map[string][]byte{"kek-1": randomKey(t)})
	other := mustKeyring(t, "kek-1", map[string][]byte{"kek-1": randomKey(t)})
	box, err := ring.seal(testCredentialID, 3, Secret{Username: "u", Token: canaryToken})
	if err != nil {
		t.Fatal(err)
	}
	tamper := func(mutate func(*sealed)) sealed {
		copied := box
		mutate(&copied)
		return copied
	}
	cases := []struct {
		name         string
		ring         *Keyring
		credentialID string
		version      int64
		box          sealed
	}{
		{"other credential id (AAD)", ring, "00000000-0000-4000-8000-000000000000", 3, box},
		{"other version (AAD)", ring, testCredentialID, 4, box},
		{
			"relabeled KEK id (wrap AAD)", mustKeyring(t, "kek-1", map[string][]byte{"kek-1": ring.keys["kek-1"], "kek-2": ring.keys["kek-1"]}),
			testCredentialID, 3, tamper(func(s *sealed) { s.KEKID = "kek-2" }),
		},
		{"unknown KEK", ring, testCredentialID, 3, tamper(func(s *sealed) { s.KEKID = "kek-9" })},
		{"other KEK material", other, testCredentialID, 3, box},
		{"tampered ciphertext", ring, testCredentialID, 3, tamper(func(s *sealed) { s.Ciphertext = encode([]byte("garbage-garbage-garbage")) })},
		{"malformed base64", ring, testCredentialID, 3, tamper(func(s *sealed) { s.Nonce = "!!" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.ring.open(tc.credentialID, tc.version, &tc.box); err == nil {
				t.Fatal("open succeeded; the envelope must not open outside its binding")
			}
		})
	}
}

func TestParseKeyringRefusesInvalidDocuments(t *testing.T) {
	good := encode(make([]byte, keyBytes))
	cases := map[string]string{
		"empty":             ``,
		"no keys":           `{"primary":"a","keys":{}}`,
		"primary missing":   `{"primary":"b","keys":{"a":"` + good + `"}}`,
		"short key":         `{"primary":"a","keys":{"a":"` + encode(make([]byte, 16)) + `"}}`,
		"not base64":        `{"primary":"a","keys":{"a":"***"}}`,
		"bad key id":        `{"primary":"a b","keys":{"a b":"` + good + `"}}`,
		"unknown field":     `{"primary":"a","keys":{"a":"` + good + `"},"extra":1}`,
		"trailing document": `{"primary":"a","keys":{"a":"` + good + `"}} {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKeyring([]byte(raw)); !errors.Is(err, ErrKeyringUnavailable) {
				t.Fatalf("ParseKeyring = %v, want ErrKeyringUnavailable", err)
			}
		})
	}
}

func TestLoadKeyringRefusesMissingFile(t *testing.T) {
	for name, path := range map[string]string{"unset": "", "missing": filepath.Join(t.TempDir(), "absent.json")} {
		if _, err := LoadKeyring(path); !errors.Is(err, ErrKeyringUnavailable) {
			t.Fatalf("%s: LoadKeyring = %v, want ErrKeyringUnavailable", name, err)
		}
	}
	if _, err := OpenStore(StoreConfig{Root: filepath.Join(t.TempDir(), "store"), Now: testStart.UTC}); !errors.Is(err, ErrKeyringUnavailable) {
		t.Fatalf("OpenStore without keyring = %v, want ErrKeyringUnavailable", err)
	}
}

func TestOpenStoreRefusesSharedRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ring := mustKeyring(t, "k", map[string][]byte{"k": randomKey(t)})
	if _, err := OpenStore(StoreConfig{Root: root, Keyring: ring, Now: testStart.UTC}); err == nil {
		t.Fatal("OpenStore accepted a root readable by other accounts")
	}
}

// TestKeyringRotationAndRewrap: a version sealed under kek-1 stays readable after kek-2 becomes
// primary, rewrap moves it to kek-2, and afterwards kek-1 can be dropped from the ring.
func TestKeyringRotationAndRewrap(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	root := filepath.Join(t.TempDir(), "store")
	before := newHarness(t, withRoot(root), withKeyring(mustKeyring(t, "kek-1", map[string][]byte{"kek-1": k1})))
	before.storeVersion(t, 1)

	rotated := newHarness(t, withRoot(root), withSigners(before.executionKey, before.submissionKey),
		withKeyring(mustKeyring(t, "kek-2", map[string][]byte{"kek-1": k1, "kek-2": k2})))
	assertRedeems(t, rotated, 1)

	// The manager and the executor cannot rewrap; only the KEK operator can.
	for name, cert := range map[string]tls.Certificate{"manager": rotated.managerCert, "executor": rotated.executorCert} {
		status, out := rotated.post(t, rotated.key.URL+"/v1/keys/rewrap", cert, "")
		if status != 403 || out["code"] != CodeIdentityForbidden {
			t.Fatalf("%s rewrap = %d %v, want 403 %s", name, status, out, CodeIdentityForbidden)
		}
	}
	status, out := rotated.post(t, rotated.key.URL+"/v1/keys/rewrap", rotated.kekCert, "")
	if status != 200 || out["rewrapped"] != "1" {
		t.Fatalf("rewrap = %d %v, want 200 rewrapped 1", status, out)
	}
	status, out = rotated.post(t, rotated.key.URL+"/v1/keys/rewrap", rotated.kekCert, "{}")
	if status != 200 || out["rewrapped"] != "0" {
		t.Fatalf("second rewrap = %d %v, want 200 rewrapped 0", status, out)
	}

	after := newHarness(t, withRoot(root), withSigners(before.executionKey, before.submissionKey),
		withKeyring(mustKeyring(t, "kek-2", map[string][]byte{"kek-2": k2})))
	assertRedeems(t, after, 1)
}

func assertRedeems(t *testing.T, h *harness, version int64) {
	t.Helper()
	token := h.signAuthorization(t, h.authorization(func(a *cloneauth.Authorization) { a.CredentialVersion = version }))
	got, err := h.redemptionClient(t, h.executorCert).Redeem(context.Background(), token)
	if err != nil || got.Token != canaryToken || got.Version != version {
		t.Fatalf("Redeem version %d = %v, %v", version, got, err)
	}
}
