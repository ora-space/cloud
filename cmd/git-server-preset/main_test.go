package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Usage errors exit 2 with one JSON line and never reach the database.
func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"unknown", "--project", "x"},
		{"check"},
		{"check", "--project", "p", "extra"},
		{"snapshot", "--project", "p", "--bogus"},
		{"-nope", "check", "--project", "p"},
	} {
		var out bytes.Buffer
		if code := run(args, &out); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		var line map[string]any
		if err := json.Unmarshal(out.Bytes(), &line); err != nil || line["ok"] != false || line["error"] == "" {
			t.Errorf("%v: output %q is not one failed JSON result", args, out.String())
		}
	}
}
