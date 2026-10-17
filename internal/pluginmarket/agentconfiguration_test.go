package pluginmarket

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The shared Agent declaration contract vectors. The file is the byte-identical copy of the
// desktop repository's crates/plugin-config/tests/fixtures/agent-configuration-contract.json
// (branch feat/213-agent-configuration-contract): the same inputs and expected outputs
// constrain the Rust compiler, this Go mirror, and every consumer that verifies a package's
// declaration bytes, so the languages cannot drift apart silently. When the desktop fixture
// changes, this copy changes in the same change.
const agentContractVectors = "testdata/agent-configuration-contract.json"

type agentContractCase struct {
	Name     string          `json:"name"`
	Source   string          `json:"source"`
	Expected json.RawMessage `json:"expected"`
}

// Every vector must reproduce the desktop compiler's exact outcome, success or bounded error
// code, with no case skipped.
func TestCompileAgentConfigurationMatchesSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(agentContractVectors)
	if err != nil {
		t.Fatal(err)
	}
	var cases []agentContractCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the shared contract vectors must not be empty")
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			var actual any
			compiled, err := CompileAgentConfiguration([]byte(testCase.Source))
			switch {
			case err != nil:
				actual = map[string]any{"error": err.(*AgentConfigurationError).Code}
			case compiled == nil:
				t.Fatalf("%s: compiled to nil without an error", testCase.Name)
			default:
				// Round-trip through JSON so number literals compare the way the vectors
				// spell them, not the way Go might narrow them.
				encoded, encodeErr := json.Marshal(compiled)
				if encodeErr != nil {
					t.Fatalf("%s: marshal compiled value: %v", testCase.Name, encodeErr)
				}
				var compiledValue any
				if err := json.Unmarshal(encoded, &compiledValue); err != nil {
					t.Fatalf("%s: re-parse compiled value: %v", testCase.Name, err)
				}
				actual = map[string]any{"ok": compiledValue}
			}
			var expected any
			if err := json.Unmarshal(testCase.Expected, &expected); err != nil {
				t.Fatalf("%s: parse expected outcome: %v", testCase.Name, err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: outcome mismatch\nactual:   %#v\nexpected: %#v", testCase.Name, actual, expected)
			}
		})
	}
}

// The fingerprint covers the exact source bytes, so reformatting changes the declaration's
// release identity even when the compiled capabilities are unchanged.
func TestCompileAgentConfigurationFingerprintsExactBytes(t *testing.T) {
	minimal := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
	pretty := "{\n  \"schemaVersion\": 2,\n  \"agent\": {\n    \"modelAccess\": {\n      \"contractVersion\": 1,\n      \"protocols\": [\"anthropic-messages\"],\n      \"modelCardinality\": \"single\"\n    }\n  }\n}"
	compact, err := CompileAgentConfiguration([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := CompileAgentConfiguration([]byte(pretty))
	if err != nil {
		t.Fatal(err)
	}
	if compact.Fingerprint == formatted.Fingerprint {
		t.Fatal("reformatting must change the declaration fingerprint")
	}
	if !reflect.DeepEqual(compact.ModelAccess, formatted.ModelAccess) {
		t.Fatal("reformatting must not change the compiled capabilities")
	}
}
