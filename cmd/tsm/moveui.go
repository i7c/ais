package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// hold is what "m" picked up: a pane, or a whole window of several panes.
type hold struct {
	pane   string // the pane; for a window, any pane of it
	window bool
}

func hostName(r Row) string {
	if r.Host != nil {
		return r.Host.Name
	}
	return ""
}

// planBase is the picture a plan is staged against: this host's, or the
// remote's it was started on.
func (m *model) planBase() *Snapshot {
	if m.planHost == "" {
		return m.snap
	}
	if h := m.host(m.planHost); h != nil && h.Snap != nil {
		return h.Snap
	}
	return &Snapshot{}
}

// stage is the server as the plan will leave it. An op that no longer
// applies — something it names closed since it was staged — ends the plan
// there; the rest goes with it, and you are told.
func (m *model) stage() *Snapshot {
	view, ok, err := Staged(m.planBase(), m.plan)
	if err != nil {
		m.plan = m.plan[:ok]
		m.setStatus("dropped staged moves: "+err.Error(), true)
	}
	if m.holding != nil && view.Pane(m.holding.pane) == nil {
		m.holding = nil
	}
	m.staged = view
	return view
}

func (m *model) holdLabel() string {
	if m.holding == nil || m.staged == nil {
		return ""
	}
	p := m.staged.Pane(m.holding.pane)
	if p == nil {
		return ""
	}
	if m.holding.window {
		return fmt.Sprintf("window %s:%d %s", p.Window.Session.Name, p.Window.Index, p.Window.Name)
	}
	return paneTitle(Row{Pane: p, Window: p.Window}, true)
}

// stageOp adds op to the plan if it applies to the staged view, and lets go
// of what was held either way it worked.
func (m *model) stageOp(op MoveOp) {
	if err := m.staged.clone().Apply(op); err != nil {
		m.setStatus(err.Error(), true)
		return
	}
	m.plan = append(m.plan, op)
	m.holding = nil
	m.rebuild()
	m.setStatus(fmt.Sprintf("staged — %d move(s), ⏎ applies them", len(m.plan)), false)
}

// drop builds the op for putting what is held at row r: p beside the row's
// pane, P into the row's session as a window of its own.
func (m *model) drop(r Row, asWindow bool) (MoveOp, error) {
	h := m.holding
	if h == nil {
		return MoveOp{}, fmt.Errorf("nothing held — m picks up the selection")
	}
	if !asWindow {
		if h.window {
			return MoveOp{}, fmt.Errorf("a window goes into a session: P here, or N for a new one")
		}
		dst := r.Target()
		if dst == nil {
			return MoveOp{}, fmt.Errorf("no window here to join")
		}
		return MoveOp{Kind: moveJoin, Src: h.pane, Dst: dst.ID}, nil
	}
	if r.Session == nil {
		return MoveOp{}, fmt.Errorf("no session here")
	}
	return m.intoSession(MoveOp{Session: r.Session.ID}), nil
}

// intoSession completes an op that moves what is held into a session: a
// window moves whole, and so does a pane that is alone in its window.
func (m *model) intoSession(op MoveOp) MoveOp {
	op.Src = m.holding.pane
	op.Kind = moveBreak
	if p := m.staged.Pane(m.holding.pane); m.holding.window || (p != nil && len(p.Window.Panes) == 1) {
		op.Kind = moveWindow
	}
	return op
}

func (m *model) leaveMove() {
	m.moving, m.plan, m.holding, m.staged, m.planHost = false, nil, nil, nil, ""
	m.rebuild()
}

// moveKey handles the keys that mean something while staging; anything else
// falls through to moving the cursor about.
func (m *model) moveKey(msg tea.KeyMsg, r Row, ok bool) (tea.Cmd, bool) {
	switch msg.String() {
	case "m":
		if !ok || r.Kind == RowResumable || r.Kind == RowHeader {
			m.setStatus("nothing to pick up here", true)
			return nil, true
		}
		if r.Kind == RowSession {
			m.setStatus("sessions stay put; pick up their windows", true)
			return nil, true
		}
		p := r.Target()
		if p == nil {
			return nil, true
		}
		if hostName(r) != m.planHost {
			if len(m.plan) > 0 {
				m.setStatus("moves stay on one host: apply or discard these first", true)
				return nil, true
			}
			m.planHost = hostName(r) // an empty plan goes where you start it
			m.rebuild()
		}
		m.holding = &hold{pane: p.ID, window: r.Kind == RowWindow && r.Pane == nil}
		m.setStatus("", false)
		return nil, true
	case "p", "P":
		if !ok {
			return nil, true
		}
		if m.holding != nil && hostName(r) != m.planHost {
			m.setStatus("a pane cannot change machines; drop it on the host it came from", true)
			return nil, true
		}
		op, err := m.drop(r, msg.String() == "P")
		if err != nil {
			m.setStatus(err.Error(), true)
			return nil, true
		}
		m.stageOp(op)
		return nil, true
	case "N":
		if m.holding == nil {
			m.setStatus("nothing held — m picks up the selection", true)
			return nil, true
		}
		suggest := slug(m.holdLabel())
		return m.askName("new session: ", suggest, func(name string) tea.Cmd {
			name = slug(name)
			if name == "" {
				m.setStatus("a session needs a name", true)
				return nil
			}
			m.stageOp(m.intoSession(MoveOp{NewSession: name}))
			return nil
		}), true
	case "u":
		if m.holding != nil {
			m.holding = nil
		} else if len(m.plan) > 0 {
			m.plan = m.plan[:len(m.plan)-1]
			m.rebuild()
		}
		return nil, true
	case "enter":
		if len(m.plan) == 0 {
			m.leaveMove()
			return nil, true
		}
		return m.act(Row{Host: m.host(m.planHost)}, Op{Action: "moves", Plan: m.plan}), true
	case "esc", "q":
		switch {
		case m.filter.Value() != "":
			return nil, false // clearing the filter comes first, as always
		case m.holding != nil:
			m.holding = nil
		case len(m.plan) == 0:
			m.leaveMove()
		default:
			m.askYes(fmt.Sprintf("discard %d staged move(s)?", len(m.plan)), func() tea.Cmd {
				m.leaveMove()
				m.setStatus("discarded", false)
				return nil
			})
		}
		return nil, true
	}
	return nil, false
}

// planLines is the right-hand panel while staging: what is held, the plan
// in words, and the tmux commands it comes to.
func (m *model) planLines(w int) []string {
	h := func(t string) string { return lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render(t) }
	key := func(k, what string) string {
		return lipgloss.NewStyle().Foreground(cAccent).Render(k) + " " + fg(cMuted).Render(what)
	}
	lines := []string{h("MOVE") + fg(cRule).Render(" "+strings.Repeat("─", max(0, w-5))), ""}
	if label := m.holdLabel(); label != "" {
		lines = append(lines,
			fg(cDim).Render("holding  ")+lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render(label),
			"         "+key("p", "join beside the selection")+"   "+key("P", "own window in its session"),
			"         "+key("N", "into a new session")+"   "+key("u/esc", "put it down"),
		)
	} else {
		lines = append(lines, fg(cMuted).Render("m picks up the selected pane or window"))
	}
	lines = append(lines, "")
	if len(m.plan) == 0 {
		return append(lines, fg(cDim).Render("nothing staged yet"))
	}
	lines = append(lines, h(fmt.Sprintf("%d staged", len(m.plan))))
	if m.planHost != "" {
		lines = append(lines, fg(cDim).Render("on "+m.planHost))
	}
	for i, d := range DescribePlan(m.planBase(), m.plan) {
		lines = append(lines, fg(cDim).Render(fmt.Sprintf("%2d  ", i+1))+fg(cText).Render(d))
	}
	lines = append(lines, "", fg(cDim).Render("tmux"))
	for _, c := range Commands(m.plan) {
		lines = append(lines, fg(cDim).Render("  "+joinArgs(c)))
	}
	return append(lines, "", key("⏎", "apply")+"  "+key("u", "undo last")+"  "+key("esc", "discard"))
}
