# bridle

The harness layer of the Nexus funnel/harness split. A small Go library that drives one deliberation turn of one model with one tool surface, emitting a stream of observable events and returning a structured `TurnResult`.

The funnel imports bridle. Aspects do not import bridle directly.

> A bridle controls and directs without being the horse — sits beneath the funnel, governs what the model produces, provider-agnostic.

## Status

Spec drafted, no implementation yet. See [`docs/2026-05-01-bridle-spec.md`](docs/2026-05-01-bridle-spec.md) for the build brief.

## Scope

- One stable provider interface, N implementations: `claude-api`, `ollama-local`, `openai-api`.
- `register_hooks`-style interception surface for §5.6 behaviors.
- `send_comms` is just a tool the funnel supplies — bridle has no special case.
- Funnel owns session JSONL; bridle proposes deltas.

## Non-goals

- Not a framework, not an agent runtime, not a session store. It does one thing: drive one turn.
- See spec §9 for the full non-goals list.

## Reference reading

PydanticAI (`iter()` / `CallToolsNode`), Eino (streaming + tool-call normalization), PI (headless JSON streaming), Strands (`register_hooks`), LangGraph (interrupt/checkpoint).
