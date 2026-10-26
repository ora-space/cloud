package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// gitRefs is Cloud's ref query in the fixture. It answers from the Git Server double's repositories,
// unless a case pins a ref to a commit (for seeded scenes whose commits exist nowhere) or makes the
// Git Server unreachable. Every query is recorded, so a case can assert which refs Cloud verified
// and that a contradictory declaration never reached the Git Server.
type gitRefs struct {
	git         *simulator.GitServer
	mu          sync.Mutex
	pinned      map[string]string
	unavailable bool
	queries     []string
}

func (g *gitRefs) ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (string, error) {
	g.mu.Lock()
	g.queries = append(g.queries, ref)
	unavailable, commit, ok := g.unavailable, "", false
	if !unavailable {
		commit, ok = g.pinned[tenantID+"/"+projectID+"/"+ref]
	}
	g.mu.Unlock()
	switch {
	case unavailable:
		return "", errors.New("git server unreachable")
	case ok:
		return commit, nil
	}
	return g.git.ResolveGitRef(ctx, tenantID, projectID, ref)
}

// pin makes ref name commit in the Project repository, as a push the Git Server accepted would.
func (g *gitRefs) pin(tenantID, projectID, ref, commit string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pinned[tenantID+"/"+projectID+"/"+ref] = commit
}

func (g *gitRefs) setUnavailable(unavailable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unavailable = unavailable
}

func (g *gitRefs) queried() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.queries...)
}

// syncProject stands in for the external line's first sync of a new Project: the source
// repository's HEAD becomes the Binding's current snapshot and the Project's pending sync succeeds.
func (f *fixture) syncProject(projectID string) string {
	f.t.Helper()
	commit, err := f.git.SeedSnapshot(context.Background(), f.store.Pool, projectID, f.source)
	must(f.t, err)
	return commit
}

// seedCloneInput is the frozen clone input a Workspace of a seeded scene was cloned with: a snapshot
// of commit in the Project's Git Server repository, checked out on the Workspace's main branch.
func seedCloneInput(tenantID, projectID, workspaceID, commit string) core.Object {
	return core.Object{
		"kind":          "clone",
		"repositoryUrl": simulator.GitServerBaseURL + "/" + tenantID + "/" + projectID + ".git",
		"snapshotRef":   core.GitSnapshotRef(uuid.NewString(), commit),
		"baseCommit":    commit,
		"workspaceRef":  core.GitWorkspaceRefPrefix(workspaceID) + "main",
	}
}

// cloneInputSpec is the wire form of a durable clone input.
func cloneInputSpec(input core.Object) *controlpb.ExecutionInput {
	return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: &controlpb.CloneSpec{
		RepositoryUrl: input.S("repositoryUrl"), SnapshotRef: input.S("snapshotRef"), BaseCommit: input.S("baseCommit"), WorkspaceRef: input.S("workspaceRef"),
	}}}
}

// checkoutOf is the successful clone execution that produced a Workspace's checkout: the one a
// delivery input must name, because the delivery takes the Workspace branch from its input.
func (f *fixture) checkoutOf(workspaceID string) string {
	f.t.Helper()
	var execution string
	must(f.t, f.store.Pool.QueryRow(`SELECT execution_id FROM clone_executions WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' ORDER BY created_at DESC LIMIT 1`, workspaceID).Scan(&execution))
	return execution
}

// pushedDelivery records that a registered delivery's ref names final, as an accepted push leaves it.
func (f *fixture) pushedDelivery(execution, final string) {
	f.t.Helper()
	var tenant, project, ref string
	must(f.t, f.store.Pool.QueryRow(`SELECT w.tenant_id,w.project_id,e.input->>'deliveryRef' FROM node_executions e JOIN workspaces w ON w.id=e.workspace_id WHERE e.execution_id=$1`, execution).Scan(&tenant, &project, &ref))
	f.refs.pin(tenant, project, ref, final)
}

// frozenClone is the clone input Cloud froze into an operation's clone step, or nil before that.
func (f *fixture) frozenClone(operationID string) core.Object {
	f.t.Helper()
	var raw sql.NullString
	must(f.t, f.store.Pool.QueryRow(`SELECT (request->'clone')::text FROM operations WHERE id=$1`, operationID).Scan(&raw))
	if !raw.Valid {
		return nil
	}
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw.String), &out))
	return out
}
