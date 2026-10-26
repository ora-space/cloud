package core

import (
	"reflect"
	"testing"
)

// The run summary is the projection both the history list and the detail page return, so these
// tests pin its complete shape and, in particular, that an unset timestamp stays JSON null: the
// OpenAPI schema declares startedAt/finishedAt nullable and the page treats null as "not reached",
// while an empty string would be an invalid date-time rendered as a blank timestamp. pluginVersion
// follows the same rule: frozen at creation and projected verbatim, so an unfrozen run reads back
// as null instead of an empty version string that could be mistaken for a real one.
func TestWorkflowRunSummaryKeepsNullableFields(t *testing.T) {
	tests := []struct {
		name string
		run  Object
		want Object
	}{
		{
			name: "pending run has no timestamps",
			run: Object{
				"id": "run-1", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "pending",
				"input":             Object{"text": "hi"},
				"nodeStates":        Object{},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         nil,
				"finishedAt":        nil,
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:00:00.000000+00:00",
				"cancelRequestedAt": nil,
				"pluginVersion":     nil,
			},
			want: Object{
				"id": "run-1", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "pending",
				"input":             Object{"text": "hi"},
				"nodeStates":        Object{},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         nil,
				"finishedAt":        nil,
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:00:00.000000+00:00",
				"cancelRequestedAt": nil,
				"pluginVersion":     nil,
			},
		},
		{
			name: "running run has a start but no finish",
			run: Object{
				"id": "run-2", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "running",
				"input": Object{},
				"nodeStates": Object{
					"agent-1": Object{"status": "running", "startedAt": "2026-10-20T09:00:01Z"},
				},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         "2026-10-20T09:00:01.000000+00:00",
				"finishedAt":        nil,
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:00:02.000000+00:00",
				"cancelRequestedAt": "2026-10-20T09:00:02.500000+00:00",
				"pluginVersion":     "1.0.0",
			},
			want: Object{
				"id": "run-2", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "running",
				"input": Object{},
				"nodeStates": Object{
					"agent-1": Object{"status": "running", "startedAt": "2026-10-20T09:00:01Z"},
				},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         "2026-10-20T09:00:01.000000+00:00",
				"finishedAt":        nil,
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:00:02.000000+00:00",
				"cancelRequestedAt": "2026-10-20T09:00:02.500000+00:00",
				"pluginVersion":     "1.0.0",
			},
		},
		{
			name: "succeeded run carries both timestamps",
			run: Object{
				"id": "run-3", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "succeeded",
				"input":             Object{},
				"nodeStates":        Object{"out": Object{"status": "succeeded", "output": Object{}}},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         "2026-10-20T09:00:01.000000+00:00",
				"finishedAt":        "2026-10-20T09:03:00.000000+00:00",
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:03:00.000000+00:00",
				"cancelRequestedAt": nil,
				"pluginVersion":     "1.0.0",
			},
			want: Object{
				"id": "run-3", "tenantId": "tenant-1", "workflowId": "workflow-1",
				"snapshotId": "snapshot-1", "name": "demo", "status": "succeeded",
				"input":             Object{},
				"nodeStates":        Object{"out": Object{"status": "succeeded", "output": Object{}}},
				"rounds":            []any{},
				"error":             "",
				"startedAt":         "2026-10-20T09:00:01.000000+00:00",
				"finishedAt":        "2026-10-20T09:03:00.000000+00:00",
				"createdAt":         "2026-10-20T09:00:00.000000+00:00",
				"updatedAt":         "2026-10-20T09:03:00.000000+00:00",
				"cancelRequestedAt": nil,
				"pluginVersion":     "1.0.0",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workflowRunSummary(tt.run); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("workflowRunSummary() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
