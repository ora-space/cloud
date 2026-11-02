package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RoundOutcome is one immutable Iteration ledger entry, keyed by zero-based array index.
type RoundOutcome struct {
	Status string  `json:"status"`
	Item   any     `json:"item"`
	Output any     `json:"output,omitempty"`
	Error  *string `json:"error,omitempty"`
}

type iterationConfig struct {
	source, collect []string
	strategy        string
	max             uint64
}

func stringParts(raw any) []string {
	items, _ := raw.([]any)
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i], _ = item.(string)
	}
	return parts
}

func (n Node) iterationConfig() iterationConfig {
	raw, _ := n.data["iterationConfig"].(object)
	cfg := iterationConfig{source: stringParts(raw["iteratorSelector"]), collect: stringParts(raw["collectSelector"]), strategy: "fail", max: 50}
	if strategy, ok := raw["errorStrategy"].(string); ok {
		cfg.strategy = strategy
	}
	if raw["maxIterations"] != nil {
		cfg.max, _ = uintNumber(raw["maxIterations"], 1<<32-1)
	}
	return cfg
}

func (p RunPayload) iterationDecisions(round int) map[string]string {
	result := map[string]string{}
	suffix := "#" + strconv.Itoa(round)
	for key, branch := range p.IterationConditionDecisions {
		if strings.HasSuffix(key, suffix) {
			result[strings.TrimSuffix(key, suffix)] = branch
		}
	}
	return result
}

func exposedIteration(ledger map[int]RoundOutcome) (output, entries []any, failed json.Number) {
	output, entries = []any{}, []any{}
	keys := make([]int, 0, len(ledger))
	for k := range ledger {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	failures := 0
	for _, k := range keys {
		entry := ledger[k]
		var message any
		if entry.Error != nil {
			message = *entry.Error
		}
		entries = append(entries, map[string]any{"item": entry.Item, "status": entry.Status, "output": entry.Output, "error": message})
		if entry.Status == StatusSucceeded {
			output = append(output, entry.Output)
		} else {
			failures++
		}
	}
	return output, entries, json.Number(strconv.Itoa(failures))
}

func iterationStarts(g *Graph, owner string, round int, runs []NodeRun, decisions map[string]string, run RunContext) []NodeStart {
	wave := newRegionProjection(g, runs, decisions, owner, round).ready()
	starts := make([]NodeStart, len(wave))
	for i, n := range wave {
		starts[i] = startFor(n, run, "", &round)
	}
	return starts
}

func (e *Engine) advanceIteration(ctx context.Context, run RunContext, g *Graph, parent NodeRun, n Node, runs []NodeRun, p RunPayload) (bool, error) {
	cfg := n.iterationConfig()
	value, assigned, err := p.VariablePool.Resolve(cfg.source)
	if err != nil {
		return e.failContainer(ctx, run, parent, "", err.Error())
	}
	items, ok := value.([]any)
	if !assigned || !ok {
		return e.failContainer(ctx, run, parent, "", "iterator source did not resolve to an array: "+qualified(cfg.source))
	}
	if uint64(len(items)) > cfg.max {
		return e.failContainer(ctx, run, parent, "", fmt.Sprintf("iterator source has %d elements, exceeding maxIterations %d", len(items), cfg.max))
	}
	ledger := p.IterationLedger[n.ID]
	current := -1
	for _, row := range runs {
		member, found := g.Node(row.NodeID)
		if found && member.RegionOwner == n.ID && row.Iteration != nil && *row.Iteration > current {
			current = *row.Iteration
		}
	}
	var entry *RoundOutcome
	if current >= 0 {
		if current >= len(items) {
			return e.failContainer(ctx, run, parent, "", "iteration round exceeds its source")
		}
		if _, settled := ledger[current]; !settled {
			projection := newRegionProjection(g, runs, p.iterationDecisions(current), n.ID, current)
			if projection.inFlight() {
				return false, nil
			}
			starts := iterationStarts(g, n.ID, current, runs, p.iterationDecisions(current), run)
			if len(starts) != 0 {
				return e.startWave(ctx, run, g, starts, p.VariablePool, p.Locale, runs)
			}
			outcome := RoundOutcome{Status: StatusSucceeded, Item: items[current]}
			collected := false
			for _, row := range runs {
				member, found := g.Node(row.NodeID)
				if !found || member.RegionOwner != n.ID || row.Iteration == nil || *row.Iteration != current {
					continue
				}
				if row.Status == StatusFailed && outcome.Error == nil {
					message := "iteration child failed"
					if row.Error != nil {
						message = *row.Error
					}
					outcome.Error = &message
				}
				if row.NodeID == cfg.collect[0] && row.Status == StatusSucceeded {
					collected = true
				}
			}
			if outcome.Error == nil && !collected {
				message := "collect target did not run this round"
				outcome.Error = &message
			}
			if outcome.Error == nil {
				var set bool
				outcome.Output, set, err = p.VariablePool.Resolve(cfg.collect)
				if err != nil {
					return e.failContainer(ctx, run, parent, "", "collect target cannot be resolved: "+err.Error())
				}
				if !set {
					message := "collect target succeeded but produced no value this round"
					outcome.Error = &message
				}
			}
			if outcome.Error != nil {
				outcome.Status = StatusFailed
			}
			entry = &outcome
		}
	}
	// Copy the ledger so planning never mutates a previously read snapshot.
	nextLedger := map[int]RoundOutcome{}
	for round, old := range ledger {
		nextLedger[round] = old
	}
	if entry != nil {
		nextLedger[current] = *entry
	}
	a := ContainerAdvance{}
	if settled, exists := nextLedger[current]; exists && settled.Status == StatusFailed && cfg.strategy == "fail" {
		message := fmt.Sprintf("iteration round %d failed: %s", current, *settled.Error)
		a.Result = containerFailure(message)
		if entry == nil {
			// Settlement and parent failure commit together; a running owner with an already
			// failed ledger is invalid state, unlike a failure being settled for the first time.
			a.Result = e.invalidContainerFailure(message)
		}
	} else if current+1 < len(items) {
		next := current + 1
		a.Starts = iterationStarts(g, n.ID, next, runs, p.iterationDecisions(next), run)
	} else {
		output, _, _ := exposedIteration(nextLedger)
		raw, _ := json.Marshal(output)
		text := string(raw)
		a.Result = &ContainerResult{Status: StatusSucceeded, Output: &text}
	}
	a.Fold = func(raw []byte) ([]byte, error) {
		payload, _, err := DecodeRunPayload(raw)
		if err != nil {
			return nil, err
		}
		if payload.IterationLedger == nil {
			payload.IterationLedger = map[string]map[int]RoundOutcome{}
		}
		payload.IterationLedger[n.ID] = nextLedger
		if a.Result == nil {
			next := current + 1
			if err := payload.VariablePool.Set(n.ID+".item", n.ID, items[next]); err != nil {
				return nil, err
			}
			if err := payload.VariablePool.Set(n.ID+".index", n.ID, json.Number(strconv.Itoa(next))); err != nil {
				return nil, err
			}
		} else if a.Result.Status == StatusSucceeded {
			output, entries, failed := exposedIteration(nextLedger)
			for key, v := range map[string]any{"output": output, "entries": entries, "failed_count": failed} {
				if err := payload.VariablePool.Set(n.ID+"."+key, n.ID, v); err != nil {
					return nil, err
				}
			}
		}
		return payload.Encode()
	}
	return e.applyContainer(ctx, run, g, parent.ID, a)
}
