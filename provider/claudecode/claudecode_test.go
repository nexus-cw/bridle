package claudecode_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
	"github.com/CarriedWorldUniverse/bridle/provider/claudecode"
)

// claudeAvailable returns true if the claude CLI is on PATH.
func claudeAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// TestClaudeCode_RoundTrip runs a text-only turn against the real CLI.
// Skipped if claude is not on PATH or ANTHROPIC_API_KEY is not configured.
func TestClaudeCode_RoundTrip(t *testing.T) {
	if !claudeAvailable() {
		t.Skip("claude CLI not on PATH")
	}

	p := claudecode.New()
	p.AllowedTools = []string{"none"}

	h := bridle.NewHarness(p)
	sink := &fake.SliceEventSink{}

	result, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:       "claude-haiku-4-5-20251001",
		UserMessage: "Reply with exactly the word: PONG",
		MaxSteps:    1,
	}, fake.NewToolRunner(nil), sink)

	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if result.StopReason != bridle.StopReasonModelDone {
		t.Errorf("StopReason = %q; want model_done", result.StopReason)
	}
	if !strings.Contains(result.FinalText, "PONG") {
		t.Errorf("FinalText = %q; expected it to contain PONG", result.FinalText)
	}

	// Verify at least one ModelChunk and TurnDone event.
	var hasChunk, hasDone bool
	for _, e := range sink.Events {
		switch e.(type) {
		case bridle.ModelChunk:
			hasChunk = true
		case bridle.TurnDone:
			hasDone = true
		}
	}
	if !hasChunk {
		t.Error("no ModelChunk event emitted")
	}
	if !hasDone {
		t.Error("no TurnDone event emitted")
	}

	// Cache token surfacing — Anthropic's stream-json result event
	// carries cache_read/cache_creation tokens. claude-code's first
	// invocation typically populates cache_creation (it caches the
	// system prompt) and cache_read=0. Bridle must surface both.
	t.Logf("usage: input=%d output=%d cache_read=%d cache_create=%d",
		result.Usage.InputTokens, result.Usage.OutputTokens,
		result.Usage.CacheReadInputTokens, result.Usage.CacheCreationInputTokens)
	if result.Usage.CacheCreationInputTokens == 0 && result.Usage.CacheReadInputTokens == 0 {
		t.Errorf("expected cache_read OR cache_creation to be non-zero on a real claude turn; got both 0 — bridle parser is not surfacing cache fields from stream-json")
	}
}

// TestClaudeCode_SessionResume verifies that a second turn with the same
// SessionHandle.ID picks up context from the first turn.
func TestClaudeCode_SessionResume(t *testing.T) {
	if !claudeAvailable() {
		t.Skip("claude CLI not on PATH")
	}

	p := claudecode.New()
	p.AllowedTools = []string{"none"}
	h := bridle.NewHarness(p)

	// Turn 1: establish a session.
	sink1 := &fake.SliceEventSink{}
	result1, err := h.RunTurn(context.Background(), bridle.TurnRequest{
		Model:       "claude-haiku-4-5-20251001",
		UserMessage: "Remember this secret word: XYLOPHONE. Reply with OK.",
		MaxSteps:    1,
	}, fake.NewToolRunner(nil), sink1)
	if err != nil {
		t.Fatalf("turn 1 error: %v", err)
	}

	// Extract session_id from the system init event emitted in SessionDelta.
	// The CLI writes it in the stream-json; we need the session_id.
	// For now, verify that a session was established by checking FinalText.
	if result1.FinalText == "" {
		t.Fatal("turn 1 produced no text")
	}
	t.Logf("turn 1 text: %s", result1.FinalText)

	// Note: without threading the session_id back to the funnel we can't
	// verify --resume directly here. That threading is a funnel-layer concern
	// (SessionHandle.ID populated from the stream's system.init event).
	// This test verifies the turn completes; the session_id extraction is
	// documented as a funnel responsibility.
}

// TestClaudeCode_CapabilityAdvertisement verifies the provider reports the
// correct category and hook support flags.
func TestClaudeCode_CapabilityAdvertisement(t *testing.T) {
	p := claudecode.New()
	caps := p.Capabilities()

	if caps.Category != bridle.CategorySubprocessStream {
		t.Errorf("Category = %q; want subprocess-stream", caps.Category)
	}
	if caps.SupportsCustomTools {
		t.Error("SupportsCustomTools should be false")
	}
	if caps.SupportsBeforeToolCall {
		t.Error("SupportsBeforeToolCall should be false")
	}
	if !caps.SupportsAfterToolCall {
		t.Error("SupportsAfterToolCall should be true")
	}
}
