package flow

import (
	"encoding/json"
	"fmt"
)

// Ticket is a ticket with what a planner needs to know about it: where it
// stands on the board, whether a proposal already holds it, and — for an
// epic — its sub-issues, to the depth they go.
//
// An epic stays ready while it has open sub-issues: sessions take its
// sub-issues, and the epic itself only once they are all closed, to finish it.
type Ticket struct {
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"`  // OPEN or CLOSED
	Status    string   `json:"status"` // on the board; "" when not on it
	HeldBy    string   `json:"held_by,omitempty"`
	SubIssues []Ticket `json:"sub_issues,omitempty"`
	OpenSubs  int      `json:"open_sub_issues"`
	// Plannable: a session could be proposed for it now — an open ticket no
	// proposal holds, not already scheduled or in progress on the board, and
	// with no open sub-issues of its own.
	Plannable bool `json:"plannable"`
	// Work is the plannable tickets under (and including) this one: what a
	// planner can propose sessions for.
	Work []string `json:"work"`
}

// maxDepth bounds how far down sub-issues are followed.
const maxDepth = 4

type subIssueNode struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	State string `json:"state"`
}

// subIssuesOf asks GitHub for an issue's sub-issues and whether it is open;
// a variable for tests.
var subIssuesOf = func(url string) (string, []subIssueNode, error) {
	out, err := ghRunner("issue", "view", url, "--json", "state,subIssues")
	if err != nil {
		return "", nil, err
	}
	var data struct {
		State     string `json:"state"`
		SubIssues struct {
			Nodes []subIssueNode `json:"nodes"`
		} `json:"subIssues"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return "", nil, fmt.Errorf("gh issue view %s: %w", url, err)
	}
	return data.State, data.SubIssues.Nodes, nil
}

// workSet is the board and the queue, read once, for building Tickets.
type workSet struct {
	w     *Workspace
	board map[string]Item   // url -> board item
	held  map[string]string // url -> proposal holding it
}

func newWorkSet(w *Workspace, db *DB, src *project) (*workSet, error) {
	items, err := src.Items()
	if err != nil {
		return nil, err
	}
	ws := &workSet{w: w, board: map[string]Item{}, held: map[string]string{}}
	for _, it := range items {
		ws.board[it.URL] = it
	}
	active, err := proposals(db, activeStates)
	if err != nil {
		return nil, err
	}
	for _, p := range active {
		for _, t := range p.Tickets {
			ws.held[t] = p.ID
		}
	}
	return ws, nil
}

// ticket builds the Ticket for url, following its sub-issues.
func (ws *workSet) ticket(url, title, state string, depth int) (Ticket, error) {
	t := Ticket{URL: url, Title: title, State: state, HeldBy: ws.held[url]}
	if it, ok := ws.board[url]; ok {
		t.Status = it.Status
		if t.Title == "" {
			t.Title = it.Title
		}
	}
	if depth < maxDepth {
		st, subs, err := subIssuesOf(url)
		if err != nil {
			return t, err
		}
		if t.State == "" {
			t.State = st
		}
		for _, n := range subs {
			sub, err := ws.ticket(n.URL, n.Title, n.State, depth+1)
			if err != nil {
				return t, err
			}
			if sub.State == "OPEN" {
				t.OpenSubs++
			}
			t.SubIssues = append(t.SubIssues, sub)
			t.Work = append(t.Work, sub.Work...)
		}
	}
	busy := t.Status == ws.w.Source.Scheduled || t.Status == ws.w.Source.InProgress
	t.Plannable = t.State == "OPEN" && t.HeldBy == "" && !busy && t.OpenSubs == 0
	if t.Plannable {
		t.Work = append(t.Work, url)
	}
	if t.Work == nil {
		t.Work = []string{}
	}
	return t, nil
}

// readyWork is every ready ticket with its sub-issues: what a planner reads.
func readyWork(w *Workspace, db *DB) ([]Ticket, error) {
	src := sourceFor(w, db)
	ws, err := newWorkSet(w, db, src)
	if err != nil {
		return nil, err
	}
	var out []Ticket
	for _, it := range ws.board {
		if it.Status != w.Source.Ready {
			continue
		}
		t, err := ws.ticket(it.URL, it.Title, "", 0)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sortTickets(out)
	return out, nil
}

func sortTickets(ts []Ticket) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].URL < ts[j-1].URL; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// plannable counts the distinct tickets sessions could be proposed for.
func plannable(ts []Ticket) int {
	seen := map[string]bool{}
	for _, t := range ts {
		for _, u := range t.Work {
			seen[u] = true
		}
	}
	return len(seen)
}

// openSubIssues is how many open sub-issues a ticket has: an epic with any
// cannot be queued itself.
func openSubIssues(url string) (int, error) {
	_, subs, err := subIssuesOf(url)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range subs {
		if s.State == "OPEN" {
			n++
		}
	}
	return n, nil
}
