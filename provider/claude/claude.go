// Package claude implements the bridle Provider interface for the Anthropic Claude API.
package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	bridle "github.com/nexus-cw/bridle"
	"github.com/nexus-cw/bridle/internal/normalize"
)

// Provider implements bridle.Provider for the Anthropic Claude API.
type Provider struct {
	client *anthropic.Client
	apiKey string
}

// New returns a Claude provider.
// If apiKey is empty, the ANTHROPIC_API_KEY environment variable is used.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey}
}

// NewWithClient returns a Claude provider using a pre-configured client.
func NewWithClient(client *anthropic.Client) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() bridle.ProviderID { return bridle.ProviderClaude }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
	}
}

func (p *Provider) getClient() *anthropic.Client {
	if p.client != nil {
		return p.client
	}
	if p.apiKey != "" {
		c := anthropic.NewClient(option.WithAPIKey(p.apiKey))
		p.client = &c
	} else {
		c := anthropic.NewClient()
		p.client = &c
	}
	return p.client
}

// RunTurn calls the Claude Messages API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	messages, err := toClaudeMessages(req.Messages)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claude: message conversion: %w", err)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		System:    toClaudeSystem(req.SystemPrompt),
		Messages:  messages,
		MaxTokens: 4096,
	}

	tools := toClaudeTools(req.Tools)
	if len(tools) > 0 {
		params.Tools = tools
	}

	msg, err := p.getClient().Messages.New(ctx, params)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("claude: API error: %w", err)
	}

	return extractResult(msg, sink)
}

func extractResult(msg *anthropic.Message, sink bridle.EventSink) (bridle.ProviderResult, error) {
	var finalText string
	var toolCalls []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent

	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			sink.Emit(bridle.ModelChunk{Text: b.Text})
			finalText += b.Text
			sessionDelta = append(sessionDelta, bridle.SessionEvent{
				Role:    bridle.RoleAssistant,
				Content: b.Text,
			})

		case anthropic.ToolUseBlock:
			toolCalls = append(toolCalls, bridle.ToolInvocation{
				ID:   b.ID,
				Name: b.Name,
				Args: b.Input,
			})
			raw, _ := json.Marshal(b)
			sessionDelta = append(sessionDelta, bridle.SessionEvent{
				Role:    bridle.RoleAssistant,
				RawJSON: raw,
			})
		}
	}

	stopReason := bridle.StopReason(normalize.ClaudeStopReason(string(msg.StopReason)))

	return bridle.ProviderResult{
		FinalText: finalText,
		ToolCalls: toolCalls,
		Usage: bridle.Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

func toClaudeMessages(msgs []bridle.ProviderMessage) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	for _, m := range msgs {
		switch m.Role {
		case "user":
			out = append(out, anthropic.NewUserMessage(
				anthropic.NewTextBlock(m.Content),
			))
		case "assistant":
			out = append(out, anthropic.NewAssistantMessage(
				anthropic.NewTextBlock(m.Content),
			))
		case "tool_result":
			out = append(out, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false),
			))
		case "system":
			// System tail events folded into a user message as context.
			out = append(out, anthropic.NewUserMessage(
				anthropic.NewTextBlock("[system context] "+m.Content),
			))
		}
	}
	return out, nil
}

func toClaudeSystem(prompt string) []anthropic.TextBlockParam {
	if prompt == "" {
		return nil
	}
	return []anthropic.TextBlockParam{{Text: prompt}}
}

func toClaudeTools(defs []bridle.ToolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		schema := anthropic.ToolInputSchemaParam{}
		if len(d.InputSchema) > 0 {
			var props interface{}
			if err := json.Unmarshal(d.InputSchema, &props); err == nil {
				schema.Properties = props
			}
		}
		out = append(out, anthropic.ToolUnionParamOfTool(schema, d.Name))
		// Description is on ToolParam, set via the variant directly.
		if d.Description != "" && len(out) > 0 {
			if out[len(out)-1].OfTool != nil {
				out[len(out)-1].OfTool.Description = anthropic.String(d.Description)
			}
		}
	}
	return out
}
