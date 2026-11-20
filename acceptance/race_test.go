package acceptance

import (
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
