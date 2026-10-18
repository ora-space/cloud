package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func historyFault(t *testing.T, err error, status int, code string) {
	t.Helper()
	var fault *Fault
	if !errors.As(err, &fault) || fault.Status != status || fault.Code != code || len(fault.Params) != 0 {
		t.Fatalf("fault = %v; want %d %s with no private parameters", err, status, code)
	}
}

func TestSessionHistoryRecordsPreservesOriginalOrderAndNumbers(t *testing.T) {
	lines := []json.RawMessage{
		json.RawMessage(`{"seq":2,"at":"2026-10-15T10:00:00Z","unknown":{"number":9007199254740993}}`),
		json.RawMessage(`{"seq":1,"type":"meta"}`),
		json.RawMessage(`{"seq":2,"update":{"sessionUpdate":"tool_call_update","rawOutput":{"result":"synthetic"}}}`),
	}
	data := []byte(" \t" + string(lines[0]) + "\r\n" + string(lines[1]) + "\n" + string(lines[2]) + "\n")
	got, err := sessionHistoryRecords(t.Context(), data)
	if err != nil || !reflect.DeepEqual(got, lines) {
		t.Fatalf("archive order or raw fields changed: %v", err)
	}
	encoded, err := json.Marshal(Object{"records": got})
	if err != nil || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("large JSON number lost precision: %s %v", encoded, err)
	}
}

func TestSessionHistoryRecordsRejectsInvalidJSONL(t *testing.T) {
	for name, data := range map[string][]byte{
		"array": []byte("[]\n"), "null": []byte("null\n"), "scalar": []byte("3\n"),
		"two objects on one line": []byte("{} {}\n"), "blank line": []byte("{}\n\n"),
		"broken object": []byte("{\n"), "invalid utf8": {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
		"leading nonbreaking space": []byte("\u00a0{}\n"), "trailing unicode space": []byte("{}\u2003\n"),
		"non-JSON ASCII whitespace": []byte("\v{}\f\n"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sessionHistoryRecords(t.Context(), data)
			historyFault(t, err, 502, "history_invalid")
		})
	}
}

func TestSessionHistoryRecordsBoundAndCancellation(t *testing.T) {
	data := []byte(strings.Repeat("{}\n", SessionHistoryRecordLimit))
	records, err := sessionHistoryRecords(t.Context(), data)
	if err != nil || len(records) != SessionHistoryRecordLimit {
		t.Fatalf("exact record limit failed: count=%d error=%v", len(records), err)
	}
	_, err = sessionHistoryRecords(t.Context(), append(data, []byte("{}\n")...))
	historyFault(t, err, 413, "history_too_large")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = sessionHistoryRecords(ctx, []byte("{}\n"))
	historyFault(t, err, 502, "history_unavailable")
}
