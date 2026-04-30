// Package claudecode implements the bridle Provider interface by driving the
// claude-code CLI in headless mode (claude -p --output-format stream-json --verbose).
//
// Shape note: The claude-code CLI manages its own tool loop internally.
// bridle's BeforeToolCall / AfterToolCall hooks and ToolRunner are NOT invoked
// on this path — the CLI executes tools itself. The stub funnel passes
// --allowedTools to restrict the CLI's tool surface to the tools it defines.
//
// This means the bridle ToolRunner contract is not fully exercised via this
// provider. For full hook + ToolRunner validation use provider/claude (direct
// API). This provider validates the session-delta, event emission, and
// stop-reason normalization paths.
//
// Spec note: This is a shape mismatch with the bridle spec (§3.3 "providers
// MUST NOT own tool execution"). Flag to @keel for a spec patch — either
// the spec needs a "subprocess provider" carve-out, or the real funnel must
// use provider/claude and manage its own auth.
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

	bridle "github.com/nexus-cw/bridle"
	"github.com/nexus-cw/bridle/internal/normalize"
)

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

func (p *Provider) Name() bridle.ProviderID { return "claude-code" }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategorySubprocessStream,
		SupportsCustomTools:    false,
		SupportsBeforeToolCall: false,
		SupportsAfterToolCall:  true,
	}
}

// RunTurn invokes the claude CLI and streams its output as bridle events.
// Tool calls made by the model are executed by the CLI itself; the bridle
// ToolRunner is NOT called on this path (see package doc).
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
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

	args = append(args, p.ExtraArgs...)

	cmd := exec.CommandContext(ctx, claudePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: start: %w", err)
	}

	result, parseErr := parseStream(stdoutPipe, sink)

	if waitErr := cmd.Wait(); waitErr != nil && parseErr == nil {
		if ctx.Err() != nil {
			result.StopReason = bridle.StopReasonAborted
		} else {
			return bridle.ProviderResult{}, fmt.Errorf("claudecode: CLI error: %w (stderr: %s)", waitErr, stderr.String())
		}
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
	)

	pendingCalls := map[string]bridle.ToolCallStart{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB line buffer
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var event map[string]json.RawMessage
		if jsonErr := json.Unmarshal(line, &event); jsonErr != nil {
			continue
		}

		var eventType string
		json.Unmarshal(event["type"], &eventType)

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
				} `json:"message"`
			}
			if jsonErr := json.Unmarshal(line, &msg); jsonErr == nil {
				for _, block := range msg.Message.Content {
					switch block.Type {
					case "text":
						sink.Emit(bridle.ModelChunk{Text: block.Text})
						finalText += block.Text
						sessionDelta = append(sessionDelta, bridle.SessionEvent{
							Role:    bridle.RoleAssistant,
							Content: block.Text,
						})
					case "tool_use":
						tc := bridle.ToolCallStart{ID: block.ID, Name: block.Name, Args: block.Input}
						sink.Emit(tc)
						pendingCalls[block.ID] = tc
						raw, _ := json.Marshal(block)
						sessionDelta = append(sessionDelta, bridle.SessionEvent{
							Role:    bridle.RoleAssistant,
							RawJSON: raw,
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
							Role:    bridle.RoleTool,
							Content: block.Content,
						})
					}
				}
			}

		case "result":
			var result struct {
				Result     string `json:"result"`
				StopReason string `json:"stop_reason"`
				Usage      struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if jsonErr := json.Unmarshal(line, &result); jsonErr == nil {
				if result.Result != "" && finalText == "" {
					finalText = result.Result
				}
				stopReason = bridle.StopReason(normalize.ClaudeStopReason(result.StopReason))
				usage.InputTokens = result.Usage.InputTokens
				usage.OutputTokens = result.Usage.OutputTokens
			}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return bridle.ProviderResult{}, fmt.Errorf("claudecode: stream read: %w", err)
	}

	if stopReason == "" {
		stopReason = bridle.StopReasonModelDone
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
