package modelgateway

import (
	"bytes"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestCipherAuthenticatesRecordIdentityKeyAndCiphertext(t *testing.T) {
	c := testCipher(t)
	secret := []byte(randomSecret(t))
	defer clear(secret)
	sealed, err := c.Seal("credential-1", secret)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := c.Open("credential-1", "test-v1", sealed)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatal("authenticated round trip failed")
	}
	clear(opened)
	for _, test := range []struct {
		id, key string
		value   []byte
	}{{"other", "test-v1", sealed}, {"credential-1", "other", sealed}, {"credential-1", "test-v1", sealed[:8]}} {
		if _, err := c.Open(test.id, test.key, test.value); err == nil {
			t.Fatal("unauthenticated record accepted")
		}
	}
	modified := append([]byte(nil), sealed...)
	modified[len(modified)-1] ^= 1
	if _, err := c.Open("credential-1", "test-v1", modified); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
}

func TestCredentialRetryFingerprintIncludesSecretOwnerAndVersion(t *testing.T) {
	c := testCipher(t)
	identity := &core.Claims{Source: "source"}
	secret := randomSecret(t)
	id := c.CredentialID(identity, "connection", 1, "operation", secret)
	if id != c.CredentialID(identity, "connection", 1, "operation", secret) {
		t.Fatal("same retry changed credential identity")
	}
	if id == c.CredentialID(identity, "connection", 2, "operation", secret) || id == c.CredentialID(identity, "connection", 1, "operation", randomSecret(t)) || id == c.CredentialID(&core.Claims{Source: "other"}, "connection", 1, "operation", secret) {
		t.Fatal("changed operation scope reused credential identity")
	}
}
