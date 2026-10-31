package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

const graphA = `{"nodes":[{"id":"start","type":"workflow","data":{"kind":"start"}}],"edges":[]}`

func TestWorkflowAuthoringLifecycle(t *testing.T) {
	s := storetest.Open(t)
	_, tenant := storetest.Login(t, s, "workflow-owner")
	ctx := context.Background()

	w, draft, err := s.CreateWorkflow(ctx, tenant, "  评审流程  ", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "  评审流程  " {
		t.Fatalf("store keeps the name it was given, trimming belongs to the caller: %q", w.Name)
	}
	if w.PublishedSnapshotID != nil {
		t.Fatalf("new workflow published %v", *w.PublishedSnapshotID)
	}
	if draft.Version != store.WorkflowDraftVersion || draft.Graph != graphA || draft.UpdatedAt == nil {
		t.Fatalf("draft = %+v", draft)
	}

	// A draft save replaces only the draft: the definition's updated_at, and with it the library
	// order, does not move.
	const graphB = `{"nodes":[],"edges":[],"viewport":{"x":1,"y":2,"zoom":0.5}}`
	updated, err := s.UpdateWorkflowDraft(ctx, tenant, w.ID, graphB)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Graph != graphB {
		t.Fatalf("draft graph = %q", updated.Graph)
	}
	reloaded, err := s.Workflow(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.UpdatedAt.Equal(w.UpdatedAt) {
		t.Fatalf("draft save moved the definition: %v -> %v", w.UpdatedAt, reloaded.UpdatedAt)
	}

	// Publish freezes the current draft, points the definition at it, and leaves the draft alone.
	var validated string
	frozen, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", func(graph string) error {
		validated = graph
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if validated != graphB {
		t.Fatalf("validated %q, froze the later content instead", validated)
	}
	if frozen.Version != "v1" || frozen.Graph != graphB || frozen.UpdatedAt != nil {
		t.Fatalf("published snapshot = %+v", frozen)
	}
	after, err := s.Workflow(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PublishedSnapshotID == nil || *after.PublishedSnapshotID != frozen.ID {
		t.Fatalf("published pointer = %v", after.PublishedSnapshotID)
	}
	if !after.UpdatedAt.After(w.UpdatedAt) {
		t.Fatalf("publish should move the definition's updated_at: %v", after.UpdatedAt)
	}

	// Rollback copies an old graph into the draft without moving the pointer.
	_, draft, err = s.CreateWorkflow(ctx, tenant, "第二份", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollbackWorkflow(ctx, tenant, w.ID, frozen.ID); err != nil {
		t.Fatal(err)
	}
	rolled, err := s.WorkflowDraft(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Graph != graphB {
		t.Fatalf("rollback draft = %q", rolled.Graph)
	}
	current, err := s.Workflow(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PublishedSnapshotID == nil || *current.PublishedSnapshotID != frozen.ID {
		t.Fatalf("rollback moved the published pointer: %v", current.PublishedSnapshotID)
	}

	// Activate moves the pointer and syncs the target graph into the draft.
	if _, err := s.UpdateWorkflowDraft(ctx, tenant, w.ID, `{"nodes":[{"id":"start","type":"workflow"}],"edges":[]}`); err != nil {
		t.Fatal(err)
	}
	second, err := s.PublishWorkflow(ctx, tenant, w.ID, "v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	activated, err := s.ActivateWorkflow(ctx, tenant, w.ID, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activated.Graph != graphB {
		t.Fatalf("activate draft = %q", activated.Graph)
	}
	current, err = s.Workflow(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PublishedSnapshotID == nil || *current.PublishedSnapshotID != frozen.ID {
		t.Fatalf("activate pointer = %v", current.PublishedSnapshotID)
	}

	versions, err := s.ListWorkflowVersions(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != "v2" || versions[1].Version != "v1" {
		t.Fatalf("versions = %+v", versions)
	}
	if versions[0].ID != second.ID {
		t.Fatalf("newest version first: %+v", versions)
	}

	// getVersion resolves the draft too, as desktop's contract does.
	byVersion, err := s.WorkflowVersion(ctx, tenant, w.ID, store.WorkflowDraftVersion)
	if err != nil || byVersion.ID != activated.ID {
		t.Fatalf("draft by version = %+v, %v", byVersion, err)
	}
}

func TestWorkflowNamesAreUniquePerLiveTenant(t *testing.T) {
	s := storetest.Open(t)
	_, tenant := storetest.Login(t, s, "names-owner")
	ctx := context.Background()

	w, _, err := s.CreateWorkflow(ctx, tenant, "评审流程", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateWorkflow(ctx, tenant, "评审流程", graphA); !errors.Is(err, store.ErrWorkflowNameConflict) {
		t.Fatalf("duplicate name = %v", err)
	}
	// Desktop's index folds ASCII case; the database enforces it so two concurrent creates cannot
	// both pass an application-level check.
	if _, _, err := s.CreateWorkflow(ctx, tenant, "Review", graphA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateWorkflow(ctx, tenant, "review", graphA); !errors.Is(err, store.ErrWorkflowNameConflict) {
		t.Fatalf("case-insensitive duplicate = %v", err)
	}
	// The tenant is part of every lookup, so another tenant's identifier never resolves this
	// definition even if it guesses the id.
	if _, err := s.Workflow(ctx, "00000000-0000-0000-0000-000000000000", w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	if _, err := s.WorkflowDraft(ctx, "00000000-0000-0000-0000-000000000000", w.ID); !errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		t.Fatalf("cross-tenant draft read = %v", err)
	}

	if _, err := s.UpdateWorkflowName(ctx, tenant, w.ID, "Review"); !errors.Is(err, store.ErrWorkflowNameConflict) {
		t.Fatalf("rename onto an existing name = %v", err)
	}
	renamed, err := s.UpdateWorkflowName(ctx, tenant, w.ID, "改名后")
	if err != nil || renamed.Name != "改名后" {
		t.Fatalf("rename = %+v %v", renamed, err)
	}
	if !renamed.UpdatedAt.After(w.UpdatedAt) {
		t.Fatalf("rename did not move updated_at")
	}
}

func TestWorkflowPublishRefusesInvalidAndDuplicateVersions(t *testing.T) {
	s := storetest.Open(t)
	_, tenant := storetest.Login(t, s, "publish-owner")
	ctx := context.Background()
	w, _, err := s.CreateWorkflow(ctx, tenant, "发布", graphA)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("graph invalid")
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", func(string) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("validation failure = %v", err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "draft", nil); !errors.Is(err, store.ErrWorkflowVersionExists) {
		t.Fatalf("reserved draft row collision = %v", err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil); !errors.Is(err, store.ErrWorkflowVersionExists) {
		t.Fatalf("duplicate version = %v", err)
	}
	// A failed publish leaves the definition unpublished.
	unpublished, _, err := s.CreateWorkflow(ctx, tenant, "未发布", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, unpublished.ID, "v1", func(string) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	still, err := s.Workflow(ctx, tenant, unpublished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.PublishedSnapshotID != nil {
		t.Fatalf("failed publish activated %v", *still.PublishedSnapshotID)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, "00000000-0000-0000-0000-000000000000", "v1", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("publish unknown workflow = %v", err)
	}
}

func TestWorkflowSnapshotDeletionProtectsDraftAndActiveVersion(t *testing.T) {
	s := storetest.Open(t)
	_, tenant := storetest.Login(t, s, "delete-owner")
	ctx := context.Background()
	w, _, err := s.CreateWorkflow(ctx, tenant, "删除保护", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v2", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, store.WorkflowDraftVersion); !errors.Is(err, store.ErrWorkflowDraftDeletion) {
		t.Fatalf("delete draft = %v", err)
	}
	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, "v2"); !errors.Is(err, store.ErrWorkflowActiveVersion) {
		t.Fatalf("delete active version = %v", err)
	}
	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, "missing"); !errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		t.Fatalf("delete missing version = %v", err)
	}
	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, "v1"); err != nil {
		t.Fatalf("delete unreferenced version: %v", err)
	}
	if _, err := s.WorkflowVersion(ctx, tenant, w.ID, "v1"); !errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		t.Fatalf("deleted version is still readable = %v", err)
	}
	// The spelling becomes reusable, as desktop's partial unique index allows, and the pointer
	// follows the newly published row.
	republished, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil)
	if err != nil {
		t.Fatalf("republish deleted version: %v", err)
	}
	if republished.ID == "" {
		t.Fatal("republished version has no identity")
	}
	current, err := s.Workflow(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PublishedSnapshotID == nil || *current.PublishedSnapshotID != republished.ID {
		t.Fatalf("published pointer = %v", current.PublishedSnapshotID)
	}
	// Deleting the definition hides it and its versions, and frees the name.
	if err := s.DeleteWorkflow(ctx, tenant, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Workflow(ctx, tenant, w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted workflow is readable = %v", err)
	}
	if _, err := s.WorkflowVersion(ctx, tenant, w.ID, "v1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("version of a deleted workflow is readable = %v", err)
	}
	if _, _, err := s.CreateWorkflow(ctx, tenant, "删除保护", graphA); err != nil {
		t.Fatalf("reuse name of a deleted workflow: %v", err)
	}
}

func TestWorkflowAutoVersionsStayDistinctUnderConcurrency(t *testing.T) {
	s := storetest.Open(t)
	_, tenant := storetest.Login(t, s, "concurrent-owner")
	ctx := context.Background()
	w, _, err := s.CreateWorkflow(ctx, tenant, "并发发布", graphA)
	if err != nil {
		t.Fatal(err)
	}
	const publishers = 6
	var wg sync.WaitGroup
	errs := make([]error, publishers)
	for i := range publishers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.PublishWorkflow(ctx, tenant, w.ID, "same-version", nil)
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrWorkflowVersionExists):
		default:
			t.Fatalf("unexpected concurrent publish error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent publishes with one version succeeded", succeeded)
	}
	// Concurrent draft saves serialize on the definition lock and keep the last write.
	var saves sync.WaitGroup
	for i := range 4 {
		saves.Add(1)
		go func(i int) {
			defer saves.Done()
			if _, err := s.UpdateWorkflowDraft(ctx, tenant, w.ID, graphA+string(rune('a'+i))); err != nil {
				t.Errorf("draft save %d: %v", i, err)
			}
		}(i)
	}
	saves.Wait()
	if _, err := s.WorkflowDraft(ctx, tenant, w.ID); err != nil {
		t.Fatal(err)
	}
}
