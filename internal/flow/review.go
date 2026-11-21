package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
)

// "ais flow review": a session's plan, reviewed as a draft pull request.
//
// The plan is committed alone, as plans/<ticket number>.md, in commits that
// carry an "Ais-Plan: <ticket>" trailer and touch nothing else. That is what
// lets finalize drop them without a conflict once the plan is approved and
// the branch holds the real work, and lets check find any that were left.

const reviewUsage = `usage: ais flow review <command> [--dir D] ...

  open --ticket REF --file F [--title T]
                 commit plan F alone as plans/<n>.md, push, open a draft PR,
                 link it on the ticket, and record that the session waits
                 for review
  update --file F
                 commit a revision of the plan, and push
  approve [--by-hand]
                 the gate: the PR is approved on GitHub, or you said so
                 (--by-hand). Copies the plan into the PR's description
  finalize       once the branch holds the work: drop the plan commits,
                 force-push, mark the PR ready for review
  check          fail if a plan commit or file is left on a branch (what
                 ais flow done runs)

--dir is the worktree (default: here). check without --dir looks at here, or
at every worktree under here (a session directory).
`

const (
	planTrailer  = "Ais-Plan"
	planMarker   = "<!-- ais:plan "          // the plan file's first line
	approvedMark = "<!-- ais:plan-approved " // in the PR description
)

func cmdReview(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, reviewUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "open":
		return cmdReviewOpen(rest)
	case "update":
		return cmdReviewUpdate(rest)
	case "approve":
		return cmdReviewApprove(rest)
	case "finalize":
		return cmdReviewFinalize(rest)
	case "check":
		return cmdReviewCheck(rest)
	}
	return usagef("unknown review command %q; try open, update, approve, finalize, check", sub)
}

// gitRunner runs git in dir; a variable for tests.
var gitRunner = func(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// ghIn runs gh in dir (for the pr commands, which read the repo from it).
func ghIn(dir, stdin string, args ...string) ([]byte, error) {
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer os.Chdir(cwd)
	return ghRunnerIn(stdin, args...)
}

// worktree is one checkout a review command acts on.
type worktree struct {
	dir    string
	branch string
	base   string // the commit the branch started from on the default branch
	remote string // origin/<default>
}

func openWorktree(dir string) (*worktree, error) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	top, err := gitRunner(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not in a git worktree (--dir)", dir)
	}
	wt := &worktree{dir: top}
	if wt.branch, err = gitRunner(top, "branch", "--show-current"); err != nil || wt.branch == "" {
		return nil, fmt.Errorf("%s is not on a branch", top)
	}
	wt.remote, _ = gitRunner(top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if wt.remote == "" {
		for _, c := range []string{"origin/main", "origin/master"} {
			if _, err := gitRunner(top, "rev-parse", "--verify", "--quiet", c); err == nil {
				wt.remote = c
				break
			}
		}
	}
	if wt.remote == "" {
		return nil, fmt.Errorf("%s: no origin default branch to compare with", top)
	}
	if wt.base, err = gitRunner(top, "merge-base", "HEAD", wt.remote); err != nil {
		return nil, err
	}
	return wt, nil
}

// planCommit is a commit on the branch carrying the plan trailer.
type planCommit struct{ sha, ticket string }

// commits is base..rev, oldest first, each with its plan ticket ("" for
// the real work).
func (wt *worktree) commits(rev string) ([]planCommit, error) {
	out, err := gitRunner(wt.dir, "log", "--reverse", "--format=%H%x00%(trailers:key="+planTrailer+",valueonly,separator=%x2C)", wt.base+".."+rev)
	if err != nil || out == "" {
		return nil, err
	}
	var list []planCommit
	for _, line := range strings.Split(out, "\n") {
		sha, ticket, _ := strings.Cut(line, "\x00")
		list = append(list, planCommit{sha: sha, ticket: strings.TrimSpace(ticket)})
	}
	return list, nil
}

// planTicket is the ticket the branch's plan is for, or "".
func (wt *worktree) planTicket() (string, error) {
	cs, err := wt.commits("HEAD")
	if err != nil {
		return "", err
	}
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].ticket != "" {
			return cs[i].ticket, nil
		}
	}
	return "", nil
}

func planPath(ticket string) string {
	return filepath.Join("plans", fmt.Sprintf("%d.md", numberOf(ticket)))
}

// commitPlan writes the plan and commits it, alone, with the trailer.
func (wt *worktree) commitPlan(ticket, title, text, subject string) error {
	rel := planPath(ticket)
	body := planMarker + ticket + " -->\n" + strings.TrimSpace(stripPlanMarker(text)) + "\n"
	path := filepath.Join(wt.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == body {
		return fmt.Errorf("the plan has not changed")
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	if _, err := gitRunner(wt.dir, "add", "--", rel); err != nil {
		return err
	}
	_, err := gitRunner(wt.dir, "commit", "--only", "-m", subject+": "+title, "-m", planTrailer+": "+ticket, "--", rel)
	return err
}

func stripPlanMarker(text string) string {
	if strings.HasPrefix(text, planMarker) {
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			return rest
		}
	}
	return text
}

func (wt *worktree) push(force bool) error {
	args := []string{"push", "-u", "origin", "HEAD"}
	if force {
		args = []string{"push", "--force-with-lease", "-u", "origin", "HEAD"}
	}
	_, err := gitRunner(wt.dir, args...)
	return err
}

// pr is the branch's pull request.
type pr struct {
	URL            string `json:"url"`
	Number         int    `json:"number"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	Body           string `json:"body"`
	ReviewDecision string `json:"reviewDecision"`
}

func (wt *worktree) pr() (*pr, error) {
	out, err := ghIn(wt.dir, "", "pr", "view", wt.branch, "--json", "url,number,state,isDraft,body,reviewDecision")
	if err != nil {
		if strings.Contains(err.Error(), "no pull requests found") {
			return nil, nil
		}
		return nil, err
	}
	var p pr
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("gh pr view: %w", err)
	}
	return &p, nil
}

// closingRef is how a PR in repo names ticket: #n at home, owner/repo#n
// from elsewhere.
func (wt *worktree) closingRef(ticket string) string {
	out, err := ghIn(wt.dir, "", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err == nil && strings.TrimSpace(string(out)) == repoOf(ticket) {
		return fmt.Sprintf("#%d", numberOf(ticket))
	}
	return shortTickets([]string{ticket})
}

// reviewProposal is the proposal of the session this runs in, or nil.
func reviewProposal(db *DB) *Proposal {
	id := os.Getenv("AIS_FLOW_PROPOSAL")
	if id == "" {
		return nil
	}
	p, err := findProposal(db, id)
	if err != nil {
		return nil
	}
	return p
}

func setPhase(db *DB, p *Proposal, phase, url string) {
	if p == nil {
		return
	}
	db.Exec(`UPDATE proposals SET phase = ?, plan_url = CASE WHEN ? = '' THEN plan_url ELSE ? END, phase_at = ? WHERE id = ?`,
		phase, url, url, stamp(now()), p.ID)
	logEvent(db, "phase-"+phase, p.ID, url)
}

func cmdReviewOpen(args []string) error {
	o, err := parseOpts(args, []string{"--dir", "--ticket", "--file", "--title", "--ws"}, nil, nil)
	if err != nil {
		return err
	}
	ref := o.str("--ticket")
	if ref == "" {
		return usagef("--ticket REF: the ticket the plan is for")
	}
	ticket, err := ticketURL(ref)
	if err != nil {
		return err
	}
	text, given, err := o.file("--file")
	if err != nil {
		return err
	}
	if !given || strings.TrimSpace(text) == "" {
		return usagef("--file F: the plan (\"-\" for stdin)")
	}
	wt, err := openWorktree(o.str("--dir"))
	if err != nil {
		return err
	}
	if existing, err := wt.planTicket(); err != nil {
		return err
	} else if existing != "" {
		return fmt.Errorf("this branch already has a plan for %s: ais flow review update", shortTickets([]string{existing}))
	}
	title := orDefault(strings.TrimSpace(o.str("--title")), planTitle(text))
	if err := wt.commitPlan(ticket, title, text, "Plan"); err != nil {
		return err
	}
	if err := wt.push(false); err != nil {
		return err
	}
	p, err := wt.pr()
	if err != nil {
		return err
	}
	url := ""
	if p != nil && p.State == "OPEN" {
		url = p.URL
	} else {
		cref := wt.closingRef(ticket)
		body := fmt.Sprintf("Part of %s\n\nThe plan for review is `%s` in this PR: comment on its lines, suggest changes, request changes or approve.\n\n"+
			"Once it is approved and the branch holds the work, `ais flow review finalize` removes the plan from the branch, keeps it in this description, and marks the PR ready.\n",
			cref, planPath(ticket))
		out, err := ghIn(wt.dir, body, "pr", "create", "--draft", "--title", "Plan: "+title, "--body-file", "-",
			"--base", strings.TrimPrefix(wt.remote, "origin/"), "--head", wt.branch)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(out))
		if len(fields) > 0 {
			url = fields[len(fields)-1]
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w, err := ticketWorkspace(cfg, o.str("--ws"))
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ticketSource(w, db).Comment(ticket, fmt.Sprintf("Plan up for review: %s", url)); err != nil {
		note("could not link the plan on the ticket: %v", err)
	}
	setPhase(db, reviewProposal(db), "plan", url)
	ais.Main([]string{"notify", "-q", fmt.Sprintf("plan for %s is up for review: %s", shortTickets([]string{ticket}), url)})
	fmt.Fprintln(stdout, url)
	return nil
}

// planTitle is the plan's "# Plan: <title>" heading, without the rest.
func planTitle(text string) string {
	for _, line := range strings.Split(stripPlanMarker(text), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			t = strings.TrimPrefix(t, "Plan: ")
			if i := strings.Index(t, "  ·"); i > 0 {
				t = t[:i]
			}
			return strings.TrimSpace(t)
		}
	}
	return "plan"
}

func cmdReviewUpdate(args []string) error {
	o, err := parseOpts(args, []string{"--dir", "--file"}, nil, nil)
	if err != nil {
		return err
	}
	text, given, err := o.file("--file")
	if err != nil {
		return err
	}
	if !given || strings.TrimSpace(text) == "" {
		return usagef("--file F: the revised plan (\"-\" for stdin)")
	}
	wt, err := openWorktree(o.str("--dir"))
	if err != nil {
		return err
	}
	ticket, err := wt.planTicket()
	if err != nil {
		return err
	}
	if ticket == "" {
		return fmt.Errorf("this branch has no plan yet: ais flow review open")
	}
	if p, err := wt.pr(); err == nil && p != nil && strings.Contains(p.Body, approvedMark) {
		return fmt.Errorf("the plan is approved already; a change now is a new plan to agree on, so say so in the PR")
	}
	if err := wt.commitPlan(ticket, planTitle(text), text, "Plan, revised"); err != nil {
		return err
	}
	if err := wt.push(false); err != nil {
		return err
	}
	note("revised the plan for %s", shortTickets([]string{ticket}))
	return nil
}

func cmdReviewApprove(args []string) error {
	o, err := parseOpts(args, []string{"--dir"}, []string{"--by-hand"}, nil)
	if err != nil {
		return err
	}
	wt, err := openWorktree(o.str("--dir"))
	if err != nil {
		return err
	}
	ticket, err := wt.planTicket()
	if err != nil {
		return err
	}
	if ticket == "" {
		return fmt.Errorf("this branch has no plan to approve")
	}
	p, err := wt.pr()
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("the plan has no pull request: ais flow review open makes one")
	}
	if strings.Contains(p.Body, approvedMark) {
		note("the plan is approved already")
		return nil
	}
	how := "on GitHub"
	if o.flags["--by-hand"] {
		how = "by hand"
	} else if p.ReviewDecision != "APPROVED" {
		return fmt.Errorf("%s is not approved on GitHub (review decision %q); when the user said so in the session, --by-hand", p.URL, orDefault(p.ReviewDecision, "none"))
	}
	plan, err := os.ReadFile(filepath.Join(wt.dir, planPath(ticket)))
	if err != nil {
		return err
	}
	day := now().Local().Format(time.DateOnly)
	body := strings.TrimRight(p.Body, "\n") + fmt.Sprintf("\n\n%s%s %s -->\n## Plan (approved %s, %s)\n\n%s\n",
		approvedMark, day, strings.ReplaceAll(how, " ", "-"), day, how, strings.TrimSpace(stripPlanMarker(string(plan))))
	if _, err := ghIn(wt.dir, body, "pr", "edit", p.URL, "--body-file", "-"); err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	setPhase(db, reviewProposal(db), "approved", p.URL)
	logEvent(db, "plan-approved", ticket, map[string]any{"pr": p.URL, "how": how})
	note("plan for %s approved (%s); do the work, then ais flow review finalize", shortTickets([]string{ticket}), how)
	return nil
}

func cmdReviewFinalize(args []string) error {
	o, err := parseOpts(args, []string{"--dir"}, nil, nil)
	if err != nil {
		return err
	}
	wt, err := openWorktree(o.str("--dir"))
	if err != nil {
		return err
	}
	cs, err := wt.commits("HEAD")
	if err != nil {
		return err
	}
	var keep, drop []planCommit
	for _, c := range cs {
		if c.ticket != "" {
			drop = append(drop, c)
		} else {
			keep = append(keep, c)
		}
	}
	if len(drop) == 0 {
		note("no plan commits on %s", wt.branch)
		return nil
	}
	p, err := wt.pr()
	if err != nil {
		return err
	}
	if p == nil || !strings.Contains(p.Body, approvedMark) {
		return fmt.Errorf("the plan is not approved yet (ais flow review approve)")
	}
	if len(keep) == 0 {
		// A head with nothing beyond its base makes GitHub mark the PR
		// merged, and a merged PR cannot be reopened.
		return fmt.Errorf("%s holds nothing but the plan; commit the work first, then finalize", wt.branch)
	}
	if merges, _ := gitRunner(wt.dir, "rev-list", "--merges", wt.base+"..HEAD"); merges != "" {
		return fmt.Errorf("%s has merge commits; rebase it onto %s first", wt.branch, wt.remote)
	}
	if out, _ := gitRunner(wt.dir, "status", "--porcelain", "--untracked-files=no"); out != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them first", wt.dir)
	}
	for _, c := range drop {
		files, err := gitRunner(wt.dir, "diff-tree", "--no-commit-id", "--name-only", "-r", c.sha)
		if err != nil {
			return err
		}
		for _, f := range strings.Fields(files) {
			if !strings.HasPrefix(f, "plans/") {
				return fmt.Errorf("plan commit %s also changes %s; it cannot be dropped cleanly", c.sha[:9], f)
			}
		}
	}
	head, _ := gitRunner(wt.dir, "rev-parse", "HEAD")
	if _, err := gitRunner(wt.dir, "checkout", "--quiet", "--detach", wt.base); err != nil {
		return err
	}
	restore := func() {
		gitRunner(wt.dir, "cherry-pick", "--abort")
		gitRunner(wt.dir, "checkout", "--quiet", "-B", wt.branch, head)
	}
	for _, c := range keep {
		if _, err := gitRunner(wt.dir, "cherry-pick", "--allow-empty", c.sha); err != nil {
			restore()
			return fmt.Errorf("replaying %s without the plan failed (%v); the branch is as it was", c.sha[:9], err)
		}
	}
	if _, err := gitRunner(wt.dir, "checkout", "--quiet", "-B", wt.branch); err != nil {
		restore()
		return err
	}
	if err := wt.push(true); err != nil {
		return fmt.Errorf("dropped the plan commits locally, but the push failed: %w (old head %s)", err, head[:9])
	}
	if p.IsDraft {
		if _, err := ghIn(wt.dir, "", "pr", "ready", p.URL); err != nil {
			note("could not mark %s ready: %v", p.URL, err)
		}
	}
	db, err := openDB()
	if err == nil {
		setPhase(db, reviewProposal(db), "finalized", p.URL)
		db.Close()
	}
	note("dropped %d plan commit(s) from %s (was %s); %s is ready for review", len(drop), wt.branch, head[:9], p.URL)
	return nil
}

func cmdReviewCheck(args []string) error {
	o, err := parseOpts(args, []string{"--dir"}, nil, nil)
	if err != nil {
		return err
	}
	dirs := o.list("--dir")
	if len(dirs) == 0 {
		here, _ := os.Getwd()
		dirs = []string{here}
	}
	problems, err := checkPlans(dirs)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("a plan is left behind:\n  %s", strings.Join(problems, "\n  "))
	}
	note("no plan left on %d worktree(s)", len(worktreesUnder(dirs)))
	return nil
}

// worktreesUnder is each dir that is a git worktree, else the worktrees
// directly inside it (a session directory holds one per repo).
func worktreesUnder(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			out = append(out, d)
			continue
		}
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			sub := filepath.Join(d, e.Name())
			if _, err := os.Stat(filepath.Join(sub, ".git")); err == nil && e.IsDir() {
				out = append(out, sub)
			}
		}
	}
	return out
}

// checkPlans lists what is left of plans in the worktrees: plan commits on
// the branch or its upstream, and plan files in the tree.
func checkPlans(dirs []string) ([]string, error) {
	var problems []string
	for _, d := range worktreesUnder(dirs) {
		wt, err := openWorktree(d)
		if err != nil {
			continue // not on a branch, or no origin: nothing a plan PR could be on
		}
		revs := []string{"HEAD"}
		if up, err := gitRunner(wt.dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && up != "" {
			revs = append(revs, up)
		}
		for _, rev := range revs {
			cs, err := wt.commits(rev)
			if err != nil {
				return nil, err
			}
			n := 0
			for _, c := range cs {
				if c.ticket != "" {
					n++
				}
			}
			if n > 0 {
				problems = append(problems, fmt.Sprintf("%s: %d plan commit(s) on %s; ais flow review finalize --dir %s", ais.Tilde(wt.dir), n, rev, ais.Tilde(wt.dir)))
			}
		}
		files, _ := gitRunner(wt.dir, "diff", "--name-only", wt.base, "HEAD", "--", "plans/")
		for _, f := range strings.Fields(files) {
			data, err := os.ReadFile(filepath.Join(wt.dir, f))
			if err == nil && strings.HasPrefix(string(data), planMarker) {
				problems = append(problems, fmt.Sprintf("%s: %s is still in the tree", ais.Tilde(wt.dir), f))
			}
		}
	}
	return problems, nil
}
