package gitpreset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

// Check asserts the contract (D4 and D5) between the Project repository and Cloud's records:
//   - every ref lies in a contract namespace and names a commit, and upstream refs belong to the
//     Project's Binding;
//   - every snapshot ref names its own target, and every tracking ref points at a snapshot;
//   - the Binding has at most one unfinished sync;
//   - the current snapshot, if any, was produced by a succeeded sync of this Binding and its ref
//     exists on the Git Server with the recorded commit, so the record never leads the repository.
//
// Violations are returned in the result, not as an error: an error means the check could not run.
func (p *Preset) Check(ctx context.Context, projectID string) (Result, error) {
	b, err := p.binding(ctx, projectID)
	if err != nil {
		return nil, err
	}
	violations := []string{}
	violate := func(format string, args ...any) { violations = append(violations, fmt.Sprintf(format, args...)) }

	refs := map[string]string{}
	repo := p.repositoryPath(b)
	present, err := exists(repo)
	if err != nil {
		return nil, err
	}
	if present {
		out, err := p.git(ctx, repo, "for-each-ref", "--format=%(refname) %(objectname) %(objecttype)")
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			ref, target := fields[0], fields[1]
			refs[ref] = target
			name, ok := core.ParseGitRef(ref)
			switch {
			case !ok:
				violate("%s is outside the contract namespaces", ref)
			case fields[2] != "commit":
				violate("%s names a %s, not a commit", ref, fields[2])
			case (name.Namespace == core.GitUpstreamSnapshot || name.Namespace == core.GitUpstreamHead) && name.Owner != b.id:
				violate("%s belongs to a Binding other than %s", ref, b.id)
			case name.Namespace == core.GitUpstreamSnapshot && name.Name != target:
				violate("%s names %s", ref, target)
			}
		}
		for ref, target := range refs {
			if name, ok := core.ParseGitRef(ref); ok && name.Namespace == core.GitUpstreamHead && refs[core.GitSnapshotRef(name.Owner, target)] != target {
				violate("%s points at %s, which has no snapshot", ref, target)
			}
		}
	}

	var unfinished int
	if err := p.DB.QueryRowContext(ctx, "SELECT count(*) FROM repository_operations WHERE binding_id=$1 AND type='repository_sync' AND state IN ('pending','leased','running')", b.id).Scan(&unfinished); err != nil {
		return nil, err
	}
	if unfinished > 1 {
		violate("binding %s has %d unfinished syncs", b.id, unfinished)
	}

	var current any
	var commit, snapshot, operation, kind, state, owner string
	err = p.DB.QueryRowContext(ctx, `SELECT s.commit_oid,s.snapshot_ref,s.sync_operation_id,o.type,o.state,o.binding_id FROM repository_snapshots s
	 JOIN repository_operations o ON o.id=s.sync_operation_id WHERE s.binding_id=$1`, b.id).Scan(&commit, &snapshot, &operation, &kind, &state, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		current = Result{"commit": commit, "snapshotRef": snapshot, "operationId": operation}
		if snapshot != core.GitSnapshotRef(b.id, commit) {
			violate("current snapshot ref %s does not name commit %s", snapshot, commit)
		}
		if kind != core.RepositorySync || state != "succeeded" || owner != b.id {
			violate("current snapshot comes from %s %s in state %s, not a succeeded sync of this Binding", kind, operation, state)
		}
		if got := refs[snapshot]; got != commit {
			violate("current snapshot %s is %q on the Git Server, want %s", snapshot, got, commit)
		}
	}
	return Result{"ok": len(violations) == 0, "projectId": b.project, "bindingId": b.id, "currentSnapshot": current, "refs": len(refs), "violations": violations}, nil
}
