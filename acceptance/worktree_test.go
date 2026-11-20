package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -w puts the session in <base>/<name>/<repo> on branch <name>, branched from
// the remote default, and tells the harness where everything is.
func TestWorktreeRun(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("backend")
	s := l.Start(repo, "run", "fake", "-w", "-d", "Port the parser")

	want := filepath.Join(l.WtBase, "port-parser", "backend")
	got := l.Session(s.ID)
	if got.Cwd != want {
		t.Fatalf("cwd = %q, want %q", got.Cwd, want)
	}
	if b := l.Git(want, "branch", "--show-current"); b != "port-parser" {
		t.Errorf("branch = %q", b)
	}
	if head, origin := l.Git(want, "rev-parse", "HEAD"), l.Git(repo, "rev-parse", "origin/main"); head != origin {
		t.Errorf("worktree starts at %s, want origin/main %s", head, origin)
	}
	wt := got.Worktree
	if wt["base"] != filepath.Join(l.WtBase, "port-parser") || wt["initial"] != want ||
		wt["branch"] != "port-parser" || wt["root"] != repo {
		t.Errorf("worktree record = %v", wt)
	}
	f := l.Fake(s.ID)
	if f.Cwd != want {
		t.Errorf("harness cwd = %q", f.Cwd)
	}
	for k, v := range map[string]string{
		"AIS_WORKTREE": want, "AIS_WORKTREE_BASE": filepath.Join(l.WtBase, "port-parser"), "AIS_REPO_MAIN": repo,
	} {
		if f.Env[k] != v {
			t.Errorf("harness env %s = %q, want %q", k, f.Env[k], v)
		}
	}
	if v := l.Format(s.Pane, "#{@cwd}"); v != want {
		t.Errorf("@cwd = %q", v)
	}
	l.Stop(s, 0)
}

// worktree.from decides where a new branch starts; auto prefers local when it
// is ahead of origin.
func TestWorktreeFromModes(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("api")
	origin := l.Git(repo, "rev-parse", "origin/main")
	l.Commit(repo, "local.txt", "local only") // main is now ahead of origin/main
	local := l.Git(repo, "rev-parse", "main")
	l.Git(repo, "checkout", "-q", "-b", "topic")
	l.Commit(repo, "topic.txt", "on topic")
	head := l.Git(repo, "rev-parse", "HEAD")

	cases := []struct{ from, want string }{
		{"auto", local}, {"origin", origin}, {"local", local}, {"head", head},
	}
	for _, c := range cases {
		s := l.Start(repo, "run", "fake", "-w", "--from", c.from, "-d", "from "+c.from)
		if got := l.Git(l.Session(s.ID).Cwd, "rev-parse", "HEAD"); got != c.want {
			t.Errorf("--from %s starts at %s, want %s", c.from, got, c.want)
		}
		l.Stop(s, 0)
	}
}

func TestWorktreeDetach(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("detached")
	s := l.Start(repo, "run", "fake", "-w", "--detach", "-d", "look around")
	cwd := l.Session(s.ID).Cwd
	if b := l.Git(cwd, "branch", "--show-current"); b != "" {
		t.Errorf("detached worktree is on branch %q", b)
	}
	l.Stop(s, 0)
}

// An existing branch of the session's name is reused, not recreated.
func TestWorktreeReusesBranch(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("reuse")
	l.Git(repo, "branch", "keep-going")
	l.Commit(repo, "later.txt", "later") // main moves on; keep-going does not
	branchTip := l.Git(repo, "rev-parse", "keep-going")
	s := l.Start(repo, "run", "fake", "-w", "-d", "keep going")
	if got := l.Git(l.Session(s.ID).Cwd, "rev-parse", "HEAD"); got != branchTip {
		t.Errorf("worktree at %s, want the existing branch tip %s", got, branchTip)
	}
	l.Stop(s, 0)
}

// worktree.copy carries untracked files across; AIS_WT_BASE moves the base.
func TestWorktreeCopyAndBaseOverride(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("svc")
	l.WriteFile(filepath.Join(repo, ".env"), "SECRET=1\n")
	l.SetConfig(strings.Replace(l.BaseConfig(), "[worktree]\n", "[worktree]\ncopy = [\".env\"]\n", 1))
	other := filepath.Join(l.Dir, "otherbase")
	pane, _ := l.Window(repo, "AIS_WT_BASE="+other+" "+l.AisCommand("run", "fake", "-w", "-d", "moved base"))
	var cwd, id string
	l.Eventually("session up", func() bool {
		for _, s := range l.State().Sessions {
			if s.Tmux.PaneID == pane {
				id = s.ID
			}
		}
		return id != "" && fileExists(filepath.Join(l.Log, id, "ready"))
	})
	cwd = l.Session(id).Cwd
	if want := filepath.Join(other, "moved-base", "svc"); cwd != want {
		t.Errorf("cwd = %q, want %q", cwd, want)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, ".env")); err != nil || string(b) != "SECRET=1\n" {
		t.Errorf(".env not copied: %v %q", err, b)
	}
}

// worktree.auto: a worktree inside a repo, in place outside one, and
// --no-worktree opts out. A typed -w outside a repo is an error.
func TestWorktreeAuto(t *testing.T) {
	l := NewLab(t)
	l.SetConfig(strings.Replace(l.BaseConfig(), "[worktree]\n", "[worktree]\nauto = true\n", 1))
	repo := l.Repo("web")
	plain := filepath.Join(l.Dir, "notarepo")
	l.WriteFile(filepath.Join(plain, ".keep"), "")

	s := l.Start(repo, "run", "fake", "-d", "auto one")
	if cwd := l.Session(s.ID).Cwd; cwd != filepath.Join(l.WtBase, "auto-one", "web") {
		t.Errorf("auto in a repo: cwd = %q", cwd)
	}
	l.Stop(s, 0)

	s = l.Start(plain, "run", "fake", "-d", "auto two")
	if cwd := l.Session(s.ID).Cwd; cwd != plain {
		t.Errorf("auto outside a repo: cwd = %q", cwd)
	}
	l.Stop(s, 0)

	s = l.Start(repo, "run", "fake", "--no-worktree", "-d", "auto three")
	if cwd := l.Session(s.ID).Cwd; cwd != repo {
		t.Errorf("--no-worktree: cwd = %q", cwd)
	}
	l.Stop(s, 0)

	before := len(l.State().Sessions)
	r := l.AisIn(plain, []string{"TMUX_PANE=" + l.HerePane()}, "", "run", "fake", "-w")
	if r.Code == 0 {
		t.Errorf("-w outside a repo succeeded")
	}
	if n := len(l.State().Sessions); n != before {
		t.Errorf("a refused -w recorded a session")
	}
}

// ais wt: a session takes a second repo, finds its worktrees again, and the
// repo directory is named after the origin URL, not the checkout.
func TestWtCommands(t *testing.T) {
	l := NewLab(t)
	backend := l.Repo("backend")
	iac := l.Repo("iac")
	renamed := filepath.Join(l.Dir, "src", "iac-checkout-2")
	must(t, os.Rename(iac, renamed))

	s := l.Start(backend, "run", "fake", "-w", "-d", "Cross repo feature")
	in := []string{"AIS_SESSION=" + s.ID}
	base := filepath.Join(l.WtBase, "cross-repo-feature")

	r := l.MustAis(in, "wt", "add", renamed)
	if strings.TrimSpace(r.Stdout) != filepath.Join(base, "iac") {
		t.Errorf("wt add printed %q, want only the path", r.Stdout)
	}
	if b := l.Git(filepath.Join(base, "iac"), "branch", "--show-current"); b != "cross-repo-feature" {
		t.Errorf("second worktree on branch %q", b)
	}
	// Adding it again is a no-op that prints the same path.
	if r := l.MustAis(in, "wt", "add", renamed); strings.TrimSpace(r.Stdout) != filepath.Join(base, "iac") {
		t.Errorf("second wt add printed %q", r.Stdout)
	}

	ls := l.MustAis(in, "wt", "ls").Stdout
	for _, want := range []string{"cross-repo-feature/backend", "cross-repo-feature/iac"} {
		if !strings.Contains(ls, want) {
			t.Errorf("wt ls lacks %s:\n%s", want, ls)
		}
	}
	if !strings.Contains(l.MustAis(nil, "wt", "ls", "-a").Stdout, "cross-repo-feature/iac") {
		t.Errorf("wt ls -a lacks the session's worktrees")
	}
	if p := strings.TrimSpace(l.MustAis(in, "wt", "path", "iac").Stdout); p != filepath.Join(base, "iac") {
		t.Errorf("wt path iac = %q", p)
	}
	if p := strings.TrimSpace(l.MustAis(in, "wt", "path").Stdout); p != filepath.Join(base, "backend") {
		t.Errorf("wt path (initial) = %q", p)
	}
	if r := l.Ais(in, "wt", "path", "nope"); r.Code == 0 {
		t.Errorf("wt path of a repo the session lacks succeeded")
	}
	if r := l.Ais(nil, "wt", "add", renamed); r.Code == 0 {
		t.Errorf("wt add outside a session succeeded")
	}
	if r := l.Ais(in, "wt", "bogus"); r.Code == 0 {
		t.Errorf("wt bogus succeeded")
	}
	l.Stop(s, 0)

	// prune removes a session directory once it is empty. (It is also meant to
	// drop git's registration of worktrees deleted by hand, but finds repos only
	// through worktrees that still exist: see README.md, known issues.)
	must(t, os.RemoveAll(filepath.Join(base, "iac")))
	must(t, os.RemoveAll(filepath.Join(base, "backend")))
	l.MustAis(nil, "wt", "prune")
	if fileExists(base) {
		t.Errorf("empty session directory survived wt prune")
	}
}
