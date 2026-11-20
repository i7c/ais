package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTicketURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/gpwsoi/backend/issues/12":  "https://github.com/gpwsoi/backend/issues/12",
		"https://github.com/gpwsoi/backend/issues/12/": "https://github.com/gpwsoi/backend/issues/12",
		"https://github.com/gpwsoi/backend/pull/7":     "https://github.com/gpwsoi/backend/issues/7",
		"gpwsoi/infra-workloads#3":                     "https://github.com/gpwsoi/infra-workloads/issues/3",
	} {
		if got, err := ticketURL(in); err != nil || got != want {
			t.Errorf("ticketURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"#12", "12", "gpwsoi/backend", "https://example.com/a/b/issues/1"} {
		if _, err := ticketURL(bad); err == nil {
			t.Errorf("ticketURL(%q) accepted", bad)
		}
	}
}

func TestParseUntil(t *testing.T) {
	from := time.Date(2026, 11, 20, 15, 30, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"eod":        time.Date(2026, 11, 20, 23, 59, 59, 0, time.Local),
		"":           time.Date(2026, 11, 20, 23, 59, 59, 0, time.Local),
		"18:00":      time.Date(2026, 11, 20, 18, 0, 0, 0, time.Local),
		"09:00":      time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local), // already past: tomorrow
		"2026-11-25": time.Date(2026, 11, 25, 23, 59, 59, 0, time.Local),
	} {
		got, err := parseUntil(in, from)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseUntil(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseUntil("tomorrowish", from); err == nil {
		t.Error("nonsense accepted")
	}
}

// An override holds until its time, then the config is back.
func TestOverridesExpire(t *testing.T) {
	dir := t.TempDir()
	saved := homeDir
	homeDir = func() string { return dir }
	defer func() { homeDir = saved }()
	clock := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	defer func() { now = time.Now }()

	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`INSERT INTO overrides (key, value, until, set_at) VALUES ('per_day', 32, ?, ?)`, stamp(clock.Add(time.Hour)), stamp(clock))
	got, _, err := effectiveLimits(db, defaultLimits)
	if err != nil || got.PerDay != 32 || got.WIP != defaultLimits.WIP {
		t.Fatalf("with override: %+v, %v", got, err)
	}
	clock = clock.Add(2 * time.Hour)
	got, _, _ = effectiveLimits(db, defaultLimits)
	if got.PerDay != defaultLimits.PerDay {
		t.Errorf("expired override still holds: %+v", got)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM overrides`).Scan(&n)
	if n != 0 {
		t.Errorf("expired override left in the table")
	}
}

// Only one planner per workspace may be open: the schema says so.
func TestOnePlannerPerWorkspace(t *testing.T) {
	dir := t.TempDir()
	saved := homeDir
	homeDir = func() string { return dir }
	defer func() { homeDir = saved }()
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ins := `INSERT INTO planners (id, workspace, state, started_at) VALUES (?, 'soi', ?, '2026-11-20T10:00:00Z')`
	if _, err := db.Exec(ins, "a", "open"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ins, "b", "open"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second open planner was accepted: %v", err)
	}
	if _, err := db.Exec(ins, "c", "decided"); err != nil {
		t.Errorf("a closed planner beside an open one was refused: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := parseConfig(map[string]any{
		"workspace": map[string]any{"soi": map[string]any{
			"source": map[string]any{"owner": "gpwsoi", "project": int64(3)},
			"repos":  map[string]any{"backend": "/r/b"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := c.Workspaces["soi"]
	if c.Limits != defaultLimits || w.TmuxSession != "soi" || w.Profile != "claude" || w.Source.Ready != "ready" ||
		w.Source.InProgress != "In Progress" || !w.Source.Comment || w.Planner.IdleTimeout != 48*time.Hour ||
		w.Planner.Prompt != "/plan-sessions soi" || w.Session.Prompt != defaultSessionPrompt || w.Repos["backend"] != "/r/b" {
		t.Errorf("defaults: limits %+v, workspace %+v", c.Limits, w)
	}
	if _, err := c.workspace(""); err != nil {
		t.Errorf("a lone workspace is not the default: %v", err)
	}
}

// A proposal typed into its pane whose session never came is queued again.
func TestReconcileRequeuesAStuckStart(t *testing.T) {
	dir := t.TempDir()
	saved := homeDir
	homeDir = func() string { return dir }
	defer func() { homeDir = saved }()
	clock := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	defer func() { now = time.Now }()
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ins := `INSERT INTO proposals (id, workspace, title, tickets, repos, profile, prompt, state, created_at, started_at)
		VALUES (?, 'soi', 't', '[]', '[]', 'claude', 'p', 'starting', ?, ?)`
	db.Exec(ins, "pstuck1", stamp(clock), stamp(clock.Add(-3*time.Minute)))
	db.Exec(ins, "pfresh1", stamp(clock), stamp(clock.Add(-30*time.Second)))
	d := &dispatcher{cfg: &Config{Workspaces: map[string]*Workspace{}}, db: db}
	if err := d.reconcile(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"pstuck1": "queued", "pfresh1": "starting"} {
		var state string
		db.QueryRow(`SELECT state FROM proposals WHERE id = ?`, id).Scan(&state)
		if state != want {
			t.Errorf("%s is %s, want %s", id, state, want)
		}
	}
}

func TestClaudeTrusts(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if claudeTrusts("/home/x/wt") {
		t.Error("trusted without a .claude.json")
	}
	os.WriteFile(filepath.Join(h, ".claude.json"), []byte(`{"projects": {"/home/x/wt": {"hasTrustDialogAccepted": true},
		"/home/x/other": {"hasTrustDialogAccepted": false}}}`), 0o644)
	for path, want := range map[string]bool{"/home/x/wt": true, "/home/x/wt/a/b": true, "/home/x/other": false, "/home/x": false} {
		if got := claudeTrusts(path); got != want {
			t.Errorf("claudeTrusts(%s) = %v, want %v", path, got, want)
		}
	}
}
