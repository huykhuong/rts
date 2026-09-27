package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// asLinkedWorktree makes wtRoot look like a linked git worktree of mainRoot,
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
	writeProjectFile(t, wtRoot, ".git", "gitdir: "+gitdir+"\n")
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	return wtRoot
}

// TestSessionContext_WorktreeUsesMainCheckoutsBrief is the 35-of-73 bug: a
// session started inside a linked git worktree, which has no .rts/ of its
// own, must still be handed the main checkout's brief rather than nothing.
func TestSessionContext_WorktreeUsesMainCheckoutsBrief(t *testing.T) {
	resetSpawned(t)
	mainRoot := fullyIndexed(t)
	wtRoot := asLinkedWorktree(t, mainRoot)

	got := SessionContext(PreToolUseInput{HookEventName: "SessionStart", Source: "startup", Cwd: wtRoot}, time.Now())

	if !strings.Contains(got, "rts project map") {
		t.Errorf("worktree session did not get the main checkout's brief:\n%s", got)
	}

	// The refresh path is resolved too: a stale check (or reindex) started
	// from the worktree cwd must run against the main checkout, not spawn a
	// pointless index of the worktree's nonexistent .rts/.
	for _, root := range spawned {
		if root == wtRoot {
			t.Errorf("spawned a reindex of the worktree itself: %v", spawned)
		}
	}
}
