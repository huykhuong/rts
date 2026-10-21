package mcp

// The graph tools: record_feature writes what a session worked out about how
// a feature hangs together, and touchpoints hands it to the next session in
// one call instead of an Explore agent.

import (
	"encoding/json"
	"fmt"
	"strings"

	"rts/graph"
)

// handleRecordFeature resolves a feature map against the index and stores it.
//
// Every name is checked before anything is written, and a map with a single
// unresolvable name is refused whole: half a map stored silently would read
// back as the complete one. The refusal lists every problem at once so the
// fix is one resend.
func handleRecordFeature(args map[string]json.RawMessage, id json.RawMessage, root string, session string, indexCache *IndexCache, logger *EventLogger) {
	var f graph.Feature
	var err error

	if f.Name, err = stringArg(args, "feature"); err != nil {
		sendError(id, err.Error())
		return
	}
	if f.Summary, err = stringArg(args, "summary"); err != nil {
		sendError(id, err.Error())
		return
	}
	if raw, ok := args["members"]; ok {
		if err := json.Unmarshal(raw, &f.Members); err != nil {
			sendError(id, fmt.Sprintf(`"members" must be an array of {"ref", "role"} objects: %v`, err))
			return
		}
	}
	if raw, ok := args["edges"]; ok {
		if err := json.Unmarshal(raw, &f.Edges); err != nil {
			sendError(id, fmt.Sprintf(`"edges" must be an array of {"src", "dst", "kind"} objects: %v`, err))
			return
		}
	}
	f.Notes = stringListArg(args, "notes")
	f.Session = session

	idx, err := indexCache.Get()
	if err != nil {
		sendError(id, fmt.Sprintf("record_feature checks every name against the symbol index, and there isn't one: %v", err))
		return
	}

	if problems := graph.Resolve(root, checkoutRoot, idx, &f); len(problems) > 0 {
		var buf strings.Builder
		fmt.Fprintf(&buf, "Nothing was saved. Fix these and send the whole map again:\n")
		for _, p := range problems {
			fmt.Fprintf(&buf, "  - %s\n", p)
		}
		sendError(id, buf.String())
		return
	}

	db, err := graph.Open(root)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not open the graph: %v", err))
		return
	}
	defer func() { _ = db.Close() }()

	// A map that mostly repeats another feature under a new name is the
	// usual way the graph fills with near-duplicates: a session maps what it
	// explored without knowing a map of it exists. It is refused with the
	// feature it overlaps, so the session re-saves that one instead — or
	// says, through distinct_from, that it really is a different feature.
	overlaps, err := graph.Overlaps(db, f)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not check for overlapping features: %v", err))
		return
	}
	if overlaps = graph.Distinct(overlaps, stringListArg(args, "distinct_from")); len(overlaps) > 0 {
		var buf strings.Builder
		fmt.Fprintf(&buf, "Nothing was saved. This map repeats an existing feature:\n")
		for _, o := range overlaps {
			fmt.Fprintf(&buf, "  - %q already has %d of this map's %d files\n", o.Name, o.Shared, o.Of)
		}
		fmt.Fprintf(&buf, "If it is the same feature, call touchpoints with that name and save your additions under it "+
			"(saving a name replaces its map, so send the whole map). If it really is a different feature, "+
			"send this map again with distinct_from set to %q.\n", overlaps[0].Name)
		sendError(id, buf.String())
		return
	}

	replaced, err := graph.Record(db, f)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not save the feature map: %v", err))
		return
	}

	var buf strings.Builder
	verb := "Saved"
	if replaced {
		verb = "Replaced"
	}
	fmt.Fprintf(&buf, "%s feature %q: %d members, %d edges, %d notes.\n", verb, f.Name, len(f.Members), len(f.Edges), len(f.Notes))
	if replaced {
		buf.WriteString("The previous map was kept in history.\n")
	}
	buf.WriteString("Later sessions get it from touchpoints, and see it listed at startup.\n")
	text := buf.String()

	logger.Log(Event{
		Tool:          "record_feature",
		Name:          f.Name,
		Matches:       len(f.Members),
		ResponseChars: len(text),
	})

	sendText(id, text)
}

// handleTouchpoints answers a feature or class name from the graph.
func handleTouchpoints(args map[string]json.RawMessage, id json.RawMessage, root string, indexCache *IndexCache, logger *EventLogger) {
	name, err := stringArg(args, "name")
	if err != nil {
		sendError(id, err.Error())
		return
	}

	depth := 1
	if raw, ok := args["depth"]; ok {
		_ = json.Unmarshal(raw, &depth)
	}

	db, err := graph.OpenExisting(root)
	if err != nil {
		text := "No features are mapped in this project yet.\n" +
			"Explore it as usual, then save what you found with record_feature.\n"
		logger.Log(Event{Tool: "touchpoints", Name: name, ResponseChars: len(text)})
		sendText(id, text)
		return
	}
	defer func() { _ = db.Close() }()

	// A missing index only costs the class-name lookup its fuzzy matching;
	// a feature name still answers.
	idx, _ := indexCache.Get()

	text, err := graph.Touchpoints(db, root, checkoutRoot, idx, name, depth)
	if err != nil {
		sendError(id, fmt.Sprintf("Could not read the graph: %v", err))
		return
	}

	logger.Log(Event{
		Tool:          "touchpoints",
		Name:          name,
		ResponseChars: len(text),
	})

	sendText(id, text)
}
