package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeDist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":          "<shell>",
		"assets/app-1a.js":    "js",
		"assets/nested/.keep": "",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if e := os.MkdirAll(filepath.Dir(p), 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(body), 0o644); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}

func TestOpenWebRequiresIndex(t *testing.T) {
	if _, e := OpenWeb(filepath.Join(t.TempDir(), "missing")); e == nil {
		t.Fatal("a missing dist_dir must fail at startup")
	}
	if _, e := OpenWeb(t.TempDir()); e == nil {
		t.Fatal("a dist_dir without index.html must fail at startup")
	}
	dir := t.TempDir()
	if e := os.Mkdir(filepath.Join(dir, "index.html"), 0o755); e != nil {
		t.Fatal(e)
	}
	if _, e := OpenWeb(dir); e == nil {
		t.Fatal("an index.html directory must fail at startup")
	}
}

func TestWebServesFilesShellAndLeavesReservedPaths(t *testing.T) {
	dir := writeDist(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if e := os.WriteFile(outside, []byte("secret"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(dir, "escape")); e != nil {
		t.Fatal(e)
	}
	web, e := OpenWeb(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = web.Close() })
	cases := []struct {
		name, method, target string
		served               bool
		body, cacheControl   string
	}{
		{"root is the shell", http.MethodGet, "/", true, "<shell>", "no-cache"},
		{"asset file", http.MethodGet, "/assets/app-1a.js", true, "js", ""},
		{"head of asset", http.MethodHead, "/assets/app-1a.js", true, "", ""},
		{"client route falls back to shell", http.MethodGet, "/w/team/projects", true, "<shell>", "no-cache"},
		{"directory falls back to shell", http.MethodGet, "/assets/nested", true, "<shell>", "no-cache"},
		{"traversal stays inside the root", http.MethodGet, "/../../etc/passwd", true, "<shell>", "no-cache"},
		{"symlink out of the root is never followed", http.MethodGet, "/escape", true, "<shell>", "no-cache"},
		{"unsafe method is not the frontend's", http.MethodPost, "/w/team", false, "", ""},
		{"api miss stays a JSON 404", http.MethodGet, "/api/v2/anything", false, "", ""},
		{"auth miss stays a JSON 404", http.MethodGet, "/auth/unknown", false, "", ""},
		{"internal stays unrouted", http.MethodGet, "/internal/v1/nodes", false, "", ""},
		{"internal root stays unrouted", http.MethodGet, "/internal", false, "", ""},
		{"health is not the frontend's", http.MethodGet, "/healthz", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			served := web.serve(rec, httptest.NewRequest(tc.method, tc.target, nil))
			if served != tc.served {
				t.Fatalf("served = %v, want %v", served, tc.served)
			}
			if !tc.served {
				if rec.Body.Len() != 0 {
					t.Fatalf("an unserved request must write nothing, got %q", rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK || rec.Body.String() != tc.body || rec.Header().Get("Cache-Control") != tc.cacheControl {
				t.Fatalf("got %d %q cache-control %q, want 200 %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"), tc.body, tc.cacheControl)
			}
		})
	}
}
