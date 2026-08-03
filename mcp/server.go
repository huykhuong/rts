package mcp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"rts/index"
	"rts/tool"
)

var buildVersion = "unknown"

// Start loops indefinitely, reading requests from Claude via Stdin
// and responding via Stdout.
func Start(buildTime string) {
	buildVersion = buildTime
	reader := bufio.NewReader(os.Stdin)
	cache := NewFileCache()

	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = "."
	}

	indexCache := NewIndexCache(cwd)

	// Create a new session
	sessionID := generateSessionID()
	logger := NewEventLogger(sessionID)

	for {
		// 1. Read incoming message line from Claude
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return // If the stream breaks (Claude closes), exit gracefully
		}

		// 2. Parse the generic shell to see what method Claude is calling
		var msg BaseMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // Skip malformed JSON strings
		}

		// 3. Route the request to the correct handler function
		switch msg.Method {
		case "initialize":
			handleInitialize(msg.ID)
		case "tools/list":
			handleToolsList(msg.ID)
		case "tools/call":
			handleToolsCall(line, msg.ID, cache, indexCache, logger)
		}
	}
}

// handleInitialize answers the initial handshake when Claude Code starts up.
func handleInitialize(id json.RawMessage) {
	// MCP requires a basic acknowledgment response to prove the server is alive.
	fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"rts","version":%q}}}`+"\n", id, buildVersion)
}

// handleToolsList advertises our skeletonizer tool directly to Claude's brain.
func handleToolsList(id json.RawMessage) {
	skeletonTool := Tool{
		Name:        "view_bionic_skeleton",
		Description: "Use this tool to inspect the structural architecture (classes, modules, methods, macros) of a heavy Ruby or Rails file without reading its entire text contents. Highly recommended for token efficiency.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"path": {
					Type:        "string",
					Description: "The relative workspace path to the Ruby/Rails file.",
				},
			},
			Required: []string{"path"},
		},
	}

	methodTool := Tool{
		Name:        "view_bionic_method",
		Description: "Extract and read the full internal implementation body of one or more methods inside a Ruby or Rails file. Pass every method you need in a single call — the file is scanned once, so requesting 5 methods costs the same I/O as 1. Run view_bionic_skeleton first to find the correct method names.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"path": {
					Type:        "string",
					Description: "The relative workspace path to the file.",
				},
				"method_names": {
					Type:        "array",
					Description: "The names of the methods to extract (e.g., ['calculate_metrics', 'reset_cache']). Pass all methods you need from this file at once.",
					Items:       &PropertyItems{Type: "string"},
				},
			},
			Required: []string{"path", "method_names"},
		},
	}

	fileTool := Tool{
		Name:        "view_file",
		Description: "Read any project file. This is the canonical file-access tool — use this instead of the native Read tool or `cat`/`sed`/`head`/`tail` in Bash. Automatically returns a skeleton for large Ruby files (>=100 lines) and full content for everything else (small Ruby files, Markdown, JSON, ERB, YAML, JS/CSS, config files, etc.). For extracting specific Ruby method bodies from a skeleton, follow up with view_bionic_method.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"path": {
					Type:        "string",
					Description: "The relative workspace path to the file.",
				},
			},
			Required: []string{"path"},
		},
	}

	findSymbolTool := Tool{
		Name:        "find_symbol",
		Description: "Find where a Ruby symbol (class, module, method, class method, scope, or constant) is defined in the project. Returns the file path, line number, and enclosing container for every definition matching the given name. Use this INSTEAD of `grep -rn 'def method_name'` or `grep -rn 'class Foo'` — it's exact, fast, and free of noise. Requires that `rts index` has been run in the project. If the index is stale after significant refactors, re-run `rts index`.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"name": {
					Type:        "string",
					Description: "The exact symbol name to look up (e.g., 'User', 'calculate_metrics', 'active'). Case-sensitive; matches classes, modules, methods, class methods, scopes, and top-level constants.",
				},
			},
			Required: []string{"name"},
		},
	}

	statsTool := Tool{
		Name:        "get_stats",
    Description: "Return a summary of RTS's token savings for the current Claude Code session. Shows how many chars/tokens were saved by using view_bionic_skeleton, view_bionic_method, view_file, and find_symbol vs the naive alternative of reading full files via native Read. Use this when the user asks 'how much did RTS save me' or when you want to self-report the value RTS is providing during a task.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{},
			Required: []string{},
		},
	}

	searchTool := Tool{
    Name:        "search_text",
    Description: "Cross-file text search across the project (like `grep -rn` but bounded and RTS-native). Use this INSTEAD of Bash `grep`/`rg` for finding usages, arbitrary strings, method call sites, config keywords, or any text pattern that isn't a Ruby symbol definition. For finding where a symbol is DEFINED (class/method/constant), use find_symbol instead — it's faster and free of noise. Uses ripgrep under the hood, respects .gitignore, and skips vendor/tmp/log directories.",
    InputSchema: InputSchema{
        Type: "object",
        Properties: map[string]Property{
            "pattern": {
                Type:        "string",
                Description: "The text or regex pattern to search for. Case-sensitive.",
            },
            "path": {
                Type:        "string",
                Description: "Optional. Subdirectory to limit the search to (e.g., 'app/controllers'). Defaults to the project root.",
            },
            "file_glob": {
                Type:        "string",
                Description: "Optional. Restrict to files matching this glob (e.g., '*.rb', '*_test.rb', '*.yml').",
            },
        },
        Required: []string{"pattern"},
    },
	}

	listTool := Tool{
    Name:        "list_directory",
    Description: "List the contents of a directory as a tree, with line counts on each file. Use this INSTEAD of Bash `ls`/`find` to explore project structure or locate where a file lives. Line counts help you pick the right follow-up tool: large files → view_bionic_skeleton, small files → view_file. Skips noise directories (.git, node_modules, vendor, tmp, log). Bounded depth to keep output manageable.",
    InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
					"path": {
							Type:        "string",
							Description: "Optional. The directory to list (e.g., 'app/controllers/projects'). Defaults to the project root.",
					},
					"depth": {
							Type:        "integer",
							Description: "Optional. How many levels deep to list (1 = immediate contents only). Defaults to 2, capped at 4.",
					},
			},
			Required: []string{},
    },
	}

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"tools": []Tool{skeletonTool, methodTool, fileTool, findSymbolTool, statsTool, searchTool, listTool},
		},
	}

	out, _ := json.Marshal(response)
	fmt.Println(string(out))
}

// handleToolsCall executes our Go skeletonizer logic and streams the text back to Claude.
func handleToolsCall(rawRequest []byte, id json.RawMessage, cache *FileCache, indexCache *IndexCache, logger *EventLogger) {
	// Use RawMessage for arguments so we can decode heterogeneous field types
	// (string for "path", []string for "method_names").
	var callReq struct {
		Params struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(rawRequest, &callReq); err != nil {
		sendError(id, fmt.Sprintf("Invalid request payload: %v", err))
		return
	}

	// Kill switch: RTS_DISABLE=1 turns every tool into a hard error pointing
	// at the native equivalent. We deliberately do NOT fall back to doing the
	// naive work ourselves — the point of the flag is a clean "no RTS"
	// baseline for A/B measurement and for isolating RTS while debugging.
	// Must run before any dispatch.
	//
	// Note: the MCP config launches the server with `env: {}`, so the flag
	// has to be set in that env block — exporting it in the shell is not
	// enough for the spawned process to see it.
	if os.Getenv("RTS_DISABLE") == "1" {
		sendError(id, fmt.Sprintf(
			"RTS is disabled (RTS_DISABLE=1); the %q tool is unavailable. "+
				"Use the native tools instead: Read for files, Grep for text search, Glob/LS for directory listing.",
			callReq.Params.Name))
		return
	}

	// We will try to do find_symbol first so Claude Code doesn't open a file it doesnt need
	if callReq.Params.Name == "find_symbol" {
		handleFindSymbol(callReq.Params.Arguments, id, indexCache, logger)
		return
	}

	if callReq.Params.Name == "get_stats" {
		handleGetStats(id, logger)
		return
	}

	if callReq.Params.Name == "search_text" {
		handleSearchText(callReq.Params.Arguments, id, logger)
    return
	}

	if callReq.Params.Name == "list_directory" {
		HandleListDirectory(callReq.Params.Arguments, id, logger)
		return
	}

	filePath, err := stringArg(callReq.Params.Arguments, "path")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	data, _, err := cache.Read(filePath)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not read file: %v", err))
		return
	}

	var resultText string

	switch callReq.Params.Name {
	case "view_bionic_skeleton":
		var buf bytes.Buffer
		if err := tool.Process(bytes.NewReader(data), &buf); err != nil {
			sendError(id, fmt.Sprintf("Error parsing file: %v", err))
			return
		}
		resultText = buf.String()

		logger.Log(Event{
			Tool: "view_bionic_skeleton",
			Path: filePath,
			ResponseChars: len(resultText),
			NaiveChars: len(data),
		})

	case "view_bionic_method":
		methodNames, err := stringArrayArg(callReq.Params.Arguments, "method_names")
		if err != nil {
			sendError(id, err.Error())
			return
		}
		if len(methodNames) == 0 {
			sendError(id, "method_names must contain at least one method name")
			return
		}

		results, err := tool.ExtractMethods(bytes.NewReader(data), methodNames) // ← NEW: bytes.NewReader
		if err != nil {
			sendError(id, fmt.Sprintf("Error extracting methods: %v", err))
			return
		}
		resultText = formatMethodResults(methodNames, results, data)
		
		logger.Log(Event{
			Tool:          "view_bionic_method",
			Path:          filePath,
			ResponseChars: len(resultText),
			NaiveChars:    len(data),
		})

	case "view_file":
		var buf bytes.Buffer
		if err := tool.ViewFile(filePath, bytes.NewReader(data), &buf); err != nil { // ← NEW: bytes.NewReader
			sendError(id, fmt.Sprintf("Error reading file: %v", err))
			return
		}
		resultText = buf.String()

		logger.Log(Event{
			Tool:          "view_file",
			Path:          filePath,
			ResponseChars: len(resultText),
			NaiveChars:    len(data),
		})

	default:
		sendError(id, fmt.Sprintf("Unknown tool: %q", callReq.Params.Name))
		return
	}

	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: ToolResult{
			Content: []TextContent{
				{Type: "text", Text: resultText},
			},
		},
	}

	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
}

// formatMethodResults renders the extracted methods in the order they were
// requested, with a clear header per method. Missing methods are reported
// at the bottom so Claude sees which ones failed rather than silently
// getting fewer results than requested.
//
// Each miss carries a "did you mean" line built from the names actually
// defined in the file, so a typo or a guessed name costs one call instead
// of a skeleton round-trip. `source` is the file's raw bytes; it is only
// re-scanned when something is missing, keeping the happy path free.
func formatMethodResults(requested []string, results map[string]string, source []byte) string {
	var buf strings.Builder
	var missing []string

	for _, name := range requested {
		body, ok := results[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		fmt.Fprintf(&buf, "# === method: %s ===\n%s\n", name, body)
	}

	if len(missing) == 0 {
		return buf.String()
	}

	fmt.Fprintf(&buf, "# === not found: %s ===\n", strings.Join(missing, ", "))

	// Suggestion pool: every method/test name the file actually defines.
	// A scan failure here must not lose the not-found report, so the error
	// is swallowed and we simply offer no suggestions.
	candidates, err := tool.ListDefinitions(bytes.NewReader(source))
	if err != nil || len(candidates) == 0 {
		return buf.String()
	}

	for _, name := range missing {
		suggestions, isClose := tool.SuggestNames(name, candidates)
		if len(suggestions) == 0 {
			continue
		}
		if isClose {
			fmt.Fprintf(&buf, "# %s — did you mean: %s?\n", name, strings.Join(suggestions, ", "))
		} else {
			fmt.Fprintf(&buf, "# %s — no close match; did you mean one of: %s?\n",
				name, strings.Join(suggestions, ", "))
		}
	}

	return buf.String()
}

// stringArg pulls a required string argument out of the arguments map.
func stringArg(args map[string]json.RawMessage, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument: %q", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("argument %q must be a string: %v", key, err)
	}
	return s, nil
}

// stringArrayArg pulls a required []string argument out of the arguments map.
func stringArrayArg(args map[string]json.RawMessage, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("missing required argument: %q", key)
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("argument %q must be an array of strings: %v", key, err)
	}
	return arr, nil
}

func sendError(id json.RawMessage, errMsg string) {
	fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":%q}}`+"\n", id, errMsg)
}

// handleFindSymbol queries the symbol index and returns a formatted
// list of matching definitions. Errors (missing index, corrupted index)
// come back as MCP errors with actionable hints — usually "run rts index".
func handleFindSymbol(args map[string]json.RawMessage, id json.RawMessage, indexCache *IndexCache, logger *EventLogger) {
	name, err := stringArg(args, "name")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	idx, err := indexCache.Get()
	if err != nil {
		sendError(id, err.Error())
		return
	}

	result := index.Find(idx, name)
	resultText := formatSymbolMatches(name, result.Symbols, result.MatchKind)

	logger.Log(Event{
		Tool: "find_symbol",
		Name:	name,
		Matches: len(result.Symbols),
		ResponseChars: len(resultText),
	})

	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:				id,
		Result: 	ToolResult{
			Content: []TextContent{{Type: "text", Text: resultText}},
		},
	}

	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
}

func handleGetStats(id json.RawMessage, logger *EventLogger) {
	stats, err := ComputeStats(logger.session)
	if err != nil {
			sendError(id, fmt.Sprintf("Could not compute stats: %v", err))
			return
	}

	resultText := formatStats(stats)

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

func formatStats(stats *SessionStats) string {
	var buf strings.Builder

	fmt.Fprintf(&buf, "RTS session stats (%s)\n", stats.SessionID)
	fmt.Fprintf(&buf, "  Build               : %s\n", buildVersion)
	fmt.Fprintln(&buf)

	if stats.Calls == 0 {
		buf.WriteString(" No tool calls logged yet in this session.\n")
		return buf.String()
	}

	// Approximate tokens: 4 chars per token is a rough industry heuristic
  // for English + code. Directional, not exact.

	approxTokensSaved := stats.SavedChars / 4
	
	fmt.Fprintf(&buf, "  Calls               : %d\n", stats.Calls)
	fmt.Fprintf(&buf, "  Response chars sent : %s\n", formatInt(stats.ResponseChars))
	fmt.Fprintf(&buf, "  Naive chars (baseline): %s\n", formatInt(stats.NaiveChars))
	fmt.Fprintf(&buf, "  Net chars saved     : %s", formatInt(stats.SavedChars))

	if stats.NaiveChars > 0 {
		percentage := float64(stats.SavedChars) / float64(stats.NaiveChars) * 100
		fmt.Fprintf(&buf, " (%.0f%% of naive)", percentage)
	}
	
	fmt.Fprintln(&buf)
	fmt.Fprintf(&buf, " Approx tokens saved : %s\n", formatInt(approxTokensSaved))

	// Per-tool breakdown, most-used first
	if len(stats.ByTool) > 0 {
		fmt.Fprintln(&buf)
		buf.WriteString("By tool:\n")
		for _, tool := range sortToolsByCalls(stats.ByTool) {
			ts := stats.ByTool[tool]
			fmt.Fprintf(&buf, " %-22s : %d call(s), %s chars saved\n", tool, ts.Calls, formatInt(ts.Saved))
		}
	}

	return buf.String()
}

// formatInt adds thousands separators to make numbers scannable:
// 124164 → "124,164". Small quality-of-life detail for the summary.
func formatInt(n int) string {
	// Handle negatives cleanly.
	if n < 0 {
			return "-" + formatInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	// Insert commas every 3 digits from the right.
	if len(s) <= 3 {
			return s
	}
	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
			out.WriteString(s[:lead])
			if len(s) > lead {
					out.WriteString(",")
			}
	}
	for i := lead; i < len(s); i += 3 {
			out.WriteString(s[i : i+3])
			if i+3 < len(s) {
					out.WriteString(",")
			}
	}
	return out.String()
}

// sortToolsByCalls returns tool names ordered by call count (descending)
func sortToolsByCalls(byTool map[string]ToolStats) []string {
	names := make([]string, 0, len(byTool))
	for name := range byTool {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if byTool[names[i]].Calls != byTool[names[j]].Calls {
			return byTool[names[i]].Calls > byTool[names[j]].Calls
		}
		return names[i] < names[j]
	})
	
	return names
}

// formatSymbolMatches renders the matched symbols as human-readable text. Empty match list producrs
// a clear not found message so Claude Code knows the lookup succeeded but returned nothing
func formatSymbolMatches(name string, matches []index.Symbol, matchKind string) string {
	if len(matches) == 0 {
		return fmt.Sprintf("No symbols named %q found in the index.\n"+
			"If you recently added this symbol, run `rts index` to refresh.\n", name)
	}

	var buf strings.Builder
	switch matchKind {
	case "segment", "suffix":
		fmt.Fprintf(&buf, "No exact match for %q; found %d symbol(s) by namespace match:\n",
			name, len(matches))
	default:
		fmt.Fprintf(&buf, "Found %d symbol(s) named %q:\n", len(matches), name)
	}

	for _, m := range matches {
		qualified := m.Name
		if m.Container != "" {
			switch m.Kind {
			case "method":
				qualified = m.Container + "#" + m.Name
			case "class":
				qualified = m.Container + "." + m.Name
			case "scope":
				qualified = m.Container + "." + m.Name
			default:
				qualified = m.Container + "::" + m.Name
			}
		}
		fmt.Fprintf(&buf, "  %-13s %-40s %s:%d\n", m.Kind, qualified, m.Path, m.Line)
	}

	return buf.String()
}

func generateSessionID() string{
	now := time.Now()	.UTC()
	buf := make([]byte, 2)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("session-%s-%s",
        now.Format("20060102-150405"),
        hex.EncodeToString(buf),
    )
}