package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// What tsm understands of "ais state" (api 1). Unknown keys are ignored, which
// is the contract: ais may add, tsm must not mind.
type AisState struct {
	API          int     `json:"api"`
	Host         string  `json:"host"`
	NotifyWindow float64 `json:"notify_window"` // minutes
	Worktree     struct {
		Base string `json:"base"`
		Auto bool   `json:"auto"`
	} `json:"worktree"`
	Profiles []AisProfile `json:"profiles"`
	Sessions []AisSession `json:"sessions"`
}

type AisProfile struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	Resumable bool     `json:"resumable"`
	Forkable  bool     `json:"forkable"`
}

type AisSession struct {
	ID          string `json:"id"`
	Profile     string `json:"profile"`
	Description string `json:"description"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Host        string `json:"host"`
	Cwd         string `json:"cwd"`
	Worktree    struct {
		Base    string `json:"base"`
		Initial string `json:"initial"`
		Branch  string `json:"branch"`
	} `json:"worktree"`
	Tmux struct {
		PaneID string `json:"pane_id"`
	} `json:"tmux"`
	StartedAt     string            `json:"started_at"`
	EndedAt       string            `json:"ended_at"`
	ExitCode      *int              `json:"exit_code"`
	Conversation  string            `json:"conversation"`
	Args          []string          `json:"args"`
	RestartOf     string            `json:"restart_of"`
	ForkOf        string            `json:"fork_of"`
	CanRestart    bool              `json:"can_restart"`
	CanResume     bool              `json:"can_resume"`
	CanFork       bool              `json:"can_fork"`
	Notifications []AisNotification `json:"notifications"`
}

type AisNotification struct {
	ID         string `json:"id"`
	At         string `json:"at"`
	Message    string `json:"message"`
	Delivered  bool   `json:"delivered"`
	Suppressed bool   `json:"suppressed"`
}

const aisAPI = 1

// Label is how a session introduces itself: what it is for, else its name.
func (s *AisSession) Label() string {
	if s.Description != "" {
		return s.Description
	}
	if s.Name != "" {
		return s.Name
	}
	return s.Profile + " in " + tilde(s.Cwd)
}

func (s *AisSession) LastNotification() *AisNotification {
	if len(s.Notifications) == 0 {
		return nil
	}
	return &s.Notifications[len(s.Notifications)-1]
}

// WantsYou is true while the session's last word was a call for you, made in
// the last window minutes and not already seen (ais suppresses the banner
// for a pane that is on screen, which means you were looking).
func (s *AisSession) WantsYou(window float64, now time.Time) bool {
	n := s.LastNotification()
	if s.Status != "active" || n == nil || n.Suppressed {
		return false
	}
	age, ok := since(n.At, now)
	return ok && age.Minutes() <= window
}

// Ais runs the ais command. Which one: $TSM_AIS, else ais on PATH.
type Ais struct {
	r   Runner
	bin string
}

func NewAis(r Runner) Ais {
	bin := os.Getenv("TSM_AIS")
	if bin == "" {
		if found, err := exec.LookPath("ais"); err == nil {
			bin = found
		}
	}
	return Ais{r: r, bin: bin}
}

func (a Ais) Available() bool { return a.bin != "" }

// Command is how to type ais into a shell: by name when the shell will find
// it by that name, so the line in its history reads naturally.
func (a Ais) Command() string {
	if found, err := exec.LookPath("ais"); err == nil && found == a.bin {
		return "ais"
	}
	return a.bin
}

func (a Ais) State() (*AisState, error) {
	if !a.Available() {
		return nil, fmt.Errorf("ais not found on PATH")
	}
	out, err := a.r.Run(a.bin, "state", "--json")
	if err != nil {
		if strings.Contains(err.Error(), "unknown command 'state'") {
			return nil, fmt.Errorf("%s predates \"ais state\"; update it (or point $TSM_AIS at one that has it)", tilde(a.bin))
		}
		return nil, err
	}
	return ParseAisState([]byte(out))
}

func ParseAisState(data []byte) (*AisState, error) {
	var st AisState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("ais state: %w", err)
	}
	if st.API != aisAPI {
		return nil, fmt.Errorf("ais state speaks api %d, tsm knows %d", st.API, aisAPI)
	}
	return &st, nil
}

// Env is where a session runs and what a shell beside it needs.
func (a Ais) Env(id string) (string, map[string]string, error) {
	out, err := a.r.Run(a.bin, "env", id, "--json")
	if err != nil {
		return "", nil, err
	}
	var data struct {
		API int               `json:"api"`
		Cwd string            `json:"cwd"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return "", nil, fmt.Errorf("ais env: %w", err)
	}
	return data.Cwd, data.Env, nil
}

// NameOf is the name ais would give a session with this description, so a
// tmux session made for it can agree with its window, branch and worktree.
func (a Ais) NameOf(description string) string {
	if !a.Available() || description == "" {
		return ""
	}
	out, err := a.r.Run(a.bin, "name", "--of", description)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (st *AisState) ByID() map[string]*AisSession {
	out := map[string]*AisSession{}
	if st == nil {
		return out
	}
	for i := range st.Sessions {
		out[st.Sessions[i].ID] = &st.Sessions[i]
	}
	return out
}

// ByPane finds running sessions by the pane ais recorded for them.
func (st *AisState) ByPane() map[string]*AisSession {
	out := map[string]*AisSession{}
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

// Resumable are the dead sessions worth offering back, newest first: ones
// that can be restarted here and have not been already — a session that was
// restarted lives on in its restart, which is the row to offer.
func (st *AisState) Resumable(limit int) []*AisSession {
	if st == nil {
		return nil
	}
	// One row per piece of work: the same profile, name and directory is the
	// same work, however many times it was started, and only its latest run
	// is worth offering — none at all while one is still running.
	work := func(s *AisSession) string { return s.Profile + "\x00" + s.Label() + "\x00" + s.Cwd }
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
	var candidates []*AisSession
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.Status == "dead" && s.CanRestart && !continued[s.ID] && !running[work(s)] {
			candidates = append(candidates, s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].EndedAt > candidates[j].EndedAt })
	seen := map[string]bool{}
	var out []*AisSession
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
