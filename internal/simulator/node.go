package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// serveClone stands in for a desktop Node executing one clone the Controller dispatched:
// PUT /clones/{executionId} runs it inside the Workspace's data, GET reads the recorded outcome.
// Outcomes are journaled per execution, so a Controller that lost the reply queries instead of
// cloning twice. The caller holds s.mu.
func (s *Substrate) serveClone(w http.ResponseWriter, r *http.Request) {
	execution := strings.TrimPrefix(r.URL.Path, "/clones/")
	if _, e := uuid.Parse(execution); e != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	file := filepath.Join(s.Root, "clones", execution+".json")
	old, err := readObject(file)
	switch {
	case err == nil:
		writeJSON(w, old)
		return
	case !errors.Is(err, os.ErrNotExist):
		http.Error(w, "journal failure", 500)
		return
	case r.Method == "GET":
		w.WriteHeader(404)
		return
	case r.Method != "PUT":
		w.WriteHeader(405)
		return
	}
	body := core.Object{}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	outcome := s.clone(r.Context(), body)
	if e := writeObject(file, outcome); e != nil {
		http.Error(w, "journal failure", 500)
		return
	}
	writeJSON(w, outcome)
}

// clone stands in for the Node's clone from a Git Server snapshot: it fetches the snapshot ref from
// the Git Server double into a new checkout without any remote, verifies the fetched commit is the
// input's base commit and checks it out on the Workspace branch's short name. It reports the commit,
// or a failure reason in the contract's enum names. The external repository is never involved.
func (s *Substrate) clone(ctx context.Context, b core.Object) core.Object {
	failed := func(reason string) core.Object {
		return core.Object{"outcome": "clone_failed", "reason": "CLONE_FAILURE_REASON_" + reason}
	}
	if s.faults["clone"] == "fail" {
		return failed("OPERATION_FAILED")
	}
	wid := b.S("workspaceId")
	if s.Git == nil {
		return failed("SOURCE_UNAVAILABLE")
	}
	source, ok := s.Git.Repository(b.S("repositoryUrl"))
	branch, branchOK := core.ParseGitRef(b.S("workspaceRef"))
	if _, e := uuid.Parse(wid); e != nil || !ok || !branchOK || branch.Namespace != core.GitWorkspaceBranch || branch.Owner != wid {
		return failed("OPERATION_FAILED")
	}
	checkout := filepath.Join(s.WorkspaceData(wid), "home", "checkout")
	if !exists(checkout) {
		if _, e := git(ctx, "init", "--quiet", checkout); e != nil {
			return failed("OPERATION_FAILED")
		}
		if _, e := git(ctx, "-C", checkout, "fetch", "--quiet", "--no-tags", "--", source, b.S("snapshotRef")); e != nil {
			_ = os.RemoveAll(checkout)
			return failed("SNAPSHOT_UNAVAILABLE")
		}
		if fetched, e := git(ctx, "-C", checkout, "rev-parse", "FETCH_HEAD"); e != nil || fetched != b.S("baseCommit") {
			_ = os.RemoveAll(checkout)
			return failed("BASE_COMMIT_MISMATCH")
		}
		if _, e := git(ctx, "-C", checkout, "checkout", "--quiet", "-B", branch.Name, b.S("baseCommit")); e != nil {
			_ = os.RemoveAll(checkout)
			return failed("OPERATION_FAILED")
		}
	}
	commit, e := git(ctx, "-C", checkout, "rev-parse", "HEAD")
	if e != nil {
		return failed("OPERATION_FAILED")
	}
	return core.Object{"outcome": "clone_ready", "path": checkout, "commit": commit}
}
