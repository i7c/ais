package main

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type mode int

const (
	modeNormal mode = iota
	modeFilter
	modeConfirm
	modeRename
	modeHelp
	modeCreate
)

const (
	tickEvery     = time.Second
	stateEvery    = 5 // ticks between "ais state" reloads
	resumableShow = 8
	gitFresh      = 10 * time.Second
)

type gitInfo struct {
	branch string
	dirty  int
	at     time.Time
}

type model struct {
	tmux Tmux
	ais  Ais

	snap   *Snapshot
	st     *AisState
	stErr  error
	rows   []Row
	cursor int
	offset int
	key    string // the selected row's Key, kept across refreshes

	collapsed map[string]bool
	mode      mode
	filter    textinput.Model
	rename    textinput.Model
	confirm   struct {
		prompt string
		row    Row
	}

	form *createForm

	captures map[string]string
	git      map[string]gitInfo
	ticks    int

	status    string
	statusErr bool
	width     int
	height    int
	placed    bool // the cursor has found its starting row
}

type (
	snapMsg struct {
		snap *Snapshot
		err  error
	}
	stateMsg struct {
		st  *AisState
		err error
	}
	captureMsg struct{ pane, text string }
	gitMsg     struct {
		dir  string
		info gitInfo
	}
	tickMsg struct{}
	doneMsg struct {
		out Outcome
		err error
	}
)

func newModel(t Tmux, a Ais, snap *Snapshot) *model {
	f := textinput.New()
	f.Prompt = "/ "
	f.Placeholder = "session, window, command, directory, description"
	m := &model{
		tmux: t, ais: a, snap: snap, filter: f, rename: textinput.New(),
		collapsed: map[string]bool{}, captures: map[string]string{}, git: map[string]gitInfo{},
	}
	m.rebuild()
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadState(), m.selectionCmds(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) loadSnap() tea.Cmd {
	t := m.tmux
	return func() tea.Msg { s, err := t.Snapshot(); return snapMsg{s, err} }
}

func (m *model) loadState() tea.Cmd {
	a := m.ais
	if !a.Available() {
		return nil
	}
	return func() tea.Msg { st, err := a.State(); return stateMsg{st, err} }
}

// selectionCmds fetch what the preview needs for the selected row: its
// pane's screen, and the git state of its directory unless that is fresh.
func (m *model) selectionCmds() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	var cmds []tea.Cmd
	if p := r.Target(); p != nil && r.Kind != RowResumable {
		t, id := m.tmux, p.ID
		cmds = append(cmds, func() tea.Msg {
			text, _ := t.Capture(id)
			return captureMsg{id, text}
		})
	}
	if dir := m.rowDir(r); dir != "" {
		if g, ok := m.git[dir]; !ok || time.Since(g.at) > gitFresh {
			runner := m.tmux.r
			cmds = append(cmds, func() tea.Msg { return gitMsg{dir, readGit(runner, dir)} })
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) rowDir(r Row) string {
	if r.Kind == RowResumable {
		return r.Ais.Cwd
	}
	if p := r.Target(); p != nil {
		return p.Dir()
	}
	return ""
}

func readGit(r Runner, dir string) gitInfo {
	info := gitInfo{at: time.Now()}
	if out, err := r.Run("git", "-C", dir, "branch", "--show-current"); err == nil {
		info.branch = strings.TrimSpace(out)
		if out, err := r.Run("git", "-C", dir, "status", "--porcelain"); err == nil {
			if t := strings.TrimRight(out, "\n"); t != "" {
				info.dirty = strings.Count(t, "\n") + 1
			}
		}
		if info.branch == "" {
			info.branch = "(detached)"
		}
	}
	return info
}

func (m *model) selected() (Row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return Row{}, false
	}
	r := m.rows[m.cursor]
	return r, r.Selectable()
}

// rebuild lays the rows out again and puts the cursor back on what it was on.
// The first time, it starts on whatever needs you, else the pane you came from.
func (m *model) rebuild() {
	m.rows = BuildRows(m.snap, m.st, TreeOptions{
		Filter: m.filter.Value(), Collapsed: m.collapsed, Now: time.Now(), Resumable: resumableShow,
	})
	if !m.placed {
		m.cursor = m.startRow()
		m.placed = true
	} else if i := m.findKey(m.key); i >= 0 {
		m.cursor = i
	}
	m.clamp(1)
}

func (m *model) startRow() int {
	for i, r := range m.rows {
		if r.Wants {
			return i
		}
	}
	for i, r := range m.rows {
		if r.Pane != nil && !r.Wants && r.Pane.ID == m.snap.Client.PaneID {
			return i
		}
	}
	return 0
}

// firstMatch is where a filter puts the cursor: on the first thing it
// matched, not on the session heading above it.
func (m *model) firstMatch() int {
	for i, r := range m.rows {
		if r.Pane != nil || r.Kind == RowResumable {
			return i
		}
	}
	return 0
}

func (m *model) findKey(key string) int {
	for i, r := range m.rows {
		if r.Key() == key {
			return i
		}
	}
	return -1
}

// clamp keeps the cursor on a selectable row, stepping in dir past headers.
func (m *model) clamp(dir int) {
	n := len(m.rows)
	if n == 0 {
		m.cursor, m.key = 0, ""
		return
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	for i := 0; i < n && !m.rows[m.cursor].Selectable(); i++ {
		m.cursor += dir
		if m.cursor >= n {
			m.cursor, dir = n-1, -1
		}
		if m.cursor < 0 {
			m.cursor, dir = 0, 1
		}
	}
	m.key = m.rows[m.cursor].Key()
}

func (m *model) move(delta int) tea.Cmd {
	before := m.cursor
	m.cursor += delta
	dir := 1
	if delta < 0 {
		dir = -1
	}
	m.clamp(dir)
	if m.cursor == before {
		return nil
	}
	return m.selectionCmds()
}

func (m *model) setStatus(text string, isErr bool) {
	m.status, m.statusErr = text, isErr
}

func (m *model) actions() Actions { return Actions{tmux: m.tmux, ais: m.ais, snap: m.snap} }

func (m *model) do(fn func(Actions) (Outcome, error)) tea.Cmd {
	acts := m.actions()
	return func() tea.Msg { out, err := fn(acts); return doneMsg{out, err} }
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.ticks++
		cmds := []tea.Cmd{tick(), m.loadSnap(), m.selectionCmds()}
		if m.ticks%stateEvery == 0 {
			cmds = append(cmds, m.loadState())
		}
		return m, tea.Batch(cmds...)

	case snapMsg:
		if msg.err == nil && msg.snap != nil {
			// Keep the client we were opened from: refreshes run outside the
			// popup's moment and could see another client's view.
			msg.snap.Client = m.snap.Client
			m.snap = msg.snap
			m.rebuild()
		}
		return m, nil

	case stateMsg:
		m.st, m.stErr = msg.st, msg.err
		if msg.err == nil {
			m.rebuild()
		}
		return m, nil

	case captureMsg:
		m.captures[msg.pane] = msg.text
		return m, nil

	case gitMsg:
		m.git[msg.dir] = msg.info
		return m, nil

	case doneMsg:
		if msg.err != nil {
			if m.mode == modeCreate && m.form != nil {
				m.form.err = msg.err.Error()
				return m, nil
			}
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		if msg.out.Quit {
			return m, tea.Quit
		}
		m.setStatus(msg.out.Status, false)
		return m, m.loadSnap()

	case tea.MouseMsg:
		return m, m.mouse(msg)

	case tea.KeyMsg:
		// Keys typed faster than tsm reads them arrive as one run of runes
		// ("jj"); outside a text field each of them is a command of its own.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste &&
			m.mode != modeFilter && m.mode != modeRename {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
			}
			return m, tea.Batch(cmds...)
		}
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		return m.move(-1)
	case msg.Button == tea.MouseButtonWheelDown:
		return m.move(1)
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		i := m.offset + msg.Y - 1 // the header takes the first line
		if msg.X >= m.listWidth() || i < 0 || i >= len(m.rows) || !m.rows[i].Selectable() {
			return nil
		}
		if i == m.cursor {
			r := m.rows[i]
			return m.do(func(a Actions) (Outcome, error) { return a.Focus(r) })
		}
		m.cursor = i
		m.key = m.rows[i].Key()
		return m.selectionCmds()
	}
	return nil
}

func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		return tea.Quit
	}
	switch m.mode {
	case modeFilter:
		switch msg.String() {
		case "esc":
			m.filter.SetValue("")
			m.filter.Blur()
			m.mode = modeNormal
			m.rebuild()
			return m.selectionCmds()
		case "enter":
			m.filter.Blur()
			m.mode = modeNormal
			return nil
		case "up", "ctrl+p", "ctrl+k":
			return m.move(-1)
		case "down", "ctrl+n", "ctrl+j":
			return m.move(1)
		}
		var cmd tea.Cmd
		before := m.filter.Value()
		m.filter, cmd = m.filter.Update(msg)
		if m.filter.Value() == before {
			return cmd
		}
		m.rebuild()
		m.cursor = m.firstMatch()
		m.clamp(1)
		return tea.Batch(cmd, m.selectionCmds())

	case modeRename:
		switch msg.String() {
		case "esc":
			m.mode = modeNormal
			return nil
		case "enter":
			m.mode = modeNormal
			name, row := strings.TrimSpace(m.rename.Value()), m.confirm.row
			return m.do(func(a Actions) (Outcome, error) { return a.Rename(row, name) })
		}
		var cmd tea.Cmd
		m.rename, cmd = m.rename.Update(msg)
		return cmd

	case modeConfirm:
		m.mode = modeNormal
		if s := msg.String(); s == "y" || s == "Y" {
			row := m.confirm.row
			return m.do(func(a Actions) (Outcome, error) { return a.Kill(row) })
		}
		m.setStatus("left alone", false)
		return nil

	case modeHelp:
		m.mode = modeNormal
		return nil

	case modeCreate:
		cmd, launch, cancel := m.form.update(msg)
		switch {
		case cancel:
			m.mode, m.form = modeNormal, nil
		case launch:
			spec, err := m.form.Spec()
			if err != nil {
				m.form.err = err.Error()
				return nil
			}
			m.form.remember()
			return m.do(func(a Actions) (Outcome, error) { return a.Create(spec) })
		}
		return cmd
	}

	r, ok := m.selected()
	m.setStatus("", false)
	switch msg.String() {
	case "q", "esc":
		if m.filter.Value() != "" {
			m.filter.SetValue("")
			m.rebuild()
			return m.selectionCmds()
		}
		return tea.Quit
	case "j", "down", "ctrl+n":
		return m.move(1)
	case "k", "up", "ctrl+p":
		return m.move(-1)
	case "g", "home":
		return m.move(-len(m.rows))
	case "G", "end":
		return m.move(len(m.rows))
	case "ctrl+d", "pgdown":
		return m.move(max(1, m.bodyHeight()/2))
	case "ctrl+u", "pgup":
		return m.move(-max(1, m.bodyHeight()/2))
	case "tab":
		// Next session, skipping over its windows.
		for i := m.cursor + 1; i < len(m.rows); i++ {
			if m.rows[i].Kind == RowSession {
				return m.move(i - m.cursor)
			}
		}
		return nil
	case "shift+tab":
		for i := m.cursor - 1; i >= 0; i-- {
			if m.rows[i].Kind == RowSession {
				return m.move(i - m.cursor)
			}
		}
		return nil
	case "h", "left":
		if !ok || r.Session == nil {
			return nil
		}
		if r.Kind != RowSession {
			for i := m.cursor - 1; i >= 0; i-- {
				if m.rows[i].Kind == RowSession && m.rows[i].Session == r.Session {
					return m.move(i - m.cursor)
				}
			}
			return nil
		}
		m.collapsed[r.Session.ID] = true
		m.rebuild()
		return nil
	case "l", "right":
		if ok && r.Kind == RowSession && m.collapsed[r.Session.ID] {
			delete(m.collapsed, r.Session.ID)
			m.rebuild()
			return nil
		}
		return m.move(1)
	case " ":
		if ok && r.Kind == RowSession {
			m.collapsed[r.Session.ID] = !m.collapsed[r.Session.ID]
			m.rebuild()
		}
		return nil
	case "/":
		m.mode = modeFilter
		return m.filter.Focus()
	case "?":
		m.mode = modeHelp
		return nil
	case "n":
		// New things go next to the selection: that is what you were
		// looking at when you thought of them.
		anchor := m.snap.Pane(m.snap.Client.PaneID)
		if ok && r.Kind != RowResumable && r.Target() != nil {
			anchor = r.Target()
		}
		m.form = newCreateForm(m.snap, m.st, anchor, m.ais.Available())
		m.mode = modeCreate
		return nil
	case "ctrl+r":
		return tea.Batch(m.loadSnap(), m.loadState(), m.selectionCmds())
	}

	if !ok {
		return nil
	}
	switch msg.String() {
	case "enter":
		return m.do(func(a Actions) (Outcome, error) { return a.Focus(r) })
	case "z":
		return m.do(func(a Actions) (Outcome, error) { return a.Zoom(r) })
	case "v":
		return m.do(func(a Actions) (Outcome, error) { return a.Sibling(r, SplitRight) })
	case "V":
		return m.do(func(a Actions) (Outcome, error) { return a.Sibling(r, SplitBelow) })
	case "t":
		return m.do(func(a Actions) (Outcome, error) { return a.Sibling(r, NewWindow) })
	case "d":
		return m.do(func(a Actions) (Outcome, error) { return a.Duplicate(r) })
	case "x":
		if r.Kind == RowResumable {
			m.setStatus("ended sessions are ais's to drop: ais prune", false)
			return nil
		}
		if prompt := KillPrompt(r); prompt != "" {
			m.mode = modeConfirm
			m.confirm.prompt, m.confirm.row = prompt, r
			return nil
		}
		return m.do(func(a Actions) (Outcome, error) { return a.Kill(r) })
	case "r":
		kind, current, can := RenameTarget(r)
		if !can {
			return nil
		}
		m.mode = modeRename
		m.confirm.row = r
		m.rename.Prompt = "rename " + kind + ": "
		m.rename.SetValue(current)
		m.rename.CursorEnd()
		return m.rename.Focus()
	}
	return nil
}
