package flow

import (
	"strings"
	"testing"
)

// A block written by renderBlock reads back with its round log, and the
// body above it is left exactly as it was.
func TestRefinementBlockRoundTrip(t *testing.T) {
	in := &RefineInput{Level: "L1", Why: "No Done-when.",
		Questions: []Question{{Text: "Which formats?", Default: "CSV", Weight: "blocks"}}}
	log := []RoundLog{{At: "2026-11-20", Level: "L1", Questions: 1}}
	body := "The ticket.\n\nWith **markdown**."
	full := body + "\n\n" + renderBlock(in, log, 3, false) + "\n"

	got, r := splitRefinement(full)
	if got != body {
		t.Errorf("body = %q", got)
	}
	if r == nil || r.Rounds != 1 || len(r.Log) != 1 || !strings.Contains(r.Text, "1. Which formats?") {
		t.Fatalf("block = %+v", r)
	}
	// A flagged L1 (the limit) and an L2 are not rounds sent back.
	log = append(log, RoundLog{At: "2026-11-21", Level: "L1", Flag: "refinement-limit"}, RoundLog{At: "2026-11-22", Level: "L2"})
	_, r = splitRefinement(body + "\n\n" + renderBlock(&RefineInput{Level: "L2"}, log, 3, false))
	if r.Rounds != 1 || len(r.Log) != 3 {
		t.Errorf("rounds = %d, log %+v", r.Rounds, r.Log)
	}
	// CRLF from a browser edit still parses.
	if _, r := splitRefinement(strings.ReplaceAll(full, "\n", "\r\n")); r == nil {
		t.Errorf("a CRLF body lost its block")
	}
	if b, r := splitRefinement("no block here"); r != nil || b != "no block here" {
		t.Errorf("a plain body: %q %+v", b, r)
	}
}

// The version ignores what an editor rewrites, and sees an answer.
func TestTicketVersion(t *testing.T) {
	r := &Refinement{Text: "1. Q\n   > Answer:"}
	v := ticketVersion("T", "body\n", r)
	if ticketVersion("T", "body  \r\n\n", &Refinement{Text: "1. Q\r\n   > Answer:  "}) != v {
		t.Errorf("whitespace changed the version")
	}
	if ticketVersion("T", "body", &Refinement{Text: "1. Q\n   > Answer: yes"}) == v {
		t.Errorf("an answer did not change the version")
	}
	if ticketVersion("T2", "body", r) == v || ticketVersion("T", "body", nil) == v {
		t.Errorf("title or block did not count")
	}
}

func TestMinLevel(t *testing.T) {
	for _, c := range [][3]string{{"L3", "L2", "L2"}, {"L2", "L3", "L2"}, {"", "L3", "L3"}, {"L2", "", "L2"}} {
		if got := minLevel(c[0], c[1]); got != c[2] {
			t.Errorf("minLevel(%s, %s) = %s", c[0], c[1], got)
		}
	}
}

// Every built-in prompt renders with or without a workspace, carries its
// version, and adds instructions only where they belong.
func TestBuiltinPromptsRender(t *testing.T) {
	w := &Workspace{Name: "soi", Repos: map[string]string{"backend": "/r/backend"}}
	w.Source.Ready, w.Refine.Status, w.Refine.MaxRounds = "ready", "refinement", 3
	w.Instructions = "ALL ROLES"
	w.Session.Instructions = "WORK ONLY"
	for _, role := range roles {
		if _, v := promptVersion(builtinPrompt(role)); v != "v1" {
			t.Errorf("%s has version %q", role, v)
		}
		d := promptData{Tickets: []string{"https://github.com/o/r/issues/1"}, Ticket: "https://github.com/o/r/issues/1",
			Repos: []repoView{{Name: "backend", Path: "./backend"}}}
		text, warning, err := renderRole(w, role, d)
		if err != nil || warning != "" {
			t.Fatalf("%s: %v %s", role, err, warning)
		}
		if strings.Contains(text, "ais-prompt:") || !strings.Contains(text, "ALL ROLES") ||
			strings.Contains(text, "WORK ONLY") != (role == "work") {
			t.Errorf("%s prompt:\n%s", role, text)
		}
		if strings.Contains(text, "gh issue view") || strings.Contains(text, "gh project item") {
			t.Errorf("%s prompt sends the agent to gh for tickets", role)
		}
		if _, _, err := renderRole(nil, role, d); err != nil {
			t.Errorf("%s without a workspace: %v", role, err)
		}
	}
	// A literal prompt from before prompts were built in keeps working.
	w.Session.Prompt = "/work-tickets {tickets} --autonomy {autonomy}\n\n{notes}"
	text, _, _ := renderRole(w, "work", promptData{Tickets: []string{"u1", "u2"}, Notes: "n", Ceiling: "L3"})
	if text != "/work-tickets u1 u2 --autonomy L3\n\nn" {
		t.Errorf("literal prompt = %q", text)
	}
}
