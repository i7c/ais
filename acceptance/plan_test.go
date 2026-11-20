package acceptance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plannerLab is flowLab with the planner played by fake, its window closing
// as soon as you leave a decided planner, and idle as given.
func plannerLab(t *testing.T, idle string) *Lab {
	l := flowLab(t)
	cfg := l.ReadFile(filepath.Join(l.AisHome, "config.toml"))[len(l.BaseConfig()):]
	l.WriteConfig(cfg + "\n[workspace.soi.planner]\nprofile = \"fake\"\nclose_after = \"0s\"\nidle_timeout = \"" + idle + "\"\n")
	return l
}

type plannerRow struct {
	ID, State, Session string
}

// waitPlanner waits until planner id's session is up, returning its id.
func (l *Lab) waitPlanner(id string) string {
	l.t.Helper()
	var sid string
	l.Eventually("planner "+id+" up", func() bool {
		for _, s := range l.State().Sessions {
			if s.FlowPlanner == id && s.Status == "active" {
				sid = s.ID
			}
		}
		return sid != "" && fileExists(filepath.Join(l.Log, sid, "ready"))
	})
	return sid
}

func (l *Lab) planStart() string {
	l.t.Helper()
	return strings.TrimSpace(l.MustFlow("plan", "soi").Stdout)
}

func TestFlowPlanLifecycle(t *testing.T) {
	l := plannerLab(t, "48h")
	ready := l.ReadFile(filepath.Join(l.Log, "gh", "project.json"))
	l.Board(issue("gpwsoi/iac", 5, "Todo"))
	if r := l.MustFlow("plan", "soi"); strings.TrimSpace(r.Stdout) != "" || !strings.Contains(r.Stderr, "no tickets") {
		t.Errorf("plan with nothing ready:\n%s", r)
	}
	l.WriteFile(filepath.Join(l.Log, "gh", "project.json"), ready)

	id := l.planStart()
	if !strings.HasPrefix(id, "n") {
		t.Fatalf("plan printed %q, want a planner id", id)
	}
	sid := l.waitPlanner(id)
	s := l.Session(sid)
	if s.Cwd != l.Home || s.Tmux.SessionName != "soi" || len(s.Worktree) != 0 || !strings.Contains(s.Description, "soi") {
		t.Errorf("planner session = %+v", s)
	}
	if argv := l.Fake(sid).Argv; argv[len(argv)-1] != "/plan-sessions soi" {
		t.Errorf("planner argv = %q", argv)
	}
	if _, ok := l.Fake(sid).Env["AIS_FLOW_PLANNER"]; ok {
		t.Errorf("AIS_FLOW_PLANNER leaked into the harness's env")
	}

	// A singleton: while it is open, plan starts no other.
	if r := l.MustFlow("plan", "soi"); strings.TrimSpace(r.Stdout) != "" || !strings.Contains(r.Stderr, "waiting for you") {
		t.Errorf("second plan:\n%s", r)
	}

	// What the planner queues is recorded as its decision.
	in := []string{"AIS_SESSION=" + sid}
	pid := strings.TrimSpace(l.MustAis(in, "flow", "queue", "add", "--title", "From the planner",
		"--tickets", "gpwsoi/backend#12", "--repos", "backend").Stdout)
	var shown struct {
		PlannerID string `json:"planner_id"`
	}
	must(t, json.Unmarshal([]byte(l.MustFlow("queue", "show", pid, "--json").Stdout), &shown))
	if shown.PlannerID != id {
		t.Errorf("proposal planner_id = %q, want %s", shown.PlannerID, id)
	}
	l.MustAis(in, "flow", "plan", "done")
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "planner-decided") || !strings.Contains(log, "1 proposal(s) queued") {
		t.Errorf("log:\n%s", log)
	}
	if r := l.Ais(in, "flow", "plan", "done"); r.Code == 0 {
		t.Errorf("plan done twice succeeded")
	}

	// The next dispatch closes the decided planner's window (close_after 0s).
	l.MustFlow("dispatch")
	l.Eventually("planner window closed", func() bool { return l.Session(sid).Status == "dead" })

	// #13 and iac#4 are still ready: the next run plans them, in a new planner.
	next := l.planStart()
	if next == "" || next == id {
		t.Fatalf("no fresh planner for the tickets left ready (got %q)", next)
	}
	l.waitPlanner(next)
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, next+" since") {
		t.Errorf("ls does not show the open planner:\n%s", ls)
	}
}

// A planner nobody answers expires after idle_timeout, counted from your
// last prompt to it; one whose session ends is aborted. Either way the next
// run starts a fresh one.
func TestFlowPlanExpiresAndAborts(t *testing.T) {
	l := plannerLab(t, "3s")
	first := l.planStart()
	sid := l.waitPlanner(first)

	time.Sleep(2 * time.Second)
	l.Submit(Started{ID: sid}, "let me think")
	time.Sleep(1500 * time.Millisecond) // 3.5s since start, 1.5s since your input
	if r := l.MustFlow("plan", "soi"); !strings.Contains(r.Stderr, "waiting for you") {
		t.Errorf("a planner you answered 1.5s ago was not kept:\n%s", r)
	}
	time.Sleep(2 * time.Second)
	second := l.planStart()
	if second == "" || second == first {
		t.Fatalf("idle planner not replaced (got %q)", second)
	}
	l.Eventually("expired planner's session ends", func() bool { return l.Session(sid).Status == "dead" })
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "planner-expired") {
		t.Errorf("log lacks the expiry:\n%s", log)
	}

	sid2 := l.waitPlanner(second)
	l.WriteFile(filepath.Join(l.Log, sid2, "exit"), "0")
	l.Eventually("planner session ends", func() bool { return l.Session(sid2).Status == "dead" })
	third := l.planStart()
	if third == "" || third == second {
		t.Fatalf("aborted planner not replaced (got %q)", third)
	}
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "planner-aborted") {
		t.Errorf("log lacks the abort:\n%s", log)
	}
}

func TestFlowWorkspace(t *testing.T) {
	l := flowLab(t)
	var ws []struct {
		Name     string            `json:"name"`
		Repos    map[string]string `json:"repos"`
		Statuses map[string]string `json:"statuses"`
	}
	must(t, json.Unmarshal([]byte(l.MustFlow("workspace", "--json").Stdout), &ws))
	if len(ws) != 1 || ws[0].Name != "soi" || len(ws[0].Repos) != 2 || ws[0].Statuses["scheduled"] != "scheduled" {
		t.Errorf("workspace = %+v", ws)
	}
	if txt := l.MustFlow("workspace", "soi").Stdout; !strings.Contains(txt, "backend") || !strings.Contains(txt, "iac") {
		t.Errorf("workspace text:\n%s", txt)
	}
}
