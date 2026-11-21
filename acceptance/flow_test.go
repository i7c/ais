package acceptance

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// boardItem is one item on the fake gh board, as gh prints it.
type boardItem struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Title   string `json:"title"`
	Content struct {
		Type       string `json:"type"`
		URL        string `json:"url"`
		Number     int    `json:"number"`
		Repository string `json:"repository"`
	} `json:"content"`
}

func issue(repo string, n int, status string) boardItem {
	var it boardItem
	it.ID = fmt.Sprintf("PVTI_%s_%d", strings.ReplaceAll(repo, "/", "_"), n)
	it.Status, it.Title = status, fmt.Sprintf("%s ticket %d", repo, n)
	it.Content.Type, it.Content.Number, it.Content.Repository = "Issue", n, repo
	it.Content.URL = fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)
	return it
}

func ghURL(repo string, n int) string { return fmt.Sprintf("https://github.com/%s/issues/%d", repo, n) }

func (l *Lab) Board(items ...boardItem) {
	l.t.Helper()
	b, err := json.Marshal(map[string]any{"items": items})
	must(l.t, err)
	l.WriteFile(filepath.Join(l.Log, "gh", "project.json"), string(b))
}

func (l *Lab) BoardStatus(url string) string {
	l.t.Helper()
	var data struct{ Items []boardItem }
	must(l.t, json.Unmarshal([]byte(l.ReadFile(filepath.Join(l.Log, "gh", "project.json"))), &data))
	for _, it := range data.Items {
		if it.Content.URL == url {
			return it.Status
		}
	}
	l.t.Fatalf("%s not on the board", url)
	return ""
}

func (l *Lab) Comments() []map[string]string {
	path := filepath.Join(l.Log, "gh", "comments.json")
	if !fileExists(path) {
		return nil
	}
	var out []map[string]string
	must(l.t, json.Unmarshal([]byte(l.ReadFile(path)), &out))
	return out
}

func (l *Lab) Flow(args ...string) Result { return l.Ais(nil, append([]string{"flow"}, args...)...) }

func (l *Lab) MustFlow(args ...string) Result {
	l.t.Helper()
	r := l.Flow(args...)
	if r.Code != 0 {
		l.t.Fatalf("ais flow %s failed:\n%s", strings.Join(args, " "), r)
	}
	return r
}

// flowLab is a lab with a workspace "soi" on the fake board and two repos.
func flowLab(t *testing.T) *Lab {
	l := NewLab(t)
	backend, iac := l.Repo("backend"), l.Repo("iac")
	l.WriteConfig(fmt.Sprintf(`
[workspace.soi]
tmux_session = "soi"
profile = "fake"

[workspace.soi.source]
owner = "gpwsoi"
project = 3

[workspace.soi.repos]
backend = %q
iac = %q
`, backend, iac))
	l.Board(
		issue("gpwsoi/backend", 12, "ready"),
		issue("gpwsoi/backend", 13, "ready"),
		issue("gpwsoi/iac", 4, "ready"),
		issue("gpwsoi/iac", 5, "Todo"),
		issue("gpwsoi/backend", 1, "Done"),
	)
	return l
}

type queued struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Tickets  []string `json:"tickets"`
	Repos    []string `json:"repos"`
	Profile  string   `json:"profile"`
	Prompt   string   `json:"prompt"`
	Notes    string   `json:"notes"`
	Priority int      `json:"priority"`
	After    []string `json:"after"`
	State    string   `json:"state"`
}

func (l *Lab) Queue(args ...string) []queued {
	l.t.Helper()
	var out []queued
	must(l.t, json.Unmarshal([]byte(l.MustFlow(append([]string{"queue", "ls", "--json"}, args...)...).Stdout), &out))
	return out
}

func TestFlowReady(t *testing.T) {
	l := flowLab(t)
	r := l.MustFlow("ready", "--ws", "soi", "--json")
	var items []struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	}
	must(t, json.Unmarshal([]byte(r.Stdout), &items))
	if len(items) != 3 {
		t.Errorf("ready = %+v, want the three ready issues", items)
	}
	// One workspace configured: it is the default.
	if txt := l.MustFlow("ready").Stdout; !strings.Contains(txt, "gpwsoi/iac#4") || strings.Contains(txt, "#5") {
		t.Errorf("ready text:\n%s", txt)
	}
	if r := l.Flow("ready", "--ws", "nope"); r.Code == 0 {
		t.Errorf("ready of an unknown workspace succeeded")
	}
}

func TestFlowQueueAdd(t *testing.T) {
	l := flowLab(t)
	r := l.MustFlow("queue", "add", "--ws", "soi", "--title", "Auth across backend and IaC",
		"--tickets", ghURL("gpwsoi/backend", 12)+",gpwsoi/iac#4", "--repos", "backend", "--repos", "iac",
		"--priority", "2", "--notes", "start with the backend")
	id := strings.TrimSpace(r.Stdout)
	if !strings.HasPrefix(id, "p") || len(id) != 7 {
		t.Fatalf("queue add printed %q, want the proposal id", r.Stdout)
	}
	for _, u := range []string{ghURL("gpwsoi/backend", 12), ghURL("gpwsoi/iac", 4)} {
		if s := l.BoardStatus(u); s != "scheduled" {
			t.Errorf("%s is %q, want scheduled", u, s)
		}
	}
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 13)); s != "ready" {
		t.Errorf("an untouched ticket moved to %q", s)
	}
	if c := l.Comments(); len(c) != 2 || !strings.Contains(c[0]["body"], id) {
		t.Errorf("comments = %v", c)
	}
	q := l.Queue()
	if len(q) != 1 || q[0].ID != id || q[0].State != "queued" || q[0].Priority != 2 || q[0].Profile != "fake" ||
		strings.Join(q[0].Repos, ",") != "backend,iac" {
		t.Fatalf("queue = %+v", q)
	}
	// The built-in work prompt: both tickets, the notes, the repos.
	want := "Notes from planning:\n\nstart with the backend"
	for _, part := range []string{"# Work on gpwsoi/backend#12, gpwsoi/iac#4", "- " + ghURL("gpwsoi/backend", 12),
		"- " + ghURL("gpwsoi/iac", 4), want, "`backend`: `./backend`", "`iac`: `./iac`", "Autonomy ceiling: L2"} {
		if !strings.Contains(q[0].Prompt, part) {
			t.Errorf("prompt lacks %q:\n%s", part, q[0].Prompt)
		}
	}
	if show := l.MustFlow("queue", "show", id[:4]).Stdout; !strings.Contains(show, want) {
		t.Errorf("queue show:\n%s", show)
	}
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "queued") || !strings.Contains(log, id) {
		t.Errorf("log:\n%s", log)
	}

	// A lower-priority one, after the first: listed second.
	second := strings.TrimSpace(l.MustFlow("queue", "add", "--title", "Follow-up", "--tickets", "gpwsoi/backend#13",
		"--repos", "backend", "--after", id).Stdout)
	q = l.Queue()
	if len(q) != 2 || q[0].ID != id || q[1].ID != second || len(q[1].After) != 1 || q[1].After[0] != id {
		t.Errorf("queue order/after = %+v", q)
	}
}

func TestFlowQueueAddRefusals(t *testing.T) {
	l := flowLab(t)
	l.MustFlow("queue", "add", "--title", "Holds #12", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	before := len(l.Queue())
	cases := map[string][]string{
		"no title":          {"--tickets", "gpwsoi/backend#13", "--repos", "backend"},
		"no tickets":        {"--title", "x", "--repos", "backend"},
		"no repos":          {"--title", "x", "--tickets", "gpwsoi/backend#13"},
		"unknown repo":      {"--title", "x", "--tickets", "gpwsoi/backend#13", "--repos", "frontend"},
		"not a ticket":      {"--title", "x", "--tickets", "twelve", "--repos", "backend"},
		"not on the board":  {"--title", "x", "--tickets", "gpwsoi/backend#99", "--repos", "backend"},
		"ticket held":       {"--title", "x", "--tickets", "gpwsoi/backend#12", "--repos", "backend"},
		"profile no prompt": {"--title", "x", "--tickets", "gpwsoi/backend#13", "--repos", "backend", "--profile", "plain"},
		"unknown after":     {"--title", "x", "--tickets", "gpwsoi/backend#13", "--repos", "backend", "--after", "pzzzzz"},
		"unknown option":    {"--title", "x", "--bogus"},
	}
	for name, args := range cases {
		if r := l.Flow(append([]string{"queue", "add"}, args...)...); r.Code == 0 {
			t.Errorf("%s: succeeded", name)
		}
	}
	if n := len(l.Queue()); n != before {
		t.Errorf("refusals queued %d proposal(s)", n-before)
	}
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 13)); s != "ready" {
		t.Errorf("a refusal moved #13 to %q", s)
	}
}

// If the board will not take the new status, nothing is queued.
func TestFlowQueueAddRollsBack(t *testing.T) {
	l := flowLab(t)
	l.WriteFile(filepath.Join(l.Log, "gh", "fail"), "item-edit")
	if r := l.Flow("queue", "add", "--title", "x", "--tickets", "gpwsoi/backend#12", "--repos", "backend"); r.Code == 0 {
		t.Fatalf("queue add succeeded although the board refused")
	}
	if q := l.Queue("--all"); len(q) != 0 {
		t.Errorf("queue after a refused board = %+v", q)
	}
	// A failing comment, though, is only logged: the status is what counts.
	l.WriteFile(filepath.Join(l.Log, "gh", "fail"), "comment")
	l.MustFlow("queue", "add", "--title", "x", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "comment-failed") {
		t.Errorf("log lacks the failed comment:\n%s", log)
	}
}

func TestFlowQueueEditAndCancel(t *testing.T) {
	l := flowLab(t)
	id := strings.TrimSpace(l.MustFlow("queue", "add", "--title", "Edit me", "--tickets", "gpwsoi/backend#12,gpwsoi/backend#13",
		"--repos", "backend").Stdout)
	l.MustFlow("queue", "edit", id, "--priority", "5", "--notes", "new notes", "--repos", "backend,iac")
	q := l.Queue()[0]
	if q.Priority != 5 || q.Notes != "new notes" || !strings.Contains(q.Prompt, "Notes from planning:\n\nnew notes") || len(q.Repos) != 2 {
		t.Errorf("after edit: %+v", q)
	}
	l.MustFlow("queue", "edit", id, "--prompt", "do it my way")
	l.MustFlow("queue", "edit", id, "--notes", "changed again")
	if q := l.Queue()[0]; q.Prompt != "do it my way" {
		t.Errorf("notes rewrote a hand-written prompt: %q", q.Prompt)
	}
	l.MustFlow("queue", "edit", id, "--prompt", "")
	if q := l.Queue()[0]; !strings.HasPrefix(q.Prompt, "# Work on ") {
		t.Errorf("an empty --prompt did not go back to the template: %q", q.Prompt)
	}
	if r := l.Flow("queue", "edit", id); r.Code == 0 {
		t.Errorf("edit with nothing to change succeeded")
	}

	// Cancel: #12 is still scheduled and goes back; #13 was moved on by hand
	// and stays where it is.
	var data struct{ Items []boardItem }
	must(t, json.Unmarshal([]byte(l.ReadFile(filepath.Join(l.Log, "gh", "project.json"))), &data))
	for i := range data.Items {
		if data.Items[i].Content.Number == 13 {
			data.Items[i].Status = "In Progress"
		}
	}
	l.Board(data.Items...)
	l.MustFlow("queue", "cancel", id, "--reason", "not now after all")
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 12)); s != "ready" {
		t.Errorf("#12 is %q after cancel, want ready", s)
	}
	if s := l.BoardStatus(ghURL("gpwsoi/backend", 13)); s != "In Progress" {
		t.Errorf("#13 is %q after cancel; a hand-moved ticket should stay", s)
	}
	if q := l.Queue(); len(q) != 0 {
		t.Errorf("queue after cancel = %+v", q)
	}
	if q := l.Queue("--all"); len(q) != 1 || q[0].State != "cancelled" {
		t.Errorf("queue --all = %+v", q)
	}
	if r := l.Flow("queue", "cancel", id); r.Code == 0 {
		t.Errorf("cancelling twice succeeded")
	}
	// And #12 can be queued again.
	l.MustFlow("queue", "add", "--title", "Again", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
}

func TestFlowLimitAndLs(t *testing.T) {
	l := flowLab(t)
	l.WriteConfig(strings.Replace(l.ReadFile(filepath.Join(l.AisHome, "config.toml"))[len(l.BaseConfig()):],
		"[workspace.soi]", "[flow]\nper_day = 10\n\n[workspace.soi]", 1))
	show := l.MustFlow("limit").Stdout
	for _, want := range []string{"wip       4  config", "per_hour  4  config", "per_day   10  config"} {
		if !strings.Contains(strings.Join(strings.Fields(show), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("limit lacks %q:\n%s", want, show)
		}
	}
	set := l.MustFlow("limit", "per_day", "32").Stdout
	if !strings.Contains(set, "override until") || !strings.Contains(set, "(config: 10)") {
		t.Errorf("after limit per_day 32:\n%s", set)
	}
	l.MustFlow("limit", "wip", "6", "--for", "2h")
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, "running 0/6") || !strings.Contains(ls, "last 24h 0/32") {
		t.Errorf("ls does not use the overrides:\n%s", ls)
	}
	l.MustFlow("limit", "--clear", "wip")
	if ls := l.MustFlow("ls").Stdout; !strings.Contains(ls, "running 0/4") {
		t.Errorf("wip override not cleared:\n%s", ls)
	}
	l.MustFlow("limit", "--clear")
	if show := l.MustFlow("limit").Stdout; strings.Contains(show, "override") {
		t.Errorf("--clear left overrides:\n%s", show)
	}
	for name, args := range map[string][]string{
		"unknown key":    {"limit", "speed", "3"},
		"not a number":   {"limit", "wip", "many"},
		"bad until":      {"limit", "wip", "3", "--until", "tomorrowish"},
		"until and for":  {"limit", "wip", "3", "--until", "eod", "--for", "1h"},
		"one positional": {"limit", "wip"},
	} {
		if r := l.Flow(args...); r.Code == 0 {
			t.Errorf("%s: succeeded", name)
		}
	}

	l.MustFlow("queue", "add", "--title", "x", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	ls := l.MustFlow("ls").Stdout
	row := ""
	for _, line := range lines(ls) {
		if strings.HasPrefix(line, "soi ") {
			row = line
		}
	}
	if f := strings.Fields(row); len(f) < 4 || f[1] != "-" || f[2] != "1" || f[3] != "0" {
		t.Errorf("ls row for soi = %q (want no planner, 1 queued, 0 running)\n%s", row, ls)
	}
}

func TestFlowConfigErrors(t *testing.T) {
	l := NewLab(t)
	if r := l.Flow("ls"); r.Code != 0 || !strings.Contains(r.Stderr, "no workspaces") {
		t.Errorf("ls without workspaces:\n%s", r)
	}
	for name, extra := range map[string]string{
		"source without project": "[workspace.x.source]\nowner = \"o\"\n",
		"unknown source kind":    "[workspace.x.source]\nkind = \"jira\"\nowner = \"o\"\nproject = 1\n",
		"bad wip":                "[flow]\nwip = -1\n",
		"bad idle timeout":       "[workspace.x.source]\nowner = \"o\"\nproject = 1\n[workspace.x.planner]\nidle_timeout = \"two days\"\n",
	} {
		l.WriteConfig(extra)
		if r := l.Flow("ls"); r.Code == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}
