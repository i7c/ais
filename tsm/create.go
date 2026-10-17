package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Making something new: what to run, where, whether in a worktree of its
// own, and where its pane goes. Every choice has a default, so "n ⏎" already
// does the likely thing: another of what you ran last, next to the selection.

const shellProfile = "" // the choice that is no profile: a plain shell

type CreateSpec struct {
	Profile  string // shellProfile for a plain shell
	Dir      string
	Worktree bool // give an ais session a worktree of its own (-w)
	Place    Placement
	About    string // ais -d; for a shell, the window or session name
	Anchor   *Pane  // what NewWindow and the splits are placed next to
}

// Argv is what gets typed into the new pane, or nil for a shell, which needs
// nothing typed at all.
func (c CreateSpec) Argv(ais string) []string {
	if c.Profile == shellProfile {
		return nil
	}
	argv := []string{ais, "run", c.Profile}
	if c.Worktree {
		argv = append(argv, "-w")
	} else {
		argv = append(argv, "--no-worktree")
	}
	if c.About != "" {
		argv = append(argv, "-d", c.About)
	}
	return argv
}

func (a Actions) Create(c CreateSpec) (Outcome, error) {
	if st, err := os.Stat(c.Dir); err != nil || !st.IsDir() {
		return Outcome{}, fmt.Errorf("%s is not a directory", tilde(c.Dir))
	}
	argv := c.Argv(a.ais.Command())
	if argv != nil && !a.ais.Available() {
		return Outcome{}, fmt.Errorf("ais is not available to start %s", c.Profile)
	}
	var id string
	var err error
	if c.Place == NewSession || c.Anchor == nil {
		id, err = a.tmux.NewSession(a.sessionName(c), c.Dir)
	} else {
		id, err = a.tmux.Open(c.Anchor, c.Dir, nil, c.Place)
	}
	if err != nil {
		return Outcome{}, err
	}
	if argv != nil {
		err = a.tmux.Type(id, argv...)
	} else if c.About != "" && c.Place == NewWindow {
		// ais names its own window; a shell gets the name it was given.
		err = a.tmux.RenameWindow(id, c.About)
	}
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Quit: true}, a.tmux.Focus(a.client(), id)
}

// sessionName is what a new session is called: the description, else the
// directory, made tmux-safe and unique among the sessions there are.
func (a Actions) sessionName(c CreateSpec) string {
	base := ""
	if c.Profile != shellProfile {
		base = a.ais.NameOf(c.About) // what ais will call the work
	}
	if base == "" {
		base = slug(c.About)
	}
	if base == "" {
		base = slug(filepath.Base(c.Dir))
	}
	if base == "" {
		base = "tsm"
	}
	taken := map[string]bool{}
	for _, s := range a.snap.Sessions {
		taken[s.Name] = true
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// slug keeps letters, digits, dashes and underscores, which is everything
// tmux is happy to see in a session name (no dots, no colons).
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}

// ---------------------------------------------------------------- where

// A Place is a directory worth offering, and why.
type Place struct {
	Dir  string `json:"dir"`
	Why  string `json:"why"` // "selected", "here", "pane", "agent", "worktree", "repo", "typed"
	Repo bool   `json:"repo"`
}

// Places are the directories to offer, most likely first: where the
// selection works, where you are, every pane's directory, where ais sessions
// ran, the session worktrees, then the repos under roots. Each directory
// once, under its most specific reason.
func Places(snap *Snapshot, st *AisState, selected *Pane, roots []string) []Place {
	var out []Place
	seen := map[string]bool{}
	add := func(dir, why string) {
		dir = filepath.Clean(dir)
		if dir == "." || seen[dir] {
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return
		}
		seen[dir] = true
		out = append(out, Place{Dir: dir, Why: why, Repo: inRepo(dir)})
	}
	if selected != nil {
		add(selected.Dir(), "selected")
	}
	if here := snap.Pane(snap.Client.PaneID); here != nil {
		add(here.Dir(), "here")
	}
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				add(p.Dir(), "pane")
			}
		}
	}
	if st != nil {
		sessions := append([]AisSession(nil), st.Sessions...)
		sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].StartedAt > sessions[j].StartedAt })
		for _, s := range sessions {
			if s.Host == st.Host {
				add(s.Cwd, "agent")
			}
		}
		if st.Worktree.Base != "" {
			for _, dir := range worktreeDirs(st.Worktree.Base) {
				add(dir, "worktree")
			}
		}
	}
	for _, dir := range repos(roots) {
		add(dir, "repo")
	}
	return out
}

// withSelected puts the selection's directory first in places a remote
// listed without knowing what would be selected.
func withSelected(places []Place, sel *Pane) []Place {
	if sel == nil {
		return places
	}
	out := []Place{{Dir: sel.Dir(), Why: "selected", Repo: false}}
	for _, p := range places {
		if p.Dir == sel.Dir() {
			out[0].Repo = p.Repo
			continue
		}
		out = append(out, p)
	}
	return out
}

// worktreeDirs are the checkouts ais made: <base>/<session>/<repo>.
func worktreeDirs(base string) []string {
	var out []string
	sessions, _ := os.ReadDir(base)
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		checkouts, _ := os.ReadDir(filepath.Join(base, s.Name()))
		for _, c := range checkouts {
			dir := filepath.Join(base, s.Name(), c.Name())
			if c.IsDir() && exists(filepath.Join(dir, ".git")) {
				out = append(out, dir)
			}
		}
	}
	return out
}

// repos are the git checkouts under roots, two levels deep at most, which
// covers a directory of repos and a directory of directories of repos.
// A repo is not looked inside.
func repos(roots []string) []string {
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if exists(filepath.Join(sub, ".git")) {
				out = append(out, sub)
			} else if depth > 1 {
				walk(sub, depth-1)
			}
		}
	}
	for _, r := range roots {
		walk(r, 2)
	}
	return out
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// inRepo is true when dir is inside a git checkout, main or worktree.
func inRepo(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, ".git")) {
			return true
		}
		if parent := filepath.Dir(d); parent == d {
			return false
		}
	}
}

// Roots are where repos live: $TSM_ROOTS (colon-separated), else ~/git.
func Roots() []string {
	if env := os.Getenv("TSM_ROOTS"); env != "" {
		var out []string
		for _, r := range strings.Split(env, ":") {
			if r != "" {
				out = append(out, expand(r))
			}
		}
		return out
	}
	return []string{filepath.Join(home, "git")}
}

func expand(path string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// FilterPlaces keeps the places every word of query matches, and offers the
// query itself first when it names a directory that is not on the list.
func FilterPlaces(places []Place, query string) []Place {
	q := strings.TrimSpace(query)
	if q == "" {
		return places
	}
	var out []Place
	if strings.HasPrefix(q, "/") || strings.HasPrefix(q, "~") || strings.HasPrefix(q, ".") {
		dir := filepath.Clean(expand(q))
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			known := false
			for _, p := range places {
				known = known || p.Dir == dir
			}
			if !known {
				out = append(out, Place{Dir: dir, Why: "typed", Repo: inRepo(dir)})
			}
		}
	}
	terms := strings.Fields(strings.ToLower(q))
	for _, p := range places {
		if matches(terms, strings.ToLower(tilde(p.Dir))) {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- memory

// The last choices, so the next "n" offers the same again: a per-user
// convenience, nothing depends on it surviving.
type lastCreate struct {
	Profile  string    `json:"profile"`
	Place    Placement `json:"place"`
	Worktree *bool     `json:"worktree,omitempty"`
}

func lastCreatePath() string { return filepath.Join(cacheDir(), "last-create.json") }

func loadLastCreate() lastCreate {
	last := lastCreate{Profile: "\x00", Place: NewWindow} // "\x00": never chosen
	if path := lastCreatePath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &last)
		}
	}
	return last
}

func saveLastCreate(last lastCreate) {
	path := lastCreatePath()
	if path == "" {
		return
	}
	if data, err := json.Marshal(last); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
}
