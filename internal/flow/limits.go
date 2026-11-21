package flow

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// effective is a limit with where it came from.
type effective struct {
	Key    string
	Value  int
	Config int
	Until  string // non-empty while an override holds
}

var limitKeys = []string{"wip", "per_hour", "per_day", "refine_per_hour", "refine_per_day"}

func configured(l Limits, key string) int {
	switch key {
	case "wip":
		return l.WIP
	case "per_hour":
		return l.PerHour
	case "refine_per_hour":
		return l.RefinePerHour
	case "refine_per_day":
		return l.RefinePerDay
	}
	return l.PerDay
}

// effectiveLimits is the config overridden by any unexpired "ais flow limit";
// expired overrides are dropped on the way.
func effectiveLimits(db *DB, l Limits) (Limits, []effective, error) {
	at := stamp(now())
	if _, err := db.Exec(`DELETE FROM overrides WHERE until <= ?`, at); err != nil {
		return l, nil, err
	}
	rows, err := db.Query(`SELECT key, value, until FROM overrides`)
	if err != nil {
		return l, nil, err
	}
	defer rows.Close()
	over := map[string][2]string{}
	for rows.Next() {
		var key, until string
		var value int
		if err := rows.Scan(&key, &value, &until); err != nil {
			return l, nil, err
		}
		over[key] = [2]string{strconv.Itoa(value), until}
	}
	out := l
	var list []effective
	for _, k := range limitKeys {
		e := effective{Key: k, Config: configured(l, k), Value: configured(l, k)}
		if o, ok := over[k]; ok {
			e.Value, _ = strconv.Atoi(o[0])
			e.Until = o[1]
		}
		switch k {
		case "wip":
			out.WIP = e.Value
		case "per_hour":
			out.PerHour = e.Value
		case "per_day":
			out.PerDay = e.Value
		case "refine_per_hour":
			out.RefinePerHour = e.Value
		case "refine_per_day":
			out.RefinePerDay = e.Value
		}
		list = append(list, e)
	}
	return out, list, rows.Err()
}

// parseUntil reads --until: eod (the end of today, local time), HH:MM (the
// next time it is that o'clock), YYYY-MM-DD (the end of that day), or an RFC
// 3339 time.
func parseUntil(s string, from time.Time) (time.Time, error) {
	local := from.Local()
	endOfDay := func(d time.Time) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.Local)
	}
	switch {
	case s == "" || s == "eod":
		return endOfDay(local), nil
	case len(s) == 5 && s[2] == ':':
		t, err := time.ParseInLocation("15:04", s, time.Local)
		if err != nil {
			break
		}
		at := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if !at.After(local) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	case len(s) == 10:
		d, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			break
		}
		return endOfDay(d), nil
	default:
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, usagef("--until %q: use eod, HH:MM, YYYY-MM-DD or an RFC 3339 time", s)
}

func cmdLimit(args []string) error {
	o, err := parseOpts(args, []string{"--until", "--for"}, []string{"--clear"}, nil)
	if err != nil {
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

	if o.flags["--clear"] {
		if len(o.pos) > 1 {
			return usagef("limit --clear takes at most one key")
		}
		err := db.tx(func(tx *sql.Tx) error {
			if len(o.pos) == 1 {
				if !contains(limitKeys, o.pos[0]) {
					return usagef("no limit %q; there are %s", o.pos[0], strings.Join(limitKeys, ", "))
				}
				_, err := tx.Exec(`DELETE FROM overrides WHERE key = ?`, o.pos[0])
				logEvent(tx, "limit-cleared", o.pos[0], nil)
				return err
			}
			_, err := tx.Exec(`DELETE FROM overrides`)
			logEvent(tx, "limit-cleared", "all", nil)
			return err
		})
		if err != nil {
			return err
		}
		return showLimits(db, cfg)
	}
	switch len(o.pos) {
	case 0:
		return showLimits(db, cfg)
	case 2:
	default:
		return usagef("limit KEY VALUE, e.g. ais flow limit per_day 32")
	}
	key, val := o.pos[0], o.pos[1]
	if !contains(limitKeys, key) {
		return usagef("no limit %q; there are %s", key, strings.Join(limitKeys, ", "))
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return usagef("%s must be a whole number, 0 or more", key)
	}
	var until time.Time
	_, hasUntil := o.one("--until")
	forText, hasFor := o.one("--for")
	switch {
	case hasUntil && hasFor:
		return usagef("--until and --for both say when; pick one")
	case hasFor:
		d, err := time.ParseDuration(forText)
		if err != nil || d <= 0 {
			return usagef("--for %q: use a duration such as 3h", forText)
		}
		until = now().Add(d)
	default:
		if until, err = parseUntil(o.str("--until"), now()); err != nil {
			return err
		}
	}
	err = db.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO overrides (key, value, until, set_at) VALUES (?, ?, ?, ?)`,
			key, n, stamp(until), stamp(now()))
		logEvent(tx, "limit", key, map[string]any{"value": n, "until": stamp(until)})
		return err
	})
	if err != nil {
		return err
	}
	return showLimits(db, cfg)
}

func showLimits(db *DB, cfg *Config) error {
	_, list, err := effectiveLimits(db, cfg.Limits)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, e := range list {
		from := "config"
		if e.Until != "" {
			t, _ := parseStamp(e.Until)
			from = fmt.Sprintf("override until %s (config: %d)", t.Local().Format("Mon 15:04"), e.Config)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", e.Key, e.Value, from)
	}
	return tw.Flush()
}

// ---------------------------------------------------------------- small helpers

func note(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ais flow: %s\n", fmt.Sprintf(format, args...))
}

func getenv(k string) string { return os.Getenv(k) }

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func elide(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
