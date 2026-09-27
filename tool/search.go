// Package tool: search.go adds a cross-file text search capability so
// Claude Code doesn't have to fall back to Bash `grep` for finding
// usages, non-definition strings, or arbitrary text patterns across
// the codebase.
//
// This complements find_symbol: find_symbol answers "where is X
// defined?" via the pre-built index. SearchText answers "where does
// X appear?" via ripgrep (with grep as fallback).
package tool

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ==================== NEW ====================
// commonRipgrepPaths lists locations rg is typically installed to that
// might not be on PATH for this process. MCP servers are often launched
// by the host app with a minimal/empty environment, so exec.LookPath("rg")
// can miss an rg that's genuinely installed — checking these directly
// makes the "prefer rg" behavior actually work in that situation.
var commonRipgrepPaths = []string{
	"/opt/homebrew/bin/rg", // Homebrew, Apple Silicon
	"/usr/local/bin/rg",    // Homebrew, Intel Mac / manual install
	"/usr/bin/rg",          // Linux package managers
	"/bin/rg",
}

// findRipgrep resolves an absolute path to an rg binary, checking PATH
// first and falling back to common install locations.
func findRipgrep() (string, bool) {
	if path, err := exec.LookPath("rg"); err == nil {
		return path, true
	}
	// os.Stat because LookPath on Silicon Mac looks into the PATH
	// while Homebrew is not on PATH by default.
	for _, path := range commonRipgrepPaths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, true
		}
	}
	return "", false
}
// =============================================

// FindRipgrep is the exported wrapper around findRipgrep, so `rts doctor`
// can report which rg (if any) SearchText would actually use. Kept as a
// thin wrapper rather than exporting findRipgrep itself, since resolution
// is an internal detail of this package.
func FindRipgrep() (string, bool) {
	return findRipgrep()
}

// ==================== NEW ====================
// Output caps to prevent flooding Claude Code's context with thousands
// of matches. If we hit either cap, we truncate and note it so Claude
// Code knows to narrow the query.
const (
	searchMaxLines   = 50
	searchMaxMatches = 200
)
// =============================================

// SearchOutcome carries the formatted search output plus the measurement
// data needed to log how much the output cap saved.
//
// NaiveChars is the counterfactual: what a plain Bash `grep` would have
// dumped into context. Text is what we actually returned. On a narrow
// search where nothing is truncated, Text is *larger* than NaiveChars
// (we add a header), so the derived saving is slightly negative. That is
// intended and honest — search_text costs a little on narrow queries and
// wins large on broad ones, the same way view_file does on small files.
type SearchOutcome struct {
	Text         string // formatted output actually returned to Claude
	NaiveChars   int    // byte length of raw rg/grep output, pre-formatting
	TotalMatches int    // match count before line truncation
	Truncated    bool   // whether the line cap kicked in
}

// ==================== NEW ====================
// SearchText runs a cross-file text search for `pattern` starting at
// `searchPath` (defaults to "." if empty), optionally restricted to
// files matching `fileGlob` (e.g. "*.rb"). Returns the formatted
// output ready to send back to Claude Code, plus the measurement data
// describing what the output cap saved.
//
// Uses ripgrep (`rg`) if available; falls back to system `grep`.
// Ripgrep is dramatically preferred — it respects .gitignore, skips
// binary files automatically, and handles Unicode. `grep` is a
// best-effort fallback so RTS works even without rg installed.
func SearchText(pattern, searchPath, fileGlob string) (SearchOutcome, error) {
	if pattern == "" {
		return SearchOutcome{}, fmt.Errorf("pattern is required")
	}
	if searchPath == "" {
		searchPath = "."
	}

	// Prefer ripgrep. findRipgrep checks PATH plus common install
	// locations — that's our signal to try grep only if rg is truly absent.
	if rgPath, ok := findRipgrep(); ok {
		return runRipgrep(rgPath, pattern, searchPath, fileGlob)
	}

	// Fallback path — note in the output that we're using grep so the
	// user can see the tool would be more precise with ripgrep installed.
	// The note lands in Text (Claude pays for it), not in NaiveChars.
	out, err := runGrep(pattern, searchPath, fileGlob)
	if err != nil {
		return out, err
	}
	out.Text = "# note: ripgrep (rg) not found on PATH; used grep as fallback\n" + out.Text
	return out, nil
}
// =============================================

// ==================== NEW ====================
// runRipgrep invokes rg with defaults that mirror how a developer
// would grep interactively: line numbers on, respects .gitignore,
// skips known-uninteresting directories.
//
// Measurement caveat: --max-count caps matches *per file* inside rg
// itself, so the raw output we measure as NaiveChars is already capped
// and understates a truly unbounded search. The cap stays — a
// conservative bias can only understate savings, never overstate them,
// and removing it risks pulling a very large output into memory. Note
// that the grep fallback has no equivalent per-file cap, so its raw
// output is genuinely complete; the two paths' NaiveChars are not
// strictly comparable.
func runRipgrep(rgPath, pattern, searchPath, fileGlob string) (SearchOutcome, error) {
	args := []string{
		"--line-number",
		"--no-heading",   // don't group by file; one match per line
		"--color=never",  // plain output; no ANSI codes
		"--max-count=" + fmt.Sprintf("%d", searchMaxMatches),
		"--glob=!.git",
		"--glob=!.rts",
		"--glob=!node_modules",
		"--glob=!vendor",
		"--glob=!tmp",
		"--glob=!log",
	}
	if fileGlob != "" {
		args = append(args, "--glob="+fileGlob)
	}
	args = append(args, "--", pattern, searchPath)

	cmd := exec.Command(rgPath, args...)
	output, err := cmd.Output()

	// rg exit code 1 = no matches (not an error for us).
	// rg exit code 2 = actual error (bad regex, permission issue, etc).
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return formatEmpty(pattern), nil
		}
		return SearchOutcome{}, fmt.Errorf("ripgrep failed: %w", err)
	}

	return formatResults(pattern, string(output)), nil
}
// =============================================

// ==================== NEW ====================
// runGrep is the fallback when ripgrep isn't installed. Uses portable
// grep flags that work on both macOS BSD grep and Linux GNU grep.
func runGrep(pattern, searchPath, fileGlob string) (SearchOutcome, error) {
	args := []string{
		"-r",             // recursive
		"-n",             // line numbers
		"-E",             // extended regex, so alternation (a|b) works like it does in rg
		"--exclude-dir=.git",
		"--exclude-dir=.rts",
		"--exclude-dir=node_modules",
		"--exclude-dir=vendor",
		"--exclude-dir=tmp",
		"--exclude-dir=log",
	}
	if fileGlob != "" {
		args = append(args, "--include="+fileGlob)
	}
	args = append(args, "--", pattern, searchPath)

	cmd := exec.Command("grep", args...)
	output, err := cmd.Output()

	// grep exit code 1 = no matches (not an error).
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return formatEmpty(pattern), nil
		}
		return SearchOutcome{}, fmt.Errorf("grep failed: %w", err)
	}

	return formatResults(pattern, string(output)), nil
}
// =============================================

// ==================== NEW ====================
// formatResults renders the raw rg/grep output as a bounded, headered
// text block. Truncates at searchMaxLines and marks the truncation so
// Claude Code can tell when to narrow its query.
//
// NaiveChars is len(raw) — the untouched bytes the underlying search
// produced, measured before any formatting or truncation.
func formatResults(pattern, raw string) SearchOutcome {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	total := len(lines)

	truncated := false
	if total > searchMaxLines {
		lines = lines[:searchMaxLines]
		truncated = true
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "# search: %q — %d match(es)\n", pattern, total)
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&buf, "# ... truncated to %d of %d lines — narrow the pattern or use file_glob to reduce results\n",
			searchMaxLines, total)
	}
	return SearchOutcome{
		Text:         buf.String(),
		NaiveChars:   len(raw),
		TotalMatches: total,
		Truncated:    truncated,
	}
}
// =============================================

// ==================== NEW ====================
// formatEmpty is the "no matches found" response — same header shape
// so Claude Code sees consistent output regardless of match count.
//
// NaiveChars stays 0: nothing matched, so there is no baseline to
// compare against. The logger only derives saved_chars when
// NaiveChars > 0, so a zero-match search reports no savings either way.
func formatEmpty(pattern string) SearchOutcome {
	return SearchOutcome{
		Text: fmt.Sprintf("# search: %q — 0 match(es)\n", pattern),
	}
}
// =============================================