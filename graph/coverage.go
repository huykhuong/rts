package graph

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rts/index"
)

// Coverage reports which files an up-to-date map already describes.
type Coverage struct {
	files map[string]bool
	dirs  []string
}

// Covers reports whether a project-relative path belongs to a feature map
// whose member is unchanged since the map was saved. A file inside a mapped
// directory counts; a member whose file has been edited does not, because
// that map is the one that needs saving again.
func (c Coverage) Covers(rel string) bool {
	if c.files[rel] {
		return true
	}
	for _, d := range c.dirs {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// Covered builds the coverage of every map in the graph.
func Covered(db *sql.DB, projectRoot string) (Coverage, error) {
	c := Coverage{files: map[string]bool{}}

	rows, err := db.Query("SELECT path, fingerprint FROM members")
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var path, fp string
		if err := rows.Scan(&path, &fp); err != nil {
			return c, err
		}

		if fp == "" {
			if info, err := os.Stat(filepath.Join(projectRoot, path)); err == nil && info.IsDir() {
				c.dirs = append(c.dirs, path)
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(projectRoot, path))
		if err == nil && index.Fingerprint(data) == fp {
			c.files[path] = true
		}
	}
	return c, rows.Err()
}

// Asked returns the areas a session has already been asked to map.
func Asked(db *sql.DB, session string) (map[string]bool, error) {
	asked := map[string]bool{}

	rows, err := db.Query("SELECT area FROM stop_asks WHERE session = ?", session)
	if err != nil {
		return asked, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var area string
		if err := rows.Scan(&area); err != nil {
			return asked, err
		}
		asked[area] = true
	}
	return asked, rows.Err()
}

// askRetention is how long an ask is remembered. A session is rarely
// resumed after a fortnight, and the table would otherwise grow by every
// area every session ever explored.
const askRetention = 14 * 24 * time.Hour

// MarkAsked records that a session was asked to map these areas, so it is
// never asked about them again.
func MarkAsked(db *sql.DB, session string, areas []string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	at := now.UTC().Format(time.RFC3339)
	for _, a := range areas {
		if _, err := tx.Exec("INSERT OR IGNORE INTO stop_asks (session, area, asked_at) VALUES (?, ?, ?)", session, a, at); err != nil {
			return err
		}
	}
	cutoff := now.Add(-askRetention).UTC().Format(time.RFC3339)
	if _, err := tx.Exec("DELETE FROM stop_asks WHERE asked_at < ?", cutoff); err != nil {
		return err
	}
	return tx.Commit()
}
