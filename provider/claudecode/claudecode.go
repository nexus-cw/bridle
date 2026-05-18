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
	"errors"
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
	// DisallowedTools blocks specific claude-native tools regardless of
	// the (default or explicit) allowlist. Passed verbatim as
	// --disallowed-tools <comma-list>. Useful for blocking tools that
	// don't make sense for funnel-driven aspects (the CLI's full default
	// allowlist exposes things like Agent/SendMessage/TaskCreate/Cron*
	// that are session-orchestration primitives — they spawn children
	// that orphan when the per-turn `claude -p` subprocess exits, or
	// they create a parallel response channel that bypasses the funnel's
	// chat auto-post). Empty = no --disallowed-tools flag passed.
	DisallowedTools []string
	// ExtraArgs are appended verbatim to the claude invocation.
	ExtraArgs []string
	// Bare, when true, passes --bare to the CLI. Bare mode skips hooks,
	// LSP, plugin sync, attribution, auto-memory, keychain reads, and
	// CLAUDE.md auto-discovery. Intended for short-lived subprocess
	// callers (e.g. cheap-judge subprocesses) that want a minimal CLI
	// surface and don't need user-level state. Context must be supplied
	// explicitly via --append-system-prompt[-file] / --add-dir / etc.
	Bare bool
	// MaxRetries is the maximum number of retry attempts for transient
	// errors (rate_limit, server_error, network_error, timeout). 0 = no
	// retry — transient errors propagate immediately. Session-id-in-use
	// retries are independent of this limit. Default: 0.
	MaxRetries int
	// RetryDelay is the initial delay between retries (doubles each
	// attempt). Default: 2s when MaxRetries > 0.
	RetryDelay time.Duration
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
//
// Retry: when MaxRetries > 0, transient errors (rate_limit, server_error,
// network_error, timeout) are retried with exponential backoff.
// Auth failures and unknown subprocess errors propagate immediately.
// Session-id-in-use retries are independent of MaxRetries.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	maxRetries := p.MaxRetries
	retryDelay := p.RetryDelay
	if retryDelay == 0 && maxRetries > 0 {
		retryDelay = 2 * time.Second
	}

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return bridle.ProviderResult{}, ctx.Err()
		}

		sessionIsNew := req.Session.New
		result, err := p.runTurnOnce(ctx, req, sink, sessionIsNew)

		if err == nil {
			return result, nil
		}

		// Session-id-in-use: retry once with --resume. Independent of
		// MaxRetries — this is a specific recovery path, not a transient
		// error retry. The second attempt (sessionIsNew=false) bypasses
		// the transient retry loop to avoid infinite nesting.
		if req.Session.ID != "" && sessionIsNew && isSessionIDInUseErr(err) {
			req.Session.New = false
			continue
		}

		// Don't retry permanent errors.
		if !isRetryable(err) {
			return result, err
		}

		// No more retries left.
		if attempt >= maxRetries {
			return result, err
		}

		// Exponential backoff, clamped.
		delay := retryDelay * (1 << attempt)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		sink.Emit(bridle.TurnError{
			Err:   fmt.Errorf("claudecode: retrying in %v (attempt %d/%d): %w", delay, attempt+1, maxRetries, err),
			Stage: "retry",
		})
		select {
		case <-ctx.Done():
			return bridle.ProviderResult{}, ctx.Err()
		case <-time.After(delay):
		}
	}
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

	args, systemPromptFile, perr := p.buildCLIArgs(req, sessionIsNew)
	if perr != nil {
		return bridle.ProviderResult{}, perr
	}
	defer func() {
		if systemPromptFile != "" {
			_ = os.Remove(systemPromptFile)
		}
	}()

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

	stderrStr := stderr.String()

	if waitErr != nil && parseErr == nil {
		if ctx.Err() != nil {
			result.StopReason = bridle.StopReasonAborted
		} else if result.FinalText != "" || len(result.ToolCalls) > 0 || len(result.SessionDelta) > 0 {
			pe := classifyProviderError(stderrStr, waitErr)
			if pe.Kind == bridle.ProviderErrorAuthFailed || pe.Kind == bridle.ProviderErrorRateLimit || pe.Kind == bridle.ProviderErrorServerError {
				// API-level error — the "content" is a synthetic
				// response from the CLI (e.g. "Not logged in"), not
				// a model answer. Discard it so the funnel doesn't
				// auto-post an auth-failure message as model output.
				sink.Emit(bridle.TurnError{Err: pe, Stage: string(pe.Kind)})
				return bridle.ProviderResult{}, pe
			}
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
			pe := classifyProviderError(stderrStr, waitErr)
			sink.Emit(bridle.TurnError{Err: pe, Stage: string(pe.Kind)})
			return bridle.ProviderResult{}, pe
		}
	}

	// Surface stderr content even when the process exited cleanly.
	// Deprecation warnings, rate-limit headers, and TLS verification
	// notes appear here — without surfacing them, operators have no
	// signal that something is degrading. Non-fatal: we still return
	// the result and don't flip StopReason.
	if stderrStr != "" && waitErr == nil && parseErr == nil && ctx.Err() == nil {
		sink.Emit(bridle.TurnError{
			Err:   fmt.Errorf("claudecode: stderr output: %s", strings.TrimSpace(stderrStr)),
			Stage: "stderr_output",
		})
	}

	// If stream ended without a result event, that's truncation.
	// Include stderr in the error so operators can see the real cause.
	if parseErr == nil && result.StopReason == "" && ctx.Err() == nil {
		msg := "claudecode: stream ended without result event"
		if stderrStr != "" {
			msg += " (stderr: " + strings.TrimSpace(stderrStr) + ")"
		}
		parseErr = fmt.Errorf("%s", msg)
		sink.Emit(bridle.TurnError{Err: parseErr, Stage: "stream_truncated"})
	}

	// If parse failed, wrap the error with stderr context so operators
	// see the real cause rather than a generic "stream read" message.
	if parseErr != nil && stderrStr != "" {
		parseErr = fmt.Errorf("claudecode: %w (stderr: %s)", parseErr, strings.TrimSpace(stderrStr))
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

		// API error detection: claude-code emits events with
		// is_api_error=true and error=<classification> when the
		// provider API returns an error (auth, rate limit, etc.).
		// Capture these so callers can surface a distinct diagnosis
		// even when the stream contains partial content before exit.
		if isAPIError(event) {
			sink.Emit(bridle.TurnError{
				Err:   fmt.Errorf("claudecode: provider API error: %s", string(event["error"])),
				Stage: "provider_api_error",
			})
		}

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
						// Reset finalText: any assistant text emitted
						// BEFORE this tool call was pre-tool exploratory
						// thinking, not the model's settled answer. Only
						// text emitted AFTER the LAST tool call is the
						// answer downstream consumers should see.
						//
						// Operator's call (chat #951, 2026-05-14):
						// "drop — chat doesn't need the tool output if we
						// actually get the assistant text." The pre-fix
						// path was concatenating pre-tool reasoning text
						// + post-tool answer into a single auto-posted
						// chat row, producing harrow's #944 double (draft
						// + rewrite of the same answer pasted together).
						//
						// The streamed-paragraphs path (multiple text
						// blocks in a row with NO tool_use between them)
						// is unaffected: those still concat naturally
						// because nothing resets between them. The
						// partial-content test pins that behavior.
						finalText = ""
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

// buildCLIArgs constructs the claude CLI argument vector for one turn.
// Extracted from runTurnOnce so the allowed-tools / MCP interaction is
// testable without spawning a subprocess.
func (p *Provider) buildCLIArgs(req bridle.ProviderRequest, sessionIsNew bool) (args []string, systemPromptFile string, err error) {
	prompt := buildPrompt(req)
	args = []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"}

	if req.AppendSystemPrompt != "" {
		spillArgs, file, perr := appendSystemPromptArgs(req.AppendSystemPrompt)
		if perr != nil {
			return nil, "", perr
		}
		systemPromptFile = file
		args = append(args, spillArgs...)
	}

	if p.Bare {
		args = append(args, "--bare")
	}

	// Allowed tools: when MCP is configured on this turn, the subprocess
	// discovers its own tools from .mcp.json (via cmd.Dir). req.Tools
	// are bridle-side tool defs — passing them as --allowedTools would
	// silently block every MCP-discovered tool whose name isn't in the
	// list. Per-aspect MCP scoping is handled by cmd.Dir, not by
	// --allowedTools. When MCP is NOT configured, req.Tools (or the
	// provider-level default) drives --allowedTools as before.
	if req.MCP == nil {
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
		if len(allowed) > 0 {
			args = append(args, "--allowedTools", strings.Join(allowed, ","))
		}
	}

	if len(p.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(p.DisallowedTools, ","))
	}

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	if req.Session.ID != "" {
		if sessionIsNew {
			args = append(args, "--session-id", req.Session.ID)
		} else {
			args = append(args, "--resume", req.Session.ID)
		}
	}

	args = append(args, p.ExtraArgs...)
	return args, systemPromptFile, nil
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

// isAPIError reports whether an event carries a CLI-level API error marker.
// claude-code sets is_api_error=true (snake_case) or isApiErrorMessage=true
// (camelCase) when the model API returns an error.
func isAPIError(event map[string]json.RawMessage) bool {
	for _, key := range []string{"is_api_error", "isApiErrorMessage"} {
		if raw, ok := event[key]; ok {
			var v bool
			if json.Unmarshal(raw, &v) == nil && v {
				return true
			}
		}
	}
	return false
}

// isRetryable reports whether a provider error is transient and should
// be retried. Rate limits, server errors, network errors, and timeouts
// are retryable. Auth failures, TLS errors, and unknown subprocess exits
// are permanent — retrying with the same credentials/network config
// produces the same result.
func isRetryable(err error) bool {
	pe := &bridle.ProviderError{}
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Kind {
	case bridle.ProviderErrorRateLimit,
		bridle.ProviderErrorServerError,
		bridle.ProviderErrorNetworkError,
		bridle.ProviderErrorTimeout:
		return true
	}
	return false
}

// classifyProviderError inspects the CLI's stderr and classifies the
// subprocess error into a bridle.ProviderError so the activity log
// surfaces a distinct diagnosis string instead of an opaque exit code.
//
// Patterns matched (case-insensitive substring):
//   - "not logged in", "authentication_failed", "run /login" → auth_failed
//   - "rate_limit", "rate limited" → rate_limit
//   - "server_error", "internal server error", "overloaded" → server_error
//   - "connection refused", "no route to host", "connection reset" → network_error
//   - "timeout", "deadline exceeded", "timed out" → timeout
//   - "certificate", "ssl", "tls" → tls_error
//
// Falls back to a generic subprocess-error ProviderError wrapping the
// waitErr so callers still get a classified error rather than a raw
// fmt.Errorf.
func classifyProviderError(stderr string, waitErr error) *bridle.ProviderError {
	lower := strings.ToLower(stderr)

	// Auth failures — the dominant case. claude-code writes the synthetic
	// "Not logged in. Please run /login." response to stderr + exits 1
	// when ANTHROPIC_API_KEY is missing/unset/expired.
	if strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "authentication_failed") ||
		strings.Contains(lower, "run /login") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorAuthFailed,
			Message: "claude-code: authentication failed — aspect not authenticated to provider. Check ANTHROPIC_API_KEY or run /login",
			Err:     waitErr,
		}
	}

	if strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limited") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorRateLimit,
			Message: "claude-code: rate limited — provider throttled the request",
			Err:     waitErr,
		}
	}

	if strings.Contains(lower, "server_error") ||
		strings.Contains(lower, "internal server error") ||
		strings.Contains(lower, "overloaded") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorServerError,
			Message: "claude-code: provider server error — the API returned an internal error",
			Err:     waitErr,
		}
	}

	if strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "no route to host") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "eof") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorNetworkError,
			Message: "claude-code: network error connecting to provider",
			Err:     waitErr,
		}
	}

	if strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "timed out") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorTimeout,
			Message: "claude-code: request timed out",
			Err:     waitErr,
		}
	}

	if strings.Contains(lower, "certificate") ||
		strings.Contains(lower, "ssl") ||
		strings.Contains(lower, "tls") {
		return &bridle.ProviderError{
			Kind:    bridle.ProviderErrorTLSError,
			Message: "claude-code: TLS error connecting to provider",
			Err:     waitErr,
		}
	}

	return &bridle.ProviderError{
		Kind:    "subprocess_exit",
		Message: "claude-code: subprocess exited with error",
		Err:     waitErr,
	}
}
