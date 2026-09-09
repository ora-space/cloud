package contract

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestPublishedOpenAPIIsValidAndCurrent(t *testing.T) {
	published, e := os.ReadFile("../../api/openapi.json")
	if e != nil {
		t.Fatal(e)
	}
	generated, e := json.MarshalIndent(Document(), "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if string(published) != string(append(generated, '\n')) {
		t.Fatal("api/openapi.json is stale; go run ./cmd/openapi")
	}
	loader := openapi3.NewLoader()
	doc, e := loader.LoadFromData(published)
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
}
