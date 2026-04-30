// Package mcpclient wraps mark3labs/mcp-go to provide the bridle-internal
// MCP client used by direct-api providers.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// Transport identifies the wire transport for an MCP server connection.
// Must match bridle.MCPTransport constants.
type Transport string

const (
	TransportStdio   Transport = "stdio"
	TransportHTTPSSE Transport = "http_sse"
)

// ServerSpec mirrors bridle.MCPServerSpec. It is duplicated here to avoid an
// import cycle between the bridle root package and this internal package.
type ServerSpec struct {
	Name      string
	Transport Transport
	Command   []string
	URL       string
	Env       map[string]string
	Header    map[string]string
}

// ToolDef is a minimal tool descriptor returned from MCP servers,
// matching the shape of bridle.ToolDef without importing it.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Client manages connections to one or more MCP servers.
type Client struct {
	conns []*serverConn
}

type serverConn struct {
	spec  ServerSpec
	cl    *mcplib.Client
	tools []ToolDef
}

// Connect opens connections to all servers in specs, calls Initialize +
// ListTools, and returns a ready Client. The caller must Close() it.
func Connect(ctx context.Context, specs []ServerSpec) (*Client, error) {
	if len(specs) == 0 {
		return &Client{}, nil
	}
	c := &Client{}
	for _, spec := range specs {
		conn, err := connectServer(ctx, spec)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("mcpclient: connect %q: %w", spec.Name, err)
		}
		c.conns = append(c.conns, conn)
	}
	return c, nil
}

func connectServer(ctx context.Context, spec ServerSpec) (*serverConn, error) {
	var cl *mcplib.Client
	var err error

	switch spec.Transport {
	case TransportStdio:
		if len(spec.Command) == 0 {
			return nil, fmt.Errorf("stdio transport requires Command")
		}
		env := envSlice(spec.Env)
		cl, err = mcplib.NewStdioMCPClient(spec.Command[0], env, spec.Command[1:]...)
		if err != nil {
			return nil, fmt.Errorf("stdio client: %w", err)
		}
	case TransportHTTPSSE:
		if spec.URL == "" {
			return nil, fmt.Errorf("http_sse transport requires URL")
		}
		cl, err = mcplib.NewSSEMCPClient(spec.URL)
		if err != nil {
			return nil, fmt.Errorf("sse client: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown transport %q", spec.Transport)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "bridle", Version: "0.1"}
	if _, err := cl.Initialize(ctx, initReq); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	listResp, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

	conn := &serverConn{spec: spec, cl: cl}
	for _, t := range listResp.Tools {
		schema, err := toolSchema(t)
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", t.Name, err)
		}
		conn.tools = append(conn.tools, ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return conn, nil
}

// Tools returns the merged tool surface from all connected servers.
func (c *Client) Tools() []ToolDef {
	var out []ToolDef
	for _, conn := range c.conns {
		out = append(out, conn.tools...)
	}
	return out
}

// Call dispatches a tool call to the server that owns the named tool.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	conn := c.connForTool(name)
	if conn == nil {
		return nil, fmt.Errorf("mcpclient: no server owns tool %q", name)
	}

	var arguments any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return nil, fmt.Errorf("mcpclient: unmarshal args for %q: %w", name, err)
		}
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = arguments

	result, err := conn.cl.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: call %q: %w", name, err)
	}
	if result.IsError {
		return nil, fmt.Errorf("mcpclient: tool %q returned error: %s", name, extractText(result.Content))
	}

	out, err := json.Marshal(result.Content)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: marshal result for %q: %w", name, err)
	}
	return out, nil
}

// IsMCPTool returns true if name is served by one of the connected servers.
func (c *Client) IsMCPTool(name string) bool {
	return c.connForTool(name) != nil
}

// Close shuts down all server connections.
func (c *Client) Close() {
	for _, conn := range c.conns {
		_ = conn.cl.Close()
	}
	c.conns = nil
}

func (c *Client) connForTool(name string) *serverConn {
	for _, conn := range c.conns {
		for _, t := range conn.tools {
			if t.Name == name {
				return conn
			}
		}
	}
	return nil
}

func toolSchema(t mcp.Tool) (json.RawMessage, error) {
	if len(t.RawInputSchema) > 0 {
		return t.RawInputSchema, nil
	}
	return json.Marshal(t.InputSchema)
}

func extractText(contents []mcp.Content) string {
	for _, c := range contents {
		if tc, ok := mcp.AsTextContent(c); ok {
			return tc.Text
		}
	}
	return "(no text content)"
}

// MergeTools merges explicit ToolDefs with MCP-loaded ToolDefs, checking
// for name collisions. The returned slice preserves explicit tools first.
func MergeTools(explicit, mcpTools []ToolDef) ([]ToolDef, error) {
	seen := make(map[string]struct{}, len(explicit))
	for _, t := range explicit {
		seen[t.Name] = struct{}{}
	}
	merged := make([]ToolDef, len(explicit), len(explicit)+len(mcpTools))
	copy(merged, explicit)
	for _, t := range mcpTools {
		if _, dup := seen[t.Name]; dup {
			return nil, collisionError("tool name collision: " + t.Name)
		}
		seen[t.Name] = struct{}{}
		merged = append(merged, t)
	}
	return merged, nil
}

type collisionError string

func (e collisionError) Error() string { return string(e) }

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
