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

// clone checks out the requested ref of a mapped repository into the Workspace's Node home and
// reports the commit, or a failure reason in the contract's enum names.
func (s *Substrate) clone(ctx context.Context, b core.Object) core.Object {
	failed := func(reason string) core.Object {
		return core.Object{"outcome": "clone_failed", "reason": "CLONE_FAILURE_REASON_" + reason}
	}
	if s.faults["clone"] == "fail" {
		return failed("OPERATION_FAILED")
	}
	wid, ref := b.S("workspaceId"), b.S("branch")
	source, ok := s.Repositories[b.S("repositoryUrl")]
	if _, e := uuid.Parse(wid); e != nil || !ok {
		return failed("SOURCE_UNAVAILABLE")
	}
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\r\n\x00") {
		return failed("BRANCH_NOT_FOUND")
	}
	checkout := filepath.Join(s.WorkspaceData(wid), "home", "checkout")
	if !exists(checkout) {
		args := []string{"clone", "--quiet"}
		if ref != "HEAD" {
			args = append(args, "--branch", ref)
		}
		if _, e := git(ctx, append(args, "--", source, checkout)...); e != nil {
			_ = os.RemoveAll(checkout)
			return failed("BRANCH_NOT_FOUND")
		}
	}
	commit, e := git(ctx, "-C", checkout, "rev-parse", "HEAD")
	if e != nil {
		return failed("OPERATION_FAILED")
	}
	return core.Object{"outcome": "clone_ready", "path": checkout, "commit": commit}
}
