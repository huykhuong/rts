package graph

import (
	"database/sql"
	"os"
	"path/filepath"

	"rts/index"
)

// otherBranch returns the branch a map was saved on when the reader is on a
// different one, and "" when they match or either is unknown.
//
// "27 changed since" read from dev, for a map saved on an unmerged branch,
// sent a steroidd session back to the code: 22 of one map's 24 files differed
// and none had been edited after it was saved — they were the branch's work.
func otherBranch(f Feature, projectRoot, checkout string) string {
	reader := checkout
	if reader == "" {
		reader = projectRoot
	}
	current := index.CurrentBranch(reader)
	if f.Branch == "" || current == "" || f.Branch == current {
		return ""
	}
	return f.Branch
}

// InferBranches fills in the branch of maps saved before the branch was
// recorded: the branch of the checkout whose files match the most of a map's
// fingerprints, when that is most of them. It runs after each `rts index`
// and touches only maps with no branch, so each is settled once. It returns
// how many it filled in.
func InferBranches(db *sql.DB, projectRoot string) (int, error) {
	rows, err := db.Query("SELECT id FROM features WHERE branch = ''")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}

	checkouts := index.Checkouts(projectRoot)
	filled := 0
	for _, id := range ids {
		f, err := load(db, id)
		if err != nil {
			return filled, err
		}

		hashed, best, bestDir := 0, 0, ""
		for _, m := range f.Members {
			if m.Fingerprint != "" {
				hashed++
			}
		}
		if hashed == 0 {
			continue
		}
		for _, dir := range checkouts {
			matches := 0
			for _, m := range f.Members {
				if m.Fingerprint == "" {
					continue
				}
				if data, err := os.ReadFile(filepath.Join(dir, m.Path)); err == nil && index.Fingerprint(data) == m.Fingerprint {
					matches++
				}
			}
			if matches > best {
				best, bestDir = matches, dir
			}
		}
		// Most of the map has to match somewhere; a map whose files have all
		// moved on says nothing about where it was written.
		if bestDir == "" || best*2 < hashed {
			continue
		}
		branch := index.CurrentBranch(bestDir)
		if branch == "" {
			continue
		}
		if _, err := db.Exec("UPDATE features SET branch = ? WHERE id = ?", branch, id); err != nil {
			return filled, err
		}
		filled++
	}
	return filled, nil
}
