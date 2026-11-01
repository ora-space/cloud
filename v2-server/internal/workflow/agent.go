package workflow

import "strings"

func validateAgent(c reader, n *node) {
	c.str("roleId")
	if c.value["executor"] != nil {
		e := c.child("executor")
		e.str("agentCli")
		e.str("modelId")
	}
	if c.value["interactive"] != nil {
		n.interactive = c.boolean("interactive")
	}
	n.templates = append(n.templates, c.str("prompt"))
	for _, skill := range c.elements("skills") {
		skill.str("skillId")
		if skill.value["enabled"] != nil {
			skill.boolean("enabled")
		}
	}
	seen := map[string]bool{}
	for _, mcp := range c.elements("mcps") {
		id := mcp.requiredString("mcpId")
		if !pluginID(id) || seen[id] {
			mcp.fail("mcpId", "MCP bindings require unique canonical plugin IDs")
		}
		seen[id] = true
		if _, exists := mcp.value["enabled"]; !exists {
			mcp.fail("enabled", "required boolean is missing")
		}
		mcp.boolean("enabled")
	}
	if c.value["promptInactivity"] != nil {
		policy := c.str("promptInactivity")
		if policy != "timeout" && policy != "wait" {
			c.fail("promptInactivity", "expected timeout or wait")
		}
	}
	if c.value["retry"] != nil {
		retry := c.child("retry")
		if _, exists := retry.value["enabled"]; !exists {
			retry.fail("enabled", "required boolean is missing")
		}
		retry.boolean("enabled")
		retry.integer("maxRetries", 0, 5)
		retry.integer("initialDelaySeconds", 0, 300)
	}
	if c.value["outputContract"] != nil {
		contract := c.child("outputContract")
		kind := contract.str("type")
		contract.str("textExposure")
		if kind == "structured" {
			n.structured = true
			schema := contract.child("schema")
			validateSchema(schema)
			if schema.value["type"] != "object" {
				schema.fail("type", "root structured output type must be object")
			}
		}
		// Unknown legacy output contract kinds are ignored by desktop's wire mapper.
	}
}

func pluginID(id string) bool {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || len(parts[0]) > 33 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '.') {
				return false
			}
		}
	}
	return true
}

func validateSchema(r reader) {
	typ := r.requiredString("type")
	if typ != "null" && !supportedType(typ) {
		r.fail("type", "unsupported structured output type")
	}
	if value, exists := r.value["properties"]; exists {
		if typ != "object" {
			r.fail("properties", "properties are only valid for object fields")
		}
		properties := r.at(value, r.path+".properties")
		for _, key := range sortedKeys(properties.value) {
			validateSchema(properties.at(properties.value[key], properties.path+"."+key))
		}
	}
	if _, exists := r.value["required"]; exists {
		if typ != "object" {
			r.fail("required", "required is only valid for object fields")
		}
		properties, _ := r.value["properties"].(object)
		for _, key := range r.strings("required") {
			if _, exists := properties[key]; !exists {
				r.fail("required", "required property is not declared")
			}
		}
	}
	if _, exists := r.value["additionalProperties"]; exists {
		if typ != "object" {
			r.fail("additionalProperties", "only valid for object fields")
		}
		r.boolean("additionalProperties")
	}
	if _, exists := r.value["items"]; exists {
		if typ != "array" {
			r.fail("items", "items is only valid for array fields")
		}
		validateSchema(r.child("items"))
	}
}

func loopName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && !strings.Contains(name, ".")
}
