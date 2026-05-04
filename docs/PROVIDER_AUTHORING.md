# Authoring a bridle Provider

This guide explains the `bridle.Provider` contract and walks through what a new provider must implement, how it integrates with the harness, and what the existing implementations get right (or wrong) that you can copy from.

It's the document to read **before** you write a new provider, and the document to update when the contract changes.

---

## What a Provider is

A `bridle.Provider` drives **one turn** of one model. It's the seam between bridle's provider-agnostic harness and a specific model surface — Anthropic's API, Google's Gemini API, the local `claude` CLI, an Ollama daemon, anything.

The harness owns:
- The deliberation loop (multi-step tool-call rounds within a turn)
- Custom-tool execution (when `SupportsCustomTools` is true)
- Lifecycle hooks (`OnBeforeToolCall` / `OnAfterToolCall` / `OnStepBoundary` / etc.)
- MCP tool plumbing (when `SupportsMCP` is true)

The Provider owns:
- Translating the harness's `ProviderRequest` into a model-native call
- Streaming events back through the `EventSink` (`ModelChunk`, `ToolCallStart`, `ToolCallResult`, etc.)
- Returning a `ProviderResult` summarizing what happened

That split is load-bearing. The harness assumes a Provider can be swapped without retesting the deliberation loop. New providers must honor it.

---

## The interface

```go
type Provider interface {
    Name() ProviderID
    Capabilities() ProviderCapabilities
    RunTurn(ctx context.Context, req ProviderRequest, sink EventSink) (ProviderResult, error)
}
```

### `Name()` — identity

Return a stable `ProviderID` constant (e.g. `bridle.ProviderClaude`, `bridle.ProviderGemini`). Add a new constant in `harness.go` when you introduce a new provider. The ID flows into `SessionEvent.Provider` so `ParseSessionEvent` can dispatch wire-format parsing back to the right provider.

### `Capabilities()` — what the harness should expect

```go
type ProviderCapabilities struct {
    Category               ProviderCategory // direct-api | subprocess-stream
    SupportsCustomTools    bool
    SupportsBeforeToolCall bool
    SupportsAfterToolCall  bool
    SupportsMCP            bool
}
```

- **`CategoryDirectAPI`** — provider talks to the model over HTTP/RPC, can intermediate every tool call. Set `SupportsCustomTools: true` and both hook flags to true.
- **`CategorySubprocessStream`** — provider shells out to a CLI that owns its own agent loop (`claude -p`, `gemini -p`). Bridle observes the stream but doesn't intercept. Set `SupportsCustomTools: false`, `SupportsBeforeToolCall: false`, `SupportsAfterToolCall: true` (we observe completed tool calls from the stream).

The harness reads capabilities once per Provider value and routes accordingly. Lying here will produce subtle failures — if you say you support custom tools and don't, the model never gets the tool list.

### `RunTurn(ctx, req, sink)` — drive one turn

This is where the work happens. The contract:

1. **Honor `ctx` cancellation.** If `<-ctx.Done()` fires mid-turn, abort cleanly. For subprocess providers: SIGTERM, grace period, SIGKILL — see the cancellation pattern below.
2. **Stream events through `sink`.** At minimum: `ModelChunk` for assistant text, `ToolCallStart`/`ToolCallResult` for tool activity, `TurnError` on errors. The funnel uses these for live UI; silent providers feel broken.
3. **Return a `ProviderResult`.** `FinalText` for the model's natural reply, `ToolCalls` for the structured list, `Usage` for token accounting, `StopReason` for why the turn ended, `SessionDelta` for replay. All fields populated.
4. **Errors are returned, not panicked.** Wrap with `%w` so the caller can `errors.Is` against your error sentinels.

---

## The cancellation pattern (subprocess providers)

This is the pattern used by `claudecode` and `geminicli`. It's been wrong twice in this repo — copy it carefully.

```go
cmd := exec.Command(...)
// ... pipes, Start() ...

// procExited is closed AFTER cmd.Wait() returns. The watcher waits on
// either ctx cancellation OR the process exiting naturally.
procExited := make(chan struct{})
go func() {
    select {
    case <-ctx.Done():
        _ = cmd.Process.Signal(sigterm())
        timer := time.NewTimer(5 * time.Second)
        defer timer.Stop()
        select {
        case <-timer.C:
            _ = cmd.Process.Kill()
        case <-procExited:
            // Process exited cleanly during grace period — no SIGKILL needed.
        }
    case <-procExited:
        // Natural exit — nothing to do.
    }
}()

waitErr := cmd.Wait()
close(procExited) // signal the cancel watcher that the process is gone
```

**Why the separate `procExited` channel matters.** Earlier drafts used a single `done` channel that the goroutine closed via `defer close(done)`. The inner `case <-done:` was unreachable because nothing else could close it. Two consequences:

1. **On clean process exit:** the watcher leaked until ctx eventually fired (could be hours).
2. **On cancellation:** SIGKILL always fired after the full grace period — even when the process responded to SIGTERM in 50ms.

`procExited` closed by the main goroutine after `cmd.Wait()` is the only correct shape. **Do not use `exec.CommandContext` here**: it sends SIGKILL immediately on cancel, with no grace period.

---

## ProviderMessage and tool-result correlation

```go
type ProviderMessage struct {
    Role       string // "user" | "assistant" | "tool_result" | "system"
    Content    string
    ToolCallID string // links a tool_result back to the call that produced it
    ToolName   string // function-declaration name; required by Gemini
}
```

For `Role == "tool_result"`, **both `ToolCallID` and `ToolName` must be populated.** Different model APIs key tool results differently:

- **Anthropic:** by `ToolCallID` only (`tool_use_id`).
- **OpenAI:** by `ToolCallID` only (`tool_call_id`).
- **Ollama:** by `ToolCallID` only.
- **Gemini:** by `ToolName` (FunctionResponse.Name must match the FunctionDeclaration.Name). The call ID goes in `FunctionResponse.ID` for the convo log but the API matches on Name.

If you write a new provider whose API needs the tool name to correlate responses, use `m.ToolName`. The harness populates both fields when constructing tool_result messages from completed `ToolInvocation`s.

---

## Lazy initialization (concurrent-safe)

Provider values may be reused across goroutines. Lazy init of expensive resources (HTTP clients, SDK clients) must be guarded:

```go
type Provider struct {
    clientOnce sync.Once
    clientErr  error
    client     *someSDK.Client
}

func (p *Provider) getClient(ctx context.Context) (*someSDK.Client, error) {
    p.clientOnce.Do(func() {
        c, err := someSDK.NewClient(ctx, ...)
        if err != nil {
            p.clientErr = fmt.Errorf("provider: client init: %w", err)
            return
        }
        p.client = c
    })
    if p.clientErr != nil {
        return nil, p.clientErr
    }
    return p.client, nil
}
```

`sync.Once` replays the failure to subsequent callers — important so a transient init failure doesn't cause silent retries that may produce inconsistent state.

---

## Tool allowlist translation (subprocess providers)

When `SupportsCustomTools` is false but the underlying CLI accepts an allowlist (`--allowedTools`, `--allowed-tools`), translate the harness's per-turn tool list into CLI flags. **Guard against the empty case:**

```go
allowed := p.AllowedTools
if len(req.Tools) > 0 {
    perTurn := make([]string, 0, len(req.Tools))
    for _, t := range req.Tools {
        if t.Name != "" {
            perTurn = append(perTurn, t.Name)
        }
    }
    if len(perTurn) > 0 {
        allowed = perTurn
    }
    // else: req.Tools had nothing usable; fall through to p.AllowedTools.
}
```

A non-empty `req.Tools` with all-empty `Name` fields would otherwise translate to `allowed=[]` and silently drop the allowlist flag entirely — the CLI then runs with the full default allowlist. That's a footgun (silent privilege escalation), so on degenerate input we fall back to the static `p.AllowedTools` rather than the "no flag at all" path.

---

## Session continuity

Providers that support session resume read `req.Session.ID`. New sessions: provider mints (or accepts) the ID and creates the session backing file/state. Existing sessions: provider points the model at the prior state.

`claudecode` example: `--session-id <uuid>` for new (CLI creates the jsonl), `--resume <uuid>` for existing. The provider stats `~/.claude/projects/*/<id>.jsonl` to decide which flag to use, since the funnel pre-mints UUIDs and doesn't know whether a session file exists yet.

If your provider has its own session shape, follow the same principle: don't trust the funnel to know whether the session is "new" or "existing"; check yourself.

---

## RawJSON in SessionEvent

When you can't represent a provider-specific block (tool_use, function_call, image, etc.) in the shared `Content` string, set `SessionEvent.RawJSON` instead. `ParseSessionEvent` in `session.go` dispatches by `Provider` to the right unmarshaler.

If you add RawJSON, also add a case for your provider in `session.go`'s parse function. Otherwise replay-from-tail will silently drop the block.

---

## Testing your provider

The repo's existing test surface:

- `claudecode_test.go` does a real CLI smoke (slow, requires `claude` on PATH).
- `internal/mcpclient` has unit tests.
- `harness_test.go` exercises the provider-agnostic harness with a fake provider.

The convention so far is one provider-level test per stream-style provider; direct-API providers don't need them because the genai/anthropic/openai SDKs are well-tested upstream. Add a test only when there's provider-specific glue worth pinning (cancellation behavior, stream parsing, allowlist translation).

---

## Checklist for a new provider

- [ ] `ProviderID` constant in `harness.go`
- [ ] `Name()`, `Capabilities()`, `RunTurn()` implemented
- [ ] Cancellation honors ctx; subprocess providers use the procExited pattern
- [ ] Lazy init guarded by `sync.Once` (if applicable)
- [ ] Tool allowlist translation guards the empty-name case (subprocess providers)
- [ ] `ProviderMessage.ToolName` consumed correctly (if tool-result correlation needs it)
- [ ] `SessionEvent.RawJSON` parsing added in `session.go` (if emitted)
- [ ] `internal/normalize` stop-reason mapping added (if model emits new stop reasons)
- [ ] `go build ./... && go test ./...` clean
- [ ] Doc comment on the provider package explaining auth, supported models, known caveats
