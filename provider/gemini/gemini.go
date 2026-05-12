// Package gemini implements the bridle Provider interface for the Google
// Gemini API (Gemini Developer API or Vertex AI), via google.golang.org/genai.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/genai"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

// Provider implements bridle.Provider for Google Gemini.
//
// One Provider value is safe for concurrent use across goroutines.
// The genai client is constructed lazily on first RunTurn (or via
// NewWithClient) and reused for the lifetime of the Provider; init is
// guarded by clientOnce so concurrent first-callers don't race.
type Provider struct {
	clientOnce sync.Once
	clientErr  error // captured by clientOnce for re-raising to subsequent callers
	client     *genai.Client
	apiKey     string
	backend    genai.Backend
}

// New returns a Gemini provider that talks to the Gemini Developer API.
// If apiKey is empty, the GEMINI_API_KEY (or GOOGLE_API_KEY) environment
// variable is used by the underlying SDK.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey, backend: genai.BackendGeminiAPI}
}

// NewVertex returns a Gemini provider configured for Vertex AI.
// Credentials are picked up from the ambient Google Cloud environment
// (GOOGLE_APPLICATION_CREDENTIALS / ADC).
func NewVertex() *Provider {
	return &Provider{backend: genai.BackendVertexAI}
}

// NewWithClient returns a Gemini provider using a pre-configured client.
func NewWithClient(client *genai.Client) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() bridle.ProviderID { return bridle.ProviderGemini }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

// getClient returns the lazily-constructed genai client. Concurrent
// callers serialize on clientOnce; the first caller's success or
// failure is replayed to all subsequent callers. If NewWithClient was
// used, p.client is non-nil before getClient is ever called and
// clientOnce is effectively a no-op.
func (p *Provider) getClient(ctx context.Context) (*genai.Client, error) {
	p.clientOnce.Do(func() {
		if p.client != nil {
			return // injected via NewWithClient — nothing to do
		}
		cfg := &genai.ClientConfig{Backend: p.backend}
		if p.apiKey != "" {
			cfg.APIKey = p.apiKey
		}
		c, err := genai.NewClient(ctx, cfg)
		if err != nil {
			p.clientErr = fmt.Errorf("gemini: client init: %w", err)
			return
		}
		p.client = c
	})
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	return p.client, nil
}

// RunTurn calls the Gemini GenerateContent API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	contents := toGeminiContents(req.Messages)

	cfg := &genai.GenerateContentConfig{}
	if req.AppendSystemPrompt != "" {
		cfg.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: req.AppendSystemPrompt}},
		}
	}
	if tools := toGeminiTools(req.Tools); len(tools) > 0 {
		cfg.Tools = tools
	}

	resp, err := client.Models.GenerateContent(ctx, req.Model, contents, cfg)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("gemini: API error: %w", err)
	}

	return extractResult(resp, sink)
}

func extractResult(resp *genai.GenerateContentResponse, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if resp == nil || len(resp.Candidates) == 0 {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}

	cand := resp.Candidates[0]
	var finalText string
	var toolCalls []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent

	if cand.Content != nil {
		for _, part := range cand.Content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				sink.Emit(bridle.ModelChunk{Text: part.Text})
				finalText += part.Text
				raw, _ := json.Marshal(map[string]any{"text": part.Text})
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: bridle.ProviderGemini,
					Role:     bridle.RoleAssistant,
					Content:  part.Text,
					RawJSON:  raw,
				})
			}
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				id := part.FunctionCall.ID
				if id == "" {
					id = part.FunctionCall.Name
				}
				toolCalls = append(toolCalls, bridle.ToolInvocation{
					ID:   id,
					Name: part.FunctionCall.Name,
					Args: argsJSON,
				})
				raw, _ := json.Marshal(map[string]any{
					"functionCall": map[string]any{
						"name": part.FunctionCall.Name,
						"args": json.RawMessage(argsJSON),
					},
				})
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: bridle.ProviderGemini,
					Role:     bridle.RoleAssistant,
					RawJSON:  raw,
				})
			}
		}
	}

	stopReason := bridle.StopReason(normalize.GeminiStopReason(string(cand.FinishReason)))

	usage := bridle.Usage{}
	if resp.UsageMetadata != nil {
		usage.InputTokens = int(resp.UsageMetadata.PromptTokenCount)
		usage.OutputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}

	return bridle.ProviderResult{
		FinalText:    finalText,
		ToolCalls:    toolCalls,
		Usage:        usage,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

func toGeminiContents(msgs []bridle.ProviderMessage) []*genai.Content {
	out := make([]*genai.Content, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "user", "system":
			out = append(out, &genai.Content{
				Role:  "user",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case "assistant":
			out = append(out, &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case "tool_result":
			// Gemini's FunctionResponse requires Name to match the
			// FunctionDeclaration.Name of the called function — not the
			// call instance ID. Earlier drafts set Name = ToolCallID
			// which causes the API to reject multi-turn tool
			// conversations with "function call id not found" or
			// silently misroute the response. ProviderMessage.ToolName
			// carries the declaration name (set by the harness when it
			// builds the tool_result message); fall back to ToolCallID
			// only as a last-ditch defense — that path will fail at the
			// API but at least the failure is visible.
			name := m.ToolName
			if name == "" {
				name = m.ToolCallID
			}
			out = append(out, &genai.Content{
				Role: "user",
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						ID:       m.ToolCallID,
						Name:     name,
						Response: map[string]any{"result": m.Content},
					},
				}},
			})
		}
	}
	return out
}

func toGeminiTools(defs []bridle.ToolDef) []*genai.Tool {
	if len(defs) == 0 {
		return nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(defs))
	for _, d := range defs {
		fd := &genai.FunctionDeclaration{
			Name:        d.Name,
			Description: d.Description,
		}
		if len(d.InputSchema) > 0 {
			var schema genai.Schema
			if err := json.Unmarshal(d.InputSchema, &schema); err == nil {
				fd.Parameters = &schema
			}
		}
		decls = append(decls, fd)
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}
