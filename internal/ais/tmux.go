package ais

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type result struct {
	Code   int
	Stdout string
	Stderr string
}

func (r result) ok() bool { return r.Code == 0 }

// run runs a command to completion and captures its output; a command that
// cannot start at all counts as exit 127.
func run(dir string, name string, args ...string) result {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 127
			errb.WriteString(err.Error())
		}
	}
	return result{Code: code, Stdout: out.String(), Stderr: errb.String()}
}

func tmux(args ...string) result { return run("", "tmux", args...) }

func haveTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func requireTmux() string {
	if os.Getenv("TMUX") == "" {
		die("not inside tmux — ais tracks sessions so it can jump back to them, " +
			"which needs a tmux pane. Start tmux first.")
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		die("inside tmux but $TMUX_PANE is unset; cannot locate this pane")
	}
	if !haveTmux() {
		die("tmux not found on PATH")
	}
	return pane
}

var locationKeys = []string{"session_name", "session_id", "window_id", "window_index", "pane_id", "pane_index"}

func locationFormat() string {
	parts := make([]string, len(locationKeys))
	for i, k := range locationKeys {
		parts[i] = "#{" + k + "}"
	}
	return strings.Join(parts, "\t")
}

func parseLocation(line string) map[string]string {
	parts := strings.Split(strings.TrimRight(line, "\n"), "\t")
	if len(parts) != len(locationKeys) {
		return nil
	}
	out := map[string]string{}
	for i, k := range locationKeys {
		out[k] = parts[i]
	}
	return out
}

func captureLocation(pane string) map[string]string {
	r := tmux("display-message", "-p", "-t", pane, "-F", locationFormat())
	if !r.ok() {
		return nil
	}
	return parseLocation(r.Stdout)
}

// paneLocations is where every pane on the server is right now, by pane id,
// in one call.
//
// Pane ids are the one thing about a pane that never changes: join-pane,
// break-pane and move-window all carry them along. Everything else here can
// change under a running session, which is why it is looked up, not trusted.
func paneLocations() map[string]map[string]string {
	r := tmux("list-panes", "-a", "-F", locationFormat())
	out := map[string]map[string]string{}
	if !r.ok() {
		return out
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if loc := parseLocation(line); loc != nil {
			out[loc["pane_id"]] = loc
		}
	}
	return out
}

// paneIsVisible is true when this pane is the one a tmux client is currently
// showing: active pane of its window, active window of its session, and some
// client attached to that session. Deliberately blind to whether the terminal
// emulator is the frontmost app — that is a second question, for later.
//
// Unknowable counts as not visible: a missed notification is worse than one
// that arrives while you are already reading the pane.
func paneIsVisible(pane string) bool {
	if pane == "" || !haveTmux() {
		return false
	}
	r := tmux("display-message", "-p", "-t", pane, "-F", "#{pane_active}\t#{window_active}\t#{session_attached}")
	if !r.ok() {
		return false
	}
	parts := strings.Split(strings.TrimSpace(r.Stdout), "\t")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return false
		}
	}
	return true
}

// shownPanes are the panes tmux clients are showing right now.
func shownPanes() map[string]bool {
	out := map[string]bool{}
	if !haveTmux() {
		return out
	}
	r := tmux("list-clients", "-F", "#{pane_id}")
	if !r.ok() {
		return out
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out
}

// The events after which a tmux client may be showing a different pane, and
// the slot ais takes in each hook's array: clear of the [0] a plain "set-hook
// -g" fills, and of tsm's 77.
var seenHooks = []string{
	"window-pane-changed", "session-window-changed", "client-session-changed",
	"client-attached", "client-focus-in",
}

const seenHookIndex = 78

// installSeenHooks has tmux run "ais _mark-seen" whenever what a client shows
// changes. Global and idempotent, so every session sets them again on the way
// in: a re-sourced tmux.conf replaces a hook's whole array, ais's slot with
// it. By path, not by name: tmux runs it with the server's PATH, not ours.
func installSeenHooks() {
	if !haveTmux() || os.Getenv("TMUX") == "" {
		return
	}
	line := shellJoin([]string{self(), "_mark-seen"})
	var args []string
	for _, hook := range seenHooks {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, "set-hook", "-g", hook+"["+strconv.Itoa(seenHookIndex)+"]", "run-shell -b "+shellQuote(line))
	}
	tmux(args...)
}

// self is this binary's resolved path, for things that run ais later from
// somewhere with another PATH.
func self() string {
	exe, err := os.Executable()
	if err != nil {
		return prog
	}
	if resolved, err := resolveLinks(exe); err == nil {
		return resolved
	}
	return exe
}

// windowName is what to call the window a session runs in: the session name
// first, so the window agrees with "ais name" and with the worktree and
// branch that name built; failing that the harness.
func windowName(r *Record) string {
	for _, s := range []string{r.Slug, slugify(r.desc()), r.harness()} {
		if s != "" {
			return s
		}
	}
	return prog
}

// nameWindow names the window after the session for as long as it runs; the
// returned func puts the old name back.
//
// Left alone, tmux names a window after the process in its active pane, which
// for every ais session is ais itself. Holding a name means turning
// automatic-rename off, so both the name and that option are put back on the
// way out; an option that was never set on this window is restored by
// unsetting it, not by writing the value it happened to inherit.
func nameWindow(pane, name string) func() {
	before := tmux("display-message", "-p", "-t", pane, "-F", "#{window_name}")
	oldName := ""
	if before.ok() {
		oldName = strings.TrimSpace(before.Stdout)
	}
	auto := tmux("show-window-options", "-v", "-t", pane, "automatic-rename")
	oldAuto := ""
	if auto.ok() {
		oldAuto = strings.TrimSpace(auto.Stdout)
	}
	renamed := tmux("set-window-option", "-t", pane, "automatic-rename", "off").ok() &&
		tmux("rename-window", "-t", pane, name).ok()
	return func() {
		if !renamed {
			return
		}
		if oldName != "" {
			tmux("rename-window", "-t", pane, oldName)
		}
		if oldAuto != "" {
			tmux("set-window-option", "-t", pane, "automatic-rename", oldAuto)
		} else {
			tmux("set-window-option", "-u", "-t", pane, "automatic-rename")
		}
	}
}

// paneMarks are the pane options that say what is running in a session's
// pane. tmux keeps them with the pane wherever it moves; @cwd is the
// session's directory, which pane_current_path cannot be trusted to know.
func paneMarks(r *Record) [][2]string {
	var out [][2]string
	for _, kv := range [][2]string{
		{"@ais_id", r.ID}, {"@ais_profile", r.profile()}, {"@ais_desc", r.desc()}, {"@cwd", r.Cwd},
	} {
		if kv[1] != "" {
			out = append(out, kv)
		}
	}
	return out
}

// markPane marks the pane for as long as the session runs; the returned func
// unsets the marks (not restores: they only ever describe a running session).
func markPane(pane string, r *Record) func() {
	marks := paneMarks(r)
	for _, kv := range marks {
		tmux("set-option", "-p", "-t", pane, kv[0], kv[1])
	}
	return func() {
		for _, kv := range marks {
			tmux("set-option", "-p", "-u", "-t", pane, kv[0])
		}
	}
}

func tmuxLabel(r *Record) string {
	if len(r.Tmux) == 0 {
		return "-"
	}
	get := func(k string) string {
		if v, ok := r.Tmux[k]; ok {
			return v
		}
		return "?"
	}
	return get("session_name") + ":" + get("window_index") + "." + get("pane_id")
}

// ---------------------------------------------------------------- shell quoting

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./-_", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

// shellSplit is shlex.split for the simple cases a config holds: words,
// single and double quotes, backslash escapes.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, quote := false, rune(0)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
