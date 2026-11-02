package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// aggregatorSelectors reads the Aggregator's ordered candidate selectors. Publication already
// required a non-empty list of unique root selectors.
func (n Node) aggregatorSelectors() [][]string {
	c, _ := n.data["aggregatorConfig"].(object)
	items, _ := c["variables"].([]any)
	result := [][]string{}
	for _, item := range items {
		raw, _ := item.([]any)
		parts := make([]string, 0, len(raw))
		for _, p := range raw {
			s, _ := p.(string)
			parts = append(parts, s)
		}
		result = append(result, parts)
	}
	return result
}

// completeAggregator is desktop's AggregatorRuntime (aggregator.rs, V1 contract): it passes the
// first assigned candidate through unchanged. Assigned means the key exists in the pool, never a
// truthiness check, so null, false, 0, "", [] and {} all hit. The node never reads branch
// decisions; branch selection reaches it only through which candidates were written. Its output
// is the value's JSON text, which the completion fold parses back into the typed variable.
func completeAggregator(n Node, c swiftCompletion) (string, error) {
	selectors := n.aggregatorSelectors()
	if len(selectors) == 0 {
		return "", fmt.Errorf("aggregator node must declare at least one selector")
	}
	names := make([]string, len(selectors))
	for i, parts := range selectors {
		if !selector(parts) {
			return "", fmt.Errorf("aggregator selector %q is not a node id and root variable", parts)
		}
		names[i] = qualified(parts)
		value, assigned, err := c.pool.Resolve(parts)
		if err != nil {
			return "", err // an undeclared candidate fails closed instead of being skipped
		}
		if assigned {
			var out bytes.Buffer
			encoder := json.NewEncoder(&out)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(value); err != nil {
				return "", err
			}
			return strings.TrimSuffix(out.String(), "\n"), nil
		}
	}
	return "", fmt.Errorf("aggregator found no assigned variable among: [%s]", strings.Join(names, ", "))
}
