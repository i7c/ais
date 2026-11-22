package acceptance

import (
	"path/filepath"
	"strings"
	"testing"
)

func (l *Lab) ghCalls() string { return l.ReadFile(filepath.Join(l.Log, "gh", "calls.log")) }

// Reading one ticket, or moving one, asks about that ticket: it does not
// list the whole board, which agents reading many tickets pay for each time.
func TestTicketDoesNotListTheBoard(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	if out := l.MustFlow("ticket", "show", url).Stdout; !strings.Contains(out, `board status "ready"`) {
		t.Errorf("show:\n%s", out)
	}
	if out := l.MustFlow("ticket", "show", "gpwsoi/backend#999").Stdout; strings.Contains(out, "board status") {
		t.Errorf("a ticket not on the board has a status:\n%s", out)
	}
	l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	if s := l.BoardStatus(url); s != "refinement" {
		t.Errorf("status after refine %q", s)
	}
	if calls := l.ghCalls(); strings.Contains(calls, "item-list") {
		t.Errorf("the board was listed:\n%s", calls)
	}
}
