package acceptance

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The key set of "ais state" is the API tsm depends on (api 1). Adding a key
// is allowed; removing or renaming one is a breaking change.
func TestStateSchema(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "-d", "schema")
	r := l.MustAis(nil, "state", "--json")

	var raw map[string]any
	must(t, json.Unmarshal([]byte(r.Stdout), &raw))
	if raw["api"] != float64(1) {
		t.Errorf("api = %v, want 1", raw["api"])
	}
	requireKeys(t, "state", raw, "api", "host", "worktree", "notify_window", "notify_seen_after", "profiles", "sessions")
	requireKeys(t, "state.worktree", raw["worktree"].(map[string]any), "base", "auto", "from", "detach")
	prof := raw["profiles"].([]any)[0].(map[string]any)
	requireKeys(t, "profile", prof, "name", "command", "flags", "resumable", "forkable", "prompts")
	sess := raw["sessions"].([]any)[0].(map[string]any)
	requireKeys(t, "session", sess,
		"id", "profile", "description", "name", "status", "host", "cwd", "worktree", "tmux",
		"pid", "started_at", "ended_at", "exit_code", "conversation", "args", "restart_of",
		"fork_of", "can_restart", "can_resume", "can_fork", "last_input_at", "notifications")
	requireKeys(t, "session.tmux", sess["tmux"].(map[string]any),
		"session_name", "session_id", "window_id", "window_index", "pane_id", "pane_index")

	st := l.State()
	names := []string{}
	for _, p := range st.Profiles {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "fake,plain" {
		t.Errorf("profiles = %v", names)
	}
	for _, p := range st.Profiles {
		if p.Name == "fake" && (!p.Resumable || !p.Forkable || !contains(p.Flags, "--from-config")) {
			t.Errorf("fake profile = %+v", p)
		}
		if p.Name == "plain" && (p.Resumable || p.Forkable) {
			t.Errorf("plain profile = %+v", p)
		}
	}
	if st.Worktree.Base != l.WtBase || st.Worktree.From != "auto" || st.Worktree.Auto {
		t.Errorf("worktree = %+v", st.Worktree)
	}
	l.Stop(s, 0)
}

func requireKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks key %q", what, k)
		}
	}
}

// Newest first; --active drops the dead; --since keeps newer notifications
// and only the sessions that have one.
func TestStateFilters(t *testing.T) {
	l := NewLab(t)
	first := l.Start(l.Home, "run", "fake", "-d", "first")
	l.Stop(first, 0)
	time.Sleep(1100 * time.Millisecond) // started_at has one-second resolution
	second := l.Start(l.Home, "run", "fake", "-d", "second")

	st := l.State()
	if len(st.Sessions) != 2 || st.Sessions[0].ID != second.ID {
		t.Errorf("order = %v, want newest first", ids(st.Sessions))
	}
	if act := l.State("--active").Sessions; len(act) != 1 || act[0].ID != second.ID {
		t.Errorf("--active = %v", ids(act))
	}

	in := []string{"AIS_SESSION=" + second.ID}
	l.MustAis(in, "notify", "one")
	cut := l.Session(second.ID).Notes[0].ID
	l.MustAis(in, "notify", "two")
	since := l.State("--since", cut).Sessions
	if len(since) != 1 || len(since[0].Notes) != 1 || since[0].Notes[0].Message != "two" {
		t.Errorf("--since %s = %+v", cut, since)
	}
	l.Stop(second, 0)
}

func ids(ss []Session) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

// ais env: what a shell beside a session needs, as shell or JSON.
func TestEnv(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("envrepo")
	s := l.Start(repo, "run", "fake", "-w", "-d", "Env check")
	cwd := l.Session(s.ID).Cwd

	r := l.MustAis(nil, "env", s.ID, "--json")
	var out struct {
		API int               `json:"api"`
		ID  string            `json:"id"`
		Cwd string            `json:"cwd"`
		Env map[string]string `json:"env"`
	}
	must(t, json.Unmarshal([]byte(r.Stdout), &out))
	if out.API != 1 || out.ID != s.ID || out.Cwd != cwd {
		t.Errorf("env --json = %+v", out)
	}
	for k, v := range map[string]string{
		"AIS_SESSION": s.ID, "AIS_SIBLING": "1", "AIS_PROFILE": "fake", "AIS_HARNESS": "fake",
		"AIS_SESSION_DESCRIPTION": "Env check", "FAKE_PROFILE_ENV": "from-profile",
		"AIS_WORKTREE": cwd, "AIS_WORKTREE_BASE": filepath.Dir(cwd), "AIS_REPO_MAIN": repo,
	} {
		if out.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, out.Env[k], v)
		}
	}

	shell := l.MustAis(nil, "env", s.ID[:6]).Stdout // a unique prefix will do
	if !strings.HasPrefix(shell, "cd "+cwd+"\n") && !strings.HasPrefix(shell, "cd '"+cwd+"'\n") {
		t.Errorf("shell form does not start with cd:\n%s", shell)
	}
	if !strings.Contains(shell, "export AIS_SIBLING=1\n") || !strings.Contains(shell, "export AIS_SESSION="+s.ID+"\n") {
		t.Errorf("shell form lacks exports:\n%s", shell)
	}
	if r := l.Ais(nil, "env", "zzzzzz"); r.Code == 0 {
		t.Errorf("env of an unknown id succeeded")
	}
	l.Stop(s, 0)
}
