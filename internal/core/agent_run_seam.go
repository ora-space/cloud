package core

// The B side's two uses of the control plane's in-transaction seams, spelled once so no business
// file has to reach for them directly.
//
// Both run on the caller's own transaction and neither executes anything: the delete is declared for
// a Controller to claim, and the target is resolved from the rows the dispatch will re-validate
// against. That is what keeps the ownership split honest — the business layer states an intent and
// names the Node it is about, and the control plane decides whether either is still true.

// declareDelete declares the run Workspace's delete_workspace operation through the control plane's
// own in-transaction seam, so a failure rolls the caller's `releasing` transition back with it and a
// run can never be releasing without a durable delete intent (IssueRun D3/D6).
//
// It always reports success, and that is deliberate rather than lenient. The seam answers "busy"
// when the Project already has an operation in flight — IssueRun D3 accepts that a Project's runs
// queue behind one operation slot — so the honest outcome is a run that is `releasing` with no
// operation yet, which RedeclareRunWorkspaceDeletesOnce reconciles on its next tick. Turning that
// into an error here would instead roll the transition back and lose the decision, and the delete
// would still have to be declared later by the same reconciliation pass.
//
// A missing Workspace row is not that case: the seam refuses it, and the refusal is a genuine
// invariant violation (a run reaches a releasing transition only through the settlement of its own
// create_workspace or after its Workspace exists), so it propagates and aborts the transaction.
func (s *Store) declareDelete(t *transaction, o Object) error {
	deleteRunWorkspace(t, o.S("id"))
	return nil
}

// sessionStartTarget resolves the execution target a run Workspace's next session or delivery
// execution must be dispatched to: the Workspace's current Node and sandbox instance.
//
// It is read from the same authoritative rows the control plane re-validates the dispatch against
// (currentNode for the Node, the Workspace's runtime generation for the sandbox), so a work item can
// never name a target the dispatch would refuse. A Workspace with no connected Node is not a target
// this function can invent: currentNode refuses it, which aborts the caller's transaction and leaves
// the work item unreleased for a later attempt rather than freezing a stale Node into the input.
func sessionStartTarget(t *transaction, wid string) Object {
	n := currentNode(t, wid)
	return Object{
		"workspaceId":       wid,
		"sandboxInstanceId": n.S("sandboxInstanceId"),
		"nodeId":            n.S("nodeId"),
	}
}
