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
	"os"
	"os/exec"
	"strings"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

const providerID bridle.ProviderID = "claude-code"

// systemPromptSpillThresholdBytes is the body length above which we write
// the system prompt to a tempfile and pass --append-system-prompt-file
// instead of inlining via --append-system-prompt. Windows CreateProcess
// caps total argv at ~32K; the system prompt is the single largest
// contributor in Frame-class callers (central nexus_md + NEXUS.md +
// SOUL.md + PRIMER + roster + toolkit blurbs). 8K leaves ~24K headroom
// for every other argv field (-p body, --allowedTools, --session-id,
// ExtraArgs), which is enough by a wide margin.
//
// Observed 2026-05-13: keel's Frame composition crossed 32K once roster
// + toolkit blurbs were added, manifesting as the misleading kernel
// error "filename or extension is too long". See task #674.
const systemPromptSpillThresholdBytes = 8 * 1024

// Provider implements bridle.Provider by shelling out to the claude CLI.
type Provider struct {
	// ClaudePath is the path to the claude binary. Defaults to "claude" (PATH lookup).
	ClaudePath string
	// AllowedTools restricts which claude-native tools the CLI may use.
	AllowedTools []string
	// ExtraArgs are appended verbatim to the claude invocation.
	ExtraArgs []string
	// Bare, when true, passes --bare to the CLI. Bare mode skips hooks,
	// LSP, plugin sync, attribution, auto-memory, keychain reads, and
	// CLAUDE.md auto-discovery. Intended for short-lived subprocess
	// callers (e.g. cheap-judge subprocesses) that want a minimal CLI
	// surface and don't need user-level state. Context must be supplied
	// explicitly via --append-system-prompt[-file] / --add-dir / etc.
	Bare bool
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

	// --permission-mode bypassPermissions: aspects in nexus run with
	// full filesystem trust by design — the operator's threat model is
	// "the aspect is me, just running headless." Without this flag,
	// claude-code's default sandbox limits file ops to the launch cwd,
	// which is the nexus process dir and useless for repo work. The
	// operator-trusted-aspect model has been the rule since agent-
	// network; bridle.claudecode honors it for parity. Aspects that
	// need restriction should run a different provider or wrap with
	// OS-level sandboxing — bridle defers to the runtime, not provider.
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"}

	// systemPromptFile is set (non-empty) when we spill the system
	// prompt to disk so we can delete it after the CLI exits. Kept in
	// scope so cleanup runs whether the call returns via the happy
	// path, parse error, or context cancellation.
	var systemPromptFile string
	defer func() {
		if systemPromptFile != "" {
			_ = os.Remove(systemPromptFile)
		}
	}()

	if req.AppendSystemPrompt != "" {
		spillArgs, file, perr := appendSystemPromptArgs(req.AppendSystemPrompt)
		if perr != nil {
			return bridle.ProviderResult{}, perr
		}
		systemPromptFile = file
		args = append(args, spillArgs...)
	}

	if p.Bare {
		args = append(args, "--bare")
	}

	// Allowed tools: per-turn list from the funnel (req.Tools.Name) takes
	// precedence; fall back to the provider-level default. The CLI owns
	// execution, so the funnel sets the *allowlist*, not the schemas.
	//
	// Guard: a non-empty req.Tools with all-empty Name fields would
	// otherwise translate to allowed=[] and silently drop the
	// --allowedTools flag, letting the CLI run with the full default
	// allowlist. That's a footgun (silent privilege escalation), so on
	// degenerate input we fall back to p.AllowedTools rather than the
	// "no flag at all" path.
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
		// else: req.Tools had nothing usable; fall through to p.AllowedTools.
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
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
	// Cwd anchors claude-code's session jsonl path AND its .mcp.json
	// discovery, both of which are derived from process cwd. Per-aspect
	// callers (e.g. nexus's funnel) set req.Cwd to the aspect's home so
	// sessions don't collide and MCP identity doesn't leak across
	// aspects sharing the same Harness. Empty falls through to the
	// bridle host's cwd, which is correct for single-aspect callers.
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	// Per-turn env overlay (task #218). req.ProviderEnv carries the
	// auth/routing keys the funnel selected for THIS turn — typically
	// ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL when running judge / cheap-
	// model paths against a non-subscription credential. Layered over
	// the parent process env so anything not explicitly overridden
	// (PATH, HOME, etc.) keeps working. Empty/nil = no overlay; the
	// subprocess inherits the bridle host's env unchanged.
	if len(req.ProviderEnv) > 0 {
		cmd.Env = mergeEnv(os.Environ(), req.ProviderEnv)
	}
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
	//
	// procExited is closed AFTER cmd.Wait() returns (see below). The
	// watcher waits on either ctx cancellation OR the process exiting
	// naturally; on cancellation it sends SIGTERM and waits up to the
	// grace period for procExited before SIGKILLing. Without procExited
	// being closed externally, the watcher would (a) leak on natural
	// exit and (b) always SIGKILL after the full grace period even when
	// the process already responded to SIGTERM.
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
		} else if result.FinalText != "" || len(result.ToolCalls) > 0 || len(result.SessionDelta) > 0 {
			// Subprocess exited non-zero AFTER producing parseable
			// content — common cause: output-token cap hit, CLI surfaces
			// it as exit 1 rather than a clean stop. Preserve the partial
			// result so the funnel can still auto-post what the model
			// said. Without this, ~10KB of substantive output gets
			// silently dropped because we return ProviderResult{} (#219).
			result.StopReason = bridle.StopReasonProcessExit
			sink.Emit(bridle.TurnError{
				Err:   fmt.Errorf("claudecode: subprocess exited non-zero with partial content: %w", waitErr),
				Stage: "subprocess_exit_partial",
			})
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

// appendSystemPromptArgs returns the CLI args for the caller's
// AppendSystemPrompt body, plus the tempfile path used (or "" if the
// inline form was taken). Callers are responsible for deleting the
// tempfile after the subprocess exits.
//
// --append-system-prompt (inline) is used when the body fits within
// systemPromptSpillThresholdBytes. Beyond that we spill to a tempfile
// and pass --append-system-prompt-file, keeping argv well under
// Windows CreateProcess's 32K ceiling. See task #674 and the const
// docstring for the why.
//
// The body must be non-empty; appendSystemPromptArgs assumes the
// caller already checked.
func appendSystemPromptArgs(body string) ([]string, string, error) {
	if len(body) <= systemPromptSpillThresholdBytes {
		return []string{"--append-system-prompt", body}, "", nil
	}
	f, err := os.CreateTemp("", "bridle-sysprompt-*.txt")
	if err != nil {
		return nil, "", fmt.Errorf("claudecode: tempfile for system prompt: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", fmt.Errorf("claudecode: write system prompt tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return nil, "", fmt.Errorf("claudecode: close system prompt tempfile: %w", err)
	}
	return []string{"--append-system-prompt-file", f.Name()}, f.Name(), nil
}

// buildPrompt returns the current turn's user message for the CLI's -p arg.
//
// Returns ONLY the most recent user message — not the full SessionTail. The
// claude-code subprocess gets prior conversation history from the session
// jsonl on --resume <id>, not from argv. Folding SessionTail into -p here
// would (a) duplicate the history the subprocess is already loading from
// disk and (b) blow Windows CreateProcess's 32K argv budget once a session
// accumulates state. Observed 2026-05-13: keel as Frame (global context)
// crossed 32K after a few turns and every spawn failed with the misleading
// "filename or extension is too long" kernel error.
//
// Direct-API providers (claude-api etc.) need history reassembled because
// they have no subprocess-owned jsonl — they use toClaudeMessages() in
// their own provider package, not this function. buildPrompt is
// claudecode-exclusive by design.
//
// See task #216 for full diagnosis.
func buildPrompt(req bridle.ProviderRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// mergeEnv overlays the per-turn key=value map onto the parent
// process's env. Per-turn keys take precedence; any KEY=VALUE pair in
// `base` whose KEY is in `overlay` is replaced. Both inputs are left
// unmodified.
func mergeEnv(base []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return base
	}
	// Index base by KEY for O(1) replacement.
	idx := make(map[string]int, len(base))
	out := make([]string, len(base))
	copy(out, base)
	for i, kv := range out {
		eq := -1
		for j := 0; j < len(kv); j++ {
			if kv[j] == '=' {
				eq = j
				break
			}
		}
		if eq > 0 {
			idx[kv[:eq]] = i
		}
	}
	for k, v := range overlay {
		entry := k + "=" + v
		if i, ok := idx[k]; ok {
			out[i] = entry
		} else {
			out = append(out, entry)
		}
	}
	return out
}
