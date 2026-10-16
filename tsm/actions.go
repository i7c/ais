package main

import (
	"fmt"
)

// Actions are what the keys do. Each returns whether tsm is done — anything
// that moves you somewhere closes the popup behind you — and a line to show.
type Actions struct {
	tmux Tmux
	ais  Ais
	snap *Snapshot
}

type Outcome struct {
	Quit   bool
	Status string
}

func (a Actions) client() string { return a.snap.Client.Name }

func (a Actions) Focus(r Row) (Outcome, error) {
	if r.Kind == RowResumable {
		return a.Restart(r.Ais)
	}
	p := r.Target()
	if p == nil {
		return Outcome{}, fmt.Errorf("nothing to focus")
	}
	// A session or window goes to whatever it was showing (Target), not to
	// its first pane.
	return Outcome{Quit: true}, a.tmux.Focus(a.client(), p.ID)
}

func (a Actions) Zoom(r Row) (Outcome, error) {
	p := r.Target()
	if p == nil || r.Kind == RowResumable {
		return Outcome{}, fmt.Errorf("nothing to zoom")
	}
	return Outcome{Quit: true}, a.tmux.Zoom(a.client(), p)
}

// place is where a new pane beside p works, and with what env: an ais pane's
// own directory and sibling env, as ais tells it; anything else's directory.
func (a Actions) place(p *Pane) (string, map[string]string) {
	if p.AisID != "" && a.ais.Available() {
		if cwd, env, err := a.ais.Env(p.AisID); err == nil && cwd != "" {
			return cwd, env
		}
	}
	return p.Dir(), nil
}

// Sibling opens a plain shell beside a pane, in the same place.
func (a Actions) Sibling(r Row, where Placement) (Outcome, error) {
	p := r.Target()
	if p == nil || r.Kind == RowResumable {
		return Outcome{}, fmt.Errorf("no pane to open a shell beside")
	}
	dir, env := a.place(p)
	id, err := a.tmux.Open(p, dir, env, where)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Quit: true}, a.tmux.Focus(a.client(), id)
}

// Duplicate starts another of whatever a pane runs, in its place: a fork of
// an agent's conversation where its profile can fork, a fresh one of the same
// profile where it cannot, and a shell for anything ais did not start.
func (a Actions) Duplicate(r Row) (Outcome, error) {
	p := r.Target()
	if p == nil || r.Kind == RowResumable {
		return Outcome{}, fmt.Errorf("nothing to duplicate")
	}
	s := r.Ais
	if s == nil || !a.ais.Available() {
		return a.Sibling(r, NewWindow)
	}
	id, err := a.tmux.Open(p, p.Dir(), nil, NewWindow)
	if err != nil {
		return Outcome{}, err
	}
	argv := []string{a.ais.Command(), "run", "--fork", s.ID}
	if !s.CanFork {
		// No fork to be had: a fresh one, started the same way, right here
		// rather than in a worktree of its own.
		argv = []string{a.ais.Command(), "run", s.Profile, "--no-worktree"}
		if s.Description != "" {
			argv = append(argv, "-d", s.Description)
		}
		if len(s.Args) > 0 {
			argv = append(append(argv, "--"), s.Args...)
		}
	}
	if err := a.tmux.Type(id, argv...); err != nil {
		return Outcome{}, err
	}
	return Outcome{Quit: true}, a.tmux.Focus(a.client(), id)
}

// Restart brings an ended ais session back in a new window beside wherever
// you are, by typing "ais restart" into it: the session runs as a job of an
// ordinary shell, and the shell is still there once it ends.
func (a Actions) Restart(s *AisSession) (Outcome, error) {
	if s == nil || !a.ais.Available() {
		return Outcome{}, fmt.Errorf("ais is not available")
	}
	anchor := a.snap.Pane(a.snap.Client.PaneID)
	if anchor == nil && len(a.snap.Sessions) > 0 {
		if w := a.snap.Sessions[0].ActiveWindow(); w != nil {
			anchor = w.ActivePane()
		}
	}
	if anchor == nil {
		return Outcome{}, fmt.Errorf("no tmux window to open it beside")
	}
	id, err := a.tmux.Open(anchor, s.Cwd, nil, NewWindow)
	if err != nil {
		return Outcome{}, err
	}
	if err := a.tmux.Type(id, a.ais.Command(), "restart", s.ID); err != nil {
		return Outcome{}, err
	}
	return Outcome{Quit: true}, a.tmux.Focus(a.client(), id)
}

// KillPrompt is the question to ask before killing what a row stands for, or
// "" when there is nothing worth asking about: a shell idling at its prompt.
func KillPrompt(r Row) string {
	switch {
	case r.Kind == RowSession:
		n := 0
		for _, w := range r.Session.Windows {
			n += len(w.Panes)
		}
		return fmt.Sprintf("kill session %s and its %d pane(s)?", r.Session.Name, n)
	case r.Kind == RowWindow && r.Pane == nil:
		return fmt.Sprintf("kill window %d %s and its %d panes?", r.Window.Index, r.Window.Name, len(r.Window.Panes))
	case r.Pane != nil && !r.Pane.Idle():
		what := r.Pane.Command
		if r.Ais != nil {
			what = r.Ais.Profile + " · " + r.Ais.Label()
		}
		return fmt.Sprintf("kill %s (%s)?", r.Pane.ID, what)
	}
	return ""
}

func (a Actions) Kill(r Row) (Outcome, error) {
	var err error
	switch {
	case r.Kind == RowSession:
		err = a.tmux.KillSession(r.Session.ID)
	case r.Kind == RowWindow && r.Pane == nil:
		err = a.tmux.KillWindow(r.Window.ID)
	case r.Pane != nil:
		err = a.tmux.KillPane(r.Pane.ID)
	default:
		return Outcome{}, fmt.Errorf("nothing to kill here")
	}
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: "killed"}, nil
}

// RenameTarget is what "r" renames on a row, and its current name.
func RenameTarget(r Row) (kind, current string, ok bool) {
	switch {
	case r.Kind == RowSession:
		return "session", r.Session.Name, true
	case r.Kind == RowWindow || r.Kind == RowPane:
		return "window", r.Window.Name, true
	}
	return "", "", false
}

func (a Actions) Rename(r Row, name string) (Outcome, error) {
	if name == "" {
		return Outcome{}, fmt.Errorf("a name cannot be empty")
	}
	var err error
	switch kind, _, _ := RenameTarget(r); kind {
	case "session":
		err = a.tmux.RenameSession(r.Session.ID, name)
	case "window":
		err = a.tmux.RenameWindow(r.Window.ID, name)
	default:
		return Outcome{}, fmt.Errorf("nothing to rename here")
	}
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: "renamed to " + name}, nil
}
