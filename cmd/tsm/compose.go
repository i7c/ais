package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// "tsm compose" writes a long prompt in an editor beside the agent, not in
// place of it. Set as a harness's editor (EDITOR="tsm compose --hook"), it
// takes what is in the prompt box, empties the box and returns at once, so
// the harness stays usable — scroll it, read it — while you write. The
// editor opens in a split on the topmost tmux that shows the agent (for an
// agent on a remote, the pane here that reaches it), where typing does not
// wait on the network. Save and quit, and the text is pasted back into the
// agent's prompt box, not sent: you press enter there.

// A Compose is a prompt on its way to an editor: what was in the box, and
// the pane it came from, on the host it came from.
type Compose struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	// Pane is the agent's pane as the host it is offered to names it: a
	// pane id on Host, or, once it has passed a relay, the way down to it
	// ("karif:%5" from the host above karif).
	Pane string `json:"pane"`
	Text string `json:"text"`
	Hops int    `json:"hops,omitempty"`
}

const (
	composeOpened   = "opened"
	composeFocused  = "focused" // one was open already: the box keeps its text
	composeDeclined = "declined"
	// composeWait bounds how long the hook waits on the hosts above: a
	// harness is frozen until it returns. The relay that answers it waits
	// less on the hosts above it in turn, and opens the editor itself if
	// they have not taken it by then.
	composeWait   = 5 * time.Second
	composeAckFor = composeWait - 500*time.Millisecond // a serving relay, on the one above
	composeUpFor  = 2 * time.Second                    // a relay, on the hosts above it

)

func cmdCompose(t Tmux, args []string) int {
	switch {
	case len(args) >= 2 && args[0] == "--hook":
		return composeHook(t, args[len(args)-1])
	case len(args) == 4 && args[0] == "--edit" && args[2] == "--to":
		return composeEdit(t, args[1], args[3])
	}
	fmt.Fprint(os.Stderr, "usage: tsm compose --hook FILE   (as a harness's EDITOR)\n")
	return 2
}

// isPromptFile is whether a harness handed its editor the prompt box, rather
// than a file to edit for real (claude's /memory opens CLAUDE.md the same
// way, and emptying that would be a disaster).
func isPromptFile(path string) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(base, "claude-prompt-") && strings.HasSuffix(base, ".md"):
		return true
	case base == "prompt.md" && strings.HasPrefix(filepath.Base(filepath.Dir(path)), "pi-editor-"):
		return true
	}
	return false
}

// realEditor is the editor to actually edit in: $TSM_EDITOR, else [compose]
// editor, else $VISUAL or $EDITOR unless that is this hook, else the first
// of nvim, vim and vi there is.
func realEditor() string {
	if e := os.Getenv("TSM_EDITOR"); e != "" {
		return e
	}
	if conf, err := LoadConfig(); err == nil && conf.Compose.Editor != "" {
		return conf.Compose.Editor
	}
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if e := os.Getenv(k); e != "" && !strings.Contains(e, "compose") {
			return e
		}
	}
	for _, e := range []string{"nvim", "vim"} {
		if _, err := exec.LookPath(e); err == nil {
			return e
		}
	}
	return "vi"
}

func runEditor(path string) error {
	cmd := exec.Command("sh", "-c", realEditor()+` "$@"`, "editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func composeHook(t Tmux, file string) int {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || !isPromptFile(file) {
		if err := runEditor(file); err != nil {
			return 1
		}
		return 0
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsm compose:", err)
		return 1
	}
	c := Compose{ID: newID(), Host: hostname(), Pane: pane, Text: string(data)}
	result := offerCompose(c, composeWait)
	if result == "" {
		var err error
		if result, err = openCompose(t, pane, "", c); err != nil {
			result = ""
			fmt.Fprintln(os.Stderr, "tsm compose:", err)
		}
	}
	switch result {
	case composeOpened:
		return emptied(file)
	case composeFocused:
		return 0
	default:
		// Nothing opened: the editor of last resort runs where the harness
		// expected it.
		if err := runEditor(file); err != nil {
			return 1
		}
		return 0
	}
}

func emptied(file string) int {
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "tsm compose:", err)
		return 1
	}
	return 0
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

// offerCompose hands a compose to the relay above, if one is connected, and
// says what a host up there did with it: "" if none took it.
func offerCompose(c Compose, wait time.Duration) string {
	conn, err := net.DialTimeout("unix", relaySocket(), 500*time.Millisecond)
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(wait))
	if json.NewEncoder(conn).Encode(sockMsg{Compose: &c}) != nil {
		return ""
	}
	var answer struct {
		Result string `json:"result"`
	}
	if json.NewDecoder(bufio.NewReader(conn)).Decode(&answer) != nil {
		return ""
	}
	switch answer.Result {
	case composeOpened, composeFocused:
		return answer.Result
	}
	return ""
}

// A target is where a draft goes back to: a pane here ("%5"), or the way to
// one through the remotes ("karif:%5", "agalloch:karif:%5").
func target(remote, pane string) string {
	if remote == "" {
		return pane
	}
	return remote + ":" + pane
}

// splitTarget takes the first remote off a target: the rest is the pane as
// that remote names it.
func splitTarget(to string) (remote, pane string) {
	if strings.HasPrefix(to, "%") {
		return "", to
	}
	if r, p, ok := strings.Cut(to, ":"); ok {
		return r, p
	}
	return "", to
}

func draftKey(remote, pane string) string {
	host := remote
	if host == "" {
		host = "local"
	}
	return slug(host + " " + pane)
}

func draftPath(key string) string { return filepath.Join(cacheDir(), "drafts", key+".md") }

// openCompose opens the editor for c in a split right of anchor, a pane on
// this tmux: the agent's own, or the one here that reaches its remote.
func openCompose(t Tmux, anchor, remote string, c Compose) (string, error) {
	key := draftKey(remote, c.Pane)
	to := target(remote, c.Pane)
	if open := t.composePane(key); open != "" {
		// One editor per agent: go to the one already open.
		t.run("select-window", "-t", open)
		t.run("select-pane", "-t", open)
		return composeFocused, nil
	}
	path := draftPath(key)
	os.MkdirAll(filepath.Dir(path), 0o700)
	text := c.Text
	if old, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(old)) != "" {
		// A draft that never made it back (the remote was gone) comes first.
		text = strings.TrimRight(string(old), "\n")
		if strings.TrimSpace(c.Text) != "" {
			text += "\n\n" + c.Text
		}
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		self = "tsm"
	}
	out, err := t.run("split-window", "-h", "-t", anchor, "-P", "-F", "#{pane_id}",
		self, "compose", "--edit", path, "--to", to)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	t.run("set-option", "-p", "-t", id, "@tsm_compose", key)
	t.run("select-pane", "-t", id, "-T", "compose → "+to)
	return composeOpened, nil
}

// composePane is the editor pane already open for a draft, if any.
func (t Tmux) composePane(key string) string {
	out, err := t.run("list-panes", "-a", "-F", "#{pane_id}\t#{@tsm_compose}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if id, k, ok := strings.Cut(line, "\t"); ok && k == key {
			return id
		}
	}
	return ""
}

// composeEdit runs in the split: edit, then paste back, keeping the draft
// until it has arrived.
func composeEdit(t Tmux, path, to string) int {
	in := bufio.NewReader(os.Stdin)
	for edit := true; ; {
		if edit {
			if err := runEditor(path); err != nil {
				fmt.Printf("the editor failed (%v); the draft is kept in %s\n", err, tilde(path))
				waitKey(in)
				return 1
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Println("tsm compose:", err)
			waitKey(in)
			return 1
		}
		if strings.TrimSpace(string(data)) == "" {
			os.Remove(path)
			return 0
		}
		err = sendDraft(t, to, string(data))
		if err == nil {
			os.Remove(path)
			return 0
		}
		fmt.Printf("\ncould not paste into %s: %v\n[r]etry  [e]dit  [q]uit (keeps the draft)  ", to, err)
		switch waitKey(in) {
		case "r":
			edit = false
		case "e":
			edit = true
		default:
			fmt.Println("draft kept:", tilde(path))
			return 1
		}
	}
}

func waitKey(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}

// sendDraft pastes text into the target's prompt box.
func sendDraft(t Tmux, to, text string) error {
	text = strings.TrimRight(text, "\n")
	remote, pane := splitTarget(to)
	if remote == "" {
		return t.Paste(pane, text)
	}
	conf, err := LoadConfig()
	if err != nil {
		return err
	}
	for _, rc := range conf.Remotes {
		if rc.Name == remote {
			_, err := Remote{Conf: rc}.Do(Op{Action: "paste", Pane: pane, Text: text})
			return err
		}
	}
	return fmt.Errorf("no remote %q in %s", remote, tilde(configPath()))
}

// Paste puts text into a pane as one bracketed paste: the program there sees
// it all at once, newlines included, and nothing in it presses enter.
func (t Tmux) Paste(pane, text string) error {
	f, err := os.CreateTemp("", "tsm-paste-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(text)
	f.Close()
	if err != nil {
		return err
	}
	buf := "tsm-compose-" + strings.TrimPrefix(pane, "%")
	if _, err := t.run("load-buffer", "-b", buf, f.Name()); err != nil {
		return err
	}
	_, err = t.run("paste-buffer", "-p", "-d", "-b", buf, "-t", pane)
	return err
}

// compose is what the relay does with a compose that came up from a remote,
// if a pane in this tmux reaches that remote: offer it further up, to a
// host whose tmux reaches this one, and open it here only if none takes it.
// The topmost tmux is the one at your fingers.
func (rl *relay) compose(c Compose, from RemoteConf) string {
	snap, err := rl.tmux.Snapshot()
	if err != nil {
		return composeDeclined
	}
	hosting := hostingPane(snap, from)
	if hosting == nil {
		return composeDeclined
	}
	if c.Hops < maxHops {
		up := c
		up.Pane, up.Hops = target(from.Name, c.Pane), c.Hops+1
		if result := offerCompose(up, composeUpFor); result != "" {
			return result
		}
	}
	result, err := openCompose(rl.tmux, hosting.ID, from.Name, c)
	if err != nil {
		return composeDeclined
	}
	return result
}
