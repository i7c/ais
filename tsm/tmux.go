package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner runs an external command and returns its stdout. Everything tsm does
// to the world goes through one, so tests can watch it instead.
type Runner interface {
	Run(name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s: %s", name, msg)
	}
	return stdout.String(), nil
}

// Tmux is tsm's handle on the server: queries and the commands it sends.
type Tmux struct{ r Runner }

func (t Tmux) run(args ...string) (string, error) { return t.r.Run("tmux", args...) }

func (t Tmux) Snapshot() (*Snapshot, error) {
	out, err := t.run(QueryArgs()...)
	if err != nil {
		return nil, err
	}
	return ParseSnapshot(out), nil
}

// Capture is the visible screen of a pane, colours and all.
func (t Tmux) Capture(pane string) (string, error) {
	return t.run("capture-pane", "-p", "-e", "-t", pane)
}

// Focus points the client at a pane, wherever it lives: its session, its
// window, then the pane itself, in one round trip.
func (t Tmux) Focus(client, pane string) error {
	args := []string{}
	if client != "" {
		args = append(args, "switch-client", "-c", client, "-t", pane, ";")
	}
	args = append(args, "select-window", "-t", pane, ";", "select-pane", "-t", pane)
	_, err := t.run(args...)
	return err
}

// Zoom focuses a pane and makes it fill its window, unless it already does.
func (t Tmux) Zoom(client string, p *Pane) error {
	if err := t.Focus(client, p.ID); err != nil {
		return err
	}
	if p.Window.Zoomed {
		return nil
	}
	_, err := t.run("resize-pane", "-Z", "-t", p.ID)
	return err
}

// Placement says where a new pane goes relative to an existing one.
type Placement int

const (
	SplitRight Placement = iota // beside it, in its window
	SplitBelow                  // under it, in its window
	NewWindow                   // a window of its own, right after it
)

// Open makes a new pane next to anchor, in dir, with env, and returns its id.
// The pane runs the default shell; anything else is typed into it (see Type),
// so it runs as a job of an interactive shell exactly as if you had typed it,
// and the shell is still there when it ends.
func (t Tmux) Open(anchor *Pane, dir string, env map[string]string, where Placement) (string, error) {
	var args []string
	switch where {
	case NewWindow:
		// new-window takes a window, not a pane: -a puts it right after.
		args = []string{"new-window", "-a", "-t", anchor.Window.ID}
	case SplitBelow:
		args = []string{"split-window", "-v", "-t", anchor.ID}
	default:
		args = []string{"split-window", "-h", "-t", anchor.ID}
	}
	args = append(args, "-P", "-F", "#{pane_id}")
	if dir != "" {
		args = append(args, "-c", dir)
	}
	for _, k := range sortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}
	out, err := t.run(args...)
	return strings.TrimSpace(out), err
}

// Type sends a command line to a pane's shell and presses enter.
func (t Tmux) Type(pane string, argv ...string) error {
	_, err := t.run("send-keys", "-t", pane, "-l", ShellJoin(argv))
	if err != nil {
		return err
	}
	_, err = t.run("send-keys", "-t", pane, "Enter")
	return err
}

func (t Tmux) KillPane(id string) error    { _, err := t.run("kill-pane", "-t", id); return err }
func (t Tmux) KillWindow(id string) error  { _, err := t.run("kill-window", "-t", id); return err }
func (t Tmux) KillSession(id string) error { _, err := t.run("kill-session", "-t", id); return err }

func (t Tmux) RenameWindow(id, name string) error {
	_, err := t.run("rename-window", "-t", id, name)
	return err
}

func (t Tmux) RenameSession(id, name string) error {
	_, err := t.run("rename-session", "-t", id, name)
	return err
}
