// Command ais starts a process from a named profile, records the run, and
// finds its tmux pane again later. See README.md.
package main

import (
	"os"

	"github.com/i7c/ais/internal/ais"
)

func main() { os.Exit(ais.Main(os.Args[1:])) }
