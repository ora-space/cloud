package controlgrpc

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// TestOperationStepRefusesANameTheControllerCannotRead is the regression for a vocabulary drift that
// used to be silent. Cloud converted a stored step with a bare map lookup, which answers a name the
// contract does not define with the enum's zero value; the Controller reads that zero value as "no step
// at all" and fails the round (`apps/ora-controller/src/cloud/operations.rs`:
// `Done | Unspecified => Err(Error::Conflict)`). So an operation could sit in a step nobody could drive
// — no progress, no error, nothing logged.
//
// 'mirror' is not a hypothetical: migration 0034 stored that step and 0035 withdrew it precisely
// because of this, and Cloud projected it as UNSPECIFIED the whole time.
func TestOperationStepRefusesANameTheControllerCannotRead(t *testing.T) {
	defined := []struct {
		stored string
		want   controlpb.OperationStep
	}{
		{"sandbox", controlpb.OperationStep_OPERATION_STEP_SANDBOX},
		{"node", controlpb.OperationStep_OPERATION_STEP_NODE},
		{"clone", controlpb.OperationStep_OPERATION_STEP_CLONE},
		{"quiesce", controlpb.OperationStep_OPERATION_STEP_QUIESCE},
		{"terminate", controlpb.OperationStep_OPERATION_STEP_TERMINATE},
		{"cleanup", controlpb.OperationStep_OPERATION_STEP_CLEANUP},
		{"plugin", controlpb.OperationStep_OPERATION_STEP_PLUGIN},
		{"done", controlpb.OperationStep_OPERATION_STEP_DONE},
		// The lookup upper-cases the stored value, so a row spelling the step the way the contract
		// enum does resolves as well. Pinned because widening that spelling is a change to who may
		// reach the Controller, not a formatting detail.
		{"Done", controlpb.OperationStep_OPERATION_STEP_DONE},
	}
	for _, c := range defined {
		t.Run("defined/"+c.stored, func(t *testing.T) {
			got, err := operationStep(c.stored)
			if err != nil {
				t.Fatalf("step %q is defined by the contract and must project, got error %v", c.stored, err)
			}
			if got != c.want {
				t.Fatalf("step %q projected as %v, want %v", c.stored, got, c.want)
			}
		})
	}

	// Retired names stay expressible, and this is the only case where the fallback is deliberate:
	// applied rows still spell them (0016) and a historical operation is never claimed, so the zero
	// value costs nothing. They are enumerated rather than inferred from "not in the map", because
	// "not in the map" is exactly the condition that must be refused.
	for _, stored := range []string{"storage", "worktree", "ready", "storage_delete"} {
		t.Run("retired/"+stored, func(t *testing.T) {
			got, err := operationStep(stored)
			if err != nil {
				t.Fatalf("retired step %q must still project for history, got error %v", stored, err)
			}
			if got != controlpb.OperationStep_OPERATION_STEP_UNSPECIFIED {
				t.Fatalf("retired step %q projected as %v, want the zero value", stored, got)
			}
		})
	}

	for _, stored := range []string{"mirror", "bogus", ""} {
		t.Run("refused/"+stored, func(t *testing.T) {
			got, err := operationStep(stored)
			if err == nil {
				t.Fatalf("step %q is not a step this Cloud defines and must be refused, got %v", stored, got)
			}
			if code := status.Code(err); code != codes.Internal {
				t.Fatalf("refusing step %q reported %v, want Internal: a vocabulary drift is this service's fault, not the caller's", stored, code)
			}
			// The name has to reach the operator: without it a typo and a missing migration look
			// identical, which is the state that let 0034 ship.
			if stored != "" && !strings.Contains(status.Convert(err).Message(), stored) {
				t.Fatalf("refusal for %q does not name the step: %v", stored, err)
			}
		})
	}
}

// TestProjectingAnUnknownStepFailsTheResponse proves the refusal reaches the response rather than being
// swallowed at the projection. Projecting a nameless step is the silent downgrade this replaces, so
// the failure has to happen where the RPC can turn it into an error.
func TestProjectingAnUnknownStepFailsTheResponse(t *testing.T) {
	if got, err := operation(core.Object{"id": "op_1", "projectId": "prj_1", "step": "mirror"}); err == nil {
		t.Fatalf("projecting an operation at an unknown step must fail, got %v", got)
	}
	unknown := core.Object{"operation": core.Object{"id": "op_1", "step": "mirror"}, "project": core.Object{}}
	if got, err := snapshot(unknown); err == nil {
		t.Fatalf("a snapshot carrying an unknown step must fail, got %v", got)
	}

	// The control: a step the contract defines still projects, so the refusal is about the name and
	// not about projection in general.
	got, err := operation(core.Object{"id": "op_1", "projectId": "prj_1", "step": "clone"})
	if err != nil {
		t.Fatalf("a defined step must project, got error %v", err)
	}
	if got.GetStep() != controlpb.OperationStep_OPERATION_STEP_CLONE || got.GetId() != "op_1" {
		t.Fatalf("defined step projected as %v", got)
	}
}
