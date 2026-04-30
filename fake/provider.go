// Package fake provides scripted test doubles for the bridle harness.
package fake

import (
	"context"
	"encoding/json"

	bridle "github.com/nexus-cw/bridle"
)

// Step describes one scripted action the fake provider should take.
type Step struct {
	// Text emitted as ModelChunk events (may be empty).
	Text string
	// ToolCalls to return to the harness for execution (may be nil).
	ToolCalls []bridle.ToolInvocation
	// StopReason for this step. Defaults to model_done if zero.
	StopReason bridle.StopReason
	// Err causes the provider to return this error instead of a result.
	Err error
}

// Provider is a scripted fake that replays a sequence of Steps.
// It does not call any model API; steps are popped in order on each RunTurn call.
type Provider struct {
	steps []Step
	pos   int
}

// NewProvider returns a fake provider that will replay the given steps.
func NewProvider(steps ...Step) *Provider {
	return &Provider{steps: steps}
}

func (p *Provider) Name() bridle.ProviderID { return "fake" }

// RunTurn pops the next scripted step and emits its events to sink.
func (p *Provider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	if p.pos >= len(p.steps) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	step := p.steps[p.pos]
	p.pos++

	if step.Err != nil {
		return bridle.ProviderResult{}, step.Err
	}

	if step.Text != "" {
		sink.Emit(bridle.ModelChunk{Text: step.Text})
	}

	stopReason := step.StopReason
	if stopReason == "" {
		if len(step.ToolCalls) > 0 {
			stopReason = bridle.StopReasonModelDone
		} else {
			stopReason = bridle.StopReasonModelDone
		}
	}

	// Build session delta.
	var delta []bridle.SessionEvent
	if step.Text != "" {
		delta = append(delta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			Content: step.Text,
		})
	}
	for _, tc := range step.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Args)
		delta = append(delta, bridle.SessionEvent{
			Role:    bridle.RoleAssistant,
			RawJSON: argsJSON,
		})
	}

	return bridle.ProviderResult{
		FinalText:    step.Text,
		ToolCalls:    step.ToolCalls,
		StopReason:   stopReason,
		SessionDelta: delta,
	}, nil
}

// StepsRemaining returns how many scripted steps have not yet been consumed.
func (p *Provider) StepsRemaining() int {
	remaining := len(p.steps) - p.pos
	if remaining < 0 {
		return 0
	}
	return remaining
}
