package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// attachArgv is how "tsm attach" reaches a remote's tmux: ssh or mosh,
// attaching to (or making) its session, with TSM_LINK in the tmux client's
// environment so the remote tsm can tell that client from any other.
func attachArgv(conf RemoteConf, link string) []string {
	remote := []string{"env", linkEnv + "=" + link, "tmux", "new-session", "-A", "-s", conf.Session}
	if len(conf.Command) > 0 {
		return append(append([]string{}, conf.Command...), remote...)
	}
	if conf.Attach == "mosh" {
		return append([]string{"mosh", conf.SSH, "--"}, remote...)
	}
	return []string{"ssh", "-t", conf.SSH, "--", ShellJoin(remote)}
}

// attach connects this pane to a remote and marks it as the way there, for
// as long as the connection lasts.
func attach(t Tmux, conf Config, name string) int {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		fmt.Fprintln(os.Stderr, "tsm attach: run it in a tmux pane; that pane becomes the way to", name)
		return 2
	}
	var rc *RemoteConf
	for i := range conf.Remotes {
		if conf.Remotes[i].Name == name {
			rc = &conf.Remotes[i]
		}
	}
	if rc == nil {
		fmt.Fprintf(os.Stderr, "tsm attach: no [[remote]] called %q in %s\n", name, tilde(configPath()))
		return 2
	}
	t.run("set-option", "-p", "-t", pane, "@tsm_remote", rc.Name)
	defer t.run("set-option", "-p", "-u", "-t", pane, "@tsm_remote")

	argv := attachArgv(*rc, hostname()+":"+pane)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The connection owns the terminal's signals; tsm only waits to unmark.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP)
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "tsm attach:", err)
		return 1
	}
	return 0
}
