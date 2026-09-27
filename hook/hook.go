package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"rts/brief"
	"rts/index"
)

// PreToolUseInput models the JSON Claude Code sends on stdin. We only
// care about a few fields; tool_input is left as RawMessage because its
// shape differs per tool (file_path for Read, command for Bash).
type PreToolUseInput struct {
	ToolName  string          `json:"tool_name"`
	Cwd       string          `json:"cwd"`
	ToolInput json.RawMessage `json:"tool_input"`

	// HookEventName is how Claude Code says which hook fired. RTS is wired
	// to two of them — PreToolUse to block wasteful reads, SessionStart to
	// hand the session its orientation — and one binary answers both.
	HookEventName string `json:"hook_event_name"`

	// Source is set on SessionStart only: "startup", "resume", "clear" or
	// "compact". It decides whether the brief is worth emitting at all.
	Source string `json:"source"`
}

// readToolInput is the shape of tool_input when tool_name == "Read".
type readToolInput struct {
	FilePath string `json:"file_path"`
}

// bashToolInput is the shape of tool_input when tool_name == "Bash".
type bashToolInput struct {
	Command string `json:"command"`
}

// denyOutput is the JSON we write to stdout for Claude Code to block a tool call.
// Claude Code sees the reason string and (per its own protocol) surfaces
// it as feedback to the model, which then tries an alternative approach.
type denyOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func Run(r io.Reader, w io.Writer) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil
	}

	var input PreToolUseInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil // fail open: allow Claude to move on
	}

	if input.HookEventName == "SessionStart" {
		context := SessionContext(input, time.Now())
		if context == "" {
			return nil
		}
		return writeContext(w, context)
	}

	reason := Decide(input)
	if reason == "" {
		return nil
	}

	return writeDeny(w, reason)
}

// SessionContext builds the orientation text for a starting session, or ""
// when this start does not warrant one.
//
// A resume is skipped on purpose: the transcript comes back with it, brief
// and all, so emitting a second copy pays twice for the same context and puts
// a stale map underneath a fresh one. A compact is NOT skipped — the summary
// that survives it routinely drops the map, which is the moment the session
// starts re-deriving the codebase by hand again.
func SessionContext(input PreToolUseInput, now time.Time) string {
	if os.Getenv("RTS_DISABLE") == "1" {
		return ""
	}

	root := input.Cwd
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return ""
		}
	}

	// A linked git worktree has no .rts/ of its own — see index.ResolveRoot
	// — so a session that starts inside one is oriented from its main
	// checkout's stores instead of starting from nothing.
	root = index.ResolveRoot(root)

	// Refreshed on every start, a resume included: a resumed session skips
	// the brief, but it still calls find_symbol against the index.
	drift, refreshing := refreshIfStale(root)

	switch input.Source {
	case "startup", "clear", "compact", "":
	default:
		return ""
	}

	return brief.Render(root, brief.Options{Now: now, Drift: drift, Refreshing: refreshing})
}

func writeDeny(w io.Writer, reason string) error {
	var out denyOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = reason

	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(encoded))

	return err
}

// contextOutput is the JSON that hands Claude Code text to put in front of a
// starting session. additionalContext is appended to the session's opening
// context rather than to any system prompt — which is the point: the static
// instructions stay byte-identical across sessions and keep being cached,
// and only this, which really does differ per project and per day, is paid
// for.
type contextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func writeContext(w io.Writer, context string) error {
	var out contextOutput
	out.HookSpecificOutput.HookEventName = "SessionStart"
	out.HookSpecificOutput.AdditionalContext = context

	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(encoded))

	return err
}

// denyMessage is the standard reason string we surface to Claude Code.
// It names the alternative tool explicitly so the model has an obvious
// next action.
const denyMessage = "Native file access is disabled for this project. " +
	"Use the view_file MCP tool instead (or view_bionic_skeleton / view_bionic_method for Ruby files)."

// readingVerbs are Bash commands whose primary purpose is to emit file
// contents. Any command starting with one of these is a candidate for
// blocking — the final decision also requires that the target path is
// under cwd.
var readingVerbs = map[string]bool{
	"cat":  true,
	"less": true,
	"more": true,
	"bat":  true,
	"view": true,
	"head": true,
	"tail": true,
	"nl":   true,
	"sed":  true,
	"awk":  true,
}

// bashSegmentSplitter breaks a Bash command line into its top-level
// pipeline segments so we can inspect each segment's leading verb.
// It splits on unquoted `|`, `;`, `&&`, `||`. This is a heuristic —
// it won't handle every exotic shell construct (heredocs, subshells
// with `$(...)`, etc.) but it covers the vast majority of real usage.
var bashSegmentSplitter = regexp.MustCompile(`\s*(?:\|\||&&|[|;])\s*`)

// Decide inspects a PreToolUse payload and returns a deny reason if the
// call should be blocked, or "" to allow it. Pure function — no I/O,
// no side effects — which makes it trivial to test.

// The native Read tool is no longer blocked. Claude Code's Edit/Write
// tools require a prior native Read as a safety precondition, so
// blocking Read broke the natural edit workflow. CLAUDE.local.md still
// tells Claude to prefer view_file for exploratory reads — this is
// a soft-prefer setup rather than a hard block.
//
// Bash reads (cat/sed/head/etc.) remain blocked, since those are
// unpaginated and the primary source of accidental token waste.
func Decide(input PreToolUseInput) string {
	// Kill switch. When RTS is disabled the MCP tools all refuse to work,
	// so the hook must stand down too — otherwise Claude Code is left with
	// no way to read files at all (disabled tools + blocking hook).
	if os.Getenv("RTS_DISABLE") == "1" {
		return ""
	}

	// Only Bash reads are blocked now.
	if input.ToolName != "Bash" {
		return ""
	}

	var t bashToolInput
	if err := json.Unmarshal(input.ToolInput, &t); err != nil {
		return ""
	}
	if checkBashCommand(t.Command, input.Cwd) {
		return denyMessage
	}

	return ""
}

func isUnderCwd(path, cwd string) bool {
	if path == "" || cwd == "" {
		return false
	}

	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}

	// Resolve relative paths against cwd, absolute paths as-is.
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Join(absCwd, path)
	}

	// Ensure trailing separator on cwd so "/cwd" doesn't match "/cwdother".
	cwdWithSep := absCwd + string(filepath.Separator)
	return absPath == absCwd || strings.HasPrefix(absPath, cwdWithSep)
}

// checkBashCommand returns true when the command contains a file-reading
// verb (cat, sed, head, etc.) targeting a path plausibly under cwd.
//
// Permissive detection strategy: for every top-level pipeline segment,
// if the segment starts with a reading verb AND has at least one
// argument that looks like a project-relative path (doesn't start with
// '/', '~', or '-'), we treat it as a project file read and block.
//
// This over-blocks a little (e.g., `cat - | grep foo` reads stdin, not
// a file, but if any arg looks path-like we'll flag it). The tradeoff
// is deliberate — we chose permissive over strict earlier because
// silent bypasses defeat the whole point of the hook.
func checkBashCommand(command, cwd string) bool {
	segments := bashSegmentSplitter.Split(command, -1)
	for _, seg := range segments {
		if segmentReadsProjectFile(seg, cwd) {
			return true
		}
	}
	return false
}

// segmentReadsProjectFile checks a single pipeline segment: does it
// start with a reading verb and reference a path-like argument that
// resolves under cwd?
func segmentReadsProjectFile(segment, cwd string) bool {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return false
	}

	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return false
	}

	verb := fields[0]
	// Strip common prefixes like `sudo`, `command`, `\cat` (leading backslash
	// used to bypass shell aliases).
	verb = strings.TrimPrefix(verb, `\`)
	if !readingVerbs[verb] {
		return false
	}

	// Examine remaining arguments. Any arg that looks like a path pointing
	// into the project is enough to flag this segment.
	for _, arg := range fields[1:] {
		if looksLikeProjectPath(arg, cwd) {
			return true
		}
	}
	return false
}

// looksLikeProjectPath heuristically decides whether a Bash argument is
// a project-file reference we care about. Permissive by design: we err
// toward flagging when uncertain.
func looksLikeProjectPath(arg, cwd string) bool {
	// Strip surrounding quotes if present. Real shells do this too.
	arg = strings.Trim(arg, `"'`)

	if arg == "" {
		return false
	}

	// Flags aren't paths.
	if strings.HasPrefix(arg, "-") {
		return false
	}

	// Home-relative paths (~/foo) are outside the project.
	if strings.HasPrefix(arg, "~") {
		return false
	}

	// Absolute paths: block only if inside cwd.
	if filepath.IsAbs(arg) {
		return isUnderCwd(arg, cwd)
	}

	// Relative paths: resolve against cwd and check.
	// (We already skipped flags and home paths above, so anything
	// else relative is treated as a project path candidate.)
	return isUnderCwd(arg, cwd)
}
