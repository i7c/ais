package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SessionStart hook (_session-seen) records the conversation the harness
// is in now — but only when the harness ais started is the one reporting.
func TestSessionSeen(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "--", "--report", "conv-after-clear")
	if c := deref(l.Session(s.ID).Conversation); c != "conv-after-clear" {
		t.Errorf("conversation = %q, want the reported one", c)
	}
	l.Stop(s, 0)

	// A harness nested inside the session must not overwrite it.
	s = l.Start(l.Home, "run", "fake", "--", "--nested-report", "nested-conv")
	minted := deref(l.Session(s.ID).Conversation)
	if minted == "nested-conv" || !uuidRe.MatchString(minted) {
		t.Errorf("nested harness changed the conversation to %q", minted)
	}

	// Neither may a shell beside it, nor anything that is not under the
	// harness at all; and none of them fails, whatever it is fed.
	for name, env := range map[string][]string{
		"sibling":   {"AIS_SESSION=" + s.ID, "AIS_SIBLING=1"},
		"unrelated": {"AIS_SESSION=" + s.ID},
		"unknown":   {"AIS_SESSION=zzzzzzzzzz"},
	} {
		r := l.AisIn(l.Home, env, `{"session_id":"intruder"}`, "_session-seen")
		if r.Code != 0 {
			t.Errorf("%s: _session-seen exited %d", name, r.Code)
		}
	}
	if r := l.AisIn(l.Home, []string{"AIS_SESSION=" + s.ID}, "not json", "_session-seen"); r.Code != 0 {
		t.Errorf("_session-seen on garbage exited %d", r.Code)
	}
	if c := deref(l.Session(s.ID).Conversation); c != minted {
		t.Errorf("conversation changed to %q by someone other than the harness", c)
	}
	l.Stop(s, 0)
}

// restart: a new session in this pane, same directory, name and worktree,
// resuming the conversation; --fresh starts a new one.
func TestRestart(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("svc")
	old := l.Start(repo, "run", "fake", "-w", "-d", "Long running work", "--", "--model", "opus")
	l.Stop(old, 1)
	before := l.Session(old.ID)
	conv := deref(before.Conversation)

	// -n says what it would run and records nothing.
	dry := l.AisIn(before.Cwd, []string{"TMUX_PANE=" + l.HerePane()}, "", "restart", old.ID, "-n")
	if dry.Code != 0 || !strings.Contains(dry.Stdout, "--resume") || !strings.Contains(dry.Stdout, conv) {
		t.Errorf("restart -n:\n%s", dry)
	}
	if n := len(l.State().Sessions); n != 1 {
		t.Errorf("restart -n recorded a session")
	}

	// No id: the last session that died where you stand.
	re := l.Start(before.Cwd, "restart")
	got := l.Session(re.ID)
	if re.ID == old.ID || deref(got.RestartOf) != old.ID {
		t.Errorf("restart id %s restart_of %v", re.ID, got.RestartOf)
	}
	if got.Cwd != before.Cwd || got.Name != before.Name || got.Worktree["base"] != before.Worktree["base"] {
		t.Errorf("restart moved: cwd %s name %s wt %v", got.Cwd, got.Name, got.Worktree)
	}
	if deref(got.Conversation) != conv {
		t.Errorf("restart conversation %q, want %q", deref(got.Conversation), conv)
	}
	want := "--from-config --resume " + conv + " --model opus"
	if argv := strings.Join(l.Fake(re.ID).Argv, " "); argv != want {
		t.Errorf("restart argv = %q, want %q", argv, want)
	}
	if r := l.AisIn(before.Cwd, []string{"TMUX_PANE=" + l.HerePane()}, "", "restart", re.ID); r.Code == 0 {
		t.Errorf("restarting a running session succeeded")
	}
	l.Stop(re, 0)

	fresh := l.Start(before.Cwd, "restart", re.ID, "--fresh")
	fc := deref(l.Session(fresh.ID).Conversation)
	if fc == conv || !uuidRe.MatchString(fc) {
		t.Errorf("--fresh conversation = %q", fc)
	}
	if argv := l.Fake(fresh.ID).Argv; indexOf(argv, "--session-id") != 1 || argv[2] != fc {
		t.Errorf("--fresh argv = %q", argv)
	}
	l.Stop(fresh, 0)
}

func TestRestartRefusals(t *testing.T) {
	l := NewLab(t)
	dir := filepath.Join(l.Dir, "gone-soon")
	l.WriteFile(filepath.Join(dir, ".keep"), "")
	s := l.Start(dir, "run", "fake")
	l.Stop(s, 0)
	must(t, os.RemoveAll(dir))
	here := []string{"TMUX_PANE=" + l.HerePane()}
	if r := l.AisIn(l.Home, here, "", "restart", s.ID); r.Code == 0 {
		t.Errorf("restart into a deleted directory succeeded")
	}
	if l.Session(s.ID).CanRestart {
		t.Errorf("can_restart true for a session whose directory is gone")
	}
	if r := l.AisIn(l.Home, here, "", "restart"); r.Code == 0 {
		t.Errorf("restart with nothing dead here succeeded")
	}
	if r := l.Ais(nil, "restart", s.ID); r.Code == 0 {
		t.Errorf("restart outside tmux succeeded")
	}
}

// fork: a second session beside a live one, same place and name, with the
// conversation branched through session.fork.
func TestFork(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("forkme")
	src := l.Start(repo, "run", "fake", "-w", "-d", "Fork source", "--", "--model", "opus")
	srcRec := l.Session(src.ID)
	conv := deref(srcRec.Conversation)

	fork := l.Start(srcRec.Cwd, "run", "--fork", src.ID)
	got := l.Session(fork.ID)
	if deref(got.ForkOf) != src.ID || got.Cwd != srcRec.Cwd || got.Name != srcRec.Name || got.Description != "Fork source" {
		t.Errorf("fork = %+v", got)
	}
	want := "--from-config --resume " + conv + " --fork-session --model opus"
	if argv := strings.Join(l.Fake(fork.ID).Argv, " "); argv != want {
		t.Errorf("fork argv = %q, want %q", argv, want)
	}
	if got.Conversation != nil {
		t.Errorf("fork records conversation %q before its harness reports one", *got.Conversation)
	}
	if l.Session(src.ID).Status != "active" {
		t.Errorf("forking stopped the source")
	}
	l.Stop(fork, 0)

	here := []string{"TMUX_PANE=" + l.HerePane()}
	if r := l.AisIn(l.Home, here, "", "run", "plain", "--fork", src.ID); r.Code == 0 {
		t.Errorf("fork naming another profile succeeded")
	}
	plain := l.Start(l.Home, "run", "plain")
	if r := l.AisIn(l.Home, here, "", "run", "--fork", plain.ID); r.Code == 0 {
		t.Errorf("fork of a profile without session.fork succeeded")
	}
	l.Stop(plain, 0)
	l.Stop(src, 0)
}

// ais name: derived from the description once, then stable; --set overrides;
// --of predicts; two sessions with one description get different names.
func TestName(t *testing.T) {
	l := NewLab(t)
	for desc, want := range map[string]string{
		"Fix the auth bug": "fix-auth-bug",
		"Refactor database connection pooling layer quickly": "refactor-database-connection",
		"Café über naïve": "cafe-uber-naive",
		"Supercalifragilistic expialidocious extraordinarily big": "supercalifragilistic",
	} {
		if got := strings.TrimSpace(l.MustAis(nil, "name", "--of", desc).Stdout); got != want {
			t.Errorf("name --of %q = %q, want %q", desc, got, want)
		}
	}
	if r := l.Ais(nil, "name", "--of", "!!!"); r.Code == 0 {
		t.Errorf("name --of with no usable characters succeeded")
	}
	if r := l.Ais(nil, "name"); r.Code == 0 {
		t.Errorf("name outside a session succeeded")
	}

	a := l.Start(l.Home, "run", "fake", "-d", "Same words here")
	b := l.Start(l.Home, "run", "fake", "-d", "Same words here")
	na := strings.TrimSpace(l.MustAis([]string{"AIS_SESSION=" + a.ID}, "name").Stdout)
	nb := strings.TrimSpace(l.MustAis([]string{"AIS_SESSION=" + b.ID}, "name").Stdout)
	if na != "same-words-here" || nb != "same-words-here-"+b.ID[:4] {
		t.Errorf("names = %q, %q", na, nb)
	}
	set := strings.TrimSpace(l.MustAis([]string{"AIS_SESSION=" + a.ID}, "name", "--set", "Brand New Name").Stdout)
	if set != "brand-new-name" || l.Session(a.ID).Name != "brand-new-name" {
		t.Errorf("--set printed %q, recorded %q", set, l.Session(a.ID).Name)
	}
	if again := strings.TrimSpace(l.MustAis([]string{"AIS_SESSION=" + a.ID}, "name").Stdout); again != "brand-new-name" {
		t.Errorf("name after --set = %q", again)
	}
	l.Stop(a, 0)
	l.Stop(b, 0)
}

func TestCurrent(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "-d", "Who am I")
	r := l.MustAis([]string{"AIS_SESSION=" + s.ID}, "current")
	if !strings.Contains(r.Stdout, s.ID) || !strings.Contains(r.Stdout, "Who am I") {
		t.Errorf("current:\n%s", r.Stdout)
	}
	if r := l.Ais(nil, "current"); r.Code == 0 {
		t.Errorf("current outside a session succeeded")
	}
	l.Stop(s, 0)
}

// select <id> moves the client to the session's pane.
func TestSelect(t *testing.T) {
	l := NewLab(t)
	l.Attach()
	if r := l.Ais(nil, "select", "x"); r.Code == 0 {
		t.Errorf("select with no sessions succeeded")
	}
	s := l.Start(l.Home, "run", "fake")
	if got := strings.TrimSpace(l.tmuxMust("list-clients", "-F", "#{pane_id}")); got == s.Pane {
		t.Fatalf("client already on the session's pane; the test proves nothing")
	}
	l.MustAis(nil, "select", s.ID[:5])
	if got := strings.TrimSpace(l.tmuxMust("list-clients", "-F", "#{pane_id}")); got != s.Pane {
		t.Errorf("client shows %s, want %s", got, s.Pane)
	}
	if r := l.Ais(nil, "select", "zzzz"); r.Code == 0 {
		t.Errorf("select of an unknown id succeeded")
	}
	l.Stop(s, 0)
}

// prune drops dead sessions and their finished worktrees, keeps unsaved work.
func TestPrune(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("prunable")
	clean := l.Start(repo, "run", "fake", "-w", "-d", "clean work")
	dirty := l.Start(repo, "run", "fake", "-w", "-d", "dirty work")
	alive := l.Start(repo, "run", "fake", "-w", "-d", "still running")
	cleanDir, dirtyDir, aliveDir := l.Session(clean.ID).Cwd, l.Session(dirty.ID).Cwd, l.Session(alive.ID).Cwd
	l.WriteFile(filepath.Join(dirtyDir, "wip.txt"), "unsaved\n")
	l.Stop(clean, 0)
	l.Stop(dirty, 0)

	l.MustAis(nil, "prune") // default: only dead sessions older than 7 days
	if n := len(l.State().Sessions); n != 3 {
		t.Errorf("prune without --all dropped recent sessions (%d left)", n)
	}

	l.MustAis(nil, "prune", "--all")
	left := ids(l.State().Sessions)
	if len(left) != 1 || left[0] != alive.ID {
		t.Errorf("after prune --all: %v", left)
	}
	if fileExists(cleanDir) {
		t.Errorf("clean worktree of a pruned session survived")
	}
	if strings.Contains(l.Git(repo, "branch", "--list", "clean-work"), "clean-work") {
		t.Errorf("merged branch of a pruned worktree survived")
	}
	if !fileExists(filepath.Join(dirtyDir, "wip.txt")) {
		t.Errorf("prune threw away uncommitted work")
	}
	if !fileExists(aliveDir) {
		t.Errorf("prune touched a running session's worktree")
	}
	l.Stop(alive, 0)
}

func TestPruneKeepWorktrees(t *testing.T) {
	l := NewLab(t)
	repo := l.Repo("keeper")
	s := l.Start(repo, "run", "fake", "-w", "-d", "keep it")
	dir := l.Session(s.ID).Cwd
	l.Stop(s, 0)
	l.MustAis(nil, "prune", "--all", "--keep-worktrees")
	if len(l.State().Sessions) != 0 || !fileExists(dir) {
		t.Errorf("--keep-worktrees: sessions %d, dir kept %v", len(l.State().Sessions), fileExists(dir))
	}
}
