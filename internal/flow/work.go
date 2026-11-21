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
	URL    string `json:"url"`
	Title  string `json:"title"`
	State  string `json:"state"`  // OPEN or CLOSED
	Status string `json:"status"` // on the board; "" when not on it
	HeldBy string `json:"held_by,omitempty"`
	// BlockedBy is the open issues GitHub records as blocking this one; it
	// is not plannable until they are closed (merely queued or running is
	// not enough: their work is not in yet).
	BlockedBy []string `json:"blocked_by,omitempty"`
	SubIssues []Ticket `json:"sub_issues,omitempty"`
	OpenSubs  int      `json:"open_sub_issues"`
	// Plannable: a session could be proposed for it now — an open ticket no
	// proposal holds, not already scheduled or in progress on the board, not
	// blocked by an open issue, and with no open sub-issues of its own.
	Plannable bool `json:"plannable"`
	// Work is the plannable tickets under (and including) this one: what a
	// planner can propose sessions for.
	Work []string `json:"work"`
	// Assessment is what refinement found, when it ran; Pending says why a
	// ticket that is otherwise free is not plannable yet: "assessing" (no
	// current assessment), "refinement" (waiting on its author), "still L1"
	// (back in ready, unchanged since it was sent to its author).
	Assessment *Assessment `json:"assessment,omitempty"`
	Pending    string      `json:"pending,omitempty"`
	// needsAssessment: free, but refinement has not seen this version.
	needsAssessment bool
}

// maxDepth bounds how far down sub-issues are followed.
const maxDepth = 4

type subIssueNode struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	State string `json:"state"`
}

// issueInfo is what GitHub says about one issue beyond the board: open or
// closed, its sub-issues, and the open issues blocking it.
type issueInfo struct {
	State     string
	SubIssues []subIssueNode
	Blockers  []string // open ones only
	Version   string   // ticketVersion of its title and body
}

// issueInfoOf asks GitHub; a variable for tests.
var issueInfoOf = func(url string) (issueInfo, error) {
	out, err := ghRunner("issue", "view", url, "--json", "title,body,state,subIssues,blockedBy")
	if err != nil {
		return issueInfo{}, err
	}
	var data struct {
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		SubIssues struct {
			Nodes []subIssueNode `json:"nodes"`
		} `json:"subIssues"`
		BlockedBy struct {
			Nodes []subIssueNode `json:"nodes"`
		} `json:"blockedBy"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return issueInfo{}, fmt.Errorf("gh issue view %s: %w", url, err)
	}
	body, ref := splitRefinement(data.Body)
	info := issueInfo{State: data.State, SubIssues: data.SubIssues.Nodes, Version: ticketVersion(data.Title, body, ref)}
	for _, b := range data.BlockedBy.Nodes {
		if b.State == "OPEN" {
			info.Blockers = append(info.Blockers, b.URL)
		}
	}
	return info, nil
}

// workSet is the board and the queue, read once, for building Tickets.
type workSet struct {
	w      *Workspace
	db     *DB
	board  map[string]Item   // url -> board item
	held   map[string]string // url -> proposal holding it
	refine bool              // refinement gates planning in this workspace
}

func newWorkSet(w *Workspace, db *DB, src Source) (*workSet, error) {
	items, err := src.Items()
	if err != nil {
		return nil, err
	}
	ws := &workSet{w: w, db: db, board: map[string]Item{}, held: map[string]string{}}
	if w.Refine.Enabled {
		ok, err := src.HasStatus(w.Refine.Status)
		if err != nil {
			return nil, err
		}
		ws.refine = ok
	}
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
	info, err := issueInfoOf(url)
	if err != nil {
		return t, err
	}
	if t.State == "" {
		t.State = info.State
	}
	t.BlockedBy = info.Blockers
	if depth < maxDepth {
		for _, n := range info.SubIssues {
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
	t.Plannable = t.State == "OPEN" && t.HeldBy == "" && !busy && t.OpenSubs == 0 && len(t.BlockedBy) == 0
	if ws.refine {
		if t.Status == ws.w.Refine.Status {
			t.Plannable, t.Pending = false, "refinement"
		}
		a, err := assessmentOf(ws.db, url)
		if err != nil {
			return t, err
		}
		if a != nil && a.Level != "" {
			a.Current = a.Version == info.Version
			t.Assessment = a
		}
		if t.Plannable {
			switch {
			case a == nil || a.Level == "" || !a.Current:
				t.Plannable, t.Pending, t.needsAssessment = false, "assessing", true
				if a != nil && a.FailedAt != "" {
					t.Pending = "assessing (the last run failed " + ago(a.FailedAt) + " ago)"
				}
			case a.Level == "L1":
				t.Plannable, t.Pending = false, "still L1: its questions are unanswered"
			}
		}
	}
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
	info, err := issueInfoOf(url)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range info.SubIssues {
		if s.State == "OPEN" {
			n++
		}
	}
	return n, nil
}
