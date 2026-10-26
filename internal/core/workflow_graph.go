package core

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Canonical workflow graph validation: the server-executable subset of the graph schema specs owns.
//
// The editor owns the document and specs owns the canonical schema; Cloud owns exactly the subset it
// can be asked to execute, and this file is that subset written down once. The publish gate, the run
// admission gate and the @ form's schema projection all read it, so a graph Cloud publishes is a
// graph Cloud has agreed to run, and the reason a graph is refused names the node or slot at fault
// instead of silently ignoring the part Cloud does not understand.
//
// Only publishing validates the graph. Saving is deliberately unchecked: an unfinished draft is a
// legitimate document — the author is mid-edit and the workflow is not runnable yet — so create and
// update accept any JSON object and only publish asks whether the document is complete. That split is
// the whole point of the draft/published distinction, and moving an install or completeness check
// into the save path is the mistake this file exists to prevent.

// workflowExecutableNodeKinds is the subset of the editor's node kinds Cloud executes. It started as
// the root decision's phase-1 set (#67 结论 4: start, agent, condition, output) and grows one
// decision at a time: `loop` and `loopExit` joined it with
// specs decisions/cloud/workflow/20261025-loop-and-loop-exit-execution.md, and `aggregator` with the
// variable-aggregator decision. Everything else stays editable and saveable, but publishing refuses
// it rather than freezing a graph whose nodes would be dropped.
var workflowExecutableNodeKinds = map[string]bool{
	"start":      true,
	"agent":      true,
	"condition":  true,
	"aggregator": true,
	"iteration":  true,
	"output":     true,
	"loop":       true,
	"loopExit":   true,
}

// workflowAgentSchemaVersion is the only `agentConfig.schemaVersion` Cloud executes. Version 4
// introduces the interactive completion declaration Cloud refuses by name; a version Cloud has
// never executed is refused by validateAgentSchemaVersion instead of being read optimistically.
const workflowAgentSchemaVersion = 3

// workflowUnsupportedAgentConfigFields are the agent-node configuration fields the first phase does
// not execute (#67 D2). The schema knows all of them and the editor may write them, but nothing in
// Cloud reads them, so publishing refuses them by name instead of freezing a document whose author
// believes a setting is in effect when no runner will ever apply it.
//
// They are listed rather than derived so the refusal stays a decision about *this* field, and in this
// order so the problem list a client renders is deterministic. `executorSlot`, `roleId`, `prompt` and
// `retry` are executed agent configuration and are deliberately absent here; the pre-slot
// `agentConfig.executor` is absent too, because specs#98 D5 makes it a field to accept and drop
// rather than refuse. `completion` is the interactive deliverable declaration (circus#426): Cloud has
// no completion gate to apply it to, so any non-null value is refused.
var workflowUnsupportedAgentConfigFields = []string{
	"skills",
	"mcps",
	"interactive",
	"outputContract",
	"completion",
	"promptInactivity",
}

// workflowDefaultSlot is the executor slot every graph may reference without declaring it, and the
// slot an agent node uses when it names none (specs#98 D2). It is reserved: a graph may not declare
// it, because declaring it would let an author redefine the one name every runner already has.
const workflowDefaultSlot = "default"

// workflowSlotNamePattern is the grammar of an executor slot name (specs#98 D1). The name is the key
// a runner's personal mapping is looked up by, so it stays a stable, easily compared identifier: a
// name with case or whitespace variants would look configured and still miss.
var workflowSlotNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Bounds on the author-written slot description, which is the only thing a runner sees when deciding
// what to map the slot to (specs#98 D1).
//
// They count characters, not bytes: the description is written for a human and D1 says so explicitly,
// and a byte count is a different limit in every language — 500 bytes is 500 Latin letters but about
// 166 Chinese ones, so a byte count would refuse a description the decision allows. Rune count is the
// unit `gitIdentityName` already uses for the same reason.
const (
	minSlotDescription = 1
	maxSlotDescription = 500
)

// maxSlotHintField bounds one `hint` string. specs#98 D3 fixes the hint's shape but no length, so this
// is Cloud's own ceiling on a value it only ever displays — kept, and counted in characters for the
// same reason the description is: a model id is an opaque string that may be written in any script,
// and a byte count would bound the Chinese ones four times tighter for no reason anyone could name.
const maxSlotHintField = 200

// workflowGraphProblem is one canonical-schema violation: a stable code plus the safe parameters that
// say where it is. It carries no prose because the wire contract is codes and parameters — a client
// localizes them, and a message written here would be a second, untranslated source of truth.
type workflowGraphProblem struct {
	Code   string
	NodeID string
	Slot   string
	Kind   string
	Field  string
	Name   string
}

// Object renders one problem for the fault's `problems` list, omitting the fields that do not apply.
// It takes its receiver by pointer because a problem is several strings and is rendered in a loop.
func (p *workflowGraphProblem) Object() Object {
	out := Object{"code": p.Code}
	if p.NodeID != "" {
		out["nodeId"] = p.NodeID
	}
	if p.Slot != "" {
		out["slot"] = p.Slot
	}
	if p.Kind != "" {
		out["kind"] = p.Kind
	}
	if p.Field != "" {
		out["field"] = p.Field
	}
	if p.Name != "" {
		out["name"] = p.Name
	}
	return out
}

// workflowNodes reads the graph's node list. The graph is a document the API never interprets, so a
// document whose nodes are not a list contributes no nodes rather than failing the read; publishing is
// what turns that into a problem.
func workflowNodes(graph Object) []any {
	nodes, _ := graph["nodes"].([]any)
	return nodes
}

// workflowGlobalVariables reads the graph's global-variable declarations. Like workflowNodes it never
// interprets the document further: an entry that is not an object contributes nothing here, and
// publishing reports the rest. It is shared by the publish gate (declaration and validation) and the
// run's ambient variable pool, so both read the same frozen list.
func workflowGlobalVariables(graph Object) []Object {
	declared, _ := graph["globalVariables"].([]any)
	out := []Object{}
	for _, item := range declared {
		if variable, ok := asWorkflowObject(item); ok {
			out = append(out, variable)
		}
	}
	return out
}

// normalizeWorkflowGlobals returns the graph with file and array[file] global values canonicalized the
// way desktop's into_global_variables does before freezing: a legacy path string becomes its
// {kind: "workspace_file", path} reference. Every other declaration is left as the author wrote it,
// and a file value that is already a canonical reference normalizes to itself. Publishing calls this
// after validation and before freezing, so the snapshot — and every consumer of it, a condition's
// variable pool included — sees the same shape desktop's pool produces. The graph must already pass
// validateWorkflowGraph: a value that reaches here was accepted there, so it is known to normalize.
func normalizeWorkflowGlobals(graph Object) Object {
	declared, ok := graph["globalVariables"].([]any)
	if !ok {
		return graph
	}
	globals := make([]any, len(declared))
	for i, item := range declared {
		globals[i] = item
		variable, ok := asWorkflowObject(item)
		if !ok {
			continue
		}
		valueType := variable.S("valueType")
		if valueType != "file" && valueType != "array[file]" {
			continue
		}
		value, hasValue := variable["value"]
		if !hasValue || value == nil {
			continue
		}
		normalized, ok := normalizeWorkflowValue(value, valueType)
		if !ok {
			continue
		}
		copied := Object{}
		for k, v := range variable {
			copied[k] = v
		}
		copied["value"] = normalized
		globals[i] = copied
	}
	graph["globalVariables"] = globals
	return graph
}

// normalizedSnapshotGraphText returns the text to freeze into a snapshot: the caller's verbatim
// document, unless normalizeWorkflowGlobals actually rewrote a legacy file or array[file] value, in
// which case the normalized graph is re-encoded. Keeping the raw text when nothing changed preserves
// the importer's byte-for-byte document storage: re-encoding a document whose numbers round-trip only
// through float64 could corrupt literals jsonb can still store, so the raw bytes are kept unless the
// snapshot must carry canonical file references. The graph must already pass validateWorkflowGraph.
func normalizedSnapshotGraphText(graph Object, raw string) string {
	before := jsonText(graph)
	if after := jsonText(normalizeWorkflowGlobals(graph)); after != before {
		return after
	}
	return raw
}

// validateWorkflowGlobals refuses global declarations the run could not honor, mirroring desktop's
// into_global_variables so a graph Cloud publishes is a graph desktop accepts. Each problem names the
// global it concerns. The rules, in order: the list itself must be a list, every entry an object, the
// name a qualified name (at least one dot and neither leading nor trailing dot), names must not
// repeat, the type must be supported, and — unless it is one of the two runtime-owned `sys.*`
// variables — the global must carry an initial value whose type matches its declaration. A present
// `sys.*` value is still checked against the type before the runtime discards it, exactly as desktop
// does. An absent or null `globalVariables` declares no globals and is not a problem.
func validateWorkflowGlobals(graph Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := graph["globalVariables"]
	if !present || raw == nil {
		return problems
	}
	declared, ok := raw.([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "globals_not_a_list"})
	}
	seen := map[string]bool{}
	for _, item := range declared {
		variable, ok := asWorkflowObject(item)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "global_not_an_object"})
			continue
		}
		name := strings.TrimSpace(variable.S("name"))
		// The platform's reserved form key is refused for that reason alone, and it is reported by
		// validateWorkflowReservedFields. Letting the qualified-name rule complain here too would ask
		// the author to fix a syntax problem as well as the name, while the name is the whole fault.
		// The comparison is exact because that is the comparison the form makes: a global is keyed by
		// its declared name, so only the exact name can collide with the platform's key.
		if variable.S("name") == fieldKeySnapshotID {
			continue
		}
		if !strings.Contains(name, ".") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
			problems = append(problems, workflowGraphProblem{Code: "global_name_invalid", Name: name})
			continue
		}
		if seen[name] {
			problems = append(problems, workflowGraphProblem{Code: "global_name_duplicate", Name: name})
			continue
		}
		seen[name] = true
		valueType := variable.S("valueType")
		if !workflowSupportedVariableType(valueType) {
			problems = append(problems, workflowGraphProblem{Code: "global_type_unsupported", Name: name})
			continue
		}
		// A null value is the same as no value — desktop's Option<Value> reads both as None.
		value, hasValue := variable["value"]
		if hasValue && value == nil {
			hasValue = false
		}
		if !hasValue && name != "sys.workflow_id" && name != "sys.timestamp" {
			problems = append(problems, workflowGraphProblem{Code: "global_value_missing", Name: name})
			continue
		}
		if hasValue {
			if _, ok := normalizeWorkflowValue(value, valueType); !ok {
				problems = append(problems, workflowGraphProblem{Code: "global_value_mismatch", Name: name})
			}
		}
	}
	return problems
}

// validateWorkflowGraph reports every way a graph violates the canonical schema, in document order so
// a client can render them next to the nodes they name. It is pure and total: a graph it cannot
// interpret produces problems rather than an error, because a malformed document is exactly what it
// is here to describe.
func validateWorkflowGraph(graph Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	nodes, ok := graph["nodes"].([]any)
	if !ok {
		problems = append(problems, workflowGraphProblem{Code: "nodes_not_a_list"})
		nodes = nil
	}
	if _, ok := graph["edges"].([]any); !ok {
		problems = append(problems, workflowGraphProblem{Code: "edges_not_a_list"})
	}
	globals := workflowGlobalVariables(graph)
	edges, _ := graph["edges"].([]any)
	problems = append(problems, validateWorkflowGlobals(graph)...)
	problems = append(problems, validateWorkflowNodes(nodes, edges, globals)...)
	problems = append(problems, validateWorkflowEdges(graph, nodes)...)
	// Container structure is checked after the edges exist and before the cycle walk: a body whose
	// membership or boundaries are wrong is described by its own codes, and the cycle walk reads the
	// same union of scopes either way.
	problems = append(problems, validateWorkflowContainers(nodes, edges)...)
	problems = append(problems, validateWorkflowAcyclic(graph)...)
	problems = append(problems, validateWorkflowSlots(graph, nodes)...)
	problems = append(problems, validateWorkflowReservedFields(nodes, globals)...)
	return problems
}

// validateWorkflowReservedFields refuses a graph that claims a name the platform owns in the `@`
// form as one of the workflow's own variables. It is the publish gate for a name, not for a value:
// the only way into the form is a declaration, so refusing the declaration is what keeps
// `snapshotId` meaning exactly one thing — the version the platform resolved — instead of sometimes
// also being a value the author asked the workflow to receive
// (20261021-snapshot-id-is-a-reserved-field.md D1).
//
// The walk is deliberately the same one the version selector uses to withhold stock versions
// (workflowDeclaresReservedField): the gate and the selector have to agree about what a declaration
// is, or a version publishing accepts could still be withheld, or the reverse. It is also why the
// qualified-name rule in validateWorkflowGlobals steps aside for this name: that rule is about
// qualification syntax, and it would only add a second, less useful complaint about the same entry.
//
// Only a Start input variable and a global variable can enter the form, so only those two positions
// are read. `launchFields` withdrawing `snapshotId` is not a declaration and is not read here: it
// asks the platform to drop its own field, which cannot collide with the cursor either.
func validateWorkflowReservedFields(nodes []any, globals []Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		o := Object(node)
		data := o.O("data")
		// A loop body's Start is not the form's Start: its variables never enter the `@` form, so a
		// reserved name there cannot collide with the platform's field.
		if data.S("kind") != "start" || workflowContainerID(o) != "" {
			continue
		}
		declared, _ := data["inputVariables"].([]any)
		for _, rawVariable := range declared {
			variable, ok := rawVariable.(map[string]any)
			if !ok {
				continue
			}
			if Object(variable).S("name") == fieldKeySnapshotID {
				problems = append(problems, workflowGraphProblem{Code: "reserved_field_name", NodeID: o.S("id"), Field: fieldKeySnapshotID})
			}
		}
	}
	// A global enters the form under its declared name, so the comparison is exact: a name that differs
	// only by surrounding whitespace is a different key and is dropped from the form by the
	// qualified-name rule rather than colliding with the platform's. It carries `name` rather than
	// `field`: the global is not inside a node, and the entry the author has to change is the global's
	// own name.
	for _, variable := range globals {
		if variable.S("name") == fieldKeySnapshotID {
			problems = append(problems, workflowGraphProblem{Code: "reserved_field_name", Name: fieldKeySnapshotID})
		}
	}
	return problems
}

// workflowDeclaresReservedField reports whether a graph claims the platform's reserved form key at
// all. It asks the publish gate's own walk, so the versions the selector withholds, the versions a
// confirm refuses and the declarations publishing refuses are one rule rather than three that could
// drift apart.
func workflowDeclaresReservedField(graph Object) bool {
	return len(validateWorkflowReservedFields(workflowNodes(graph), workflowGlobalVariables(graph))) > 0
}

// validateWorkflowAcyclic refuses a graph that contains a cycle. The first phase executes a DAG and
// nothing else (#67 结论 4, D1 条件④): the projection that decides what runs is a topological read of
// committed state, so a node on a cycle either waits for an upstream that waits for it or goes round
// once and drops its back edge, and neither is what its author drew. Publishing would freeze a
// document whose run cannot mean what it says, which is exactly what this gate exists to prevent.
// Loops and iterations are not part of the first phase, so a graph that wants one is refused rather
// than executed as something else.
//
// The reported node is the source of the back edge the walk found, which is on a cycle by
// construction; a node merely downstream of one is not reported, because the author has nothing to
// fix there. The walk itself is shared with the run's advance pass — see workflowGraphCycle — so a
// graph Cloud refuses to publish and a graph Cloud refuses to run are the same graph.
//
// The graph is read by the same reader the run uses, so a document whose nodes are not a list is
// simply empty here: validateWorkflowNodes has already said so, and this pass must not pile a second,
// less useful complaint on top of it.
func validateWorkflowAcyclic(graph Object) []workflowGraphProblem {
	source := workflowGraphCycle(readWorkflowGraph(graph))
	if source == "" {
		return nil
	}
	return []workflowGraphProblem{{Code: "graph_cycle", NodeID: source}}
}

// validateWorkflowNodes checks node identity, kind, per-kind configuration and the start-node count.
// The editor allows at most one Start node in the outer graph, and a run needs exactly one entry
// point, so zero and two are both refused: a graph with none is unfinished, and one with two has no
// defined place to begin. A loop body's own start is a second `start` in the document; it is counted
// and checked by validateWorkflowContainers, not here, and it never becomes the run's entry point.
//
// Selectors are validated against the roots visible in the node's scope, not the flat declaration
// set: a body node may read its loop's carried variables, and an outer node may read the loop's
// published outputs. See workflowVisibleRoots.
func validateWorkflowNodes(nodes, edges []any, globals []Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	seen := map[string]bool{}
	declaredRoots := workflowDeclaredRoots(nodes, globals)
	containers := workflowNodeContainers(nodes)
	iterations := map[string]bool{}
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if ok && node.O("data").S("kind") == "iteration" {
			iterations[node.S("id")] = true
		}
	}
	loops := workflowLoopRootsByName(nodes)
	starts := 0
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "node_not_an_object"})
			continue
		}
		o := Object(node)
		id := o.S("id")
		if id == "" {
			problems = append(problems, workflowGraphProblem{Code: "node_id_missing"})
			continue
		}
		if seen[id] {
			problems = append(problems, workflowGraphProblem{Code: "node_id_duplicate", NodeID: id})
			continue
		}
		seen[id] = true
		kind := o.O("data").S("kind")
		visibleRoots := workflowScopeRoots(nodes, edges, declaredRoots, id)
		if iterations[containers[id]] {
			// A region's members read the current round's item and index, which exist only inside
			// that region and only while it runs.
			visibleRoots[containers[id]] = map[string]bool{"item": true, "index": true}
		}
		switch {
		case kind == "":
			problems = append(problems, workflowGraphProblem{Code: "node_kind_missing", NodeID: id})
		case !workflowExecutableNodeKinds[kind]:
			problems = append(problems, workflowGraphProblem{Code: "node_kind_unsupported", NodeID: id, Kind: kind})
		case kind == "start":
			if containers[id] == "" {
				starts++
			}
		case kind == "agent":
			// Only an agent node is asked about its configuration, and only once its kind is known to
			// be one Cloud executes: a `tool` node carrying `skills` is already refused for the kind,
			// and reporting its config too would ask the author to fix a field of a node they have to
			// delete anyway.
			problems = append(problems, validateAgentConfig(id, o.O("data"))...)
		case kind == "condition":
			if containers[id] == "" {
				visibleRoots = workflowVisibleRoots(declaredRoots, loops, "")
				for member, container := range containers {
					if container != "" {
						delete(visibleRoots, member)
					}
				}
			}
			problems = append(problems, validateConditionConfig(id, o.O("data"), visibleRoots)...)
		case kind == "output":
			problems = append(problems, validateWorkflowOutputConfig(id, o.O("data"), visibleRoots)...)
		case kind == "aggregator":
			allRoots := workflowVisibleRoots(declaredRoots, nil, containers[id])
			if iterations[containers[id]] {
				allRoots[containers[id]]["item"] = true
				allRoots[containers[id]]["index"] = true
			}
			problems = append(problems, workflowAggregatorScopeProblems(id, o.O("data"), allRoots, visibleRoots)...)
		case kind == "iteration":
			problems = append(problems, validateIterationConfig(id, o.O("data"), nodes, edges, declaredRoots)...)
		case kind == "loop" && containers[id] == "":
			// The loop's own initial values resolve in the outer scope, while feedback, until and
			// outputs resolve against a round pool; validateLoopConfig derives both from the flat
			// declarations instead of the node's container visibility.
			problems = append(problems, validateLoopConfig(id, o.O("data"), declaredRoots, loops, nodes, edges)...)
		}
	}
	if starts != 1 {
		problems = append(problems, workflowGraphProblem{Code: "start_node_count"})
	}
	return problems
}

// validateAgentConfig refuses the agent-node configuration the first phase does not execute, naming
// both the node and the field (#67 D2), and checks the shape of the configuration it does execute.
// The alternative — ignoring an unsupported field — is the one outcome the decision rules out: an
// author who set `interactive` would read a publish that succeeded as "my setting is in effect", and
// nothing would ever apply it.
//
// A key present with a JSON null is not a refusal, and neither is the value the editor writes when
// nothing is enabled: an empty `skills`/`mcps` list or `interactive: false`. The decision's reason
// for refusing these fields is that they change execution behavior, and a null changes none, exactly
// like an empty list or a false flag; refusing them would be the same mistake in the other
// direction, rejecting a document for a value that asks for nothing. The same reading is what
// `validSlotHint` applies to an absent-or-null `hint`. `retry` is no longer refused — it is executed
// (retry decision D1) — so it is validated for shape instead of rejected.
func validateAgentConfig(nodeID string, data Object) []workflowGraphProblem {
	config := data.O("agentConfig")
	problems := validateAgentSchemaVersion(nodeID, config)
	for _, field := range workflowUnsupportedAgentConfigFields {
		value, present := config[field]
		if !present || value == nil || !agentConfigFieldEnabled(field, value) {
			continue
		}
		problems = append(problems, workflowGraphProblem{Code: "node_config_unsupported", NodeID: nodeID, Field: field})
	}
	return append(problems, validateAgentRetryConfig(nodeID, config)...)
}

// validateAgentSchemaVersion refuses an agent configuration whose schema version Cloud does not
// execute. The version is the envelope that says which fields carry meaning: version 4 introduces
// the interactive completion declaration, which Cloud rejects by name, and a version Cloud has
// never seen could carry other behavior changes it cannot apply. Only version 3 is accepted; an
// absent or null version is a document written before versions were checked, and keeps the
// behavior it already had.
//
// Version 1 and 2 are refused as well: Cloud has never interpreted them, so accepting them would
// be the same silent assumption this gate exists to prevent. The check runs before the field list
// so a graph that is both mis-versioned and carrying an unsupported field reports the envelope
// first.
func validateAgentSchemaVersion(nodeID string, config Object) []workflowGraphProblem {
	value, present := config["schemaVersion"]
	if !present || value == nil {
		return nil
	}
	if version, ok := value.(float64); ok && version == workflowAgentSchemaVersion {
		return nil
	}
	return []workflowGraphProblem{{Code: "agent_schema_version_unsupported", NodeID: nodeID, Field: "agentConfig.schemaVersion"}}
}

// validateAgentRetryConfig checks `agentConfig.retry` against the policy shape the scheduler executes
// (retry decision D1). Absent and null mean the default policy, so neither is a problem. A present
// value must be an object carrying all three fields, each with its own type and range, because a
// partially filled object would make the author execute a policy they never wrote: the missing fields
// would silently fall back to defaults, so "I only wanted the delay" would also change the enabled
// flag or the budget. The object's shape is validated as a unit and every invalid field is reported,
// so one publish answers with the whole list the author has to fix.
//
// Unknown keys are ignored, like everywhere else in the graph document: the schema may grow fields
// Cloud does not execute yet, and refusing them here would make publishing fail for a newer editor.
func validateAgentRetryConfig(nodeID string, config Object) []workflowGraphProblem {
	raw, present := config["retry"]
	if !present || raw == nil {
		return nil
	}
	retry, ok := asWorkflowObject(raw)
	if !ok {
		return []workflowGraphProblem{{Code: "agent_retry_not_an_object", NodeID: nodeID, Field: "agentConfig.retry"}}
	}
	problems := []workflowGraphProblem{}
	if _, ok := retry["enabled"].(bool); !ok {
		problems = append(problems, workflowGraphProblem{Code: "agent_retry_enabled_invalid", NodeID: nodeID, Field: "agentConfig.retry.enabled"})
	}
	if n, ok := workflowRetryInteger(retry["maxRetries"]); !ok || n < workflowRetryMinMaxRetries || n > workflowRetryMaxMaxRetries {
		problems = append(problems, workflowGraphProblem{Code: "agent_retry_max_retries_invalid", NodeID: nodeID, Field: "agentConfig.retry.maxRetries"})
	}
	if n, ok := workflowRetryInteger(retry["initialDelaySeconds"]); !ok ||
		n < workflowRetryMinInitialDelaySeconds || n > workflowRetryMaxInitialDelaySeconds {
		problems = append(problems, workflowGraphProblem{Code: "agent_retry_initial_delay_invalid", NodeID: nodeID, Field: "agentConfig.retry.initialDelaySeconds"})
	}
	return problems
}

// agentConfigFieldEnabled reports whether a non-null agent-config value still enables behavior the
// first phase does not execute. `skills` and `mcps` are lists whose empty form is the editor's
// "nothing selected" default, and `interactive` is a boolean whose `false` is the same; those are the
// only fields with a "nothing selected" form, so every other non-null value is enabled. A value of
// the wrong shape is enabled: the validator refuses by name and does not re-check the schema.
func agentConfigFieldEnabled(field string, value any) bool {
	switch field {
	case "skills", "mcps":
		list, ok := value.([]any)
		return !ok || len(list) != 0
	case "interactive":
		enabled, ok := value.(bool)
		return !ok || enabled
	default:
		return true
	}
}

// workflowUnsupportedConditionConfigs is the list of condition-node configuration Cloud cannot
// evaluate the way desktop's condition.rs does, and which publishing therefore refuses. It is
// written out rather than derived because it is a statement about the two implementations, not about
// Cloud's own code: each entry is a place where the evaluator would otherwise answer silently —
// an unknown operator is a no-match, an unknown logic falls back to `and`, a missing case id becomes
// the else branch, and a one-segment selector becomes the whole node output (circus#202 A 条件 2,
// #141 结论 5). Every one of them is decidable from the document alone, which is what makes refusing
// at publish possible; the differences that depend on a *resolved* value's type at run time are not,
// and are named in the ADR instead.
//
// A condition node with no cases, or a case with no rules, is deliberately NOT on this list: falling
// through to else is what an unfinished node is meant to do, and both sides already agree on it.
//
// circus#210 closed the first entry: an expected value carrying `{{#selector#}}` now resolves the
// same way desktop does — an exclusive placeholder keeps the bound JSON type, a mixed one renders a
// string, and an unset selector fails the node — so the gate no longer refuses it.
//
// `condition_selector_undeclared` is the one entry about the variable pool rather than the operator:
// desktop declares every node's roots up front, so a selector naming a node or root the graph never
// declares is `Undeclared` and fails the node, while Cloud's evaluator has no declaration set and
// would answer "absent" — `exists` would return false and the run would take a branch the author
// could not have written. The node exists and its kind exposes a fixed set of roots, so the gate can
// decide this from the document alone, which is what makes refusing at publish the right side.
var workflowUnsupportedConditionConfigs = []string{
	"condition_cases_not_a_list",
	"condition_case_not_an_object",
	"condition_case_id_missing",
	"condition_logic_unsupported",
	"condition_rules_not_a_list",
	"condition_rule_not_an_object",
	"condition_selector_unsupported",
	"condition_selector_undeclared",
	"condition_operator_unsupported",
}

// workflowDeclaredRoots maps each graph node to the variable roots its kind exposes, the declaration
// set desktop's WorkflowVariablePool::from_graph builds before a run starts. The roots follow the
// approved contract rather than what today's nodes happen to write: an agent exposes `output` and
// `structured_output` when it declares an output contract, an output node exposes `output`, a start
// node exposes `input` plus its declared form variables, and a condition node exposes none. The
// agent and output values arrive with cloud#70/#74; until then a selector on them is declared but
// unassigned, the same state desktop has before the node produces its output.
//
// The map also holds the pool's two global sources, keyed by the same first-segment writer a selector
// uses: the runtime-owned `sys.workflow_id` and `sys.timestamp` are always declared, and each user
// global declares its writer's second segment. A global names a three-segment key like `foo.bar.baz`,
// but desktop's pool resolves a selector by its first two segments only, so such a global is declared
// yet unreachable there — Cloud therefore declares only two-segment globals and refuses a selector on
// the longer name the same way desktop reports it Undeclared.
//
// The map is keyed by node id and holds an empty set for kinds that expose no roots, so a selector
// on a condition node is refused rather than accidentally matching an absent key.
func workflowDeclaredRoots(nodes []any, globals []Object) map[string]map[string]bool {
	roots := make(map[string]map[string]bool, len(nodes))
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		o := Object(node)
		id := o.S("id")
		if id == "" {
			continue
		}
		data := o.O("data")
		set := map[string]bool{}
		switch data.S("kind") {
		case "start":
			set["input"] = true
			declared, _ := data["inputVariables"].([]any)
			for _, rawVariable := range declared {
				variable, ok := rawVariable.(map[string]any)
				if !ok {
					continue
				}
				if name := Object(variable).S("name"); name != "" {
					set[name] = true
				}
			}
		case "agent":
			set["output"] = true
			// The contract lives in the node's `agentConfig`, the same place desktop's wire node reads
			// it from; a contract present with a null value declares nothing extra. desktop declares
			// structured_output only for an OutputContract::Structured, while this accepts any non-null
			// contract; the difference is unreachable today because outputContract is refused by
			// node_config_unsupported, so tighten it when cloud#70/#74 makes contracts reachable.
			if contract, present := data.O("agentConfig")["outputContract"]; present && contract != nil {
				set["structured_output"] = true
			}
		case "output":
			set["output"] = true
		case "aggregator":
			// The aggregator publishes exactly one root: the value of the first assigned candidate.
			set["output"] = true
		case "iteration":
			set["output"] = true
			set["entries"] = true
			set["failed_count"] = true
		case "loop":
			// A loop exposes two root groups under the same writer: its carried variables (visible
			// only inside the body) and its named outputs (visible only outside). They share a name
			// space here because the visibility filter separates them by scope; the runtime never
			// writes both under one key.
			for _, variable := range workflowObjectEntries(data.O("loopConfig")["variables"]) {
				if name := variable.S("name"); name != "" {
					set[name] = true
				}
			}
			for _, output := range workflowObjectEntries(data.O("loopConfig")["outputs"]) {
				if name := output.S("name"); name != "" {
					set[name] = true
				}
			}
		}
		roots[id] = set
	}
	declareWorkflowRoot(roots, "sys", "workflow_id")
	declareWorkflowRoot(roots, "sys", "timestamp")
	for _, variable := range globals {
		if writer, root, ok := workflowGlobalSelector(variable.S("name")); ok {
			declareWorkflowRoot(roots, writer, root)
		}
	}
	return roots
}

// declareWorkflowRoot records one writer-owned root, creating the writer's set on first use. The
// writer is either a graph node id or a global's first name segment, so a global namespace that
// shares a node's id unions its root into the node's set — the same flat key space desktop's pool
// uses.
func declareWorkflowRoot(roots map[string]map[string]bool, writer, root string) {
	set, ok := roots[writer]
	if !ok {
		set = map[string]bool{}
		roots[writer] = set
	}
	set[root] = true
}

// workflowGlobalSelector splits a validated global name into the writer and root a selector would
// address. It reports ok=false unless the name is exactly two dot-separated segments, because
// desktop's selector resolution joins only the first two segments — a three-segment global is
// declared under its full name and never reachable, so Cloud must not declare it reachable either.
func workflowGlobalSelector(name string) (writer, root string, ok bool) {
	i := strings.IndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	writer, root = name[:i], name[i+1:]
	if strings.Contains(root, ".") {
		return "", "", false
	}
	return writer, root, true
}

// validateConditionConfig refuses the condition configuration the first phase does not evaluate the
// way desktop does. Every problem names the node and the field, so the author is told which branch
// of which node to fix rather than that the graph is invalid somewhere.
func validateConditionConfig(nodeID string, data Object, declaredRoots map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := data["cases"]
	if !present || raw == nil {
		return problems
	}
	cases, ok := raw.([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_cases_not_a_list", NodeID: nodeID, Field: "cases"})
	}
	for _, item := range cases {
		c, ok := asWorkflowObject(item)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "condition_case_not_an_object", NodeID: nodeID, Field: "cases"})
			continue
		}
		if c.S("id") == "" {
			problems = append(problems, workflowGraphProblem{Code: "condition_case_id_missing", NodeID: nodeID, Field: "cases.id"})
		}
		// An absent logic is `and`, which is what the evaluator applies and what the editor omits;
		// only a value neither side recognizes is refused.
		switch c.S("logic") {
		case "", "and", "or":
		default:
			problems = append(problems, workflowGraphProblem{Code: "condition_logic_unsupported", NodeID: nodeID, Field: "cases.logic"})
		}
		rules, ok := c["conditions"].([]any)
		if !ok {
			// An absent or null rule list is the same unfinished case as an empty one, and is left
			// to fall through; a list of some other type is a document this gate cannot read.
			if value, present := c["conditions"]; present && value != nil {
				problems = append(problems, workflowGraphProblem{Code: "condition_rules_not_a_list", NodeID: nodeID, Field: "cases.conditions"})
			}
			continue
		}
		for _, rawRule := range rules {
			problems = append(problems, validateConditionRule(nodeID, rawRule, declaredRoots)...)
		}
	}
	return problems
}

// validateConditionRule checks one rule of one case. The operator set is read from the evaluator's
// own table rather than restated, so the gate cannot admit an operator the evaluator would then
// no-match — the two lists are the same list.
func validateConditionRule(nodeID string, raw any, declaredRoots map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	r, ok := asWorkflowObject(raw)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_rule_not_an_object", NodeID: nodeID, Field: "cases.conditions"})
	}
	selector, ok := r["variableSelector"].([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
	}
	// The selector needs the node AND a path into its output. The editor hands a new comparison an
	// empty list, and one element names the whole output, which desktop refuses and Cloud would read
	// as everything the node produced.
	wellFormed := len(selector) >= 2
	if !wellFormed {
		problems = append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
	}
	for _, element := range selector {
		if text, ok := element.(string); !ok || text == "" {
			problems = append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
			wellFormed = false
			break
		}
	}
	// A well-formed selector must still name a node the graph declares and a root that node's kind
	// exposes. Desktop declares the pool from the graph, so anything else is Undeclared there and
	// fails the node; Cloud has no declarations at run time and would quietly answer "absent", so the
	// gate refuses the document instead. Malformed selectors already failed above; reporting them
	// twice would only bury the fixable problem.
	if wellFormed {
		node, nodeOK := selector[0].(string)
		root, rootOK := selector[1].(string)
		if !nodeOK || !rootOK || !declaredRoots[node][root] {
			problems = append(problems, workflowGraphProblem{Code: "condition_selector_undeclared", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
		}
	}
	if _, ok := workflowConditionOperators[r.S("operator")]; !ok {
		problems = append(problems, workflowGraphProblem{Code: "condition_operator_unsupported", NodeID: nodeID, Field: "cases.conditions.operator"})
	}
	// An expected-value template names the same variable pool as variableSelector, so a placeholder
	// must hold to the same declarations. Cloud's condition node writes `selectedBranch` at run time,
	// but desktop declares no root on a condition node, so without this check a template like
	// {{#c.selectedBranch#}} would resolve on Cloud and fail desktop; the gate refuses it the same way
	// it refuses a bare selector. Only strings can carry a placeholder.
	if text, ok := r["value"].(string); ok {
		for _, selector := range workflowTemplateSelectors(text) {
			if !declaredRoots[selector.nodeID][selector.root] {
				problems = append(problems, workflowGraphProblem{Code: "condition_selector_undeclared", NodeID: nodeID, Field: "cases.conditions.value"})
			}
		}
	}
	return problems
}

// validateWorkflowEdges checks that every edge joins two nodes that exist. An edge to a node the
// document does not contain is a dangling reference the run would have to guess at, and the editor
// only produces one when the document was edited outside it.
func validateWorkflowEdges(graph Object, nodes []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	edges, _ := graph["edges"].([]any)
	known := map[string]bool{}
	for _, raw := range nodes {
		if node, ok := raw.(map[string]any); ok {
			if id := Object(node).S("id"); id != "" {
				known[id] = true
			}
		}
	}
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "edge_not_an_object"})
			continue
		}
		o := Object(edge)
		for _, endpoint := range []string{o.S("source"), o.S("target")} {
			if endpoint == "" || !known[endpoint] {
				problems = append(problems, workflowGraphProblem{Code: "edge_endpoint_unknown", NodeID: endpoint})
			}
		}
	}
	return problems
}

// validateWorkflowSlots checks the graph's executor-slot declarations and the agent nodes' references
// to them (specs#98 D1/D2). A reference to an undeclared slot is refused here rather than at run
// time: the author is the only one who can say what the slot means, and a run created against an
// undeclared slot could never resolve an executor for it.
//
// A declared slot no node references is *not* a problem: an author may be mid-edit or keeping one for
// later, and refusing it would block publishing for a mistake with no runtime consequence.
func validateWorkflowSlots(graph Object, nodes []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	declared := map[string]bool{}
	for _, slot := range workflowSlotDeclarations(graph) {
		name := slot.S("name")
		switch {
		case name == "":
			problems = append(problems, workflowGraphProblem{Code: "slot_name_missing"})
		case !workflowSlotNamePattern.MatchString(name):
			problems = append(problems, workflowGraphProblem{Code: "slot_name_invalid", Slot: name})
		case name == workflowDefaultSlot:
			problems = append(problems, workflowGraphProblem{Code: "slot_name_reserved", Slot: name})
		case declared[name]:
			problems = append(problems, workflowGraphProblem{Code: "slot_name_duplicate", Slot: name})
		}
		if name != "" {
			declared[name] = true
		}
		description := strings.TrimSpace(slot.S("description"))
		if n := utf8.RuneCountInString(description); n < minSlotDescription || n > maxSlotDescription {
			problems = append(problems, workflowGraphProblem{Code: "slot_description_invalid", Slot: name})
		}
		if hint, ok := slot["hint"]; ok && hint != nil && !validSlotHint(hint) {
			problems = append(problems, workflowGraphProblem{Code: "slot_hint_invalid", Slot: name})
		}
	}
	for _, reference := range workflowReferencedSlots(nodes) {
		if reference.Name == workflowDefaultSlot || declared[reference.Name] {
			continue
		}
		for _, nodeID := range reference.Nodes {
			problems = append(problems, workflowGraphProblem{Code: "slot_not_declared", NodeID: nodeID, Slot: reference.Name})
		}
	}
	return problems
}

// validSlotHint reports whether a hint is the shape specs#98 D3 declares: an object whose `agentCli`
// and `modelId`, when present, are non-empty strings. A hint never resolves an executor, so it is
// checked only far enough to keep the document well formed for the runner that reads it.
func validSlotHint(hint any) bool {
	h, ok := asWorkflowObject(hint)
	if !ok {
		return false
	}
	for _, key := range []string{"agentCli", "modelId"} {
		value, present := h[key]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" || utf8.RuneCountInString(text) > maxSlotHintField {
			return false
		}
	}
	return true
}

// workflowSlotDeclarations reads the graph's executor-slot declarations. The graph is a document the
// API never interprets, so an entry that is not an object contributes nothing rather than failing the
// request; publishing is what turns the rest into problems.
func workflowSlotDeclarations(graph Object) []Object {
	declared, _ := graph["executorSlots"].([]any)
	out := []Object{}
	for _, item := range declared {
		if slot, ok := asWorkflowObject(item); ok {
			out = append(out, slot)
		}
	}
	return out
}

// workflowSlotReference is one executor slot an agent node asks for, together with the nodes that ask
// for it. Both travel together because every diagnostic and every runner-facing message has to name
// the slot *and* the node: "strong is not configured" is only actionable once the author knows which
// step depends on it.
type workflowSlotReference struct {
	Name  string
	Nodes []string
}

// workflowReferencedSlots collects the executor slots a graph's agent nodes reference, in the order
// the nodes appear so a message lists them the way the canvas draws them. An agent node that names no
// slot references `default` (specs#98 D2), which is also how a node still carrying the pre-slot
// `agentConfig.executor` field is read: cloud does not migrate its stored documents, so an old graph
// keeps running as the one-slot graph it always was (specs#98 D5).
//
// The node list is a slice because a node id may be repeated in a malformed document, and an entry
// with no id is dropped: an unnamed node cannot be pointed at in a message.
func workflowReferencedSlots(nodes []any) []workflowSlotReference {
	index := map[string]int{}
	out := []workflowSlotReference{}
	for _, raw := range nodes {
		o, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		data := o.O("data")
		if data.S("kind") != "agent" {
			continue
		}
		id := o.S("id")
		if id == "" {
			continue
		}
		name := data.O("agentConfig").S("executorSlot")
		if name == "" {
			name = workflowDefaultSlot
		}
		if i, ok := index[name]; ok {
			out[i].Nodes = append(out[i].Nodes, id)
			continue
		}
		index[name] = len(out)
		out = append(out, workflowSlotReference{Name: name, Nodes: []string{id}})
	}
	return out
}

// requireValidWorkflowGraph refuses a publish whose graph violates the canonical schema. The
// violations travel as parameters so the editor can mark each node, and the fault stays one stable
// code so a client cannot accidentally treat one kind of violation as another.
func requireValidWorkflowGraph(graph Object) {
	problems := validateWorkflowGraph(graph)
	if len(problems) == 0 {
		return
	}
	list := make([]any, 0, len(problems))
	for _, problem := range problems {
		list = append(list, problem.Object())
	}
	rejectWith(400, "workflow_graph_invalid", Object{"problems": list})
}
