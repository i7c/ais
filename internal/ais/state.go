package ais

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/i7c/ais/internal/api"
)

// What "ais state" and "ais env" print is a contract with other programs (tsm
// first among them), not a dump of sessions.json: the record can change shape
// whenever ais needs it to, this cannot without bumping api.Version.

func publicSession(r *Record, cfg config, host string) api.Session {
	profile := r.profile()
	var conf *sessionConf
	if profile != "" {
		conf = harnessSettings(cfg, profile).Session
	}
	conversation := r.conversation()
	here := r.Hostname == host
	alive := r.Status == "active"
	status := "dead"
	if alive {
		status = "active"
	}
	wt := r.wt()
	args := []string{}
	if r.HarnessArgs != nil {
		args = append(args, *r.HarnessArgs...)
	}
	notes := make([]api.Notification, 0, len(r.Notes))
	for _, n := range r.Notes {
		notes = append(notes, api.Notification{
			ID: n.ID, At: n.At, Message: n.Message, Delivered: n.Delivered,
			Suppressed: n.Suppressed, Seen: n.SeenAt != "", SeenAt: n.SeenAt,
		})
	}
	ended := ""
	if r.EndedAt != nil {
		ended = *r.EndedAt
	}
	return api.Session{
		ID:          r.ID,
		Profile:     profile,
		Description: r.desc(),
		Name:        r.Slug,
		Status:      status,
		Host:        r.Hostname,
		Cwd:         r.Cwd,
		Worktree:    api.Worktree{Base: wt.Base, Initial: wt.Initial, Branch: wt.Branch, Root: wt.Root},
		Tmux: api.Location{
			SessionName: r.Tmux["session_name"], SessionID: r.Tmux["session_id"],
			WindowID: r.Tmux["window_id"], WindowIndex: r.Tmux["window_index"],
			PaneID: r.Tmux["pane_id"], PaneIndex: r.Tmux["pane_index"],
		},
		PID:          r.pid(),
		StartedAt:    r.StartedAt,
		EndedAt:      ended,
		ExitCode:     r.ExitCode,
		Conversation: conversation,
		// What was typed after the profile: run it again with these and you
		// get the same thing, minus the conversation.
		Args:      args,
		RestartOf: r.RestartOf,
		ForkOf:    r.ForkOf,
		// What can be done with it from here, so a caller need not re-derive
		// ais's rules: restart needs it dead, local and with somewhere to go
		// back to; resuming the conversation, and forking it, need the
		// profile to know how.
		CanRestart:    !alive && here && r.Cwd != "" && isDir(r.Cwd),
		CanResume:     conversation != "" && conf != nil && len(conf.Resume) > 0,
		CanFork:       here && conversation != "" && conf != nil && len(conf.Fork) > 0,
		LastInputAt:   r.LastInputAt,
		FlowProposal:  r.FlowProposal,
		Notifications: notes,
	}
}

func publicProfile(name string, cfg config) api.Profile {
	st := harnessSettings(cfg, name)
	flags := st.Flags
	if flags == nil {
		flags = []string{}
	}
	return api.Profile{
		Name: name, Command: st.Command, Flags: flags,
		Resumable: st.Session != nil && len(st.Session.Resume) > 0,
		Forkable:  st.Session != nil && len(st.Session.Fork) > 0,
		Prompts:   len(st.Prompt) > 0,
	}
}

func emitJSON(v any) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	must(enc.Encode(v))
	os.Stdout.Write(buf.Bytes())
	return 0
}

// cmdState is every profile and session as JSON: the one read other programs
// make. Sessions are reaped first, so "active" means running right now and
// the tmux location is where the pane is now. --since keeps only
// notifications newer than the given id, and only sessions that have one.
func cmdState(activeOnly bool, since string) int {
	cfg := loadConfig()
	ss := reapedSessions()
	if activeOnly {
		ss = active(ss)
	}
	byStartedDesc(ss)
	host := hostname()
	out := make([]api.Session, 0, len(ss))
	for _, r := range ss {
		out = append(out, publicSession(r, cfg, host))
	}
	if since != "" {
		kept := out[:0]
		for _, s := range out {
			var notes []api.Notification
			for _, n := range s.Notifications {
				if n.ID > since {
					notes = append(notes, n)
				}
			}
			if len(notes) > 0 {
				s.Notifications = notes
				kept = append(kept, s)
			}
		}
		out = kept
	}
	wt := worktreeSettings(cfg)
	ns := notifySettings()
	profiles := []api.Profile{}
	for _, name := range profileNames(cfg) {
		profiles = append(profiles, publicProfile(name, cfg))
	}
	return emitJSON(struct {
		API             int                  `json:"api"`
		Host            string               `json:"host"`
		Worktree        api.WorktreeDefaults `json:"worktree"`
		NotifyWindow    float64              `json:"notify_window"`
		NotifySeenAfter float64              `json:"notify_seen_after"`
		Profiles        []api.Profile        `json:"profiles"`
		Sessions        []api.Session        `json:"sessions"`
	}{api.Version, host, api.WorktreeDefaults{Base: wt.Base, Auto: wt.Auto, From: wt.From, Detach: wt.Detach},
		ns.Window, ns.SeenAfter, profiles, out})
}

// siblingEnv is what a plain shell beside this session needs to be in the
// same place: the profile's own env, the session's identity, and where its
// worktrees are. AIS_SIBLING marks the shell as not the agent itself.
func siblingEnv(r *Record, cfg config) map[string]string {
	profile := r.profile()
	env := map[string]string{}
	for k, v := range harnessSettings(cfg, profile).Env {
		env[k] = v
	}
	env["AIS_SESSION"] = r.ID
	env["AIS_SIBLING"] = "1"
	env["AIS_PROFILE"] = profile
	env["AIS_HARNESS"] = profile
	if r.desc() != "" {
		env["AIS_SESSION_DESCRIPTION"] = r.desc()
	}
	for k, v := range worktreeEnv(r) {
		env[k] = v
	}
	return env
}

// worktreeEnv is AIS_WORKTREE_BASE, AIS_WORKTREE and AIS_REPO_MAIN, as far as
// known.
func worktreeEnv(r *Record) map[string]string {
	rec := r.wt()
	env := map[string]string{}
	if rec.Base != "" {
		env["AIS_WORKTREE_BASE"] = rec.Base
	}
	if rec.Initial != "" {
		env["AIS_WORKTREE"] = rec.Initial
		root := rec.Root
		if root == "" {
			root = repoRoot(rec.Initial)
		}
		if root != "" {
			env["AIS_REPO_MAIN"] = root
		}
	}
	return env
}

func cmdEnv(id string, asJSON bool) int {
	cfg := loadConfig()
	r := pickSession(readSessions(), id)
	env := siblingEnv(r, cfg)
	if asJSON {
		return emitJSON(api.Env{API: api.Version, ID: r.ID, Cwd: r.Cwd, Env: env})
	}
	fmt.Println("cd " + shellQuote(r.Cwd))
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	fmt.Print(b.String())
	return 0
}
