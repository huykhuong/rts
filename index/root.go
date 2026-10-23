package index

// Where `.rts/` state lives when the working directory is a linked git
// worktree, not the main checkout.
//
// The user runs many Claude Code sessions inside git worktrees created under
// a project's main checkout (e.g. `.claude/worktrees/<name>/`). `.rts/` is
// gitignored, so a worktree never has one of its own, and every RTS store —
// the symbol index, the project map, the recorded findings — started from
// scratch there. Over two weeks, 35 of 73 sessions ran in such a worktree:
// none got a SessionStart brief, none saw findings a sibling session had
// recorded, and find_symbol told each of them to run `rts index`, as if the
// project had never been indexed at all.
//
// The fix: a linked worktree uses its main checkout's `.rts/` for reading,
// writing and indexing. Symbol paths in the index are relative to the
// project root, so the main checkout's index resolves correctly against a
// worktree's own files — the code is the same code, just checked out twice —
// and findings become shared across every worktree instead of siloed per one.
//
// This walks the on-disk `.git` files directly rather than shelling out to
// `git rev-parse --git-common-dir`: the MCP server is launched with an empty
// environment (`env: {}`), so `git` is not on its PATH there.

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveRoot returns the directory whose `.rts/` RTS should use for dir.
// For a normal repo (or no repo at all) that is dir itself. For a linked
// worktree whose main checkout has already been indexed, it is the main
// checkout's root instead.
//
// Any failure to read or parse the git metadata returns dir unchanged —
// this is a convenience, not a load-bearing lookup, and a wrong guess would
// be worse than falling back to the worktree's own (possibly empty) .rts/.
func ResolveRoot(dir string) string {
	gitPath, ok := findDotGit(dir)
	if !ok {
		return dir
	}

	info, err := os.Stat(gitPath)
	if err != nil {
		return dir
	}

	// A directory .git means dir is a normal repo or the main checkout
	// itself — nothing to redirect.
	if info.IsDir() {
		return dir
	}

	mainRoot, ok := resolveLinkedWorktree(gitPath)
	if !ok {
		return dir
	}

	if _, err := os.Stat(filepath.Join(mainRoot, ".rts", "symbols.json")); err != nil {
		return dir
	}

	return mainRoot
}

// findDotGit walks upward from dir looking for the nearest ancestor
// containing a `.git` entry, stopping at the filesystem root.
func findDotGit(dir string) (path string, ok bool) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}

	for {
		candidate := filepath.Join(current, ".git")
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, true
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", false // reached the filesystem root
		}
		current = parent
	}
}

// resolveLinkedWorktree follows a worktree's `.git` file to the main
// checkout's root.
//
// gitPath holds a line `gitdir: <path>` pointing at
// `<main>/.git/worktrees/<name>`. That directory in turn holds `commondir`,
// a path (often relative, e.g. `../..`) to the main checkout's actual `.git`
// directory. The main checkout root is that common dir's parent.
func resolveLinkedWorktree(gitPath string) (mainRoot string, ok bool) {
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", false
	}

	gitdir, ok := parseGitdirFile(string(data))
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitPath), gitdir)
	}

	commonData, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", false
	}
	commondir := strings.TrimSpace(string(commonData))
	if commondir == "" {
		return "", false
	}
	if !filepath.IsAbs(commondir) {
		commondir = filepath.Join(gitdir, commondir)
	}

	return filepath.Dir(filepath.Clean(commondir)), true
}

// parseGitdirFile extracts the path out of a worktree `.git` file's
// `gitdir: <path>` line.
func parseGitdirFile(contents string) (string, bool) {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		rest, found := strings.CutPrefix(line, "gitdir:")
		if !found {
			continue
		}
		path := strings.TrimSpace(rest)
		if path == "" {
			return "", false
		}
		return path, true
	}
	return "", false
}

// CheckoutRoot returns the top of the git checkout containing dir — the
// worktree's own top for a linked worktree, not the main checkout — or dir
// itself outside a repo. Paths a session touched are relative to this, while
// the stores they are checked against live at ResolveRoot.
func CheckoutRoot(dir string) string {
	gitPath, ok := findDotGit(dir)
	if !ok {
		return dir
	}
	return filepath.Dir(gitPath)
}

// Checkouts returns the main checkout and every linked worktree of the repo
// whose main checkout is root, read from git's own worktree records so it
// needs no git binary. A worktree whose directory is gone is left out.
func Checkouts(root string) []string {
	out := []string{root}

	records, _ := filepath.Glob(filepath.Join(root, ".git", "worktrees", "*", "gitdir"))
	for _, rec := range records {
		data, err := os.ReadFile(rec)
		if err != nil {
			continue
		}
		dir := filepath.Dir(strings.TrimSpace(string(data)))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

// CurrentBranch returns the branch checked out in the checkout containing
// dir, read from git's HEAD file, or "" outside a repo or on a detached HEAD.
func CurrentBranch(dir string) string {
	gitPath, ok := findDotGit(dir)
	if !ok {
		return ""
	}
	gitDir := gitPath
	if info, err := os.Stat(gitPath); err == nil && !info.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		target, ok := parseGitdirFile(string(data))
		if !ok {
			return ""
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(gitPath), target)
		}
		gitDir = target
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return ref
}
