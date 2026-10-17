package main

import (
	"fmt"
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
	remoteEvery   = 3 // ticks between remote refreshes
	captureFresh  = 2 * time.Second
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
		yes    func() tea.Cmd
	}
	ask struct { // what the rename field's answer is for
		done func(string) tea.Cmd
	}

	// Staging moves: a layer over the normal list, which then shows the
	// server as it will be once plan has run.
	moving  bool
	plan    []MoveOp
	holding *hold
	staged  *Snapshot

	form *createForm

	remotes  []*Host
	planHost string            // the host a staged plan is for; "" for this one
	captures map[string]string // by captureKey
	captured map[string]time.Time
	git      map[string]gitInfo
	ticks    int

	status    string
	statusErr bool
	width     int
	height    int
	touched   bool // a key or click came in: the cursor is yours from now on
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
	captureMsg struct{ key, text string }
	remoteMsg  struct {
		host string
		p    *Payload
		err  error
	}
	gitMsg struct {
		dir  string
		info gitInfo
	}
	tickMsg struct{}
	doneMsg struct {
		out  Outcome
		err  error
		host string // a remote the op ran on, to look at again
	}
)

func newModel(t Tmux, a Ais, snap *Snapshot, conf Config) *model {
	f := textinput.New()
	f.Prompt = "/ "
	f.Placeholder = "session, window, command, directory, description"
	m := &model{
		tmux: t, ais: a, snap: snap, filter: f, rename: textinput.New(),
		collapsed: map[string]bool{}, captures: map[string]string{}, git: map[string]gitInfo{},
		captured: map[string]time.Time{},
	}
	for _, rc := range conf.Remotes {
		h := &Host{Name: rc.Name, Remote: Remote{Conf: rc}}
		if p := h.Remote.Cached(); p != nil {
			h.take(p) // what it looked like last time, until it answers
		}
		m.remotes = append(m.remotes, h)
	}
	m.rebuild()
	return m
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadState(), m.selectionCmds(), tick()}
	for _, h := range m.remotes {
		cmds = append(cmds, m.fetch(h))
	}
	return tea.Batch(cmds...)
}

func (m *model) host(name string) *Host {
	for _, h := range m.remotes {
		if h.Name == name {
			return h
		}
	}
	return nil
}

// fetch asks a remote for its picture, unless it is already being asked.
func (m *model) fetch(h *Host) tea.Cmd {
	if h == nil || h.Loading {
		return nil
	}
	h.Loading = true
	r, name := h.Remote, h.Name
	return func() tea.Msg { p, err := r.Fetch(); return remoteMsg{name, p, err} }
}

func captureKey(r Row, p *Pane) string {
	if r.Host != nil {
		return r.Host.Name + "|" + p.ID
	}
	return p.ID
}

func (m *model) stateFor(r Row) *AisState {
	if r.Host != nil {
		return r.Host.State
	}
	return m.st
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
		key, id := captureKey(r, p), p.ID
		switch {
		case r.Host == nil:
			t := m.tmux
			cmds = append(cmds, func() tea.Msg {
				text, _ := t.Capture(id)
				return captureMsg{key, text}
			})
		case time.Since(m.captured[key]) > captureFresh:
			// A round trip each: only for what is selected, and not again
			// until the last one is a little old.
			m.captured[key] = time.Now()
			rem := r.Host.Remote
			cmds = append(cmds, func() tea.Msg {
				text, err := rem.Capture(id)
				if err != nil {
					return nil
				}
				return captureMsg{key, text}
			})
		}
	}
	if dir := m.rowDir(r); dir != "" && r.Host == nil {
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
// Until you touch it, it sits where you are looking: the pane you came from,
// or, when that pane is the way to a remote, the pane you see there — which
// can arrive after the popup opens.
func (m *model) rebuild() {
	shown, remotes := m.snap, m.remotes
	if m.moving {
		view := m.stage()
		if m.planHost == "" {
			shown = view
		} else {
			remotes = nil
			for _, h := range m.remotes {
				if h.Name == m.planHost {
					staged := *h
					staged.Snap = view
					h = &staged
				}
				remotes = append(remotes, h)
			}
		}
	}
	m.rows = BuildAll(shown, m.st, remotes, TreeOptions{
		Filter: m.filter.Value(), Collapsed: m.collapsed, Now: time.Now(), Resumable: resumableShow,
	})
	if !m.touched {
		m.cursor = m.startRow()
	} else if i := m.findKey(m.key); i >= 0 {
		m.cursor = i
	}
	m.clamp(1)
}

func (m *model) startRow() int {
	here := m.snap.Client.PaneID
	for _, h := range m.remotes {
		if hosting := hostingPane(m.snap, h.Remote.Conf); hosting != nil && hosting.ID == here {
			if i := m.paneRow(h, h.Viewing(hosting)); i >= 0 {
				return i
			}
		}
	}
	if i := m.paneRow(nil, here); i >= 0 {
		return i
	}
	return 0
}

// paneRow is where pane id of host h sits in the tree (not among the callers
// above it), or -1.
func (m *model) paneRow(h *Host, id string) int {
	for i, r := range m.rows {
		if id != "" && r.Pane != nil && !r.Wants && r.Host == h && r.Pane.ID == id {
			return i
		}
	}
	return -1
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

func (m *model) askYes(prompt string, yes func() tea.Cmd) {
	m.mode = modeConfirm
	m.confirm.prompt, m.confirm.yes = prompt, yes
}

func (m *model) askName(prompt, current string, done func(string) tea.Cmd) tea.Cmd {
	m.mode = modeRename
	m.ask.done = done
	m.rename.Prompt = prompt
	m.rename.SetValue(current)
	m.rename.CursorEnd()
	return m.rename.Focus()
}

func (m *model) setStatus(text string, isErr bool) {
	m.status, m.statusErr = text, isErr
}

func (m *model) actions() Actions { return Actions{tmux: m.tmux, ais: m.ais, snap: m.snap} }

// act does op on the row's host: here, or on the remote it lives on — after
// which, if the op moved the remote's view, ours moves to the pane that
// remote is reached through, so you see what it did.
func (m *model) act(r Row, op Op) tea.Cmd {
	if r.Host == nil {
		acts, st := m.actions(), m.st
		return func() tea.Msg { out, err := Exec(acts, st, op); return doneMsg{out: out, err: err} }
	}
	h := r.Host
	hosting := hostingPane(m.snap, h.Remote.Conf)
	if hosting != nil {
		op.Link = link(hosting)
	}
	t, client, rem, name := m.tmux, m.snap.Client.Name, h.Remote, h.Name
	return func() tea.Msg {
		out, err := rem.Do(op)
		if err == nil && out.Quit {
			if hosting == nil {
				return doneMsg{err: fmt.Errorf("done on %s, but no pane here is connected to it: tsm attach %s", name, name), host: name}
			}
			err = t.Focus(client, hosting.ID)
		}
		return doneMsg{out: out, err: err, host: name}
	}
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
		if m.ticks%remoteEvery == 0 {
			for _, h := range m.remotes {
				cmds = append(cmds, m.fetch(h))
			}
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
		m.captures[msg.key] = msg.text
		return m, nil

	case gitMsg:
		m.git[msg.dir] = msg.info
		return m, nil

	case remoteMsg:
		h := m.host(msg.host)
		if h == nil {
			return m, nil
		}
		h.Loading, h.Err = false, msg.err
		if msg.err == nil {
			h.take(msg.p)
		}
		m.rebuild()
		return m, m.selectionCmds()

	case doneMsg:
		if m.moving && msg.out.Status != "" || m.moving && msg.err != nil {
			m.leaveMove()
		}
		if msg.err != nil {
			if msg.out.Status != "" {
				msg.err = fmt.Errorf("%s, then: %w", msg.out.Status, msg.err)
			}
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
		return m, tea.Batch(m.loadSnap(), m.fetch(m.host(msg.host)))

	case tea.MouseMsg:
		m.touched = m.touched || msg.Action != tea.MouseActionMotion
		return m, m.mouse(msg)

	case tea.KeyMsg:
		m.touched = true
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
			return m.act(r, OpFor("focus", r))
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
			return m.ask.done(strings.TrimSpace(m.rename.Value()))
		}
		var cmd tea.Cmd
		m.rename, cmd = m.rename.Update(msg)
		return cmd

	case modeConfirm:
		m.mode = modeNormal
		if s := msg.String(); s == "y" || s == "Y" {
			return m.confirm.yes()
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
			return m.act(Row{Host: m.form.host}, Op{Action: "create", Create: spec.Wire()})
		}
		return cmd
	}

	r, ok := m.selected()
	m.setStatus("", false)
	if m.moving {
		if cmd, handled := m.moveKey(msg, r, ok); handled {
			return cmd
		}
	}
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
		m.collapsed[collapseKey(r.Host, r.Session)] = true
		m.rebuild()
		return nil
	case "l", "right":
		if ok && r.Kind == RowSession && m.collapsed[collapseKey(r.Host, r.Session)] {
			delete(m.collapsed, collapseKey(r.Host, r.Session))
			m.rebuild()
			return nil
		}
		return m.move(1)
	case " ":
		if ok && r.Kind == RowSession {
			k := collapseKey(r.Host, r.Session)
			m.collapsed[k] = !m.collapsed[k]
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
		if ok && r.Host != nil {
			// New things on a remote go where the selection is, there.
			var anchor *Pane
			if r.Kind != RowResumable {
				anchor = r.Target()
			}
			if r.Host.Snap == nil {
				m.setStatus(r.Host.Name+" has not answered yet", true)
				return nil
			}
			m.form = newCreateForm(r.Host.Snap, r.Host.State, anchor, r.Host.State != nil, withSelected(r.Host.Places, anchor))
			m.form.host = r.Host
			m.mode = modeCreate
			return nil
		}
		anchor := m.snap.Pane(m.snap.Client.PaneID)
		if ok && r.Kind != RowResumable && r.Target() != nil {
			anchor = r.Target()
		}
		m.form = newCreateForm(m.snap, m.st, anchor, m.ais.Available(), nil)
		m.mode = modeCreate
		return nil
	case "ctrl+r":
		return tea.Batch(m.loadSnap(), m.loadState(), m.selectionCmds())
	}

	if !ok || m.moving {
		return nil // while staging, only moves act
	}
	switch msg.String() {
	case "m":
		m.moving, m.plan = true, nil
		m.rebuild()
		cmd, _ := m.moveKey(msg, r, ok)
		return cmd
	case "enter":
		return m.act(r, OpFor("focus", r))
	case "z":
		return m.act(r, OpFor("zoom", r))
	case "v", "V", "t":
		op := OpFor("sibling", r)
		op.Place = map[string]Placement{"v": SplitRight, "V": SplitBelow, "t": NewWindow}[msg.String()]
		return m.act(r, op)
	case "d":
		return m.act(r, OpFor("duplicate", r))
	case "x":
		if r.Kind == RowResumable {
			m.setStatus("ended sessions are ais's to drop: ais prune", false)
			return nil
		}
		if prompt := KillPrompt(r); prompt != "" {
			m.askYes(prompt, func() tea.Cmd {
				return m.act(r, OpFor("kill", r))
			})
			return nil
		}
		return m.act(r, OpFor("kill", r))
	case "r":
		kind, current, can := RenameTarget(r)
		if !can {
			return nil
		}
		return m.askName("rename "+kind+": ", current, func(name string) tea.Cmd {
			op := OpFor("rename", r)
			op.Name = name
			return m.act(r, op)
		})
	}
	return nil
}
