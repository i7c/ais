package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (l *Lab) ghFail(what string) {
	l.t.Helper()
	os.Remove(filepath.Join(l.Log, "gh", "edited"))
	l.WriteFile(filepath.Join(l.Log, "gh", "fail"), what)
}

// A write that stopped half way, the ticket's body written and the rest not,
// is the same round when it is tried again.
func TestRefineRetryIsTheSameRound(t *testing.T) {
	for _, stop := range []string{"read-after-edit", "item-edit"} {
		t.Run(stop, func(t *testing.T) {
			l := refineLab(t, 3)
			url := ghURL("gpwsoi/backend", 12)
			l.Body(url, "Vague.")
			for range 2 {
				l.ghFail(stop)
				if r := l.FlowIn(l1Findings, "ticket", "refine", url, "--file", "-"); r.Code == 0 {
					t.Fatalf("refine succeeded through a failing %s", stop)
				}
			}
			l.ghFail("")
			l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
			body := l.BodyOf(url)
			if !strings.Contains(body, "round 1 of 3") || strings.Count(body, `"level":"L1"`) != 1 || l.BoardStatus(url) != "refinement" {
				t.Errorf("status %s, body:\n%s", l.BoardStatus(url), body)
			}
			n := 0
			for _, c := range l.Comments() {
				if strings.Contains(c["body"], "Refinement round 1 of 3") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d round comments, want 1: %v", n, l.Comments())
			}

			// A finished round is not a retry: the next one counts.
			l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
			if b := l.BodyOf(url); !strings.Contains(b, "round 2 of 3") {
				t.Errorf("the second round:\n%s", b)
			}
		})
	}
}

// ...also when the second try failed before it wrote anything, with other
// findings: the first one's block is still the one on the ticket.
func TestRefineRetryAfterFailedEdit(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Body(url, "Vague.")
	l.ghFail("item-edit")
	l.FlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	l.ghFail("edit")
	other := strings.Replace(l1Findings, "Which formats?", "Which columns?", 1)
	l.FlowIn(other, "ticket", "refine", url, "--file", "-")
	l.ghFail("")
	l.MustFlowIn(other, "ticket", "refine", url, "--file", "-")
	if b := l.BodyOf(url); !strings.Contains(b, "round 1 of 3") || !strings.Contains(b, "Which columns?") {
		t.Errorf("body:\n%s", b)
	}
}

// ...but not when you answered what the stopped write left on the ticket.
func TestRefineAfterAnswerIsANewRound(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Body(url, "Vague.")
	l.ghFail("item-edit")
	l.FlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	l.ghFail("")
	l.Body(url, strings.Replace(l.BodyOf(url), "> Answer:", "> Answer: CSV", 1))
	l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	if b := l.BodyOf(url); !strings.Contains(b, "round 2 of 3") {
		t.Errorf("body:\n%s", b)
	}
}

// A stopped round tried again with another outcome is still one round.
func TestRefineRetryWithAnotherOutcome(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Body(url, "Vague.")
	l.ghFail("read-after-edit")
	l.FlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	l.ghFail("")
	l.MustFlowIn(l3Findings, "ticket", "assess", url, "--level", "L3", "--file", "-")
	if b := l.BodyOf(url); strings.Contains(b, `"level":"L1"`) || !strings.Contains(b, "L3") {
		t.Errorf("body:\n%s", b)
	}
}
