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

// SessionHandle is an opaque reference to provider-side session state.
// The funnel mints handles and maps them to threads; the provider uses the
// ID to resume state (e.g., --resume <session-id> for subprocess-stream).
// For direct-api providers, Handle may be empty; state comes from SessionTail.
type SessionHandle struct {
	ID string // opaque to the funnel; meaningful to the provider
}

// SessionEvent is a single entry in a session's event log.
// The harness consumes SessionTail on the way in and proposes SessionDelta
// on the way out.
type SessionEvent struct {
	Provider ProviderID      `json:"provider,omitempty"` // who produced this event
	Role     SessionRole     `json:"role"`
	Content  string          `json:"content,omitempty"`
	// RawJSON carries provider-specific blocks (tool_use, tool_result, etc.)
	// that don't fit the plain content field. Valid only in conjunction with Provider.
	RawJSON json.RawMessage `json:"raw,omitempty"`
}
