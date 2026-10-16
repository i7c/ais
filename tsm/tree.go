package main

import (
	"strings"
	"time"
)

type RowKind int

const (
	RowHeader    RowKind = iota // a section title; never selectable
	RowSession                  // a tmux session
	RowWindow                   // a window; with one pane it stands for that pane too
	RowPane                     // a pane in a window of several
	RowResumable                // an ais session that ended and can be restarted
)

// A Row is one line of the list. Exactly the pointers its kind implies are
// set; Pane is set on a single-pane window row as well, since that row is
// both and every pane action applies to it.
type Row struct {
	Kind    RowKind
	Title   string // header text
	Session *Session
	Window  *Window
	Pane    *Pane
	Ais     *AisSession // the ais session behind Pane, or the resumable one
	Depth   int
	Wants   bool   // in the "needs you" section
	Aside   string // what tells this row from another with the same title
}

// Key identifies a row across refreshes, so the cursor stays on the same
// thing while the list around it changes.
func (r Row) Key() string {
	prefix := ""
	if r.Wants {
		prefix = "!"
	}
	switch r.Kind {
	case RowSession:
		return "s" + r.Session.ID
	case RowWindow:
		return prefix + "w" + r.Window.ID
	case RowPane:
		return prefix + "p" + r.Pane.ID
	case RowResumable:
		return "r" + r.Ais.ID
	}
	return "h" + r.Title
}

func (r Row) Selectable() bool { return r.Kind != RowHeader }

// Target is the pane a row stands for: itself, or what its window or session
// is showing. Previews and focus both go there.
func (r Row) Target() *Pane {
	switch {
	case r.Pane != nil:
		return r.Pane
	case r.Window != nil:
		return r.Window.ActivePane()
	case r.Session != nil:
		if w := r.Session.ActiveWindow(); w != nil {
			return w.ActivePane()
		}
	}
	return nil
}

type TreeOptions struct {
	Filter    string
	Collapsed map[string]bool // by session id
	Now       time.Time
	Resumable int // how many ended sessions to offer; all of them while filtering
}

// BuildRows lays out the list: who wants you, then every session, then the
// sessions that ended and can be picked up again.
func BuildRows(snap *Snapshot, st *AisState, opt TreeOptions) []Row {
	byID := st.ByID()
	byPane := st.ByPane()
	window := 60.0
	if st != nil && st.NotifyWindow > 0 {
		window = st.NotifyWindow
	}
	terms := strings.Fields(strings.ToLower(opt.Filter))
	filtering := len(terms) > 0

	var wants, tree []Row
	for _, s := range snap.Sessions {
		var body []Row
		for _, w := range s.Windows {
			var panes []Row
			for _, p := range w.Panes {
				a := byID[p.AisID]
				if a == nil && p.AisID == "" {
					// Started by an ais from before pane marks: the record
					// still knows which pane it is in.
					a = byPane[p.ID]
				}
				if filtering && !matches(terms, paneText(s, w, p, a)) {
					continue
				}
				if a != nil && a.WantsYou(window, opt.Now) && p.ID != snap.Client.PaneID {
					wants = append(wants, Row{Kind: RowPane, Session: s, Window: w, Pane: p, Ais: a, Depth: 1, Wants: true})
				}
				panes = append(panes, Row{Kind: RowPane, Session: s, Window: w, Pane: p, Ais: a, Depth: 2})
			}
			switch {
			case len(panes) == 0:
			case len(w.Panes) == 1:
				// One pane is the window: say it on one line.
				r := panes[0]
				r.Kind, r.Depth = RowWindow, 1
				body = append(body, r)
			default:
				body = append(body, Row{Kind: RowWindow, Session: s, Window: w, Depth: 1})
				body = append(body, panes...)
			}
		}
		if len(body) == 0 {
			continue
		}
		tree = append(tree, Row{Kind: RowSession, Session: s})
		if !opt.Collapsed[s.ID] || filtering {
			tree = append(tree, body...)
		}
	}

	var rows []Row
	if len(wants) > 0 {
		rows = append(rows, Row{Kind: RowHeader, Title: "needs you"})
		rows = append(rows, wants...)
	}
	rows = append(rows, tree...)

	limit := opt.Resumable
	if filtering {
		limit = 0
	}
	var ended []Row
	labels := map[string]int{}
	for _, a := range st.Resumable(limit) {
		if filtering && !matches(terms, strings.ToLower(strings.Join([]string{a.Label(), a.Name, a.Profile, a.Cwd}, " "))) {
			continue
		}
		labels[a.Label()]++
		ended = append(ended, Row{Kind: RowResumable, Ais: a, Depth: 1})
	}
	for i := range ended {
		if a := ended[i].Ais; labels[a.Label()] > 1 {
			ended[i].Aside = a.Name
			if a.Name == "" {
				ended[i].Aside = tilde(a.Cwd)
			}
		}
	}
	if len(ended) > 0 {
		rows = append(rows, Row{Kind: RowHeader, Title: "resumable"})
		rows = append(rows, ended...)
	}
	return rows
}

func paneText(s *Session, w *Window, p *Pane, a *AisSession) string {
	parts := []string{s.Name, w.Name, p.Command, p.Dir(), p.Title, p.Profile, p.Desc}
	if a != nil {
		parts = append(parts, a.Name, a.ID)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func matches(terms []string, text string) bool {
	for _, t := range terms {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}
