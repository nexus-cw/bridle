package claudecode

import (
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
)

// TestParseStream_PartialContentNoResultEvent pins the regression for #219:
// when claude-code emits assistant content but the subprocess dies before
// sending the terminal "result" event, parseStream must surface the
// accumulated FinalText. parseStream itself returns parseErr ==
// "stream ended without result event"-style? actually that check is done
// in the caller; parseStream returns nil err with empty StopReason. The
// caller (runTurnOnce) is responsible for preserving the partial result
// on non-zero subprocess exit. This test verifies the parseStream half:
// content arrives in result.FinalText even when there's no terminal event.
//
// The runTurnOnce branch that turns this into a posted message is exercised
// by the real-CLI integration tests; here we just pin that the accumulator
// does not require the result event to retain content.
func TestParseStream_PartialContentNoResultEvent(t *testing.T) {
	// Stream of two assistant text blocks, no `result` event, scanner hits
	// EOF cleanly (simulating a subprocess that streamed content then exited
	// non-zero without finishing its protocol).
	stream := strings.NewReader(strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First substantive paragraph of the extract."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" Second paragraph carrying more analysis."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" Final clarifying question at the tail."}]}}`,
	}, "\n"))

	sink := &fake.SliceEventSink{}
	result, err := parseStream(stream, sink)
	if err != nil {
		t.Fatalf("parseStream: unexpected error: %v", err)
	}

	want := "First substantive paragraph of the extract." +
		" Second paragraph carrying more analysis." +
		" Final clarifying question at the tail."
	if result.FinalText != want {
		t.Errorf("FinalText mismatch:\n got=%q\nwant=%q", result.FinalText, want)
	}

	if result.StopReason != "" {
		// parseStream leaves StopReason empty when no result event arrived;
		// the caller (runTurnOnce) assigns StopReasonProcessExit in that
		// case. If this changes, runTurnOnce's branch needs revisiting too.
		t.Errorf("StopReason: got %q; parseStream should leave it empty when no result event arrives", result.StopReason)
	}

	// Verify all three assistant blocks emitted ModelChunk events. Pre-fix
	// the funnel was discarding everything but the tail; the events
	// themselves were always emitted correctly — the loss happened later
	// in runTurnOnce. This guards against a regression where parseStream
	// itself starts dropping non-final blocks.
	var chunkCount int
	for _, e := range sink.Events {
		if _, ok := e.(bridle.ModelChunk); ok {
			chunkCount++
		}
	}
	if chunkCount != 3 {
		t.Errorf("ModelChunk count: got %d; want 3", chunkCount)
	}
}

// TestStopReasonProcessExit_Defined ensures the new constant is wired
// (compile-time check; trivially fails if someone removes the constant).
func TestStopReasonProcessExit_Defined(t *testing.T) {
	if bridle.StopReasonProcessExit == "" {
		t.Fatal("bridle.StopReasonProcessExit must be defined and non-empty for #219 to function")
	}
	if bridle.StopReasonProcessExit == bridle.StopReasonAborted {
		t.Fatal("StopReasonProcessExit must be distinct from StopReasonAborted — they encode different conditions")
	}
}
