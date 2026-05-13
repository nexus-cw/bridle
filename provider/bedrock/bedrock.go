// Package bedrock implements the bridle Provider interface for AWS Bedrock
// using the cross-model Converse API.
//
// Auth uses standard AWS SDK credential resolution: env vars, shared
// credentials file, instance role, etc. Honour the AWS_PROFILE / AWS_REGION
// environment variables in the spawning environment.
//
// Pricing-wise the cheapest practical models are amazon.nova-micro-v1:0 and
// amazon.nova-lite-v1:0 — fit for caretaker / interchange / watchdog work
// where Claude/Sonnet would be overkill.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

// converseClient is the minimal Bedrock client surface bedrock.Provider uses.
// Real callers get the concrete *bedrockruntime.Client; tests substitute a fake.
type converseClient interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// Provider implements bridle.Provider for AWS Bedrock via the Converse API.
//
// Concurrency: safe for use across goroutines. The internal client is built
// lazily and reused; getClient serializes init on p.mu.
type Provider struct {
	mu        sync.Mutex
	client    converseClient
	clientErr error // cached for permanent failures only; ctx errors are not cached
	region    string

	// Profile selects an AWS shared-config profile (overrides AWS_PROFILE if set).
	Profile string

	// Endpoint overrides the Bedrock service endpoint URL. Maps to the SDK's
	// BaseEndpoint option. Use for enterprise gateways that front Bedrock
	// with a corporate URL but still expect SigV4 signing. Leave empty for
	// the standard regional endpoint.
	Endpoint string

	// HTTPClient overrides the SDK's default HTTP transport. Use to inject
	// a corporate CA bundle, a proxy, or custom TLS for enterprise deploys.
	// Leave nil to use the SDK default.
	HTTPClient *http.Client

	// Inference parameters. All optional — zero values fall through to the
	// model's Bedrock default. MaxTokens defaults to 4096 if unset (matches
	// provider/claude/claude.go for Anthropic models).
	MaxTokens     int32    // 0 → 4096
	Temperature   *float32 // nil → model default
	TopP          *float32 // nil → model default
	StopSequences []string // empty → no caller-defined stop sequences

	// EnablePromptCaching, when true, emits CachePoint blocks at strategic
	// positions (after system prompt, after tool definitions, after each
	// tool_result batch) so Anthropic models on Bedrock can hit the prompt
	// cache. Bedrock supports up to 4 cache breakpoints; we stay within that.
	// Non-Anthropic models ignore cache points cleanly.
	EnablePromptCaching bool
}

// New returns a Bedrock provider. Region falls back to AWS_REGION env if empty.
// Credentials resolve via standard SDK chain (env, shared config, IAM role).
func New(region string) *Provider {
	return &Provider{region: region}
}

// NewWithClient returns a Bedrock provider using a pre-configured client.
// Use for advanced setups (custom credential providers, smithy middleware,
// non-SigV4 auth) where the constructor's Endpoint/HTTPClient fields are
// insufficient. The provided client must satisfy bridle's converseClient
// surface — concrete *bedrockruntime.Client does.
func NewWithClient(client converseClient) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() bridle.ProviderID { return bridle.ProviderBedrock }

func (p *Provider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

// getClient lazily initializes the Bedrock client. Concurrent RunTurn calls
// are serialized through p.mu; once the client is built, callers see it
// without contention via the early read path.
//
// Error caching policy: a permanent failure (bad profile, missing region with
// no fallback, etc.) is cached so subsequent callers fail fast. Context
// cancellation / deadline-exceeded errors are NOT cached — a transient ctx
// failure on the first call must not permanently brick a long-lived Provider.
func (p *Provider) getClient(ctx context.Context) (converseClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if p.region != "" {
		opts = append(opts, awsconfig.WithRegion(p.region))
	}
	if p.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p.Profile))
	}
	if p.HTTPClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(p.HTTPClient))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		wrapped := fmt.Errorf("bedrock: load aws config: %w", err)
		// Don't cache transient ctx failures — let the next caller retry.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			p.clientErr = wrapped
		}
		return nil, wrapped
	}
	clientOpts := []func(*bedrockruntime.Options){}
	if p.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *bedrockruntime.Options) {
			o.BaseEndpoint = aws.String(p.Endpoint)
		})
	}
	p.client = bedrockruntime.NewFromConfig(cfg, clientOpts...)
	return p.client, nil
}

// RunTurn calls the Bedrock Converse API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	messages, err := toBedrockMessages(req.Messages, p.EnablePromptCaching)
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	in := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(req.Model),
		Messages: messages,
	}

	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	in.InferenceConfig = &types.InferenceConfiguration{
		MaxTokens: aws.Int32(maxTokens),
	}
	if p.Temperature != nil {
		in.InferenceConfig.Temperature = p.Temperature
	}
	if p.TopP != nil {
		in.InferenceConfig.TopP = p.TopP
	}
	if len(p.StopSequences) > 0 {
		in.InferenceConfig.StopSequences = p.StopSequences
	}

	if req.AppendSystemPrompt != "" {
		in.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: req.AppendSystemPrompt},
		}
		if p.EnablePromptCaching {
			in.System = append(in.System, &types.SystemContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
	}
	// ToolChoice "none" means "no tools may be called this turn" per
	// the TurnRequest contract. Bedrock Converse has no native "none"
	// value, so honour the contract by dropping the tool list entirely
	// rather than sending tools+auto (which the model can still call).
	tools := req.Tools
	if req.ToolChoice == "none" {
		tools = nil
	}
	if toolCfg, err := toBedrockTools(tools, req.ToolChoice, p.EnablePromptCaching); err != nil {
		return bridle.ProviderResult{}, err
	} else if toolCfg != nil {
		in.ToolConfig = toolCfg
	}

	streamIn := &bedrockruntime.ConverseStreamInput{
		ModelId:         in.ModelId,
		Messages:        in.Messages,
		System:          in.System,
		InferenceConfig: in.InferenceConfig,
		ToolConfig:      in.ToolConfig,
	}
	streamOut, err := client.ConverseStream(ctx, streamIn)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("bedrock: ConverseStream: %w", err)
	}
	return extractStreamResult(ctx, streamOut, sink)
}

// extractStreamResult drains the ConverseStream event stream into a
// ProviderResult while emitting ModelChunk / ToolCallStart events to sink
// as they arrive. Event shapes per the Bedrock Converse streaming docs:
//
//   - MessageStart           — top-level role
//   - ContentBlockStart      — begin of a content block (text or tool_use)
//   - ContentBlockDelta      — partial text or partial tool_use input JSON
//   - ContentBlockStop       — end of a content block
//   - MessageStop            — top-level stop_reason
//   - Metadata               — usage + trace
//
// Each ContentBlock has an Index that ties Start/Delta/Stop together.
func extractStreamResult(ctx context.Context, out *bedrockruntime.ConverseStreamOutput, sink bridle.EventSink) (bridle.ProviderResult, error) {
	_ = ctx
	stream := out.GetStream()
	defer stream.Close()

	var (
		finalText    strings.Builder
		toolCalls    []bridle.ToolInvocation
		sessionDelta []bridle.SessionEvent
		usage        bridle.Usage
		rawStop      string
	)

	type blockState struct {
		kind      string // "text" | "tool_use"
		toolID    string
		toolName  string
		toolInput strings.Builder
		textBuf   strings.Builder
	}
	blocks := map[int32]*blockState{}

	for event := range stream.Events() {
		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs := &blockState{}
			if tu, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				bs.kind = "tool_use"
				bs.toolID = aws.ToString(tu.Value.ToolUseId)
				bs.toolName = aws.ToString(tu.Value.Name)
				sink.Emit(bridle.ToolCallStart{ID: bs.toolID, Name: bs.toolName})
			} else {
				bs.kind = "text"
			}
			blocks[idx] = bs

		case *types.ConverseStreamOutputMemberContentBlockDelta:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs, ok := blocks[idx]
			if !ok {
				bs = &blockState{kind: "text"}
				blocks[idx] = bs
			}
			switch d := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				sink.Emit(bridle.ModelChunk{Text: d.Value})
				finalText.WriteString(d.Value)
				bs.textBuf.WriteString(d.Value)
			case *types.ContentBlockDeltaMemberToolUse:
				bs.toolInput.WriteString(aws.ToString(d.Value.Input))
			}

		case *types.ConverseStreamOutputMemberContentBlockStop:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs, ok := blocks[idx]
			if !ok {
				continue
			}
			switch bs.kind {
			case "text":
				if bs.textBuf.Len() > 0 {
					sessionDelta = append(sessionDelta, bridle.SessionEvent{
						Provider: bridle.ProviderBedrock,
						Role:     bridle.RoleAssistant,
						Content:  bs.textBuf.String(),
					})
				}
			case "tool_use":
				args := json.RawMessage(bs.toolInput.String())
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				toolCalls = append(toolCalls, bridle.ToolInvocation{
					ID:   bs.toolID,
					Name: bs.toolName,
					Args: args,
				})
				raw, _ := json.Marshal(map[string]any{
					"toolUseId": bs.toolID,
					"name":      bs.toolName,
					"input":     args,
				})
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: bridle.ProviderBedrock,
					Role:     bridle.RoleAssistant,
					RawJSON:  raw,
				})
			}
			delete(blocks, idx)

		case *types.ConverseStreamOutputMemberMessageStop:
			rawStop = string(ev.Value.StopReason)

		case *types.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				usage.InputTokens = int(aws.ToInt32(ev.Value.Usage.InputTokens))
				usage.OutputTokens = int(aws.ToInt32(ev.Value.Usage.OutputTokens))
				if ev.Value.Usage.CacheReadInputTokens != nil {
					usage.CacheReadInputTokens = int(aws.ToInt32(ev.Value.Usage.CacheReadInputTokens))
				}
				if ev.Value.Usage.CacheWriteInputTokens != nil {
					usage.CacheCreationInputTokens = int(aws.ToInt32(ev.Value.Usage.CacheWriteInputTokens))
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: stream: %w", err)
	}

	if rawStop == string(types.StopReasonGuardrailIntervened) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: guardrail_intervened: response blocked by configured guardrail")
	}
	if rawStop == string(types.StopReasonContentFiltered) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: content_filtered: response blocked by content filter")
	}

	return bridle.ProviderResult{
		FinalText:    finalText.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		StopReason:   bridle.StopReason(normalize.BedrockStopReason(rawStop)),
		SessionDelta: sessionDelta,
	}, nil
}

// toBedrockMessages flattens bridle ProviderMessages into Bedrock Converse
// messages. Bedrock requires strict user/assistant alternation, where
// tool_result is a user content block. We accumulate user-role blocks
// (text + tool_result) into pendingUserBlocks and flush them as a single
// user message only when we hit an assistant turn or end of stream.
//
// Assistant turns are emitted with both text content and reconstructed
// tool_use blocks (from ProviderMessage.ToolCalls, populated by the
// harness in run.go's tool loop).
func toBedrockMessages(msgs []bridle.ProviderMessage, enableCache bool) ([]types.Message, error) {
	out := make([]types.Message, 0, len(msgs))
	var pendingUserBlocks []types.ContentBlock

	flushUser := func() {
		if len(pendingUserBlocks) == 0 {
			return
		}
		out = append(out, types.Message{
			Role:    types.ConversationRoleUser,
			Content: pendingUserBlocks,
		})
		pendingUserBlocks = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case "tool_result":
			pendingUserBlocks = append(pendingUserBlocks, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: aws.String(m.ToolCallID),
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: m.Content},
					},
				},
			})

		case "user", "system":
			// Both fold into a user content block. System is taken separately
			// via ConverseInput.System; any system in the message stream is
			// an inline context note from the harness.
			if m.Content != "" {
				pendingUserBlocks = append(pendingUserBlocks, &types.ContentBlockMemberText{Value: m.Content})
			}

		case "assistant":
			flushUser()
			blocks := []types.ContentBlock{}
			if m.Content != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if len(tc.Args) > 0 {
					if err := json.Unmarshal(tc.Args, &input); err != nil {
						return nil, fmt.Errorf("bedrock: tool_use %q args unmarshal: %w", tc.Name, err)
					}
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String(tc.ID),
						Name:      aws.String(tc.Name),
						Input:     document.NewLazyDocument(input),
					},
				})
			}
			if len(blocks) == 0 {
				continue // skip empty assistant turn rather than send invalid request
			}
			out = append(out, types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: blocks,
			})

		default:
			continue
		}
	}
	flushUser()

	if enableCache && len(out) > 0 {
		last := &out[len(out)-1]
		if last.Role == types.ConversationRoleUser {
			last.Content = append(last.Content, &types.ContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
	}
	return out, nil
}

func toBedrockTools(defs []bridle.ToolDef, choice string, enableCache bool) (*types.ToolConfiguration, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	tools := make([]types.Tool, 0, len(defs)+1)
	for _, d := range defs {
		spec := types.ToolSpecification{
			Name: aws.String(d.Name),
		}
		if d.Description != "" {
			spec.Description = aws.String(d.Description)
		}
		if len(d.InputSchema) > 0 {
			var schema any
			if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("bedrock: tool %q schema unmarshal: %w", d.Name, err)
			}
			spec.InputSchema = &types.ToolInputSchemaMemberJson{
				Value: document.NewLazyDocument(schema),
			}
		}
		tools = append(tools, &types.ToolMemberToolSpec{Value: spec})
	}
	if enableCache {
		tools = append(tools, &types.ToolMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
	}
	cfg := &types.ToolConfiguration{Tools: tools}
	switch choice {
	case "", "auto":
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
	case "any":
		cfg.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
	case "none":
		// Bedrock has no explicit "none" — return tools without a choice and
		// the model may still call one. For strict no-tools, the caller
		// should pass req.Tools = nil. Document and fall through to auto.
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
	default:
		cfg.ToolChoice = &types.ToolChoiceMemberTool{
			Value: types.SpecificToolChoice{Name: aws.String(choice)},
		}
	}
	return cfg, nil
}

