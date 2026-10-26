package core

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Readiness and final admission must name the same public, actionable gap after a saved
// Claude configuration loses part of its connection. The read projection must not leak the
// workflow resolver's internal reason codes or let the failed admission persist a binding.
func TestPersonalAgentReadinessAndAdmissionUsePublicModelGaps(t *testing.T) {
	for _, test := range []struct {
		name, change, code string
	}{
		{"disabled connection", "UPDATE personal_model_connections SET enabled=false WHERE id=$1", "model_connection_unavailable"},
		{"deleted connection", "UPDATE personal_model_connections SET deleted_at=now() WHERE id=$1", "model_connection_unavailable"},
		{"missing credential", "UPDATE personal_model_connections SET credential_id=NULL WHERE id=$1", "model_credential_required"},
		{"withdrawn model", `UPDATE personal_model_connections SET models='[{"id":"other/model","name":"Other","contextWindow":64000,"maxTokens":4096}]'::jsonb WHERE id=$1`, "model_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newPersonalAgentScene(t)
			if _, err := s.request("PUT", "", uuid.NewString(), s.body(0)); err != nil {
				t.Fatal(err)
			}
			s.store.PersonalAgentRuntime = &PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
			request := &PublicRequest{Method: "GET", Path: "/api/v1/tenants/" + s.seed.tenant + "/issues/" + s.seed.issue + "/conversation", TenantID: s.seed.tenant, IssueID: s.seed.issue, Identity: s.claims}
			page, _, err := s.store.Public(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			ready := []Object{{"agentId": s.seed.agent, "requiresPersonalModel": true, "errorCode": nil}}
			if !reflect.DeepEqual(page["agentReadiness"], ready) {
				t.Fatalf("configured Claude readiness = %v, want %v", page["agentReadiness"], ready)
			}
			if _, err = s.store.Pool.Exec(test.change, s.connection.S("id")); err != nil {
				t.Fatal(err)
			}
			page, _, err = s.store.Public(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			want := []Object{{"agentId": s.seed.agent, "requiresPersonalModel": true, "errorCode": test.code}}
			if !reflect.DeepEqual(page["agentReadiness"], want) {
				t.Fatalf("unusable Claude readiness = %v, want %v", page["agentReadiness"], want)
			}
			_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
				bindRunModel(tx, s.seed.run, s.uid, Object{"agentPluginId": claudeAgentCLI, "agentPluginVersion": "1.0.0"}, "")
				return nil
			})
			expectModelFault(t, err, test.code)
			var bindings, configurations int
			if err := s.store.Pool.QueryRow(`SELECT (SELECT count(*) FROM run_model_bindings), (SELECT count(*) FROM run_agent_configurations)`).Scan(&bindings, &configurations); err != nil || bindings != 0 || configurations != 0 {
				t.Fatalf("refused admission left bindings=%d configurations=%d, error=%v", bindings, configurations, err)
			}
		})
	}
}
