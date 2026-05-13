package bridle

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrModelRequired is returned by RunTurn when TurnRequest.Model is empty.
var ErrModelRequired = errors.New("bridle: TurnRequest.Model is required")

// ProviderID identifies a model provider.
type ProviderID string

const (
	ProviderClaude    ProviderID = "claude-api"
	ProviderOllama    ProviderID = "ollama-local"
	ProviderOpenAI    ProviderID = "openai-api"
	ProviderBedrock   ProviderID = "bedrock"
	ProviderGemini    ProviderID = "gemini-api"
	ProviderGeminiCLI ProviderID = "gemini-cli"
)

// StopReason explains why a turn ended.
type StopReason string

const (
	StopReasonModelDone StopReason = "model_done"
	StopReasonMaxSteps  StopReason = "max_steps"
	StopReasonError     StopReason = "error"
	StopReasonAborted   StopReason = "aborted"
	// StopReasonProcessExit is set when the underlying provider process
	// exited non-zero AFTER producing parseable assistant content. The
	// returned ProviderResult carries whatever the model said before
	// the exit — callers should treat the result as truncated-but-real,
	// not discard it. Common cause: hitting an output-token cap and the
	// CLI surfacing that as a non-zero exit rather than a clean stop.
	StopReasonProcessExit StopReason = "process_exit"
)

// Usage holds token and cost data for a turn.
//
// InputTokens is the count of UNCACHED prompt tokens billed at full
// rate. CacheReadInputTokens and CacheCreationInputTokens surface
// claude-api's prompt-caching behavior — the former is read at a
// discount, the latter is the new content being added to cache. Cache
// fields are zero for providers that don't expose caching (or don't
// run a cache-eligible request).
//
// Sum (InputTokens + CacheReadInputTokens + CacheCreationInputTokens)
// approximates the total prompt size the model received. Use that
// for context-fullness reasoning; use InputTokens alone for billing
// estimates of fresh content.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int     // Anthropic prompt-cache hit count
	CacheCreationInputTokens int     // tokens written into the prompt cache this turn
	CostUSD                  float64 // provider-reported or estimated; 0 if unknown
}

// ToolInvocation records a single tool call the model made.
type ToolInvocation struct {
	ID     string
	Name   string
	Args   json.RawMessage
	Result json.RawMessage
	Err    string
}

// InboxItem is a comms message that arrived during the previous turn.
// The harness folds these into the prompt context before the first model call.
// Read-only from the harness's perspective.
//
// MsgID is the chat msg_id this item was sourced from. It carries
// through into the prompt so the model can reference items by id when
// triaging ("triage(msg_id=17, decision='reply')"). Zero means the
// item didn't originate from a chat message — it's an internal/synthetic
// item the funnel injected, and the triage contract doesn't apply.
type InboxItem struct {
	From    string
	Content string
	MsgID   int64
	RawJSON json.RawMessage

	// ThreadRoot is the canonical thread identity for the message
	// (linked-list root id; nexus task #226). The funnel uses it to
	// key per-thread session state so each thread gets its own
	// claude-code jsonl, preventing SessionTail bleed across threads.
	// Zero = legacy/non-chat synthetic item or pre-#226 row.
	ThreadRoot int64
}

// TurnRequest is the complete input for one deliberation turn.
type TurnRequest struct {
	// Identity & framing
	AspectID     string         // who's running (cost/triage/identity attribution)
	AppendSystemPrompt string         // composed by funnel: NEXUS.md + SOUL.md + PRIMER + harness rules
	Session      SessionHandle  // opaque handle for provider-side state (subprocess-stream: resume key)
	SessionTail  []SessionEvent // recent events for direct-api providers to lower into the request

	// This turn
	UserMessage string      // the prompt that opens this turn (may be empty for autonomous)
	Inbox       []InboxItem // mid-turn comms accumulated since last turn

	// Tool surface
	Tools []ToolDef         // explicit in-process tool defs
	MCP   *MCPClientConfig  // MCP-loaded tools; nil = no MCP tools this turn

	// Provider
	Provider ProviderID // claude-api | ollama-local | openai-api | claude-code
	Model    string     // REQUIRED — provider-specific model id; RunTurn returns ErrModelRequired if empty
	MaxSteps int        // hard cap on tool-call rounds; 0 = unlimited

	// ToolChoice optionally constrains how the model picks tools.
	// Empty string → provider default (typically "auto").
	// "auto" → model decides whether to call a tool.
	// "any" → model must call exactly one tool, free choice of which.
	// "none" → no tools may be called this turn (text only).
	// Any other value → name of a specific tool that must be called.
	// Not all providers honour all values; unsupported values fall back to "auto".
	ToolChoice string

	// Cwd is the working directory for subprocess-style providers
	// (currently claude-code). Empty falls through to the bridle host
	// process's cwd. Per-request rather than per-Harness because
	// different aspects sharing one Harness need distinct cwds —
	// claude-code derives its session jsonl path AND its .mcp.json
	// discovery from cwd, so two aspects with the same Harness but
	// overlapping cwds collide sessions and leak MCP identity from one
	// into the other. Direct-API providers (claude-api, ollama, openai)
	// ignore this field — they have no subprocess to anchor.
	Cwd string
}

// TurnResult is the structured outcome of a completed turn.
type TurnResult struct {
	FinalText    string           // model's last assistant text (may be empty for tool-only turns)
	ToolCalls    []ToolInvocation // ordered list of what the model actually did
	StepCount    int
	Usage        Usage
	StopReason   StopReason
	SessionDelta []SessionEvent // events to propose to the funnel-owned JSONL
}

// EventSink receives events as the turn unfolds.
type EventSink interface {
	Emit(Event)
}

// Harness drives one deliberation turn with one provider.
type Harness struct {
	provider Provider
	hooks    hookRegistry
}

// NewHarness creates a Harness backed by the given provider.
func NewHarness(p Provider) *Harness {
	return &Harness{provider: p}
}

// RunTurn drives one turn: calls the provider, executes tool calls via runner,
// fires hooks at documented points, and emits events to sink.
// Cancellation via ctx returns a partial TurnResult with StopReason=aborted.
// Returns ErrModelRequired if req.Model is empty.
func (h *Harness) RunTurn(ctx context.Context, req TurnRequest, runner ToolRunner, sink EventSink) (result TurnResult, err error) {
	if req.Model == "" {
		return TurnResult{StopReason: StopReasonError}, ErrModelRequired
	}
	defer func() {
		if r := recover(); r != nil {
			e := panicErr(r)
			sink.Emit(TurnError{Err: e, Stage: "harness-recover"})
			result.StopReason = StopReasonError
			err = e
		}
	}()
	return h.runTurn(ctx, req, runner, sink)
}
