package core

import "testing"

// Metadata is not capability: every exact execution requirement must match the live incarnation.
func TestDeclaredAgentCapabilityMatrix(t *testing.T) {
	configuration := Object{"target": "x86_64-unknown-linux-gnu", "adapterVersion": int64(1), "protocol": "anthropic-messages"}
	valid := Object{"contractVersion": int64(1), "target": "x86_64-unknown-linux-gnu", "protocols": []string{"openai-completions", "anthropic-messages"}}
	for _, test := range []struct {
		name                  string
		capability            Object
		proxy, valid, capable bool
	}{
		{"absent", Object{}, true, true, false},
		{"exact", valid, true, true, true},
		{"no mTLS", valid, false, false, false},
		{"other target", Object{"contractVersion": int64(1), "target": "aarch64-unknown-linux-gnu", "protocols": []string{"anthropic-messages"}}, true, true, false},
		{"other protocol", Object{"contractVersion": int64(1), "target": "x86_64-unknown-linux-gnu", "protocols": []string{"openai-completions"}}, true, true, false},
		{"unknown version", Object{"contractVersion": int64(2), "target": "x86_64-unknown-linux-gnu", "protocols": []string{"anthropic-messages"}}, true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validAgentAdapterCapability(test.capability, test.proxy); got != test.valid {
				t.Fatalf("registration validity=%v", got)
			}
			if got := declaredAgentCapable(Object{"modelProxy": test.proxy, "modelAdapter": test.capability}, configuration); got != test.capable {
				t.Fatalf("dispatch capability=%v", got)
			}
		})
	}
	if !declaredAgentCapable(Object{}, Object{}) {
		t.Fatal("legacy input acquired a declared-adapter requirement")
	}
}
