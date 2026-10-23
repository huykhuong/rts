package graph

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rts/index"
	"rts/secrets"
)

const (
	// MaxSummaryChars keeps a summary to the two or three sentences a reader
	// takes in before the member list.
	MaxSummaryChars = 400

	// MaxNoteChars and MaxNotes bound what a map carries beyond its
	// structure. Notes are the part nothing can check, so they are the part
	// to keep small.
	MaxNoteChars = 400
	MaxNotes     = 8

	// MaxMembers and MaxEdges are ceilings on one feature. A "feature" with
	// two hundred members is the whole app under a feature's name, and its
	// touchpoints answer would cost what the exploration it replaces did.
	MaxMembers = 60
	MaxEdges   = 120
)

// maxHistory is how many replaced versions of a map are kept.
const maxHistory = 3

// Feature is one feature map as a session writes it and as it is read back.
type Feature struct {
	Name      string
	Summary   string
	Members   []Member
	Edges     []Edge
	Notes     []string
	UpdatedAt time.Time
	Session   string

	// Branch is the branch this map matches: the one checked out where it
	// was saved, or, for a map saved before branches were recorded, the one
	// whose files match it best. Read from another branch, a member that
	// differs may only be that branch's unmerged work, not an edit made
	// since.
	Branch string

	// refPaths is the file each resolved ref was found in, members and edge
	// ends alike. Record keeps it in the refs table, which is how Prune can
	// tell branch-only code that still exists from code that is gone.
	refPaths map[string]string
}

// Member is a class, module or file that makes up a feature.
type Member struct {
	Ref         string `json:"ref"`            // qualified class name, or a project-relative path
	Role        string `json:"role,omitempty"` // model, controller, job, view, test...
	Path        string `json:"path,omitempty"` // filled in from the index on write
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Edge is one connection between two refs.
type Edge struct {
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Kind   string `json:"kind"`
	Origin string `json:"origin,omitempty"`
}

// Resolve checks a feature against the code and fills in what the index
// knows: each class ref becomes its fully qualified name and gains the path
// it is defined in, and each member is fingerprinted.
//
// It returns every problem it finds rather than the first, because the
// caller is a model that will fix and resend: one round trip per typo would
// turn a ten-member map into ten calls.
//
// checkout is the session's own checkout — a worktree, usually — and may be
// "" or projectRoot itself. A name the main checkout's index lacks is looked
// for there, because a session building a feature on a branch maps exactly
// the files the main checkout does not have yet.
func Resolve(projectRoot, checkout string, idx *index.IndexFile, f *Feature) []string {
	var problems []string
	r := &resolver{root: projectRoot, checkout: checkout, idx: idx}
	f.refPaths = map[string]string{}

	f.Name = strings.TrimSpace(f.Name)
	f.Summary = strings.TrimSpace(f.Summary)
	if f.Name == "" {
		problems = append(problems, "feature name is empty")
	}
	if f.Summary == "" {
		problems = append(problems, "summary is empty: say in a sentence or two what the feature does")
	}
	if len(f.Summary) > MaxSummaryChars {
		problems = append(problems, fmt.Sprintf("summary is %d characters; keep it under %d", len(f.Summary), MaxSummaryChars))
	}
	if len(f.Members) == 0 {
		problems = append(problems, "no members: list the classes and files that make up the feature")
	}
	if len(f.Members) > MaxMembers {
		problems = append(problems, fmt.Sprintf("%d members; a feature holds at most %d — split it into smaller features", len(f.Members), MaxMembers))
	}
	if len(f.Edges) > MaxEdges {
		problems = append(problems, fmt.Sprintf("%d edges; a feature holds at most %d", len(f.Edges), MaxEdges))
	}
	if len(f.Notes) > MaxNotes {
		problems = append(problems, fmt.Sprintf("%d notes; keep at most %d", len(f.Notes), MaxNotes))
	}

	seen := map[string]bool{}
	members := make([]Member, 0, len(f.Members))
	for _, m := range f.Members {
		ref, path, err := r.resolve(m.Ref)
		if err != nil {
			problems = append(problems, fmt.Sprintf("member %q: %v", m.Ref, err))
			continue
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true

		m.Ref, m.Path = ref, path
		f.refPaths[ref] = path
		m.Role = strings.TrimSpace(m.Role)
		if data, err := r.read(path); err == nil {
			m.Fingerprint = index.Fingerprint(data)
		}
		members = append(members, m)
	}
	f.Members = members

	edgeSeen := map[Edge]bool{}
	edges := make([]Edge, 0, len(f.Edges))
	for _, e := range f.Edges {
		e.Kind = strings.TrimSpace(e.Kind)
		if !validKind(e.Kind) {
			problems = append(problems, fmt.Sprintf("edge %s → %s: kind %q is not one of %s", e.Src, e.Dst, e.Kind, strings.Join(EdgeKinds, ", ")))
			continue
		}

		src, srcPath, srcErr := r.resolve(e.Src)
		if srcErr != nil {
			problems = append(problems, fmt.Sprintf("edge source %q: %v", e.Src, srcErr))
		}
		dst, dstPath, dstErr := r.resolve(e.Dst)
		if dstErr != nil {
			problems = append(problems, fmt.Sprintf("edge target %q: %v", e.Dst, dstErr))
		}
		if srcErr != nil || dstErr != nil {
			continue
		}

		e.Src, e.Dst, e.Origin = src, dst, "claude"
		f.refPaths[src], f.refPaths[dst] = srcPath, dstPath
		if edgeSeen[e] {
			continue
		}
		edgeSeen[e] = true
		edges = append(edges, e)
	}
	f.Edges = edges

	notes := make([]string, 0, len(f.Notes))
	for _, n := range f.Notes {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if len(n) > MaxNoteChars {
			problems = append(problems, fmt.Sprintf("a note is %d characters; keep each under %d", len(n), MaxNoteChars))
			continue
		}
		if word, ok := namesScratchFile(n); ok {
			problems = append(problems, fmt.Sprintf("a note names %q, a scratch file that will not outlive this checkout; say what it does instead of where it is", word))
			continue
		}
		if word, ok := namesProtectedFile(n); ok {
			problems = append(problems, fmt.Sprintf("a note names %q, a protected credential file; feature maps never mention one", word))
			continue
		}
		notes = append(notes, n)
	}
	f.Notes = notes

	return problems
}

// resolver resolves refs against the main checkout's index and, failing
// that, the session's own checkout, then the project's other worktrees.
// Each extra checkout is indexed at most once per write, and only when a
// name is missing from every index before it.
//
// The other worktrees matter for a dispatcher: a session that runs in the
// main checkout and hands work to workers in worktrees is the one that
// understands what they built, and the code exists only in their
// checkouts until it is merged. Hangout's dispatcher had both its maps
// refused on 2026-10-21 because every file was in a worker's worktree.
type resolver struct {
	root, checkout string
	idx            *index.IndexFile

	others     []string // the session's checkout, then the other worktrees
	othersRead bool
	indexes    map[string]*index.IndexFile
	foundIn    map[string]string // path → the checkout it was found in, when not the session's or the main one
}

// resolve turns a ref into its canonical form and the file it lives in.
//
// A ref with a slash or a file extension is a path and must exist in some
// checkout of the project; anything else is a class or module name and must
// be in an index. A method name, an ambiguous short name or a name nothing
// defines is refused with what was found instead, so the fix is one edit
// away.
func (r *resolver) resolve(ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("empty")
	}

	if strings.Contains(ref, "/") || filepath.Ext(ref) != "" {
		rel := filepath.Clean(strings.TrimPrefix(ref, "./"))
		if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			return "", "", fmt.Errorf("paths are project-relative")
		}
		if secrets.IsProtected(rel) {
			return "", "", fmt.Errorf("a protected credential file; feature maps never include one")
		}
		if existsIn(r.root, r.checkout, rel) {
			return rel, rel, nil
		}
		for _, dir := range r.otherCheckouts() {
			if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				r.remember(rel, dir)
				return rel, rel, nil
			}
		}
		return "", "", fmt.Errorf("no such file or directory in this project or its worktrees")
	}

	q, path, err := lookupClass(r.idx, ref)
	for _, dir := range r.otherCheckouts() {
		if !errors.Is(err, errNotDefined) {
			break
		}
		if idx := r.indexOf(dir); idx != nil {
			if q, path, err = lookupClass(idx, ref); err == nil {
				r.remember(path, dir)
			}
		}
	}
	return q, path, err
}

// otherCheckouts lists, once, the checkouts to try after the main one: the
// session's own first, then every other worktree git records.
func (r *resolver) otherCheckouts() []string {
	if r.othersRead {
		return r.others
	}
	r.othersRead = true

	seen := map[string]bool{filepath.Clean(r.root): true}
	add := func(dir string) {
		if dir != "" && !seen[filepath.Clean(dir)] {
			seen[filepath.Clean(dir)] = true
			r.others = append(r.others, dir)
		}
	}
	add(r.checkout)
	for _, dir := range index.Checkouts(r.root) {
		add(dir)
	}
	return r.others
}

func (r *resolver) indexOf(dir string) *index.IndexFile {
	if idx, ok := r.indexes[dir]; ok {
		return idx
	}
	if r.indexes == nil {
		r.indexes = map[string]*index.IndexFile{}
	}
	symbols, err := index.BuildIndex(dir)
	if err != nil {
		r.indexes[dir] = nil
		return nil
	}
	r.indexes[dir] = &index.IndexFile{Symbols: symbols}
	return r.indexes[dir]
}

// remember notes that a path exists only in another worktree, so its
// fingerprint is taken from there.
func (r *resolver) remember(path, dir string) {
	if dir == r.checkout {
		return
	}
	if r.foundIn == nil {
		r.foundIn = map[string]string{}
	}
	r.foundIn[path] = dir
}

// read reads a member's file from wherever it was found.
func (r *resolver) read(path string) ([]byte, error) {
	if dir, ok := r.foundIn[path]; ok {
		return os.ReadFile(filepath.Join(dir, path))
	}
	return readIn(r.root, r.checkout, path)
}

var errNotDefined = errors.New("nothing by that name in the index: no class, module, interface, type, function or constant (run `rts index` if it is new)")

func lookupClass(idx *index.IndexFile, ref string) (string, string, error) {
	if idx == nil {
		return "", "", errNotDefined
	}

	found := index.Find(idx, ref)
	byName := map[string][]index.Symbol{}
	var methods int
	for _, sym := range found.Symbols {
		if !index.Mappable(sym) {
			methods++
			continue
		}
		q := index.QualifiedName(sym)
		byName[q] = append(byName[q], sym)
	}

	switch {
	case len(byName) == 1:
		for q, defs := range byName {
			return q, definingPath(defs), nil
		}
	case len(byName) > 1:
		names := make([]string, 0, len(byName))
		for q := range byName {
			names = append(names, q)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("ambiguous; use the full name: %s", strings.Join(names, ", "))
	case methods > 0:
		return "", "", fmt.Errorf("is a method; name the class it belongs to")
	}
	return "", "", errNotDefined
}

// readIn reads a project-relative file from the session's checkout when it
// is there, and from the main checkout otherwise.
func readIn(root, checkout, rel string) ([]byte, error) {
	if checkout != "" {
		if data, err := os.ReadFile(filepath.Join(checkout, rel)); err == nil {
			return data, nil
		}
	}
	return os.ReadFile(filepath.Join(root, rel))
}

func existsIn(root, checkout, rel string) bool {
	if checkout != "" {
		if _, err := os.Stat(filepath.Join(checkout, rel)); err == nil {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// definingPath picks the file to fingerprint for a class that is opened in
// several. A test file reopening a class is not where the class lives.
func definingPath(defs []index.Symbol) string {
	paths := make([]string, 0, len(defs))
	for _, d := range defs {
		paths = append(paths, d.Path)
	}
	sort.SliceStable(paths, func(i, j int) bool {
		ti, tj := isTestPath(paths[i]), isTestPath(paths[j])
		if ti != tj {
			return !ti
		}
		return paths[i] < paths[j]
	})
	return paths[0]
}

func isTestPath(p string) bool {
	return strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "spec/") || strings.HasSuffix(p, "_test.rb") || strings.HasSuffix(p, "_spec.rb")
}

// namesProtectedFile reports the first word in a note that the secrets guard
// would refuse to open. The guard redacts key values wherever they appear;
// this is the stricter rule on top, that a map never even points at one.
func namesProtectedFile(note string) (string, bool) {
	for _, w := range pathWords(note) {
		if secrets.IsProtected(w) {
			return w, true
		}
	}
	return "", false
}

// pathWords splits a note into the words that could be file names: runs of
// letters, digits and . / _ -, with a sentence's closing full stop dropped.
func pathWords(note string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(note, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '.' || r == '/' || r == '_' || r == '-':
			return false
		}
		return true
	}) {
		if w = strings.TrimRight(w, "."); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// scratchDirs are folders whose files do not last: git-ignored output and
// working space, and Claude Code's own worktrees and plans. A map is read for
// months, so a note pointing into one points at nothing soon after.
var scratchDirs = map[string]bool{
	"tmp": true, "log": true, "coverage": true, "node_modules": true, ".claude": true,
}

// namesScratchFile reports the first word in a note that names a file in a
// scratch folder. Only paths to a file count, so a note can still say that
// something is written under tmp/ at runtime.
func namesScratchFile(note string) (string, bool) {
	for _, w := range pathWords(note) {
		parts := strings.Split(strings.TrimPrefix(w, "./"), "/")
		if len(parts) > 1 && scratchDirs[parts[0]] && filepath.Ext(parts[len(parts)-1]) != "" {
			return w, true
		}
	}
	return "", false
}

// Record stores a resolved feature, replacing any earlier map of the same
// name. The replaced version is kept in feature_history first, so a session
// that rewrites a good map badly costs one restore rather than the map.
//
// Only Claude-written edges are replaced. Edges `rts index` read off the code
// belong to the code, not to whichever session last mapped the feature.
func Record(db *sql.DB, f Feature) (replaced bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)

	var id int64
	err = tx.QueryRow("SELECT id FROM features WHERE name = ?", f.Name).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.Exec("INSERT INTO features (name, summary, updated_at, session, branch) VALUES (?, ?, ?, ?, ?)", f.Name, f.Summary, now, f.Session, f.Branch)
		if err != nil {
			return false, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		replaced = true
		old, err := load(tx, id)
		if err != nil {
			return false, err
		}
		snapshot, err := json.Marshal(old)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec("INSERT INTO feature_history (name, snapshot, replaced_at) VALUES (?, ?, ?)", old.Name, string(snapshot), now); err != nil {
			return false, err
		}
		// Keep the last few versions, enough to undo a bad rewrite or two;
		// every re-save would otherwise add one forever.
		if _, err := tx.Exec(`DELETE FROM feature_history WHERE name = ?1 COLLATE NOCASE AND id NOT IN (
			SELECT id FROM feature_history WHERE name = ?1 COLLATE NOCASE ORDER BY id DESC LIMIT ?2)`, old.Name, maxHistory); err != nil {
			return false, err
		}
		for _, stmt := range []string{
			"DELETE FROM members WHERE feature_id = ?",
			"DELETE FROM edges WHERE feature_id = ? AND origin = 'claude'",
			"DELETE FROM notes WHERE feature_id = ?",
		} {
			if _, err := tx.Exec(stmt, id); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec("UPDATE features SET name = ?, summary = ?, updated_at = ?, session = ?, branch = ? WHERE id = ?", f.Name, f.Summary, now, f.Session, f.Branch, id); err != nil {
			return false, err
		}
	}

	for _, m := range f.Members {
		if _, err := tx.Exec("INSERT INTO members (feature_id, ref, role, path, fingerprint) VALUES (?, ?, ?, ?, ?)", id, m.Ref, m.Role, m.Path, m.Fingerprint); err != nil {
			return false, err
		}
	}
	for _, e := range f.Edges {
		if _, err := tx.Exec("INSERT OR IGNORE INTO edges (feature_id, src, dst, kind, origin, session) VALUES (?, ?, ?, ?, 'claude', ?)", id, e.Src, e.Dst, e.Kind, f.Session); err != nil {
			return false, err
		}
	}
	for i, n := range f.Notes {
		if _, err := tx.Exec("INSERT INTO notes (feature_id, ord, body) VALUES (?, ?, ?)", id, i, n); err != nil {
			return false, err
		}
	}
	for ref, path := range f.refPaths {
		if _, err := tx.Exec("INSERT INTO refs (ref, path) VALUES (?, ?) ON CONFLICT(ref) DO UPDATE SET path = excluded.path", ref, path); err != nil {
			return false, err
		}
	}

	return replaced, tx.Commit()
}
