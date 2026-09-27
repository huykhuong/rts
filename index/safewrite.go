package index

// Writes that are safe to make while something else is reading, and a lock
// so two index runs never write at once.
//
// Neither mattered while `rts index` only ran when a person typed it. It now
// also runs by itself, started in the background by the SessionStart hook
// whenever the code has drifted — which means it routinely writes while an
// MCP server in another session is reloading symbols.json, and two sessions
// opening together would both start one.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// writeFileAtomic writes through a temp file in the same directory and a
// rename, so a reader sees either the old file or the new one and never a
// truncated half. IndexCache reloads symbols.json whenever its mtime moves,
// and a reload landing mid-write would fail to parse and hand find_symbol an
// error for that call.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// indexLockStale is how old a lock may get before it is taken to belong to a
// run that died. An index of a four-thousand-file Rails app takes about two
// seconds; five minutes is far past any real run and short enough that a
// killed one never blocks indexing for long.
const indexLockStale = 5 * time.Minute

// AcquireLock takes the project's index lock. ok is false when another run
// holds it, in which case the caller should skip rather than wait: the run
// already in flight is producing exactly the index this one would have.
func AcquireLock(projectRoot string) (release func(), ok bool, err error) {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, false, fmt.Errorf("creating .rts directory: %w", err)
	}

	lockPath := filepath.Join(dir, "index.lock")

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, fmt.Errorf("locking index: %w", err)
		}

		info, statErr := os.Stat(lockPath)
		if statErr != nil || time.Since(info.ModTime()) < indexLockStale {
			return nil, false, nil
		}
		_ = os.Remove(lockPath) // left behind by a run that died; retry once
	}

	return nil, false, nil
}
