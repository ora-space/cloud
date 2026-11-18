package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// A manifest must cover the exact fixed inventory, including failed and unstarted attempts.
func TestWorkflowHistoryRejectsIncompleteOrContradictoryInventories(t *testing.T) {
	input := Object{"checkoutExecutionId": "checkout", "workflow": Object{"workflowRunId": "run", "sessions": []Object{
		{"executionOrder": int64(0), "nodeId": "a", "attempt": int64(1), "executionId": "failed", "historyKey": "run/failed"},
		{"executionOrder": int64(1), "nodeId": "a", "attempt": int64(2), "executionId": "retry", "historyKey": "run/retry"},
		{"executionOrder": int64(2), "nodeId": "b", "attempt": int64(1), "executionId": "unstarted", "historyKey": "run/unstarted"},
	}}}
	manifest := workflowHistoryManifest{SchemaVersion: 1, WorkflowRunID: "run", CheckoutExecutionID: "checkout", Sessions: []workflowHistoryEntry{
		{ExecutionOrder: 0, NodeID: "a", Attempt: 1, ExecutionID: "failed", Status: "archived", Object: &workflowHistoryObject{Key: "run/failed", Size: 3, SHA256: strings.Repeat("a", 64)}},
		{ExecutionOrder: 1, NodeID: "a", Attempt: 2, ExecutionID: "retry", Status: "archived", Object: &workflowHistoryObject{Key: "run/retry", Size: 4, SHA256: strings.Repeat("b", 64)}},
		{ExecutionOrder: 2, NodeID: "b", Attempt: 1, ExecutionID: "unstarted", Status: "not_started"},
	}}
	encode := func(m workflowHistoryManifest) []byte {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, ok := decodeWorkflowHistory(encode(manifest), input); !ok {
		t.Fatal("valid complete manifest rejected")
	}
	for _, test := range []struct {
		name   string
		mutate func(*workflowHistoryManifest)
	}{
		{"omitted failed attempt", func(m *workflowHistoryManifest) { m.Sessions = m.Sessions[1:] }},
		{"duplicate identity", func(m *workflowHistoryManifest) { m.Sessions[1].ExecutionID = m.Sessions[0].ExecutionID }},
		{"wrong checkout", func(m *workflowHistoryManifest) { m.CheckoutExecutionID = "other" }},
		{"wrong run", func(m *workflowHistoryManifest) { m.WorkflowRunID = "other" }},
		{"unknown schema", func(m *workflowHistoryManifest) { m.SchemaVersion = 2 }},
		{"unavailable history", func(m *workflowHistoryManifest) { m.Sessions[0].Status = "unavailable" }},
		{"objectless established history", func(m *workflowHistoryManifest) { m.Sessions[0].Object = nil }},
		{"fabricated unstarted object", func(m *workflowHistoryManifest) { m.Sessions[2].Object = m.Sessions[0].Object }},
		{"wrong digest", func(m *workflowHistoryManifest) { m.Sessions[0].Object.SHA256 = "bad" }},
		{"negative size", func(m *workflowHistoryManifest) { m.Sessions[0].Object.Size = -1 }},
		{"cross-run object", func(m *workflowHistoryManifest) { m.Sessions[0].Object.Key = "other/object" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var copy workflowHistoryManifest
			if err := json.Unmarshal(encode(manifest), &copy); err != nil {
				t.Fatal(err)
			}
			test.mutate(&copy)
			if _, ok := decodeWorkflowHistory(encode(copy), input); ok {
				t.Fatal("contradictory manifest accepted")
			}
		})
	}
	for _, data := range [][]byte{append(encode(manifest), []byte(" {}")...), []byte(strings.Replace(string(encode(manifest)), `"schema_version":1`, `"schema_version":1,"unknown":true`, 1))} {
		if _, ok := decodeWorkflowHistory(data, input); ok {
			t.Fatal("extra fields or JSON accepted")
		}
	}
}
