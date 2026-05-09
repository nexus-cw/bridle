package fake

import (
	"context"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// SubprocessStep describes one scripted event the SubprocessProvider emits.
// It mirrors what the claudecode provider would surface after parsing the CLI stream.
type SubprocessStep struct {
	// Text emitted as a ModelChunk (may be empty).
	Text string
	// ToolCallStart events emitted before ToolCallResults (optional).
	ToolCalls []bridle.ToolCallStart
	// ToolResults paired with ToolCalls by index. Must be same length as ToolCalls.
	ToolResults []bridle.ToolCallResult
	// StopReason for this step.
	StopReason bridle.StopReason
	// Err causes the provider to return this error.
	Err error
}

// SubprocessProvider is a fake subprocess-stream provider. It replays scripted
// SubprocessSteps without spawning any process. Useful for testing funnel-side
// behavior against a provider that does NOT support BeforeToolCall or custom tools.
type SubprocessProvider struct {
	steps []SubprocessStep
	pos   int
}

// NewSubprocessProvider returns a fake subprocess-stream provider that replays steps.
func NewSubprocessProvider(steps ...SubprocessStep) *SubprocessProvider {
	return &SubprocessProvider{steps: steps}
}

func (p *SubprocessProvider) Name() bridle.ProviderID { return "fake-subprocess" }

func (p *SubprocessProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategorySubprocessStream,
		SupportsCustomTools:    false,
		SupportsBeforeToolCall: false,
		SupportsAfterToolCall:  true,
		SupportsMCP:            false,
	}
}

// RunTurn pops the next scripted step and emits its events to sink.
// Tool calls are emitted as ToolCallStart+ToolCallResult pairs (subprocess owns execution).
// The harness does NOT call ToolRunner on this path.
func (p *SubprocessProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if p.pos >= len(p.steps) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	step := p.steps[p.pos]
	p.pos++

	if step.Err != nil {
		return bridle.ProviderResult{}, step.Err
	}

	var finalText string
	var toolInvocations []bridle.ToolInvocation
	var sessionDelta []bridle.SessionEvent
	stepCount := 0

	if step.Text != "" {
		sink.Emit(bridle.ModelChunk{Text: step.Text})
		finalText = step.Text
		sessionDelta = append(sessionDelta, bridle.SessionEvent{
			Provider: "fake-subprocess",
			Role:     bridle.RoleAssistant,
			Content:  step.Text,
		})
	}

	for i, tc := range step.ToolCalls {
		sink.Emit(tc)
		var result bridle.ToolCallResult
		if i < len(step.ToolResults) {
			result = step.ToolResults[i]
		} else {
			result = bridle.ToolCallResult{ID: tc.ID}
		}
		sink.Emit(result)
		toolInvocations = append(toolInvocations, bridle.ToolInvocation{
			ID:     tc.ID,
			Name:   tc.Name,
			Args:   tc.Args,
			Result: result.Result,
			Err:    result.Err,
		})
		stepCount++
		sink.Emit(bridle.StepBoundary{Step: stepCount})
	}

	stopReason := step.StopReason
	if stopReason == "" {
		stopReason = bridle.StopReasonModelDone
	}

	return bridle.ProviderResult{
		FinalText:    finalText,
		ToolCalls:    toolInvocations,
		StepCount:    stepCount,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
}

// StepsRemaining returns how many scripted steps have not yet been consumed.
func (p *SubprocessProvider) StepsRemaining() int {
	remaining := len(p.steps) - p.pos
	if remaining < 0 {
		return 0
	}
	return remaining
}
