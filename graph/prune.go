package graph

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"rts/index"
)

// PruneResult says what a prune removed, for `rts index` to report.
type PruneResult struct {
	Members  int
	Edges    int
	Features int
}

// Prune drops every member and edge that names code the index no longer
// contains, then every feature left with no members. It runs after each
// `rts index`.
//
// A map that has lost one class is still mostly right and keeps the rest;
// a map whose whole feature is gone can only mislead, so it goes. Nothing is
// rewritten: a ref either still resolves or is removed.
//
// Code that exists only on a worktree's branch is not in the main index, and
// pruning it there would delete every map a session wrote while building a
// feature. A ref the index lacks survives while the file it was found in
// still exists in the main checkout or any worktree — so branch work lasts
// until it is merged, and goes when its worktree is removed unmerged.
func Prune(db *sql.DB, projectRoot string, idx *index.IndexFile) (PruneResult, error) {
	var res PruneResult

	known := make(map[string]bool, len(idx.Symbols))
	for _, sym := range idx.Symbols {
		if index.Mappable(sym) {
			known[index.QualifiedName(sym)] = true
		}
	}
	checkouts := index.Checkouts(projectRoot)
	inAnyCheckout := func(rel string) bool {
		for _, c := range checkouts {
			if _, err := os.Stat(filepath.Join(c, rel)); err == nil {
				return true
			}
		}
		return false
	}

	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	refPaths := map[string]string{}
	rows, err := tx.Query("SELECT ref, path FROM refs")
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var ref, path string
		if err := rows.Scan(&ref, &path); err != nil {
			_ = rows.Close()
			return res, err
		}
		refPaths[ref] = path
	}
	_ = rows.Close()

	exists := func(ref string) bool {
		if strings.Contains(ref, "/") || filepath.Ext(ref) != "" {
			return inAnyCheckout(ref)
		}
		if known[ref] {
			return true
		}
		path, ok := refPaths[ref]
		return ok && inAnyCheckout(path)
	}

	refs, err := distinct(tx, "SELECT ref FROM members UNION SELECT src FROM edges UNION SELECT dst FROM edges")
	if err != nil {
		return res, err
	}

	for _, ref := range refs {
		if exists(ref) {
			continue
		}
		if _, err := tx.Exec("DELETE FROM refs WHERE ref = ?", ref); err != nil {
			return res, err
		}
		r, err := tx.Exec("DELETE FROM members WHERE ref = ?", ref)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Members += int(n)

		r, err = tx.Exec("DELETE FROM edges WHERE src = ?1 OR dst = ?1", ref)
		if err != nil {
			return res, err
		}
		n, _ = r.RowsAffected()
		res.Edges += int(n)
	}

	r, err := tx.Exec("DELETE FROM features WHERE id NOT IN (SELECT feature_id FROM members)")
	if err != nil {
		return res, err
	}
	n, _ := r.RowsAffected()
	res.Features = int(n)

	return res, tx.Commit()
}

func distinct(tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
