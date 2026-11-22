package acceptance

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitLab is refineLab with two tickets to refine, one after the other, and a
// planner played by fake that waits for refinement as given.
func waitLab(t *testing.T, wait string) (l *Lab, first, second string) {
	l = refineLab(t, 3)
	cfg := l.ReadFile(filepath.Join(l.AisHome, "config.toml"))[len(l.BaseConfig()):]
	l.WriteConfig(cfg + "\n[workspace.soi.planner]\nprofile = \"fake\"\nmin_interval = \"0s\"\nrefine_wait = \"" + wait + "\"\n")
	l.Close(ghURL("gpwsoi/iac", 4))
	l.MustFlow("dispatch")
	for _, p := range l.Proposals() {
		if p.State != "queued" {
			first = p.Tickets[0]
		} else {
			second = p.Tickets[0]
		}
	}
	if first == "" || second == "" {
		t.Fatalf("want one refinement started and one queued: %+v", l.Proposals())
	}
	return l, first, second
}

// endRefine has the going refinement run of url record findings and exit, or
// just exit with none.
func (l *Lab) endRefine(url string, findings string, args ...string) {
	l.t.Helper()
	run := l.refineRun(url)
	sid := l.waitRunning(run.ID)
	if findings != "" {
		l.MustFlowIn(findings, append([]string{"ticket", args[0], url, "--file", "-"}, args[1:]...)...)
	}
	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "0")
	l.Eventually("refinement of "+url+" is done", func() bool { return l.proposalState(run.ID) == "done" })
}

// settled lets a finished run get past its last step, starting a planner.
func (l *Lab) settled() {
	l.t.Helper()
	l.Eventually("no refinement process left", func() bool { return len(l.Runs("--active", "--kind", "headless")) == 0 })
	time.Sleep(300 * time.Millisecond)
}

func (l *Lab) planners() int {
	l.t.Helper()
	n := 0
	for _, s := range l.State().Sessions {
		if s.FlowPlanner != "" {
			n++
		}
	}
	return n
}

// waitsAfterFirst ends the first run with an L3, which makes it plannable,
// and checks that no planner opens while the second is still to come.
func (l *Lab) waitsAfterFirst(first string) {
	l.t.Helper()
	l.endRefine(first, l3Findings, "assess", "--level", "L3")
	l.settled()
	if n := l.planners(); n != 0 || !strings.Contains(l.MustFlow("log").Stdout, "planner-waits") {
		l.t.Fatalf("%d planner(s) opened with a refinement still queued; log:\n%s", n, l.MustFlow("log").Stdout)
	}
	if r := l.MustFlow("plan", "soi", "--auto"); strings.TrimSpace(r.Stdout) != "" || !strings.Contains(r.Stderr, "refinement") {
		l.t.Errorf("the timer's check while refinement is going:\n%s", r)
	}
}

// The planner opens when the last refinement ends, even one that sent its
// ticket back and so lets nothing through itself.
func TestFlowPlannerWaitsForRefinement(t *testing.T) {
	l, first, second := waitLab(t, "1h")
	l.waitsAfterFirst(first)
	l.MustFlow("dispatch")
	if n := l.planners(); n != 0 {
		t.Fatalf("%d planner(s) opened by dispatch with a refinement going", n)
	}
	l.endRefine(second, l1Findings, "refine")
	l.Eventually("a planner opens", func() bool { return l.planners() == 1 })
}

// ...and when the last one fails.
func TestFlowPlannerWaitEndsOnFailedRefinement(t *testing.T) {
	l, first, second := waitLab(t, "1h")
	l.waitsAfterFirst(first)
	l.MustFlow("dispatch")
	l.endRefine(second, "")
	l.Eventually("a planner opens", func() bool { return l.planners() == 1 })
}

// A run that never ends holds planning back for refine_wait at most.
func TestFlowPlannerWaitIsCapped(t *testing.T) {
	l, first, second := waitLab(t, "8s")
	l.waitsAfterFirst(first)
	l.MustFlow("dispatch")
	l.waitRunning(l.refineRun(second).ID)
	time.Sleep(9 * time.Second)
	l.MustFlow("dispatch")
	l.Eventually("a planner opens past the cap", func() bool { return l.planners() == 1 })
	if l.proposalState(l.refineRun(second).ID) != "running" {
		t.Errorf("the second refinement is not running any more")
	}
}

// By hand, a planner starts at once; with refine_wait 0 it always does.
func TestFlowPlannerNoWait(t *testing.T) {
	l, first, _ := waitLab(t, "1h")
	l.waitsAfterFirst(first)
	l.MustFlow("plan", "soi")
	l.Eventually("a planner opens by hand", func() bool { return l.planners() == 1 })

	l, first, _ = waitLab(t, "0s")
	l.endRefine(first, l3Findings, "assess", "--level", "L3")
	l.Eventually("a planner opens without waiting", func() bool { return l.planners() == 1 })
}
