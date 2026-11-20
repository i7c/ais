package main

import (
	"encoding/json"
	"testing"
)

func TestIsPromptFile(t *testing.T) {
	for path, want := range map[string]bool{
		"/tmp/claude-1000/claude-prompt-9d3e55f4-61a4.md": true,
		"/tmp/pi-editor-FRhWZm/prompt.md":                 true,
		"/home/me/project/CLAUDE.md":                      false, // /memory
		"/tmp/prompt.md":                                  false,
		"/tmp/claude-prompt-x.txt":                        false,
	} {
		if got := isPromptFile(path); got != want {
			t.Errorf("isPromptFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTargetRoundTrip(t *testing.T) {
	for _, c := range []struct{ remote, pane string }{{"", "%3"}, {"karif", "%12"}, {"a:b", "%1"}} {
		r, p := splitTarget(target(c.remote, c.pane))
		if r != c.remote || p != c.pane {
			t.Errorf("round trip of %q %q gave %q %q", c.remote, c.pane, r, p)
		}
	}
	if draftKey("", "%3") == draftKey("karif", "%3") {
		t.Error("a local and a remote pane share a draft")
	}
}

func TestRealEditorSkipsTheHook(t *testing.T) {
	t.Setenv("TSM_EDITOR", "")
	t.Setenv("VISUAL", "tsm compose --hook")
	t.Setenv("EDITOR", "nvim")
	if got := realEditor(); got != "nvim" {
		t.Errorf("realEditor() = %q, want nvim", got)
	}
	t.Setenv("EDITOR", "tsm compose --hook")
	if got := realEditor(); got != "vi" {
		t.Errorf("realEditor() = %q, want vi", got)
	}
	t.Setenv("TSM_EDITOR", "hx")
	if got := realEditor(); got != "hx" {
		t.Errorf("realEditor() = %q, want hx", got)
	}
}

// The relay socket takes notes from ais and composes from the hook alike.
func TestSockMsg(t *testing.T) {
	var note sockMsg
	json.Unmarshal([]byte(`{"id":"n1","message":"hi","host":"h"}`), &note)
	if note.Compose != nil || note.ID != "n1" || note.Message != "hi" {
		t.Errorf("a note decoded as %+v", note)
	}
	var c sockMsg
	json.Unmarshal([]byte(`{"compose":{"id":"c1","host":"h","pane":"%0","text":"x"}}`), &c)
	if c.Compose == nil || c.Compose.Pane != "%0" {
		t.Errorf("a compose decoded as %+v", c)
	}
}
