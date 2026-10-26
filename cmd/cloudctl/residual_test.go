package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestCloudctlResidualManagement(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want core.ResidualManagementRequest
	}{
		{
			name: "scoped paginated list",
			args: []string{"list", "--tenant-id", "tenant", "--project-id", "project", "--run-id", "run", "--workspace-id", "workspace", "--operation-id", "operation", "--reclaim-id", "reclaim", "--limit", "7", "--after", "cursor"},
			want: core.ResidualManagementRequest{Action: core.ResidualList, TenantID: "tenant", ProjectID: "project", RunID: "run", WorkspaceID: "workspace", OperationID: "operation", ReclaimID: "reclaim", Limit: 7, After: "cursor", ExternalFacts: map[string]string{}},
		},
		{
			name: "get intent",
			args: []string{"get", "--tenant-id", "tenant", "--intent-id", "intent"},
			want: core.ResidualManagementRequest{Action: core.ResidualGet, TenantID: "tenant", IntentID: "intent", Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "get resource",
			args: []string{"get", "--tenant-id", "tenant", "--run-id", "run", "--workspace-id", "workspace"},
			want: core.ResidualManagementRequest{Action: core.ResidualGet, TenantID: "tenant", RunID: "run", WorkspaceID: "workspace", Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "get reclaim",
			args: []string{"get", "--tenant-id", "tenant", "--reclaim-id", "reclaim"},
			want: core.ResidualManagementRequest{Action: core.ResidualGet, TenantID: "tenant", ReclaimID: "reclaim", Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "terminate legacy D8 exact scope",
			args: []string{"terminate", "--tenant-id", "tenant", "--project-id", "project", "--run-id", "run", "--workspace-id", "workspace", "--operation-id", "failed-delete", "--reason", "operator abandoned execution", "--key", "terminate-key", "--operator-label", "self-reported", "--impact-confirmed"},
			want: core.ResidualManagementRequest{Action: core.ResidualTerminate, TenantID: "tenant", ProjectID: "project", RunID: "run", WorkspaceID: "workspace", OperationID: "failed-delete", Reason: "operator abandoned execution", Key: "terminate-key", OperatorLabel: "self-reported", ImpactConfirmed: true, Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "independent data deletion",
			args: []string{"delete-data", "--tenant-id", "tenant", "--intent-id", "root", "--reason", "delete retained data", "--key", "delete-key", "--data-delete-confirmed=true"},
			want: core.ResidualManagementRequest{Action: core.ResidualDeleteData, TenantID: "tenant", IntentID: "root", Reason: "delete retained data", Key: "delete-key", DataDeleteConfirmed: true, Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "explicit D8 without a historical delete",
			args: []string{"terminate", "--tenant-id", "tenant", "--run-id", "run", "--workspace-id", "workspace", "--allow-missing-delete-operation", "--reason", "verified recovery copies", "--key", "missing-delete", "--impact-confirmed"},
			want: core.ResidualManagementRequest{Action: core.ResidualTerminate, TenantID: "tenant", RunID: "run", WorkspaceID: "workspace", AllowMissingDeleteOperation: true, Reason: "verified recovery copies", Key: "missing-delete", ImpactConfirmed: true, Limit: 100, ExternalFacts: map[string]string{}},
		},
		{
			name: "external facts preserve exact references",
			args: []string{"confirm-external", "--tenant-id", "tenant", "--intent-id", "unfinished", "--reason", "external disposal", "--key", "external-key", "--evidence-note", "host decommissioned", "--external-fact", "sandbox:original=ticket=123", "--external-fact", "data:workspace=ticket-456", "--responsibility-transferred"},
			want: core.ResidualManagementRequest{Action: core.ResidualConfirmExternal, TenantID: "tenant", IntentID: "unfinished", Reason: "external disposal", Key: "external-key", EvidenceNote: "host decommissioned", ResponsibilityTransferred: true, Limit: 100, ExternalFacts: map[string]string{"sandbox:original": "ticket=123", "data:workspace": "ticket-456"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", "operator.yaml", "residual"}, tc.args...)
			opts, err := parseCommand(args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if opts.command != "residual" || opts.configFile != "operator.yaml" || !reflect.DeepEqual(opts.residual, tc.want) {
				t.Fatalf("options = %#v; want request %#v with residual command and operator.yaml", opts, tc.want)
			}
			var output bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err = dispatchResidual(ctx, &opts.residual, &output, func(actualCtx context.Context, request *core.ResidualManagementRequest) (core.Object, error) {
				calls++
				if actualCtx != ctx || !reflect.DeepEqual(request, &tc.want) {
					t.Fatalf("dispatcher changed context or request: %#v", request)
				}
				return core.Object{"state": "registered"}, nil
			})
			if err != nil || calls != 1 || output.String() != "{\"state\":\"registered\"}\n" {
				t.Fatalf("dispatch error=%v calls=%d output=%q", err, calls, output.String())
			}
		})
	}
}

func TestCloudctlResidualRejectsUnsafeOrAmbiguousArguments(t *testing.T) {
	terminate := []string{"terminate", "--tenant-id", "tenant", "--workspace-id", "workspace", "--reason", "reason", "--key", "key"}
	external := []string{"confirm-external", "--tenant-id", "tenant", "--intent-id", "intent", "--reason", "reason", "--key", "key", "--evidence-note", "note", "--responsibility-transferred"}
	cases := [][]string{
		{},
		{"unknown"},
		{"list"},
		{"list", "--tenant-id", " "},
		{"list", "--tenant-id", "tenant", "--limit", "0"},
		{"list", "--tenant-id", "tenant", "--limit", "invalid"},
		{"list", "--tenant-id", "tenant", "trailing"},
		{"get", "--tenant-id", "tenant", "--project-id", "project"},
		{"get", "--tenant-id", "tenant", "--intent-id", "intent", "--workspace-id", "workspace"},
		terminate,
		appendCopy(terminate, "--impact-confirmed=false"),
		appendCopy(terminate, "--impact-confirmed", "--data-delete-confirmed"),
		appendCopy(terminate, "--impact-confirmed", "--allow-missing-delete-operation"),
		appendCopy(terminate, "--impact-confirmed", "--run-id", "run", "--allow-missing-delete-operation", "--operation-id", "old-delete"),
		{"terminate", "--tenant-id", "tenant", "--project-id", "project", "--reason", "reason", "--key", "key", "--impact-confirmed"},
		{"terminate", "--tenant-id", "tenant", "--run-id", "run", "--reason", " ", "--key", "key", "--impact-confirmed"},
		{"terminate", "--tenant-id", "tenant", "--run-id", "run", "--reason", "reason", "--key", " ", "--impact-confirmed"},
		{"terminate", "--tenant-id", "tenant", "--run-id", "run", "--reason", "reason", "--impact-confirmed"},
		{"delete-data", "--tenant-id", "tenant", "--workspace-id", "workspace", "--reason", "reason", "--key", "key", "--data-delete-confirmed"},
		{"delete-data", "--tenant-id", "tenant", "--intent-id", "intent", "--reason", "reason", "--key", "key", "--data-delete-confirmed=false"},
		external,
		appendCopy(external, "--external-fact", "sandbox:one="),
		appendCopy(external, "--external-fact", "sandbox:=ticket"),
		appendCopy(external, "--external-fact", "unqualified=ticket"),
		appendCopy(external, "--external-fact", "unknown:id=ticket"),
		appendCopy(external, "--external-fact", "sandbox:one=a", "--external-fact", "sandbox:one=b"),
		{"confirm-external", "--tenant-id", "tenant", "--intent-id", "intent", "--reason", "reason", "--key", "key", "--evidence-note", " ", "--external-fact", "data:workspace=ticket", "--responsibility-transferred"},
		{"confirm-external", "--tenant-id", "tenant", "--intent-id", "intent", "--reason", "reason", "--key", "key", "--evidence-note", "note", "--external-fact", "data:workspace=ticket", "--responsibility-transferred=false"},
		{"list", "--tenant-id", "tenant", "--actor-subject", "spoof"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := parseResidual(args)
			if err == nil || err.Error() != "invalid_cli_arguments" {
				t.Fatalf("error = %v; want invalid_cli_arguments", err)
			}
		})
	}
}

func appendCopy(original []string, more ...string) []string {
	return append(append([]string(nil), original...), more...)
}

func TestCloudctlResidualErrorsNeverOutputInfrastructureDetails(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&core.Fault{Code: "residual_mode_conflict", Status: 409}, "residual_mode_conflict"},
		{errors.New("connection failed: password=do-not-output"), "internal_error"},
	} {
		var output bytes.Buffer
		err := dispatchResidual(context.Background(), &core.ResidualManagementRequest{}, &output, func(context.Context, *core.ResidualManagementRequest) (core.Object, error) {
			return nil, tc.err
		})
		if err == nil || err.Error() != tc.code || output.Len() != 0 {
			t.Fatalf("error = %v, stdout = %q; want %s and empty stdout", err, output.String(), tc.code)
		}
	}
}

func TestCloudctlResidualStopEvidenceOutput(t *testing.T) {
	for _, action := range []string{"list", "get"} {
		for _, name := range []string{"stop unconfirmed", "without evidence"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				evidence := core.Object{}
				if name == "stop unconfirmed" {
					evidence["stop_unconfirmed"] = core.Object{"kind": "stop_unconfirmed", "source": "cloud", "observedAt": "2026-10-21T08:00:00Z", "executionId": "original-execution"}
				}
				resource := core.Object{"runId": "run", "stopEvidence": evidence}
				want := core.Object{"resource": resource, "residual": resource}
				if action == "list" {
					want = core.Object{"items": []core.Object{resource}, "nextCursor": ""}
				}
				args := []string{action, "--tenant-id", "tenant"}
				if action == "get" {
					args = append(args, "--run-id", "run")
				}
				request, err := parseResidual(args)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				err = dispatchResidual(t.Context(), &request, &output, func(context.Context, *core.ResidualManagementRequest) (core.Object, error) {
					return want, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if output.String() != string(encoded)+"\n" {
					t.Fatalf("output = %q; want %s followed by newline", output.String(), encoded)
				}
			})
		}
	}
}

func TestCloudctlPreservesLegacyCommandsAndRejectsMixedSyntax(t *testing.T) {
	for _, command := range []string{"migrate", "bootstrap"} {
		opts, err := parseCommand([]string{"-config", "file", "-command", command})
		if err != nil || opts.command != command || opts.configFile != "file" {
			t.Fatalf("legacy command %s: options=%#v error=%v", command, opts, err)
		}
	}
	for _, args := range [][]string{
		{"-command", "residual"},
		{"-command", "migrate", "residual", "list", "--tenant-id", "tenant"},
		{"-name", "tenant", "residual", "list", "--tenant-id", "tenant"},
		{"-command", "bootstrap", "unexpected"},
		{"--unknown", "password=do-not-output"},
	} {
		if _, err := parseCommand(args); err == nil || err.Error() != "invalid_cli_arguments" {
			t.Fatalf("args=%q error=%v; want invalid_cli_arguments", args, err)
		}
	}
}
