//go:build linux

package agentruntime

import (
	"bytes"
	"context"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The model is a fixture, while the Agent, shell, checkout and skill bytes are real.
type marketSkillFixture struct{}

func (marketSkillFixture) Complete(w http.ResponseWriter, r *http.Request, m nodeservices.Model) {
	buffer := httptest.NewRecorder()
	nodeservices.FixtureProvider{}.Complete(buffer, r, m)
	for k, vs := range buffer.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(buffer.Code)
	_, _ = w.Write(bytes.ReplaceAll(buffer.Body.Bytes(), []byte("(cat README.md"), []byte("(cp .opencode/skills/ask-matt/SKILL.md used-market-skill.md; cat README.md")))
}

// Uses the official mattpocock.skills 1.3.1 release and the same real Node assets as chat.
func TestRealMarketSkillNode(t *testing.T) {
	assets, file := os.Getenv("V2_NODE_ASSETS_DIR"), os.Getenv("V2_TEST_SKILL_ARCHIVE")
	if assets == "" || file == "" {
		t.Skip("real Node assets and Skill archive required")
	}
	home, e := os.UserHomeDir()
	if e != nil {
		t.Fatal(e)
	}
	tmp, e := os.MkdirTemp(home, "vs-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo := filepath.Join(tmp, "repo.git")
	if out, e := exec.Command("git", "init", "--bare", repo).CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	gitDir, e := exec.Command("git", "--exec-path").Output()
	if e != nil {
		t.Fatal(e)
	}
	git := func(args ...string) string {
		t.Helper()
		out, e := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if e != nil {
			t.Fatal(string(out), e)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(tmp, "seed")
	git("init", work)
	if e := os.WriteFile(filepath.Join(work, "README.md"), []byte("Fixture project"), 0600); e != nil {
		t.Fatal(e)
	}
	git("-C", work, "add", "README.md")
	git("-C", work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "Fixture")
	commit := git("-C", work, "rev-parse", "HEAD")
	ref := "refs/ora/upstreams/4da35252-6789-4760-9041-49b68ce3874f/snapshots/" + commit
	git("-C", work, "push", repo, "HEAD:"+ref)
	project := Project{MirrorPath: repo, Commit: commit, SnapshotRef: ref}
	d, e := New(Options{Root: filepath.Join(tmp, "n"), AssetsDirectory: assets, GitBackend: filepath.Join(strings.TrimSpace(string(gitDir)), "git-http-backend"), ResolveProject: func(context.Context, execution.Scope, string) (Project, error) { return project, nil }, Bindings: map[string]nodeservices.Binding{"fixture": {Provider: marketSkillFixture{}, Protocol: nodeservices.OpenAICompletions, Model: nodeservices.Model{ID: "fixture", Name: "Fixture", ContextWindow: 4096, MaxTokens: 256}}}})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close(ctx)
	d.startProject = func(p *projectRuntime) error {
		e := p.start()
		if e != nil {
			t.Logf("start Node: %v", e)
			filepath.WalkDir(tmp, func(path string, entry os.DirEntry, err error) error {
				if err == nil && strings.HasSuffix(path, ".log") {
					b, _ := os.ReadFile(path)
					if len(b) > 2000 {
						b = b[len(b)-2000:]
					}
					t.Logf("%s: %s", entry.Name(), b)
				}
				return nil
			})
		}
		return e
	}
	p, e := d.project(ctx, "test", project)
	if e != nil {
		t.Fatal(e)
	}
	checkout := filepath.Join(p.root, "w", "r", "example")
	if e := os.MkdirAll(checkout, 0700); e != nil {
		t.Fatal(e)
	}
	if e := p.attachSkills(checkout); e != nil {
		t.Fatal(e)
	}
	const id = "official/mattpocock.skills"
	const version = "1.3.1"
	const digest = "c3de8a2c6863a1864901fc55e7eb42e22fe0829fb958249de59c5d4e715def64"
	if e := d.ApplySkill(ctx, "test", id, version, file, digest, false); e != nil {
		t.Fatal(e)
	}
	discover := func(root string) bool {
		for _, base := range []string{".opencode", ".claude"} {
			data, e := os.ReadFile(filepath.Join(root, base, "skills", "ask-matt", "SKILL.md"))
			if e != nil || !strings.Contains(string(data), "ask-matt") {
				return false
			}
		}
		return true
	}
	if !discover(checkout) {
		t.Fatal("installed skill not usable at project discovery paths")
	}
	later := filepath.Join(p.root, "w", "r", "later")
	if e := os.MkdirAll(later, 0700); e != nil {
		t.Fatal(e)
	}
	if e := p.attachSkills(later); e != nil || !discover(later) {
		t.Fatal("new checkout missing installed skill", e)
	}
	if e := d.ApplySkill(ctx, "test", id, version, file, strings.Repeat("0", 64), false); e == nil || !discover(checkout) {
		t.Fatal("invalid update displaced installed skill")
	}
	removeVersion := version
	if update := os.Getenv("V2_TEST_SKILL_UPDATE_ARCHIVE"); update != "" {
		const next = "1.3.2"
		const sum = "403e5bb8eee173c9fc847a578e339ed82e9408654169f001568430ba4090d409"
		if e := d.ApplySkill(ctx, "test", id, next, update, sum, false); e != nil {
			t.Fatal(e)
		}
		target, e := os.Readlink(filepath.Join(checkout, ".opencode", "skills", "ask-matt"))
		if e != nil || !strings.Contains(target, "/1.3.2/") || !discover(checkout) {
			t.Fatal("updated skill not discoverable", target, e)
		}
		removeVersion = next
	}
	session := execution.Session{ID: "3418fbc1-2775-4d45-b16d-bc96ccbe8863", WorkspaceID: "world", Target: &execution.Target{ProjectID: "test"}, Executor: &execution.Executor{ID: "opencode:fixture", AgentID: pluginID, PluginVersion: pluginVersion, Model: execution.Model{ID: "fixture"}}}
	turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: "5542e8ae-f3bc-4a88-9a82-43b584abd2d6"}, Prompt: execution.TextPrompt("Read the installed project skill and save the instructions as used-market-skill.md.")}
	result, e := d.Run(ctx, execution.Scope{TenantID: "tenant", UserID: "user"}, session, turn, func(context.Context, execution.Event) error { return nil })
	if e != nil || result.Status != execution.StatusCompleted {
		t.Fatal("skill Agent turn", result, e)
	}
	p.mu.Lock()
	used := p.checkouts[session.WorkspaceID].path
	p.mu.Unlock()
	data, e := os.ReadFile(filepath.Join(used, "used-market-skill.md"))
	if e != nil || !strings.Contains(string(data), "ask-matt") {
		t.Fatal("real Agent did not read project skill", e)
	}
	if e := d.End(ctx, session.ID); e != nil {
		t.Fatal(e)
	}
	if e := d.ApplySkill(ctx, "test", id, removeVersion, "", digest, true); e != nil {
		t.Fatal(e)
	}
	if discover(checkout) || discover(later) {
		t.Fatal("removed skill still present")
	}
}
