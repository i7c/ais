package ais

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type config map[string]any

func loadConfig() config {
	if !exists(configPath) {
		must(os.MkdirAll(aisHome, 0o755))
		must(os.WriteFile(configPath, []byte(defaultConfig), 0o644))
		note("wrote a starter config to %s", tilde(configPath))
	}
	return readConfig()
}

func readConfig() config {
	cfg := config{}
	if _, err := toml.DecodeFile(configPath, (*map[string]any)(&cfg)); err != nil {
		die("%s: %v", tilde(configPath), err)
	}
	return cfg
}

func must(err error) {
	if err != nil {
		die("%v", err)
	}
}

// table is cfg[key] as a table; nil when absent, fatal when not a table.
func table(m map[string]any, key, name string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		die("config: [%s] must be a table", name)
	}
	return t
}

// strings is v as a list of strings, a lone string counting as one.
func stringList(v any, what string) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				die("config: %s must be a list of strings", what)
			}
			out = append(out, s)
		}
		return out
	}
	die("config: %s must be a list of strings", what)
	return nil
}

// profileTable is [profile.<name>] laid over [harness.<name>].
//
// [harness.*] is what profiles were called while every one of them launched
// an agent; it is still read, so a config written then keeps working, and a
// later [profile.*] table only has to say what it changes. env merges, every
// other key is replaced whole.
func profileTable(cfg config, profile string) map[string]any {
	merged := map[string]any{}
	for _, section := range []string{"harness", "profile"} {
		t := table(cfg, section, section)
		if t == nil {
			continue
		}
		specific, ok := t[profile]
		if !ok {
			specific, ok = t[filepath.Base(profile)]
		}
		if !ok {
			continue
		}
		spec, isTable := specific.(map[string]any)
		if !isTable {
			die("config: [%s.%s] must be a table", section, profile)
		}
		env := map[string]any{}
		if e, ok := merged["env"].(map[string]any); ok {
			for k, v := range e {
				env[k] = v
			}
		}
		if e, ok := spec["env"].(map[string]any); ok {
			for k, v := range e {
				env[k] = v
			}
		}
		for k, v := range spec {
			merged[k] = v
		}
		if len(env) > 0 {
			merged["env"] = env
		}
	}
	return merged
}

// profileNames is every profile the config mentions, under either name.
func profileNames(cfg config) []string {
	seen := map[string]bool{}
	for _, section := range []string{"harness", "profile"} {
		if t, ok := cfg[section].(map[string]any); ok {
			for k, v := range t {
				if _, isTable := v.(map[string]any); isTable {
					seen[k] = true
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// sessionConf is [profile.<name>.session]: how a harness names conversations.
type sessionConf struct {
	New, Resume, Fork, Conflicts []string
	ID                           string // "uuid4" or "short"
}

type settings struct {
	Flags        []string
	Env          map[string]string
	Command      []string
	Session      *sessionConf
	RenameWindow bool
	Worktree     wtSettings
}

// harnessSettings merges [defaults] with the profile's table: flags
// concatenate, env merges.
func harnessSettings(cfg config, harness string) settings {
	defaults := table(cfg, "defaults", "defaults")
	specific := profileTable(cfg, harness)

	flags := append(stringList(defaults["flags"], "flags for "+harness), stringList(specific["flags"], "flags for "+harness)...)

	env := map[string]string{}
	for _, src := range []any{defaults["env"], specific["env"]} {
		if src == nil {
			continue
		}
		t, ok := src.(map[string]any)
		if !ok {
			die("config: env for %s must be a table", harness)
		}
		for k, v := range t {
			s, ok := v.(string)
			if !ok {
				die("config: env.%s must be a string", k)
			}
			env[k] = expand(s)
		}
	}

	command := stringList(specific["command"], "command for "+harness)
	if len(command) == 0 {
		command = []string{harness}
	}
	return settings{
		Flags:        flags,
		Env:          env,
		Command:      command,
		Session:      sessionSettings(specific["session"], harness),
		RenameWindow: renameWindowSetting(cfg),
		Worktree:     worktreeSettings(cfg),
	}
}

func renameWindowSetting(cfg config) bool {
	t := table(cfg, "tmux", "tmux")
	v, ok := t["rename"]
	if !ok {
		return true
	}
	b, isBool := v.(bool)
	if !isBool {
		die("config: tmux.rename must be true or false")
	}
	return b
}

// sessionSettings validates [profile.<name>.session]; nil without one.
//
// Both templates are optional on their own: a harness that can resume but not
// be told its id up front still works, ais just has no id to give it until
// the harness reports one.
func sessionSettings(v any, harness string) *sessionConf {
	if v == nil {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		die("config: [profile.%s.session] must be a table", harness)
	}
	list := func(key string) []string {
		return stringList(t[key], fmt.Sprintf("session.%s for %s", key, harness))
	}
	conf := &sessionConf{New: list("new"), Resume: list("resume"), Fork: list("fork"), Conflicts: list("conflicts")}
	conf.ID = "uuid4"
	if id, ok := t["id"]; ok {
		s, _ := id.(string)
		if s != "uuid4" && s != "short" {
			die(`config: session.id for %s must be "uuid4" or "short"`, harness)
		}
		conf.ID = s
	}
	for key, tmpl := range map[string][]string{"new": conf.New, "resume": conf.Resume, "fork": conf.Fork} {
		if len(tmpl) > 0 && !anyContains(tmpl, "{id}") {
			die("config: session.%s for %s must contain {id} somewhere", key, harness)
		}
	}
	// Asking for the same conversation twice, in two ways, is an argv error in
	// every harness worth supporting — so ais stands down when you name one.
	flags := map[string]bool{}
	for _, part := range concat(conf.New, conf.Resume, conf.Fork, conf.Conflicts) {
		if strings.HasPrefix(part, "-") {
			flags[part] = true
		}
	}
	conf.Conflicts = sortedKeys(flags)
	return conf
}

// wtSettings is [worktree]: where session worktrees go and what follows them.
type wtSettings struct {
	Base   string
	Copy   []string
	Fetch  bool
	Auto   bool
	From   string
	Detach bool
}

var wtFrom = []string{"auto", "origin", "local", "head"}

// worktreeSettings reads [worktree]; $AIS_WT_BASE wins over the configured
// base, so a one-off run can be sent somewhere else without editing config.
func worktreeSettings(cfg config) wtSettings {
	t := table(cfg, "worktree", "worktree")
	base := wtBase
	if v, ok := t["base"]; ok {
		s, isStr := v.(string)
		if !isStr {
			die("config: worktree.base must be a string")
		}
		base = s
	}
	if env := os.Getenv("AIS_WT_BASE"); env != "" {
		base = env
	}
	var copies []string
	for _, c := range stringList(t["copy"], "worktree.copy") {
		if c != "" {
			copies = append(copies, c)
		}
	}
	boolean := func(key string, fallback bool) bool {
		v, ok := t[key]
		if !ok {
			return fallback
		}
		b, isBool := v.(bool)
		if !isBool {
			die("config: worktree.%s must be true or false", key)
		}
		return b
	}
	from := "auto"
	if v, ok := t["from"]; ok {
		s, _ := v.(string)
		if !contains(wtFrom, s) {
			die("config: worktree.from must be one of %s", strings.Join(wtFrom, ", "))
		}
		from = s
	}
	return wtSettings{
		Base:   expand(base),
		Copy:   copies,
		Fetch:  boolean("fetch", true),
		Auto:   boolean("auto", false),
		From:   from,
		Detach: boolean("detach", false),
	}
}

func (w wtSettings) with(from string, detach *bool) wtSettings {
	if from != "" {
		w.From = from
	}
	if detach != nil {
		w.Detach = *detach
	}
	return w
}

type notifyConf struct {
	Sound       string
	Window      float64 // minutes
	WhenVisible string  // "skip" or "send"
	SeenAfter   float64 // seconds
	Sink        []string
}

const (
	notifyKeep      = 20    // per session
	notifyMaxAge    = 86400 // seconds; older entries are dropped on the next write
	notifyWindow    = 60.0  // minutes counted as "recent" by ls/status/select
	notifySound     = "Glass"
	notifySeenAfter = 3.0 // seconds on screen before a pane's call counts as seen
)

// notifySettings reads [notify] without forcing a config write when there
// is nothing to read.
func notifySettings() notifyConf {
	t := map[string]any{}
	if exists(configPath) {
		if n := table(readConfig(), "notify", "notify"); n != nil {
			t = n
		}
	}
	number := func(key string, fallback float64, msg string) float64 {
		v, ok := t[key]
		if !ok {
			return fallback
		}
		var f float64
		switch x := v.(type) {
		case int64:
			f = float64(x)
		case float64:
			f = x
		default:
			die("config: %s", msg)
		}
		if f < 0 {
			die("config: %s", msg)
		}
		return f
	}
	conf := notifyConf{
		Sound:       notifySound,
		Window:      number("window", notifyWindow, "notify.window must be a non-negative number of minutes"),
		WhenVisible: "skip",
		SeenAfter:   number("seen_after", notifySeenAfter, "notify.seen_after must be a non-negative number of seconds"),
	}
	if v, ok := t["sound"]; ok {
		s, isStr := v.(string)
		if !isStr {
			die("config: notify.sound must be a string")
		}
		conf.Sound = s
	}
	if v, ok := t["when_visible"]; ok {
		s, _ := v.(string)
		if s != "skip" && s != "send" {
			die(`config: notify.when_visible must be "skip" or "send"`)
		}
		conf.WhenVisible = s
	}
	switch v := t["sink"].(type) {
	case nil:
	case string:
		conf.Sink = shellSplit(v)
	default:
		conf.Sink = stringList(v, "notify.sink")
	}
	for i, s := range conf.Sink {
		conf.Sink[i] = expandUser(s)
	}
	return conf
}

// ---------------------------------------------------------------- small helpers

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

func anyContains(list []string, sub string) bool {
	for _, x := range list {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
