// Command ais starts a process from a named profile, records the run, and
// finds its tmux pane again later; "ais flow" plans and dispatches sessions.
// See README.md and docs/flow-proposal.md.
package main

import (
	"os"

	"github.com/i7c/ais/internal/ais"
	"github.com/i7c/ais/internal/flow"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "flow" {
		os.Exit(flow.Main(os.Args[2:]))
	}
	os.Exit(ais.Main(os.Args[1:]))
}
