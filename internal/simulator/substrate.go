// Package simulator provides phase-one HTTP execution doubles backed by disk and real Git.
// It is test/development infrastructure, not a production Controller, Node, or Substrate.
package simulator

import (
	"context"
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
	mu           sync.Mutex
	faults       map[string]string
}

func NewSubstrate(root string, repos map[string]string) (*Substrate, error) {
	absolute, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Join(absolute, "effects"), 0o700); e != nil {
		return nil, e
	}
	return &Substrate{Root: absolute, Repositories: repos, faults: map[string]string{}}, nil
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
	b, e := os.ReadFile(path)
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
	temp := path + ".tmp"
	if e := os.WriteFile(temp, b, 0o600); e != nil {
		return e
	}
	f, e := os.OpenFile(temp, os.O_RDWR, 0o600)
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
	return os.Rename(temp, path)
}

func git(ctx context.Context, args ...string) (string, error) {
	args = append([]string{"-c", "core.longpaths=true"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	b, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git command failed: %w: %s", e, b)
	}
	return strings.TrimSpace(string(b)), nil
}
func exists(path string) bool { _, e := os.Stat(path); return e == nil }
func (s *Substrate) perform(ctx context.Context, id string, b core.Object) (core.Object, error) {
	pid, wid := b.S("projectId"), b.S("workspaceId")
	root := filepath.Join(s.Root, "projects", pid)
	bare := filepath.Join(root, "repository.git")
	checkout := filepath.Join(root, "workspaces", wid, "checkout")
	runtime := filepath.Join(root, "workspaces", wid, "runtime")
	out := core.Object{}
	switch b.S("kind") {
	case "storage_ensure":
		if e := os.MkdirAll(root, 0o700); e != nil {
			return nil, e
		}
		out["layoutVersion"] = 1
	case "worktree_ensure":
		if wid == "" {
			return nil, fmt.Errorf("workspace required")
		}
		source, ok := s.Repositories[b.S("repositoryUrl")]
		if !ok {
			return nil, fmt.Errorf("repository must be explicitly mapped for simulator")
		}
		if !exists(bare) {
			if _, e := git(ctx, "clone", "--bare", "--", source, bare); e != nil {
				return nil, e
			}
		}
		ref := b.S("requestedRef")
		if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\r\n\x00") {
			return nil, fmt.Errorf("invalid ref")
		}
		branch := "ora/" + wid
		if !exists(checkout) {
			if e := os.MkdirAll(filepath.Dir(checkout), 0o700); e != nil {
				return nil, e
			}
			commit, e := git(ctx, "--git-dir", bare, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
			if e != nil {
				return nil, e
			}
			if _, e = git(ctx, "--git-dir", bare, "show-ref", "--verify", "refs/heads/"+branch); e == nil {
				_, e = git(ctx, "--git-dir", bare, "worktree", "add", "--", checkout, branch)
			} else {
				_, e = git(ctx, "--git-dir", bare, "worktree", "add", "-b", branch, "--", checkout, commit)
			}
			if e != nil {
				return nil, e
			}
		}
		commit, e := git(ctx, "-C", checkout, "rev-parse", "HEAD")
		if e != nil {
			return nil, e
		}
		if e := os.MkdirAll(runtime, 0o700); e != nil {
			return nil, e
		}
		out["commitId"], out["jobTerminated"] = commit, true
	case "sandbox_ensure":
		if wid == "" || !exists(checkout) {
			return nil, fmt.Errorf("checkout required")
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
	case "worktree_delete":
		if wid == "" {
			return nil, fmt.Errorf("workspace required")
		}
		if exists(checkout) {
			if _, e := git(ctx, "--git-dir", bare, "worktree", "remove", "--force", "--", checkout); e != nil {
				return nil, e
			}
		}
		if _, e := git(ctx, "--git-dir", bare, "show-ref", "--verify", "refs/heads/ora/"+wid); e == nil {
			if _, e = git(ctx, "--git-dir", bare, "branch", "-D", "--", "ora/"+wid); e != nil {
				return nil, e
			}
		}
		if e := os.RemoveAll(filepath.Join(root, "workspaces", wid)); e != nil {
			return nil, e
		}
		out["jobTerminated"], out["removed"] = true, true
	case "storage_delete":
		if e := os.RemoveAll(root); e != nil {
			return nil, e
		}
		out["removed"] = true
	default:
		return nil, fmt.Errorf("unknown effect kind")
	}
	return out, nil
}
