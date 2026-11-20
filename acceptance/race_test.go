package acceptance

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// A reader polling state while a session starts (tsm does, constantly) must
// not find the session dead in the moment before its harness has a pid.
func TestStartingSessionSurvivesConcurrentReaders(t *testing.T) {
	l := NewLab(t)
	for i := 0; i < 5; i++ {
		pane, _ := l.Window(l.Home, l.AisCommand("run", "fake"))
		var id string
		l.Eventually("fake up in "+pane, func() bool {
			for _, s := range l.State().Sessions { // polls as fast as it can
				if s.Tmux.PaneID == pane {
					id = s.ID
				}
			}
			return id != "" && fileExists(filepath.Join(l.Log, id, "ready"))
		})
		if got := l.Session(id); got.Status != "active" {
			t.Fatalf("run %d: session %s is %s while its harness runs", i, id, got.Status)
		}
	}
}

// A session buried alive by an earlier reap (before launcher_pid) comes back
// once a reader sees its process and its own pane mark; one whose pane mark
// is gone stays dead.
func TestReapedButRunningSessionIsRevived(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake")
	gone := l.Start(l.Home, "run", "fake")
	l.tmuxMust("set-option", "-p", "-u", "-t", gone.Pane, "@ais_id") // as if its pane were reused
	path := filepath.Join(l.AisHome, "sessions.json")
	var raw map[string]any
	must(t, json.Unmarshal([]byte(l.ReadFile(path)), &raw))
	for _, rec := range raw["sessions"].([]any) {
		m := rec.(map[string]any)
		m["status"], m["reaped"], m["ended_at"] = "dead", true, "2026-11-20T11:14:51+00:00"
	}
	b, _ := json.Marshal(raw)
	l.WriteFile(path, string(b))

	if got := l.Session(s.ID); got.Status != "active" || got.EndedAt != nil {
		t.Errorf("running session not revived: status %s ended %v", got.Status, got.EndedAt)
	}
	if got := l.Session(gone.ID); got.Status != "dead" {
		t.Errorf("session without its pane mark revived")
	}
	l.Stop(s, 0)
	if got := l.Session(s.ID); got.Status != "dead" || deref(got.ExitCode) != 0 {
		t.Errorf("revived session did not end normally: %s %v", got.Status, got.ExitCode)
	}
}
