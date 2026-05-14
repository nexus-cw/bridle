package claudecode

import (
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
)

// TestParseStream_PreToolTextDropped pins the regression for #245:
// when claude-code emits text → tool_use → text, only the post-last-tool
// text should end up in FinalText. Pre-tool text is exploratory thinking
// the model used to inform its tool calls; it isn't the model's settled
// answer, and concatenating it with the post-tool answer produced the
// "harrow #944 double" bug (two versions of the same answer pasted
// together in a single chat row).
//
// Operator call (chat #951, 2026-05-14): drop pre-tool text.
func TestParseStream_PreToolTextDropped(t *testing.T) {
	// Stream simulating harrow's turn: opener text → tool_use → answer text.
	stream := strings.NewReader(strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I'll research that."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_1","name":"WebSearch","input":{"q":"x"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"results"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The answer is X."}]}}`,
		`{"type":"result","result":"The answer is X.","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`,
	}, "\n"))

	sink := &fake.SliceEventSink{}
	result, err := parseStream(stream, sink)
	if err != nil {
		t.Fatalf("parseStream: unexpected error: %v", err)
	}

	want := "The answer is X."
	if result.FinalText != want {
		t.Errorf("FinalText: got %q; want %q (pre-tool text must be dropped)", result.FinalText, want)
	}
}

// TestParseStream_MultiToolPreToolReset pins the harrow #944 shape exactly:
// text → tool → text → tool → text. Only the final text block survives.
// Without the reset-on-tool-use, finalText accumulated all three (the
// pre-fix bug pasted "First version" + "Final version" in chat).
func TestParseStream_MultiToolPreToolReset(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Starting research."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_1","name":"WebSearch","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"r1"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First version of the answer."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_2","name":"WebSearch","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_2","content":"r2"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Final version of the answer."}]}}`,
		`{"type":"result","result":"Final version of the answer.","stop_reason":"end_turn","usage":{}}`,
	}, "\n"))

	sink := &fake.SliceEventSink{}
	result, err := parseStream(stream, sink)
	if err != nil {
		t.Fatalf("parseStream: unexpected error: %v", err)
	}

	want := "Final version of the answer."
	if result.FinalText != want {
		t.Errorf("FinalText: got %q; want %q (pre-tool text must be dropped on every tool_use)", result.FinalText, want)
	}

	// Tool invocations should still be recorded.
	if len(result.ToolCalls) != 2 {
		t.Errorf("ToolCalls: got %d; want 2", len(result.ToolCalls))
	}

	// All three text blocks should still emit ModelChunk events — the
	// streaming visibility (e.g. activity log, terminal echo) is unaffected
	// by the FinalText reset. Only the *answer-channel* (FinalText) drops
	// the pre-tool text.
	var chunkCount int
	for _, e := range sink.Events {
		if _, ok := e.(bridle.ModelChunk); ok {
			chunkCount++
		}
	}
	if chunkCount != 3 {
		t.Errorf("ModelChunk count: got %d; want 3 (all text blocks should emit visibility events)", chunkCount)
	}
}
