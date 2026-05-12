// Package openai implements the bridle Provider interface for the OpenAI API.
package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

// Provider implements bridle.Provider for the OpenAI API.
type Provider struct {
	client *openai.Client
	apiKey string
}

// New returns an OpenAI provider.
// If apiKey is empty, the OPENAI_API_KEY environment variable is used.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey}
}

// NewWithClient returns an OpenAI provider using a pre-configured client.
func NewWithClient(client *openai.Client) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() bridle.ProviderID { return bridle.ProviderOpenAI }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

func (p *Provider) getClient() *openai.Client {
	if p.client != nil {
		return p.client
	}
	if p.apiKey != "" {
		c := openai.NewClient(option.WithAPIKey(p.apiKey))
		p.client = &c
	} else {
		c := openai.NewClient()
		p.client = &c
	}
	return p.client
}

// RunTurn calls the OpenAI Chat Completions API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	messages := toOpenAIMessages(req.AppendSystemPrompt, req.Messages)
	tools := toOpenAITools(req.Tools)

	params := openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: messages,
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	completion, err := p.getClient().Chat.Completions.New(ctx, params)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("openai: API error: %w", err)
	}

	return extractResult(completion, sink)
}

func extractResult(completion *openai.ChatCompletion, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if len(completion.Choices) == 0 {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}

	choice := completion.Choices[0]
	msg := choice.Message

	var finalText string
	var toolCalls []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent

	if msg.Content != "" {
		sink.Emit(bridle.ModelChunk{Text: msg.Content})
		finalText = msg.Content
		sessionDelta = append(sessionDelta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			Content: msg.Content,
		})
	}

	for _, tc := range msg.ToolCalls {
		argsJSON := json.RawMessage(tc.Function.Arguments)
		toolCalls = append(toolCalls, bridle.ToolInvocation{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: argsJSON,
		})
		raw, _ := json.Marshal(tc)
		sessionDelta = append(sessionDelta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			RawJSON: raw,
		})
	}

	stopReason := bridle.StopReason(normalize.OpenAIStopReason(string(choice.FinishReason)))

	return bridle.ProviderResult{
		FinalText: finalText,
		ToolCalls: toolCalls,
		Usage: bridle.Usage{
			InputTokens:  int(completion.Usage.PromptTokens),
			OutputTokens: int(completion.Usage.CompletionTokens),
		},
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

func toOpenAIMessages(systemPrompt string, msgs []bridle.ProviderMessage) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion

	if systemPrompt != "" {
		out = append(out, openai.SystemMessage(systemPrompt))
	}

	for _, m := range msgs {
		switch m.Role {
		case "user":
			out = append(out, openai.UserMessage(m.Content))
		case "assistant":
			out = append(out, openai.AssistantMessage(m.Content))
		case "tool_result":
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		case "system":
			out = append(out, openai.SystemMessage(m.Content))
		}
	}
	return out
}

func toOpenAITools(defs []bridle.ToolDef) []openai.ChatCompletionToolParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(defs))
	for _, d := range defs {
		fn := shared.FunctionDefinitionParam{
			Name: d.Name,
		}
		if d.Description != "" {
			fn.Description = openai.String(d.Description)
		}
		if len(d.InputSchema) > 0 {
			var params shared.FunctionParameters
			if err := json.Unmarshal(d.InputSchema, &params); err == nil {
				fn.Parameters = params
			}
		}
		out = append(out, openai.ChatCompletionToolParam{
			Function: fn,
		})
	}
	return out
}
