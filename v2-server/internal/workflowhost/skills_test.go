package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestWorkflowFreezesSelectedSkillAndClearsItForNextNode(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.CreateSkill(context.Background(), f.scope.TenantID, f.scope.UserID, store.SkillInput{Name: "selected-review", Description: "Review", Content: "UNIQUE_REVIEW_INSTRUCTION"})
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"skills":[{"skillId":"selected-review","enabled":true},{"skillId":"missing-disabled","enabled":false}]`, 1)
	runID := f.run(t, graph, str("review"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	request := f.exec.request(a)
	if request.Skills == nil || len(request.Skills.Documents) != 1 || request.Skills.Documents[0].Content != "UNIQUE_REVIEW_INSTRUCTION" {
		t.Fatalf("not frozen: %+v", request.Skills)
	}
	if !strings.Contains(promptText(t, request.Prompt), "<required_skills>") {
		t.Fatal("missing required skill instructions")
	}
	f.exec.finish(a, completed("reviewed"))
	b := f.exec.awaitStart(t)
	next := f.exec.request(b)
	if next.Skills == nil || len(next.Skills.Documents) != 0 || len(next.Skills.Plugins) != 0 {
		t.Fatalf("next node inherited skills: %+v", next.Skills)
	}
	f.exec.finish(b, completed("done"))
	f.await(t, runID, terminal)
}

func TestStartRefusesASkillMissingFromAnyNodeOfTheGraph(t *testing.T) {
	t.Run("unreached branch personal skill", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		graph := strings.Replace(
			reviewBranches,
			`"prompt":"返工"`,
			`"prompt":"返工","skills":[{"skillId":"ignored-disabled","enabled":false},{"skillId":"missing-rework","enabled":true}]`,
			1,
		)
		runID := f.run(t, graph, str("支付重构"))

		_, err := f.host.StartRun(ctx, f.scope, runID)
		if !errors.Is(err, server.ErrWorkflowSkillNotFound) {
			t.Fatalf("start error = %v, want missing skill", err)
		}
		var missing *server.WorkflowSkillNotFound
		if !errors.As(err, &missing) || missing.NodeID != "fix" || missing.NodeTitle != "返工" || missing.SkillID != "missing-rework" {
			t.Fatalf("WorkflowSkillNotFound = %+v, want node fix (返工) skill missing-rework", missing)
		}
		f.exec.mu.Lock()
		started := len(f.exec.started)
		f.exec.mu.Unlock()
		if started != 0 {
			t.Fatalf("started %d Agents despite missing skill", started)
		}
		run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != store.WorkflowStatusPending {
			t.Fatalf("run status = %s, want pending", run.Status)
		}

		// Creating the skill in the starter's catalog makes the same pending run startable.
		if _, err := f.s.CreateSkill(ctx, f.scope.TenantID, f.scope.UserID, store.SkillInput{
			Name: "missing-rework", Description: "返工清单", Content: "# 返工步骤",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
			t.Fatalf("start after creating skill = %v", err)
		}
		f.exec.finish(f.exec.awaitStart(t), completed(`{"approved":true}`))
		f.exec.finish(f.exec.awaitStart(t), completed("已发布"))
		f.await(t, runID, terminal)
	})

	t.Run("loop body blank title and undeployed marketplace skill", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		loopGraph := `{
		  "schemaVersion": 2,
		  "nodes":[
		    {"id":"start","data":{"kind":"start"}},
		    {"id":"loop","data":{"kind":"loop","loopConfig":{
		      "maxIterations":2,
		      "variables":[{"name":"draft","valueType":"string","initial":{"kind":"constant","value":""},"feedback":["worker","output"]}],
		      "until":{"logic":"and","conditions":[{"variableSelector":["worker","output"],"operator":"equals","value":"done"}]},
		      "outputs":[{"name":"result","variableSelector":["worker","output"]}]
		    }}},
		    {"id":"entry","parentId":"loop","data":{"kind":"start","containerId":"loop"}},
		    {"id":"worker","data":{"kind":"agent","title":"   ","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"工作","skills":[{"skillId":"official/mattpocock.skills","enabled":true}]}}},
		    {"id":"out","data":{"kind":"output","outputs":[{"name":"res","variableSelector":["loop","result"]}]}}
		  ],
		  "edges":[
		    {"source":"start","target":"loop"},
		    {"source":"entry","target":"worker"},
		    {"source":"loop","target":"out"}
		  ]
		}`
		runID := f.run(t, loopGraph, str("检查循环技能"))
		_, err := f.host.StartRun(ctx, f.scope, runID)
		var missing *server.WorkflowSkillNotFound
		if !errors.Is(err, server.ErrWorkflowSkillNotFound) || !errors.As(err, &missing) {
			t.Fatalf("start error = %v, want WorkflowSkillNotFound", err)
		}
		if missing.NodeID != "worker" || missing.NodeTitle != "worker" || missing.SkillID != "official/mattpocock.skills" {
			t.Fatalf("missing = %+v, want worker/worker/official/mattpocock.skills", missing)
		}
	})

	t.Run("iteration member personal skill", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		graph := strings.Replace(
			iterationGraph("fail", []string{"one"}, 10),
			`"kind":"agent","agentConfig":{`,
			`"kind":"agent","title":"批量处理","agentConfig":{"skills":[{"skillId":"iter-missing-skill","enabled":true}],`,
			1,
		)
		runID := f.run(t, graph, str("检查迭代技能"))
		_, err := f.host.StartRun(ctx, f.scope, runID)
		var missing *server.WorkflowSkillNotFound
		if !errors.Is(err, server.ErrWorkflowSkillNotFound) || !errors.As(err, &missing) {
			t.Fatalf("start error = %v, want WorkflowSkillNotFound", err)
		}
		if missing.NodeID != "worker" || missing.NodeTitle != "批量处理" || missing.SkillID != "iter-missing-skill" {
			t.Fatalf("missing = %+v, want worker/批量处理/iter-missing-skill", missing)
		}
		f.exec.mu.Lock()
		started := len(f.exec.started)
		f.exec.mu.Unlock()
		if started != 0 {
			t.Fatalf("started %d Agents despite missing iteration skill", started)
		}
		if _, err := f.s.CreateSkill(ctx, f.scope.TenantID, f.scope.UserID, store.SkillInput{
			Name: "iter-missing-skill", Description: "迭代技能", Content: "# 迭代检查",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
			t.Fatalf("start after creating iteration skill = %v", err)
		}
		f.exec.finish(f.exec.awaitStart(t), completed("one-done"))
		run, _ := f.await(t, runID, terminal)
		if run.Status != store.WorkflowStatusSucceeded {
			t.Fatalf("run = %+v, want succeeded", run)
		}
	})
}

func TestSkillDeletedAfterStartFailsTheNodeResumably(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	skill, err := f.s.CreateSkill(ctx, f.scope.TenantID, f.scope.UserID, store.SkillInput{
		Name: "review-checklist", Description: "审查", Content: "# 审查清单",
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(twoAgents, `"prompt":"审阅这段初稿：{{#a.output#}}"`,
		`"prompt":"审阅这段初稿：{{#a.output#}}","skills":[{"skillId":"review-checklist","enabled":true}]`, 1)
	runID := f.run(t, graph, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	if err := f.s.DeleteSkill(ctx, f.scope.TenantID, f.scope.UserID, skill.ID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(a, completed("初稿"))

	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v, want failed", run)
	}
	b := node(nodes, "b")
	if kind := errorKind(t, b); kind != "missing_agent_config" {
		t.Fatalf("node b failure = %s, want missing_agent_config", kind)
	}
	var detail struct {
		ErrorDetail struct {
			Message       string `json:"message"`
			Resumable     bool   `json:"resumable"`
			AutoRetryable bool   `json:"auto_retryable"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(b.Payload, &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.ErrorDetail.Resumable || detail.ErrorDetail.AutoRetryable || !strings.Contains(detail.ErrorDetail.Message, "review-checklist") {
		t.Fatalf("detail = %+v", detail.ErrorDetail)
	}

	if _, err := f.s.CreateSkill(ctx, f.scope.TenantID, f.scope.UserID, store.SkillInput{
		Name: "review-checklist", Description: "审查", Content: "# 恢复后的审查清单",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatalf("resume after recreating skill = %v", err)
	}
	bSession := f.exec.awaitStart(t)
	f.exec.finish(bSession, completed("已审阅"))
	resumed, _ := f.await(t, runID, terminal)
	if resumed.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("resumed run = %+v, want succeeded", resumed)
	}
}

func TestDependencyLookupDatabaseErrorIsReportedAsRepository(t *testing.T) {
	t.Run("start preflight preserves database error", func(t *testing.T) {
		f, admin := newFixtureWithAdmin(t)
		ctx := context.Background()
		graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"skills":[{"skillId":"review-checklist","enabled":true}]`, 1)
		runID := f.run(t, graph, str("技能库故障"))
		if _, err := admin.Exec(ctx, `ALTER TABLE v2_skills RENAME COLUMN content TO broken_content`); err != nil {
			t.Fatal(err)
		}
		_, err := f.host.StartRun(ctx, f.scope, runID)
		if err == nil || errors.Is(err, server.ErrWorkflowSkillNotFound) {
			t.Fatalf("start error = %v, want database error rather than ErrWorkflowSkillNotFound", err)
		}
	})

	t.Run("start preflight preserves plugin skill database error", func(t *testing.T) {
		f, admin := newFixtureWithAdmin(t)
		ctx := context.Background()
		graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"skills":[{"skillId":"official/review","enabled":true}]`, 1)
		runID := f.run(t, graph, str("插件技能库故障"))
		if _, err := admin.Exec(ctx, `ALTER TABLE v2_skill_results RENAME COLUMN installed_version TO broken_installed_version`); err != nil {
			t.Fatal(err)
		}
		_, err := f.host.StartRun(ctx, f.scope, runID)
		if err == nil || errors.Is(err, server.ErrWorkflowSkillNotFound) {
			t.Fatalf("start error = %v, want database error rather than ErrWorkflowSkillNotFound", err)
		}
	})

	// A dependency the node reads live (its Skills, its role) that hits a database error mid-run is
	// neither a missing dependency nor a broken Agent session: it is recorded as desktop's
	// `repository` kind, resumable but never auto-retried, with a message that names neither the
	// table nor the statement. Interactive nodes keep the kind because it is not auto-retryable.
	for _, tc := range []struct {
		name     string
		config   string
		breakSQL string
		message  string
	}{
		{
			name:     "skills",
			config:   `"skills":[{"skillId":"review-checklist","enabled":true}]`,
			breakSQL: `ALTER TABLE v2_skills RENAME COLUMN content TO broken_content`,
			message:  "could not resolve selected Skills",
		},
		{
			name:     "role",
			config:   `"roleId":"reviewer"`,
			breakSQL: `ALTER TABLE v2_roles RENAME COLUMN content TO broken_content`,
			message:  "could not resolve the selected role",
		},
	} {
		for _, interactive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s %s node mid-run database error is repository", tc.name, map[bool]string{false: "automatic", true: "interactive"}[interactive]), func(t *testing.T) {
				f, admin := newFixtureWithAdmin(t)
				ctx := context.Background()
				if _, err := f.s.CreateSkill(ctx, f.scope.TenantID, f.scope.UserID, store.SkillInput{
					Name: "review-checklist", Description: "审查", Content: "# 审查",
				}); err != nil {
					t.Fatal(err)
				}
				var graph string
				if interactive {
					graph = strings.Replace(
						interactiveChain,
						`"prompt":"起草：{{#start.input#}}","interactive":true`,
						`"prompt":"起草：{{#start.input#}}","interactive":true,`+tc.config+`,"retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}`,
						1,
					)
				} else {
					graph = strings.Replace(twoAgents, `"roleId":"reviewer"`, tc.config+`,"retry":{"enabled":false,"maxRetries":1,"initialDelaySeconds":0}`, 1)
				}
				runID := f.run(t, graph, str("依赖库故障"))
				entered, release := f.holdDispatchOf(t, "a")
				if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("Agent dispatch never started")
				}
				if _, err := admin.Exec(ctx, tc.breakSQL); err != nil {
					t.Fatal(err)
				}
				release()

				run, nodes := f.await(t, runID, terminal)
				if run.Status != store.WorkflowStatusFailed {
					t.Fatalf("run = %+v, want failed", run)
				}
				var detail struct {
					ErrorDetail struct {
						Kind          string `json:"kind"`
						Message       string `json:"message"`
						Resumable     bool   `json:"resumable"`
						AutoRetryable bool   `json:"auto_retryable"`
					} `json:"error_detail"`
				}
				if err := json.Unmarshal(node(nodes, "a").Payload, &detail); err != nil {
					t.Fatal(err)
				}
				if detail.ErrorDetail.Kind != string(workflow.FailureRepository) || !detail.ErrorDetail.Resumable || detail.ErrorDetail.AutoRetryable {
					t.Fatalf("error_detail = %+v, want repository resumable=true auto_retryable=false", detail.ErrorDetail)
				}
				if detail.ErrorDetail.Message != tc.message {
					t.Fatalf("error_detail.message = %q, want exact generic message %q", detail.ErrorDetail.Message, tc.message)
				}
				if run.Error == nil || strings.Contains(*run.Error, "broken_content") || strings.Contains(*run.Error, "SQLSTATE") {
					t.Fatalf("run error leaks the database detail: %v", run.Error)
				}
			})
		}
	}
}

func TestWorkflowRejectsMissingEnabledMCPAndIgnoresDisabledBindings(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newFixture(t)
			config := fmt.Sprintf(`"mcps":[{"mcpId":"official/tavily","enabled":%t},{"mcpId":"demo/other","enabled":%t}]`, enabled, enabled)
			graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, config, 1)
			runID := f.run(t, graph, str("check"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			if enabled {
				run, nodes := f.await(t, runID, terminal)
				if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "missing_agent_config" {
					t.Fatal("missing MCP was not rejected")
				}
				var payload struct {
					ErrorDetail struct {
						Code          string
						AutoRetryable bool `json:"auto_retryable"`
					} `json:"error_detail"`
				}
				if err := json.Unmarshal(node(nodes, "a").Payload, &payload); err != nil || payload.ErrorDetail.Code != "mcp_plugin_missing" || payload.ErrorDetail.AutoRetryable {
					t.Fatal("missing MCP is not a stable non-retryable failure")
				}
				f.exec.mu.Lock()
				defer f.exec.mu.Unlock()
				if len(f.exec.started) != 0 {
					t.Fatal("started with missing MCP")
				}
				return
			}
			a := f.exec.awaitStart(t)
			if strings.Contains(promptText(t, f.exec.request(a).Prompt), "official/tavily") {
				t.Fatal("MCP binding leaked into the Agent prompt")
			}
			f.exec.finish(a, completed("reviewed"))
			f.exec.finish(f.exec.awaitStart(t), completed("done"))
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"review":"done"}` {
				t.Fatalf("MCP configuration prevented completion: %+v", run)
			}
			for _, id := range []string{"a", "b"} {
				var payload struct {
					Unapplied []string `json:"unapplied"`
				}
				if err := json.Unmarshal(node(nodes, id).Payload, &payload); err != nil {
					t.Fatal(err)
				}
				// Neither node sets a retry policy; desktop's default retry applies to root Agents.
				if slices.Contains(payload.Unapplied, "mcp") {
					t.Fatalf("node %s retains unapplied MCP = %v", id, payload.Unapplied)
				}
			}
		})
	}
}
