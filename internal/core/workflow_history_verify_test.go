package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// The production HTTP verifier must inspect every object, not only a successful manifest HEAD.
func TestWorkflowHistoryVerificationReadsManifestAndEveryRawArchive(t *testing.T) {
	digest := func(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
	first, second := []byte("failed attempt\r\n"), []byte("successful attempt\n")
	manifest := workflowHistoryManifest{SchemaVersion: 1, WorkflowRunID: "run", CheckoutExecutionID: "checkout", Sessions: []workflowHistoryEntry{
		{ExecutionOrder: 0, NodeID: "a", Attempt: 1, ExecutionID: "first", Status: "archived", Object: &workflowHistoryObject{Key: "run/first", Size: int64(len(first)), SHA256: digest(first)}},
		{ExecutionOrder: 1, NodeID: "a", Attempt: 2, ExecutionID: "retry", Status: "archived", Object: &workflowHistoryObject{Key: "run/retry", Size: int64(len(second)), SHA256: digest(second)}},
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	input := Object{"checkoutExecutionId": "checkout", "workflow": Object{"workflowRunId": "run", "sessions": []Object{
		{"executionOrder": int64(0), "nodeId": "a", "attempt": int64(1), "executionId": "first", "historyKey": "run/first"},
		{"executionOrder": int64(1), "nodeId": "a", "attempt": int64(2), "executionId": "retry", "historyKey": "run/retry"},
	}}}
	for _, test := range []struct {
		name, missing, corrupt string
		unavailable            bool
		want                   bool
	}{
		{name: "complete", want: true},
		{name: "first missing", missing: "run/first"},
		{name: "last missing", missing: "run/retry"},
		{name: "wrong bytes", corrupt: "run/retry"},
		{name: "temporarily unavailable", unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := map[string][]byte{"run/manifest": data, "run/first": first, "run/retry": second}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := strings.TrimPrefix(r.URL.Path, "/revisions/")
				if test.unavailable {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				bytes, ok := objects[key]
				if !ok || key == test.missing {
					http.NotFound(w, r)
					return
				}
				if key == test.corrupt {
					bytes = []byte("corrupt")
				}
				hash := sha256.Sum256(bytes)
				w.Header().Set("Content-Length", fmt.Sprint(len(bytes)))
				w.Header().Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(hash[:]))
				if r.Method == http.MethodGet {
					_, _ = w.Write(bytes)
				}
			}))
			defer server.Close()
			s := &Store{ObjectStore: &objectstore.Config{Endpoint: server.URL, Region: "test", Bucket: "revisions", PathStyle: true, AccessKeyID: "test", SecretAccessKey: "fixture", UploadGrantTTL: time.Minute}}
			valid, err := s.verifyWorkflowHistory(t.Context(), Object{"input": input}, Object{"key": "run/manifest", "size": int64(len(data)), "sha256": digest(data)})
			if valid != test.want || (test.unavailable && ErrorCode(err).Code != "object_store_unavailable") || (!test.unavailable && err != nil) {
				t.Fatalf("valid=%v err=%v", valid, err)
			}
		})
	}
}
