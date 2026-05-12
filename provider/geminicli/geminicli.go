// Package geminicli implements the bridle Provider interface using the
// gemini CLI in headless mode (gemini -p --output-format stream-json -y).
//
// Category: subprocess-stream. The CLI manages its own agentic loop;
// bridle parses the stdout event stream. BeforeToolCall does not fire;
// AfterToolCall fires after each parsed tool_use/tool_result pair (observe-only).
//
// Auth: the CLI itself decides — Google login (Gemini Pro/Ultra subscription
// quota), API key, or Vertex AI. Bridle does not pass credentials.
//
// Session continuity: gemini's --resume flag accepts "latest" or a numeric
// index, NOT a UUID. The Session.ID field is passed through verbatim, so the
// caller must supply one of those forms. The init event's session_id is
// recorded as a SessionEvent for the funnel's records but cannot itself be
// fed back to --resume.
package geminicli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

const providerID bridle.ProviderID = "gemini-cli"

// Provider implements bridle.Provider by shelling out to the gemini CLI.
type Provider struct {
	// GeminiPath is the path to the gemini binary. Defaults to "gemini" (PATH lookup).
	GeminiPath string
	// AllowedTools restricts which gemini-native tools the CLI may use.
	// Maps to --allowed-tools (deprecated CLI flag but still honored).
	AllowedTools []string
	// SkipTrust passes --skip-trust so the CLI runs in untrusted folders.
	SkipTrust bool
	// Yolo passes -y so the CLI auto-approves tool calls (recommended for headless).
	Yolo bool
	// ExtraArgs are appended verbatim to the gemini invocation.
	ExtraArgs []string
}

// New returns a geminicli Provider with default settings (yolo on, skip-trust on).
// Headless agent use almost always wants both — without yolo, tool calls block;
// without skip-trust, the CLI refuses to operate outside trusted folders.
func New() *Provider {
	return &Provider{GeminiPath: "gemini", Yolo: true, SkipTrust: true}
}

func (p *Provider) Name() bridle.ProviderID { return providerID }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategorySubprocessStream,
		SupportsCustomTools:    false,
		SupportsBeforeToolCall: false,
		SupportsAfterToolCall:  true,
		SupportsMCP:            false,
	}
}

// RunTurn invokes the gemini CLI and streams its output as bridle events.
// Tool calls are executed by the CLI; bridle's ToolRunner is not called.
// Cancellation via ctx sends SIGTERM then SIGKILL after a 5s grace period.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	geminiPath := p.GeminiPath
	if geminiPath == "" {
		geminiPath = "gemini"
	}

	prompt := buildPrompt(req)

	args := []string{"-p", prompt, "--output-format", "stream-json"}

	if p.Yolo {
		args = append(args, "-y")
	}
	if p.SkipTrust {
		args = append(args, "--skip-trust")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	// Allowed tools: per-turn list from the funnel (req.Tools.Name) takes
	// precedence; fall back to the provider-level default. The CLI owns
	// execution, so the funnel sets the *allowlist*, not the schemas.
	//
	// Guard: a non-empty req.Tools with all-empty Name fields would
	// otherwise translate to allowed=[] and emit no --allowed-tools
	// flags, letting the CLI run with the full default allowlist. That's
	// a footgun (silent privilege escalation), so on degenerate input
	// we fall back to p.AllowedTools rather than the empty path.
	allowed := p.AllowedTools
	if len(req.Tools) > 0 {
		perTurn := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Name != "" {
				perTurn = append(perTurn, t.Name)
			}
		}
		if len(perTurn) > 0 {
			allowed = perTurn
		}
	}
	for _, t := range allowed {
		args = append(args, "--allowed-tools", t)
	}
	// gemini --resume accepts "latest" or a numeric index — not a UUID.
	// Pass through whatever the caller supplied; document mismatch elsewhere.
	if req.Session.ID != "" {
		args = append(args, "--resume", req.Session.ID)
	}

	args = append(args, p.ExtraArgs...)

	cmd := exec.Command(geminiPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("geminicli: pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("geminicli: start: %w", err)
	}

	// Cancel watcher: SIGTERM + grace period + SIGKILL.
	//
	// procExited is closed AFTER cmd.Wait() returns. The watcher waits
	// on either ctx cancellation OR the process exiting naturally; on
	// cancellation it sends SIGTERM and waits up to the grace period
	// for procExited before SIGKILLing. Without procExited being closed
	// externally, the watcher would (a) leak on natural exit and (b)
	// always SIGKILL after the full grace period even when the process
	// already responded to SIGTERM.
	procExited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(sigterm())
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = cmd.Process.Kill()
			case <-procExited:
				// Process exited cleanly during grace period — no SIGKILL needed.
			}
		case <-procExited:
			// Natural exit — nothing to do.
		}
	}()

	streamDone := make(chan struct{})
	var result bridle.ProviderResult
	var parseErr error
	go func() {
		defer close(streamDone)
		result, parseErr = parseStream(stdoutPipe, sink)
	}()

	waitErr := cmd.Wait()
	close(procExited) // signal the cancel watcher that the process is gone
	<-streamDone

	if waitErr != nil && parseErr == nil {
		if ctx.Err() != nil {
			result.StopReason = bridle.StopReasonAborted
		} else {
			sink.Emit(bridle.TurnError{Err: fmt.Errorf("geminicli: %w", waitErr), Stage: "subprocess_exit"})
			return bridle.ProviderResult{}, fmt.Errorf("geminicli: CLI error: %w (stderr: %s)", waitErr, stderr.String())
		}
	}

	if parseErr == nil && result.StopReason == "" && ctx.Err() == nil {
		parseErr = fmt.Errorf("geminicli: stream ended without result event")
		sink.Emit(bridle.TurnError{Err: parseErr, Stage: "stream_truncated"})
	}

	return result, parseErr
}

// parseStream reads stream-json lines and maps them to bridle events + result.
//
// Event shapes (observed from gemini CLI v0.x stream-json output):
//   {"type":"init", "session_id":"<uuid>", "model":"<id>"}
//   {"type":"message", "role":"user|assistant", "content":"...", "delta":true?}
//   {"type":"tool_use", "tool_name":"...", "tool_id":"...", "parameters":{...}}
//   {"type":"tool_result", "tool_id":"...", "status":"success|..."}
//   {"type":"result", "status":"success|...", "stats":{"input_tokens":..,"output_tokens":..,"tool_calls":..}}
func parseStream(r io.Reader, sink bridle.EventSink) (bridle.ProviderResult, error) {
	var (
		finalText    string
		toolCalls    []bridle.ToolInvocation
		sessionDelta []bridle.SessionEvent
		usage        bridle.Usage
		stopReason   bridle.StopReason
		stepCount    int
		gotResult    bool
	)

	pendingCalls := map[string]bridle.ToolCallStart{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			// CLI prints free-form banners ("YOLO mode is enabled.", etc.) on stdout.
			// Skip anything that isn't a JSON object.
			continue
		}

		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}

		switch head.Type {
		case "init":
			var ev struct {
				SessionID string `json:"session_id"`
				Model     string `json:"model"`
			}
			if err := json.Unmarshal(line, &ev); err == nil {
				raw, _ := json.Marshal(map[string]any{
					"session_id": ev.SessionID,
					"model":      ev.Model,
				})
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: providerID,
					Role:     bridle.RoleSystem,
					RawJSON:  raw,
				})
			}

		case "message":
			var ev struct {
				Role    string `json:"role"`
				Content string `json:"content"`
				Delta   bool   `json:"delta"`
			}
			if err := json.Unmarshal(line, &ev); err == nil {
				if ev.Role == "assistant" {
					sink.Emit(bridle.ModelChunk{Text: ev.Content})
					finalText += ev.Content
					sessionDelta = append(sessionDelta, bridle.SessionEvent{
						Provider: providerID,
						Role:     bridle.RoleAssistant,
						Content:  ev.Content,
					})
				}
				// User echoes are recorded but not re-emitted; the funnel already has them.
			}

		case "tool_use":
			var ev struct {
				ToolName   string          `json:"tool_name"`
				ToolID     string          `json:"tool_id"`
				Parameters json.RawMessage `json:"parameters"`
			}
			if err := json.Unmarshal(line, &ev); err == nil {
				tc := bridle.ToolCallStart{ID: ev.ToolID, Name: ev.ToolName, Args: ev.Parameters}
				sink.Emit(tc)
				pendingCalls[ev.ToolID] = tc
				raw, _ := json.Marshal(ev)
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: providerID,
					Role:     bridle.RoleAssistant,
					RawJSON:  raw,
				})
			}

		case "tool_result":
			var ev struct {
				ToolID string          `json:"tool_id"`
				Status string          `json:"status"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(line, &ev); err == nil {
				resultJSON := ev.Result
				if len(resultJSON) == 0 {
					resultJSON, _ = json.Marshal(map[string]any{"status": ev.Status})
				}
				sink.Emit(bridle.ToolCallResult{ID: ev.ToolID, Result: resultJSON})

				if start, ok := pendingCalls[ev.ToolID]; ok {
					toolCalls = append(toolCalls, bridle.ToolInvocation{
						ID:     start.ID,
						Name:   start.Name,
						Args:   start.Args,
						Result: resultJSON,
					})
					delete(pendingCalls, ev.ToolID)
					stepCount++
					sink.Emit(bridle.StepBoundary{Step: stepCount})
				}
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: providerID,
					Role:     bridle.RoleTool,
					Content:  string(resultJSON),
				})
			}

		case "result":
			var ev struct {
				Status string `json:"status"`
				Stats  struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"stats"`
			}
			if err := json.Unmarshal(line, &ev); err == nil {
				usage.InputTokens = ev.Stats.InputTokens
				usage.OutputTokens = ev.Stats.OutputTokens
				if ev.Status == "success" {
					stopReason = bridle.StopReasonModelDone
				} else {
					stopReason = bridle.StopReasonError
				}
				gotResult = true
			}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return bridle.ProviderResult{}, fmt.Errorf("geminicli: stream read: %w", err)
	}

	if !gotResult {
		return bridle.ProviderResult{
			FinalText:    finalText,
			ToolCalls:    toolCalls,
			StepCount:    stepCount,
			Usage:        usage,
			SessionDelta: sessionDelta,
		}, nil
	}

	return bridle.ProviderResult{
		FinalText:    finalText,
		ToolCalls:    toolCalls,
		StepCount:    stepCount,
		Usage:        usage,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

// buildPrompt assembles the messages into a single prompt string for the CLI.
// Mirrors claudecode.buildPrompt — last user message becomes the prompt, prior
// turns are folded into a "Prior context:" preamble.
func buildPrompt(req bridle.ProviderRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}

	var contextLines []string
	var userPrompt string

	for i, m := range req.Messages {
		if m.Role == "user" && i == len(req.Messages)-1 {
			userPrompt = m.Content
		} else if m.Content != "" {
			contextLines = append(contextLines, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
		}
	}

	preamble := ""
	if req.AppendSystemPrompt != "" {
		preamble = "System: " + req.AppendSystemPrompt + "\n\n"
	}

	if len(contextLines) == 0 {
		return preamble + userPrompt
	}
	return fmt.Sprintf("%sPrior context:\n%s\n\n%s", preamble, strings.Join(contextLines, "\n"), userPrompt)
}
