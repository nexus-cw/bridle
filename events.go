package bridle

import (
	"encoding/json"
	"errors"
)

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

// ProviderErrorKind classifies a provider-level error so callers can
// surface a distinct diagnosis string instead of an opaque exit code.
type ProviderErrorKind string

const (
	ProviderErrorAuthFailed   ProviderErrorKind = "auth_failed"
	ProviderErrorRateLimit    ProviderErrorKind = "rate_limit"
	ProviderErrorServerError  ProviderErrorKind = "server_error"
	ProviderErrorNetworkError ProviderErrorKind = "network_error"
	ProviderErrorTimeout      ProviderErrorKind = "timeout"
	ProviderErrorTLSError     ProviderErrorKind = "tls_error"
)

// ProviderError is a classified provider-level error.
type ProviderError struct {
	Kind    ProviderErrorKind
	Message string
	Err     error // underlying error (may be nil)
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *ProviderError) Unwrap() error { return e.Err }

// IsProviderErrorKind reports whether err (or any error in its chain) is
// a ProviderError with the given kind.
func IsProviderErrorKind(err error, kind ProviderErrorKind) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind == kind
	}
	return false
}

func (ModelChunk) event()     {}
func (ToolCallStart) event()  {}
func (ToolCallResult) event() {}
func (StepBoundary) event()   {}
func (TurnDone) event()       {}
func (TurnError) event()      {}
