package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type formField int

const (
	fWhat formField = iota
	fWhere
	fWorktree
	fPlace
	fAbout
	fieldCount
)

var placements = []Placement{NewWindow, SplitRight, SplitBelow, NewSession}

// createForm is the "n" screen. It starts on where, since that is the
// choice that differs most from one time to the next; what and place
// default to whatever was chosen last.
type createForm struct {
	profiles []string // ais profiles, then the shell
	what     int
	places   []Place
	shown    []Place
	where    textinput.Model
	pick     int
	worktree bool
	place    int
	about    textinput.Model
	focus    formField
	anchor   *Pane
	auto     bool // ais worktree.auto: the default for a repo
	err      string
}

func newCreateForm(snap *Snapshot, st *AisState, anchor *Pane, aisOK bool) *createForm {
	f := &createForm{anchor: anchor, focus: fWhere}
	if st != nil && aisOK {
		for _, p := range st.Profiles {
			f.profiles = append(f.profiles, p.Name)
		}
		f.auto = st.Worktree.Auto
	}
	f.profiles = append(f.profiles, shellProfile)

	last := loadLastCreate()
	for i, p := range f.profiles {
		if p == last.Profile {
			f.what = i
		}
	}
	f.worktree = f.auto
	if last.Worktree != nil {
		f.worktree = *last.Worktree
	}
	for i, p := range placements {
		if p == last.Place {
			f.place = i
		}
	}
	if anchor == nil {
		f.place = len(placements) - 1 // nothing to put it next to
	}

	f.where = textinput.New()
	f.where.Prompt = "/ "
	f.where.Placeholder = "type to filter, or a path"
	f.where.Focus()
	f.about = textinput.New()
	f.about.Prompt = ""
	f.about.Placeholder = "what it is for (optional)"

	f.places = Places(snap, st, anchor, Roots())
	f.refilter()
	return f
}

func (f *createForm) refilter() {
	f.shown = FilterPlaces(f.places, f.where.Value())
	f.pick = min(f.pick, max(0, len(f.shown)-1))
}

func (f *createForm) profile() string { return f.profiles[f.what] }

func (f *createForm) dir() (Place, bool) {
	if f.pick < len(f.shown) {
		return f.shown[f.pick], true
	}
	return Place{}, false
}

// worktreeApplies: only an ais session can get a worktree, and only of a repo.
func (f *createForm) worktreeApplies() bool {
	p, ok := f.dir()
	return ok && p.Repo && f.profile() != shellProfile
}

func (f *createForm) Spec() (CreateSpec, error) {
	p, ok := f.dir()
	if !ok {
		return CreateSpec{}, fmt.Errorf("no directory matches")
	}
	place := placements[f.place]
	return CreateSpec{
		Profile:  f.profile(),
		Dir:      p.Dir,
		Worktree: f.worktree && f.worktreeApplies(),
		Place:    place,
		About:    strings.TrimSpace(f.about.Value()),
		Anchor:   f.anchor,
	}, nil
}

func (f *createForm) remember() {
	wt := f.worktree
	last := lastCreate{Profile: f.profile(), Place: placements[f.place]}
	if f.worktreeApplies() {
		last.Worktree = &wt
	} else if prev := loadLastCreate(); prev.Worktree != nil {
		last.Worktree = prev.Worktree // not asked this time: keep the old answer
	}
	saveLastCreate(last)
}

func (f *createForm) moveFocus(delta int) {
	for range fieldCount {
		f.focus = (f.focus + formField(delta) + fieldCount) % fieldCount
		if f.focus != fWorktree || f.worktreeApplies() {
			break
		}
	}
	f.where.Blur()
	f.about.Blur()
	switch f.focus {
	case fWhere:
		f.where.Focus()
	case fAbout:
		f.about.Focus()
	}
}

func (f *createForm) cycle(delta int) {
	switch f.focus {
	case fWhat:
		f.what = (f.what + delta + len(f.profiles)) % len(f.profiles)
	case fWorktree:
		f.worktree = !f.worktree
	case fPlace:
		n := len(placements)
		if f.anchor == nil {
			return // a new session is the only place there is
		}
		f.place = (f.place + delta + n) % n
	}
}

// update handles a key; done reports the form wants to launch or cancel.
func (f *createForm) update(msg tea.KeyMsg) (cmd tea.Cmd, launch, cancel bool) {
	f.err = ""
	switch msg.String() {
	case "esc":
		return nil, false, true
	case "enter":
		return nil, true, false
	case "tab":
		f.moveFocus(1)
		return nil, false, false
	case "shift+tab":
		f.moveFocus(-1)
		return nil, false, false
	}

	switch f.focus {
	case fWhere:
		switch msg.String() {
		case "up", "ctrl+p", "ctrl+k":
			f.pick = max(0, f.pick-1)
			return nil, false, false
		case "down", "ctrl+n", "ctrl+j":
			f.pick = min(max(0, len(f.shown)-1), f.pick+1)
			return nil, false, false
		}
		before := f.where.Value()
		f.where, cmd = f.where.Update(msg)
		if f.where.Value() != before {
			f.pick = 0
			f.refilter()
		}
		return cmd, false, false
	case fAbout:
		switch msg.String() {
		case "up":
			f.moveFocus(-1)
			return nil, false, false
		case "down":
			f.moveFocus(1)
			return nil, false, false
		}
		f.about, cmd = f.about.Update(msg)
		return cmd, false, false
	}
	switch msg.String() {
	case "left", "h":
		f.cycle(-1)
	case "right", "l", " ":
		f.cycle(1)
	case "up", "k":
		f.moveFocus(-1)
	case "down", "j":
		f.moveFocus(1)
	}
	return nil, false, false
}

func profileLabel(p string) string {
	if p == shellProfile {
		return "shell"
	}
	return p
}

func (f *createForm) placeLabel(p Placement) string {
	switch p {
	case NewWindow:
		if f.anchor != nil {
			return "window in " + f.anchor.Window.Session.Name
		}
		return "new window"
	case SplitRight:
		return "split right"
	case SplitBelow:
		return "split below"
	}
	return "new session"
}

// view renders the form w wide in at most h lines.
func (f *createForm) view(w, h int, ais string) []string {
	label := func(field formField, text string) string {
		st := lipgloss.NewStyle().Foreground(cDim).Width(10)
		if f.focus == field {
			st = st.Foreground(cAccent).Bold(true)
		}
		return st.Render(text)
	}
	choices := func(field formField, items []string, at int) string {
		var parts []string
		for i, it := range items {
			switch {
			case i == at && f.focus == field:
				parts = append(parts, lipgloss.NewStyle().Foreground(cText).Background(cSelBg).Bold(true).Render(" "+it+" "))
			case i == at:
				parts = append(parts, lipgloss.NewStyle().Foreground(cText).Bold(true).Render(" "+it+" "))
			default:
				parts = append(parts, fg(cDim).Render(" "+it+" "))
			}
		}
		return strings.Join(parts, "")
	}

	var whats []string
	for _, p := range f.profiles {
		whats = append(whats, profileLabel(p))
	}
	var places []string
	for _, p := range placements {
		places = append(places, f.placeLabel(p))
	}
	at := f.place
	if f.anchor == nil {
		places, at = places[len(places)-1:], 0
	}

	head := []string{
		lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("NEW") +
			fg(cRule).Render(" "+strings.Repeat("─", max(0, w-4))),
		"",
		label(fWhat, "what") + choices(fWhat, whats, f.what),
		label(fWhere, "where") + f.where.View(),
	}
	tail := []string{}
	switch {
	case f.profile() == shellProfile:
		tail = append(tail, label(fWorktree, "worktree")+fg(cDim).Render(" – a shell runs where you put it"))
	case !f.worktreeApplies():
		tail = append(tail, label(fWorktree, "worktree")+fg(cDim).Render(" – not a git repo"))
	default:
		wt := 1
		if f.worktree {
			wt = 0
		}
		tail = append(tail, label(fWorktree, "worktree")+choices(fWorktree, []string{"new", "here"}, wt))
	}
	tail = append(tail,
		label(fPlace, "place")+choices(fPlace, places, at),
		label(fAbout, "about")+f.about.View(),
		"",
	)
	if spec, err := f.Spec(); err == nil {
		run := "a shell"
		if argv := spec.Argv(ais); argv != nil {
			run = ShellJoin(argv)
		}
		tail = append(tail,
			fg(cDim).Render(fmt.Sprintf("%-10s", "runs"))+fg(cMuted).Render(run),
			fg(cDim).Render(fmt.Sprintf("%-10s", "in"))+fg(cMuted).Render(shortDir(spec.Dir, max(10, w-10))),
		)
		if spec.Worktree {
			tail = append(tail, fg(cDim).Render(strings.Repeat(" ", 10)+"(ais makes the worktree from this repo, named after the session)"))
		}
	}
	if f.err != "" {
		tail = append(tail, fg(cWant).Render(f.err))
	}

	room := max(1, h-len(head)-len(tail))
	list := f.placeLines(w, room)
	return append(append(head, list...), tail...)
}

func (f *createForm) placeLines(w, room int) []string {
	if len(f.shown) == 0 {
		return []string{strings.Repeat(" ", 10) + fg(cDim).Render("nothing matches")}
	}
	start := 0
	if f.pick >= room {
		start = f.pick - room + 1
	}
	var out []string
	for i := start; i < len(f.shown) && len(out) < room; i++ {
		p := f.shown[i]
		why := fg(cDim).Render(p.Why)
		if p.Repo {
			why = fg(cAccent).Render("⎇ ") + why
		}
		text := shortDir(p.Dir, max(10, w-10-ansi.StringWidth(why)-3))
		mark, st := "  ", fg(cMuted)
		if i == f.pick {
			mark, st = fg(cAccent).Render("▸ "), lipgloss.NewStyle().Foreground(cText).Bold(true)
		}
		left := strings.Repeat(" ", 10) + mark + st.Render(text)
		gap := w - ansi.StringWidth(left) - ansi.StringWidth(why)
		out = append(out, left+strings.Repeat(" ", max(1, gap))+why)
	}
	return out
}
