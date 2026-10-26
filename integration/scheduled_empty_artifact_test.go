package integration

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

func TestScheduledEmptyArtifactUploadsAndRegisters(t *testing.T) {
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	f := setup(t)
	scene, execution := scheduledArtifactSceneForManifest(t, f, &controlpb.ScheduledArtifactManifest{
		Items: []*controlpb.ScheduledArtifactItem{{Index: 0, DisplayName: "empty.log", Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_LOG, SizeBytes: 0, Sha256: digest}},
	})
	if f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, scene.executionID) != 1 {
		t.Fatal("an empty file must not roll back the session terminal receipt")
	}
	checksum, err := hex.DecodeString(digest)
	must(t, err)
	encoded := base64.StdEncoding.EncodeToString(checksum)
	var uploaded atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != 0 || r.Header.Get("X-Amz-Checksum-Sha256") != encoded {
				t.Error("empty PUT must contain zero bytes and the signed empty-content checksum")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			uploaded.Store(true)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if !uploaded.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", "0")
			w.Header().Set("X-Amz-Checksum-Sha256", encoded)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	f.store.ObjectStore = &objectstore.Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "artifacts", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret"}
	granted, err := controlpb.NewAgentRunServiceClient(f.controlConn).GrantScheduledArtifactUpload(asController("ctrl-a"), &controlpb.GrantScheduledArtifactUploadRequest{
		Epoch: 1, ExecutionId: execution, Artifacts: []*controlpb.ScheduledArtifactUpload{{Index: 0, Size: 0, Sha256: digest}},
	})
	must(t, err)
	grant := granted.Grants[0]
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, grant.GetUrl(), http.NoBody)
	must(t, err)
	for key, value := range grant.GetHeaders() {
		req.Header.Set(key, value)
	}
	resp, err := server.Client().Do(req)
	must(t, err)
	must(t, resp.Body.Close())
	if resp.StatusCode != http.StatusOK {
		t.Fatal("empty artifact PUT failed")
	}
	result := &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_ScheduledArtifactsDelivered{ScheduledArtifactsDelivered: &controlpb.ScheduledArtifactsDelivered{
			Artifacts: []*controlpb.StoredObject{{Key: grant.GetObjectKey(), Size: 0, Sha256: digest}},
		}},
	}
	_, err = f.deliveryTerminal(scene, execution, "empty-upload", 1, result, "empty-upload")
	must(t, err)
	if f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1 AND size_bytes=0 AND sha256=$2`, scene.runID, digest) != 1 {
		t.Fatal("verified empty artifact must be registered")
	}
	f.store.ObjectStore = f.objects.config
	finishScheduledRevision(t, f, scene)
	if f.scalar(`SELECT count(*) FROM issue_runs WHERE id=$1 AND phase='releasing'`, scene.runID) != 1 {
		t.Fatal("empty artifact must not prevent Revision delivery and Workspace release")
	}
}
