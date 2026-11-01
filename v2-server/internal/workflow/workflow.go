// Package workflow owns v2 workflow document analysis and publication validation.
// It has no persistence, transport, catalog, or execution dependencies.
package workflow

import (
	"fmt"
)

// Analysis separates known spare nodes from unrecognized authoring node kinds.
// Both slices are non-nil, including for an empty document.
type Analysis struct {
	UnusedNodeIDs     []string           `json:"unusedNodeIds"`
	UnrecognizedNodes []UnrecognizedNode `json:"unrecognizedNodes"`
}

// UnrecognizedNode describes a kind the desktop graph vocabulary does not register.
type UnrecognizedNode struct {
	NodeID string `json:"nodeId"`
	Kind   string `json:"kind"`
}

// ValidationError identifies a document defect without exposing host or database details.
// Code is a domain category; the HTTP adapter owns its public error-code mapping.
type ValidationError struct {
	Code   string
	NodeID string
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	where := e.Field
	if e.NodeID != "" {
		where = "node " + e.NodeID + ": " + where
	}
	return fmt.Sprintf("workflow %s (%s): %s", e.Code, where, e.Reason)
}

func invalid(code, nodeID, field, reason string) error {
	return &ValidationError{Code: code, NodeID: nodeID, Field: field, Reason: reason}
}

// AnalyzeWorkflow validates authoring identities and containment, then reports
// entry-unreachable nodes. It does not require complete executable configuration.
// The supplied graph is never rewritten or persisted.
func AnalyzeWorkflow(graph string) (Analysis, error) {
	doc, err := project(graph)
	if err != nil {
		return Analysis{}, err
	}
	return doc.analysis, nil
}

// ValidateWorkflow applies desktop's publish-time graph parser to the reachable
// projection, including node configuration, variable declarations and container
// boundaries. It does not assert start-time readiness or installed capabilities.
// Drafts should be saved without invoking this function.
func ValidateWorkflow(graph string) error {
	doc, err := project(graph)
	if err != nil {
		return err
	}
	return validateDocument(doc)
}
