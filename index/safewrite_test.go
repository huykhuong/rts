package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireLock_SecondRunSkips(t *testing.T) {
	root := t.TempDir()

	release, ok, err := AcquireLock(root)
	if err != nil || !ok {
		t.Fatalf("first AcquireLock = %v, %v; want ok", ok, err)
	}

	if _, ok, err := AcquireLock(root); err != nil || ok {
		t.Fatalf("second AcquireLock = %v, %v; want held by the first", ok, err)
	}

	release()
	if _, ok, _ := AcquireLock(root); !ok {
		t.Error("lock was not free after release")
	}
}

func TestAcquireLock_BreaksALockLeftByADeadRun(t *testing.T) {
	// A run killed mid-index leaves its lock behind. Honouring it forever
	// would stop every automatic reindex in the project for good.
	root := t.TempDir()
	if _, ok, _ := AcquireLock(root); !ok {
		t.Fatal("could not take the lock")
	}

	old := time.Now().Add(-2 * indexLockStale)
	if err := os.Chtimes(filepath.Join(root, ".rts", "index.lock"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, ok, err := AcquireLock(root); err != nil || !ok {
		t.Errorf("stale lock was not broken: ok=%v err=%v", ok, err)
	}
}

func TestWriteFileAtomic_ReplacesAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "symbols.json")

	if err := writeFileAtomic(path, []byte("old")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content = %q, %v; want new", got, err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only symbols.json", names)
	}

	if info, _ := os.Stat(path); info.Mode().Perm() != 0644 {
		t.Errorf("mode = %v, want 0644 like the plain write it replaced", info.Mode().Perm())
	}
}
