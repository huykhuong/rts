package hook

// Keeping the index current without anyone having to remember to.
//
// Two weeks of real use showed what a manual `rts index` turns into: an index
// seventeen days old across thirty-eight sessions, because nobody ever ran it.
// find_symbol then misses whatever was added since, and the brief opens every
// session with a map of a codebase that has moved on.
//
// So the SessionStart hook checks for drift and, when there is some, starts
// the reindex itself — detached, so the session never waits on it. On a
// four-thousand-file Rails app a reindex takes about two seconds, and the MCP
// server reloads symbols.json by mtime, so find_symbol is current a moment
// after the session opens.

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"rts/index"
)

// spawnIndex starts `rts index` in root and returns without waiting for it.
// A variable so tests can observe the call without forking a process.
var spawnIndex = func(root string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(exe, "index")
	cmd.Dir = root

	// stdin, stdout and stderr stay nil, which gives the child /dev/null.
	// Inheriting the hook's stdout would be doubly wrong: anything the index
	// printed would corrupt the JSON Claude Code is reading, and the child
	// holding the pipe open would make Claude Code wait for it to finish —
	// the exact delay running it in the background exists to avoid.
	//
	// Its own session, so it outlives the hook process that started it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// refreshIfStale starts a background reindex when the project's index has
// drifted, and reports what it found so the brief does not check twice.
//
// Only a project that has been indexed before is touched. Opening Claude Code
// in some other repo must never leave a .rts directory behind in it — running
// `rts index` once is how a project opts in.
//
// drift is nil when it could not be determined, which means the index predates
// content fingerprints. That case is refreshed too: a rebuild is exactly what
// gives it fingerprints.
func refreshIfStale(root string) (drift *index.Drift, refreshing bool) {
	if _, err := os.Stat(filepath.Join(root, ".rts", "symbols.json")); err != nil {
		return nil, false
	}

	checked, err := index.CheckDrift(root)
	if err == nil {
		drift = &checked
		if !checked.Stale() {
			return drift, false
		}
	}

	if spawnIndex(root) != nil {
		return drift, false
	}
	return drift, true
}
