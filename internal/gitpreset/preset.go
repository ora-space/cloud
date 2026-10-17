// Package gitpreset prepares and checks the Git Server contract's fixtures for acceptance: it
// stands in for the external line (snapshots, Operations) when the internal line is accepted, and
// for the internal line (Workspace branches) when the external line is. It writes the repository
// volume and the Cloud database directly, bypassing the Git Server's authorization on purpose, so
// it is development tooling only and never part of a production image.
package gitpreset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

// UsageError is an invalid invocation, distinct from a failed operation or a contract violation.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usage(format string, args ...any) error { return &UsageError{msg: fmt.Sprintf(format, args...)} }

// Preset holds the database and repository volume one command works on.
type Preset struct {
	DB *sql.DB
	// Root is the Git Server's repository root; Git is the git executable.
	Root, Git string
}

// Result is the one-line JSON object a command prints.
type Result map[string]any

// binding is the Project's tenant and its one Binding.
type binding struct {
	tenant, project, id, branch string
}

func (p *Preset) binding(ctx context.Context, projectID string) (binding, error) {
	if !canonical(projectID) {
		return binding{}, usage("--project must be a canonical UUID")
	}
	b := binding{project: projectID}
	err := p.DB.QueryRowContext(ctx, `SELECT p.tenant_id,b.id,b.branch FROM projects p JOIN repository_bindings b ON b.project_id=p.id
	 WHERE p.id=$1 AND p.deleted_at IS NULL`, projectID).Scan(&b.tenant, &b.id, &b.branch)
	if errors.Is(err, sql.ErrNoRows) {
		return binding{}, fmt.Errorf("project %s has no live Binding", projectID)
	}
	return b, err
}

// Snapshot writes refs/ora/upstreams/<binding>/snapshots/<oid> and the tracking ref of the
// Binding's branch, then records a succeeded sync and makes the snapshot the Binding's current one,
// in one transaction. The commit is either already in the repository, or a synthetic history of n
// commits created for the purpose (deterministic, so the same n yields the same commit).
func (p *Preset) Snapshot(ctx context.Context, projectID, commit string, synthetic int) (Result, error) {
	if (commit == "") == (synthetic == 0) || synthetic < 0 {
		return nil, usage("snapshot needs exactly one of --commit <oid> or --synthetic <n>")
	}
	if commit != "" && !core.GitObjectID(commit) {
		return nil, usage("--commit must be a lowercase object name")
	}
	b, err := p.binding(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tracking := core.GitUpstreamHeadPrefix(b.id) + b.branch
	if _, ok := core.ParseGitRef(tracking); !ok {
		return nil, fmt.Errorf("binding branch %q cannot name a tracking ref", b.branch)
	}
	repo, err := gitserver.EnsureRepository(ctx, p.Root, p.Git, b.tenant, b.project)
	if err != nil {
		return nil, err
	}
	if synthetic > 0 {
		if commit, err = p.history(ctx, repo, synthetic); err != nil {
			return nil, err
		}
	} else if err := p.requireCommit(ctx, repo, commit); err != nil {
		return nil, err
	}
	snapshot := core.GitSnapshotRef(b.id, commit)
	if current, err := p.resolve(ctx, repo, snapshot); err != nil {
		return nil, err
	} else if current != "" && current != commit {
		return nil, fmt.Errorf("%s already names %s", snapshot, current)
	}
	for _, ref := range []string{snapshot, tracking} {
		if _, err := p.git(ctx, repo, "update-ref", ref, commit); err != nil {
			return nil, err
		}
	}
	operation, err := p.recordSnapshot(ctx, b, commit, snapshot)
	if err != nil {
		return nil, err
	}
	return Result{"ok": true, "projectId": b.project, "bindingId": b.id, "commit": commit, "snapshotRef": snapshot, "trackingRef": tracking, "operationId": operation}, nil
}

func (p *Preset) recordSnapshot(ctx context.Context, b binding, commit, snapshot string) (string, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	// The Binding's unfinished sync, such as the one every new Project starts with, is the sync this
	// snapshot completes; only without one is a new succeeded sync recorded.
	operation, err := advanceSync(ctx, tx, b, "succeeded", nil)
	if err != nil {
		return "", err
	}
	if operation == "" {
		if err := tx.QueryRowContext(ctx, `INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,finished_at)
		 VALUES(gen_random_uuid(),'repository_sync',$1,$2,$3,'succeeded','preset',clock_timestamp()) RETURNING id`, b.tenant, b.project, b.id).Scan(&operation); err != nil {
			return "", fmt.Errorf("record sync: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_snapshots(binding_id,tenant_id,project_id,commit_oid,snapshot_ref,sync_operation_id) VALUES($1,$2,$3,$4,$5,$6)
	 ON CONFLICT(binding_id) DO UPDATE SET commit_oid=EXCLUDED.commit_oid,snapshot_ref=EXCLUDED.snapshot_ref,sync_operation_id=EXCLUDED.sync_operation_id,recorded_at=clock_timestamp()`,
		b.id, b.tenant, b.project, commit, snapshot, operation); err != nil {
		return "", fmt.Errorf("record snapshot: %w", err)
	}
	return operation, tx.Commit()
}

// Ref writes one ref of any contract namespace, for example a Workspace branch to publish from.
// The commit must already be in the repository.
func (p *Preset) Ref(ctx context.Context, projectID, ref, commit string) (Result, error) {
	name, ok := core.ParseGitRef(ref)
	if !ok {
		return nil, usage("--ref must be a ref of a contract namespace")
	}
	if !core.GitObjectID(commit) {
		return nil, usage("--commit must be a lowercase object name")
	}
	b, err := p.binding(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if (name.Namespace == core.GitUpstreamSnapshot || name.Namespace == core.GitUpstreamHead) && name.Owner != b.id {
		return nil, usage("upstream refs must belong to the Project's Binding %s", b.id)
	}
	repo, err := gitserver.EnsureRepository(ctx, p.Root, p.Git, b.tenant, b.project)
	if err != nil {
		return nil, err
	}
	if err := p.requireCommit(ctx, repo, commit); err != nil {
		return nil, err
	}
	if _, err := p.git(ctx, repo, "update-ref", ref, commit); err != nil {
		return nil, err
	}
	return Result{"ok": true, "projectId": b.project, "ref": ref, "commit": commit}, nil
}

// operationStates are the states each Operation type may be preset to (contract D5 item 3).
var operationStates = map[string]map[string]bool{
	core.RepositorySync:    {"pending": false, "leased": false, "running": false, "succeeded": true, "failed": true, "superseded": true, "cancelled": true},
	core.RepositoryPublish: {"awaiting_approval": false, "pending": false, "leased": false, "running": false, "uncertain": false, "succeeded": true, "conflict": true, "rejected": true, "failed": true, "cancelled": true},
}

// advanceSync moves the Binding's unfinished sync, if there is one, to the given sync state and
// returns its ID; "" means the Binding had none. A terminal state also sets finished_at.
func advanceSync(ctx context.Context, tx *sql.Tx, b binding, state string, errorCode any) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `UPDATE repository_operations SET state=$2,error_code=$3,finished_at=CASE WHEN $4 THEN clock_timestamp() END
	 WHERE binding_id=$1 AND type='repository_sync' AND state IN ('pending','leased','running') RETURNING id`, b.id, state, errorCode, operationStates[core.RepositorySync][state]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("advance sync: %w", err)
	}
	return id, nil
}

// Operation writes an Operation in a given state, for waiting, failure and deletion scenarios.
// Terminal states get finished_at. A sync moves the Binding's unfinished sync — every new Project
// starts with one — to the requested state instead of adding a second row beside it, so any sync
// state can be preset; only without one is a new sync row written. A publish is always a new row.
func (p *Preset) Operation(ctx context.Context, projectID, kind, state, errorCode string) (Result, error) {
	types := map[string]string{"sync": core.RepositorySync, "publish": core.RepositoryPublish}
	t, ok := types[kind]
	if !ok {
		return nil, usage("--type must be sync or publish")
	}
	terminal, ok := operationStates[t][state]
	if !ok {
		return nil, usage("%s cannot be in state %q", t, state)
	}
	b, err := p.binding(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var code any
	if errorCode != "" {
		code = errorCode
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if t == core.RepositorySync {
		if id, err = advanceSync(ctx, tx, b, state, code); err != nil {
			return nil, err
		}
	}
	if id == "" {
		err = tx.QueryRowContext(ctx, `INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,error_code,finished_at)
		 VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,'preset',$6,CASE WHEN $7 THEN clock_timestamp() END) RETURNING id`, t, b.tenant, b.project, b.id, state, code, terminal).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("record operation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return Result{"ok": true, "projectId": b.project, "bindingId": b.id, "operationId": id, "type": t, "state": state}, nil
}

// history creates n deterministic commits in the bare repository and returns the last.
func (p *Preset) history(ctx context.Context, repo string, n int) (string, error) {
	parent := ""
	for i := 1; i <= n; i++ {
		blob, err := p.gitInput(ctx, repo, "preset commit "+strconv.Itoa(i)+"\n", "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		tree, err := p.gitInput(ctx, repo, "100644 blob "+blob+"\tREADME\n", "mktree")
		if err != nil {
			return "", err
		}
		args := []string{"commit-tree", tree, "-m", "preset " + strconv.Itoa(i)}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		if parent, err = p.git(ctx, repo, args...); err != nil {
			return "", err
		}
	}
	return parent, nil
}

func (p *Preset) requireCommit(ctx context.Context, repo, commit string) error {
	kind, err := p.git(ctx, repo, "cat-file", "-t", commit)
	if err != nil || kind != "commit" {
		return fmt.Errorf("%s is not a commit in the Project repository", commit)
	}
	return nil
}

// resolve returns the commit an exact ref names, or "" when it is absent.
func (p *Preset) resolve(ctx context.Context, repo, ref string) (string, error) {
	out, err := p.git(ctx, repo, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == ref {
			return fields[1], nil
		}
	}
	return "", nil
}

func (p *Preset) git(ctx context.Context, repo string, args ...string) (string, error) {
	return p.gitInput(ctx, repo, "", args...)
}

// gitInput runs git on the bare repository with a fixed identity and date, so synthetic history
// is reproducible, and without any user or system configuration.
func (p *Preset) gitInput(ctx context.Context, repo, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.Git, append([]string{"--git-dir", repo}, args...)...) //nolint:gosec // G204: arguments are validated refs, object names and fixed subcommands.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Ora preset", "GIT_AUTHOR_EMAIL=preset@ora.invalid", "GIT_COMMITTER_NAME=Ora preset", "GIT_COMMITTER_EMAIL=preset@ora.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func canonical(id string) bool {
	_, ok := core.ParseGitRef(core.GitDeliveryRefPrefix + id)
	return ok
}

// repositoryPath is where the Project repository is; it may not exist yet.
func (p *Preset) repositoryPath(b binding) string {
	return gitserver.RepositoryPath(p.Root, b.tenant, b.project)
}

func exists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && info.IsDir(), err
}
