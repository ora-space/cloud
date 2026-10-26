package controlgrpc

import (
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// The JSON shapes below are the durable form of the contract messages. Conflicts compare this
// encoding, so a replayed dispatch with the same input matches and a different one does not.

func inputObject(in *controlpb.ExecutionInput) (core.Object, error) {
	if in == nil {
		return nil, invalidInput("invalid_dispatch")
	}
	switch spec := in.GetSpec().(type) {
	case *controlpb.ExecutionInput_Clone:
		return cloneObject(spec.Clone)
	case *controlpb.ExecutionInput_InstallPlugins:
		return core.Object{"kind": "install_plugins", "plugins": installObjects(spec.InstallPlugins.GetPlugins())}, nil
	case *controlpb.ExecutionInput_RemovePlugins:
		return core.Object{"kind": "remove_plugins", "plugins": removalObjects(spec.RemovePlugins.GetPlugins())}, nil
	case *controlpb.ExecutionInput_AgentSession:
		return sessionObject(spec.AgentSession)
	case *controlpb.ExecutionInput_DeliverRevision:
		return deliveryObject(spec.DeliverRevision)
	case *controlpb.ExecutionInput_DeliverScheduledArtifacts:
		return scheduledArtifactsObject(spec.DeliverScheduledArtifacts)
	default:
		return nil, invalidInput("invalid_dispatch")
	}
}

func input(o core.Object) *controlpb.ExecutionInput {
	switch o.S("kind") {
	case "install_plugins":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_InstallPlugins{InstallPlugins: &controlpb.InstallPluginsSpec{Plugins: installMessages(rows(o["plugins"]))}}}
	case "remove_plugins":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_RemovePlugins{RemovePlugins: &controlpb.RemovePluginsSpec{Plugins: removalMessages(rows(o["plugins"]))}}}
	case "agent_session":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_AgentSession{AgentSession: sessionMessage(o)}}
	case "deliver_revision":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_DeliverRevision{DeliverRevision: deliveryMessage(o)}}
	case "deliver_scheduled_artifacts":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_DeliverScheduledArtifacts{DeliverScheduledArtifacts: &controlpb.DeliverScheduledArtifactsSpec{SessionExecutionId: o.S("sessionExecutionId")}}}
	default:
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: cloneMessage(o)}}
	}
}

// cloneObject is the durable form of a Workspace clone input: a Git Server snapshot. Its shape is
// checked here so a malformed input never reaches the registry, where it could only conflict with
// the operation's frozen input (contract D1).
func cloneObject(spec *controlpb.CloneSpec) (core.Object, error) {
	snapshot, snapshotOK := core.ParseGitRef(spec.GetSnapshotRef())
	workspace, workspaceOK := core.ParseGitRef(spec.GetWorkspaceRef())
	if !gitRepositoryURL(spec.GetRepositoryUrl()) || !core.GitObjectID(spec.GetBaseCommit()) ||
		!snapshotOK || snapshot.Namespace != core.GitUpstreamSnapshot || snapshot.Name != spec.GetBaseCommit() ||
		!workspaceOK || workspace.Namespace != core.GitWorkspaceBranch || workspace.Name == "HEAD" {
		return nil, invalidInput("invalid_dispatch")
	}
	return core.Object{"kind": "clone", "repositoryUrl": spec.GetRepositoryUrl(), "snapshotRef": spec.GetSnapshotRef(), "baseCommit": spec.GetBaseCommit(), "workspaceRef": spec.GetWorkspaceRef()}, nil
}

func cloneMessage(o core.Object) *controlpb.CloneSpec {
	return &controlpb.CloneSpec{RepositoryUrl: o.S("repositoryUrl"), SnapshotRef: o.S("snapshotRef"), BaseCommit: o.S("baseCommit"), WorkspaceRef: o.S("workspaceRef")}
}

// gitRepositoryURL accepts the Git Server repository addresses Cloud writes into inputs: https,
// with a host, and no userinfo, query or fragment for a credential or option to hide in.
func gitRepositoryURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && strings.HasSuffix(u.Path, ".git")
}

func installObjects(plugins []*controlpb.PluginInstall) []core.Object {
	out := make([]core.Object, 0, len(plugins))
	for _, plugin := range plugins {
		item := core.Object{"pluginId": plugin.GetPluginId(), "version": plugin.GetVersion()}
		if plugin.GetUniversal() != nil {
			item["universal"] = core.Object{"url": plugin.GetUniversal().GetUrl(), "sha256": plugin.GetUniversal().GetSha256()}
		}
		if len(plugin.GetTargets()) > 0 {
			targets := make([]core.Object, 0, len(plugin.GetTargets()))
			for _, target := range plugin.GetTargets() {
				targets = append(targets, core.Object{"target": target.GetTarget(), "url": target.GetDownload().GetUrl(), "sha256": target.GetDownload().GetSha256()})
			}
			item["targets"] = targets
		}
		if agent := plugin.GetAgentConfiguration(); agent != nil {
			item["agentConfiguration"] = core.Object{"schemaVersion": agent.GetSchemaVersion(), "declarationSha256": agent.GetDeclarationSha256()}
		}
		out = append(out, item)
	}
	return out
}

func installMessages(plugins []core.Object) []*controlpb.PluginInstall {
	out := make([]*controlpb.PluginInstall, 0, len(plugins))
	for _, plugin := range plugins {
		item := &controlpb.PluginInstall{PluginId: plugin.S("pluginId"), Version: plugin.S("version")}
		if universal := plugin.O("universal"); universal.S("url") != "" {
			item.Universal = &controlpb.PluginDownload{Url: universal.S("url"), Sha256: universal.S("sha256")}
		}
		for _, target := range rows(plugin["targets"]) {
			item.Targets = append(item.Targets, &controlpb.PluginTargetDownload{Target: target.S("target"), Download: &controlpb.PluginDownload{Url: target.S("url"), Sha256: target.S("sha256")}})
		}
		if agent, declared := core.PluginAgentConfiguration(plugin); declared {
			item.AgentConfiguration = &controlpb.PluginAgentConfiguration{SchemaVersion: boundedSchemaVersion(agent.N("schemaVersion")), DeclarationSha256: agent.S("declarationSha256")}
		}
		out = append(out, item)
	}
	return out
}

func removalObjects(plugins []*controlpb.PluginRemoval) []core.Object {
	out := make([]core.Object, 0, len(plugins))
	for _, plugin := range plugins {
		out = append(out, core.Object{"pluginId": plugin.GetPluginId(), "version": plugin.GetVersion()})
	}
	return out
}

func removalMessages(plugins []core.Object) []*controlpb.PluginRemoval {
	out := make([]*controlpb.PluginRemoval, 0, len(plugins))
	for _, plugin := range plugins {
		out = append(out, &controlpb.PluginRemoval{PluginId: plugin.S("pluginId"), Version: plugin.S("version")})
	}
	return out
}

func sessionObject(spec *controlpb.AgentSessionSpec) (core.Object, error) {
	if spec.GetAgentPluginId() == "" || spec.GetCheckoutExecutionId() == "" || spec.GetGitIdentity().GetName() == "" {
		return nil, invalidInput("invalid_dispatch")
	}
	turn := spec.GetInitialTurn()
	out := core.Object{
		"kind": "agent_session", "agentPluginId": spec.GetAgentPluginId(), "agentPluginVersion": spec.GetAgentPluginVersion(),
		"checkoutExecutionId": spec.GetCheckoutExecutionId(),
		"gitIdentity":         core.Object{"name": spec.GetGitIdentity().GetName(), "email": spec.GetGitIdentity().GetEmail()},
		"initialTurn":         core.Object{"turnId": turn.GetTurnId(), "content": contentObjects(turn.GetContent())},
	}
	if id := spec.GetModelBindingId(); id != "" {
		out["modelBindingId"] = id
	}
	if configuration := spec.GetAgentConfiguration(); configuration != nil {
		frozen, err := agentConfigurationObject(configuration)
		if err != nil {
			return nil, err
		}
		out["agentConfiguration"] = frozen
	}
	if prior := spec.GetPriorRevision(); prior != nil {
		out["priorRevision"] = priorObject(prior)
	}
	if listings := spec.GetWorkspaceDirectory(); len(listings) > 0 {
		directory := make([]core.Object, 0, len(listings))
		for _, l := range listings {
			listing := core.Object{"workspaceId": l.GetWorkspaceId(), "kind": l.GetKind()}
			if l.GetIssueRunId() != "" {
				listing["issueId"], listing["issueTitle"], listing["issueRunId"], listing["runStatus"] = l.GetIssueId(), l.GetIssueTitle(), l.GetIssueRunId(), l.GetRunStatus()
			}
			if l.GetWorkflowRunId() != "" {
				listing["workflowRunId"], listing["workflowName"], listing["runStatus"] = l.GetWorkflowRunId(), l.GetWorkflowName(), l.GetRunStatus()
			}
			directory = append(directory, listing)
		}
		out["workspaceDirectory"] = directory
	}
	// The admission is part of the frozen input: a dispatch that disagrees with the work item about
	// whether this run is a scheduled occurrence conflicts like any other changed field.
	switch spec.GetTrigger() {
	case controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_UNSPECIFIED:
	case controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_SCHEDULED:
		out["trigger"] = "scheduled"
	default:
		return nil, invalidInput("invalid_dispatch")
	}
	return out, nil
}

// scheduledArtifactsObject is the durable form of the artifact-upload input: it names the ended
// session whose declaration Cloud persisted and never a location. Object keys belong to Cloud and
// reach the Node only through an upload grant.
func scheduledArtifactsObject(spec *controlpb.DeliverScheduledArtifactsSpec) (core.Object, error) {
	if spec.GetSessionExecutionId() == "" {
		return nil, invalidInput("invalid_dispatch")
	}
	return core.Object{"kind": "deliver_scheduled_artifacts", "sessionExecutionId": spec.GetSessionExecutionId()}, nil
}

// priorObject is the durable form of a PriorRevision. A session spec's names where the Node fetches
// it and what it was based on; a delivery spec's carries only the identity and final commit.
func priorObject(prior *controlpb.PriorRevision) core.Object {
	out := core.Object{"revisionId": prior.GetRevisionId(), "finalCommit": prior.GetFinalCommit()}
	if prior.GetGitRepositoryUrl() != "" || prior.GetDeliveryRef() != "" || prior.GetBaseCommit() != "" {
		out["gitRepositoryUrl"], out["deliveryRef"], out["baseCommit"] = prior.GetGitRepositoryUrl(), prior.GetDeliveryRef(), prior.GetBaseCommit()
	}
	return out
}

func priorMessage(o core.Object) *controlpb.PriorRevision {
	if o.S("revisionId") == "" {
		return nil
	}
	return &controlpb.PriorRevision{RevisionId: o.S("revisionId"), FinalCommit: o.S("finalCommit"), GitRepositoryUrl: o.S("gitRepositoryUrl"), DeliveryRef: o.S("deliveryRef"), BaseCommit: o.S("baseCommit")}
}

func directoryMessages(rows []core.Object) []*controlpb.WorkspaceListing {
	out := make([]*controlpb.WorkspaceListing, 0, len(rows))
	for _, row := range rows {
		out = append(out, &controlpb.WorkspaceListing{WorkspaceId: row.S("workspaceId"), Kind: row.S("kind"), IssueId: row.S("issueId"), IssueTitle: row.S("issueTitle"), IssueRunId: row.S("issueRunId"), RunStatus: row.S("runStatus"), WorkflowRunId: row.S("workflowRunId"), WorkflowName: row.S("workflowName")})
	}
	return out
}

func sessionMessage(o core.Object) *controlpb.AgentSessionSpec {
	git := o.O("gitIdentity")
	turn := o.O("initialTurn")
	return &controlpb.AgentSessionSpec{
		AgentPluginId: o.S("agentPluginId"), AgentPluginVersion: o.S("agentPluginVersion"), CheckoutExecutionId: o.S("checkoutExecutionId"),
		ModelBindingId:     o.S("modelBindingId"),
		AgentConfiguration: agentConfigurationMessage(o.O("agentConfiguration")),
		GitIdentity:        &controlpb.GitIdentity{Name: git.S("name"), Email: git.S("email")},
		InitialTurn:        &controlpb.UserTurn{TurnId: turn.S("turnId"), Content: contentMessages(rows(turn["content"]))},
		PriorRevision:      priorMessage(o.O("priorRevision")),
		WorkspaceDirectory: directoryMessages(rows(o["workspaceDirectory"])),
		Trigger:            sessionTriggerMessage(o),
	}
}

// sessionTriggerMessage is the admission Cloud froze for this session. An absent field is a manual
// IssueRun, which is also how every session released before the field existed reads.
func sessionTriggerMessage(o core.Object) controlpb.AgentSessionTrigger {
	if o.S("trigger") == "scheduled" {
		return controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_SCHEDULED
	}
	return controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_UNSPECIFIED
}

func contentObjects(blocks []*controlpb.ContentBlock) []core.Object {
	out := make([]core.Object, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, core.Object{"text": block.GetText().GetText()})
	}
	return out
}

func contentMessages(blocks []core.Object) []*controlpb.ContentBlock {
	out := make([]*controlpb.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, &controlpb.ContentBlock{Block: &controlpb.ContentBlock_Text{Text: &controlpb.TextContent{Text: block.S("text")}}})
	}
	return out
}

func deliveryObject(spec *controlpb.DeliverRevisionSpec) (core.Object, error) {
	delivery, deliveryOK := core.ParseGitRef(spec.GetDeliveryRef())
	workspace, workspaceOK := core.ParseGitRef(spec.GetWorkspaceRef())
	if (spec.GetSessionExecutionId() == "" && spec.GetWorkflow() == nil) || spec.GetHistoryKey() == "" || !gitRepositoryURL(spec.GetGitRepositoryUrl()) ||
		!deliveryOK || delivery.Namespace != core.GitDelivery || !workspaceOK || workspace.Namespace != core.GitWorkspaceBranch {
		return nil, invalidInput("invalid_dispatch")
	}
	out := core.Object{
		"kind": "deliver_revision", "sessionExecutionId": spec.GetSessionExecutionId(), "checkoutExecutionId": spec.GetCheckoutExecutionId(),
		"baseCommit": spec.GetBaseCommit(), "revisionRef": spec.GetRevisionRef(), "historyKey": spec.GetHistoryKey(),
		"gitRepositoryUrl": spec.GetGitRepositoryUrl(), "deliveryRef": spec.GetDeliveryRef(), "workspaceRef": spec.GetWorkspaceRef(),
	}
	if prior := spec.GetPriorRevision(); prior != nil {
		out["priorRevision"] = priorObject(prior)
	}
	if workflow := spec.GetWorkflow(); workflow != nil {
		if spec.GetSessionExecutionId() != "" || spec.GetPriorRevision() != nil || workflow.GetWorkflowRunId() == "" || len(workflow.GetSessions()) == 0 {
			return nil, invalidInput("invalid_dispatch")
		}
		sessions := make([]core.Object, 0, len(workflow.GetSessions()))
		for _, attempt := range workflow.GetSessions() {
			sessions = append(sessions, core.Object{"executionOrder": int64(attempt.GetExecutionOrder()), "nodeId": attempt.GetNodeId(), "attempt": int64(attempt.GetAttempt()), "executionId": attempt.GetExecutionId(), "historyKey": attempt.GetHistoryKey()})
		}
		out["workflow"] = core.Object{"workflowRunId": workflow.GetWorkflowRunId(), "sessions": sessions}
	}
	return out, nil
}

func deliveryMessage(o core.Object) *controlpb.DeliverRevisionSpec {
	out := &controlpb.DeliverRevisionSpec{
		SessionExecutionId: o.S("sessionExecutionId"), CheckoutExecutionId: o.S("checkoutExecutionId"), BaseCommit: o.S("baseCommit"),
		RevisionRef: o.S("revisionRef"), HistoryKey: o.S("historyKey"),
		GitRepositoryUrl: o.S("gitRepositoryUrl"), DeliveryRef: o.S("deliveryRef"), WorkspaceRef: o.S("workspaceRef"),
		PriorRevision: priorMessage(o.O("priorRevision")),
	}
	if workflow := o.O("workflow"); len(workflow) > 0 {
		out.Workflow = &controlpb.WorkflowHistoryInput{WorkflowRunId: workflow.S("workflowRunId")}
		for _, attempt := range rows(workflow["sessions"]) {
			out.Workflow.Sessions = append(out.Workflow.Sessions, &controlpb.WorkflowSessionAttempt{ExecutionOrder: workflowOrdinal(attempt.N("executionOrder")), NodeId: attempt.S("nodeId"), Attempt: workflowOrdinal(attempt.N("attempt")), ExecutionId: attempt.S("executionId"), HistoryKey: attempt.S("historyKey")})
		}
	}
	return out
}

// workflowOrdinal refuses overflow instead of silently changing a frozen attempt identity.
func workflowOrdinal(value int64) uint32 {
	if value < 0 || value > math.MaxUint32 {
		return 0
	}
	return uint32(value)
}

func resultObject(res *controlpb.ExecutionResult) (core.Object, error) {
	node := res.GetNode()
	if node == nil || node.GetNodeId() == "" || node.GetNodeIncarnationId() == "" {
		return nil, invalidInput("invalid_result")
	}
	out := core.Object{"node": core.Object{"nodeId": node.GetNodeId(), "nodeIncarnationId": node.GetNodeIncarnationId()}}
	switch outcome := res.GetOutcome().(type) {
	case *controlpb.ExecutionResult_CloneReady:
		out["outcome"], out["path"], out["commit"] = "clone_ready", outcome.CloneReady.GetPath(), outcome.CloneReady.GetCommit()
	case *controlpb.ExecutionResult_CloneFailed:
		out["outcome"], out["reason"] = "clone_failed", outcome.CloneFailed.GetReason().String()
		if outcome.CloneFailed.RetainedPath != nil {
			out["retainedPath"] = outcome.CloneFailed.GetRetainedPath()
		}
	case *controlpb.ExecutionResult_PluginsResult:
		out["outcome"] = "plugins_result"
		out["items"] = pluginItemObjects(outcome.PluginsResult.GetItems())
	case *controlpb.ExecutionResult_PluginsFailed:
		out["outcome"], out["reason"] = "plugins_failed", shortEnum(outcome.PluginsFailed.GetReason().String(), "PLUGINS_FAILURE_REASON_")
	case *controlpb.ExecutionResult_AgentSessionEnded:
		out["outcome"], out["reason"] = "agent_session_ended", shortEnum(outcome.AgentSessionEnded.GetReason().String(), "AGENT_SESSION_END_REASON_")
		ended := outcome.AgentSessionEnded
		if ended.Detail != nil {
			out["detail"] = ended.GetDetail()
		}
		// The business verdict stays a structured value Cloud only validates: it is never derived
		// from the Thread, the end reason or the execution status, and it travels only when the
		// executor reported it (an unspecified verdict is a malformed report, not "not reported").
		if result := ended.GetScheduledResult(); result != nil {
			if result.GetVerdict() == controlpb.ScheduledVerdict_SCHEDULED_VERDICT_UNSPECIFIED {
				return nil, invalidInput("invalid_result")
			}
			scheduled := core.Object{"verdict": shortEnum(result.GetVerdict().String(), "SCHEDULED_VERDICT_")}
			if result.VerdictDetail != nil {
				if utf8.RuneCountInString(result.GetVerdictDetail()) > core.MaxScheduledRunVerdictDetailRunes {
					return nil, invalidInput("invalid_result")
				}
				scheduled["verdictDetail"] = result.GetVerdictDetail()
			}
			out["scheduledResult"] = scheduled
		}
		// The declaration is the executor's report of what it will upload; it travels with the
		// terminal event so Cloud can freeze the window and hand out grants. Dropping it here would
		// lose the run's artifacts silently - the settlement would see an empty manifest and stop -
		// so the boundary either carries it whole or refuses it.
		if declared := ended.GetArtifactManifest(); declared != nil {
			items := make([]core.Object, 0, len(declared.GetItems()))
			for _, item := range declared.GetItems() {
				if item.GetSizeBytes() > math.MaxInt64 {
					return nil, invalidInput("invalid_result")
				}
				kind := shortEnum(item.GetKind().String(), "SCHEDULED_ARTIFACT_KIND_")
				if kind == "" {
					return nil, invalidInput("invalid_result")
				}
				items = append(items, core.Object{
					// The durable object stores plain integers: N() reads float64/int64, so a uint32 or
					// uint64 kept as-is would read back as zero and silently lose the index or size.
					"index": int64(item.GetIndex()), "displayName": item.GetDisplayName(), "kind": kind,
					"sizeBytes": int64(item.GetSizeBytes()), "sha256": item.GetSha256(), // #nosec G115 -- values above MaxInt64 are refused above.
				})
			}
			rejections := make([]core.Object, 0, len(declared.GetRejections()))
			for _, rejection := range declared.GetRejections() {
				reason := shortEnum(rejection.GetReason().String(), "SCHEDULED_ARTIFACT_REJECTION_REASON_")
				if reason == "" {
					return nil, invalidInput("invalid_result")
				}
				rejections = append(rejections, core.Object{"displayName": rejection.GetDisplayName(), "reason": reason})
			}
			out["artifactManifest"] = core.Object{"items": items, "rejections": rejections}
			if declared.GetOmittedRejections() != 0 {
				out.O("artifactManifest")["omittedRejections"] = int64(declared.GetOmittedRejections())
			}
			if declared.GetWalkTruncated() {
				out.O("artifactManifest")["walkTruncated"] = true
			}
		}
	case *controlpb.ExecutionResult_RevisionDelivered:
		out["outcome"] = "revision_delivered"
		delivered := outcome.RevisionDelivered
		out["finalCommit"], out["baseCommit"], out["revisionRef"] = delivered.GetFinalCommit(), delivered.GetBaseCommit(), delivered.GetRevisionRef()
		out["history"] = storedObject(delivered.GetHistory())
	case *controlpb.ExecutionResult_RevisionUnchanged:
		out["outcome"] = "revision_unchanged"
		unchanged := outcome.RevisionUnchanged
		out["finalCommit"], out["baseCommit"], out["revisionRef"] = unchanged.GetFinalCommit(), unchanged.GetBaseCommit(), unchanged.GetRevisionRef()
		out["history"] = storedObject(unchanged.GetHistory())
	case *controlpb.ExecutionResult_ScheduledArtifactsDelivered:
		out["outcome"] = "scheduled_artifacts_delivered"
		artifacts := outcome.ScheduledArtifactsDelivered.GetArtifacts()
		uploaded := make([]core.Object, 0, len(artifacts))
		for _, artifact := range artifacts {
			if artifact == nil || artifact.GetSize() > math.MaxInt64 {
				return nil, invalidInput("invalid_result")
			}
			uploaded = append(uploaded, storedObject(artifact))
		}
		out["artifacts"] = uploaded
	case *controlpb.ExecutionResult_ScheduledArtifactsFailed:
		out["outcome"], out["reason"] = "scheduled_artifacts_failed", shortEnum(outcome.ScheduledArtifactsFailed.GetReason().String(), "SCHEDULED_ARTIFACTS_FAILURE_REASON_")
	case *controlpb.ExecutionResult_RevisionFailed:
		out["outcome"], out["reason"] = "revision_failed", shortEnum(outcome.RevisionFailed.GetReason().String(), "REVISION_FAILURE_REASON_")
	default:
		return nil, invalidInput("invalid_result")
	}
	return out, nil
}

func pluginItemObjects(items []*controlpb.PluginItemResult) []core.Object {
	out := make([]core.Object, 0, len(items))
	for _, item := range items {
		row := core.Object{"pluginId": item.GetPluginId()}
		switch outcome := item.GetOutcome().(type) {
		case *controlpb.PluginItemResult_Installed:
			row["outcome"], row["version"] = "installed", outcome.Installed.GetVersion()
		case *controlpb.PluginItemResult_Removed:
			row["outcome"] = "removed"
		case *controlpb.PluginItemResult_Failed:
			row["outcome"], row["reason"] = "failed", shortEnum(outcome.Failed.GetReason().String(), "PLUGIN_FAILURE_REASON_")
		}
		out = append(out, row)
	}
	return out
}

func storedObject(o *controlpb.StoredObject) core.Object {
	if o == nil {
		return nil
	}
	size := o.GetSize()
	stored := int64(0)
	if size <= math.MaxInt64 {
		stored = int64(size) // #nosec G115 -- bounded by MaxInt64 above.
	}
	return core.Object{"key": o.GetKey(), "size": stored, "sha256": o.GetSha256()}
}

func result(o core.Object) *controlpb.ExecutionResult {
	node := o.O("node")
	out := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node.S("nodeId"), NodeIncarnationId: node.S("nodeIncarnationId")}}
	switch o.S("outcome") {
	case "clone_ready":
		out.Outcome = &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: o.S("path"), Commit: o.S("commit")}}
	case "plugins_result":
		out.Outcome = &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{Items: pluginItemMessages(rows(o["items"]))}}
	case "plugins_failed":
		out.Outcome = &controlpb.ExecutionResult_PluginsFailed{PluginsFailed: &controlpb.PluginsFailed{Reason: controlpb.PluginsFailureReason(enum(controlpb.PluginsFailureReason_value, "PLUGINS_FAILURE_REASON_", o.S("reason")))}}
	case "agent_session_ended":
		ended := &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason(enum(controlpb.AgentSessionEndReason_value, "AGENT_SESSION_END_REASON_", o.S("reason")))}
		if detail := o.S("detail"); detail != "" {
			ended.Detail = &detail
		}
		// A stored verdict returns to the Controller as the same structured value it reported, so a
		// recovered execution is queried with the business result it actually carried.
		if scheduled := o.O("scheduledResult"); len(scheduled) > 0 {
			reported := &controlpb.ScheduledRunResult{Verdict: controlpb.ScheduledVerdict(enum(controlpb.ScheduledVerdict_value, "SCHEDULED_VERDICT_", scheduled.S("verdict")))}
			if detail := scheduled.S("verdictDetail"); detail != "" {
				reported.VerdictDetail = &detail
			}
			ended.ScheduledResult = reported
		}
		// A stored declaration returns to the Controller as the same structure it reported, so a
		// recovered session is queried with the artifacts it actually carried.
		if declared := o.O("artifactManifest"); len(declared) > 0 {
			manifest := &controlpb.ScheduledArtifactManifest{
				OmittedRejections: boundedSchemaVersion(declared.N("omittedRejections")), WalkTruncated: declared.B("walkTruncated"),
			}
			for _, item := range objectsOf(declared["items"]) {
				index, size := item.N("index"), item.N("sizeBytes")
				if index < 0 || index > math.MaxUint32 {
					index = 0
				}
				if size < 0 {
					size = 0
				}
				manifest.Items = append(manifest.Items, &controlpb.ScheduledArtifactItem{
					Index: uint32(index), DisplayName: item.S("displayName"), // #nosec G115 -- bounded above.
					Kind:      controlpb.ScheduledArtifactKind(enum(controlpb.ScheduledArtifactKind_value, "SCHEDULED_ARTIFACT_KIND_", item.S("kind"))),
					SizeBytes: uint64(size), Sha256: item.S("sha256"), // #nosec G115 -- nonnegative above.
				})
			}
			for _, rejection := range objectsOf(declared["rejections"]) {
				manifest.Rejections = append(manifest.Rejections, &controlpb.ScheduledArtifactRejection{
					DisplayName: rejection.S("displayName"),
					Reason:      controlpb.ScheduledArtifactRejectionReason(enum(controlpb.ScheduledArtifactRejectionReason_value, "SCHEDULED_ARTIFACT_REJECTION_REASON_", rejection.S("reason"))),
				})
			}
			ended.ArtifactManifest = manifest
		}
		out.Outcome = &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: ended}
	case "revision_delivered":
		out.Outcome = &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{FinalCommit: o.S("finalCommit"), BaseCommit: o.S("baseCommit"), RevisionRef: o.S("revisionRef"), History: storedMessage(o.O("history"))}}
	case "revision_unchanged":
		out.Outcome = &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{FinalCommit: o.S("finalCommit"), BaseCommit: o.S("baseCommit"), RevisionRef: o.S("revisionRef"), History: storedMessage(o.O("history"))}}
	case "revision_failed":
		out.Outcome = &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: controlpb.RevisionFailureReason(enum(controlpb.RevisionFailureReason_value, "REVISION_FAILURE_REASON_", o.S("reason")))}}
	case "scheduled_artifacts_delivered":
		delivered := &controlpb.ScheduledArtifactsDelivered{}
		for _, artifact := range rows(o["artifacts"]) {
			delivered.Artifacts = append(delivered.Artifacts, storedMessage(artifact))
		}
		out.Outcome = &controlpb.ExecutionResult_ScheduledArtifactsDelivered{ScheduledArtifactsDelivered: delivered}
	case "scheduled_artifacts_failed":
		out.Outcome = &controlpb.ExecutionResult_ScheduledArtifactsFailed{ScheduledArtifactsFailed: &controlpb.ScheduledArtifactsFailed{Reason: controlpb.ScheduledArtifactsFailureReason(enum(controlpb.ScheduledArtifactsFailureReason_value, "SCHEDULED_ARTIFACTS_FAILURE_REASON_", o.S("reason")))}}
	default:
		failed := &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason(controlpb.CloneFailureReason_value[o.S("reason")])}
		if retained, ok := o["retainedPath"].(string); ok {
			failed.RetainedPath = &retained
		}
		out.Outcome = &controlpb.ExecutionResult_CloneFailed{CloneFailed: failed}
	}
	return out
}

func pluginItemMessages(items []core.Object) []*controlpb.PluginItemResult {
	out := make([]*controlpb.PluginItemResult, 0, len(items))
	for _, item := range items {
		row := &controlpb.PluginItemResult{PluginId: item.S("pluginId")}
		switch item.S("outcome") {
		case "installed":
			row.Outcome = &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: item.S("version")}}
		case "removed":
			row.Outcome = &controlpb.PluginItemResult_Removed{Removed: &controlpb.PluginItemRemoved{}}
		default:
			row.Outcome = &controlpb.PluginItemResult_Failed{Failed: &controlpb.PluginItemFailed{Reason: controlpb.PluginFailureReason(enum(controlpb.PluginFailureReason_value, "PLUGIN_FAILURE_REASON_", item.S("reason")))}}
		}
		out = append(out, row)
	}
	return out
}

func storedMessage(o core.Object) *controlpb.StoredObject {
	size := o.N("size")
	if size < 0 {
		size = 0
	}
	return &controlpb.StoredObject{Key: o.S("key"), Size: uint64(size), Sha256: o.S("sha256")}
}

func record(row core.Object) *controlpb.ExecutionRecord {
	out := &controlpb.ExecutionRecord{OperationId: row.S("operationId"), NodeOperationId: row.S("nodeOperationId"), ExecutionId: row.S("executionId"), NodeId: row.S("nodeId"), Input: input(row.O("input"))}
	if res := row.O("result"); len(res) > 0 {
		out.Result = result(res)
	}
	return out
}

func shortEnum(enumName, prefix string) string { return name(enumName, prefix) }

// grantMessage converts one in-memory upload grant. The URL is not read back from the database.
func grantMessage(o core.Object) *controlpb.UploadGrant {
	headers := map[string]string{}
	switch raw := o["headers"].(type) {
	case map[string]string:
		headers = raw
	case map[string]any:
		for k, v := range raw {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}
	grant := &controlpb.UploadGrant{ObjectKey: o.S("objectKey"), Url: o.S("url"), Method: o.S("method"), Headers: headers}
	if exp, ok := o["expiresAt"].(time.Time); ok {
		grant.ExpiresAt = timestamppb.New(exp)
	}
	return grant
}

// invalidInput rejects a malformed execution input or result before it reaches the registry. It is
// a 400 Fault mapped in fault.go, so the status carries ErrorDetail INVALID_INPUT like every other
// invalid argument the contract reports.
func invalidInput(code string) error {
	return toStatus(&core.Fault{Code: code, Status: 400, Params: core.Object{}})
}

// boundedSchemaVersion converts the durable plan's schema version without overflowing: a value
// outside u32 cannot have passed the sync's validation, so mapping it to zero keeps the wire
// input unequal to the frozen plan and the dispatch is refused rather than truncated.
func boundedSchemaVersion(value int64) uint32 {
	if value < 0 || value > math.MaxUint32 {
		return 0
	}
	return uint32(value) // #nosec G115 -- bounded above.
}

// objectsOf decodes an array of objects out of a stored result. The value arrives either as the typed
// slice the codec just built or as the generic form jsonb returns, so both are accepted rather than
// making one caller reshape what the other produced.
func objectsOf(value any) []core.Object {
	switch rows := value.(type) {
	case []core.Object:
		return rows
	case []any:
		out := make([]core.Object, 0, len(rows))
		for _, row := range rows {
			switch item := row.(type) {
			case core.Object:
				out = append(out, item)
			case map[string]any:
				out = append(out, core.Object(item))
			}
		}
		return out
	default:
		return nil
	}
}
