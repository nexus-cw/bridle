# Stub Funnel Validation Report — bridle v0.1

**Date:** 2026-05-01  
**Validated by:** anvil

## What was tested

The stub funnel exercises bridle's contract under a funnel-shaped caller:
- Deliberation loop calling `RunTurn` repeatedly
- Inbox folding (synthetic comms item into first turn's context)
- `send_comms`, `now`, `read_file` tool surface via bridle's `ToolRunner`
- `SessionTail` / `SessionDelta` round-tripped through an in-memory tail + temp JSONL file
- Hook surface: `BeforeToolCall`, `OnStepBoundary` registered and firing
- Log-decision turn at the end

## Claude path (claude-code headless)

**Provider:** `provider/claudecode/` — shells out to `claude -p --output-format stream-json --verbose`  
**Model:** `claude-haiku-4-5-20251001`

### What works
- Text-only deliberation turn: `ModelChunk` events emitted correctly, `TurnDone` fires, session delta (assistant text) written to JSONL
- Stop reason normalization: `end_turn` → `model_done`
- Usage tokens reported in final result event

### What doesn't work / findings

**SPEC MISMATCH — §3.3 violation (subprocess provider):**  
The claude-code CLI manages its own tool loop internally. bridle's `ToolRunner` is never called on this path — the CLI executes tools itself. This means:
- `BeforeToolCall` / `AfterToolCall` hooks don't fire for custom tools
- `ToolCallStart` / `ToolCallResult` events are only emitted for CLI-native tools the CLI chose to use (e.g., it uses `ToolSearch` internally when confused about missing tools)
- Custom tools passed via `TurnRequest.Tools` are ignored — the CLI has no mechanism to receive external tool definitions

The spec says (§3.3): "Providers MUST NOT own tool execution." The claudecode subprocess provider violates this by design.

**Consequence:** For the real funnel, the Claude path either needs:
1. `provider/claude/` (direct API) with managed auth — operator to clarify if API key can be made available outside the CLI's credential store
2. A revised spec carve-out for "subprocess providers" that documents this difference explicitly

**Log-decision turn:** The model couldn't find `log_decision` because the CLI sees its own harness tool surface (ToolSearch, etc.), not the bridle tool surface. The log-decision turn was a no-op on this path.

### Session JSONL sample (claude path, text-only)
```json
{"role":"assistant","content":"4"}
```
Single assistant event, correctly serialized.

---

## Ollama path (local Docker container)

**Provider:** `provider/ollama/`  
**Container:** `ollama/ollama` (running ~2 days, up to date at test time)  
**Model:** `llama3.2:3b` (pulled fresh 2026-05-01)  
**Server:** `http://localhost:11434`

### What works
- Tool calling round-trip: `now` tool called by model, bridle hook fired (`BeforeToolCall`), `ToolRunner` executed, result returned, `ToolCallResult` emitted
- `StepBoundary` event fires after tool execution round
- Session delta contains all expected events: assistant text, raw tool call JSON, tool result
- Log-decision turn: `log_decision` tool called correctly, hook fired, ToolRunner returned, decision parsed
- `MaxSteps` constraint respected
- `BeforeToolCall` / `OnStepBoundary` hooks fired at correct points

### Bugs found and fixed during testing

**Bug (provider/ollama): nil http.Client**  
`api.NewClient(u, nil)` stores a nil `*http.Client` which panics on first use. Fixed to pass `http.DefaultClient`.

**Bug (stubfunnel): TurnRequest.Model not set in log-decision turn**  
The log-decision `TurnRequest` didn't set `Model`, causing `400 Bad Request: model is required` from Ollama. Fixed.

### Findings (non-blocking, model behavior)

**Llama3.2:3b leaks tool schema into response text:**  
The model's first chunk often starts with `}; {"name": "send_comms", ...}` — it's echoing part of its internal tool representation in the text stream. This is model-level behavior on small models with function-calling, not a bridle bug. The tool execution itself is correct; only the `FinalText` value is polluted.

**Llama3.2:3b doesn't always call `send_comms` as a tool:**  
When asked to "use send_comms to report the time," the model called `now` as a tool correctly but then described the `send_comms` call as text (`send_comms(...)`) rather than making the actual tool call. Again, small-model function-calling limitation. With a larger model (qwen2.5:7b available in the container) this would likely behave better.

### Session JSONL sample (Ollama path, tool round-trip)
```json
{"role":"assistant","content":"}; {\"name\": \"send_comms\", ...}"}
{"role":"assistant","raw":{...tool_use block...}}
{"role":"tool","content":"{\"time\":\"2026-04-30T15:55:00Z\"}"}
{"role":"assistant","content":"The current time is April 30, 2026, 3:55 PM.\n\nsend_comms(...)"}
```

Session round-trip is correct: all events appended, order preserved, funnel-owns-file contract holds.

---

## Spec mismatches to flag to @keel

1. **§3.3 subprocess provider carve-out needed.** The claudecode provider cannot comply with "providers MUST NOT own tool execution" by design — the CLI subprocess runs its own agentic loop. Options:
   - Add a "subprocess provider" category to §3 that explicitly documents what guarantees are relaxed
   - Or: require funnel to use `provider/claude` (direct API) for the Claude path and solve the auth problem separately

2. **`TurnRequest.Model` field not threaded through all harness-internal calls.** The `lowerRequest` function correctly passes `Model` to `ProviderRequest`, but callsites that construct `TurnRequest` (e.g., the stub funnel's log-decision turn) can accidentally omit it. This is a stub-funnel bug, not a spec bug — but the spec could be clearer that `Model` is required.

3. **`SessionEvent.RawJSON` shape.** The raw JSON stored for tool calls is provider-specific (Claude's `tool_use` block vs Ollama's `ToolCall` struct). When the funnel reads `SessionTail` back, it needs to know which provider wrote which entries. Spec §6 doesn't address this. Flag for discussion when the real funnel's session schema is defined.

---

## Summary

| Path | Text turn | Tool call | Hooks | Session JSONL | Log-decision |
|------|-----------|-----------|-------|---------------|--------------|
| Claude (claudecode) | ✅ | ❌ (CLI owns tools) | ⚠️ fires on CLI-native tools only | ✅ | ❌ model can't find custom tool |
| Ollama (llama3.2:3b) | ✅ | ✅ | ✅ | ✅ | ✅ |

Bridle's contract holds on the Ollama path. The Claude path reveals a fundamental provider-shape mismatch that needs a spec decision before the real funnel is built.
