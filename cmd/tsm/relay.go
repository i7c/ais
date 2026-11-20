package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// "tsm relay" is the upper end: one long-running process per host, holding
// a relay connection to each configured remote. What comes up is offered
// further up (if a relay above is connected to this host), else shown here,
// else held for whoever connects next. Started from tmux.conf, it ends with
// the tmux server; elsewhere (a laptop without tmux) it runs until stopped.

func itoa(n int) string { return strconv.Itoa(n) }

type relayStatus struct {
	PID     int                    `json:"pid"`
	Started time.Time              `json:"started"`
	Remotes map[string]remoteState `json:"remotes"`
}

type remoteState struct {
	State string    `json:"state"` // connecting connected down
	Since string    `json:"since,omitempty"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

type relay struct {
	tmux   Tmux
	inTmux bool
	mu     sync.Mutex
	status relayStatus
	conns  map[string]*relayConn // by remote name, while connected
	poked  chan struct{}         // something you may be looking at changed
}

// relayConn is one live connection, and the latest picture it sent up.
type relayConn struct {
	conf    RemoteConf
	w       *wireWriter
	payload *Payload
}

func statusPath() string { return filepath.Join(cacheDir(), "relay.json") }
func sincePath(name string) string {
	return filepath.Join(cacheDir(), "relay-"+slug(name)+".json")
}

func (rl *relay) set(name string, s remoteState) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	s.At = time.Now()
	if s.Since == "" {
		s.Since = rl.status.Remotes[name].Since
	}
	rl.status.Remotes[name] = s
	writeFileAtomic(statusPath(), rl.status)
}

func loadSince(name string) string {
	var v struct {
		Since string `json:"since"`
	}
	if data, err := os.ReadFile(sincePath(name)); err == nil {
		json.Unmarshal(data, &v)
	}
	return v.Since
}

func saveSince(name, since string) {
	if since != "" && since > loadSince(name) {
		writeFileAtomic(sincePath(name), map[string]string{"since": since})
	}
}

func runRelay(t Tmux, conf Config) int {
	if len(conf.Remotes) == 0 {
		fmt.Fprintln(os.Stderr, "tsm relay: no [[remote]] in", tilde(configPath()), "— nothing to relay from")
		return 0
	}
	os.MkdirAll(cacheDir(), 0o700)
	lock, err := os.OpenFile(filepath.Join(cacheDir(), "relay.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsm relay:", err)
		return 1
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return 0 // one is running already; that one will do
	}
	rl := &relay{tmux: t, inTmux: os.Getenv("TMUX") != "",
		status: relayStatus{PID: os.Getpid(), Started: time.Now(), Remotes: map[string]remoteState{}},
		conns:  map[string]*relayConn{}, poked: make(chan struct{}, 1)}
	if rl.inTmux {
		t.HookChanges()
		go watchTmux(t, rl.poked, nil)
		go rl.watchSeen()
	}
	for _, rc := range conf.Remotes {
		go rl.keep(Remote{Conf: rc})
	}
	for {
		time.Sleep(10 * time.Second)
		if rl.inTmux {
			t.HookChanges() // a re-sourced tmux.conf takes them away
		}
		// Started from tmux.conf: when the server goes, so does its relay.
		if rl.inTmux {
			if _, err := t.run("list-sessions", "-F", "x"); err != nil {
				return 0
			}
		}
	}
}

// keep holds the connection to one remote, reconnecting with backoff.
func (rl *relay) keep(r Remote) {
	wait := 2 * time.Second
	for {
		rl.set(r.Conf.Name, remoteState{State: "connecting"})
		heard, err := rl.session(r)
		state := remoteState{State: "down"}
		if err != nil {
			state.Error = err.Error()
		}
		rl.set(r.Conf.Name, state)
		if heard {
			wait = 2 * time.Second // it worked for a while: try again soon
		}
		time.Sleep(wait)
		wait = min(wait*2, time.Minute)
	}
}

// session is one relay connection, for as long as it lasts. heard says
// whether the far end answered at all.
func (rl *relay) session(r Remote) (heard bool, err error) {
	argv := r.argv(r.tsmLine("relay", "--serve"))
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
		if !heard && err == nil {
			msg := strings.TrimSpace(stderr.String())
			if lines := strings.Split(msg, "\n"); msg != "" {
				err = fmt.Errorf("%s", lines[len(lines)-1])
			}
		}
	}()

	w := &wireWriter{enc: json.NewEncoder(stdin)}
	name := r.Conf.Name
	conn := &relayConn{conf: r.Conf, w: w}
	rl.mu.Lock()
	rl.conns[name] = conn
	rl.mu.Unlock()
	defer func() {
		rl.mu.Lock()
		if rl.conns[name] == conn {
			delete(rl.conns, name)
		}
		rl.mu.Unlock()
	}()
	if err := w.send(wire{Type: "hello", Proto: proto, Since: loadSince(name)}); err != nil {
		return false, err
	}
	dec := json.NewDecoder(bufio.NewReader(stdout))
	for {
		var m wire
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return heard, fmt.Errorf("connection closed")
			}
			return heard, err
		}
		if !heard {
			heard = true
			rl.set(name, remoteState{State: "connected"})
		}
		switch m.Type {
		case "notify":
			if m.Note == nil {
				continue
			}
			n := *m.Note
			go func() {
				result := rl.route(n, r.Conf)
				w.send(wire{Type: "ack", ID: n.ID, Result: result})
				saveSince(name, n.ID)
			}()
		case "replay":
			if len(m.Notes) > 0 {
				rl.route(summarize(m.Notes, name), r.Conf)
			}
			saveSince(name, m.Since)
			for _, n := range m.Notes {
				saveSince(name, n.ID)
			}
		case "state":
			if m.Payload != nil && m.Payload.Snap != nil {
				m.Payload.Snap.link()
				r.save(m.Payload) // the popup opens on this, live
				rl.mu.Lock()
				conn.payload = m.Payload
				rl.mu.Unlock()
				rl.poke() // the pane you look at over there may have changed
			}
		case "error":
			return heard, fmt.Errorf("%s: %s", name, m.Result)
		}
	}
}

// route decides what becomes of a note that came up from below: whether you
// are looking at it so far, then up, here, or held.
func (rl *relay) route(n Note, from RemoteConf) string {
	if n.Visible && rl.inTmux {
		// If the remote is reached through a pane here, you are looking at
		// the note only if a client here is showing that pane.
		if snap, err := rl.tmux.Snapshot(); err == nil {
			if p := hostingPane(snap, from); p != nil {
				n.Visible = rl.looking(p.ID)
			}
		}
	}
	n.Hops++
	if n.Hops < maxHops {
		if result, ok := offerUpstream(n, ackWait); ok && result != resultUndelivered {
			return result
		}
	}
	result := deliver(n)
	if result == resultUndelivered {
		holdNote(n)
	}
	return result
}

func (rl *relay) poke() {
	select {
	case rl.poked <- struct{}{}:
	default: // one is pending already: it covers this one too
	}
}

// seenAfter is how long a pane must stay on screen for its call to be seen,
// when the far end's ais does not say.
const seenAfter = 3 * time.Second

// A gaze is a remote pane you are looking at through the pane here that
// reaches it, and how long you must keep at it for its call to count as seen.
type gaze struct {
	pane  string
	dwell time.Duration
	w     *wireWriter
}

// gazes is what you are looking at on each remote right now. The remote's
// own ais cannot tell: a client attached there keeps showing its pane
// whether or not the pane here that holds it is on your screen.
func (rl *relay) gazes() map[string]gaze {
	out := map[string]gaze{}
	snap, err := rl.tmux.Snapshot()
	if err != nil {
		return out
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for name, c := range rl.conns {
		p := c.payload
		if p == nil || p.State == nil || p.State.NotifySeenAfter == nil {
			continue // nothing there would know what to do with it
		}
		hosting := hostingPane(snap, c.conf)
		if hosting == nil || !rl.looking(hosting.ID) {
			continue
		}
		if cl, ok := pickClient(p.Clients, link(hosting)); ok && cl.PaneID != "" {
			out[name] = gaze{cl.PaneID, time.Duration(*p.State.NotifySeenAfter * float64(time.Second)), c.w}
		}
	}
	return out
}

// watchSeen tells each remote when you have looked at one of its panes long
// enough for its call to be seen: once per look, however long it lasts.
func (rl *relay) watchSeen() {
	type look struct {
		pane string
		at   time.Time
		told bool
	}
	looks := map[string]*look{}
	recheck := make(chan struct{}, 1)
	for {
		select {
		case <-rl.poked:
			time.Sleep(settleFor)
		case <-recheck:
		}
		now := rl.gazes()
		for name := range looks {
			if g, ok := now[name]; !ok || g.pane != looks[name].pane {
				delete(looks, name) // looked away: the next look starts over
			}
		}
		for name, g := range now {
			l := looks[name]
			if l == nil {
				l = &look{pane: g.pane, at: time.Now()}
				looks[name] = l
				time.AfterFunc(g.dwell, func() {
					select {
					case recheck <- struct{}{}:
					default:
					}
				})
			}
			if !l.told && time.Since(l.at) >= g.dwell {
				l.told = g.w.send(wire{Type: "seen", Pane: g.pane}) == nil
			}
		}
	}
}

// looking: some client of this tmux is showing pane id right now.
func (rl *relay) looking(id string) bool {
	for _, c := range listClients(rl.tmux) {
		if c.PaneID == id {
			return true
		}
	}
	return false
}

func relayStatusCmd() int {
	data, err := os.ReadFile(statusPath())
	if err != nil {
		fmt.Println("tsm relay: not running here (no status)")
		return 1
	}
	var st relayStatus
	json.Unmarshal(data, &st)
	alive := syscall.Kill(st.PID, 0) == nil
	state := "running"
	if !alive {
		state = "not running (last ran as pid " + itoa(st.PID) + ")"
	}
	fmt.Printf("relay: %s, since %s\n", state, st.Started.Format(time.DateTime))
	for name, r := range st.Remotes {
		line := fmt.Sprintf("  %-12s %-10s as of %s", name, r.State, r.At.Format(time.TimeOnly))
		if r.Error != "" {
			line += "  " + r.Error
		}
		fmt.Println(line)
	}
	if _, ok := offerUpstreamProbe(); ok {
		fmt.Println("above: a relay is connected to this host")
	} else {
		fmt.Println("above: no relay is connected to this host")
	}
	return 0
}
