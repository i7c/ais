package acceptance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type runRow struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Log      string `json:"log"`
}

func (l *Lab) Runs(args ...string) map[string]runRow {
	l.t.Helper()
	var rows []runRow
	must(l.t, json.Unmarshal([]byte(l.MustAis(nil, append([]string{"runs", "--json"}, args...)...).Stdout), &rows))
	out := map[string]runRow{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// A headless run needs no pane, prints to its log, and is listed, counted
// and readable like any other run, before and after prune.
func TestRunHeadless(t *testing.T) {
	l := NewLab(t)
	done := make(chan Result, 1)
	go func() {
		done <- l.AisIn(l.Home, []string{"TMUX=", "TMUX_PANE="}, "", "run", "fake", "--headless", "-d", "Nightly check", "--prompt", "look around")
	}()
	var sid string
	l.Eventually("the headless session is up", func() bool {
		for _, s := range l.State().Sessions {
			if s.Description == "Nightly check" && s.Status == "active" {
				sid = s.ID
			}
		}
		return sid != "" && fileExists(filepath.Join(l.Log, sid, "ready"))
	})
	s := l.Session(sid)
	if !s.Headless || s.Tmux.PaneID != "" || s.Log != filepath.Join(l.AisHome, "logs", sid+".log") {
		t.Errorf("headless session = %+v", s)
	}
	if f := l.Fake(sid); f.Prompt() != "look around" || indexOf(f.Argv, "-p") < 0 {
		t.Errorf("argv = %q", f.Argv)
	}
	// Reaping goes by the process: a live headless run stays active.
	l.MustAis(nil, "ls")
	if l.Session(sid).Status != "active" {
		t.Errorf("a live headless run was reaped")
	}
	if r := l.Ais(nil, "select", sid); r.Code == 0 {
		t.Errorf("select of a headless session succeeded")
	}
	if r := l.Runs("--active")[sid]; r.Kind != "headless" || r.Status != "active" {
		t.Errorf("runs --active: %+v", r)
	}

	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "3")
	if r := <-done; r.Code != 3 {
		t.Errorf("ais run --headless exited %d, want the harness's 3\n%s", r.Code, r)
	}
	if out := l.MustAis(nil, "log", sid).Stdout; !strings.Contains(out, "fake: exiting 3") {
		t.Errorf("log = %q", out)
	}

	// An interactive one beside it, and a transcript for --full.
	pane := l.Start(l.Home, "run", "fake", "-d", "By hand")
	conv := deref(l.Session(pane.ID).Conversation)
	l.WriteFile(filepath.Join(l.Home, ".claude", "projects", "-lab", conv+".jsonl"),
		`{"type":"user","timestamp":"2026-11-21T10:00:00Z","message":{"role":"user","content":"hello there"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-11-21T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`+"\n"+
			`{"type":"summary","summary":"skipped"}`+"\n")
	l.Stop(pane, 0)
	if out := l.MustAis(nil, "log", pane.ID).Stdout; !strings.Contains(out, "── you") || !strings.Contains(out, "hello there") ||
		!strings.Contains(out, "Looking.") || !strings.Contains(out, `→ Bash {"command":"ls"}`) || strings.Contains(out, "skipped") {
		t.Errorf("transcript:\n%s", out)
	}

	check := func(when string) {
		runs := l.Runs()
		if r := runs[sid]; r.Kind != "headless" || r.ExitCode == nil || *r.ExitCode != 3 {
			t.Errorf("%s: headless run %+v", when, r)
		}
		if r := runs[pane.ID]; r.Kind != "interactive" || r.ExitCode == nil || *r.ExitCode != 0 {
			t.Errorf("%s: interactive run %+v", when, r)
		}
		var st struct {
			Kinds map[string]map[string]struct {
				Started, Failed int
			} `json:"kinds"`
		}
		must(t, json.Unmarshal([]byte(l.MustAis(nil, "stats", "--json").Stdout), &st))
		if c := st.Kinds["total"]["24h"]; c.Started != 2 || c.Failed != 1 {
			t.Errorf("%s: stats total 24h = %+v", when, c)
		}
		if c := st.Kinds["headless"]["1h"]; c.Started != 1 {
			t.Errorf("%s: stats headless 1h = %+v", when, c)
		}
	}
	check("before prune")
	l.MustAis(nil, "prune", "--all")
	if n := len(l.State().Sessions); n != 0 {
		t.Errorf("%d sessions left after prune --all", n)
	}
	check("after prune")
	if out := l.MustAis(nil, "log", sid).Stdout; !strings.Contains(out, "fake: exiting 3") {
		t.Errorf("log after prune = %q", out)
	}
	if r := l.Ais(nil, "runs", "--since", "nonsense"); r.Code == 0 {
		t.Errorf("runs --since nonsense succeeded")
	}
	if out := l.MustAis(nil, "runs", "--kind", "headless").Stdout; !strings.Contains(out, sid) || strings.Contains(out, pane.ID) {
		t.Errorf("runs --kind headless:\n%s", out)
	}
}
