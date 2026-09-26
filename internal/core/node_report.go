package core

// Controller-reported Nodes (specs decisions/cloud/operation/0-workspace-runtime-follows-desktop-node.md
// D4). A desktop Node holds no Cloud credential and only talks to its Controller, so the lease-holding
// Controller registers each Node incarnation it completed a handshake with and reports its heartbeat,
// disconnect, end and idle evidence. Lease validity was checked by the caller; the rules below bind
// every report to the Workspace's current generation and live sandbox, as the Node credential did.

// nodeReport runs one report_node_* action for the lease holder.
func nodeReport(t *transaction, r *ControlRequest) Object {
	if r.Action == "report_node_register" {
		return registerReportedNode(t, r)
	}
	id := r.Body.S("nodeInstanceId")
	require(validID(id), 404, "not_found")
	n := t.one("SELECT * FROM node_instances WHERE id=$1 AND node_incarnation_id IS NOT NULL", id)
	require(n != nil, 404, "not_found")
	sandbox := t.one("SELECT * FROM sandbox_instances WHERE id=$1", n.S("sandboxInstanceId"))
	if r.Action == "report_node_end" && n["endedAt"] != nil {
		// Ending is idempotent: the incarnation is already over, whoever observed it first.
		return n
	}
	require(n["endedAt"] == nil, 409, "stale_node")
	w := nodeScope(t, n.S("workspaceId"), sandbox.S("id"), sandbox.N("generation"))
	switch r.Action {
	case "report_node_status":
		nodeStatus(t, r, n, w)
	case "report_node_end":
		version(n, r.Body.N("version"))
		t.exec("UPDATE node_instances SET connection_state='ended',ended_at=now(),idle_admission_epoch=NULL,version=version+1 WHERE id=$1", id)
		closeUnavailable(t, w)
	case "report_node_idle":
		if refused := nodeIdle(t, r, n, w); refused != nil {
			return refused
		}
	default:
		reject(404, "not_found")
	}
	return t.one("SELECT * FROM node_instances WHERE id=$1", id)
}

// registerReportedNode allocates Cloud's row for one Node incarnation, idempotently by
// (sandbox, incarnation). The NodeId must be the one the sandbox's ensure effect reported, and a
// new incarnation waits until the previous one of the same sandbox was ended.
func registerReportedNode(t *transaction, r *ControlRequest) Object {
	sid, generation := r.Body.S("sandboxInstanceId"), r.Body.N("generation")
	nodeID, incarnation := r.Body.S("nodeId"), r.Body.S("nodeIncarnationId")
	require(validID(sid) && generation > 0 && nodeID != "" && len(nodeID) <= 200 && incarnation != "" && len(incarnation) <= 200, 400, "invalid_node_report")
	require(r.Body.N("protocolVersion") == 1, 400, "unsupported_protocol")
	sandbox := t.one("SELECT * FROM sandbox_instances WHERE id=$1", sid)
	require(sandbox != nil, 404, "not_found")
	w := nodeScope(t, sandbox.S("workspaceId"), sid, generation)
	if existing := t.one("SELECT * FROM node_instances WHERE sandbox_instance_id=$1 AND node_incarnation_id=$2", sid, incarnation); existing != nil {
		require(existing.S("nodeId") == nodeID && existing["endedAt"] == nil, 409, "stale_node")
		return existing
	}
	require(w.S("desiredState") == "running", 409, "execution_closed")
	ensuredNode(t, sid, nodeID)
	require(t.one("SELECT id FROM node_instances WHERE sandbox_instance_id=$1 AND ended_at IS NULL", sid) == nil, 409, "node_already_registered")
	// The handshake is complete when the Controller reports: a desktop Node finishes its own
	// recovery before it accepts a session, so the incarnation is initialized from the start.
	id := newID()
	t.exec("INSERT INTO node_instances(id,sandbox_instance_id,workspace_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id) VALUES($1,$2,$3,$4,'connected',1,true,$5,$6)", id, sid, w.S("id"), r.Service.Subject, nodeID, incarnation)
	return t.one("SELECT * FROM node_instances WHERE id=$1", id)
}
