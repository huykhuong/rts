package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"rts/secrets"
	"rts/tool"
)

// handleSearchText runs a cross-file search via tool.SearchText and
// returns the formatted results. Optional 'path' and 'file_glob' args let Claude Code
// narrow the search to a subdirectory or file type.
func handleSearchText(args map[string]json.RawMessage, id json.RawMessage, logger *EventLogger) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	// The request was scrubbed on the way in, so a pattern that held a key
	// value now holds the placeholder — which, read as a regex, would match
	// unrelated lines. Searching for a key's value is refused outright.
	if strings.Contains(pattern, secrets.Redacted) {
		sendError(id, "Refused: the search pattern contains a protected secret value.")
		return
	}

	searchPath, _ := stringArg(args, "path")
	fileGlob, _ := stringArg(args, "file_glob")

	outcome, err := tool.SearchText(pattern, searchPath, fileGlob)
	if err != nil {
		sendError(id, fmt.Sprintf("Search failed: %v", err))
		return
	}

	logger.Log(Event{
		Tool:          "search_text",
		Name:          pattern,
		Matches:       outcome.TotalMatches,
		ResponseChars: len(outcome.Text),
		NaiveChars:    outcome.NaiveChars,
	})

	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: ToolResult{
			Content: []TextContent{{Type: "text", Text: outcome.Text}},
		},
	}

	out, _ := json.Marshal(resp)
	writeLine(string(out))
}

func HandleListDirectory(args map[string]json.RawMessage, id json.RawMessage, logger *EventLogger) {
	dirPath, _ := stringArg(args, "path")
	depth, _ := intArg(args, "depth")

	resultText, err := tool.ListDirectory(dirPath, depth)
	if err != nil {
		sendError(id, fmt.Sprintf("Directory listing failed: %v", err))
		return
	}

	logger.Log(Event{
		Tool:          "list_directory",
		Path:          dirPath,
		ResponseChars: len(resultText),
	})

	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: ToolResult{
			Content: []TextContent{{Type: "text", Text: resultText}},
		},
	}

	out, _ := json.Marshal(resp)
	writeLine(string(out))
}

func intArg(args map[string]json.RawMessage, key string) (int, error) {
	raw, ok := args[key]
	if !ok {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("argument %q must be an integer: %v", key, err)
	}
	return n, nil
}
