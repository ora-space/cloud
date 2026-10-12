package credentialstore

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// This file is the cross-process acceptance for the authorization half of the credential boundary.
//
// The other tests in this package mount the Store's handler in the test process, which proves the
// decisions but not the deployment: they never load a configuration file, never read key material off
// a mount, and never have a process to lose. These tests run the real `cmd/credential-store` binary
// as a child process, over a real mutually authenticated TLS listener, with the same file layout a
// deployment uses — so what they prove is that the shipped command, wired the way the deployment
// wires it, issues nothing and delivers nothing it should not.
//
// What they deliberately do not prove: that a real *Cloud server* issues the authorization. There is
// no Cloud process that does that yet — `cloneauth.Issue` has no production caller, because the only
// call sites sit behind a guard that refuses every credentialed clone (see the repository report).
// Issuance here runs Cloud's real code, from a real mounted signing key, against a real Store
// process; the caller is the test rather than the Cloud server, and that is the whole of the gap.

// syncBuffer collects a child process's output. os/exec copies into a non-file writer from its own
// goroutine, so every read of the collected text is synchronized: the race detector runs over this
// package, and a bare bytes.Buffer would be a genuine data race rather than a false positive.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// moduleRoot walks up from the test's working directory to the module root, so the test can build the
// command it is about to run without depending on where `go test` was invoked from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no go.mod found above %s", directory)
		}
		directory = parent
	}
}

// freeAddress reserves one loopback port by binding it and releasing it. The window between the
// release and the Service's own bind is small, and this package runs no other test that binds a port.
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return address
}

// writePEMFile writes one PEM block. Generated key material reaches only temporary paths, never a
// durable fixture (cloud/AGENTS.md).
func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

// writeCertificateChain writes a leaf and the authority that signed it, which is the form the
// Service's listener presents.
func writeCertificateChain(t *testing.T, path string, certificate tls.Certificate) {
	t.Helper()
	var encoded []byte
	for _, der := range certificate.Certificate {
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

// writePrivateKey writes one PKCS#8 private key, the form `openssl genpkey` produces.
func writePrivateKey(t *testing.T, path string, key crypto.Signer) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	writePEMFile(t, path, "PRIVATE KEY", der)
}

// storeDeployment is one laid-out deployment: the built command, the certificates and keys a
// deployment mounts, the configuration file it reads, and the process running from them.
type storeDeployment struct {
	binary       string
	configPath   string
	volume       string
	logFile      string
	address      string
	pki          *testPKI
	ingressCert  tls.Certificate
	resolverCert tls.Certificate
	// outgoing and incoming are the two signing keys the Store trusts at once, which is the state a
	// deployment is in during a rotation. untrusted is a third key it does not trust.
	outgoing  ed25519.PrivateKey
	incoming  ed25519.PrivateKey
	untrusted ed25519.PrivateKey

	command *exec.Cmd
	state   *processState
	output  *syncBuffer
}

// newStoreDeployment builds the command and lays out everything it will read.
func newStoreDeployment(t *testing.T) *storeDeployment {
	t.Helper()
	base := t.TempDir()
	binary := filepath.Join(base, "credential-store")
	build := exec.Command("go", "build", "-o", binary, "./cmd/credential-store")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/credential-store: %v\n%s", err, output)
	}

	pki := newTestPKI(t)
	writePEMFile(t, filepath.Join(base, "ca.pem"), "CERTIFICATE", pki.caDER)
	server := pki.issue(t, 1, testServerIdentity, true)
	serverCertificate := filepath.Join(base, "server.pem")
	serverKey := filepath.Join(base, "server-key.pem")
	writeCertificateChain(t, serverCertificate, server)
	writePrivateKey(t, serverKey, server.PrivateKey.(ed25519.PrivateKey))

	outgoingPublic, outgoing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the outgoing signing key: %v", err)
	}
	incomingPublic, incoming, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the incoming signing key: %v", err)
	}
	_, untrusted, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an untrusted signing key: %v", err)
	}
	publicFile := writePublicKeyFile(t, filepath.Join(base, "cloneauth.pem"), outgoingPublic, incomingPublic)

	volume := filepath.Join(base, "volume")
	if err := os.MkdirAll(volume, 0o700); err != nil {
		t.Fatalf("prepare the volume: %v", err)
	}
	keyFile := writeKeyringFile(t, filepath.Join(base, "kek.json"), "kek-primary", map[string][]byte{"kek-primary": keyMaterial(t)})

	address := freeAddress(t)
	logFile := filepath.Join(base, "credential-store.log")
	configPath := filepath.Join(base, "credential-store.yaml")
	// Both destinations are configured. The file is where the deployment's durable audit records land,
	// in the JSON form an operator would actually inspect; stdout carries the same lines through the
	// console encoder and is what a failed start or a crash is reported from.
	config := fmt.Sprintf(`listen: %s
volume: %s
kek_file: %s
authorization_public_key_file: %s
certificate_file: %s
private_key_file: %s
client_ca_file: %s
ingress_writer_identity: %s
broker_resolver_identity: %s
kek_operator_identity: %s
superseded_retention: 1h
logger:
  level: info
  enable_console: true
  filename: %s
`,
		address, volume, keyFile, publicFile, serverCertificate, serverKey,
		filepath.Join(base, "ca.pem"), testIngressIdentity, testResolverIdentity, testOperatorIdentity, logFile)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}

	return &storeDeployment{
		binary:       binary,
		configPath:   configPath,
		volume:       volume,
		logFile:      logFile,
		address:      address,
		pki:          pki,
		ingressCert:  pki.issue(t, 10, testIngressIdentity, false),
		resolverCert: pki.issue(t, 11, testResolverIdentity, false),
		outgoing:     outgoing,
		incoming:     incoming,
		untrusted:    untrusted,
	}
}

// auditLog reads the Service's durable audit records. The Service writes them before it answers, so a
// record for an operation the client has already seen the response to is on disk by the time this runs.
func (d *storeDeployment) auditLog(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(d.logFile)
	if err != nil {
		t.Fatalf("read the Service's audit log: %v", err)
	}
	return string(raw)
}

// start runs the Service and waits until it accepts connections. Every failure message carries the
// child's own output, because a process that refused to start is the interesting case.
func (d *storeDeployment) start(t *testing.T) {
	t.Helper()
	command := exec.Command(d.binary, "-config", d.configPath)
	command.Dir = filepath.Dir(d.configPath)
	// The Service binds `CREDENTIAL_STORE_*` from its environment by design, so the child is given an
	// environment of its own: nothing the test process happens to export can reach its configuration,
	// and the configuration file is the only thing that decides how it runs.
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	output := &syncBuffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatalf("start the credential store: %v", err)
	}
	// The exit is published as a closed channel with the result written before the close, so a reader
	// that sees the close also sees the result, and no reader can consume the signal out from under
	// another. `stop` waits on this instead of on a value, which is what keeps a restart from racing
	// the previous process.
	state := &processState{done: make(chan struct{})}
	go func() {
		state.err = command.Wait()
		close(state.done)
	}()
	d.command, d.state, d.output = command, state, output
	t.Cleanup(func() { d.stop(t) })
	d.await(t)
}

// processState is how one child process ended, published once and readable by any number of callers.
type processState struct {
	done chan struct{}
	// err is written before done is closed, so reading it after receiving from done is safe.
	err error
}

// wait reports the child's exit result, or false while it is still running.
func (p *processState) wait() (error, bool) {
	select {
	case <-p.done:
		return p.err, true
	default:
		return nil, false
	}
}

// await waits until the Service has bound its listener *and* reported it.
//
// The startup line is the signal rather than a bare TCP connect: the Service binds its listener before
// it starts serving, and a connect that succeeds in that window would have the test race the server's
// first accept. Waiting for the line the process writes immediately before `Serve` also means a child
// that dies during startup is reported with its own output rather than as a connection failure.
func (d *storeDeployment) await(t *testing.T) {
	t.Helper()
	const ready = "credential store listening"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err, exited := d.state.wait(); exited {
			t.Fatalf("the credential store exited before it listened (%v):\n%s", err, d.output.String())
		}
		if strings.Contains(d.output.String(), ready) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the credential store did not report %q on %s in time:\n%s", ready, d.address, d.output.String())
}

// stop terminates the Service and waits for it, so a restart cannot race the previous process.
func (d *storeDeployment) stop(t *testing.T) {
	t.Helper()
	if d.command == nil {
		return
	}
	command, state := d.command, d.state
	d.command, d.state = nil, nil
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal the credential store: %v", err)
	}
	select {
	case <-state.done:
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		<-state.done
		t.Fatalf("the credential store did not stop on SIGTERM:\n%s", d.output.String())
	}
}

// exitNote says whether the child is still running, so a transport failure distinguishes a Service
// that refused from one that died.
func (d *storeDeployment) exitNote() string {
	if d.state == nil {
		return ""
	}
	if err, exited := d.state.wait(); exited {
		return fmt.Sprintf("\nthe credential store exited: %v", err)
	}
	return "\nthe credential store is still running"
}

// client returns an HTTP client presenting one service identity.
func (d *storeDeployment) client(t *testing.T, certificate tls.Certificate) *http.Client {
	t.Helper()
	return d.pki.client(t, &certificate)
}

// anonymousClient returns a client that trusts the Service but presents no certificate, so the
// handshake itself is what fails.
func (d *storeDeployment) anonymousClient(t *testing.T) *http.Client {
	t.Helper()
	return d.pki.client(t, nil)
}

// issue signs one binding with a chosen key, so a test can present an authorization the deployment
// does trust and one it does not.
func (d *storeDeployment) issue(t *testing.T, key ed25519.PrivateKey, binding cloneauth.Binding) string {
	t.Helper()
	token, err := cloneauth.Issue(key, binding)
	if err != nil {
		t.Fatalf("issue authorization: %v", err)
	}
	return token
}

// binding builds a complete binding for the deployment under test. The instants are explicit so the
// expiry case can present an authorization whose window has already closed without sleeping.
func (d *storeDeployment) binding(credentialID string, issued time.Time, lifetime time.Duration) cloneauth.Binding {
	return cloneauth.Binding{
		AuthorizationID:   testAuthorizationID(1),
		Operation:         cloneauth.OperationClone,
		TenantID:          "tenant-1",
		OwnerUserID:       "user-1",
		ProjectID:         "project-1",
		WorkspaceID:       "workspace-1",
		ExecutionID:       "execution-1",
		CredentialID:      credentialID,
		CredentialVersion: 1,
		Repository:        "https://github.com/ora-space/private",
		NodeID:            testNodeID,
		NodeInstanceID:    testNodeInstanceID,
		NodeIncarnationID: testNodeIncarnationID,
		RuntimeGeneration: testRuntimeGeneration,
		ControlEpoch:      testControlEpoch,
		IssuedAt:          issued,
		ExpiresAt:         issued.Add(lifetime),
	}
}

// request performs one call against the running Service.
//
// A transport failure carries the child's own output, because "the Service died while answering" is
// the interesting case and its log is the only account of why.
func (d *storeDeployment) request(t *testing.T, client *http.Client, method, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, "https://"+d.address+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v%s\n--- credential store output ---\n%s", method, path, err, d.exitNote(), d.output.String())
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response to %s %s: %v%s\n--- credential store output ---\n%s", method, path, err, d.exitNote(), d.output.String())
	}
	return response.StatusCode, raw
}

// write stores one credential through the ingress identity and returns the version the Service
// assigned it.
func (d *storeDeployment) write(t *testing.T, client *http.Client, credentialID, token string) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"credentialId":%q,"token":%q}`, credentialID, token)
	status, raw := d.request(t, client, http.MethodPost, "/v1/credentials", body)
	if status != http.StatusOK {
		t.Fatalf("write status = %d, want 200 (body %s)\n--- credential store output ---\n%s", status, raw, d.output.String())
	}
	version, ok := decodeBody(t, raw)["version"].(float64)
	if !ok {
		t.Fatalf("write response carries no version: %s", raw)
	}
	return int64(version)
}

// resolve redeems one authorization through the resolver identity.
func (d *storeDeployment) resolve(t *testing.T, client *http.Client, credentialID, authorization string) (int, map[string]any) {
	t.Helper()
	status, raw := d.request(t, client, http.MethodPost, "/v1/credentials/"+credentialID+"/resolve", resolveBody(t, authorization))
	return status, decodeBody(t, raw)
}

// TestCredentialStoreProcessDeliversAndConsumesOneAuthorization is the acceptance the decisions ask
// for at the process boundary: one authorization, issued by Cloud's own code from a mounted signing
// key, buys exactly one token from a real Service process, and every later attempt at the same
// authorization is refused — including after the Service has been restarted, which is what makes the
// consumption record durable state rather than process memory (D10).
func TestCredentialStoreProcessDeliversAndConsumesOneAuthorization(t *testing.T) {
	deployment := newStoreDeployment(t)
	deployment.start(t)
	const credentialID = "00000000-0000-4000-8000-0000000000aa"
	const token = "ghp_process_bound_token"

	ingress := deployment.client(t, deployment.ingressCert)
	resolver := deployment.client(t, deployment.resolverCert)
	if version := deployment.write(t, ingress, credentialID, token); version != 1 {
		t.Fatalf("first write reported version %d, want 1", version)
	}

	authorization := deployment.issue(t, deployment.outgoing, deployment.binding(credentialID, time.Now().UTC(), time.Minute))
	status, body := deployment.resolve(t, resolver, credentialID, authorization)
	if status != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (body %v)", status, body)
	}
	if body["token"] != token {
		t.Fatalf("resolved token = %v, want %q", body["token"], token)
	}

	status, body = deployment.resolve(t, resolver, credentialID, authorization)
	if status != http.StatusConflict || body["code"] != "authorization_consumed" {
		t.Fatalf("replay = %d %v, want 409 authorization_consumed", status, body)
	}

	// The consumption record is the Service's own, in its own volume: the restarted process must find
	// it without asking Cloud or the Node anything.
	deployment.stop(t)
	deployment.start(t)
	resolver = deployment.client(t, deployment.resolverCert)
	status, body = deployment.resolve(t, resolver, credentialID, authorization)
	if status != http.StatusConflict || body["code"] != "authorization_consumed" {
		t.Fatalf("replay after restart = %d %v, want 409 authorization_consumed", status, body)
	}

	// A fresh authorization for the same credential still works after the restart, so the refusal
	// above is about the spent authorization rather than about a Service that stopped delivering.
	fresh := deployment.binding(credentialID, time.Now().UTC(), time.Minute)
	fresh.AuthorizationID = testAuthorizationID(2)
	freshToken := deployment.issue(t, deployment.outgoing, fresh)
	status, body = deployment.resolve(t, resolver, credentialID, freshToken)
	if status != http.StatusOK {
		t.Fatalf("resolve after restart status = %d, want 200 (body %v)", status, body)
	}
	if body["token"] != token {
		t.Fatalf("resolved token after restart = %v, want %q", body["token"], token)
	}

	// The consumption record is in the Service's own volume, and it is evidence rather than a secret:
	// it names the authorization, the runtime that spent it and the outcome, and carries nothing from
	// which the token could be recovered (D10).
	recordPath := filepath.Join(deployment.volume, authorizationDirectory, testAuthorizationID(1)+".json")
	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read the consumption record: %v", err)
	}
	if !strings.Contains(string(record), `"result":"`+consumptionDelivered+`"`) {
		t.Fatalf("consumption record reads %s, want result %s", record, consumptionDelivered)
	}
	if strings.Contains(string(record), token) {
		t.Fatalf("the consumption record contains the token: %s", record)
	}

	logged := deployment.auditLog(t)
	if strings.Contains(logged, token) {
		t.Fatalf("the Service audit log contains the token:\n%s", logged)
	}
	if strings.Contains(logged, authorization) || strings.Contains(logged, freshToken) {
		t.Fatalf("the Service audit log contains an authorization:\n%s", logged)
	}
	if !strings.Contains(logged, `"operation":"resolve"`) || !strings.Contains(logged, `"result":"succeeded"`) {
		t.Fatalf("the Service audit log carries no successful resolve record:\n%s", logged)
	}
	if !strings.Contains(logged, `"result":"authorization_consumed"`) {
		t.Fatalf("the Service audit log carries no replay refusal:\n%s", logged)
	}
	// The console destination carries the same records, and must be equally free of secrets.
	console := deployment.output.String()
	if strings.Contains(console, token) || strings.Contains(console, authorization) {
		t.Fatalf("the Service console output contains a secret:\n%s", console)
	}
}

// TestCredentialStoreProcessEnforcesCapabilitySeparation proves the two factors D3 requires are both
// enforced by the running Service rather than by its callers: a valid authorization is worthless to
// an identity that holds the write capability, and the resolve capability cannot write.
func TestCredentialStoreProcessEnforcesCapabilitySeparation(t *testing.T) {
	deployment := newStoreDeployment(t)
	deployment.start(t)
	const credentialID = "00000000-0000-4000-8000-0000000000bb"

	ingress := deployment.client(t, deployment.ingressCert)
	resolver := deployment.client(t, deployment.resolverCert)
	deployment.write(t, ingress, credentialID, "ghp_separation")
	authorization := deployment.issue(t, deployment.outgoing, deployment.binding(credentialID, time.Now().UTC(), time.Minute))

	status, body := deployment.resolve(t, ingress, credentialID, authorization)
	if status != http.StatusForbidden || body["code"] != "capability_denied" {
		t.Fatalf("resolve as the ingress identity = %d %v, want 403 capability_denied", status, body)
	}

	writeBody := fmt.Sprintf(`{"credentialId":%q,"token":"ghp_should_not_be_written"}`, "00000000-0000-4000-8000-0000000000bc")
	status, raw := deployment.request(t, resolver, http.MethodPost, "/v1/credentials", writeBody)
	if status != http.StatusForbidden {
		t.Fatalf("write as the resolver identity = %d, want 403 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "capability_denied" {
		t.Fatalf("write as the resolver identity code = %v, want capability_denied", code)
	}

	// The refused resolve must not have spent the authorization.
	status, body = deployment.resolve(t, resolver, credentialID, authorization)
	if status != http.StatusOK {
		t.Fatalf("resolve after the refused attempt = %d %v, want 200", status, body)
	}
}

// TestCredentialStoreProcessRefusesEveryInvalidAuthorization walks the refusal matrix over the real
// transport, so each outcome D7 requires to be distinguishable is observed at the wire rather than
// inferred from a returned error value.
func TestCredentialStoreProcessRefusesEveryInvalidAuthorization(t *testing.T) {
	deployment := newStoreDeployment(t)
	deployment.start(t)
	const credentialID = "00000000-0000-4000-8000-0000000000cc"

	ingress := deployment.client(t, deployment.ingressCert)
	resolver := deployment.client(t, deployment.resolverCert)
	deployment.write(t, ingress, credentialID, "ghp_refusals")

	now := time.Now().UTC()
	cases := []struct {
		name          string
		authorization func() string
		wantStatus    int
		wantCode      string
	}{
		{
			name: "signed by a key the deployment does not trust",
			authorization: func() string {
				return deployment.issue(t, deployment.untrusted, deployment.binding(credentialID, now, time.Minute))
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_invalid",
		},
		{
			name: "issued for another runtime",
			authorization: func() string {
				binding := deployment.binding(credentialID, now, time.Minute)
				binding.ControlEpoch = testControlEpoch + 1
				return deployment.issue(t, deployment.outgoing, binding)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_binding_mismatch",
		},
		{
			name: "already past its expiry",
			authorization: func() string {
				return deployment.issue(t, deployment.outgoing, deployment.binding(credentialID, now.Add(-2*time.Minute), time.Minute))
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_expired",
		},
		{
			name: "not an authorization at all",
			authorization: func() string {
				return "not-a-cloud-issued-authorization"
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_invalid",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, body := deployment.resolve(t, resolver, credentialID, test.authorization())
			if status != test.wantStatus || body["code"] != test.wantCode {
				t.Fatalf("resolve = %d %v, want %d %s", status, body, test.wantStatus, test.wantCode)
			}
		})
	}

	// None of the refusals may have spent anything, and the identity that was refused at the
	// handshake below must not have reached the Store at all.
	status, body := deployment.resolve(t, resolver, credentialID, deployment.issue(t, deployment.outgoing, deployment.binding(credentialID, now, time.Minute)))
	if status != http.StatusOK {
		t.Fatalf("resolve after the refusals = %d %v, want 200", status, body)
	}
}

// TestCredentialStoreProcessRefusesAnAnonymousCaller proves the listener is not merely checking a
// header: a client with no certificate never completes the handshake, so it cannot reach a
// capability decision, a credential id, or an authorization at all (D3).
func TestCredentialStoreProcessRefusesAnAnonymousCaller(t *testing.T) {
	deployment := newStoreDeployment(t)
	deployment.start(t)
	const credentialID = "00000000-0000-4000-8000-0000000000dd"

	ingress := deployment.client(t, deployment.ingressCert)
	deployment.write(t, ingress, credentialID, "ghp_anonymous")
	authorization := deployment.issue(t, deployment.outgoing, deployment.binding(credentialID, time.Now().UTC(), time.Minute))

	response, err := deployment.anonymousClient(t).Post(
		"https://"+deployment.address+"/v1/credentials/"+credentialID+"/resolve",
		"application/json",
		bytes.NewReader([]byte(resolveBody(t, authorization))),
	)
	if err == nil {
		response.Body.Close()
		t.Fatalf("an anonymous client reached the Service with status %d", response.StatusCode)
	}
	// The Service is still healthy, so the refusal is the handshake rather than a process that died.
	resolver := deployment.client(t, deployment.resolverCert)
	if status, body := deployment.resolve(t, resolver, credentialID, authorization); status != http.StatusOK {
		t.Fatalf("resolve after the anonymous attempt = %d %v, want 200", status, body)
	}
}

// TestCredentialStoreProcessTrustsEveryMountedSigningKey proves the rotation overlap holds across the
// process boundary too: with two keys mounted, an authorization signed by either is delivered, which
// is what lets a deployment change its signing key without refusing the clones already authorized
// under the outgoing one.
func TestCredentialStoreProcessTrustsEveryMountedSigningKey(t *testing.T) {
	deployment := newStoreDeployment(t)
	deployment.start(t)
	const credentialID = "00000000-0000-4000-8000-0000000000ee"

	ingress := deployment.client(t, deployment.ingressCert)
	resolver := deployment.client(t, deployment.resolverCert)
	deployment.write(t, ingress, credentialID, "ghp_rotation")

	for index, signer := range []ed25519.PrivateKey{deployment.outgoing, deployment.incoming} {
		binding := deployment.binding(credentialID, time.Now().UTC(), time.Minute)
		binding.AuthorizationID = testAuthorizationID(40 + index)
		status, body := deployment.resolve(t, resolver, credentialID, deployment.issue(t, signer, binding))
		if status != http.StatusOK {
			t.Fatalf("resolve with mounted key %d = %d %v, want 200", index, status, body)
		}
	}
}
