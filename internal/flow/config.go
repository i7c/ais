// Package flow is "ais flow": workspaces, a planner session that proposes
// work, a queue of the sessions you accepted, and a dispatcher that starts
// them within limits. Design: docs/flow-proposal.md.
//
// Everything is one-shot: systemd timers run the scheduled parts, state is a
// SQLite file next to sessions.json, and sessions are ordinary ais sessions.
package flow

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/i7c/ais/internal/ais"
)

// Limits caps how much flow starts. Zero means "not configured" only while
// reading; Defaults fills it in.
type Limits struct {
	WIP           int // live flow sessions: work, refinement runs, open planners
	PerHour       int // work session starts in any rolling 60 minutes
	PerDay        int // work session starts in any rolling 24 hours
	RefinePerHour int // refinement run starts in any rolling 60 minutes
	RefinePerDay  int // refinement run starts in any rolling 24 hours
}

var defaultLimits = Limits{WIP: 4, PerHour: 4, PerDay: 16, RefinePerHour: 6, RefinePerDay: 24}

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

// RoleConf is how a role's prompt is made: the built-in one, with
// Instructions added; or PromptFile instead of it; or Prompt, a literal
// first prompt (a skill, say) as before prompts were built in.
type RoleConf struct {
	Prompt       string // literal; "" = built in
	PromptFile   string
	Instructions string // inline, then the file's text
}

type PlannerConf struct {
	RoleConf
	Profile     string
	Cwd         string
	Schedule    string // systemd OnCalendar
	IdleTimeout time.Duration
	// CloseAfter is how long a decided planner's window stays open with no
	// input from you, so its last words can be read.
	CloseAfter time.Duration
	// MinInterval is the least time between two scheduled planner starts in
	// a workspace, so declining everything does not bring the same question
	// back every hour. A finished session (ais flow done) does not wait.
	MinInterval time.Duration
}

type SessionConf struct {
	RoleConf        // Prompt is a template here: {tickets}, {notes}
	Autonomy string // the ceiling when neither the proposal nor the board names one
}

// RefineConf is the refinement sweep: tickets that turn ready are assessed
// by a headless run before any planner sees them.
type RefineConf struct {
	RoleConf
	Enabled   bool
	Profile   string // needs a batch template
	Cwd       string
	Status    string // the board status of a ticket waiting on its author
	MaxRounds int
	Sweep     time.Duration // how often the dispatch tick looks
	Retry     time.Duration // how long a failed run holds its ticket back
	WIP       int           // refine runs at once in this workspace
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
	Refine      RefineConf
	RoleConf    // Instructions here go to every role
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

// defaultSchedule checks every hour of the working day. Checks are cheap: a
// planner only opens when there is plannable work, room for more sessions,
// and none is open — and one left alone closes itself after idle_timeout.
const defaultSchedule = "Mon..Fri *-*-* 08..20:00:00"

func parseConfig(raw map[string]any) (*Config, error) {
	c := &Config{Limits: defaultLimits, Workspaces: map[string]*Workspace{}, raw: raw}
	r := reader{}
	if t := r.table(raw, "flow", "[flow]"); t != nil {
		c.Limits.WIP = r.positive(t, "wip", "flow.wip", defaultLimits.WIP)
		c.Limits.PerHour = r.positive(t, "per_hour", "flow.per_hour", defaultLimits.PerHour)
		c.Limits.PerDay = r.positive(t, "per_day", "flow.per_day", defaultLimits.PerDay)
		c.Limits.RefinePerHour = r.positive(t, "refine_per_hour", "flow.refine_per_hour", defaultLimits.RefinePerHour)
		c.Limits.RefinePerDay = r.positive(t, "refine_per_day", "flow.refine_per_day", defaultLimits.RefinePerDay)
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
		w.RoleConf = r.role(t, where)
		pl := r.table(t, "planner", "["+where+".planner]")
		w.Planner = PlannerConf{
			RoleConf: r.role(pl, where+".planner"),
			Profile:  r.str(pl, "profile", where+".planner.profile", w.Profile),
			Cwd:      ais.ExpandPath(r.str(pl, "cwd", where+".planner.cwd", "")),
			Schedule: r.str(pl, "schedule", where+".planner.schedule", defaultSchedule),
		}
		w.Planner.IdleTimeout = r.duration(pl, "idle_timeout", where+".planner.idle_timeout", 48*time.Hour)
		w.Planner.CloseAfter = r.durationOrZero(pl, "close_after", where+".planner.close_after", 5*time.Minute)
		w.Planner.MinInterval = r.durationOrZero(pl, "min_interval", where+".planner.min_interval", 2*time.Hour)
		se := r.table(t, "session", "["+where+".session]")
		w.Session = SessionConf{RoleConf: r.role(se, where+".session"),
			Autonomy: r.level(se, "autonomy", where+".session.autonomy", "L2")}
		rf := r.table(t, "refine", "["+where+".refine]")
		w.Refine = RefineConf{
			RoleConf:  r.role(rf, where+".refine"),
			Enabled:   r.boolean(rf, "enabled", where+".refine.enabled", true),
			Profile:   r.str(rf, "profile", where+".refine.profile", w.Profile),
			Cwd:       ais.ExpandPath(r.str(rf, "cwd", where+".refine.cwd", w.Planner.Cwd)),
			Status:    r.str(rf, "status", where+".refine.status", "refinement"),
			MaxRounds: r.positive(rf, "max_rounds", where+".refine.max_rounds", 3),
			Sweep:     r.durationOrZero(rf, "sweep", where+".refine.sweep", 30*time.Minute),
			Retry:     r.durationOrZero(rf, "retry", where+".refine.retry", time.Hour),
			WIP:       r.positive(rf, "wip", where+".refine.wip", 1),
		}
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

func (r *reader) durationOrZero(m map[string]any, key, what string, fallback time.Duration) time.Duration {
	s := r.str(m, key, what, "")
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		r.fail("%s must be a duration such as \"5m\" (or \"0s\")", what)
		return fallback
	}
	return d
}

// role reads a role's prompt settings: prompt, prompt_file, instructions
// and instructions_file (both may be given; the inline text comes first).
func (r *reader) role(m map[string]any, where string) RoleConf {
	rc := RoleConf{
		Prompt:       r.str(m, "prompt", where+".prompt", ""),
		PromptFile:   ais.ExpandPath(r.str(m, "prompt_file", where+".prompt_file", "")),
		Instructions: strings.TrimSpace(r.str(m, "instructions", where+".instructions", "")),
	}
	if rc.Prompt != "" && rc.PromptFile != "" {
		r.fail("%s: prompt and prompt_file both replace the built-in prompt; pick one", where)
	}
	if path := r.str(m, "instructions_file", where+".instructions_file", ""); path != "" {
		data, err := os.ReadFile(ais.ExpandPath(path))
		if err != nil {
			r.fail("%s.instructions_file: %v", where, err)
		} else if text := strings.TrimSpace(string(data)); text != "" {
			rc.Instructions = strings.TrimSpace(rc.Instructions + "\n\n" + text)
		}
	}
	return rc
}

// level reads an autonomy level: L1, L2 or L3.
func (r *reader) level(m map[string]any, key, what, fallback string) string {
	v := strings.ToUpper(r.str(m, key, what, fallback))
	if _, ok := levelRank[v]; !ok {
		r.fail("%s must be L1, L2 or L3", what)
		return fallback
	}
	return v
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
