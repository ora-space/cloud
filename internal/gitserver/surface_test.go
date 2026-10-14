package gitserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These are the guarantees that make the two surfaces one service rather than two: a process serves
// exactly the surfaces it was given, and no surface's route is answered by another's handler.
// Everything else about each surface is tested on its own — `server_test.go` for the mirror read,
// the harness in `service_test.go` for the repository boundary — because one process must not
// become one trust boundary.

// TestAServiceWithNoSurfaceIsRefused pins the startup refusal. A process wired with neither a
// repository surface nor a mirror surface has no route to serve, and coming up as a server that
// answers "forbidden" to everything would be indistinguishable from a working one refusing: an
// operator would see a healthy process and no traffic, and nothing would say which of the two
// deployments they actually had.
func TestAServiceWithNoSurfaceIsRefused(t *testing.T) {
	service, err := New(&Options{GitProgram: "git", Health: func(context.Context) error { return nil }})
	if err == nil {
		_ = service.Close()
		t.Fatal("a service with no surface started")
	}
	if !strings.Contains(err.Error(), "at least one surface") {
		t.Fatalf("New with no surface = %v, want a refusal naming the missing surfaces", err)
	}
}

// TestEachServiceAnswersOnlyItsOwnSurface is the route-disjointness guarantee: every route, on every
// handler, on a service that serves one surface.
//
// It is what makes the two listeners safe to place on two networks. The mirror listener is reachable
// by anything on the sandbox segment, and if its handler answered a repository route — or the ref
// query, or a write endpoint — then the address a caller can reach would decide which trust model
// applied instead of the listener it arrived on. The statuses are stated exactly rather than as
// "not 404" because a surface answering its own route must still refuse the caller it was not given.
func TestEachServiceAnswersOnlyItsOwnSurface(t *testing.T) {
	digest := strings.Repeat("a", 64)
	// The repository surface spells one Project one way, so these are the canonical UUIDs a path may
	// carry. A path that is not canonical is not a repository route at all — which is a 404 on every
	// handler and not evidence about which surface owns what.
	const tenant, project = "0b5a3a5e-3f4b-4a7c-9c1e-7b1d2f3a4c5d", "9d8c7b6a-5e4f-4d3c-8b2a-1f0e9d8c7b6a"
	// owner is the handler the route belongs to, or empty when no surface mounts it at all.
	routes := []struct {
		name, owner, method, path string
	}{
		{"the mirror advertisement", "mirror", http.MethodGet, "/mirror/v1/" + digest + ".git/info/refs?service=git-upload-pack"},
		{"the mirror fetch", "mirror", http.MethodPost, "/mirror/v1/" + digest + ".git/git-upload-pack"},
		// The write endpoint of the smart HTTP protocol is mounted by no surface, so a Node cannot
		// reach receive-pack through the listener it is allowed to use.
		{"a mirror write endpoint", "", http.MethodPost, "/mirror/v1/" + digest + ".git/git-receive-pack"},
		{"the repository advertisement", "repository", http.MethodGet, "/" + tenant + "/" + project + ".git/info/refs?service=git-upload-pack"},
		{"a repository write endpoint", "repository", http.MethodPost, "/" + tenant + "/" + project + ".git/git-receive-pack"},
		{"the ref query", "ref", http.MethodGet, "/internal/v1/refs?tenantId=" + tenant + "&projectId=" + project + "&ref=refs/ora/deliveries/w"},
	}
	cases := []struct {
		name    string
		service func(*testing.T) *Service
		// serves names the surfaces this process was wired with; owns is the status the handler of a
		// route this process serves answers with, given a probe that presents no certificate and no
		// identity of any kind.
		serves map[string]bool
		owns   map[string]int
	}{
		{
			name: "mirror only",
			service: func(t *testing.T) *Service {
				return newService(t, &Options{
					GitProgram: "git",
					Mirror:     &MirrorOptions{Mirrors: newMirrorStore(t), Origins: &stubResolver{}, State: &stubState{}},
					Health:     func(context.Context) error { return nil },
				})
			},
			serves: map[string]bool{"mirror": true},
			// The stub resolver refuses every address, which is this surface deciding for itself: it
			// reached its own answer rather than finding no such route.
			owns: map[string]int{"mirror": http.StatusForbidden},
		},
		{
			name: "repository only",
			service: func(t *testing.T) *Service {
				return newService(t, &Options{
					GitProgram: "git",
					Repository: &RepositoryOptions{Store: &fakeStore{}, RepositoryRoot: t.TempDir(), CloudClient: []byte{1}, Hook: []string{"true"}},
					Health:     func(context.Context) error { return nil },
				})
			},
			serves: map[string]bool{"repository": true, "ref": true},
			// A certificate is required before anything else on these routes and the probe presents
			// none, so the surface is answering and it is the transport that says no.
			owns: map[string]int{"repository": http.StatusUnauthorized, "ref": http.StatusUnauthorized},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service := c.service(t)
			t.Cleanup(func() { _ = service.Close() })
			handlerFor := map[string]http.HandlerFunc{
				"repository": service.RepositoryHandler,
				"ref":        service.RefHandler,
				"mirror":     service.MirrorHandler,
			}
			for _, route := range routes {
				for name, handler := range handlerFor {
					want := http.StatusNotFound
					if name == route.owner && c.serves[name] {
						want = c.owns[name]
					}
					if got := status(t, handler, route.method, route.path); got != want {
						t.Fatalf("%s: %s %s on the %s handler = %d, want %d", route.name, route.method, route.path, name, got, want)
					}
				}
			}
		})
	}
}

// newService builds a service or fails the test. Every case above is about what a service does once
// it exists, not about how it was wired, so a wiring failure is the test's failure and not a case's.
func newService(t *testing.T, options *Options) *Service {
	t.Helper()
	service, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

// status sends one request through one handler and reports the status it answered with.
func status(t *testing.T, handler http.HandlerFunc, method, path string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(method, path, nil))
	return recorder.Code
}

// TestLoadConfigValidatesOnlyTheSurfaceThatWillStart pins what turning the repository surface off
// means for configuration: it is not a relaxation of anything, and it does not make the process
// refuse over references nothing consults. A mirror-only deployment has no repository root and no
// repository listen address, and it must start.
func TestLoadConfigValidatesOnlyTheSurfaceThatWillStart(t *testing.T) {
	t.Setenv("GIT_SERVER_REPOSITORY_ADDR", "not-an-address")
	t.Setenv("GIT_SERVER_REPOSITORY_ROOT", "relative/path")
	t.Setenv("GIT_SERVER_REPOSITORY_ENABLED", "true")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("an unusable repository reference was accepted while the surface was enabled")
	}

	t.Setenv("GIT_SERVER_REPOSITORY_ENABLED", "false")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("a mirror-only deployment was refused over references it does not read: %v", err)
	}
	if config.RepositoryEnabled {
		t.Fatal("the repository surface was reported enabled after it was turned off")
	}

	// Unset means on. The fail-closed direction for a deployment that did not mention the surface is
	// to demand its material at startup, not to stop serving it without saying so.
	t.Setenv("GIT_SERVER_REPOSITORY_ENABLED", "")
	t.Setenv("GIT_SERVER_REPOSITORY_ADDR", ":8445")
	t.Setenv("GIT_SERVER_REPOSITORY_ROOT", t.TempDir())
	config, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !config.RepositoryEnabled {
		t.Fatal("the repository surface defaulted off")
	}
}
