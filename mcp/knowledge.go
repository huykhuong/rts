package mcp

// The knowledge tools: the read that orients a session, and the one write in
// an otherwise read-only server.
//
// Every other RTS tool answers a question about the code as it is. These two
// carry something across the boundary between sessions — which is the only
// cost RTS could not previously touch, because it is not paid inside a single
// session at all. It is paid at the start of every new one.

import (
	"encoding/json"
	"fmt"
	"strings"

	"rts/brief"
	"rts/insight"
)

// handleProjectMap returns the same orientation text the SessionStart hook
// emits: the derived map, the recorded findings, and the drift warning when
// there is drift.
//
// It exists as a tool as well as a hook because the two reach different
// places. The hook cannot reach Claude Code's built-in Explore and Plan
// subagents — a documented upstream limitation RTS has already accepted
// elsewhere — and a subagent is precisely the context that opens knowing
// nothing about the codebase. A tool it can call is the one route in.
func handleProjectMap(id json.RawMessage, root string, logger *EventLogger) {
	text := brief.Render(root, brief.Options{Checkout: checkoutRoot, Full: true})
	if text == "" {
		text = "No project map for this repo yet, and no findings recorded.\n" +
			"Run `rts index` in the project root to build one.\n"
	}

	logger.Log(Event{
		Tool:          "project_map",
		ResponseChars: len(text),
	})

	sendText(id, text)
}

// handleRecordInsight writes one finding into .rts/insights.json.
//
// The finding is stored as a note, never as a fact: everything that reads it
// back says so. That separation is the reason this is safe to expose at all —
// a wrong note costs the next session a check, where a wrong "fact" would
// cost it a wrong answer it never thought to question.
func handleRecordInsight(args map[string]json.RawMessage, id json.RawMessage, root string, session string, logger *EventLogger) {
	title, err := stringArg(args, "title")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	body, err := stringArg(args, "body")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	// Links are optional: a finding about the project as a whole ("migrations
	// are run through bin/deploy, never rails db:migrate") names no file, and
	// demanding one would push the model into inventing a plausible path.
	paths := stringListArg(args, "paths")
	symbols := stringListArg(args, "symbols")
	supersedes, _ := stringArg(args, "supersedes")

	stored, superseded, err := insight.Record(root, insight.Insight{
		Title:   title,
		Body:    body,
		Paths:   paths,
		Symbols: symbols,
		Session: session,
	}, supersedes)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not record that finding: %v", err))
		return
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "Recorded [%s] %s\n", stored.ID, stored.Title)
	// Reported from what the write actually did, never from what was asked
	// for: a mistyped id that silently recorded a second, contradictory note
	// while confirming the correction is the exact failure this store exists
	// to avoid.
	switch {
	case superseded:
		fmt.Fprintf(&buf, "Superseded and removed [%s].\n", supersedes)
	case supersedes != "":
		fmt.Fprintf(&buf, "No finding [%s] to supersede — this was recorded as a new one. "+
			"Check the id against project_map if you meant to replace something.\n", supersedes)
	}
	if len(stored.Paths) > 0 {
		fmt.Fprintf(&buf, "Linked to: %s\n", strings.Join(stored.Paths, ", "))
	}
	if len(stored.Symbols) > 0 {
		fmt.Fprintf(&buf, "Concerns: %s\n", strings.Join(stored.Symbols, ", "))
	}
	buf.WriteString("The next session in this project will see it at startup.\n")

	text := buf.String()

	logger.Log(Event{
		Tool:          "record_insight",
		Name:          stored.ID,
		ResponseChars: len(text),
	})

	sendText(id, text)
}

// stringListArg reads a list argument that may arrive as either a JSON array
// or a single delimited string.
//
// Providers differ on whether an array parameter survives the round trip, and
// a model that sends "a.rb, b.rb" instead of ["a.rb","b.rb"] was previously
// getting every link dropped in silence — the finding recorded, apparently
// fine, with nothing linking it to the code it was about. Both shapes are
// accepted, and the splitting is ours.
//
// A malformed value yields no links rather than an error: links are optional,
// and refusing to record a real finding over the punctuation around its file
// list would trade something valuable for something cosmetic.
func stringListArg(args map[string]json.RawMessage, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}

	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}

	var joined string
	if err := json.Unmarshal(raw, &joined); err != nil {
		return nil
	}

	var out []string
	for _, field := range strings.FieldsFunc(joined, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	}) {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// sendText writes a plain-text tool result. The three handlers here all end
// the same way, and repeating the envelope in each was one more place for the
// JSON-RPC shape to drift.
func sendText(id json.RawMessage, text string) {
	resp := ToolResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: ToolResult{
			Content: []TextContent{{Type: "text", Text: text}},
		},
	}

	out, _ := json.Marshal(resp)
	writeLine(string(out))
}
