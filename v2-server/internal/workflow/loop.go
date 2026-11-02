package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type loopVariable struct {
	Name      string `json:"name"`
	ValueType string `json:"valueType"`
	Initial   struct {
		Kind     string   `json:"kind"`
		Value    any      `json:"value"`
		Selector []string `json:"selector"`
	} `json:"initial"`
	Feedback []string `json:"feedback"`
}

type loopConfig struct {
	MaxIterations         int            `json:"maxIterations"`
	MaxIterationsBehavior string         `json:"maxIterationsBehavior"`
	Variables             []loopVariable `json:"variables"`
	Outputs               []struct {
		Name     string   `json:"name"`
		Selector []string `json:"variableSelector"`
	} `json:"outputs"`
	until []conditionCase
}

type loopState struct {
	VariablePool       VariablePool      `json:"variablePool"`
	ConditionDecisions map[string]string `json:"conditionDecisions"`
	Exit               *loopExit         `json:"exit,omitempty"`
}

type loopExit struct {
	NodeRunID   string `json:"nodeRunId"`
	RequestedAt int64  `json:"requestedAt"`
	// Keep desktop's externally tagged Result representation, including an empty Ok object.
	Result map[string]any `json:"result"`
}

func decodeLoopState(raw []byte) (loopState, error) {
	var state loopState
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&state); err != nil {
		return state, fmt.Errorf("invalid Loop round state: %w", err)
	}
	if state.VariablePool.Catalog == nil || state.VariablePool.Values == nil {
		return state, fmt.Errorf("Loop round has no variable pool")
	}
	if state.ConditionDecisions == nil {
		state.ConditionDecisions = map[string]string{}
	}
	return state, nil
}

func (n Node) loopConfig() (loopConfig, error) {
	raw, _ := json.Marshal(n.data["loopConfig"])
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var cfg loopConfig
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	v, _ := n.data["loopConfig"].(object)
	until, _ := v["until"].(object)
	copy := object{"id": "until", "logic": until["logic"], "conditions": until["conditions"]}
	var err error
	cfg.until, err = (Node{data: object{"cases": []any{copy}}}).conditionCases()
	return cfg, err
}

func requiredValue(pool *VariablePool, parts []string) (any, error) {
	v, assigned, err := pool.Resolve(parts)
	if err != nil {
		return nil, err
	}
	if !assigned {
		return nil, fmt.Errorf("Loop selector has no value in this round: %s", strings.Join(parts, "."))
	}
	return v, nil
}

func (c loopConfig) initial(pool *VariablePool) (map[string]any, error) {
	carried := map[string]any{}
	for _, v := range c.Variables {
		value := v.Initial.Value
		if v.Initial.Kind == "variable" {
			var err error
			value, err = requiredValue(pool, v.Initial.Selector)
			if err != nil {
				return nil, err
			}
		}
		normalized, ok := normalizeValue(value, v.ValueType)
		if !ok {
			return nil, fmt.Errorf("Loop variable %s does not match declared type %s", v.Name, v.ValueType)
		}
		carried[v.Name] = normalized
	}
	return carried, nil
}

func (g *Graph) loopPool(n Node, c loopConfig, outer VariablePool, carried map[string]any) (VariablePool, error) {
	body, _ := g.LoopBody(n.ID)
	pool := NewPool(body)
	ancestors := g.root.ancestors(n.ID)
	globals, _ := validateGlobals(g.doc.raw, true)
	for key, def := range outer.Catalog {
		if !ancestors[def.Writer] && globals[key] == "" {
			continue
		}
		pool.Catalog[key] = def
		if v, ok := outer.Values[key]; ok {
			pool.Values[key] = v
		}
	}
	for _, v := range c.Variables {
		pool.Declare(n.ID+"."+v.Name, v.ValueType, n.ID)
		if err := pool.Set(n.ID+"."+v.Name, n.ID, carried[v.Name]); err != nil {
			return pool, err
		}
	}
	return pool, nil
}

func (c loopConfig) outputs(pool *VariablePool) (map[string]any, error) {
	result := map[string]any{}
	for _, binding := range c.Outputs {
		v, err := requiredValue(pool, binding.Selector)
		if err != nil {
			return nil, err
		}
		result[binding.Name] = v
	}
	return result, nil
}

// complete reads all feedback from one immutable snapshot before evaluating until. Exit
// deliberately bypasses this function: a break never requires unused next-round feedback.
func (c loopConfig) complete(round int, pool *VariablePool) (carried, outputs map[string]any, reason string, err error) {
	carried = map[string]any{}
	for _, v := range c.Variables {
		value, readErr := requiredValue(pool, v.Feedback)
		if readErr != nil {
			return nil, nil, "", readErr
		}
		normalized, ok := normalizeValue(value, v.ValueType)
		if !ok {
			return nil, nil, "", fmt.Errorf("Loop variable %s does not match declared type %s", v.Name, v.ValueType)
		}
		carried[v.Name] = normalized
	}
	selected, err := evaluateCondition(c.until, pool)
	if err != nil {
		return nil, nil, "", err
	}
	if selected != ElseBranch {
		reason = "loop_succeeded"
	} else if round == c.MaxIterations && c.MaxIterationsBehavior == "succeed" {
		reason = "loop_max_iterations"
	}
	if reason != "" {
		outputs, err = c.outputs(pool)
		return nil, outputs, reason, err
	}
	if round >= c.MaxIterations {
		observed := []string{}
		for _, clause := range c.until {
			for _, rule := range clause.rules {
				value, set, readErr := pool.Resolve(rule.selector)
				shown := "unset"
				if readErr != nil {
					shown = "unavailable"
				} else if set {
					shown = strings.Join(strings.Fields(renderValue(value)), " ")
				}
				runes := []rune(shown)
				if len(runes) > 160 {
					shown = string(runes[:160]) + "…"
				}
				observed = append(observed, strings.Join(rule.selector, ".")+"="+shown)
			}
		}
		if len(observed) == 0 {
			observed = []string{"no until comparison"}
		}
		return nil, nil, "", fmt.Errorf("Loop did not terminate within %d rounds; last observed %s", c.MaxIterations, strings.Join(observed, ", "))
	}
	return carried, nil, "", nil
}
