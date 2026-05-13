package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

type fakeClient struct {
	converseResp       *bedrockruntime.ConverseOutput
	converseStreamResp *bedrockruntime.ConverseStreamOutput
	err                error
	lastStreamInput    *bedrockruntime.ConverseStreamInput
}

func (f *fakeClient) Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return f.converseResp, f.err
}

func (f *fakeClient) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	f.lastStreamInput = in
	return f.converseStreamResp, f.err
}

type recordingSink struct {
	events []bridle.Event
}

func (r *recordingSink) Emit(e bridle.Event) {
	r.events = append(r.events, e)
}

func TestToBedrockMessages_FoldsConsecutiveUserBlocks(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "assistant", ToolCalls: []bridle.ToolInvocation{
			{ID: "t1", Name: "f", Args: json.RawMessage(`{"x":1}`)},
		}},
		{Role: "tool_result", ToolCallID: "t1", Content: "ok"},
		{Role: "user", Content: "anything new?"},
	}
	out, err := toBedrockMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: assistant{tool_use}, user{tool_result + text} — two messages.
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
	if out[0].Role != types.ConversationRoleAssistant {
		t.Errorf("msg 0 role = %v, want assistant", out[0].Role)
	}
	if out[1].Role != types.ConversationRoleUser {
		t.Errorf("msg 1 role = %v, want user", out[1].Role)
	}
	if len(out[1].Content) != 2 {
		t.Errorf("user msg should have tool_result + text = 2 blocks, got %d", len(out[1].Content))
	}
}

func TestToBedrockMessages_ReconstructsAssistantToolUse(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "user", Content: "do thing"},
		{Role: "assistant", Content: "calling", ToolCalls: []bridle.ToolInvocation{
			{ID: "t1", Name: "do", Args: json.RawMessage(`{"a":2}`)},
		}},
	}
	out, err := toBedrockMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Role != types.ConversationRoleAssistant {
		t.Fatalf("expected assistant turn at index 1, got %+v", out)
	}
	if len(out[1].Content) != 2 {
		t.Errorf("assistant should have 2 blocks (text + tool_use), got %d", len(out[1].Content))
	}
	if _, ok := out[1].Content[1].(*types.ContentBlockMemberToolUse); !ok {
		t.Errorf("assistant block 1 should be ToolUse, got %T", out[1].Content[1])
	}
}

func TestToBedrockMessages_CachePointOnLastUser(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "user", Content: "hello"},
	}
	out, err := toBedrockMessages(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 message, got %d", len(out))
	}
	if len(out[0].Content) != 2 {
		t.Fatalf("user msg with caching should have text + cache_point = 2 blocks, got %d", len(out[0].Content))
	}
	if _, ok := out[0].Content[1].(*types.ContentBlockMemberCachePoint); !ok {
		t.Errorf("last content block should be CachePoint, got %T", out[0].Content[1])
	}
}

func TestToBedrockMessages_NoCachePointWhenDisabled(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "user", Content: "hello"},
	}
	out, _ := toBedrockMessages(msgs, false)
	if len(out[0].Content) != 1 {
		t.Errorf("caching disabled: want 1 block, got %d", len(out[0].Content))
	}
}

func TestToBedrockTools_ToolChoiceMapping(t *testing.T) {
	defs := []bridle.ToolDef{{Name: "f", Description: "f"}}

	cases := []struct {
		choice string
		want   any
	}{
		{"", (*types.ToolChoiceMemberAuto)(nil)},
		{"auto", (*types.ToolChoiceMemberAuto)(nil)},
		{"any", (*types.ToolChoiceMemberAny)(nil)},
		{"none", (*types.ToolChoiceMemberAuto)(nil)},
		{"specific_tool", (*types.ToolChoiceMemberTool)(nil)},
	}
	for _, c := range cases {
		cfg, err := toBedrockTools(defs, c.choice, false)
		if err != nil {
			t.Fatalf("choice %q: %v", c.choice, err)
		}
		if cfg == nil || cfg.ToolChoice == nil {
			t.Fatalf("choice %q: nil ToolChoice", c.choice)
		}
		// Compare concrete types via type assertion shape.
		switch c.want.(type) {
		case *types.ToolChoiceMemberAuto:
			if _, ok := cfg.ToolChoice.(*types.ToolChoiceMemberAuto); !ok {
				t.Errorf("choice %q: want Auto, got %T", c.choice, cfg.ToolChoice)
			}
		case *types.ToolChoiceMemberAny:
			if _, ok := cfg.ToolChoice.(*types.ToolChoiceMemberAny); !ok {
				t.Errorf("choice %q: want Any, got %T", c.choice, cfg.ToolChoice)
			}
		case *types.ToolChoiceMemberTool:
			if _, ok := cfg.ToolChoice.(*types.ToolChoiceMemberTool); !ok {
				t.Errorf("choice %q: want Tool, got %T", c.choice, cfg.ToolChoice)
			}
		}
	}
}

func TestToBedrockTools_CachePointAppended(t *testing.T) {
	defs := []bridle.ToolDef{{Name: "f", Description: "f"}}
	cfg, err := toBedrockTools(defs, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("want 2 tools (f + cache_point), got %d", len(cfg.Tools))
	}
	if _, ok := cfg.Tools[1].(*types.ToolMemberCachePoint); !ok {
		t.Errorf("last tool should be CachePoint, got %T", cfg.Tools[1])
	}
}

func TestProvider_ToolChoiceNone_DropsTools(t *testing.T) {
	// "none" contract: model must not call tools this turn. Bedrock has no
	// native "none", so RunTurn must drop req.Tools entirely instead of
	// sending tools + auto (which would let the model call them anyway).
	fc := &fakeClient{err: errors.New("stop after capture")}
	p := NewWithClient(fc)
	_, _ = p.RunTurn(context.Background(), bridle.ProviderRequest{
		Model:      "anthropic.claude-3-haiku-20240307-v1:0",
		ToolChoice: "none",
		Tools:      []bridle.ToolDef{{Name: "f", Description: "f"}},
		Messages:   []bridle.ProviderMessage{{Role: "user", Content: "hi"}},
	}, &recordingSink{})
	if fc.lastStreamInput == nil {
		t.Fatal("fake client did not capture ConverseStream input")
	}
	if fc.lastStreamInput.ToolConfig != nil {
		t.Errorf("ToolChoice=none should drop ToolConfig, got %+v", fc.lastStreamInput.ToolConfig)
	}
}

func TestProvider_StreamError(t *testing.T) {
	p := NewWithClient(&fakeClient{err: errors.New("network down")})
	sink := &recordingSink{}
	_, err := p.RunTurn(context.Background(), bridle.ProviderRequest{
		Model: "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []bridle.ProviderMessage{
			{Role: "user", Content: "hi"},
		},
	}, sink)
	if err == nil {
		t.Fatal("expected error")
	}
}
