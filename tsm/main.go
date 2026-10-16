// tsm — a tmux session manager: every session, window and pane in one popup,
// with what is running where, and the things you do to them a key away.
//
// It holds no state of its own. tmux is the record of what exists; ais, when
// it is installed, adds what it knows about the processes it started.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

const usage = `usage: tsm                open the manager (made for a tmux popup)
       tsm attach NAME    connect this pane to remote NAME's tmux, and
                          mark it as the way there
       tsm snapshot       print every session, window and pane as JSON

  bind-key a display-popup -E -w 90% -h 80% -T " tsm " "tsm"

config: ~/.config/tsm/config.toml ($TSM_CONFIG), for remotes:
  [[remote]]
  name = "karif"     # ssh destination too, unless ssh = "..." says otherwise
  attach = "mosh"    # or "ssh" (default)
  session = "main"   # the tmux session to attach to there

env: TSM_AIS   the ais to use (default: ais on PATH)
     TSM_ROOTS where repos live, colon-separated (default: ~/git)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	t := Tmux{r: execRunner{}}
	conf, confErr := LoadConfig()
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return 0
		// What another host's tsm asks of this one, over ssh.
		case "_state":
			return serveState(t, NewAis(execRunner{}))
		case "_op":
			return serveOp(t, NewAis(execRunner{}), os.Stdin)
		case "_capture":
			if len(args) != 2 {
				return 2
			}
			return serveCapture(t, args[1])
		case "attach":
			if confErr != nil {
				fmt.Fprintln(os.Stderr, "tsm:", confErr)
				return 1
			}
			if len(args) != 2 {
				fmt.Fprint(os.Stderr, usage)
				return 2
			}
			return attach(t, conf, args[1])
		case "snapshot":
			snap, err := t.Snapshot()
			if err != nil {
				fmt.Fprintln(os.Stderr, "tsm:", err)
				return 1
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(snap); err != nil {
				fmt.Fprintln(os.Stderr, "tsm:", err)
				return 1
			}
			return 0
		default:
			fmt.Fprintf(os.Stderr, "tsm: unknown command %q\n\n%s", args[0], usage)
			return 2
		}
	}

	// Taken before the first frame, so the popup opens on the real list and
	// not on an empty one that fills in a moment later.
	snap, err := t.Snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsm: cannot read tmux:", err)
		return 1
	}
	m := newModel(t, NewAis(execRunner{}), snap, conf)
	if confErr != nil {
		m.setStatus(confErr.Error(), true)
	}
	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tsm:", err)
		return 1
	}
	return 0
}
