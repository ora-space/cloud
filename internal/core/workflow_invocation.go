package core

// workflowInvocationInput isolates the pending form-to-execution contract: only Start and global
// variables are execution input. Platform launch fields and Issue context remain on the IssueRun.
// A platform-named key declared by the author is still a workflow variable and must survive.
//
// The reserved `input` kickoff selector is not a declared variable, but the Start variable pool
// resolves [start, input] from it: an invocation that named a kickoff has to carry it onto the run or
// the run would silently evaluate against the authored instruction instead. kickoff is nil when the
// entry named none, and its type is not filtered here — requireWorkflowRunInput judges it with the
// same rule a direct run's is judged by, so every entry refuses a non-string kickoff alike.
func workflowInvocationInput(graph, values Object, kickoff any) Object {
	input := Object{}
	fields := workflowInputFields(graph)
	for index := range fields {
		if value, ok := values[fields[index].Key]; ok {
			input[fields[index].Key] = value
		}
	}
	if kickoff != nil {
		input["input"] = kickoff
	}
	return input
}

// enqueueWorkflowInvocation creates both sides of an invocation inside the caller's transaction.
// Snapshot selection/admission and verified initiator resolution remain at the entry point. Unlike
// the direct development simulator, an Issue invocation always freezes its real executor bindings.
func enqueueWorkflowInvocation(t *transaction, tid, iid string, workflow, snapshot, values, issueInput Object, kickoff any, triggerKind string, triggerRef any, uid string) Object {
	executionInput := workflowInvocationInput(snapshot.O("graph"), values, kickoff)
	i := issue(t, tid, iid)
	invocation := insertWorkflowRun(t, tid, workflow.S("id"), snapshot, executionInput, workflow.S("name"), uid, i.S("projectRef"))
	freezeWorkflowRunExecutors(t, invocation.S("id"), uid, snapshot.O("graph"))
	freezeWorkflowSnapshot(issueInput, snapshot.S("id"))
	return enqueueRunWithInvocation(t, tid, iid, "workflow", workflow.S("id"), issueInput, triggerKind, triggerRef, "user", uid, invocation.S("id"))
}
