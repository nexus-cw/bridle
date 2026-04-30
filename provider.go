package bridle

import "context"

// Provider is the interface every model backend must implement.
// Provider-specific weirdness (streaming, wire format, tool-schema translation)
// stays inside the implementation; the harness sees a uniform event stream.
type Provider interface {
	Name() ProviderID
	RunTurn(ctx context.Context, req ProviderRequest, sink EventSink) (ProviderResult, error)
}

// ProviderRequest is the harness-internal lowered form of TurnRequest.
// System prompt is assembled, session tail is flattened to the provider's
// message format, tools are translated to provider-specific schema, and
// inbox items are folded in.
type ProviderRequest struct {
	AspectID     string
	SystemPrompt string
	Messages     []ProviderMessage
	Tools        []ToolDef
	MaxSteps     int
	Model        string
}

// ProviderMessage is a single exchange entry in provider-agnostic form.
type ProviderMessage struct {
	Role string // "user" | "assistant" | "tool_result" | "system"
	Content    string
	ToolCallID string // links a tool_result back to the call that produced it
}

// ProviderResult is the harness-internal result from one provider turn step.
type ProviderResult struct {
	FinalText    string
	ToolCalls    []ToolInvocation
	StepCount    int
	Usage        Usage
	StopReason   StopReason
	SessionDelta []SessionEvent
}
