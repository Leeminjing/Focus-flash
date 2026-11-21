// Package tool dispatches model calls to explicitly registered tools.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
)

// Tool runs an operation with validated JSON object arguments.
// Implementations should honor cancellation through ctx.
type Tool func(ctx context.Context, args json.RawMessage) (string, error)

// Definition describes a registered tool to a model adapter.
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type Registration struct {
	Definition Definition
	// Validate can enforce tool-specific parameter constraints before Run.
	Validate func(args json.RawMessage) error
	Run      Tool
}

type Call struct {
	ID   string
	Name string
	Args json.RawMessage
}

type Result struct {
	CallID string
	Name   string
	Output string
	Error  string
}

func validArguments(args json.RawMessage) bool {
	trimmed := bytes.TrimSpace(args)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(args)
}
