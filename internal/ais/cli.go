package ais

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Main runs ais with args (without the program name) and returns its exit
// code. die anywhere below unwinds to here, running deferred cleanups first.
func Main(args []string) (code int) {
	defer func() {
		if e := recover(); e != nil {
			x, ok := e.(exit)
			if !ok {
				panic(e)
			}
			if x.msg != "" {
				fmt.Fprintf(os.Stderr, "%s: %s\n", prog, x.msg)
			}
			code = x.code
		}
	}()
	return dispatch(args)
}

func usage() string {
	return strings.NewReplacer("{self}", tilde(self()), "{config}", tilde(configPath), "{sessions}", tilde(sessionPath)).Replace(usageText)
}

// ---------------------------------------------------------------- a small argparse

type optKind int

const (
	flagOpt  optKind = iota // -n: true when present
	valueOpt                // -d TEXT
	negOpt                  // --x / --no-x: three states
	listOpt                 // --pane P, repeatable
)

type option struct {
	names []string // first is the canonical key
	kind  optKind
	// choices limits a value, as argparse's choices= did
	choices []string
}

type parsed struct {
	flags  map[string]bool
	values map[string]string
	neg    map[string]bool // present only when given
	lists  map[string][]string
	pos    []string
	extra  []string // what parse could not place (parse_known_args)
}

func (p parsed) value(key string) *string {
	if v, ok := p.values[key]; ok {
		return &v
	}
	return nil
}

func (p parsed) negated(key string) *bool {
	if v, ok := p.neg[key]; ok {
		return &v
	}
	return nil
}

// parse reads args against opts the way argparse did: --opt=value and
// -dVALUE spellings, unique prefixes of long options, --no-x for negatable
// ones. Unknown options land in extra when known is true, else they fail.
func parse(args []string, opts []option, known bool) parsed {
	p := parsed{flags: map[string]bool{}, values: map[string]string{}, neg: map[string]bool{}, lists: map[string][]string{}}
	lookup := func(name string) (*option, bool, bool) { // option, negated, found
		var hits []*option
		var negs []bool
		for i := range opts {
			o := &opts[i]
			for _, n := range o.names {
				cands := []string{n}
				if o.kind == negOpt && strings.HasPrefix(n, "--") {
					cands = append(cands, "--no-"+n[2:])
				}
				for ci, c := range cands {
					if c == name {
						return o, ci == 1, true
					}
					if strings.HasPrefix(name, "--") && len(name) > 2 && strings.HasPrefix(c, name) {
						hits = append(hits, o)
						negs = append(negs, ci == 1)
					}
				}
			}
		}
		if len(hits) == 1 {
			return hits[0], negs[0], true
		}
		if len(hits) > 1 {
			dieCode(2, "error: ambiguous option: %s", name)
		}
		return nil, false, false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			p.pos = append(p.pos, arg)
			continue
		}
		name, inline, hasInline := arg, "", false
		if strings.HasPrefix(arg, "--") {
			name, inline, hasInline = strings.Cut(arg, "=")
		} else if len(arg) > 2 {
			// -dVALUE, or a cluster of short flags
			if o, _, ok := lookup(arg[:2]); ok && o.kind == valueOpt {
				name, inline, hasInline = arg[:2], arg[2:], true
			}
		}
		o, isNeg, ok := lookup(name)
		if !ok {
			if known {
				p.extra = append(p.extra, arg)
				continue
			}
			dieCode(2, "error: unrecognized arguments: %s", arg)
		}
		key := o.names[0]
		switch o.kind {
		case flagOpt:
			p.flags[key] = true
		case negOpt:
			p.neg[key] = !isNeg
		case valueOpt, listOpt:
			v := inline
			if !hasInline {
				if i+1 >= len(args) {
					dieCode(2, "error: argument %s: expected one argument", strings.Join(o.names, "/"))
				}
				i++
				v = args[i]
			}
			if len(o.choices) > 0 && !contains(o.choices, v) {
				dieCode(2, "error: argument %s: invalid choice: %s (choose from %s)", strings.Join(o.names, "/"), pyRepr(v), strings.Join(o.choices, ", "))
			}
			if o.kind == listOpt {
				p.lists[key] = append(p.lists[key], v)
			} else {
				p.values[key] = v
			}
		}
	}
	return p
}

var worktreeOpts = []option{
	{names: []string{"--from"}, kind: valueOpt, choices: wtFrom},
	{names: []string{"--detach"}, kind: negOpt},
}

// ---------------------------------------------------------------- dispatch

// splitArgv splits at the first bare "--": (ais args, harness argv or nil).
func splitArgv(argv []string) ([]string, []string, bool) {
	for i, a := range argv {
		if a == "--" {
			return argv[:i], argv[i+1:], true
		}
	}
	return argv, nil, false
}

// startSession takes the flags a session start takes, whether it came as
// "run" or "--".
func startSession(aisArgs, harnessArgv []string) int {
	p := parse(aisArgs, append([]option{
		{names: []string{"-d", "--description"}, kind: valueOpt},
		// Three states, not two: -w, --no-worktree, and said nothing — which
		// is the only one worktree.auto gets to decide.
		{names: []string{"-w", "--worktree"}, kind: negOpt},
		{names: []string{"--cwd"}, kind: valueOpt},
		{names: []string{"--fork"}, kind: valueOpt},
		{names: []string{"-n", "--dry-run"}, kind: flagOpt},
		{names: []string{"-h", "--help"}, kind: flagOpt},
	}, worktreeOpts...), true)
	if p.flags["-h"] {
		fmt.Print(usage())
		return 0
	}
	unknown := append(p.extra, p.pos...)
	if len(unknown) > 0 {
		die("unknown option(s): %s", strings.Join(unknown, " "))
	}
	a := runArgs{
		Description: p.value("-d"),
		Worktree:    p.negated("-w"),
		From:        p.values["--from"],
		Detach:      p.negated("--detach"),
		Cwd:         p.values["--cwd"],
		Fork:        p.values["--fork"],
		DryRun:      p.flags["-n"],
	}
	if a.Fork != "" && (a.Worktree != nil && *a.Worktree || a.From != "" || a.Detach != nil) {
		die("--fork runs in the session's own directory; -w has nothing to do there")
	}
	if a.Worktree != nil && !*a.Worktree && (a.From != "" || a.Detach != nil) {
		die("--from and --detach say how to make a worktree; --no-worktree makes none")
	}
	if len(harnessArgv) == 0 && a.Fork == "" {
		die("name the profile to start, e.g. %s run claude", prog)
	}
	return cmdRun(a, harnessArgv)
}

var commands = []string{
	"select", "ls", "status", "notify", "prune", "current", "name",
	"restart", "install", "instructions", "wt", "state", "env",
	"_preview", "_session-seen", "_mark-seen",
}

func dispatch(argv []string) int {
	aisArgs, harnessArgv, hasDashes := splitArgv(argv)

	// "ais run <profile> [flags] [-- args]" and "ais [flags] -- <profile>
	// [args]" are one command. The first spells out what the second
	// abbreviates, and has room for the profile to be left out where --fork
	// implies it.
	if len(aisArgs) > 0 && aisArgs[0] == "run" {
		rest := append([]string{}, aisArgs[1:]...)
		var profile []string
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			profile, rest = []string{rest[0]}, rest[1:]
		}
		return startSession(rest, append(profile, harnessArgv...))
	}
	if hasDashes {
		if len(harnessArgv) == 0 {
			die("nothing after '--': name the profile, e.g. ais -- claude")
		}
		return startSession(aisArgs, harnessArgv)
	}
	if len(aisArgs) == 0 || aisArgs[0] == "-h" || aisArgs[0] == "--help" || aisArgs[0] == "help" {
		fmt.Print(usage())
		return 0
	}
	cmd, rest := aisArgs[0], aisArgs[1:]
	if !contains(commands, cmd) {
		hint := ""
		if !strings.HasPrefix(cmd, "-") {
			hint = fmt.Sprintf("; to run a harness, separate it with '--': %s -- %s", prog, shellJoin(aisArgs))
		}
		die("unknown command %s%s", pyRepr(cmd), hint)
	}

	switch cmd {
	case "select":
		p := parse(rest, []option{{names: []string{"--client"}, kind: valueOpt}}, false)
		return cmdSelect(first(p.pos, 1), p.values["--client"])
	case "ls", "status":
		p := parse(rest, []option{
			{names: []string{"-a", "--all"}, kind: flagOpt},
			{names: []string{"--window"}, kind: valueOpt},
		}, false)
		noPositionals(p, 0)
		var window *float64
		if v := p.value("--window"); v != nil {
			f, err := strconv.ParseFloat(*v, 64)
			if err != nil {
				dieCode(2, "error: argument --window: invalid float value: %s", pyRepr(*v))
			}
			window = &f
		}
		return cmdLs(p.flags["-a"], cmd == "status", window)
	case "notify":
		p := parse(rest, []option{
			{names: []string{"-t", "--title"}, kind: valueOpt},
			{names: []string{"--tag"}, kind: valueOpt},
			{names: []string{"-f", "--force"}, kind: flagOpt},
			{names: []string{"--no-sound"}, kind: flagOpt},
			{names: []string{"-q", "--quiet"}, kind: flagOpt},
		}, false)
		if len(p.pos) == 0 {
			dieCode(2, "error: the following arguments are required: message")
		}
		return cmdNotify(notifyArgs{Message: p.pos, Title: p.value("-t"), Tag: p.value("--tag"),
			Force: p.flags["-f"], NoSound: p.flags["--no-sound"], Quiet: p.flags["-q"]})
	case "prune":
		p := parse(rest, []option{
			{names: []string{"--all"}, kind: flagOpt},
			{names: []string{"--days"}, kind: valueOpt},
			{names: []string{"--keep-worktrees"}, kind: flagOpt},
		}, false)
		noPositionals(p, 0)
		days := 7.0
		if v := p.value("--days"); v != nil {
			f, err := strconv.ParseFloat(*v, 64)
			if err != nil {
				dieCode(2, "error: argument --days: invalid float value: %s", pyRepr(*v))
			}
			days = f
		}
		return cmdPrune(p.flags["--all"], days, p.flags["--keep-worktrees"])
	case "wt":
		p := parse(rest, append([]option{{names: []string{"-a", "--all"}, kind: flagOpt}}, worktreeOpts...), false)
		noPositionals(p, 2)
		return cmdWt(wtArgs{Sub: orDefault(first(p.pos, 1), "ls"), Repo: first(p.pos, 2), All: p.flags["-a"],
			From: p.values["--from"], Detach: p.negated("--detach")})
	case "current":
		noPositionals(parse(rest, nil, false), 0)
		return cmdCurrent()
	case "name":
		p := parse(rest, []option{
			{names: []string{"--set"}, kind: valueOpt},
			{names: []string{"--of"}, kind: valueOpt},
		}, false)
		noPositionals(p, 0)
		return cmdName(p.value("--set"), p.value("--of"))
	case "restart":
		p := parse(rest, []option{
			{names: []string{"--fresh"}, kind: flagOpt},
			{names: []string{"-n", "--dry-run"}, kind: flagOpt},
		}, false)
		noPositionals(p, 1)
		return cmdRestart(first(p.pos, 1), p.flags["--fresh"], p.flags["-n"])
	case "install":
		p := parse(rest, []option{
			{names: []string{"--harness", "--profile"}, kind: valueOpt},
			{names: []string{"-n", "--dry-run"}, kind: flagOpt},
			{names: []string{"--force"}, kind: flagOpt},
		}, false)
		noPositionals(p, 0)
		return cmdInstall(orDefault(p.values["--harness"], "claude"), p.flags["-n"], p.flags["--force"])
	case "instructions":
		p := parse(rest, []option{{names: []string{"--markers"}, kind: flagOpt}}, false)
		noPositionals(p, 1)
		return cmdInstructions(orDefault(first(p.pos, 1), "claude"), p.flags["--markers"])
	case "state":
		p := parse(rest, []option{
			{names: []string{"--json"}, kind: flagOpt}, // the only format; accepted for clarity
			{names: []string{"--active"}, kind: flagOpt},
			{names: []string{"--since"}, kind: valueOpt},
		}, false)
		noPositionals(p, 0)
		return cmdState(p.flags["--active"], p.values["--since"])
	case "env":
		p := parse(rest, []option{{names: []string{"--json"}, kind: flagOpt}}, false)
		noPositionals(p, 1)
		if len(p.pos) == 0 {
			dieCode(2, "error: the following arguments are required: id")
		}
		return cmdEnv(p.pos[0], p.flags["--json"])
	case "_preview":
		p := parse(rest, nil, false)
		noPositionals(p, 1)
		if len(p.pos) == 0 {
			dieCode(2, "error: the following arguments are required: id")
		}
		return cmdPreview(p.pos[0])
	case "_session-seen":
		p := parse(rest, []option{{names: []string{"-v", "--verbose"}, kind: flagOpt}}, true)
		return cmdSessionSeen(first(p.pos, 1), p.flags["-v"])
	case "_mark-seen":
		p := parse(rest, []option{
			{names: []string{"--pane"}, kind: listOpt},
			{names: []string{"-v", "--verbose"}, kind: flagOpt},
		}, false)
		return cmdMarkSeen(p.lists["--pane"], p.flags["-v"])
	}
	die("unknown command %s; try '%s --help'", pyRepr(cmd), prog)
	return 1
}

func first(list []string, n int) string {
	if len(list) >= n {
		return list[n-1]
	}
	return ""
}

func noPositionals(p parsed, allowed int) {
	if len(p.pos) > allowed {
		dieCode(2, "error: unrecognized arguments: %s", strings.Join(p.pos[allowed:], " "))
	}
}
