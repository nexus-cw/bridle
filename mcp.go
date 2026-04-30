package bridle

// MCPClientConfig describes how bridle connects to MCP servers and what tool
// surface the model sees from them. The funnel constructs this; bridle consumes it.
// Ignored by subprocess-stream providers (SupportsMCP=false).
type MCPClientConfig struct {
	Servers []MCPServerSpec
}

// MCPServerSpec describes a single MCP server connection.
type MCPServerSpec struct {
	Name      string            // local identifier, used in tool-call provenance
	Transport MCPTransport      // stdio | http_sse
	Command   []string          // argv to spawn the server (stdio only)
	URL       string            // server URL (http_sse only)
	Env       map[string]string // environment variables for spawned server (stdio only)
	Header    map[string]string // request headers (http_sse only)
}

// MCPTransport identifies the wire transport for an MCP server connection.
type MCPTransport string

const (
	MCPTransportStdio   MCPTransport = "stdio"
	MCPTransportHTTPSSE MCPTransport = "http_sse"
)

// ErrToolNameCollision is returned by RunTurn when a tool name appears in both
// TurnRequest.Tools (explicit) and the MCP-loaded tool surface.
var ErrToolNameCollision = errorString("bridle: tool name collision between explicit Tools and MCP-loaded tools")

type errorString string

func (e errorString) Error() string { return string(e) }
