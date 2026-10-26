package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The frozen plan item carries an association exactly when the release declares one. The catalog
// keeps an undeclared listing's association as SQL NULL, so a frozen selection holds a JSON null
// (or, from before the association existed, no key); neither may become an empty association,
// which the Node rejects as an invalid declaration.
func TestPluginPayloadCarriesOnlyADeclaredAgentConfiguration(t *testing.T) {
	declared := Object{"schemaVersion": float64(2), "declarationSha256": "ab", "copyPath": "agent-configuration.json", "declaration": "{}"}
	for name, tc := range map[string]struct {
		release string
		want    any
	}{
		"null association":   {`{"id":"official/a","version":"1.0.0","agentConfiguration":null}`, nil},
		"absent association": {`{"id":"official/a","version":"1.0.0"}`, nil},
		"declared":           {mustJSON(t, Object{"id": "official/a", "version": "1.0.0", "agentConfiguration": declared}), Object{"schemaVersion": int64(2), "declarationSha256": "ab"}},
	} {
		t.Run(name, func(t *testing.T) {
			var release Object
			if e := json.Unmarshal([]byte(tc.release), &release); e != nil {
				t.Fatal(e)
			}
			got, present := pluginPayload(release)["agentConfiguration"]
			if tc.want == nil {
				if present {
					t.Fatalf("undeclared release must carry no association key, got %#v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("declared association must carry only its identity verbatim: got %#v want %#v", got, tc.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	text, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return string(text)
}
