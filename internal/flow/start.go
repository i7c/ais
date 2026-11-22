package flow

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
)

// cmdStart is what a dispatched pane runs: the proposal's session, through
// ais itself, so it is an ordinary ais session in every way — with the first
// prompt, a worktree of each of its repos, and the proposal recorded on it.
// It returns when the session ends, with its exit code.
func cmdStart(args []string) (int, error) {
	if len(args) != 1 {
		return 2, usagef("start takes one proposal id (dispatch types it; you want dispatch --now)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return 1, err
	}
	db, err := openDB()
	if err != nil {
		return 1, err
	}
	p, err := findProposal(db, args[0])
	if err != nil {
		db.Close()
		return 1, err
	}
	if p.State != "starting" {
		db.Close()
		return 1, fmt.Errorf("%s is %s, not waiting to start; ais flow dispatch --now %s starts a queued one", p.ID, p.State, p.ID)
	}
	w, err := cfg.workspace(p.Workspace)
	if err != nil {
		db.Close()
		return 1, err
	}
	if p.Kind == "refine" {
		db.Close()
		return startRefine(cfg, w, p)
	}
	// A prompt from the workspace is made now, not when it was queued: it
	// picks up what refinement found since, and a newer built-in prompt.
	if !p.CustomPrompt {
		text, warning, err := renderRole(w, "work", workData(w, db, p))
		if warning != "" {
			note("%s", warning)
		}
		if err != nil {
			db.Close()
			return 1, err
		}
		p.Prompt = text
		db.Exec(`UPDATE proposals SET prompt = ? WHERE id = ?`, text, p.ID)
	}
	var paths []string
	for _, name := range p.Repos {
		path, ok := w.Repos[name]
		if !ok {
			db.Close()
			return 1, fmt.Errorf("workspace %s has no repo %q any more", w.Name, name)
		}
		paths = append(paths, path)
	}
	db.Close()

	for k, v := range w.Env {
		os.Setenv(k, v)
	}
	os.Setenv("AIS_FLOW_PROPOSAL", p.ID)
	os.Setenv("AIS_FLOW_WORKSPACE", w.Name)

	// The session is recorded a moment after ais starts; watch for it, and
	// tell the queue and the board as soon as it is there.
	marked, stop := watchStart(cfg, p)
	code := ais.Main([]string{"run", p.Profile, "-d", p.Title, "--prompt", p.Prompt, "--repos", strings.Join(paths, ",")})
	settleWatch(marked, stop)
	return code, nil
}

// watchStart ties the proposal to its session as soon as ais has recorded
// it, and tells the queue and the board.
func watchStart(cfg *Config, p *Proposal) (marked, stop chan struct{}) {
	marked, stop = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(marked)
		deadline := time.Now().Add(startTimeout)
		for time.Now().Before(deadline) {
			if sid := ais.SessionForProposal(p.ID); sid != "" {
				if db, err := openDB(); err == nil {
					d := &dispatcher{cfg: cfg, db: db}
					if err := d.markRunning(p, sid); err != nil {
						note("%s: %v", p.ID, err)
					}
					db.Close()
				}
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return marked, stop
}

// settleWatch waits for the watcher: a session can end before it saw it (a
// failed start), so look once more, then stop watching.
func settleWatch(marked, stop chan struct{}) {
	select {
	case <-marked:
	case <-time.After(time.Second):
		close(stop)
		<-marked
	}
}

// startRefine is a refinement run: headless, in the refine directory, no
// worktree. It holds a slot only while it runs: when the harness exits, the
// proposal is done, and a run that recorded no outcome holds its ticket back
// for refine.retry so a broken run cannot spin.
func startRefine(cfg *Config, w *Workspace, p *Proposal) (int, error) {
	if len(p.Tickets) != 1 {
		return 1, fmt.Errorf("%s: a refinement run is for one ticket", p.ID)
	}
	text, warning, err := renderRole(w, "refine", promptData{Ticket: p.Tickets[0], Repos: workspaceRepos(w)})
	if warning != "" {
		note("%s", warning)
	}
	if err != nil {
		return 1, err
	}
	for k, v := range w.Env {
		os.Setenv(k, v)
	}
	os.Setenv("AIS_FLOW_PROPOSAL", p.ID)
	os.Setenv("AIS_FLOW_WORKSPACE", w.Name)
	marked, stop := watchStart(cfg, p)
	cwd := orDefault(w.Refine.Cwd, home())
	code := ais.Main([]string{"run", p.Profile, "--no-worktree", "--cwd", cwd, "-d", p.Title, "--headless", "--prompt", text})
	settleWatch(marked, stop)
	finishRefine(cfg, w, p, code)
	return code, nil
}

// finishRefine frees a refinement run's slot and checks it left an outcome.
func finishRefine(cfg *Config, w *Workspace, p *Proposal, code int) {
	db, err := openDB()
	if err != nil {
		note("%s: %v", p.ID, err)
		return
	}
	defer db.Close()
	var started string
	db.QueryRow(`SELECT started_at FROM proposals WHERE id = ?`, p.ID).Scan(&started)
	a, _ := assessmentOf(db, p.Tickets[0])
	recorded := a != nil && a.Level != "" && a.At >= started
	db.Exec(`UPDATE proposals SET state='done', done_at=? WHERE id=? AND state IN ('running','starting')`, stamp(now()), p.ID)
	if recorded {
		logEvent(db, "done", p.ID, map[string]any{"refined": p.Tickets[0], "level": a.Level, "exit": code})
		note("%s done: %s assessed %s", p.ID, shortTickets(p.Tickets), a.Level)
	} else {
		db.Exec(`INSERT INTO assessments (url, workspace, level, version, at, failed_at) VALUES (?, ?, '', '', ?, ?)
			ON CONFLICT(url) DO UPDATE SET failed_at = excluded.failed_at`, p.Tickets[0], w.Name, stamp(now()), stamp(now()))
		logEvent(db, "refine-failed", p.ID, map[string]any{"ticket": p.Tickets[0], "exit": code})
		note("%s ended (exit %d) without assessing %s; it is tried again after %s", p.ID, code, shortTickets(p.Tickets), w.Refine.Retry)
		ais.Main([]string{"notify", "-q", fmt.Sprintf("%s: refinement of %s ended without an outcome (exit %d); ais flow log", w.Name, shortTickets(p.Tickets), code)})
	}
	// An assessed ticket may be what a planner was waiting for.
	if recorded && a.Level != "L1" {
		if err := planWorkspace(db, cfg, w, planAfterDone, false); err != nil {
			note("could not start a planner: %v", err)
		}
	} else if err := endWait(db, cfg, w, false); err != nil {
		// One may be waiting for this run, whatever came of it.
		note("could not start a planner: %v", err)
	}
}

// markRunning ties a proposal to its session, and moves its tickets on —
// tickets first, so whoever sees "running" sees the board agree. Doing it
// twice (the start's watcher and a dispatch both noticing) is harmless.
func (d *dispatcher) markRunning(p *Proposal, sid string) error {
	var state string
	if err := d.db.QueryRow(`SELECT state FROM proposals WHERE id = ?`, p.ID).Scan(&state); err != nil || state != "starting" {
		return err
	}
	w, err := d.cfg.workspace(p.Workspace)
	if err != nil {
		return err
	}
	src := sourceFor(w, d.db)
	var items []Item
	if p.Kind != "refine" { // a ticket being refined stays where it is, held
		items, err = src.Find(p.Tickets)
		if err == nil {
			err = src.SetStatus(items, w.Source.InProgress)
		}
	}
	if err != nil {
		// The session runs either way; the board is told what it can be.
		logEvent(d.db, "status-failed", p.ID, err.Error())
		note("%s: its session runs, but its tickets were not moved to %q: %v", p.ID, w.Source.InProgress, err)
	}
	return d.db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET state='running', ais_session=? WHERE id=? AND state='starting'`, sid, p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			logEvent(tx, "running", p.ID, map[string]any{"session": sid})
		}
		return nil
	})
}

// cmdDone marks a proposal done, which is the only thing that frees its
// slot. With no argument: the proposal of the session this runs in.
func cmdDone(args []string) error {
	o, err := parseOpts(args, nil, []string{"--force"}, nil)
	if err != nil {
		return err
	}
	if len(o.pos) > 1 {
		return usagef("done takes at most one proposal or session id")
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	ref := os.Getenv("AIS_SESSION")
	if len(o.pos) == 1 {
		ref = o.pos[0]
	}
	if ref == "" {
		return usagef("name the proposal (or its session), or run this inside the session")
	}
	p, err := proposalFor(db, ref)
	if err != nil {
		return err
	}
	if p.State != "running" && p.State != "starting" {
		hint := ""
		if p.State == "queued" {
			hint = "; ais flow queue cancel drops a queued one"
		}
		return fmt.Errorf("%s is %s%s", p.ID, p.State, hint)
	}
	// The plan must not ship: a session is not done while one is left on
	// any of its branches.
	if dirs := sessionDirs(p.ID); len(dirs) > 0 && !o.flags["--force"] {
		problems, err := checkPlans(dirs)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return fmt.Errorf("%s is not done: a plan is left behind (--force marks it done anyway)\n  %s", p.ID, strings.Join(problems, "\n  "))
		}
	}
	err = db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET state='done', done_at=? WHERE id=? AND state IN ('running','starting')`, stamp(now()), p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s changed while this ran", p.ID)
		}
		logEvent(tx, "done", p.ID, p.Title)
		return nil
	})
	if err != nil {
		return err
	}
	note("%s done (%s); its slot is free", p.ID, p.Title)
	// A free slot is a reason to plan: if there is work and room, the
	// planner opens now rather than at the next scheduled check.
	if cfg, err := loadConfig(); err == nil {
		if w, err := cfg.workspace(p.Workspace); err == nil {
			if err := planWorkspace(db, cfg, w, planAfterDone, false); err != nil {
				note("could not start a planner: %v", err)
			}
		}
	}
	return nil
}

// sessionDirs is where a proposal's sessions worked: their session
// directories (or worktrees), restarts included.
func sessionDirs(proposal string) []string {
	sessions, err := ais.Sessions()
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range sessions {
		if s.FlowProposal != proposal {
			continue
		}
		dir := orDefault(s.Worktree.Base, s.Cwd)
		if dir != "" && !contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}

// proposalFor finds a proposal by its session's id (the session it was
// started in, or a restart of it), else by its own id or a prefix of it.
func proposalFor(db *DB, ref string) (*Proposal, error) {
	if found, err := proposals(db, `ais_session = ?`, ref); err != nil {
		return nil, err
	} else if len(found) > 0 {
		return found[len(found)-1], nil
	}
	if pid := proposalOfSession(ref); pid != "" {
		return findProposal(db, pid)
	}
	p, err := findProposal(db, ref)
	if err != nil {
		return nil, fmt.Errorf("%s is neither a proposal nor a session ais flow started", ref)
	}
	return p, nil
}

// proposalOfSession is the flow proposal recorded on an ais session (a
// restart of a dispatched session carries it too).
func proposalOfSession(sid string) string {
	sessions, err := ais.Sessions()
	if err != nil {
		return ""
	}
	for _, s := range sessions {
		if s.ID == sid {
			return s.FlowProposal
		}
	}
	return ""
}
