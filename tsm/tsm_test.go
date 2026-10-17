package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// line builds one list-panes line in tmux's field order.
func line(sess, sname, win string, widx int, wname, pane string, pidx int, cmd, path string, marks ...string) string {
	m := append(marks, "", "", "", "", "", "0")[:6]
	f := []string{
		sess, sname, "1", win, fmt.Sprint(widx), wname, "1", "0", "1700000000",
		pane, fmt.Sprint(pidx), "1", cmd, path, "42", "title", "0", "80", "24",
		m[0], m[1], m[2], m[3], m[4], m[5],
	}
	return strings.Join(f, sep)
}

func fixture() *Snapshot {
	out := strings.Join([]string{
		line("$1", "soi", "@1", 1, "backend", "%1", 1, "python3", "/home/x/git/repo", "aaa", "claude", "fix it", "/wt/fix/repo"),
		line("$1", "soi", "@2", 2, "dev", "%2", 1, "zsh", "/home/x/dev"),
		line("$1", "soi", "@2", 2, "dev", "%3", 2, "tofu", "/home/x/dev"),
		line("$2", "irc", "@3", 1, "weechat", "%4", 1, "weechat", "/home/x"),
		clientMarker + strings.Join([]string{"/dev/pts/1", "$1", "%2", "1"}, sep),
	}, "\n")
	return ParseSnapshot(out)
}

func TestParseSnapshotKeepsTheTree(t *testing.T) {
	s := fixture()
	if n, p := s.Counts(); n != 2 || p != 4 {
		t.Fatalf("counts = %d sessions, %d panes", n, p)
	}
	if s.Client != (Client{Name: "/dev/pts/1", SessionID: "$1", PaneID: "%2"}) {
		t.Fatalf("client = %+v", s.Client)
	}
	p := s.Pane("%1")
	if p.AisID != "aaa" || p.Dir() != "/wt/fix/repo" || p.Location() != "soi:1.1" {
		t.Fatalf("pane = %+v dir %s at %s", p, p.Dir(), p.Location())
	}
	if s.Pane("%3").Dir() != "/home/x/dev" {
		t.Fatal("an unmarked pane works where tmux sees it")
	}
}

func TestQueryFormatMatchesTheParser(t *testing.T) {
	args := QueryArgs()
	if got := strings.Count(args[3], sep) + 1; got != len(paneFields) {
		t.Fatalf("format has %d fields, parser wants %d", got, len(paneFields))
	}
}

func kinds(rows []Row) string {
	var b []string
	for _, r := range rows {
		b = append(b, map[RowKind]string{RowHeader: "H", RowSession: "S", RowWindow: "W", RowPane: "P", RowResumable: "R"}[r.Kind])
	}
	return strings.Join(b, "")
}

func TestOnePaneWindowsAreOneRow(t *testing.T) {
	rows := BuildRows(fixture(), nil, TreeOptions{Now: time.Now()})
	// soi: backend (merged), dev (window + 2 panes); irc: weechat (merged)
	if got := kinds(rows); got != "SWWPPSW" {
		t.Fatalf("rows = %s", got)
	}
	if rows[1].Pane == nil || rows[1].Pane.ID != "%1" {
		t.Fatal("a merged window row stands for its pane")
	}
	if rows[2].Pane != nil {
		t.Fatal("a window of several panes is not one of them")
	}
}

func TestCollapseAndFilter(t *testing.T) {
	snap := fixture()
	rows := BuildRows(snap, nil, TreeOptions{Collapsed: map[string]bool{"$1": true}})
	if got := kinds(rows); got != "SSW" {
		t.Fatalf("collapsed rows = %s", got)
	}
	rows = BuildRows(snap, nil, TreeOptions{Filter: "tofu", Collapsed: map[string]bool{"$1": true}})
	if got := kinds(rows); got != "SWP" || rows[2].Pane.ID != "%3" {
		t.Fatalf("filtering opens collapsed sessions and keeps only matches: %s", kinds(rows))
	}
	if rows := BuildRows(snap, nil, TreeOptions{Filter: "soi dev zsh"}); len(rows) != 3 {
		t.Fatalf("every term must match: %s", kinds(rows))
	}
}

func state(now time.Time, sessions ...AisSession) *AisState {
	return &AisState{API: 1, Host: "h", NotifyWindow: 60, Sessions: sessions}
}

func TestWhoeverCalledComesFirst(t *testing.T) {
	now := time.Now()
	st := state(now, AisSession{
		ID: "aaa", Profile: "claude", Status: "active", Host: "h",
		Notifications: []AisNotification{{At: now.Add(-time.Minute).Format(time.RFC3339), Message: "need you"}},
	})
	rows := BuildRows(fixture(), st, TreeOptions{Now: now})
	if !strings.HasPrefix(kinds(rows), "HP") || !rows[1].Wants || rows[1].Pane.ID != "%1" {
		t.Fatalf("rows = %s", kinds(rows))
	}
	st.Sessions[0].Notifications[0].Suppressed = true
	if rows := BuildRows(fixture(), st, TreeOptions{Now: now}); rows[0].Kind == RowHeader {
		t.Fatal("a notification you were looking at does not call for you")
	}
}

func TestOldSessionsAreFoundByTheirPane(t *testing.T) {
	st := state(time.Now(), AisSession{ID: "old", Profile: "pi", Status: "active", Host: "h"})
	st.Sessions[0].Tmux.PaneID = "%4"
	rows := BuildRows(fixture(), st, TreeOptions{})
	if r := rows[len(rows)-1]; r.Ais == nil || r.Ais.ID != "old" {
		t.Fatal("an unmarked pane ais recorded is still its session")
	}
}

func TestResumableOffersEachPieceOfWorkOnce(t *testing.T) {
	dead := func(id, label, cwd, ended string) AisSession {
		return AisSession{ID: id, Profile: "claude", Description: label, Status: "dead", Host: "h", Cwd: cwd, EndedAt: ended, CanRestart: true}
	}
	st := state(time.Now(),
		dead("a1", "auth", "/w/auth", "2026-01-01T00:00:00Z"),
		dead("a2", "auth", "/w/auth", "2026-01-02T00:00:00Z"), // the same work, later
		dead("b1", "parser", "/w/parser", "2026-01-03T00:00:00Z"),
		dead("c1", "docs", "/w/docs", "2026-01-04T00:00:00Z"),
		AisSession{ID: "c2", Profile: "claude", Description: "docs", Status: "active", Host: "h", Cwd: "/w/docs"},
		dead("d1", "old", "/w/old", "2026-01-05T00:00:00Z"),
		AisSession{ID: "d2", RestartOf: "d1", Status: "dead", Host: "h"},
	)
	var ids []string
	for _, s := range st.Resumable(0) {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"b1", "a2"}) {
		t.Fatalf("resumable = %v", ids)
	}
}

func TestAisStateVersionIsChecked(t *testing.T) {
	if _, err := ParseAisState([]byte(`{"api": 2}`)); err == nil {
		t.Fatal("a newer api must be refused, not misread")
	}
	st, err := ParseAisState([]byte(`{"api": 1, "sessions": [{"id": "x", "future_key": true}]}`))
	if err != nil || st.Sessions[0].ID != "x" {
		t.Fatal("unknown keys are ignored")
	}
}

type fakeRunner struct {
	mu    sync.Mutex // the relay calls it from more than one goroutine
	calls [][]string
	reply func(args []string) string
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(args), nil
	}
	return "", nil
}

func (f *fakeRunner) find(cmd string) []string {
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == cmd {
			return c
		}
	}
	return nil
}

func actions(r *fakeRunner) Actions {
	return Actions{tmux: Tmux{r: r}, ais: Ais{r: r, bin: "ais"}, snap: fixture()}
}

func TestANewWindowGoesAfterTheAnchorsWindow(t *testing.T) {
	r := &fakeRunner{reply: func(a []string) string { return "%9\n" }}
	snap := fixture()
	if _, err := (Tmux{r: r}).Open(snap.Pane("%3"), "/d", map[string]string{"B": "2", "A": "1"}, NewWindow); err != nil {
		t.Fatal(err)
	}
	want := []string{"tmux", "new-window", "-a", "-t", "@2", "-P", "-F", "#{pane_id}", "-c", "/d", "-e", "A=1", "-e", "B=2"}
	if !reflect.DeepEqual(r.calls[0], want) {
		t.Fatalf("got %v", r.calls[0])
	}
}

func TestDuplicateForksWhatCanFork(t *testing.T) {
	for _, tc := range []struct {
		sess  AisSession
		typed string
	}{
		{AisSession{ID: "aaa", Profile: "claude", CanFork: true}, "ais run --fork aaa"},
		{AisSession{ID: "aaa", Profile: "api", Description: "dev server", Args: []string{"--port", "8001"}},
			"ais run api --no-worktree -d 'dev server' -- --port 8001"},
	} {
		r := &fakeRunner{reply: func(a []string) string {
			if a[0] == "new-window" {
				return "%9\n"
			}
			return ""
		}}
		acts := actions(r)
		row := Row{Kind: RowWindow, Pane: acts.snap.Pane("%1"), Window: acts.snap.Pane("%1").Window, Ais: &tc.sess}
		out, err := acts.Duplicate(row)
		if err != nil || !out.Quit {
			t.Fatalf("duplicate: %v %+v", err, out)
		}
		send := r.find("send-keys")
		if send == nil || send[len(send)-1] != tc.typed {
			t.Fatalf("typed %v, want %q", send, tc.typed)
		}
		if focus := r.find("switch-client"); focus == nil || focus[5] != "%9" {
			t.Fatalf("does not land in the new pane: %v", r.calls)
		}
	}
}

func TestAShellBesideAnAgentGetsItsPlace(t *testing.T) {
	r := &fakeRunner{reply: func(a []string) string {
		if a[0] == "env" {
			return `{"api":1,"cwd":"/wt/fix/repo","env":{"AIS_WORKTREE":"/wt/fix/repo"}}`
		}
		return "%9\n"
	}}
	acts := actions(r)
	p := acts.snap.Pane("%1")
	if _, err := acts.Sibling(Row{Kind: RowWindow, Pane: p, Window: p.Window}, SplitRight); err != nil {
		t.Fatal(err)
	}
	split := r.find("split-window")
	if !reflect.DeepEqual(split[3:], []string{"-t", "%1", "-P", "-F", "#{pane_id}", "-c", "/wt/fix/repo", "-e", "AIS_WORKTREE=/wt/fix/repo"}) {
		t.Fatalf("split = %v", split)
	}
}

func TestOnlyIdleShellsDieWithoutAsking(t *testing.T) {
	snap := fixture()
	w := snap.Pane("%2").Window
	if KillPrompt(Row{Kind: RowPane, Pane: snap.Pane("%2"), Window: w}) != "" {
		t.Fatal("an idle shell is not worth a question")
	}
	if KillPrompt(Row{Kind: RowPane, Pane: snap.Pane("%3"), Window: w}) == "" {
		t.Fatal("a running tofu is")
	}
	if KillPrompt(Row{Kind: RowWindow, Window: w}) == "" || KillPrompt(Row{Kind: RowSession, Session: w.Session}) == "" {
		t.Fatal("so is a window of several panes, and a session")
	}
}

func TestShellJoin(t *testing.T) {
	got := ShellJoin([]string{"ais", "run", "-d", "it's here", "--cwd=/a b", "%1"})
	if got != `ais run -d 'it'\''s here' '--cwd=/a b' %1` {
		t.Fatal(got)
	}
}

func TestFitIsExactlyAsWideAsAsked(t *testing.T) {
	for _, s := range []string{"", "short", "\x1b[31mred and quite a lot longer than ten\x1b[0m", "日本語のテキスト"} {
		if w := ansi.StringWidth(fit(s, 10)); w != 10 {
			t.Fatalf("fit(%q) is %d wide", s, w)
		}
	}
}

func TestTmuxIsToldItSpeaksUTF8(t *testing.T) {
	last := func(env []string) string { return env[len(env)-1] }
	if got := last(tmuxEnv([]string{"PATH=/bin"})); got != "LC_CTYPE=C.UTF-8" {
		t.Fatalf("no locale at all (ssh): %s", got)
	}
	if got := tmuxEnv([]string{"LANG=en_US.UTF-8"}); len(got) != 1 {
		t.Fatal("a UTF-8 locale is left alone")
	}
	if got := last(tmuxEnv([]string{"LANG=en_US.UTF-8", "LC_ALL=C"})); got != "LC_ALL=C.UTF-8" {
		t.Fatalf("LC_ALL=C would win over LC_CTYPE: %s", got)
	}
}

func TestABellCallsForYou(t *testing.T) {
	out := strings.Join([]string{
		line("$1", "soi", "@1", 1, "backend", "%1", 1, "python3", "/home/x/git/repo", "aaa", "claude", "fix it", "/wt/fix/repo", "", "1"),
		line("$1", "soi", "@2", 2, "dev", "%2", 1, "zsh", "/home/x/dev", "", "", "", "", "", "1"),
		line("$2", "irc", "@3", 1, "weechat", "%4", 1, "weechat", "/home/x", "", "", "", "", "", "1"),
		clientMarker + strings.Join([]string{"/dev/pts/1", "$1", "%2", "1"}, sep),
	}, "\n")
	snap := ParseSnapshot(out)
	if !snap.Pane("%4").Window.Bell {
		t.Fatal("tmux's bell flag is read")
	}
	now := time.Now()
	st := state(now, AisSession{ID: "aaa", Profile: "claude", Status: "active", Host: "h",
		Notifications: []AisNotification{{At: now.Add(-time.Minute).Format(time.RFC3339), Message: "need you"}}})
	var callers []string
	for _, r := range BuildRows(snap, st, TreeOptions{Now: now}) {
		if r.Wants {
			callers = append(callers, r.Pane.ID)
		}
	}
	// %1 once though it rang too; %2 rang where you are looking, which tmux
	// would not even flag.
	if !reflect.DeepEqual(callers, []string{"%1", "%4"}) {
		t.Fatalf("callers = %v", callers)
	}
}
