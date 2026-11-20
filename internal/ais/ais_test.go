package ais

// Unit tests for what the acceptance suite cannot stage cheaply: process
// ancestry, config validation, and the record and settings.json formats.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// dies reports whether fn calls die.
func dies(fn func()) (died bool) {
	defer func() {
		if e := recover(); e != nil {
			_, died = e.(exit)
		}
	}()
	fn()
	return false
}

// The command names are the ones macOS reports: claude is a real binary, so
// ps gives "claude", while a Python ais showed up as its interpreter.
func TestSpeaksFor(t *testing.T) {
	r := &Record{PID: intPtr(500), Harness: "claude", Command: []string{"claude", "--session-id", "x"}}
	chain := func(a ...ancestor) bool {
		saved := ancestry
		defer func() { ancestry = saved }()
		ancestry = func() []ancestor { return a }
		return speaksFor(r)
	}
	if !chain(ancestor{499, "sh"}, ancestor{500, "claude"}, ancestor{400, "ais"}) {
		t.Error("a hook under the session's harness may not speak")
	}
	if chain(ancestor{700, "sh"}, ancestor{699, "claude"}, ancestor{500, "claude"}) {
		t.Error("a nested harness spoke")
	}
	if chain(ancestor{900, "sh"}, ancestor{800, "zsh"}, ancestor{1, "launchd"}) {
		t.Error("an unrelated process spoke")
	}
	if !chain() {
		t.Error("unreadable ancestry should count as the common case")
	}
	saved := ancestry
	ancestry = func() []ancestor { return []ancestor{{1, "launchd"}} }
	defer func() { ancestry = saved }()
	if !speaksFor(&Record{Harness: "claude"}) {
		t.Error("a record with no pid yet should be allowed to speak")
	}
}

func TestArgvHandling(t *testing.T) {
	conflicts := []string{"-c", "--continue", "--fork-session", "--resume", "--session-id", "-r"}
	for argv, want := range map[string]string{
		"--resume abc": "--resume", "--resume=abc": "--resume", "-c": "-c",
		"--model opus": "", "-- --resume x": "",
	} {
		if got := namesASession(strings.Fields(argv), conflicts); got != want {
			t.Errorf("namesASession(%q) = %q, want %q", argv, got, want)
		}
	}
	if got := stripKnownFlags([]string{"--model", "opus", "--session-id", "u"}, conflicts); !reflect.DeepEqual(got, []string{"--model", "opus"}) {
		t.Errorf("strip = %q", got)
	}
	if got := stripKnownFlags([]string{"--session-id=u", "-p"}, conflicts); !reflect.DeepEqual(got, []string{"-p"}) {
		t.Errorf("strip = %q", got)
	}
}

func TestSessionSettings(t *testing.T) {
	if !dies(func() { sessionSettings(map[string]any{"new": []any{"--session-id"}}, "claude") }) {
		t.Error("a template without {id} was accepted")
	}
	if !dies(func() { sessionSettings(map[string]any{"id": "sequential"}, "claude") }) {
		t.Error("an unknown id kind was accepted")
	}
	if sessionSettings(nil, "aider") != nil {
		t.Error("an absent table is not an error, and not a table")
	}
	conf := sessionSettings(map[string]any{
		"new": []any{"--session-id", "{id}"}, "resume": []any{"--resume", "{id}"}, "conflicts": []any{"-c"},
	}, "claude")
	if !reflect.DeepEqual(conf.Conflicts, []string{"--resume", "--session-id", "-c"}) {
		t.Errorf("conflicts = %q", conf.Conflicts)
	}
	// Every block "ais install" writes has to survive being read back.
	for harness, block := range map[string]string{"claude": claudeSessionConfig, "pi": piSessionConfig} {
		var cfg map[string]any
		if _, err := toml.Decode(block, &cfg); err != nil {
			t.Fatal(err)
		}
		table := cfg["profile"].(map[string]any)[harness].(map[string]any)["session"]
		c := sessionSettings(table, harness)
		if !contains(c.Conflicts, "--session-id") || len(c.New) == 0 || len(c.Resume) == 0 {
			t.Errorf("%s shipped table = %+v", harness, c)
		}
	}
	if id := mintSessionID("short"); len(id) != idLength {
		t.Errorf("short id %q", id)
	}
	if parts := strings.Split(mintSessionID("uuid4"), "-"); len(parts) != 5 {
		t.Errorf("uuid4 has %d parts", len(parts))
	}
}

func TestProfileTableMergesOldAndNewSpelling(t *testing.T) {
	cfg := config{
		"harness": map[string]any{"x": map[string]any{"flags": []any{"--old"}, "env": map[string]any{"A": "1", "B": "old"}}},
		"profile": map[string]any{"x": map[string]any{"env": map[string]any{"B": "new"}}, "y": map[string]any{}},
	}
	got := profileTable(cfg, "x")
	if !reflect.DeepEqual(got["flags"], []any{"--old"}) {
		t.Errorf("flags = %v", got["flags"])
	}
	if env := got["env"].(map[string]any); env["A"] != "1" || env["B"] != "new" {
		t.Errorf("env = %v", env)
	}
	if names := profileNames(cfg); !reflect.DeepEqual(names, []string{"x", "y"}) {
		t.Errorf("names = %v", names)
	}
}

func TestWorktreeSettings(t *testing.T) {
	t.Setenv("AIS_WT_BASE", "")
	if got := worktreeSettings(config{}); got.Base != filepath.Join(home, "wt") || !got.Fetch || got.Auto || got.From != "auto" || got.Detach {
		t.Errorf("defaults = %+v", got)
	}
	if got := worktreeSettings(config{"worktree": map[string]any{"base": "~/elsewhere", "copy": ".env"}}); got.Base != filepath.Join(home, "elsewhere") || !reflect.DeepEqual(got.Copy, []string{".env"}) {
		t.Errorf("base/copy = %+v", got)
	}
	t.Setenv("AIS_WT_BASE", "/tmp/somewhere")
	if got := worktreeSettings(config{"worktree": map[string]any{"base": "~/elsewhere"}}); got.Base != "/tmp/somewhere" {
		t.Errorf("AIS_WT_BASE lost to the config: %s", got.Base)
	}
	for _, bad := range []map[string]any{{"base": int64(7)}, {"copy": []any{int64(1)}}, {"fetch": "yes"}, {"auto": "yes"}, {"from": "upstream"}, {"detach": "yes"}} {
		if !dies(func() { worktreeSettings(config{"worktree": bad}) }) {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Fix the auth bug":            "fix-auth-bug",
		"the a an":                    "the-a-an", // only stopwords: keep them
		"Café über naïve":             "cafe-uber-naive",
		"x" + strings.Repeat("y", 40): ("x" + strings.Repeat("y", 40))[:32],
		"!!!":                         "",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func withHome(t *testing.T) {
	dir := t.TempDir()
	saved := [3]string{aisHome, sessionPath, lockPath}
	aisHome, sessionPath, lockPath = dir, filepath.Join(dir, "sessions.json"), filepath.Join(dir, "sessions.lock")
	t.Cleanup(func() { aisHome, sessionPath, lockPath = saved[0], saved[1], saved[2] })
}

func TestConversationHistory(t *testing.T) {
	withHome(t)
	writeSessions([]*Record{{ID: "abc", SessionIDs: []string{}}})
	rememberSessionID("abc", "one")
	rememberSessionID("abc", "one")
	rememberSessionID("abc", "two")
	if got := readSessions()[0].SessionIDs; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Errorf("history = %q", got)
	}
	for i := 0; i < sessionIDKeep+5; i++ {
		rememberSessionID("abc", "id-"+string(rune('a'+i)))
	}
	if got := readSessions()[0]; len(got.SessionIDs) != sessionIDKeep || got.conversation() != "id-"+string(rune('a'+sessionIDKeep+4)) {
		t.Errorf("capped history = %q", got.SessionIDs)
	}
	if rememberSessionID("nope", "one") {
		t.Error("an unknown session was updated")
	}
}

// Keys this ais does not know — written by a newer or older one — survive a
// read-modify-write, and null stays null.
func TestRecordKeepsUnknownKeys(t *testing.T) {
	withHome(t)
	raw := `{"version": 1, "sessions": [{"id": "abc", "description": null, "future": {"x": [1, 2]},
		"command": ["c"], "session_ids": [], "status": "dead", "pid": null, "hostname": "h", "cwd": "/",
		"tmux": {}, "started_at": "2026-01-01T00:00:00+00:00", "ended_at": null, "exit_code": null,
		"notifications": [{"id": "n1", "at": "2026-01-01T00:00:00+00:00", "message": "m",
		  "delivered": true, "suppressed": false, "colour": "blue"}]}]}`
	must(os.WriteFile(sessionPath, []byte(raw), 0o644))
	mutate(func(ss []*Record) []*Record { return ss })
	var back struct {
		Sessions []map[string]any `json:"sessions"`
	}
	b, _ := os.ReadFile(sessionPath)
	must(json.Unmarshal(b, &back))
	s := back.Sessions[0]
	if !reflect.DeepEqual(s["future"], map[string]any{"x": []any{1.0, 2.0}}) {
		t.Errorf("future = %v", s["future"])
	}
	if v, ok := s["description"]; !ok || v != nil {
		t.Errorf("description = %v (present %v), want null", v, ok)
	}
	if n := s["notifications"].([]any)[0].(map[string]any); n["colour"] != "blue" {
		t.Errorf("notification lost its unknown key: %v", n)
	}
}

// Rewriting someone's settings.json keeps its key order and its numbers.
func TestOrderedJSON(t *testing.T) {
	in := `{"zeta": 1, "alpha": {"b": [1.50, "x"], "a": null}, "hooks": {}}`
	v, err := decodeOrdered([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(encodeOrdered(v))
	if strings.Index(out, "zeta") > strings.Index(out, "alpha") || strings.Index(out, `"b"`) > strings.Index(out, `"a"`) {
		t.Errorf("order lost:\n%s", out)
	}
	if !strings.Contains(out, "1.50") || !strings.Contains(out, "null") {
		t.Errorf("values changed:\n%s", out)
	}
	if _, err := decodeOrdered([]byte(`{"a": 1} trailing`)); err == nil {
		t.Error("trailing data accepted")
	}
}

// startPoint in the cases the acceptance suite does not stage.
func TestStartPointEdgeCases(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	must(os.WriteFile(filepath.Join(dir, "gitconfig"), []byte("[user]\nname = t\nemail = t@t\n[init]\ndefaultBranch = main\n"), 0o644))
	g := func(d string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(d, msg string) {
		must(os.WriteFile(filepath.Join(d, msg), []byte(msg), 0o644))
		g(d, "add", msg)
		g(d, "commit", "-qm", msg)
	}

	// No remote at all: from=origin falls back to the local default branch.
	solo := filepath.Join(dir, "solo")
	g(dir, "init", "-q", solo)
	commit(solo, "one")
	if got := startPoint(solo, "origin"); got != "main" {
		t.Errorf("no remote, from=origin: %s", got)
	}

	// A remote not called origin is still followed.
	bare := filepath.Join(dir, "up.git")
	g(dir, "init", "-q", "--bare", bare)
	g(solo, "push", "-q", bare, "main")
	clone := filepath.Join(dir, "clone")
	g(dir, "clone", "-q", "-o", "upstream", bare, clone)
	if got := startPoint(clone, "origin"); got != "upstream/main" {
		t.Errorf("remote named upstream: %s", got)
	}

	// Diverged: auto takes the remote.
	commit(clone, "local")
	other := filepath.Join(dir, "other")
	g(dir, "clone", "-q", bare, other)
	commit(other, "remote")
	g(other, "push", "-q", "origin", "main")
	g(clone, "fetch", "-q", "upstream")
	if got := startPoint(clone, "auto"); got != "upstream/main" {
		t.Errorf("diverged, from=auto: %s", got)
	}
}
