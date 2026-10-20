package mcp

import "encoding/json"

// BaseMessage is the generic shell for any message passing between Claude and Exo.
// Every single request from Claude has a JSON-RPC version, an ID, and a method name.
type BaseMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
}

// Tool represents a capability we are advertising to Claude.
// We use this to tell Claude: "Hey, I have a tool called X, and here is how you use it."
type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema InputSchema      `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are the MCP hints a client reads to decide when a tool may
// run. Claude Code refuses every MCP tool in plan mode unless it declares
// readOnlyHint — which, with no annotations at all, made RTS unusable in plan
// mode and in any read-only session.
type ToolAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint"`
}

// InputSchema uses JSON Schema format to define the arguments our tool requires.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

// Property defines an individual argument (e.g., "path" must be a string,
// or "method_names" must be an array of strings).
type Property struct {
	Type        string         `json:"type"`
	Description string         `json:"description"`
	Items       *PropertyItems `json:"items,omitempty"`
}

// PropertyItems describes the element type when Property.Type is "array".
type PropertyItems struct {
	Type string `json:"type"`
}

// ToolResponse is what we send back to Claude after running a tool.
// MCP expects the answer to be wrapped in a "content" array.
type ToolResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  ToolResult  `json:"result"`
}

type ToolResult struct {
	Content []TextContent `json:"content"`
}

type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}