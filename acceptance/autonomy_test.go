package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tickets, refinement and plan review: docs/autonomy-proposal.md.

func (l *Lab) Body(url, body string) {
	l.t.Helper()
	path := filepath.Join(l.Log, "gh", "bodies.json")
	all := map[string]string{}
	if fileExists(path) {
		must(l.t, json.Unmarshal([]byte(l.ReadFile(path)), &all))
	}
	all[url] = body
	b, _ := json.Marshal(all)
	l.WriteFile(path, string(b))
}

func (l *Lab) BodyOf(url string) string {
	l.t.Helper()
	all := map[string]string{}
	must(l.t, json.Unmarshal([]byte(l.ReadFile(filepath.Join(l.Log, "gh", "bodies.json"))), &all))
	return all[url]
}

// SetStatus moves a ticket on the fake board, as you would by hand.
func (l *Lab) SetStatus(url, status string) {
	l.t.Helper()
	path := filepath.Join(l.Log, "gh", "project.json")
	var data struct {
		Items []boardItem `json:"items"`
	}
	must(l.t, json.Unmarshal([]byte(l.ReadFile(path)), &data))
	for i := range data.Items {
		if data.Items[i].Content.URL == url {
			data.Items[i].Status = status
		}
	}
	b, _ := json.Marshal(data)
	l.WriteFile(path, string(b))
}

// FlowIn runs ais flow with stdin.
func (l *Lab) FlowIn(stdin string, args ...string) Result {
	return l.AisIn(l.Home, nil, stdin, append([]string{"flow"}, args...)...)
}

func (l *Lab) MustFlowIn(stdin string, args ...string) Result {
	l.t.Helper()
	r := l.FlowIn(stdin, args...)
	if r.Code != 0 {
		l.t.Fatalf("ais flow %s failed:\n%s", strings.Join(args, " "), r)
	}
	return r
}

// refineLab is flowLab with a board that has a refinement status, a sweep
// on every tick and max_rounds as given.
func refineLab(t *testing.T, maxRounds int) *Lab {
	l := flowLab(t)
	l.WriteFile(filepath.Join(l.Log, "gh", "statuses"), "Todo\nready\nrefinement\nscheduled\nIn Progress\nDone\n")
	cfg := l.ReadFile(filepath.Join(l.AisHome, "config.toml"))[len(l.BaseConfig()):]
	l.WriteConfig(cfg + fmt.Sprintf("\n[workspace.soi.refine]\nsweep = \"0s\"\nmax_rounds = %d\n", maxRounds))
	return l
}

type proposalRow struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	State   string   `json:"state"`
	Tickets []string `json:"tickets"`
	Phase   string   `json:"phase"`
	PlanURL string   `json:"plan_url"`
}

func (l *Lab) Proposals() []proposalRow {
	l.t.Helper()
	var out []proposalRow
	must(l.t, json.Unmarshal([]byte(l.MustFlow("queue", "ls", "--all", "--json").Stdout), &out))
	return out
}

func (l *Lab) refineRun(url string) proposalRow {
	l.t.Helper()
	for _, p := range l.Proposals() {
		if p.Kind == "refine" && len(p.Tickets) == 1 && p.Tickets[0] == url && p.State != "done" {
			return p
		}
	}
	l.t.Fatalf("no refinement run for %s: %+v", url, l.Proposals())
	return proposalRow{}
}

const l1Findings = `{"level": "L1", "why": "No Done-when (R2).",
 "criteria": [{"id": "R2", "name": "Done when", "met": false, "evidence": "none anywhere"}],
 "evidence": [{"text": "export streams rows", "source": "backend/export.go:40"}],
 "decisions": [], "questions": [{"text": "Which formats?", "default": "CSV", "weight": "blocks", "raised": ""}],
 "rewrite": ["Say why."], "notes": []}`

const l3Findings = `{"level": "L3", "why": "Everything is in the chain.", "criteria": [], "evidence": [],
 "decisions": [{"text": "CSV and XLSX.", "round": 1, "by": "you"}], "questions": [], "rewrite": [], "notes": ["use excelize"]}`

func TestFlowTicket(t *testing.T) {
	l := flowLab(t)
	epic, sub := ghURL("gpwsoi/backend", 12), ghURL("gpwsoi/backend", 13)
	l.SubIssues(epic, sub)
	l.Body(epic, "The outcome: offline reports.")
	l.Body(sub, "Export to CSV.\n\n**Done when**\n- [ ] a CSV downloads")

	show := l.MustFlow("ticket", "show", "gpwsoi/backend#13", "--context").Stdout
	for _, part := range []string{"# gpwsoi/backend ticket 13", "board status \"ready\"", "Export to CSV.",
		"## Ancestor 1: gpwsoi/backend ticket 12", "offline reports"} {
		if !strings.Contains(show, part) {
			t.Errorf("show lacks %q:\n%s", part, show)
		}
	}
	var v struct {
		Title  string `json:"title"`
		Status string `json:"status"`
		Parent struct {
			URL string `json:"url"`
		} `json:"parent"`
	}
	must(t, json.Unmarshal([]byte(l.MustFlow("ticket", "show", sub, "--json").Stdout), &v))
	if v.Status != "ready" || v.Parent.URL != epic {
		t.Errorf("show --json = %+v", v)
	}

	l.MustFlow("ticket", "comment", sub, "--body", "hand-off")
	if c := l.Comments(); len(c) != 1 || c[0]["url"] != sub || c[0]["body"] != "hand-off" {
		t.Errorf("comments = %v", c)
	}
	created := strings.TrimSpace(l.MustFlowIn("the follow-up", "ticket", "new", "--parent", epic, "--title", "Follow up", "--body-file", "-").Stdout)
	if !strings.HasPrefix(created, "https://github.com/gpwsoi/backend/issues/") || !strings.Contains(l.MustFlow("ticket", "show", created).Stdout, epic) {
		t.Errorf("new printed %q", created)
	}
	if s := l.BoardStatus(created); s != "Todo" {
		t.Errorf("the new ticket is %q on the board, want it added", s)
	}
	l.MustFlow("ticket", "close", sub, "--not-planned", "--comment", "superseded")
	closes := l.ReadFile(filepath.Join(l.Log, "gh", "closes.json"))
	if !strings.Contains(closes, `"reason": "not planned"`) || !strings.Contains(closes, "superseded") {
		t.Errorf("closes = %s", closes)
	}
	if r := l.Flow("ticket", "close", sub); r.Code != 2 {
		t.Errorf("close without --done or --not-planned: %s", r)
	}
}

// A ticket that turns ready is refined before any planner sees it: an L1
// goes back to its author with questions in a block of its body, comes back
// once edited and moved to ready, and an L2/L3 becomes plannable. A
// refinement run's slot frees itself when it exits.
func TestFlowRefinement(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Body(url, "Make exports better.")
	l.Close(ghURL("gpwsoi/backend", 13), ghURL("gpwsoi/iac", 4)) // one ticket to watch

	if ts := l.Ready(); len(find(ts, url).Work) != 0 {
		t.Errorf("an unassessed ticket is plannable: %+v", find(ts, url))
	}
	l.MustFlow("dispatch")
	run := l.refineRun(url)
	sid := l.waitRunning(run.ID)
	f := l.Fake(sid)
	if i := indexOf(f.Argv, "--"); i < 1 || f.Argv[i-1] != "-p" || !strings.HasPrefix(f.Prompt(), "# Refine a ticket before it is planned") ||
		!strings.Contains(f.Prompt(), "Your ticket: **"+url+"**") {
		t.Errorf("refine argv = %q", f.Argv)
	}
	if f.Env["AIS_FLOW_WORKSPACE"] != "soi" || len(l.Session(sid).Worktree) != 0 {
		t.Errorf("refine run env %v, worktree %v", f.Env, l.Session(sid).Worktree)
	}
	if s := l.BoardStatus(url); s != "ready" {
		t.Errorf("a ticket being refined moved to %q", s)
	}

	// The run sends it back: questions in the block, status, a comment.
	l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	body := l.BodyOf(url)
	if !strings.HasPrefix(body, "Make exports better.\n\n<!-- ais:refinement ") || !strings.Contains(body, "round 1 of 3") ||
		!strings.Contains(body, "1. Which formats?") || !strings.Contains(body, "> Answer:") {
		t.Errorf("body after refine:\n%s", body)
	}
	if s := l.BoardStatus(url); s != "refinement" {
		t.Errorf("status after refine %q", s)
	}
	if c := l.Comments(); len(c) == 0 || !strings.Contains(c[len(c)-1]["body"], "Refinement round 1 of 3") {
		t.Errorf("comments = %v", c)
	}
	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "0")
	l.Eventually("the refine run is done by itself", func() bool { return l.proposalState(run.ID) == "done" })

	// Moved back to ready unchanged: still L1, nothing runs.
	l.SetStatus(url, "ready")
	l.MustFlow("dispatch")
	for _, p := range l.Proposals() {
		if p.Kind == "refine" && p.State != "done" {
			t.Errorf("an unchanged ticket was refined again: %+v", p)
		}
	}
	var pending []struct {
		URL     string `json:"url"`
		Pending string `json:"pending"`
	}
	must(t, json.Unmarshal([]byte(l.MustFlow("ready", "--json").Stdout), &pending))
	if len(pending) == 0 || !strings.HasPrefix(pending[0].Pending, "still L1") {
		t.Errorf("ready after an unchanged return: %+v", pending)
	}

	// Answered: a new version, a new run, and an L3 makes it plannable.
	l.Body(url, strings.Replace(l.BodyOf(url), "> Answer:", "> Answer: CSV and XLSX", 1))
	l.MustFlow("dispatch")
	run2 := l.refineRun(url)
	sid2 := l.waitRunning(run2.ID)
	l.MustFlowIn(l3Findings, "ticket", "assess", url, "--level", "L3", "--file", "-")
	if b := l.BodyOf(url); !strings.Contains(b, "D1 (round 1): CSV and XLSX.") || strings.Contains(b, "   > Answer:") ||
		!strings.Contains(b, "## Refinement · L3 · ") || !strings.Contains(b, "after 1 round") {
		t.Errorf("body after assess:\n%s", b)
	}
	if ts := l.Ready(); len(find(ts, url).Work) != 0 {
		t.Errorf("a ticket is plannable while its refinement run holds it: %+v", find(ts, url))
	}
	l.WriteFile(filepath.Join(l.Log, sid2, "exit"), "0")
	l.Eventually("the second run is done", func() bool { return l.proposalState(run2.ID) == "done" })
	if ts := l.Ready(); !contains(find(ts, url).Work, url) {
		t.Errorf("an L3 ticket is not plannable: %+v", find(ts, url))
	}

	// The work prompt carries what refinement found.
	pid := l.queueAdd("--title", "Exports", "--tickets", url, "--repos", "backend", "--autonomy", "L3")
	prompt := l.MustFlow("prompt", "show", "work", "--proposal", pid).Stdout
	if !strings.Contains(prompt, "Autonomy ceiling: L3") || !strings.Contains(prompt, "gpwsoi/backend#12 L3") {
		t.Errorf("work prompt:\n%s", prompt)
	}
}

// After max_rounds back to its author, the next L1 goes to the planner
// flagged, with its questions open, instead of round after round.
func TestFlowRefinementLimit(t *testing.T) {
	l := refineLab(t, 1)
	url := ghURL("gpwsoi/backend", 12)
	l.Body(url, "Vague.")
	l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	l.Body(url, strings.Replace(l.BodyOf(url), "> Answer:", "> Answer: no idea", 1))
	l.SetStatus(url, "ready")
	r := l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	if !strings.Contains(r.Stderr, "limit") || l.BoardStatus(url) != "ready" {
		t.Errorf("past the limit: %s, status %s", r, l.BoardStatus(url))
	}
	if b := l.BodyOf(url); !strings.Contains(b, "Refinement limit reached") || !strings.Contains(b, "[refinement-limit]") {
		t.Errorf("body:\n%s", b)
	}
	if ts := l.Ready(); !contains(find(ts, url).Work, url) {
		t.Errorf("a ticket past the limit is not plannable: %+v", find(ts, url))
	}
	// By hand, one more round is allowed.
	id := strings.TrimSpace(l.MustFlow("refine", url).Stdout)
	if !strings.HasPrefix(id, "r") {
		t.Fatalf("refine printed %q", id)
	}
	l.MustFlowIn(l1Findings, "ticket", "refine", url, "--file", "-")
	if s := l.BoardStatus(url); s != "refinement" {
		t.Errorf("the extra round did not go back to its author: %s", s)
	}
}

// A run that ends without an outcome holds its ticket back for refine.retry
// instead of starting again on every tick.
func TestFlowRefinementFailedRun(t *testing.T) {
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Close(ghURL("gpwsoi/backend", 13), ghURL("gpwsoi/iac", 4))
	l.MustFlow("dispatch")
	run := l.refineRun(url)
	sid := l.waitRunning(run.ID)
	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "1")
	l.Eventually("the failed run is done", func() bool { return l.proposalState(run.ID) == "done" })
	l.MustFlow("dispatch")
	for _, p := range l.Proposals() {
		if p.Kind == "refine" && p.State != "done" {
			t.Errorf("a failed ticket was retried at once: %+v", p)
		}
	}
	if log := l.MustFlow("log").Stdout; !strings.Contains(log, "refine-failed") {
		t.Errorf("log:\n%s", log)
	}
}

// The plan is a draft PR; approval is the gate; finalize drops the plan
// commits once the branch holds the work, and check finds any left.
func TestFlowReview(t *testing.T) {
	l := flowLab(t)
	src := filepath.Join(l.Dir, "src", "backend")
	l.Git(src, "checkout", "-q", "-b", "export-v2")
	plan := filepath.Join(l.Dir, "plan.md")
	l.WriteFile(plan, "# Plan: Export v2  ·  L2 (ceiling L3)  ·  #12\n\n## Approach\nStream it.\n")
	review := func(args ...string) Result {
		return l.AisIn(src, nil, "", append([]string{"flow", "review"}, args...)...)
	}

	if r := review("finalize"); r.Code != 0 {
		t.Errorf("finalize with no plan: %s", r)
	}
	r := review("open", "--ticket", "gpwsoi/backend#12", "--file", plan)
	if r.Code != 0 {
		t.Fatalf("open:\n%s", r)
	}
	prURL := strings.TrimSpace(r.Stdout)
	if log := l.Git(src, "log", "-1", "--format=%s%n%b"); !strings.Contains(log, "Plan: Export v2") || !strings.Contains(log, "Ais-Plan: "+ghURL("gpwsoi/backend", 12)) {
		t.Errorf("plan commit:\n%s", log)
	}
	if remote := l.Git(src, "ls-remote", "origin", "export-v2"); remote == "" {
		t.Errorf("the plan was not pushed")
	}
	prs := l.ReadFile(filepath.Join(l.Log, "gh", "prs.json"))
	if !strings.Contains(prs, `"isDraft": true`) || !strings.Contains(prs, "Part of #12") || !strings.Contains(prs, prURL) {
		t.Errorf("prs = %s", prs)
	}
	if c := l.Comments(); len(c) != 1 || !strings.Contains(c[0]["body"], prURL) {
		t.Errorf("the ticket was not told: %v", c)
	}
	if r := review("check"); r.Code == 0 || !strings.Contains(r.Stderr, "plan commit") {
		t.Errorf("check with a plan on the branch: %s", r)
	}

	l.WriteFile(plan, "# Plan: Export v2\n\n## Approach\nStream it, in pages.\n")
	if r := review("update", "--file", plan); r.Code != 0 {
		t.Fatalf("update:\n%s", r)
	}
	if r := review("finalize"); r.Code == 0 || !strings.Contains(r.Stderr, "not approved") {
		t.Errorf("finalize before approval: %s", r)
	}
	if r := review("approve"); r.Code == 0 || !strings.Contains(r.Stderr, "not approved on GitHub") {
		t.Errorf("approve with no GitHub approval: %s", r)
	}
	if r := review("approve", "--by-hand"); r.Code != 0 {
		t.Fatalf("approve --by-hand:\n%s", r)
	}
	if prs := l.ReadFile(filepath.Join(l.Log, "gh", "prs.json")); !strings.Contains(prs, "in pages") || !strings.Contains(prs, "ais:plan-approved") {
		t.Errorf("the approved plan is not in the PR: %s", prs)
	}
	if r := review("finalize"); r.Code == 0 || !strings.Contains(r.Stderr, "nothing but the plan") {
		t.Errorf("finalize with only the plan: %s", r)
	}

	l.Commit(src, "export.go", "Stream exports in pages")
	if r := review("finalize"); r.Code != 0 {
		t.Fatalf("finalize:\n%s", r)
	}
	if log := l.Git(src, "log", "--format=%s", "origin/main..HEAD"); log != "Stream exports in pages" {
		t.Errorf("branch after finalize:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(src, "plans", "12.md")); err == nil {
		t.Errorf("the plan file is still there")
	}
	if l.Git(src, "rev-parse", "HEAD") != l.Git(src, "rev-parse", "origin/export-v2") {
		t.Errorf("finalize did not push")
	}
	if prs := l.ReadFile(filepath.Join(l.Log, "gh", "prs.json")); !strings.Contains(prs, `"isDraft": false`) {
		t.Errorf("the PR is still a draft: %s", prs)
	}
	if r := review("check"); r.Code != 0 {
		t.Errorf("check after finalize: %s", r)
	}
}

// ais flow done refuses while a plan is left on the session's branches.
func TestFlowDoneChecksPlans(t *testing.T) {
	l := flowLab(t)
	pid := l.queueAdd("--title", "Has a plan", "--tickets", "gpwsoi/backend#12", "--repos", "backend")
	l.MustFlow("dispatch")
	sid := l.waitRunning(pid)
	wt := filepath.Join(l.Session(sid).Cwd, "backend")
	plan := filepath.Join(l.Dir, "plan.md")
	l.WriteFile(plan, "# Plan: x\n")
	if r := l.AisIn(wt, []string{"AIS_FLOW_PROPOSAL=" + pid}, "", "flow", "review", "open", "--ticket", "gpwsoi/backend#12", "--file", plan); r.Code != 0 {
		t.Fatalf("open:\n%s", r)
	}
	if p := l.Proposals(); p[0].Phase != "plan" || p[0].PlanURL == "" {
		t.Errorf("phase not recorded: %+v", p)
	}
	if r := l.Flow("done", pid); r.Code == 0 || !strings.Contains(r.Stderr, "plan is left behind") {
		t.Errorf("done with a plan left: %s", r)
	}
	if r := l.Flow("done", pid, "--force"); r.Code != 0 {
		t.Errorf("done --force: %s", r)
	}
}

// Prompts are built in, take the workspace's instructions, and a full
// replacement written for an older built-in prompt is warned about.
func TestFlowPrompts(t *testing.T) {
	l := flowLab(t)
	extra := filepath.Join(l.Dir, "soi.md")
	l.WriteFile(extra, "ADRs go to gpwsoi/adr.")
	repl := filepath.Join(l.Dir, "planner.md")
	l.WriteFile(repl, "<!-- ais-prompt: planner v0 -->\nPlan {{.Workspace}} my way.\n")
	cfg := l.ReadFile(filepath.Join(l.AisHome, "config.toml"))[len(l.BaseConfig()):]
	l.WriteConfig(strings.Replace(cfg, "[workspace.soi]\n", "[workspace.soi]\ninstructions_file = \""+extra+"\"\n", 1) +
		"\n[workspace.soi.session]\ninstructions = \"Prefer small PRs.\"\n\n[workspace.soi.planner]\nprompt_file = \"" + repl + "\"\n")

	work := l.MustFlow("prompt", "show", "work", "--tickets", "gpwsoi/backend#12").Stdout
	for _, part := range []string{"# Work on gpwsoi/backend#12", "## Instructions for this workspace", "ADRs go to gpwsoi/adr.",
		"Prefer small PRs.", "| R7 | Risk |"} {
		if !strings.Contains(work, part) {
			t.Errorf("work prompt lacks %q", part)
		}
	}
	refine := l.MustFlow("prompt", "show", "refine", "--ticket", "gpwsoi/backend#12").Stdout
	if !strings.Contains(refine, "ADRs go to gpwsoi/adr.") || strings.Contains(refine, "Prefer small PRs.") {
		t.Errorf("refine prompt instructions:\n%s", refine)
	}
	r := l.MustFlow("prompt", "show", "planner")
	if strings.TrimSpace(r.Stdout) != "Plan soi my way." || !strings.Contains(r.Stderr, "built-in one is v1") {
		t.Errorf("replaced planner prompt:\n%s", r)
	}
	if b := l.MustFlow("prompt", "show", "planner", "--builtin").Stdout; !strings.HasPrefix(b, "<!-- ais-prompt: planner v1 -->") {
		t.Errorf("--builtin:\n%s", b)
	}
}
