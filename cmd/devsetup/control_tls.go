package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const localControllerIdentity = "spiffe://ora.local/controller/local-controller"

var controlMaterialNames = []string{"ca.key", "ca.pem", "server.key", "server.pem", "controller.key", "controller.pem"}

// materializeControl keeps the trust bundle together: a partial bundle needs explicit
// repair rather than silently replacing a CA or an identity already trusted by a peer.
func materializeControl(dir string) error {
	present := 0
	for _, name := range controlMaterialNames {
		info, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect control material %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("control material %s must be a regular file", name)
		}
		present++
	}
	if present == len(controlMaterialNames) {
		if err := validateControlBundle(dir); err != nil {
			return fmt.Errorf("invalid control mTLS bundle in %s; restore the original files or remove the whole bundle to regenerate: %w", dir, err)
		}
		report("control mTLS bundle", false)
		return nil
	}
	if present != 0 {
		return fmt.Errorf("control mTLS files in %s must exist together; restore the missing files or remove the whole bundle to regenerate", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "Ora-local-management-CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0),
		BasicConstraintsValid: true, IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caCert, caKey, err := createControlCertificate(ca, ca, nil)
	if err != nil {
		return err
	}
	server := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "cloud"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		DNSNames:    []string{"localhost", "cloud"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverCert, serverKey, err := createControlCertificate(server, ca, caKey)
	if err != nil {
		return err
	}
	identity, err := url.Parse(localControllerIdentity)
	if err != nil {
		return err
	}
	controller := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "local-controller"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		URIs: []*url.URL{identity}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	controllerCert, controllerKey, err := createControlCertificate(controller, ca, caKey)
	if err != nil {
		return err
	}
	// Generate everything before writing. O_EXCL prevents concurrent setup from
	// overwriting another issuer; an interrupted write remains an explicit partial bundle.
	for _, entry := range []struct {
		name string
		cert []byte
		key  ed25519.PrivateKey
	}{
		{"ca", caCert, caKey}, {"server", serverCert, serverKey}, {"controller", controllerCert, controllerKey},
	} {
		keyDER, err := x509.MarshalPKCS8PrivateKey(entry.key)
		if err != nil {
			return err
		}
		for _, file := range []struct {
			name string
			data []byte
		}{
			{entry.name + ".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
			{entry.name + ".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: entry.cert})},
		} {
			created, err := writeNew(filepath.Join(dir, file.name), file.data)
			if err != nil {
				return err
			}
			if !created {
				return fmt.Errorf("control material %s appeared during setup; rerun after checking the bundle", file.name)
			}
		}
	}
	report("control mTLS bundle", true)
	return nil
}

// validateControlBundle catches interrupted final writes and mismatched or expired
// identities before setup reports success. It never repairs or rotates trust implicitly.
func validateControlBundle(dir string) error {
	certificates := make(map[string]*x509.Certificate)
	for _, name := range []string{"ca", "server", "controller"} {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
		if err != nil {
			return fmt.Errorf("load %s identity: %w", name, err)
		}
		certificate, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return fmt.Errorf("parse %s certificate: %w", name, err)
		}
		certificates[name] = certificate
	}
	ca := certificates["ca"]
	if err := ca.CheckSignatureFrom(ca); err != nil {
		return fmt.Errorf("verify local CA: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, identity := range []struct {
		name  string
		usage x509.ExtKeyUsage
	}{
		{"server", x509.ExtKeyUsageServerAuth},
		{"controller", x509.ExtKeyUsageClientAuth},
	} {
		if _, err := certificates[identity.name].Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{identity.usage}}); err != nil {
			return fmt.Errorf("verify %s certificate: %w", identity.name, err)
		}
	}
	for _, host := range []string{"localhost", "cloud", "127.0.0.1", "::1"} {
		if err := certificates["server"].VerifyHostname(host); err != nil {
			return fmt.Errorf("verify server address: %w", err)
		}
	}
	for _, uri := range certificates["controller"].URIs {
		if uri.String() == localControllerIdentity {
			return nil
		}
	}
	return fmt.Errorf("controller certificate must identify %s", localControllerIdentity)
}

func createControlCertificate(template, parent *x509.Certificate, issuer ed25519.PrivateKey) ([]byte, ed25519.PrivateKey, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template.SerialNumber = serial.Add(serial, big.NewInt(1))
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if issuer == nil {
		issuer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, issuer)
	return der, key, err
}

func renderControlEnv(dir string) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"CLOUD_CONTROL_CERTIFICATE_FILE", filepath.Join(dir, "server.pem")},
		{"CLOUD_CONTROL_PRIVATE_KEY_FILE", filepath.Join(dir, "server.key")},
		{"CLOUD_CONTROL_CLIENT_CA_FILE", filepath.Join(dir, "ca.pem")},
		{"CLOUD_CONTROL_CONTROLLER_IDENTITY", localControllerIdentity},
	} {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], quoteEnv(kv[1]))
	}
	return b.String()
}
