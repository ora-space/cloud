// Package simulator provides phase-one HTTP execution doubles backed by disk and real Git.
// It is test/development infrastructure, not a production Controller, Node, or Substrate.
package simulator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// Substrate persists stable effect journals and project data outside cloud's database.
type Substrate struct {
	Root         string
	Repositories map[string]string
	// Artifacts maps plugin release URLs to local fixture files so the
	// simulated Node "downloads" real bytes and verifies real digests without
	// network access. An unmapped URL fails the install, mirroring production.
	Artifacts map[string]string
	mu        sync.Mutex
	faults    map[string]string
}

func NewSubstrate(root string, repos map[string]string) (*Substrate, error) {
	absolute, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	for _, journal := range []string{"effects", "clones"} {
		if e := os.MkdirAll(filepath.Join(absolute, journal), 0o700); e != nil {
			return nil, e
		}
	}
	return &Substrate{Root: absolute, Repositories: repos, Artifacts: map[string]string{}, faults: map[string]string{}}, nil
}

// MapArtifact binds a plugin release URL to a local fixture file, mirroring
// the repository mapping the simulated Node's clone uses.
func (s *Substrate) MapArtifact(url, localPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Artifacts[url] = localPath
}

// SetFault injects explicit simulator failures; never used by cloud core.
func (s *Substrate) SetFault(kind, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[kind] = mode
}

func (s *Substrate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/clones/") {
		s.serveClone(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/effects/")
	if _, e := uuid.Parse(id); e != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	file := filepath.Join(s.Root, "effects", id+".json")
	old, err := readObject(file)
	if r.Method == "GET" {
		if errors.Is(err, os.ErrNotExist) {
			w.WriteHeader(404)
			return
		}
		if err != nil {
			http.Error(w, "journal failure", 500)
			return
		}
		writeJSON(w, old)
		return
	}
	if r.Method != "PUT" {
		w.WriteHeader(405)
		return
	}
	body := core.Object{}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	for _, key := range []string{"projectId"} {
		if _, e := uuid.Parse(body.S(key)); e != nil {
			w.WriteHeader(400)
			return
		}
	}
	if body.S("workspaceId") != "" {
		if _, e := uuid.Parse(body.S("workspaceId")); e != nil {
			w.WriteHeader(400)
			return
		}
	}
	if old != nil {
		if jsonString(old.O("request")) != jsonString(body) {
			w.WriteHeader(409)
			return
		}
		if old.S("state") == "succeeded" {
			writeJSON(w, old)
			return
		}
	}
	effect := core.Object{"id": id, "externalId": "sim-" + id, "state": "running", "request": body, "result": core.Object{}}
	if err = writeObject(file, effect); err != nil {
		http.Error(w, "journal failure", 500)
		return
	}
	mode := s.faults[body.S("kind")]
	if mode == "fail" || mode == "unconfirmed" {
		w.WriteHeader(503)
		writeJSON(w, effect)
		return
	}
	result, err := s.perform(r.Context(), id, body)
	if err != nil {
		effect["state"] = "failed"
		effect["error"] = "external_failure"
		effect["diagnostic"] = err.Error()
		if err = writeObject(file, effect); err != nil {
			http.Error(w, "journal failure", 500)
			return
		}
		w.WriteHeader(503)
		writeJSON(w, effect)
		return
	}
	effect["result"] = result
	effect["state"] = "succeeded"
	if err = writeObject(file, effect); err != nil {
		http.Error(w, "journal failure", 500)
		return
	}
	if mode == "lose_response" {
		w.WriteHeader(504)
		return
	}
	writeJSON(w, effect)
}
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func jsonString(v any) string                { b, _ := json.Marshal(v); return string(b) }
func readObject(path string) (core.Object, error) {
	b, e := os.ReadFile(filepath.Clean(path)) // #nosec G304,G703 -- simulator helper reading from isolated test root.
	if e != nil {
		return nil, e
	}
	var o core.Object
	e = json.Unmarshal(b, &o)
	return o, e
}

func writeObject(path string, o core.Object) error {
	b, e := json.Marshal(o)
	if e != nil {
		return e
	}
	temp := filepath.Clean(path + ".tmp")
	if e := os.WriteFile(temp, b, 0o600); e != nil { // #nosec G304,G703 -- simulator helper writing to isolated test root.
		return e
	}
	f, e := os.OpenFile(temp, os.O_RDWR, 0o600) // #nosec G304,G703 -- simulator helper writing to isolated test root.
	if e != nil {
		return e
	}
	e = f.Sync()
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(temp, filepath.Clean(path))
}

func git(ctx context.Context, args ...string) (string, error) {
	args = append([]string{"-c", "core.longpaths=true"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204,G702 -- simulator helper executing git fixture operations.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	b, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git command failed: %w: %s", e, b)
	}
	return strings.TrimSpace(string(b)), nil
}
func exists(path string) bool { _, e := os.Stat(path); return e == nil }

// WorkspaceData is one Workspace's persistent data, the stand-in for its data volume: it outlives
// every sandbox of the Workspace and is removed only by workspace_data_delete. The simulated Node's
// home, its clone and installed plugins all live in it.
func (s *Substrate) WorkspaceData(workspaceID string) string {
	return filepath.Join(s.Root, "workspaces", workspaceID)
}

func (s *Substrate) perform(ctx context.Context, id string, b core.Object) (core.Object, error) {
	wid := b.S("workspaceId")
	if _, e := uuid.Parse(wid); e != nil {
		return nil, fmt.Errorf("workspace required")
	}
	data := s.WorkspaceData(wid)
	out := core.Object{}
	switch b.S("kind") {
	case "sandbox_ensure":
		if e := os.MkdirAll(filepath.Join(data, "home"), 0o700); e != nil {
			return nil, e
		}
		file := filepath.Join(s.Root, "effects", id+".sandbox.json")
		sandbox, e := readObject(file)
		if errors.Is(e, os.ErrNotExist) {
			sandbox = core.Object{"id": id, "workspaceId": wid, "nodeId": uuid.NewString(), "terminated": false}
			e = writeObject(file, sandbox)
		}
		if e != nil {
			return nil, e
		}
		if sandbox.B("terminated") {
			return nil, fmt.Errorf("sandbox terminated")
		}
		out["sandboxInstanceId"], out["nodeId"] = id, sandbox.S("nodeId")
	case "sandbox_terminate":
		sid := b.S("sandboxInstanceId")
		if _, e := uuid.Parse(sid); e != nil {
			return nil, e
		}
		file := filepath.Join(s.Root, "effects", sid+".sandbox.json")
		sandbox, e := readObject(file)
		if e != nil {
			return nil, e
		}
		if sandbox.S("workspaceId") != wid {
			return nil, fmt.Errorf("sandbox scope mismatch")
		}
		sandbox["terminated"] = true
		if e := writeObject(file, sandbox); e != nil {
			return nil, e
		}
		out["terminated"] = true
	case "workspace_data_delete":
		if e := os.RemoveAll(data); e != nil {
			return nil, e
		}
		out["removed"] = true
	case "plugin_ensure":
		// Simulated Node install: resolve the release bytes locally, verify the
		// mandatory SHA-256, and atomically place the archive under the
		// desktop layout plugins/installed/<ns>/<name>/<version>/. Real Node
		// wiring (target selection, zip extraction, Deno runtime) is a later
		// phase; the digest check is real and never optional.
		artifactURL, expected := s.selectArtifact(b)
		if artifactURL == "" {
			return nil, fmt.Errorf("no release artifact in effect payload")
		}
		local, ok := s.Artifacts[artifactURL]
		if !ok {
			return nil, fmt.Errorf("artifact must be explicitly mapped for simulator")
		}
		artifact, e := os.ReadFile(local)
		if e != nil {
			return nil, e
		}
		sum := sha256.Sum256(artifact)
		if hex.EncodeToString(sum[:]) != strings.ToLower(expected) {
			return nil, fmt.Errorf("sha256 mismatch")
		}
		parts := strings.SplitN(b.S("pluginId"), "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid plugin id")
		}
		installDir := filepath.Join(data, "home", "plugins", "installed", parts[0], parts[1], b.S("version"))
		if e := os.MkdirAll(installDir, 0o700); e != nil {
			return nil, e
		}
		if e := writeObject(filepath.Join(installDir, "artifact.json"), core.Object{"url": artifactURL, "sha256": expected, "bytes": len(artifact)}); e != nil {
			return nil, e
		}
		out["installed"] = true
		out["version"] = b.S("version")
	case "plugin_delete":
		parts := strings.SplitN(b.S("pluginId"), "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid plugin id")
		}
		// Uninstall is idempotent: an absent install dir still reports removed.
		if e := os.RemoveAll(filepath.Join(data, "home", "plugins", "installed", parts[0], parts[1])); e != nil {
			return nil, e
		}
		out["removed"] = true
	default:
		return nil, fmt.Errorf("unknown effect kind")
	}
	return out, nil
}

// selectArtifact picks the release the simulated Node installs: a universal
// release when the payload carries one, otherwise the first targeted artifact.
// The real Node selects by its own host target; the simulator has no host
// profile, so the first target stands in and the selection policy stays on the
// execution plane.
func (s *Substrate) selectArtifact(b core.Object) (url, sha string) {
	if u := b.O("universal"); u.S("url") != "" {
		return u.S("url"), u.S("sha256")
	}
	if targets, e := objects(b, "targets"); e == nil && len(targets) > 0 {
		return targets[0].S("url"), targets[0].S("sha256")
	}
	return "", ""
}
