package flow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
)

// startTimeout is how long a proposal may stay "starting" — typed into its
// pane, its session not yet recorded — before the next dispatch puts it back
// in the queue.
const startTimeout = 2 * time.Minute

func cmdDispatch(args []string) error {
	o, err := parseOpts(args, []string{"--now"}, []string{"--dry-run"}, map[string]string{"-n": "--dry-run"})
	if err != nil {
		return err
	}
	if len(o.pos) > 0 {
		return usagef("unexpected %q (to start one now: --now ID)", o.pos[0])
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	d := &dispatcher{cfg: cfg, db: db, dry: o.flags["--dry-run"], closed: map[string]bool{}}
	if id, ok := o.one("--now"); ok {
		return d.now(id)
	}
	return d.tick()
}

type dispatcher struct {
	cfg    *Config
	db     *DB
	dry    bool
	closed map[string]bool // ticket URL -> closed, asked once per run
}

// tick is one scheduled run: reconcile, then start what the limits allow.
func (d *dispatcher) tick() error {
	if err := d.reconcile(); err != nil {
		return err
	}
	started := 0
	for {
		p, why, err := d.next()
		if err != nil {
			return err
		}
		if p == nil {
			if started == 0 {
				note("nothing started: %s", why)
			}
			return nil
		}
		if err := d.start(p, false); err != nil {
			// One proposal that cannot start must not block the rest of
			// the queue for ever, nor stop this run.
			note("%s: %v", p.ID, err)
			logEvent(d.db, "start-failed", p.ID, err.Error())
			return nil
		}
		started++
		if d.dry {
			return nil
		}
	}
}

// reconcile puts back proposals whose start never arrived.
func (d *dispatcher) reconcile() error {
	stuck, err := proposals(d.db, `state = 'starting' AND started_at < ?`, stamp(now().Add(-startTimeout)))
	if err != nil {
		return err
	}
	for _, p := range stuck {
		if sid := ais.SessionForProposal(p.ID); sid != "" {
			// It did start; "ais flow start" just never got to say so.
			if err := d.markRunning(p, sid); err != nil {
				return err
			}
			continue
		}
		if d.dry {
			note("would put %s back in the queue: its session never started", p.ID)
			continue
		}
		d.db.Exec(`UPDATE proposals SET state='queued', started_at='' WHERE id=? AND state='starting'`, p.ID)
		logEvent(d.db, "start-timeout", p.ID, "its session never started; queued again")
		note("%s never started; it is queued again", p.ID)
	}
	return nil
}

// next is the proposal to start now, or nil and why not.
func (d *dispatcher) next() (*Proposal, string, error) {
	limits, _, err := effectiveLimits(d.db, d.cfg.Limits)
	if err != nil {
		return nil, "", err
	}
	all, err := countFor(d.db, "")
	if err != nil {
		return nil, "", err
	}
	if all.Running >= limits.WIP {
		return nil, fmt.Sprintf("%d of %d sessions running (wip)", all.Running, limits.WIP), nil
	}
	hour, err := startsSince(d.db, now().Add(-time.Hour))
	if err != nil {
		return nil, "", err
	}
	if hour >= limits.PerHour {
		return nil, fmt.Sprintf("%d started in the last hour (per_hour %d)", hour, limits.PerHour), nil
	}
	day, err := startsSince(d.db, now().Add(-24*time.Hour))
	if err != nil {
		return nil, "", err
	}
	if day >= limits.PerDay {
		return nil, fmt.Sprintf("%d started in the last 24h (per_day %d)", day, limits.PerDay), nil
	}
	queued, err := proposals(d.db, `state = 'queued'`)
	if err != nil {
		return nil, "", err
	}
	if len(queued) == 0 {
		return nil, "the queue is empty", nil
	}
	reasons := []string{}
	for _, p := range queued {
		w, ok := d.cfg.Workspaces[p.Workspace]
		if !ok {
			reasons = append(reasons, p.ID+": workspace "+p.Workspace+" is gone from config")
			continue
		}
		if w.WIP > 0 {
			c, err := countFor(d.db, w.Name)
			if err != nil {
				return nil, "", err
			}
			if c.Running >= w.WIP {
				reasons = append(reasons, fmt.Sprintf("%s: %s has %d of %d running", p.ID, w.Name, c.Running, w.WIP))
				continue
			}
		}
		waiting, err := d.waitingFor(p)
		if err != nil {
			return nil, "", err
		}
		if waiting != "" {
			reasons = append(reasons, p.ID+": after "+waiting)
			continue
		}
		return p, "", nil
	}
	return nil, strings.Join(reasons, "; "), nil
}

// waitingFor is the first of a proposal's "after" entries not yet met: a
// proposal that is not done (or cancelled), or a ticket still open.
func (d *dispatcher) waitingFor(p *Proposal) (string, error) {
	for _, ref := range p.After {
		if strings.HasPrefix(ref, "https://") {
			closed, err := d.ticketClosed(ref)
			if err != nil {
				return "", err
			}
			if !closed {
				return shortTickets(([]string{ref})), nil
			}
			continue
		}
		var state string
		err := d.db.QueryRow(`SELECT state FROM proposals WHERE id = ?`, ref).Scan(&state)
		if err == sql.ErrNoRows {
			continue // a proposal that no longer exists holds nothing up
		}
		if err != nil {
			return "", err
		}
		if state != "done" && state != "cancelled" {
			return ref + " (" + state + ")", nil
		}
	}
	return "", nil
}

func (d *dispatcher) ticketClosed(url string) (bool, error) {
	if v, ok := d.closed[url]; ok {
		return v, nil
	}
	out, err := ghRunner("issue", "view", url, "--json", "state")
	if err != nil {
		return false, err
	}
	var data struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return false, fmt.Errorf("gh issue view: %w", err)
	}
	d.closed[url] = data.State == "CLOSED"
	return d.closed[url], nil
}

// now starts one proposal at once: past the limits and its "after", but
// logged and counted like any other start.
func (d *dispatcher) now(ref string) error {
	p, err := findProposal(d.db, ref)
	if err != nil {
		return err
	}
	if p.State != "queued" {
		return fmt.Errorf("%s is %s; only a queued proposal can start", p.ID, p.State)
	}
	if waiting, err := d.waitingFor(p); err == nil && waiting != "" {
		note("%s was to wait for %s; starting it anyway", p.ID, waiting)
	}
	return d.start(p, true)
}

// start opens a window in the workspace's tmux session and types
// "ais flow start <id>" into its shell, which does the rest; the pane stays a
// shell after the agent exits, as one you started by hand would.
func (d *dispatcher) start(p *Proposal, manual bool) error {
	w, ok := d.cfg.Workspaces[p.Workspace]
	if !ok {
		return fmt.Errorf("workspace %s is gone from config", p.Workspace)
	}
	if err := d.checkTrust(p); err != nil {
		return err
	}
	command := shellJoin([]string{ais.Self(), "flow", "start", p.ID})
	if d.dry {
		fmt.Fprintf(stdout, "would start %s (%s) in tmux session %s: %s\n", p.ID, p.Title, w.TmuxSession, command)
		return nil
	}
	err := d.db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET state='starting', started_at=?, manual=? WHERE id=? AND state='queued'`,
			stamp(now()), manual, p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s stopped being queued while this ran", p.ID)
		}
		if _, err := tx.Exec(`INSERT INTO starts (at, proposal_id, manual) VALUES (?, ?, ?)`, stamp(now()), p.ID, manual); err != nil {
			return err
		}
		logEvent(tx, "dispatched", p.ID, map[string]any{"title": p.Title, "manual": manual, "tmux": w.TmuxSession})
		return nil
	})
	if err != nil {
		return err
	}
	pane, err := openPane(w.TmuxSession, home())
	if err == nil {
		err = typeInto(pane, command)
	}
	if err != nil {
		d.db.Exec(`UPDATE proposals SET state='queued', started_at='' WHERE id=? AND state='starting'`, p.ID)
		return fmt.Errorf("could not open its tmux window: %w", err)
	}
	note("started %s (%s) in %s", p.ID, p.Title, w.TmuxSession+":"+pane)
	fmt.Fprintln(stdout, p.ID)
	return nil
}

// checkTrust refuses to start claude where its folder-trust dialog would
// stop it before it reads its first prompt: a session directory under a
// worktree base Claude does not trust.
func (d *dispatcher) checkTrust(p *Proposal) error {
	cmd, err := ais.ProfileCommand(d.cfg.raw, p.Profile)
	if err != nil || len(cmd) == 0 || filepath.Base(cmd[0]) != "claude" {
		return err
	}
	base, err := ais.WorktreeBase(d.cfg.raw)
	if err != nil {
		return err
	}
	if !claudeTrusts(base) {
		return fmt.Errorf("Claude does not trust %s yet, so its trust dialog would stop the session; run claude there once and accept", base)
	}
	return nil
}

// claudeTrusts is whether Claude Code has accepted path or a folder above it.
func claudeTrusts(path string) bool {
	file := filepath.Join(home(), ".claude.json")
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		file = filepath.Join(dir, ".claude.json")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var state struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(data, &state) != nil {
		return false
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if state.Projects[p].Trusted {
			return true
		}
		if p == "/" || p == "." {
			return false
		}
	}
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// ---------------------------------------------------------------- tmux

func tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// openPane is a new shell window in the tmux session, made if missing.
func openPane(session, dir string) (string, error) {
	if _, err := tmux("has-session", "-t", "="+session); err != nil {
		return tmux("new-session", "-d", "-s", session, "-c", dir, "-P", "-F", "#{pane_id}")
	}
	return tmux("new-window", "-d", "-t", "="+session+":", "-c", dir, "-P", "-F", "#{pane_id}")
}

func typeInto(pane, line string) error {
	if _, err := tmux("send-keys", "-t", pane, "-l", line); err != nil {
		return err
	}
	_, err := tmux("send-keys", "-t", pane, "Enter")
	return err
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
