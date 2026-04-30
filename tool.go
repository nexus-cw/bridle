package bridle

import (
	"context"
	"encoding/json"
)

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the expected arguments.
	InputSchema json.RawMessage
}

// ToolCall is a single invocation the model requested.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// ToolRunner executes tool calls on behalf of the harness.
// The funnel supplies the implementation; the harness never owns tools.
type ToolRunner interface {
	Run(ctx context.Context, call ToolCall) (json.RawMessage, error)
}
