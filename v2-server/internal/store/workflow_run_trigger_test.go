package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestScheduledTriggerCreatesOneRunAndFindsIt(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "trigger-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-trigger")
	w, snapshot := publishedWorkflow(t, s, tenant, "定时运行", "v1")
	in := store.WorkflowRunInput{ProjectID: project.ID, Name: "每周整理", TriggerKind: "schedule", TriggerRef: "occurrence-1"}

	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, snapshot.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	// The trigger's identity holds one run, ever.
	if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, snapshot.ID, in); !errors.Is(err, store.ErrWorkflowRunTriggerTaken) {
		t.Fatalf("second run of one trigger = %v", err)
	}
	found, err := s.ScheduledWorkflowRun(ctx, tenant, "occurrence-1")
	if err != nil || found.ID != run.ID || found.Name != "每周整理" || found.Status != store.WorkflowStatusPending {
		t.Fatalf("found = %+v %v", found, err)
	}
	if _, err := s.ScheduledWorkflowRun(ctx, tenant, "occurrence-2"); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("unknown trigger = %v", err)
	}
	// Manual runs carry no trigger identity and never collide.
	for range 2 {
		if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, snapshot.ID, store.WorkflowRunInput{ProjectID: project.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ScheduledWorkflowRun(ctx, tenant, ""); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("a manual run was found as scheduled: %v", err)
	}
}
