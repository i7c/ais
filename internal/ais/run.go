package ais

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// runArgs is what a session start was told, whether it came as "run" or "--".
type runArgs struct {
	Description *string
	Worktree    *bool // -w, --no-worktree, or neither
	From        string
	Detach      *bool
	Cwd         string
	Fork        string
	DryRun      bool
	Prompt      *string  // --prompt TEXT
	PromptFile  string   // --prompt-file PATH, "-" for stdin
	Repos       []string // --repos: a worktree of each, side by side
}

// wtRequest is acted on by launch once the session has an id and a name to
// build the path from; never stored on the record.
type wtRequest struct {
	Root, Repo, From string
	Detach           *bool
	// Several repos (--repos): one worktree each under the session directory,
	// which becomes the session's cwd. Root and Repo are unused then.
	Many []repoRef
}

type repoRef struct{ Root, Repo string }

// firstPrompt is the text --prompt or --prompt-file gave, or "".
func firstPrompt(a runArgs) string {
	if a.Prompt != nil && a.PromptFile != "" {
		die("--prompt and --prompt-file both give the first prompt; pick one")
	}
	if a.Prompt != nil {
		return *a.Prompt
	}
	if a.PromptFile == "" {
		return ""
	}
	var data []byte
	var err error
	if a.PromptFile == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(expandUser(a.PromptFile))
	}
	if err != nil {
		die("cannot read the prompt: %v", err)
	}
	return strings.TrimRight(string(data), "\n")
}

// reposRequest resolves --repos to main checkouts, refusing what is not a
// repo and two repos that would want the same directory.
func reposRequest(paths []string, a runArgs) *wtRequest {
	if a.Worktree != nil {
		die("--repos makes the worktrees itself; leave out -w and --no-worktree")
	}
	want := &wtRequest{From: a.From, Detach: a.Detach}
	seen := map[string]string{}
	for _, p := range paths {
		root := repoRoot(expandUser(p))
		if root == "" {
			die("--repos: %s is not a git repository", tilde(p))
		}
		name := repoName(root)
		if other, dup := seen[name]; dup {
			if other == root {
				continue // the same repo named twice
			}
			die("--repos: %s and %s would both be %s/", tilde(other), tilde(root), name)
		}
		seen[name] = root
		want.Many = append(want.Many, repoRef{Root: root, Repo: name})
	}
	return want
}

func cmdRun(a runArgs, harnessArgv []string) int {
	cfg := loadConfig()
	pane := requireTmux()
	if a.Fork != "" {
		return runFork(a, cfg, pane, harnessArgv)
	}

	harness := harnessArgv[0]
	st := harnessSettings(cfg, harness)
	harnessArgs := harnessArgv[1:]
	cwd := runDirectory(a)
	var root string
	var many *wtRequest
	if len(a.Repos) > 0 {
		many = reposRequest(a.Repos, a)
	} else {
		root = worktreeRoot(a.Worktree, st.Worktree.Auto, cwd)
	}
	prompt := firstPrompt(a)
	var promptArgs []string
	if prompt != "" {
		if len(st.Prompt) == 0 {
			die(`profile %s takes no first prompt; give it one with prompt = ["--", "{prompt}"] under [profile.%s]`, harness, harness)
		}
		for _, part := range st.Prompt {
			promptArgs = append(promptArgs, strings.ReplaceAll(part, "{prompt}", prompt))
		}
	}

	// Mint the conversation id here rather than discovering it later: known
	// before the child exists, it lands in the record in the same write as
	// everything else, and there is nothing to scrape afterwards.
	sid, sessionFlags := "", []string(nil)
	if conf := st.Session; conf != nil && len(conf.New) > 0 {
		if typed := namesASession(harnessArgs, conf.Conflicts); typed != "" {
			note("you passed %s; leaving the conversation id to %s", typed, harness)
		} else {
			sid = mintSessionID(conf.ID)
			sessionFlags = sessionArgv(conf.New, sid)
		}
	}
	// The prompt goes last and stays out of harness_args: a restart resumes
	// the conversation, it does not ask the first question again.
	argv := concat(st.Command, st.Flags, sessionFlags, harnessArgs, promptArgs)

	location := captureLocation(pane)
	if location == nil {
		die("could not read the tmux location of pane %s", pane)
	}
	args := append([]string{}, harnessArgs...)
	r := &Record{
		Description:   a.Description,
		Profile:       harness,
		Harness:       harness,
		Command:       argv,
		HarnessArgs:   &args,
		SessionIDs:    nonNil(sid),
		Status:        "active",
		Hostname:      hostname(),
		Cwd:           cwd,
		Tmux:          location,
		StartedAt:     nowISO(),
		InitialPrompt: prompt,
		// Set by "ais flow start", and kept out of the harness's env (see
		// sessionEnv), so a session the agent starts itself is not taken for
		// the proposal's.
		FlowProposal: os.Getenv("AIS_FLOW_PROPOSAL"),
	}
	want := many
	if root != "" {
		want = &wtRequest{Root: root, Repo: repoName(root), From: a.From, Detach: a.Detach}
	}
	return launch(r, argv, st, want, a.DryRun)
}

func nonNil(id string) []string {
	if id == "" {
		return []string{}
	}
	return []string{id}
}

// runDirectory is where a new session starts: --cwd if given, else here.
func runDirectory(a runArgs) string {
	if a.Cwd == "" {
		wd, err := os.Getwd()
		must(err)
		return wd
	}
	path, err := filepath.Abs(expandUser(a.Cwd))
	must(err)
	if !isDir(path) {
		die("--cwd %s is not a directory", tilde(path))
	}
	return path
}

// runFork starts a copy of a session: same profile, place and work, a new
// conversation. The copy runs beside the source rather than instead of it,
// so unlike restart the source may well be alive.
func runFork(a runArgs, cfg config, pane string, extra []string) int {
	source := pickSession(readSessions(), a.Fork)
	host := hostname()
	if source.Hostname != host {
		die("session %s ran on %s, not %s; its conversation lives on that machine", source.ID, orQ(source.Hostname), host)
	}
	profile := source.profile()
	if len(extra) > 0 && extra[0] != profile {
		die("session %s is a %s session, not %s", source.ID, profile, extra[0])
	}
	st := harnessSettings(cfg, profile)
	conf := st.Session
	conversation := source.conversation()
	if conf == nil || len(conf.Fork) == 0 {
		die("no session.fork configured for %s; nothing to fork with", profile)
	}
	if conversation == "" {
		die("session %s has no known %s conversation to fork", source.ID, profile)
	}
	cwd := source.Cwd
	if a.Cwd != "" {
		cwd = runDirectory(a)
	}
	if !isDir(cwd) {
		die("%s is gone; nothing to fork into", orDefault(tilde(cwd), "its directory"))
	}
	var harnessArgs []string
	if source.HarnessArgs != nil {
		harnessArgs = append(harnessArgs, *source.HarnessArgs...)
	}
	if len(extra) > 1 {
		harnessArgs = append(harnessArgs, extra[1:]...)
	}
	argv := concat(st.Command, st.Flags, sessionArgv(conf.Fork, conversation), harnessArgs)
	location := captureLocation(pane)
	if location == nil {
		die("could not read the tmux location of pane %s", pane)
	}
	desc := a.Description
	if desc == nil || *desc == "" {
		desc = source.Description
	}
	if harnessArgs == nil {
		harnessArgs = []string{}
	}
	r := &Record{
		Description: desc,
		Slug:        source.Slug,     // same work, so the same name...
		Worktree:    source.Worktree, // ...and the same worktree
		Profile:     profile,
		Harness:     profile,
		Command:     argv,
		HarnessArgs: &harnessArgs,
		SessionIDs:  []string{}, // the harness names the branch; its hook reports it
		ForkOf:      source.ID,
		Status:      "active",
		Hostname:    host,
		Cwd:         cwd,
		Tmux:        location,
		StartedAt:   nowISO(),
	}
	note("forking %s (%s) from %s", source.ID, elide(describe(source), 48), conversation)
	return launch(r, argv, st, nil, a.DryRun)
}

func orQ(s string) string { return orDefault(s, "?") }

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// inheritedEnv is this process's env without another session's identity in it.
func inheritedEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !contains(sessionEnv, k) {
			out[k] = v
		}
	}
	return out
}

var sessionEnv = []string{
	"AIS_SESSION", "AIS_SIBLING", "AIS_PROFILE", "AIS_HARNESS",
	"AIS_SESSION_DESCRIPTION", "AIS_WORKTREE", "AIS_WORKTREE_BASE",
	"AIS_REPO_MAIN", "AIS_FLOW_PROPOSAL",
}

// pickSession is the one session whose id starts with wanted.
func pickSession(ss []*Record, wanted string) *Record {
	var matches []*Record
	for _, s := range ss {
		if strings.HasPrefix(s.ID, wanted) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		die("no session with id starting %s", pyRepr(wanted))
	}
	if len(matches) > 1 {
		die("%s matches %d sessions", pyRepr(wanted), len(matches))
	}
	return matches[0]
}

func pyRepr(s string) string { return "'" + strings.ReplaceAll(s, "'", `\'`) + "'" }

// launch registers the session, runs argv to completion and records how it
// ended. Shared by run, fork and restart: everything that differs between
// them has been decided by the time it gets here.
func launch(r *Record, argv []string, st settings, want *wtRequest, dryRun bool) int {
	exe, err := exec.LookPath(argv[0])
	if err != nil {
		die("harness not found on PATH: %s", argv[0])
	}
	harness := r.harness()
	// Until the harness exists and has a pid of its own, ais is the process
	// that says whether the session is alive: without it, a reader reaping in
	// that moment (tsm polls state constantly) would bury a session that is
	// only starting.
	r.LauncherPID = os.Getpid()

	if dryRun {
		r.ID = newID(readSessions())
	} else {
		mutate(func(ss []*Record) []*Record {
			r.ID = newID(ss)
			return append(ss, r)
		})
		// Settle the name now rather than leaving it to the first "ais name":
		// the window is about to be labelled with it, and storeSlug is what
		// keeps two sessions that share a description apart.
		if candidate := slugify(r.desc()); candidate != "" && r.Slug == "" {
			r.Slug = orDefault(storeSlug(r.ID, candidate, false), candidate)
		}
		if want != nil {
			makeSessionWorktree(r, want, st.Worktree)
		}
	}

	env := inheritedEnv()
	for k, v := range st.Env {
		env[k] = v
	}
	env["AIS_SESSION"] = r.ID
	env["AIS_PROFILE"] = harness
	env["AIS_HARNESS"] = harness
	if r.desc() != "" {
		env["AIS_SESSION_DESCRIPTION"] = r.desc()
	}
	for k, v := range worktreeEnv(r) {
		env[k] = v
	}

	if dryRun {
		fmt.Println("session:", r.ID, "(dry run, not recorded)")
		fmt.Println("command:", shellJoin(argv))
		for _, k := range sortedKeys(st.Env) {
			fmt.Printf("env:     %s=%s\n", k, st.Env[k])
		}
		fmt.Printf("env:     AIS_SESSION=%s\n", r.ID)
		fmt.Printf("env:     AIS_PROFILE=%s\n", harness)
		fmt.Printf("env:     AIS_HARNESS=%s\n", harness)
		if r.desc() != "" {
			fmt.Printf("env:     AIS_SESSION_DESCRIPTION=%s\n", r.desc())
		}
		if len(r.SessionIDs) > 0 {
			fmt.Printf("%s: %s\n", harness, r.SessionIDs[len(r.SessionIDs)-1])
		}
		if want != nil {
			name := orDefault(slugify(r.desc()), r.ID)
			wt := st.Worktree.with(want.From, want.Detach)
			how := "branch " + name
			if wt.Detach {
				how = "detached"
			}
			repos := want.Many
			if len(repos) == 0 {
				repos = []repoRef{{want.Root, want.Repo}}
			}
			for _, rr := range repos {
				planned := filepath.Join(st.Worktree.Base, name, rr.Repo)
				fmt.Println("worktree:", tilde(planned), fmt.Sprintf("(%s from %s, not created)", how, wt.From))
			}
		}
		fmt.Println("cwd:    ", tilde(r.Cwd))
		fmt.Println("tmux:   ", tmuxLabel(r))
		if st.RenameWindow {
			fmt.Println("window: ", windowName(r))
		}
		return 0
	}

	pane := r.Tmux["pane_id"]
	if pane != "" {
		if st.RenameWindow {
			defer nameWindow(pane, windowName(r))()
		}
		defer markPane(pane, r)()
		installSeenHooks()
	}
	// ais stands in the session's directory itself, not just its child: tmux
	// reports the directory of the pane's foreground process group, which ais
	// leads, so a split next to the agent would otherwise open wherever ais
	// was started rather than in the agent's worktree.
	if err := os.Chdir(r.Cwd); err != nil {
		finish(r.ID, 126)
		die("cannot enter %s: %v", tilde(r.Cwd), err)
	}

	child := &exec.Cmd{Path: exe, Args: argv, Dir: r.Cwd, Env: envList(env),
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	// Handled, not ignored: exec resets handled signals to their default in
	// the child, whereas an ignored one would be inherited, and Ctrl-C would
	// stop working inside the harness. SIGINT and SIGWINCH reach the child
	// directly (same foreground process group); TERM and HUP are forwarded,
	// and ais waits so it can still write the session's exit status.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := child.Start(); err != nil {
		finish(r.ID, 126)
		die("could not start %s: %v", argv[0], err)
	}
	pid := child.Process.Pid
	update(r.ID, func(s *Record) { s.PID = &pid })

	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigs:
				if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
					child.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	err = child.Wait()
	close(done)
	code := exitCode(child, err)
	finish(r.ID, code)
	return code
}

// exitCode is the child's exit status, 128+n for death by signal n.
func exitCode(child *exec.Cmd, err error) int {
	if child.ProcessState == nil {
		return 1
	}
	if ws, ok := child.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return child.ProcessState.ExitCode()
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range sortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// makeSessionWorktree puts a "-w" session in its own worktree, and moves it
// there. Runs after the session is on record, because the path is built out
// of the name; a failure here leaves a registered session with no harness
// behind it, so it is closed off before dying.
func makeSessionWorktree(r *Record, want *wtRequest, wt wtSettings) {
	if len(want.Many) > 0 {
		makeSessionWorktrees(r, want, wt)
		return
	}
	name := sessionDirname(r)
	base := filepath.Join(wt.Base, name)
	dest := filepath.Join(base, want.Repo)
	func() {
		defer func() {
			if e := recover(); e != nil {
				finish(r.ID, 1)
				panic(e)
			}
		}()
		ensureWorktree(want.Root, dest, name, wt.with(want.From, want.Detach))
	}()
	r.Cwd = dest
	r.Worktree = &WorktreeRec{Base: base, Initial: dest, Branch: name, Root: want.Root}
	recordWorktree(r.ID, base, dest, name, want.Root)
	note("working in %s", tilde(dest))
}

// makeSessionWorktrees is --repos: a worktree of each repo, side by side
// under the session directory, which is where the session then starts. It
// is not itself a repo; Claude Code trusts it when worktree.base is trusted.
func makeSessionWorktrees(r *Record, want *wtRequest, wt wtSettings) {
	name := sessionDirname(r)
	base := filepath.Join(wt.Base, name)
	settings := wt.with(want.From, want.Detach)
	func() {
		defer func() {
			if e := recover(); e != nil {
				finish(r.ID, 1)
				panic(e)
			}
		}()
		for _, rr := range want.Many {
			ensureWorktree(rr.Root, filepath.Join(base, rr.Repo), name, settings)
		}
	}()
	r.Cwd = base
	r.Worktree = &WorktreeRec{Base: base, Branch: name}
	update(r.ID, func(s *Record) {
		s.Cwd = base
		s.Worktree = &WorktreeRec{Base: base, Branch: name}
	})
	note("working in %s (%d repos)", tilde(base), len(want.Many))
}

func finish(id string, code int) {
	update(id, func(r *Record) {
		r.Status = "dead"
		r.EndedAt = strPtr(nowISO())
		r.ExitCode = intPtr(code)
	})
}

func sessionArgv(template []string, sid string) []string {
	out := make([]string, len(template))
	for i, part := range template {
		out[i] = strings.ReplaceAll(part, "{id}", sid)
	}
	return out
}

func mintSessionID(kind string) string {
	if kind == "short" {
		return randomString(idLength)
	}
	return uuid4()
}

// namesASession is the first session-naming flag typed by hand, if any.
// Matches "--resume x" and "--resume=x" alike; anything after a bare "--" is
// the harness's own business and is left alone.
func namesASession(argv, conflicts []string) string {
	for _, arg := range argv {
		if arg == "--" {
			break
		}
		head, _, _ := strings.Cut(arg, "=")
		if contains(conflicts, head) {
			return head
		}
	}
	return ""
}

// stripKnownFlags drops session-naming flags and their values from a
// recorded argv.
func stripKnownFlags(argv, flags []string) []string {
	out := []string{}
	skip := false
	for _, arg := range argv {
		if skip {
			skip = false
			continue
		}
		head, _, hasValue := strings.Cut(arg, "=")
		if contains(flags, head) {
			skip = !hasValue
			continue
		}
		out = append(out, arg)
	}
	return out
}

// ---------------------------------------------------------------- restart

// pickRestartable is the session restart should revive. With no id: the last
// thing that died here, nearly always the session whose pane you stand in.
func pickRestartable(ss []*Record, wanted string) *Record {
	if wanted != "" {
		return pickSession(ss, wanted)
	}
	here, _ := os.Getwd()
	host := hostname()
	var mine []*Record
	for _, s := range ss {
		if s.Status == "dead" && s.Cwd == here && s.Hostname == host {
			mine = append(mine, s)
		}
	}
	if len(mine) == 0 {
		die("no dead session recorded in %s; name one: %s restart <id>", tilde(here), prog)
	}
	key := func(s *Record) string {
		if s.EndedAt != nil && *s.EndedAt != "" {
			return *s.EndedAt
		}
		return s.StartedAt
	}
	sort.SliceStable(mine, func(i, j int) bool { return key(mine[i]) < key(mine[j]) })
	return mine[len(mine)-1]
}

// cmdRestart starts a session's harness again, in this pane, where it left
// off. A restart is its own session — new id, new pane, its own
// notifications — linked back to the one it continues. What it inherits is
// what has to stay put: the directory, the name (so the worktree still
// matches) and the harness conversation.
func cmdRestart(id string, fresh, dryRun bool) int {
	cfg := loadConfig()
	pane := requireTmux()
	ss := reapedSessions()
	target := pickRestartable(ss, id)
	host := hostname()
	if target.Hostname != host {
		die("session %s ran on %s, not %s; its conversation lives on that machine", target.ID, orQ(target.Hostname), host)
	}
	if target.Status == "active" && processAlive(target.pid()) {
		die("session %s is still running — %s select %s", target.ID, prog, target.ID)
	}
	if !isDir(target.Cwd) {
		die("%s is gone; nothing to restart into", orDefault(tilde(target.Cwd), "its directory"))
	}
	harness := target.profile()
	st := harnessSettings(cfg, harness)
	conf := st.Session
	var conflicts []string
	if conf != nil {
		conflicts = conf.Conflicts
	}
	var harnessArgs []string
	if target.HarnessArgs != nil {
		harnessArgs = append([]string{}, *target.HarnessArgs...)
	} else {
		// Recorded before ais kept the raw args: recover them from the argv
		// it did record, minus the parts ais put there itself.
		skip := len(st.Command) + len(st.Flags)
		rest := []string{}
		if len(target.Command) > skip {
			rest = target.Command[skip:]
		}
		harnessArgs = stripKnownFlags(rest, conflicts)
	}

	conversation := target.conversation()
	typed := ""
	if conf != nil {
		typed = namesASession(harnessArgs, conf.Conflicts)
	}
	var sessionFlags, ids []string
	switch {
	case typed != "":
		note("session %s was started with %s; leaving it to %s", target.ID, typed, harness)
	case !fresh && conversation != "" && conf != nil && len(conf.Resume) > 0:
		sessionFlags = sessionArgv(conf.Resume, conversation)
		ids = []string{conversation} // same conversation continues; the harness keeps the id
	case !fresh && conversation != "":
		note("no session.resume configured for %s; starting a fresh conversation", harness)
	}
	if len(sessionFlags) == 0 && conf != nil && len(conf.New) > 0 {
		id := mintSessionID(conf.ID)
		sessionFlags = sessionArgv(conf.New, id)
		ids = []string{id}
	}
	if ids == nil {
		ids = []string{}
	}
	argv := concat(st.Command, st.Flags, sessionFlags, harnessArgs)

	location := captureLocation(pane)
	if location == nil {
		die("could not read the tmux location of pane %s", pane)
	}
	r := &Record{
		Description: target.Description,
		Slug:        target.Slug,     // deliberately shared: same work, same worktree
		Worktree:    target.Worktree, // and literally the same one: cwd is in it
		Profile:     harness,
		Harness:     harness,
		Command:     argv,
		HarnessArgs: &harnessArgs,
		SessionIDs:  ids,
		RestartOf:   target.ID,
		// A restart goes on with the same piece of work, flow proposal
		// included; a fork is a second agent, and does not.
		FlowProposal: target.FlowProposal,
		Status:       "active",
		Hostname:     host,
		Cwd:          target.Cwd,
		Tmux:         location,
		StartedAt:    nowISO(),
	}
	what := "fresh conversation"
	if len(ids) == 1 && ids[0] == conversation {
		what = "resuming " + conversation
	}
	note("restarting %s (%s) — %s", target.ID, elide(describe(target), 48), what)
	return launch(r, argv, st, nil, dryRun)
}

func describe(r *Record) string {
	if d := strings.TrimSpace(r.desc()); d != "" {
		return d
	}
	return fmt.Sprintf("(%s in %s)", orDefault(r.harness(), "session"), tilde(orQ(r.Cwd)))
}
