package claudecode

import (
	"os"
	"strings"
	"testing"
)

// TestAppendSystemPromptArgs_InlineUnderThreshold verifies that a body
// that fits within the spill threshold is passed inline via
// --append-system-prompt, with no tempfile created.
func TestAppendSystemPromptArgs_InlineUnderThreshold(t *testing.T) {
	body := strings.Repeat("a", systemPromptSpillThresholdBytes)
	args, file, err := appendSystemPromptArgs(body)
	if err != nil {
		t.Fatalf("appendSystemPromptArgs error: %v", err)
	}
	if file != "" {
		t.Errorf("expected no tempfile for body of size %d (threshold %d); got %q",
			len(body), systemPromptSpillThresholdBytes, file)
		_ = os.Remove(file)
	}
	if len(args) != 2 || args[0] != "--append-system-prompt" || args[1] != body {
		t.Errorf("expected [--append-system-prompt <body>]; got %v", args)
	}
}

// TestAppendSystemPromptArgs_SpillOverThreshold verifies that a body
// past the threshold is written to a tempfile, that the tempfile is
// readable, and that --append-system-prompt-file points at it. The
// caller is responsible for cleanup; the test does it.
func TestAppendSystemPromptArgs_SpillOverThreshold(t *testing.T) {
	body := strings.Repeat("b", systemPromptSpillThresholdBytes+1)
	args, file, err := appendSystemPromptArgs(body)
	if err != nil {
		t.Fatalf("appendSystemPromptArgs error: %v", err)
	}
	if file == "" {
		t.Fatalf("expected tempfile for body of size %d (threshold %d); got empty",
			len(body), systemPromptSpillThresholdBytes)
	}
	defer os.Remove(file)

	if len(args) != 2 || args[0] != "--append-system-prompt-file" || args[1] != file {
		t.Errorf("expected [--append-system-prompt-file <file>]; got %v", args)
	}

	// Verify the tempfile actually contains the body — a half-written
	// file would silently truncate the system prompt.
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read tempfile %s: %v", file, err)
	}
	if string(got) != body {
		t.Errorf("tempfile contents differ from body (len got=%d want=%d)", len(got), len(body))
	}
}

// TestAppendSystemPromptArgs_VeryLargeBody mimics a Frame-class system
// prompt past the 32K argv ceiling that triggered task #674. The spill
// path must handle it without truncation.
func TestAppendSystemPromptArgs_VeryLargeBody(t *testing.T) {
	body := strings.Repeat("nexus-frame-composition-payload\n", 2000) // ~64KB
	args, file, err := appendSystemPromptArgs(body)
	if err != nil {
		t.Fatalf("appendSystemPromptArgs error: %v", err)
	}
	if file == "" {
		t.Fatalf("expected tempfile for body of size %d", len(body))
	}
	defer os.Remove(file)

	if args[0] != "--append-system-prompt-file" {
		t.Errorf("expected --append-system-prompt-file flag; got %q", args[0])
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("tempfile size %d != body size %d (truncation?)", len(got), len(body))
	}
}
