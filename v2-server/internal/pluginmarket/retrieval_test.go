package pluginmarket

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestCredentialsCannotMoveToAnotherSource(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	input := Credentials{"fixture-id", "fixture-secret"}
	raw, e := SealCredentials(input, "space/a")
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(input.SecretKey)) {
		t.Fatal("stored plaintext")
	}
	if c, e := OpenCredentials(raw, "space/a"); e != nil || c != input {
		t.Fatal("could not recover credentials")
	}
	if _, e := OpenCredentials(raw, "space/b"); e == nil {
		t.Fatal("credentials crossed space boundary")
	}
}
func TestSignOnlyConfiguredBucket(t *testing.T) {
	r := Retrieval{Mode: "s3_sigv4", Endpoint: "https://s3.example.com", Bucket: "packages", Region: "us-east-1"}
	for _, tc := range []struct {
		locator string
		signed  bool
	}{{"skills/one.orax", true}, {"https://s3.example.com/packages/one.orax", true}, {"https://s3.example.com/other/one.orax", false}, {"https://another.example/packages/one.orax", false}} {
		if _, signed, e := packageURL(tc.locator, r); e != nil || tc.signed != signed {
			t.Fatal(tc.locator, signed, e)
		}
	}
}
