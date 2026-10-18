package core

import "testing"

// TestPostgresUnsupportedJSON pins the raw-byte rules that keep one document from aborting the
// result transaction. Go's decoder accepts most of these inputs — it even replaces unpaired
// surrogate escapes — so the document's raw bytes are the only place the distinction survives.
func TestPostgresUnsupportedJSON(t *testing.T) {
	for _, tc := range []struct {
		name        string
		content     string
		reason      string
		unsupported bool
	}{
		{name: "plain text", content: `{"a":"plain"}`},
		{name: "escaped backslash", content: "{\"a\":\"\\\\u0000\"}"},
		{name: "nul escape", content: "{\"a\":\"\\u0000\"}", reason: "unsupported_unicode_escape", unsupported: true},
		{name: "lone low surrogate", content: "{\"a\":\"\\udc00\"}", reason: "unsupported_unicode_escape", unsupported: true},
		{name: "lone high surrogate", content: "{\"a\":\"\\ud800\"}", reason: "unsupported_unicode_escape", unsupported: true},
		{name: "valid surrogate pair", content: "{\"a\":\"\\ud83d\\ude00\"}"},
		{name: "invalid utf-8", content: "{\"a\":\"" + string([]byte{0xff}) + "\"}", reason: "invalid_utf8", unsupported: true},
		{name: "smallest representable scale", content: `{"a":1e-16383}`},
		{name: "scale beyond numeric", content: `{"a":1e-16384}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "scale beyond numeric with zero", content: `{"a":0e-20000}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "fraction digits count toward scale", content: `{"a":1.0e-16383}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "trailing zero counts toward scale", content: `{"a":10e-16384}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "trailing zero within scale", content: `{"a":100e-16383}`},
		{name: "largest representable magnitude", content: `{"a":1e131071}`},
		{name: "magnitude beyond numeric", content: `{"a":1e131072}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "leading fraction zeros do not consume magnitude", content: `{"a":0.0001e131075}`},
		{name: "leading fraction zeros with too large magnitude", content: `{"a":0.0001e131076}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "fraction digit shifts magnitude", content: `{"a":0.5e131072}`},
		{name: "fraction digit shifts magnitude too far", content: `{"a":0.5e131073}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "negative number", content: `{"a":-1e-20000}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "largest representable exponent", content: `{"a":0e1073741823}`},
		{name: "largest representable exponent with a sign", content: `{"a":0e+1073741823}`},
		{name: "exponent beyond numeric", content: `{"a":0e1073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "exponent beyond numeric with a sign", content: `{"a":0e+1073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "leading zeros do not relax the exponent bound", content: `{"a":0e01073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "long leading zeros do not relax the exponent bound", content: `{"a":0e` + "000000000000000000000000000000" + `1073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "exponent beyond numeric with a fraction", content: `{"a":0.0e1073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "very long exponent", content: `{"a":0e99999999999999999999}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "negative exponent beyond scale", content: `{"a":0e-1073741824}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "number inside a string", content: `{"a":"1e-20000"}`},
		{name: "nested number", content: `{"a":[{"b":1e-20000}]}`, reason: "numeric_out_of_range", unsupported: true},
		{name: "number before an escaped string", content: "{\"a\":1,\"b\":\"\\u0000\"}", reason: "unsupported_unicode_escape", unsupported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, unsupported := postgresUnsupportedJSON([]byte(tc.content))
			if unsupported != tc.unsupported || reason != tc.reason {
				t.Fatalf("postgresUnsupportedJSON(%q) = %q, %v; want %q, %v",
					tc.content, reason, unsupported, tc.reason, tc.unsupported)
			}
		})
	}
}
