package main

import (
	"fmt"
)

// An Op is one thing to do to a host's tmux, written down so it can be done
// somewhere else: the popup does local ones itself, and sends remote ones to
// the tsm on that host ("tsm _op"), which does them with the same code.
// It names rows by tmux ids, which mean the same on both ends.
type Op struct {
	Action  string    `json:"action"` // focus zoom sibling duplicate restart kill rename create moves
	Kind    RowKind   `json:"kind"`
	Session string    `json:"session,omitempty"`
	Window  string    `json:"window,omitempty"`
	Pane    string    `json:"pane,omitempty"`
	Ais     string    `json:"ais,omitempty"` // the ais session, for restart
	Place   Placement `json:"place,omitempty"`
	Name    string    `json:"name,omitempty"`
	Create  *WireSpec `json:"create,omitempty"`
	Plan    []MoveOp  `json:"plan,omitempty"`
	// Link names the tmux client to move, on a host tsm reaches over ssh
	// and so has no client of its own there (see "tsm attach").
	Link string `json:"link,omitempty"`
}

// WireSpec is a CreateSpec with its anchor named rather than pointed at.
type WireSpec struct {
	Profile  string    `json:"profile"`
	Dir      string    `json:"dir"`
	Worktree bool      `json:"worktree"`
	Place    Placement `json:"place"`
	About    string    `json:"about"`
	Anchor   string    `json:"anchor,omitempty"`
}

func (c CreateSpec) Wire() *WireSpec {
	w := &WireSpec{Profile: c.Profile, Dir: c.Dir, Worktree: c.Worktree, Place: c.Place, About: c.About}
	if c.Anchor != nil {
		w.Anchor = c.Anchor.ID
	}
	return w
}

// OpFor writes down what an action on a row means.
func OpFor(action string, r Row) Op {
	op := Op{Action: action, Kind: r.Kind}
	if r.Session != nil {
		op.Session = r.Session.ID
	}
	if r.Window != nil {
		op.Window = r.Window.ID
	}
	if r.Pane != nil {
		op.Pane = r.Pane.ID
	}
	if r.Ais != nil {
		op.Ais = r.Ais.ID
	}
	return op
}

// row finds an op's row again in a host's own snapshot and ais state.
func (op Op) row(snap *Snapshot, st *AisState) (Row, error) {
	r := Row{Kind: op.Kind}
	if op.Kind == RowResumable {
		r.Ais = st.ByID()[op.Ais]
		if r.Ais == nil {
			return r, fmt.Errorf("ais session %s is not on record", op.Ais)
		}
		return r, nil
	}
	for _, s := range snap.Sessions {
		if s.ID == op.Session {
			r.Session = s
		}
		for _, w := range s.Windows {
			if w.ID == op.Window {
				r.Window = w
			}
			for _, p := range w.Panes {
				if p.ID == op.Pane {
					r.Pane = p
				}
			}
		}
	}
	switch {
	case op.Pane != "" && r.Pane == nil:
		return r, fmt.Errorf("pane %s is gone", op.Pane)
	case op.Window != "" && r.Window == nil:
		return r, fmt.Errorf("window %s is gone", op.Window)
	case op.Session != "" && r.Session == nil:
		return r, fmt.Errorf("session %s is gone", op.Session)
	}
	if r.Pane != nil {
		r.Ais = st.ByID()[r.Pane.AisID]
		if r.Ais == nil && r.Pane.AisID == "" {
			r.Ais = st.ByPane()[r.Pane.ID]
		}
	}
	return r, nil
}

// Exec does op with acts, which carry the host's snapshot.
func Exec(acts Actions, st *AisState, op Op) (Outcome, error) {
	if op.Action == "moves" {
		done, err := acts.tmux.RunMoves(op.Plan)
		if err != nil {
			return Outcome{Status: fmt.Sprintf("ran %d of %d move(s)", done, len(op.Plan))}, err
		}
		return Outcome{Status: fmt.Sprintf("moved — %d change(s) made", done)}, nil
	}
	if op.Action == "create" {
		if op.Create == nil {
			return Outcome{}, fmt.Errorf("create without a spec")
		}
		c := op.Create
		spec := CreateSpec{Profile: c.Profile, Dir: c.Dir, Worktree: c.Worktree, Place: c.Place, About: c.About}
		if c.Anchor != "" {
			spec.Anchor = acts.snap.Pane(c.Anchor)
		}
		return acts.Create(spec)
	}
	r, err := op.row(acts.snap, st)
	if err != nil {
		return Outcome{}, err
	}
	switch op.Action {
	case "focus":
		return acts.Focus(r)
	case "zoom":
		return acts.Zoom(r)
	case "sibling":
		return acts.Sibling(r, op.Place)
	case "duplicate":
		return acts.Duplicate(r)
	case "restart":
		return acts.Restart(r.Ais)
	case "kill":
		return acts.Kill(r)
	case "rename":
		return acts.Rename(r, op.Name)
	}
	return Outcome{}, fmt.Errorf("unknown action %q", op.Action)
}
