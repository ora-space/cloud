package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontendBoundary(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "<html>app</html>", "app.js": "javascript"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) })
	h, err := WithFrontend(api, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, accept string
		status               int
		body                 string
	}{
		{"GET", "/", "", 200, "<html>app</html>"}, {"GET", "/settings/plugins", "text/html", 200, "<html>app</html>"},
		{"HEAD", "/settings/plugins", "text/html", 200, ""}, {"GET", "/app.js", "", 200, "javascript"},
		{"GET", "/missing.js", "text/html", 404, ""}, {"GET", "/missing", "application/json", 404, ""},
		{"GET", "/directory", "text/html", 404, ""}, {"GET", "/leak", "text/html", 404, ""},
		{"GET", "/../secret", "text/html", 404, ""}, {"GET", "/api/missing", "text/html", 418, ""},
		{"GET", "/auth/missing", "text/html", 418, ""}, {"GET", "/internal/missing", "text/html", 418, ""},
		{"POST", "/settings", "text/html", 405, ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Accept", tc.accept)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status == 200 && w.Body.String() != tc.body {
				t.Fatalf("body=%q", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("secret leaked")
			}
			if tc.status == 200 && w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatal("cache policy missing")
			}
		})
	}
	if _, err := WithFrontend(api, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := WithFrontend(api, dir); err == nil {
		t.Fatal("missing index accepted")
	}
}

func TestFrontendPreservesRealAPIRouting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("SPA"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := WithFrontend(New(nil, Options{}), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/not-real", 404}, {"GET", "/auth/not-real", 404}, {"GET", "/internal/not-real", 404},
		{"GET", "/_private/not-real", 404}, {"GET", "/git/not-real", 404}, {"GET", "/runtime/not-real", 404}, {"GET", "/repo.git/not-real", 404}, {"GET", "/auth/login", 405},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Body.String() == "SPA" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
