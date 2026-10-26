package controlgrpc

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// A scheduled session crosses the contract as the admission Cloud froze: the trigger belongs to the
// durable input, so a dispatch that drops or changes it conflicts with the work item instead of
// silently running a scheduled occurrence as a manual session.
func TestScheduledSessionTriggerRoundTrip(t *testing.T) {
	const scheduled = `{"agentPluginId":"official/a","agentPluginVersion":"1.0.0","checkoutExecutionId":"checkout",` +
		`"gitIdentity":{"email":"ada@example.invalid","name":"Ada"},` +
		`"initialTurn":{"content":[{"text":"review"}],"turnId":"turn-1"},` +
		`"kind":"agent_session","trigger":"scheduled"}`
	const manual = `{"agentPluginId":"official/a","agentPluginVersion":"1.0.0","checkoutExecutionId":"checkout",` +
		`"gitIdentity":{"email":"ada@example.invalid","name":"Ada"},` +
		`"initialTurn":{"content":[{"text":"review"}],"turnId":"turn-1"},"kind":"agent_session"}`

	for name, tc := range map[string]struct {
		frozen  string
		trigger controlpb.AgentSessionTrigger
	}{
		"scheduled": {frozen: scheduled, trigger: controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_SCHEDULED},
		"manual":    {frozen: manual, trigger: controlpb.AgentSessionTrigger_AGENT_SESSION_TRIGGER_UNSPECIFIED},
	} {
		t.Run(name, func(t *testing.T) {
			var frozen core.Object
			if err := json.Unmarshal([]byte(tc.frozen), &frozen); err != nil {
				t.Fatal(err)
			}
			message := sessionMessage(frozen)
			if message.GetTrigger() != tc.trigger {
				t.Fatalf("trigger = %v, want %v", message.GetTrigger(), tc.trigger)
			}
			back, err := sessionObject(message)
			if err != nil {
				t.Fatal(err)
			}
			text, err := json.Marshal(back)
			if err != nil {
				t.Fatal(err)
			}
			// The dispatch equality gate compares this exact encoding, so the round trip must
			// reproduce the frozen input byte for byte.
			if string(text) != tc.frozen {
				t.Fatalf("round trip must reproduce the frozen input:\n got %s\nwant %s", text, tc.frozen)
			}
		})
	}
}

// The business verdict crosses as a structured value Cloud validates and stores verbatim; an
// unspecified verdict or an over-long detail is a malformed result rather than "not reported".
func TestScheduledRunResultCrossesAsAStructuredValue(t *testing.T) {
	node := &controlpb.NodeIdentity{NodeId: "node-1", NodeIncarnationId: "incarnation-1"}
	endedResult := func(detail *string, verdict controlpb.ScheduledVerdict) *controlpb.ExecutionResult {
		return &controlpb.ExecutionResult{
			Node: node,
			Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{
				Reason:          controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_IDLE_TIMEOUT,
				ScheduledResult: &controlpb.ScheduledRunResult{Verdict: verdict, VerdictDetail: detail},
			}},
		}
	}

	stored, err := resultObject(endedResult(proto.String("no comparison base"), controlpb.ScheduledVerdict_SCHEDULED_VERDICT_RANGE_UNRESOLVED))
	if err != nil {
		t.Fatal(err)
	}
	if want := (core.Object{"verdict": "range_unresolved", "verdictDetail": "no comparison base"}); !objectEqual(stored.O("scheduledResult"), want) {
		t.Fatalf("stored scheduled result = %+v, want %+v", stored.O("scheduledResult"), want)
	}
	// A stored result returns to the Controller with the business facts it carried, so a recovered
	// dispatch is queried exactly as it reported.
	back := result(stored).GetAgentSessionEnded().GetScheduledResult()
	if back.GetVerdict() != controlpb.ScheduledVerdict_SCHEDULED_VERDICT_RANGE_UNRESOLVED || back.GetVerdictDetail() != "no comparison base" {
		t.Fatalf("stored verdict did not survive the round trip: %v", back)
	}
	// A run that reported no verdict stores no field, which reads as "not reported".
	bare, err := resultObject(&controlpb.ExecutionResult{
		Node: node,
		Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{
			Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bare["scheduledResult"] != nil {
		t.Fatalf("an unreported verdict must store no field, got %+v", bare["scheduledResult"])
	}
	if result(bare).GetAgentSessionEnded().GetScheduledResult() != nil {
		t.Fatal("an unreported verdict must also remain absent in a recovery query")
	}
	if _, err := resultObject(endedResult(nil, controlpb.ScheduledVerdict_SCHEDULED_VERDICT_UNSPECIFIED)); err == nil {
		t.Fatal("an unspecified verdict must be refused")
	}
	if _, err := resultObject(endedResult(proto.String(string(make([]byte, core.MaxScheduledRunVerdictDetailRunes+1))), controlpb.ScheduledVerdict_SCHEDULED_VERDICT_FAIL)); err == nil {
		t.Fatal("an over-long verdict detail must be refused")
	}
}

// The declared artifacts cross the contract in both directions. Losing them at this seam is silent:
// Cloud would freeze an empty window and never ask the Node to upload anything.
func TestScheduledArtifactManifestRoundTripsThroughTheResult(t *testing.T) {
	declared := &controlpb.ScheduledArtifactManifest{
		OmittedRejections: 448, WalkTruncated: true,
		Items: []*controlpb.ScheduledArtifactItem{
			{Index: 0, DisplayName: "reports/review.md", Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_MARKDOWN, SizeBytes: 5, Sha256: strings.Repeat("ab", 32)},
			{Index: 1, DisplayName: "reports/result.json", Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_JSON, SizeBytes: 7, Sha256: strings.Repeat("cd", 32)},
		},
		Rejections: []*controlpb.ScheduledArtifactRejection{
			{DisplayName: "notes.pdf", Reason: controlpb.ScheduledArtifactRejectionReason_SCHEDULED_ARTIFACT_REJECTION_REASON_UNSUPPORTED_TYPE},
		},
	}
	stored, err := resultObject(&controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: "node-1", NodeIncarnationId: "incarnation-1"},
		Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{
			Reason:           controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_IDLE_TIMEOUT,
			ArtifactManifest: declared,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := stored.O("artifactManifest")
	if items := objectsOf(manifest["items"]); len(items) != 2 ||
		items[0].S("displayName") != "reports/review.md" || items[0].S("kind") != "markdown" ||
		items[0].N("sizeBytes") != 5 || items[0].S("sha256") != strings.Repeat("ab", 32) ||
		items[1].N("index") != 1 || items[1].S("kind") != "json" {
		t.Fatalf("stored manifest items = %+v, want both declared items verbatim", manifest["items"])
	}
	if rejections := objectsOf(manifest["rejections"]); len(rejections) != 1 ||
		rejections[0].S("displayName") != "notes.pdf" || rejections[0].S("reason") != "unsupported_type" {
		t.Fatalf("stored manifest rejections = %+v, want the refusal recorded", manifest["rejections"])
	}
	// The stored declaration returns to the Controller unchanged, including after a jsonb round trip.
	var decoded core.Object
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	back := result(decoded).GetAgentSessionEnded().GetArtifactManifest()
	if !proto.Equal(back, declared) {
		t.Fatalf("manifest did not survive the round trip:\n got %v\nwant %v", back, declared)
	}
	// A run without artifacts carries no manifest, which is what "declared nothing" looks like.
	bare, err := resultObject(&controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: "node-1", NodeIncarnationId: "incarnation-1"},
		Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{
			Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bare["artifactManifest"] != nil {
		t.Fatalf("a run that declared nothing must store no manifest, got %+v", bare["artifactManifest"])
	}
}

// objectEqual compares two durable objects by their canonical encoding.
func objectEqual(got, want core.Object) bool {
	gotText, gotErr := json.Marshal(got)
	wantText, wantErr := json.Marshal(want)
	return gotErr == nil && wantErr == nil && string(gotText) == string(wantText)
}
