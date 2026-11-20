// Package api is the JSON contract ais offers other programs: what
// "ais state --json" and "ais env <id> --json" print, version 1.
//
// Readers must ignore keys they do not know, so ais may add keys without a new
// version; removing or renaming one is a breaking change and bumps Version.
// The acceptance suite (acceptance/) pins the key set independently of these
// types, so a rename here cannot pass unnoticed.
//
// Nullable fields (ended_at, exit_code, conversation, restart_of, fork_of,
// pid) decode to their zero values, which is all a reader needs, and encode
// back to null (Session.MarshalJSON), which is what ais promises.
package api

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Version is the api key this package speaks.
const Version = 1

// State is "ais state --json".
type State struct {
	API          int     `json:"api"`
	Host         string  `json:"host"`
	NotifyWindow float64 `json:"notify_window"` // minutes
	// How long a pane must stay on screen before its call is seen; nil from
	// an ais too old to mark calls seen at all.
	NotifySeenAfter *float64         `json:"notify_seen_after"` // seconds
	Worktree        WorktreeDefaults `json:"worktree"`
	Profiles        []Profile        `json:"profiles"`
	Sessions        []Session        `json:"sessions"`
}

// WorktreeDefaults is how ais makes a worktree when nobody says otherwise.
type WorktreeDefaults struct {
	Base string `json:"base"`
	Auto bool   `json:"auto"`
	// From is empty from an ais too old to be told.
	From   string `json:"from"`
	Detach bool   `json:"detach"`
}

// Profile is a named way to start a process.
type Profile struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	Flags     []string `json:"flags"`
	Resumable bool     `json:"resumable"`
	Forkable  bool     `json:"forkable"`
}

// Session is one recorded run.
type Session struct {
	ID            string         `json:"id"`
	Profile       string         `json:"profile"`
	Description   string         `json:"description"`
	Name          string         `json:"name"`
	Status        string         `json:"status"` // "active" or "dead"
	Host          string         `json:"host"`
	Cwd           string         `json:"cwd"`
	Worktree      Worktree       `json:"worktree"`
	Tmux          Location       `json:"tmux"`
	PID           int            `json:"pid"`
	StartedAt     string         `json:"started_at"`
	EndedAt       string         `json:"ended_at"`
	ExitCode      *int           `json:"exit_code"`
	Conversation  string         `json:"conversation"`
	Args          []string       `json:"args"`
	RestartOf     string         `json:"restart_of"`
	ForkOf        string         `json:"fork_of"`
	CanRestart    bool           `json:"can_restart"`
	CanResume     bool           `json:"can_resume"`
	CanFork       bool           `json:"can_fork"`
	Notifications []Notification `json:"notifications"`
}

// Worktree is where a session's worktrees live; empty for one without.
type Worktree struct {
	Base    string `json:"base,omitempty"`
	Initial string `json:"initial,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Root    string `json:"root,omitempty"`
}

// Location is where a session's pane is, as of the read.
type Location struct {
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id"`
	WindowID    string `json:"window_id"`
	WindowIndex string `json:"window_index"`
	PaneID      string `json:"pane_id"`
	PaneIndex   string `json:"pane_index"`
}

// Notification is one "ais notify".
type Notification struct {
	ID         string `json:"id,omitempty"`
	At         string `json:"at"`
	Message    string `json:"message"`
	Delivered  bool   `json:"delivered"`
	Suppressed bool   `json:"suppressed"`
	Seen       bool   `json:"seen"` // its pane has been on screen since
	SeenAt     string `json:"seen_at,omitempty"`
}

// Env is "ais env <id> --json".
type Env struct {
	API int               `json:"api"`
	ID  string            `json:"id"`
	Cwd string            `json:"cwd"`
	Env map[string]string `json:"env"`
}

// MarshalJSON writes a session as ais does: unknown values as null, lists
// as lists even when empty.
func (s Session) MarshalJSON() ([]byte, error) {
	type plain Session
	out := struct {
		plain
		PID          *int    `json:"pid"`
		StartedAt    *string `json:"started_at"`
		EndedAt      *string `json:"ended_at"`
		Conversation *string `json:"conversation"`
		RestartOf    *string `json:"restart_of"`
		ForkOf       *string `json:"fork_of"`
	}{plain: plain(s)}
	if s.PID != 0 {
		out.PID = &s.PID
	}
	out.StartedAt = nullable(s.StartedAt)
	out.EndedAt = nullable(s.EndedAt)
	out.Conversation = nullable(s.Conversation)
	out.RestartOf = nullable(s.RestartOf)
	out.ForkOf = nullable(s.ForkOf)
	if out.Args == nil {
		out.Args = []string{}
	}
	if out.Notifications == nil {
		out.Notifications = []Notification{}
	}
	return json.Marshal(out)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ParseState decodes "ais state --json", refusing another api version.
func ParseState(data []byte) (*State, error) {
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("ais state: %w", err)
	}
	if st.API != Version {
		return nil, fmt.Errorf("ais state speaks api %d, this reader knows %d", st.API, Version)
	}
	return &st, nil
}

// ParseEnv decodes "ais env <id> --json".
func ParseEnv(data []byte) (*Env, error) {
	var e Env
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("ais env: %w", err)
	}
	return &e, nil
}

// Label is how a session introduces itself: what it is for, else its name.
func (s *Session) Label() string {
	if s.Description != "" {
		return s.Description
	}
	if s.Name != "" {
		return s.Name
	}
	return s.Profile + " in " + tilde(s.Cwd)
}

func (s *Session) LastNotification() *Notification {
	if len(s.Notifications) == 0 {
		return nil
	}
	return &s.Notifications[len(s.Notifications)-1]
}

// NotifiedRecently is whether the session's last notification is inside the
// window: it called for you, whether or not you have looked since.
func (s *Session) NotifiedRecently(window float64, now time.Time) bool {
	n := s.LastNotification()
	if n == nil {
		return false
	}
	age, ok := since(n.At, now)
	return ok && age.Minutes() <= window
}

// WantsYou is true while the session's last word was a call for you, made in
// the last window minutes and not already seen: ais suppresses the banner
// for a pane that is on screen, which means you were looking, and marks the
// call seen once its pane has stayed on screen for a few seconds.
func (s *Session) WantsYou(window float64, now time.Time) bool {
	n := s.LastNotification()
	if s.Status != "active" || n == nil || n.Suppressed || n.Seen {
		return false
	}
	age, ok := since(n.At, now)
	return ok && age.Minutes() <= window
}

func (st *State) ByID() map[string]*Session {
	out := map[string]*Session{}
	if st == nil {
		return out
	}
	for i := range st.Sessions {
		out[st.Sessions[i].ID] = &st.Sessions[i]
	}
	return out
}

// ByPane finds running sessions by the pane ais recorded for them.
func (st *State) ByPane() map[string]*Session {
	out := map[string]*Session{}
	if st == nil {
		return out
	}
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.Status == "active" && s.Tmux.PaneID != "" && s.Host == st.Host {
			out[s.Tmux.PaneID] = s
		}
	}
	return out
}

// ForPane is the running session a pane shows: the one its @ais_id mark
// names, else the one ais recorded for paneID — panes started before ais
// marked them only have the record.
func (st *State) ForPane(paneID, mark string) *Session {
	if st == nil {
		return nil
	}
	if mark != "" {
		for i := range st.Sessions {
			if st.Sessions[i].ID == mark {
				return &st.Sessions[i]
			}
		}
		return nil
	}
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.Status == "active" && s.Tmux.PaneID == paneID && s.Host == st.Host {
			return s
		}
	}
	return nil
}

// Resumable are the dead sessions worth offering back, newest first: ones
// that can be restarted here and have not been already — a session that was
// restarted lives on in its restart, which is the row to offer.
func (st *State) Resumable(limit int) []*Session {
	if st == nil {
		return nil
	}
	// One row per piece of work: the same profile, name and directory is the
	// same work, however many times it was started, and only its latest run
	// is worth offering — none at all while one is still running.
	work := func(s *Session) string { return s.Profile + "\x00" + s.Label() + "\x00" + s.Cwd }
	continued := map[string]bool{}
	running := map[string]bool{}
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.RestartOf != "" {
			continued[s.RestartOf] = true
		}
		if s.Status == "active" {
			running[work(s)] = true
		}
	}
	var candidates []*Session
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.Status == "dead" && s.CanRestart && !continued[s.ID] && !running[work(s)] {
			candidates = append(candidates, s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].EndedAt > candidates[j].EndedAt })
	seen := map[string]bool{}
	var out []*Session
	for _, s := range candidates {
		if !seen[work(s)] {
			seen[work(s)] = true
			out = append(out, s)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

var home, _ = os.UserHomeDir()

func tilde(path string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + path[len(home):]
	}
	return path
}

func since(stamp string, now time.Time) (time.Duration, bool) {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}
