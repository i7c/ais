package acceptance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Records written by older versions of ais, and by other hosts, must keep
// working: sessions.json outlives every ais binary that writes it.
func TestLegacyAndForeignRecords(t *testing.T) {
	l := NewLab(t)
	host := l.State().Host
	dir := filepath.Join(l.Dir, "legacy")
	l.WriteFile(filepath.Join(dir, ".keep"), "")
	fake := filepath.Join(l.Bin, "fake")
	records := map[string]any{
		"version": 1,
		"sessions": []map[string]any{
			{
				// Before profiles: "harness" only, and no harness_args, so
				// restart has to recover the typed args from "command".
				"id": "legacy2345", "harness": "fake", "description": "Old style",
				"command":     []string{fake, "--from-config", "--session-id", "old-conv", "--model", "opus"},
				"session_ids": []string{"old-conv"},
				"status":      "dead", "pid": nil, "hostname": host, "cwd": dir,
				"tmux":       map[string]string{"session_name": "lab", "pane_id": "%99"},
				"started_at": "2026-01-01T10:00:00+00:00", "ended_at": "2026-01-01T11:00:00+00:00",
				"exit_code": 0,
			},
			{
				"id": "remote2345", "profile": "fake", "harness": "fake", "description": "On another machine",
				"command": []string{fake}, "harness_args": []string{}, "session_ids": []string{"far-conv"},
				"status": "active", "pid": 1, "hostname": "elsewhere", "cwd": "/nowhere",
				"tmux":       map[string]string{"session_name": "x", "pane_id": "%1"},
				"started_at": "2026-01-02T10:00:00+00:00", "ended_at": nil, "exit_code": nil,
			},
		},
	}
	b, err := json.Marshal(records)
	must(t, err)
	l.WriteFile(filepath.Join(l.AisHome, "sessions.json"), string(b))

	legacy := l.Session("legacy2345")
	if legacy.Profile != "fake" || deref(legacy.Conversation) != "old-conv" || !legacy.CanRestart || !legacy.CanResume {
		t.Errorf("legacy record = %+v", legacy)
	}
	dry := l.AisIn(dir, []string{"TMUX_PANE=" + l.HerePane()}, "", "restart", "legacy", "-n")
	if dry.Code != 0 || !strings.Contains(dry.Stdout, "--resume old-conv --model opus") || strings.Contains(dry.Stdout, "--session-id") {
		t.Errorf("restart of a legacy record:\n%s", dry)
	}

	// A session on another host is never reaped from here, nor restarted.
	remote := l.Session("remote2345")
	if remote.Status != "active" || remote.CanRestart || remote.CanFork {
		t.Errorf("foreign record = %+v", remote)
	}
	if r := l.AisIn(l.Home, []string{"TMUX_PANE=" + l.HerePane()}, "", "restart", "remote"); r.Code == 0 {
		t.Errorf("restart of another host's session succeeded")
	}
}

// [harness.<name>] is the old spelling of [profile.<name>] and still works.
func TestOldConfigSpelling(t *testing.T) {
	l := NewLab(t)
	l.SetConfig(strings.Replace(l.BaseConfig(), "[profile.plain]", "[harness.plain]", 1))
	s := l.Start(l.Home, "run", "plain")
	if l.Fake(s.ID).Argv != nil {
		t.Errorf("old-spelling profile ran with argv %q", l.Fake(s.ID).Argv)
	}
	l.Stop(s, 0)
}

// Without a config file ais writes a starter one and runs.
func TestStarterConfig(t *testing.T) {
	l := NewLab(t)
	l.SetConfig("")
	cfg := filepath.Join(l.AisHome, "config.toml")
	must(t, removeFile(cfg))
	l.MustAis(nil, "state")
	if text := l.ReadFile(cfg); !strings.Contains(text, "[profile.claude]") {
		t.Errorf("starter config:\n%s", text)
	}
}
