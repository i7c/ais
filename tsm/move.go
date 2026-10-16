package main

import (
	"fmt"
	"strings"
)

// Moving things around is staged: each drop is a MoveOp, applied at once to a
// copy of the snapshot so the list shows the result, and to tmux only when
// you confirm. Ops name panes by id, which tmux keeps through every move, so
// a plan made against one picture of the server still means the same thing
// when it runs — or fails loudly if something it names has gone.

type MoveKind int

const (
	moveJoin   MoveKind = iota // a pane into another pane's window, beside it
	moveBreak                  // a pane out into a new window of a session
	moveWindow                 // a whole window into a session
)

type MoveOp struct {
	Kind MoveKind
	Src  string // pane id; for moveWindow, any pane of the window
	Dst  string // moveJoin: the pane to go beside
	// moveBreak and moveWindow: the session, by id, or a new one by name.
	Session    string
	NewSession string
}

const freshPrefix = "+"

func (op MoveOp) sessionKey() string {
	if op.NewSession != "" {
		return freshPrefix + op.NewSession
	}
	return op.Session
}

// clone copies a snapshot deeply enough to rearrange: sessions, windows and
// pane slices are new, the panes themselves are copies.
func (s *Snapshot) clone() *Snapshot {
	out := &Snapshot{Client: s.Client, BaseIndex: s.BaseIndex}
	for _, sess := range s.Sessions {
		ns := *sess
		ns.Windows = nil
		for _, w := range sess.Windows {
			nw := *w
			nw.Session = &ns
			nw.Panes = nil
			for _, p := range w.Panes {
				np := *p
				np.Window = &nw
				nw.Panes = append(nw.Panes, &np)
			}
			ns.Windows = append(ns.Windows, &nw)
		}
		out.Sessions = append(out.Sessions, &ns)
	}
	return out
}

func (s *Snapshot) session(key string) *Session {
	for _, sess := range s.Sessions {
		if sess.ID == key {
			return sess
		}
	}
	return nil
}

func (s *Snapshot) detachPane(p *Pane) {
	w := p.Window
	for i, q := range w.Panes {
		if q == p {
			w.Panes = append(w.Panes[:i:i], w.Panes[i+1:]...)
			break
		}
	}
	if len(w.Panes) == 0 {
		s.detachWindow(w)
	}
}

func (s *Snapshot) detachWindow(w *Window) {
	sess := w.Session
	for i, x := range sess.Windows {
		if x == w {
			sess.Windows = append(sess.Windows[:i:i], sess.Windows[i+1:]...)
			break
		}
	}
	if len(sess.Windows) == 0 {
		for i, x := range s.Sessions {
			if x == sess {
				s.Sessions = append(s.Sessions[:i:i], s.Sessions[i+1:]...)
				break
			}
		}
	}
}

// target finds or (for a new one) makes the session an op moves into.
func (s *Snapshot) target(op MoveOp) (*Session, error) {
	key := op.sessionKey()
	if sess := s.session(key); sess != nil {
		return sess, nil
	}
	if op.NewSession == "" {
		return nil, fmt.Errorf("session %s is gone", op.Session)
	}
	for _, sess := range s.Sessions {
		if sess.Name == op.NewSession {
			return nil, fmt.Errorf("there already is a session called %s", op.NewSession)
		}
	}
	sess := &Session{ID: key, Name: op.NewSession, Fresh: true}
	s.Sessions = append(s.Sessions, sess)
	return sess, nil
}

// nextIndex is the index tmux gives a window arriving in sess: the first
// free one from the base index up. A new session holds its placeholder at
// the base index until the plan is done, so nothing arrives there.
func (s *Snapshot) nextIndex(sess *Session) int {
	used := map[int]bool{}
	if sess.Fresh {
		used[s.BaseIndex] = true
	}
	for _, w := range sess.Windows {
		used[w.Index] = true
	}
	n := s.BaseIndex
	for used[n] {
		n++
	}
	return n
}

// Apply makes one op happen to the snapshot, as tmux will make it happen to
// the server, or says why it cannot.
func (s *Snapshot) Apply(op MoveOp) error {
	p := s.Pane(op.Src)
	if p == nil {
		return fmt.Errorf("pane %s is gone", op.Src)
	}
	from := p.Location()
	switch op.Kind {
	case moveJoin:
		dst := s.Pane(op.Dst)
		if dst == nil {
			return fmt.Errorf("pane %s is gone", op.Dst)
		}
		if dst.Window == p.Window {
			return fmt.Errorf("it is already in that window")
		}
		s.detachPane(p)
		w := dst.Window
		at := 0
		for i, q := range w.Panes {
			if q == dst {
				at = i + 1
			}
		}
		w.Panes = append(w.Panes[:at:at], append([]*Pane{p}, w.Panes[at:]...)...)
		p.Window = w
		p.Moved = from
	case moveBreak:
		if len(p.Window.Panes) == 1 {
			return fmt.Errorf("a pane alone in its window moves as the window")
		}
		sess, err := s.target(op)
		if err != nil {
			return err
		}
		s.detachPane(p)
		// Named after the pane that makes it, so it is the same window on
		// every rebuild and the cursor can stay on it.
		w := &Window{
			ID: freshPrefix + "w" + p.ID, Index: s.nextIndex(sess),
			Name: p.Command, Session: sess, Panes: []*Pane{p}, Active: false, Moved: from,
		}
		p.Window, p.Moved = w, ""
		sess.Windows = append(sess.Windows, w)
	case moveWindow:
		w := p.Window
		sess, err := s.target(op)
		if err != nil {
			return err
		}
		if w.Session == sess {
			return fmt.Errorf("it is already in that session")
		}
		from = fmt.Sprintf("%s:%d", w.Session.Name, w.Index)
		s.detachWindow(w)
		w.Index, w.Session, w.Moved = s.nextIndex(sess), sess, from
		sess.Windows = append(sess.Windows, w)
	}
	return nil
}

// Staged is the server as it will be once plan has run: the snapshot with
// every op applied, in order. ok counts the ops that still apply; the first
// that does not ends the plan there, with why.
func Staged(base *Snapshot, plan []MoveOp) (view *Snapshot, ok int, err error) {
	view = base.clone()
	for _, op := range plan {
		if err := view.Apply(op); err != nil {
			return view, ok, err
		}
		ok++
	}
	return view, ok, nil
}

// Commands is what running plan means to tmux, for showing before it runs.
// A new session is made with a placeholder pane, since tmux has no empty
// sessions, and the placeholder goes once something has arrived.
func Commands(plan []MoveOp) [][]string {
	var out [][]string
	var made []string
	for _, op := range plan {
		if op.NewSession != "" && !contains(made, op.NewSession) {
			made = append(made, op.NewSession)
			out = append(out, []string{"new-session", "-d", "-s", op.NewSession})
		}
		out = append(out, opArgs(op))
	}
	for _, name := range made {
		out = append(out, []string{"kill-pane", "-t", "<placeholder in " + name + ">"})
	}
	return out
}

func opArgs(op MoveOp) []string {
	dst := op.Session + ":"
	if op.NewSession != "" {
		dst = op.NewSession + ":"
	}
	switch op.Kind {
	case moveJoin:
		return []string{"join-pane", "-d", "-h", "-s", op.Src, "-t", op.Dst}
	case moveBreak:
		return []string{"break-pane", "-d", "-s", op.Src, "-t", dst}
	default:
		return []string{"move-window", "-d", "-s", op.Src, "-t", dst}
	}
}

// RunMoves carries out plan, in order, stopping at the first failure. done
// says how many ops ran; whatever ran stays done, as it would by hand.
func (t Tmux) RunMoves(plan []MoveOp) (done int, err error) {
	placeholders := map[string]string{}
	defer func() {
		// Whatever arrived keeps the session alive; a session nothing
		// arrived in (a later op failed) goes with its placeholder, which is
		// what should become of it.
		for _, id := range placeholders {
			t.run("kill-pane", "-t", id)
		}
	}()
	for _, op := range plan {
		if name := op.NewSession; name != "" {
			if _, ok := placeholders[name]; !ok {
				id, err := t.NewSession(name, "")
				if err != nil {
					return done, err
				}
				placeholders[name] = id
			}
		}
		if _, err := t.run(opArgs(op)...); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// Describe says what an op does in words, against the snapshot before it.
func Describe(before *Snapshot, op MoveOp) string {
	p := before.Pane(op.Src)
	if p == nil {
		return "(gone) " + op.Src
	}
	what := paneTitle(Row{Pane: p, Window: p.Window}, true)
	if op.Kind == moveWindow {
		what = fmt.Sprintf("window %s:%d %s", p.Window.Session.Name, p.Window.Index, p.Window.Name)
	}
	into := op.NewSession + " (new)"
	if op.NewSession == "" {
		if s := before.session(op.Session); s != nil {
			into = s.Name
		}
	}
	switch op.Kind {
	case moveJoin:
		if d := before.Pane(op.Dst); d != nil {
			return fmt.Sprintf("%s → beside %s in %s", what, paneTitle(Row{Pane: d, Window: d.Window}, true), d.Window.Session.Name+":"+fmt.Sprint(d.Window.Index))
		}
	case moveBreak:
		return fmt.Sprintf("%s → own window in %s", what, into)
	}
	return fmt.Sprintf("%s → %s", what, into)
}

// DescribePlan describes each op against the snapshot as of just before it.
func DescribePlan(base *Snapshot, plan []MoveOp) []string {
	view := base.clone()
	var out []string
	for _, op := range plan {
		out = append(out, Describe(view, op))
		if view.Apply(op) != nil {
			break
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinArgs(args []string) string { return strings.Join(args, " ") }
