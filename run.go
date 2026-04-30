package bridle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// runTurn is the inner implementation, called by RunTurn after the panic trap.
func (h *Harness) runTurn(ctx context.Context, req TurnRequest, runner ToolRunner, sink EventSink) (TurnResult, error) {
	// Lower TurnRequest → ProviderRequest.
	preq := lowerRequest(req)

	// BeforeModelCall hook (step 0 = the initial call).
	hc := BeforeModelCallCtx{Request: req, Step: 0}
	var aborted bool
	var herr error
	hc, aborted, herr = h.hooks.runBeforeModelCall(ctx, hc)
	if herr != nil {
		return partialAbort(), herr
	}
	if aborted {
		return partialAbort(), nil
	}

	var (
		allInvocations []ToolInvocation
		totalUsage     Usage
		stepCount      int
		finalText      string
		stopReason     StopReason
		sessionDelta   []SessionEvent
	)

	for {
		if ctx.Err() != nil {
			return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
		}

		// Run the provider turn.
		presult, err := h.provider.RunTurn(ctx, preq, sink)
		if err != nil {
			sink.Emit(TurnError{Err: err, Stage: "provider"})
			return TurnResult{
				FinalText:  finalText,
				ToolCalls:  allInvocations,
				StepCount:  stepCount,
				Usage:      totalUsage,
				StopReason: StopReasonError,
			}, err
		}

		finalText = presult.FinalText
		totalUsage = addUsage(totalUsage, presult.Usage)
		sessionDelta = append(sessionDelta, presult.SessionDelta...)

		// No tool calls → turn is done.
		if len(presult.ToolCalls) == 0 {
			stopReason = presult.StopReason
			break
		}

		// Execute each tool call.
		var toolMessages []ProviderMessage
		for _, inv := range presult.ToolCalls {
			call := ToolCall{ID: inv.ID, Name: inv.Name, Args: inv.Args}

			// BeforeToolCall hook.
			btc := BeforeToolCallCtx{Call: call, Step: stepCount + 1}
			btc, aborted, herr = h.hooks.runBeforeToolCall(ctx, btc)
			if herr != nil {
				return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), herr
			}
			if aborted {
				return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
			}
			call = btc.Call

			sink.Emit(ToolCallStart{ID: call.ID, Name: call.Name, Args: call.Args})

			if ctx.Err() != nil {
				return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
			}

			resultJSON, runErr := runner.Run(ctx, call)
			var toolErrStr string
			if runErr != nil {
				toolErrStr = runErr.Error()
				resultJSON = json.RawMessage(`null`)
			}

			tcr := ToolCallResult{ID: call.ID, Result: resultJSON, Err: toolErrStr}
			sink.Emit(tcr)

			// AfterToolCall hook.
			atc := AfterToolCallCtx{Call: call, Result: tcr, Step: stepCount + 1}
			atc, aborted, herr = h.hooks.runAfterToolCall(ctx, atc)
			if herr != nil {
				return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), herr
			}
			if aborted {
				return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
			}

			completed := ToolInvocation{
				ID:     call.ID,
				Name:   call.Name,
				Args:   call.Args,
				Result: atc.Result.Result,
				Err:    atc.Result.Err,
			}
			allInvocations = append(allInvocations, completed)

			resultStr := string(atc.Result.Result)
			if atc.Result.Err != "" {
				resultStr = fmt.Sprintf("error: %s", atc.Result.Err)
			}
			toolMessages = append(toolMessages, ProviderMessage{
				Role:       "tool_result",
				Content:    resultStr,
				ToolCallID: call.ID,
			})
			sessionDelta = append(sessionDelta, SessionEvent{Provider: h.provider.Name(), Role: RoleTool, Content: resultStr})
		}

		stepCount++

		// OnStepBoundary hook.
		sbc := OnStepBoundaryCtx{Step: stepCount}
		_, aborted, herr = h.hooks.runOnStepBoundary(ctx, sbc)
		if herr != nil {
			return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), herr
		}
		if aborted {
			return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
		}
		sink.Emit(StepBoundary{Step: stepCount})

		// MaxSteps guard.
		if req.MaxSteps > 0 && stepCount >= req.MaxSteps {
			stopReason = StopReasonMaxSteps
			break
		}

		// Append tool results to message history.
		preq.Messages = append(preq.Messages, toolMessages...)

		// BeforeModelCall hook for the next round.
		hc = BeforeModelCallCtx{Request: req, Step: stepCount}
		hc, aborted, herr = h.hooks.runBeforeModelCall(ctx, hc)
		if herr != nil {
			return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), herr
		}
		if aborted {
			return partialAbortWith(finalText, allInvocations, stepCount, totalUsage), nil
		}
		_ = hc
	}

	result := TurnResult{
		FinalText:    finalText,
		ToolCalls:    allInvocations,
		StepCount:    stepCount,
		Usage:        totalUsage,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}

	// OnTurnDone hook — may mutate SessionDelta.
	otd := OnTurnDoneCtx{Result: &result}
	h.hooks.runOnTurnDone(ctx, otd) //nolint:errcheck
	sink.Emit(TurnDone{Result: result})
	return result, nil
}

func lowerRequest(req TurnRequest) ProviderRequest {
	var messages []ProviderMessage

	for _, e := range req.SessionTail {
		messages = append(messages, ProviderMessage{
			Role:    string(e.Role),
			Content: e.Content,
		})
	}

	if len(req.Inbox) > 0 {
		content := "Messages received since last turn:\n"
		for _, item := range req.Inbox {
			content += fmt.Sprintf("[from %s]: %s\n", item.From, item.Content)
		}
		messages = append(messages, ProviderMessage{Role: "user", Content: content})
	}

	if req.UserMessage != "" {
		messages = append(messages, ProviderMessage{Role: "user", Content: req.UserMessage})
	}

	return ProviderRequest{
		AspectID:     req.AspectID,
		SystemPrompt: req.SystemPrompt,
		Messages:     messages,
		Tools:        req.Tools,
		MaxSteps:     req.MaxSteps,
		Model:        req.Model,
	}
}

func addUsage(a, b Usage) Usage {
	return Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		CostUSD:      a.CostUSD + b.CostUSD,
	}
}

func partialAbort() TurnResult {
	return TurnResult{StopReason: StopReasonAborted}
}

func partialAbortWith(text string, invocations []ToolInvocation, steps int, usage Usage) TurnResult {
	return TurnResult{
		FinalText:  text,
		ToolCalls:  invocations,
		StepCount:  steps,
		Usage:      usage,
		StopReason: StopReasonAborted,
	}
}

func panicErr(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return errors.New(fmt.Sprintf("panic: %v", r))
}
