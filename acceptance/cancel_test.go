package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deferred ticket stays open, goes to the backlog status, and carries the
// comment; a closed one cannot be deferred.
func TestFlowTicketDefer(t *testing.T) {
	l := flowLab(t)
	url := ghURL("gpwsoi/backend", 12)
	l.MustFlow("ticket", "defer", "gpwsoi/backend#12", "--comment", "not now")
	if s := l.BoardStatus(url); s != "Todo" {
		t.Errorf("deferred ticket is %q, want Todo", s)
	}
	if c := l.Comments(); len(c) != 1 || c[0]["url"] != url || c[0]["body"] != "not now" {
		t.Errorf("comments = %v", c)
	}
	if closes := filepath.Join(l.Log, "gh", "closes.json"); fileExists(closes) {
		t.Errorf("defer closed the ticket: %s", l.ReadFile(closes))
	}

	l.WriteFile(filepath.Join(l.Log, "gh", "closed"), ghURL("gpwsoi/backend", 13)+"\n")
	if r := l.Flow("ticket", "defer", "gpwsoi/backend#13"); r.Code == 0 {
		t.Errorf("deferring a closed ticket: %s", r)
	}
}

// Cancelling a running session frees it as cancelled, not done; its ticket
// goes to the backlog, its plan-only PR is closed and its branch cleared, and
// once the ticket is ready again a planner can take it.
func TestFlowCancel(t *testing.T) {
	l := flowLab(t)
	url := ghURL("gpwsoi/backend", 12)
	pid := l.queueAdd("--title", "Has a plan", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	if r := l.Flow("cancel", pid); r.Code == 0 || !strings.Contains(r.Stderr, "queue cancel") {
		t.Errorf("cancel of a queued proposal: %s", r)
	}
	l.MustFlow("dispatch")
	sid := l.waitRunning(pid)
	wt := filepath.Join(l.Session(sid).Cwd, "backend")
	plan := filepath.Join(l.Dir, "plan.md")
	l.WriteFile(plan, "# Plan: x\n")
	if r := l.AisIn(wt, []string{"AIS_FLOW_PROPOSAL=" + pid}, "", "flow", "review", "open", "--ticket", "gpwsoi/backend#12", "--file", plan); r.Code != 0 {
		t.Fatalf("open:\n%s", r)
	}
	branch := l.Git(wt, "branch", "--show-current")
	if s := l.BoardStatus(url); s != "In Progress" {
		t.Fatalf("running ticket is %q", s)
	}

	if r := l.Flow("cancel", pid, "--reason", "later"); r.Code != 0 {
		t.Fatalf("cancel:\n%s", r)
	}
	if p := l.Proposals(); p[0].State != "cancelled" {
		t.Errorf("proposal after cancel: %+v", p[0])
	}
	if s := l.BoardStatus(url); s != "Todo" {
		t.Errorf("ticket after cancel is %q, want Todo", s)
	}
	var prs map[string]map[string]any
	must(t, json.Unmarshal([]byte(l.ReadFile(filepath.Join(l.Log, "gh", "prs.json"))), &prs))
	if pr := prs[branch]; pr["state"] != "CLOSED" || !strings.Contains(pr["closeComment"].(string), "later") {
		t.Errorf("plan PR after cancel: %v", pr)
	}
	if remote := l.Git(wt, "ls-remote", "origin", branch); remote != "" {
		t.Errorf("the branch is still on origin: %s", remote)
	}
	if _, err := os.Stat(filepath.Join(wt, "plans", "12.md")); err == nil {
		t.Errorf("the plan file is still in the worktree")
	}
	if r := l.AisIn(wt, nil, "", "flow", "review", "check"); r.Code != 0 {
		t.Errorf("check after cancel: %s", r)
	}
	if r := l.Flow("cancel", pid); r.Code == 0 {
		t.Errorf("cancelling twice: %s", r)
	}

	l.SetStatus(url, "ready")
	var ready []struct {
		URL       string `json:"url"`
		Plannable bool   `json:"plannable"`
	}
	must(t, json.Unmarshal([]byte(l.MustFlow("ready", "--ws", "soi", "--json").Stdout), &ready))
	found := false
	for _, r := range ready {
		if r.URL == url {
			found = r.Plannable
		}
	}
	if !found {
		t.Errorf("the ticket is not plannable again after cancel: %+v", ready)
	}
}

// A branch with work besides the plan is left alone, and cancel says so.
func TestFlowCancelKeepsWork(t *testing.T) {
	l := flowLab(t)
	pid := l.queueAdd("--title", "Has work", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	l.MustFlow("dispatch")
	sid := l.waitRunning(pid)
	wt := filepath.Join(l.Session(sid).Cwd, "backend")
	plan := filepath.Join(l.Dir, "plan.md")
	l.WriteFile(plan, "# Plan: x\n")
	if r := l.AisIn(wt, []string{"AIS_FLOW_PROPOSAL=" + pid}, "", "flow", "review", "open", "--ticket", "gpwsoi/backend#12", "--file", plan); r.Code != 0 {
		t.Fatalf("open:\n%s", r)
	}
	l.Commit(wt, "export.go", "Real work")
	head := l.Git(wt, "rev-parse", "HEAD")
	r := l.Flow("cancel", pid)
	if r.Code == 0 || !strings.Contains(r.Stderr, "work besides the plan") {
		t.Errorf("cancel with work on the branch: %s", r)
	}
	if p := l.Proposals(); p[0].State != "cancelled" {
		t.Errorf("proposal: %+v", p[0])
	}
	if l.Git(wt, "rev-parse", "HEAD") != head {
		t.Errorf("cancel moved a branch that held work")
	}
}
