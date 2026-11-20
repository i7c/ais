package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Serving relays check their socket often here, or the tests would wait.
func init() { reclaimEvery = 20 * time.Millisecond }

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
	a := aisWith(`{"id":"001","message":"old"},{"id":"002","message":"seen","delivered":true},{"id":"003","message":"looked at","suppressed":true},{"id":"004","message":"missed"},{"id":"005","message":"looked at later","seen":true}`)
	notes, since := replay(a, "001")
	if len(notes) != 1 || notes[0].Message != "missed" || notes[0].Pane != "%1" || since != "005" {
		t.Fatalf("notes %+v since %s", notes, since)
	}
	if notes[0].Title != "claude — fix it" {
		t.Fatal(notes[0].Title)
	}
	if notes, since := replay(a, ""); len(notes) != 0 || since != "005" {
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

// Someone takes the socket over and dies without tidying up: the
// connection still alive takes it back, so notes reach it again.
func TestAnAbandonedSocketIsTakenBack(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTemp(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tm := Tmux{r: &fakeRunner{reply: func([]string) string { return "" }}}

	upR, upW := io.Pipe()
	downR, downW := io.Pipe()
	defer downW.Close()
	go serveRelay(tm, aisWith(""), downR, upW)
	notes := make(chan *Note, 4)
	go func() {
		dec := json.NewDecoder(bufio.NewReader(upR))
		for {
			var m wire
			if dec.Decode(&m) != nil {
				return
			}
			if m.Type == "notify" {
				notes <- m.Note
			}
		}
	}()

	sock := relaySocket()
	for i := 0; !listening(sock); i++ {
		if i > 200 {
			t.Fatal("never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The usurper: a socket file of its own, left behind with nobody on it.
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if listening(sock) {
		t.Fatal("the stale socket should refuse")
	}

	for i := 0; !listening(sock); i++ {
		if i > 200 {
			t.Fatal("the socket was never taken back")
		}
		time.Sleep(5 * time.Millisecond)
	}
	go offerUpstream(Note{ID: "1", Message: "back"}, time.Second)
	select {
	case n := <-notes:
		if n.Message != "back" {
			t.Fatalf("note %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the note did not come up")
	}
}

// A newer connection that is alive keeps the socket.
func TestALiveTakeoverIsLeftAlone(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTemp(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tm := Tmux{r: &fakeRunner{reply: func([]string) string { return "" }}}

	upR, upW := io.Pipe()
	downR, downW := io.Pipe()
	defer downW.Close()
	go io.Copy(io.Discard, upR)
	go serveRelay(tm, aisWith(""), downR, upW)
	sock := relaySocket()
	for i := 0; !listening(sock); i++ {
		if i > 200 {
			t.Fatal("never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	newer, _ := os.Stat(sock)
	time.Sleep(10 * reclaimEvery)
	if now, err := os.Stat(sock); err != nil || !os.SameFile(now, newer) {
		t.Fatal("the newer connection lost its socket")
	}
}

// Where you look on this host goes up the moment tmux says it changed, not
// when the heartbeat comes round.
func TestAChangeGoesUpAtOnce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	snapOut := strings.Join([]string{
		line("$1", "s", "@1", 1, "w", "%1", 1, "zsh", "/"),
		line("$1", "s", "@1", 1, "w", "%2", 2, "zsh", "/"),
		clientMarker + strings.Join([]string{"", "", "", "0"}, sep),
	}, "\n")
	var mu sync.Mutex
	looking := "%1"
	r := &fakeRunner{reply: func(args []string) string {
		if args[0] == "list-clients" {
			mu.Lock()
			defer mu.Unlock()
			return strings.Join([]string{"/dev/pts/3", "0", "$1", looking, "1"}, sep) + "\n"
		}
		return snapOut
	}}
	upR, upW := io.Pipe()
	done, poked := make(chan struct{}), make(chan struct{}, 1)
	defer close(done)
	go pushState(Tmux{r: r}, Ais{}, &wireWriter{enc: json.NewEncoder(upW)}, done, poked)
	states := make(chan *Payload)
	go func() {
		dec := json.NewDecoder(upR)
		for {
			var m wire
			if dec.Decode(&m) != nil {
				return
			}
			states <- m.Payload
		}
	}()
	viewing := func(within time.Duration) string {
		select {
		case p := <-states:
			return p.Clients[0].PaneID
		case <-time.After(within):
			return "nothing"
		}
	}
	if got := viewing(time.Second); got != "%1" {
		t.Fatalf("first picture: looking at %s", got)
	}
	mu.Lock()
	looking = "%2"
	mu.Unlock()
	poked <- struct{}{}
	if got := viewing(pushAtLeast / 5); got != "%2" {
		t.Fatalf("after the change: %s, the heartbeat is %s away", got, pushAtLeast)
	}
	if r.find("set-hook") == nil {
		t.Fatal("tmux was never asked to announce changes")
	}
}

// tmux signals a change through hooks of tsm's own, in a slot a tmux.conf's
// plain set-hook does not use; a hook an older tmux lacks costs only itself.
func TestTheHooksAreSetOneByOneWhenTmuxRefusesOne(t *testing.T) {
	r := &refuses{word: "pane-title-changed[77]"}
	Tmux{r: r}.HookChanges()
	if len(r.calls) != 1+len(changeHooks) {
		t.Fatalf("%d calls", len(r.calls))
	}
	want := []string{"tmux", "set-hook", "-g", "window-pane-changed[77]", "wait-for -S " + changedChannel}
	found := false
	for _, c := range r.calls[1:] {
		found = found || reflect.DeepEqual(c, want)
	}
	if !found {
		t.Fatalf("no %q among %q", want, r.calls)
	}
}

// refuses fails any tmux command that mentions word.
type refuses struct {
	word  string
	calls [][]string
}

func (f *refuses) Run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	for _, a := range args {
		if a == f.word {
			return "", fmt.Errorf("invalid option: %s", a)
		}
	}
	return "", nil
}

// seenRelay is a relay whose tmux shows pane %4, the one reaching karif, and
// whose karif connection says you look at %9 there through it.
func seenRelay(showing string, seenAfter *float64) (*relay, *bytes.Buffer, *sync.Mutex) {
	r := &fakeRunner{reply: func(a []string) string {
		switch a[0] {
		case "list-panes":
			return strings.Join([]string{
				line("$1", "soi", "@1", 1, "code", "%1", 1, "zsh", "/x"),
				line("$2", "far", "@3", 1, "karif", "%4", 1, "ssh", "/x", "", "", "", "", "karif"),
			}, "\n") + "\n"
		case "list-clients":
			return strings.Join([]string{"/dev/pts/1", "1", "$1", showing, "1"}, sep) + "\n"
		}
		return ""
	}}
	var buf bytes.Buffer
	var mu sync.Mutex
	rl := &relay{tmux: Tmux{r: r}, conns: map[string]*relayConn{}, poked: make(chan struct{}, 1)}
	hosting := &Pane{ID: "%4"}
	rl.conns["karif"] = &relayConn{
		conf: RemoteConf{Name: "karif"},
		w:    &wireWriter{enc: json.NewEncoder(lockedWriter{&buf, &mu})},
		payload: &Payload{
			State:   &AisState{NotifySeenAfter: seenAfter},
			Clients: []Client{{PaneID: "%7"}, {PaneID: "%9", Link: link(hosting)}},
		},
	}
	return rl, &buf, &mu
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestAGazeIsThePaneYouSeeThroughTheConnection(t *testing.T) {
	zero := 0.0
	rl, _, _ := seenRelay("%4", &zero)
	if g := rl.gazes()["karif"]; g.pane != "%9" {
		t.Fatalf("looking at karif through %%4 is looking at its %%9, got %+v", g)
	}
	rl, _, _ = seenRelay("%1", &zero)
	if len(rl.gazes()) != 0 {
		t.Fatal("a client left showing the remote is not you looking at it")
	}
	rl, _, _ = seenRelay("%4", nil)
	if len(rl.gazes()) != 0 {
		t.Fatal("an ais that cannot mark calls seen is not told")
	}
}

func TestALongEnoughLookIsToldOnce(t *testing.T) {
	wait := 0.05
	rl, buf, mu := seenRelay("%4", &wait)
	go rl.watchSeen()
	rl.poke()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	early := buf.Len()
	mu.Unlock()
	if early != 0 {
		t.Fatal("told before the look was long enough")
	}
	time.Sleep(100 * time.Millisecond)
	rl.poke()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Count(buf.String(), `"type":"seen"`); got != 1 || !strings.Contains(buf.String(), `"pane":"%9"`) {
		t.Fatalf("want one seen for %%9, got %q", buf.String())
	}
}

func TestASeenFromAboveReachesAis(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTemp(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tm := Tmux{r: &fakeRunner{reply: func([]string) string { return "" }}}
	r := &fakeRunner{reply: func([]string) string { return "" }}
	upR, upW := io.Pipe()
	downR, downW := io.Pipe()
	defer downW.Close()
	go io.Copy(io.Discard, upR)
	go serveRelay(tm, Ais{r: r, bin: "ais"}, downR, upW)
	json.NewEncoder(downW).Encode(wire{Type: "seen", Pane: "%9"})
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := r.find("_mark-seen")
		r.mu.Unlock()
		if got != nil {
			if !reflect.DeepEqual(got, []string{"ais", "_mark-seen", "--pane", "%9"}) {
				t.Fatalf("ran %v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ais was never told")
		}
	}
}
