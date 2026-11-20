package acceptance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func (l *Lab) SubIssues(parent string, subs ...string) {
	l.t.Helper()
	path := filepath.Join(l.Log, "gh", "subissues.json")
	all := map[string][]string{}
	if fileExists(path) {
		must(l.t, json.Unmarshal([]byte(l.ReadFile(path)), &all))
	}
	all[parent] = subs
	b, _ := json.Marshal(all)
	l.WriteFile(path, string(b))
}

func (l *Lab) Close(urls ...string) {
	path := filepath.Join(l.Log, "gh", "closed")
	text := ""
	if fileExists(path) {
		text = l.ReadFile(path)
	}
	l.WriteFile(path, text+strings.Join(urls, "\n")+"\n")
}

type readyTicket struct {
	URL       string        `json:"url"`
	State     string        `json:"state"`
	Status    string        `json:"status"`
	HeldBy    string        `json:"held_by"`
	OpenSubs  int           `json:"open_sub_issues"`
	Plannable bool          `json:"plannable"`
	Work      []string      `json:"work"`
	SubIssues []readyTicket `json:"sub_issues"`
}

func (l *Lab) Ready() []readyTicket {
	l.t.Helper()
	var out []readyTicket
	must(l.t, json.Unmarshal([]byte(l.MustFlow("ready", "--json").Stdout), &out))
	return out
}

func find(ts []readyTicket, url string) *readyTicket {
	for i := range ts {
		if ts[i].URL == url {
			return &ts[i]
		}
	}
	return nil
}

// An epic marked ready stays ready: sessions take its sub-issues, and the
// epic itself only once they are all closed.
func TestFlowEpic(t *testing.T) {
	l := flowLab(t)
	epic, a, b, c := ghURL("gpwsoi/org", 103), ghURL("gpwsoi/org", 105), ghURL("gpwsoi/org", 106), ghURL("gpwsoi/org", 107)
	l.Board(issue("gpwsoi/org", 103, "ready"), issue("gpwsoi/org", 105, "Todo"), issue("gpwsoi/org", 106, "Todo"),
		issue("gpwsoi/org", 107, "In Progress"))
	l.SubIssues(epic, a, b, c)

	e := find(l.Ready(), epic)
	if e == nil || e.OpenSubs != 3 || e.Plannable || strings.Join(e.Work, " ") != a+" "+b {
		t.Fatalf("epic = %+v; want 3 open sub-issues, itself not plannable, work = #105 #106 (#107 is in progress)", e)
	}
	if txt := l.MustFlow("ready").Stdout; !strings.Contains(txt, "epic: 3 sub-issue(s) open, 2 plannable") || !strings.Contains(txt, "gpwsoi/org#105") {
		t.Errorf("ready text:\n%s", txt)
	}

	// The epic itself cannot be queued while it has open sub-issues...
	if r := l.Flow("queue", "add", "--title", "the epic", "--tickets", epic, "--repos", "backend"); r.Code == 0 || !strings.Contains(r.Stderr, "open sub-issue") {
		t.Errorf("queueing the epic:\n%s", r)
	}
	// ...its sub-issues can, and the epic stays ready.
	p := l.queueAdd("--title", "First sub-issue", "--tickets", a, "--repos", "backend")
	if s := l.BoardStatus(epic); s != "ready" {
		t.Errorf("epic moved to %q", s)
	}
	e = find(l.Ready(), epic)
	if strings.Join(e.Work, " ") != b || e.SubIssues[0].HeldBy != p {
		t.Errorf("after queueing #105: work %v, #105 held by %q", e.Work, e.SubIssues[0].HeldBy)
	}

	// All sub-issues closed: the epic is the work left, to finish it.
	l.Close(a, b, c)
	e = find(l.Ready(), epic)
	if e.OpenSubs != 0 || !e.Plannable || strings.Join(e.Work, " ") != epic {
		t.Errorf("finished epic = %+v", e)
	}
	l.queueAdd("--title", "Finish the epic", "--tickets", epic, "--repos", "backend")
	if s := l.BoardStatus(epic); s != "scheduled" {
		t.Errorf("finished epic is %q after queueing, want scheduled", s)
	}
}

// A sub-issue with sub-issues of its own is an epic too: its leaves are the
// work.
func TestFlowNestedEpic(t *testing.T) {
	l := flowLab(t)
	top, mid, leaf := ghURL("gpwsoi/org", 1), ghURL("gpwsoi/org", 2), ghURL("gpwsoi/org", 3)
	l.Board(issue("gpwsoi/org", 1, "ready"), issue("gpwsoi/org", 2, "Todo"), issue("gpwsoi/org", 3, "Todo"))
	l.SubIssues(top, mid)
	l.SubIssues(mid, leaf)
	if e := find(l.Ready(), top); e == nil || strings.Join(e.Work, " ") != leaf || e.SubIssues[0].Plannable {
		t.Errorf("nested epic = %+v", e)
	}
}

// The timer's planner check needs work, room and min_interval; by hand it
// only needs work; a finished session asks at once.
func TestFlowPlanTriggers(t *testing.T) {
	l := plannerLab(t, "48h")
	l.MustFlow("limit", "wip", "1")
	held := l.queueAdd("--title", "Fills the queue", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	if r := l.MustFlow("plan", "soi", "--auto"); strings.TrimSpace(r.Stdout) != "" || !strings.Contains(r.Stderr, "fill wip") {
		t.Errorf("timer check with the queue full:\n%s", r)
	}

	// Started and finished: the slot it frees opens a planner straight away.
	l.MustFlow("dispatch")
	sid := l.waitRunning(held)
	r := l.MustAis([]string{"AIS_SESSION=" + sid}, "flow", "done")
	planner := strings.TrimSpace(r.Stdout)
	if !strings.HasPrefix(planner, "n") {
		t.Fatalf("done did not open a planner:\n%s", r)
	}
	psid := l.waitPlanner(planner)
	l.MustAis([]string{"AIS_SESSION=" + psid}, "flow", "plan", "done")
	l.MustFlow("dispatch") // closes its window

	// min_interval (2h by default) holds the timer back, not a hand.
	if r := l.MustFlow("plan", "soi", "--auto"); !strings.Contains(r.Stderr, "min_interval") {
		t.Errorf("timer check right after a planner:\n%s", r)
	}
	if id := strings.TrimSpace(l.MustFlow("plan", "soi").Stdout); !strings.HasPrefix(id, "n") {
		t.Errorf("plan by hand did not start a planner")
	}
}
