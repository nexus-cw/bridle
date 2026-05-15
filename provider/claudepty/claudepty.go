// Package claudepty implements the bridle Provider interface by spawning
// acp-claude-pty as a stdio subprocess and speaking ACP to it.
//
// Category: subprocess-stream. The acp-claude-pty binary spawns a real
// `claude` CLI inside a PTY and exposes the persistent REPL over ACP;
// bridle's claudepty.Provider is the ACP client side. Tool calls are
// owned by claude-code inside the PTY; bridle's ToolRunner is not
// invoked. BeforeToolCall does not fire; AfterToolCall fires once
// tool-use parsing lands on the binary side (currently inline as text).
//
// Lifecycle: one acp-claude-pty subprocess per Provider instance. The
// process holds a persistent claude REPL globally; bridle.Provider.RunTurn
// maps to one ACP Prompt request. Restart of the underlying claude REPL
// is the caller's concern (Provider.Close + Provider.start).
package claudepty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	bridle "github.com/CarriedWorldUniverse/bridle"
	acp "github.com/coder/acp-go-sdk"
)

const providerID = bridle.ProviderClaudePty

// Provider implements bridle.Provider by running acp-claude-pty as a
// stdio subprocess and speaking ACP to it.
//
// Provider is safe for concurrent RunTurn calls: the underlying ACP
// connection serializes prompts on the subprocess side via the binary's
// send-mutex.
type Provider struct {
	// BinaryPath is the path to the acp-claude-pty executable. Defaults
	// to "acp-claude-pty" (PATH lookup).
	BinaryPath string
	// ExtraArgs are appended verbatim to the binary invocation, after
	// the driver-required --cwd argument.
	ExtraArgs []string

	mu         sync.Mutex
	cmd        *exec.Cmd
	conn       *acp.ClientSideConnection
	cli        *sinkClient
	sessionID  acp.SessionId
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	startedCwd string
}

// New returns a claudepty Provider with default settings.
func New() *Provider { return &Provider{BinaryPath: "acp-claude-pty"} }

func (p *Provider) Name() bridle.ProviderID { return providerID }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategorySubprocessStream,
		SupportsCustomTools:    false,
		SupportsBeforeToolCall: false,
		SupportsAfterToolCall:  false,
		SupportsMCP:            false,
	}
}

// RunTurn maps one bridle turn onto one ACP Prompt request against the
// acp-claude-pty subprocess. The user-visible content of the latest
// message is sent as a text content block; assistant message chunks
// streamed back are emitted as bridle.ModelChunk events.
//
// The subprocess is spawned lazily on first RunTurn (per-Provider) and
// is reused across subsequent calls. Callers tearing down the Provider
// should invoke Close.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if err := p.ensureStarted(ctx, req.Cwd); err != nil {
		return p.errorResult(sink, "start", err)
	}

	prompt := buildPromptText(req)
	if prompt == "" {
		return p.errorResult(sink, "build-prompt", errors.New("claudepty: empty prompt"))
	}

	resp, err := p.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: p.sessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	if err != nil {
		return p.errorResult(sink, "prompt", err)
	}

	// Final text is accumulated by the SessionUpdate callback on the
	// sink-shim Client; pull it out via the shim's drain method.
	p.mu.Lock()
	cli := p.cli
	p.mu.Unlock()
	if cli == nil {
		return p.errorResult(sink, "drain", errors.New("claudepty: nil client after prompt"))
	}
	finalText := cli.drainText()
	if finalText != "" {
		sink.Emit(bridle.ModelChunk{Text: finalText})
	}
	result := bridle.ProviderResult{
		FinalText:  finalText,
		StepCount:  1,
		StopReason: stopReasonFor(resp.StopReason),
	}
	sink.Emit(bridle.TurnDone{Result: bridleTurnResultFromProvider(result)})
	return result, nil
}

// Close terminates the acp-claude-pty subprocess if running.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shutdownLocked()
}

// ensureStarted spawns the subprocess + initializes the ACP session if
// not already running. If a previous start used a different cwd, the
// subprocess is restarted in the new cwd — acp-claude-pty is bound to a
// single spawn dir for its lifetime.
func (p *Provider) ensureStarted(ctx context.Context, cwd string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd != nil && cwd != "" && cwd != p.startedCwd {
		if err := p.shutdownLocked(); err != nil {
			return err
		}
	}
	if p.cmd != nil {
		return nil
	}

	args := []string{"--cwd", cwd}
	args = append(args, p.ExtraArgs...)

	cmd := exec.CommandContext(ctx, p.BinaryPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("claudepty: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("claudepty: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("claudepty: start subprocess: %w", err)
	}

	cli := newSinkClient()
	conn := acp.NewClientSideConnection(cli, stdin, stdout)

	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	}); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("claudepty: initialize: %w", err)
	}

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("claudepty: new session: %w", err)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.stdout = stdout
	p.conn = conn
	p.cli = cli
	p.sessionID = session.SessionId
	p.startedCwd = cwd
	return nil
}

// shutdownLocked tears down the subprocess. Caller holds p.mu.
func (p *Provider) shutdownLocked() error {
	if p.cmd == nil {
		return nil
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	p.cmd = nil
	p.stdin = nil
	p.stdout = nil
	p.conn = nil
	p.cli = nil
	p.sessionID = ""
	p.startedCwd = ""
	return nil
}

// errorResult emits TurnError and returns a ProviderResult with
// StopReasonError. Centralizes the error-emission pattern so callers
// don't forget the sink.
func (p *Provider) errorResult(sink bridle.EventSink, stage string, err error) (bridle.ProviderResult, error) {
	sink.Emit(bridle.TurnError{Err: err, Stage: stage})
	return bridle.ProviderResult{StopReason: bridle.StopReasonError}, err
}

// buildPromptText extracts the user-visible text content from the
// provider request. We use only the last user message — the acp-claude-pty
// binary holds the persistent REPL state, so prior turns are implicit in
// the server-side session, not the prompt.
func buildPromptText(req bridle.ProviderRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return strings.TrimSpace(req.Messages[i].Content)
		}
	}
	return ""
}

// stopReasonFor maps an ACP StopReason to a bridle StopReason.
func stopReasonFor(r acp.StopReason) bridle.StopReason {
	switch r {
	case acp.StopReasonEndTurn:
		return bridle.StopReasonModelDone
	case acp.StopReasonCancelled:
		return bridle.StopReasonAborted
	case acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		return bridle.StopReasonMaxSteps
	case acp.StopReasonRefusal:
		return bridle.StopReasonError
	default:
		return bridle.StopReasonModelDone
	}
}

// bridleTurnResultFromProvider lifts a ProviderResult into a TurnResult
// for the TurnDone event. The harness normally does this lifting itself;
// subprocess-stream providers that emit TurnDone directly need to do it
// manually.
func bridleTurnResultFromProvider(r bridle.ProviderResult) bridle.TurnResult {
	return bridle.TurnResult{
		FinalText:    r.FinalText,
		ToolCalls:    r.ToolCalls,
		StepCount:    r.StepCount,
		Usage:        r.Usage,
		StopReason:   r.StopReason,
		SessionDelta: r.SessionDelta,
	}
}
