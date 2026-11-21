package flow

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The refinement block: the part of a ticket's body ais flow owns. It sits
// at the end, between two HTML comments; everything above it is the ticket
// as people wrote it and is never rewritten. The opening comment carries the
// machine-readable state (the round log), so any tracker with a markdown body
// can hold it.

const (
	blockOpen  = "<!-- ais:refinement "
	blockClose = "<!-- /ais:refinement -->"
)

// Refinement is the block as read back from a ticket.
type Refinement struct {
	Rounds int        `json:"rounds"` // refinement rounds that went back to you (L1 outcomes)
	Log    []RoundLog `json:"log"`
	Text   string     `json:"text"` // the block as it stands, your answers included
}

type RoundLog struct {
	At        string `json:"at"`
	Level     string `json:"level"`
	Questions int    `json:"questions"`
	Flag      string `json:"flag,omitempty"`
}

var blockRE = regexp.MustCompile(`(?s)\n*` + regexp.QuoteMeta(blockOpen) + `(\{.*?\})\s*-->(.*?)` + regexp.QuoteMeta(blockClose) + `\s*$`)

// splitRefinement is the body without the block, and the block (nil when
// there is none, or it was damaged past reading).
func splitRefinement(body string) (string, *Refinement) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	m := blockRE.FindStringSubmatchIndex(body)
	if m == nil {
		return body, nil
	}
	var meta struct {
		Log []RoundLog `json:"log"`
	}
	if json.Unmarshal([]byte(body[m[2]:m[3]]), &meta) != nil {
		return body, nil
	}
	r := &Refinement{Log: meta.Log, Text: strings.TrimSpace(body[m[4]:m[5]])}
	if r.Log == nil {
		r.Log = []RoundLog{}
	}
	for _, l := range r.Log {
		if l.Level == "L1" && l.Flag == "" {
			r.Rounds++
		}
	}
	return strings.TrimRight(body[:m[0]], " \n"), r
}

// ticketVersion is what "the ticket changed" means: its title, its body and
// the block (your answers are in it), with the whitespace a tracker's editor
// may rewrite smoothed out. Comments are not part of it: flow writes some
// of its own.
func ticketVersion(title, body string, r *Refinement) string {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimRight(l, " \t")
		}
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
	h := sha256.New()
	h.Write([]byte(norm(title) + "\x00" + norm(body) + "\x00"))
	if r != nil {
		h.Write([]byte(norm(r.Text)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// RefineInput is what a refinement run hands "ticket assess" or "ticket
// refine": the findings as data, so flow renders the block and agents never
// edit a ticket's body themselves.
type RefineInput struct {
	Level     string      `json:"level"` // L1, L2 or L3
	Why       string      `json:"why"`   // one or two lines: why this level
	Criteria  []Criterion `json:"criteria"`
	Evidence  []Finding   `json:"evidence"`
	Decisions []Decision  `json:"decisions"` // answered questions, settled for good
	Questions []Question  `json:"questions"`
	Rewrite   []string    `json:"rewrite"` // parts of the ticket its author should rewrite
	Notes     []string    `json:"notes"`   // for the session that plans it: how, not what
}

type Criterion struct {
	ID       string `json:"id"` // R1..R7
	Name     string `json:"name"`
	Met      bool   `json:"met"`
	Evidence string `json:"evidence"`
}

type Finding struct {
	Text   string `json:"text"`
	Source string `json:"source"` // where it came from: path:line, URL, ticket
}

type Decision struct {
	Text  string `json:"text"`
	Round int    `json:"round"`
	By    string `json:"by"` // who decided: "you", a ticket, an ADR
}

type Question struct {
	Text    string `json:"text"`
	Default string `json:"default"`
	Weight  string `json:"weight"` // blocks, changes, detail
	Raised  string `json:"raised"` // the answer or evidence that raised it, from round 2 on
}

func readRefineInput(o opts) (*RefineInput, error) {
	text, given, err := o.file("--file")
	if err != nil {
		return nil, err
	}
	if !given {
		return nil, usagef("--file F is required: the findings as JSON (\"-\" for stdin); ais flow prompt show refine describes it")
	}
	var in RefineInput
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("--file: not the findings JSON: %w", err)
	}
	return &in, nil
}

// renderBlock writes the block for a round. limit is set when the soft limit
// turned an L1 into a ticket planned with its questions open.
func renderBlock(in *RefineInput, log []RoundLog, maxRounds int, limit bool) string {
	var b strings.Builder
	meta, _ := json.Marshal(map[string]any{"v": 1, "log": log})
	fmt.Fprintf(&b, "%s%s -->\n", blockOpen, meta)
	rounds := 0
	for _, l := range log {
		if l.Level == "L1" && l.Flag == "" {
			rounds++
		}
	}
	last := log[len(log)-1]
	head := fmt.Sprintf("## Refinement · %s · %s", last.Level, last.At)
	switch {
	case last.Level == "L1" && last.Flag == "":
		head = fmt.Sprintf("## Refinement · round %d of %d · L1 · %s", rounds, maxRounds, last.At)
	case rounds == 1:
		head += " · after 1 round"
	case rounds > 1:
		head += fmt.Sprintf(" · after %d rounds", rounds)
	}
	b.WriteString(head + "\n\n")
	if len(in.Questions) > 0 || len(in.Rewrite) > 0 {
		b.WriteString("_Written by `ais flow`. Answer after each `> Answer:`, or rewrite what it asks you to above this block; then move the ticket back to ready._\n\n")
	} else {
		b.WriteString("_Written by `ais flow`._\n\n")
	}
	if limit {
		fmt.Fprintf(&b, "**Refinement limit reached** (%d rounds): this ticket goes to planning with the questions below still open. Plan it, abandon it, or run `ais flow refine` for another round.\n\n", maxRounds)
	}
	if in.Why != "" {
		fmt.Fprintf(&b, "**Why %s:** %s\n\n", in.Level, strings.TrimSpace(in.Why))
	}
	if len(in.Criteria) > 0 {
		b.WriteString("**Readiness**\n")
		for _, c := range in.Criteria {
			mark := "✗"
			if c.Met {
				mark = "✓"
			}
			fmt.Fprintf(&b, "- %s %s %s — %s\n", mark, c.ID, c.Name, strings.TrimSpace(c.Evidence))
		}
		b.WriteString("\n")
	}
	if len(in.Evidence) > 0 {
		b.WriteString("**Evidence**\n")
		for _, f := range in.Evidence {
			fmt.Fprintf(&b, "- %s", strings.TrimSpace(f.Text))
			if f.Source != "" {
				fmt.Fprintf(&b, " (%s)", f.Source)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(in.Decisions) > 0 {
		b.WriteString("**Decisions**\n")
		for i, d := range in.Decisions {
			fmt.Fprintf(&b, "- D%d", i+1)
			if d.Round > 0 {
				fmt.Fprintf(&b, " (round %d)", d.Round)
			}
			fmt.Fprintf(&b, ": %s", strings.TrimSpace(d.Text))
			if d.By != "" {
				fmt.Fprintf(&b, " — %s", d.By)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(in.Questions) > 0 {
		if in.Level == "L1" && !limit {
			b.WriteString("**Questions**\n")
		} else {
			b.WriteString("**Open questions** (for the plan; they do not block it)\n")
		}
		for i, q := range in.Questions {
			fmt.Fprintf(&b, "%d. %s", i+1, strings.TrimSpace(q.Text))
			if q.Weight != "" {
				fmt.Fprintf(&b, " _[%s]_", q.Weight)
			}
			if q.Default != "" {
				fmt.Fprintf(&b, " *Default: %s*", strings.TrimSpace(q.Default))
			}
			if q.Raised != "" {
				fmt.Fprintf(&b, " (raised by: %s)", strings.TrimSpace(q.Raised))
			}
			b.WriteString("\n   > Answer:\n")
		}
		b.WriteString("\n")
	}
	if len(in.Rewrite) > 0 {
		b.WriteString("**Please rewrite**\n")
		for _, r := range in.Rewrite {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(r))
		}
		b.WriteString("\n")
	}
	if len(in.Notes) > 0 {
		b.WriteString("**Notes for planning**\n")
		for _, n := range in.Notes {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(n))
		}
		b.WriteString("\n")
	}
	b.WriteString("**Runs:**")
	for i, l := range log {
		sep := " ·"
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "%s %s %s", sep, l.At, l.Level)
		if l.Questions > 0 {
			fmt.Fprintf(&b, " (%d questions)", l.Questions)
		}
		if l.Flag != "" {
			fmt.Fprintf(&b, " [%s]", l.Flag)
		}
	}
	b.WriteString("\n" + blockClose)
	return b.String()
}

// cmdRefineNow queues a refinement run for one ticket at once, whatever
// its version, its board status or the round limit (it allows one round
// past it): "ais flow refine REF".
func cmdRefineNow(args []string) error {
	o, err := parseOpts(args, []string{"--ws"}, nil, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if t.w == nil {
		return fmt.Errorf("name the ticket's workspace (--ws)")
	}
	is, err := t.src.Issue(t.url)
	if err != nil {
		return err
	}
	if is.State != "OPEN" {
		return fmt.Errorf("%s is closed", shortTickets([]string{t.url}))
	}
	_, ref := splitRefinement(is.Body)
	p := &Proposal{ID: newID("r"), Workspace: t.w.Name, Title: "Refine " + shortTickets([]string{t.url}) + ": " + elide(is.Title, 50),
		Tickets: []string{t.url}, Repos: []string{}, Profile: t.w.Refine.Profile,
		Prompt: "(rendered when it starts: ais flow prompt show refine)", Kind: "refine", Priority: refinePriority,
		After: []string{}, State: "queued", CreatedAt: stamp(now()), Manual: true}
	err = t.db.tx(func(tx *sql.Tx) error {
		held, err := proposals(tx, activeStates)
		if err != nil {
			return err
		}
		for _, other := range held {
			if contains(other.Tickets, t.url) {
				return fmt.Errorf("%s is already held by %s (%s, %s)", shortTickets([]string{t.url}), other.ID, other.Title, other.State)
			}
		}
		if ref != nil && ref.Rounds >= t.w.Refine.MaxRounds {
			var extra int
			tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?`, "extra-rounds:"+t.url).Scan(&extra)
			if ref.Rounds >= t.w.Refine.MaxRounds+extra {
				tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, "extra-rounds:"+t.url, fmt.Sprint(ref.Rounds-t.w.Refine.MaxRounds+1))
			}
		}
		tx.Exec(`UPDATE assessments SET failed_at = '' WHERE url = ?`, t.url)
		if err := insertProposal(tx, p); err != nil {
			return err
		}
		logEvent(tx, "queued", p.ID, map[string]any{"title": p.Title, "tickets": p.Tickets, "kind": "refine", "by": "hand"})
		return nil
	})
	if err != nil {
		return err
	}
	note("queued %s; ais flow dispatch --now %s starts it at once", p.ID, p.ID)
	fmt.Fprintln(stdout, p.ID)
	return nil
}

// Assessment is flow's record of the last refinement outcome for a ticket.
type Assessment struct {
	URL      string `json:"url"`
	Level    string `json:"level"` // "" when the last run failed
	Version  string `json:"version"`
	Flag     string `json:"flag,omitempty"`
	At       string `json:"at"`
	FailedAt string `json:"failed_at,omitempty"`
	Current  bool   `json:"current"` // the ticket has not changed since
}

func assessmentOf(db *DB, url string) (*Assessment, error) {
	a := &Assessment{URL: url}
	err := db.QueryRow(`SELECT level, version, flag, at, failed_at FROM assessments WHERE url = ?`, url).
		Scan(&a.Level, &a.Version, &a.Flag, &a.At, &a.FailedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func saveAssessment(x execer, ws string, a *Assessment) error {
	_, err := x.Exec(`INSERT INTO assessments (url, workspace, level, version, flag, at, failed_at) VALUES (?,?,?,?,?,?,'')
		ON CONFLICT(url) DO UPDATE SET workspace=excluded.workspace, level=excluded.level, version=excluded.version,
		flag=excluded.flag, at=excluded.at, failed_at=''`, a.URL, ws, a.Level, a.Version, a.Flag, a.At)
	return err
}

func cmdTicketAssess(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--level", "--file"}, nil, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	in, err := readRefineInput(o)
	if err != nil {
		return err
	}
	level := strings.ToUpper(orDefault(o.str("--level"), in.Level))
	if level != "L2" && level != "L3" {
		return usagef("assess records L2 or L3; an L1 ticket goes back to its author: ais flow ticket refine")
	}
	in.Level = level
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if t.w == nil {
		return fmt.Errorf("assess needs the ticket's workspace (--ws)")
	}
	a, err := t.writeRound(in, false)
	if err != nil {
		return err
	}
	note("%s assessed %s; it stays %s for the planner", shortTickets([]string{t.url}), a.Level, t.w.Source.Ready)
	return nil
}

func cmdTicketRefine(args []string) error {
	o, err := parseOpts(args, []string{"--ws", "--file"}, nil, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	in, err := readRefineInput(o)
	if err != nil {
		return err
	}
	if in.Level != "" && strings.ToUpper(in.Level) != "L1" {
		return usagef("refine sends an L1 ticket back to its author; record L2 or L3 with ais flow ticket assess")
	}
	in.Level = "L1"
	if len(in.Questions) == 0 && len(in.Rewrite) == 0 {
		return usagef("an L1 outcome asks something: give questions or rewrite requests")
	}
	t, err := openTicketCmd(o, true)
	if err != nil {
		return err
	}
	defer t.close()
	if t.w == nil {
		return fmt.Errorf("refine needs the ticket's workspace (--ws)")
	}
	if ok, err := t.src.HasStatus(t.w.Refine.Status); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("the board has no %q status to move the ticket to; add the option, or set [workspace.%s.refine] status", t.w.Refine.Status, t.w.Name)
	}
	a, err := t.writeRound(in, true)
	if err != nil {
		return err
	}
	if a.Flag != "" {
		note("%s: refinement limit reached; recorded as %s with its questions open, it stays %s for the planner",
			shortTickets([]string{t.url}), a.Level, t.w.Source.Ready)
		return nil
	}
	note("%s moved to %s with %d question(s); it comes back when you move it to %s", shortTickets([]string{t.url}),
		t.w.Refine.Status, len(in.Questions), t.w.Source.Ready)
	return nil
}

// writeRound renders the block into the ticket, records the assessment for
// the version the tracker now holds, and for an L1 outcome under the round
// limit moves the ticket to the refinement status and says so in a comment.
func (t *ticketCmd) writeRound(in *RefineInput, l1 bool) (*Assessment, error) {
	is, err := t.src.Issue(t.url)
	if err != nil {
		return nil, err
	}
	body, old := splitRefinement(is.Body)
	var log []RoundLog
	if old != nil {
		log = old.Log
	}
	day := now().Local().Format(time.DateOnly)
	entry := RoundLog{At: day, Level: in.Level, Questions: len(in.Questions)}
	limit := false
	var extra int
	t.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?`, "extra-rounds:"+t.url).Scan(&extra)
	if l1 && old != nil && old.Rounds >= t.w.Refine.MaxRounds+extra {
		limit = true
		entry.Flag = "refinement-limit"
	}
	log = append(log, entry)
	block := renderBlock(in, log, t.w.Refine.MaxRounds, limit)
	if err := t.src.SetBody(t.url, strings.TrimRight(body, " \n")+"\n\n"+block+"\n"); err != nil {
		return nil, err
	}
	// The version is what the tracker gives back, not what was sent: an
	// editor or the API may smooth whitespace, and the next sweep compares
	// against what it reads.
	back, err := t.src.Issue(t.url)
	if err != nil {
		return nil, err
	}
	bbody, bref := splitRefinement(back.Body)
	a := &Assessment{URL: t.url, Level: in.Level, Version: ticketVersion(back.Title, bbody, bref), At: stamp(now())}
	if limit {
		a.Level, a.Flag = "L2", "refinement-limit"
	}
	if err := saveAssessment(t.db, t.w.Name, a); err != nil {
		return nil, err
	}
	logEvent(t.db, "assessed", t.url, map[string]any{"level": a.Level, "flag": a.Flag, "questions": len(in.Questions)})
	if !l1 || limit {
		// Plannable now: a ticket that was waiting on its author (refined
		// by hand with ais flow refine) goes back to ready.
		if items, err := t.src.Find([]string{t.url}); err == nil && len(items) == 1 && items[0].Status == t.w.Refine.Status {
			if err := t.src.SetStatus(items, t.w.Source.Ready); err != nil {
				return a, fmt.Errorf("recorded %s, but could not move the ticket back to %q: %w", a.Level, t.w.Source.Ready, err)
			}
		}
	}
	if l1 && !limit {
		items, err := t.src.Find([]string{t.url})
		if err == nil {
			err = t.src.SetStatus(items, t.w.Refine.Status)
		}
		if err != nil {
			return a, fmt.Errorf("wrote the questions, but could not move the ticket to %q: %w", t.w.Refine.Status, err)
		}
		rounds := 1
		if old != nil {
			rounds = old.Rounds + 1
		}
		msg := fmt.Sprintf("Refinement round %d of %d (`ais flow`): %d question(s)", rounds, t.w.Refine.MaxRounds, len(in.Questions))
		if len(in.Rewrite) > 0 {
			msg += fmt.Sprintf(", %d part(s) to rewrite", len(in.Rewrite))
		}
		msg += fmt.Sprintf(". Answer in the Refinement block of the description, then move the ticket back to `%s`.", t.w.Source.Ready)
		if err := t.src.Comment(t.url, msg); err != nil {
			logEvent(t.db, "comment-failed", t.url, err.Error())
		}
	}
	return a, nil
}
