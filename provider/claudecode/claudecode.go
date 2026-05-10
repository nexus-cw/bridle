// Package claudecode implements the bridle Provider interface using the
// claude-code CLI in headless mode (claude -p --output-format stream-json --verbose).
//
// Category: subprocess-stream. The CLI manages its own agentic loop;
// bridle parses the stdout event stream. BeforeToolCall does not fire;
// AfterToolCall fires after each parsed tool_use/tool_result pair (observe-only).
//
// Session continuity: the caller passes a SessionHandle with a non-empty ID.
// SessionHandle.New=true → the funnel is starting a fresh session for this
// ID, so we pass --session-id <id> (CLI creates the jsonl). New=false → the
// funnel is continuing an existing session, so we pass --resume <id> (CLI
// loads the jsonl, errors if not found).
package claudecode

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
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

const providerID bridle.ProviderID = "claude-code"

// Provider implements bridle.Provider by shelling out to the claude CLI.
type Provider struct {
	// ClaudePath is the path to the claude binary. Defaults to "claude" (PATH lookup).
	ClaudePath string
	// AllowedTools restricts which claude-native tools the CLI may use.
	AllowedTools []string
	// ExtraArgs are appended verbatim to the claude invocation.
	ExtraArgs []string
}

// New returns a claudecode Provider with default settings.
func New() *Provider {
	return &Provider{ClaudePath: "claude"}
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

// RunTurn invokes the claude CLI and streams its output as bridle events.
// If req.Session.ID is non-empty, passes --resume <id> for continuity.
// Tool calls are executed by the CLI; bridle's ToolRunner is not called.
// Cancellation via ctx sends SIGTERM then SIGKILL after a grace period.
//
// Session-id resilience: when Session.New is true but claude-code reports
// "Session ID already in use", retry once with --resume. This handles
// the case where a prior turn started, wrote the jsonl, and errored
// before the funnel could flip Session.New=false. Without the fallback
// the session was permanently bricked.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	result, err := p.runTurnOnce(ctx, req, sink, req.Session.New)
	if err != nil && req.Session.ID != "" && req.Session.New && isSessionIDInUseErr(err) {
		// First attempt was --session-id and the id is already taken
		// (probably from a prior failed turn that wrote the jsonl).
		// Retry once with --resume — the on-disk session is the right
		// continuation point.
		return p.runTurnOnce(ctx, req, sink, false)
	}
	return result, err
}

// isSessionIDInUseErr matches claude-code's "Session ID ... is already
// in use" error message. Substring match — claude-code's exact format
// includes ANSI color codes which we'd rather not pin against.
func isSessionIDInUseErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "is already in use")
}

func (p *Provider) runTurnOnce(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink, sessionIsNew bool) (bridle.ProviderResult, error) {
	claudePath := p.ClaudePath
	if claudePath == "" {
		claudePath = "claude"
	}

	prompt := buildPrompt(req)

	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}

	if req.SystemPrompt != "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}

	if len(p.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(p.AllowedTools, ","))
	}

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	// Session continuity: --session-id creates a fresh CLI session jsonl
	// with this id; --resume loads an existing one. The funnel signals
	// which via Session.New so the provider doesn't have to probe disk.
	// claude-code errors loudly on --resume for a non-existent id, so
	// getting this wrong on the first turn is the difference between a
	// working aspect and a permanent NoConversationFound on every turn.
	if req.Session.ID != "" {
		if sessionIsNew {
			args = append(args, "--session-id", req.Session.ID)
		} else {
			args = append(args, "--resume", req.Session.ID)
		}
	}

	args = append(args, p.ExtraArgs...)

	// Don't use exec.CommandContext — that SIGKILLs immediately on cancel.
	// We want SIGTERM first, then SIGKILL after a grace period.
	cmd := exec.Command(claudePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: start: %w", err)
	}

	// Cancel watcher: SIGTERM + grace period + SIGKILL.
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(sigterm())
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = cmd.Process.Kill()
			case <-done:
			}
		case <-done:
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
	<-streamDone

	if waitErr != nil && parseErr == nil {
		if ctx.Err() != nil {
			result.StopReason = bridle.StopReasonAborted
		} else {
			sink.Emit(bridle.TurnError{Err: fmt.Errorf("claudecode: %w", waitErr), Stage: "subprocess_exit"})
			return bridle.ProviderResult{}, fmt.Errorf("claudecode: CLI error: %w (stderr: %s)", waitErr, stderr.String())
		}
	}

	// If stream ended without a result event, that's truncation.
	if parseErr == nil && result.StopReason == "" && ctx.Err() == nil {
		parseErr = fmt.Errorf("claudecode: stream ended without result event")
		sink.Emit(bridle.TurnError{Err: parseErr, Stage: "stream_truncated"})
	}

	return result, parseErr
}

// parseStream reads stream-json lines and maps them to bridle events + result.
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
		if len(line) == 0 {
			continue
		}

		var event map[string]json.RawMessage
		if jsonErr := json.Unmarshal(line, &event); jsonErr != nil {
			continue // malformed line — skip, don't fail the turn
		}

		var eventType string
		json.Unmarshal(event["type"], &eventType) //nolint:errcheck

		switch eventType {
		case "assistant":
			var msg struct {
				Message struct {
					Content []struct {
						Type  string          `json:"type"`
						Text  string          `json:"text"`
						ID    string          `json:"id"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"content"`
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						OutputTokens             int `json:"output_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if jsonErr := json.Unmarshal(line, &msg); jsonErr == nil {
				// Cache stats live on per-message usage in the
				// assistant stream event, NOT the result event.
				// Accumulate as the stream goes by; the result
				// event's totals don't include cache breakdown.
				usage.CacheReadInputTokens += msg.Message.Usage.CacheReadInputTokens
				usage.CacheCreationInputTokens += msg.Message.Usage.CacheCreationInputTokens
				for _, block := range msg.Message.Content {
					switch block.Type {
					case "text":
						sink.Emit(bridle.ModelChunk{Text: block.Text})
						finalText += block.Text
						sessionDelta = append(sessionDelta, bridle.SessionEvent{
							Provider: providerID,
							Role:     bridle.RoleAssistant,
							Content:  block.Text,
						})
					case "tool_use":
						tc := bridle.ToolCallStart{ID: block.ID, Name: block.Name, Args: block.Input}
						sink.Emit(tc)
						pendingCalls[block.ID] = tc
						raw, _ := json.Marshal(block)
						sessionDelta = append(sessionDelta, bridle.SessionEvent{
							Provider: providerID,
							Role:     bridle.RoleAssistant,
							RawJSON:  raw,
						})
					}
				}
			}

		case "user":
			var msg struct {
				Message struct {
					Content []struct {
						Type      string `json:"type"`
						ToolUseID string `json:"tool_use_id"`
						Content   string `json:"content"`
					} `json:"content"`
				} `json:"message"`
			}
			if jsonErr := json.Unmarshal(line, &msg); jsonErr == nil {
				for _, block := range msg.Message.Content {
					if block.Type == "tool_result" {
						resultJSON, _ := json.Marshal(block.Content)
						tcr := bridle.ToolCallResult{
							ID:     block.ToolUseID,
							Result: resultJSON,
						}
						sink.Emit(tcr)

						if start, ok := pendingCalls[block.ToolUseID]; ok {
							toolCalls = append(toolCalls, bridle.ToolInvocation{
								ID:     start.ID,
								Name:   start.Name,
								Args:   start.Args,
								Result: resultJSON,
							})
							delete(pendingCalls, block.ToolUseID)
							stepCount++
							sink.Emit(bridle.StepBoundary{Step: stepCount})
						}
						sessionDelta = append(sessionDelta, bridle.SessionEvent{
							Provider: providerID,
							Role:     bridle.RoleTool,
							Content:  block.Content,
						})
					}
				}
			}

		case "result":
			var res struct {
				Result     string `json:"result"`
				StopReason string `json:"stop_reason"`
				Usage      struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if jsonErr := json.Unmarshal(line, &res); jsonErr == nil {
				if res.Result != "" && finalText == "" {
					finalText = res.Result
				}
				stopReason = bridle.StopReason(normalize.ClaudeStopReason(res.StopReason))
				usage.InputTokens = res.Usage.InputTokens
				usage.OutputTokens = res.Usage.OutputTokens
				usage.CacheReadInputTokens = res.Usage.CacheReadInputTokens
				usage.CacheCreationInputTokens = res.Usage.CacheCreationInputTokens
				gotResult = true
			}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: stream read: %w", err)
	}

	if !gotResult {
		// Leave StopReason empty so RunTurn can detect stream_truncated.
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

	if len(contextLines) == 0 {
		return userPrompt
	}
	return fmt.Sprintf("Prior context:\n%s\n\n%s", strings.Join(contextLines, "\n"), userPrompt)
}
