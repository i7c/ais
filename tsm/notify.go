package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A Note is a notification on its way to wherever you are. It starts as what
// "ais notify" hands its sink, and travels up the chain of connections — a
// server, the host whose tmux holds the connection to it, the laptop looking
// at that — until a host with somewhere to show it does.
type Note struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Tag     string `json:"tag,omitempty"`
	Session string `json:"session,omitempty"` // the ais session
	Harness string `json:"harness,omitempty"`
	Pane    string `json:"pane,omitempty"` // on Host
	Host    string `json:"host"`           // where it was raised
	// Visible: you are looking at it, as far as every host it passed through
	// can tell. Each one that holds the way onward ands in its own answer.
	Visible bool `json:"visible"`
	Force   bool `json:"force,omitempty"`
	// Hops counts the relays it has passed. Two hosts set up as each other's
	// remotes would pass a note back and forth for ever; this ends that.
	Hops int `json:"hops,omitempty"`
}

const maxHops = 5

// What became of a note. Undelivered means nobody could show it: it is kept,
// and offered again when somebody connects.
const (
	resultShown       = "shown"
	resultSuppressed  = "suppressed"
	resultUndelivered = "undelivered"
)

// runtimeDir is where this host's relay socket lives: private to the user.
func runtimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
		return filepath.Join(dir, fmt.Sprintf("tsm-%d", os.Getuid()))
	}
	return filepath.Join(dir, "tsm")
}

// relaySocket is where this host's relay listens; a runtime directory too
// long for a unix socket path gives way to a short private one.
func relaySocket() string {
	dir := runtimeDir()
	if len(dir)+len("/relay.sock") > 100 {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("tsm-%d", os.Getuid()))
	}
	return filepath.Join(dir, "relay.sock")
}

// offerUpstream passes a note to the relay connected to this host from
// above, if one is, and says what it made of it. ok is false when nobody is
// connected, which is the normal state of a host you are not looking at.
func offerUpstream(n Note, timeout time.Duration) (result string, ok bool) {
	conn, err := net.DialTimeout("unix", relaySocket(), time.Second)
	if err != nil {
		return "", false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(n); err != nil {
		return "", false
	}
	var answer struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&answer); err != nil || answer.Result == "" {
		return "", false
	}
	return answer.Result, true
}

// offerUpstreamProbe: is a relay above listening at all?
func offerUpstreamProbe() (struct{}, bool) {
	conn, err := net.DialTimeout("unix", relaySocket(), time.Second)
	if err != nil {
		return struct{}{}, false
	}
	conn.Close()
	return struct{}{}, true
}

// cmdNotify is "tsm notify --stdin": ais's [notify] sink. It answers what
// became of the note, or declines (exit 1) so ais delivers it itself.
func cmdNotify(in io.Reader) int {
	var n Note
	if err := json.NewDecoder(in).Decode(&n); err != nil {
		fmt.Fprintln(os.Stderr, "tsm notify: not a notification:", err)
		return 2
	}
	if n.Host == "" {
		n.Host = hostname()
	}
	result, ok := offerUpstream(n, 10*time.Second)
	if !ok || result == resultUndelivered {
		return 1
	}
	json.NewEncoder(os.Stdout).Encode(map[string]string{"result": result})
	return 0
}

// ---------------------------------------------------------------- showing

// show puts a banner on this machine's screen, if it has a way to.
func show(n Note) error {
	title := n.Title
	if n.Host != "" && n.Host != hostname() {
		title = n.Host + " · " + title
	}
	if cmd := os.Getenv("TSM_NOTIFY_CMD"); cmd != "" {
		// Whatever you would rather have: terminal-notifier, a webhook, a
		// test's log. It gets the title and the message as arguments.
		return exec.Command("sh", "-c", cmd+` "$@"`, "tsm-notify", title, n.Message).Run()
	}
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s sound name \"Glass\"",
			appleString(n.Message), appleString(title))
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("no display here")
		}
		if _, err := exec.LookPath("notify-send"); err != nil {
			return fmt.Errorf("notify-send missing")
		}
		return exec.Command("notify-send", "--app-name=tsm", title, n.Message).Run()
	}
	return fmt.Errorf("no way to show notifications on %s", runtime.GOOS)
}

func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// frontmostIsTerminal: on a Mac, whether the app in front is a terminal —
// the last thing that has to be true for you to be looking at a pane.
// Unknowable counts as no: a banner too many beats one missed.
func frontmostIsTerminal() bool {
	if runtime.GOOS != "darwin" {
		return true // nothing to ask; the tmux hops have said what they know
	}
	front, err := exec.Command("lsappinfo", "front").Output()
	if err != nil {
		return false
	}
	out, err := exec.Command("lsappinfo", "info", "-only", "name", strings.TrimSpace(string(front))).Output()
	if err != nil {
		return false
	}
	name := strings.ToLower(string(out))
	for _, t := range []string{"ghostty", "iterm", "terminal", "kitty", "wezterm", "alacritty"} {
		if strings.Contains(name, t) {
			return true
		}
	}
	return false
}

// deliver decides a note's fate on a host that has no relay above it.
func deliver(n Note) string {
	if n.Visible && !n.Force && frontmostIsTerminal() {
		return resultSuppressed
	}
	if err := show(n); err != nil {
		return resultUndelivered
	}
	return resultShown
}
