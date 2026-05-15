package claudepty

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
	acp "github.com/coder/acp-go-sdk"
)

// mockAgent is a minimal acp.Agent that drives the client side through
// a single Prompt → SessionUpdate → PromptResponse cycle. Initialize and
// NewSession are wired so the client's start sequence completes.
type mockAgent struct {
	conn     *acp.AgentSideConnection
	response string
}

func (m *mockAgent) Initialize(ctx context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}
func (m *mockAgent) Authenticate(ctx context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (m *mockAgent) NewSession(ctx context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId("mock-session")}, nil
}
func (m *mockAgent) Cancel(ctx context.Context, _ acp.CancelNotification) error { return nil }
func (m *mockAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	_ = m.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText(m.response),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}
func (m *mockAgent) SetSessionMode(ctx context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (m *mockAgent) ListSessions(ctx context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (m *mockAgent) ResumeSession(ctx context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (m *mockAgent) CloseSession(ctx context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (m *mockAgent) SetSessionConfigOption(ctx context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

// connectMock wires a sinkClient (the same shim Provider uses) directly
// to a mockAgent via in-memory pipes, bypassing the subprocess. This
// lets the test verify the client-side event mapping in isolation.
func connectMock(t *testing.T, response string) (*sinkClient, *acp.ClientSideConnection, func()) {
	t.Helper()

	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	cli := newSinkClient()
	csc := acp.NewClientSideConnection(cli, clientToAgentW, agentToClientR)

	ag := &mockAgent{response: response}
	asc := acp.NewAgentSideConnection(ag, agentToClientW, clientToAgentR)
	ag.conn = asc

	cleanup := func() {
		_ = clientToAgentW.Close()
		_ = agentToClientW.Close()
	}
	return cli, csc, cleanup
}

func TestProvider_NameAndCapabilities(t *testing.T) {
	p := New()
	if p.Name() != bridle.ProviderClaudePty {
		t.Errorf("Name = %q, want %q", p.Name(), bridle.ProviderClaudePty)
	}
	caps := p.Capabilities()
	if caps.Category != bridle.CategorySubprocessStream {
		t.Errorf("Category = %q, want %q", caps.Category, bridle.CategorySubprocessStream)
	}
	if caps.SupportsCustomTools {
		t.Error("SupportsCustomTools should be false")
	}
}

func TestBuildPromptText_UsesLatestUserMessage(t *testing.T) {
	req := bridle.ProviderRequest{
		Messages: []bridle.ProviderMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "reply"},
			{Role: "user", Content: "  second  "},
		},
	}
	got := buildPromptText(req)
	if got != "second" {
		t.Errorf("buildPromptText = %q, want %q", got, "second")
	}
}

func TestBuildPromptText_EmptyOnNoUserMessage(t *testing.T) {
	req := bridle.ProviderRequest{
		Messages: []bridle.ProviderMessage{
			{Role: "assistant", Content: "only assistant"},
		},
	}
	if got := buildPromptText(req); got != "" {
		t.Errorf("buildPromptText = %q, want empty", got)
	}
}

func TestStopReasonFor_AllVariants(t *testing.T) {
	cases := []struct {
		in   acp.StopReason
		want bridle.StopReason
	}{
		{acp.StopReasonEndTurn, bridle.StopReasonModelDone},
		{acp.StopReasonCancelled, bridle.StopReasonAborted},
		{acp.StopReasonMaxTokens, bridle.StopReasonMaxSteps},
		{acp.StopReasonMaxTurnRequests, bridle.StopReasonMaxSteps},
		{acp.StopReasonRefusal, bridle.StopReasonError},
		{acp.StopReason("unknown"), bridle.StopReasonModelDone},
	}
	for _, tc := range cases {
		if got := stopReasonFor(tc.in); got != tc.want {
			t.Errorf("stopReasonFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSinkClient_AccumulatesAndDrains(t *testing.T) {
	cli, csc, cleanup := connectMock(t, "hello from mock\n")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := csc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := csc.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := csc.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	got := cli.drainText()
	if !strings.Contains(got, "hello from mock") {
		t.Errorf("drainText = %q, want to contain 'hello from mock'", got)
	}

	// Second drain returns empty.
	if again := cli.drainText(); again != "" {
		t.Errorf("second drain = %q, want empty", again)
	}
}

func TestProvider_EmitsModelChunkAndTurnDone_AgainstMock(t *testing.T) {
	// This exercises Provider's RunTurn end-to-end against an in-memory
	// mock by short-circuiting the subprocess. We construct the Provider
	// state by hand rather than calling ensureStarted.
	cli, csc, cleanup := connectMock(t, "mock response text")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := csc.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := csc.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	p := &Provider{
		// Pre-fill cmd with a sentinel exec.Cmd so ensureStarted's
		// already-started short-circuit fires; the in-memory conn/cli
		// replace the would-be subprocess plumbing.
		cmd:        &exec.Cmd{},
		conn:       csc,
		cli:        cli,
		sessionID:  sess.SessionId,
		startedCwd: t.TempDir(),
	}

	sink := &fake.SliceEventSink{}
	req := bridle.ProviderRequest{
		Messages: []bridle.ProviderMessage{{Role: "user", Content: "drive turn"}},
	}
	result, err := p.RunTurn(ctx, req, sink)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.FinalText != "mock response text" {
		t.Errorf("FinalText = %q, want %q", result.FinalText, "mock response text")
	}
	if result.StopReason != bridle.StopReasonModelDone {
		t.Errorf("StopReason = %q, want %q", result.StopReason, bridle.StopReasonModelDone)
	}

	var sawChunk, sawDone bool
	for _, ev := range sink.Events {
		switch e := ev.(type) {
		case bridle.ModelChunk:
			if e.Text == "mock response text" {
				sawChunk = true
			}
		case bridle.TurnDone:
			sawDone = true
		}
	}
	if !sawChunk {
		t.Error("missing ModelChunk event")
	}
	if !sawDone {
		t.Error("missing TurnDone event")
	}
}
