# bridle spec patch 1 — provider categories + claudecode

**Date:** 2026-05-01 (drafted same day as v0.1 spec, after stub-funnel validation)
**Patches:** `2026-05-01-bridle-spec.md` (v0.1)
**Driver:** anvil's stub-funnel validation surfaced a §3.3 mismatch with claude-code-headless. Operator (#8485, #8487, #8489, #8491) directed: support both direct API and headless-CLI paths; provider-internal mechanics stay inside bridle; no funnel changes.
**Empirical basis:** anvil #8493 — stream-json stdout carries every event bridle needs; on-disk JSONL persists independently for `--resume`.

---

## 1. What this patch changes

1. Introduces **provider categories** (§3a) — `direct-api` vs `subprocess-stream`. Documents which contract guarantees apply to each.
2. Adds a second Claude provider — `provider/claudecode` — alongside `provider/claude`. Public users get the API path; subscription users get the headless path. Both expose the same bridle contract to the funnel.
3. Refines the hooks table (§4.1) to be per-category — some hooks don't fire on subprocess-stream providers because the subprocess owns the tool loop.
4. Refines the tool surface section (§5) — on subprocess-stream providers, the CLI's externally-configured tool surface (mcphub) IS the funnel's tool surface; bridle's `ToolRunner` is observe-only.
5. Refines session semantics (§6) — funnel hands bridle an opaque `SessionHandle`; provider decides how state persists. Funnel owns the lifecycle (which handle binds to which thread), not the bytes.
6. Two minor clarifications from anvil's validation report:
   - `TurnRequest.Model` is required (was implicit; now explicit).
   - `SessionEvent.RawJSON` is provider-tagged so the funnel can route correctly when reading a tail back.

The funnel-facing contract (§2) is unchanged. Aspects don't see any of this.

---

## 2. New section §3a — Provider categories

### 2.1 Two categories

**`direct-api`.** Provider talks directly to a model API. bridle owns the tool loop: model emits tool calls, bridle invokes the funnel's `ToolRunner`, results go back to the model. Full contract per v0.1 §3.

Members: `provider/claude` (Anthropic API), `provider/ollama`, `provider/openai`.

**`subprocess-stream`.** Provider spawns an external subprocess that runs its own agentic loop and emits a structured event stream (typically newline-delimited JSON on stdout). bridle parses the stream into bridle events. The subprocess owns tool execution against an externally-configured tool surface (mcphub MCP servers, CLI-native tools, etc.).

Members: `provider/claudecode` (claude-code-headless).

### 2.2 What changes per category

| Aspect | direct-api | subprocess-stream |
|---|---|---|
| Tool execution | bridle's `ToolRunner` | subprocess's external surface |
| `BeforeToolCall` hook | fires | does not fire |
| `AfterToolCall` hook | fires | fires post-parse (observe-only) |
| Custom in-process tools | supported | not supported |
| Session storage | provider-internal (e.g. message-history threading) | subprocess's own persistence (e.g. `~/.claude/projects/.../<sessionId>.jsonl`) |
| `SessionTail` consumption | provider lowers it into the API request | provider passes a `SessionHandle` to the subprocess (e.g. `--resume <session-id>`) and lets the subprocess load history itself |

The funnel-facing bridle contract is identical. Behavior differences are documented per-category so the funnel can decide which provider to route a turn through (e.g., if it needs custom in-process tools, it picks a `direct-api` provider; if it wants subscription-bundled metering with mcphub-configured tools, it picks `subprocess-stream`).

### 2.3 Capability advertisement

```go
type ProviderCapabilities struct {
    Category               ProviderCategory // direct-api | subprocess-stream
    SupportsCustomTools    bool             // can the funnel pass arbitrary Tools?
    SupportsBeforeToolCall bool             // does BeforeToolCall fire?
    SupportsAfterToolCall  bool             // does AfterToolCall fire?
    // … extend as new dimensions surface
}

type Provider interface {
    Name() ProviderID
    Capabilities() ProviderCapabilities
    RunTurn(ctx context.Context, req providerRequest, sink EventSink) (providerResult, error)
}
```

Funnel reads `Capabilities()` once at startup (or per-dispatch) to decide routing. Fail loudly if the funnel hands a `direct-api`-only feature (e.g., a custom Tools array) to a `subprocess-stream` provider — silent degradation is the worse failure mode.

---

## 3. New provider — `provider/claudecode`

### 3.1 Mechanism

```
funnel calls bridle.RunTurn(req, sink)
  → provider/claudecode:
      ├── derives session-id from req.SessionHandle (new vs resume)
      ├── spawns: claude -p --output-format stream-json --verbose
      │           --session-id <uuid>   (first turn)
      │           --resume <uuid>       (subsequent turns)
      ├── reads stdout line-by-line
      ├── parses each JSON event:
      │     system.init   → noop (or sink.Emit a debug event)
      │     assistant.text          block → ModelChunk
      │     assistant.tool_use      block → ToolCallStart (with full input as Args)
      │                                   → AfterToolCall hook fires (observe-only)
      │     user.tool_result        block → ToolCallResult
      │                                   → StepBoundary on round transition
      │     result                  event → captures Usage + StopReason
      ├── waits for subprocess exit
      └── returns TurnResult populated from parsed events;
          SessionDelta carries the parsed events tagged with provider=claudecode.
```

### 3.2 Properties

- **No path-walking required.** Stream-json on stdout carries every event bridle needs (anvil #8493 verified).
- **`--resume` works for free.** The CLI persists its session JSONL to disk independently of output mode. bridle doesn't read that file; it just hands the same session-id back on the next turn.
- **Tool surface = mcphub.** The funnel configures the CLI's MCP servers (per-aspect plugin set, per #83) before dispatching turns. Whatever tools the CLI's runtime exposes ARE the funnel's tools. bridle's `ToolRunner` on this path is observe-only — `AfterToolCall` fires after parsing each `tool_use`/`tool_result` pair so the funnel can record/observe but cannot deny mid-call.
- **Errors:** subprocess exit non-zero → `TurnError{Stage: "subprocess_exit"}`. Malformed event line → log + skip (don't fail the turn for one bad line). Stream EOF before `result` event → `TurnError{Stage: "stream_truncated"}`.
- **Cancellation:** `ctx` cancel → SIGTERM the subprocess, wait briefly, SIGKILL if still alive. Return partial `TurnResult` with `StopReason = aborted`.

### 3.3 What it does NOT do

- Does not parse the on-disk JSONL. The CLI's session file is persistence, not an event source.
- Does not own session-id allocation. Funnel mints session-ids when starting new threads; bridle just threads them through.
- Does not configure mcphub. mcphub is a separate concern owned by the funnel.

---

## 4. Refinement: §4.1 hooks table per category

Replaces v0.1 §4.1 hook table with this per-category version:

| Hook | direct-api | subprocess-stream | Can do |
|---|---|---|---|
| `BeforeModelCall` | fires | fires (before subprocess spawn) | mutate request, abort turn |
| `AfterModelChunk` | fires | fires | observe, abort turn |
| `BeforeToolCall` | fires | **does not fire** (subprocess owns tool loop) | mutate args, deny call |
| `AfterToolCall` | fires (after `ToolRunner.Run`) | fires (after parsing tool_result event) | mutate result, abort turn |
| `OnStepBoundary` | fires | fires | observe, abort turn |
| `OnTurnDone` | fires | fires | mutate `TurnResult.SessionDelta` |

Funnel must check `Capabilities().SupportsBeforeToolCall` before relying on it. If a hook is registered but the provider doesn't fire it, that's not an error — it's a contract surface mismatch the funnel chose to ignore. We could make this a startup error instead; revisit if it bites.

---

## 5. Refinement: §5 tool surface

Insert after the "send_comms is just a tool" paragraph:

> **Subprocess-stream providers don't invoke bridle's `ToolRunner` at all.** On those paths, the subprocess executes tools against its externally-configured tool surface (e.g., the claude-code CLI's MCP-loaded tools per the mcphub pattern, #83). bridle observes the resulting tool calls in the subprocess's event stream and fires `AfterToolCall` for each one (observe-only), so the funnel can record/audit. `BeforeToolCall` does not fire — the subprocess has already executed the call by the time bridle sees it. The funnel's hook only sees what already happened; steering is at most a *next-turn* concern.
>
> **Custom in-process tools require `direct-api` providers.** If a funnel wants the model to call a Go function the funnel implemented (a `ToolDef` with no MCP backing), it must dispatch via a `direct-api` provider. `subprocess-stream` providers ignore custom Tools — the subprocess only sees the tool surface its runtime was configured with.

---

## 6. Refinement: §6 session semantics

Replaces v0.1 §6 with:

- **Funnel owns the session lifecycle.** Funnel mints `SessionHandle` values, maps them to threads, decides when handles get reused (resume) vs retired.
- **Provider owns session storage.** Funnel hands the provider a handle on each `RunTurn`; provider persists state however it needs to. The funnel is *not* the writer to a JSONL file the provider reads from. (This corrects v0.1's "funnel owns the JSONL file, single writer" — that was true for `direct-api` providers we'd build to consume an externally-supplied tail; it doesn't generalize to `subprocess-stream`.)
- **Funnel hands the harness a `SessionTail`** for `direct-api` providers — the recent events the provider should lower into its API request. For `subprocess-stream` providers, `SessionTail` is informational/auxiliary; the subprocess loads its own history via `--resume`. Provider decides what to do with whatever the funnel hands in.
- **Harness returns a `SessionDelta`** — the events from this turn, normalized to bridle's `SessionEvent` shape, **tagged with `Provider`** so the funnel knows what kind of `RawJSON` it's looking at.
- **Funnel decides whether to keep the delta** (the log-decision turn, per #81). If yes, funnel persists the delta to whatever store the funnel keeps. Provider-stored state (e.g., the CLI's on-disk JSONL) is the provider's, not the funnel's — funnel's persistence is its own observability/audit/history layer, not necessarily the same bytes the provider would read on resume.

```go
type SessionHandle struct {
    ID    string  // opaque to the funnel; meaningful to the provider
    // Provider-specific extension fields go in the provider via ID-keyed lookup.
}

type SessionEvent struct {
    Provider ProviderID      // who produced this event
    Kind     SessionEventKind // user_msg | assistant_text | tool_call | tool_result | system
    RawJSON  json.RawMessage  // provider-specific; only valid in conjunction with Provider
    // … Timestamp, etc.
}
```

When the funnel reads a `SessionTail` for redisplay/UI, it dispatches the `RawJSON` parsing through the relevant provider — bridle exposes a helper `ParseSessionEvent(SessionEvent) (NormalizedView, error)` so the funnel doesn't need to know each provider's wire format.

---

## 7. Refinement: §2.1 `TurnRequest`

Two changes:

1. **`Model` is required.** v0.1's comment said "provider-specific model id" but didn't enforce. Make it explicit: `RunTurn` returns `ErrModelRequired` if `Model == ""`. (anvil's stub-funnel hit `400 Bad Request: model is required` from Ollama because the log-decision-turn caller forgot to set it; surface that as a bridle-layer error before we hit the provider.)

2. **Add `SessionHandle`.** Replaces the unstructured assumption that `SessionTail` was the only session input.

```go
type TurnRequest struct {
    AspectID      string
    SystemPrompt  string
    Session       SessionHandle    // NEW: opaque-to-funnel handle for provider-side state
    SessionTail   []SessionEvent   // existing: tail for direct-api lower-into-request
    UserMessage   string
    Inbox         []InboxItem
    Tools         []ToolDef
    Provider      ProviderID
    Model         string           // REQUIRED — RunTurn errors if empty
    MaxSteps      int
}
```

For `direct-api` providers, `Session` may be empty (provider re-derives state from `SessionTail` per turn). For `subprocess-stream`, `Session.ID` IS the resume key.

---

## 8. Layout addition

Add to v0.1 §12 layout:

```
provider/
├── claude/         // direct-api Anthropic
├── claudecode/     // subprocess-stream claude-code-headless    ← NEW
├── ollama/         // direct-api ollama
└── openai/         // direct-api OpenAI
```

`provider/claudecode/` will need a stream-json parser. Reference:
- The agent-network harness already does this in JS — `code/harness/turn-stream.js` and `code/harness/session-tail.js` parse the same event format (the on-disk JSONL is the same format as stream-json — both are JSON-per-line of the CLI's event stream).
- Anvil can port the relevant logic to Go; the parsing is straightforward (well-known fields, no recursion into provider quirks).

---

## 9. What this patch does NOT change

- §2 funnel-facing contract (other than the two `TurnRequest` fields above).
- §7 triage integration.
- §8 error handling.
- §9 non-goals.
- §10 test surface — but adds: claudecode round-trip test against a live CLI, ollama and claude-api parity check that capability-advertisement matches reality.
- §11 reference patterns.
- §13 what anvil receives — anvil already has v0.1; this patch arrives on top.
- §14 out of scope.

---

## 10. Open questions (defer until forced)

- **Capability mismatch handling:** if a funnel registers `BeforeToolCall` but the chosen provider is `subprocess-stream` (which doesn't fire it), is that a startup error, a logged warning, or silently allowed? Defaulting to **logged warning** for v0.2; revisit if a real case demands stricter. **Security caveat:** if a funnel uses `BeforeToolCall` as a security gate (deny a tool call before it runs — e.g., refuse `bash` on a sensitive path), routing that funnel through a `subprocess-stream` provider silently downgrades the gate to post-facto observation. That's a real security hole, not a logging nuisance. Funnels with security-gating hooks must either (a) refuse to register against a `subprocess-stream` provider at all, or (b) explicitly opt into the downgrade. We're defaulting to logged warning for v0.2, but the gate-vs-observe distinction needs to be re-examined the moment the funnel grows a security policy.
- **`SessionHandle` provider-extension fields:** if a provider needs more than an `ID` (e.g., openai-style threads with their own ids on top of the model session), does the handle grow? For now, no — `ID` is enough for the providers we're building.
- **Custom-tool fallback on subprocess-stream:** could bridle synthesize a `ToolRunner`-call for tools the subprocess didn't see? E.g., model emits `<tool_call>foo</tool_call>` in text and bridle catches it, runs `ToolRunner.Run`, splices result into next turn's prompt. Possible but baroque; not worth it unless a real case demands.

---

## 11. Build order for anvil

1. Add `ProviderCategory` + `ProviderCapabilities` types; thread through existing `Provider` interface.
2. Add `SessionHandle` to `TurnRequest`. Make `Model` required at `RunTurn` boundary.
3. Tag `SessionDelta` events with `Provider`. Add `ParseSessionEvent` helper (delegates to per-provider parser).
4. Build `provider/claudecode/` — spawn, stream-json parse, event mapping, exit handling, cancellation. Add round-trip test against the local CLI.
5. Update `fake/` to support both categories (a fake subprocess-stream provider that scripts a stdout JSON sequence is useful for funnel-side capability tests).
6. Update stub-funnel to exercise both Claude paths in sequence. Confirm the bridle contract still holds uniformly from the funnel's view (modulo the documented per-category hook differences).

Same iteration discipline as v0.1: ship when validated, polish second.
