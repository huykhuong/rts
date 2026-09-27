package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"rts/index"
	"rts/insight"
)

// asLinkedWorktree makes wtRoot look like a linked git worktree of mainRoot —
// the fixture shape index.ResolveRoot parses: a `.git` file naming a gitdir
// under `<main>/.git/worktrees/<name>`, and a `commondir` file inside it
// pointing back at `<main>/.git`.
func asLinkedWorktree(t *testing.T, mainRoot string) string {
	t.Helper()

	wtRoot := t.TempDir()
	gitdir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0755); err != nil {
		t.Fatalf("mkdir gitdir: %v", err)
	}
	writeInto(t, wtRoot, ".git", "gitdir: "+gitdir+"\n")
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	return wtRoot
}

// TestHandleRecordInsight_WorktreeRootResolvesToMainCheckout is the wiring
// Start does before calling handleRecordInsight: a session whose cwd is a
// linked git worktree records its finding into the main checkout's
// insights.json, so every other worktree (and the main checkout itself) sees
// it too, instead of it being siloed in a .rts/ directory the worktree does
// not have.
func TestHandleRecordInsight_WorktreeRootResolvesToMainCheckout(t *testing.T) {
	mainRoot := knowledgeProject(t)
	// ResolveRoot's signal that redirecting to mainRoot is worthwhile is
	// .rts/symbols.json; knowledgeProject only writes the map.
	writeInto(t, mainRoot, ".rts/symbols.json", `{"version":1,"symbols":[]}`)
	wtRoot := asLinkedWorktree(t, mainRoot)

	resolved := index.ResolveRoot(wtRoot)
	if resolved != mainRoot {
		t.Fatalf("index.ResolveRoot(%s) = %s, want main root %s", wtRoot, resolved, mainRoot)
	}

	captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "Findings recorded from a worktree land in the main checkout",
			"body":  "record_insight is called with the resolved root, not the worktree cwd.",
		}), json.RawMessage("1"), resolved, "test-session", NewEventLogger("test-session", resolved))
	})

	store, err := insight.Load(mainRoot)
	if err != nil {
		t.Fatalf("Load(mainRoot): %v", err)
	}
	if len(store.Insights) != 1 {
		t.Fatalf("main checkout holds %d findings, want 1", len(store.Insights))
	}

	if _, err := os.Stat(filepath.Join(wtRoot, ".rts")); !os.IsNotExist(err) {
		t.Errorf("a .rts directory was created in the worktree itself")
	}
}
