package flow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/i7c/ais/internal/ais"
)

// The schema. ais keeps owning sessions.json; flow refers to sessions by ais
// id and never writes that file. Times are UTC RFC 3339, so comparing them
// as strings is comparing them as times.
const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS planners (
	id TEXT PRIMARY KEY,
	workspace TEXT NOT NULL,
	ais_session TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,            -- open, decided, expired, aborted
	started_at TEXT NOT NULL,
	closed_at TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT ''
);
-- One open planner per workspace: a database fact, not a convention.
CREATE UNIQUE INDEX IF NOT EXISTS one_open_planner ON planners(workspace) WHERE state = 'open';
CREATE TABLE IF NOT EXISTS proposals (
	id TEXT PRIMARY KEY,
	workspace TEXT NOT NULL,
	title TEXT NOT NULL,
	tickets TEXT NOT NULL,          -- JSON list of issue URLs
	repos TEXT NOT NULL,            -- JSON list of workspace repo names
	profile TEXT NOT NULL,
	prompt TEXT NOT NULL,
	custom_prompt INTEGER NOT NULL DEFAULT 0, -- 1: written by hand, not from the template
	notes TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 0,
	after TEXT NOT NULL DEFAULT '[]', -- JSON list of proposal ids or issue URLs
	state TEXT NOT NULL,            -- queued, starting, running, done, cancelled
	planner_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	ais_session TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL DEFAULT '',
	done_at TEXT NOT NULL DEFAULT '',
	manual INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS starts (at TEXT NOT NULL, proposal_id TEXT NOT NULL, manual INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS overrides (key TEXT PRIMARY KEY, value INTEGER NOT NULL, until TEXT NOT NULL, set_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS gh_ids (workspace TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	kind TEXT NOT NULL,
	subject TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
-- The last refinement outcome per ticket, for the version it was made on.
CREATE TABLE IF NOT EXISTS assessments (
	url TEXT PRIMARY KEY,
	workspace TEXT NOT NULL,
	level TEXT NOT NULL,            -- L1, L2, L3; '' after a run that failed
	version TEXT NOT NULL,          -- the ticket it was made on (ticketVersion)
	flag TEXT NOT NULL DEFAULT '',  -- refinement-limit
	at TEXT NOT NULL,
	failed_at TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO meta (key, value) VALUES ('schema', '1');
`

// columns added after schema 1, to proposals: each is added to a database
// made before it, so an old flow.db keeps working.
var addedColumns = [][2]string{
	{"kind", `TEXT NOT NULL DEFAULT 'work'`}, // work, refine
	{"autonomy", `TEXT NOT NULL DEFAULT ''`}, // the ceiling it was queued with
	{"phase", `TEXT NOT NULL DEFAULT ''`},    // plan, approved, finalized
	{"plan_url", `TEXT NOT NULL DEFAULT ''`}, // the plan PR
	{"phase_at", `TEXT NOT NULL DEFAULT ''`},
}

func migrate(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('proposals')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, c := range addedColumns {
		if !have[c[0]] {
			if _, err := db.Exec(`ALTER TABLE proposals ADD COLUMN ` + c[0] + ` ` + c[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

type DB struct{ *sql.DB }

// homeDir is where flow.db lives: ais's own directory. A variable for tests.
var homeDir = ais.Home

func dbPath() string { return filepath.Join(homeDir(), "flow.db") }

// openDB opens (and if need be creates) flow.db. Transactions take the write
// lock up front (BEGIN IMMEDIATE), so a timer and a command line cannot both
// read a queue and both decide to act on it.
func openDB() (*DB, error) {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + dbPath() + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", dbPath(), err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", dbPath(), err)
	}
	return &DB{db}, nil
}

// tx runs fn in one immediate transaction.
func (db *DB) tx(fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func logEvent(x execer, kind, subject string, detail any) {
	text := ""
	switch d := detail.(type) {
	case nil:
	case string:
		text = d
	default:
		b, _ := json.Marshal(d)
		text = string(b)
	}
	x.Exec(`INSERT INTO events (at, kind, subject, detail) VALUES (?, ?, ?, ?)`, stamp(now()), kind, subject, text)
}

// now is a variable so tests can move the clock.
var now = time.Now

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

const idAlphabet = "23456789abcdefghijkmnpqrstuvwxyz"

func newID(prefix string) string {
	b := make([]byte, 6)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(idAlphabet))))
		b[i] = idAlphabet[n.Int64()]
	}
	return prefix + string(b)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func fromJSON(s string) []string {
	var out []string
	json.Unmarshal([]byte(s), &out)
	return out
}
