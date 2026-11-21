package flow

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/i7c/ais/internal/ais"
)

// sweep queues a refinement run for every ticket that turned plannable
// since refinement last saw it: ready itself or under a ready epic, free,
// and with no assessment of its current version. It runs from the dispatch
// tick, at most every refine.sweep per workspace; the dispatcher then starts
// the runs within the limits like any other proposal. Nothing in it fails
// the tick: what goes wrong is logged and tried at the next sweep.
func (d *dispatcher) sweep() {
	var names []string
	for n := range d.cfg.Workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w := d.cfg.Workspaces[n]
		if !w.Refine.Enabled {
			continue
		}
		queued, err := d.sweepWorkspace(w)
		if err != nil {
			note("refinement sweep of %s: %v", w.Name, err)
			logEvent(d.db, "sweep-failed", w.Name, err.Error())
			continue
		}
		if queued > 0 {
			note("%s: queued %d refinement run(s)", w.Name, queued)
		}
	}
}

func (d *dispatcher) sweepWorkspace(w *Workspace) (int, error) {
	key := "sweep:" + w.Name
	var last string
	d.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&last)
	if t, ok := parseStamp(last); ok && now().Sub(t) < w.Refine.Sweep {
		return 0, nil
	}
	if !d.dry {
		// Stamped first: a sweep that fails is not retried every tick.
		d.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, stamp(now()))
	}
	if ok, err := sourceFor(w, d.db).HasStatus(w.Refine.Status); err != nil {
		return 0, err
	} else if !ok {
		logEvent(d.db, "refine-off", w.Name, fmt.Sprintf("the board has no %q status", w.Refine.Status))
		return 0, nil
	}
	if ok, err := ais.TakesBatch(d.cfg.raw, w.Refine.Profile); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("profile %s cannot run headless (no batch template), so it cannot refine; set [workspace.%s.refine] profile", w.Refine.Profile, w.Name)
	}
	tickets, err := readyWork(w, d.db)
	if err != nil {
		return 0, err
	}
	var due []Ticket
	seen := map[string]bool{}
	var walk func(t Ticket)
	walk = func(t Ticket) {
		if t.needsAssessment && !seen[t.URL] {
			seen[t.URL] = true
			due = append(due, t)
		}
		for _, sub := range t.SubIssues {
			walk(sub)
		}
	}
	for _, t := range tickets {
		walk(t)
	}
	queued := 0
	for _, t := range due {
		if a, _ := assessmentOf(d.db, t.URL); a != nil && a.FailedAt != "" {
			if at, ok := parseStamp(a.FailedAt); ok && now().Sub(at) < w.Refine.Retry {
				continue
			}
		}
		title := "Refine " + shortTickets([]string{t.URL}) + ": " + elide(t.Title, 50)
		if d.dry {
			fmt.Fprintf(stdout, "would queue a refinement run: %s\n", title)
			continue
		}
		p := &Proposal{ID: newID("r"), Workspace: w.Name, Title: title, Tickets: []string{t.URL}, Repos: []string{},
			Profile: w.Refine.Profile, Prompt: "(rendered when it starts: ais flow prompt show refine)", Kind: "refine",
			Priority: refinePriority, After: []string{}, State: "queued", CreatedAt: stamp(now())}
		err := d.db.tx(func(tx *sql.Tx) error {
			// Held already (a planner queued it meanwhile): leave it.
			held, err := proposals(tx, activeStates)
			if err != nil {
				return err
			}
			for _, other := range held {
				if contains(other.Tickets, t.URL) {
					return nil
				}
			}
			if err := insertProposal(tx, p); err != nil {
				return err
			}
			logEvent(tx, "queued", p.ID, map[string]any{"title": p.Title, "tickets": p.Tickets, "kind": "refine"})
			queued++
			return nil
		})
		if err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// refinePriority puts refinement runs ahead of work: a planner waits for
// their outcome, and a run is short.
const refinePriority = 100
