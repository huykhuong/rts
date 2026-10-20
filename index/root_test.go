package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mainWithIndex builds a fake main checkout — a directory .git plus an
// indexed .rts/symbols.json — and returns its root.
func mainWithIndex(t *testing.T, indexed bool) string {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if indexed {
		writeFile(t, root, ".rts/symbols.json", `{"version":1,"symbols":[]}`)
	}
	return root
}

// linkedWorktree builds a fake linked worktree pointing at mainRoot: a `.git`
// file with a `gitdir:` line, the gitdir directory it names, and a
// `commondir` file inside that holding commondirValue. Returns the
// worktree's root.
func linkedWorktree(t *testing.T, mainRoot, commondirValue string) string {
	t.Helper()

	wtRoot := t.TempDir()
	gitdir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0755); err != nil {
		t.Fatalf("mkdir gitdir: %v", err)
	}
	writeFile(t, wtRoot, ".git", "gitdir: "+gitdir+"\n")
	writeFile(t, gitdir, "commondir", commondirValue+"\n")
	return wtRoot
}

func TestResolveRoot_NormalRepoReturnsDirUnchanged(t *testing.T) {
	root := mainWithIndex(t, true)

	if got := ResolveRoot(root); got != root {
		t.Errorf("ResolveRoot(%s) = %s, want unchanged", root, got)
	}
}

func TestResolveRoot_LinkedWorktreeRelativeCommondir(t *testing.T) {
	mainRoot := mainWithIndex(t, true)
	wtRoot := linkedWorktree(t, mainRoot, "../..")

	if got := ResolveRoot(wtRoot); got != mainRoot {
		t.Errorf("ResolveRoot(%s) = %s, want main root %s", wtRoot, got, mainRoot)
	}
}

func TestResolveRoot_LinkedWorktreeAbsoluteCommondir(t *testing.T) {
	mainRoot := mainWithIndex(t, true)
	absoluteGitDir := filepath.Join(mainRoot, ".git")
	wtRoot := linkedWorktree(t, mainRoot, absoluteGitDir)

	if got := ResolveRoot(wtRoot); got != mainRoot {
		t.Errorf("ResolveRoot(%s) = %s, want main root %s", wtRoot, got, mainRoot)
	}
}

func TestResolveRoot_MainCheckoutNotIndexedReturnsWorktreeUnchanged(t *testing.T) {
	// The main checkout exists and is a real linked-worktree target, but it
	// has never been indexed — `.rts/symbols.json` is the signal that
	// redirecting there is actually useful, not just structurally possible.
	mainRoot := mainWithIndex(t, false)
	wtRoot := linkedWorktree(t, mainRoot, "../..")

	if got := ResolveRoot(wtRoot); got != wtRoot {
		t.Errorf("ResolveRoot(%s) = %s, want unchanged (main not indexed)", wtRoot, got)
	}
}

func TestResolveRoot_SubdirectoryInsideWorktreeStillResolves(t *testing.T) {
	mainRoot := mainWithIndex(t, true)
	wtRoot := linkedWorktree(t, mainRoot, "../..")

	sub := filepath.Join(wtRoot, "app", "models")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if got := ResolveRoot(sub); got != mainRoot {
		t.Errorf("ResolveRoot(%s) = %s, want main root %s", sub, got, mainRoot)
	}
}

func TestResolveRoot_NoGitAnywhereReturnsDirUnchanged(t *testing.T) {
	// t.TempDir() is not inside a git repo. findDotGit walks upward toward
	// the filesystem root, so this only stays true so long as the walk
	// terminates there rather than escaping into some real repo above —
	// which is exactly what the fixture below is: nothing but a bare
	// directory, so the walk finds nothing at all before reaching "/".
	root := t.TempDir()

	if got := ResolveRoot(root); got != root {
		t.Errorf("ResolveRoot(%s) = %s, want unchanged", root, got)
	}
}

func TestResolveRoot_MalformedGitFileReturnsDirUnchanged(t *testing.T) {
	wtRoot := t.TempDir()
	writeFile(t, wtRoot, ".git", "not a gitdir line at all\n")

	if got := ResolveRoot(wtRoot); got != wtRoot {
		t.Errorf("ResolveRoot(%s) = %s, want unchanged", wtRoot, got)
	}
}

func TestResolveRoot_MissingCommondirReturnsDirUnchanged(t *testing.T) {
	mainRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mainRoot, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	wtRoot := t.TempDir()
	gitdir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0755); err != nil {
		t.Fatalf("mkdir gitdir: %v", err)
	}
	// No commondir file written inside gitdir.
	writeFile(t, wtRoot, ".git", "gitdir: "+gitdir+"\n")

	if got := ResolveRoot(wtRoot); got != wtRoot {
		t.Errorf("ResolveRoot(%s) = %s, want unchanged (no commondir)", wtRoot, got)
	}
}

// TestResolveRoot_RealGitWorktree exercises the real thing: an actual `git
// worktree add`, parsed without ever invoking git ourselves. Skipped when
// git isn't installed.
func TestResolveRoot_RealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	mainRoot := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = mainRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=rts-test", "GIT_AUTHOR_EMAIL=rts-test@example.com",
			"GIT_COMMITTER_NAME=rts-test", "GIT_COMMITTER_EMAIL=rts-test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q")
	writeFile(t, mainRoot, "README.md", "hello\n")
	run("add", "README.md")
	run("commit", "-q", "-m", "initial")

	wtRoot := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-q", "-b", "wt-branch", wtRoot)

	writeFile(t, mainRoot, ".rts/symbols.json", `{"version":1,"symbols":[]}`)

	got := ResolveRoot(wtRoot)

	// macOS temp paths resolve through a /var -> /private/var symlink; both
	// sides need it applied before comparing.
	wantResolved, err := filepath.EvalSymlinks(mainRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(mainRoot): %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(got=%s): %v", got, err)
	}

	if gotResolved != wantResolved {
		t.Errorf("ResolveRoot(%s) = %s, want main root %s", wtRoot, gotResolved, wantResolved)
	}
}

func TestCheckouts_ListsTheMainCheckoutAndLiveWorktrees(t *testing.T) {
	root := t.TempDir()
	live, gone := t.TempDir(), filepath.Join(t.TempDir(), "removed")

	for name, dir := range map[string]string{"live": live, "gone": gone} {
		rec := filepath.Join(root, ".git", "worktrees", name)
		if err := os.MkdirAll(rec, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rec, "gitdir"), []byte(filepath.Join(dir, ".git")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got := Checkouts(root)
	if len(got) != 2 || got[0] != root || got[1] != live {
		t.Errorf("Checkouts = %v, want [%s %s]", got, root, live)
	}
}
