package core

import (
	"reflect"
	"testing"
)

func TestControlledResidualListStopEvidence(t *testing.T) {
	stores := controlledStores(t)
	for _, evidence := range []bool{true, false} {
		name := "without evidence"
		if evidence {
			name = "stop unconfirmed"
		}
		t.Run(name, func(t *testing.T) {
			scene := seedControlledScene(t, stores(t), false)
			want := seedControlledStopEvidence(t, scene, evidence)
			out := seedControlledManage(t, scene.store, ResidualManagementRequest{Action: ResidualList, TenantID: scene.tenant})
			items := residualObjects(out["items"])
			if len(items) != 1 || items[0].S("runId") != scene.run || items[0]["stopEvidence"] == nil || !reflect.DeepEqual(items[0].O("stopEvidence"), want) {
				t.Fatalf("list lost residual identity or stop evidence: got %v, want %v", out, want)
			}
			assertControlledObservationOnly(t, scene)
		})
	}
}

func TestControlledResidualGetStopEvidence(t *testing.T) {
	stores := controlledStores(t)
	for _, name := range []string{"without evidence", "stop unconfirmed"} {
		for _, selector := range []string{"resource", "intent", "resource with intent"} {
			t.Run(name+"/"+selector, func(t *testing.T) {
				scene := seedControlledScene(t, stores(t), false)
				want := seedControlledStopEvidence(t, scene, name == "stop unconfirmed")
				request := ResidualManagementRequest{Action: ResidualGet, TenantID: scene.tenant, RunID: scene.run}
				if selector != "resource" {
					root := seedControlledRoot(t, scene)
					if selector == "intent" {
						request.RunID, request.IntentID = "", root.S("id")
					}
				}
				out := seedControlledManage(t, scene.store, request)
				if out.O("residual").S("runId") != scene.run || out.O("residual")["stopEvidence"] == nil || !reflect.DeepEqual(out.O("residual").O("stopEvidence"), want) {
					t.Fatalf("get lost residual identity or stop evidence: got %v, want %v", out, want)
				}
				if selector == "resource" {
					if out.O("resource")["stopEvidence"] == nil || !reflect.DeepEqual(out.O("resource").O("stopEvidence"), want) {
						t.Fatalf("get resource lost stop evidence: got %v, want %v", out, want)
					}
					assertControlledObservationOnly(t, scene)
				}
			})
		}
	}
}

func seedControlledStopEvidence(t *testing.T, scene residualReclaimScene, present bool) Object {
	t.Helper()
	evidence := Object{}
	if present {
		evidence["stop_unconfirmed"] = map[string]any{"kind": "stop_unconfirmed", "source": "cloud", "observedAt": "2026-10-21T08:00:00Z", "executionId": scene.delivery}
		seedResidualExec(t, scene.store, "UPDATE agent_run_residuals SET stop_evidence=$2 WHERE run_id=$1", scene.run, jsonText(evidence))
	}
	return evidence
}

func assertControlledObservationOnly(t *testing.T, scene residualReclaimScene) {
	t.Helper()
	seedResidualAssert(t, scene, `SELECT reclaim_state='reconciling' AND reclaim_mode IS NULL AND cleared_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_intents WHERE run_id=$1)
 AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')
 FROM agent_run_residuals r WHERE run_id=$1`, scene.run)
}
