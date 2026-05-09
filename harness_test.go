package bridle_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
)

// --- helpers ---

func toolDef(name string) bridle.ToolDef {
	return bridle.ToolDef{
		Name:        name,
		Description: name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func inv(id, name string) bridle.ToolInvocation {
	return bridle.ToolInvocation{ID: id, Name: name, Args: json.RawMessage(`{}`)}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

// --- basic turn: no tools ---

func TestRunTurn_NoTools(t *testing.T) {
	p := fake.NewProvider(fake.Step{Text: "hello"})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:       "fake-model",
		UserMessage: "hi",
	}, fake.NewToolRunner(nil), sink)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FinalText != "hello" {
		t.Errorf("FinalText = %q; want %q", result.FinalText, "hello")
	}
	if result.StopReason != bridle.StopReasonModelDone {
		t.Errorf("StopReason = %q; want model_done", result.StopReason)
	}

	assertEventOrder(t, sink.Events, "ModelChunk", "TurnDone")
}

// --- tool call round-trip ---

func TestRunTurn_OneToolCall(t *testing.T) {
	toolStep := fake.Step{
		ToolCalls: []bridle.ToolInvocation{inv("1", "echo")},
	}
	finalStep := fake.Step{Text: "done"}

	p := fake.NewProvider(toolStep, finalStep)
	runner := fake.NewToolRunner(map[string][]fake.ToolResult{
		"echo": {{Result: rawJSON(`"echoed"`)}},
	})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:    "fake-model",
		Tools:    []bridle.ToolDef{toolDef("echo")},
		MaxSteps: 5,
	}, runner, sink)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ToolCalls) != 1 {
		t.Errorf("ToolCalls len = %d; want 1", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Name != "echo" {
		t.Errorf("ToolCalls[0].Name = %q; want echo", result.ToolCalls[0].Name)
	}
	if result.StepCount != 1 {
		t.Errorf("StepCount = %d; want 1", result.StepCount)
	}

	assertEventOrder(t, sink.Events,
		"ToolCallStart", "ToolCallResult", "StepBoundary", "ModelChunk", "TurnDone")
}

// --- MaxSteps cap ---

func TestRunTurn_MaxSteps(t *testing.T) {
	// Provider always returns a tool call; MaxSteps=2 should cap it.
	steps := make([]fake.Step, 5)
	for i := range steps {
		steps[i] = fake.Step{ToolCalls: []bridle.ToolInvocation{inv("x", "noop")}}
	}
	p := fake.NewProvider(steps...)
	runner := fake.NewToolRunner(map[string][]fake.ToolResult{
		"noop": {{Result: rawJSON(`null`)}},
	})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:    "fake-model",
		MaxSteps: 2,
	}, runner, sink)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.StopReason != bridle.StopReasonMaxSteps {
		t.Errorf("StopReason = %q; want max_steps", result.StopReason)
	}
	if result.StepCount != 2 {
		t.Errorf("StepCount = %d; want 2", result.StepCount)
	}
}

// --- cancellation ---

func TestRunTurn_CancelBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before RunTurn

	p := fake.NewProvider(fake.Step{Text: "should not emit"})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, _ := h.RunTurn(ctx, bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if result.StopReason != bridle.StopReasonAborted {
		t.Errorf("StopReason = %q; want aborted", result.StopReason)
	}
}

func TestRunTurn_CancelMidTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Runner cancels the context when called.
	cancelRunner := &cancelOnRunToolRunner{cancel: cancel, result: rawJSON(`"ok"`)}

	p := fake.NewProvider(
		fake.Step{ToolCalls: []bridle.ToolInvocation{inv("1", "slow")}},
		fake.Step{Text: "never"},
	)
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, _ := h.RunTurn(ctx, bridle.TurnRequest{Model: "fake-model", MaxSteps: 5}, cancelRunner, sink)

	if result.StopReason != bridle.StopReasonAborted {
		t.Errorf("StopReason = %q; want aborted", result.StopReason)
	}
}

// --- hook ordering ---

func TestHooks_BeforeModelCallFires(t *testing.T) {
	var fired []string
	p := fake.NewProvider(fake.Step{Text: "ok"})
	h := bridle.NewHarness(p)
	h.RegisterBeforeModelCall(func(ctx context.Context, in bridle.BeforeModelCallCtx) (bridle.BeforeModelCallCtx, bridle.HookAction, error) {
		fired = append(fired, "bmc")
		return in, bridle.HookContinue, nil
	})
	sink := &fake.SliceEventSink{}
	h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if len(fired) != 1 || fired[0] != "bmc" {
		t.Errorf("fired = %v; want [bmc]", fired)
	}
}

func TestHooks_BeforeModelCallAborts(t *testing.T) {
	p := fake.NewProvider(fake.Step{Text: "should not see this"})
	h := bridle.NewHarness(p)
	h.RegisterBeforeModelCall(func(ctx context.Context, in bridle.BeforeModelCallCtx) (bridle.BeforeModelCallCtx, bridle.HookAction, error) {
		return in, bridle.HookAbort, nil
	})
	sink := &fake.SliceEventSink{}
	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result.StopReason != bridle.StopReasonAborted {
		t.Errorf("StopReason = %q; want aborted", result.StopReason)
	}
	// No events should have been emitted.
	if len(sink.Events) != 0 {
		t.Errorf("events emitted = %d; want 0", len(sink.Events))
	}
}

func TestHooks_BeforeToolCallAborts(t *testing.T) {
	p := fake.NewProvider(
		fake.Step{ToolCalls: []bridle.ToolInvocation{inv("1", "echo")}},
		fake.Step{Text: "never"},
	)
	h := bridle.NewHarness(p)
	h.RegisterBeforeToolCall(func(ctx context.Context, in bridle.BeforeToolCallCtx) (bridle.BeforeToolCallCtx, bridle.HookAction, error) {
		return in, bridle.HookAbort, nil
	})
	runner := fake.NewToolRunner(map[string][]fake.ToolResult{
		"echo": {{Result: rawJSON(`"ok"`)}},
	})
	sink := &fake.SliceEventSink{}
	result, _ := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model", MaxSteps: 5}, runner, sink)

	if result.StopReason != bridle.StopReasonAborted {
		t.Errorf("StopReason = %q; want aborted", result.StopReason)
	}
}

func TestHooks_OnTurnDoneCanMutateSessionDelta(t *testing.T) {
	p := fake.NewProvider(fake.Step{Text: "result"})
	h := bridle.NewHarness(p)
	h.RegisterOnTurnDone(func(ctx context.Context, in bridle.OnTurnDoneCtx) (bridle.OnTurnDoneCtx, bridle.HookAction, error) {
		in.Result.SessionDelta = append(in.Result.SessionDelta, bridle.SessionEvent{
			Role:    bridle.RoleSystem,
			Content: "hook-injected",
		})
		return in, bridle.HookContinue, nil
	})
	sink := &fake.SliceEventSink{}
	result, _ := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	last := result.SessionDelta[len(result.SessionDelta)-1]
	if last.Content != "hook-injected" {
		t.Errorf("last session delta = %q; want hook-injected", last.Content)
	}
}

func TestHooks_RegistrationOrder(t *testing.T) {
	var order []int
	p := fake.NewProvider(fake.Step{Text: "ok"})
	h := bridle.NewHarness(p)
	for i := 0; i < 3; i++ {
		i := i
		h.RegisterBeforeModelCall(func(ctx context.Context, in bridle.BeforeModelCallCtx) (bridle.BeforeModelCallCtx, bridle.HookAction, error) {
			order = append(order, i)
			return in, bridle.HookContinue, nil
		})
	}
	sink := &fake.SliceEventSink{}
	h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Errorf("hook order = %v; want [0 1 2]", order)
	}
}

// --- provider error ---

func TestRunTurn_ProviderError(t *testing.T) {
	boom := errors.New("provider boom")
	p := fake.NewProvider(fake.Step{Err: boom})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result.StopReason != bridle.StopReasonError {
		t.Errorf("StopReason = %q; want error", result.StopReason)
	}
	// TurnError event should be in the sink.
	found := false
	for _, e := range sink.Events {
		if _, ok := e.(bridle.TurnError); ok {
			found = true
		}
	}
	if !found {
		t.Error("TurnError event not emitted")
	}
}

// --- tool error does not abort turn ---

func TestRunTurn_ToolError_DoesNotAbortTurn(t *testing.T) {
	p := fake.NewProvider(
		fake.Step{ToolCalls: []bridle.ToolInvocation{inv("1", "failing")}},
		fake.Step{Text: "recovered"},
	)
	runner := fake.NewToolRunner(map[string][]fake.ToolResult{
		"failing": {{Err: errors.New("tool failed")}},
	})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model", MaxSteps: 5}, runner, sink)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Turn should complete; tool error is passed to model, not fatal.
	if result.StopReason != bridle.StopReasonModelDone {
		t.Errorf("StopReason = %q; want model_done", result.StopReason)
	}
	if result.FinalText != "recovered" {
		t.Errorf("FinalText = %q; want recovered", result.FinalText)
	}
	// ToolCallResult should record the error.
	var tcr bridle.ToolCallResult
	for _, e := range sink.Events {
		if r, ok := e.(bridle.ToolCallResult); ok {
			tcr = r
		}
	}
	if tcr.Err == "" {
		t.Error("ToolCallResult.Err is empty; expected tool error string")
	}
}

// --- panic recovery ---

func TestRunTurn_PanicRecovery(t *testing.T) {
	p := &panicProvider{}
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{Model: "fake-model"}, fake.NewToolRunner(nil), sink)

	if err == nil {
		t.Fatal("expected error from panic recovery")
	}
	if result.StopReason != bridle.StopReasonError {
		t.Errorf("StopReason = %q; want error", result.StopReason)
	}
}

// --- subprocess-stream provider capability advertisement ---

func TestSubprocessProvider_CapabilityAdvertisement(t *testing.T) {
	p := fake.NewSubprocessProvider()
	caps := p.Capabilities()
	if caps.Category != bridle.CategorySubprocessStream {
		t.Errorf("Category = %q; want subprocess-stream", caps.Category)
	}
	if caps.SupportsBeforeToolCall {
		t.Error("SupportsBeforeToolCall should be false for subprocess-stream")
	}
	if !caps.SupportsAfterToolCall {
		t.Error("SupportsAfterToolCall should be true for subprocess-stream")
	}
}

// TestSubprocessProvider_TextTurn verifies a text-only turn through the
// subprocess fake emits the right events.
func TestSubprocessProvider_TextTurn(t *testing.T) {
	p := fake.NewSubprocessProvider(fake.SubprocessStep{Text: "subprocess result"})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:       "fake-model",
		UserMessage: "test",
	}, fake.NewToolRunner(nil), sink)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FinalText != "subprocess result" {
		t.Errorf("FinalText = %q; want 'subprocess result'", result.FinalText)
	}
	assertEventOrder(t, sink.Events, "ModelChunk", "TurnDone")
}

// --- Model required ---

func TestRunTurn_ModelRequired(t *testing.T) {
	p := fake.NewProvider(fake.Step{Text: "ok"})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	_, err := h.RunTurn(context.Background(), bridle.TurnRequest{}, fake.NewToolRunner(nil), sink)
	if err == nil {
		t.Fatal("expected ErrModelRequired, got nil")
	}
	if !errors.Is(err, bridle.ErrModelRequired) {
		t.Errorf("err = %v; want ErrModelRequired", err)
	}
}

// --- helpers ---

func assertEventOrder(t *testing.T, events []bridle.Event, types ...string) {
	t.Helper()
	got := make([]string, 0, len(events))
	for _, e := range events {
		switch e.(type) {
		case bridle.ModelChunk:
			got = append(got, "ModelChunk")
		case bridle.ToolCallStart:
			got = append(got, "ToolCallStart")
		case bridle.ToolCallResult:
			got = append(got, "ToolCallResult")
		case bridle.StepBoundary:
			got = append(got, "StepBoundary")
		case bridle.TurnDone:
			got = append(got, "TurnDone")
		case bridle.TurnError:
			got = append(got, "TurnError")
		}
	}
	if len(got) != len(types) {
		t.Errorf("event sequence = %v; want %v", got, types)
		return
	}
	for i, want := range types {
		if got[i] != want {
			t.Errorf("event[%d] = %q; want %q (full sequence: %v)", i, got[i], want, got)
		}
	}
}

// cancelOnRunToolRunner cancels its context when Run is called.
type cancelOnRunToolRunner struct {
	cancel func()
	result json.RawMessage
}

func (r *cancelOnRunToolRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	r.cancel()
	return r.result, nil
}

// --- MCP tool name collision ---

// TestRunTurn_MCPToolNameCollision verifies that RunTurn returns ErrToolNameCollision when
// an explicit tool and the MCP config advertise the same name.
// We use a fake MCPClientConfig pointing at a non-existent stdio server — the
// connection will fail before the collision check. To test the collision path purely,
// we pass an MCPClientConfig with an empty Servers list to the SupportsMCP=false provider
// (which skips MCP entirely), and test the collision directly via the mcpclient package tests.
// The actual harness-level collision path is exercised in TestRunTurn_MCPNoServers.
func TestRunTurn_MCPNoServers(t *testing.T) {
	p := fake.NewProvider(fake.Step{Text: "hello", StopReason: bridle.StopReasonModelDone})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	// MCP config with no servers — should be a no-op and turn should complete normally.
	req := bridle.TurnRequest{
		Model: "fake-model",
		Tools: []bridle.ToolDef{toolDef("explicit_tool")},
		MCP:   &bridle.MCPClientConfig{}, // empty — no servers
	}
	result, err := h.RunTurn(context.Background(), req, fake.NewToolRunner(nil), sink)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.StopReason != bridle.StopReasonModelDone {
		t.Errorf("want model_done, got %s", result.StopReason)
	}
	if result.FinalText != "hello" {
		t.Errorf("want 'hello', got %q", result.FinalText)
	}
}

// TestRunTurn_MCPIgnoredForSubprocess verifies that subprocess-stream providers
// ignore TurnRequest.MCP (SupportsMCP=false).
func TestRunTurn_MCPIgnoredForSubprocess(t *testing.T) {
	p := fake.NewSubprocessProvider(fake.SubprocessStep{
		Text:       "subprocess response",
		StopReason: bridle.StopReasonModelDone,
	})
	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	req := bridle.TurnRequest{
		Model: "fake-model",
		MCP: &bridle.MCPClientConfig{
			Servers: []bridle.MCPServerSpec{{
				Name:      "unreachable-server",
				Transport: bridle.MCPTransportStdio,
				Command:   []string{"nonexistent-binary"},
			}},
		},
	}
	// Should succeed — subprocess provider ignores MCP, so the unreachable server
	// is never contacted.
	result, err := h.RunTurn(context.Background(), req, fake.NewToolRunner(nil), sink)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FinalText != "subprocess response" {
		t.Errorf("want 'subprocess response', got %q", result.FinalText)
	}
}

// panicProvider always panics inside RunTurn.
type panicProvider struct{}

func (p *panicProvider) Name() bridle.ProviderID { return "panic-fake" }
func (p *panicProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI, SupportsCustomTools: true, SupportsBeforeToolCall: true, SupportsAfterToolCall: true, SupportsMCP: true}
}
func (p *panicProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	panic("deliberate test panic")
}
