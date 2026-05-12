# Bridle Bedrock Provider — Tier 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring `provider/bedrock/` to production usability for Claude-on-Bedrock with multi-turn tool conversations, streaming, prompt caching, inference params, custom endpoints, and the two open reviewer findings resolved.

**Architecture:** Split into two PRs. **PR A** extends `ProviderMessage` with structured `ToolCalls` so any direct-api provider can reconstruct assistant tool-use history — touches every direct-api provider as plumbing. **PR B** is the bedrock-specific work: `ConverseStream` for token streaming, `InferenceConfig`, prompt caching via `CachePointBlock`, `ToolChoice`, custom endpoint/HTTP client, and the two reviewer fixes.

**Tech Stack:** Go 1.25, `github.com/aws/aws-sdk-go-v2/service/bedrockruntime`, smithy `eventstream` for ConverseStream parsing, `internal/normalize` for stop-reason mapping.

---

## Phase A — `ProviderMessage.ToolCalls` (PR 1)

**Why:** The harness loop in `run.go` (line ~176) appends `tool_result` messages but never reconstructs the preceding `assistant{tool_use}` turn. Bedrock (and Anthropic, OpenAI, Gemini) all require strict `assistant{tool_use}` → `user{tool_result}` alternation. Current code sends `[..., user, tool_result, tool_result, ...]` which is invalid history. Claude API may be lenient about it; Bedrock rejects.

**Scope:** In-loop reconstruction only. Cross-turn (resume from `SessionTail`) preservation is a separate follow-up — flagged at the end.

### Task A1: Add `ToolCalls` field to `ProviderMessage`

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider.go:66-71`

- [ ] **Step 1: Extend the struct + doc comment**

Replace the existing `ProviderMessage` definition (currently lines 55-71) with:

```go
// ProviderMessage is a single exchange entry in provider-agnostic form.
//
// For Role == "tool_result", both ToolCallID and ToolName must be set.
// ToolCallID is the call instance identifier the assistant emitted (used
// to correlate this result with that specific invocation). ToolName is
// the function-declaration name that was called (e.g. "send_chat") —
// some providers (Gemini's FunctionResponse) require it to be present
// alongside the call id, because their wire format keys responses by
// declaration name, not by call id. Providers that key only by call id
// (Anthropic, OpenAI, Ollama) ignore ToolName and the field can be left
// empty without harm.
//
// For Role == "assistant", ToolCalls carries the structured tool_use
// blocks the model emitted on this turn. Providers that send assistant
// history back to the model (claude, openai, gemini, bedrock) MUST
// reconstruct these as native tool_use blocks; sending only Content as
// plain text loses the tool-call structure and breaks multi-turn tool
// conversations on strict providers (Bedrock rejects, Anthropic and
// OpenAI are lenient but degrade). Content and ToolCalls can both be
// non-empty — text and tool_use blocks coexist in one assistant turn.
type ProviderMessage struct {
	Role       string // "user" | "assistant" | "tool_result" | "system"
	Content    string
	ToolCallID string           // links a tool_result back to the call that produced it
	ToolName   string           // function-declaration name; required for tool_result on Gemini
	ToolCalls  []ToolInvocation // tool_use blocks for assistant turns; nil on other roles
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add provider.go
git commit -m "feat(bridle): ProviderMessage.ToolCalls for assistant tool_use round-trip

Direct-api providers that send assistant history back to the model must
preserve tool_use block structure across turns. Adding a structured
ToolCalls field on ProviderMessage so providers can reconstruct them
natively instead of collapsing to text. No behaviour change yet —
provider implementations are updated in follow-up commits."
```

### Task A2: Reconstruct assistant turn in `run.go` loop

**Files:**
- Modify: `/Users/jacinta/Source/bridle/run.go:174-177`

- [ ] **Step 1: Find the insertion point**

In `runTurn`, locate the section right before the existing `preq.Messages = append(preq.Messages, toolMessages...)` (around line 176). The current code appends only tool results; we need to insert the assistant turn first.

- [ ] **Step 2: Insert assistant reconstruction before tool_result append**

Replace this block:

```go
		// Append tool results to message history.
		preq.Messages = append(preq.Messages, toolMessages...)
```

with:

```go
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
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add run.go
git commit -m "fix(bridle): reconstruct assistant tool_use turn between rounds

The harness loop was appending tool_result messages without the
preceding assistant turn that emitted the tool_use blocks. Bedrock
rejects this as invalid history (consecutive same-role); Anthropic and
OpenAI are more lenient but lose the tool-call structure. Insert the
assistant turn (using new ProviderMessage.ToolCalls) before each
tool_result batch so all direct-api providers see correct alternation."
```

### Task A3: Update claude provider to round-trip `ToolCalls`

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/claude/claude.go:138-162`

- [ ] **Step 1: Replace the assistant case in `toClaudeMessages`**

The current `case "assistant"` (line 146-149) only emits a text block. Replace with code that emits text + tool_use blocks:

```go
		case "assistant":
			blocks := []anthropic.ContentBlockParamUnion{}
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, tc.Args, tc.Name))
			}
			if len(blocks) == 0 {
				// Empty assistant turn — skip rather than emit invalid empty content
				continue
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
```

- [ ] **Step 2: Verify the Anthropic SDK helper signature**

Check the anthropic-sdk-go API: `anthropic.NewToolUseBlock` takes `(id, input, name string)` where input is `json.RawMessage`. If the signature differs in v1.38.0, look in `vendor/github.com/anthropics/anthropic-sdk-go/` or run `go doc github.com/anthropics/anthropic-sdk-go.NewToolUseBlock`.

Run: `cd /Users/jacinta/Source/bridle && go doc github.com/anthropics/anthropic-sdk-go.NewToolUseBlock`
Expected: signature confirming arg order. Adjust the code above if needed.

- [ ] **Step 3: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add provider/claude/claude.go
git commit -m "feat(claude): emit tool_use blocks from ProviderMessage.ToolCalls

Assistant turns with tool_use are now reconstructed natively from the
new ProviderMessage.ToolCalls field, instead of collapsing the tool
call to text. Required for the harness's in-loop reconstruction to
round-trip cleanly through the Messages API."
```

### Task A4: Update openai provider

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/openai/openai.go` (find the `toOpenAIMessages` function)

- [ ] **Step 1: Read the current `toOpenAIMessages` function**

Run: `cd /Users/jacinta/Source/bridle && grep -n "toOpenAIMessages\|case \"assistant\"" provider/openai/openai.go`

The assistant case currently sets `.Content` only. OpenAI's Chat Completions assistant message takes a `tool_calls` array of `{id, type:"function", function:{name, arguments}}` where `arguments` is a JSON string.

- [ ] **Step 2: Replace the assistant case**

Find the assistant case and replace its construction with one that populates `ToolCalls` (OpenAI's field) from the `ProviderMessage.ToolCalls`. Pseudocode (adjust to match the openai-go SDK shapes in this codebase):

```go
case "assistant":
	msg := openai.ChatCompletionAssistantMessageParam{}
	if m.Content != "" {
		msg.Content = openai.String(m.Content)
	}
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ChatCompletionMessageToolCallParam{
			ID:   tc.ID,
			Type: "function",
			Function: openai.ChatCompletionMessageToolCallFunctionParam{
				Name:      tc.Name,
				Arguments: string(tc.Args),
			},
		})
	}
	out = append(out, openai.ChatCompletionAssistantMessageParamUnion{...assistant: msg...})
```

Verify exact param-union construction by reading nearby code in the file — the SDK's union types are fiddly.

- [ ] **Step 3: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add provider/openai/openai.go
git commit -m "feat(openai): emit assistant.tool_calls from ProviderMessage.ToolCalls

Round-trip the structured tool-call history through OpenAI's tool_calls
field instead of collapsing to text content. Required for multi-turn
tool conversations to behave correctly on the OpenAI Chat Completions
endpoint."
```

### Task A5: Update gemini provider

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/gemini/gemini.go:185-227`

- [ ] **Step 1: Replace the assistant case in `toGeminiContents`**

The current `case "assistant"` (lines 194-198) emits only a `Text` part with role `"model"`. Replace to include FunctionCall parts:

```go
		case "assistant":
			parts := []*genai.Part{}
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				_ = json.Unmarshal(tc.Args, &args)
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   tc.ID,
						Name: tc.Name,
						Args: args,
					},
				})
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, &genai.Content{
				Role:  "model",
				Parts: parts,
			})
```

- [ ] **Step 2: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add provider/gemini/gemini.go
git commit -m "feat(gemini): emit FunctionCall parts from ProviderMessage.ToolCalls

Reconstruct the assistant turn with native FunctionCall parts so
Gemini sees the structured tool-call history that pairs with the
FunctionResponse parts it requires for tool_result correlation."
```

### Task A6: Ollama provider — minimal update

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/ollama/ollama.go` (assistant case in its message lowering)

- [ ] **Step 1: Inspect what ollama-go exposes**

Run: `cd /Users/jacinta/Source/bridle && grep -n "case \"assistant\"\|ToolCalls" provider/ollama/ollama.go`

Ollama's API exposes assistant tool calls as a list of `{function: {name, arguments}}`. The SDK type is `api.ToolCall`. If the assistant case currently sets only `Content`, replicate the pattern from openai: emit native tool_calls from `m.ToolCalls`.

- [ ] **Step 2: Implement the assistant case**

Apply the same pattern: build text + tool_calls and add to the messages slice. Adjust to the ollama SDK's struct names (likely `api.Message{Role: "assistant", Content: ..., ToolCalls: []api.ToolCall{...}}`).

- [ ] **Step 3: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add provider/ollama/ollama.go
git commit -m "feat(ollama): emit assistant tool_calls from ProviderMessage.ToolCalls"
```

### Task A7: claudecode + geminicli — sanity check (no change expected)

**Files:** none expected to change.

- [ ] **Step 1: Verify subprocess providers don't consume `ProviderMessage` outbound**

Both `claudecode` and `geminicli` are subprocess-stream providers — they don't lower messages back to a model. They consume `req.UserMessage` and `req.AppendSystemPrompt`, not the message history (the subprocess owns its own session). Quick grep to confirm:

Run: `cd /Users/jacinta/Source/bridle && grep -n "ProviderMessage\|m.ToolCalls\|m\.Content" provider/claudecode/claudecode.go provider/geminicli/geminicli.go`

Expected: no references that would need updating. If anything looks off, flag it; otherwise no commit.

### Task A8: Test the full Phase A change

**Files:**
- No new test files (codebase has no provider-level tests except claudecode).

- [ ] **Step 1: Run the full test suite**

Run: `cd /Users/jacinta/Source/bridle && go test ./...`
Expected: PASS for `bridle`, `internal/mcpclient`, `provider/claudecode`. Other packages have no tests but should not regress.

- [ ] **Step 2: Run vet + build**

Run: `cd /Users/jacinta/Source/bridle && go vet ./... && go build ./...`
Expected: clean.

### Task A9: Push and open PR A

- [ ] **Step 1: Push the branch**

```bash
cd /Users/jacinta/Source/bridle
git checkout -b feat/provider-message-toolcalls
git push -u origin feat/provider-message-toolcalls
```

- [ ] **Step 2: Open PR**

```bash
gh pr create --title "feat(bridle): ProviderMessage.ToolCalls for multi-turn tool conversations" --body "$(cat <<'EOF'
## Summary

Adds a structured \`ToolCalls\` field to \`ProviderMessage\` so direct-api providers can reconstruct assistant \`tool_use\` blocks across turns.

**Why:** The harness loop in \`run.go\` previously appended \`tool_result\` messages without the preceding assistant turn that emitted the corresponding \`tool_use\` blocks. Anthropic and OpenAI tolerate this with degraded behaviour; Bedrock rejects the request as invalid history. Without this, multi-turn Claude-on-Bedrock with tools is unusable.

**Changes:**
- \`provider.go\`: add \`ToolCalls []ToolInvocation\` to \`ProviderMessage\`
- \`run.go\`: insert reconstructed assistant turn before each tool_result batch in the harness loop
- \`provider/claude\`, \`provider/openai\`, \`provider/gemini\`, \`provider/ollama\`: emit native tool_use / tool_calls / FunctionCall blocks from the new field

**Out of scope:** Cross-turn (SessionTail) reconstruction — when a turn is resumed from session history, \`lowerRequest\` walks \`SessionTail\` and currently discards \`RawJSON\`. That's a separate plumbing fix for a follow-up PR.

## Test plan

- [ ] \`go build ./...\` passes
- [ ] \`go test ./...\` passes
- [ ] Manual smoke against Claude API with a multi-turn tool conversation (bedrock provider can't be tested here — that's PR B)
EOF
)"
```

- [ ] **Step 3: Run code reviewer**

Dispatch `feature-dev:code-reviewer` against the diff. Address any high-confidence findings before merging.

- [ ] **Step 4: Merge if reviewer passes**

```bash
gh pr merge --squash --delete-branch
```

---

## Phase B — Bedrock Tier 2 (PR 2)

**Prereq:** Phase A merged. Start a fresh branch off `main`.

### Task B1: Introduce `converseClient` interface + new Provider fields

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go:30-49` (Provider struct + constructors)

- [ ] **Step 1: Define the internal interface**

Near the top of the file (after imports), add:

```go
// converseClient is the minimal Bedrock client surface bedrock.Provider uses.
// Real callers get the concrete *bedrockruntime.Client; tests substitute a fake.
type converseClient interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}
```

- [ ] **Step 2: Update Provider struct fields**

Replace the existing `Provider` struct (lines 30-38) with:

```go
// Provider implements bridle.Provider for AWS Bedrock via the Converse API.
//
// Concurrency: safe for use across goroutines. The internal client is built
// lazily and reused; getClient serializes init on p.mu.
type Provider struct {
	mu        sync.Mutex
	client    converseClient
	clientErr error // cached for permanent failures only; ctx errors are not cached
	region    string

	// Profile selects an AWS shared-config profile (overrides AWS_PROFILE if set).
	Profile string

	// Endpoint overrides the Bedrock service endpoint URL. Maps to the SDK's
	// BaseEndpoint option. Use for enterprise gateways that front Bedrock
	// with a corporate URL but still expect SigV4 signing. Leave empty for
	// the standard regional endpoint.
	Endpoint string

	// HTTPClient overrides the SDK's default HTTP transport. Use to inject
	// a corporate CA bundle, a proxy, or custom TLS for enterprise deploys.
	// Leave nil to use the SDK default.
	HTTPClient *http.Client

	// Inference parameters. All optional — zero values fall through to the
	// model's Bedrock default. MaxTokens defaults to 4096 if unset (matches
	// provider/claude/claude.go for Anthropic models).
	MaxTokens     int32    // 0 → 4096
	Temperature   *float32 // nil → model default
	TopP          *float32 // nil → model default
	StopSequences []string // empty → no caller-defined stop sequences

	// EnablePromptCaching, when true, emits CachePoint blocks at strategic
	// positions (after system prompt, after tool definitions, after each
	// tool_result batch) so Anthropic models on Bedrock can hit the prompt
	// cache. Bedrock supports up to 4 cache breakpoints; we stay within that.
	// Non-Anthropic models ignore cache points cleanly.
	EnablePromptCaching bool
}
```

- [ ] **Step 3: Update `New` to accept HTTPClient/Endpoint via the struct (no new positional args)**

`New(region string)` stays the same. Callers set `Endpoint`, `HTTPClient`, inference params, etc. on the returned `*Provider` before first use.

Update `NewWithClient` to accept the interface:

```go
// NewWithClient returns a Bedrock provider using a pre-configured client.
// Use for advanced setups (custom credential providers, smithy middleware,
// non-SigV4 auth) where the constructor's Endpoint/HTTPClient fields are
// insufficient. The provided client must satisfy bridle's converseClient
// surface — concrete *bedrockruntime.Client does.
func NewWithClient(client converseClient) *Provider {
	return &Provider{client: client}
}
```

- [ ] **Step 4: Update `getClient` to honour Endpoint + HTTPClient**

Replace the existing `getClient` body. Inside the `if p.client != nil` short-circuit stays. After loading config, build the client with options:

```go
	clientOpts := []func(*bedrockruntime.Options){}
	if p.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *bedrockruntime.Options) {
			o.BaseEndpoint = aws.String(p.Endpoint)
		})
	}
	p.client = bedrockruntime.NewFromConfig(cfg, clientOpts...)
```

And before `awsconfig.LoadDefaultConfig`, append HTTPClient option:

```go
	if p.HTTPClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(p.HTTPClient))
	}
```

- [ ] **Step 5: Import `net/http`**

Add `"net/http"` to the imports block.

- [ ] **Step 6: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add provider/bedrock/bedrock.go
git commit -m "refactor(bedrock): internal converseClient interface + endpoint/HTTP fields

Introduce a minimal converseClient interface so unit tests can fake the
SDK without standing up real AWS credentials. Add Endpoint, HTTPClient,
and inference-config fields on Provider — the enterprise pattern is
'SigV4 against a corporate gateway URL with a custom CA bundle', covered
by Endpoint + HTTPClient without forcing callers to NewWithClient.

NewWithClient now accepts the interface; concrete *bedrockruntime.Client
satisfies it. Behaviour unchanged for existing callers."
```

### Task B2: Fix the two open reviewer findings

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go` — `extractResult` (guardrail leak) and `toBedrockMessages` (consecutive user-after-tool_result).

#### B2a: Guardrail/content-filter empty-result fix

- [ ] **Step 1: Locate the safety-stop block**

In `extractResult`, around lines 195-213, the function builds `result` then conditionally wraps with an error. Replace this block:

```go
	result := bridle.ProviderResult{...}

	if rawStop == string(types.StopReasonGuardrailIntervened) {
		return result, fmt.Errorf("bedrock: guardrail_intervened: response blocked by configured guardrail")
	}
	if rawStop == string(types.StopReasonContentFiltered) {
		return result, fmt.Errorf("bedrock: content_filtered: response blocked by content filter")
	}
	return result, nil
```

with:

```go
	// Safety stops: return empty ProviderResult{} alongside the error so the
	// harness doesn't leak partial tool_use blocks or session events from a
	// blocked turn. Matches the failure-mode contract of provider/claude and
	// provider/openai — error paths return no usable result.
	if rawStop == string(types.StopReasonGuardrailIntervened) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: guardrail_intervened: response blocked by configured guardrail")
	}
	if rawStop == string(types.StopReasonContentFiltered) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: content_filtered: response blocked by content filter")
	}

	return bridle.ProviderResult{
		FinalText:    finalText,
		ToolCalls:    toolCalls,
		Usage:        usage,
		StopReason:   stopReason,
		SessionDelta: sessionDelta,
	}, nil
```

#### B2b: Consecutive user-after-tool_result fold

- [ ] **Step 1: Restructure `toBedrockMessages`**

Bedrock's Converse rule: messages alternate strictly user/assistant. `tool_result` IS a user content block. So `[tool_result, tool_result, user_text]` from bridle must become ONE user message with three content blocks, not three messages.

Replace the function (currently lines 229-280):

```go
// toBedrockMessages flattens bridle ProviderMessages into Bedrock Converse
// messages. Bedrock requires strict user/assistant alternation, where
// tool_result is a user content block. We accumulate user-role blocks
// (text + tool_result) into pendingUserBlocks and flush them as a single
// user message only when we hit an assistant turn or end of stream.
//
// Assistant turns are emitted with both text content and reconstructed
// tool_use blocks (from ProviderMessage.ToolCalls, populated by the
// harness in run.go's tool loop).
func toBedrockMessages(msgs []bridle.ProviderMessage) ([]types.Message, error) {
	out := make([]types.Message, 0, len(msgs))
	var pendingUserBlocks []types.ContentBlock

	flushUser := func() {
		if len(pendingUserBlocks) == 0 {
			return
		}
		out = append(out, types.Message{
			Role:    types.ConversationRoleUser,
			Content: pendingUserBlocks,
		})
		pendingUserBlocks = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case "tool_result":
			pendingUserBlocks = append(pendingUserBlocks, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: aws.String(m.ToolCallID),
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: m.Content},
					},
				},
			})

		case "user", "system":
			// Both fold into a user content block. System is taken separately
			// via ConverseInput.System; any system in the message stream is
			// an inline context note from the harness.
			if m.Content != "" {
				pendingUserBlocks = append(pendingUserBlocks, &types.ContentBlockMemberText{Value: m.Content})
			}

		case "assistant":
			flushUser()
			blocks := []types.ContentBlock{}
			if m.Content != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if len(tc.Args) > 0 {
					if err := json.Unmarshal(tc.Args, &input); err != nil {
						return nil, fmt.Errorf("bedrock: tool_use %q args unmarshal: %w", tc.Name, err)
					}
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String(tc.ID),
						Name:      aws.String(tc.Name),
						Input:     document.NewLazyDocument(input),
					},
				})
			}
			if len(blocks) == 0 {
				continue // skip empty assistant turn rather than send invalid request
			}
			out = append(out, types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: blocks,
			})

		default:
			continue
		}
	}
	flushUser()
	return out, nil
}
```

- [ ] **Step 2: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add provider/bedrock/bedrock.go
git commit -m "fix(bedrock): empty result on guardrail + fold consecutive user blocks

Address two findings from the pre-merge review:

1. Guardrail/content-filter path now returns an empty ProviderResult{}
   alongside the error, matching the contract of claude/openai providers.
   Previously a blocked turn could leak partial tool_use blocks and
   session events into harness state.

2. toBedrockMessages now accumulates user-role content (text +
   tool_result) into a single Bedrock user message instead of emitting
   one message per bridle ProviderMessage. The previous flushToolResults
   path emitted consecutive user-role messages when a user text
   ProviderMessage followed tool_results, which Bedrock rejects as
   invalid alternation.

Assistant turns now also emit native tool_use blocks reconstructed from
ProviderMessage.ToolCalls (populated by run.go's harness loop in the
prior PR)."
```

### Task B3: Inference config — wire `MaxTokens`/`Temperature`/`TopP`/`StopSequences`

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go` — `RunTurn`

- [ ] **Step 1: Populate `ConverseInput.InferenceConfig`**

In `RunTurn`, after building `in := &bedrockruntime.ConverseInput{...}` and before the system prompt block, add:

```go
	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	in.InferenceConfig = &types.InferenceConfiguration{
		MaxTokens: aws.Int32(maxTokens),
	}
	if p.Temperature != nil {
		in.InferenceConfig.Temperature = p.Temperature
	}
	if p.TopP != nil {
		in.InferenceConfig.TopP = p.TopP
	}
	if len(p.StopSequences) > 0 {
		in.InferenceConfig.StopSequences = p.StopSequences
	}
```

- [ ] **Step 2: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add provider/bedrock/bedrock.go
git commit -m "feat(bedrock): wire inference config — MaxTokens (default 4096), Temperature, TopP, StopSequences

Without an explicit MaxTokens, Anthropic models on Bedrock cap output
at the model default (often ~512), which truncates long responses.
4096 matches provider/claude/claude.go and is a sane ceiling for chat.
Temperature/TopP/StopSequences pass through when set; nil/empty values
leave model defaults intact."
```

### Task B4: Add `ToolChoice` support

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider.go` — add `ToolChoice` to `ProviderRequest`
- Modify: `/Users/jacinta/Source/bridle/harness.go` — add `ToolChoice` to `TurnRequest`
- Modify: `/Users/jacinta/Source/bridle/run.go` — propagate in `lowerRequest`
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go` — honour it in `toBedrockTools`

- [ ] **Step 1: Add the field to `TurnRequest`**

In `harness.go`, after `MaxSteps`, add:

```go
	// ToolChoice optionally constrains how the model picks tools.
	// Empty string → provider default (typically "auto").
	// "auto" → model decides whether to call a tool.
	// "any" → model must call exactly one tool, free choice of which.
	// "none" → no tools may be called this turn (text only).
	// Any other value → name of a specific tool that must be called.
	// Not all providers honour all values; unsupported values fall back to "auto".
	ToolChoice string
```

- [ ] **Step 2: Mirror it in `ProviderRequest`**

In `provider.go`, after `Tools`, add `ToolChoice string`.

- [ ] **Step 3: Propagate in `lowerRequest`**

In `run.go`'s `lowerRequest`, the returned `ProviderRequest{...}` literal needs `ToolChoice: req.ToolChoice,`.

- [ ] **Step 4: Honour in bedrock `toBedrockTools`**

Change the signature to accept the choice string and return both the config + the choice-mapped value. Or simpler: just take choice as a second arg and set `ToolConfiguration.ToolChoice`:

```go
func toBedrockTools(defs []bridle.ToolDef, choice string) (*types.ToolConfiguration, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	tools := make([]types.Tool, 0, len(defs))
	for _, d := range defs {
		// ... existing code ...
	}
	cfg := &types.ToolConfiguration{Tools: tools}
	switch choice {
	case "", "auto":
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
	case "any":
		cfg.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
	case "none":
		// Bedrock has no explicit "none" — return tools without a choice and
		// the model may still call one. For strict no-tools, the caller
		// should pass req.Tools = nil. Document and fall through to auto.
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
	default:
		cfg.ToolChoice = &types.ToolChoiceMemberTool{
			Value: types.SpecificToolChoice{Name: aws.String(choice)},
		}
	}
	return cfg, nil
}
```

Update the call site in `RunTurn` to pass `req.ToolChoice`.

- [ ] **Step 5: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add provider.go harness.go run.go provider/bedrock/bedrock.go
git commit -m "feat(bridle): TurnRequest.ToolChoice with bedrock implementation

Adds a provider-agnostic ToolChoice on TurnRequest/ProviderRequest with
values \"\" (default) / \"auto\" / \"any\" / \"none\" / <tool-name>. Bedrock
honours all forms; \"none\" falls back to auto since Converse has no
explicit none — callers wanting strict no-tools should pass req.Tools=nil.
Other providers ignore the field for now; follow-up PRs can wire them."
```

### Task B5: Prompt caching via `CachePointBlock`

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go`

- [ ] **Step 1: Cache points after system prompt**

In `RunTurn`, after setting `in.System` if `AppendSystemPrompt` is non-empty, conditionally append a cache point:

```go
	if req.AppendSystemPrompt != "" {
		in.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: req.AppendSystemPrompt},
		}
		if p.EnablePromptCaching {
			in.System = append(in.System, &types.SystemContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
	}
```

- [ ] **Step 2: Cache point after tool definitions**

Bedrock's `ToolConfiguration.Tools` is a flat slice and doesn't take a cache point directly — instead the convention is to insert a CachePoint as the LAST tool entry. The SDK has `types.ToolMemberCachePoint`. In `toBedrockTools`, if caching is enabled (pass a flag in), append:

Change signature: `toBedrockTools(defs, choice, enableCache)`. After the tool loop:

```go
	if enableCache {
		tools = append(tools, &types.ToolMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
	}
```

Update the call site to pass `p.EnablePromptCaching`.

- [ ] **Step 3: Cache point at end of last user message**

In `toBedrockMessages`, the cleanest spot is at the END of the very last `pendingUserBlocks` flush before returning. After the final `flushUser()` (which is also at the end of the for loop just before `return out, nil`), if caching was enabled we want a cache point on the final user message — but only on its content, before flushing.

Restructure: pass `enableCache bool` to `toBedrockMessages` and inside, track the index of the last user message in `out`. After the loop and final flush, if caching enabled and `out` ends with a user message, append a cache-point block to that message's content:

```go
func toBedrockMessages(msgs []bridle.ProviderMessage, enableCache bool) ([]types.Message, error) {
	// ... existing accumulator logic ...
	flushUser()

	if enableCache && len(out) > 0 {
		last := &out[len(out)-1]
		if last.Role == types.ConversationRoleUser {
			last.Content = append(last.Content, &types.ContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
	}
	return out, nil
}
```

Update the call site to pass `p.EnablePromptCaching`.

- [ ] **Step 4: Surface cache token usage**

In `extractResult`, after building `usage.InputTokens`/`usage.OutputTokens`, the Bedrock `ConverseOutput.Usage` struct exposes `CacheReadInputTokens` and `CacheWriteInputTokens` (verify the exact field names with `go doc github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types.TokenUsage`). Wire them:

```go
	if resp.Usage != nil {
		usage.InputTokens = int(aws.ToInt32(resp.Usage.InputTokens))
		usage.OutputTokens = int(aws.ToInt32(resp.Usage.OutputTokens))
		if resp.Usage.CacheReadInputTokens != nil {
			usage.CacheReadInputTokens = int(aws.ToInt32(resp.Usage.CacheReadInputTokens))
		}
		if resp.Usage.CacheWriteInputTokens != nil {
			usage.CacheCreationInputTokens = int(aws.ToInt32(resp.Usage.CacheWriteInputTokens))
		}
	}
```

If the SDK field names differ, adjust accordingly.

- [ ] **Step 5: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add provider/bedrock/bedrock.go
git commit -m "feat(bedrock): prompt caching via CachePointBlock

When Provider.EnablePromptCaching is true, emit cache points at three
positions: after the system prompt, after the tool definitions, and at
the end of the last user message. Bedrock's 4-cache-point limit covers
this comfortably. Cache read/write token counts surface on Usage via
the existing CacheReadInputTokens / CacheCreationInputTokens fields,
matching the claude-api semantics for downstream cost accounting.

Non-Anthropic models silently ignore cache points, so leaving the flag
on for mixed-model deployments is safe."
```

### Task B6: Switch to `ConverseStream`

**Files:**
- Modify: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock.go` — replace synchronous `Converse` with streamed `ConverseStream` + event accumulator.

- [ ] **Step 1: Build the streamed input**

In `RunTurn`, replace `client.Converse(ctx, in)` with `ConverseStream`. The input type is `*bedrockruntime.ConverseStreamInput` with the same fields as `ConverseInput` — just construct that instead. Add a converter or inline:

```go
	streamIn := &bedrockruntime.ConverseStreamInput{
		ModelId:         in.ModelId,
		Messages:        in.Messages,
		System:          in.System,
		InferenceConfig: in.InferenceConfig,
		ToolConfig:      in.ToolConfig,
	}
	streamOut, err := client.ConverseStream(ctx, streamIn)
	if err != nil {
		return bridle.ProviderResult{}, fmt.Errorf("bedrock: ConverseStream: %w", err)
	}
	return extractStreamResult(ctx, streamOut, sink)
```

Drop the existing `extractResult` call. We're keeping `extractResult` as a helper only if needed for fallback testing — or remove it entirely. For now, remove it and inline a new `extractStreamResult`.

- [ ] **Step 2: Implement `extractStreamResult`**

Add this function to `bedrock.go`:

```go
// extractStreamResult drains the ConverseStream event stream into a
// ProviderResult while emitting ModelChunk / ToolCallStart events to sink
// as they arrive. Event shapes per the Bedrock Converse streaming docs:
//
//   - MessageStart           — top-level role
//   - ContentBlockStart      — begin of a content block (text or tool_use)
//   - ContentBlockDelta      — partial text or partial tool_use input JSON
//   - ContentBlockStop       — end of a content block
//   - MessageStop            — top-level stop_reason
//   - Metadata               — usage + trace
//
// Each ContentBlock has an Index that ties Start/Delta/Stop together.
func extractStreamResult(ctx context.Context, out *bedrockruntime.ConverseStreamOutput, sink bridle.EventSink) (bridle.ProviderResult, error) {
	defer out.GetStream().Close()

	var (
		finalText    strings.Builder
		toolCalls    []bridle.ToolInvocation
		sessionDelta []bridle.SessionEvent
		usage        bridle.Usage
		rawStop      string
	)

	// Accumulate per-block state, keyed by block index.
	type blockState struct {
		kind      string // "text" | "tool_use"
		toolID    string
		toolName  string
		toolInput strings.Builder // accumulates partial JSON deltas
		textBuf   strings.Builder // accumulates partial text deltas (for session event)
	}
	blocks := map[int32]*blockState{}

	for event := range out.GetStream().Events() {
		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs := &blockState{}
			if tu, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				bs.kind = "tool_use"
				bs.toolID = aws.ToString(tu.Value.ToolUseId)
				bs.toolName = aws.ToString(tu.Value.Name)
				sink.Emit(bridle.ToolCallStart{ID: bs.toolID, Name: bs.toolName})
			} else {
				bs.kind = "text"
			}
			blocks[idx] = bs

		case *types.ConverseStreamOutputMemberContentBlockDelta:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs, ok := blocks[idx]
			if !ok {
				// Stream out of order — initialize as text (most common)
				bs = &blockState{kind: "text"}
				blocks[idx] = bs
			}
			switch d := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				sink.Emit(bridle.ModelChunk{Text: d.Value})
				finalText.WriteString(d.Value)
				bs.textBuf.WriteString(d.Value)
			case *types.ContentBlockDeltaMemberToolUse:
				bs.toolInput.WriteString(aws.ToString(d.Value.Input))
			}

		case *types.ConverseStreamOutputMemberContentBlockStop:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			bs, ok := blocks[idx]
			if !ok {
				continue
			}
			switch bs.kind {
			case "text":
				if bs.textBuf.Len() > 0 {
					sessionDelta = append(sessionDelta, bridle.SessionEvent{
						Provider: bridle.ProviderBedrock,
						Role:     bridle.RoleAssistant,
						Content:  bs.textBuf.String(),
					})
				}
			case "tool_use":
				args := json.RawMessage(bs.toolInput.String())
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				toolCalls = append(toolCalls, bridle.ToolInvocation{
					ID:   bs.toolID,
					Name: bs.toolName,
					Args: args,
				})
				raw, _ := json.Marshal(map[string]any{
					"toolUseId": bs.toolID,
					"name":      bs.toolName,
					"input":     args,
				})
				sessionDelta = append(sessionDelta, bridle.SessionEvent{
					Provider: bridle.ProviderBedrock,
					Role:     bridle.RoleAssistant,
					RawJSON:  raw,
				})
			}
			delete(blocks, idx)

		case *types.ConverseStreamOutputMemberMessageStop:
			rawStop = string(ev.Value.StopReason)

		case *types.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				usage.InputTokens = int(aws.ToInt32(ev.Value.Usage.InputTokens))
				usage.OutputTokens = int(aws.ToInt32(ev.Value.Usage.OutputTokens))
				if ev.Value.Usage.CacheReadInputTokens != nil {
					usage.CacheReadInputTokens = int(aws.ToInt32(ev.Value.Usage.CacheReadInputTokens))
				}
				if ev.Value.Usage.CacheWriteInputTokens != nil {
					usage.CacheCreationInputTokens = int(aws.ToInt32(ev.Value.Usage.CacheWriteInputTokens))
				}
			}
		}
	}

	if err := out.GetStream().Err(); err != nil {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: stream: %w", err)
	}

	// Safety stops: return empty result alongside error (see B2a contract).
	if rawStop == string(types.StopReasonGuardrailIntervened) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: guardrail_intervened: response blocked by configured guardrail")
	}
	if rawStop == string(types.StopReasonContentFiltered) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonError},
			fmt.Errorf("bedrock: content_filtered: response blocked by content filter")
	}

	return bridle.ProviderResult{
		FinalText:    finalText.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		StopReason:   bridle.StopReason(normalize.BedrockStopReason(rawStop)),
		SessionDelta: sessionDelta,
	}, nil
}
```

Add `"strings"` to the imports if not already present.

- [ ] **Step 3: Verify the AWS SDK type names**

The `types.ConverseStreamOutputMember*` names above are best-effort. Verify with:

Run: `cd /Users/jacinta/Source/bridle && go doc -all github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types | grep -A 1 "ConverseStreamOutputMember\|ContentBlockStartMember\|ContentBlockDeltaMember"`

Adjust the type assertions in the switch above to match exactly.

- [ ] **Step 4: Verify build**

Run: `cd /Users/jacinta/Source/bridle && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/bedrock/bedrock.go
git commit -m "feat(bedrock): stream tokens via ConverseStream

Replace the synchronous Converse call with ConverseStream. Text deltas
emit ModelChunk events as they arrive; tool_use blocks accumulate
partial JSON across deltas and emit a single ToolCallStart at block-start.
Stop reason and usage are pulled from MessageStop / Metadata events
at end of stream.

Cache token counts (CacheReadInputTokens / CacheWriteInputTokens) are
extracted from the Metadata event and surface on Usage exactly like
the non-streaming path."
```

### Task B7: Unit tests against fake `converseClient`

**Files:**
- Create: `/Users/jacinta/Source/bridle/provider/bedrock/bedrock_test.go`

- [ ] **Step 1: Write a fake client + table-driven tests**

```go
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

type fakeClient struct {
	converseResp       *bedrockruntime.ConverseOutput
	converseStreamResp *bedrockruntime.ConverseStreamOutput
	err                error
	lastInput          *bedrockruntime.ConverseStreamInput
}

func (f *fakeClient) Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return f.converseResp, f.err
}

func (f *fakeClient) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	f.lastInput = in
	return f.converseStreamResp, f.err
}

type recordingSink struct {
	events []bridle.Event
}

func (r *recordingSink) Emit(e bridle.Event) {
	r.events = append(r.events, e)
}

func TestToBedrockMessages_FoldsConsecutiveUserBlocks(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "assistant", ToolCalls: []bridle.ToolInvocation{
			{ID: "t1", Name: "f", Args: json.RawMessage(`{"x":1}`)},
		}},
		{Role: "tool_result", ToolCallID: "t1", Content: "ok"},
		{Role: "user", Content: "anything new?"},
	}
	out, err := toBedrockMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: assistant{tool_use}, user{tool_result + text} — two messages.
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
	if out[0].Role != types.ConversationRoleAssistant {
		t.Errorf("msg 0 role = %v, want assistant", out[0].Role)
	}
	if out[1].Role != types.ConversationRoleUser {
		t.Errorf("msg 1 role = %v, want user", out[1].Role)
	}
	if len(out[1].Content) != 2 {
		t.Errorf("user msg should have tool_result + text = 2 blocks, got %d", len(out[1].Content))
	}
}

func TestToBedrockMessages_ReconstructsAssistantToolUse(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "user", Content: "do thing"},
		{Role: "assistant", Content: "calling", ToolCalls: []bridle.ToolInvocation{
			{ID: "t1", Name: "do", Args: json.RawMessage(`{"a":2}`)},
		}},
	}
	out, err := toBedrockMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Role != types.ConversationRoleAssistant {
		t.Fatalf("expected assistant turn at index 1, got %+v", out)
	}
	// assistant should have text + tool_use blocks
	if len(out[1].Content) != 2 {
		t.Errorf("assistant should have 2 blocks (text + tool_use), got %d", len(out[1].Content))
	}
}

func TestToBedrockMessages_CachePointOnLastUser(t *testing.T) {
	msgs := []bridle.ProviderMessage{
		{Role: "user", Content: "hello"},
	}
	out, err := toBedrockMessages(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Content) != 2 {
		t.Errorf("user msg with caching should have text + cache_point = 2 blocks, got %d", len(out[0].Content))
	}
	if _, ok := out[0].Content[1].(*types.ContentBlockMemberCachePoint); !ok {
		t.Errorf("last content block should be CachePoint, got %T", out[0].Content[1])
	}
}

func TestProvider_StreamError(t *testing.T) {
	p := NewWithClient(&fakeClient{err: errors.New("network down")})
	sink := &recordingSink{}
	_, err := p.RunTurn(context.Background(), bridle.ProviderRequest{
		Model: "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []bridle.ProviderMessage{
			{Role: "user", Content: "hi"},
		},
	}, sink)
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run tests**

Run: `cd /Users/jacinta/Source/bridle && go test ./provider/bedrock/ -v`
Expected: PASS for all tests.

If `bridle.Event` is an interface in `events.go` and `Emit` signature differs, adjust the recordingSink. Verify with `go doc github.com/CarriedWorldUniverse/bridle.EventSink`.

- [ ] **Step 3: Commit**

```bash
git add provider/bedrock/bedrock_test.go
git commit -m "test(bedrock): fold/reconstruct/cache-point unit tests + fake client

Covers the three trickiest behaviours of toBedrockMessages:
- consecutive user-role content (tool_result + user_text) folded into
  one Bedrock user message (the reviewer-flagged bug)
- assistant tool_use reconstruction from ProviderMessage.ToolCalls
  (the multi-turn fix)
- cache point inserted at the end of the last user message when
  EnablePromptCaching is true

A minimal converseClient fake lets these run without AWS credentials."
```

### Task B8: Full test pass + dispatch reviewer

- [ ] **Step 1: Run full suite + vet + build**

```bash
cd /Users/jacinta/Source/bridle
go vet ./... && go test ./... && go build ./...
```

Expected: clean across the board.

- [ ] **Step 2: Push branch + open PR**

```bash
git checkout -b feat/bedrock-tier-2
git push -u origin feat/bedrock-tier-2
gh pr create --title "feat(bedrock): tier 2 — streaming, caching, multi-turn tools, enterprise endpoint" --body "$(cat <<'EOF'
## Summary

Brings the bedrock provider to production usability for the Claude-on-Bedrock pattern (enterprise auth + multi-turn tool conversations + token streaming).

**Depends on:** \`feat/provider-message-toolcalls\` (PR A) — merge that first.

**Changes:**
- Streaming via \`ConverseStream\` (ModelChunk emits per delta)
- Inference config: MaxTokens (default 4096), Temperature, TopP, StopSequences exposed on Provider
- Prompt caching via CachePointBlock at three positions (system / tools / last user); cache token counts surface on Usage
- ToolChoice support: \"\" / \"auto\" / \"any\" / \"none\" / <tool-name>
- Multi-turn tool conversations: assistant tool_use reconstructed from ProviderMessage.ToolCalls (matches PR A)
- Enterprise endpoint + HTTPClient fields: SigV4 against a corporate gateway URL with a custom CA bundle is now first-class
- Fixed reviewer findings: blocked-turn returns empty ProviderResult{}; tool_result + user text now fold into a single Bedrock user message
- Unit tests against a fake converseClient — no AWS credentials needed

## Test plan

- [x] \`go test ./provider/bedrock/\` — fold / reconstruct / cache-point covered
- [x] \`go test ./...\` — full suite green
- [ ] Manual smoke against real Bedrock (Claude Sonnet, with tools, multi-turn) — operator's work env
EOF
)"
```

- [ ] **Step 3: Run code reviewer**

Dispatch `feature-dev:code-reviewer` against the diff. Use this prompt:

> Review feat/bedrock-tier-2 against main. Repo at /Users/jacinta/Source/bridle. Focus: Bedrock Converse streaming correctness (event type assertions, accumulator logic), cache point placement (matches Bedrock's 4-point limit), inference config wiring, ToolChoice mapping, and that the fold-consecutive-user-blocks fix really resolves the consecutive same-role issue. Confirm no AWS credentials are needed to run the unit tests. Report only high-confidence issues. Under 250 words.

- [ ] **Step 4: Address findings, then merge if reviewer passes**

```bash
gh pr merge --squash --delete-branch
```

---

## Out of scope for this plan (follow-ups)

- **Cross-turn `SessionTail` reconstruction.** `lowerRequest` in `run.go` walks `SessionTail` and discards `RawJSON`. When an aspect resumes a thread mid-tool-conversation, the assistant `tool_use` history won't reconstruct from session storage. Fix: parse `SessionEvent.RawJSON` into `ProviderMessage.ToolCalls` in `lowerRequest` for assistant events. Separate PR.
- **Guardrail config / image / document content blocks / performance config.** Tier 3 surface. Punt until there's a concrete caller.
- **Streaming `ConverseStream` for non-Anthropic Bedrock models.** Nova / Llama / Mistral on Bedrock all flow through the same Converse API; should Just Work, but unverified. Note in the PR body.
