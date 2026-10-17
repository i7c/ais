package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// a fake ais whose state holds the given notifications on one session.
func aisWith(notes string) Ais {
	state := `{"api":1,"host":"karif","sessions":[{"id":"s1","profile":"claude","description":"fix it","status":"active","host":"karif","tmux":{"pane_id":"%1"},"notifications":[` + notes + `]}]}`
	return Ais{r: &fakeRunner{reply: func([]string) string { return state }}, bin: "ais"}
}

func shortTemp(t *testing.T) string {
	// unix sockets want short paths; t.TempDir can be long
	dir, err := os.MkdirTemp("/tmp", "tsmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestReplayIsWhatNobodySaw(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	a := aisWith(`{"id":"001","message":"old"},{"id":"002","message":"seen","delivered":true},{"id":"003","message":"looked at","suppressed":true},{"id":"004","message":"missed"}`)
	notes, since := replay(a, "001")
	if len(notes) != 1 || notes[0].Message != "missed" || notes[0].Pane != "%1" || since != "004" {
		t.Fatalf("notes %+v since %s", notes, since)
	}
	if notes[0].Title != "claude — fix it" {
		t.Fatal(notes[0].Title)
	}
	if notes, since := replay(a, ""); len(notes) != 0 || since != "004" {
		t.Fatal("a first meeting replays nothing, and learns where now is")
	}
}

func TestHeldNotesAreReplayedOnce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now().Format(time.RFC3339)
	holdNote(Note{ID: "005", At: now, Message: "from below"})
	holdNote(Note{ID: "006", At: time.Now().Add(-48 * time.Hour).Format(time.RFC3339), Message: "stale"})
	notes, _ := replay(Ais{}, "001")
	if len(notes) != 1 || notes[0].Message != "from below" {
		t.Fatalf("got %+v", notes)
	}
	if notes, _ := replay(Ais{}, "001"); len(notes) != 0 {
		t.Fatal("held notes go to whoever connects, once")
	}
	holdNote(Note{ID: "007", At: now, Message: "before anyone came"})
	if notes, _ := replay(Ais{}, ""); len(notes) != 1 {
		t.Fatal("even to a first meeting")
	}
}

func TestMissedNotesMakeOneBanner(t *testing.T) {
	one := []Note{{ID: "1", Message: "a"}}
	if summarize(one, "karif").Message != "a" {
		t.Fatal("one note is just that note")
	}
	many := summarize([]Note{{ID: "1", Message: "a"}, {ID: "2", Message: "b"}, {ID: "3", Message: "c"}, {ID: "4", Message: "d"}}, "karif")
	if many.ID != "4" || !strings.HasPrefix(many.Title, "4 notifications") || many.Message != "b\nc\nd" {
		t.Fatalf("got %+v", many)
	}
}

func TestDeliveryDecides(t *testing.T) {
	log := filepath.Join(t.TempDir(), "shown")
	t.Setenv("TSM_NOTIFY_CMD", "printf '%s|%s\\n' >> "+log)
	if got := deliver(Note{Title: "claude — x", Message: "done", Host: "karif"}); got != resultShown {
		t.Fatal(got)
	}
	data, _ := os.ReadFile(log)
	if strings.TrimSpace(string(data)) != "karif · claude — x|done" {
		t.Fatalf("shown as %q", data)
	}
	if got := deliver(Note{Visible: true}); got != resultSuppressed {
		t.Fatal("what you are looking at is not announced")
	}
	if got := deliver(Note{Visible: true, Force: true}); got != resultShown {
		t.Fatal("unless forced")
	}
	t.Setenv("TSM_NOTIFY_CMD", "false")
	if got := deliver(Note{}); got != resultUndelivered {
		t.Fatal(got)
	}
}

// The serve end, in-process: notes from "tsm notify" go up the pipe, the
// ack comes back down, and the sink hears the answer.
func TestANoteGoesUpAndItsAnswerComesBack(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTemp(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	snapOut := strings.Join([]string{
		line("$1", "s", "@1", 1, "w", "%1", 1, "zsh", "/"),
		clientMarker + strings.Join([]string{"", "", "", "0"}, sep),
	}, "\n")
	tm := Tmux{r: &fakeRunner{reply: func([]string) string { return snapOut }}}
	a := aisWith(`{"id":"009","message":"earlier"}`)

	upR, upW := io.Pipe()     // serve → relay
	downR, downW := io.Pipe() // relay → serve
	go serveRelay(tm, a, downR, upW)
	dec := json.NewDecoder(bufio.NewReader(upR))
	enc := json.NewEncoder(downW)

	enc.Encode(wire{Type: "hello", Since: "001"})
	got := map[string]wire{}
	deadline := time.After(5 * time.Second)
	msgs := make(chan wire)
	go func() {
		for {
			var m wire
			if dec.Decode(&m) != nil {
				return
			}
			msgs <- m
		}
	}()
	for got["replay"].Type == "" || got["state"].Type == "" {
		select {
		case m := <-msgs:
			got[m.Type] = m
		case <-deadline:
			t.Fatalf("got only %v", got)
		}
	}
	if r := got["replay"]; len(r.Notes) != 1 || r.Notes[0].Message != "earlier" {
		t.Fatalf("replay %+v", r)
	}
	if s := got["state"]; s.Payload.Snap.Pane("%1") == nil {
		t.Fatal("the picture goes up too")
	}

	answer := make(chan string)
	go func() {
		r, ok := offerUpstream(Note{ID: "010", Message: "need you"}, 5*time.Second)
		if !ok {
			r = "nobody there"
		}
		answer <- r
	}()
	for {
		select {
		case m := <-msgs:
			if m.Type != "notify" {
				continue
			}
			if m.Note.Message != "need you" || m.Note.Host == "" {
				t.Fatalf("note %+v", m.Note)
			}
			enc.Encode(wire{Type: "ack", ID: m.Note.ID, Result: resultSuppressed})
		case r := <-answer:
			if r != resultSuppressed {
				t.Fatalf("the sink heard %q", r)
			}
			downW.Close()
			return
		case <-deadline:
			t.Fatal("no answer")
		}
	}
}

func TestWithNobodyAboveTheSinkDeclines(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTemp(t))
	if code := cmdNotify(strings.NewReader(`{"id":"1","message":"x"}`)); code != 1 {
		t.Fatalf("exit %d: ais must deliver it itself", code)
	}
}
