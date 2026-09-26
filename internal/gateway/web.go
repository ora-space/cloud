package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
)

// WebConfig optionally serves the built frontend (`frontend/dist`) from the Gateway. The browser
// must reach the frontend, /auth and /api on one origin because the session cookie and the Origin
// check are bound to public.base_url, and the Gateway is that origin. Empty disables it.
type WebConfig struct {
	DistDir string `mapstructure:"dist_dir"`
}

// webIndex is the single-page application shell every client-side route falls back to.
const webIndex = "index.html"

// reservedWebPrefixes are the Gateway's own and proxied paths. A miss under them stays a JSON 404 so
// API clients never receive the HTML shell in place of an error.
var reservedWebPrefixes = []string{"/api/", "/auth/", "/internal/"}

// Web serves a built single-page frontend. Files are opened through an os.Root, so neither `..`
// nor a symbolic link inside the directory can reach a file outside it.
type Web struct {
	root *os.Root
}

// OpenWeb opens the built frontend directory and requires its index.html, so a missing or empty
// build fails the Gateway at startup instead of serving 404s.
func OpenWeb(dir string) (*Web, error) {
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, fmt.Errorf("open web dist_dir: %w", e)
	}
	info, e := root.Stat(webIndex)
	if e == nil && !info.Mode().IsRegular() {
		e = errors.New("not a regular file")
	}
	if e != nil {
		return nil, errors.Join(fmt.Errorf("web dist_dir %s: %s: %w", dir, webIndex, e), root.Close())
	}
	return &Web{root: root}, nil
}

// Close releases the directory handle.
func (w *Web) Close() error {
	return w.root.Close()
}

// serve answers a GET or HEAD the Gateway has no route for with the requested file, or with the
// application shell for a client-side route. It reports false when the request is not the
// frontend's, leaving the caller's JSON 404 in place.
func (w *Web) serve(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	clean := path.Clean("/" + r.URL.Path)
	if clean == "/healthz" {
		return false
	}
	for _, prefix := range reservedWebPrefixes {
		if strings.HasPrefix(clean+"/", prefix) {
			return false
		}
	}
	name := strings.TrimPrefix(clean, "/")
	if name == "" || !w.serveFile(rw, r, name, false) {
		// The shell must not be cached: a new build changes the hashed assets it references.
		return w.serveFile(rw, r, webIndex, true)
	}
	return true
}

// serveFile writes one regular file; it reports false when the name is missing or not a file.
func (w *Web) serveFile(rw http.ResponseWriter, r *http.Request, name string, shell bool) bool {
	f, e := w.root.Open(name)
	if e != nil {
		return false
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return false
	}
	if shell {
		rw.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(rw, r, info.Name(), info.ModTime(), f)
	return true
}
