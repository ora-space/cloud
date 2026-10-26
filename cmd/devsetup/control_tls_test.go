package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/controlgrpc"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

func TestControlMaterialSupportsSecureController(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "control")
	if err := materializeControl(dir); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, name := range controlMaterialNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o", name, info.Mode().Perm())
		}
	}
	if err := materializeControl(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rerun changed %s: %v", name, err)
		}
	}
	control := config.ControlConfig{
		CertificateFile: filepath.Join(dir, "server.pem"), PrivateKeyFile: filepath.Join(dir, "server.key"),
		ClientCAFile: filepath.Join(dir, "ca.pem"), ControllerIdentity: localControllerIdentity,
	}
	serverTLS, err := control.TLS()
	if err != nil {
		t.Fatal(err)
	}
	server, err := controlgrpc.NewSecure(nil, serverTLS, control.ControllerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	healthpb.RegisterHealthServer(server, health.NewServer())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(before["ca.pem"]) {
		t.Fatal("invalid generated CA")
	}
	pair, err := tls.X509KeyPair(before["controller.pem"], before["controller.key"])
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || len(leaf.URIs) != 1 || leaf.URIs[0].String() != localControllerIdentity {
		t.Fatalf("generated Controller URI does not match configured identity: %v", err)
	}
	for _, host := range []string{"localhost", "cloud", "127.0.0.1", "::1"} {
		if err := serverTLS.Certificates[0].Leaf.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		certs  []tls.Certificate
		wantOK bool
	}{
		{"controller", []tls.Certificate{pair}, true},
		{"no certificate", nil, false},
		{"server certificate as client", serverTLS.Certificates, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: tc.certs,
			})))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, "x-ora-controller-id", "local-controller")
			_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			if (err == nil) != tc.wantOK {
				t.Fatalf("mTLS call error = %v, want success %v", err, tc.wantOK)
			}
		})
	}
}

func TestControlMaterialRejectsInvalidExistingBundleWithoutChangingFiles(t *testing.T) {
	type corruptionCase struct {
		name   string
		damage func(*testing.T, string)
	}
	cases := []corruptionCase{
		{"mismatched private key", func(t *testing.T, dir string) {
			copyControlFile(t, filepath.Join(dir, "server.key"), filepath.Join(dir, "controller.key"))
		}},
		{"mismatched CA private key", func(t *testing.T, dir string) {
			copyControlFile(t, filepath.Join(dir, "controller.key"), filepath.Join(dir, "ca.key"))
		}},
		{"different issuer", func(t *testing.T, dir string) {
			other := t.TempDir()
			if err := materializeControl(other); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"controller.pem", "controller.key"} {
				copyControlFile(t, filepath.Join(other, name), filepath.Join(dir, name))
			}
		}},
		{"expired certificate", func(t *testing.T, dir string) {
			reissueControlLeaf(t, dir, "controller", func(cert *x509.Certificate) {
				cert.NotBefore = time.Now().Add(-2 * time.Hour)
				cert.NotAfter = time.Now().Add(-time.Hour)
			})
		}},
		{"wrong client purpose", func(t *testing.T, dir string) {
			reissueControlLeaf(t, dir, "controller", func(cert *x509.Certificate) {
				cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			})
		}},
		{"wrong Controller identity", func(t *testing.T, dir string) {
			reissueControlLeaf(t, dir, "controller", func(cert *x509.Certificate) {
				cert.URIs = []*url.URL{{Scheme: "spiffe", Host: "ora.local", Path: "/controller/other"}}
			})
		}},
		{"wrong server address", func(t *testing.T, dir string) {
			reissueControlLeaf(t, dir, "server", func(cert *x509.Certificate) {
				cert.DNSNames = []string{"other"}
			})
		}},
	}
	for _, name := range controlMaterialNames {
		cases = append(cases, corruptionCase{"truncated " + name, func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("interrupted write"), 0o600); err != nil {
				t.Fatal(err)
			}
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := materializeControl(dir); err != nil {
				t.Fatal(err)
			}
			tc.damage(t, dir)
			before := make(map[string][]byte)
			for _, name := range controlMaterialNames {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = data
			}
			if err := materializeControl(dir); err == nil || !strings.Contains(err.Error(), "invalid control mTLS bundle") {
				t.Fatalf("invalid bundle must fail during setup, got %v", err)
			}
			for name, want := range before {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("failed setup changed %s: %v", name, err)
				}
			}
		})
	}
}

func copyControlFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reissue a well-formed, CA-signed identity so rejection tests exercise the
// certificate's contract rather than failing PEM parsing or key matching first.
func reissueControlLeaf(t *testing.T, dir, name string, change func(*x509.Certificate)) {
	t.Helper()
	issuer, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(issuer.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	change(cert)
	key, ok := issuer.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("expected Ed25519 CA")
	}
	der, privateKey, err := createControlCertificate(cert, ca, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestControlMaterialRejectsIncompleteBundle(t *testing.T) {
	for _, missing := range controlMaterialNames {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			if err := materializeControl(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, missing)); err != nil {
				t.Fatal(err)
			}
			if err := materializeControl(dir); err == nil || !strings.Contains(err.Error(), "must exist together") {
				t.Fatalf("partial bundle accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, missing)); !os.IsNotExist(err) {
				t.Fatalf("missing %s regenerated: %v", missing, err)
			}
		})
	}
}

func TestMaterializeWritesControlEnv(t *testing.T) {
	root := t.TempDir()
	cfg := &devConfig{Plugins: defaultPluginsConfig()}
	envPath := filepath.Join(root, "dev.env")
	if err := materialize(cfg, filepath.Join(root, "gateway"), envPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"CLOUD_CONTROL_CERTIFICATE_FILE":    filepath.Join(root, "control", "server.pem"),
		"CLOUD_CONTROL_PRIVATE_KEY_FILE":    filepath.Join(root, "control", "server.key"),
		"CLOUD_CONTROL_CLIENT_CA_FILE":      filepath.Join(root, "control", "ca.pem"),
		"CLOUD_CONTROL_CONTROLLER_IDENTITY": localControllerIdentity,
	} {
		if !strings.Contains(string(data), name+"="+quoteEnv(value)+"\n") {
			t.Fatalf("env missing %s", name)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "control", "controller.pem"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := materialize(cfg, filepath.Join(root, "gateway"), envPath); err == nil {
		t.Fatal("setup must fail before rewriting env when the final certificate was truncated")
	}
	after, err := os.ReadFile(envPath)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("failed setup rewrote the environment: %v", err)
	}
}
