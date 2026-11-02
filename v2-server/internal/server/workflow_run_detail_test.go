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

func TestRunDetailSeparatesIterationDecisions(t *testing.T) {
	payload := []byte(`{"conditionDecisions":{"gate":"outer"},"iterationConditionDecisions":{"gate#0":"yes","gate#1":"else"},"variablePool":{"values":{"private":"value"}}}`)
	if got := payloadDecisions(payload, "iterationConditionDecisions"); !maps.Equal(got, map[string]string{"gate#0": "yes", "gate#1": "else"}) {
		t.Fatal(got)
	}
	if got := conditionDecisions(payload); !maps.Equal(got, map[string]string{"gate": "outer"}) {
		t.Fatal(got)
	}
	for _, raw := range []string{"", "{}", `{"iterationConditionDecisions":null}`, `{"iterationConditionDecisions":{"bad":1}}`} {
		if got := payloadDecisions([]byte(raw), "iterationConditionDecisions"); got == nil || len(got) != 0 {
			t.Fatal(got)
		}
	}
}
