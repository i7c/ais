// Package acceptance drives the ais command line as a black box.
//
// Every test gets a lab of its own: a HOME, an AIS_HOME, a worktree base and a
// tmux server on a private socket (tmux -S, never TMUX_TMPDIR, so the real
// server is out of reach). Agents are played by testdata/fake, a shell script
// that records how it was started and exits when told to.
//
// The suite is the contract a reimplementation of ais must meet, so it asserts
// what callers depend on — exit codes, stdout, the JSON API, files, tmux
// state — and avoids pinning the wording of messages meant for people.
//
//	go test ./acceptance/                 # against ../ais
//	AIS_BIN=/path/to/ais go test ./acceptance/
package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Lab is one isolated ais world.
type Lab struct {
	t       *testing.T
	Dir     string // everything lives under here
	Home    string
	AisHome string
	WtBase  string
	Bin     string // fake, sink, notify-send, and ais itself
	Log     string // where fake and the stand-ins write what happened
	Sock    string
	pid     string // the tmux server's
	env     []string
	ais     string
	windows int
}

// aisUnderTest is the ais to test: $AIS_BIN, else the one in this checkout.
func aisUnderTest(t *testing.T) string {
	if bin := os.Getenv("AIS_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "ais")
}

func testdata(name string) string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "testdata", name)
}

// NewLab builds the world and starts its tmux server; Cleanup tears it down.
func NewLab(t *testing.T) *Lab {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	// A socket path must stay under ~100 bytes, which t.TempDir need not.
	sockDir, err := os.MkdirTemp("", "aisacc-")
	if err != nil {
		t.Fatal(err)
	}
	l := &Lab{
		t:       t,
		Dir:     dir,
		Home:    filepath.Join(dir, "home"),
		WtBase:  filepath.Join(dir, "wt"),
		Bin:     filepath.Join(dir, "bin"),
		Log:     filepath.Join(dir, "log"),
		Sock:    filepath.Join(sockDir, "s"),
		ais:     aisUnderTest(t),
	}
	l.AisHome = filepath.Join(l.Home, ".ais")
	for _, d := range []string{l.Home, l.AisHome, l.Bin, l.Log} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for _, name := range []string{"fake", "sink", "notify-send"} {
		must(t, os.Symlink(testdata(name), filepath.Join(l.Bin, name)))
	}
	// "ais" on PATH is the one under test, ahead of any installed one: hooks
	// and the fake harness call it by name.
	must(t, os.Symlink(l.ais, filepath.Join(l.Bin, "ais")))
	l.WriteFile(filepath.Join(l.Home, ".gitconfig"), `[user]
	name = Lab
	email = lab@example.invalid
[init]
	defaultBranch = main
[advice]
	detachedHead = false
`)

	// Built from nothing, so no TMUX, AIS_* or CLAUDE_* leaks in from the
	// session running the tests.
	l.env = []string{
		"PATH=" + l.Bin + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + l.Home,
		"AIS_HOME=" + l.AisHome,
		"FAKE_LOG=" + l.Log,
		"XDG_CONFIG_HOME=" + filepath.Join(l.Home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(l.Home, ".cache"),
		"GIT_CONFIG_NOSYSTEM=1",
		"LANG=C.UTF-8",
		"LC_CTYPE=C.UTF-8",
		"TERM=xterm-256color",
		"SHELL=/bin/sh",
		"USER=" + os.Getenv("USER"),
	}
	l.WriteConfig("")

	l.tmuxMust("-f", "/dev/null", "new-session", "-d", "-s", "lab", "-x", "200", "-y", "50", "-c", l.Home)
	l.pid = strings.TrimSpace(l.tmuxMust("display-message", "-p", "#{pid}"))
	t.Cleanup(func() {
		exec.Command("tmux", "-S", l.Sock, "kill-server").Run()
		os.RemoveAll(sockDir)
	})
	return l
}

// BaseConfig is the config.toml every lab starts with: two profiles played
// by fake — one that names conversations as claude does, one that does not —
// worktrees under the lab, and notifications through the stand-in sink.
func (l *Lab) BaseConfig() string {
	fake := filepath.Join(l.Bin, "fake")
	return fmt.Sprintf(`[profile.fake]
command = [%q]
flags = ["--from-config"]

[profile.fake.env]
FAKE_PROFILE_ENV = "from-profile"

[profile.fake.session]
id = "uuid4"
new = ["--session-id", "{id}"]
resume = ["--resume", "{id}"]
fork = ["--resume", "{id}", "--fork-session"]
conflicts = ["-c", "--continue", "--resume", "--fork-session"]

[profile.plain]
command = [%q]

[worktree]
base = %q

[tmux]
rename = true

[notify]
sink = [%q]
seen_after = 0
`, fake, fake, l.WtBase, filepath.Join(l.Bin, "sink"))
}

// WriteConfig replaces config.toml with the base config plus extra.
func (l *Lab) WriteConfig(extra string) {
	l.WriteFile(filepath.Join(l.AisHome, "config.toml"), l.BaseConfig()+"\n"+extra)
}

// SetConfig replaces config.toml outright.
func (l *Lab) SetConfig(text string) {
	l.WriteFile(filepath.Join(l.AisHome, "config.toml"), text)
}

func (l *Lab) WriteFile(path, text string) {
	l.t.Helper()
	must(l.t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(l.t, os.WriteFile(path, []byte(text), 0o644))
}

func (l *Lab) ReadFile(path string) string {
	l.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		l.t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------- tmux

func (l *Lab) tmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-S", l.Sock}, args...)...)
	cmd.Env = l.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func (l *Lab) tmuxMust(args ...string) string {
	l.t.Helper()
	out, err := l.tmux(args...)
	if err != nil {
		l.t.Fatal(err)
	}
	return out
}

// Format asks tmux for one format string about a target pane or window.
func (l *Lab) Format(target, format string) string {
	l.t.Helper()
	return strings.TrimSpace(l.tmuxMust("display-message", "-p", "-t", target, format))
}

// Window opens a window running shell (via /bin/sh -c) in dir and returns
// its pane id. The shell's exit code lands in <Log>/pane-<n>.exit, and the
// pane then stays open, as an interactive shell would after ais returns.
func (l *Lab) Window(dir, shell string) (pane string, exitFile string) {
	l.t.Helper()
	l.windows++
	exitFile = filepath.Join(l.Log, fmt.Sprintf("pane-%d.exit", l.windows))
	script := fmt.Sprintf("%s; echo $? > %s; exec sleep 86400", shell, exitFile)
	pane = strings.TrimSpace(l.tmuxMust(
		"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "lab:", "-c", dir, script,
	))
	return pane, exitFile
}

// Attach puts a real client on the lab server, through a pseudo-terminal, so
// "on screen" means something. The client shows lab's current window.
func (l *Lab) Attach() {
	l.t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		l.t.Skip("script(1) not installed; cannot attach a client")
	}
	cmd := exec.Command("script", "-qfec", "tmux -S "+l.Sock+" attach -t lab", "/dev/null")
	cmd.Env = append(l.env, "COLUMNS=200", "LINES=50")
	must(l.t, cmd.Start())
	l.t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	l.Eventually("a client attaches", func() bool {
		out, _ := l.tmux("list-clients", "-F", "#{client_name}")
		return strings.TrimSpace(out) != ""
	})
}

// Show makes pane the one lab's client is looking at.
func (l *Lab) Show(pane string) {
	l.t.Helper()
	l.tmuxMust("select-window", "-t", pane)
	l.tmuxMust("select-pane", "-t", pane)
}

// ---------------------------------------------------------------- running ais

// Result is one finished ais invocation.
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

func (r Result) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
}

// Ais runs ais from outside any pane, the way tsm or a timer would, but
// pointed at the lab's tmux server. extraEnv entries are KEY=VALUE.
func (l *Lab) Ais(extraEnv []string, args ...string) Result {
	return l.AisIn(l.Home, extraEnv, "", args...)
}

// AisIn is Ais in dir, with stdin.
func (l *Lab) AisIn(dir string, extraEnv []string, stdin string, args ...string) Result {
	l.t.Helper()
	cmd := exec.Command(l.ais, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, l.env...), "TMUX="+l.Sock+","+l.pid+",0")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			l.t.Fatalf("ais %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return Result{Code: code, Stdout: out.String(), Stderr: errb.String()}
}

// MustAis is Ais that fails the test on a non-zero exit.
func (l *Lab) MustAis(extraEnv []string, args ...string) Result {
	l.t.Helper()
	r := l.Ais(extraEnv, args...)
	if r.Code != 0 {
		l.t.Fatalf("ais %s failed:\n%s", strings.Join(args, " "), r)
	}
	return r
}

// AisCommand is how to type ais into a lab pane.
func (l *Lab) AisCommand(args ...string) string {
	return shellJoin(append([]string{"ais"}, args...))
}

// ---------------------------------------------------------------- sessions

// Started is a session running in a lab pane.
type Started struct {
	ID       string
	Pane     string
	ExitFile string // the pane shell's record of how ais itself exited
}

// Start types an ais command into a new window in dir and waits until the
// session it starts is recorded and its fake harness is up.
func (l *Lab) Start(dir string, args ...string) Started {
	l.t.Helper()
	pane, exitFile := l.Window(dir, l.AisCommand(args...))
	var id string
	l.Eventually("session in pane "+pane+" is up", func() bool {
		for _, s := range l.State().Sessions {
			if s.Tmux.PaneID == pane && s.Status == "active" {
				id = s.ID
			}
		}
		return id != "" && fileExists(filepath.Join(l.Log, id, "ready"))
	})
	return Started{ID: id, Pane: pane, ExitFile: exitFile}
}

// Stop tells a session's fake harness to exit with code and waits until ais
// has recorded the end and returned to the pane's shell.
func (l *Lab) Stop(s Started, code int) {
	l.t.Helper()
	l.WriteFile(filepath.Join(l.Log, s.ID, "exit"), fmt.Sprint(code))
	l.Eventually("ais in "+s.Pane+" returns", func() bool { return fileExists(s.ExitFile) })
}

// AisExit is what the ais process in a stopped session's pane exited with.
func (l *Lab) AisExit(s Started) string {
	return strings.TrimSpace(l.ReadFile(s.ExitFile))
}

// Fake reads what the fake harness of a session recorded about its start.
type Fake struct {
	Argv []string
	Cwd  string
	Env  map[string]string
}

func (l *Lab) Fake(id string) Fake {
	l.t.Helper()
	dir := filepath.Join(l.Log, id)
	f := Fake{Env: map[string]string{}}
	f.Argv = lines(l.ReadFile(filepath.Join(dir, "argv")))
	f.Cwd = strings.TrimSpace(l.ReadFile(filepath.Join(dir, "cwd")))
	for _, line := range lines(l.ReadFile(filepath.Join(dir, "env"))) {
		k, v, _ := strings.Cut(line, "=")
		f.Env[k] = v
	}
	return f
}

// ---------------------------------------------------------------- the API

// State mirrors "ais state --json", api 1. Only what the tests read.
type State struct {
	API      int       `json:"api"`
	Host     string    `json:"host"`
	Worktree WtConfig  `json:"worktree"`
	Window   float64   `json:"notify_window"`
	SeenAft  float64   `json:"notify_seen_after"`
	Profiles []Profile `json:"profiles"`
	Sessions []Session `json:"sessions"`
}

type WtConfig struct {
	Base   string `json:"base"`
	Auto   bool   `json:"auto"`
	From   string `json:"from"`
	Detach bool   `json:"detach"`
}

type Profile struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	Flags     []string `json:"flags"`
	Resumable bool     `json:"resumable"`
	Forkable  bool     `json:"forkable"`
}

type Session struct {
	ID           string            `json:"id"`
	Profile      string            `json:"profile"`
	Description  string            `json:"description"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Host         string            `json:"host"`
	Cwd          string            `json:"cwd"`
	Worktree     map[string]string `json:"worktree"`
	Tmux         TmuxLoc           `json:"tmux"`
	PID          *int              `json:"pid"`
	StartedAt    *string           `json:"started_at"`
	EndedAt      *string           `json:"ended_at"`
	ExitCode     *int              `json:"exit_code"`
	Conversation *string           `json:"conversation"`
	Args         []string          `json:"args"`
	RestartOf    *string           `json:"restart_of"`
	ForkOf       *string           `json:"fork_of"`
	CanRestart   bool              `json:"can_restart"`
	CanResume    bool              `json:"can_resume"`
	CanFork      bool              `json:"can_fork"`
	Notes        []Notification    `json:"notifications"`
}

type TmuxLoc struct {
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id"`
	WindowID    string `json:"window_id"`
	WindowIndex string `json:"window_index"`
	PaneID      string `json:"pane_id"`
	PaneIndex   string `json:"pane_index"`
}

type Notification struct {
	ID         string `json:"id"`
	At         string `json:"at"`
	Message    string `json:"message"`
	Delivered  bool   `json:"delivered"`
	Suppressed bool   `json:"suppressed"`
	Seen       bool   `json:"seen"`
	SeenAt     string `json:"seen_at"`
}

func (l *Lab) State(args ...string) State {
	l.t.Helper()
	r := l.MustAis(nil, append([]string{"state", "--json"}, args...)...)
	var st State
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		l.t.Fatalf("state is not JSON: %v\n%s", err, r.Stdout)
	}
	return st
}

// Session is one session from state, failing the test if it is missing.
func (l *Lab) Session(id string) Session {
	l.t.Helper()
	for _, s := range l.State().Sessions {
		if s.ID == id {
			return s
		}
	}
	l.t.Fatalf("session %s not in state", id)
	return Session{}
}

// SessionsFile is sessions.json, decoded loosely.
func (l *Lab) SessionsFile() map[string]any {
	l.t.Helper()
	var v map[string]any
	must(l.t, json.Unmarshal([]byte(l.ReadFile(filepath.Join(l.AisHome, "sessions.json"))), &v))
	return v
}

// ---------------------------------------------------------------- git

// Repo makes a repository with one commit, cloned from a bare "origin" so it
// has a remote default branch, and returns the clone's path.
func (l *Lab) Repo(name string) string {
	l.t.Helper()
	bare := filepath.Join(l.Dir, "remotes", name+".git")
	clone := filepath.Join(l.Dir, "src", name)
	seed := filepath.Join(l.Dir, "seed", name)
	l.Git(l.Dir, "init", "-q", "--bare", bare)
	l.Git(l.Dir, "init", "-q", seed)
	l.WriteFile(filepath.Join(seed, "README"), name+"\n")
	l.Git(seed, "add", "README")
	l.Git(seed, "commit", "-q", "-m", "first")
	l.Git(seed, "push", "-q", bare, "main")
	l.Git(l.Dir, "clone", "-q", bare, clone)
	return clone
}

// Commit adds a commit to repo touching file.
func (l *Lab) Commit(repo, file, msg string) {
	l.t.Helper()
	l.WriteFile(filepath.Join(repo, file), msg+"\n")
	l.Git(repo, "add", file)
	l.Git(repo, "commit", "-q", "-m", msg)
}

func (l *Lab) Git(dir string, args ...string) string {
	l.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = l.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		l.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------- helpers

// Eventually polls cond for up to 10 s.
func (l *Lab) Eventually(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	l.t.Fatalf("timed out waiting for: %s", what)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lines(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

func indexOf(list []string, item string) int {
	for i, x := range list {
		if x == item {
			return i
		}
	}
	return -1
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// HerePane is the first pane of the lab, for commands that must believe they
// run inside tmux without starting anything there.
func (l *Lab) HerePane() string {
	l.t.Helper()
	return l.Format("lab:0", "#{pane_id}")
}

func parentOf(t *testing.T, pid string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid ...; comm may hold spaces, so cut after ')'.
	rest := string(b[bytes.LastIndexByte(b, ')')+2:])
	return strings.Fields(rest)[1]
}

func killHard(t *testing.T, pid string) {
	t.Helper()
	if out, err := exec.Command("kill", "-KILL", pid).CombinedOutput(); err != nil {
		t.Fatalf("kill %s: %v %s", pid, err, out)
	}
}
