package credentialstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

func TestRedemptionHappyPathPerPurpose(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	authorizations := map[string]cloneauth.Authorization{
		"publish": h.authorization(),
		"sync": h.authorization(func(a *cloneauth.Authorization) {
			a.Purpose = cloneauth.PurposeSync
			a.SourceRef, a.SourceCommit, a.TargetRef, a.ExpectedOldOID = "", "", "", ""
			a.Branch, a.ExpectedTrackingOID = "main", "0000000000000000000000000000000000000000"
		}),
		"probe": h.authorization(func(a *cloneauth.Authorization) {
			a.Purpose = cloneauth.PurposeProbe
			a.OperationID, a.ProjectID, a.BindingID = "", "", ""
			a.SourceRef, a.SourceCommit, a.TargetRef, a.ExpectedOldOID = "", "", "", ""
			a.VerificationID, a.Service = "2d8b9c20-3f4e-4e5d-8c1a-7f6e5d4c3b22", cloneauth.ServiceReceivePack
		}),
	}
	client := h.redemptionClient(t, h.executorCert)
	for name, authorization := range authorizations {
		got, err := client.Redeem(context.Background(), h.signAuthorization(t, authorization))
		want := Credential{Username: DefaultUsername, Token: canaryToken, Version: 1}
		if err != nil || got != want {
			t.Fatalf("%s: Redeem = %v, %v; want %v", name, got, err, want)
		}
		if _, err := os.Stat(filepath.Join(h.root, redemptionsDir, authorization.AuthorizationID)); err != nil {
			t.Fatalf("%s: redemption record missing: %v", name, err)
		}
	}
}

func TestRedemptionRefusals(t *testing.T) {
	cases := []struct {
		name string
		// prepare returns the client to use and the authorization token to redeem.
		prepare    func(t *testing.T, h *harness) (*RedemptionClient, string)
		wantErr    error
		wantStatus int
		wantCode   string
	}{
		{"second redemption", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			client := h.redemptionClient(t, h.executorCert)
			token := h.signAuthorization(t, h.authorization())
			if _, err := client.Redeem(context.Background(), token); err != nil {
				t.Fatalf("first Redeem: %v", err)
			}
			return client, token
		}, ErrAlreadyRedeemed, 409, CodeAlreadyRedeemed},
		{"submission grant used for redemption", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			return h.redemptionClient(t, h.executorCert), h.signSubmission(t, h.submission())
		}, ErrInvalidAuthorization, 400, CodeInvalidAuthorization},
		{"authorization signed with the submission key", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			token, err := cloneauth.Sign(h.submissionKey, h.authorization())
			if err != nil {
				t.Fatal(err)
			}
			return h.redemptionClient(t, h.executorCert), token
		}, ErrInvalidAuthorization, 400, CodeInvalidAuthorization},
		{"expired authorization", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			token := h.signAuthorization(t, h.authorization())
			h.clock.Advance(2 * time.Minute)
			return h.redemptionClient(t, h.executorCert), token
		}, ErrInvalidAuthorization, 400, CodeInvalidAuthorization},
		{"anonymous sync authorization", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			a := h.authorization(func(a *cloneauth.Authorization) {
				a.Purpose = cloneauth.PurposeSync
				a.SourceRef, a.SourceCommit, a.TargetRef, a.ExpectedOldOID = "", "", "", ""
				a.Branch, a.ExpectedTrackingOID = "main", "0000000000000000000000000000000000000000"
				a.CredentialID, a.CredentialVersion = "", 0
			})
			return h.redemptionClient(t, h.executorCert), h.signAuthorization(t, a)
		}, ErrInvalidAuthorization, 400, CodeInvalidAuthorization},
		{"wrong client identity (cloud credential manager)", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			return h.redemptionClient(t, h.managerCert), h.signAuthorization(t, h.authorization())
		}, ErrIdentityRefused, 403, CodeIdentityForbidden},
		{"wrong client identity (kek operator)", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			return h.redemptionClient(t, h.kekCert), h.signAuthorization(t, h.authorization())
		}, ErrIdentityRefused, 403, CodeIdentityForbidden},
		{"certificate with two URI SANs", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			cert := h.pki.client(t, ExecutorIdentity, CredentialManagerIdentity)
			return h.redemptionClient(t, cert), h.signAuthorization(t, h.authorization())
		}, ErrIdentityRefused, 401, CodeIdentityRequired},
		{"certificate without URI SAN", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			return h.redemptionClient(t, h.pki.client(t)), h.signAuthorization(t, h.authorization())
		}, ErrIdentityRefused, 401, CodeIdentityRequired},
		{"unknown credential", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			return h.redemptionClient(t, h.executorCert), h.signAuthorization(t, h.authorization())
		}, ErrCredentialNotFound, 404, CodeCredentialNotFound},
		{"unknown version", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			a := h.authorization(func(a *cloneauth.Authorization) { a.CredentialVersion = 2 })
			return h.redemptionClient(t, h.executorCert), h.signAuthorization(t, a)
		}, ErrCredentialUnavailable, 410, CodeCredentialUnavailable},
		{"revoked credential", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			token := h.signAuthorization(t, h.authorization())
			if err := h.managementClient(t, h.managerCert).Revoke(context.Background(), testCredentialID); err != nil {
				t.Fatal(err)
			}
			return h.redemptionClient(t, h.executorCert), token
		}, ErrCredentialUnavailable, 410, CodeCredentialUnavailable},
		{"superseded version after one hour", func(t *testing.T, h *harness) (*RedemptionClient, string) {
			h.storeVersion(t, 1)
			h.storeVersion(t, 2)
			h.clock.Advance(SupersededRetention)
			return h.redemptionClient(t, h.executorCert), h.signAuthorization(t, h.authorization())
		}, ErrCredentialUnavailable, 410, CodeCredentialUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			client, token := tc.prepare(t, h)
			got, err := client.Redeem(context.Background(), token)
			var refusal *RefusalError
			if !errors.Is(err, tc.wantErr) || !errors.As(err, &refusal) || refusal.Status != tc.wantStatus || refusal.Code != tc.wantCode {
				t.Fatalf("Redeem = %v, %v; want %v (%d %s)", got, err, tc.wantErr, tc.wantStatus, tc.wantCode)
			}
			if got != (Credential{}) {
				t.Fatal("a refused redemption returned credential material")
			}
		})
	}
}

// TestRedemptionExecutorIdentityMismatch: the listener's trusted identity and the identity the
// authorization names must be the same; a peer the listener trusts still cannot redeem an
// authorization issued to another executor.
func TestRedemptionExecutorIdentityMismatch(t *testing.T) {
	identities := DefaultIdentities()
	identities.Redeemer = "spiffe://ora.local/repo-executor-canary"
	h := newHarness(t, withIdentities(identities))
	h.storeVersion(t, 1)
	authorization := h.authorization()
	_, err := h.redemptionClient(t, h.executorCert).Redeem(context.Background(), h.signAuthorization(t, authorization))
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Status != 403 || refusal.Code != CodeExecutorMismatch {
		t.Fatalf("Redeem = %v, want 403 %s", err, CodeExecutorMismatch)
	}
	if _, err := os.Stat(filepath.Join(h.root, redemptionsDir, authorization.AuthorizationID)); !os.IsNotExist(err) {
		t.Fatalf("a refused authorization was spent: %v", err)
	}
}

func TestRedemptionSupersededVersionWithinRetention(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	h.storeVersion(t, 2)
	h.clock.Advance(SupersededRetention - time.Second)
	assertRedeems(t, h, 1)
	assertRedeems(t, h, 2)

	// The sweep destroys version 1 once its window closes; version 2 stays.
	h.clock.Advance(time.Second)
	result, err := h.store.Sweep(context.Background())
	if err != nil || result.Versions != 1 {
		t.Fatalf("Sweep = %+v, %v; want 1 version destroyed", result, err)
	}
	stored, _, err := h.store.volume.readRecord(testCredentialID)
	if err != nil || len(stored.Versions) != 1 || stored.Versions[0].Version != 2 {
		t.Fatalf("after sweep record versions = %+v, %v; want only version 2", stored, err)
	}
	assertRedeems(t, h, 2)
}

func TestSweepRemovesExpiredClaims(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	assertRedeems(t, h, 1)
	if result, err := h.store.Sweep(context.Background()); err != nil || result.Claims != 0 {
		t.Fatalf("early Sweep = %+v, %v; want fresh claims kept", result, err)
	}
	h.clock.Advance(claimRetention + time.Second)
	if result, err := h.store.Sweep(context.Background()); err != nil || result.Claims != 2 {
		t.Fatalf("Sweep = %+v, %v; want the submission and redemption claims removed", result, err)
	}
}

// TestRedemptionRecordPrecedesDecryption simulates a decryption failure: the redemption claim must
// already exist, so the authorization cannot be redeemed again once the fault clears.
func TestRedemptionRecordPrecedesDecryption(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	path := filepath.Join(h.root, credentialsDir, testCredentialID+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored record
	if err := json.Unmarshal(original, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Versions[0].Sealed.Ciphertext = encode([]byte("not the sealed secret at all"))
	corrupt, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	authorization := h.authorization()
	token := h.signAuthorization(t, authorization)
	client := h.redemptionClient(t, h.executorCert)
	if _, err := client.Redeem(context.Background(), token); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Redeem with corrupt ciphertext = %v, want ErrUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, redemptionsDir, authorization.AuthorizationID)); err != nil {
		t.Fatalf("redemption record must exist after a decryption failure: %v", err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Redeem(context.Background(), token); !errors.Is(err, ErrAlreadyRedeemed) {
		t.Fatalf("Redeem after the fault cleared = %v, want ErrAlreadyRedeemed", err)
	}
}

// TestConcurrentRedemptionDeliversOnce races several redemptions of one authorization; exactly one
// may succeed and every other must be already_redeemed.
func TestConcurrentRedemptionDeliversOnce(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	token := h.signAuthorization(t, h.authorization())
	client := h.redemptionClient(t, h.executorCert)
	const racers = 8
	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			<-start
			_, errs[i] = client.Redeem(context.Background(), token)
		})
	}
	close(start)
	wg.Wait()
	delivered := 0
	for _, err := range errs {
		switch {
		case err == nil:
			delivered++
		case errors.Is(err, ErrAlreadyRedeemed):
		default:
			t.Fatalf("unexpected outcome %v", err)
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered %d times, want exactly once", delivered)
	}
}

func TestRedemptionRequestShape(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	token := h.signAuthorization(t, h.authorization())
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown field":  {`{"authorization":"` + token + `","credentialId":"x"}`, 400, CodeInvalidRequest},
		"trailing value": {`{"authorization":"` + token + `"} {}`, 400, CodeInvalidRequest},
		"not JSON":       {`authorization=` + token, 400, CodeInvalidRequest},
		"oversize":       {`{"authorization":"` + strings.Repeat("a", maxRequestBody) + `"}`, 413, CodeRequestTooLarge},
	}
	for name, tc := range cases {
		status, out := h.post(t, h.redemption.URL+"/v1/redemptions", h.executorCert, tc.body)
		if status != tc.status || out["code"] != tc.code {
			t.Fatalf("%s: redeem = %d %v, want %d %s", name, status, out, tc.status, tc.code)
		}
	}
	// None of the malformed requests spent the authorization.
	assertRedeemsToken(t, h, token)
	if status, out := h.get(t, h.redemption.URL+"/v1/credentials/"+testCredentialID, h.executorCert); status != http.StatusNotFound {
		t.Fatalf("management route on the redemption listener = %d %v, want 404", status, out)
	}
}

func assertRedeemsToken(t *testing.T, h *harness, token string) {
	t.Helper()
	if _, err := h.redemptionClient(t, h.executorCert).Redeem(context.Background(), token); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
}

// TestPlaintextNeverLogged drives every path that touches the secret, including failures, and
// scans every log entry for the token, the username and the bearer grants.
func TestPlaintextNeverLogged(t *testing.T) {
	h := newHarness(t)
	grantToken := h.signSubmission(t, h.submission())
	if status, out, _ := h.put(t, testCredentialID, grantToken, testOrigin, `{"token":"`+canaryToken+`","username":"canary-user"}`); status != 200 {
		t.Fatalf("submit = %d %v", status, out)
	}
	h.put(t, testCredentialID, grantToken, testOrigin, `{"token":"`+canaryToken+`"}`)     // replay
	h.put(t, testCredentialID, grantToken, testOrigin, `{"token":"`+canaryToken+"\\n\"}") // bad token
	authorization := h.signAuthorization(t, h.authorization())
	client := h.redemptionClient(t, h.executorCert)
	got, err := client.Redeem(context.Background(), authorization)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Redeem(context.Background(), authorization)
	_, _ = h.redemptionClient(t, h.managerCert).Redeem(context.Background(), authorization)
	if err := h.managementClient(t, h.managerCert).Revoke(context.Background(), testCredentialID); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Redeem(context.Background(), h.signAuthorization(t, h.authorization()))

	for _, value := range []any{got, &got, Secret{Username: "canary-user", Token: canaryToken}} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, canaryToken) {
				t.Fatalf("%s of %T prints the token", format, value)
			}
		}
	}
	entries := h.logs.All()
	if len(entries) == 0 {
		t.Fatal("no audit lines were written")
	}
	forbidden := []string{canaryToken, "canary-user", grantToken, authorization}
	for _, entry := range entries {
		rendered := entry.Message + fmt.Sprintf(" %v", entry.ContextMap())
		for _, secret := range forbidden {
			if strings.Contains(rendered, secret) {
				t.Fatalf("log entry %q leaks a secret", entry.Message)
			}
		}
	}
}
