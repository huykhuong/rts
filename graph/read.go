package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rts/index"
)

// querier is what both *sql.DB and *sql.Tx offer, so one loader serves a
// plain read and the snapshot Record takes inside its transaction.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func load(q querier, id int64) (Feature, error) {
	var f Feature
	var updated string
	if err := q.QueryRow("SELECT name, summary, updated_at, session FROM features WHERE id = ?", id).Scan(&f.Name, &f.Summary, &updated, &f.Session); err != nil {
		return f, err
	}
	f.UpdatedAt, _ = time.Parse(time.RFC3339, updated)

	rows, err := q.Query("SELECT ref, role, path, fingerprint FROM members WHERE feature_id = ? ORDER BY rowid", id)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Ref, &m.Role, &m.Path, &m.Fingerprint); err != nil {
			_ = rows.Close()
			return f, err
		}
		f.Members = append(f.Members, m)
	}
	_ = rows.Close()

	rows, err = q.Query("SELECT src, dst, kind, origin FROM edges WHERE feature_id = ? ORDER BY rowid", id)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Origin); err != nil {
			_ = rows.Close()
			return f, err
		}
		f.Edges = append(f.Edges, e)
	}
	_ = rows.Close()

	rows, err = q.Query("SELECT body FROM notes WHERE feature_id = ? ORDER BY ord", id)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return f, err
		}
		f.Notes = append(f.Notes, n)
	}
	_ = rows.Close()

	return f, nil
}

// Summary is one line of the "Mapped features" list.
type Summary struct {
	Name      string
	Members   int
	Changed   int // members whose file no longer matches its fingerprint
	UpdatedAt time.Time
}

// Summaries lists every feature, most recently updated first.
func Summaries(db *sql.DB, projectRoot string) ([]Summary, error) {
	rows, err := db.Query("SELECT id FROM features ORDER BY updated_at DESC, name")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()

	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		f, err := load(db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Summary{
			Name:      f.Name,
			Members:   len(f.Members),
			Changed:   len(changedMembers(projectRoot, f.Members)),
			UpdatedAt: f.UpdatedAt,
		})
	}
	return out, nil
}

// changedMembers returns the members whose file is not what it was when the
// map was written. Hashed from the file on disk, not read from
// fingerprints.json: that file is only as fresh as the last `rts index`, and
// a warning that lags the code by a day is the one that gets trusted wrongly.
func changedMembers(projectRoot string, members []Member) map[string]bool {
	changed := map[string]bool{}
	for _, m := range members {
		if m.Fingerprint == "" {
			continue // a directory: nothing to hash
		}
		data, err := os.ReadFile(filepath.Join(projectRoot, m.Path))
		if err != nil || index.Fingerprint(data) != m.Fingerprint {
			changed[m.Ref] = true
		}
	}
	return changed
}

// Touchpoints answers a feature name or a class name with readable text.
//
// A feature name returns that feature's map. A class or path returns every
// feature it belongs to, then what it connects to within depth steps across
// all maps — the "what else touches this" answer no single map holds.
func Touchpoints(db *sql.DB, projectRoot string, idx *index.IndexFile, query string, depth int) (string, error) {
	query = strings.TrimSpace(query)
	if depth < 1 {
		depth = 1
	}
	if depth > 2 {
		depth = 2
	}

	var id int64
	err := db.QueryRow("SELECT id FROM features WHERE name = ?", query).Scan(&id)
	if err == nil {
		f, err := load(db, id)
		if err != nil {
			return "", err
		}
		var buf strings.Builder
		renderFeature(&buf, projectRoot, f)
		return buf.String(), nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	ref := query
	if idx != nil {
		if resolved, _, err := resolveRef(projectRoot, idx, query); err == nil {
			ref = resolved
		}
	}

	features, err := featuresTouching(db, ref)
	if err != nil {
		return "", err
	}
	neighbours, err := neighbourhood(db, ref, depth)
	if err != nil {
		return "", err
	}

	if len(features) == 0 && len(neighbours) == 0 {
		return missText(db, query)
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "%s\n", ref)
	if len(features) > 0 {
		fmt.Fprintf(&buf, "Part of: %s\n", strings.Join(features, ", "))
	}
	if len(neighbours) > 0 {
		fmt.Fprintf(&buf, "\nConnections within %d step(s):\n", depth)
		for _, line := range neighbours {
			fmt.Fprintf(&buf, "  %s\n", line)
		}
	}
	buf.WriteString("\nCall touchpoints with a feature name above for its full map.\n")
	return buf.String(), nil
}

func featuresTouching(db *sql.DB, ref string) ([]string, error) {
	rows, err := db.Query(`
		SELECT DISTINCT f.name FROM features f
		WHERE f.id IN (SELECT feature_id FROM members WHERE ref = ?1)
		   OR f.id IN (SELECT feature_id FROM edges WHERE src = ?1 OR dst = ?1)
		ORDER BY f.name`, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// neighbourhood walks edges outward from ref, both directions, and returns
// each edge it crossed as "src —kind→ dst", nearest first.
func neighbourhood(db *sql.DB, ref string, depth int) ([]string, error) {
	seen := map[string]bool{ref: true}
	frontier := []string{ref}
	var lines []string
	printed := map[string]bool{}

	for step := 0; step < depth && len(frontier) > 0; step++ {
		var next []string
		for _, node := range frontier {
			rows, err := db.Query("SELECT DISTINCT src, dst, kind FROM edges WHERE src = ?1 OR dst = ?1 ORDER BY kind, src, dst", node)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var src, dst, kind string
				if err := rows.Scan(&src, &dst, &kind); err != nil {
					_ = rows.Close()
					return nil, err
				}
				line := fmt.Sprintf("%s %s → %s", src, kind, dst)
				if !printed[line] {
					printed[line] = true
					lines = append(lines, line)
				}
				for _, other := range []string{src, dst} {
					if !seen[other] {
						seen[other] = true
						next = append(next, other)
					}
				}
			}
			_ = rows.Close()
		}
		frontier = next
	}
	return lines, nil
}

func missText(db *sql.DB, query string) (string, error) {
	rows, err := db.Query("SELECT name FROM features ORDER BY name")
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		names = append(names, n)
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "Nothing mapped for %q.\n", query)
	if len(names) > 0 {
		fmt.Fprintf(&buf, "Mapped features: %s\n", strings.Join(names, ", "))
	} else {
		buf.WriteString("No features are mapped in this project yet.\n")
	}
	buf.WriteString("Explore it as usual, then save what you found with record_feature.\n")
	return buf.String(), nil
}

// renderFeature prints a map with its structure first and its notes last,
// under the same caveat the brief puts on recorded findings: the structure
// was checked against the code when it was written, the notes never were.
func renderFeature(buf *strings.Builder, projectRoot string, f Feature) {
	changed := changedMembers(projectRoot, f.Members)

	fmt.Fprintf(buf, "Feature: %s\n", f.Name)
	fmt.Fprintf(buf, "%s\n", f.Summary)
	// Say what the structure rests on, separately from the notes. The first
	// with/without-map comparison read the map, called it "useful, but its
	// notes are unverified", and re-checked every class it named — paying for
	// the exploration twice. The names were checked against the code when the
	// map was saved, and an unchanged fingerprint means the files are
	// byte-identical since; that is worth stating, and is all that is stated.
	mapped := ""
	if !f.UpdatedAt.IsZero() {
		mapped = " on " + f.UpdatedAt.Local().Format("2006-01-02")
	}
	if len(changed) == 0 {
		fmt.Fprintf(buf, "Mapped%s by a session that read these files. Every name below resolved to real code when it was saved, "+
			"and none of the files has changed since, so the members and connections can be used as they are.\n", mapped)
	} else {
		fmt.Fprintf(buf, "Mapped%s by a session that read these files. Every name below resolved to real code when it was saved; "+
			"files marked [changed] have been edited since.\n", mapped)
	}

	outgoing := map[string][]Edge{}
	for _, e := range f.Edges {
		outgoing[e.Src] = append(outgoing[e.Src], e)
	}

	// Only class members print a path column; a path member is its own path,
	// and letting its length set the width pushed every class's path off the
	// right edge.
	width := 0
	for _, m := range f.Members {
		if m.Path != m.Ref && len(m.Ref) > width {
			width = len(m.Ref)
		}
	}

	buf.WriteString("\n")
	shown := map[string]bool{}
	for _, m := range f.Members {
		shown[m.Ref] = true
		line := m.Ref
		if m.Path != m.Ref {
			line = fmt.Sprintf("%-*s  %s", width, m.Ref, m.Path)
		}
		if m.Role != "" {
			line += "  (" + m.Role + ")"
		}
		if changed[m.Ref] {
			line += "  [changed]"
		}
		fmt.Fprintf(buf, "  %s\n", line)
		for _, e := range outgoing[m.Ref] {
			fmt.Fprintf(buf, "    %s → %s%s\n", e.Kind, e.Dst, originMark(e))
		}
	}

	// Edges from a ref that is not itself a member — Board has_many
	// BoardTemplate, say — still belong in the answer.
	var others []string
	for src := range outgoing {
		if !shown[src] {
			others = append(others, src)
		}
	}
	sort.Strings(others)
	for _, src := range others {
		fmt.Fprintf(buf, "  %s\n", src)
		for _, e := range outgoing[src] {
			fmt.Fprintf(buf, "    %s → %s%s\n", e.Kind, e.Dst, originMark(e))
		}
	}

	if len(f.Notes) > 0 {
		buf.WriteString("\nNotes (written by a past session; not verified):\n")
		for _, n := range f.Notes {
			fmt.Fprintf(buf, "  - %s\n", n)
		}
	}

	if len(changed) > 0 {
		fmt.Fprintf(buf, "\n%d member(s) marked [changed] have been edited since this map was written. "+
			"Confirm the connections around them before relying on them, and save the corrected map with record_feature.\n", len(changed))
	}
}

func originMark(e Edge) string {
	if e.Origin == "code" {
		return "  (from the code)"
	}
	return ""
}
