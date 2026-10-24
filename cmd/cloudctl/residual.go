package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

type residualManager func(context.Context, *core.ResidualManagementRequest) (core.Object, error)

func invalidArguments() error {
	return &core.Fault{Code: "invalid_cli_arguments", Status: 400, Params: core.Object{}}
}

func parseResidual(args []string) (core.ResidualManagementRequest, error) {
	var request core.ResidualManagementRequest
	if len(args) == 0 {
		return request, invalidArguments()
	}
	allowed := residualFlags(args[0])
	if allowed == nil {
		return request, invalidArguments()
	}
	flags := flag.NewFlagSet("residual "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&request.TenantID, "tenant-id", "", "tenant ID (required)")
	flags.StringVar(&request.ProjectID, "project-id", "", "project filter")
	flags.StringVar(&request.RunID, "run-id", "", "original Run ID")
	flags.StringVar(&request.WorkspaceID, "workspace-id", "", "original Workspace ID")
	flags.StringVar(&request.OperationID, "operation-id", "", "original failed delete operation ID")
	flags.StringVar(&request.ReclaimID, "reclaim-id", "", "reclamation responsibility ID")
	flags.StringVar(&request.IntentID, "intent-id", "", "existing root or child intent ID")
	flags.StringVar(&request.Key, "key", "", "tenant-wide idempotency key")
	flags.StringVar(&request.Reason, "reason", "", "operator reason")
	flags.StringVar(&request.OperatorLabel, "operator-label", "", "self-reported operator label")
	flags.IntVar(&request.Limit, "limit", 100, "page size")
	flags.StringVar(&request.After, "after", "", "page cursor")
	flags.BoolVar(&request.ImpactConfirmed, "impact-confirmed", false, "confirm termination impact")
	flags.BoolVar(&request.AllowMissingDeleteOperation, "allow-missing-delete-operation", false, "explicitly select a D8 Run with no historical delete operation")
	flags.BoolVar(&request.DataDeleteConfirmed, "data-delete-confirmed", false, "confirm permanent data deletion")
	flags.BoolVar(&request.ResponsibilityTransferred, "responsibility-transferred", false, "accept remaining external responsibility")
	flags.StringVar(&request.EvidenceNote, "evidence-note", "", "external disposition explanation")
	facts := externalFacts{}
	flags.Var(&facts, "external-fact", "repeatable kind:id=external-reference; use openTargets from get")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return request, invalidArguments()
	}
	validFlags := true
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			validFlags = false
		}
	})
	if !validFlags || strings.TrimSpace(request.TenantID) == "" {
		return request, invalidArguments()
	}
	request.ExternalFacts = facts
	switch args[0] {
	case "list":
		request.Action = core.ResidualList
		if request.Limit < 1 {
			return request, invalidArguments()
		}
	case "get":
		request.Action = core.ResidualGet
		if (request.IntentID != "" && hasResourceSelector(&request)) || (request.IntentID == "" && request.RunID == "" && request.WorkspaceID == "" && request.ReclaimID == "") {
			return request, invalidArguments()
		}
	case "terminate":
		request.Action = core.ResidualTerminate
		if (request.RunID == "" && request.WorkspaceID == "") || !request.ImpactConfirmed {
			return request, invalidArguments()
		}
		if request.AllowMissingDeleteOperation && (request.RunID == "" || request.WorkspaceID == "" || request.OperationID != "") {
			return request, invalidArguments()
		}
	case "delete-data":
		request.Action = core.ResidualDeleteData
		if request.IntentID == "" || !request.DataDeleteConfirmed {
			return request, invalidArguments()
		}
	case "confirm-external":
		request.Action = core.ResidualConfirmExternal
		if request.IntentID == "" || !request.ResponsibilityTransferred || strings.TrimSpace(request.EvidenceNote) == "" || len(facts) == 0 {
			return request, invalidArguments()
		}
	}
	if args[0] != "list" && args[0] != "get" && (strings.TrimSpace(request.Key) == "" || strings.TrimSpace(request.Reason) == "") {
		return request, invalidArguments()
	}
	return request, nil
}

// Allowed flags keep broad query filters from becoming write selectors. Core
// resolves identities and rechecks authorization; the CLI never chooses an actor.
func residualFlags(action string) map[string]bool {
	var names []string
	switch action {
	case "list":
		names = []string{"project-id", "run-id", "workspace-id", "operation-id", "reclaim-id", "limit", "after"}
	case "get":
		names = []string{"project-id", "run-id", "workspace-id", "operation-id", "reclaim-id", "intent-id"}
	case "terminate":
		names = []string{"project-id", "run-id", "workspace-id", "operation-id", "reason", "key", "operator-label", "impact-confirmed", "allow-missing-delete-operation"}
	case "delete-data":
		names = []string{"intent-id", "reason", "key", "operator-label", "data-delete-confirmed"}
	case "confirm-external":
		names = []string{"intent-id", "reason", "key", "operator-label", "evidence-note", "external-fact", "responsibility-transferred"}
	default:
		return nil
	}
	allowed := map[string]bool{"tenant-id": true}
	for _, name := range names {
		allowed[name] = true
	}
	return allowed
}

func hasResourceSelector(r *core.ResidualManagementRequest) bool {
	return r.ProjectID != "" || r.RunID != "" || r.WorkspaceID != "" || r.OperationID != "" || r.ReclaimID != ""
}

// External references are opaque evidence, not assertions of physical success.
// Reject duplicates rather than silently replacing evidence supplied earlier.
type externalFacts map[string]string

func (f *externalFacts) String() string { return "kind:id=external-reference" }

func (f *externalFacts) Set(value string) error {
	target, reference, found := strings.Cut(value, "=")
	kind, id, qualified := strings.Cut(target, ":")
	if !found || !qualified || (kind != "sandbox" && kind != "data") || strings.TrimSpace(id) == "" || strings.TrimSpace(reference) == "" {
		return invalidArguments()
	}
	if _, exists := (*f)[target]; exists {
		return invalidArguments()
	}
	(*f)[target] = reference
	return nil
}

func dispatchResidual(ctx context.Context, request *core.ResidualManagementRequest, output io.Writer, manage residualManager) error {
	out, err := manage(ctx, request)
	if err != nil {
		// Faults carry the stable safe code. Infrastructure errors may contain a
		// connection string, so never print their implementation details.
		return core.ErrorCode(err)
	}
	return json.NewEncoder(output).Encode(out)
}
