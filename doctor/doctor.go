// Package doctor implements `rts doctor`, a one-command health check.
//
// The motivating bug class: several past debugging sessions were spent
// chasing behavior that turned out to be a stale binary, a missing symbol
// index, or ripgrep not being resolvable from the MCP server's stripped
// environment. Each of those is a two-second check when you know to make
// it — this subcommand makes all of them at once.
//
// Doctor is a diagnostic, not a gate: it always exits 0, and every check
// degrades to a WARN or FAIL line rather than panicking when the thing it
// inspects is absent.
package doctor

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"rts/index"
	"rts/insight"
	"rts/tool"
)

// staleIndexAfter is how old .rts/symbols.json may get before doctor
// suggests re-running `rts index`. Symbol indexes go stale gradually —
// a week is long enough to avoid nagging, short enough to catch a real
// drift after a refactor.
const staleIndexAfter = 7 * 24 * time.Hour

// status is the verdict for a single check.
type status string

const (
	statusPass status = "PASS"
	statusWarn status = "WARN"
	statusFail status = "FAIL"
)

// check is one line of the report.
type check struct {
	name   string
	status status
	detail string
}

// Run performs every health check and writes a human-readable report to w.
// It never returns an error and the caller should always exit 0 — doctor
// reports problems, it doesn't fail on them.
func Run(w io.Writer, buildTime string) {
	projectRoot, err := os.Getwd()
	if err != nil {
		projectRoot = "."
	}

	// An unresolvable home dir isn't fatal here; the individual checks
	// that need it report it themselves.
	home, _ := os.UserHomeDir()

	report(w, collect(buildTime, projectRoot, home, time.Now()))
}

// collect runs the checks and returns them in report order. Everything it
// needs from the outside world (project root, home dir, current time) is
// passed in, which keeps the whole check suite testable without touching
// the real environment.
func collect(buildTime, projectRoot, home string, now time.Time) []check {
	return []check{
		checkBuild(buildTime, now),
		checkKillSwitch(),
		checkRipgrep(),
		checkIndex(projectRoot, now),
		checkMap(projectRoot, now),
		checkInsights(projectRoot),
		checkLog(home),
		checkRTSDir(home),
	}
}

// checkBuild reports the ldflags-injected build timestamp and how old it
// is. "unknown" means the binary was built with a plain `go build` rather
// than `make install`, which is exactly the situation that produces
// phantom "my fix didn't take effect" debugging.
func checkBuild(buildTime string, now time.Time) check {
	if buildTime == "" || buildTime == "unknown" {
		return check{"build", statusWarn,
			"build time unknown — binary wasn't built via `make install` (ldflags missing)"}
	}

	t, err := time.Parse(time.RFC3339, buildTime)
	if err != nil {
		return check{"build", statusWarn,
			fmt.Sprintf("%s (unparseable timestamp)", buildTime)}
	}

	return check{"build", statusPass,
		fmt.Sprintf("built %s (%s)", buildTime, humanAge(now.Sub(t)))}
}

// checkKillSwitch surfaces RTS_DISABLE, because a forgotten kill switch
// looks exactly like a broken install from the outside.
//
// Caveat worth stating in the output: this reads the environment of the
// doctor process, which is the shell's. The MCP server is launched with
// `env: {}` and reads its own — so the two can legitimately disagree.
func checkKillSwitch() check {
	if os.Getenv("RTS_DISABLE") == "1" {
		return check{"kill switch", statusWarn,
			"RTS_DISABLE=1 in this shell — RTS tools refuse to run and the hook allows everything"}
	}
	return check{"kill switch", statusPass,
		"RTS_DISABLE not set here (note: the MCP server reads its own env block, not this shell)"}
}

// checkRipgrep resolves rg the same way SearchText does, so the report
// reflects what search_text would actually use rather than what `which rg`
// says.
func checkRipgrep() check {
	if path, ok := tool.FindRipgrep(); ok {
		return check{"ripgrep", statusPass, path}
	}
	return check{"ripgrep", statusWarn,
		"not found on PATH or in common install locations; search_text falls back to grep"}
}

// checkIndex inspects .rts/symbols.json: present, parseable, how many
// symbols, and how stale.
func checkIndex(projectRoot string, now time.Time) check {
	path := filepath.Join(projectRoot, ".rts", "symbols.json")

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return check{"symbol index", statusWarn,
			fmt.Sprintf("no index at %s — run `rts index` to enable find_symbol", path)}
	}
	if err != nil {
		return check{"symbol index", statusFail,
			fmt.Sprintf("cannot stat %s: %v", path, err)}
	}

	idx, err := index.Load(projectRoot)
	if err != nil {
		return check{"symbol index", statusFail, fmt.Sprintf("unreadable: %v", err)}
	}

	age := now.Sub(info.ModTime())
	detail := fmt.Sprintf("%d symbols, updated %s", len(idx.Symbols), humanAge(age))

	if age > staleIndexAfter {
		return check{"symbol index", statusWarn, detail + " — consider re-running `rts index`"}
	}
	return check{"symbol index", statusPass, detail}
}

// checkMap inspects .rts/map.json — the derived orientation a session opens
// with — and, more usefully, how far the code has moved since it was built.
//
// Drift is the failure this check exists for. A missing map announces itself
// the moment anyone looks at a session; a map that quietly describes last
// month's codebase does not, and it is worse, because it is believed.
func checkMap(projectRoot string, now time.Time) check {
	path := filepath.Join(projectRoot, ".rts", "map.json")

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return check{"project map", statusWarn,
			fmt.Sprintf("no map at %s — run `rts index` so sessions start oriented", path)}
	}

	pm, err := index.LoadMap(projectRoot)
	if err != nil {
		return check{"project map", statusFail, fmt.Sprintf("unreadable: %v", err)}
	}

	detail := fmt.Sprintf("%d symbols, %d files, built %s",
		pm.Counts.Symbols, pm.Counts.Files, humanAge(now.Sub(pm.GeneratedAt)))

	drift, err := index.CheckDrift(projectRoot)
	if err != nil {
		// No fingerprints beside the index means it was written by an older
		// rts. Say so plainly rather than reporting a confident "fresh".
		return check{"project map", statusWarn,
			detail + " — no content fingerprints; re-run `rts index` to enable drift detection"}
	}

	if drift.Stale() {
		return check{"project map", statusWarn,
			fmt.Sprintf("%s — %s since; re-run `rts index`", detail, drift.Describe())}
	}

	// The touched count is the noise the old mtime check mistook for signal.
	// Naming it explains an otherwise baffling absence of warnings right
	// after a rebase.
	if drift.Touched > 0 {
		return check{"project map", statusPass,
			fmt.Sprintf("%s — up to date (%d file(s) restamped by git, none changed)", detail, drift.Touched)}
	}

	return check{"project map", statusPass, detail}
}

// checkInsights reports the recorded findings.
//
// It is the one store under .rts that `rts index` cannot rebuild, so a parse
// failure here is a FAIL rather than the WARN a missing derived file gets:
// the fix is to recover the file, never to regenerate it.
func checkInsights(projectRoot string) check {
	path := insight.Path(projectRoot)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return check{"findings", statusPass,
			"none recorded yet — Claude writes them with record_insight"}
	}

	store, err := insight.Load(projectRoot)
	if err != nil {
		return check{"findings", statusFail, fmt.Sprintf("unreadable: %v", err)}
	}

	live := insight.Live(projectRoot, store)
	detail := fmt.Sprintf("%s — %d recorded", path, len(store.Insights))

	if dropped := len(store.Insights) - len(live); dropped > 0 {
		detail += fmt.Sprintf(", %d hidden (every file they named is gone)", dropped)
	}

	return check{"findings", statusPass, detail}
}

// checkLog verifies ~/.rts/events.jsonl is appendable and counts the
// events in it. A missing log is a WARN, not a failure — it just means
// nothing has been logged yet.
func checkLog(home string) check {
	if os.Getenv("RTS_LOG") == "0" {
		return check{"event log", statusWarn,
			"RTS_LOG=0 — event logging disabled, get_stats will have nothing to report"}
	}
	if home == "" {
		return check{"event log", statusFail,
			"home directory unknown, so logging is disabled"}
	}

	path := filepath.Join(home, ".rts", "events.jsonl")

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return check{"event log", statusWarn,
			fmt.Sprintf("%s does not exist yet (no events logged)", path)}
	}

	// Appendability is what the logger actually needs — check that
	// directly rather than inferring it from permission bits.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return check{"event log", statusFail,
			fmt.Sprintf("%s is not writable: %v", path, err)}
	}
	_ = f.Close()

	count, err := countLines(path)
	if err != nil {
		return check{"event log", statusWarn,
			fmt.Sprintf("%s is writable but unreadable: %v", path, err)}
	}

	return check{"event log", statusPass,
		fmt.Sprintf("%s — %d event(s)", path, count)}
}

// checkRTSDir confirms ~/.rts exists and can actually be written to, by
// creating and removing a probe file. Permission bits alone lie often
// enough (read-only mounts, ACLs) to be worth the real write.
func checkRTSDir(home string) check {
	if home == "" {
		return check{"~/.rts dir", statusFail, "cannot determine home directory"}
	}

	dir := filepath.Join(home, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return check{"~/.rts dir", statusFail,
			fmt.Sprintf("cannot create %s: %v", dir, err)}
	}

	probe := filepath.Join(dir, ".doctor-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return check{"~/.rts dir", statusFail,
			fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	_ = f.Close()
	_ = os.Remove(probe)

	return check{"~/.rts dir", statusPass, fmt.Sprintf("%s is writable", dir)}
}

// countLines counts newline-delimited records in a file. Used for the
// event count; the log is JSONL so lines == events.
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Event lines are short, but a pathological path value could be long;
	// match the generous buffer the rest of the codebase uses.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	count := 0
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}

// report renders the checks as an aligned PASS/WARN/FAIL list followed by
// a one-line tally.
func report(w io.Writer, checks []check) {
	fmt.Fprintln(w, "rts doctor")
	fmt.Fprintln(w)

	width := 0
	for _, c := range checks {
		if len(c.name) > width {
			width = len(c.name)
		}
	}

	var passed, warned, failed int
	for _, c := range checks {
		fmt.Fprintf(w, "  [%s] %-*s  %s\n", c.status, width, c.name, c.detail)
		switch c.status {
		case statusPass:
			passed++
		case statusWarn:
			warned++
		case statusFail:
			failed++
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d passed, %d warning(s), %d failure(s)\n", passed, warned, failed)
}

// humanAge renders a duration as a coarse, readable age. Precision beyond
// the leading unit isn't useful for a health report.
func humanAge(d time.Duration) string {
	if d < 0 {
		return "in the future"
	}

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
