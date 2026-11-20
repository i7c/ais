package flow

import (
	"fmt"
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

func countFor(db *DB, ws string) (counts, error) {
	var c counts
	q := `SELECT COUNT(*) FROM proposals WHERE state = ?`
	args := func(state string) []any {
		if ws == "" {
			return []any{state}
		}
		return []any{state, ws}
	}
	if ws != "" {
		q += ` AND workspace = ?`
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

// startsSince counts the starts (automatic and manual alike) since t.
func startsSince(db *DB, t time.Time) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM starts WHERE at > ?`, stamp(t)).Scan(&n)
	return n, err
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
		c, err := countFor(db, n)
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
	all, err := countFor(db, "")
	if err != nil {
		return err
	}
	hour, err := startsSince(db, now().Add(-time.Hour))
	if err != nil {
		return err
	}
	day, err := startsSince(db, now().Add(-24*time.Hour))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nrunning %d/%d   started in the last hour %d/%d, last 24h %d/%d\n",
		all.Running, limits.WIP, hour, limits.PerHour, day, limits.PerDay)
	return nil
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
