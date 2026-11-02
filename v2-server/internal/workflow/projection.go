package workflow

// Node run statuses shared with the store.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ElseBranch is the Condition handle an unlabeled edge defaults to.
const ElseBranch = "else"

type projected int

const (
	notReached projected = iota
	inactive
	ready
	running
	awaitingInput
	succeeded
	failed
	cancelled
)

// projection derives every outer node's scheduling state from the committed rows and
// Condition decisions, as desktop's BranchProjection::new. Branch activation is never stored,
// so a restart recomputes it. Iteration region members are invisible to the outer projection.
type projection struct {
	g         *Graph
	decisions map[string]string
	states    map[string]projected
	region    string
}

func newProjection(g *Graph, runs []NodeRun, decisions map[string]string) *projection {
	return projectRound(g, runs, decisions, "", nil)
}

func newRegionProjection(g *Graph, runs []NodeRun, decisions map[string]string, owner string, round int) *projection {
	return projectRound(g, runs, decisions, owner, &round)
}

func projectRound(g *Graph, runs []NodeRun, decisions map[string]string, region string, round *int) *projection {
	p := &projection{g: g, decisions: decisions, states: map[string]projected{}, region: region}
	if region != "" {
		p.states[region] = succeeded
	}
	for _, r := range runs {
		n := g.root.byID[r.NodeID]
		if n == nil || !p.outer(n) {
			continue
		}
		if (round == nil && r.Iteration != nil) || (round != nil && (r.Iteration == nil || *r.Iteration != *round)) {
			continue
		}
		switch r.Status {
		case StatusSucceeded:
			p.states[r.NodeID] = succeeded
		case StatusFailed:
			p.states[r.NodeID] = failed
		case StatusCancelled:
			p.states[r.NodeID] = cancelled
		case StatusRunning:
			p.states[r.NodeID] = running
		case StatusPending:
			p.states[r.NodeID] = awaitingInput
		}
	}
	for changed := true; changed; {
		changed = false
		for _, n := range g.root.nodes {
			if !p.outer(n) {
				continue
			}
			if _, known := p.states[n.id]; known {
				continue
			}
			if s := p.compute(n.id); s == ready || s == inactive {
				p.states[n.id] = s
				changed = true
			}
		}
	}
	return p
}

func (p *projection) outer(n *node) bool {
	if p.region != "" {
		return !n.container && n.owner == p.region
	}
	return n.container || n.owner == ""
}

func (p *projection) compute(id string) projected {
	edges := p.g.Incoming(id)
	if len(edges) == 0 {
		if start, ok := p.g.Start(); ok && start.ID == id {
			return ready
		}
		return inactive
	}
	anyActive := false
	for _, e := range edges {
		switch p.edge(e) {
		case notReached:
			return notReached
		case failed:
			return failed
		case ready:
			anyActive = true
		}
	}
	if anyActive {
		return ready
	}
	return inactive
}

// edge resolves one edge as ready (active), inactive, failed or notReached (pending).
func (p *projection) edge(e Edge) projected {
	switch p.states[e.Source] {
	case succeeded:
		if p.g.root.byID[e.Source].kind != "condition" {
			return ready
		}
		selected, decided := p.decisions[e.Source]
		handle := e.SourceHandle
		if handle == "" {
			handle = ElseBranch
		}
		if decided && selected == handle {
			return ready
		}
		return inactive
	case inactive, cancelled:
		return inactive
	case failed:
		return failed
	}
	return notReached
}

func (p *projection) ready() []Node {
	result := []Node{}
	for _, n := range p.g.root.nodes {
		if p.outer(n) && p.states[n.id] == ready {
			result = append(result, p.g.view(n))
		}
	}
	return result
}

func (p *projection) inFlight() bool {
	for _, s := range p.states {
		if s == running || s == awaitingInput {
			return true
		}
	}
	return false
}
