package acceptance

import (
	"path/filepath"
	"strings"
	"testing"
)

// waitRunning waits until a proposal's session is up and returns its id.
func (l *Lab) waitRunning(pid string) string {
	l.t.Helper()
	var sid string
	l.Eventually(pid+" running", func() bool {
		for _, q := range l.Queue("--all") {
			if q.ID == pid && q.State == "running" {
				for _, s := range l.State().Sessions {
					if s.FlowProposal == pid && s.Status == "active" {
						sid = s.ID
					}
				}
			}
		}
		return sid != "" && fileExists(filepath.Join(l.Log, sid, "ready"))
	})
	return sid
}

func (l *Lab) proposalState(pid string) string {
	for _, q := range l.Queue("--all") {
		if q.ID == pid {
			return q.State
		}
	}
	return ""
}

func (l *Lab) queueAdd(args ...string) string {
	l.t.Helper()
	return strings.TrimSpace(l.MustFlow(append([]string{"queue", "add"}, args...)...).Stdout)
}

// The dispatcher starts the highest-priority proposal whose turn it is, in
// its workspace's tmux session, with its prompt and a worktree of each repo;
// "after" and the limits hold the rest back; done frees a slot.
func TestFlowDispatch(t *testing.T) {
	l := flowLab(t)
	p1 := l.queueAdd("--title", "First across two repos", "--tickets", "gpwsoi/backend#12", "--repos", "backend,iac", "--priority", "1")
	p2 := l.queueAdd("--title", "Second after first", "--tickets", "gpwsoi/backend#13", "--repos", "backend", "--priority", "1", "--after", p1)
	p3 := l.queueAdd("--title", "Third alone", "--tickets", "gpwsoi/iac#4", "--repos", "iac")

	if out := l.MustFlow("dispatch", "--dry-run"); !strings.Contains(out.Stdout, "would start "+p1) || l.proposalState(p1) != "queued" {
		t.Errorf("dry run:\n%s (state %s)", out, l.proposalState(p1))
	}

	if out := strings.TrimSpace(l.MustFlow("dispatch").Stdout); !strings.Contains(out, p1) || !strings.Contains(out, p3) || strings.Contains(out, p2) {
		t.Errorf("dispatch started %q; want %s and %s, not %s (it waits for %s)", out, p1, p3, p2, p1)
	}
	s1 := l.waitRunning(p1)
	l.waitRunning(p3)

	sess := l.Session(s1)
	base := filepath.Join(l.WtBase, "first-across-two-repos")
	if sess.Cwd != base || sess.Description != "First across two repos" || sess.Profile != "fake" || sess.Tmux.SessionName != "soi" {
		t.Errorf("session = %+v", sess)
	}
	for _, repo := range []string{"backend", "iac"} {
		if b := l.Git(filepath.Join(base, repo), "branch", "--show-current"); b != "first-across-two-repos" {
			t.Errorf("%s worktree on %q", repo, b)
		}
	}
	if p := l.Fake(s1).Prompt(); !strings.HasPrefix(p, "# Work on gpwsoi/backend#12") ||
		!strings.Contains(p, "- https://github.com/gpwsoi/backend/issues/12") || !strings.Contains(p, "ais flow ticket show") {
		t.Errorf("prompt = %q", p)
	}
	if sess.LastInputAt != nil {
		t.Errorf("the dispatched prompt counted as your input")
	}
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 12)); s != "In Progress" {
		t.Errorf("#12 is %q, want In Progress", s)
	}
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 13)); s != "scheduled" {
		t.Errorf("#13 is %q, want still scheduled", s)
	}
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, "running 2/4") || !strings.Contains(ls, p1) || !strings.Contains(ls, s1) {
		t.Errorf("ls:\n%s", ls)
	}

	// wip 2 is full: nothing more starts, and the reason is said.
	l.MustFlow("limit", "wip", "2")
	if out := l.MustFlow("dispatch"); strings.TrimSpace(out.Stdout) != "" || !strings.Contains(out.Stderr, "wip") {
		t.Errorf("dispatch at wip:\n%s", out)
	}

	// done, from inside the session: p1 is finished, its slot is free, and
	// p2's turn has come.
	l.MustAis([]string{"AIS_SESSION=" + s1}, "flow", "done")
	if st := l.proposalState(p1); st != "done" {
		t.Fatalf("p1 is %s after done", st)
	}
	if out := strings.TrimSpace(l.MustFlow("dispatch").Stdout); out != p2 {
		t.Errorf("after p1 done, dispatch started %q, want %s", out, p2)
	}
	l.waitRunning(p2)
	if r := l.Flow("done", p1); r.Code == 0 {
		t.Errorf("done twice succeeded")
	}
}

// --now starts one past the limits; a ticket in "after" holds a proposal
// until it closes.
func TestFlowDispatchNowAndTicketAfter(t *testing.T) {
	l := flowLab(t)
	l.MustFlow("limit", "per_hour", "0")
	p := l.queueAdd("--title", "Urgent", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	if out := l.MustFlow("dispatch"); strings.TrimSpace(out.Stdout) != "" || !strings.Contains(out.Stderr, "per_hour") {
		t.Errorf("dispatch at per_hour 0:\n%s", out)
	}
	l.MustFlow("dispatch", "--now", p[:4])
	l.waitRunning(p)
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, "started in the last hour 1/0") {
		t.Errorf("a manual start was not counted:\n%s", ls)
	}
	if r := l.Flow("dispatch", "--now", p); r.Code == 0 {
		t.Errorf("--now of a running proposal succeeded")
	}

	l.MustFlow("limit", "--clear")
	waiting := l.queueAdd("--title", "After a ticket", "--tickets", "gpwsoi/iac#4", "--repos", "iac", "--after", "gpwsoi/iac#5")
	if out := l.MustFlow("dispatch"); strings.TrimSpace(out.Stdout) != "" || !strings.Contains(out.Stderr, "gpwsoi/iac#5") {
		t.Errorf("dispatch with an open after-ticket:\n%s", out)
	}
	l.WriteFile(filepath.Join(l.Log, "gh", "closed"), ghURL("gpwsoi/iac", 5)+"\n")
	if out := strings.TrimSpace(l.MustFlow("dispatch").Stdout); out != waiting {
		t.Errorf("after the ticket closed, dispatch started %q", out)
	}
	l.waitRunning(waiting)
}

// A dispatched session that is restarted keeps its proposal, and done from
// inside the restart finds it.
func TestFlowDoneAfterRestart(t *testing.T) {
	l := flowLab(t)
	p := l.queueAdd("--title", "Survives a restart", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	l.MustFlow("dispatch")
	sid := l.waitRunning(p)
	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "0")
	l.Eventually("session ends", func() bool { return l.Session(sid).Status == "dead" })
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, "ended; ais flow done") {
		t.Errorf("ls does not flag a session that ended without done:\n%s", ls)
	}
	re := l.Start(l.Session(sid).Cwd, "restart", sid)
	if fp := l.Session(re.ID).FlowProposal; fp != p {
		t.Errorf("restart has flow_proposal %q, want %s", fp, p)
	}
	l.MustAis([]string{"AIS_SESSION=" + re.ID}, "flow", "done")
	if st := l.proposalState(p); st != "done" {
		t.Errorf("proposal is %s after done from the restart", st)
	}
	l.Stop(re, 0)
}

func TestFlowInstall(t *testing.T) {
	l := flowLab(t)
	dir := filepath.Join(l.Home, ".config", "systemd", "user")
	if out := l.MustFlow("install", "-n").Stdout; !strings.Contains(out, "would write") || fileExists(filepath.Join(dir, "ais-flow-dispatch.timer")) {
		t.Errorf("install -n:\n%s", out)
	}
	// --no-enable: the suite must never reach the real systemd user manager.
	l.MustFlow("install", "--no-enable")
	svc := l.ReadFile(filepath.Join(dir, "ais-flow-dispatch.service"))
	if !strings.Contains(svc, "ExecStart="+l.ais+" flow dispatch") && !strings.Contains(svc, " flow dispatch") {
		t.Errorf("service:\n%s", svc)
	}
	if tm := l.ReadFile(filepath.Join(dir, "ais-flow-dispatch.timer")); !strings.Contains(tm, "OnCalendar=*:0/5") {
		t.Errorf("timer:\n%s", tm)
	}
	if pl := l.ReadFile(filepath.Join(dir, "ais-flow-plan-soi.timer")); !strings.Contains(pl, "Unit=ais-flow-plan@soi.service") || !strings.Contains(pl, "OnCalendar=Mon..Fri *-*-* 08..20:00:00") {
		t.Errorf("planner timer:\n%s", pl)
	}
	if out := l.MustFlow("install", "--no-enable").Stdout; strings.Count(out, "unchanged") != 4 {
		t.Errorf("second install:\n%s", out)
	}
	l.WriteFile(filepath.Join(dir, "ais-flow-dispatch.timer"), "[Timer]\nOnCalendar=hourly\n")
	if out := l.MustFlow("install", "--no-enable").Stdout; !strings.Contains(out, "left alone") {
		t.Errorf("install overwrote a timer it did not write:\n%s", out)
	}
}
