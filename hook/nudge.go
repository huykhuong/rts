package hook

// The read-time nudge: when a session reads code a feature map already
// covers, point it at the map — once per feature per session.
//
// The startup brief lists the mapped features, and sessions walk past it.
// A steroidd worker on 2026-10-21 had the list in context, read
// html_generator.rb (a member of five maps) at its ninth tool call, and went
// on to rediscover by hand what those maps held. The list arrives before the
// session knows what its task touches; this arrives at the moment it
// starts touching it.
//
// Dispatch workers get their task from a file outside the repo
// (worker-task.md), so a read of such a file is checked too, and so is each
// message as it is sent, along with any file it points to: a task that
// names a mapped feature, or a file or class in one, is pointed at the map
// before the exploring begins.

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"rts/graph"
	"rts/index"
)

// maxNudgeFeatures bounds the features one nudge names; a file in more maps
// than this is shared plumbing, and the largest few are the useful ones.
const maxNudgeFeatures = 4

// maxTaskFileBytes bounds the outside file read for feature names. A task
// file is a page or two; anything larger is not one.
const maxTaskFileBytes = 256 * 1024

// nudgeTools are the reads that can trigger a nudge.
var nudgeTools = map[string]bool{
	"Read":                           true,
	"mcp__rts__view_file":            true,
	"mcp__rts__view_bionic_skeleton": true,
	"mcp__rts__view_bionic_method":   true,
}

type postToolOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// NudgeContext returns the note to add after a read, or "".
func NudgeContext(input PreToolUseInput, now time.Time) string {
	if os.Getenv("RTS_DISABLE") == "1" || !nudgeTools[input.ToolName] || input.Cwd == "" || input.SessionID == "" {
		return ""
	}

	var args struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(input.ToolInput, &args)
	p := args.FilePath
	if p == "" {
		p = args.Path
	}
	if p == "" {
		return ""
	}

	root := index.ResolveRoot(input.Cwd)
	db, err := graph.OpenExisting(root)
	if err != nil {
		return "" // nothing mapped in this project
	}
	defer func() { _ = db.Close() }()

	checkout := index.CheckoutRoot(input.Cwd)
	var features []graph.FeatureRef
	var subject string
	if rel, ok := projectRelative(p, input.Cwd, checkout); ok {
		features, _ = graph.FeaturesContaining(db, rel)
		subject = rel
	} else if abs := absolute(p, input.Cwd); !insideDir(abs, checkout) {
		features = featuresNamedIn(db, abs)
		subject = filepath.Base(abs)
	}
	if len(features) == 0 {
		return ""
	}

	fresh := freshFeatures(db, input, features, now)
	if len(fresh) == 0 {
		return ""
	}
	return nudgeText(subject, fresh)
}

// PromptContext points a new message at the maps it touches: the features
// it names, and those named by any file it points to — a dispatch worker's
// whole task arrives as "Read <path>/worker-task.md and follow it", and the
// session may read that file with a shell command no PostToolUse nudge sees.
func PromptContext(input PreToolUseInput, now time.Time) string {
	if os.Getenv("RTS_DISABLE") == "1" || input.Cwd == "" || input.SessionID == "" || strings.TrimSpace(input.Prompt) == "" {
		return ""
	}
	root := index.ResolveRoot(input.Cwd)
	db, err := graph.OpenExisting(root)
	if err != nil {
		return ""
	}
	defer func() { _ = db.Close() }()

	checkout := index.CheckoutRoot(input.Cwd)
	features := featuresNamedInText(db, input.Prompt)
	for _, p := range pathsIn(input.Prompt) {
		if rel, ok := projectRelative(p, input.Cwd, checkout); ok {
			more, _ := graph.FeaturesContaining(db, rel)
			features = append(features, more...)
		} else if abs := absolute(p, input.Cwd); !insideDir(abs, checkout) {
			features = append(features, featuresNamedIn(db, abs)...)
		}
	}
	features = uniqueFeatures(features)
	if len(features) == 0 {
		return ""
	}

	fresh := freshFeatures(db, input, features, now)
	if len(fresh) == 0 {
		return ""
	}
	return nudgeLead("this task touches", fresh)
}

// freshFeatures drops the features this session was already pointed at or
// already opened, records the rest as pointed at, and returns those worth
// naming. Every match is recorded, not only the ones named, so the next
// read in the same plumbing does not name the rest one by one.
func freshFeatures(db *sql.DB, input PreToolUseInput, features []graph.FeatureRef, now time.Time) []graph.FeatureRef {
	nudged, _ := graph.Nudged(db, input.SessionID)
	opened := openedFeatures(input.TranscriptPath)
	var fresh []graph.FeatureRef
	for _, f := range features {
		if !nudged[f.Name] && !opened[strings.ToLower(f.Name)] {
			fresh = append(fresh, f)
		}
	}
	if len(fresh) == 0 {
		return nil
	}

	names := make([]string, 0, len(features))
	for _, f := range features {
		names = append(names, f.Name)
	}
	if err := graph.MarkNudged(db, input.SessionID, names, now); err != nil {
		return nil // cannot remember it, so do not say it: it would repeat on every read
	}

	if len(fresh) > maxNudgeFeatures {
		fresh = fresh[:maxNudgeFeatures]
	}
	return fresh
}

// pathPattern finds file paths in a message: absolute, home-relative or
// project-relative, ending in an extension.
var pathPattern = regexp.MustCompile(`(?:~|\.{0,2})?/?[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+\.[A-Za-z0-9]+`)

func pathsIn(text string) []string {
	home, _ := os.UserHomeDir()
	var out []string
	for _, p := range pathPattern.FindAllString(text, -1) {
		if strings.HasPrefix(p, "~/") && home != "" {
			p = filepath.Join(home, p[2:])
		}
		out = append(out, p)
	}
	return out
}

func uniqueFeatures(features []graph.FeatureRef) []graph.FeatureRef {
	seen := map[string]bool{}
	var out []graph.FeatureRef
	for _, f := range features {
		if !seen[f.Name] {
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Members > out[j].Members })
	return out
}

func nudgeText(subject string, features []graph.FeatureRef) string {
	return nudgeLead(subject+" is part of", features)
}

func nudgeLead(lead string, features []graph.FeatureRef) string {
	parts := make([]string, 0, len(features))
	for _, f := range features {
		parts = append(parts, fmt.Sprintf("%q (%d files)", f.Name, f.Members))
	}
	return fmt.Sprintf("RTS: %s mapped feature(s) %s. If your task goes beyond a quick lookup — "+
		"changing this feature or tracing how its parts connect — call touchpoints with the one that matches "+
		"before reading further: it lists that feature's other files, how they connect, and what earlier "+
		"sessions found, so you can skip rediscovering it. For a one-line answer you already have, carry on. "+
		"You will not be reminded about these features again in this session.", lead, strings.Join(parts, ", "))
}

// featuresNamedIn finds the features a file outside the project mentions:
// by feature name, or by a member's path or class name. Short, common words
// are not matched on their own — a task that says "image" is not about the
// Image model.
func featuresNamedIn(db *sql.DB, abs string) []graph.FeatureRef {
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() > maxTaskFileBytes {
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	return featuresNamedInText(db, string(data))
}

// featuresNamedInText finds the features a piece of text mentions.
func featuresNamedInText(db *sql.DB, text string) []graph.FeatureRef {
	lower := strings.ToLower(text)

	rows, err := db.Query(`SELECT f.name, m.ref, m.path,
		(SELECT count(*) FROM members c WHERE c.feature_id = f.id)
		FROM features f JOIN members m ON m.feature_id = f.id`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	matched := map[string]bool{}
	var order []string
	for rows.Next() {
		var name, ref, path string
		var n int
		if rows.Scan(&name, &ref, &path, &n) != nil {
			continue
		}
		if _, seen := counts[name]; !seen {
			order = append(order, name)
		}
		counts[name] = n
		if matched[name] {
			continue
		}
		switch {
		case strings.Contains(lower, strings.ToLower(name)):
			matched[name] = true
		case strings.Contains(path, "/") && strings.Contains(text, path):
			matched[name] = true
		case distinctiveClass(ref) && wordIn(text, ref):
			matched[name] = true
		}
	}

	var out []graph.FeatureRef
	for _, name := range order {
		if matched[name] {
			out = append(out, graph.FeatureRef{Name: name, Members: counts[name]})
		}
	}
	return out
}

// distinctiveClass reports whether a class name is specific enough to mean
// that class when it appears in prose: namespaced, or at least two words
// joined in CamelCase.
func distinctiveClass(ref string) bool {
	if strings.Contains(ref, "/") || strings.Contains(ref, ".") {
		return false
	}
	if strings.Contains(ref, "::") {
		return true
	}
	humps := 0
	for _, r := range ref {
		if unicode.IsUpper(r) {
			humps++
		}
	}
	return humps >= 2
}

func wordIn(text, word string) bool {
	re, err := regexp.Compile(`(^|[^A-Za-z0-9_:])` + regexp.QuoteMeta(word) + `($|[^A-Za-z0-9_])`)
	return err == nil && re.MatchString(text)
}

// openedFeatures returns, lower-cased, the feature names this session has
// already passed to touchpoints.
func openedFeatures(transcriptPath string) map[string]bool {
	opened := map[string]bool{}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return opened
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 1<<20)
	marker := []byte("mcp__rts__touchpoints")
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, marker) {
			var tl transcriptLine
			if json.Unmarshal(line, &tl) == nil {
				var blocks []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Name string `json:"name"`
					} `json:"input"`
				}
				if json.Unmarshal(tl.Message.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "tool_use" && b.Name == "mcp__rts__touchpoints" {
							opened[strings.ToLower(strings.TrimSpace(b.Input.Name))] = true
						}
					}
				}
			}
		}
		if err != nil {
			return opened
		}
	}
}

func absolute(p, cwd string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func insideDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writePostToolContext(w io.Writer, context string) error {
	return writeEventContext(w, "PostToolUse", context)
}

func writeEventContext(w io.Writer, event, context string) error {
	var out postToolOutput
	out.HookSpecificOutput.HookEventName = event
	out.HookSpecificOutput.AdditionalContext = context

	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(encoded))
	return err
}
