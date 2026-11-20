package ais

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// currentSession is the session this process runs in, or die saying why not.
func currentSession() *Record {
	sid := os.Getenv("AIS_SESSION")
	if sid == "" {
		die("not inside an ais session; %s wt works on the session's own directory", prog)
	}
	r := findID(readSessions(), sid)
	if r == nil {
		die("AIS_SESSION=%s is not in %s", sid, tilde(sessionPath))
	}
	return r
}

type wtArgs struct {
	Sub, Repo string
	All       bool
	From      string
	Detach    *bool
}

func cmdWt(a wtArgs) int {
	switch a.Sub {
	case "add":
		return cmdWtAdd(a)
	case "ls", "list":
		return cmdWtLs(a)
	case "path":
		return cmdWtPath(a)
	case "prune":
		return cmdWtPrune()
	}
	die("unknown worktree command %s; try one of: add, ls, path, prune", pyRepr(a.Sub))
	return 1
}

// cmdWtAdd gives this session a worktree of another repo, beside the ones it
// has. Works whether or not the session was started with -w: a session that
// finds it needs a second repo halfway through should not have to restart.
func cmdWtAdd(a wtArgs) int {
	r := currentSession()
	wt := worktreeSettings(loadConfig()).with(a.From, a.Detach)
	target := expandUser(orDefault(a.Repo, "."))
	root := repoRoot(target)
	if root == "" {
		abs, _ := filepath.Abs(target)
		die("not a git repository: %s", tilde(abs))
	}
	name := sessionDirname(r)
	base := sessionWorktreeBase(r, wt)
	dest := filepath.Join(base, repoName(root))
	ensureWorktree(root, dest, name, wt)
	if !recordWorktree(r.ID, base, "", name, "") {
		note("session %s is not on record; worktree not remembered", r.ID)
	}
	fmt.Println(dest) // stdout is the path alone: cd "$(ais wt add ~/git/foo)"
	return 0
}

func cmdWtLs(a wtArgs) int {
	wt := worktreeSettings(loadConfig())
	var bases []string
	initial := ""
	if a.All {
		bases = subdirs(wt.Base)
	} else {
		r := currentSession()
		bases = []string{sessionWorktreeBase(r, wt)}
		initial = r.wt().Initial
	}
	rows := 0
	for _, base := range bases {
		for _, path := range sessionWorktrees(base) {
			mark := " "
			if initial != "" && path == initial {
				mark = "*"
			}
			label, err := filepath.Rel(wt.Base, path)
			if err != nil || strings.HasPrefix(label, "..") {
				label = tilde(path) // base moved in config since it was made
			}
			line := fmt.Sprintf("%s %-44s %-30s %s", mark, elide(label, 44), elide(worktreeBranch(path), 30), worktreeState(path))
			fmt.Println(strings.TrimRight(line, " "))
			rows++
		}
	}
	if rows == 0 {
		if a.All {
			note("no worktrees under %s", tilde(wt.Base))
		} else {
			note("this session has no worktrees (%s wt add <repo> makes one)", prog)
		}
	}
	return 0
}

func cmdWtPath(a wtArgs) int {
	r := currentSession()
	wt := worktreeSettings(loadConfig())
	base := sessionWorktreeBase(r, wt)
	if a.Repo != "" {
		dest := filepath.Join(base, a.Repo)
		if !isWorktree(dest) {
			die("this session has no worktree of %s (%s)", pyRepr(a.Repo), tilde(dest))
		}
		fmt.Println(dest)
		return 0
	}
	if initial := r.wt().Initial; initial != "" && isWorktree(initial) {
		fmt.Println(initial)
		return 0
	}
	found := sessionWorktrees(base)
	if len(found) == 0 {
		die("this session has no worktrees (%s wt add <repo> makes one)", prog)
	}
	if len(found) > 1 {
		names := make([]string, len(found))
		for i, p := range found {
			names[i] = filepath.Base(p)
		}
		die("this session has several worktrees (%s); name one", strings.Join(names, ", "))
	}
	fmt.Println(found[0])
	return 0
}

// cmdWtPrune drops registrations git still holds for worktrees that are gone.
// Directory-level housekeeping only: it never removes a worktree that is
// still there. "ais prune" is what retires a dead session's work.
func cmdWtPrune() int {
	wt := worktreeSettings(loadConfig())
	if !isDir(wt.Base) {
		note("nothing to prune (%s does not exist)", tilde(wt.Base))
		return 0
	}
	var roots []string
	emptied := 0
	for _, base := range subdirs(wt.Base) {
		for _, path := range sessionWorktrees(base) {
			if root := repoRoot(path); root != "" && !contains(roots, root) {
				roots = append(roots, root)
			}
		}
		if os.Remove(base) == nil {
			emptied++
		}
	}
	for _, root := range roots {
		git("-C", root, "worktree", "prune")
	}
	note("pruned %d checkout(s), removed %d empty session director(ies)", len(roots), emptied)
	return 0
}

// ---------------------------------------------------------------- ls / prune

func byStartedDesc(ss []*Record) {
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].StartedAt > ss[j].StartedAt })
}

// cmdLs: one line per session. status: the same, plus recent notifications.
// The NOTIFY column is the age of the last notification; "●" in the margin
// means it landed inside the notify window and is probably still waiting.
func cmdLs(all, detail bool, window *float64) int {
	w := notifySettings().Window
	if window != nil {
		w = *window
	}
	ss := reapedSessions()
	if !all {
		ss = active(ss)
	}
	byStartedDesc(ss)
	if detail {
		// Whatever called for you goes first, most recent call at the top.
		sort.SliceStable(ss, func(i, j int) bool {
			ci, cj := calling(ss[i], w), calling(ss[j], w)
			if ci != cj {
				return ci
			}
			return lastAt(ss[i]) > lastAt(ss[j])
		})
	}
	if len(ss) == 0 {
		if all {
			note("no sessions")
		} else {
			note("no active sessions")
		}
		return 0
	}
	here := os.Getenv("AIS_SESSION")
	fmt.Printf("%-1s %-11s %-7s %4s %6s %-10s %-18s DESCRIPTION\n", "", "ID", "STATUS", "AGE", "NOTIFY", "HARNESS", "TMUX")
	anyRecent := false
	for _, s := range ss {
		mark := " "
		if calling(s, w) {
			mark = "●"
		} else if s.ID == here {
			mark = "*"
		}
		last := s.lastNote()
		notifyAge := "-"
		if last != nil {
			notifyAge = ageStr(secondsSince(last.At))
		}
		fmt.Printf("%s %-11s %-7s %4s %6s %-10s %-18s %s\n", mark, s.ID, orQ(s.Status), ageStr(secondsSince(s.StartedAt)),
			notifyAge, elide(orQ(s.harness()), 10), elide(tmuxLabel(s), 18), elide(describe(s), 48))
		if detail {
			recent := recentNotes(s, w)
			if len(recent) > 0 {
				anyRecent = true
			}
			if len(recent) > 3 {
				recent = recent[len(recent)-3:]
			}
			for _, n := range recent {
				flag := ""
				if !n.Delivered {
					flag = "  (no banner)"
				}
				fmt.Printf("    %4s ago  %s%s\n", ageStr(secondsSince(n.At)), n.Message, flag)
			}
		}
	}
	if detail && !anyRecent {
		note("no notifications in the last %dm", int(w))
	}
	return 0
}

func lastAt(r *Record) string {
	if n := r.lastNote(); n != nil {
		return n.At
	}
	return ""
}

func active(ss []*Record) []*Record {
	var out []*Record
	for _, s := range ss {
		if s.Status == "active" {
			out = append(out, s)
		}
	}
	return out
}

func cmdPrune(all bool, days float64, keepWorktrees bool) int {
	cutoff := float64(nowUnix()) - days*86400
	var dropped, kept []*Record
	mutate(func(ss []*Record) []*Record {
		reap(ss)
		dropped, kept = nil, nil
		for _, s := range ss {
			if s.Status == "active" {
				kept = append(kept, s)
				continue
			}
			if all {
				dropped = append(dropped, s)
				continue
			}
			ended := s.StartedAt
			if s.EndedAt != nil && *s.EndedAt != "" {
				ended = *s.EndedAt
			}
			stamp := 0.0
			if t, ok := parseStamp(ended); ok {
				stamp = float64(t.Unix())
			}
			if stamp < cutoff {
				dropped = append(dropped, s)
			} else {
				kept = append(kept, s)
			}
		}
		return kept
	})
	note("pruned %d dead session(s)", len(dropped))
	if !keepWorktrees {
		retireWorktrees(dropped, kept)
	}
	return 0
}

// retireWorktrees takes away the worktrees of pruned sessions, but only the
// finished ones. Uncommitted changes and unpushed commits both count as work
// that has nowhere else to live: a session record is cheap to lose, a
// directory of somebody's afternoon is not. A restart shares its parent's
// name, and so its directory — so a base still claimed by a surviving
// session is left entirely alone.
func retireWorktrees(dropped, kept []*Record) {
	live := map[string]bool{}
	var cwds []string
	for _, s := range kept {
		if b := s.wt().Base; b != "" {
			live[b] = true
		}
		if s.Cwd != "" {
			cwds = append(cwds, s.Cwd)
		}
	}
	removed := 0
	var held []string
	for _, s := range dropped {
		base := s.wt().Base
		if base == "" || live[base] || !isDir(base) {
			continue
		}
		standing := false
		for _, c := range cwds {
			if c == base || strings.HasPrefix(c, base+"/") {
				standing = true
			}
		}
		if standing {
			continue // somebody still on record is standing in it
		}
		live[base] = true // two dropped sessions can share one base; handle it once
		for _, path := range sessionWorktrees(base) {
			if state := worktreeState(path); state != "" {
				held = append(held, fmt.Sprintf("%s (%s)", tilde(path), state))
				continue
			}
			if removeWorktree(path) {
				removed++
			}
		}
		os.Remove(base)
	}
	if removed > 0 {
		note("removed %d finished worktree(s)", removed)
	}
	for _, item := range held {
		note("kept %s", item)
	}
	if len(held) > 0 {
		note("%s prune leaves unsaved work alone; push it or delete it by hand", prog)
	}
}
