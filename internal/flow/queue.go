package flow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/i7c/ais/internal/ais"
)

// Proposal is a session you accepted, waiting to be started or running.
type Proposal struct {
	ID           string   `json:"id"`
	Workspace    string   `json:"workspace"`
	Title        string   `json:"title"`
	Tickets      []string `json:"tickets"`
	Repos        []string `json:"repos"`
	Profile      string   `json:"profile"`
	Prompt       string   `json:"prompt"`
	CustomPrompt bool     `json:"custom_prompt"`
	Notes        string   `json:"notes"`
	Priority     int      `json:"priority"`
	After        []string `json:"after"`
	State        string   `json:"state"`
	PlannerID    string   `json:"planner_id"`
	CreatedAt    string   `json:"created_at"`
	AisSession   string   `json:"ais_session"`
	StartedAt    string   `json:"started_at"`
	DoneAt       string   `json:"done_at"`
	Manual       bool     `json:"manual"`
}

const proposalCols = `id, workspace, title, tickets, repos, profile, prompt, custom_prompt, notes, priority,
	after, state, planner_id, created_at, ais_session, started_at, done_at, manual`

type scanner interface{ Scan(dest ...any) error }

func scanProposal(row scanner) (*Proposal, error) {
	var p Proposal
	var tickets, repos, after string
	if err := row.Scan(&p.ID, &p.Workspace, &p.Title, &tickets, &repos, &p.Profile, &p.Prompt, &p.CustomPrompt,
		&p.Notes, &p.Priority, &after, &p.State, &p.PlannerID, &p.CreatedAt, &p.AisSession, &p.StartedAt,
		&p.DoneAt, &p.Manual); err != nil {
		return nil, err
	}
	p.Tickets, p.Repos, p.After = fromJSON(tickets), fromJSON(repos), fromJSON(after)
	return &p, nil
}

// proposals lists proposals, highest priority first, then oldest.
func proposals(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]*Proposal, error) {
	query := `SELECT ` + proposalCols + ` FROM proposals`
	if where != "" {
		query += ` WHERE ` + where
	}
	rows, err := q.Query(query+` ORDER BY priority DESC, created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// findProposal is the one proposal whose id starts with ref.
func findProposal(db *DB, ref string) (*Proposal, error) {
	all, err := proposals(db, `id LIKE ? || '%'`, ref)
	if err != nil {
		return nil, err
	}
	switch len(all) {
	case 0:
		return nil, fmt.Errorf("no proposal with id starting %q", ref)
	case 1:
		return all[0], nil
	}
	return nil, fmt.Errorf("%q matches %d proposals", ref, len(all))
}

// active is the states in which a proposal holds its tickets.
const activeStates = `state IN ('queued', 'starting', 'running')`

func renderPrompt(w *Workspace, tickets []string, notes string) string {
	text := strings.ReplaceAll(w.Session.Prompt, "{tickets}", strings.Join(tickets, " "))
	text = strings.ReplaceAll(text, "{notes}", notes)
	return strings.TrimRight(text, " \n")
}

func cmdQueueAdd(args []string) error {
	o, err := parseOpts(args,
		[]string{"--ws", "--title", "--tickets", "--repos", "--profile", "--priority", "--after", "--notes", "--notes-file", "--prompt", "--prompt-file"},
		nil, map[string]string{"-w": "--ws", "-t": "--title"})
	if err != nil {
		return err
	}
	if len(o.pos) > 0 {
		return usagef("unexpected %q", o.pos[0])
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w, err := cfg.workspace(o.str("--ws"))
	if err != nil {
		return err
	}
	title := strings.TrimSpace(o.str("--title"))
	if title == "" {
		return usagef("--title is required: what the session is for")
	}
	var tickets []string
	for _, ref := range o.list("--tickets") {
		u, err := ticketURL(ref)
		if err != nil {
			return err
		}
		if !contains(tickets, u) {
			tickets = append(tickets, u)
		}
	}
	if len(tickets) == 0 {
		return usagef("--tickets is required: the tickets this session works on")
	}
	repos, err := checkRepos(w, o.list("--repos"))
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return usagef("--repos is required: the workspace repos it works in (%s)", strings.Join(repoNames(w), ", "))
	}
	profile := orDefault(o.str("--profile"), w.Profile)
	if ok, err := ais.TakesPrompt(cfg.raw, profile); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("profile %s takes no first prompt, so flow cannot start it", profile)
	}
	priority := 0
	if v, ok := o.one("--priority"); ok {
		if priority, err = strconv.Atoi(v); err != nil {
			return usagef("--priority must be a whole number")
		}
	}
	notes, _, err := o.text("--notes")
	if err != nil {
		return err
	}
	prompt, custom, err := o.text("--prompt")
	if err != nil {
		return err
	}
	if !custom {
		prompt = renderPrompt(w, tickets, notes)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	after, err := resolveAfter(db, o.list("--after"))
	if err != nil {
		return err
	}
	src := sourceFor(w, db)
	items, err := src.Find(tickets)
	if err != nil {
		return err
	}

	p := &Proposal{ID: newID("p"), Workspace: w.Name, Title: title, Tickets: tickets, Repos: repos,
		Profile: profile, Prompt: prompt, CustomPrompt: custom, Notes: notes, Priority: priority,
		After: after, State: "queued", PlannerID: plannerFromEnv(), CreatedAt: stamp(now())}
	err = db.tx(func(tx *sql.Tx) error {
		// A ticket is worked on by one session at a time.
		held, err := proposals(tx, activeStates)
		if err != nil {
			return err
		}
		for _, other := range held {
			for _, t := range tickets {
				if contains(other.Tickets, t) {
					return fmt.Errorf("%s is already in %s (%s, %s)", t, other.ID, other.Title, other.State)
				}
			}
		}
		_, err = tx.Exec(`INSERT INTO proposals (`+proposalCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Workspace, p.Title, toJSON(p.Tickets), toJSON(p.Repos), p.Profile, p.Prompt, p.CustomPrompt,
			p.Notes, p.Priority, toJSON(p.After), p.State, p.PlannerID, p.CreatedAt, "", "", "", false)
		if err == nil {
			logEvent(tx, "queued", p.ID, map[string]any{"title": p.Title, "tickets": p.Tickets, "planner": p.PlannerID})
		}
		return err
	})
	if err != nil {
		return err
	}
	// The board is told after the queue holds it; if the board refuses, the
	// proposal goes again, so the two never disagree for long.
	if err := src.SetStatus(items, w.Source.Scheduled); err != nil {
		db.Exec(`DELETE FROM proposals WHERE id = ?`, p.ID)
		logEvent(db, "queue-failed", p.ID, err.Error())
		return err
	}
	if w.Source.Comment {
		for _, t := range tickets {
			if err := src.Comment(t, fmt.Sprintf("Scheduled for a session by `ais flow` (%s): %s", p.ID, p.Title)); err != nil {
				logEvent(db, "comment-failed", p.ID, err.Error())
			}
		}
	}
	fmt.Fprintln(stdout, p.ID)
	return nil
}

func plannerFromEnv() string { return getenv("AIS_FLOW_PLANNER") }

// checkRepos keeps the names the workspace knows, in order, refusing others.
func checkRepos(w *Workspace, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if _, ok := w.Repos[n]; !ok {
			return nil, fmt.Errorf("workspace %s has no repo %q (it has %s)", w.Name, n, strings.Join(repoNames(w), ", "))
		}
		if !contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func repoNames(w *Workspace) []string {
	var out []string
	for n := range w.Repos {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resolveAfter accepts proposal ids (or unique prefixes) and tickets.
func resolveAfter(db *DB, refs []string) ([]string, error) {
	out := []string{}
	for _, ref := range refs {
		if u, err := ticketURL(ref); err == nil {
			out = append(out, u)
			continue
		}
		p, err := findProposal(db, ref)
		if err != nil {
			return nil, fmt.Errorf("--after %s: %w", ref, err)
		}
		out = append(out, p.ID)
	}
	return out, nil
}

func cmdQueueLs(args []string) error {
	o, err := parseOpts(args, []string{"--ws"}, []string{"--all", "--json"}, map[string]string{"-a": "--all", "-w": "--ws"})
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	where, qargs := activeStates, []any{}
	if o.flags["--all"] {
		where = "1"
	}
	if ws := o.str("--ws"); ws != "" {
		where += " AND workspace = ?"
		qargs = append(qargs, ws)
	}
	all, err := proposals(db, where, qargs...)
	if err != nil {
		return err
	}
	if o.flags["--json"] {
		if all == nil {
			all = []*Proposal{}
		}
		return writeJSON(all)
	}
	if len(all) == 0 {
		note("the queue is empty")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRI\tSTATE\tWS\tTITLE\tTICKETS\tREPOS\tAFTER")
	for _, p := range all {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Priority, p.State, p.Workspace,
			elide(p.Title, 40), shortTickets(p.Tickets), strings.Join(p.Repos, ","), orDefault(strings.Join(shortTickets2(p.After), ","), "-"))
	}
	return tw.Flush()
}

func shortTickets(urls []string) string { return strings.Join(shortTickets2(urls), " ") }

// shortTickets2 writes issue URLs as owner/repo#n, the way people say them.
func shortTickets2(refs []string) []string {
	out := make([]string, len(refs))
	for i, u := range refs {
		out[i] = u
		if rest, ok := strings.CutPrefix(u, "https://github.com/"); ok {
			if repo, n, ok := strings.Cut(rest, "/issues/"); ok {
				out[i] = repo + "#" + n
			}
		}
	}
	return out
}

func cmdQueueShow(args []string) error {
	o, err := parseOpts(args, nil, []string{"--json"}, nil)
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return usagef("queue show takes one proposal id")
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := findProposal(db, o.pos[0])
	if err != nil {
		return err
	}
	if o.flags["--json"] {
		return writeJSON(p)
	}
	for _, kv := range [][2]string{
		{"id", p.ID}, {"title", p.Title}, {"workspace", p.Workspace}, {"state", p.State},
		{"priority", strconv.Itoa(p.Priority)}, {"profile", p.Profile},
		{"tickets", strings.Join(shortTickets2(p.Tickets), " ")}, {"repos", strings.Join(p.Repos, " ")},
		{"after", orDefault(strings.Join(shortTickets2(p.After), " "), "-")},
		{"planner", orDefault(p.PlannerID, "-")}, {"created", p.CreatedAt},
		{"session", orDefault(p.AisSession, "-")}, {"started", orDefault(p.StartedAt, "-")},
		{"done", orDefault(p.DoneAt, "-")},
	} {
		fmt.Fprintf(stdout, "%9s  %s\n", kv[0], kv[1])
	}
	if p.Notes != "" {
		fmt.Fprintf(stdout, "\nnotes:\n%s\n", p.Notes)
	}
	how := "from the workspace template"
	if p.CustomPrompt {
		how = "written by hand"
	}
	fmt.Fprintf(stdout, "\nprompt (%s):\n%s\n", how, p.Prompt)
	return nil
}

func cmdQueueEdit(args []string) error {
	o, err := parseOpts(args,
		[]string{"--title", "--priority", "--repos", "--after", "--notes", "--notes-file", "--prompt", "--prompt-file", "--profile"},
		nil, map[string]string{"-t": "--title"})
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return usagef("queue edit takes one proposal id")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := findProposal(db, o.pos[0])
	if err != nil {
		return err
	}
	if p.State != "queued" {
		return fmt.Errorf("%s is %s; only a queued proposal can change", p.ID, p.State)
	}
	w, err := cfg.workspace(p.Workspace)
	if err != nil {
		return err
	}
	changed := []string{}
	if v, ok := o.one("--title"); ok && strings.TrimSpace(v) != "" {
		p.Title, changed = strings.TrimSpace(v), append(changed, "title")
	}
	if v, ok := o.one("--priority"); ok {
		if p.Priority, err = strconv.Atoi(v); err != nil {
			return usagef("--priority must be a whole number")
		}
		changed = append(changed, "priority")
	}
	if _, ok := o.vals["--repos"]; ok {
		if p.Repos, err = checkRepos(w, o.list("--repos")); err != nil {
			return err
		}
		if len(p.Repos) == 0 {
			return usagef("a session works in at least one repo")
		}
		changed = append(changed, "repos")
	}
	if _, ok := o.vals["--after"]; ok {
		if p.After, err = resolveAfter(db, o.list("--after")); err != nil {
			return err
		}
		changed = append(changed, "after")
	}
	if v, ok := o.one("--profile"); ok {
		if takes, err := ais.TakesPrompt(cfg.raw, v); err != nil {
			return err
		} else if !takes {
			return fmt.Errorf("profile %s takes no first prompt, so flow cannot start it", v)
		}
		p.Profile, changed = v, append(changed, "profile")
	}
	notes, notesGiven, err := o.text("--notes")
	if err != nil {
		return err
	}
	if notesGiven {
		p.Notes, changed = notes, append(changed, "notes")
	}
	prompt, promptGiven, err := o.text("--prompt")
	if err != nil {
		return err
	}
	switch {
	case promptGiven && prompt == "":
		// An empty prompt goes back to the template.
		p.CustomPrompt, p.Prompt, changed = false, renderPrompt(w, p.Tickets, p.Notes), append(changed, "prompt")
	case promptGiven:
		p.CustomPrompt, p.Prompt, changed = true, prompt, append(changed, "prompt")
	case notesGiven && !p.CustomPrompt:
		p.Prompt = renderPrompt(w, p.Tickets, p.Notes)
	}
	if len(changed) == 0 {
		return usagef("nothing to change")
	}
	err = db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET title=?, priority=?, repos=?, after=?, profile=?, notes=?, prompt=?, custom_prompt=?
			WHERE id=? AND state='queued'`, p.Title, p.Priority, toJSON(p.Repos), toJSON(p.After), p.Profile, p.Notes,
			p.Prompt, p.CustomPrompt, p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s stopped being queued while this ran", p.ID)
		}
		logEvent(tx, "edited", p.ID, changed)
		return nil
	})
	if err != nil {
		return err
	}
	note("%s: changed %s", p.ID, strings.Join(changed, ", "))
	return nil
}

func cmdQueueCancel(args []string) error {
	o, err := parseOpts(args, []string{"--reason"}, nil, nil)
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return usagef("queue cancel takes one proposal id")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := findProposal(db, o.pos[0])
	if err != nil {
		return err
	}
	if p.State != "queued" {
		return fmt.Errorf("%s is %s; only a queued proposal can be cancelled", p.ID, p.State)
	}
	err = db.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE proposals SET state='cancelled', done_at=? WHERE id=? AND state='queued'`, stamp(now()), p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s stopped being queued while this ran", p.ID)
		}
		logEvent(tx, "cancelled", p.ID, o.str("--reason"))
		return nil
	})
	if err != nil {
		return err
	}
	// Tickets still marked scheduled go back to the pile; one somebody moved
	// on by hand is theirs to keep.
	w, err := cfg.workspace(p.Workspace)
	if err != nil {
		return err
	}
	src := sourceFor(w, db)
	items, err := src.Find(p.Tickets)
	if err != nil {
		return fmt.Errorf("cancelled %s, but could not read its tickets back: %w", p.ID, err)
	}
	var back []Item
	for _, it := range items {
		if it.Status == w.Source.Scheduled {
			back = append(back, it)
		}
	}
	if err := src.SetStatus(back, w.Source.Ready); err != nil {
		return fmt.Errorf("cancelled %s, but: %w", p.ID, err)
	}
	note("cancelled %s; %d ticket(s) back to %s", p.ID, len(back), w.Source.Ready)
	return nil
}

func cmdReady(args []string) error {
	o, err := parseOpts(args, []string{"--ws"}, []string{"--json"}, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	ws := o.str("--ws")
	if ws == "" && len(o.pos) == 1 {
		ws = o.pos[0]
	} else if len(o.pos) > 0 {
		return usagef("unexpected %q", o.pos[len(o.pos)-1])
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w, err := cfg.workspace(ws)
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	items, err := sourceFor(w, db).Ready()
	if err != nil {
		return err
	}
	if o.flags["--json"] {
		if items == nil {
			items = []Item{}
		}
		return writeJSON(items)
	}
	if len(items) == 0 {
		note("no tickets marked %s on %s/%d", w.Source.Ready, w.Source.Owner, w.Source.Project)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, it := range items {
		fmt.Fprintf(tw, "%s#%d\t%s\t%s\n", it.Repo, it.Number, elide(it.Title, 60), it.URL)
	}
	return tw.Flush()
}

func writeJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}
