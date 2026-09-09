package core

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the only accepted internal identity format. User claims bind the calling service.
type Claims struct {
	jwt.RegisteredClaims
	Kind        string `json:"kind"`
	Role        string `json:"role,omitempty"`
	Caller      string `json:"caller,omitempty"`
	Source      string `json:"source,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	SandboxID   string `json:"sandboxId,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
}

// TrustedKey pins issuer, purpose, and service role as well as the signing key.
type (
	TrustedKey struct {
		ID            string            `mapstructure:"id"`
		Issuer        string            `mapstructure:"issuer"`
		Kind          string            `mapstructure:"kind"`
		Role          string            `mapstructure:"role"`
		PublicKeyFile string            `mapstructure:"public_key_file"`
		Key           ed25519.PublicKey `mapstructure:"-"`
	}
	// Authenticator verifies Ed25519 credentials; it cannot issue credentials.
	Authenticator struct {
		Keys     map[string]TrustedKey
		Audience string
		Now      func() time.Time
	}
)

func NewAuthenticator(audience string, keys []TrustedKey) (*Authenticator, error) {
	if audience == "" || len(keys) == 0 {
		return nil, fmt.Errorf("audience and trusted keys required")
	}
	a := &Authenticator{Keys: map[string]TrustedKey{}, Audience: audience, Now: time.Now}
	for _, k := range keys {
		if k.ID == "" || k.Issuer == "" || (k.Kind != "service" && k.Kind != "user") {
			return nil, fmt.Errorf("invalid trusted key configuration")
		}
		if _, ok := a.Keys[k.ID]; ok {
			return nil, fmt.Errorf("duplicate key id")
		}
		if len(k.Key) == 0 {
			b, e := os.ReadFile(k.PublicKeyFile)
			if e != nil {
				return nil, e
			}
			block, _ := pem.Decode(b)
			if block == nil {
				return nil, fmt.Errorf("invalid public PEM")
			}
			v, e := x509.ParsePKIXPublicKey(block.Bytes)
			if e != nil {
				return nil, e
			}
			var ok bool
			k.Key, ok = v.(ed25519.PublicKey)
			if !ok {
				return nil, fmt.Errorf("Ed25519 key required")
			}
		}
		a.Keys[k.ID] = k
	}
	return a, nil
}

func (a *Authenticator) Verify(raw, kind string) (*Claims, error) {
	c := &Claims{}
	token, e := jwt.ParseWithClaims(raw, c, func(token *jwt.Token) (any, error) {
		id, ok := token.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("kid required")
		}
		k, ok := a.Keys[id]
		if !ok || k.Kind != kind || c.Kind != kind || k.Issuer != c.Issuer || (kind == "service" && k.Role != c.Role) {
			return nil, fmt.Errorf("untrusted credential")
		}
		return k.Key, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience(a.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(a.Now))
	if e != nil || token == nil || !token.Valid || c.Subject == "" || c.IssuedAt == nil || c.ExpiresAt == nil {
		return nil, fmt.Errorf("invalid credential")
	}
	if c.ExpiresAt.Sub(c.IssuedAt.Time) > 5*time.Minute || !c.ExpiresAt.After(c.IssuedAt.Time) || c.IssuedAt.After(a.Now()) {
		return nil, fmt.Errorf("invalid credential lifetime")
	}
	if kind == "user" && (c.Source == "" || c.Caller == "") {
		return nil, fmt.Errorf("identity and caller binding required")
	}
	return c, nil
}
