// Package graph is RTS's map of how a codebase hangs together: which classes
// and files make up a feature, and how they connect.
//
// It exists for the one question the other stores cannot answer. The symbol
// index says where `BoardTemplate` is defined; it cannot say which fourteen
// files are the board template feature or what calls what between them, so
// every session that needed that worked it out again with an Explore agent
// and threw it away at the end.
//
// The understanding comes from Claude, not from RTS: a session that explored
// a feature writes down what it found (record_feature), and every later
// session reads it back in one call (touchpoints). What RTS adds is the part
// a model cannot be trusted with on its own — every name in a map must
// resolve to real code when it is written, every member file is fingerprinted
// so a map that the code has moved under says so, and `rts index` drops what
// the code no longer contains.
//
// Unlike symbols.json and map.json, which `rts index` rebuilds whole, the
// graph is added to a feature at a time by many sessions over months, two of
// them at once whenever worktrees share a checkout's .rts. That is the job
// SQLite exists for, and why this one store is not JSON. Nothing outside this
// package knows it is SQLite.
package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SchemaVersion is stored in PRAGMA user_version. Like insights, the graph
// holds conclusions nothing can rebuild, so an unknown version is an error
// the caller sees rather than a reason to start over.
const SchemaVersion = 5

// EdgeKinds is the closed list of connections a map may claim. It is closed so
// that queries can filter on a kind and a made-up one is refused at the door
// rather than quietly becoming a second spelling of an existing one.
var EdgeKinds = []string{
	"uses", "calls", "inherits", "includes",
	"has_many", "has_one", "belongs_to",
	"renders", "enqueues", "routes_to", "tests",
}

// IF NOT EXISTS throughout because two sessions can open a fresh graph at the
// same moment, both read user_version 0, and both run this.
const schema = `
CREATE TABLE IF NOT EXISTS features (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	summary    TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	session    TEXT NOT NULL DEFAULT '',
	branch     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS members (
	feature_id  INTEGER NOT NULL REFERENCES features(id) ON DELETE CASCADE,
	ref         TEXT NOT NULL,
	role        TEXT NOT NULL DEFAULT '',
	path        TEXT NOT NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (feature_id, ref)
);
CREATE TABLE IF NOT EXISTS edges (
	feature_id INTEGER NOT NULL REFERENCES features(id) ON DELETE CASCADE,
	src        TEXT NOT NULL,
	dst        TEXT NOT NULL,
	kind       TEXT NOT NULL,
	origin     TEXT NOT NULL DEFAULT 'claude',
	session    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (feature_id, src, dst, kind)
);
CREATE INDEX IF NOT EXISTS edges_src ON edges(src);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst);
CREATE TABLE IF NOT EXISTS notes (
	feature_id INTEGER NOT NULL REFERENCES features(id) ON DELETE CASCADE,
	ord        INTEGER NOT NULL,
	body       TEXT NOT NULL,
	PRIMARY KEY (feature_id, ord)
);
CREATE TABLE IF NOT EXISTS feature_history (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	snapshot    TEXT NOT NULL,
	replaced_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS refs (
	ref  TEXT PRIMARY KEY,
	path TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nudges (
	session    TEXT NOT NULL,
	feature    TEXT NOT NULL,
	nudged_at  TEXT NOT NULL,
	PRIMARY KEY (session, feature)
);
CREATE TABLE IF NOT EXISTS stop_asks (
	session  TEXT NOT NULL,
	area     TEXT NOT NULL,
	asked_at TEXT NOT NULL,
	PRIMARY KEY (session, area)
);
`

// Path is where the graph lives.
func Path(projectRoot string) string {
	return filepath.Join(projectRoot, ".rts", "graph.db")
}

// Open opens the project's graph, creating it on first use.
//
// WAL lets a reader in one session carry on while another session writes,
// and the busy timeout turns two sessions finishing at the same moment into a
// short wait instead of a "database is locked" error for one of them.
//
// Transactions begin IMMEDIATE, taking the write lock up front. Record reads
// before it writes, and a transaction that starts as a reader cannot wait its
// way into writing once another session has committed under it: SQLite fails
// it at once with SQLITE_BUSY, busy timeout or not. Eight concurrent writers
// lost five of their maps that way before this.
func Open(projectRoot string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Join(projectRoot, ".rts"), 0755); err != nil {
		return nil, fmt.Errorf("creating .rts directory: %w", err)
	}
	return open(Path(projectRoot))
}

// OpenExisting opens the graph only if one has been written. Readers that
// run in every project — the brief above all — must not leave an empty
// graph.db behind in repos that have never mapped anything.
func OpenExisting(projectRoot string) (*sql.DB, error) {
	if _, err := os.Stat(Path(projectRoot)); err != nil {
		return nil, err
	}
	return open(Path(projectRoot))
}

func open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}

	// Version 1 lacked stop_asks, version 2 refs, version 3 nudges, version 4
	// features.branch. Every statement in the schema is IF NOT EXISTS, so
	// running it over an older graph adds the missing tables and leaves the
	// feature maps alone; the one added column is added below.
	switch version {
	case SchemaVersion:
		return nil
	case 0, 1, 2, 3, 4:
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}
		if err := addColumnIfMissing(tx, "features", "branch", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("written by a different rts version (schema %d, expected %d)", version, SchemaVersion)
	}
}

// addColumnIfMissing adds a column to a table an older version created
// without it. SQLite has no ADD COLUMN IF NOT EXISTS, so it asks first.
func addColumnIfMissing(tx *sql.Tx, table, column, decl string) error {
	rows, err := tx.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_ = rows.Close()
	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

func validKind(kind string) bool {
	for _, k := range EdgeKinds {
		if k == kind {
			return true
		}
	}
	return false
}
