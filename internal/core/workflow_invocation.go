package core

// workflowInvocationInput isolates the pending form-to-execution contract: only Start and global
// variables are execution input. Platform launch fields and Issue context remain on the IssueRun.
// A platform-named key declared by the author is still a workflow variable and must survive.
func workflowInvocationInput(graph, values Object) Object {
	input := Object{}
	fields := workflowInputFields(graph)
	for index := range fields {
		if value, ok := values[fields[index].Key]; ok {
			input[fields[index].Key] = value
		}
	}
	return input
}

// enqueueWorkflowInvocation creates both sides of an invocation inside the caller's transaction.
// Snapshot selection/admission and initiator resolution remain at the entry point; project/workspace
// identity and slot binding will join the shared insertion seam when #70 supplies those columns.
func enqueueWorkflowInvocation(t *transaction, tid, iid string, workflow, snapshot, values, issueInput Object, triggerKind string, triggerRef any, uid string) Object {
	executionInput := workflowInvocationInput(snapshot.O("graph"), values)
	invocation := insertWorkflowRun(t, tid, workflow.S("id"), snapshot, executionInput, workflow.S("name"))
	freezeWorkflowSnapshot(issueInput, snapshot.S("id"))
	return enqueueRunWithInvocation(t, tid, iid, "workflow", workflow.S("id"), issueInput, triggerKind, triggerRef, "user", uid, invocation.S("id"))
}
