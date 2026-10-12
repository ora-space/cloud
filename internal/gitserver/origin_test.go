package gitserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/gitread"
)

// originPKI is a CA with the leaves these tests need: the resolver's own server certificate and the
// Git Server's client identity. It is local to this file because it exists to prove the *client*
// half; the sandbox-server's own tests prove the server half against the same shape.
type originPKI struct {
	pool       *x509.CertPool
	ca         *x509.Certificate
	caKey      ed25519.PrivateKey
	caPath     string
	server     leaf
	gitServer  leaf
	otherLeaf  leaf
	serverName string
}

type leaf struct {
	certificate tls.Certificate
	certPath    string
	keyPath     string
}

func newOriginPKI(t *testing.T) *originPKI {
	t.Helper()
	root := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ora-origin-test-ca"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pki := &originPKI{ca: ca, caKey: private, serverName: "origin.test"}
	pki.pool = x509.NewCertPool()
	pki.pool.AddCert(ca)
	pki.caPath = writePEM(t, root, "ca.pem", "CERTIFICATE", der)
	pki.server = pki.issue(t, "origin-resolver", x509.ExtKeyUsageServerAuth, nil, []string{"origin.test"})
	pki.gitServer = pki.issue(t, "git-server", x509.ExtKeyUsageClientAuth, []string{GitServerServiceURI}, nil)
	pki.otherLeaf = pki.issue(t, "some-other-service", x509.ExtKeyUsageClientAuth, nil, nil)
	return pki
}

func (p *originPKI) issue(t *testing.T, commonName string, usage x509.ExtKeyUsage, uris, dns []string) leaf {
	t.Helper()
	dir := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, DNSNames: dns,
		NotBefore: p.ca.NotBefore, NotAfter: p.ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	for _, raw := range uris {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = append(template.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, public, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: secret})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return leaf{
		certificate: pair,
		certPath:    writePEM(t, dir, commonName+".pem", "CERTIFICATE", der),
		keyPath:     writePEM(t, dir, commonName+".key", "PRIVATE KEY", secret),
	}
}

func writePEM(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// identity is the Git Server's own configuration for calling a resolver whose certificate is pinned
// at serverCert.
func (p *originPKI) identity(client leaf, serverCert string) OriginIdentity {
	return OriginIdentity{
		CertFile: client.certPath, KeyFile: client.keyPath, ServiceURI: GitServerServiceURI,
		CAFile: p.caPath, ServerCertificate: serverCert, ServerName: p.serverName,
	}
}

// resolverServer runs a TLS server that authenticates its caller the way the origin listener does:
// the chain must verify against the CA and the leaf must be the pinned Git Server identity. It
// answers with whatever the handler writes.
func (p *originPKI) resolverServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	pinned, err := readCertificate(p.gitServer.certPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.server.certificate},
		ClientCAs:    p.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !equalDER(cs.PeerCertificates[0].Raw, pinned.Raw) {
				return errors.New("not the pinned caller")
			}
			return nil
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func equalDER(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// client builds an OriginClient pointed at server with the identity described.
func (p *originPKI) client(t *testing.T, server *httptest.Server, identity OriginIdentity) (*OriginClient, error) {
	t.Helper()
	target := server.URL
	// httptest serves on 127.0.0.1; the certificate names origin.test, so the client is told to
	// reach the address under the name the certificate carries.
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Host = net.JoinHostPort("127.0.0.1", parsed.Port())
	return NewOriginClient(parsed.String(), identity, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestOriginClientResolvesThroughTheGitServerIdentity(t *testing.T) {
	pki := newOriginPKI(t)
	var sawPath, sawAddress string
	var sawCaller string
	server := pki.resolverServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawAddress = r.URL.Path, r.URL.Query().Get("address")
		sawCaller = r.TLS.PeerCertificates[0].Subject.CommonName
		_ = json.NewEncoder(w).Encode(originIdentity{
			ContainerID:   "9c1f2ab34d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8",
			ContainerName: "ora-workspace-1", WorkspaceID: "workspace-1",
			SandboxID: "8f2b1c4e-5a6d-4e7f-8a9b-0c1d2e3f4a5b", EffectID: "8f2b1c4e-5a6d-4e7f-8a9b-0c1d2e3f4a5b",
			Atespace: "local", Address: "172.24.0.2",
		})
	})
	client, err := pki.client(t, server, pki.identity(pki.gitServer, pki.server.certPath))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The two identifiers differ in this fixture on purpose: Cloud's row for a live sandbox is keyed
	// by the sandbox instance id, so binding the Docker container id instead would compare a
	// substrate id against an instance id and refuse every read in a real deployment.
	want := gitread.Origin{
		WorkspaceID:   "workspace-1",
		ContainerID:   "8f2b1c4e-5a6d-4e7f-8a9b-0c1d2e3f4a5b",
		ContainerName: "ora-workspace-1",
	}
	if origin != want {
		t.Fatalf("origin = %+v, want %+v", origin, want)
	}
	if sawPath != originPath || sawAddress != "172.24.0.2" {
		t.Fatalf("the resolver saw %s?address=%s", sawPath, sawAddress)
	}
	if sawCaller != "git-server" {
		t.Fatalf("the resolver authenticated %q, want the Git Server's own identity", sawCaller)
	}
}

func TestOriginClientDistinguishesARefusalFromAFailureToAnswer(t *testing.T) {
	pki := newOriginPKI(t)
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "no container holds the address", status: http.StatusForbidden, body: "forbidden", want: ErrOriginUnresolvable},
		{name: "the resolver is unreadable", status: http.StatusServiceUnavailable, body: "container state unavailable", want: ErrOriginUnavailable},
		{name: "the resolver answered a malformed request", status: http.StatusBadRequest, body: "invalid address", want: ErrOriginUnavailable},
		{name: "the resolver failed", status: http.StatusInternalServerError, body: "internal error", want: ErrOriginUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := pki.resolverServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			})
			client, err := pki.client(t, server, pki.identity(pki.gitServer, pki.server.certPath))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2")); !errors.Is(err, tc.want) {
				t.Fatalf("status %d produced %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestOriginClientRefusesAnAnswerThatIsNotAnIdentity(t *testing.T) {
	pki := newOriginPKI(t)
	cases := map[string]string{
		"no container":        `{"workspaceId":"workspace-1","address":"172.24.0.2"}`,
		"no workspace":        `{"containerId":"container-1","address":"172.24.0.2"}`,
		"another address":     `{"containerId":"container-1","workspaceId":"workspace-1","address":"172.24.0.9"}`,
		"not an object":       `[]`,
		"empty body":          ``,
		"an unknown identity": `{"sandboxId":"container-1","atespace":"local"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := pki.resolverServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})
			client, err := pki.client(t, server, pki.identity(pki.gitServer, pki.server.certPath))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2"))
			if !errors.Is(err, ErrOriginUnavailable) {
				t.Fatalf("a 200 with %s produced %v, want unavailability", body, err)
			}
		})
	}
}

func TestOriginClientPinsTheResolverItTalksTo(t *testing.T) {
	pki := newOriginPKI(t)
	// A complete, valid identity, so that the only thing that can produce unavailability in the
	// subtests below is the pinning they are about: with an incomplete answer the resolver's status
	// would be indistinguishable from a broken pin.
	server := pki.resolverServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(originIdentity{
			ContainerID: "container-1", SandboxID: "instance-1", WorkspaceID: "workspace-1",
		})
	})

	t.Run("the certificate is not the pinned one", func(t *testing.T) {
		// A different service's certificate, issued by the same CA and valid for serving, must not
		// be able to answer as the resolver.
		impostor := pki.issue(t, "impostor", x509.ExtKeyUsageServerAuth, nil, []string{"origin.test"})
		client, err := pki.client(t, server, pki.identity(pki.gitServer, impostor.certPath))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2")); !errors.Is(err, ErrOriginUnavailable) {
			t.Fatalf("an unpinned certificate produced %v, want unavailability", err)
		}
	})

	t.Run("an identity the resolver has not pinned", func(t *testing.T) {
		// The previous Git Server identity after a rotation: a valid certificate for this same
		// service, signed by the same CA, that the resolver no longer pins.
		previous := pki.issue(t, "git-server-previous", x509.ExtKeyUsageClientAuth, []string{GitServerServiceURI}, nil)
		client, err := pki.client(t, server, pki.identity(previous, pki.server.certPath))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2")); !errors.Is(err, ErrOriginUnavailable) {
			t.Fatalf("a caller the resolver did not pin produced %v, want unavailability", err)
		}
	})

	t.Run("the resolver is unreachable", func(t *testing.T) {
		client, err := pki.client(t, server, pki.identity(pki.gitServer, pki.server.certPath))
		if err != nil {
			t.Fatal(err)
		}
		server.Close()
		if _, err := client.Resolve(context.Background(), netip.MustParseAddr("172.24.0.2")); !errors.Is(err, ErrOriginUnavailable) {
			t.Fatalf("an unreachable resolver produced %v, want unavailability", err)
		}
	})
}

func TestOriginIdentityMustBeTheGitServersOwnClientIdentity(t *testing.T) {
	pki := newOriginPKI(t)
	base := pki.identity(pki.gitServer, pki.server.certPath)
	cases := map[string]func(*OriginIdentity){
		"no certificate":                   func(i *OriginIdentity) { i.CertFile, i.KeyFile = "", "" },
		"no key":                           func(i *OriginIdentity) { i.KeyFile = "" },
		"no CA":                            func(i *OriginIdentity) { i.CAFile = "" },
		"no pinned resolver":               func(i *OriginIdentity) { i.ServerCertificate = "" },
		"no server name":                   func(i *OriginIdentity) { i.ServerName = "" },
		"a certificate that is not a file": func(i *OriginIdentity) { i.CertFile = filepath.Join(t.TempDir(), "absent.pem") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			identity := base
			mutate(&identity)
			if _, err := identity.TLS(); err == nil {
				t.Fatal("an incomplete identity was accepted")
			}
		})
	}

	t.Run("a certificate for another service", func(t *testing.T) {
		// The identity must assert the Git Server's own service URI. A certificate the same CA
		// issued to something else is not this service's identity, however valid it is.
		identity := pki.identity(pki.otherLeaf, pki.server.certPath)
		if _, err := identity.TLS(); err == nil {
			t.Fatal("a certificate asserting no service identity was accepted")
		}
	})

	t.Run("a certificate asserting a different service", func(t *testing.T) {
		other := pki.issue(t, "controller", x509.ExtKeyUsageClientAuth, []string{"spiffe://ora.local/service/controller"}, nil)
		identity := pki.identity(other, pki.server.certPath)
		if _, err := identity.TLS(); err == nil {
			t.Fatal("another service's identity was accepted as the Git Server's")
		}
	})

	t.Run("a server-only certificate", func(t *testing.T) {
		serverOnly := pki.issue(t, "server-only", x509.ExtKeyUsageServerAuth, []string{GitServerServiceURI}, nil)
		identity := pki.identity(serverOnly, pki.server.certPath)
		if _, err := identity.TLS(); err == nil {
			t.Fatal("a certificate that is not valid for client authentication was accepted")
		}
	})

	t.Run("a plaintext resolver URL", func(t *testing.T) {
		if _, err := NewOriginClient("http://127.0.0.1:18001", base, time.Second, nil); err == nil {
			t.Fatal("a plaintext resolver URL was accepted")
		}
	})
}
