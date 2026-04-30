package bridle

import "context"

// HookAction tells the harness what to do after a hook returns.
type HookAction int

const (
	HookContinue HookAction = iota
	HookAbort                // end the turn; partial TurnResult returned with StopReason=aborted
)

// Hook is the generic hook signature. T is the mutable context value passed
// in and returned. Registration order is the execution order.
type Hook[T any] func(ctx context.Context, in T) (T, HookAction, error)

// BeforeModelCallCtx carries context passed to BeforeModelCall hooks.
type BeforeModelCallCtx struct {
	Request TurnRequest
	Step    int
}

// AfterModelChunkCtx carries context passed to AfterModelChunk hooks.
type AfterModelChunkCtx struct {
	Chunk ModelChunk
	Step  int
}

// BeforeToolCallCtx carries context passed to BeforeToolCall hooks.
type BeforeToolCallCtx struct {
	Call ToolCall
	Step int
}

// AfterToolCallCtx carries context passed to AfterToolCall hooks.
type AfterToolCallCtx struct {
	Call   ToolCall
	Result ToolCallResult
	Step   int
}

// OnStepBoundaryCtx carries context passed to OnStepBoundary hooks.
type OnStepBoundaryCtx struct {
	Step int
}

// OnTurnDoneCtx carries context passed to OnTurnDone hooks.
// Hooks may mutate SessionDelta before it is returned to the funnel.
type OnTurnDoneCtx struct {
	Result *TurnResult
}

// hookRegistry holds all registered hooks for a Harness instance.
type hookRegistry struct {
	beforeModelCall  []Hook[BeforeModelCallCtx]
	afterModelChunk  []Hook[AfterModelChunkCtx]
	beforeToolCall   []Hook[BeforeToolCallCtx]
	afterToolCall    []Hook[AfterToolCallCtx]
	onStepBoundary   []Hook[OnStepBoundaryCtx]
	onTurnDone       []Hook[OnTurnDoneCtx]
}

// RegisterBeforeModelCall adds a hook that fires before each model invocation.
func (h *Harness) RegisterBeforeModelCall(fn Hook[BeforeModelCallCtx]) {
	h.hooks.beforeModelCall = append(h.hooks.beforeModelCall, fn)
}

// RegisterAfterModelChunk adds a hook that fires on each ModelChunk event.
func (h *Harness) RegisterAfterModelChunk(fn Hook[AfterModelChunkCtx]) {
	h.hooks.afterModelChunk = append(h.hooks.afterModelChunk, fn)
}

// RegisterBeforeToolCall adds a hook that fires before each tool execution.
func (h *Harness) RegisterBeforeToolCall(fn Hook[BeforeToolCallCtx]) {
	h.hooks.beforeToolCall = append(h.hooks.beforeToolCall, fn)
}

// RegisterAfterToolCall adds a hook that fires after each tool execution.
func (h *Harness) RegisterAfterToolCall(fn Hook[AfterToolCallCtx]) {
	h.hooks.afterToolCall = append(h.hooks.afterToolCall, fn)
}

// RegisterOnStepBoundary adds a hook that fires between tool-call rounds.
func (h *Harness) RegisterOnStepBoundary(fn Hook[OnStepBoundaryCtx]) {
	h.hooks.onStepBoundary = append(h.hooks.onStepBoundary, fn)
}

// RegisterOnTurnDone adds a hook that fires after the turn completes.
// Hooks may mutate TurnResult.SessionDelta.
func (h *Harness) RegisterOnTurnDone(fn Hook[OnTurnDoneCtx]) {
	h.hooks.onTurnDone = append(h.hooks.onTurnDone, fn)
}

// runBeforeModelCall fires all BeforeModelCall hooks in registration order.
// Returns (updated ctx, aborted, error).
func (r *hookRegistry) runBeforeModelCall(ctx context.Context, hc BeforeModelCallCtx) (BeforeModelCallCtx, bool, error) {
	return runHooks(ctx, hc, r.beforeModelCall)
}

func (r *hookRegistry) runAfterModelChunk(ctx context.Context, hc AfterModelChunkCtx) (AfterModelChunkCtx, bool, error) {
	return runHooks(ctx, hc, r.afterModelChunk)
}

func (r *hookRegistry) runBeforeToolCall(ctx context.Context, hc BeforeToolCallCtx) (BeforeToolCallCtx, bool, error) {
	return runHooks(ctx, hc, r.beforeToolCall)
}

func (r *hookRegistry) runAfterToolCall(ctx context.Context, hc AfterToolCallCtx) (AfterToolCallCtx, bool, error) {
	return runHooks(ctx, hc, r.afterToolCall)
}

func (r *hookRegistry) runOnStepBoundary(ctx context.Context, hc OnStepBoundaryCtx) (OnStepBoundaryCtx, bool, error) {
	return runHooks(ctx, hc, r.onStepBoundary)
}

func (r *hookRegistry) runOnTurnDone(ctx context.Context, hc OnTurnDoneCtx) (OnTurnDoneCtx, bool, error) {
	return runHooks(ctx, hc, r.onTurnDone)
}

func runHooks[T any](ctx context.Context, in T, hooks []Hook[T]) (T, bool, error) {
	cur := in
	for _, h := range hooks {
		out, action, err := h(ctx, cur)
		if err != nil {
			return cur, false, err
		}
		cur = out
		if action == HookAbort {
			return cur, true, nil
		}
	}
	return cur, false, nil
}
