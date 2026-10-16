package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A Host is a remote tmux as tsm last saw it.
type Host struct {
	Name    string
	Remote  Remote
	Snap    *Snapshot
	State   *AisState
	Places  []Place
	At      time.Time // when the remote end took the picture
	Err     error     // the last fetch failed, with this
	Loading bool
}

func (h *Host) take(p *Payload) {
	h.Snap, h.State, h.Places, h.At = p.Snap, p.State, p.Places, p.At
}

// Stale is true when the picture is old enough to say so.
func (h *Host) Stale(now time.Time) bool { return h.At.IsZero() || now.Sub(h.At) > 10*time.Second }

// link is what the client "tsm attach" started from pane p carries in its
// environment, unique across the machines you attach from.
func link(p *Pane) string { return hostname() + ":" + p.ID }

// hostingPane is the local pane a remote is reached through: the one "tsm
// attach" marked, else one whose foreground ssh or mosh names the remote.
func hostingPane(local *Snapshot, conf RemoteConf) *Pane {
	var candidates []*Pane
	for _, s := range local.Sessions {
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				if p.Remote == conf.Name {
					return p
				}
				switch p.Command {
				case "ssh", "mosh", "mosh-client", "autossh", "et":
					candidates = append(candidates, p)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	procs := readProcs()
	for _, p := range candidates {
		for _, pid := range descendants(procs, p.PID) {
			if namesHost(procs[pid].args, conf) {
				return p
			}
		}
	}
	return nil
}

type proc struct {
	ppid int
	args []string
}

// readProcs reads every process's parent and argv from /proc. Linux only;
// elsewhere it finds nothing, and only "tsm attach" marks are used.
func readProcs() map[int]proc {
	out := map[int]proc{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may hold spaces, so cut at ')'.
		rest := string(stat)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		out[pid] = proc{ppid: ppid, args: strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")}
	}
	return out
}

func descendants(procs map[int]proc, root int) []int {
	children := map[int][]int{}
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	var out []int
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// namesHost: an ssh, mosh or mosh-client command line with the remote's
// destination as one of its words (mosh-client carries the original
// arguments in its -# word, hence the split on spaces).
func namesHost(args []string, conf RemoteConf) bool {
	if len(args) == 0 {
		return false
	}
	switch filepath.Base(args[0]) {
	case "ssh", "mosh", "mosh-client", "autossh", "et":
	default:
		return false
	}
	for _, a := range args[1:] {
		for _, word := range strings.Fields(a) {
			if word == conf.SSH || word == conf.Name || strings.HasSuffix(word, "@"+conf.SSH) {
				return true
			}
		}
	}
	return false
}
