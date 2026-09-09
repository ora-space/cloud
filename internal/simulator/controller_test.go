package simulator

import (
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestObjectsRejectsMalformedCloudResponse(t *testing.T) {
	_, err := objects(core.Object{"effects": []any{"not-an-object"}}, "effects")
	if err == nil || !strings.Contains(err.Error(), "item 0 is not an object") {
		t.Fatalf("malformed response was not rejected: %v", err)
	}
}

func TestCredentialsRejectUnknownRole(t *testing.T) {
	credentials, err := NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = credentials.Token("unknown", core.Claims{}); err == nil {
		t.Fatal("unknown simulator role was signed")
	}
}
