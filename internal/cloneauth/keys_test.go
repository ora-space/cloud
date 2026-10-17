package cloneauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func pemBlock(t *testing.T, blockType string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

func writeKeyFile(t *testing.T, content ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	var all []byte
	for _, c := range content {
		all = append(all, c...)
	}
	if err := os.WriteFile(path, all, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadKeys(t *testing.T) {
	public, private := newKey(t)
	otherPublic, _ := newKey(t)
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	publicDER, _ := x509.MarshalPKIXPublicKey(public)
	otherDER, _ := x509.MarshalPKIXPublicKey(otherPublic)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	ecPrivateDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	ecPublicDER, _ := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)

	loaded, err := LoadPrivateKey(writeKeyFile(t, pemBlock(t, "PRIVATE KEY", privateDER)))
	if err != nil || !loaded.Equal(private) {
		t.Fatalf("LoadPrivateKey = %v; want the written key", err)
	}
	keys, err := LoadPublicKeys(writeKeyFile(t, pemBlock(t, "PUBLIC KEY", publicDER), pemBlock(t, "PUBLIC KEY", otherDER)))
	if err != nil || len(keys) != 2 || !keys[0].Equal(public) || !keys[1].Equal(otherPublic) {
		t.Fatalf("LoadPublicKeys = %d keys, %v; want both written keys in order", len(keys), err)
	}

	privateFailures := map[string]string{
		"two blocks":   writeKeyFile(t, pemBlock(t, "PRIVATE KEY", privateDER), pemBlock(t, "PRIVATE KEY", privateDER)),
		"wrong type":   writeKeyFile(t, pemBlock(t, "EC PRIVATE KEY", privateDER)),
		"not pkcs8":    writeKeyFile(t, pemBlock(t, "PRIVATE KEY", []byte("junk"))),
		"not ed25519":  writeKeyFile(t, pemBlock(t, "PRIVATE KEY", ecPrivateDER)),
		"no block":     writeKeyFile(t, []byte("not pem")),
		"missing file": filepath.Join(t.TempDir(), "absent.pem"),
		"no path":      "",
	}
	for name, path := range privateFailures {
		if _, err := LoadPrivateKey(path); !errors.Is(err, ErrKey) {
			t.Fatalf("LoadPrivateKey(%s) = %v, want ErrKey", name, err)
		}
	}
	publicFailures := map[string]string{
		"mixed good and bad": writeKeyFile(t, pemBlock(t, "PUBLIC KEY", publicDER), pemBlock(t, "PUBLIC KEY", ecPublicDER)),
		"wrong type":         writeKeyFile(t, pemBlock(t, "CERTIFICATE", publicDER)),
		"not pkix":           writeKeyFile(t, pemBlock(t, "PUBLIC KEY", []byte("junk"))),
		"empty file":         writeKeyFile(t),
		"missing file":       filepath.Join(t.TempDir(), "absent.pem"),
	}
	for name, path := range publicFailures {
		if _, err := LoadPublicKeys(path); !errors.Is(err, ErrKey) {
			t.Fatalf("LoadPublicKeys(%s) = %v, want ErrKey", name, err)
		}
	}
}
