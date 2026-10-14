package controlgrpc

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// A resumed model-backed session must retain both independent snapshots across the wire.
func TestSessionWirePreservesModelBindingAndPriorRevision(t *testing.T) {
	want := core.Object{
		"kind": "agent_session", "agentPluginId": "official/ora-space.opencode", "agentPluginVersion": "0.6.4",
		"checkoutExecutionId": "checkout-1", "modelBindingId": "binding-1",
		"gitIdentity": core.Object{"name": "User", "email": "user@example.invalid"},
		"initialTurn": core.Object{"turnId": "turn-1", "content": []core.Object{{"text": "Continue this task"}}},
		"priorRevision": core.Object{
			"revisionId": "revision-1", "finalCommit": strings.Repeat("a", 40),
			"bundle": core.Object{"key": "revision-1/bundle", "size": int64(42), "sha256": strings.Repeat("b", 64)},
		},
	}
	message := sessionMessage(want)
	fields := message.ProtoReflect().Descriptor().Fields()
	if fields.ByName("prior_revision").Number() != 6 || fields.ByName("model_binding_id").Number() != 7 {
		t.Fatal("published Revision field and new model reference must use distinct wire numbers")
	}
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded controlpb.AgentSessionSpec
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := sessionObject(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session snapshot changed across the wire: got %#v, want %#v", got, want)
	}
}
