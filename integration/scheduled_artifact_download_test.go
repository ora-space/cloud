package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

// Exercise the binary route through real HTTP and PostgreSQL, including the JSON error boundary.
func TestScheduledArtifactDownloadReturnsVerifiedAttachment(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	agent := seedScheduledTaskAgent(t, f.store.Pool, scene.seed)
	task := seedEnabledScheduledTask(t, f.store, scene.seed, agent, "artifact", 1)
	data := []byte("<html><script>alert(1)</script></html>\x00")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	key := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/0"
	_, err := f.store.Pool.Exec(`INSERT INTO scheduled_run_artifacts
		(tenant_id,issue_run_id,task_id,scheduled_at,artifact_index,object_key,display_name,kind,size_bytes,sha256)
		VALUES($1,$2,$3,now(),0,$4,'reports/检查.html','html',$5,$6)`, scene.tenantID, scene.runID, task, key, len(data), digest)
	must(t, err)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scheduled/"+key || r.Method != "GET" || r.URL.Query().Get("X-Amz-Signature") == "" {
			t.Errorf("unexpected storage request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(data)
	}))
	defer storage.Close()
	f.store.ObjectStore = &objectstore.Config{
		Endpoint: storage.URL, PublicEndpoint: "http://sandbox-only.invalid:9000",
		Region: "us-east-1", Bucket: "scheduled", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret",
	}
	service, err := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(t, err)
	user := f.user
	user.Caller = "gateway-a"
	token, err := f.client.Credentials.Token("user", user)
	must(t, err)
	url := f.cloud.URL + "/api/v1/tenants/" + scene.tenantID + "/issues/" + scene.issueID + "/runs/" + scene.runID + "/artifacts/0"
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, http.NoBody)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+service)
	req.Header.Set("X-Ora-User-Token", token)
	resp, err := f.cloud.Client().Do(req)
	must(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	must(t, err)
	kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	must(t, err)
	if resp.StatusCode != 200 || string(got) != string(data) || kind != "attachment" || params["filename"] != "检查.html" {
		t.Fatalf("download = %d %q %s %v", resp.StatusCode, got, kind, params)
	}
	for name, want := range map[string]string{"Content-Type": "application/octet-stream", "Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff"} {
		if got := resp.Header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	_, err = f.store.Pool.Exec("UPDATE scheduled_run_artifacts SET sha256=repeat('a',64) WHERE issue_run_id=$1", scene.runID)
	must(t, err)
	path := req.URL.Path
	status, out, err := f.threadRequest("GET", path, "", "")
	must(t, err)
	if status != 404 || out.S("code") != "artifact_unavailable" {
		t.Fatalf("invalid bytes = %d %v, want JSON artifact_unavailable", status, out)
	}
	status, out, err = f.threadRequest("GET", path+"?objectKey=other", "", "")
	must(t, err)
	if status != 400 || out.S("code") != "invalid_session_filter" {
		t.Fatalf("caller-selected key = %d %v", status, out)
	}
}
