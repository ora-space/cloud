package config

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

// writeSigningKey writes one PKCS#8 Ed25519 private key, the only shape cloneauth loads.
func writeSigningKey(t *testing.T, dir, name string) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling the signing key: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// writeTLSMaterial writes one self-signed CA and a client pair signed by it. Nothing here is
// verified against a server: Open performs no network calls.
func writeTLSMaterial(t *testing.T, dir string) (certFile, keyFile, caFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a TLS key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "repository-config-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling the TLS key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	write := func(name string, contents []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return path
	}
	return write("client.pem", certPEM), write("client.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), write("ca.pem", certPEM)
}

// repositoryFixtures is a configuration that opens: every file exists and every address is an
// https origin.
func repositoryFixtures(t *testing.T) *RepositoryConfig {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile, caFile := writeTLSMaterial(t, dir)
	return &RepositoryConfig{
		Enabled:     true,
		ExecutorURL: "https://repo-executor:8443", ExecutorCAFile: caFile, DispatcherCertFile: certFile, DispatcherKeyFile: keyFile,
		AuthorizationKeyFile: writeSigningKey(t, dir, "authorization.pem"),
		CredentialStoreURL:   "https://credential-store:8444", CredentialStoreCAFile: caFile, ManagerCertFile: certFile, ManagerKeyFile: keyFile,
		SubmissionKeyFile:    writeSigningKey(t, dir, "submission.pem"),
		CredentialIngressURL: "https://credentials.example.com",
		GitRefsURL:           "https://git-server:8446", GitRefsCAFile: caFile, GitRefsCertFile: certFile, GitRefsKeyFile: keyFile,
		Slots: 2, Lease: time.Minute, SyncTimeout: 3 * time.Minute, PublishTimeout: time.Minute,
	}
}

func TestOpenIsInertWhenTheExternalLineIsDisabled(t *testing.T) {
	// Every path is bogus on purpose: a disabled line must not read a single file.
	cfg := &RepositoryConfig{Enabled: false, AuthorizationKeyFile: "/nonexistent/key.pem", ExecutorURL: "not a url"}
	opened, err := cfg.Open()
	if err != nil {
		t.Fatalf("Open of a disabled line = %v, want no error", err)
	}
	if opened != nil {
		t.Fatalf("Open of a disabled line = %+v, want nil", opened)
	}
}

func TestOpenBuildsTheWholeExternalLine(t *testing.T) {
	cfg := repositoryFixtures(t)
	opened, err := cfg.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.Executor == nil || opened.CredentialStore == nil || opened.Refs == nil {
		t.Fatalf("opened %+v, want the executor, credential store and ref clients", opened)
	}
	if len(opened.AuthorizationKey) != ed25519.PrivateKeySize || len(opened.SubmissionKey) != ed25519.PrivateKeySize {
		t.Fatal("Open did not load both signing keys")
	}
	// One key signing both grants would make a submission signature mean nothing new.
	if opened.AuthorizationKey.Equal(opened.SubmissionKey) {
		t.Fatal("the authorization and submission keys are the same")
	}
	if opened.IngressURL != "https://credentials.example.com" {
		t.Fatalf("ingress URL = %q, want the configured origin", opened.IngressURL)
	}
	// The ref client is handed to core and the worker as their reader, so it must satisfy it.
	var _ core.GitRefReader = opened.Refs
}

func TestOpenRejectsIncompleteMaterial(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing the garbage file: %v", err)
	}
	missing := filepath.Join(dir, "absent.pem")
	for _, tc := range []struct {
		name  string
		want  string
		tweak func(*RepositoryConfig)
	}{
		{
			name:  "a missing authorization key",
			want:  "repository.authorization_key_file",
			tweak: func(c *RepositoryConfig) { c.AuthorizationKeyFile = missing },
		},
		{
			name:  "a missing submission key",
			want:  "repository.submission_key_file",
			tweak: func(c *RepositoryConfig) { c.SubmissionKeyFile = missing },
		},
		{
			name:  "one key signing both grants",
			want:  "must differ",
			tweak: func(c *RepositoryConfig) { c.SubmissionKeyFile = c.AuthorizationKeyFile },
		},
		{
			name:  "a missing dispatcher identity",
			want:  "repository executor TLS",
			tweak: func(c *RepositoryConfig) { c.DispatcherCertFile = missing },
		},
		{
			name:  "an executor CA that is not a certificate",
			want:  "repository executor TLS",
			tweak: func(c *RepositoryConfig) { c.ExecutorCAFile = garbage },
		},
		{
			name:  "a plaintext executor address",
			want:  "repo executor endpoint",
			tweak: func(c *RepositoryConfig) { c.ExecutorURL = "http://repo-executor:8443" },
		},
		{
			name:  "a missing manager identity",
			want:  "repository credential store TLS",
			tweak: func(c *RepositoryConfig) { c.ManagerKeyFile = missing },
		},
		{
			name:  "a plaintext credential store address",
			want:  "credential store base URL",
			tweak: func(c *RepositoryConfig) { c.CredentialStoreURL = "http://credential-store:8444" },
		},
		{
			name:  "a missing ref query CA",
			want:  "git ref query CA",
			tweak: func(c *RepositoryConfig) { c.GitRefsCAFile = missing },
		},
		{
			name:  "a plaintext ref query address",
			want:  "https origin",
			tweak: func(c *RepositoryConfig) { c.GitRefsURL = "http://git-server:8446" },
		},
		{
			name:  "a missing browser ingress origin",
			want:  "credential_ingress_url is required",
			tweak: func(c *RepositoryConfig) { c.CredentialIngressURL = "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := repositoryFixtures(t)
			tc.tweak(cfg)
			opened, err := cfg.Open()
			if err == nil {
				t.Fatalf("Open accepted %s and returned %+v", tc.name, opened)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
