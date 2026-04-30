package fake

import (
	"context"
	"encoding/json"
	"fmt"

	bridle "github.com/nexus-cw/bridle"
)

// ToolResult is a scripted response for a named tool.
type ToolResult struct {
	// Result is the JSON payload to return on success.
	Result json.RawMessage
	// Err causes the runner to return this error instead.
	Err error
}

// ToolRunner is a scripted fake that returns canned results for named tools.
// For any tool not in the map it returns an error.
type ToolRunner struct {
	responses map[string][]ToolResult
}

// NewToolRunner returns a fake runner with the given per-tool response queues.
// Responses are consumed in order; the last entry repeats if the queue is exhausted.
func NewToolRunner(responses map[string][]ToolResult) *ToolRunner {
	return &ToolRunner{responses: responses}
}

// Run returns the next scripted result for the named tool.
func (r *ToolRunner) Run(_ context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	queue, ok := r.responses[call.Name]
	if !ok || len(queue) == 0 {
		return nil, fmt.Errorf("fake: no response scripted for tool %q", call.Name)
	}

	// Pop the first entry; if there's only one left, leave it in place (repeat semantics).
	entry := queue[0]
	if len(queue) > 1 {
		r.responses[call.Name] = queue[1:]
	}

	if entry.Err != nil {
		return nil, entry.Err
	}
	return entry.Result, nil
}

// SliceEventSink is a test EventSink that collects emitted events into a slice.
type SliceEventSink struct {
	Events []bridle.Event
}

func (s *SliceEventSink) Emit(e bridle.Event) {
	s.Events = append(s.Events, e)
}
