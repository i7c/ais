package main

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// layout writes a snapshot as "session:index[pane pane] ...", which is all
// a move can change.
func layout(s *Snapshot) string {
	var parts []string
	for _, sess := range s.Sessions {
		for _, w := range sess.Windows {
			var ids []string
			for _, p := range w.Panes {
				ids = append(ids, p.ID)
			}
			parts = append(parts, sess.Name+":"+strconv.Itoa(w.Index)+"["+strings.Join(ids, " ")+"]")
		}
	}
	return strings.Join(parts, " ")
}

// fixture: soi:1[%1] soi:2[%2 %3] irc:1[%4]

func TestStagingShowsWhatTmuxWillDo(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan []MoveOp
		want string
	}{
		{"join", []MoveOp{{Kind: moveJoin, Src: "%3", Dst: "%1"}},
			"soi:1[%1 %3] soi:2[%2] irc:1[%4]"},
		{"a window emptied by a join goes", []MoveOp{{Kind: moveJoin, Src: "%1", Dst: "%2"}},
			"soi:2[%2 %1 %3] irc:1[%4]"},
		{"break into another session", []MoveOp{{Kind: moveBreak, Src: "%3", Session: "$2"}},
			"soi:1[%1] soi:2[%2] irc:1[%4] irc:2[%3]"},
		{"a session emptied by a move goes", []MoveOp{{Kind: moveWindow, Src: "%4", Session: "$1"}},
			"soi:1[%1] soi:2[%2 %3] soi:3[%4]"},
		{"a new session, after its placeholder", []MoveOp{{Kind: moveWindow, Src: "%1", NewSession: "work"}},
			"soi:2[%2 %3] irc:1[%4] work:2[%1]"},
		{"later moves see earlier ones", []MoveOp{
			{Kind: moveBreak, Src: "%3", NewSession: "work"},
			{Kind: moveJoin, Src: "%4", Dst: "%3"},
		}, "soi:1[%1] soi:2[%2] work:2[%3 %4]"},
	} {
		view, ok, err := Staged(fixture(), tc.plan)
		if err != nil || ok != len(tc.plan) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := layout(view); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestStagingNeverTouchesTheRealSnapshot(t *testing.T) {
	snap := fixture()
	before := layout(snap)
	Staged(snap, []MoveOp{{Kind: moveWindow, Src: "%4", Session: "$1"}})
	if layout(snap) != before {
		t.Fatal("the snapshot changed under the staged view")
	}
}

func TestMovesThatMakeNoSenseAreRefused(t *testing.T) {
	for _, op := range []MoveOp{
		{Kind: moveJoin, Src: "%3", Dst: "%2"},       // same window already
		{Kind: moveWindow, Src: "%1", Session: "$1"}, // same session already
		{Kind: moveBreak, Src: "%1", Session: "$2"},  // alone: moves as the window
		{Kind: moveJoin, Src: "%9", Dst: "%1"},       // gone
		{Kind: moveWindow, Src: "%1", NewSession: "irc"},
	} {
		if err := fixture().Apply(op); err == nil {
			t.Errorf("%+v was allowed", op)
		}
	}
}

func TestAPlanStopsWhereItStopsApplying(t *testing.T) {
	_, ok, err := Staged(fixture(), []MoveOp{
		{Kind: moveJoin, Src: "%3", Dst: "%1"},
		{Kind: moveJoin, Src: "%9", Dst: "%1"},
		{Kind: moveJoin, Src: "%4", Dst: "%1"},
	})
	if ok != 1 || err == nil {
		t.Fatalf("ok = %d, err = %v", ok, err)
	}
}

func TestTheCommandsForAPlan(t *testing.T) {
	got := Commands([]MoveOp{
		{Kind: moveJoin, Src: "%3", Dst: "%1"},
		{Kind: moveWindow, Src: "%4", NewSession: "work"},
	})
	want := [][]string{
		{"join-pane", "-d", "-h", "-s", "%3", "-t", "%1"},
		{"new-session", "-d", "-s", "work"},
		{"move-window", "-d", "-s", "%4", "-t", "work:"},
		{"kill-pane", "-t", "<placeholder in work>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

type failAt struct {
	fakeRunner
	cmd string
}

func (f *failAt) Run(name string, args ...string) (string, error) {
	out, _ := f.fakeRunner.Run(name, args...)
	if args[0] == f.cmd {
		return "", errors.New("no")
	}
	return out, nil
}

func TestRunningAPlanCleansUpItsPlaceholders(t *testing.T) {
	r := &failAt{fakeRunner: fakeRunner{reply: func(a []string) string { return "%50\n" }}, cmd: "join-pane"}
	done, err := Tmux{r: r}.RunMoves([]MoveOp{
		{Kind: moveWindow, Src: "%4", NewSession: "work"},
		{Kind: moveJoin, Src: "%3", Dst: "%1"},
		{Kind: moveWindow, Src: "%1", Session: "$2"},
	})
	if done != 1 || err == nil {
		t.Fatalf("done %d, err %v", done, err)
	}
	last := r.calls[len(r.calls)-1]
	if !reflect.DeepEqual(last, []string{"tmux", "kill-pane", "-t", "%50"}) {
		t.Fatalf("placeholder not cleaned up: %v", r.calls)
	}
	for _, c := range r.calls {
		if c[1] == "move-window" && c[5] == "%1" {
			t.Fatal("ran past the failure")
		}
	}
}

func TestPlansAreDescribedInWords(t *testing.T) {
	got := DescribePlan(fixture(), []MoveOp{
		{Kind: moveJoin, Src: "%3", Dst: "%1"},
		{Kind: moveWindow, Src: "%4", NewSession: "work"},
	})
	if len(got) != 2 || !strings.Contains(got[0], "→ beside") || !strings.HasSuffix(got[1], "→ work (new)") {
		t.Fatalf("got %q", got)
	}
}
