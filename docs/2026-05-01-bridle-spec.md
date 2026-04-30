# bridle — Build Spec

**Date:** 2026-05-01
**Status:** Draft for anvil — build brief
**Name:** bridle (chosen 2026-05-01 — operator confirmed; cross-aspect consensus 5/6)
**Repo:** `nexus-cw/bridle/`
**Depends on:** `2026-04-30-hand-dispatch-v0_1.md` (funnel/harness model), §5.6 hooks (Frame role spec), §5.7 triage
**Reference reading:** PydanticAI (`iter()` / `CallToolsNode`), Eino (streaming + tool-call normalization), PI (headless JSON streaming), Strands (`register_hooks`), LangGraph (interrupt/checkpoint).

---

## 1. Purpose

A small, focused Go library that owns the **harness layer** of the funnel/harness split: turning a single deliberation turn (a prompt + tool surface + session JSONL fragment) into a stream of model-driven steps, exposing every intermediate event to the funnel, and returning a structured turn-result. It is **the AI-specific adapter** — the shared infrastructure (deliberation loop, comms surface, triage, session, idle state) lives one layer up, in the funnel.

Build it as a Go library with a stable provider interface and N implementations: `claude-api`, `ollama-local`, `openai-api`. The funnel imports it; aspects do not import it directly.

**Non-purpose.** This is not a framework, not an agent runtime, not a session store. It does one thing: drive *one turn* of one model with one tool surface and emit observable events.

---

## 2. Funnel ↔ harness contract

### 2.1 Input — `TurnRequest`

```go
type TurnRequest struct {
    // Identity & framing
    AspectID    string          // who's running (for cost/triage/identity attribution)
    SystemPrompt string         // composed by funnel: NEXUS.md + SOUL.md + PRIMER + harness rules
    SessionTail []SessionEvent  // funnel-owned JSONL fragment to seed model context

    // This turn
    UserMessage  string         // the prompt that opens this turn (may be empty for autonomous)
    Inbox        []InboxItem    // mid-turn comms accumulated since last turn (see §3.4)

    // Tool surface
    Tools        []ToolDef      // tools the model may call this turn (incl. send_comms)

    // Provider
    Provider     ProviderID     // claude-api | ollama-local | openai-api | ...
    Model        string         // provider-specific model id
    MaxSteps     int            // hard cap on tool-call rounds inside this turn
}
```

### 2.2 Output — streamed events + final `TurnResult`

The harness emits events as the turn unfolds; the funnel subscribes. Events are not optional — observability is the whole point of the split.

```go
type Event interface{ event() }

type ModelChunk      struct{ Text string }                       // streamed text
type ToolCallStart   struct{ ID, Name string; Args json.RawMessage }
type ToolCallResult  struct{ ID string; Result json.RawMessage; Err string }
type StepBoundary    struct{ Step int }                          // between tool-call rounds
type TurnDone        struct{ Result TurnResult }
type TurnError       struct{ Err error; Stage string }           // never panic across the boundary
```

```go
type TurnResult struct {
    FinalText    string         // the model's last assistant text (may be empty if turn ended in tool-only)
    ToolCalls    []ToolInvocation  // ordered, what the model actually did
    StepCount    int
    Usage        Usage          // tokens in/out, cost estimate, provider-reported
    StopReason   StopReason     // model_done | max_steps | error | aborted
    SessionDelta []SessionEvent // events to append to the funnel-owned JSONL
}
```

The funnel is the only writer to session JSONL. The harness *proposes* `SessionDelta`; the funnel decides whether to keep this turn (the **log-decision turn**, see §1 contract in v0.1 hand-dispatch spec).

### 2.3 Function shape

```go
func (h *Harness) RunTurn(ctx context.Context, req TurnRequest, sink EventSink) (TurnResult, error)
```

`EventSink` is `interface{ Emit(Event) }` — channel-backed in production, slice-backed in tests. Cancellation is via `ctx`; cancel mid-turn returns a partial `TurnResult` with `StopReason = aborted`.

---

## 3. Provider abstraction

One interface. N implementations. Provider-specific weirdness stays inside the implementation; the funnel sees a uniform stream.

### 3.1 Interface

```go
type Provider interface {
    Name() ProviderID
    RunTurn(ctx context.Context, req providerRequest, sink EventSink) (providerResult, error)
}
```

`providerRequest` is the harness-internal lowered form of `TurnRequest` — system prompt assembled, session tail flattened to provider's message format, tools translated to provider's schema (Claude tool_use, Ollama function-calling, OpenAI tools).

### 3.2 What providers must normalize

- **Tool-call shape.** Whatever the wire format (Claude `tool_use` blocks, OpenAI `function_call`, Ollama JSON), surface as uniform `ToolCallStart` / `ToolCallResult`. Eino's tool-call normalization is the reference pattern.
- **Streaming vs non-streaming.** Provider chooses internally. PI is headless JSON streaming; PydanticAI's `iter()` exposes per-node intercepts in non-streaming. Either is fine — the *event stream out* is uniform regardless.
- **Step boundary.** Emit `StepBoundary` between tool-call rounds. A "step" is one round of model-emits-tool-calls → harness-runs-tools → results-back-to-model.
- **Stop reasons.** Map provider stop-reasons to the harness's `StopReason` enum.

### 3.3 What providers MUST NOT do

- Own session state. Funnel owns JSONL.
- Decide turn boundaries. Funnel decides when to start a new turn.
- Call comms tools directly. `send_comms` is a tool like any other — the funnel-supplied tool runner handles it.
- Retry on their own. Retries belong in the funnel (or one layer up), so retry policy is uniform across providers.

### 3.4 Inbox handling

The `Inbox` field carries comms that arrived *during* the previous turn. The harness folds them into the prompt context (provider-specific format) before the first model call. Inbox items are read-only from the harness's perspective — they're just structured context, not actions.

---

## 4. Hook surface (§5.6)

The harness exposes `register_hooks`-style interception points so the funnel can implement §5.6 behaviors (spend caps, content filtering, mid-turn steering) without forking the harness.

### 4.1 Hook points

| Hook                 | Fires                              | Can do |
|----------------------|------------------------------------|--------|
| `BeforeModelCall`    | Before each model invocation       | mutate request, abort turn |
| `AfterModelChunk`    | On each `ModelChunk` event         | observe, abort turn |
| `BeforeToolCall`     | Before tool runner executes a call | mutate args, deny call (returns error to model) |
| `AfterToolCall`      | After tool runner returns          | mutate result, abort turn |
| `OnStepBoundary`     | Between tool-call rounds           | observe, abort turn |
| `OnTurnDone`         | After turn completes               | mutate `TurnResult.SessionDelta` |

### 4.2 Hook signature

```go
type Hook[T any] func(ctx context.Context, in T) (T, HookAction, error)

type HookAction int
const (
    HookContinue HookAction = iota
    HookAbort                 // turn ends; partial TurnResult returned
)
```

Reference: Strands' `register_hooks` model. Same shape, simplified to what the §5.6 cases actually need. Don't over-build; add hook points only when a real call site demands one.

---

## 5. Tool surface

The harness invokes tools via a `ToolRunner` the funnel supplies. The harness does not own tool implementations.

```go
type ToolRunner interface {
    Run(ctx context.Context, call ToolCall) (json.RawMessage, error)
}
```

`send_comms` is just a tool. The funnel-supplied `ToolRunner` knows how to handle it (post to comms broker as the dispatching aspect). The harness has no special case for it — it's a `BeforeToolCall` → `Run` → `AfterToolCall` like any other tool.

This is the **#81 lock**: comms is a tool, callable mid-turn at any step boundary. The funnel's log-decision turn at the end of deliberation decides whether the turn becomes thread history.

---

## 6. Session semantics

- **Funnel owns the JSONL file.** Single writer; harness never opens it.
- **Funnel hands the harness a `SessionTail`** — typically the last N events, possibly compacted.
- **Harness returns a `SessionDelta`** — the events from this turn (user msg, tool calls, tool results, assistant text). Provider-format-agnostic: it's the harness's normalized form.
- **Funnel decides whether to keep the delta.** The log-decision turn (#81): one final cheap-model invocation that decides "does this turn become thread history?" If yes, funnel appends `SessionDelta`; if no, funnel discards.

The harness has no concept of "this turn was useful" — that's a funnel decision based on the log-decision turn output.

---

## 7. Triage integration (§5.7)

Triage runs **before** the funnel calls `RunTurn`. The harness sees only turns that triage decided to engage with. So:

- Triage is *not* in the harness library.
- Triage MAY use the harness (sidecar invocation: a separate `RunTurn` call against a cheap model with a triage-specific tool surface, in a sidecar session that's never written to the main JSONL).
- The harness must support being invoked with `SessionTail = nil` and a tiny synthesized prompt — that's all triage needs.

---

## 8. Error handling

- **Provider errors** (network, 5xx, rate-limit) → `TurnError` event + return error from `RunTurn`. Funnel decides retry.
- **Tool errors** (tool runner returned err) → emitted as `ToolCallResult{Err: ...}` to the model. Model decides what to do. Not a turn-fatal error.
- **Hook abort** → turn ends with `StopReason = aborted`. Partial `TurnResult` returned, no error.
- **Context cancel** → same as hook abort, `StopReason = aborted`.
- **Panic inside provider/tool** → trapped at harness boundary, returned as `TurnError{Stage: "..."}`. Never propagates to funnel.

This is layer 1 of the three-layer turn safety net (per v0.1 spec). Layer 2 (harness-layer crash trap) is the funnel's `defer recover()` around `RunTurn`. Layer 3 (dispatcher worker timeout) is in the dispatcher.

---

## 9. Non-goals

- **No streaming-mid-token to UI.** Token-level streaming inside a `ModelChunk` event is the provider's business; the harness emits whole chunks to the sink. UI streaming is a funnel concern (the funnel can chunk further or buffer).
- **No session ownership.** Stated above; restating because it's the most likely violation.
- **No internal task queue.** One `RunTurn` call = one turn. Concurrency is the funnel/dispatcher's problem.
- **No retry policy.** Stated in §3.3.
- **No multi-provider-in-one-turn.** A turn picks a provider and stays on it. Multi-provider routing is a funnel concern.
- **No prompt composition.** Funnel composes the system prompt; harness receives it pre-assembled.
- **No model-fallback chain.** If the chosen provider fails, the funnel decides what to do.

---

## 10. Test surface

The library should ship with:

1. **A fake provider** that scripts a sequence of events. Test funnel integration without hitting any model.
2. **A fake tool runner** that returns canned results. Test hook ordering and tool-call normalization.
3. **Round-trip tests per real provider** (claude-api, ollama-local, openai-api): one tool-using turn, one tool-less turn, one aborted turn.
4. **Hook ordering tests.** Each hook fires at the documented point; abort actually aborts.
5. **Cancellation tests.** Cancel mid-stream, mid-tool-call, between steps — all return clean `StopReason = aborted`.
6. **Provider normalization tests.** Same logical turn against each provider produces structurally equivalent event streams.

---

## 11. Reference patterns (read these before building)

| Source       | Pattern to lift                                          | Why |
|--------------|-----------------------------------------------------------|-----|
| PydanticAI   | `iter()` returning a node iterator; `CallToolsNode` intercept | Cleanest model for "let me see every step without forcing streaming" |
| Eino         | Tool-call normalization across providers                  | Reference for §3.2 normalization layer |
| PI (Princeton SWE-agent) | Headless JSON streaming output            | Reference for emitting structured events to a non-interactive consumer |
| Strands      | `register_hooks` registration model                       | Reference for §4 hook surface |
| LangGraph    | Interrupt + checkpoint                                    | Future-relevant for #82 aspect-task-list resume; not needed for v0.1 |

These are read-and-port references. We're building Go; we're not depending on any of them. Rationale (per terminalbench finding): model capability dominates harness sophistication. Keep the harness simple and let the model do the work.

---

## 12. Layout sketch

```
bridle/
├── README.md
├── go.mod
├── harness.go              // RunTurn + TurnRequest + TurnResult
├── events.go               // Event interface + concrete events
├── hooks.go                // hook types + registration
├── tool.go                 // ToolRunner interface + ToolCall types
├── session.go              // SessionEvent + SessionDelta types
├── provider/
│   ├── provider.go         // Provider interface
│   ├── claude/             // Claude API impl
│   ├── ollama/             // ollama-local impl
│   └── openai/             // OpenAI API impl
├── fake/
│   ├── provider.go         // scripted fake provider
│   └── tool_runner.go      // scripted fake tool runner
└── internal/
    └── normalize/          // tool-call/stop-reason/usage normalization helpers
```

---

## 13. What anvil receives

This document, plus:

- The v0.1 hand-dispatch spec (`2026-04-30-hand-dispatch-v0_1.md`) for funnel/harness model context.
- The Frame role spec (`2026-04-28-frame-role-spec.md`) for §5.6 hook semantics.
- A pointer to the five reference projects in §11 with permission to read their source.
- The repo name + path (pending #87 naming).
- A short orientation: "Build the library against the contract in §2–§9. Provider impls can land one at a time — start with `claude-api` since that's what every aspect uses today. ollama-local and openai-api can follow."

Open questions to resolve at build time, not now:

- Exact `SessionEvent` shape (compatible with funnel's existing JSONL — needs a small spec patch once the funnel lives).
- Tool schema translation details per provider (Claude tool_use ≠ OpenAI tools ≠ Ollama function-calling — straightforward mapping but tedious; build per-provider).
- Whether hooks compose by registration order or priority (default: registration order; revisit if a real case demands priority).

---

## 14. Out of scope for v0.1

These are deliberate omissions to keep v0.1 small. Land if/when a concrete need surfaces:

- Multi-turn batching (one library call running multiple turns).
- Cross-turn caching (provider-side prompt caching is fine; library-side caching is not v0.1).
- Speculative execution (run two providers in parallel, take fastest).
- Built-in observability sinks (prometheus, otel). Funnel does this; harness emits structured events and stops.
- Built-in cost ledger. `Usage` is reported; persistence is funnel's job.
