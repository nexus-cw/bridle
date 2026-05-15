package claudepty

import (
	"context"
	"errors"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// sinkClient is the minimal acp.Client implementation that claudepty
// needs. It accumulates agent_message_chunk text deliveries from the
// server-side SessionUpdate notifications, and stubs every other Client
// method (the acp-claude-pty server does not currently issue tool calls
// or filesystem requests).
type sinkClient struct {
	mu   sync.Mutex
	text strings.Builder
}

func newSinkClient() *sinkClient { return &sinkClient{} }

// drainText returns the accumulated agent-message text and resets the
// buffer. Safe to call between Prompt invocations.
func (c *sinkClient) drainText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.text.String()
	c.text.Reset()
	return out
}

// SessionUpdate is the only Client method that does real work: it
// accumulates agent_message_chunk text into the buffer. Other update
// kinds are ignored for now (tool calls, plans, etc. become meaningful
// once the parser-tier work on acp-claude-pty grows past the
// bracketed-effect placeholder).
func (c *sinkClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	if n.Update.AgentMessageChunk == nil {
		return nil
	}
	if t := n.Update.AgentMessageChunk.Content.Text; t != nil {
		c.mu.Lock()
		c.text.WriteString(t.Text)
		c.mu.Unlock()
	}
	return nil
}

// --- Client surface stubs ---

var errNotImplemented = errors.New("claudepty: client method not implemented")

func (c *sinkClient) ReadTextFile(ctx context.Context, _ acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errNotImplemented
}
func (c *sinkClient) WriteTextFile(ctx context.Context, _ acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errNotImplemented
}
func (c *sinkClient) RequestPermission(ctx context.Context, _ acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errNotImplemented
}
func (c *sinkClient) CreateTerminal(ctx context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNotImplemented
}
func (c *sinkClient) KillTerminal(ctx context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNotImplemented
}
func (c *sinkClient) TerminalOutput(ctx context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNotImplemented
}
func (c *sinkClient) ReleaseTerminal(ctx context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNotImplemented
}
func (c *sinkClient) WaitForTerminalExit(ctx context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNotImplemented
}
