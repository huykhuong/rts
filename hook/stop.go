package hook

// The Stop hook: when a session has explored code that no feature map
// covers, ask it — once per area — to save what it learned before it stops.
//
// Waiting for a session to call record_feature on its own does not work.
// record_insight has existed since 2026-10-07 and steroidd holds seven notes;
// the board template exploration that cost 149 turns was thrown away the
// same afternoon. The ask comes at the one moment the session still has the
// understanding in context, which is also the moment it is cheapest to write
// down.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rts/graph"
	"rts/index"
)

const (
	// minAreaFiles is how many files in one area a session must have read
	// before it is asked to map it. Fewer is a glance, not an exploration,
	// and a map written from a glance is the kind that misleads.
	minAreaFiles = 5

	// maxListedFiles bounds the example files named per area in the ask.
	maxListedFiles = 4

	// maxListedAreas bounds the areas the ask names. File-name areas split a
	// cross-cutting feature into many — the bar chart session that built
	// this touched 18 — and the ones with the most files say where it was.
	maxListedAreas = 8
)

// stopOutput blocks the stop, which hands the reason to Claude as the next
// thing to act on.
type stopOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// touch is what a session did to one file.
type touch struct {
	edited bool
}

// area is the files a session touched under one name stem.
type area struct {
	name   string
	files  []string
	edited bool
}

// StopReason returns the ask for a stopping session, or "" to let it stop.
func StopReason(input PreToolUseInput, now time.Time) string {
	if os.Getenv("RTS_DISABLE") == "1" || input.StopHookActive || input.TranscriptPath == "" {
		return ""
	}
	// record_feature writes, so plan mode refuses it. Asking would only
	// spend a turn on a refusal, and marking the area asked would mean the
	// session is never asked once it leaves plan mode.
	if input.PermissionMode == "plan" {
		return ""
	}

	cwd := input.Cwd
	if cwd == "" {
		return ""
	}
	root := index.ResolveRoot(cwd)
	if _, err := os.Stat(filepath.Join(root, ".rts", "symbols.json")); err != nil {
		return "" // record_feature checks names against the index; without one it can only refuse
	}

	touched := scanSession(input.TranscriptPath, cwd, index.CheckoutRoot(cwd))
	if len(touched) == 0 {
		return ""
	}

	var coverage graph.Coverage
	var asked map[string]bool
	if db, err := graph.OpenExisting(root); err == nil {
		coverage, _ = graph.Covered(db, root)
		asked, _ = graph.Asked(db, input.SessionID)
		_ = db.Close()
	}

	areas := uncoveredAreas(touched, coverage, asked)
	if len(areas) == 0 {
		return ""
	}

	db, err := graph.Open(root)
	if err != nil {
		return "" // cannot remember the ask, so do not make it: it would repeat every reply
	}
	defer func() { _ = db.Close() }()

	names := make([]string, 0, len(areas))
	for _, a := range areas {
		names = append(names, a.name)
	}
	if err := graph.MarkAsked(db, input.SessionID, names, now); err != nil {
		return ""
	}

	return askText(areas)
}

// uncoveredAreas groups the touched files no up-to-date map covers by area,
// and keeps the areas worth a map: enough files read to have understood
// something, or any file edited — new code is exactly what no map has yet.
func uncoveredAreas(touched map[string]touch, coverage graph.Coverage, asked map[string]bool) []area {
	byName := map[string]*area{}
	for path, t := range touched {
		// An edited file is never covered. Coverage is checked against the
		// main checkout, and a session in a worktree edits its own copy: the
		// map still matches the main checkout while describing code this
		// session has just changed.
		if !t.edited && coverage.Covers(path) {
			continue
		}
		name := areaOf(path)
		if name == "" || asked[name] {
			continue
		}
		a := byName[name]
		if a == nil {
			a = &area{name: name}
			byName[name] = a
		}
		a.files = append(a.files, path)
		a.edited = a.edited || t.edited
	}

	var out []area
	for _, a := range byName {
		if a.edited || len(a.files) >= minAreaFiles {
			sort.Strings(a.files)
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].files) != len(out[j].files) {
			return len(out[i].files) > len(out[j].files)
		}
		return out[i].name < out[j].name
	})
	return out
}

func askText(areas []area) string {
	var buf strings.Builder
	buf.WriteString("RTS: before you stop, save what you learned in this session as feature maps, so the next session " +
		"gets it from touchpoints instead of exploring again. No map covers these areas you worked in:\n\n")

	shownAreas := areas
	if len(shownAreas) > maxListedAreas {
		shownAreas = shownAreas[:maxListedAreas]
	}
	for _, a := range shownAreas {
		what := fmt.Sprintf("%d file(s) read", len(a.files))
		if a.edited {
			what = fmt.Sprintf("%d file(s), some edited", len(a.files))
		}
		shown := a.files
		if len(shown) > maxListedFiles {
			shown = shown[:maxListedFiles]
		}
		fmt.Fprintf(&buf, "  %s — %s: %s", a.name, what, strings.Join(shown, ", "))
		if len(a.files) > len(shown) {
			fmt.Fprintf(&buf, ", and %d more", len(a.files)-len(shown))
		}
		buf.WriteString("\n")
	}
	if rest := areas[len(shownAreas):]; len(rest) > 0 {
		names := make([]string, 0, len(rest))
		for _, a := range rest {
			names = append(names, a.name)
		}
		fmt.Fprintf(&buf, "  and %d smaller area(s): %s\n", len(rest), strings.Join(names, ", "))
	}

	buf.WriteString("\nCall record_feature once per feature you actually understood. An area is grouped by file name, " +
		"so one feature may span several areas, or one area may not be a feature at all. " +
		"Map only what you read; if you only glanced at an area, skip it and say so in one line. " +
		"If a map already exists under another name, call touchpoints and re-save it with what changed. " +
		"You will not be asked about these areas again in this session.")
	return buf.String()
}

// scanSession collects the files a session read or edited, from its own
// transcript and from the transcripts of the subagents it started — an
// Explore agent's reads are the session's exploration as much as its own.
func scanSession(transcriptPath, cwd, checkout string) map[string]touch {
	touched := map[string]touch{}

	paths := []string{transcriptPath}
	subagents, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents", "*.jsonl"))
	paths = append(paths, subagents...)

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		scanTranscript(f, cwd, checkout, touched)
		_ = f.Close()
	}
	return touched
}

// transcriptLine and toolUse are the parts of a transcript line this reads.
type transcriptLine struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type toolUse struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
	} `json:"input"`
}

// readTools and editTools name the tools whose file argument counts. Searches
// do not: a grep that matched forty files read none of them.
var readTools = map[string]bool{
	"Read":                           true,
	"mcp__rts__view_file":            true,
	"mcp__rts__view_bionic_skeleton": true,
	"mcp__rts__view_bionic_method":   true,
}

var editTools = map[string]bool{
	"Edit":         true,
	"MultiEdit":    true,
	"Write":        true,
	"NotebookEdit": true,
}

func scanTranscript(r io.Reader, cwd, checkout string, touched map[string]touch) {
	reader := bufio.NewReaderSize(r, 1<<20)
	marker := []byte(`"tool_use"`)

	for {
		line, err := reader.ReadBytes('\n')
		// Tool results can make a line megabytes long; most lines carry no
		// tool call, and skipping them unparsed keeps a long session cheap.
		if len(line) > 0 && bytes.Contains(line, marker) {
			var tl transcriptLine
			if json.Unmarshal(line, &tl) == nil {
				var blocks []toolUse
				if json.Unmarshal(tl.Message.Content, &blocks) == nil {
					for _, b := range blocks {
						record(b, cwd, checkout, touched)
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func record(b toolUse, cwd, checkout string, touched map[string]touch) {
	if b.Type != "tool_use" {
		return
	}
	edited := editTools[b.Name]
	if !edited && !readTools[b.Name] {
		return
	}

	p := b.Input.FilePath
	if p == "" {
		p = b.Input.NotebookPath
	}
	if p == "" {
		p = b.Input.Path
	}
	rel, ok := projectRelative(p, cwd, checkout)
	if !ok {
		return
	}

	t := touched[rel]
	t.edited = t.edited || edited
	touched[rel] = t
}

// projectRelative turns a tool's path into one relative to the checkout,
// rejecting anything outside it or in a directory nobody maps.
func projectRelative(p, cwd, checkout string) (string, bool) {
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(checkout, filepath.Clean(p))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") || unmappedDirs[part] {
			return "", false
		}
	}
	return filepath.ToSlash(rel), true
}

var unmappedDirs = map[string]bool{
	"node_modules": true, "vendor": true, "tmp": true, "log": true,
	"coverage": true, "public": true, "builds": true,
}

// roleSuffixes are the Rails file-name endings that say what a file is for,
// not what it is about. board_templates_controller.rb and
// board_template_test.rb are both about board_template.
var roleSuffixes = []string{
	"_controller", "_job", "_helper", "_mailer", "_serializer", "_policy",
	"_component", "_preview", "_decorator", "_presenter",
}

// genericStems are file names that say nothing about the feature: a view
// called index.html.erb is about whatever folder it sits in.
var genericStems = map[string]bool{
	"index": true, "show": true, "new": true, "edit": true, "form": true,
	"create": true, "update": true, "destroy": true, "application": true,
	"base": true, "shared": true, "main": true, "default": true,
	"view": true, "layout": true, "app": true,
}

// areaOf names the area a file belongs to: its file name stripped of
// extensions, a leading partial underscore, its test suffix and its Rails
// role, then made singular. A generic name takes its folder's name instead.
func areaOf(rel string) string {
	rel = filepath.FromSlash(rel)
	dir, stem := filepath.Dir(rel), stemOf(filepath.Base(rel))
	for genericStems[stem] {
		if dir == "." || dir == string(filepath.Separator) {
			return ""
		}
		dir, stem = filepath.Dir(dir), stemOf(filepath.Base(dir))
	}
	return stem
}

func stemOf(base string) string {
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	s := strings.ToLower(strings.TrimPrefix(base, "_"))
	s = strings.TrimLeft(s, "0123456789_") // a deploy task's timestamp names nothing
	for _, suf := range []string{"_test", "_spec"} {
		s = strings.TrimSuffix(s, suf)
	}
	for _, suf := range roleSuffixes {
		s = strings.TrimSuffix(s, suf)
	}
	return singular(s)
}

// singular undoes the plural a Rails controller or folder name carries. It
// handles the regular endings, which is what Rails names are; an irregular
// plural only costs an area being named twice.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"), strings.HasSuffix(s, "is"):
		return s
	case strings.HasSuffix(s, "s") && len(s) > 1:
		return s[:len(s)-1]
	}
	return s
}

func writeStop(w io.Writer, reason string) error {
	encoded, err := json.Marshal(stopOutput{Decision: "block", Reason: reason})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(encoded))
	return err
}
