package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Colours: the accent is the orange your tmux status bar already uses for the
// current window, so tsm looks like it belongs to the same screen.
var (
	cAccent = lipgloss.AdaptiveColor{Dark: "#ffaa00", Light: "#b35900"}
	cWant   = lipgloss.AdaptiveColor{Dark: "#ff6b6b", Light: "#c62828"}
	cDim    = lipgloss.AdaptiveColor{Dark: "#6c6c6c", Light: "#9e9e9e"}
	cMuted  = lipgloss.AdaptiveColor{Dark: "#a8a8a8", Light: "#5f5f5f"}
	cText   = lipgloss.AdaptiveColor{Dark: "#e4e4e4", Light: "#1c1c1c"}
	cSelBg  = lipgloss.AdaptiveColor{Dark: "#323232", Light: "#e8e8e8"}
	cRule   = lipgloss.AdaptiveColor{Dark: "#3a3a3a", Light: "#d0d0d0"}
	cOK     = lipgloss.AdaptiveColor{Dark: "#87d787", Light: "#2e7d32"}

	profileColors = map[string]lipgloss.AdaptiveColor{
		"claude": {Dark: "#e8956b", Light: "#b5532f"},
		"pi":     {Dark: "#82aaff", Light: "#2f5fbf"},
	}
	cProfile = lipgloss.AdaptiveColor{Dark: "#5fd7af", Light: "#00875f"}
)

func fg(c lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

func profileColor(name string) lipgloss.TerminalColor {
	if c, ok := profileColors[name]; ok {
		return c
	}
	return cProfile
}

func (m *model) listWidth() int {
	if !m.showPreview() {
		return m.width
	}
	return max(36, m.width*9/20)
}

func (m *model) showPreview() bool { return m.width >= 90 }

func (m *model) bodyHeight() int { return max(1, m.height-2) }

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	bh := m.bodyHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+bh {
		m.offset = m.cursor - bh + 1
	}
	m.offset = max(0, min(m.offset, max(0, len(m.rows)-bh)))

	lw := m.listWidth()
	list := make([]string, bh)
	for i := range bh {
		at := m.offset + i
		if at < len(m.rows) {
			list[i] = m.renderRow(m.rows[at], at == m.cursor, lw)
		} else {
			list[i] = strings.Repeat(" ", lw)
		}
	}

	var b strings.Builder
	b.WriteString(m.header())
	b.WriteByte('\n')
	if m.showPreview() {
		pw := m.width - lw - 3
		var preview []string
		if m.mode == modeHelp {
			preview = helpLines()
		} else {
			preview = m.preview(pw, bh)
		}
		rule := fg(cRule).Render("│")
		for i := range bh {
			line := ""
			if i < len(preview) {
				line = preview[i]
			}
			b.WriteString(list[i] + " " + rule + " " + fit(line, pw) + "\n")
		}
	} else {
		lines := list
		if m.mode == modeHelp {
			lines = nil
			for _, l := range helpLines() {
				lines = append(lines, " "+l)
			}
		}
		for i := range bh {
			line := ""
			if i < len(lines) {
				line = lines[i]
			}
			b.WriteString(fit(line, m.width) + "\n")
		}
	}
	b.WriteString(m.footer())
	return b.String()
}

// fit makes an ANSI-coloured line exactly w cells wide, cut or padded, with
// its colours reset at the end so nothing bleeds into the next column.
func fit(line string, w int) string {
	if w <= 0 {
		return ""
	}
	line = ansi.Truncate(line, w, "")
	if pad := w - ansi.StringWidth(line); pad > 0 {
		line += "\x1b[0m" + strings.Repeat(" ", pad)
	}
	return line + "\x1b[0m"
}

func (m *model) header() string {
	sessions, panes := m.snap.Counts()
	left := lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render(" tsm")
	switch {
	case m.mode == modeFilter:
		left += "  " + m.filter.View()
	case m.filter.Value() != "":
		left += "  " + fg(cMuted).Render("/ "+m.filter.Value())
	}
	wants := 0
	for _, r := range m.rows {
		if r.Wants {
			wants++
		}
	}
	right := fg(cDim).Render(fmt.Sprintf("%d panes · %d sessions", panes, sessions))
	if wants > 0 {
		right += fg(cDim).Render(" · ") + fg(cWant).Render(fmt.Sprintf("● %d need you", wants))
	}
	right += " "
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		return fit(left, m.width)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) footer() string {
	key := func(k, what string) string {
		return lipgloss.NewStyle().Foreground(cAccent).Render(k) + " " + fg(cMuted).Render(what)
	}
	var line string
	switch {
	case m.mode == modeConfirm:
		line = " " + fg(cWant).Render(m.confirm.prompt) + "  " + key("y", "yes") + "  " + key("any", "no")
	case m.mode == modeRename:
		line = " " + m.rename.View()
	case m.status != "":
		c := cOK
		if m.statusErr {
			c = cWant
		}
		line = " " + fg(c).Render(m.status)
	case m.stErr != nil:
		line = " " + fg(cWant).Render("ais: "+m.stErr.Error())
	default:
		hints := []string{
			key("⏎", "go"), key("v/V", "split"), key("t", "shell"), key("d", "dup"),
			key("x", "kill"), key("r", "rename"), key("z", "zoom"), key("/", "filter"), key("?", "more"),
		}
		if r, ok := m.selected(); ok && r.Kind == RowResumable {
			hints = []string{key("⏎", "restart"), key("/", "filter"), key("?", "more"), key("esc", "close")}
		}
		line = " " + strings.Join(hints, "  ")
	}
	return fit(line, m.width)
}

func helpLines() []string {
	k := func(keys, what string) string {
		return lipgloss.NewStyle().Foreground(cAccent).Width(12).Render(keys) + fg(cText).Render(what)
	}
	h := func(t string) string { return lipgloss.NewStyle().Bold(true).Foreground(cMuted).Render(t) }
	return []string{
		h("move"),
		k("j k ↑ ↓", "up and down"),
		k("tab S-tab", "next / previous session"),
		k("h l ← →", "collapse / expand a session"),
		k("g G", "top / bottom"),
		k("/", "filter; esc clears it"),
		"",
		h("act on the selection"),
		k("⏎", "go there (restart, for an ended session)"),
		k("z", "go there, zoomed"),
		k("v / V", "shell beside it, split right / below"),
		k("t", "shell beside it, in a new window"),
		k("d", "duplicate: fork an agent, else a shell"),
		k("x", "kill (asks unless it is an idle shell)"),
		k("r", "rename the session or window"),
		k("ctrl-r", "refresh now"),
		"",
		fg(cDim).Render("a new shell beside an ais pane starts in its"),
		fg(cDim).Render("worktree, with its profile's env"),
		"",
		fg(cDim).Render("any key closes this"),
	}
}

func shortDir(dir string, w int) string {
	d := tilde(dir)
	if ansi.StringWidth(d) <= w {
		return d
	}
	parts := strings.Split(d, "/")
	for len(parts) > 2 {
		parts = parts[1:]
		if c := "…/" + strings.Join(parts, "/"); ansi.StringWidth(c) <= w {
			return c
		}
	}
	return ansi.Truncate(d, w, "…")
}

// paneTitle is what a pane is called in the list: what an ais session is for,
// a window name somebody chose, else where it is.
func paneTitle(r Row, merged bool) string {
	p := r.Pane
	if r.Ais != nil {
		return r.Ais.Label()
	}
	if p.Desc != "" {
		return p.Desc
	}
	if merged && r.Window.Name != "" && r.Window.Name != p.Command {
		return r.Window.Name
	}
	return shortDir(p.Dir(), 40)
}

func (m *model) renderRow(r Row, selected bool, w int) string {
	base := lipgloss.NewStyle()
	if selected {
		base = base.Background(cSelBg)
	}
	st := func(c lipgloss.TerminalColor) lipgloss.Style { return base.Foreground(c) }

	mark := " "
	switch {
	case r.Kind == RowHeader:
	case r.Wants || (r.Ais != nil && r.Pane != nil && m.wants(r.Ais)):
		mark = st(cWant).Render("●")
	case r.Pane != nil && r.Pane.ID == m.snap.Client.PaneID:
		mark = st(cAccent).Render("◆")
	}
	if selected {
		mark = st(cAccent).Render("▌")
		if r.Wants || (r.Ais != nil && r.Pane != nil && m.wants(r.Ais)) {
			mark = st(cWant).Render("▌")
		}
	}
	indent := base.Render(strings.Repeat("  ", r.Depth))

	var left, right string
	switch r.Kind {
	case RowHeader:
		t := " " + strings.ToUpper(r.Title) + " "
		left = lipgloss.NewStyle().Foreground(cDim).Render("──") +
			lipgloss.NewStyle().Bold(true).Foreground(cMuted).Render(t) +
			lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("─", max(0, w-ansi.StringWidth(t)-3)))
		return fit(" "+left, w)
	case RowSession:
		arrow := "▾"
		if m.collapsed[r.Session.ID] && m.filter.Value() == "" {
			arrow = "▸"
		}
		name := st(cText).Bold(true)
		if r.Session.ID == m.snap.Client.SessionID {
			name = st(cAccent).Bold(true)
		}
		left = st(cDim).Render(arrow+" ") + name.Render(r.Session.Name)
		n := len(r.Session.Windows)
		info := fmt.Sprintf("%d window", n)
		if n != 1 {
			info += "s"
		}
		if r.Session.Attached > 0 {
			info += " · attached"
		}
		right = st(cDim).Render(info)
	case RowWindow, RowPane:
		prefix := ""
		if r.Kind == RowWindow {
			prefix = st(cDim).Render(fmt.Sprintf("%d ", r.Window.Index))
		} else {
			prefix = st(cDim).Render("· ")
		}
		if r.Wants {
			prefix = st(cDim).Render(r.Session.Name + ":" + fmt.Sprint(r.Window.Index) + " ")
		}
		if r.Pane == nil {
			left = prefix + st(cText).Render(r.Window.Name)
			right = st(cDim).Render(fmt.Sprintf("%d panes", len(r.Window.Panes)))
			if r.Window.Zoomed {
				right = st(cAccent).Render("zoomed ") + right
			}
			break
		}
		left = prefix + st(cText).Render(paneTitle(r, r.Kind == RowWindow))
		switch {
		case r.Ais != nil:
			right = st(profileColor(r.Ais.Profile)).Render(r.Ais.Profile)
		case r.Pane.Profile != "":
			right = st(profileColor(r.Pane.Profile)).Render(r.Pane.Profile)
		case r.Pane.Idle():
			right = st(cDim).Render(r.Pane.Command)
		default:
			right = st(cProfile).Render(r.Pane.Command)
		}
	case RowResumable:
		left = st(cDim).Render("○ ") + st(cMuted).Render(r.Ais.Label())
		if r.Aside != "" {
			left += st(cDim).Render(" · " + r.Aside)
		}
		ago := "-"
		if d, ok := since(r.Ais.EndedAt, time.Now()); ok {
			ago = ageStr(d)
		}
		right = st(profileColor(r.Ais.Profile)).Render(r.Ais.Profile) + st(cDim).Render(fmt.Sprintf(" %4s", ago))
	}

	head := mark + indent
	room := w - ansi.StringWidth(head) - ansi.StringWidth(right) - 2
	left = ansi.Truncate(left, max(0, room), "…")
	gap := w - ansi.StringWidth(head) - ansi.StringWidth(left) - ansi.StringWidth(right) - 1
	return head + left + base.Render(strings.Repeat(" ", max(1, gap))) + right + base.Render(" ") + "\x1b[0m"
}

func (m *model) wants(a *AisSession) bool {
	window := 60.0
	if m.st != nil && m.st.NotifyWindow > 0 {
		window = m.st.NotifyWindow
	}
	return a.WantsYou(window, time.Now())
}

func (m *model) preview(w, h int) []string {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	now := time.Now()
	label := func(k string) string { return fg(cDim).Render(fmt.Sprintf("%-9s", k)) }
	text := func(s string) string { return fg(cText).Render(s) }
	var lines []string
	title := func(s string) { lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(cText).Render(s)) }
	field := func(k, v string) { lines = append(lines, label(k)+text(v)) }
	gitLine := func(dir string) {
		if g, ok := m.git[dir]; ok && g.branch != "" {
			v := fg(cAccent).Render("⎇ " + g.branch)
			if g.dirty > 0 {
				v += fg(cWant).Render(fmt.Sprintf("  ✚%d", g.dirty))
			} else {
				v += fg(cOK).Render("  clean")
			}
			lines = append(lines, label("git")+v)
		}
	}

	if r.Kind == RowResumable {
		a := r.Ais
		title(a.Label())
		lines = append(lines, "")
		field("profile", a.Profile)
		field("dir", tilde(a.Cwd))
		gitLine(a.Cwd)
		ended := "?"
		if d, ok := since(a.EndedAt, now); ok {
			ended = ageStr(d) + " ago"
		}
		if a.ExitCode != nil {
			ended += fmt.Sprintf(" · exit %d", *a.ExitCode)
		}
		field("ended", ended)
		field("id", a.ID)
		lines = append(lines, "")
		how := "a fresh conversation"
		if a.CanResume {
			how = "resuming its conversation"
		}
		lines = append(lines, fg(cMuted).Render("⏎ restarts it in a new window, "+how))
		return lines
	}

	p := r.Target()
	if p == nil {
		return nil
	}
	a := r.Ais
	if a == nil && p.AisID != "" && m.st != nil {
		a = m.st.ByID()[p.AisID]
	}
	switch {
	case r.Kind == RowSession:
		title(r.Session.Name)
		n := 0
		for _, w := range r.Session.Windows {
			n += len(w.Panes)
		}
		field("session", fmt.Sprintf("%d windows · %d panes", len(r.Session.Windows), n))
		lines = append(lines, fg(cDim).Render("showing ")+fg(cMuted).Render(p.Location()))
	case a != nil:
		title(a.Label())
	default:
		title(paneTitle(Row{Pane: p, Window: p.Window}, true))
	}
	if r.Kind != RowSession {
		what := p.Command
		if a != nil {
			what = fg(profileColor(a.Profile)).Render(a.Profile) + fg(cDim).Render(" via ais "+a.ID)
		} else {
			what = text(what)
		}
		lines = append(lines, label("runs")+what+fg(cDim).Render(fmt.Sprintf("  pid %d", p.PID)))
		field("dir", tilde(p.Dir()))
		gitLine(p.Dir())
		lines = append(lines, label("at")+text(p.Location())+fg(cDim).Render(fmt.Sprintf("  %s  %dx%d", p.ID, p.Width, p.Height)))
		if p.Window.Activity > 0 {
			field("active", ageStr(now.Sub(time.Unix(p.Window.Activity, 0)))+" ago")
		}
		if a != nil {
			if d, ok := since(a.StartedAt, now); ok {
				started := ageStr(d) + " ago"
				if a.RestartOf != "" {
					started += " · restart of " + a.RestartOf
				}
				if a.ForkOf != "" {
					started += " · fork of " + a.ForkOf
				}
				field("started", started)
			}
			notes := a.Notifications
			if len(notes) > 3 {
				notes = notes[len(notes)-3:]
			}
			for i := len(notes) - 1; i >= 0; i-- {
				n := notes[i]
				ago := "?"
				if d, ok := since(n.At, now); ok {
					ago = ageStr(d)
				}
				dot := fg(cDim).Render("·")
				if i == len(notes)-1 && m.wants(a) {
					dot = fg(cWant).Render("●")
				}
				lines = append(lines, dot+" "+fg(cDim).Render(fmt.Sprintf("%-4s", ago))+" "+fg(cMuted).Render(n.Message))
			}
		}
	}
	lines = append(lines, fg(cRule).Render(strings.Repeat("─", w)))

	room := h - len(lines)
	if room > 0 {
		screen := strings.Split(strings.TrimRight(m.captures[p.ID], "\n "), "\n")
		// Trailing blank lines are the empty bottom of the screen; the last
		// lines with something on them are the ones worth the space.
		for len(screen) > 0 && strings.TrimSpace(ansi.Strip(screen[len(screen)-1])) == "" {
			screen = screen[:len(screen)-1]
		}
		if len(screen) > room {
			screen = screen[len(screen)-room:]
		}
		lines = append(lines, screen...)
	}
	return lines
}
