package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	return runCmd(name, exec.CommandContext(ctx, name, args...))
}

// RunUntilDone is Run for a command that takes as long as it takes: it is
// never killed, so a "tmux wait-for" left behind just ends by itself.
func (execRunner) RunUntilDone(name string, args ...string) (string, error) {
	return runCmd(name, exec.Command(name, args...))
}

func runCmd(name string, cmd *exec.Cmd) (string, error) {
	if name == "tmux" {
		cmd.Env = tmuxEnv(os.Environ())
	}
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

// tmuxEnv makes sure tmux believes in UTF-8. Without a UTF-8 locale (the
// usual state of a command run over ssh) tmux replaces every control
// character in -F output with "_" — tsm's field separator included — and
// every non-ASCII character in a name with it too.
func tmuxEnv(env []string) []string {
	get := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v
			}
		}
		return ""
	}
	locale := get("LC_ALL")
	if locale == "" {
		locale = get("LC_CTYPE")
	}
	if locale == "" {
		locale = get("LANG")
	}
	l := strings.ToLower(locale)
	if strings.Contains(l, "utf-8") || strings.Contains(l, "utf8") {
		return env
	}
	// LC_ALL wins over everything, so it is the one to set when something
	// non-UTF-8 is in the way; otherwise the narrower LC_CTYPE will do.
	if get("LC_ALL") != "" {
		return append(env, "LC_ALL=C.UTF-8")
	}
	return append(env, "LC_CTYPE=C.UTF-8")
}

// Tmux is tsm's handle on the server: queries and the commands it sends.
type Tmux struct{ r Runner }

func (t Tmux) run(args ...string) (string, error) { return t.r.Run("tmux", args...) }

// changedChannel is the wait-for channel tmux signals, through changeHooks,
// whenever something a picture of it shows has changed.
const changedChannel = "tsm-changed"

// changeHooks are the events that change what a picture shows, the pane each
// client is looking at first. Commands, directories and the like change
// without an event; the heartbeat catches those.
var changeHooks = []string{
	"client-session-changed", "session-window-changed", "window-pane-changed",
	"client-attached", "client-detached", "client-active",
	"session-created", "session-closed", "session-renamed",
	"window-linked", "window-unlinked", "window-renamed", "window-layout-changed",
	"pane-exited", "pane-died", "pane-title-changed", "alert-bell",
}

// hookIndex is the slot tsm takes in each of those hooks' arrays, well clear
// of the [0] your own "set-hook -g" fills.
const hookIndex = 77

// HookChanges makes tmux signal changedChannel on every change hook. It is
// safe to repeat, and is repeated: a plain "set-hook -g" (a re-sourced
// tmux.conf) replaces a hook's whole array, tsm's slot with it.
func (t Tmux) HookChanges() error {
	set := func(h string) []string {
		return []string{"set-hook", "-g", fmt.Sprintf("%s[%d]", h, hookIndex), "wait-for -S " + changedChannel}
	}
	var all []string
	for _, h := range changeHooks {
		if len(all) > 0 {
			all = append(all, ";")
		}
		all = append(all, set(h)...)
	}
	if _, err := t.run(all...); err == nil {
		return nil
	}
	// One hook this tmux does not know stops the rest: set them one by one.
	var err error
	for _, h := range changeHooks {
		if _, e := t.run(set(h)...); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// untilDoner runs a command that may take as long as it likes.
type untilDoner interface {
	RunUntilDone(name string, args ...string) (string, error)
}

// WaitChanged returns once tmux signals changedChannel: at once if it did
// since the last wait. It must not be killed while it waits: tmux keeps a
// killed waiter, and from then on forgets signals nobody waits for — the ones
// between one wait and the next.
func (t Tmux) WaitChanged() error {
	r, ok := t.r.(untilDoner)
	if !ok {
		return fmt.Errorf("tmux: cannot wait for changes here")
	}
	_, err := r.RunUntilDone("tmux", "wait-for", changedChannel)
	return err
}

// SignalChanged wakes whoever waits for a change, this process's own waiter
// among them, so it ends rather than being left behind.
func (t Tmux) SignalChanged() {
	t.run("wait-for", "-S", changedChannel)
}

func (t Tmux) Snapshot() (*Snapshot, error) {
	out, err := t.run(QueryArgs()...)
	if err != nil {
		return nil, err
	}
	snap := ParseSnapshot(out)
	if len(snap.Sessions) == 0 && strings.Count(out, "\n") > 1 {
		// tmux answered, with panes, and none of it parsed: a mangled
		// separator, most likely. Saying so beats showing nothing.
		return nil, fmt.Errorf("tmux's answer did not parse (its locale is not UTF-8?): %.80q", out)
	}
	return snap, nil
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
	NewSession                  // a session of its own
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

// NewSession starts a detached session called name, working in dir, and
// returns its first pane.
func (t Tmux) NewSession(name, dir string) (string, error) {
	args := []string{"new-session", "-d", "-s", name, "-P", "-F", "#{pane_id}"}
	if dir != "" {
		args = append(args, "-c", dir)
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
