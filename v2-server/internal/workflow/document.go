package workflow

import (
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/strictjson"
)

type object = map[string]any

type node struct {
	id, kind, owner string
	container       bool
	raw, data       object
	selectors       [][]string
	templates       []string
	bindings        [][]string // Condition/Output bindings, subject to Loop visibility.
	aggregator      [][]string
	iterator        []string
	collect         []string
	initial         [][]string
	loopRefs        [][]string
	inputs          map[string]string
	structured      bool
	interactive     bool
}

type edge struct {
	source, target string
	raw            object
}

type document struct {
	raw      object
	nodes    []*node
	byID     map[string]*node
	edges    []edge
	active   map[string]bool
	unused   map[string]bool
	analysis Analysis
}

func knownKind(kind string) bool {
	switch kind {
	case "start", "agent", "prompt", "condition", "aggregator", "tool", "output", "loop", "loopExit", "iteration":
		return true
	}
	return false
}

func nonemptyString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && strings.TrimSpace(s) != ""
}

// project follows desktop execution_document.rs: structural defects anywhere in
// the document cannot be hidden by excluding spare nodes from configuration checks.
func project(source string) (*document, error) {
	if !strictjson.Valid([]byte(source)) {
		return nil, invalid("invalid_json", "", "graph", "expected lossless UTF-8 JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	var raw object
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return nil, invalid("invalid_json", "", "graph", "expected a JSON object")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, invalid("invalid_json", "", "graph", "expected exactly one JSON document")
	}
	if !finiteNumbers(raw) {
		return nil, invalid("invalid_json", "", "graph", "JSON number is outside the supported range")
	}
	nodes, ok := raw["nodes"].([]any)
	if !ok {
		return nil, invalid("missing_nodes", "", "nodes", "expected an array")
	}
	edges, ok := raw["edges"].([]any)
	if !ok {
		return nil, invalid("missing_edges", "", "edges", "expected an array")
	}
	d := &document{raw: raw, byID: map[string]*node{}, active: map[string]bool{}, unused: map[string]bool{},
		analysis: Analysis{UnusedNodeIDs: []string{}, UnrecognizedNodes: []UnrecognizedNode{}}}
	starts := map[string]string{}
	hasLoop := false
	for _, value := range nodes {
		rawNode, _ := value.(object)
		id, valid := nonemptyString(rawNode["id"])
		if !valid {
			return nil, invalid("invalid_node", "", "id", "missing non-empty node ID")
		}
		data, _ := rawNode["data"].(object)
		kind, valid := nonemptyString(data["kind"])
		if !valid {
			return nil, invalid("invalid_node", id, "data.kind", "missing node kind")
		}
		n := &node{id: id, kind: kind, raw: rawNode, data: data}
		if v, exists := data["containerId"]; exists {
			n.container = true
			if n.owner, valid = nonemptyString(v); !valid {
				return nil, invalid("invalid_node", id, "data.containerId", "expected a non-empty string")
			}
		}
		if v := rawNode["parentId"]; v != nil {
			parent, valid := nonemptyString(v)
			if !valid || (n.container && parent != n.owner) {
				return nil, invalid("invalid_node", id, "parentId", "invalid parent or disagreement with containerId")
			}
			n.owner = parent
		}
		if d.byID[id] != nil {
			return nil, invalid("duplicate_node_id", id, "id", "node IDs must be unique")
		}
		d.byID[id] = n
		d.nodes = append(d.nodes, n)
		if kind == "start" {
			if _, exists := starts[n.owner]; exists {
				return nil, invalid("multiple_start_nodes", id, "data.kind", "more than one Start in a scope")
			}
			starts[n.owner] = id
		}
		hasLoop = hasLoop || kind == "loop"
		if !knownKind(kind) {
			d.analysis.UnrecognizedNodes = append(d.analysis.UnrecognizedNodes, UnrecognizedNode{NodeID: id, Kind: kind})
		}
	}
	for _, n := range d.nodes {
		if n.owner == "" {
			continue
		}
		parent := d.byID[n.owner]
		expected := "iteration"
		if n.container {
			expected = "loop"
		}
		if parent == nil || parent.kind != expected || parent.owner != "" || n.kind == "loop" || n.kind == "iteration" {
			return nil, invalid("invalid_region", n.id, "parentId", "invalid or nested container ownership")
		}
	}
	if hasLoop && raw["schemaVersion"] != json.Number("2") {
		return nil, invalid("invalid_node", "", "schemaVersion", "container graphs require schemaVersion 2")
	}
	outgoing := map[string][]string{}
	for _, value := range edges {
		rawEdge, _ := value.(object)
		source, validSource := nonemptyString(rawEdge["source"])
		target, validTarget := nonemptyString(rawEdge["target"])
		if !validSource || !validTarget {
			return nil, invalid("invalid_node", "", "edges", "edge requires non-empty source and target")
		}
		from, to := d.byID[source], d.byID[target]
		if from == nil || to == nil {
			return nil, invalid("dangling_edge", "", "edges", "edge references a missing node")
		}
		if from.owner != to.owner && !(from.kind == "iteration" && to.owner == source) {
			return nil, invalid("invalid_region", source, "edges", "edge crosses a container boundary")
		}
		d.edges = append(d.edges, edge{source: source, target: target, raw: rawEdge})
		outgoing[source] = append(outgoing[source], target)
	}
	if starts[""] == "" {
		// Missing entry is a start-time error. Publication still checks the entire graph.
		for _, n := range d.nodes {
			d.active[n.id] = true
		}
		return d, nil
	}
	frontier := []string{starts[""]}
	for len(frontier) != 0 {
		id := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if d.active[id] {
			continue
		}
		d.active[id] = true
		if d.byID[id].kind == "loop" {
			entry := starts[id]
			if entry == "" {
				return nil, invalid("invalid_node", id, "nodes", "every active Loop requires one Start node")
			}
			frontier = append(frontier, entry)
		}
		frontier = append(frontier, outgoing[id]...)
	}
	for _, n := range d.nodes {
		if !d.active[n.id] {
			d.unused[n.id] = true
			if knownKind(n.kind) {
				d.analysis.UnusedNodeIDs = append(d.analysis.UnusedNodeIDs, n.id)
			}
		}
	}
	sort.Strings(d.analysis.UnusedNodeIDs)
	return d, nil
}

// serde_json rejects numbers outside the finite f64 range even in metadata.
// Keep integer lexemes for the subsequent typed integer checks.
func finiteNumbers(value any) bool {
	switch v := value.(type) {
	case json.Number:
		_, err := v.Float64()
		return err == nil
	case object:
		for _, child := range v {
			if !finiteNumbers(child) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !finiteNumbers(child) {
				return false
			}
		}
	}
	return true
}
