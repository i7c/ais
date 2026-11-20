package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseStateRefusesOtherVersions(t *testing.T) {
	for _, data := range []string{`{"api": 2}`, `{}`, `not json`} {
		if _, err := ParseState([]byte(data)); err == nil {
			t.Errorf("ParseState(%s) accepted it", data)
		}
	}
}

// Unknown keys are the contract's way of growing: a reader must not mind.
func TestParseStateIgnoresUnknownKeys(t *testing.T) {
	st, err := ParseState([]byte(`{"api": 1, "host": "h", "brand_new": [1],
		"sessions": [{"id": "a", "status": "active", "host": "h", "future": {"x": 1},
		  "tmux": {"pane_id": "%3"}, "exit_code": null, "conversation": null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := st.ByPane()["%3"]; s == nil || s.ID != "a" || s.ExitCode != nil || s.Conversation != "" {
		t.Errorf("ByPane = %+v", st.ByPane())
	}
}

func TestForPanePrefersTheMark(t *testing.T) {
	st := &State{Host: "h", Sessions: []Session{
		{ID: "recorded", Status: "active", Host: "h", Tmux: Location{PaneID: "%1"}},
		{ID: "marked", Status: "active", Host: "h", Tmux: Location{PaneID: "%9"}},
	}}
	if s := st.ForPane("%1", "marked"); s == nil || s.ID != "marked" {
		t.Errorf("with a mark: %+v", s)
	}
	if s := st.ForPane("%1", ""); s == nil || s.ID != "recorded" {
		t.Errorf("without a mark: %+v", s)
	}
	if s := st.ForPane("%1", "gone"); s != nil {
		t.Errorf("a mark naming no session found %+v", s)
	}
}

func TestWantsYou(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := now.Add(-5 * time.Minute).Format(time.RFC3339)
	s := Session{Status: "active", Notifications: []Notification{{At: at}}}
	if !s.WantsYou(60, now) || !s.NotifiedRecently(60, now) {
		t.Errorf("a fresh unseen call does not want you")
	}
	if s.WantsYou(1, now) {
		t.Errorf("a call outside the window still wants you")
	}
	s.Notifications[0].Seen = true
	if s.WantsYou(60, now) || !s.NotifiedRecently(60, now) {
		t.Errorf("seen: wants %v, recent %v", s.WantsYou(60, now), s.NotifiedRecently(60, now))
	}
}

func TestParseEnv(t *testing.T) {
	e, err := ParseEnv([]byte(`{"api": 1, "id": "x", "cwd": "/w", "env": {"AIS_SESSION": "x"}}`))
	if err != nil || e.Cwd != "/w" || e.Env["AIS_SESSION"] != "x" {
		t.Errorf("ParseEnv = %+v, %v", e, err)
	}
}

// What ais writes: nulls for what is not known, [] for empty lists, and only
// the worktree keys it has.
func TestSessionEncodesLikeAis(t *testing.T) {
	b, err := json.Marshal(Session{ID: "x", Status: "active", Worktree: Worktree{Base: "/w"}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pid", "ended_at", "exit_code", "conversation", "restart_of", "fork_of"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
	if a, ok := m["args"].([]any); !ok || len(a) != 0 {
		t.Errorf("args = %v, want []", m["args"])
	}
	if wt := m["worktree"].(map[string]any); len(wt) != 1 || wt["base"] != "/w" {
		t.Errorf("worktree = %v", wt)
	}
	if strings.Count(string(b), `"pid"`) != 1 {
		t.Errorf("pid encoded twice: %s", b)
	}
}
