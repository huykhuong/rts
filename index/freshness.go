package index

// Freshness: whether the symbol index still describes the code on disk.
//
// This used to compare modification times, and that was wrong in a way that
// took a real session to expose. Measured on a working Rails repo an hour
// after a rebase: 493 Ruby files reported as changed, 492 of them
// byte-identical to HEAD. Every git operation that rewrites the working tree
// — rebase, checkout, branch switch, stash pop, merge — restamps mtimes
// wholesale, so the warning fired at full volume on days when nothing had
// changed at all.
//
// A warning that cries wolf is worse than no warning. It trained the one
// session that read it to distrust the map, and it buried the single file
// that genuinely had moved under 492 that had not.
//
// So freshness is decided by content. Each indexed file's bytes are
// fingerprinted at index time; the check re-fingerprints and compares. mtime
// survives only as a cheap pre-filter — a file whose mtime has not moved
// cannot have changed, so it is never read — which keeps the common case as
// fast as the old walk while making the answer true.
//
// The fingerprint set is also what makes deletions visible. The mtime check
// structurally could not see one: a file that is gone has no mtime to be
// late. A path in the fingerprints with nothing on disk is a deletion, and
// deletions are exactly what leaves find_symbol pointing at files that no
// longer exist.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FingerprintVersion is bumped when the on-disk shape changes. Like the rest
// of the derived stores, a mismatch is discarded rather than migrated.
const FingerprintVersion = 1

// Fingerprints is the content hash of every file the index was built from.
type Fingerprints struct {
	Version     int               `json:"version"`
	GeneratedAt time.Time         `json:"generated_at"`
	Files       map[string]string `json:"files"` // relative path -> content hash
}

// Drift is what changed since the index was built.
//
// Touched is reported separately and deliberately: it is the count of files a
// git operation restamped without changing, which is the noise the old check
// mistook for signal. Nothing acts on it, but a diagnostic that can say "492
// files were touched by a rebase and none of them changed" explains an
// otherwise baffling absence of warnings.
type Drift struct {
	Changed int
	Added   int
	Removed int
	Touched int
}

// Stale reports whether anything actually moved. Touched is excluded, which
// is the entire point.
func (d Drift) Stale() bool {
	return d.Changed+d.Added+d.Removed > 0
}

// Total is how many files are out of step with the index.
func (d Drift) Total() int {
	return d.Changed + d.Added + d.Removed
}

// Describe renders the drift as a phrase for a one-line warning, naming only
// the categories that are non-zero.
func (d Drift) Describe() string {
	parts := make([]string, 0, 3)
	if d.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", d.Changed))
	}
	if d.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", d.Added))
	}
	if d.Removed > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", d.Removed))
	}

	switch len(parts) {
	case 0:
		return "nothing changed"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return parts[0] + ", " + parts[1] + " and " + parts[2]
	}
}

// shouldIndex is the single answer to "does this file belong in the symbol
// index?", asked by the indexer, the map builder and the freshness check
// alike.
//
// It lives in one place because it did not, once: the map builder walked a
// tree the indexer skipped, so db/migrate counted toward the layout table and
// toward nothing else, and the two stores described different codebases.
func shouldIndex(path string) bool {
	if filepath.Ext(path) != ".rb" && !IsScript(path) {
		return false
	}
	// skippedPathSuffixes are written with leading and trailing separators
	// ("/db/migrate/") so they match a path segment rather than a substring
	// of a name. WalkDir hands out paths carrying the walk root, so they
	// always have a leading separator; a relative path like
	// "db/migrate/x.rb" does not, and would otherwise slip through. One
	// leading separator is added when it is missing so both shapes answer
	// the same.
	probe := path
	if !strings.HasPrefix(probe, string(filepath.Separator)) {
		probe = string(filepath.Separator) + probe
	}

	for _, suffix := range skippedPathSuffixes {
		if strings.Contains(probe, suffix) {
			return false
		}
	}
	return true
}

// skipDir is the single answer to "should this directory be excluded from
// the walk?", asked by the indexer, the map builder and the freshness check
// alike — the directory-level counterpart to shouldIndex above.
//
// Hidden directories are skipped alongside the named entries in skippedDirs:
// a real Rails repo kept git worktrees under .claude/worktrees/<name>/, each
// a full copy of the codebase, and the index walked into all three, tripling
// every symbol and pointing find_symbol at stale worktree copies most of the
// time. Other dotdirs (.ruby-lsp, .github, .agents, .fly) are tooling, not
// project source, and are excluded for the same reason.
//
// The walk root itself is never skipped: filepath.WalkDir invokes the
// callback on the root before anything else, and a project whose own
// directory name starts with a dot must still be indexed.
func skipDir(path, name, root string) bool {
	if path == root {
		return false
	}
	return skippedDirs[name] || strings.HasPrefix(name, ".")
}

// fingerprint hashes a file's bytes. FNV-64a: this detects change, it does
// not defend against a forger, and a non-cryptographic hash is the right
// trade for something run over several thousand files.
func fingerprint(data []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%016x", h.Sum64())
}

// BuildFingerprints hashes every file the index covers.
func BuildFingerprints(projectRoot string) (*Fingerprints, error) {
	fp := &Fingerprints{
		Version:     FingerprintVersion,
		GeneratedAt: time.Now().UTC(),
		Files:       map[string]string{},
	}

	err := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(path, d.Name(), projectRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldIndex(path) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return nil
		}

		fp.Files[rel] = fingerprint(data)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return fp, nil
}

// WriteFingerprints serializes to .rts/fingerprints.json.
//
// Kept out of map.json on purpose: the map is a small artifact a person might
// reasonably open and read, and several thousand hash lines would drown it.
func WriteFingerprints(projectRoot string, fp *Fingerprints) error {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating .rts directory: %w", err)
	}

	data, err := json.Marshal(fp)
	if err != nil {
		return fmt.Errorf("marshaling fingerprints: %w", err)
	}

	outPath := filepath.Join(dir, "fingerprints.json")
	if err := writeFileAtomic(outPath, data); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}
	return nil
}

// LoadFingerprints reads .rts/fingerprints.json.
func LoadFingerprints(projectRoot string) (*Fingerprints, error) {
	path := filepath.Join(projectRoot, ".rts", "fingerprints.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var fp Fingerprints
	if err := json.Unmarshal(data, &fp); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if fp.Version != FingerprintVersion {
		return nil, fmt.Errorf("%s was written by a different rts version", path)
	}

	return &fp, nil
}

// CheckDrift compares the code on disk against what the index was built from.
//
// An index with no fingerprints beside it — written by an older rts — reports
// no drift rather than guessing. The alternative is falling back to the mtime
// check this exists to replace, which would reintroduce the false positives
// on exactly the installs that have not re-indexed yet.
func CheckDrift(projectRoot string) (Drift, error) {
	var drift Drift

	fp, err := LoadFingerprints(projectRoot)
	if err != nil {
		return drift, err
	}

	seen := make(map[string]bool, len(fp.Files))

	err = filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(path, d.Name(), projectRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldIndex(path) {
			return nil
		}

		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return nil
		}

		want, known := fp.Files[rel]
		if !known {
			drift.Added++
			return nil
		}
		seen[rel] = true

		// mtime as a pre-filter only: a file whose mtime predates the index
		// cannot have changed since, so it is never read. Everything past
		// this point is decided on content.
		if info, statErr := d.Info(); statErr == nil && !info.ModTime().After(fp.GeneratedAt) {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		if fingerprint(data) == want {
			drift.Touched++ // restamped by git, identical content
		} else {
			drift.Changed++
		}
		return nil
	})
	if err != nil {
		return drift, err
	}

	for rel := range fp.Files {
		if !seen[rel] {
			drift.Removed++
		}
	}

	return drift, nil
}

// Fingerprint is the content hash the index records for a file, exported so
// the graph can stamp a feature map with the same hash and tell later whether
// the files under it have moved.
func Fingerprint(data []byte) string {
	return fingerprint(data)
}
