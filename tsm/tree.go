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
	Host    *Host  // the remote it is on; nil for this host
	Wants   bool   // in the "needs you" section
	Aside   string // what tells this row from another with the same title
	Rang    bool   // a bell that reached you through a connection came from here
}

// Key identifies a row across refreshes, so the cursor stays on the same
// thing while the list around it changes.
func (r Row) Key() string {
	prefix := ""
	if r.Host != nil {
		prefix = r.Host.Name + "|" // pane %1 exists on every host
	}
	if r.Wants {
		prefix += "!"
	}
	switch r.Kind {
	case RowSession:
		return prefix + "s" + r.Session.ID
	case RowWindow:
		return prefix + "w" + r.Window.ID
	case RowPane:
		return prefix + "p" + r.Pane.ID
	case RowResumable:
		return prefix + "r" + r.Ais.ID
	}
	return prefix + "h" + r.Title
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

// BuildRows lays out the list for this host alone: who wants you, then every
// session, then the sessions that ended and can be picked up again.
func BuildRows(snap *Snapshot, st *AisState, opt TreeOptions) []Row {
	return BuildAll(snap, st, nil, opt)
}

// BuildAll lays out every host: who wants you, wherever they are, first;
// then this host's sessions and ended ones; then each remote's, under a
// heading of its own.
func BuildAll(snap *Snapshot, st *AisState, remotes []*Host, opt TreeOptions) []Row {
	relayed, quiet := relayedBells(snap, remotes)
	wants, tree, ended := sections(snap, st, nil, opt, "", quiet)
	type part struct {
		host        *Host
		tree, ended []Row
	}
	var parts []part
	for _, h := range remotes {
		if h.Snap == nil {
			parts = append(parts, part{host: h})
			continue
		}
		w, t, e := sections(h.Snap, h.State, h, opt, relayed[h.Name], nil)
		wants = append(wants, w...)
		parts = append(parts, part{h, t, e})
	}

	var rows []Row
	if len(wants) > 0 {
		rows = append(rows, Row{Kind: RowHeader, Title: "needs you"})
		rows = append(rows, wants...)
	}
	rows = append(rows, tree...)
	if len(ended) > 0 {
		rows = append(rows, Row{Kind: RowHeader, Title: "resumable"})
		rows = append(rows, ended...)
	}
	for _, p := range parts {
		filtered := opt.Filter != "" && len(p.tree)+len(p.ended) == 0
		if filtered {
			continue // nothing there matches; the heading would only be noise
		}
		rows = append(rows, Row{Kind: RowHeader, Title: p.host.Name, Host: p.host})
		rows = append(rows, p.tree...)
		if len(p.ended) > 0 {
			rows = append(rows, Row{Kind: RowHeader, Title: "resumable · " + p.host.Name, Host: p.host})
			rows = append(rows, p.ended...)
		}
	}
	return rows
}

// relayedBells finds the bells that came over a connection. A remote's tmux
// does not flag a bell in a window its client is showing, even when you are
// looking somewhere else here; the bell then reaches this tmux through the
// connection's pane instead. So a ringing window here that shows the way to a
// remote means the pane that remote's linked client shows rang: relayed says
// which, by host, and quiet the panes here whose bell it explains.
func relayedBells(snap *Snapshot, remotes []*Host) (relayed map[string]string, quiet map[string]bool) {
	rang := false
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			rang = rang || w.Bell
		}
	}
	if !rang {
		return nil, nil // the usual case: no need to look for connections
	}
	relayed, quiet = map[string]string{}, map[string]bool{}
	for _, h := range remotes {
		if h.Snap == nil || h.Remote.Conf.Name == "" {
			continue
		}
		hosting := hostingPane(snap, h.Remote.Conf)
		if hosting == nil || !hosting.Window.Bell || hosting.Window.ActivePane() != hosting {
			continue
		}
		if id := h.Viewing(hosting); id != "" && h.Snap.Pane(id) != nil {
			relayed[h.Name], quiet[hosting.ID] = id, true
		}
	}
	return relayed, quiet
}

// sections builds one host's rows: its callers, its tree, its ended sessions.
// rang is a pane whose bell came over a connection; quiet are panes whose
// own bell is one of those.
func sections(snap *Snapshot, st *AisState, host *Host, opt TreeOptions, rang string, quiet map[string]bool) (wants, tree, ended []Row) {
	byID := st.ByID()
	byPane := st.ByPane()
	window := 60.0
	if st != nil && st.NotifyWindow > 0 {
		window = st.NotifyWindow
	}
	terms := strings.Fields(strings.ToLower(opt.Filter))
	filtering := len(terms) > 0

	for _, s := range snap.Sessions {
		var body []Row
		for _, w := range s.Windows {
			var panes []Row
			called := false // something in the window is already among the callers
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
					wants = append(wants, Row{Kind: RowPane, Session: s, Window: w, Pane: p, Ais: a, Depth: 1, Wants: true, Host: host})
					called = true
				}
				r := Row{Kind: RowPane, Session: s, Window: w, Pane: p, Ais: a, Depth: 2, Host: host, Rang: p.ID == rang}
				if r.Rang && !called {
					caller := r
					caller.Depth, caller.Wants = 1, true
					wants = append(wants, caller)
					called = true
				}
				panes = append(panes, r)
			}
			// A bell, in a window you were not looking at, calls for you too
			// (weechat rings it for a highlight or a private message). tmux
			// forgets it once you look, and so does this.
			if w.Bell && !called {
				if p := w.ActivePane(); p != nil && p.ID != snap.Client.PaneID && !quiet[p.ID] {
					for _, r := range panes {
						if r.Pane == p {
							r.Depth, r.Wants = 1, true
							wants = append(wants, r)
						}
					}
				}
			}
			switch {
			case len(panes) == 0:
			case len(w.Panes) == 1:
				// One pane is the window: say it on one line.
				r := panes[0]
				r.Kind, r.Depth = RowWindow, 1
				body = append(body, r)
			default:
				body = append(body, Row{Kind: RowWindow, Session: s, Window: w, Depth: 1, Host: host})
				body = append(body, panes...)
			}
		}
		if len(body) == 0 {
			continue
		}
		tree = append(tree, Row{Kind: RowSession, Session: s, Host: host})
		if !opt.Collapsed[collapseKey(host, s)] || filtering {
			tree = append(tree, body...)
		}
	}

	limit := opt.Resumable
	if filtering {
		limit = 0
	}
	labels := map[string]int{}
	for _, a := range st.Resumable(limit) {
		if filtering && !matches(terms, strings.ToLower(strings.Join([]string{a.Label(), a.Name, a.Profile, a.Cwd}, " "))) {
			continue
		}
		labels[a.Label()]++
		ended = append(ended, Row{Kind: RowResumable, Ais: a, Depth: 1, Host: host})
	}
	for i := range ended {
		if a := ended[i].Ais; labels[a.Label()] > 1 {
			ended[i].Aside = a.Name
			if a.Name == "" {
				ended[i].Aside = tilde(a.Cwd)
			}
		}
	}
	return wants, tree, ended
}

// collapseKey names a session for the collapsed set: ids repeat across hosts.
func collapseKey(host *Host, s *Session) string {
	if host == nil {
		return s.ID
	}
	return host.Name + "|" + s.ID
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
