package ais

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

func nowUnix() int64 { return time.Now().Unix() }

func clientExists(name string) bool {
	r := tmux("list-clients", "-F", "#{client_name}")
	return contains(strings.Split(r.Stdout, "\n"), name)
}

// currentClient is the client tmux would act on for us, named explicitly. A
// popup has no $TMUX_PANE, but $TMUX still carries the session its client is
// attached to, which is all tmux needs to resolve "the current client".
func currentClient() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	r := tmux("display-message", "-p", "-F", "#{client_name}")
	if !r.ok() {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// resolveClient is the client to move: what the caller named, else what tmux
// infers. A --client naming no live client is dropped rather than fatal: the
// usual reason is a key binding whose "#{client_name}" was never expanded.
func resolveClient(name string) string {
	if name != "" && !clientExists(name) {
		name = ""
	}
	return orDefault(name, currentClient())
}

func clientPane(client string) string {
	if client == "" {
		return ""
	}
	r := tmux("display-message", "-p", "-c", client, "-F", "#{pane_id}")
	if !r.ok() {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

func sessionAtPane(pane string, ss []*Record) string {
	if pane == "" {
		return ""
	}
	for _, s := range ss {
		if s.Tmux["pane_id"] == pane {
			return s.ID
		}
	}
	return ""
}

// jumpTo points a tmux client at this session's pane — where the pane is now,
// not where it started: panes move between windows and sessions.
func jumpTo(r *Record, client string) int {
	pane := r.Tmux["pane_id"]
	if pane == "" {
		die("session %s has no recorded tmux location", r.ID)
	}
	loc := captureLocation(pane)
	if loc == nil || loc["pane_id"] != pane {
		die("tmux pane %s for session %s is gone", pane, r.ID)
	}
	target := orDefault(loc["session_id"], loc["session_name"])
	var res result
	switch {
	case client != "":
		res = tmux("switch-client", "-c", client, "-t", target)
	case os.Getenv("TMUX") != "":
		res = tmux("switch-client", "-t", target)
	default:
		// Outside tmux: attaching is the only way to get a client onto it.
		cmd := exec.Command("tmux", "attach-session", "-t", target)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			die("tmux: could not reach that session")
		}
	}
	if res.Code != 0 {
		die("tmux: %s", orDefault(strings.TrimSpace(res.Stderr), "could not reach that session"))
	}
	tmux("select-window", "-t", pane)
	tmux("select-pane", "-t", pane)
	return 0
}

func cmdSelect(id, client string) int {
	ss := active(reapedSessions())
	byStartedDesc(ss)
	if len(ss) == 0 {
		note("no active sessions")
		return 1
	}
	window := notifySettings().Window
	// Sessions that have called for you float to the top, so jumping between
	// the ones that want attention is a matter of taking the first rows.
	sort.SliceStable(ss, func(i, j int) bool {
		ci, cj := calling(ss[i], window), calling(ss[j], window)
		if ci != cj {
			return ci
		}
		return lastAt(ss[i]) > lastAt(ss[j])
	})
	client = resolveClient(client)
	here := os.Getenv("AIS_SESSION")
	if here == "" {
		here = sessionAtPane(clientPane(client), ss)
	}
	if id != "" {
		var matches []*Record
		for _, s := range ss {
			if strings.HasPrefix(s.ID, id) {
				matches = append(matches, s)
			}
		}
		if len(matches) == 0 {
			die("no active session with id starting %s", pyRepr(id))
		}
		if len(matches) > 1 {
			die("%s matches %d sessions", pyRepr(id), len(matches))
		}
		return jumpTo(matches[0], client)
	}
	chosen := pick(ss, here, window)
	if chosen == nil {
		return 130
	}
	return jumpTo(chosen, client)
}

func pick(ss []*Record, here string, window float64) *Record {
	rows := make([]string, len(ss))
	for i, s := range ss {
		fresh := recentNotes(s, window)
		mark := " "
		if calling(s, window) {
			mark = "●"
		} else if s.ID == here {
			mark = "*"
		}
		var trailer string
		if len(fresh) > 0 {
			last := fresh[len(fresh)-1]
			trailer = fmt.Sprintf("%4s  %s", ageStr(secondsSince(last.At)), elide(last.Message, 34))
		} else {
			trailer = fmt.Sprintf("%4s  %s", "", elide(tilde(orQ(s.Cwd)), 34))
		}
		rows[i] = s.ID + "\t" + fmt.Sprintf("%s %-58s %4s  %-10s %s", mark, elide(describe(s), 58),
			ageStr(secondsSince(s.StartedAt)), orQ(s.harness()), trailer)
	}

	if _, err := exec.LookPath("fzf"); err == nil && isTerminal(os.Stdin) {
		preview := shellQuote(self()) + " _preview {1}"
		cmd := exec.Command("fzf", "--delimiter=\t", "--with-nth=2..", "--prompt=ais session> ",
			"--header=enter select   esc cancel   ● notified recently", "--height=~60%", "--reverse",
			"--preview", preview, "--preview-window=right:44%")
		cmd.Stdin = strings.NewReader(strings.Join(rows, "\n"))
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil
		}
		sid, _, _ := strings.Cut(string(out), "\t")
		return findID(ss, strings.TrimSpace(sid))
	}

	// fzf missing or not a tty: numbered prompt
	for i, row := range rows {
		_, text, _ := strings.Cut(row, "\t")
		fmt.Printf("%3d. %s\n", i+1, text)
	}
	fmt.Print("select> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(ss) {
		die("not a valid selection")
	}
	return ss[n-1]
}

// cmdPreview renders the fzf preview pane for one session.
func cmdPreview(id string) int {
	r := findID(readSessions(), id)
	if r == nil {
		return 1
	}
	pid := "-"
	if r.pid() != 0 {
		pid = strconv.Itoa(r.pid())
	}
	fields := [][2]string{
		{"id", r.ID},
		{"name", orDefault(r.Slug, "-")},
		{"status", orQ(r.Status)},
		{"harness", orQ(r.harness())},
		{"age", ageStr(secondsSince(r.StartedAt))},
		{"started", orQ(r.StartedAt)},
		{"cwd", tilde(orQ(r.Cwd))},
		{"tmux", tmuxLabel(r)},
		{"pid", pid},
		{"host", orQ(r.Hostname)},
		{orDefault(r.harness(), "session"), orDefault(r.conversation(), "-")},
	}
	if b := r.wt().Base; b != "" {
		fields = append(fields, [2]string{"worktrees", tilde(b)})
	}
	if r.RestartOf != "" {
		fields = append(fields, [2]string{"restart of", r.RestartOf})
	}
	if d := strings.TrimSpace(r.desc()); d != "" {
		fmt.Println(d)
		fmt.Println()
	}
	for _, kv := range fields {
		fmt.Printf("%8s  %s\n", kv[0], kv[1])
	}
	fmt.Printf("%8s  %s\n", "command", shellJoin(r.Command))
	items := r.Notes
	if len(items) > 5 {
		items = items[len(items)-5:]
	}
	if len(items) > 0 {
		fmt.Println()
		fmt.Println("notifications")
		for _, n := range items {
			flag := ""
			if !n.Delivered {
				flag = "  (no banner)"
			}
			fmt.Printf("%8s  %s%s\n", ageStr(secondsSince(n.At)), n.Message, flag)
		}
	}
	return 0
}

func cmdCurrent() int {
	sid := os.Getenv("AIS_SESSION")
	if sid == "" {
		note("not running inside an ais session")
		return 1
	}
	if findID(readSessions(), sid) == nil {
		note("AIS_SESSION=%s is not in %s", sid, tilde(sessionPath))
		return 1
	}
	return cmdPreview(sid)
}
