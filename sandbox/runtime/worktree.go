package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
)

func executeWorktree(c *websocket.Conn, m *oraEnvelope) error {
	if err := validateExecution(m); err != nil {
		return err
	}
	var p struct {
		Spec worktreeSpec `json:"spec"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return err
	}
	var err error
	if err := validateSpec(&p.Spec); err != nil {
		return err
	}
	oraStore.Lock()
	existing, ok := oraStore.state.Executions[m.ExecutionID]
	oraStore.Unlock()
	if ok {
		if existing.OperationID != m.OperationID || existing.Action != m.MessageType {
			return fmt.Errorf("execution_id is already bound to another operation")
		}
		if existing.State == "completed" {
			return writeOraFrame(c, eventEnvelope(&existing))
		}
		p.Spec = existing.Spec
	} else {
		rec := executionRecord{OperationID: m.OperationID, ExecutionID: m.ExecutionID, RequestID: m.RequestID, State: "accepted", Action: m.MessageType, Spec: p.Spec}
		oraStore.Lock()
		oraStore.state.Executions[m.ExecutionID] = rec
		err := saveOraStateLocked()
		oraStore.Unlock()
		if err != nil {
			return err
		}
	}
	oraStore.Lock()
	rec := oraStore.state.Executions[m.ExecutionID]
	rec.State = "running"
	oraStore.state.Executions[m.ExecutionID] = rec
	err = saveOraStateLocked()
	oraStore.Unlock()
	if err != nil {
		return err
	}
	result, msgType := performWorktree(m.MessageType, &p.Spec)
	oraStore.Lock()
	rec = oraStore.state.Executions[m.ExecutionID]
	rec.State = "completed"
	rec.Sequence = oraStore.state.NextSequence
	rec.MessageType = msgType
	rec.Result = result
	oraStore.state.NextSequence++
	oraStore.state.Executions[m.ExecutionID] = rec
	err = saveOraStateLocked()
	oraStore.Unlock()
	if err != nil {
		return err
	}
	log.Printf("ora_node execution_completed operation_id=%q execution_id=%q result=%q sequence=%d", m.OperationID, m.ExecutionID, msgType, rec.Sequence)
	return writeOraFrame(c, eventEnvelope(&rec))
}

func replayUnacked(c *websocket.Conn) error {
	oraStore.Lock()
	records := make([]*executionRecord, 0, len(oraStore.state.Executions))
	for executionID := range oraStore.state.Executions {
		rec := oraStore.state.Executions[executionID]
		if rec.State == "completed" && !rec.Acked {
			records = append(records, &rec)
		}
	}
	oraStore.Unlock()
	for _, rec := range records {
		if err := writeOraFrame(c, eventEnvelope(rec)); err != nil {
			return err
		}
	}
	return nil
}

func eventEnvelope(r *executionRecord) map[string]any {
	m := map[string]any{"message_type": r.MessageType, "protocol_version": 1, "operation_id": r.OperationID, "execution_id": r.ExecutionID, "sequence": r.Sequence, "payload": r.Result}
	if r.RequestID != "" {
		m["request_id"] = r.RequestID
	}
	return m
}

func validateSpec(s *worktreeSpec) error {
	if s.NodeID != identity().NodeID {
		return fmt.Errorf("spec node_id does not match connected node")
	}
	if s.WorkspaceID == "" || s.WorktreeID == "" || s.Repository == "" || s.MainWorkspace.WorkspaceID == "" || s.MainWorkspace.Path == "" || s.BaseRef == "" || s.ExpectedBranch == "" || s.PathPolicy.Kind != "node_managed" || s.PathPolicy.DirectoryName == "" {
		return fmt.Errorf("invalid worktree spec")
	}
	if !safeName(s.PathPolicy.DirectoryName) {
		return fmt.Errorf("path_policy.directory_name is unsafe")
	}
	return nil
}
func safeName(s string) bool { return s != "." && s != ".." && !strings.ContainsAny(s, "/\\") }
func performWorktree(action string, s *worktreeSpec) (result map[string]any, messageType string) {
	if action == "remove_worktree" {
		return removeWorktree(s)
	}
	return ensureWorktree(s)
}

func ensureWorktree(s *worktreeSpec) (result map[string]any, messageType string) {
	main, err := authorizedPath(s.MainWorkspace.Path, oraRepoRoot)
	if err != nil {
		return failureResult(s, "path_outside_authorized_root", err), "worktree_failed"
	}
	target := filepath.Join(oraWorktreeRoot, s.PathPolicy.DirectoryName)
	if _, err = os.Stat(filepath.Join(main, ".git")); err != nil {
		if _, e := os.Stat(main); e != nil {
			return failureResult(s, "main_workspace_not_found", e), "worktree_failed"
		}
		return failureResult(s, "invalid_main_workspace", err), "worktree_failed"
	}
	commit, err := gitOutput(main, "rev-parse", "--verify", s.BaseRef+"^{commit}")
	if err != nil {
		return failureResult(s, "base_ref_not_found", err), "worktree_failed"
	}
	if _, err = os.Stat(target); err == nil {
		branch, e := gitOutput(target, "branch", "--show-current")
		if e == nil && strings.TrimSpace(branch) == s.ExpectedBranch {
			return readyResult(s, target, strings.TrimSpace(commit)), "worktree_ready"
		}
		return failureResult(s, "worktree_conflict", fmt.Errorf("target already exists")), "worktree_failed"
	}
	if err = os.MkdirAll(oraWorktreeRoot, 0o755); err != nil {
		return failureResult(s, "operation_failed", err), "worktree_failed"
	}
	if _, err = gitOutput(main, "worktree", "add", "-b", s.ExpectedBranch, target, strings.TrimSpace(commit)); err != nil {
		return failureResult(s, "branch_conflict", err), "worktree_failed"
	}
	return readyResult(s, target, strings.TrimSpace(commit)), "worktree_ready"
}

func removeWorktree(s *worktreeSpec) (result map[string]any, messageType string) {
	main, err := authorizedPath(s.MainWorkspace.Path, oraRepoRoot)
	if err != nil {
		return failureResult(s, "path_outside_authorized_root", err), "worktree_removal_failed"
	}
	target := filepath.Join(oraWorktreeRoot, s.PathPolicy.DirectoryName)
	outcome := "already_absent"
	if _, err = os.Stat(target); err == nil {
		if _, err = gitOutput(main, "worktree", "remove", "--force", target); err != nil {
			return failureResult(s, "operation_failed", err), "worktree_removal_failed"
		}
		outcome = "removed"
	}
	_, _ = gitOutput(main, "branch", "-D", s.ExpectedBranch)
	return map[string]any{"node": identity(), "workspace_id": s.WorkspaceID, "worktree_id": s.WorktreeID, "outcome": outcome}, "worktree_removed"
}

func authorizedPath(path, root string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	r, _ := filepath.Abs(root)
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s is outside %s", p, r)
	}
	return p, nil
}

func readyResult(s *worktreeSpec, path, commit string) map[string]any {
	return map[string]any{"node": identity(), "workspace_id": s.WorkspaceID, "worktree_id": s.WorktreeID, "facts": map[string]any{"path": path, "branch": s.ExpectedBranch, "base_commit": commit}}
}

func failureResult(s *worktreeSpec, code string, err error) map[string]any {
	return map[string]any{"node": identity(), "workspace_id": s.WorkspaceID, "worktree_id": s.WorktreeID, "failure": map[string]any{"code": code, "message": err.Error()}}
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(b)))
	}
	return string(b), nil
}
