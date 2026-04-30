# bridle spec patch 2 — standard MCP client support

**Date:** 2026-05-01
**Patches:** `2026-05-01-bridle-spec.md` (v0.1) and `2026-05-01-bridle-spec-patch-1.md`
**Driver:** Operator #8514 — bridle should support standard MCP clients directly. mcphub (the future runtime-mutable variant) is a thin layer on top, not a separate transport. This collapses two notional code paths into one.
**Reframes:** patch-1 §3.1 ("subprocess-stream's tool surface IS mcphub") — under patch 2, the tool surface IS *an MCP client*; whether that client is static or mcphub-dynamic is the consumer's choice, not bridle's.

---

## 1. What this patch changes

1. Adds a **standard MCP client** to bridle. Any consumer can hand bridle an MCP server config and bridle wires the tools uniformly — across direct-api providers (where bridle's `ToolRunner` runs them) and subprocess-stream providers (where the subprocess runs them, but bridle still observes the same tool surface).
2. Reframes the funnel↔bridle tool surface contract: a funnel passes either (a) explicit `Tools []ToolDef` for in-process Go functions OR (b) an `MCPClientConfig` for MCP-loaded tools OR (c) both. bridle merges them.
3. Documents that **mcphub is just a `MCPClientConfig` source** — when mcphub lands (#83), the funnel hands bridle an mcphub-backed config. bridle doesn't grow new code paths; mcphub plugs in via the same MCP client interface.

---

## 2. New section §5a — MCP client support

### 2.1 Interface

```go
// MCPClientConfig describes how bridle should connect to MCP servers
// and what tool surface the model sees from them. Funnel constructs
// this; bridle consumes it.
type MCPClientConfig struct {
    Servers []MCPServerSpec  // one or more MCP servers to connect
    // Future: filters, allow/deny lists, tool-name remapping. Not v1.
}

type MCPServerSpec struct {
    Name      string            // local identifier, used in tool-call provenance
    Transport MCPTransport      // stdio | http_sse
    Command   []string          // for stdio: argv to spawn the server
    URL       string            // for http_sse: server URL
    Env       map[string]string // env vars for the spawned server (stdio only)
    Header    map[string]string // headers to send (http_sse only)
}

type MCPTransport string
const (
    MCPTransportStdio   MCPTransport = "stdio"
    MCPTransportHTTPSSE MCPTransport = "http_sse"
)
```

### 2.2 TurnRequest extension

```go
type TurnRequest struct {
    // … existing fields per v0.1 §2.1 + patch-1 §7 …
    Tools  []ToolDef         // existing — explicit in-process tool defs
    MCP    *MCPClientConfig  // NEW — MCP-loaded tools
}
```

`Tools` and `MCP` are not exclusive. A turn can carry both: explicit Go-function tools AND MCP server tools. bridle merges them into a single tool surface for the model. Name collisions (an explicit tool with the same name as an MCP-exposed tool) are an error at `RunTurn` time — `ErrToolNameCollision`.

If both are nil/empty, the turn runs with no tools (text-only). Same as today.

### 2.3 Lifecycle

bridle handles the MCP client lifecycle internally:
- **Connect** on first `RunTurn` that uses a given config; reuse the connection across subsequent calls with the same config.
- **List tools** via the MCP `tools/list` call after connect. These become the tool surface the model sees.
- **Call tools** via the MCP `tools/call` call when the model emits a tool_use. For direct-api providers, this happens inside bridle's tool loop. For subprocess-stream providers, this DOESN'T happen — the subprocess has its own MCP client wired by its runtime config, and bridle observes the resulting tool calls in the event stream.
- **Disconnect** on shutdown / on config change. bridle does not retain connections across config swaps.

This is the simplest possible lifecycle. Connection pooling, multiplexing, and reconnection-on-failure are deferred (out of scope per §9).

### 2.4 Provider-specific behavior

| Provider category | What bridle does with `MCP` |
|---|---|
| `direct-api` | Connect to MCP server(s), list tools, expose them to the model alongside `Tools`. When model calls an MCP tool, bridle forwards the call via MCP. When model calls an explicit `Tools` function, bridle invokes the funnel's `ToolRunner`. |
| `subprocess-stream` | **Ignored.** The subprocess (e.g. claude-code-headless) has its own MCP client wired by its runtime config (e.g. `.mcp.json` next to the project root). bridle does not configure the subprocess's MCP. If the funnel needs to influence subprocess MCP, it does so by writing the subprocess's config files before spawning — that's funnel-side, not bridle-side. |

Capability advertisement (per patch-1 §2.3) gets one new field:

```go
type ProviderCapabilities struct {
    Category               ProviderCategory
    SupportsCustomTools    bool
    SupportsBeforeToolCall bool
    SupportsAfterToolCall  bool
    SupportsMCP            bool  // NEW — does the provider consume TurnRequest.MCP?
}
```

`direct-api` providers report `SupportsMCP=true`. `subprocess-stream` reports `SupportsMCP=false`. Funnels with MCP needs route through `direct-api` providers (or pre-configure the subprocess's own MCP setup before dispatch).

---

## 3. mcphub is just an MCPClientConfig source

When mcphub (#83) lands, it's a thin layer the funnel uses to *generate* an `MCPClientConfig` per dispatch — possibly with a different tool set per aspect, possibly with a runtime-mutable surface. From bridle's perspective, mcphub is invisible: bridle gets an `MCPClientConfig`, connects, lists tools, runs the turn.

This means:
- bridle never grows mcphub-specific code.
- The funnel decides whether to use a static `MCPClientConfig` (read once from a JSON file) or a dynamic one (mcphub-supplied per dispatch).
- "mcphub" is, mechanically, "the same MCPClient bridle uses, but with a swap-the-tool-set extension on top." Operator #8514: *"the mcp hub is - a mcp client with a changable tool set"*.

---

## 4. Refinement: §5 tool surface (v0.1)

Replaces the v0.1 §5 paragraph with:

> The harness consumes tools from two sources: explicit `Tools []ToolDef` (in-process Go functions, run via the funnel-supplied `ToolRunner`) and `MCP *MCPClientConfig` (MCP server-exposed tools, run via bridle's internal MCP client). Both reach the model as a uniform tool surface; the model doesn't see a distinction. Name collisions across the two sources are an error at `RunTurn` time.
>
> `send_comms` is a tool. The funnel's `ToolRunner` typically owns it (in-process); MCP-supplied is also fine if the funnel prefers an MCP server route. The harness has no special case for it either way.
>
> **Subprocess-stream providers:** see patch-1 §5. The subprocess owns tool execution against its own configured tool surface; bridle's `ToolRunner` is observe-only and `MCPClientConfig` is ignored.

---

## 5. Build order for anvil

1. Add `MCPClientConfig` + `MCPServerSpec` types. No connection logic yet — just the surface.
2. Add `MCP` field to `TurnRequest`. Plumb through `ProviderRequest`.
3. Add `SupportsMCP` to `ProviderCapabilities`. Update existing providers to report it.
4. Build the MCP client. Reference: there's an official Anthropic MCP Go SDK (likely `github.com/anthropics/mcp-go` or similar — check). If a usable Go MCP client exists, lift it; if not, the protocol is small enough to implement directly. stdio transport first, http_sse after.
5. Wire MCP-tool-call resolution into `direct-api` providers. The model's tool_use event flows through MCP for MCP-sourced tools, through `ToolRunner` for explicit-source tools.
6. Tests: fake MCP server (scripts a `tools/list` response + canned `tools/call` results), round-trip turn against the fake, name-collision error, both-sources-merged round-trip.

Same iteration discipline as patch-1: ship when validated.

---

## 6. Non-goals for this patch

- **Connection pooling / multiplexing.** One config = one connection per server. Reuse across `RunTurn` calls with the same config. No pool.
- **Reconnect on failure.** If an MCP server dies mid-turn, the turn errors out (`TurnError{Stage: "mcp_disconnected"}`). Funnel decides retry. Resilience is funnel-level.
- **mcphub-specific code paths.** Patch-2 keeps bridle MCP-only; mcphub is a future config source, not a future provider category.
- **Cross-server tool name resolution.** If two MCP servers expose the same tool name, that's a collision (just like explicit-vs-MCP collisions). Namespacing by server name (`server.tool`) is a future option, not v1.

---

## 7. Open questions (defer until forced)

- **MCP client library choice.** Use an existing Go SDK if it exists and is maintainable; roll our own if not. Anvil to evaluate at build time.
- **Tool schema format.** MCP tool schemas are JSON Schema; bridle's `ToolDef` schema may need a small adapter. Anvil to align at build time.
- **Auth on http_sse transport.** v1 supports `Header` for static bearer tokens. OAuth / mTLS / etc. not v1.

---

## 8. What anvil receives

This patch + patches v0.1 + patch-1. The `nexus/frame` v0.1 spec build plan (`docs/2026-05-01-frame-65-build-plan.md` in nexus-cw/nexus) gates P6 on this — bridle needs MCP support before the funnel's first deliberation loop ships. Suggested order:

1. Land patch-2 (this doc) on bridle.
2. nexus-cw/nexus continues §6.5 P2–P5 in parallel — none of those need MCP.
3. nexus-cw/nexus P6 consumes bridle-with-MCP.

No blocking dependency between this patch and nexus §6.5 P2–P5.
