package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The remote half: another host's tmux, reached by running tsm there.
//
// Every request is one command over ssh, multiplexed on a control connection
// tsm keeps for itself (ControlMaster, persisting for hours), so after the
// first one each costs about one round trip and no handshake. What came back
// last is cached on disk, which is what the popup shows the moment it opens.

const proto = 1 // what "tsm _state" and "tsm _op" speak; both ends must agree

// Payload is what "tsm _state" answers: a host's whole picture in one trip.
type Payload struct {
	Proto  int       `json:"proto"`
	Host   string    `json:"host"`
	Snap   *Snapshot `json:"snapshot"`
	State  *AisState `json:"ais,omitempty"`
	Places []Place   `json:"places,omitempty"`
	// Clients is who is looking at that tmux, used last first; Snap.Client
	// stays empty, since nobody there opened this.
	Clients []Client  `json:"clients,omitempty"`
	At      time.Time `json:"at"` // when the remote end took it
}

type Remote struct {
	Conf RemoteConf
}

// cacheDir is $XDG_CACHE_HOME/tsm, else ~/.cache/tsm — on every OS, like
// the config (Go's own answer on a Mac would be ~/Library/Caches).
func cacheDir() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "tsm")
	}
	if home == "" {
		return filepath.Join(os.TempDir(), "tsm")
	}
	return filepath.Join(home, ".cache", "tsm")
}

// controlPath is where the shared connection's socket lives. ssh's %C is 40
// hex digits, and a unix socket path cannot pass ~104 bytes, so a long cache
// directory gives way to a short private one.
func controlPath() string {
	dir := cacheDir()
	if len(dir)+len("/cm-")+40 > 100 {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("tsm-%d", os.Getuid()))
	}
	os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "cm-%C")
}

// argv is how to run a command line on the remote host.
func (r Remote) argv(line string) []string {
	if len(r.Conf.Command) > 0 {
		return append(append([]string{}, r.Conf.Command...), "sh", "-c", line)
	}
	return []string{
		"ssh",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath(),
		"-o", "ControlPersist=8h",
		// Never ask: there is nobody to answer a prompt inside a popup.
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=15",
		r.Conf.SSH, "--", line,
	}
}

// tsmLine is the remote command line running tsm with args. A login-less
// ssh shell often lacks ~/.local/bin and ~/go/bin on PATH, which is where tsm
// usually lives, so they go in front.
func (r Remote) tsmLine(args ...string) string {
	bin := r.Conf.TSM
	if bin == "" {
		bin = "tsm"
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	if strings.HasPrefix(bin, "~/") {
		bin = `"$HOME"/` + shellQuote(bin[2:])
	} else {
		bin = shellQuote(bin)
	}
	return `PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" ` + bin + " " + strings.Join(quoted, " ")
}

func (r Remote) run(stdin []byte, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	os.MkdirAll(cacheDir(), 0o700)
	argv := r.argv(r.tsmLine(args...))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if ctx.Err() != nil {
			msg = "timed out"
		}
		if msg == "" {
			msg = err.Error()
		}
		if lines := strings.Split(msg, "\n"); len(lines) > 1 {
			msg = lines[len(lines)-1] // ssh's last word is the one that matters
		}
		return out.Bytes(), fmt.Errorf("%s: %s", r.Conf.Name, msg)
	}
	return out.Bytes(), nil
}

// Fetch asks the remote tsm for its whole picture.
func (r Remote) Fetch() (*Payload, error) {
	out, err := r.run(nil, 15*time.Second, "_state")
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("%s: not a tsm answer (%v)", r.Conf.Name, err)
	}
	if p.Proto != proto {
		return nil, fmt.Errorf("%s runs a tsm speaking proto %d, this one %d: update one of them", r.Conf.Name, p.Proto, proto)
	}
	if p.Snap == nil {
		return nil, fmt.Errorf("%s: answer has no snapshot", r.Conf.Name)
	}
	p.Snap.link()
	r.save(&p)
	return &p, nil
}

func (r Remote) Capture(pane string) (string, error) {
	out, err := r.run(nil, 10*time.Second, "_capture", pane)
	return string(out), err
}

// Do sends an op to be done there.
func (r Remote) Do(op Op) (Outcome, error) {
	body, _ := json.Marshal(op)
	out, err := r.run(body, 20*time.Second, "_op")
	var res opResult
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{}, fmt.Errorf("%s: not a tsm answer (%v)", r.Conf.Name, jerr)
	}
	if res.Error != "" {
		return res.Outcome, fmt.Errorf("%s: %s", r.Conf.Name, res.Error)
	}
	return res.Outcome, nil
}

type opResult struct {
	Outcome Outcome `json:"outcome"`
	Error   string  `json:"error,omitempty"`
}

func (r Remote) cachePath() string {
	return filepath.Join(cacheDir(), "remote-"+slug(r.Conf.Name)+".json")
}

func (r Remote) save(p *Payload) {
	if data, err := json.Marshal(p); err == nil {
		os.MkdirAll(cacheDir(), 0o700)
		tmp := r.cachePath() + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			os.Rename(tmp, r.cachePath())
		}
	}
}

// Cached is the last answer, however old: better a picture with its age on
// it than an empty section while ssh connects.
func (r Remote) Cached() *Payload {
	data, err := os.ReadFile(r.cachePath())
	if err != nil {
		return nil
	}
	var p Payload
	if json.Unmarshal(data, &p) != nil || p.Proto != proto || p.Snap == nil {
		return nil
	}
	p.Snap.link()
	return &p
}

// link restores the back-pointers JSON leaves out.
func (s *Snapshot) link() {
	for _, sess := range s.Sessions {
		for _, w := range sess.Windows {
			w.Session = sess
			for _, p := range w.Panes {
				p.Window = w
			}
		}
	}
}
