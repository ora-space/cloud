package workflow

import (
	"sort"
	"strings"
)

// Graph is the executable root graph of a published document, mirroring desktop's
// WorkflowGraph after WorkflowGraph::parse. Spare nodes are excluded; Iteration region members
// stay in the root graph and report their owner; Loop bodies are separate scopes.
// A Graph is immutable and every query is deterministic.
type Graph struct {
	doc    *document
	root   *scope
	scopes map[string]*scope
	rank   map[string]int
}

// Node is one executable node with its authored configuration kept verbatim.
type Node struct {
	ID          string
	Kind        string
	Title       string
	Description string
	// RegionOwner is the Iteration that contains this node, or empty for outer nodes.
	RegionOwner string
	data        object
	bindings    [][]string
}

// Edge is one dependency. SourceHandle is empty when the authored edge has none, which a
// Condition treats as its else branch.
type Edge struct {
	Source, Target, SourceHandle string
}

// ParseGraph applies the publication rules and returns the executable root graph.
func ParseGraph(source string) (*Graph, error) {
	doc, err := project(source)
	if err != nil {
		return nil, err
	}
	scopes, err := buildScopes(doc)
	if err != nil {
		return nil, err
	}
	g := &Graph{doc: doc, root: scopes[""], scopes: scopes, rank: map[string]int{}}
	for i, n := range g.root.order {
		g.rank[n.id] = i
	}
	return g, nil
}

func (g *Graph) view(n *node) Node {
	title, _ := n.data["title"].(string)
	description, _ := n.data["description"].(string)
	owner := ""
	if !n.container {
		owner = n.owner
	}
	return Node{ID: n.id, Kind: n.kind, Title: title, Description: description, RegionOwner: owner, data: n.data, bindings: n.bindings}
}

// Nodes lists the root graph in document order, the order desktop schedules a ready wave in.
func (g *Graph) Nodes() []Node {
	result := make([]Node, 0, len(g.root.nodes))
	for _, n := range g.root.nodes {
		result = append(result, g.view(n))
	}
	return result
}

// Node returns one root-graph node.
func (g *Graph) Node(id string) (Node, bool) {
	n := g.root.byID[id]
	if n == nil {
		return Node{}, false
	}
	return g.view(n), true
}

// Start returns the root entry node, if the graph has one.
func (g *Graph) Start() (Node, bool) {
	for _, n := range g.root.nodes {
		if n.kind == "start" {
			return g.view(n), true
		}
	}
	return Node{}, false
}

func edgeOf(e edge) Edge {
	handle, _ := e.raw["sourceHandle"].(string)
	return Edge{Source: e.source, Target: e.target, SourceHandle: handle}
}

// Incoming lists the edges into one node in authored order.
func (g *Graph) Incoming(id string) []Edge {
	result := []Edge{}
	for _, e := range g.root.edges {
		if e.target == id {
			result = append(result, edgeOf(e))
		}
	}
	return result
}

// Outgoing lists the edges out of one node in authored order.
func (g *Graph) Outgoing(id string) []Edge {
	result := []Edge{}
	for _, e := range g.root.edges {
		if e.source == id {
			result = append(result, edgeOf(e))
		}
	}
	return result
}

// Predecessors lists direct predecessors in topological order.
func (g *Graph) Predecessors(id string) []Node {
	return g.ranked(g.Incoming(id), func(e Edge) string { return e.Source })
}

// Successors lists direct successors in topological order.
func (g *Graph) Successors(id string) []Node {
	return g.ranked(g.Outgoing(id), func(e Edge) string { return e.Target })
}

func (g *Graph) ranked(edges []Edge, end func(Edge) string) []Node {
	seen := map[string]bool{}
	result := []Node{}
	for _, e := range edges {
		id := end(e)
		if !seen[id] {
			seen[id] = true
			result = append(result, g.view(g.root.byID[id]))
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return g.rank[result[i].ID] < g.rank[result[j].ID] })
	return result
}

// TopologicalOrder lists the root graph upstream first.
func (g *Graph) TopologicalOrder() []Node {
	result := make([]Node, 0, len(g.root.order))
	for _, n := range g.root.order {
		result = append(result, g.view(n))
	}
	return result
}

// StartVariable is one typed Start input declaration.
type StartVariable struct {
	Name      string
	ValueType string
	Required  bool
	Options   []string
	MaxLength *int
	Value     any
}

// StartVariables reads a Start node's validated input declarations.
func (n Node) StartVariables() []StartVariable {
	items, _ := n.data["inputVariables"].([]any)
	result := []StartVariable{}
	for _, item := range items {
		v, _ := item.(object)
		name, _ := v["name"].(string)
		typ, _ := v["valueType"].(string)
		required, _ := v["required"].(bool)
		sv := StartVariable{Name: strings.TrimSpace(name), ValueType: typ, Required: required, Value: v["value"]}
		if options, ok := v["options"].([]any); ok {
			for _, o := range options {
				if s, ok := o.(string); ok {
					sv.Options = append(sv.Options, strings.TrimSpace(s))
				}
			}
		}
		if max, ok := uintNumber(v["maxLength"], 1<<31-1); ok && v["maxLength"] != nil {
			length := int(max)
			sv.MaxLength = &length
		}
		result = append(result, sv)
	}
	return result
}

// OutputBinding names one result an Output node resolves from the pool.
type OutputBinding struct {
	Name     string
	Selector []string
}

// OutputBindings lists an Output node's complete bindings; incomplete legacy rows are ignored
// at parse, as on desktop.
func (n Node) OutputBindings() []OutputBinding {
	items, _ := n.data["outputs"].([]any)
	result := []OutputBinding{}
	for _, item := range items {
		v, _ := item.(object)
		name, hasName := v["name"].(string)
		parts := []string{}
		raw, _ := v["variableSelector"].([]any)
		for _, p := range raw {
			s, _ := p.(string)
			parts = append(parts, s)
		}
		if hasName && selector(parts) {
			result = append(result, OutputBinding{Name: name, Selector: parts})
		}
	}
	return result
}

// AgentConfig is the part of agentConfig that v2 executes. Role, Skill and MCP bindings and
// retry/interactive settings are kept in the document but not applied by v2.
type AgentConfig struct {
	Prompt, AgentCLI, ModelID string
	// Unapplied names configured capabilities v2 runs without, so the run can say so.
	Unapplied []string
	// Structured is the output schema when the node requires a JSON object answer.
	Structured object
}

// Agent reads an Agent node's configuration.
func (n Node) Agent() AgentConfig {
	c, _ := n.data["agentConfig"].(object)
	prompt, _ := c["prompt"].(string)
	executor, _ := c["executor"].(object)
	cli, _ := executor["agentCli"].(string)
	model, _ := executor["modelId"].(string)
	cfg := AgentConfig{Prompt: prompt, AgentCLI: cli, ModelID: model}
	if role, _ := c["roleId"].(string); strings.TrimSpace(role) != "" {
		cfg.Unapplied = append(cfg.Unapplied, "role")
	}
	for _, key := range []string{"skills", "mcps"} {
		items, _ := c[key].([]any)
		for _, item := range items {
			if v, _ := item.(object); v["enabled"] != false {
				cfg.Unapplied = append(cfg.Unapplied, strings.TrimSuffix(key, "s"))
				break
			}
		}
	}
	if interactive, _ := c["interactive"].(bool); interactive {
		cfg.Unapplied = append(cfg.Unapplied, "interactive")
	}
	if retry, _ := c["retry"].(object); retry["enabled"] == true {
		cfg.Unapplied = append(cfg.Unapplied, "retry")
	}
	if contract, _ := c["outputContract"].(object); contract["type"] == "structured" {
		cfg.Structured, _ = contract["schema"].(object)
	}
	return cfg
}
