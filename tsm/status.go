package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type StatusCounts struct {
	Wants, Busy, Panes, Sessions int
}

func CountStatus(snap *Snapshot, st *AisState, now time.Time) StatusCounts {
	var c StatusCounts
	for _, r := range BuildRows(snap, st, TreeOptions{Now: now}) {
		if r.Wants {
			c.Wants++
		}
	}
	for _, s := range snap.Sessions {
		c.Sessions++
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				c.Panes++
				if p.ID != snap.Client.PaneID && p.Busy(now) {
					c.Busy++
				}
			}
		}
	}
	return c
}

func (c StatusCounts) Tmux() string {
	var parts []string
	if c.Wants > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]●%d", cWant.Dark, c.Wants))
	}
	if c.Busy > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]•%d", cOK.Dark, c.Busy))
	}
	parts = append(parts, fmt.Sprintf("#[fg=%s]%d·%d", cDim.Dark, c.Panes, c.Sessions))
	return strings.Join(parts, " ") + "#[default]"
}

func statusCmd(t Tmux, a Ais, args []string) int {
	pane := ""
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "--pane":
		pane = args[1]
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	snap, err := t.Snapshot()
	if err != nil {
		fmt.Println("tsm: no tmux")
		return 1
	}
	if pane != "" {
		snap.Client.PaneID = pane
	}
	var st *AisState
	if a.Available() {
		st, _ = a.State()
	}
	fmt.Println(CountStatus(snap, st, time.Now()).Tmux())
	return 0
}
