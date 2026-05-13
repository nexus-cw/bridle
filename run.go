package bridle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/bridle/internal/mcpclient"
)

// runTurn is the inner implementation, called by RunTurn after the panic trap.
func (h *Harness) runTurn(ctx context.Context, req TurnRequest, runner ToolRunner, sink EventSink) (TurnResult, error) {
	// Connect MCP servers and merge tool surface (direct-api providers only).
	var mcpClient *mcpclient.Client
	caps := h.provider.Capabilities()
	if caps.SupportsMCP && req.MCP != nil {
		specs := lowerMCPConfig(req.MCP)
		var err error
		mcpClient, err = mcpclient.Connect(ctx, specs)
		if err != nil {
			return TurnResult{StopReason: StopReasonError}, err
		}
		defer mcpClient.Close()

		mcpTools := mcpClient.Tools()
		merged, err := mergeToolSurface(req.Tools, mcpTools)
		if err != nil {
			return TurnResult{StopReason: StopReasonError}, err
		}
		req.Tools = merged
	}

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

			var resultJSON json.RawMessage
			var runErr error
			if mcpClient != nil && mcpClient.IsMCPTool(call.Name) {
				resultJSON, runErr = mcpClient.Call(ctx, call.Name, call.Args)
			} else {
				resultJSON, runErr = runner.Run(ctx, call)
			}
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
				ToolName:   call.Name, // required by Gemini's FunctionResponse contract; ignored by other providers
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

		// Reconstruct the assistant turn that emitted those tool_use blocks
		// before appending the tool_results. Bedrock (and strict providers)
		// require assistant{tool_use} → user{tool_result} alternation; sending
		// tool_results without the preceding assistant turn is rejected.
		// finalText may be empty for tool-only assistant turns — that's fine,
		// providers emit a content-less assistant message with just tool_use.
		preq.Messages = append(preq.Messages, ProviderMessage{
			Role:      "assistant",
			Content:   finalText,
			ToolCalls: presult.ToolCalls,
		})

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
		// Format inbox items with their msg_id so the model can call
		// triage(msg_id=...) for each one. Without the id in the prompt
		// the model has nothing to reference and the triage contract
		// can't bind. Items with MsgID==0 are synthetic/internal and
		// don't participate in triage.
		content := "Messages received since last turn:\n"
		var triagedIDs []int64
		for _, item := range req.Inbox {
			if item.MsgID > 0 {
				content += fmt.Sprintf("[msg_id=%d from=%s]: %s\n", item.MsgID, item.From, item.Content)
				triagedIDs = append(triagedIDs, item.MsgID)
			} else {
				content += fmt.Sprintf("[from %s]: %s\n", item.From, item.Content)
			}
		}
		// Triage requirement only fires when the funnel registered the
		// triage + send_chat tools (req.Tools non-empty). claude-code-
		// backed funnels run with nil Tools because the subprocess owns
		// its tool surface natively (see #181); telling the model to
		// call triage() when triage isn't callable just makes it loop
		// trying to find the tool and refuse the request. The triage
		// contract is a feature of direct-API providers; subprocess-
		// stream paths surface replies via the auto-post path instead.
		hasTriageTool := false
		for _, t := range req.Tools {
			if t.Name == "triage" {
				hasTriageTool = true
				break
			}
		}
		if len(triagedIDs) > 0 && hasTriageTool {
			content += "\n## Triage requirement\n"
			content += "You MUST call triage(msg_id, decision, reason) once for EVERY chat msg_id above before this turn ends.\n"
			content += "  - decision=\"reply\" if you used send_chat to address that msg_id (cite it via reply_to or in-content reference)\n"
			content += "  - decision=\"skip\" with a reason for any message you intentionally do not reply to\n"
			content += "Skip reasons: addressed_to_other, acknowledgement_only, out_of_scope, duplicate, noise, or a freeform sentence.\n"
			content += "msg_ids requiring triage this turn: "
			for i, id := range triagedIDs {
				if i > 0 {
					content += ", "
				}
				content += fmt.Sprintf("%d", id)
			}
			content += "\n"
		}
		messages = append(messages, ProviderMessage{Role: "user", Content: content})
	}

	if req.UserMessage != "" {
		messages = append(messages, ProviderMessage{Role: "user", Content: req.UserMessage})
	}

	return ProviderRequest{
		AspectID:     req.AspectID,
		AppendSystemPrompt: req.AppendSystemPrompt,
		Session:      req.Session,
		Messages:     messages,
		Tools:        req.Tools,
		ToolChoice:   req.ToolChoice,
		MCP:          req.MCP,
		MaxSteps:     req.MaxSteps,
		Model:        req.Model,
		Cwd:          req.Cwd,
	}
}

func addUsage(a, b Usage) Usage {
	return Usage{
		InputTokens:              a.InputTokens + b.InputTokens,
		OutputTokens:             a.OutputTokens + b.OutputTokens,
		CacheReadInputTokens:     a.CacheReadInputTokens + b.CacheReadInputTokens,
		CacheCreationInputTokens: a.CacheCreationInputTokens + b.CacheCreationInputTokens,
		CostUSD:                  a.CostUSD + b.CostUSD,
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

// lowerMCPConfig converts a bridle MCPClientConfig to the internal mcpclient ServerSpec slice.
func lowerMCPConfig(cfg *MCPClientConfig) []mcpclient.ServerSpec {
	if cfg == nil {
		return nil
	}
	specs := make([]mcpclient.ServerSpec, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		specs = append(specs, mcpclient.ServerSpec{
			Name:      s.Name,
			Transport: mcpclient.Transport(s.Transport),
			Command:   s.Command,
			URL:       s.URL,
			Env:       s.Env,
			Header:    s.Header,
		})
	}
	return specs
}

// mergeToolSurface merges explicit ToolDefs with MCP-loaded ToolDefs, checking
// for name collisions. Returns ErrToolNameCollision on a duplicate name.
func mergeToolSurface(explicit []ToolDef, mcpTools []mcpclient.ToolDef) ([]ToolDef, error) {
	seen := make(map[string]struct{}, len(explicit))
	for _, t := range explicit {
		seen[t.Name] = struct{}{}
	}
	merged := make([]ToolDef, len(explicit), len(explicit)+len(mcpTools))
	copy(merged, explicit)
	for _, t := range mcpTools {
		if _, dup := seen[t.Name]; dup {
			return nil, ErrToolNameCollision
		}
		seen[t.Name] = struct{}{}
		merged = append(merged, ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return merged, nil
}
