package main

import (
	"testing"
	"time"
)

func TestStatusCountsWhatThePopupShows(t *testing.T) {
	now := time.Now()
	st := state(now, AisSession{
		ID: "aaa", Profile: "claude", Status: "active", Host: "h",
		Notifications: []AisNotification{{At: now.Add(-time.Minute).Format(time.RFC3339), Message: "need you"}},
	})
	snap := fixture()
	snap.Pane("%2").LastOutput = now.Unix()
	snap.Pane("%3").LastOutput = now.Unix()
	snap.Pane("%4").Window.Bell = true

	got := CountStatus(snap, st, now)
	want := StatusCounts{Wants: 2, Busy: 1, Panes: 4, Sessions: 2}
	if got != want {
		t.Fatalf("counts = %+v, want %+v (the pane you look at is not busy)", got, want)
	}
}

func TestStatusLeavesOutWhatIsZero(t *testing.T) {
	if got := (StatusCounts{Panes: 4, Sessions: 2}).Tmux(); got != "#[fg=#6c6c6c]4·2#[default]" {
		t.Fatalf("quiet = %q", got)
	}
	if got := (StatusCounts{Wants: 1, Busy: 3, Panes: 4, Sessions: 2}).Tmux(); got != "#[fg=#ff6b6b]●1 #[fg=#87d787]•3 #[fg=#6c6c6c]4·2#[default]" {
		t.Fatalf("busy = %q", got)
	}
}
