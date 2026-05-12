// Package stubfunnel is a temporary validation harness for bridle v0.1/patch1.
//
// It is NOT the funnel. It exists solely to exercise bridle's contract under a
// realistic funnel-shaped caller — deliberation loop, inbox folding, send_comms
// as a tool, session JSONL round-trip, log-decision turn — against real model
// providers before the real funnel is built.
//
// This will be replaced or deleted when the real funnel (gated on §6.5 /
// #79/#80/#81) lands. Do not build on top of it.
//
// Usage:
//
//	go run ./stubfunnel/ -provider claude -prompt "what time is it?"
//	go run ./stubfunnel/ -provider claudeapi -prompt "what time is it?"
//	go run ./stubfunnel/ -provider ollama  -model llama3.2:3b -prompt "what time is it?"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeapi "github.com/CarriedWorldUniverse/bridle/provider/claude"
	"github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	"github.com/CarriedWorldUniverse/bridle/provider/ollama"
)

func main() {
	providerFlag := flag.String("provider", "claude", "provider: claude | claudeapi | ollama")
	modelFlag := flag.String("model", "", "model id (default per-provider)")
	promptFlag := flag.String("prompt", "What time is it? Use the now tool.", "user prompt for the deliberation turn")
	maxStepsFlag := flag.Int("max-steps", 5, "max tool-call rounds per turn")
	maxTurnsFlag := flag.Int("max-turns", 3, "max deliberation turns before giving up")
	sessionFileFlag := flag.String("session", "", "path to session JSONL file (default: temp file)")
	flag.Parse()

	ctx := context.Background()

	// Resolve provider.
	var provider bridle.Provider
	model := *modelFlag
	switch *providerFlag {
	case "claude":
		p := claudecode.New()
		p.AllowedTools = []string{"none"}
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		provider = p
	case "claudeapi":
		p := claudeapi.New("")
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		provider = p
	case "ollama":
		provider = ollama.New()
		if model == "" {
			model = "llama3.2:3b"
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown provider %q (want: claude | claudeapi | ollama)\n", *providerFlag)
		os.Exit(1)
	}

	caps := provider.Capabilities()
	fmt.Printf("[stubfunnel] provider=%s model=%s category=%s custom-tools=%v before-tool-call=%v\n",
		*providerFlag, model, caps.Category, caps.SupportsCustomTools, caps.SupportsBeforeToolCall)

	// Session JSONL — funnel owns the file; harness only reads/proposes.
	sessionPath := *sessionFileFlag
	if sessionPath == "" {
		f, err := os.CreateTemp("", "bridle-session-*.jsonl")
		if err != nil {
			fatalf("create session file: %v", err)
		}
		f.Close()
		sessionPath = f.Name()
		defer os.Remove(sessionPath)
	}
	fmt.Printf("[stubfunnel] session=%s\n", sessionPath)

	// Tool surface.
	tools := []bridle.ToolDef{
		{
			Name:        "send_comms",
			Description: "Send a message to the network (stub: prints to stdout).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		},
		{
			Name:        "now",
			Description: "Returns the current UTC time as an ISO-8601 string.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "read_file",
			Description: "Read a file from the local filesystem.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
	}

	runner := &stubToolRunner{}
	harness := bridle.NewHarness(provider)

	// Register an observability hook — log every tool call.
	harness.RegisterBeforeToolCall(func(ctx context.Context, in bridle.BeforeToolCallCtx) (bridle.BeforeToolCallCtx, bridle.HookAction, error) {
		fmt.Printf("[hook:BeforeToolCall] step=%d tool=%s id=%s\n", in.Step, in.Call.Name, in.Call.ID)
		return in, bridle.HookContinue, nil
	})
	harness.RegisterOnStepBoundary(func(ctx context.Context, in bridle.OnStepBoundaryCtx) (bridle.OnStepBoundaryCtx, bridle.HookAction, error) {
		fmt.Printf("[hook:OnStepBoundary] step=%d\n", in.Step)
		return in, bridle.HookContinue, nil
	})

	// Deliberation loop.
	var inbox []bridle.InboxItem
	var sessionTail []bridle.SessionEvent
	var lastResult bridle.TurnResult

	// Seed inbox with a synthetic comms item to exercise inbox folding.
	inbox = append(inbox, bridle.InboxItem{
		From:    "operator",
		Content: "Reminder: always use the now tool to report time.",
	})

	for turn := 0; turn < *maxTurnsFlag; turn++ {
		fmt.Printf("\n[stubfunnel] === deliberation turn %d ===\n", turn+1)

		sink := &logSink{prefix: fmt.Sprintf("turn%d", turn+1)}

		req := bridle.TurnRequest{
			AspectID:     "stubfunnel",
			AppendSystemPrompt: "You are a test aspect in the Nexus network. Use tools when asked. When you want to send information back to the network, use send_comms.",
			SessionTail:  sessionTail,
			UserMessage:  *promptFlag,
			Inbox:        inbox,
			Tools:        tools,
			Provider:     bridle.ProviderID(*providerFlag),
			Model:        model,
			MaxSteps:     *maxStepsFlag,
		}

		// Clear inbox after first turn (it's been folded in).
		inbox = nil
		// Clear user message after first turn (autonomous continuation).
		if turn > 0 {
			req.UserMessage = ""
		}

		result, err := harness.RunTurn(ctx, req, runner, sink)
		if err != nil {
			fmt.Printf("[stubfunnel] RunTurn error: %v\n", err)
			break
		}

		lastResult = result
		fmt.Printf("[stubfunnel] turn %d done: stop=%s steps=%d input_tokens=%d output_tokens=%d\n",
			turn+1, result.StopReason, result.StepCount, result.Usage.InputTokens, result.Usage.OutputTokens)
		if result.FinalText != "" {
			fmt.Printf("[stubfunnel] final_text: %s\n", result.FinalText)
		}

		// Funnel owns session: append SessionDelta to the tail and write to JSONL.
		sessionTail = append(sessionTail, result.SessionDelta...)
		if err := appendSessionDelta(sessionPath, result.SessionDelta); err != nil {
			fmt.Printf("[stubfunnel] session write error: %v\n", err)
		}

		// Stop if model is done and said something.
		if result.StopReason == bridle.StopReasonModelDone && result.FinalText != "" {
			break
		}
	}

	// Log-decision turn: ask a cheap stub whether to keep this turn in thread history.
	fmt.Printf("\n[stubfunnel] === log-decision turn ===\n")
	keepDecision := runLogDecisionTurn(ctx, harness, model, lastResult.FinalText, sessionTail)
	fmt.Printf("[stubfunnel] log-decision: keep=%v\n", keepDecision)

	// Print session JSONL contents.
	fmt.Printf("\n[stubfunnel] === session JSONL (%s) ===\n", sessionPath)
	data, _ := os.ReadFile(sessionPath)
	fmt.Printf("%s\n", data)

	fmt.Println("\n[stubfunnel] done.")
}

// runLogDecisionTurn runs a single cheap turn whose only job is to emit a
// keep/discard JSON decision about whether to append the turn to thread history.
func runLogDecisionTurn(ctx context.Context, h *bridle.Harness, model string, turnText string, tail []bridle.SessionEvent) bool {
	logTool := []bridle.ToolDef{{
		Name:        "log_decision",
		Description: "Record whether this turn should be kept in thread history.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"keep":{"type":"boolean"},"reason":{"type":"string"}},"required":["keep","reason"]}`),
	}}

	prompt := fmt.Sprintf("The deliberation turn produced this output: %q\n\nShould this turn be kept in thread history? Call log_decision with keep=true or keep=false and a brief reason.", turnText)

	sink := &logSink{prefix: "log-decision"}
	runner := &logDecisionRunner{}

	req := bridle.TurnRequest{
		AspectID:     "stubfunnel",
		AppendSystemPrompt: "You are a log-decision judge. Your only job is to call log_decision.",
		SessionTail:  tail,
		UserMessage:  prompt,
		Tools:        logTool,
		Model:        model,
		MaxSteps:     1,
	}
	result, err := h.RunTurn(ctx, req, runner, sink)
	if err != nil || len(result.ToolCalls) == 0 {
		fmt.Printf("[log-decision] no tool call (err=%v); defaulting to keep=true\n", err)
		return true
	}

	// Parse the log_decision call args.
	for _, tc := range result.ToolCalls {
		if tc.Name == "log_decision" {
			var args struct {
				Keep   bool   `json:"keep"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(tc.Args, &args); err == nil {
				fmt.Printf("[log-decision] keep=%v reason=%q\n", args.Keep, args.Reason)
				return args.Keep
			}
		}
	}
	return true
}

// appendSessionDelta appends SessionEvents to the JSONL file.
func appendSessionDelta(path string, delta []bridle.SessionEvent) error {
	if len(delta) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range delta {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// loadSessionTail reads a JSONL file into a SessionEvent slice.
func loadSessionTail(path string) ([]bridle.SessionEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var events []bridle.SessionEvent
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var e bridle.SessionEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse session line: %w", err)
		}
		events = append(events, e)
	}
	return events, nil
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

// --- tool runner ---

type stubToolRunner struct{}

func (r *stubToolRunner) Run(_ context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	switch call.Name {
	case "send_comms":
		var args struct {
			Message string `json:"message"`
		}
		json.Unmarshal(call.Args, &args)
		fmt.Printf("[send_comms] %s\n", args.Message)
		return json.RawMessage(`{"ok":true}`), nil

	case "now":
		t := time.Now().UTC().Format(time.RFC3339)
		result, _ := json.Marshal(map[string]string{"time": t})
		return result, nil

	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		json.Unmarshal(call.Args, &args)
		abs := args.Path
		if !filepath.IsAbs(abs) {
			cwd, _ := os.Getwd()
			abs = filepath.Join(cwd, abs)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read_file: %w", err)
		}
		result, _ := json.Marshal(map[string]string{"content": string(data)})
		return result, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", call.Name)
	}
}

// logDecisionRunner handles log_decision tool calls (returns a fixed ok).
type logDecisionRunner struct{}

func (r *logDecisionRunner) Run(_ context.Context, call bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

// --- event sink ---

type logSink struct {
	prefix string
}

func (s *logSink) Emit(e bridle.Event) {
	switch ev := e.(type) {
	case bridle.ModelChunk:
		fmt.Printf("[%s:chunk] %s", s.prefix, ev.Text)
	case bridle.ToolCallStart:
		fmt.Printf("[%s:tool_start] id=%s name=%s args=%s\n", s.prefix, ev.ID, ev.Name, ev.Args)
	case bridle.ToolCallResult:
		if ev.Err != "" {
			fmt.Printf("[%s:tool_result] id=%s err=%s\n", s.prefix, ev.ID, ev.Err)
		} else {
			fmt.Printf("[%s:tool_result] id=%s result=%s\n", s.prefix, ev.ID, ev.Result)
		}
	case bridle.StepBoundary:
		fmt.Printf("[%s:step_boundary] step=%d\n", s.prefix, ev.Step)
	case bridle.TurnDone:
		fmt.Printf("[%s:turn_done] stop=%s\n", s.prefix, ev.Result.StopReason)
	case bridle.TurnError:
		fmt.Printf("[%s:turn_error] stage=%s err=%v\n", s.prefix, ev.Stage, ev.Err)
	}
}

// Suppress unused import warning for filepath in case read_file isn't called.
var _ = filepath.Join
