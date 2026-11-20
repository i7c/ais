package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --prompt hands the harness its first message, last on its argv; that
// message is not you typing, but everything after it is.
func TestRunWithPrompt(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "-d", "Prompted", "--prompt", "/work-tickets #12", "--", "--model", "x")
	argv := l.Fake(s.ID).Argv
	if n := len(argv); n < 4 || argv[n-2] != "--" || argv[n-1] != "/work-tickets #12" || argv[n-4] != "--model" {
		t.Errorf("argv = %q, want the typed args, then -- and the prompt", argv)
	}
	got := l.Session(s.ID)
	if strings.Join(got.Args, " ") != "--model x" {
		t.Errorf("args = %q; the prompt must not be replayed by restart", got.Args)
	}
	if got.LastInputAt != nil {
		t.Errorf("last_input_at = %q after only the first prompt", *got.LastInputAt)
	}

	l.Submit(s, "hello there")
	first := deref(l.Session(s.ID).LastInputAt)
	if first == "" {
		t.Fatalf("a typed prompt did not set last_input_at")
	}
	// The first prompt's text is skipped once, not forever.
	l.Submit(s, "/work-tickets #12")
	if deref(l.Session(s.ID).LastInputAt) == "" {
		t.Errorf("the same text typed again was not counted")
	}
	l.Stop(s, 0)

	re := l.Start(l.Home, "restart", s.ID)
	if argv := l.Fake(re.ID).Argv; contains(argv, "/work-tickets #12") {
		t.Errorf("restart asked the first question again: %q", argv)
	}
	l.Stop(re, 0)
}

func TestPromptFileAndRefusals(t *testing.T) {
	l := NewLab(t)
	file := filepath.Join(l.Dir, "prompt.txt")
	l.WriteFile(file, "from a file\n")
	s := l.Start(l.Home, "run", "fake", "--prompt-file", file)
	if argv := l.Fake(s.ID).Argv; argv[len(argv)-1] != "from a file" {
		t.Errorf("--prompt-file argv = %q", argv)
	}
	l.Stop(s, 0)

	pane, _ := l.Window(l.Home, "echo 'from stdin' | "+l.AisCommand("run", "fake", "--prompt-file", "-"))
	var id string
	l.Eventually("stdin-prompted session up", func() bool {
		for _, s := range l.State().Sessions {
			if s.Tmux.PaneID == pane {
				id = s.ID
			}
		}
		return id != "" && fileExists(filepath.Join(l.Log, id, "ready"))
	})
	if argv := l.Fake(id).Argv; argv[len(argv)-1] != "from stdin" {
		t.Errorf("--prompt-file - argv = %q", argv)
	}

	// A profile ais knows by name gets the built-in template; one it does
	// not, and that has none, refuses a prompt before recording anything.
	l.WriteConfig("[profile.claude]\ncommand = [\"" + filepath.Join(l.Bin, "fake") + "\"]\n")
	for _, p := range l.State().Profiles {
		if (p.Name == "claude" || p.Name == "fake") != p.Prompts {
			t.Errorf("profile %s prompts = %v", p.Name, p.Prompts)
		}
	}
	c := l.Start(l.Home, "run", "claude", "--prompt", "hi")
	if argv := l.Fake(c.ID).Argv; strings.Join(argv[len(argv)-2:], " ") != "-- hi" {
		t.Errorf("built-in claude template argv = %q", argv)
	}
	l.Stop(c, 0)
	before := len(l.State().Sessions)
	here := []string{"TMUX_PANE=" + l.HerePane()}
	for name, args := range map[string][]string{
		"profile without a template": {"run", "plain", "--prompt", "hi"},
		"both prompt flags":          {"run", "fake", "--prompt", "a", "--prompt-file", file},
		"prompt with fork":           {"run", "--fork", "abc", "--prompt", "a"},
		"missing prompt file":        {"run", "fake", "--prompt-file", filepath.Join(l.Dir, "nope")},
	} {
		if r := l.Ais(here, args...); r.Code == 0 {
			t.Errorf("%s: succeeded", name)
		}
	}
	if n := len(l.State().Sessions); n != before {
		t.Errorf("refusals recorded %d session(s)", n-before)
	}
}

// _activity only counts the session's own harness, like _session-seen.
func TestActivityGuards(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake")
	for name, env := range map[string][]string{
		"sibling":   {"AIS_SESSION=" + s.ID, "AIS_SIBLING=1"},
		"unrelated": {"AIS_SESSION=" + s.ID},
		"unknown":   {"AIS_SESSION=zzzzzzzzzz"},
	} {
		if r := l.AisIn(l.Home, env, `{"prompt":"x"}`, "_activity"); r.Code != 0 {
			t.Errorf("%s: _activity exited %d", name, r.Code)
		}
	}
	if r := l.AisIn(l.Home, []string{"AIS_SESSION=" + s.ID}, "garbage", "_activity"); r.Code != 0 {
		t.Errorf("_activity on garbage exited %d", r.Code)
	}
	if got := l.Session(s.ID).LastInputAt; got != nil {
		t.Errorf("last_input_at = %q, set by someone other than the harness", *got)
	}
	l.Stop(s, 0)
}

// --repos: one worktree per repo, side by side, and the session starts in
// the directory that holds them.
func TestRunRepos(t *testing.T) {
	l := NewLab(t)
	backend, iac := l.Repo("backend"), l.Repo("iac")
	dry := l.AisIn(l.Home, []string{"TMUX_PANE=" + l.HerePane()}, "", "run", "fake", "-n", "-d", "Cross cut", "--repos", backend+","+iac)
	if dry.Code != 0 || strings.Count(dry.Stdout, "worktree:") != 2 {
		t.Errorf("dry run:\n%s", dry)
	}

	s := l.Start(l.Home, "run", "fake", "-d", "Cross cut", "--repos", backend, "--repos", iac)
	base := filepath.Join(l.WtBase, "cross-cut")
	got := l.Session(s.ID)
	if got.Cwd != base || l.Fake(s.ID).Cwd != base {
		t.Errorf("cwd = %q (harness %q), want %q", got.Cwd, l.Fake(s.ID).Cwd, base)
	}
	if got.Worktree["base"] != base || got.Worktree["branch"] != "cross-cut" {
		t.Errorf("worktree record = %v", got.Worktree)
	}
	for _, repo := range []string{"backend", "iac"} {
		if b := l.Git(filepath.Join(base, repo), "branch", "--show-current"); b != "cross-cut" {
			t.Errorf("%s on branch %q", repo, b)
		}
	}
	env := l.Fake(s.ID).Env
	if env["AIS_WORKTREE_BASE"] != base {
		t.Errorf("AIS_WORKTREE_BASE = %q", env["AIS_WORKTREE_BASE"])
	}
	in := []string{"AIS_SESSION=" + s.ID}
	ls := l.MustAis(in, "wt", "ls").Stdout
	if !strings.Contains(ls, "cross-cut/backend") || !strings.Contains(ls, "cross-cut/iac") {
		t.Errorf("wt ls:\n%s", ls)
	}
	if p := strings.TrimSpace(l.MustAis(in, "wt", "path", "iac").Stdout); p != filepath.Join(base, "iac") {
		t.Errorf("wt path iac = %q", p)
	}
	l.Stop(s, 0)

	re := l.Start(base, "restart", s.ID)
	if l.Session(re.ID).Cwd != base {
		t.Errorf("restart of a multi-repo session moved to %q", l.Session(re.ID).Cwd)
	}
	l.Stop(re, 0)

	plain := filepath.Join(l.Dir, "plain")
	must(t, os.MkdirAll(plain, 0o755))
	twin := filepath.Join(l.Dir, "src", "backend-copy")
	l.Git(l.Dir, "clone", "-q", filepath.Join(l.Dir, "remotes", "backend.git"), twin)
	before := len(l.State().Sessions)
	here := []string{"TMUX_PANE=" + l.HerePane()}
	for name, args := range map[string][]string{
		"not a repo":      {"run", "fake", "--repos", backend + "," + plain},
		"same name twice": {"run", "fake", "--repos", backend + "," + twin},
		"with -w":         {"run", "fake", "-w", "--repos", backend},
		"with fork":       {"run", "--fork", "abc", "--repos", backend},
	} {
		if r := l.AisIn(backend, here, "", args...); r.Code == 0 {
			t.Errorf("%s: succeeded", name)
		}
	}
	if n := len(l.State().Sessions); n != before {
		t.Errorf("refusals recorded %d session(s)", n-before)
	}
}
