// Package ollama implements the bridle Provider interface for a local Ollama server.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/ollama/ollama/api"

	bridle "github.com/nexus-cw/bridle"
	"github.com/nexus-cw/bridle/internal/normalize"
)

const defaultBaseURL = "http://localhost:11434"

// Provider implements bridle.Provider for a local Ollama server.
type Provider struct {
	client  *api.Client
	baseURL string
}

// New returns an Ollama provider pointing at the default localhost:11434.
func New() *Provider {
	return &Provider{baseURL: defaultBaseURL}
}

// NewWithURL returns an Ollama provider pointing at a custom server URL.
func NewWithURL(baseURL string) *Provider {
	return &Provider{baseURL: baseURL}
}

// NewWithClient returns an Ollama provider using a pre-configured client.
func NewWithClient(client *api.Client) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() bridle.ProviderID { return bridle.ProviderOllama }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
	}
}

func (p *Provider) getClient() (*api.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	u, err := url.Parse(p.baseURL)
	if err != nil {
		return nil, fmt.Errorf("ollama: invalid base URL %q: %w", p.baseURL, err)
	}
	client := api.NewClient(u, http.DefaultClient)
	p.client = client
	return client, nil
}

// RunTurn calls the Ollama Chat API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	client, err := p.getClient()
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	messages := toOllamaMessages(req.Messages)
	tools := toOllamaTools(req.Tools)

	stream := false
	chatReq := &api.ChatRequest{
		Model:    req.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   &stream,
		Options:  map[string]any{},
	}
	if req.SystemPrompt != "" {
		chatReq.Messages = append([]api.Message{
			{Role: "system", Content: req.SystemPrompt},
		}, chatReq.Messages...)
	}

	var finalResp api.ChatResponse
	err = client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
		finalResp = resp
		if resp.Message.Content != "" {
			sink.Emit(bridle.ModelChunk{Text: resp.Message.Content})
		}
		return nil
	})
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("ollama: Chat error: %w", err)
	}

	return extractResult(finalResp), nil
}

func extractResult(resp api.ChatResponse) bridle.ProviderResult {
	var toolCalls []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent

	if resp.Message.Content != "" {
		sessionDelta = append(sessionDelta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			Content: resp.Message.Content,
		})
	}

	for _, tc := range resp.Message.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		id := tc.ID
		if id == "" {
			id = tc.Function.Name
		}
		toolCalls = append(toolCalls, bridle.ToolInvocation{
			ID:   id,
			Name: tc.Function.Name,
			Args: argsJSON,
		})
		raw, _ := json.Marshal(tc)
		sessionDelta = append(sessionDelta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			RawJSON: raw,
		})
	}

	stopReason := bridle.StopReason(normalize.OllamaStopReason(resp.DoneReason))

	return bridle.ProviderResult{
		FinalText: resp.Message.Content,
		ToolCalls: toolCalls,
		Usage: bridle.Usage{
			InputTokens:  resp.PromptEvalCount,
			OutputTokens: resp.EvalCount,
		},
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}
}

func toOllamaMessages(msgs []bridle.ProviderMessage) []api.Message {
	out := make([]api.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "user", "assistant", "system":
			out = append(out, api.Message{Role: m.Role, Content: m.Content})
		case "tool_result":
			out = append(out, api.Message{
				Role:       "tool",
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
			})
		}
	}
	return out
}

func toOllamaTools(defs []bridle.ToolDef) api.Tools {
	if len(defs) == 0 {
		return nil
	}
	tools := make(api.Tools, 0, len(defs))
	for _, d := range defs {
		fn := api.ToolFunction{
			Name:        d.Name,
			Description: d.Description,
		}
		if len(d.InputSchema) > 0 {
			var schema map[string]interface{}
			if err := json.Unmarshal(d.InputSchema, &schema); err == nil {
				if props, ok := schema["properties"]; ok {
					propsJSON, _ := json.Marshal(props)
					var propsMap map[string]api.ToolProperty
					if err := json.Unmarshal(propsJSON, &propsMap); err == nil {
						pm := api.NewToolPropertiesMap()
						for k, v := range propsMap {
							pm.Set(k, v)
						}
						fn.Parameters.Properties = pm
					}
				}
				if req, ok := schema["required"].([]interface{}); ok {
					for _, r := range req {
						if s, ok := r.(string); ok {
							fn.Parameters.Required = append(fn.Parameters.Required, s)
						}
					}
				}
			}
		}
		tools = append(tools, api.Tool{
			Type:     "function",
			Function: fn,
		})
	}
	return tools
}
