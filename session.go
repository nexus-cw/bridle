package bridle

import "encoding/json"

// SessionRole identifies who produced a session event.
type SessionRole string

const (
	RoleUser      SessionRole = "user"
	RoleAssistant SessionRole = "assistant"
	RoleTool      SessionRole = "tool"
	RoleSystem    SessionRole = "system"
)

// SessionEvent is a single entry in the funnel-owned JSONL session.
// The harness consumes SessionTail on the way in and proposes SessionDelta
// on the way out; the funnel is the sole writer to the JSONL file.
//
// Shape is intentionally minimal for v0.1. It will align with the funnel's
// full JSONL format when that schema is defined.
type SessionEvent struct {
	Role    SessionRole     `json:"role"`
	Content string          `json:"content,omitempty"`
	// RawJSON carries provider-specific blocks (tool_use, tool_result, etc.)
	// that don't fit the plain content field.
	RawJSON json.RawMessage `json:"raw,omitempty"`
}
