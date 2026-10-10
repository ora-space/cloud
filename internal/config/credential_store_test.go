package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/credentialstore"
)

// writePKI mints a self-signed authority and one leaf for `identity`, writing the three PEM files a
// deployment mounts. The key material lives only in the test's temporary directory and never reaches
// a durable fixture.
func writePKI(t *testing.T, dir, name, identity string) (certificateFile, privateKeyFile, caFile string) {
	t.Helper()
	authorityPub, authorityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate authority key: %v", err)
	}
	authority := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "config test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	authorityDER, err := x509.CreateCertificate(rand.Reader, authority, authority, authorityPub, authorityKey)
	if err != nil {
		t.Fatalf("create authority certificate: %v", err)
	}
	parsedAuthority, err := x509.ParseCertificate(authorityDER)
	if err != nil {
		t.Fatalf("parse authority certificate: %v", err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatalf("parse identity %q: %v", identity, err)
	}
	leafPub, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{name},
		URIs:         []*url.URL{uri},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, parsedAuthority, leafPub, authorityKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	certificateFile = filepath.Join(dir, name+".crt")
	privateKeyFile = filepath.Join(dir, name+".key")
	caFile = filepath.Join(dir, name+".ca.crt")
	writePEM(t, certificateFile, "CERTIFICATE", leafDER)
	writePEM(t, privateKeyFile, "PRIVATE KEY", keyDER)
	writePEM(t, caFile, "CERTIFICATE", authorityDER)
	return certificateFile, privateKeyFile, caFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeKeyring writes a one-key keyring the way a deployment mounts one, so the startup gate is
// exercised with real, decodable key material that stays inside the test directory.
func writeKeyring(t *testing.T, dir string) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("draw key material: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"primary": "kek-1",
		"keys":    []map[string]string{{"id": "kek-1", "key": base64.StdEncoding.EncodeToString(key)}},
	})
	if err != nil {
		t.Fatalf("encode keyring: %v", err)
	}
	path := filepath.Join(dir, "kek.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write keyring: %v", err)
	}
	return path
}

// credentialService builds a deployment that is complete except where a case changes it.
func credentialService(t *testing.T, dir string) CredentialStoreServiceConfig {
	t.Helper()
	certificate, key, ca := writePKI(t, dir, "store.test", "spiffe://ora/cloud/credential-store")
	return CredentialStoreServiceConfig{
		Listen:                 ":8443",
		Volume:                 filepath.Join(dir, "volume"),
		KEKFile:                writeKeyring(t, dir),
		CertificateFile:        certificate,
		PrivateKeyFile:         key,
		ClientCAFile:           ca,
		IngressWriterIdentity:  "spiffe://ora/cloud/credential-ingress",
		BrokerResolverIdentity: "spiffe://ora/node/credential-executor",
		KEKOperatorIdentity:    "spiffe://ora/ops/kek-operator",
	}
}

// start mirrors the sequence `cmd/credential-store` runs before it binds the listener, so a case
// asserts that the deployment is refused at startup rather than that one accessor happens to fail.
func start(t *testing.T, cfg CredentialStoreServiceConfig) error {
	t.Helper()
	store, err := cfg.Store()
	if err != nil {
		return err
	}
	tlsConfig, err := cfg.ServerTLS()
	if err != nil {
		return err
	}
	_, err = credentialstore.NewServer(store, credentialstore.ServerConfig{TLS: tlsConfig, Identities: cfg.Identities(), Log: nil})
	return err
}

// TestCredentialStoreServiceRefusesAnIncompleteDeployment pins the startup gate D6 requires at the
// configuration layer: a service with no volume, no key-encryption key, no server identity, no client
// authority, or an identity table that would collapse the capability separation must fail before the
// listener is bound rather than come up healthy and unusable (D1, D2, D3, D6, D8).
func TestCredentialStoreServiceRefusesAnIncompleteDeployment(t *testing.T) {
	dir := t.TempDir()
	complete := credentialService(t, dir)
	if err := start(t, complete); err != nil {
		t.Fatalf("a complete deployment did not start: %v", err)
	}
	// The listener always demands a verified client certificate, and TLS 1.3 is the floor: an anonymous
	// caller must never reach a capability check at all (D3).
	tlsConfig, err := complete.ServerTLS()
	if err != nil {
		t.Fatalf("ServerTLS: %v", err)
	}
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || tlsConfig.ClientCAs == nil {
		t.Fatal("server TLS does not require a client certificate from a trusted authority")
	}
	if tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("server TLS floor = %x, want TLS 1.3", tlsConfig.MinVersion)
	}
	// The three identities the deployment names are the ones the capability table resolves.
	identities, err := credentialstore.NewIdentities(complete.Identities())
	if err != nil {
		t.Fatalf("NewIdentities: %v", err)
	}
	for identity, want := range map[string]credentialstore.Capability{
		complete.IngressWriterIdentity:  credentialstore.CapabilityWrite,
		complete.BrokerResolverIdentity: credentialstore.CapabilityResolve,
		complete.KEKOperatorIdentity:    credentialstore.CapabilityKeys,
	} {
		if got, known := identities.Capability(identity); !known || got != want {
			t.Fatalf("identity %q resolved to (%q, %v), want %q", identity, got, known, want)
		}
	}

	permissiveVolume := filepath.Join(dir, "readable")
	if err := os.MkdirAll(permissiveVolume, 0o755); err != nil {
		t.Fatalf("prepare readable volume: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CredentialStoreServiceConfig)
		reason string
	}{
		{"no listen address", func(c *CredentialStoreServiceConfig) { c.Listen = "  " }, "the service would bind nothing"},
		{"no volume", func(c *CredentialStoreServiceConfig) { c.Volume = "" }, "there is no other place to keep material"},
		{"no key-encryption key", func(c *CredentialStoreServiceConfig) { c.KEKFile = filepath.Join(dir, "absent.json") }, "an absent KEK must not read as an empty store"},
		{"a volume other accounts can read", func(c *CredentialStoreServiceConfig) { c.Volume = permissiveVolume }, "the volume would hand out the offline material for free"},
		{"no server certificate", func(c *CredentialStoreServiceConfig) { c.CertificateFile = "" }, "the service could not prove who it is"},
		{"no client authority", func(c *CredentialStoreServiceConfig) { c.ClientCAFile = "" }, "no client certificate could be verified"},
		{"a blank identity", func(c *CredentialStoreServiceConfig) { c.BrokerResolverIdentity = "" }, "the resolver capability would be held by nobody"},
		{"the same identity in two roles", func(c *CredentialStoreServiceConfig) { c.BrokerResolverIdentity = c.IngressWriterIdentity }, "one certificate would hold both write and resolve"},
		{"an identity that is not a spiffe URI", func(c *CredentialStoreServiceConfig) { c.IngressWriterIdentity = "https://ora/cloud/ingress" }, "the identity would not match the platform's spelling"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := credentialService(t, t.TempDir())
			test.mutate(&cfg)
			if err := start(t, cfg); err == nil {
				t.Fatalf("an incomplete deployment started: %s", test.reason)
			}
		})
	}
}

// TestLoadCredentialStoreConfigBindsOnlyItsOwnEnvironment pins the deployment separation D1 relies on:
// the service reads `CREDENTIAL_STORE_*` and nothing a Cloud configuration or the Cloud server's own
// prefix can set, so Cloud cannot inherit the service's volume or listen address and no Cloud variable
// can point the service at Cloud's storage.
func TestLoadCredentialStoreConfigBindsOnlyItsOwnEnvironment(t *testing.T) {
	t.Setenv("CREDENTIAL_STORE_LISTEN", ":9443")
	t.Setenv("CREDENTIAL_STORE_VOLUME", "/var/lib/credential-store")
	t.Setenv("CREDENTIAL_STORE_KEK_FILE", "/etc/credential-store/kek.json")
	t.Setenv("CREDENTIAL_STORE_SUPERSEDED_RETENTION", "90m")
	// A Cloud-prefixed spelling of the same field must not reach the service, and neither must the
	// Cloud server's own listen address.
	t.Setenv("CLOUD_KEK_FILE", "/cloud/kek.json")
	t.Setenv("CLOUD_LISTEN", ":9999")

	// An empty path lets viper search its documented locations, none of which holds a service file in
	// this repository; every asserted value therefore comes from the environment.
	cfg, err := LoadCredentialStoreConfig("")
	if err != nil {
		t.Fatalf("LoadCredentialStoreConfig: %v", err)
	}
	if cfg.Listen != ":9443" {
		t.Fatalf("listen = %q, want the value from CREDENTIAL_STORE_LISTEN", cfg.Listen)
	}
	if cfg.Volume != "/var/lib/credential-store" || cfg.KEKFile != "/etc/credential-store/kek.json" {
		t.Fatalf("volume/key file = (%q, %q), want the mounted paths the service was given", cfg.Volume, cfg.KEKFile)
	}
	if cfg.SupersededRetention != 90*time.Minute {
		t.Fatalf("superseded retention = %v, want 90m", cfg.SupersededRetention)
	}
	if cfg.CertificateFile != "" {
		t.Fatalf("an unset field resolved to %q", cfg.CertificateFile)
	}

	// A configuration file that exists but cannot be read is an error rather than a silent empty
	// deployment: D6 must not turn a typo in a mounted path into a service with no identities.
	if _, err := LoadCredentialStoreConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("an unreadable configuration file was accepted")
	}
}
