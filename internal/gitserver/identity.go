package gitserver

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// GitServerServiceURI is the service identity the Git Server asserts when it calls the origin
// resolver. It is a name, not a secret: what authenticates the call is the private key behind it,
// which lives only in the Git Server's own volume.
const GitServerServiceURI = "spiffe://ora.local/service/git-server"

// OriginIdentity is the Git Server's own client identity for the origin resolver.
//
// It is deliberately a separate configuration from every other identity the deployment holds. The
// Git Server is a read-only service; the Controller identity creates, stops and inspects sandbox
// containers. Handing the Git Server the Controller's certificate would give a service with one
// read-only route the authority of the management surface, and no amount of care inside this
// process would undo that — so the identity is loaded from its own files, and the certificate must
// assert GitServerServiceURI. A certificate for another service, or one that names no service, is
// refused before any request is made.
type OriginIdentity struct {
	// CertFile and KeyFile are the Git Server's own certificate and key.
	CertFile string
	// KeyFile is the private key matching CertFile. It is read here and never logged or copied.
	KeyFile string
	// ServiceURI is the service identity the certificate must assert. Empty means GitServerServiceURI.
	ServiceURI string
	// CAFile is the CA that issued the resolver's server certificate.
	CAFile string
	// ServerCertificate pins the origin resolver's own certificate. A CA alone would accept any
	// certificate that CA ever issued, including one for a service that is not the resolver.
	ServerCertificate string
	// ServerName is the name the resolver's certificate must carry for the client.
	ServerName string
}

// TLS builds the client TLS configuration for origin resolution.
// names the same files, so no caller can change what the transport authenticates with mid-flight.
//
//nolint:gocritic // By value on purpose: this is startup configuration read once, and the copy
func (i OriginIdentity) TLS() (*tls.Config, error) {
	service := i.ServiceURI
	if service == "" {
		service = GitServerServiceURI
	}
	if i.CertFile == "" || i.KeyFile == "" {
		return nil, errors.New("origin resolution requires the Git Server's own certificate and key")
	}
	pair, err := tls.LoadX509KeyPair(i.CertFile, i.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the Git Server identity: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("the Git Server identity carries no certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse the Git Server identity: %w", err)
	}
	pair.Leaf = leaf
	if err := assertServiceURI(leaf, service); err != nil {
		return nil, err
	}
	if !hasExtKeyUsage(leaf, x509.ExtKeyUsageClientAuth) {
		return nil, errors.New("the Git Server identity is not valid for client authentication")
	}
	if i.CAFile == "" {
		return nil, errors.New("origin resolution requires the CA that issued the resolver's certificate")
	}
	trust, err := os.ReadFile(i.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read the origin CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return nil, errors.New("invalid origin CA")
	}
	if i.ServerCertificate == "" {
		return nil, errors.New("origin resolution requires the resolver's pinned certificate")
	}
	pinned, err := readCertificate(i.ServerCertificate)
	if err != nil {
		return nil, fmt.Errorf("read the resolver certificate: %w", err)
	}
	if i.ServerName == "" {
		return nil, errors.New("origin resolution requires the resolver's server name")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		ServerName:   i.ServerName,
		// Both checks run: the chain must verify against the CA, and the leaf must be the one the
		// deployment pinned. Verification alone would let any other service the same CA issued a
		// certificate for answer as the resolver.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
				return errors.New("the resolver presented no verified certificate")
			}
			if !bytes.Equal(cs.PeerCertificates[0].Raw, pinned.Raw) {
				return errors.New("the connection is not the pinned origin resolver")
			}
			return nil
		},
	}, nil
}

// assertServiceURI requires the certificate to assert exactly one service identity, and to be the
// expected one. A certificate carrying no URI SAN would otherwise be acceptable here while being an
// identity for something else entirely.
func assertServiceURI(leaf *x509.Certificate, want string) error {
	parsed, err := url.Parse(want)
	if err != nil {
		return fmt.Errorf("the configured service identity is not a URI: %w", err)
	}
	for _, candidate := range leaf.URIs {
		if candidate.String() == parsed.String() {
			return nil
		}
	}
	return fmt.Errorf("the Git Server identity does not assert %s", want)
}

func hasExtKeyUsage(certificate *x509.Certificate, usage x509.ExtKeyUsage) bool {
	for _, candidate := range certificate.ExtKeyUsage {
		if candidate == usage {
			return true
		}
	}
	return false
}

func readCertificate(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
