package main

import (
	"fmt"
	"strconv"
	"strings"
)

// A Snapshot is everything tmux knows about its sessions, windows and panes at
// one moment, plus which client asked. It is built from a single tmux call and
// never kept: tmux is the record, tsm only ever holds a picture of it.
type Snapshot struct {
	Sessions  []*Session `json:"sessions"`
	Client    Client     `json:"client"`
	BaseIndex int        `json:"base_index"` // where tmux starts numbering windows
}

type Client struct {
	Name      string `json:"name"` // e.g. /dev/pts/7; empty outside tmux
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"` // the pane the client is looking at: "here"
}

type Session struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Attached int       `json:"attached"`
	Windows  []*Window `json:"windows"`
	Fresh    bool      `json:"-"` // staged: does not exist yet
}

type Window struct {
	ID       string   `json:"id"`
	Index    int      `json:"index"`
	Name     string   `json:"name"`
	Active   bool     `json:"active"`
	Zoomed   bool     `json:"zoomed"`
	Activity int64    `json:"activity"` // unix seconds of the last output in the window
	Panes    []*Pane  `json:"panes"`
	Session  *Session `json:"-"`
	Moved    string   `json:"-"` // staged: where it came from
}

type Pane struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Active  bool   `json:"active"`
	Command string `json:"command"` // tmux's pane_current_command
	Path    string `json:"path"`    // tmux's pane_current_path
	PID     int    `json:"pid"`
	Title   string `json:"title"`
	Dead    bool   `json:"dead"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`

	// Marks an ais session sets on its pane while it runs; empty otherwise.
	AisID   string `json:"ais_id"`
	Profile string `json:"profile"`
	Desc    string `json:"desc"`
	Cwd     string `json:"cwd"`

	Window *Window `json:"-"`
	Moved  string  `json:"-"` // staged: where it came from
}

// Dir is where the pane works: the directory ais says its session runs in,
// else wherever tmux sees the foreground process.
func (p *Pane) Dir() string {
	if p.Cwd != "" {
		return p.Cwd
	}
	return p.Path
}

var shells = map[string]bool{
	"zsh": true, "bash": true, "fish": true, "sh": true, "dash": true,
	"nu": true, "ksh": true, "tcsh": true, "-zsh": true, "-bash": true,
}

// Idle is true for a pane that is just a shell at its prompt: nothing that
// killing it would interrupt.
func (p *Pane) Idle() bool { return p.AisID == "" && shells[p.Command] }

func (p *Pane) Location() string {
	w := p.Window
	return fmt.Sprintf("%s:%d.%d", w.Session.Name, w.Index, p.Index)
}

// Fields are separated by ASCII unit separators: unlike tabs or spaces, no
// window name or pane title will ever contain one.
const sep = "\x1f"

var paneFields = []string{
	"session_id", "session_name", "session_attached",
	"window_id", "window_index", "window_name", "window_active",
	"window_zoomed_flag", "window_activity",
	"pane_id", "pane_index", "pane_active", "pane_current_command",
	"pane_current_path", "pane_pid", "pane_title", "pane_dead",
	"pane_width", "pane_height",
	"@ais_id", "@ais_profile", "@ais_desc", "@cwd",
}

var clientFields = []string{"client_name", "session_id", "pane_id", "base-index"}

// clientMarker starts the one line that describes the client, so it can share
// a single tmux invocation with the pane listing.
const clientMarker = "tsm-client" + sep

func format(fields []string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = "#{" + f + "}"
	}
	return strings.Join(parts, sep)
}

// QueryArgs is the tmux invocation a snapshot is built from: every pane on
// the server, then the client tsm was opened from.
func QueryArgs() []string {
	return []string{
		"list-panes", "-a", "-F", format(paneFields), ";",
		"display-message", "-p", "-F", clientMarker + format(clientFields),
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ParseSnapshot reads the output of QueryArgs, keeping tmux's own order.
func ParseSnapshot(out string) *Snapshot {
	snap := &Snapshot{}
	sessions := map[string]*Session{}
	windows := map[string]*Window{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, clientMarker); ok {
			f := strings.Split(rest, sep)
			if len(f) == len(clientFields) {
				snap.Client = Client{Name: f[0], SessionID: f[1], PaneID: f[2]}
				snap.BaseIndex = atoi(f[3])
			}
			continue
		}
		f := strings.Split(line, sep)
		if len(f) != len(paneFields) {
			continue
		}
		s := sessions[f[0]]
		if s == nil {
			s = &Session{ID: f[0], Name: f[1], Attached: atoi(f[2])}
			sessions[f[0]] = s
			snap.Sessions = append(snap.Sessions, s)
		}
		w := windows[f[3]]
		if w == nil {
			activity, _ := strconv.ParseInt(f[8], 10, 64)
			w = &Window{
				ID: f[3], Index: atoi(f[4]), Name: f[5], Active: f[6] == "1",
				Zoomed: f[7] == "1", Activity: activity, Session: s,
			}
			windows[f[3]] = w
			s.Windows = append(s.Windows, w)
		}
		w.Panes = append(w.Panes, &Pane{
			ID: f[9], Index: atoi(f[10]), Active: f[11] == "1", Command: f[12],
			Path: f[13], PID: atoi(f[14]), Title: f[15], Dead: f[16] == "1",
			Width: atoi(f[17]), Height: atoi(f[18]),
			AisID: f[19], Profile: f[20], Desc: f[21], Cwd: f[22],
			Window: w,
		})
	}
	return snap
}

func (s *Snapshot) Pane(id string) *Pane {
	for _, sess := range s.Sessions {
		for _, w := range sess.Windows {
			for _, p := range w.Panes {
				if p.ID == id {
					return p
				}
			}
		}
	}
	return nil
}

func (s *Snapshot) Counts() (sessions, panes int) {
	for _, sess := range s.Sessions {
		for _, w := range sess.Windows {
			panes += len(w.Panes)
		}
	}
	return len(s.Sessions), panes
}

// ActivePane is the pane a session or window shows: what to preview for it.
func (w *Window) ActivePane() *Pane {
	for _, p := range w.Panes {
		if p.Active {
			return p
		}
	}
	if len(w.Panes) > 0 {
		return w.Panes[0]
	}
	return nil
}

func (s *Session) ActiveWindow() *Window {
	for _, w := range s.Windows {
		if w.Active {
			return w
		}
	}
	if len(s.Windows) > 0 {
		return s.Windows[0]
	}
	return nil
}
