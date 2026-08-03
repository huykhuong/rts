package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PreToolUseInput models the JSON Claude Code sends on stdin. We only
// care about a few fields; tool_input is left as RawMessage because its
// shape differs per tool (file_path for Read, command for Bash).
type PreToolUseInput struct {
	ToolName	string	`json:"tool_name"`
	Cwd				string	`json:"cwd"`
	ToolInput json.RawMessage `json:"tool_input"`
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

	reason := Decide(input)
	if reason == "" {
		return nil
	}

	return writeDeny(w, reason)
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