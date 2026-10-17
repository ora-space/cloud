package credentialstore

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// The fixed service identities of the external Git line (SPIFFE URI SANs, trust domain ora.local).
const (
	// ExecutorIdentity is the Repo Executor, the only caller that may redeem plaintext.
	ExecutorIdentity = cloneauth.ExecutorIdentity
	// CredentialManagerIdentity is Cloud's credential manager: metadata reads and revocation only.
	CredentialManagerIdentity = "spiffe://ora.local/cloud/credential-manager"
	// KEKOperatorIdentity is the deployment's key operator: DEK rewrap only.
	KEKOperatorIdentity = "spiffe://ora.local/credential-store/kek-operator"
)

// errNoPeerIdentity reports a connection without a verified leaf carrying exactly one URI SAN.
var errNoPeerIdentity = errors.New("client certificate does not carry exactly one URI identity")

// Identities names the one client identity each mTLS listener trusts.
type Identities struct {
	// Redeemer may redeem plaintext on the redemption listener.
	Redeemer string
	// Manager may read metadata and revoke on the management listener.
	Manager string
	// KeyOperator may rewrap DEKs on the key listener.
	KeyOperator string
}

// DefaultIdentities returns the identities of the deployed topology.
func DefaultIdentities() Identities {
	return Identities{Redeemer: ExecutorIdentity, Manager: CredentialManagerIdentity, KeyOperator: KEKOperatorIdentity}
}

// Validate refuses a missing or malformed identity and any identity trusted by two listeners: a
// certificate that could both redeem and manage would collapse the separation the listeners exist
// to create, and the failure would be invisible at runtime because every call still authenticates.
func (i Identities) Validate() error {
	seen := make(map[string]string, 3)
	for _, entry := range []struct{ role, identity string }{
		{"redeemer", i.Redeemer}, {"manager", i.Manager}, {"key operator", i.KeyOperator},
	} {
		if err := validSPIFFE(entry.identity); err != nil {
			return fmt.Errorf("%s identity %w", entry.role, err)
		}
		if other, dup := seen[entry.identity]; dup {
			return fmt.Errorf("identity %q is trusted by both the %s and the %s listener", entry.identity, other, entry.role)
		}
		seen[entry.identity] = entry.role
	}
	return nil
}

func validSPIFFE(identity string) error {
	if identity == "" {
		return errors.New("is not configured")
	}
	parsed, err := url.Parse(identity)
	if err != nil || parsed.Scheme != "spiffe" || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != identity {
		return errors.New("is not a canonical spiffe:// URI")
	}
	return nil
}

// PeerIdentity returns the single URI SAN of the verified client leaf. The chain must already be
// verified by the TLS layer against the configured client CA; a leaf with zero or several URI SANs
// is refused rather than matched on any one of them, so a certificate cannot smuggle a second name.
func PeerIdentity(state *tls.ConnectionState) (string, error) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return "", errNoPeerIdentity
	}
	leaf := state.VerifiedChains[0][0]
	if len(leaf.URIs) != 1 {
		return "", errNoPeerIdentity
	}
	return leaf.URIs[0].String(), nil
}
