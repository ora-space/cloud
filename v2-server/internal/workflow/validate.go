package workflow

import "strings"

type scope struct {
	id           string
	nodes        []*node
	byID         map[string]*node
	edges        []edge
	order        []*node
	predecessors map[string]map[string]bool
}

func validateDocument(d *document) error {
	_, err := buildScopes(d)
	return err
}

// buildScopes validates the reachable projection and returns each scope's checked topology:
// "" is the root graph, other keys are Loop bodies. Iteration region members stay in the root.
func buildScopes(d *document) (map[string]*scope, error) {
	scopes := map[string]*scope{"": {byID: map[string]*node{}}}
	loops := []*node{}
	for _, n := range d.nodes {
		if !d.active[n.id] {
			continue
		}
		if !knownKind(n.kind) {
			return nil, invalid("unknown_node_type", n.id, "data.kind", "unrecognized node type")
		}
		if err := validateNode(n); err != nil {
			return nil, err
		}
		owner := ""
		if n.container {
			owner = n.owner
		}
		if scopes[owner] == nil {
			scopes[owner] = &scope{id: owner, byID: map[string]*node{}}
		}
		scopes[owner].nodes = append(scopes[owner].nodes, n)
		scopes[owner].byID[n.id] = n
		if n.kind == "loop" {
			if _, hasParent := n.raw["parentId"]; hasParent {
				return nil, invalid("invalid_region", n.id, "parentId", "nested Loops are not supported")
			}
			loops = append(loops, n)
		}
		if n.kind == "loopExit" && !n.container {
			return nil, invalid("invalid_region", n.id, "data.containerId", "loopExit must belong to a Loop")
		}
	}
	globals, err := validateGlobals(d.raw, len(loops) != 0)
	if err != nil {
		return nil, err
	}
	for _, e := range d.edges {
		if !d.active[e.source] || !d.active[e.target] {
			continue
		}
		from := d.byID[e.source]
		r := newReader(e.raw, "invalid_node", from.id, "edge")
		r.str("sourceHandle")
		if *r.err != nil {
			return nil, *r.err
		}
		if from.kind == "loopExit" {
			return nil, invalid("invalid_region", from.id, "edges", "loopExit must have no outgoing edges")
		}
		owner := ""
		if from.container {
			owner = from.owner
		}
		scopes[owner].edges = append(scopes[owner].edges, e)
	}
	for _, id := range sortedKeys(scopes) {
		s := scopes[id]
		if err := s.topology(); err != nil {
			return nil, err
		}
		if len(loops) != 0 {
			if err := s.requireEntry(); err != nil {
				return nil, err
			}
		}
		if err := s.validateRegions(); err != nil {
			return nil, err
		}
		if id != "" {
			for _, n := range s.nodes {
				if n.kind == "output" {
					return nil, invalid("invalid_region", n.id, "data.kind", "Loop bodies export through loopConfig.outputs")
				}
			}
		}
		if err := s.validateDeclarations(globals); err != nil {
			return nil, err
		}
	}
	if len(loops) != 0 {
		if err := validateLoopBindings(scopes, loops, globals); err != nil {
			return nil, err
		}
	}
	for _, n := range d.nodes {
		if !d.active[n.id] {
			continue
		}
		for _, parts := range n.selectors {
			if d.unused[parts[0]] {
				return nil, invalid("unused_reference", n.id, "selector", "variable references unused node "+parts[0])
			}
		}
		for _, text := range n.templates {
			for {
				_, after, found := strings.Cut(text, "{{#")
				if !found {
					break
				}
				selected, rest, closed := strings.Cut(after, "#}}")
				if !closed {
					break
				} // Malformed templates are diagnosed at execution, as on desktop.
				producer, _, qualified := strings.Cut(selected, ".")
				if qualified && d.unused[producer] {
					return nil, invalid("unused_reference", n.id, "template", "template references unused node "+producer)
				}
				text = rest
			}
		}
	}
	return scopes, nil
}

// topology is iterative: author-sized graphs must not consume one Go stack frame
// per node. Ancestor sets are computed only for the policy seams that need them.
func (s *scope) topology() error {
	indegree := map[string]int{}
	outgoing := map[string][]string{}
	starts := 0
	for _, n := range s.nodes {
		indegree[n.id] = 0
		if n.kind == "start" {
			starts++
		}
	}
	if starts > 1 {
		return invalid("multiple_start_nodes", "", "nodes", "more than one Start in a scope")
	}
	for _, e := range s.edges {
		indegree[e.target]++
		outgoing[e.source] = append(outgoing[e.source], e.target)
	}
	queue := []*node{}
	for _, n := range s.nodes {
		if indegree[n.id] == 0 {
			queue = append(queue, n)
		}
	}
	for i := 0; i < len(queue); i++ {
		n := queue[i]
		s.order = append(s.order, n)
		for _, id := range outgoing[n.id] {
			indegree[id]--
			if indegree[id] == 0 {
				queue = append(queue, s.byID[id])
			}
		}
	}
	if len(s.order) != len(s.nodes) {
		return invalid("cycle_detected", "", "edges", "workflow graph contains a cycle")
	}
	return nil
}

func (s *scope) ancestors(id string) map[string]bool {
	if s.predecessors == nil {
		s.predecessors = map[string]map[string]bool{}
	}
	if cached := s.predecessors[id]; cached != nil {
		return cached
	}
	parents := map[string][]string{}
	for _, e := range s.edges {
		parents[e.target] = append(parents[e.target], e.source)
	}
	seen := map[string]bool{}
	queue := append([]string{}, parents[id]...)
	for len(queue) > 0 {
		parent := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[parent] {
			continue
		}
		seen[parent] = true
		queue = append(queue, parents[parent]...)
	}
	s.predecessors[id] = seen
	return seen
}

func (s *scope) requireEntry() error {
	entry := ""
	for _, n := range s.nodes {
		if n.kind == "start" {
			entry = n.id
		}
	}
	if entry == "" {
		return invalid("invalid_region", s.id, "nodes", "every container scope requires one Start")
	}
	for _, n := range s.nodes {
		if n.id != entry && !s.ancestors(n.id)[entry] {
			return invalid("invalid_region", n.id, "edges", "unreachable scope node")
		}
	}
	return nil
}

func (s *scope) validateRegions() error {
	for _, n := range s.nodes {
		if n.owner == "" || n.container {
			continue
		}
		owner := s.byID[n.owner]
		if owner == nil || owner.kind != "iteration" {
			return invalid("invalid_region", n.id, "parentId", "parent must be an iteration")
		}
		if n.kind == "output" || n.kind == "iteration" || n.interactive {
			return invalid("invalid_region", n.id, "parentId", "iteration cannot contain Output, nested composites or interactive nodes")
		}
		if !s.ancestors(n.id)[n.owner] {
			return invalid("invalid_region", n.id, "edges", "region member is not reachable from its owner")
		}
	}
	return nil
}

func validateLoopBindings(scopes map[string]*scope, loops []*node, globals map[string]string) error {
	root := scopes[""]
	visible := func(parts []string, allowed map[string]bool) bool {
		return allowed[parts[0]] || globals[qualified(parts)] != ""
	}
	check := func(n *node, refs [][]string, allowed map[string]bool) error {
		for _, parts := range refs {
			if !visible(parts, allowed) {
				return invalid("invalid_region", n.id, "selector", "selector is outside the visible Loop scope")
			}
		}
		return nil
	}
	for _, n := range root.nodes {
		if err := check(n, n.bindings, root.ancestors(n.id)); err != nil {
			return err
		}
	}
	for _, loop := range loops {
		body := scopes[loop.id]
		if body == nil {
			return invalid("invalid_region", loop.id, "nodes", "Loop requires a body")
		}
		ancestors := root.ancestors(loop.id)
		if err := check(loop, loop.initial, ancestors); err != nil {
			return err
		}
		allowed := map[string]bool{loop.id: true}
		for id := range ancestors {
			allowed[id] = true
		}
		for id := range body.byID {
			allowed[id] = true
		}
		if err := check(loop, loop.loopRefs, allowed); err != nil {
			return err
		}
		for _, n := range body.nodes {
			allowed := map[string]bool{loop.id: true}
			for id := range ancestors {
				allowed[id] = true
			}
			for id := range body.ancestors(n.id) {
				allowed[id] = true
			}
			if err := check(n, n.bindings, allowed); err != nil {
				return err
			}
		}
	}
	return nil
}
