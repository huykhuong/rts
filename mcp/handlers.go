package mcp

import (
	"encoding/json"
	"fmt"
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

	searchPath, _ := stringArg(args, "path")
	fileGlob, _ := stringArg(args, "file_glob")

	resultText, err := tool.SearchText(pattern, searchPath, fileGlob)
	if err != nil {
		sendError(id, fmt.Sprintf("Search failed: %v", err))
		return
	}

	logger.Log(Event{
		Tool: "search_text",
		Name:	pattern,
		ResponseChars: len(resultText),
	})

	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:	id,
		Result: ToolResult{
			Content: []TextContent{{Type: "text", Text: resultText}},
		},
	}

	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
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
		Tool:		"list_directory",
		Path:		dirPath,
		ResponseChars: len(resultText),
	})
	
	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:	id,
		Result: ToolResult{
			Content: []TextContent{{Type: "text", Text: resultText}},
		},
	}

	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
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