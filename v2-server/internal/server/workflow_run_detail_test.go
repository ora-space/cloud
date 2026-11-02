package server

import (
	"maps"
	"testing"
)

func TestRunDetailShowsOnlyConditionDecisions(t *testing.T) {
	cases := map[string]map[string]string{
		``:   {},
		`{}`: {},
		`{"conditionDecisions":{"cond":"else","gate":"ok"},"variablePool":{"values":{"secret":"x"}}}`: {"cond": "else", "gate": "ok"},
		`not json`: {},
	}
	for payload, want := range cases {
		if got := conditionDecisions([]byte(payload)); !maps.Equal(got, want) || got == nil {
			t.Errorf("%q: got %v, want %v", payload, got, want)
		}
	}
}
