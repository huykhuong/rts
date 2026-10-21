package graph

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// FeatureRef names a feature and how many members it has.
type FeatureRef struct {
	Name    string
	Members int
}

// FeaturesContaining returns the features a project-relative file belongs
// to, as a member itself or inside a member directory, largest first.
func FeaturesContaining(db *sql.DB, rel string) ([]FeatureRef, error) {
	rows, err := db.Query(`
		SELECT f.name, (SELECT count(*) FROM members c WHERE c.feature_id = f.id)
		FROM features f
		WHERE EXISTS (
			SELECT 1 FROM members m
			WHERE m.feature_id = f.id
			  AND (m.path = ?1 OR (m.fingerprint = '' AND substr(?1, 1, length(m.path) + 1) = m.path || '/'))
		)
		ORDER BY 2 DESC, f.name`, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []FeatureRef
	for rows.Next() {
		var r FeatureRef
		if err := rows.Scan(&r.Name, &r.Members); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Nudged returns the features a session has already been pointed at.
func Nudged(db *sql.DB, session string) (map[string]bool, error) {
	nudged := map[string]bool{}
	rows, err := db.Query("SELECT feature FROM nudges WHERE session = ?", session)
	if err != nil {
		return nudged, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nudged, err
		}
		nudged[name] = true
	}
	return nudged, rows.Err()
}

// MarkNudged records that a session was pointed at these features, so a
// later read in the same feature says nothing.
func MarkNudged(db *sql.DB, session string, features []string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	at := now.UTC().Format(time.RFC3339)
	for _, f := range features {
		if _, err := tx.Exec("INSERT OR IGNORE INTO nudges (session, feature, nudged_at) VALUES (?, ?, ?)", session, f, at); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM nudges WHERE nudged_at < ?", now.Add(-askRetention).UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// Overlap is an existing feature that already holds much of a new map.
type Overlap struct {
	Name   string
	Shared int // members of the new map the existing feature also has
	Of     int // members of the new map
}

const (
	// duplicateShare and duplicateMin decide when a new map is most likely
	// an existing feature under another name. Measured on steroidd's first
	// nine maps: the one real duplicate ("board templates" inside "html
	// board templates") shared 70% of its files; related but distinct
	// features, which share registries such as html_generator.rb, shared at
	// most 40%.
	duplicateShare = 0.6
	duplicateMin   = 3
)

// Overlaps returns the existing features, under other names, that already
// hold most of f's members.
func Overlaps(db *sql.DB, f Feature) ([]Overlap, error) {
	if len(f.Members) == 0 {
		return nil, nil
	}
	paths := map[string]bool{}
	for _, m := range f.Members {
		paths[m.Path] = true
	}

	rows, err := db.Query("SELECT f.name, m.path FROM members m JOIN features f ON f.id = m.feature_id WHERE f.name <> ? COLLATE NOCASE", f.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	shared := map[string]int{}
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			return nil, err
		}
		if paths[path] {
			shared[name]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Overlap
	for name, n := range shared {
		if n >= duplicateMin && float64(n) >= duplicateShare*float64(len(paths)) {
			out = append(out, Overlap{Name: name, Shared: n, Of: len(paths)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Shared > out[j].Shared })
	return out, nil
}

// lowerAll returns a set of names folded to lower case.
func lowerAll(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.ToLower(strings.TrimSpace(n))] = true
	}
	return out
}

// Distinct drops the overlaps a caller has declared to be different
// features.
func Distinct(overlaps []Overlap, declared []string) []Overlap {
	skip := lowerAll(declared)
	var out []Overlap
	for _, o := range overlaps {
		if !skip[strings.ToLower(o.Name)] {
			out = append(out, o)
		}
	}
	return out
}
