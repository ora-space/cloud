package workflowhost

import (
	"fmt"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// promptWorkspaceRoot names the Agent's working directory. Desktop states the absolute worktree
// path; v2's execution service clones the run directory inside Node after Start accepts the
// prompt and never exposes its path, so the prompt names the working directory the Agent is
// already started in.
const promptWorkspaceRoot = "."

// agentPrompt is desktop's assemble_workflow_prompt (crates/backend/src/workflow/run/prompt.rs)
// without the parts v2 does not apply: required Skills, the role block and previous-failure
// injection (v2 never retries). The node's prompt is already rendered against the pool. Each
// section is one ACP text block, separated by a blank line because providers may concatenate
// block text verbatim.
func agentPrompt(d workflow.AgentDispatch, renderedTask string) execution.Prompt {
	c := promptCopyFor(d.Locale)
	sections := []string{
		workspaceRootBlock(d.Locale),
		currentStepBlock(d.Node, renderedTask, c),
		workflowContextBlock(d, c),
	}
	prompt := make(execution.Prompt, 0, len(sections))
	for i, text := range sections {
		if i < len(sections)-1 && !strings.HasSuffix(text, "\n\n") {
			text += "\n\n"
		}
		prompt = append(prompt, execution.TextPrompt(text)[0])
	}
	return prompt
}

func workspaceRootBlock(locale string) string {
	if locale == workflow.LocaleZhCN {
		return "<workspace_root>\n你的工作区根目录是：\n" + promptWorkspaceRoot + "\n\n该目录是本次工作流运行的主工作空间，项目文件、Git 仓库与构建产物通常都在其中。\n\n规则：\n1. 任务需要时，可以读取、搜索、创建、修改和删除该目录之外的文件——例如依赖源码、共享配置、其他仓库，或用户指定的路径。不要仅因为路径位于工作区之外就停下或拒绝任务。\n2. 运行项目命令时，以该工作区根目录作为工作目录，除非任务明确要求在别处执行。\n</workspace_root>"
	}
	return "<workspace_root>\nYour workspace root is:\n" + promptWorkspaceRoot + "\n\nThis directory is the primary workspace for this workflow run: the project files, Git repository, and build output normally live inside it.\n\nRules:\n1. When the task requires it, you may read, search, create, modify, and delete files outside this directory — dependency sources, shared configuration, other repositories, or paths the user points you at. Do not stop or refuse a task merely because a path lies outside the workspace.\n2. Run project commands with this workspace root as the working directory, unless the task explicitly calls for running them elsewhere.\n</workspace_root>"
}

func currentStepBlock(n workflow.Node, task string, c promptCopy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<current_workflow_step>\n%s\n", c.currentStepIntro)
	fmt.Fprintf(&b, "%s: %s\n", c.step, nodeLabel(n))
	if d := strings.TrimSpace(n.Description); d != "" {
		fmt.Fprintf(&b, "%s:\n%s\n", c.description, d)
	}
	if t := strings.TrimSpace(task); t != "" {
		fmt.Fprintf(&b, "%s:\n%s\n", c.taskInstructions, t)
	}
	b.WriteString("</current_workflow_step>")
	return b.String()
}

// workflowContextBlock renders the topology with each node's live status. The dispatching wave
// read the attempts before creating this one, so the current node is shown running.
func workflowContextBlock(d workflow.AgentDispatch, c promptCopy) string {
	status := map[string]string{}
	for _, r := range d.NodeRuns {
		status[r.NodeID] = r.Status // later attempts of a node win, as desktop's map does
	}
	status[d.Node.ID] = workflow.StatusRunning
	var b strings.Builder
	fmt.Fprintf(&b, "<workflow_context>\n## %s\n%s\n", c.workflowOverview, c.workflowOverviewIntro)
	fmt.Fprintf(&b, "%s: %s\n", c.directPredecessors, nodeList(d.Graph.Predecessors(d.Node.ID), c))
	fmt.Fprintf(&b, "%s: %s\n", c.directSuccessors, nodeList(d.Graph.Successors(d.Node.ID), c))
	fmt.Fprintf(&b, "\n%s:\n", c.topologyAndStatus)
	for _, n := range d.Graph.TopologicalOrder() {
		label := c.notStarted
		if s, ok := status[n.ID]; ok {
			label = c.status[s]
		}
		current := ""
		if n.ID == d.Node.ID {
			current = c.currentMarker
		}
		fmt.Fprintf(&b, "- [%s%s] %s -> %s\n", label, current, nodeLabel(n), nodeList(d.Graph.Successors(n.ID), c))
	}
	if d.RunInput != nil && strings.TrimSpace(*d.RunInput) != "" {
		fmt.Fprintf(&b, "\n## %s\n%s\n%s\n", c.originalRequest, c.originalRequestIntro, strings.TrimSpace(*d.RunInput))
	}
	b.WriteString("</workflow_context>")
	return b.String()
}

func nodeLabel(n workflow.Node) string {
	if title := strings.TrimSpace(n.Title); title != "" {
		return fmt.Sprintf("\"%s\" (`%s`)", title, n.ID)
	}
	return "`" + n.ID + "`"
}

func nodeList(nodes []workflow.Node, c promptCopy) string {
	if len(nodes) == 0 {
		return c.none
	}
	labels := make([]string, len(nodes))
	for i, n := range nodes {
		labels[i] = nodeLabel(n)
	}
	return strings.Join(labels, ", ")
}

// promptCopy is desktop's PromptCopy, limited to the sections v2 renders.
type promptCopy struct {
	currentStepIntro, step, description, taskInstructions                string
	workflowOverview, workflowOverviewIntro, directPredecessors          string
	directSuccessors, topologyAndStatus, notStarted, currentMarker, none string
	originalRequest, originalRequestIntro                                string
	status                                                               map[string]string
}

func promptCopyFor(locale string) promptCopy {
	if locale == workflow.LocaleZhCN {
		return promptCopy{
			currentStepIntro:      "你负责执行下述工作流步骤。请专注于当前步骤，并让最终回复成为可供后续步骤直接使用的清晰交付内容。",
			step:                  "当前步骤",
			description:           "步骤说明",
			taskInstructions:      "任务要求",
			workflowOverview:      "工作流全景",
			workflowOverviewIntro: "以下是当前步骤开始时的完整工作流快照。请据此理解自己所处的位置及后续交付方向；除非当前任务明确要求，否则不要代替其他节点执行任务。",
			directPredecessors:    "直接前置节点",
			directSuccessors:      "直接后续节点",
			topologyAndStatus:     "执行拓扑与当前状态",
			notStarted:            "未开始",
			currentMarker:         "，当前节点",
			none:                  "（无）",
			originalRequest:       "工作流原始请求",
			originalRequestIntro:  "本次工作流由以下请求启动。请将其作为全局目标，同时优先遵守范围更具体的当前步骤要求。",
			status: map[string]string{
				workflow.StatusPending: "等待输入", workflow.StatusRunning: "运行中", workflow.StatusSucceeded: "已成功",
				workflow.StatusFailed: "已失败", workflow.StatusCancelled: "已取消",
			},
		}
	}
	return promptCopy{
		currentStepIntro:      "You are responsible for the workflow step identified below. Focus on this step and make your final response a clear handoff for downstream steps.",
		step:                  "Step",
		description:           "Description",
		taskInstructions:      "Task instructions",
		workflowOverview:      "Workflow overview",
		workflowOverviewIntro: "This is the complete workflow snapshot at the moment this step starts. Use it to understand your position and the downstream handoff; do not perform other nodes' tasks unless your current instructions explicitly require it.",
		directPredecessors:    "Direct predecessors",
		directSuccessors:      "Direct successors",
		topologyAndStatus:     "Execution topology and current status",
		notStarted:            "not_started",
		currentMarker:         ", current",
		none:                  "(none)",
		originalRequest:       "Original workflow request",
		originalRequestIntro:  "The workflow was started with the following request. Use it as global intent while obeying the narrower current-step instructions.",
		status: map[string]string{
			workflow.StatusPending: "awaiting_input", workflow.StatusRunning: "running", workflow.StatusSucceeded: "succeeded",
			workflow.StatusFailed: "failed", workflow.StatusCancelled: "cancelled",
		},
	}
}
