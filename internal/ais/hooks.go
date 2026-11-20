package ais

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM) // EPERM: exists, just not ours
}

// reap marks sessions dead when their process or tmux pane is gone. Only
// sessions recorded on this host are judged: a pid from another machine says
// nothing about a pid here.
func reap(ss []*Record) int {
	host := hostname()
	revive(ss, host)
	var candidates []*Record
	for _, s := range ss {
		if s.Status == "active" && s.Hostname == host {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return 0
	}
	panes := paneLocations() // one tmux call for the whole sweep
	reaped := 0
	for _, s := range candidates {
		pid := s.pid()
		if pid == 0 {
			pid = s.LauncherPID // see launch()
		}
		paneID := s.Tmux["pane_id"]
		loc, paneLive := panes[paneID]
		// pids get reused, so require both signals before declaring death
		if processAlive(pid) && paneID != "" && paneLive {
			merged := map[string]string{}
			for k, v := range s.Tmux {
				merged[k] = v
			}
			for k, v := range loc {
				merged[k] = v
			}
			s.Tmux = merged // keep the record up with wherever the pane is now
			continue
		}
		s.Status = "dead"
		if s.EndedAt == nil || *s.EndedAt == "" {
			s.EndedAt = strPtr(nowISO())
		}
		s.Reaped = true
		reaped++
	}
	return reaped
}

// revive undoes a reap that was wrong: a session that is still running.
//
// Before launcher_pid, a reader that reaped in the moment between a session
// being recorded and its harness getting a pid buried it alive, and a reap is
// never revisited. A reaped session whose process is alive and whose pane
// still carries its own @ais_id mark is running: only ais sets that mark, and
// it takes it away when the session ends, so pid reuse cannot fake it.
func revive(ss []*Record, host string) {
	var buried []*Record
	for _, s := range ss {
		if s.Status == "dead" && s.Reaped && s.Hostname == host && processAlive(s.pid()) {
			buried = append(buried, s)
		}
	}
	if len(buried) == 0 {
		return
	}
	r := tmux("list-panes", "-a", "-F", "#{pane_id}\t#{@ais_id}")
	if !r.ok() {
		return
	}
	marks := map[string]string{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if pane, mark, ok := strings.Cut(line, "\t"); ok {
			marks[pane] = mark
		}
	}
	for _, s := range buried {
		if marks[s.Tmux["pane_id"]] == s.ID {
			s.Status = "active"
			s.EndedAt = nil
			s.ExitCode = nil
			s.Reaped = false
		}
	}
}

// rememberSessionID appends a conversation id to a session's history.
func rememberSessionID(id, conversation string) bool {
	return update(id, func(r *Record) {
		var ids []string
		for _, i := range r.SessionIDs {
			if i != "" {
				ids = append(ids, i)
			}
		}
		if len(ids) > 0 && ids[len(ids)-1] == conversation {
			return // same conversation, nothing moved
		}
		ids = append(ids, conversation)
		if len(ids) > sessionIDKeep {
			ids = ids[len(ids)-sessionIDKeep:]
		}
		r.SessionIDs = ids
	})
}

// A harness conversation can be replaced mid-session (claude's /clear starts
// a fresh one in the same process), so the ids are a history, newest last.
const sessionIDKeep = 20

type ancestor struct {
	pid  int
	comm string
}

// ancestry is this process's ancestors, nearest first; empty when ps cannot
// be read, which callers must treat as "unknown", not as evidence.
// ancestry is a variable so tests can hand in a process tree.
var ancestry = psAncestry

func psAncestry() []ancestor {
	r := run("", "ps", "-Ao", "pid=,ppid=,comm=")
	if !r.ok() {
		return nil
	}
	type info struct {
		ppid int
		comm string
	}
	table := map[int]info{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		table[pid] = info{ppid, filepath.Base(strings.Join(fields[2:], " "))}
	}
	var chain []ancestor
	seen := map[int]bool{}
	pid := os.Getpid()
	for {
		in, ok := table[pid]
		if !ok || seen[pid] {
			break
		}
		seen[pid] = true
		pid = in.ppid
		if pid <= 1 {
			break
		}
		chain = append(chain, ancestor{pid, table[pid].comm})
	}
	return chain
}

// speaksFor is true when this process belongs to the session's own harness.
//
// AIS_SESSION is inherited by everything the harness spawns, including
// another harness — a nested "claude -p" would otherwise report its own
// throwaway conversation as the session's. So look up the ancestry for the
// nearest harness process: if there is one and it is not the one ais started,
// this is a nested run and it does not get to speak.
//
// Unknowable counts as yes: on the host that started it, the overwhelmingly
// common case is the genuine session.
func speaksFor(r *Record) bool {
	pid := r.pid()
	chain := ancestry()
	if pid == 0 || len(chain) == 0 {
		return true
	}
	names := map[string]bool{}
	if n := filepath.Base(r.Harness); r.Harness != "" {
		names[n] = true
	}
	if len(r.Command) > 0 && r.Command[0] != "" {
		names[filepath.Base(r.Command[0])] = true
	}
	for _, a := range chain {
		if a.pid == pid {
			return true
		}
		if names[a.comm] {
			return false // a nearer harness than ours: nested
		}
	}
	return false // ais's harness process is nowhere above us
}

// cmdSessionSeen is the harness telling ais which conversation it is in now.
// Wired up as a SessionStart hook by "ais install". Never fails loudly: a hook
// that errors is a hook that interrupts the work it was meant to observe.
func cmdSessionSeen(conversation string, verbose bool) (code int) {
	defer func() {
		if e := recover(); e != nil {
			code = 0
		}
	}()
	if conversation == "" {
		var raw []byte
		if !isTerminal(os.Stdin) {
			raw, _ = io.ReadAll(os.Stdin)
		}
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if len(strings.TrimSpace(string(raw))) > 0 && json.Unmarshal(raw, &payload) == nil {
			conversation = payload.SessionID
		}
	}
	if conversation == "" {
		// No payload: the harness still exports its own id, and a hook runs
		// as its child.
		for _, v := range []string{"AIS_HARNESS_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "PI_SESSION_ID"} {
			if conversation = os.Getenv(v); conversation != "" {
				break
			}
		}
	}
	if conversation == "" {
		return 0
	}
	sid := os.Getenv("AIS_SESSION")
	if sid == "" || os.Getenv("AIS_SIBLING") != "" {
		return 0 // a harness running outside ais
	}
	r := findID(readSessions(), sid)
	if r == nil || r.Hostname != hostname() || !speaksFor(r) {
		return 0
	}
	if !rememberSessionID(sid, conversation) {
		return 0
	}
	if verbose {
		note("%s: %s session %s", sid, orDefault(r.harness(), "harness"), conversation)
	}
	return 0
}

// cmdActivity is the harness telling ais you sent it a prompt: claude's
// UserPromptSubmit hook (payload on stdin) or pi's input event (--text). It
// stamps last_input_at, which is how long a session has waited for you.
//
// The first prompt ais gave the session arrives through the same hook, but it
// was ais typing, not you: the first submit whose text is that prompt only
// marks it delivered. Like every hook, it never fails loudly.
func cmdActivity(text *string) (code int) {
	defer func() {
		if e := recover(); e != nil {
			code = 0
		}
	}()
	prompt := ""
	if text != nil {
		prompt = *text
	} else if !isTerminal(os.Stdin) {
		raw, _ := io.ReadAll(os.Stdin)
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			prompt = payload.Prompt
		}
	}
	sid := os.Getenv("AIS_SESSION")
	if sid == "" || os.Getenv("AIS_SIBLING") != "" {
		return 0
	}
	r := findID(readSessions(), sid)
	if r == nil || r.Hostname != hostname() || !speaksFor(r) {
		return 0
	}
	update(sid, func(s *Record) {
		if s.InitialPrompt != "" && !s.InitialPromptSeen &&
			strings.TrimSpace(prompt) == strings.TrimSpace(s.InitialPrompt) {
			s.InitialPromptSeen = true
			return
		}
		s.LastInputAt = nowISO()
	})
	return 0
}

// markSeen stamps seen_at on the unseen notifications, older than before, of
// the sessions in panes; it returns the ids of those that had any. It reads
// first and writes only if there is something to stamp: tmux runs this on
// every switch, and nearly all of them find nothing.
func markSeen(panes map[string]bool, before string) []string {
	unseen := func(s *Record) []*Note {
		if s.Status != "active" || !panes[s.Tmux["pane_id"]] {
			return nil
		}
		var out []*Note
		for i := range s.Notes {
			if n := &s.Notes[i]; n.SeenAt == "" && n.ID < before {
				out = append(out, n)
			}
		}
		return out
	}
	if len(panes) == 0 {
		return nil
	}
	any := false
	for _, s := range readSessions() {
		if len(unseen(s)) > 0 {
			any = true
		}
	}
	if !any {
		return nil
	}
	var marked []string
	mutate(func(ss []*Record) []*Record {
		stamp := nowISO()
		for _, s := range ss {
			items := unseen(s)
			for _, n := range items {
				n.SeenAt = stamp
			}
			if len(items) > 0 {
				marked = append(marked, s.ID)
			}
		}
		return ss
	})
	return marked
}

// cmdMarkSeen is what tmux runs whenever a client changes what it shows:
// wait seen_after seconds, and whatever ais pane is still on screen has been
// seen. --pane names one instead, already looked at long enough (tsm relay).
func cmdMarkSeen(panesArg []string, verbose bool) int {
	before := notificationID()
	var panes map[string]bool
	if len(panesArg) > 0 {
		panes = map[string]bool{}
		for _, p := range panesArg {
			panes[p] = true
		}
	} else {
		panes = shownPanes()
		if wait := notifySettings().SeenAfter; len(panes) > 0 && wait > 0 {
			time.Sleep(time.Duration(wait * float64(time.Second)))
			still := shownPanes()
			for p := range panes {
				if !still[p] {
					delete(panes, p)
				}
			}
		}
	}
	for _, id := range markSeen(panes, before) {
		if verbose {
			fmt.Println(id)
		}
	}
	return 0
}

// isTerminal: a character device on stdin is a terminal (or /dev/null, which
// has nothing to read either way).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
