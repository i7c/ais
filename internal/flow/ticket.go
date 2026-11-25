package flow

import (
	"fmt"
	"os"
	"strings"
)

// "ais flow ticket": the task layer as agents see it. Every prompt ais
// gives an agent reads and writes tickets through these commands, so the
// tracker behind them can change without the prompts changing.

const ticketUsage = `usage: ais flow ticket <command> REF [options]

  show REF [--context] [--json]
                     the ticket: body, status, comments, sub-issues,
                     blockers; --context adds its ancestors to the top and
                     its blockers' last word
  comment REF (--body TEXT | --body-file F)
                     post a comment (a hand-off, a result)
  close REF (--done | --not-planned) [--comment TEXT]
                     close it with its evidence, or abandon it
  defer REF [--comment TEXT | --comment-file F]
                     not now: it stays open and goes back to the board's
                     backlog status, from where it can be made ready again
  new --title T (--body TEXT | --body-file F) [--parent REF] [--repo O/N]
      [--blocked-by REF,...] [--type T]
                     create a ticket (a follow-up: give its parent); prints
                     its reference
  assess REF --level L2|L3 --file F
  refine REF --file F
                     record a refinement run's outcome (see ais flow prompt
                     show refine); F is JSON, "-" for stdin

REF is an issue URL or owner/repo#number. The workspace is --ws, else the one
this session was started for, else the only one configured.
`

// ticketWorkspace is the workspace a ticket command acts in, or nil for a
// ticket outside them: --ws, the session's own, or the only one there is.
func ticketWorkspace(cfg *Config, ws string) (*Workspace, error) {
	if ws == "" {
		ws = os.Getenv("AIS_FLOW_WORKSPACE")
	}
	if ws != "" {
		return cfg.workspace(ws)
	}
	if len(cfg.Workspaces) == 1 {
		return cfg.workspace("")
	}
	return nil, nil
}

// ticketSource is the source for tickets in w, or plain issues without one.
func ticketSource(w *Workspace, db *DB) Source {
	if w == nil {
		return issueSource{}
	}
	return sourceFor(w, db)
}

func cmdTicket(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, ticketUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show", "view":
		return cmdTicketShow(rest)
	case "comment":
		return cmdTicketComment(rest)
	case "close":
		return cmdTicketClose(rest)
	case "defer":
		return cmdTicketDefer(rest)
	case "new", "create":
		return cmdTicketNew(rest)
	case "assess":
		return cmdTicketAssess(rest)
	case "refine":
		return cmdTicketRefine(rest)
	}
	return usagef("unknown ticket command %q; try show, comment, close, defer, new, assess, refine", sub)
}

// ticketCmd is what every ticket command starts from.
type ticketCmd struct {
	cfg *Config
	w   *Workspace // nil outside a workspace
	db  *DB
	src Source
	url string
}

func openTicketCmd(o opts, needRef bool) (*ticketCmd, error) {
	t := &ticketCmd{}
	if needRef {
		if len(o.pos) != 1 {
			return nil, usagef("name one ticket: its URL or owner/repo#number")
		}
		u, err := ticketURL(o.pos[0])
		if err != nil {
			return nil, err
		}
		t.url = u
	} else if len(o.pos) > 0 {
		return nil, usagef("unexpected %q", o.pos[0])
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	t.cfg = cfg
	if t.w, err = ticketWorkspace(cfg, o.str("--ws")); err != nil {
		return nil, err
	}
	if t.db, err = openDB(); err != nil {
		return nil, err
	}
	t.src = ticketSource(t.w, t.db)
	return t, nil
}

func (t *ticketCmd) close() { t.db.Close() }

// TicketView is "ticket show --json": the ticket, its board facts, the
// refinement block apart from the body, and with --context its chain.
type TicketView struct {
	*Issue
	Status     string        `json:"status,omitempty"`
	Autonomy   string        `json:"autonomy,omitempty"` // the board's ceiling
	Assessment *Assessment   `json:"assessment,omitempty"`
	Refinement *Refinement   `json:"refinement,omitempty"`
	Ancestors  []*Issue      `json:"ancestors,omitempty"`
	Blockers   []BlockerView `json:"blockers,omitempty"`
}

type BlockerView struct {
	Ref
	LastComment *Comment `json:"last_comment,omitempty"`
	ClosedBy    []Ref    `json:"closed_by_prs,omitempty"`
}

func cmdTicketShow(args []string) error {
	o, err := parseOpts(args, []string{"--ws"}, []string{"--context", "--json"}, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	v, err := t.view(o.flags["--context"])
	if err != nil {
		return err
	}
	if o.flags["--json"] {
		return writeJSON(v)
	}
	fmt.Fprint(stdout, v.markdown())
	return nil
}

func (t *ticketCmd) view(context bool) (*TicketView, error) {
	is, err := t.src.Issue(t.url)
	if err != nil {
		return nil, err
	}
	v := &TicketView{Issue: is}
	is.Body, v.Refinement = splitRefinement(is.Body)
	if items, err := t.src.Find([]string{t.url}); err == nil && len(items) == 1 {
		v.Status, v.Autonomy = items[0].Status, items[0].Autonomy
	}
	if t.w != nil {
		if a, err := assessmentOf(t.db, t.url); err == nil && a != nil {
			a.Current = a.Version == ticketVersion(is.Title, is.Body, v.Refinement)
			v.Assessment = a
		}
	}
	if !context {
		return v, nil
	}
	seen := map[string]bool{is.URL: true}
	for p := is.Parent; p != nil && !seen[p.URL] && len(v.Ancestors) < maxDepth+2; {
		seen[p.URL] = true
		anc, err := t.src.Issue(p.URL)
		if err != nil {
			return nil, err
		}
		anc.Body, _ = splitRefinement(anc.Body)
		anc.Comments = lastN(anc.Comments, 3)
		v.Ancestors = append(v.Ancestors, anc)
		p = anc.Parent
	}
	for _, b := range is.BlockedBy {
		bl, err := t.src.Issue(b.URL)
		if err != nil {
			return nil, err
		}
		bv := BlockerView{Ref: Ref{URL: bl.URL, Title: bl.Title, State: bl.State}, ClosedBy: bl.ClosedBy}
		if len(bl.Comments) > 0 {
			bv.LastComment = &bl.Comments[len(bl.Comments)-1]
		}
		v.Blockers = append(v.Blockers, bv)
	}
	return v, nil
}

func lastN(cs []Comment, n int) []Comment {
	if len(cs) > n {
		return cs[len(cs)-n:]
	}
	return cs
}

// markdown is the view as an agent reads it best: a document, not JSON.
func (v *TicketView) markdown() string {
	var b strings.Builder
	is := v.Issue
	fmt.Fprintf(&b, "# %s\n\n", is.Title)
	fmt.Fprintf(&b, "- ticket: %s (%s#%d)\n", is.URL, is.Repo, is.Number)
	state := is.State
	if is.StateReason != "" && is.State == "CLOSED" {
		state += " (" + strings.ToLower(is.StateReason) + ")"
	}
	fmt.Fprintf(&b, "- state: %s", state)
	if v.Status != "" {
		fmt.Fprintf(&b, ", board status %q", v.Status)
	}
	b.WriteString("\n")
	if is.Type != "" {
		fmt.Fprintf(&b, "- type: %s\n", is.Type)
	}
	if len(is.Labels) > 0 {
		fmt.Fprintf(&b, "- labels: %s\n", strings.Join(is.Labels, ", "))
	}
	if v.Autonomy != "" {
		fmt.Fprintf(&b, "- autonomy ceiling on the board: %s\n", v.Autonomy)
	}
	if a := v.Assessment; a != nil {
		cur := "for this version of the ticket"
		if !a.Current {
			cur = "for an earlier version; it changed since"
		}
		fmt.Fprintf(&b, "- assessed: %s %s (%s)", a.Level, cur, a.At)
		if a.Flag != "" {
			fmt.Fprintf(&b, ", flagged %q", a.Flag)
		}
		b.WriteString("\n")
	}
	if is.Parent != nil {
		fmt.Fprintf(&b, "- parent: %s %s\n", is.Parent.URL, is.Parent.Title)
	}
	refs := func(name string, rs []Ref) {
		if len(rs) == 0 {
			return
		}
		fmt.Fprintf(&b, "- %s:\n", name)
		for _, r := range rs {
			fmt.Fprintf(&b, "  - %s %s %s\n", r.URL, orDefault(r.State, ""), r.Title)
		}
	}
	refs("sub-issues", is.SubIssues)
	refs("blocked by", is.BlockedBy)
	refs("pull requests that close it", is.ClosedBy)
	fmt.Fprintf(&b, "\n## Body\n\n%s\n", orDefault(strings.TrimSpace(is.Body), "(empty)"))
	if r := v.Refinement; r != nil {
		fmt.Fprintf(&b, "\n## Refinement block (round %d; ais flow owns it)\n\n%s\n", r.Rounds, strings.TrimSpace(r.Text))
	}
	comments := func(cs []Comment) {
		for _, c := range cs {
			fmt.Fprintf(&b, "\n### %s, %s\n\n%s\n", orDefault(c.Author, "?"), c.At, strings.TrimSpace(c.Body))
		}
	}
	if len(is.Comments) > 0 {
		fmt.Fprintf(&b, "\n## Comments (%d)\n", len(is.Comments))
		comments(is.Comments)
	}
	for i, anc := range v.Ancestors {
		fmt.Fprintf(&b, "\n---\n\n## Ancestor %d: %s\n\n- ticket: %s, %s\n\n%s\n", i+1, anc.Title, anc.URL, anc.State,
			orDefault(strings.TrimSpace(anc.Body), "(empty)"))
		if len(anc.Comments) > 0 {
			fmt.Fprintf(&b, "\n### Its latest comments\n")
			comments(anc.Comments)
		}
	}
	if len(v.Blockers) > 0 {
		b.WriteString("\n---\n\n## Blockers\n")
		for _, bl := range v.Blockers {
			fmt.Fprintf(&b, "\n### %s (%s) %s\n", bl.Title, bl.State, bl.URL)
			for _, pr := range bl.ClosedBy {
				fmt.Fprintf(&b, "- closed by %s\n", pr.URL)
			}
			if bl.LastComment != nil {
				fmt.Fprintf(&b, "\nLast comment, %s:\n\n%s\n", bl.LastComment.Author, strings.TrimSpace(bl.LastComment.Body))
			}
		}
	}
	return b.String()
}

func cmdTicketComment(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--body", "--body-file"}, nil, map[string]string{"-w": "--ws", "-b": "--body", "-F": "--body-file"})
	if err != nil {
		return err
	}
	body, given, err := o.text("--body")
	if err != nil {
		return err
	}
	if !given || strings.TrimSpace(body) == "" {
		return usagef("say what to post: --body TEXT or --body-file F")
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.src.Comment(t.url, body); err != nil {
		return err
	}
	logEvent(t.db, "ticket-comment", t.url, elide(body, 80))
	note("commented on %s", shortTickets([]string{t.url}))
	return nil
}

func cmdTicketClose(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--comment"}, []string{"--done", "--not-planned"}, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	if o.flags["--done"] == o.flags["--not-planned"] {
		return usagef("say how: --done (with its evidence) or --not-planned (abandoned)")
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.src.Close(t.url, o.flags["--not-planned"], o.str("--comment")); err != nil {
		return err
	}
	how := "done"
	if o.flags["--not-planned"] {
		how = "not planned"
	}
	logEvent(t.db, "ticket-closed", t.url, how)
	note("closed %s as %s", shortTickets([]string{t.url}), how)
	return nil
}

// cmdTicketDefer takes a ticket off the work for now. It is not closed: a
// closed ticket is never plannable again, and this one is meant to be.
func cmdTicketDefer(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--comment", "--comment-file"}, nil, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	comment, _, err := o.text("--comment")
	if err != nil {
		return err
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if t.w == nil {
		return fmt.Errorf("defer moves a ticket on a workspace's board; name the workspace with --ws")
	}
	is, err := t.src.Issue(t.url)
	if err != nil {
		return err
	}
	if is.State != "OPEN" {
		return fmt.Errorf("%s is %s; reopen it first if it should come back", shortTickets([]string{t.url}), strings.ToLower(is.State))
	}
	items, err := t.src.Find([]string{t.url})
	if err != nil {
		return err
	}
	backlog := t.w.Source.Backlog
	if err := t.src.SetStatus(items, backlog); err != nil {
		return err
	}
	logEvent(t.db, "ticket-deferred", t.url, elide(comment, 80))
	note("%s is back in %q", shortTickets([]string{t.url}), backlog)
	if strings.TrimSpace(comment) != "" {
		if err := t.src.Comment(t.url, comment); err != nil {
			return fmt.Errorf("moved to %q, but the comment was not posted: %w", backlog, err)
		}
	}
	if held, err := proposals(t.db, activeStates); err == nil {
		for _, p := range held {
			if contains(p.Tickets, t.url) {
				note("%s still holds it (%s): ais flow cancel %s, or queue cancel if it is only queued", p.ID, p.State, p.ID)
			}
		}
	}
	return nil
}

func cmdTicketNew(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--title", "--body", "--body-file", "--parent", "--repo", "--blocked-by", "--type"},
		nil, map[string]string{"-w": "--ws", "-t": "--title", "-b": "--body", "-F": "--body-file", "-R": "--repo"})
	if err != nil {
		return err
	}
	n := NewIssue{Title: strings.TrimSpace(o.str("--title")), Repo: o.str("--repo"), Type: o.str("--type")}
	if n.Title == "" {
		return usagef("--title is required")
	}
	if n.Body, _, err = o.text("--body"); err != nil {
		return err
	}
	if v := o.str("--parent"); v != "" {
		if n.Parent, err = ticketURL(v); err != nil {
			return err
		}
	}
	for _, ref := range o.list("--blocked-by") {
		u, err := ticketURL(ref)
		if err != nil {
			return err
		}
		n.BlockedBy = append(n.BlockedBy, u)
	}
	t, err := openTicketCmd(o, false)
	if err != nil {
		return err
	}
	defer t.close()
	url, err := t.src.Create(n)
	if url != "" {
		logEvent(t.db, "ticket-created", url, n.Title)
		fmt.Fprintln(stdout, url)
	}
	return err
}
