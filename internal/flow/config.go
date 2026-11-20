// Package flow is "ais flow": workspaces, a planner session that proposes
// work, a queue of the sessions you accepted, and a dispatcher that starts
// them within limits. Design: docs/flow-proposal.md.
//
// Everything is one-shot: systemd timers run the scheduled parts, state is a
// SQLite file next to sessions.json, and sessions are ordinary ais sessions.
package flow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
)

// Limits caps how much flow starts. Zero means "not configured" only while
// reading; Defaults fills it in.
type Limits struct {
	WIP     int // live dispatched sessions
	PerHour int // starts in any rolling 60 minutes
	PerDay  int // starts in any rolling 24 hours
}

var defaultLimits = Limits{WIP: 4, PerHour: 4, PerDay: 16}

// Source is where a workspace's tickets come from and how their status is
// told. Only GitHub Projects (v2) for now.
type SourceConf struct {
	Kind       string // "github-project"
	Owner      string
	Project    int
	Field      string
	Ready      string
	Scheduled  string
	InProgress string
	Comment    bool // leave a short comment on a ticket when it is scheduled
}

type PlannerConf struct {
	Profile     string
	Prompt      string
	Cwd         string
	Schedule    string // systemd OnCalendar
	IdleTimeout time.Duration
}

type SessionConf struct {
	Prompt string // template: {tickets}, {notes}
}

type Workspace struct {
	Name        string
	TmuxSession string
	Profile     string
	WIP         int // 0: only the global cap applies
	Env         map[string]string
	Source      SourceConf
	Repos       map[string]string // short name -> main checkout
	Planner     PlannerConf
	Session     SessionConf
}

type Config struct {
	Limits     Limits
	Workspaces map[string]*Workspace
	raw        map[string]any // the whole ais config, for profile checks
}

func (c *Config) workspace(name string) (*Workspace, error) {
	if name == "" {
		if len(c.Workspaces) == 1 {
			for _, w := range c.Workspaces {
				return w, nil
			}
		}
		return nil, fmt.Errorf("name the workspace (--ws); configured: %s", c.names())
	}
	w, ok := c.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("no workspace %q in config.toml; configured: %s", name, c.names())
	}
	return w, nil
}

func (c *Config) names() string {
	if len(c.Workspaces) == 0 {
		return "none (add a [workspace.<name>] table)"
	}
	var out []string
	for n := range c.Workspaces {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func loadConfig() (*Config, error) {
	raw, err := ais.Config()
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

const defaultSessionPrompt = "/work-tickets {tickets}\n\n{notes}"

func parseConfig(raw map[string]any) (*Config, error) {
	c := &Config{Limits: defaultLimits, Workspaces: map[string]*Workspace{}, raw: raw}
	r := reader{}
	if t := r.table(raw, "flow", "[flow]"); t != nil {
		c.Limits.WIP = r.positive(t, "wip", "flow.wip", defaultLimits.WIP)
		c.Limits.PerHour = r.positive(t, "per_hour", "flow.per_hour", defaultLimits.PerHour)
		c.Limits.PerDay = r.positive(t, "per_day", "flow.per_day", defaultLimits.PerDay)
	}
	for name, v := range r.table(raw, "workspace", "[workspace]") {
		where := "workspace." + name
		t, ok := v.(map[string]any)
		if !ok {
			r.fail("[%s] must be a table", where)
			continue
		}
		w := &Workspace{
			Name:        name,
			TmuxSession: r.str(t, "tmux_session", where+".tmux_session", name),
			Profile:     r.str(t, "profile", where+".profile", "claude"),
			WIP:         r.positive(t, "wip", where+".wip", 0),
			Env:         r.strings(t, "env", where+".env"),
			Repos:       map[string]string{},
		}
		src := r.table(t, "source", "["+where+".source]")
		w.Source = SourceConf{
			Kind:       r.str(src, "kind", where+".source.kind", "github-project"),
			Owner:      r.str(src, "owner", where+".source.owner", ""),
			Project:    r.positive(src, "project", where+".source.project", 0),
			Field:      r.str(src, "field", where+".source.field", "Status"),
			Ready:      r.str(src, "ready", where+".source.ready", "ready"),
			Scheduled:  r.str(src, "scheduled", where+".source.scheduled", "scheduled"),
			InProgress: r.str(src, "in_progress", where+".source.in_progress", "In Progress"),
			Comment:    r.boolean(src, "comment", where+".source.comment", true),
		}
		if w.Source.Kind != "github-project" {
			r.fail("%s.source.kind must be \"github-project\" (the only source so far)", where)
		}
		if w.Source.Owner == "" || w.Source.Project == 0 {
			r.fail("%s.source needs owner and project", where)
		}
		for short, path := range r.strings(t, "repos", where+".repos") {
			w.Repos[short] = ais.ExpandPath(path)
		}
		pl := r.table(t, "planner", "["+where+".planner]")
		w.Planner = PlannerConf{
			Profile:  r.str(pl, "profile", where+".planner.profile", w.Profile),
			Prompt:   r.str(pl, "prompt", where+".planner.prompt", "/plan-sessions "+name),
			Cwd:      ais.ExpandPath(r.str(pl, "cwd", where+".planner.cwd", "")),
			Schedule: r.str(pl, "schedule", where+".planner.schedule", "Mon..Fri *-*-* 09,14:00:00"),
		}
		w.Planner.IdleTimeout = r.duration(pl, "idle_timeout", where+".planner.idle_timeout", 48*time.Hour)
		se := r.table(t, "session", "["+where+".session]")
		w.Session = SessionConf{Prompt: r.str(se, "prompt", where+".session.prompt", defaultSessionPrompt)}
		c.Workspaces[name] = w
	}
	if r.err != nil {
		return nil, r.err
	}
	return c, nil
}

// reader collects the first config error and keeps going with defaults, so
// one bad key does not hide the rest of the parse.
type reader struct{ err error }

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("config: "+format, args...)
	}
}

func (r *reader) table(m map[string]any, key, what string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		r.fail("%s must be a table", what)
	}
	return t
}

func (r *reader) str(m map[string]any, key, what, fallback string) string {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	s, ok := v.(string)
	if !ok {
		r.fail("%s must be a string", what)
		return fallback
	}
	return s
}

func (r *reader) boolean(m map[string]any, key, what string, fallback bool) bool {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	b, ok := v.(bool)
	if !ok {
		r.fail("%s must be true or false", what)
		return fallback
	}
	return b
}

func (r *reader) positive(m map[string]any, key, what string, fallback int) int {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	n, ok := v.(int64)
	if !ok || n <= 0 {
		r.fail("%s must be a positive whole number", what)
		return fallback
	}
	return int(n)
}

func (r *reader) duration(m map[string]any, key, what string, fallback time.Duration) time.Duration {
	s := r.str(m, key, what, "")
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		r.fail("%s must be a duration such as \"48h\"", what)
		return fallback
	}
	return d
}

func (r *reader) strings(m map[string]any, key, what string) map[string]string {
	t := r.table(m, key, what)
	out := map[string]string{}
	for k, v := range t {
		s, ok := v.(string)
		if !ok {
			r.fail("%s.%s must be a string", what, k)
			continue
		}
		out[k] = s
	}
	return out
}
