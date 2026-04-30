package bridle

import "encoding/json"

// Event is the union type for all observable harness events.
type Event interface {
	event() // unexported marker; keeps the interface closed to this package
}

// ModelChunk carries a streamed text fragment from the model.
type ModelChunk struct {
	Text string
}

// ToolCallStart fires when the model requests a tool call, before execution.
type ToolCallStart struct {
	ID   string
	Name string
	Args json.RawMessage
}

// ToolCallResult fires after the tool runner returns (or errors).
type ToolCallResult struct {
	ID     string
	Result json.RawMessage
	Err    string // non-empty if the tool runner returned an error
}

// StepBoundary fires between tool-call rounds.
// Step 1 = the first round; fires after its results are sent back to the model.
type StepBoundary struct {
	Step int
}

// TurnDone fires after the turn completes successfully.
type TurnDone struct {
	Result TurnResult
}

// TurnError fires when the provider or harness hits a non-recoverable error.
// Never panics across the harness boundary.
type TurnError struct {
	Err   error
	Stage string // "provider", "tool", "harness-recover", etc.
}

func (ModelChunk) event()    {}
func (ToolCallStart) event() {}
func (ToolCallResult) event() {}
func (StepBoundary) event()  {}
func (TurnDone) event()      {}
func (TurnError) event()     {}
