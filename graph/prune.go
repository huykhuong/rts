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
func Prune(db *sql.DB, projectRoot string, idx *index.IndexFile) (PruneResult, error) {
	var res PruneResult

	known := make(map[string]bool, len(idx.Symbols))
	for _, sym := range idx.Symbols {
		if sym.Kind == "class" || sym.Kind == "module" {
			known[index.QualifiedName(sym)] = true
		}
	}
	exists := func(ref string) bool {
		if strings.Contains(ref, "/") || filepath.Ext(ref) != "" {
			_, err := os.Stat(filepath.Join(projectRoot, ref))
			return err == nil
		}
		return known[ref]
	}

	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	refs, err := distinct(tx, "SELECT ref FROM members UNION SELECT src FROM edges UNION SELECT dst FROM edges")
	if err != nil {
		return res, err
	}

	for _, ref := range refs {
		if exists(ref) {
			continue
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
