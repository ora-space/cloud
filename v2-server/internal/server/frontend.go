package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
)

// WithFrontend serves a build directory on the API origin. An empty directory preserves API-only
// operation. os.Root confines every open, including symlink resolution, to the configured tree.
func WithFrontend(api http.Handler, directory string) (http.Handler, error) {
	if directory == "" {
		return api, nil
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("frontend directory: %w", err)
	}
	file, err := root.Open("index.html")
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("index.html must be regular")
		}
		file.Close()
	}
	root.Close()
	if err != nil {
		return nil, fmt.Errorf("frontend index: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		switch first {
		case "api", "auth", "internal", "_internal", "healthz", "git", "node", "model", "packages", "runtime", "repo.git":
			api.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(first, "_") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if strings.Contains(name, "\\") || path.Clean("/"+name) != "/"+name && name != "" {
			http.NotFound(w, r)
			return
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer root.Close()
		if name == "" {
			name = "index.html"
		}
		file, err := root.Open(name)
		// Only extensionless browser navigations use the SPA. Failed containment checks and missing
		// assets never become a successful HTML response.
		if errors.Is(err, os.ErrNotExist) && path.Ext(name) == "" && strings.Contains(r.Header.Get("Accept"), "text/html") {
			name = "index.html"
			file, err = root.Open(name)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		// Revalidate all files, including HTML, across upgrades; avoid stale entrypoints and cached
		// responses accidentally surviving a package replacement.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, info.ModTime(), file)
	}), nil
}
