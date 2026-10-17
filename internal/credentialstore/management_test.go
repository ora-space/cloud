package credentialstore

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagementMetadata(t *testing.T) {
	h := newHarness(t)
	client := h.managementClient(t, h.managerCert)
	got, err := client.Get(context.Background(), testCredentialID)
	if want := (Metadata{CredentialID: testCredentialID}); err != nil || got != want {
		t.Fatalf("Get unknown = %+v, %v; want %+v", got, err, want)
	}
	h.storeVersion(t, 1)
	second := h.storeVersion(t, 2)
	got, err = client.Get(context.Background(), testCredentialID)
	if want := (Metadata{CredentialID: testCredentialID, CurrentVersion: 2, SubmissionID: second.SubmissionID}); err != nil || got != want {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, want)
	}
	// The wire shape is exactly the four metadata fields: nothing secret, nothing sealed.
	status, out := h.get(t, h.management.URL+"/v1/credentials/"+testCredentialID, h.managerCert)
	if status != http.StatusOK || len(out) != 4 || out["currentVersion"] != "2" || out["revoked"] != "false" {
		t.Fatalf("GET = %d %v, want the four metadata fields", status, out)
	}
	if status, out := h.get(t, h.management.URL+"/v1/credentials/not-a-uuid", h.managerCert); status != http.StatusBadRequest {
		t.Fatalf("GET invalid id = %d %v, want 400", status, out)
	}
}

func TestManagementRevoke(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	h.storeVersion(t, 2)
	client := h.managementClient(t, h.managerCert)
	for i := range 2 {
		if err := client.Revoke(context.Background(), testCredentialID); err != nil {
			t.Fatalf("Revoke #%d: %v", i+1, err)
		}
	}
	got, err := client.Get(context.Background(), testCredentialID)
	if err != nil || !got.Revoked || got.CurrentVersion != 2 {
		t.Fatalf("Get after revoke = %+v, %v; want revoked with the last version reported", got, err)
	}
	stored, found, err := h.store.volume.readRecord(testCredentialID)
	if err != nil || !found || stored.RevokedAt == nil || len(stored.Versions) != 0 {
		t.Fatalf("record after revoke = %+v, %v; want a tombstone without versions", stored, err)
	}
	entries, err := os.ReadDir(filepath.Join(h.root, credentialsDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(h.root, credentialsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "ciphertext") || strings.Contains(string(raw), "wrappedDek") {
			t.Fatalf("%s still holds sealed material after revoke", entry.Name())
		}
	}
	// Revoking an id that never existed leaves a tombstone, so a late submission cannot create it.
	const unknown = "00000000-0000-4000-8000-000000000003"
	if err := client.Revoke(context.Background(), unknown); err != nil {
		t.Fatalf("Revoke unknown: %v", err)
	}
	if got, err := client.Get(context.Background(), unknown); err != nil || !got.Revoked {
		t.Fatalf("Get unknown after revoke = %+v, %v; want revoked", got, err)
	}
}

func TestManagementIdentityBoundary(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	for name, cert := range map[string]struct {
		status int
		code   string
		client *ManagementClient
	}{
		"executor":     {403, CodeIdentityForbidden, h.managementClient(t, h.executorCert)},
		"kek operator": {403, CodeIdentityForbidden, h.managementClient(t, h.kekCert)},
		"no URI SAN":   {401, CodeIdentityRequired, h.managementClient(t, h.pki.client(t))},
	} {
		_, err := cert.client.Get(context.Background(), testCredentialID)
		var refusal *RefusalError
		if !errors.Is(err, ErrIdentityRefused) || !errors.As(err, &refusal) || refusal.Status != cert.status || refusal.Code != cert.code {
			t.Fatalf("%s Get = %v, want %d %s", name, err, cert.status, cert.code)
		}
		if err := cert.client.Revoke(context.Background(), testCredentialID); !errors.Is(err, ErrIdentityRefused) {
			t.Fatalf("%s Revoke = %v, want ErrIdentityRefused", name, err)
		}
	}
	// A connection without any client certificate never completes the handshake.
	if response, err := h.httpClient(t).Get(h.management.URL + "/v1/credentials/" + testCredentialID); err == nil {
		response.Body.Close()
		t.Fatal("management listener accepted a connection without a client certificate")
	}
	// The manager cannot redeem; covered with the full matrix in TestRedemptionRefusals.
	if _, err := h.redemptionClient(t, h.managerCert).Redeem(context.Background(), h.signAuthorization(t, h.authorization())); !errors.Is(err, ErrIdentityRefused) {
		t.Fatalf("manager Redeem = %v, want ErrIdentityRefused", err)
	}
	if got, err := h.managementClient(t, h.managerCert).Get(context.Background(), testCredentialID); err != nil || got.Revoked {
		t.Fatalf("refused callers changed state: %+v, %v", got, err)
	}
	if status, out := h.post(t, h.management.URL+"/v1/credentials/"+testCredentialID+"/revoke", h.managerCert, `{"token":"x"}`); status != http.StatusBadRequest {
		t.Fatalf("revoke with a body = %d %v, want 400", status, out)
	}
	if status, out := h.post(t, h.management.URL+"/v1/credentials/"+testCredentialID+"/versions", h.managerCert, `{"token":"x"}`); status != http.StatusNotFound {
		t.Fatalf("management write route = %d %v, want 404: the manager cannot write plaintext", status, out)
	}
}

func TestClientConfigValidation(t *testing.T) {
	h := newHarness(t)
	good := h.pki.clientTLS(h.managerCert)
	cases := map[string]ClientConfig{
		"plain http":     {BaseURL: "http://127.0.0.1:1", TLS: good},
		"userinfo":       {BaseURL: "https://u:p@127.0.0.1:1", TLS: good},
		"query":          {BaseURL: "https://127.0.0.1:1?x=1", TLS: good},
		"no tls":         {BaseURL: "https://127.0.0.1:1"},
		"no certificate": {BaseURL: "https://127.0.0.1:1", TLS: h.pki.clientTLS()},
	}
	for name, cfg := range cases {
		if _, err := NewManagementClient(&cfg); err == nil {
			t.Fatalf("%s: NewManagementClient accepted an invalid configuration", name)
		}
	}
	unreachable, err := NewRedemptionClient(&ClientConfig{BaseURL: "https://127.0.0.1:1", TLS: good})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unreachable.Redeem(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Redeem against a closed port = %v, want ErrUnavailable", err)
	}
}
