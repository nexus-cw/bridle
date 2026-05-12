package bridle

import "context"

// ProviderCategory classifies how a provider executes tool calls.
type ProviderCategory string

const (
	// CategoryDirectAPI — provider talks directly to a model API; bridle owns the tool loop.
	CategoryDirectAPI ProviderCategory = "direct-api"
	// CategorySubprocessStream — provider spawns a subprocess that runs its own agentic loop
	// and emits a structured event stream. The subprocess owns tool execution.
	CategorySubprocessStream ProviderCategory = "subprocess-stream"
)

// ProviderCapabilities advertises what a provider supports so the harness
// and funnel can route turns correctly.
type ProviderCapabilities struct {
	Category               ProviderCategory
	SupportsCustomTools    bool // funnel can pass arbitrary Tools via TurnRequest
	SupportsBeforeToolCall bool // BeforeToolCall hook fires
	SupportsAfterToolCall  bool // AfterToolCall hook fires
	SupportsMCP            bool // provider consumes TurnRequest.MCP (direct-api only)
}

// Provider is the interface every model backend must implement.
// Provider-specific weirdness (streaming, wire format, tool-schema translation)
// stays inside the implementation; the harness sees a uniform event stream.
type Provider interface {
	Name() ProviderID
	Capabilities() ProviderCapabilities
	RunTurn(ctx context.Context, req ProviderRequest, sink EventSink) (ProviderResult, error)
}

// ProviderRequest is the harness-internal lowered form of TurnRequest.
// System prompt is assembled, session tail is flattened to the provider's
// message format, tools are translated to provider-specific schema, and
// inbox items are folded in.
type ProviderRequest struct {
	AspectID     string
	AppendSystemPrompt string
	Session      SessionHandle  // for subprocess-stream: resume key; for direct-api: may be empty
	Messages     []ProviderMessage
	Tools        []ToolDef
	MCP          *MCPClientConfig  // nil = no MCP tools
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
