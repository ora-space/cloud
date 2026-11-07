package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// Manifest bounds protect the control-plane verifier independently of raw session history size.
const workflowHistoryManifestLimit int64 = 8 << 20

type workflowHistoryManifest struct {
	SchemaVersion       int64                  `json:"schema_version"`
	WorkflowRunID       string                 `json:"workflow_run_id"`
	CheckoutExecutionID string                 `json:"checkout_execution_id"`
	Sessions            []workflowHistoryEntry `json:"sessions"`
}

type workflowHistoryEntry struct {
	ExecutionOrder int64                  `json:"execution_order"`
	NodeID         string                 `json:"node_id"`
	Attempt        int64                  `json:"attempt"`
	ExecutionID    string                 `json:"execution_id"`
	Status         string                 `json:"status"`
	Object         *workflowHistoryObject `json:"object,omitempty"`
}

type workflowHistoryObject struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// verifyWorkflowHistory reads the Node-authored manifest and verifies every raw object outside SQL.
func (s *Store) verifyWorkflowHistory(ctx context.Context, e, history Object) (bool, error) {
	data, err := objectstore.ReadVerified(ctx, s.ObjectStore, history.S("key"), history.N("size"), history.S("sha256"), workflowHistoryManifestLimit)
	if err != nil {
		if errors.Is(err, objectstore.ErrReadInvalid) || errors.Is(err, objectstore.ErrReadTooLarge) {
			return false, nil
		}
		return false, &Fault{Status: 503, Code: "object_store_unavailable", Params: Object{}}
	}
	manifest, ok := decodeWorkflowHistory(data, e.O("input"))
	if !ok {
		return false, nil
	}
	for _, entry := range manifest.Sessions {
		if entry.Status == "not_started" {
			continue
		}
		valid, err := objectstore.Verify(ctx, s.ObjectStore, entry.Object.Key, entry.Object.Size, entry.Object.SHA256)
		if err != nil {
			return false, &Fault{Status: 503, Code: "object_store_unavailable", Params: Object{}}
		}
		if !valid {
			return false, nil
		}
	}
	return true, nil
}

// decodeWorkflowHistory compares the entire inventory, including order and null-object semantics.
func decodeWorkflowHistory(data []byte, input Object) (workflowHistoryManifest, bool) {
	var manifest workflowHistoryManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil {
		return manifest, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return manifest, false
	}
	workflow := input.O("workflow")
	sessions := objectRows(workflow["sessions"])
	if manifest.SchemaVersion != 1 || manifest.WorkflowRunID != workflow.S("workflowRunId") || manifest.CheckoutExecutionID != input.S("checkoutExecutionId") || len(manifest.Sessions) != len(sessions) || len(sessions) == 0 {
		return manifest, false
	}
	seen := map[string]bool{}
	for index, entry := range manifest.Sessions {
		attempt := sessions[index]
		if entry.ExecutionOrder != attempt.N("executionOrder") || entry.NodeID != attempt.S("nodeId") || entry.Attempt != attempt.N("attempt") || entry.ExecutionID != attempt.S("executionId") || seen[entry.ExecutionID] {
			return manifest, false
		}
		seen[entry.ExecutionID] = true
		switch entry.Status {
		case "not_started":
			if entry.Object != nil {
				return manifest, false
			}
		case "archived":
			if entry.Object == nil || entry.Object.Key != attempt.S("historyKey") || !validStoredObject(Object{"size": entry.Object.Size, "sha256": entry.Object.SHA256}) {
				return manifest, false
			}
		default:
			return manifest, false
		}
	}
	return manifest, true
}
