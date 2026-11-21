package ais

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Every run ais has started, ongoing and past: sessions.json holds the live
// ones and the recently dead, history.jsonl what prune took out of it. prune
// archives before it drops, so the two together are the whole record, and
// "ais runs" and "ais stats" read both.
var (
	historyPath = filepath.Join(aisHome, "history.jsonl")
	logsDir     = filepath.Join(aisHome, "logs")
)

// logPath is where a headless session's output goes.
func logPath(id string) string { return filepath.Join(logsDir, id+".log") }

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// archive appends records to history.jsonl, one per line. It runs under the
// sessions lock, before prune writes sessions.json without them, so a record
// is never in neither.
func archive(rs []*Record) {
	if len(rs) == 0 {
		return
	}
	var buf bytes.Buffer
	for _, r := range rs {
		b, err := json.Marshal(r)
		must(err)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		die("cannot archive pruned sessions to %s: %v", tilde(historyPath), err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		die("cannot archive pruned sessions to %s: %v", tilde(historyPath), err)
	}
}

func readHistory() []*Record {
	f, err := os.Open(historyPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []*Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		r := &Record{}
		if json.Unmarshal(line, r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	return out
}

// allRuns is every run on record, newest first: live state wins over an
// archived copy of the same id.
func allRuns() []*Record {
	live := reapedSessions()
	seen := map[string]bool{}
	var out []*Record
	for _, r := range live {
		seen[r.ID] = true
		out = append(out, r)
	}
	for _, r := range readHistory() {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return startedAt(out[i]).After(startedAt(out[j])) })
	return out
}

func startedAt(r *Record) time.Time {
	t, _ := parseStamp(r.StartedAt)
	return t
}

// runKind is what started a run: a headless one (ais run --headless, which
// is how flow refines), a flow planner, a flow work session, or you.
func runKind(r *Record) string {
	switch {
	case r.Headless:
		return "headless"
	case r.FlowPlanner != "":
		return "planner"
	case r.FlowProposal != "":
		return "flow"
	}
	return "interactive"
}

var runKinds = []string{"interactive", "planner", "flow", "headless"}

// took is how long a run ran, or has been running.
func took(r *Record) (time.Duration, bool) {
	start, ok := parseStamp(r.StartedAt)
	if !ok {
		return 0, false
	}
	end := time.Now()
	if r.Status != "active" {
		if r.EndedAt == nil {
			return 0, false
		}
		if end, ok = parseStamp(*r.EndedAt); !ok {
			return 0, false
		}
	}
	return end.Sub(start), true
}

func durStr(d time.Duration, ok bool) string {
	if !ok {
		return "-"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// parseSince reads a span back from now: a Go duration (90m, 3h) or days
// and weeks (7d, 2w).
func parseSince(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	unit := s[len(s)-1]
	if unit == 'd' || unit == 'w' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		day := 24 * time.Hour
		if unit == 'w' {
			day *= 7
		}
		return time.Duration(n * float64(day)), true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

type runsArgs struct {
	Since  string
	Kind   string
	Active bool
	Limit  int // 0: no limit
	JSON   bool
}

// runRow is one run in "ais runs --json".
type runRow struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	Profile     string  `json:"profile"`
	Description string  `json:"description"`
	Host        string  `json:"host"`
	Cwd         string  `json:"cwd"`
	StartedAt   string  `json:"started_at"`
	EndedAt     *string `json:"ended_at"`
	Seconds     *int    `json:"seconds"`
	ExitCode    *int    `json:"exit_code"`
	Log         string  `json:"log,omitempty"`
	Proposal    string  `json:"flow_proposal,omitempty"`
	Planner     string  `json:"flow_planner,omitempty"`
}

func cmdRuns(a runsArgs) int {
	if a.Kind != "" && !contains(runKinds, a.Kind) {
		dieCode(2, "error: --kind %s: one of %s", a.Kind, strings.Join(runKinds, ", "))
	}
	var cutoff time.Time
	if a.Since != "" {
		d, ok := parseSince(a.Since)
		if !ok {
			dieCode(2, "error: --since %s: use a span such as 3h, 90m, 7d or 2w", pyRepr(a.Since))
		}
		cutoff = time.Now().Add(-d)
	}
	var rs []*Record
	for _, r := range allRuns() {
		if a.Kind != "" && runKind(r) != a.Kind {
			continue
		}
		if a.Active && r.Status != "active" {
			continue
		}
		if !cutoff.IsZero() && startedAt(r).Before(cutoff) {
			continue
		}
		rs = append(rs, r)
	}
	more := 0
	if a.Limit > 0 && len(rs) > a.Limit {
		more = len(rs) - a.Limit
		rs = rs[:a.Limit]
	}
	if a.JSON {
		rows := []runRow{}
		for _, r := range rs {
			row := runRow{ID: r.ID, Kind: runKind(r), Status: r.Status, Profile: r.profile(), Description: r.desc(),
				Host: r.Hostname, Cwd: r.Cwd, StartedAt: r.StartedAt, EndedAt: r.EndedAt, ExitCode: r.ExitCode,
				Log: r.Log, Proposal: r.FlowProposal, Planner: r.FlowPlanner}
			if d, ok := took(r); ok {
				s := int(d.Seconds())
				row.Seconds = &s
			}
			rows = append(rows, row)
		}
		return emitJSON(rows)
	}
	if len(rs) == 0 {
		note("no runs")
		return 0
	}
	fmt.Printf("%-11s %-11s %-6s %-12s %6s %4s %-10s DESCRIPTION\n", "ID", "KIND", "STATUS", "STARTED", "TOOK", "EXIT", "PROFILE")
	for _, r := range rs {
		exit := "-"
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		} else if r.Status != "active" {
			exit = "?"
		}
		status := r.Status
		if status == "active" {
			status = "live"
		}
		fmt.Printf("%-11s %-11s %-6s %-12s %6s %4s %-10s %s\n", r.ID, runKind(r), elide(orQ(status), 6),
			startedLabel(r), durStr(took(r)), exit, elide(orQ(r.profile()), 10), elide(describe(r), 56))
	}
	if more > 0 {
		note("%d older run(s) not shown; -a shows all, --since narrows", more)
	}
	return 0
}

func startedLabel(r *Record) string {
	t, ok := parseStamp(r.StartedAt)
	if !ok {
		return "?"
	}
	t = t.Local()
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "today " + t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 02 15:04")
}

// ---------------------------------------------------------------- log

type logArgs struct {
	ID     string
	Full   bool
	Follow bool
	Path   bool
}

// cmdLog shows what a run said. A headless run's own log by default; --full
// (and any interactive run) the harness's conversation transcript.
func cmdLog(a logArgs) int {
	r := pickRun(a.ID)
	if !a.Full && r.Headless && r.Log != "" {
		if a.Path {
			fmt.Println(r.Log)
			return 0
		}
		return showFile(r, r.Log, a.Follow, nil)
	}
	files := transcripts(r)
	if len(files) == 0 {
		why := "it has no recorded conversation"
		if c := r.conversation(); c != "" {
			why = "no transcript of conversation " + c + " under " + tilde(claudeDir()) + "/projects"
		}
		if r.Headless && r.Log != "" {
			why += "; without --full you get its log"
		}
		die("no transcript for %s: %s", r.ID, why)
	}
	if a.Path {
		for _, f := range files {
			fmt.Println(f)
		}
		return 0
	}
	for i, f := range files {
		follow := a.Follow && i == len(files)-1
		if code := showFile(r, f, follow, writeEntry); code != 0 {
			return code
		}
	}
	return 0
}

// pickRun is the one run, live or archived, whose id starts with wanted.
func pickRun(wanted string) *Record {
	var matches []*Record
	for _, r := range allRuns() {
		if strings.HasPrefix(r.ID, wanted) {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		die("no run with id starting %s", pyRepr(wanted))
	}
	if len(matches) > 1 {
		die("%s matches %d runs", pyRepr(wanted), len(matches))
	}
	return matches[0]
}

// transcripts are the conversation files of a claude run, oldest first: one
// per conversation it had (a /clear starts another).
func transcripts(r *Record) []string {
	var out []string
	for _, c := range r.SessionIDs {
		if c == "" {
			continue
		}
		hits, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", c+".jsonl"))
		out = append(out, hits...)
	}
	return out
}

// showFile prints path, through render when it is a transcript, then, with
// follow, keeps printing what is added until the run is over. render gets
// whole lines only: a line still being written waits for the next round.
func showFile(r *Record, path string, follow bool, render func(io.Writer, []byte)) int {
	f, err := os.Open(path)
	if err != nil {
		die("cannot read %s: %v", tilde(path), err)
	}
	defer f.Close()
	var carry []byte
	drain := func(final bool) {
		chunk, err := io.ReadAll(f)
		if err != nil {
			die("cannot read %s: %v", tilde(path), err)
		}
		if render == nil {
			os.Stdout.Write(chunk)
			return
		}
		carry = append(carry, chunk...)
		for {
			i := bytes.IndexByte(carry, '\n')
			if i < 0 {
				break
			}
			render(os.Stdout, carry[:i])
			carry = carry[i+1:]
		}
		if final && len(carry) > 0 {
			render(os.Stdout, carry)
			carry = nil
		}
	}
	drain(!follow)
	for follow {
		if !runAlive(r.ID) {
			drain(true) // whatever it wrote on the way out
			return 0
		}
		time.Sleep(500 * time.Millisecond)
		drain(false)
	}
	return 0
}

func runAlive(id string) bool {
	s := findID(reapedSessions(), id)
	return s != nil && s.Status == "active"
}

// writeEntry prints one line of a claude transcript as conversation: what was
// asked, what was said, which tools ran. Everything else in it is skipped.
func writeEntry(w io.Writer, line []byte) {
	var e struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		IsMeta    bool   `json:"isMeta"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.IsMeta || (e.Type != "user" && e.Type != "assistant") {
		return
	}
	stamp := ""
	if t, ok := parseStamp(e.Timestamp); ok {
		stamp = "  " + t.Local().Format("15:04:05")
	}
	var text string
	if json.Unmarshal(e.Message.Content, &text) == nil {
		if e.Type == "user" {
			fmt.Fprintf(w, "\n── you%s\n%s\n", stamp, strings.TrimSpace(text))
		} else {
			fmt.Fprintf(w, "\n── agent%s\n%s\n", stamp, strings.TrimSpace(text))
		}
		return
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"is_error"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return
	}
	headed := false
	head := func(who string) {
		if !headed {
			fmt.Fprintf(w, "\n── %s%s\n", who, stamp)
			headed = true
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if e.Type == "user" {
				head("you")
			} else {
				head("agent")
			}
			fmt.Fprintln(w, strings.TrimSpace(b.Text))
		case "tool_use":
			head("agent")
			fmt.Fprintf(w, "  → %s %s\n", b.Name, oneLine(string(b.Input), 160))
		case "tool_result":
			mark := "←"
			if b.IsError {
				mark = "✗"
			}
			fmt.Fprintf(w, "  %s %s\n", mark, oneLine(resultText(b.Content), 160))
		}
	}
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, " ")
	}
	return string(raw)
}

func oneLine(s string, width int) string {
	return elide(strings.Join(strings.Fields(s), " "), width)
}

// ---------------------------------------------------------------- stats

type statsWindow struct {
	Name string
	Span time.Duration
}

var statsWindows = []statsWindow{
	{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour},
}

// statCell is what ran inside one window, for one kind.
type statCell struct {
	Started int     `json:"started"`
	Failed  int     `json:"failed"` // ended with an exit code other than 0
	Hours   float64 `json:"hours"`  // session time inside the window, open or working
}

// cmdStats counts the runs started in the last hour, day, week and month,
// by kind, with how many failed and how many hours they ran.
func cmdStats(asJSON bool) int {
	runs := allRuns()
	now := time.Now()
	table := map[string]map[string]*statCell{}
	for _, k := range append(append([]string{}, runKinds...), "total") {
		table[k] = map[string]*statCell{}
		for _, w := range statsWindows {
			table[k][w.Name] = &statCell{}
		}
	}
	running := map[string]int{}
	for _, r := range runs {
		kind := runKind(r)
		if r.Status == "active" {
			running[kind]++
			running["total"]++
		}
		start, ok := parseStamp(r.StartedAt)
		if !ok {
			continue
		}
		d, hasEnd := took(r)
		end := start.Add(d)
		for _, w := range statsWindows {
			from := now.Add(-w.Span)
			for _, k := range []string{kind, "total"} {
				c := table[k][w.Name]
				if !start.Before(from) {
					c.Started++
					if r.ExitCode != nil && *r.ExitCode != 0 {
						c.Failed++
					}
				}
				if hasEnd && end.After(from) {
					s := start
					if s.Before(from) {
						s = from
					}
					c.Hours += end.Sub(s).Hours()
				}
			}
		}
	}
	if asJSON {
		out := struct {
			Windows []string                        `json:"windows"`
			Kinds   map[string]map[string]*statCell `json:"kinds"`
			Running map[string]int                  `json:"running"`
		}{Kinds: table, Running: running}
		for _, w := range statsWindows {
			out.Windows = append(out.Windows, w.Name)
		}
		return emitJSON(out)
	}
	fmt.Printf("%-12s %4s", "started", "now")
	for _, w := range statsWindows {
		fmt.Printf(" %6s", w.Name)
	}
	fmt.Println()
	for _, k := range append(append([]string{}, runKinds...), "total") {
		fmt.Printf("%-12s %4d", k, running[k])
		for _, w := range statsWindows {
			fmt.Printf(" %6d", table[k][w.Name].Started)
		}
		fmt.Println()
	}
	fmt.Println()
	fmt.Printf("%-12s %4s", "", "")
	for _, w := range statsWindows {
		fmt.Printf(" %6s", w.Name)
	}
	fmt.Println()
	fmt.Printf("%-12s %4s", "failed", "")
	for _, w := range statsWindows {
		fmt.Printf(" %6d", table["total"][w.Name].Failed)
	}
	fmt.Println()
	fmt.Printf("%-12s %4s", "session hrs", "")
	for _, w := range statsWindows {
		fmt.Printf(" %6.1f", table["total"][w.Name].Hours)
	}
	fmt.Println()
	return 0
}
