package acceptance

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var idRe = regexp.MustCompile(`^[23456789abcdefghijkmnpqrstuvwxyz]{10}$`)

// A session is recorded with everything tsm reads, its harness gets the argv
// and env ais promises, and the end is recorded with the exit code.
func TestRunRecordsSessionAndHarnessSeesIt(t *testing.T) {
	l := NewLab(t)
	dir := filepath.Join(l.Dir, "plainwork")
	l.WriteFile(filepath.Join(dir, ".keep"), "")

	s := l.Start(dir, "run", "fake", "-d", "Fix the auth bug", "--", "--model", "opus")
	if !idRe.MatchString(s.ID) {
		t.Errorf("session id %q is not 10 chars of the ais alphabet", s.ID)
	}

	got := l.Session(s.ID)
	if got.Status != "active" || got.Profile != "fake" || got.Description != "Fix the auth bug" {
		t.Errorf("recorded %+v", got)
	}
	if got.Name != "fix-auth-bug" {
		t.Errorf("name = %q, want fix-auth-bug", got.Name)
	}
	if got.Cwd != dir {
		t.Errorf("cwd = %q, want %q", got.Cwd, dir)
	}
	if got.Tmux.PaneID != s.Pane || got.Tmux.SessionName != "lab" || got.Tmux.WindowID == "" {
		t.Errorf("tmux = %+v", got.Tmux)
	}
	if got.PID == nil || *got.PID == 0 {
		t.Errorf("no pid recorded")
	}
	if !contains(got.Args, "--model") || !contains(got.Args, "opus") || len(got.Args) != 2 {
		t.Errorf("args = %v, want exactly what was typed after --", got.Args)
	}
	conv := deref(got.Conversation)
	if !uuidRe.MatchString(conv) {
		t.Errorf("conversation %q is not the uuid4 ais minted", conv)
	}
	if !got.CanResume || !got.CanFork || got.CanRestart {
		t.Errorf("can_* on a live session = resume %v fork %v restart %v", got.CanResume, got.CanFork, got.CanRestart)
	}

	f := l.Fake(s.ID)
	want := []string{"--from-config", "--session-id", conv, "--model", "opus"}
	if strings.Join(f.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %q, want %q (command + config flags + session flags + typed)", f.Argv, want)
	}
	if f.Cwd != dir {
		t.Errorf("harness cwd = %q", f.Cwd)
	}
	for k, v := range map[string]string{
		"AIS_SESSION": s.ID, "AIS_PROFILE": "fake", "AIS_HARNESS": "fake",
		"AIS_SESSION_DESCRIPTION": "Fix the auth bug", "FAKE_PROFILE_ENV": "from-profile",
	} {
		if f.Env[k] != v {
			t.Errorf("harness env %s = %q, want %q", k, f.Env[k], v)
		}
	}
	if _, ok := f.Env["AIS_WORKTREE"]; ok {
		t.Errorf("AIS_WORKTREE set for a session without a worktree")
	}

	// The pane is marked and the window named for as long as it runs.
	for opt, want := range map[string]string{
		"@ais_id": s.ID, "@ais_profile": "fake", "@ais_desc": "Fix the auth bug", "@cwd": dir,
	} {
		if v := l.Format(s.Pane, "#{"+opt+"}"); v != want {
			t.Errorf("pane %s = %q, want %q", opt, v, want)
		}
	}
	if w := l.Format(s.Pane, "#{window_name}"); w != "fix-auth-bug" {
		t.Errorf("window name = %q", w)
	}

	l.Stop(s, 3)
	if code := l.AisExit(s); code != "3" {
		t.Errorf("ais exited %s, want the harness's 3", code)
	}
	got = l.Session(s.ID)
	if got.Status != "dead" || deref(got.ExitCode) != 3 || got.EndedAt == nil {
		t.Errorf("after exit: status %s exit %v ended %v", got.Status, deref(got.ExitCode), got.EndedAt)
	}
	if !got.CanRestart {
		t.Errorf("a dead local session with its directory should be restartable")
	}
	if v := l.Format(s.Pane, "#{@ais_id}"); v != "" {
		t.Errorf("pane mark left behind: %q", v)
	}
	if w := l.Format(s.Pane, "#{window_name}"); w == "fix-auth-bug" {
		t.Errorf("window name not restored")
	}
}

// "ais [opts] -- <profile> args" is "ais run <profile> [opts] -- args".
func TestRunAbbreviatedForm(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "-d", "short form", "--", "fake", "--model", "x")
	got := l.Session(s.ID)
	if got.Profile != "fake" || got.Description != "short form" {
		t.Errorf("recorded %+v", got)
	}
	argv := l.Fake(s.ID).Argv
	if argv[len(argv)-2] != "--model" || argv[len(argv)-1] != "x" {
		t.Errorf("argv = %q", argv)
	}
	l.Stop(s, 0)
}

// A dry run says what it would run and records nothing.
func TestRunDryRun(t *testing.T) {
	l := NewLab(t)
	r := l.AisIn(l.Home, []string{"TMUX_PANE=" + l.HerePane()}, "", "run", "fake", "-n", "-d", "a dry one", "--", "--x")
	if r.Code != 0 {
		t.Fatalf("dry run failed:\n%s", r)
	}
	for _, want := range []string{"--from-config", "--session-id", "--x", "AIS_SESSION="} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, r.Stdout)
		}
	}
	if n := len(l.State().Sessions); n != 0 {
		t.Errorf("dry run recorded %d session(s)", n)
	}
}

// A conversation flag typed by hand wins: ais mints no id of its own.
func TestRunTypedConversationFlagIsLeftToHarness(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "--", "--resume", "abc")
	argv := l.Fake(s.ID).Argv
	if contains(argv, "--session-id") {
		t.Errorf("ais added --session-id next to a typed --resume: %q", argv)
	}
	if c := l.Session(s.ID).Conversation; c != nil {
		t.Errorf("conversation = %q, want none recorded", *c)
	}
	l.Stop(s, 0)
}

// A profile without a session table runs bare and cannot be resumed.
func TestRunProfileWithoutSessionTable(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "plain")
	if argv := l.Fake(s.ID).Argv; len(argv) != 0 {
		t.Errorf("argv = %q, want nothing", argv)
	}
	got := l.Session(s.ID)
	if got.Conversation != nil || got.CanResume || got.CanFork {
		t.Errorf("plain profile: conversation %v resume %v fork %v", got.Conversation, got.CanResume, got.CanFork)
	}
	l.Stop(s, 0)
}

func TestRunCwd(t *testing.T) {
	l := NewLab(t)
	dir := filepath.Join(l.Dir, "elsewhere")
	l.WriteFile(filepath.Join(dir, ".keep"), "")
	s := l.Start(l.Home, "run", "fake", "--cwd", dir)
	if got := l.Session(s.ID).Cwd; got != dir {
		t.Errorf("cwd = %q, want %q", got, dir)
	}
	if got := l.Fake(s.ID).Cwd; got != dir {
		t.Errorf("harness cwd = %q", got)
	}
	l.Stop(s, 0)

	r := l.AisIn(l.Home, []string{"TMUX_PANE=" + l.HerePane()}, "", "run", "fake", "--cwd", filepath.Join(l.Dir, "nope"))
	if r.Code == 0 {
		t.Errorf("--cwd to a missing directory succeeded")
	}
}

// Refusals happen before anything is recorded.
func TestRunRefusals(t *testing.T) {
	l := NewLab(t)
	here := []string{"TMUX_PANE=" + l.HerePane()}
	cases := map[string]struct {
		env  []string
		args []string
	}{
		"outside tmux":           {nil, []string{"run", "fake"}},
		"harness not on PATH":    {here, []string{"run", "no-such-harness"}},
		"no profile named":       {here, []string{"run"}},
		"nothing after --":       {here, []string{"--"}},
		"unknown run option":     {here, []string{"run", "fake", "--bogus"}},
		"fork with -w":           {here, []string{"run", "--fork", "abc", "-w"}},
		"no-worktree with from":  {here, []string{"run", "fake", "--no-worktree", "--from", "origin"}},
		"unknown command":        {nil, []string{"frobnicate"}},
	}
	for name, c := range cases {
		r := l.Ais(c.env, c.args...)
		if r.Code == 0 {
			t.Errorf("%s: exit 0, want a refusal\n%s", name, r)
		}
	}
	if n := len(l.State().Sessions); n != 0 {
		t.Errorf("refusals recorded %d session(s)", n)
	}
}

func TestRenameOff(t *testing.T) {
	l := NewLab(t)
	l.SetConfig(strings.Replace(l.BaseConfig(), "rename = true", "rename = false", 1))
	s := l.Start(l.Home, "run", "fake", "-d", "Keep my window name")
	if w := l.Format(s.Pane, "#{window_name}"); w == "keep-my-window-name" {
		t.Errorf("window renamed although tmux.rename = false")
	}
	l.Stop(s, 0)
}

// A session whose ais was killed outright is found dead by the next reader.
func TestReapAfterHardKill(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake")
	fakePID := strings.TrimSpace(l.ReadFile(filepath.Join(l.Log, s.ID, "pid")))
	aisPID := parentOf(t, fakePID)
	killHard(t, aisPID)
	killHard(t, fakePID)
	l.Eventually("session reaped", func() bool { return l.Session(s.ID).Status == "dead" })
	if got := l.Session(s.ID); got.ExitCode != nil {
		t.Errorf("a reaped session has exit code %d; nobody saw it exit", *got.ExitCode)
	}
	if raw := l.SessionsFile(); raw["version"] != float64(1) {
		t.Errorf("sessions.json version = %v, want 1", raw["version"])
	}
}

