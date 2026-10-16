package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// The commands another host's tsm runs here, over ssh. They answer in JSON
// on stdout and never prompt: there is nobody on this end to answer.

// linkEnv is set on the tmux client "tsm attach" starts on a remote host,
// naming the local pane the connection runs in. It is how the remote end
// knows which of its clients is the one you are looking at.
const linkEnv = "TSM_LINK"

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func serveState(t Tmux, a Ais) int {
	snap, err := t.Snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsm:", err)
		return 1
	}
	// Nobody is looking from here: whatever tmux called the current client
	// was a guess, and "here" would mark the wrong pane.
	snap.Client = Client{}
	p := Payload{Proto: proto, Host: hostname(), Snap: snap, At: time.Now()}
	if a.Available() {
		if st, err := a.State(); err == nil {
			p.State = st
		}
	}
	p.Places = Places(snap, p.State, nil, Roots())
	return writeJSON(p)
}

func serveCapture(t Tmux, pane string) int {
	out, err := t.Capture(pane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsm:", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

func serveOp(t Tmux, a Ais, in io.Reader) int {
	var op Op
	if err := json.NewDecoder(in).Decode(&op); err != nil {
		return writeJSON(opResult{Error: "unreadable op: " + err.Error()})
	}
	snap, err := t.Snapshot()
	if err != nil {
		return writeJSON(opResult{Error: err.Error()})
	}
	// Whatever the op ends by showing, it shows to the client that came in
	// through the link, if there is one to find.
	snap.Client = Client{}
	if c, ok := linkedClient(t, op.Link); ok {
		snap.Client = c
	}
	var st *AisState
	if a.Available() {
		st, _ = a.State()
	}
	out, err := Exec(Actions{tmux: t, ais: a, snap: snap}, st, op)
	res := opResult{Outcome: out}
	if err != nil {
		res.Error = err.Error()
	}
	return writeJSON(res)
}

func writeJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "tsm:", err)
		return 1
	}
	return 0
}

type clientInfo struct {
	Client
	pid      int
	activity int64
}

func listClients(t Tmux) []clientInfo {
	out, err := t.run("list-clients", "-F", format([]string{"client_name", "client_pid", "session_id", "pane_id", "client_activity"}))
	if err != nil {
		return nil
	}
	var cs []clientInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, sep)
		if len(f) != 5 {
			continue
		}
		act, _ := strconv.ParseInt(f[4], 10, 64)
		cs = append(cs, clientInfo{Client{Name: f[0], SessionID: f[2], PaneID: f[3]}, atoi(f[1]), act})
	}
	return cs
}

// linkedClient is the client a link names: the one whose process carries
// TSM_LINK=link in its environment. Failing that, the only client there is,
// or the one used last — a guess, but the guess a person would make.
func linkedClient(t Tmux, link string) (Client, bool) {
	cs := listClients(t)
	if len(cs) == 0 {
		return Client{}, false
	}
	if link != "" {
		want := []byte(linkEnv + "=" + link)
		for _, c := range cs {
			env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", c.pid))
			if err != nil {
				continue
			}
			for _, kv := range bytes.Split(env, []byte{0}) {
				if bytes.Equal(kv, want) {
					return c.Client, true
				}
			}
		}
	}
	best := cs[0]
	for _, c := range cs[1:] {
		if c.activity > best.activity {
			best = c
		}
	}
	return best.Client, true
}
