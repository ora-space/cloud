package workflow

// probeRule stands in for unappliedAgentSettings and returns the inputs it was given, so a test
// can compare what the analysis and the run path pass the rule, whatever today's rule reads.
func probeRule(config object, at placement) []string {
	prompt, _ := config["prompt"].(string)
	return []string{"prompt=" + prompt, "region=" + at.region, "loop=" + at.loop}
}

// ProbeAnalysis runs the analysis's unapplied listing with probeRule, keyed by node ID.
func ProbeAnalysis(source string) (map[string][]string, error) {
	doc, err := project(source)
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	for _, entry := range unappliedSettings(doc, probeRule) {
		result[entry.NodeID] = entry.Settings
	}
	return result, nil
}

// ProbeRunPath applies probeRule to the very inputs Node.Agent passes unappliedAgentSettings.
func ProbeRunPath(n Node) []string {
	config, _ := n.data["agentConfig"].(object)
	return probeRule(config, n.at)
}
