package flow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const usageText = `usage: ais flow <command> [options]

Planned and dispatched sessions: see docs/flow-proposal.md.

  plan [WS] [--auto] [--dry-run]
                               start a planner session if there is plannable
                               work (ready tickets, or an epic's open
                               sub-issues) and none is open; close one that
                               has waited idle_timeout for you, or whose
                               session ended. --auto (the timer) also needs
                               room below wip and min_interval since the last
                               one; ais flow done asks too, when it frees a
                               slot
  plan done [ID|WS] [--reason TEXT]
                               you decided: the planner is finished, its
                               window closes once you leave it alone
  workspace [WS] [--json]      a workspace's repos, profile and statuses
  ready [--ws W] [--json]      the workspace's tickets marked ready, epics with
                               their sub-issues: what is held, what plannable
  queue add --ws W --title T --tickets T1,T2 --repos R1,R2
            [--profile P] [--priority N] [--after ID|TICKET,...] [--autonomy L2|L3]
            [--notes TEXT | --notes-file F] [--prompt TEXT | --prompt-file F]
                               queue a session you accepted; its tickets move
                               to the workspace's scheduled status
  queue ls [--ws W] [--all] [--json]
                               the queue, highest priority first
  queue show ID                one proposal, prompt included
  queue edit ID [--title T] [--priority N] [--repos ...] [--after ...] [--autonomy ...]
                [--notes ... | --notes-file F] [--prompt ... | --prompt-file F]
  queue cancel ID [--reason TEXT]
                               drop a queued proposal; its tickets go back to
                               ready
  limit                        the limits in force, and where each comes from
  limit KEY VALUE [--until eod|HH:MM|YYYY-MM-DD | --for 3h]
                               raise or lower wip, per_hour or per_day for a
                               while (default: until the end of the day)
  limit --clear [KEY]          drop overrides
  dispatch [--dry-run]         start queued sessions while the limits allow
                               (what the timer runs every five minutes)
  dispatch --now ID            start one now, past the limits and its "after"
  done [ID] [--force]          the session's work is finished: frees its slot.
                               ID is a proposal or session; default: this one.
                               Refuses while review check finds a plan left
                               on its branches (--force: anyway)
  ticket <cmd> REF ...         the task layer, for agents and you: show,
                               comment, close, new, assess, refine;
                               ais flow ticket --help
  review <cmd> ...             a session's plan as a draft PR: open, update,
                               approve, finalize, check; ais flow review --help
  refine REF [--ws W]          queue a refinement run for a ticket now, past
                               its round limit and an unchanged version
  prompt show ROLE | rubric    the prompts sessions start with (planner, work,
                               refine), as built in or overridden

  install [-n] [--no-enable]   write and enable the systemd user timer
  ls                           workspaces: planner, queue, running, limits used
  log [-n N]                   what flow did, newest first

Tickets are issue URLs or owner/repo#number; lists take commas or repeats.
`

// errUsage is a mistake on the command line: exit 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

var stdout io.Writer = os.Stdout

// Main runs "ais flow" with args (after "flow") and returns the exit code.
func Main(args []string) int {
	if len(args) > 0 && args[0] == "start" {
		var code int
		var err error
		if len(args) == 3 && args[1] == "--planner" {
			code, err = cmdStartPlanner(args[2])
		} else {
			code, err = cmdStart(args[1:])
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ais flow: %s\n", err)
		}
		return code
	}
	err := run(args)
	if err == nil {
		return 0
	}
	fmt.Fprintf(os.Stderr, "ais flow: %s\n", err)
	var u usageError
	if errors.As(err, &u) {
		return 2
	}
	return 1
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usageText)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ready":
		return cmdReady(rest)
	case "queue":
		if len(rest) == 0 {
			return cmdQueueLs(nil)
		}
		sub, rest := rest[0], rest[1:]
		switch sub {
		case "add":
			return cmdQueueAdd(rest)
		case "ls", "list":
			return cmdQueueLs(rest)
		case "show":
			return cmdQueueShow(rest)
		case "edit":
			return cmdQueueEdit(rest)
		case "cancel":
			return cmdQueueCancel(rest)
		}
		return usagef("unknown queue command %q; try add, ls, show, edit, cancel", sub)
	case "plan":
		return cmdPlan(rest)
	case "workspace", "workspaces", "ws":
		return cmdWorkspace(rest)
	case "dispatch":
		return cmdDispatch(rest)
	case "done":
		return cmdDone(rest)
	case "install":
		return cmdInstall(rest)
	case "limit", "limits":
		return cmdLimit(rest)
	case "ls", "status":
		return cmdLs(rest)
	case "log":
		return cmdLog(rest)
	case "ticket", "tickets":
		return cmdTicket(rest)
	case "prompt", "prompts":
		return cmdPrompt(rest)
	case "review":
		return cmdReview(rest)
	case "refine":
		return cmdRefineNow(rest)
	}
	return usagef("unknown command %q; try 'ais flow --help'", cmd)
}

// ---------------------------------------------------------------- arguments

// opts is a parsed command line: --name value (or --name=value) for the
// value options, --name for the flags, the rest positional. List options
// may repeat and take commas.
type opts struct {
	vals  map[string][]string
	flags map[string]bool
	pos   []string
}

func parseOpts(args []string, values, flags []string, aliases map[string]string) (opts, error) {
	o := opts{vals: map[string][]string{}, flags: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.pos = append(o.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if full, ok := aliases[name]; ok {
			name = full
		}
		switch {
		case contains(flags, name) && !hasVal:
			o.flags[name] = true
		case contains(values, name):
			if !hasVal {
				if i+1 >= len(args) {
					return o, usagef("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			o.vals[name] = append(o.vals[name], val)
		default:
			return o, usagef("unknown option %s", a)
		}
	}
	return o, nil
}

func (o opts) one(name string) (string, bool) {
	v := o.vals[name]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func (o opts) str(name string) string { v, _ := o.one(name); return v }

// list is every value of a repeatable option, commas split.
func (o opts) list(name string) []string {
	var out []string
	for _, v := range o.vals[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// text is --x TEXT or --x-file PATH ("-": stdin); ok when either was given.
func (o opts) text(name string) (string, bool, error) {
	v, inline := o.one(name)
	path, fromFile := o.one(name + "-file")
	if inline && fromFile {
		return "", false, usagef("%s and %s-file both say it; pick one", name, name)
	}
	if !fromFile {
		return v, inline, nil
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimRight(string(data), "\n"), true, nil
}

// file is --x PATH read whole ("-": stdin); ok when it was given.
func (o opts) file(name string) (string, bool, error) {
	path, ok := o.one(name)
	if !ok {
		return "", false, nil
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
