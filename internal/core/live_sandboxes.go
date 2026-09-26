package core

// liveSandboxes lists the sandboxes the lease holder should hold Node sessions with, so a
// restarted Controller can rebuild them without waiting for each Workspace's next operation. A
// sandbox qualifies when it is neither terminating nor terminated, belongs to its Workspace's
// current generation, and its ensure effect succeeded: only then does Cloud know the NodeId the
// handshake must present and the Substrate identity the router needs. A terminating sandbox is
// left out because its operation's terminate step already stopped its session on purpose.
//
// The read changes nothing, so it is safe to repeat; it is still fenced by the lease because only
// the holder may connect to Nodes, and a Node binds to a single Controller.
func liveSandboxes(t *transaction) Object {
	sandboxes := t.list(`SELECT s.id,s.workspace_id,s.generation,COALESCE(s.substrate_sandbox_id,e.external_id) AS substrate_sandbox_id,s.observed_state,e.result->>'nodeId' AS node_id
		FROM sandbox_instances s
		JOIN workspaces w ON w.id=s.workspace_id AND w.runtime_generation=s.generation
		JOIN external_effects e ON e.id=s.id AND e.kind='sandbox_ensure' AND e.state='succeeded'
		WHERE s.terminated_at IS NULL AND s.observed_state<>'terminating'
		ORDER BY s.id`)
	nodes := t.list(`SELECT n.* FROM node_instances n
		JOIN sandbox_instances s ON s.id=n.sandbox_instance_id
		WHERE s.terminated_at IS NULL AND n.ended_at IS NULL
		ORDER BY n.id`)
	bySandbox := map[string][]Object{}
	for _, n := range nodes {
		bySandbox[n.S("sandboxInstanceId")] = append(bySandbox[n.S("sandboxInstanceId")], n)
	}
	for _, s := range sandboxes {
		s["nodes"] = bySandbox[s.S("id")]
	}
	return Object{"sandboxes": sandboxes}
}
