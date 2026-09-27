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
	logger := NewEventLogger(sessionID, cwd)

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
			handleToolsCall(line, msg.ID, cwd, sessionID, cache, indexCache, logger)
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
			Type:       "object",
			Properties: map[string]Property{},
			Required:   []string{},
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

	projectMapTool := Tool{
		Name:        "project_map",
		Description: "Orient yourself in this codebase without exploring it. Returns the derived project map — what kind of project this is, where the code lives, the files past sessions reached for most, the files too large to read whole — plus any findings earlier sessions recorded about this repo. Call this FIRST in a session that needs to understand the project, before list_directory or exploratory reads: it is the map, and every other lookup is easier once you know what exists. Takes no arguments. Requires `rts index` to have been run.",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]Property{},
			Required:   []string{},
		},
	}

	recordInsightTool := Tool{
		Name: "record_insight",
		Description: "Write a finding about this codebase back into RTS, so the NEXT Claude Code session starts already knowing it instead of rediscovering it. " +
			"Use it when the user tells you to note, remember or flag something about the project, and when you have worked out something durable and non-obvious that cost you real exploration — an architectural decision and its reason, a convention the code follows but never states, a gotcha, where a surprising responsibility actually lives. " +
			"Do NOT record unprompted trivia, anything already obvious from a file's name or structure, anything specific to the current task, or a restatement of what the code plainly says — find_symbol already answers that, and a store full of noise is worse than an empty one. " +
			"This stores a note, not a verified fact: it is always presented to future sessions as somebody's conclusion, to be checked rather than cited. If you find an existing finding is wrong, correct it by recording the right one with `supersedes` set to its id, rather than adding a second contradictory note. " +
			"Say in one line what you recorded, after you record it.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"title": {
					Type:        "string",
					Description: "One line naming the finding, specific enough to be useful in a list (e.g. 'Phase pipeline is convergent, not batched').",
				},
				"body": {
					Type:        "string",
					Description: "The finding itself: what it is, and what it rests on. Write it for a session that has none of your current context.",
				},
				"paths": {
					Type:        "array",
					Description: "Optional. Project-relative file paths the finding rests on. A finding whose files are all later deleted is dropped automatically, so linking it is what keeps the store from rotting.",
					Items:       &PropertyItems{Type: "string"},
				},
				"symbols": {
					Type:        "array",
					Description: "Optional. Symbol names the finding concerns (e.g. ['Project::DataLayerManager', 'advance!']), so a future session can jump straight to them with find_symbol.",
					Items:       &PropertyItems{Type: "string"},
				},
				"supersedes": {
					Type:        "string",
					Description: "Optional. The id of an existing finding this one replaces (e.g. 'i7'). The old one is removed in the same write.",
				},
			},
			Required: []string{"title", "body"},
		},
	}

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"tools": []Tool{projectMapTool, skeletonTool, methodTool, fileTool, findSymbolTool, recordInsightTool, statsTool, searchTool, listTool},
		},
	}

	out, _ := json.Marshal(response)
	fmt.Println(string(out))
}

// handleToolsCall executes our Go skeletonizer logic and streams the text back to Claude.
func handleToolsCall(rawRequest []byte, id json.RawMessage, root string, session string, cache *FileCache, indexCache *IndexCache, logger *EventLogger) {
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

	// The knowledge tools are routed first because neither takes a path, and
	// the path-taking branch below treats a missing "path" argument as an
	// error rather than as "this tool doesn't need one".
	if callReq.Params.Name == "project_map" {
		handleProjectMap(id, root, logger)
		return
	}

	if callReq.Params.Name == "record_insight" {
		handleRecordInsight(callReq.Params.Arguments, id, root, session, logger)
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
			Tool:          "view_bionic_skeleton",
			Path:          filePath,
			ResponseChars: len(resultText),
			NaiveChars:    len(data),
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
	resultText := formatSymbolMatches(name, result.Symbols, result.MatchKind, idx)

	logger.Log(Event{
		Tool:          "find_symbol",
		Name:          name,
		Matches:       len(result.Symbols),
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
		ID:      id,
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
func formatSymbolMatches(name string, matches []index.Symbol, matchKind string, idx *index.IndexFile) string {
	if len(matches) == 0 {
		return formatSymbolMiss(name, idx)
	}

	var buf strings.Builder

	// Lead with what was found, never with what was not.
	//
	// This read "No exact match for %q; found 4 symbol(s)..." and a real
	// session took it for a failure — it had been handed four correct
	// answers and went looking elsewhere, paying for follow-up calls it did
	// not need. A namespace match IS the answer to "where is PdfRequester
	// defined?"; the qualified names in the rows below already show the
	// query was unqualified, so the header saying so too bought nothing and
	// cost the result its credibility.
	switch matchKind {
	case "segment", "suffix":
		fmt.Fprintf(&buf, "Found %d symbol(s) matching %q (qualified differently in the code):\n",
			len(matches), name)
	default:
		fmt.Fprintf(&buf, "Found %d symbol(s) named %q:\n", len(matches), name)
	}

	for _, m := range matches {
		// Render the name the way Ruby writes it, because this output gets
		// read and copied. A nested class is Projects::Reports::Controller,
		// never Projects::Reports.Controller — the separators were swapped
		// here, so a class came out with a dot (not valid Ruby) and a class
		// method came out with "::" (also wrong).
		qualified := m.Name
		if m.Container != "" {
			switch m.Kind {
			case "method":
				qualified = m.Container + "#" + m.Name // User#full_name
			case "class_method", "scope":
				qualified = m.Container + "." + m.Name // User.active
			default:
				qualified = m.Container + "::" + m.Name // Report::PdfRequester
			}
		}
		fmt.Fprintf(&buf, "  %-13s %-40s %s:%d\n", m.Kind, qualified, m.Path, m.Line)
	}

	return buf.String()
}

// formatSymbolMiss answers a lookup that found nothing, and tries to answer
// the question behind it rather than just reporting the gap.
//
// The old message blamed staleness on every miss — "if you recently added
// this symbol, run rts index". A real session searched for
// Projects::Reports::PdfDownloadsController, which does not exist under that
// name and never did: the class is ClientPortal::PdfDownloadsController. It
// was told to reindex, which would not have helped, instead of being shown
// the near-miss that would have.
//
// So near-misses come first, drawn from the names actually in the index — the
// same "did you mean" view_bionic_method has offered all along, which was
// simply never wired to the symbol lookup. Staleness is mentioned last, and
// only as one possibility among two.
func formatSymbolMiss(name string, idx *index.IndexFile) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "No symbols named %q found in the index.\n", name)

	// Only a genuinely close name is offered. SuggestNames also returns a
	// consolation list when nothing resembles the query, and a confident
	// "did you mean: User?" in answer to a lookup for something unrelated is
	// worse than admitting the miss — it invites a second wrong call.
	if suggestions, close := suggestSymbols(name, idx); close && len(suggestions) > 0 {
		fmt.Fprintf(&buf, "Did you mean: %s?\n", strings.Join(suggestions, ", "))
		return buf.String()
	}

	buf.WriteString("Nothing similar in the index either — check the spelling, " +
		"or run `rts index` if this symbol was added since the last one.\n")
	return buf.String()
}

// maxSymbolSuggestions bounds the near-miss list. Enough to correct a wrong
// namespace or a typo, short enough not to read as a result set.
const maxSymbolSuggestions = 6

// suggestSymbols finds names close to the query.
//
// The query's last "::" segment is what gets matched, because a wrong guess
// is usually a wrong namespace around a right class name — which is exactly
// the Projects::Reports::PdfDownloadsController case. Suggestions come back
// fully qualified so the correction is visible: you asked for
// Projects::Reports::X, the code has ClientPortal::X.
// The bool reports whether the suggestions are actually close, mirroring
// tool.SuggestNames — see formatSymbolMiss for why the distinction matters.
func suggestSymbols(name string, idx *index.IndexFile) ([]string, bool) {
	if idx == nil {
		return nil, false
	}

	needle := name
	if i := strings.LastIndex(needle, "::"); i >= 0 {
		needle = needle[i+2:]
	}

	// Bare names are what SuggestNames compares; the qualified forms are what
	// gets shown, so both are collected in one pass.
	qualified := map[string][]string{}
	bare := make([]string, 0, len(idx.Symbols))
	seen := map[string]bool{}

	for _, sym := range idx.Symbols {
		if !seen[sym.Name] {
			seen[sym.Name] = true
			bare = append(bare, sym.Name)
		}
		full := sym.Name
		if sym.Container != "" {
			full = sym.Container + "::" + sym.Name
		}
		qualified[sym.Name] = appendUnique(qualified[sym.Name], full)
	}

	// The exact-segment case first, and it is the important one: the query
	// named a real class under a namespace that does not exist. Find cannot
	// answer it — its tiers compare against the whole query string — and
	// tool.SuggestNames deliberately skips exact matches, because in method
	// extraction an exact match would already have been found. Here it is
	// the answer, so it is handled before the fuzzy pass rather than lost
	// between the two.
	if exact := qualified[needle]; len(exact) > 0 {
		if len(exact) > maxSymbolSuggestions {
			exact = exact[:maxSymbolSuggestions]
		}
		return exact, true
	}

	candidates, close := tool.SuggestNames(needle, bare)

	var out []string
	for _, c := range candidates {
		for _, full := range qualified[c] {
			out = append(out, full)
			if len(out) == maxSymbolSuggestions {
				return out, close
			}
		}
	}
	return out, close
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func generateSessionID() string {
	now := time.Now().UTC()
	buf := make([]byte, 2)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("session-%s-%s",
		now.Format("20060102-150405"),
		hex.EncodeToString(buf),
	)
}
