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

const usage = `usage: tsm             open the manager (made for a tmux popup)
       tsm snapshot    print every session, window and pane as JSON

  bind-key a display-popup -E -w 90% -h 80% -T " tsm " "tsm"

env: TSM_AIS names the ais to use (default: ais on PATH)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	t := Tmux{r: execRunner{}}
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return 0
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
	m := newModel(t, NewAis(execRunner{}), snap)
	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tsm:", err)
		return 1
	}
	return 0
}
