package flow

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/i7c/ais/internal/ais"
)

// cmdCancel abandons a running session's work: unlike done, nothing was
// finished. Its tickets go back to the backlog, still open, and a branch that
// holds only a plan is cleared, so that a later session for the same tickets
// starts from the default branch rather than from this one's plan.
func cmdCancel(args []string) error {
	o, err := parseOpts(args, []string{"--reason"}, nil, nil)
	if err != nil {
		return err
	}
	if len(o.pos) > 1 {
		return usagef("cancel takes at most one proposal or session id")
	}
	ref := os.Getenv("AIS_SESSION")
	if len(o.pos) == 1 {
		ref = o.pos[0]
	}
	if ref == "" {
		return usagef("name the proposal (or its session), or run this inside the session")
	}
	reason := strings.TrimSpace(o.str("--reason"))
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := proposalFor(db, ref)
	if err != nil {
		return err
	}
	switch p.State {
	case "running", "starting":
	case "queued":
		return fmt.Errorf("%s is only queued: ais flow queue cancel %s", p.ID, p.ID)
	default:
		return fmt.Errorf("%s is %s", p.ID, p.State)
	}
	w, err := cfg.workspace(p.Workspace)
	if err != nil {
		return err
	}
	src := sourceFor(w, db)
	work := p.Kind != "refine" // a refinement run never moved its ticket
	if work {
		ok, err := src.HasStatus(w.Source.Backlog)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("the board has no %q status for the tickets to go back to (source.backlog)", w.Source.Backlog)
		}
	}
	dirs := sessionDirs(p.ID)
	err = db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET state='cancelled', done_at=? WHERE id=? AND state IN ('running','starting')`, stamp(now()), p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s changed while this ran", p.ID)
		}
		logEvent(tx, "cancelled", p.ID, reason)
		return nil
	})
	if err != nil {
		return err
	}
	note("cancelled %s (%s); its slot is free", p.ID, p.Title)

	// From here on the proposal is cancelled whatever happens; each step
	// says what it could not do, and the rest still runs.
	var failed []string
	if work {
		moved, err := backToBacklog(src, w, p, reason)
		if err != nil {
			failed = append(failed, err.Error())
		}
		if len(moved) > 0 {
			note("%s back in %q", shortTickets(moved), w.Source.Backlog)
		}
	}
	for _, d := range worktreesUnder(dirs) {
		if err := clearPlanBranch(d, p, reason); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("cancelled %s, but:\n  %s", p.ID, strings.Join(failed, "\n  "))
	}
	return nil
}

// backToBacklog moves the proposal's tickets that are still where flow put
// them; one somebody moved on by hand is theirs to keep.
func backToBacklog(src Source, w *Workspace, p *Proposal, reason string) ([]string, error) {
	items, err := src.Find(p.Tickets)
	if err != nil {
		return nil, fmt.Errorf("could not read its tickets back: %w", err)
	}
	var back []Item
	var urls []string
	for _, it := range items {
		if it.Status == w.Source.InProgress || it.Status == w.Source.Scheduled {
			back = append(back, it)
			urls = append(urls, it.URL)
		}
	}
	if err := src.SetStatus(back, w.Source.Backlog); err != nil {
		return nil, err
	}
	if w.Source.Comment {
		msg := fmt.Sprintf("Session cancelled by `ais flow` (%s): %s. Back in %q; nothing of it was merged.", p.ID, p.Title, w.Source.Backlog)
		if reason != "" {
			msg += "\n\nReason: " + reason
		}
		for _, u := range urls {
			if err := src.Comment(u, msg); err != nil {
				note("could not comment on %s: %v", shortTickets([]string{u}), err)
			}
		}
	}
	return urls, nil
}

// clearPlanBranch closes the plan PR of a branch that holds nothing but the
// plan, deletes the branch on the remote, and drops the plan commits here.
// GitHub keeps a closed PR's commits, so the plan stays readable there. A
// branch with real work, or with uncommitted changes, is left as it is.
func clearPlanBranch(dir string, p *Proposal, reason string) error {
	wt, err := openWorktree(dir)
	if err != nil {
		return nil // not on a branch, or no origin: no plan PR to clear
	}
	cs, err := wt.commits("HEAD")
	if err != nil {
		return err
	}
	plans := 0
	for _, c := range cs {
		if c.ticket != "" {
			plans++
		}
	}
	if plans == 0 {
		return nil
	}
	if plans != len(cs) {
		return fmt.Errorf("%s: %s holds work besides the plan; left as it is, with its PR", ais.Tilde(wt.dir), wt.branch)
	}
	if out, _ := gitRunner(wt.dir, "status", "--porcelain"); out != "" {
		return fmt.Errorf("%s has uncommitted changes; %s and its PR are left as they are", ais.Tilde(wt.dir), wt.branch)
	}
	pr, err := wt.pr()
	if err != nil {
		return err
	}
	if pr != nil && pr.State == "OPEN" {
		msg := fmt.Sprintf("Closed by `ais flow cancel` (%s): the work was not started. The plan stays readable in this PR's commits.", p.ID)
		if reason != "" {
			msg += "\n\nReason: " + reason
		}
		if _, err := ghIn(wt.dir, "", "pr", "close", pr.URL, "--comment", msg); err != nil {
			return fmt.Errorf("%s: could not close %s: %w", ais.Tilde(wt.dir), pr.URL, err)
		}
		note("closed %s", pr.URL)
	}
	if _, err := gitRunner(wt.dir, "ls-remote", "--exit-code", "--heads", "origin", wt.branch); err == nil {
		if _, err := gitRunner(wt.dir, "push", "origin", "--delete", wt.branch); err != nil {
			return fmt.Errorf("%s: could not delete origin/%s: %w", ais.Tilde(wt.dir), wt.branch, err)
		}
	}
	gitRunner(wt.dir, "branch", "--unset-upstream")
	if _, err := gitRunner(wt.dir, "checkout", "--quiet", "-B", wt.branch, wt.base); err != nil {
		return fmt.Errorf("%s: could not drop the plan commits: %w", ais.Tilde(wt.dir), err)
	}
	note("%s: %s is back at %s, without the plan", ais.Tilde(wt.dir), wt.branch, wt.base[:9])
	return nil
}
