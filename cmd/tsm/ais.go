package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/i7c/ais/internal/api"
)

// What tsm understands of "ais state" and "ais env": the contract lives in
// internal/api, shared with ais itself.
type (
	AisState        = api.State
	AisProfile      = api.Profile
	AisSession      = api.Session
	AisNotification = api.Notification
)

func ParseAisState(data []byte) (*AisState, error) { return api.ParseState(data) }

// aisForPane is the running ais session a pane shows.
func aisForPane(st *AisState, p *Pane) *AisSession {
	if p == nil {
		return nil
	}
	return st.ForPane(p.ID, p.AisID)
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
		} else if p := filepath.Join(home, ".local", "bin", "ais"); exists(p) {
			// Where it usually is, even when a login-less ssh shell's PATH
			// does not say so.
			bin = p
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

// MarkSeen tells ais that pane has been on screen long enough for its call
// to count as seen.
func (a Ais) MarkSeen(pane string) error {
	if !a.Available() {
		return fmt.Errorf("ais not found on PATH")
	}
	_, err := a.r.Run(a.bin, "_mark-seen", "--pane", pane)
	return err
}

// Env is where a session runs and what a shell beside it needs.
func (a Ais) Env(id string) (string, map[string]string, error) {
	out, err := a.r.Run(a.bin, "env", id, "--json")
	if err != nil {
		return "", nil, err
	}
	data, err := api.ParseEnv([]byte(out))
	if err != nil {
		return "", nil, err
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
