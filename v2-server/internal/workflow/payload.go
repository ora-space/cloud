package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RunPayload is a run's private execution state, serialized with desktop's WorkflowRunPayload
// keys so persisted runs stay readable by the same projections. v2 freezes no Skill
// materialization (per-node Skills are not applied), so that receipt is always empty.
type RunPayload struct {
	Locale                      string            `json:"locale"`
	SkillMaterialization        skillReceipt      `json:"skillMaterialization"`
	StartNodeID                 *string           `json:"startNodeId"`
	VariablePool                VariablePool      `json:"variablePool"`
	ConditionDecisions          map[string]string `json:"conditionDecisions"`
	IterationLedger             map[string]any    `json:"iterationLedger,omitempty"`
	IterationConditionDecisions map[string]string `json:"iterationConditionDecisions,omitempty"`
	InjectLastFailure           bool              `json:"injectLastFailure"`
}

type skillReceipt struct {
	Bindings []any `json:"bindings"`
}

// Locales a run may freeze for prompt copy.
const (
	LocaleZhCN = "zh-CN"
	LocaleEnUS = "en-US"
)

// NewRunPayload derives a fresh payload for one run of the graph.
func NewRunPayload(g *Graph, locale string) RunPayload {
	if locale != LocaleZhCN {
		locale = LocaleEnUS
	}
	p := RunPayload{
		Locale:               locale,
		SkillMaterialization: skillReceipt{Bindings: []any{}},
		VariablePool:         NewPool(g),
		ConditionDecisions:   map[string]string{},
		InjectLastFailure:    true,
	}
	if start, ok := g.Start(); ok {
		p.StartNodeID = &start.ID
	}
	return p
}

// DecodeRunPayload reads a stored payload. An empty or "{}" payload means the run has not been
// prepared yet and reports false.
func DecodeRunPayload(raw []byte) (RunPayload, bool, error) {
	var probe map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &probe) != nil || len(probe) == 0 {
		return RunPayload{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	p := RunPayload{InjectLastFailure: true}
	if err := decoder.Decode(&p); err != nil {
		return RunPayload{}, false, fmt.Errorf("decode workflow run payload: %w", err)
	}
	if p.VariablePool.Catalog == nil {
		p.VariablePool.Catalog = map[string]VariableDefinition{}
	}
	if p.VariablePool.Values == nil {
		p.VariablePool.Values = map[string]any{}
	}
	if p.ConditionDecisions == nil {
		p.ConditionDecisions = map[string]string{}
	}
	if p.SkillMaterialization.Bindings == nil {
		p.SkillMaterialization.Bindings = []any{}
	}
	return p, true, nil
}

// Encode serializes the payload for storage.
func (p RunPayload) Encode() ([]byte, error) { return json.Marshal(p) }

// SeedSystem refreshes sys.workflow_id and sys.timestamp whenever a run begins executing, as
// desktop's start_run does.
func (p *RunPayload) SeedSystem(workflowID string, nowMillis int64) error {
	if _, err := p.VariablePool.write("sys.workflow_id", "sys", workflowID); err != nil {
		return err
	}
	_, err := p.VariablePool.write("sys.timestamp", "sys", json.Number(strconv.FormatInt(nowMillis, 10)))
	return err
}

// Complete folds one succeeded node's result into the payload, as desktop's
// update_run_execution_state: `{node}.output` for result-producing nodes, the Condition
// decision outside the pool, a structured object, or the Aggregator's typed value.
func (p *RunPayload) Complete(nodeID, kind string, iteration *int, output *string, structured any) error {
	if kind != "condition" && kind != "aggregator" && output != nil {
		if _, err := p.VariablePool.write(nodeID+".output", nodeID, *output); err != nil {
			return err
		}
	}
	switch kind {
	case "condition":
		if output != nil {
			if iteration != nil {
				if p.IterationConditionDecisions == nil {
					p.IterationConditionDecisions = map[string]string{}
				}
				p.IterationConditionDecisions[fmt.Sprintf("%s#%d", nodeID, *iteration)] = *output
			} else {
				p.ConditionDecisions[nodeID] = *output
			}
		}
	case "agent":
		if structured != nil {
			if _, err := p.VariablePool.write(nodeID+".structured_output", nodeID, structured); err != nil {
				return err
			}
		}
	case "aggregator":
		if output != nil {
			decoder := json.NewDecoder(strings.NewReader(*output))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("aggregator output is not JSON: %w", err)
			}
			if _, err := p.VariablePool.write(nodeID+".output", nodeID, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// InputRejection names the one Start variable a submission was refused for. Reason is
// undeclared, not_start_owned, type_mismatch or length_exceeded.
type InputRejection struct {
	Variable, Reason, Detail string
}

func (e *InputRejection) Error() string {
	switch e.Reason {
	case "undeclared":
		return "no declared Start variable has this name"
	case "not_start_owned":
		return "variable is not owned by the run's Start node"
	case "type_mismatch":
		return "value does not match the declared type " + e.Detail
	default:
		return "value exceeds the maximum length " + e.Detail
	}
}

// ApplyStartValues folds user-supplied Start values into the pool, as desktop's
// update_task_input_in_payload: alphabetical order, the first rejection aborts the whole
// submission, and null clears a value.
func (p *RunPayload) ApplyStartValues(values map[string]json.RawMessage) error {
	if len(values) == 0 {
		return nil
	}
	if p.StartNodeID == nil {
		return errors.New("run payload has no Start node owner")
	}
	writer := *p.StartNodeID
	next := p.VariablePool
	next.Values = map[string]any{}
	for k, v := range p.VariablePool.Values {
		next.Values[k] = v
	}
	for _, name := range sortedKeys(values) {
		key := writer + "." + name
		def, declared := next.Catalog[key]
		if !declared {
			return &InputRejection{Variable: name, Reason: "undeclared"}
		}
		if def.Writer != writer {
			return &InputRejection{Variable: name, Reason: "not_start_owned"}
		}
		decoder := json.NewDecoder(bytes.NewReader(values[name]))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return &InputRejection{Variable: name, Reason: "type_mismatch", Detail: def.ValueType}
		}
		if value == nil {
			delete(next.Values, key)
			continue
		}
		if err := next.Set(key, writer, value); err != nil {
			var poolErr *PoolError
			if errors.As(err, &poolErr) && poolErr.Kind == "length" {
				return &InputRejection{Variable: name, Reason: "length_exceeded", Detail: poolErr.Detail}
			}
			return &InputRejection{Variable: name, Reason: "type_mismatch", Detail: def.ValueType}
		}
	}
	next.Revision++
	p.VariablePool = next
	return nil
}

// MirrorInput copies the run's kickoff text into `{start}.input`, as desktop's
// mirror_run_input_into_pool.
func (p *RunPayload) MirrorInput(input *string) error {
	if p.StartNodeID == nil {
		return nil
	}
	key := *p.StartNodeID + ".input"
	if _, declared := p.VariablePool.Catalog[key]; !declared {
		return nil
	}
	if input == nil {
		if _, had := p.VariablePool.Values[key]; had {
			delete(p.VariablePool.Values, key)
			p.VariablePool.Revision++
		}
		return nil
	}
	return p.VariablePool.Set(key, *p.StartNodeID, *input)
}

// StartError is a start-time refusal with desktop's public detail strings.
type StartError struct {
	Code, NodeID, Variable, Detail string
}

func (e *StartError) Error() string {
	if e.Variable != "" {
		return fmt.Sprintf("workflow start %s (%s): %s", e.Code, e.Variable, e.Detail)
	}
	return fmt.Sprintf("workflow start %s (node %s): %s", e.Code, e.NodeID, e.Detail)
}

// ValidateStartInputs enforces required values and select options at the execution boundary.
func (p *RunPayload) ValidateStartInputs(start Node) error {
	for _, v := range start.StartVariables() {
		value, assigned := p.VariablePool.Values[start.ID+"."+v.Name]
		if !assigned {
			value = v.Value
		}
		missing := value == nil
		switch x := value.(type) {
		case string:
			missing = x == ""
		case []any:
			missing = len(x) == 0
		}
		if v.Required && missing {
			return &StartError{Code: "workflow_run_input_invalid", Variable: v.Name, Detail: "required value is missing"}
		}
		if text, ok := value.(string); ok && len(v.Options) != 0 && !slices.Contains(v.Options, text) {
			return &StartError{Code: "workflow_run_input_invalid", Variable: v.Name, Detail: "value is not one of the configured options"}
		}
	}
	return nil
}
