package flow

import (
	"fmt"

	"github.com/i7c/ais/internal/ais"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

// counts is what a workspace (or all of them) has going.
type counts struct {
	Queued, Running int
	Planner         string // the open planner's id, if any
	PlannerSince    string
}

// countFor counts a workspace's proposals ("": all of them) of one kind
// ("": every kind). Refinement runs count toward the global wip, but not a
// workspace's (they have refine.wip), nor toward room for a planner.
// Planners are not proposals; live adds them for the global wip.
func countFor(db *DB, ws string, kind ...string) (counts, error) {
	var c counts
	q := `SELECT COUNT(*) FROM proposals WHERE state = ?`
	args := func(state string) []any {
		out := []any{state}
		if ws != "" {
			out = append(out, ws)
		}
		if len(kind) > 0 {
			out = append(out, kind[0])
		}
		return out
	}
	if ws != "" {
		q += ` AND workspace = ?`
	}
	if len(kind) > 0 {
		q += ` AND kind = ?`
	}
	if err := db.QueryRow(q, args("queued")...).Scan(&c.Queued); err != nil {
		return c, err
	}
	var running, starting int
	if err := db.QueryRow(q, args("running")...).Scan(&running); err != nil {
		return c, err
	}
	if err := db.QueryRow(q, args("starting")...).Scan(&starting); err != nil {
		return c, err
	}
	c.Running = running + starting
	if ws != "" {
		err := db.QueryRow(`SELECT id, started_at FROM planners WHERE workspace = ? AND state = 'open'`, ws).Scan(&c.Planner, &c.PlannerSince)
		if err != nil && err.Error() != "sql: no rows in result set" {
			return c, err
		}
	}
	return c, nil
}

// startsSince counts the starts (automatic and manual alike) since t of one
// kind of proposal: work and refinement runs each have their own rate.
func startsSince(db *DB, t time.Time, kind string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM starts s LEFT JOIN proposals p ON p.id = s.proposal_id
		WHERE s.at > ? AND COALESCE(p.kind, 'work') = ?`, stamp(t), kind).Scan(&n)
	return n, err
}

// openPlanners counts the planner sessions open now, in every workspace:
// they are sessions too, and count toward the global wip.
func openPlanners(db *DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM planners WHERE state = 'open'`).Scan(&n)
	return n, err
}

// live is everything flow has going now, for the global wip: work sessions,
// refinement runs and open planners.
func live(db *DB) (int, error) {
	all, err := countFor(db, "")
	if err != nil {
		return 0, err
	}
	planners, err := openPlanners(db)
	return all.Running + planners, err
}

func cmdLs(args []string) error {
	if _, err := parseOpts(args, nil, nil, nil); err != nil {
		return err
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
	limits, _, err := effectiveLimits(db, cfg.Limits)
	if err != nil {
		return err
	}
	var names []string
	for n := range cfg.Workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		note("no workspaces configured; add a [workspace.<name>] table to config.toml")
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKSPACE\tPLANNER\tQUEUED\tRUNNING\tSOURCE")
	for _, n := range names {
		w := cfg.Workspaces[n]
		c, err := countFor(db, n, "work")
		if err != nil {
			return err
		}
		planner := "-"
		if c.Planner != "" {
			planner = c.Planner + " since " + ago(c.PlannerSince)
		}
		running := strconv.Itoa(c.Running)
		if w.WIP > 0 {
			running += "/" + strconv.Itoa(w.WIP)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s/%d %s\n", n, planner, c.Queued, running, w.Source.Owner, w.Source.Project, w.Source.Field)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	running, err := live(db)
	if err != nil {
		return err
	}
	var used [4]int
	for i, q := range []struct {
		span time.Duration
		kind string
	}{{time.Hour, "work"}, {24 * time.Hour, "work"}, {time.Hour, "refine"}, {24 * time.Hour, "refine"}} {
		if used[i], err = startsSince(db, now().Add(-q.span), q.kind); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "\nrunning %d/%d (work sessions, refinement runs and planners)\n"+
		"work started in the last hour %d/%d, last 24h %d/%d\n"+
		"refinement started in the last hour %d/%d, last 24h %d/%d\n",
		running, limits.WIP, used[0], limits.PerHour, used[1], limits.PerDay, used[2], limits.RefinePerHour, used[3], limits.RefinePerDay)
	if err := listRunning(db); err != nil {
		return err
	}
	return listWaiting(db)
}

// listWaiting is what waits on you: plans up for review, and tickets sent
// back to their author (the last outcome flow recorded for them is L1).
func listWaiting(db *DB) error {
	review, err := proposals(db, `state IN ('starting', 'running') AND phase = 'plan'`)
	if err != nil {
		return err
	}
	if len(review) > 0 {
		fmt.Fprintln(stdout)
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "REVIEW\tWS\tTITLE\tPLAN\tWAITING")
		for _, p := range review {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Workspace, elide(p.Title, 40), p.PlanURL, ago(p.PhaseAt))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	rows, err := db.Query(`SELECT url, workspace, at FROM assessments WHERE level = 'L1' ORDER BY at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var lines [][3]string
	for rows.Next() {
		var url, ws, at string
		if err := rows.Scan(&url, &ws, &at); err != nil {
			return err
		}
		lines = append(lines, [3]string{url, ws, at})
	}
	if len(lines) > 0 {
		fmt.Fprintln(stdout)
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "REFINEMENT\tWS\tSENT BACK")
		for _, l := range lines {
			fmt.Fprintf(tw, "%s\t%s\t%s ago\n", shortTickets([]string{l[0]}), l[1], ago(l[2]))
		}
		return tw.Flush()
	}
	return rows.Err()
}

// listRunning shows what holds a slot, and whether its session still runs:
// a session that ended without "ais flow done" still holds one.
func listRunning(db *DB) error {
	running, err := proposals(db, `state IN ('starting', 'running')`)
	if err != nil || len(running) == 0 {
		return err
	}
	status := map[string]string{} // proposal -> its newest session's status
	session := map[string]string{}
	if sessions, err := ais.Sessions(); err == nil {
		for _, s := range sessions { // newest first
			if s.FlowProposal != "" && session[s.FlowProposal] == "" {
				session[s.FlowProposal], status[s.FlowProposal] = s.ID, s.Status
			}
		}
	}
	fmt.Fprintln(stdout)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNING	WS	TITLE	SESSION	SINCE")
	for _, p := range running {
		s := orDefault(session[p.ID], "-")
		if p.Kind == "refine" {
			s += " (refinement)"
		}
		switch {
		case p.State == "starting":
			s = "starting"
		case status[p.ID] == "dead":
			s += " (ended; ais flow done " + p.ID + " frees the slot)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Workspace, elide(p.Title, 40), s, ago(p.StartedAt))
	}
	return tw.Flush()
}

func ago(s string) string {
	t, ok := parseStamp(s)
	if !ok {
		return s
	}
	d := now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func cmdLog(args []string) error {
	o, err := parseOpts(args, []string{"-n"}, nil, nil)
	if err != nil {
		return err
	}
	limit := 20
	if v, ok := o.one("-n"); ok {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			return usagef("-n must be a positive number")
		}
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT at, kind, subject, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	n := 0
	for rows.Next() {
		var at, kind, subject, detail string
		if err := rows.Scan(&at, &kind, &subject, &detail); err != nil {
			return err
		}
		t, _ := parseStamp(at)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Local().Format("01-02 15:04"), kind, subject, elide(detail, 80))
		n++
	}
	if n == 0 {
		note("nothing has happened yet")
	}
	return tw.Flush()
}
