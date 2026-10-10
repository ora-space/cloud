package credentialstore

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Capability is one of the three non-overlapping powers the Store grants to service identities.
//
// D3 splits them because a single compromised credential must not become both "read every secret"
// and "write or erase every secret": the ingress path can replace material but can never read it
// back, and the resolving path can read one secret but can never write, list, or erase.
type Capability string

const (
	// CapabilityWrite covers storing a new secret, rotating it, and revoking it.
	CapabilityWrite Capability = "write"
	// CapabilityResolve covers resolving one secret behind a single-use authorization.
	CapabilityResolve Capability = "resolve"
	// CapabilityKeys covers deployment-side key operations such as re-wrapping data keys under a new
	// KEK. It is held by the deployment, never by Cloud's or the Node's runtime path.
	CapabilityKeys Capability = "keys"
)

// IdentityConfig names the three fixed service identities the deployment issued.
//
// Every field is required. There are no defaults and no wildcard identity: D3 fixes the set at
// three, and a Store that started without them would accept no caller at all while looking healthy,
// so a missing identity is a startup refusal instead.
type IdentityConfig struct {
	// IngressWriter is the identity Cloud's credential ingress authenticates as.
	IngressWriter string
	// BrokerResolver is the identity the credential-executing workload authenticates as.
	BrokerResolver string
	// KEKOperator is the identity the deployment's key operations authenticate as.
	KEKOperator string
}

// Identities resolves a presented certificate identity to the single capability it holds.
type Identities struct {
	byIdentity map[string]Capability
}

// NewIdentities validates the deployment's identity set.
//
// The three identities must be present, distinct, and valid SPIFFE identifiers. Distinctness is
// enforced rather than assumed: reusing one certificate for two roles would collapse the separation
// D3 exists to create, and the failure would be invisible at runtime because every call would still
// authenticate.
func NewIdentities(cfg IdentityConfig) (*Identities, error) {
	byIdentity := make(map[string]Capability, 3)
	for _, entry := range []struct {
		identity   string
		capability Capability
	}{
		{cfg.IngressWriter, CapabilityWrite},
		{cfg.BrokerResolver, CapabilityResolve},
		{cfg.KEKOperator, CapabilityKeys},
	} {
		if err := validServiceIdentity(entry.identity); err != nil {
			return nil, fmt.Errorf("%s identity: %w", entry.capability, err)
		}
		if _, duplicate := byIdentity[entry.identity]; duplicate {
			return nil, fmt.Errorf("service identity %q holds more than one capability", entry.identity)
		}
		byIdentity[entry.identity] = entry.capability
	}
	return &Identities{byIdentity: byIdentity}, nil
}

// Capability returns the capability a presented identity holds.
func (i *Identities) Capability(identity string) (Capability, bool) {
	capability, ok := i.byIdentity[identity]
	return capability, ok
}

// validServiceIdentity accepts only a SPIFFE URI naming exactly one workload.
//
// The same shape Cloud's control listener uses for its Controller identity is reused deliberately:
// one identity spelling across the platform means a reviewer reads one rule, and a URI with extra
// path segments or a query cannot smuggle a second, unchecked name into the certificate.
func validServiceIdentity(identity string) error {
	if strings.TrimSpace(identity) == "" {
		return errors.New("is not configured")
	}
	uri, err := url.Parse(identity)
	if err != nil {
		return errors.New("is not a valid URI")
	}
	if uri.Scheme != "spiffe" || uri.Host == "" {
		return errors.New("is not a spiffe:// URI with a trust domain")
	}
	if uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil {
		return errors.New("must not carry a query, fragment, or user info")
	}
	segments := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return errors.New("must name exactly one workload as /<kind>/<name>")
	}
	return nil
}

// PresentedIdentity returns the configured identity a mutually authenticated peer proved.
//
// Verification is deliberately manual rather than delegated to a name check: the certificate chain
// must already have been validated by the TLS layer against the client CA, and the identity is the
// SPIFFE URI in the leaf's subject alternative names. A peer presenting a certificate that verified
// but names an identity nobody holds is rejected here, not treated as anonymous.
func (i *Identities) PresentedIdentity(state *tls.ConnectionState) (string, error) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return "", errors.New("client certificate is required")
	}
	for _, presented := range state.PeerCertificates[0].URIs {
		if _, known := i.byIdentity[presented.String()]; known {
			return presented.String(), nil
		}
	}
	return "", errors.New("client certificate does not name a known service identity")
}
