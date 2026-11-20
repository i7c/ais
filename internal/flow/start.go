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

	// The session is recorded a moment after ais starts; watch for it, and
	// tell the queue and the board as soon as it is there.
	marked := make(chan struct{})
	stop := make(chan struct{})
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
	code := ais.Main([]string{"run", p.Profile, "-d", p.Title, "--prompt", p.Prompt, "--repos", strings.Join(paths, ",")})
	// A session can end before the watcher saw it (a failed start): look
	// once more, then stop watching.
	select {
	case <-marked:
	case <-time.After(time.Second):
		close(stop)
		<-marked
	}
	return code, nil
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
	items, err := src.Find(p.Tickets)
	if err == nil {
		err = src.SetStatus(items, w.Source.InProgress)
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
	o, err := parseOpts(args, nil, nil, nil)
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
	return nil
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
