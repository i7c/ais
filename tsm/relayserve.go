package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// "tsm relay --serve" is the lower end of a relay connection: started over
// ssh by the relay of the host above, it lives exactly as long as that
// connection. While it does, this host's notifications go up through it, and
// so does its tmux picture whenever it changes.
//
// The wire is one JSON object per line, both ways.
type wire struct {
	Type    string   `json:"type"` // hello ack | notify replay state
	Proto   int      `json:"proto,omitempty"`
	Since   string   `json:"since,omitempty"`
	ID      string   `json:"id,omitempty"`
	Result  string   `json:"result,omitempty"`
	Note    *Note    `json:"note,omitempty"`
	Notes   []Note   `json:"notes,omitempty"`
	Payload *Payload `json:"payload,omitempty"`
}

type wireWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *wireWriter) send(m wire) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(m)
}

const (
	pushEvery = 2 * time.Second
	// Even with nothing changed, the picture goes up this often, so the
	// popup above can tell a quiet host from a lost one (it calls a picture
	// older than 10s stale).
	pushAtLeast = 5 * time.Second
	ackWait     = 9 * time.Second
)

// reclaimEvery is how often a serving relay checks that its socket is still
// there for it.
var reclaimEvery = 5 * time.Second

func serveRelay(t Tmux, a Ais, in io.Reader, out io.Writer) int {
	w := &wireWriter{enc: json.NewEncoder(out)}
	every := reclaimEvery
	sock := relaySocket()
	os.MkdirAll(filepath.Dir(sock), 0o700)
	// Another laptop's relay may hold the socket; the newest connection is
	// the one somebody is sitting at, so it takes over.
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		w.send(wire{Type: "error", Result: err.Error()})
		return 1
	}
	// Go would unlink the path on Close, whoever's socket is there by then;
	// it is removed below only while it is still this one.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	mine, _ := os.Stat(sock)
	var held sync.Mutex // ln and mine, which reclaiming replaces
	defer func() {
		held.Lock()
		defer held.Unlock()
		ln.Close()
		if now, err := os.Stat(sock); err == nil && mine != nil && os.SameFile(now, mine) {
			os.Remove(sock)
		}
	}()

	var mu sync.Mutex
	waiting := map[string]chan string{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dec := json.NewDecoder(bufio.NewReader(in))
		for {
			var m wire
			if err := dec.Decode(&m); err != nil {
				return // the connection went: so does this end
			}
			switch m.Type {
			case "hello":
				notes, since := replay(a, m.Since)
				w.send(wire{Type: "replay", Notes: notes, Since: since})
			case "ack":
				mu.Lock()
				if ch := waiting[m.ID]; ch != nil {
					ch <- m.Result
				}
				mu.Unlock()
			}
		}
	}()

	accept := func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(ackWait + 2*time.Second))
				var n Note
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&n) != nil {
					return
				}
				if n.Host == "" {
					n.Host = hostname()
				}
				ch := make(chan string, 1)
				mu.Lock()
				waiting[n.ID] = ch
				mu.Unlock()
				defer func() { mu.Lock(); delete(waiting, n.ID); mu.Unlock() }()
				result := resultUndelivered
				if w.send(wire{Type: "notify", Note: &n}) == nil {
					select {
					case result = <-ch:
					case <-time.After(ackWait):
					}
				}
				json.NewEncoder(conn).Encode(map[string]string{"result": result})
			}(conn)
		}
	}
	go accept(ln)

	// The socket is the only way ais here reaches this connection. Should
	// someone take the path over and then go without tidying up, nothing
	// would listen there and every notification would be refused, so take
	// it back — unless a newer connection is still listening on it.
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			held.Lock()
			select {
			case <-done: // on its way out: the socket goes with it
				held.Unlock()
				return
			default:
			}
			now, err := os.Stat(sock)
			if (err != nil || !os.SameFile(now, mine)) && !listening(sock) {
				os.Remove(sock)
				if nl, err := net.Listen("unix", sock); err == nil {
					nl.(*net.UnixListener).SetUnlinkOnClose(false)
					ln.Close()
					ln = nl
					mine, _ = os.Stat(sock)
					go accept(nl)
				}
			}
			held.Unlock()
		}
	}()

	go pushState(t, a, w, done)
	<-done
	return 0
}

// listening is whether something answers on the unix socket at path.
func listening(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pushState sends the host's picture up whenever it changes, and at least
// every so often, so the popup above opens on it fresh.
func pushState(t Tmux, a Ais, w *wireWriter, done <-chan struct{}) {
	var last []byte
	var lastAis, lastPush time.Time
	var st *AisState
	tick := time.NewTicker(pushEvery)
	defer tick.Stop()
	for {
		snap, err := t.Snapshot()
		if err == nil {
			snap.Client = Client{}
			clients := viewers(t)
			key, _ := json.Marshal([]any{snap, clients})
			changed := !bytes.Equal(key, last)
			if a.Available() && (changed || time.Since(lastAis) > 15*time.Second) {
				if s, err := a.State(); err == nil {
					st, lastAis = s, time.Now()
					changed = true
				}
			}
			if changed || time.Since(lastPush) >= pushAtLeast {
				last, lastPush = key, time.Now()
				p := &Payload{Proto: proto, Host: hostname(), Snap: snap, State: st, Clients: clients, At: time.Now()}
				p.Places = Places(snap, st, nil, Roots())
				if w.send(wire{Type: "state", Payload: p}) != nil {
					return
				}
			}
		}
		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

// replay is what the host above missed since it last heard from here: this
// host's own notifications nobody saw, and those that came up from below
// while nobody was above to take them. Without a since — a first meeting —
// this host's own history is not news; it only learns where "now" is.
func replay(a Ais, since string) ([]Note, string) {
	var notes []Note
	newest := since
	if a.Available() {
		if st, err := a.State(); err == nil {
			for _, s := range st.Sessions {
				for _, n := range s.Notifications {
					if n.ID > newest {
						newest = n.ID
					}
					if since == "" || n.ID <= since || n.Delivered || n.Suppressed {
						continue
					}
					notes = append(notes, Note{
						ID: n.ID, At: n.At, Message: n.Message, Session: s.ID,
						Title: s.Profile + " — " + s.Label(), Harness: s.Profile,
						Pane: s.Tmux.PaneID, Host: st.Host,
					})
				}
			}
		}
	}
	// Held notes are undelivered by definition: whoever connects gets them,
	// first meeting or not.
	for _, n := range takeHeld() {
		if n.ID > newest {
			newest = n.ID
		}
		if n.ID > since {
			notes = append(notes, n)
		}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	return notes, newest
}

// ---------------------------------------------------------------- held

// Notes that came up from below while nobody was above, kept for whoever
// connects next. A handful, a day at most: they are old news after that.
const (
	heldKeep = 50
	heldAge  = 24 * time.Hour
)

func heldPath() string { return filepath.Join(cacheDir(), "held.json") }

func holdNote(n Note) {
	var held []Note
	if data, err := os.ReadFile(heldPath()); err == nil {
		json.Unmarshal(data, &held)
	}
	held = append(held, n)
	var keep []Note
	for _, h := range held {
		if at, err := time.Parse(time.RFC3339, h.At); err == nil && time.Since(at) > heldAge {
			continue
		}
		keep = append(keep, h)
	}
	if len(keep) > heldKeep {
		keep = keep[len(keep)-heldKeep:]
	}
	writeFileAtomic(heldPath(), keep)
}

func takeHeld() []Note {
	data, err := os.ReadFile(heldPath())
	if err != nil {
		return nil
	}
	os.Remove(heldPath())
	var held []Note
	json.Unmarshal(data, &held)
	return held
}

func writeFileAtomic(path string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// summarize folds missed notes into one banner: a screenful of them after
// a night away is worse than a line saying how many and the latest.
func summarize(notes []Note, from string) Note {
	if len(notes) == 1 {
		return notes[0]
	}
	last := notes[len(notes)-1]
	var lines []string
	for _, n := range notes[max(0, len(notes)-3):] {
		lines = append(lines, n.Message)
	}
	return Note{
		ID: last.ID, At: last.At, Host: from,
		Title:   strings.TrimSpace(fmtCount(len(notes)) + " while you were away"),
		Message: strings.Join(lines, "\n"),
	}
}

func fmtCount(n int) string {
	if n == 1 {
		return "1 notification"
	}
	return itoa(n) + " notifications"
}
