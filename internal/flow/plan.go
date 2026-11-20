package flow

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
	"github.com/i7c/ais/internal/api"
)

// Planner is one planner session's life: open until you decide (decided), it
// waits too long for you (expired), or its session goes away (aborted).
type Planner struct {
	ID         string
	Workspace  string
	AisSession string
	State      string
	StartedAt  string
	ClosedAt   string
	Reason     string
}

func scanPlanners(rows *sql.Rows, err error) ([]*Planner, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Planner
	for rows.Next() {
		var p Planner
		if err := rows.Scan(&p.ID, &p.Workspace, &p.AisSession, &p.State, &p.StartedAt, &p.ClosedAt, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

const plannerCols = `id, workspace, ais_session, state, started_at, closed_at, reason`

func planners(db *DB, where string, args ...any) ([]*Planner, error) {
	return scanPlanners(db.Query(`SELECT `+plannerCols+` FROM planners WHERE `+where+` ORDER BY started_at`, args...))
}

// plannerOfSession is the open planner a session is, or nil.
func plannerOfSession(db *DB, sid string) (*Planner, error) {
	if sid == "" {
		return nil, nil
	}
	found, err := planners(db, `ais_session = ? AND state = 'open'`, sid)
	if err != nil || len(found) == 0 {
		if err == nil && sid != "" {
			// Its session may not have been tied to it yet.
			if pid := plannerIDOfSession(sid); pid != "" {
				found, err = planners(db, `id = ? AND state = 'open'`, pid)
			}
		}
		if err != nil || len(found) == 0 {
			return nil, err
		}
	}
	return found[0], nil
}

func plannerIDOfSession(sid string) string {
	sessions, _ := ais.Sessions()
	for _, s := range sessions {
		if s.ID == sid {
			return s.FlowPlanner
		}
	}
	return ""
}

func sessionByID(sessions []api.Session, id string) *api.Session {
	for i := range sessions {
		if sessions[i].ID == id {
			return &sessions[i]
		}
	}
	return nil
}

func cmdPlan(args []string) error {
	if len(args) > 0 && args[0] == "done" {
		return cmdPlanDone(args[1:])
	}
	o, err := parseOpts(args, []string{"--ws"}, []string{"--dry-run"}, map[string]string{"-n": "--dry-run", "-w": "--ws"})
	if err != nil {
		return err
	}
	ws := o.str("--ws")
	if ws == "" && len(o.pos) == 1 {
		ws = o.pos[0]
	} else if len(o.pos) > 0 {
		return usagef("plan takes one workspace")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w, err := cfg.workspace(ws)
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	dry := o.flags["--dry-run"]
	if err := closeDecidedPlanners(db, cfg, dry); err != nil {
		return err
	}

	// 1. An open planner: gone, waited too long for you, or still waiting.
	open, err := planners(db, `workspace = ? AND state = 'open'`, w.Name)
	if err != nil {
		return err
	}
	if len(open) > 0 {
		pl := open[0]
		closed, err := settleOpenPlanner(db, w, pl, dry)
		if err != nil {
			return err
		}
		if !closed {
			note("planner %s is open since %s; waiting for you", pl.ID, ago(pl.StartedAt))
			return nil
		}
	}

	// 2. Anything to plan?
	items, err := sourceFor(w, db).Ready()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		note("no tickets marked %s; no planner needed", w.Source.Ready)
		return nil
	}
	if dry {
		fmt.Fprintf(stdout, "would start a planner for %s: %d ready ticket(s)\n", w.Name, len(items))
		return nil
	}

	// 3. Start one. The unique index makes a race between two of these
	// end with one planner, not two.
	pl := &Planner{ID: newID("n"), Workspace: w.Name, State: "open", StartedAt: stamp(now())}
	if _, err := db.Exec(`INSERT INTO planners (id, workspace, state, started_at) VALUES (?, ?, 'open', ?)`,
		pl.ID, pl.Workspace, pl.StartedAt); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			note("another planner for %s opened just now", w.Name)
			return nil
		}
		return err
	}
	logEvent(db, "planner-started", pl.ID, map[string]any{"workspace": w.Name, "ready": len(items)})
	pane, err := openPane(w.TmuxSession, home())
	if err == nil {
		err = typeInto(pane, shellJoin([]string{ais.Self(), "flow", "start", "--planner", pl.ID}))
	}
	if err != nil {
		db.Exec(`UPDATE planners SET state='aborted', closed_at=?, reason=? WHERE id=?`, stamp(now()), "no tmux window: "+err.Error(), pl.ID)
		return fmt.Errorf("could not open the planner's tmux window: %w", err)
	}
	note("started planner %s for %s (%d ready ticket(s)) in %s", pl.ID, w.Name, len(items), w.TmuxSession+":"+pane)
	fmt.Fprintln(stdout, pl.ID)
	return nil
}

// settleOpenPlanner closes an open planner whose session is gone (aborted)
// or that has waited longer than idle_timeout for you (expired), and says
// whether it did. Waiting is counted from your last prompt to it, or from
// its start when you never sent one.
func settleOpenPlanner(db *DB, w *Workspace, pl *Planner, dry bool) (bool, error) {
	sessions, err := ais.Sessions()
	if err != nil {
		return false, err
	}
	sid := pl.AisSession
	if sid == "" {
		sid = ais.SessionForPlanner(pl.ID)
	}
	s := sessionByID(sessions, sid)
	close := func(state, reason string) (bool, error) {
		if dry {
			fmt.Fprintf(stdout, "would close planner %s (%s): %s\n", pl.ID, state, reason)
			return true, nil
		}
		if s != nil && s.Status == "active" && s.Tmux.PaneID != "" {
			tmux("kill-pane", "-t", s.Tmux.PaneID)
		}
		db.Exec(`UPDATE planners SET state=?, closed_at=?, reason=? WHERE id=? AND state='open'`, state, stamp(now()), reason, pl.ID)
		logEvent(db, "planner-"+state, pl.ID, reason)
		note("closed planner %s (%s): %s", pl.ID, state, reason)
		return true, nil
	}
	switch {
	case s == nil:
		started, _ := parseStamp(pl.StartedAt)
		if now().Sub(started) < startTimeout {
			return false, nil // still starting
		}
		return close("aborted", "its session never started")
	case s.Status != "active":
		return close("aborted", "its session ended without a decision")
	}
	since := pl.StartedAt
	if s.LastInputAt != "" {
		since = s.LastInputAt
	}
	if t, ok := parseStamp(since); ok && now().Sub(t) >= w.Planner.IdleTimeout {
		return close("expired", fmt.Sprintf("no input for %s", w.Planner.IdleTimeout))
	}
	return false, nil
}

// closeDecidedPlanners closes the windows of planners you decided in, once
// you have left them alone for close_after: the last message stays readable
// for a while, and typing on keeps the window.
func closeDecidedPlanners(db *DB, cfg *Config, dry bool) error {
	decided, err := planners(db, `state = 'decided' AND reason NOT LIKE '%window closed%'`)
	if err != nil || len(decided) == 0 {
		return err
	}
	sessions, err := ais.Sessions()
	if err != nil {
		return err
	}
	for _, pl := range decided {
		s := sessionByID(sessions, pl.AisSession)
		mark := func() {
			db.Exec(`UPDATE planners SET reason = reason || ' (window closed)' WHERE id = ?`, pl.ID)
		}
		if s == nil || s.Status != "active" {
			if !dry {
				mark()
			}
			continue
		}
		grace := 5 * time.Minute
		if w, ok := cfg.Workspaces[pl.Workspace]; ok {
			grace = w.Planner.CloseAfter
		}
		t, ok := parseStamp(pl.ClosedAt)
		if input, seen := parseStamp(s.LastInputAt); seen && input.After(t) {
			t = input // you typed on after deciding: the clock starts again
		}
		if !ok || now().Sub(t) < grace {
			continue
		}
		if dry {
			fmt.Fprintf(stdout, "would close the window of decided planner %s\n", pl.ID)
			continue
		}
		tmux("kill-pane", "-t", s.Tmux.PaneID)
		mark()
		logEvent(db, "planner-window-closed", pl.ID, "")
	}
	return nil
}

// cmdPlanDone records that you decided: the planner is finished, and the
// next scheduled run may start a fresh one. Inside the planner session it
// needs no argument; elsewhere, name the planner or its workspace.
func cmdPlanDone(args []string) error {
	o, err := parseOpts(args, []string{"--reason"}, nil, nil)
	if err != nil {
		return err
	}
	if len(o.pos) > 1 {
		return usagef("plan done takes at most one planner id or workspace")
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	var pl *Planner
	if len(o.pos) == 1 {
		found, err := planners(db, `state = 'open' AND (workspace = ? OR id LIKE ? || '%')`, o.pos[0], o.pos[0])
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return fmt.Errorf("no single open planner matches %q", o.pos[0])
		}
		pl = found[0]
	} else if pl, err = plannerOfSession(db, os.Getenv("AIS_SESSION")); err != nil {
		return err
	} else if pl == nil {
		return usagef("this is not an open planner's session; name the planner or its workspace")
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM proposals WHERE planner_id = ?`, pl.ID).Scan(&queued)
	reason := orDefault(o.str("--reason"), fmt.Sprintf("%d proposal(s) queued", queued))
	res, err := db.Exec(`UPDATE planners SET state='decided', closed_at=?, reason=?,
		ais_session = CASE WHEN ais_session = '' THEN ? ELSE ais_session END WHERE id=? AND state='open'`,
		stamp(now()), reason, os.Getenv("AIS_SESSION"), pl.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("planner %s is no longer open", pl.ID)
	}
	logEvent(db, "planner-decided", pl.ID, reason)
	note("planner %s done: %s; its window closes once you leave it alone", pl.ID, reason)
	return nil
}

// cmdStartPlanner is what a planner's pane runs: the planner session,
// through ais, in the planner's directory, with the planner prompt.
func cmdStartPlanner(id string) (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 1, err
	}
	db, err := openDB()
	if err != nil {
		return 1, err
	}
	found, err := planners(db, `id = ? AND state = 'open'`, id)
	db.Close()
	if err != nil {
		return 1, err
	}
	if len(found) == 0 {
		return 1, fmt.Errorf("no open planner %s", id)
	}
	pl := found[0]
	w, err := cfg.workspace(pl.Workspace)
	if err != nil {
		return 1, err
	}
	for k, v := range w.Env {
		os.Setenv(k, v)
	}
	os.Setenv("AIS_FLOW_PLANNER", pl.ID)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(startTimeout)
		for time.Now().Before(deadline) {
			if sid := ais.SessionForPlanner(pl.ID); sid != "" {
				if db, err := openDB(); err == nil {
					db.Exec(`UPDATE planners SET ais_session = ? WHERE id = ?`, sid, pl.ID)
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
	cwd := orDefault(w.Planner.Cwd, home())
	code := ais.Main([]string{"run", w.Planner.Profile, "--no-worktree", "--cwd", cwd,
		"-d", "Plan the next sessions for " + w.Name, "--prompt", w.Planner.Prompt})
	select {
	case <-done:
	case <-time.After(time.Second):
		close(stop)
		<-done
	}
	return code, nil
}

// cmdWorkspace describes workspaces: what a planner needs to propose
// sessions (the repos it may name, the profile, the statuses).
func cmdWorkspace(args []string) error {
	o, err := parseOpts(args, nil, []string{"--json"}, nil)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var list []*Workspace
	if len(o.pos) == 1 {
		w, err := cfg.workspace(o.pos[0])
		if err != nil {
			return err
		}
		list = []*Workspace{w}
	} else {
		for _, w := range cfg.Workspaces {
			list = append(list, w)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	type out struct {
		Name     string            `json:"name"`
		Profile  string            `json:"profile"`
		Tmux     string            `json:"tmux_session"`
		Repos    map[string]string `json:"repos"`
		Source   string            `json:"source"`
		Statuses map[string]string `json:"statuses"`
		WIP      int               `json:"wip"`
	}
	var outs []out
	for _, w := range list {
		outs = append(outs, out{w.Name, w.Profile, w.TmuxSession, w.Repos,
			fmt.Sprintf("github.com/orgs/%s/projects/%d", w.Source.Owner, w.Source.Project),
			map[string]string{"ready": w.Source.Ready, "scheduled": w.Source.Scheduled, "in_progress": w.Source.InProgress}, w.WIP})
	}
	if o.flags["--json"] {
		return writeJSON(outs)
	}
	for _, w := range outs {
		fmt.Fprintf(stdout, "%s  (profile %s, tmux session %s, %s)\n", w.Name, w.Profile, w.Tmux, w.Source)
		for _, name := range repoNames(&Workspace{Repos: w.Repos}) {
			fmt.Fprintf(stdout, "  %-18s %s\n", name, w.Repos[name])
		}
	}
	return nil
}
