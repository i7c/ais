package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The dispatcher is a systemd one-shot service, and systemd kills whatever is
// left in a service's control group when it ends. A refinement run it started
// must outlive it, with the dispatcher's environment.
func TestFlowRefinementOutlivesDispatchService(t *testing.T) {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if _, err := exec.LookPath("systemd-run"); err != nil || runtime == "" || !fileExists(filepath.Join(runtime, "systemd", "private")) {
		t.Skip("no systemd user manager")
	}
	l := refineLab(t, 3)
	url := ghURL("gpwsoi/backend", 12)
	l.Close(ghURL("gpwsoi/backend", 13), ghURL("gpwsoi/iac", 4))

	// TMUX points at the lab's server: nothing here may reach the real one.
	args := []string{"--user", "--collect", "--quiet", "--wait", "-p", "Type=oneshot"}
	for _, kv := range append(append([]string{}, l.env...), "TMUX="+l.Sock+","+l.pid+",0", "XDG_RUNTIME_DIR="+runtime, "LAB_VALUE=two words, $HOME and 100%") {
		args = append(args, "--setenv="+kv)
	}
	cmd := exec.Command("systemd-run", append(args, l.ais, "flow", "dispatch")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dispatch as a service: %v\n%s", err, out)
	}

	run := l.refineRun(url)
	sid := l.waitRunning(run.ID)
	time.Sleep(500 * time.Millisecond)
	s := l.Session(sid)
	if s.Status != "active" || !s.Headless {
		t.Fatalf("the run did not outlive the dispatch service: %+v", s)
	}
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", *s.PID))
	must(t, err)
	if !strings.Contains(string(env), "\x00LAB_VALUE=two words, $HOME and 100%\x00") {
		t.Errorf("the run does not have the dispatcher's environment")
	}
	l.WriteFile(filepath.Join(l.Log, sid, "exit"), "0")
	l.Eventually("the run is done by itself", func() bool { return l.proposalState(run.ID) == "done" })
	if out := l.MustAis(nil, "log", sid).Stdout; !strings.Contains(out, "fake: exiting 0") {
		t.Errorf("log = %q", out)
	}
}
