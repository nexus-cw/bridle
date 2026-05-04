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
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/internal/normalize"
)

// Provider implements bridle.Provider for AWS Bedrock via the Converse API.
type Provider struct {
	client *bedrockruntime.Client
	region string
	// Profile selects an AWS shared-config profile (overrides AWS_PROFILE if set).
	Profile string
}

// New returns a Bedrock provider. Region falls back to AWS_REGION env if empty.
// Credentials resolve via standard SDK chain (env, shared config, IAM role).
func New(region string) *Provider {
	return &Provider{region: region}
}

// NewWithClient returns a Bedrock provider using a pre-configured client.
func NewWithClient(client *bedrockruntime.Client) *Provider {
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

func (p *Provider) getClient(ctx context.Context) (*bedrockruntime.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if p.region != "" {
		opts = append(opts, awsconfig.WithRegion(p.region))
	}
	if p.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p.Profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config: %w", err)
	}
	p.client = bedrockruntime.NewFromConfig(cfg)
	return p.client, nil
}

// RunTurn calls the Bedrock Converse API and emits bridle events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	messages, err := toBedrockMessages(req.Messages)
	if err != nil {
		return bridle.ProviderResult{}, err
	}

	in := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(req.Model),
		Messages: messages,
	}
	if req.AppendSystemPrompt != "" {
		in.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: req.AppendSystemPrompt},
		}
	}
	if toolCfg, err := toBedrockTools(req.Tools); err != nil {
		return bridle.ProviderResult{}, err
	} else if toolCfg != nil {
		in.ToolConfig = toolCfg
	}

	resp, err := client.Converse(ctx, in)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("bedrock: Converse: %w", err)
	}

	return extractResult(resp, sink)
}

func extractResult(resp *bedrockruntime.ConverseOutput, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if resp == nil || resp.Output == nil {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}

	msgWrap, ok := resp.Output.(*types.ConverseOutputMemberMessage)
	if !ok || msgWrap == nil {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	msg := msgWrap.Value

	var finalText string
	var toolCalls []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent

	for _, block := range msg.Content {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			sink.Emit(bridle.ModelChunk{Text: b.Value})
			finalText += b.Value
			sessionDelta = append(sessionDelta, bridle.SessionEvent{
				Provider: bridle.ProviderBedrock,
				Role:     bridle.RoleAssistant,
				Content:  b.Value,
			})

		case *types.ContentBlockMemberToolUse:
			argsJSON, _ := documentToJSON(b.Value.Input)
			id := aws.ToString(b.Value.ToolUseId)
			name := aws.ToString(b.Value.Name)
			toolCalls = append(toolCalls, bridle.ToolInvocation{
				ID:   id,
				Name: name,
				Args: argsJSON,
			})
			raw, _ := json.Marshal(map[string]any{
				"toolUseId": id,
				"name":      name,
				"input":     json.RawMessage(argsJSON),
			})
			sessionDelta = append(sessionDelta, bridle.SessionEvent{
				Provider: bridle.ProviderBedrock,
				Role:     bridle.RoleAssistant,
				RawJSON:  raw,
			})
		}
	}

	usage := bridle.Usage{}
	if resp.Usage != nil {
		usage.InputTokens = int(aws.ToInt32(resp.Usage.InputTokens))
		usage.OutputTokens = int(aws.ToInt32(resp.Usage.OutputTokens))
	}

	stopReason := bridle.StopReason(normalize.BedrockStopReason(string(resp.StopReason)))

	return bridle.ProviderResult{
		FinalText:    finalText,
		ToolCalls:    toolCalls,
		Usage:        usage,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

func toBedrockMessages(msgs []bridle.ProviderMessage) ([]types.Message, error) {
	out := make([]types.Message, 0, len(msgs))
	for _, m := range msgs {
		var role types.ConversationRole
		var blocks []types.ContentBlock
		switch m.Role {
		case "user", "system":
			// Converse takes system separately via the System field; if a
			// ProviderMessage sneaks in with role=system, fold it into a user
			// turn rather than dropping it.
			role = types.ConversationRoleUser
			blocks = []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}}
		case "assistant":
			role = types.ConversationRoleAssistant
			blocks = []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}}
		case "tool_result":
			role = types.ConversationRoleUser
			blocks = []types.ContentBlock{
				&types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String(m.ToolCallID),
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{Value: m.Content},
						},
					},
				},
			}
		default:
			continue
		}
		out = append(out, types.Message{Role: role, Content: blocks})
	}
	return out, nil
}

func toBedrockTools(defs []bridle.ToolDef) (*types.ToolConfiguration, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	tools := make([]types.Tool, 0, len(defs))
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
	return &types.ToolConfiguration{Tools: tools}, nil
}

// documentToJSON marshals a smithy document.Interface back to JSON bytes.
// Used for tool-call inputs which arrive as opaque documents on the wire.
func documentToJSON(d document.Interface) (json.RawMessage, error) {
	if d == nil {
		return json.RawMessage("{}"), nil
	}
	var v any
	if err := d.UnmarshalSmithyDocument(&v); err != nil {
		return nil, fmt.Errorf("bedrock: tool input unmarshal: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("bedrock: tool input marshal: %w", err)
	}
	return b, nil
}
