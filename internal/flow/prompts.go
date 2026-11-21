package flow

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/i7c/ais/internal/ais"
)

// The prompts every role is started with. They are built into ais, and
// versioned with the commands they call, so a dispatched session works on
// any machine ais is installed on: no skill has to be there. A workspace adds
// to them with instructions, or replaces one with prompt_file.

//go:embed prompts/*.md
var promptFS embed.FS

var roles = []string{"planner", "work", "refine"}

var levelRank = map[string]int{"L1": 1, "L2": 2, "L3": 3}

// minLevel is the lower of two levels; "" counts as no limit.
func minLevel(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case levelRank[a] <= levelRank[b]:
		return a
	}
	return b
}

func builtinPrompt(name string) string {
	data, err := promptFS.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic("ais flow: no built-in prompt " + name)
	}
	return string(data)
}

var versionRE = regexp.MustCompile(`^<!-- ais-prompt: (\w+) (v\d+) -->`)

// promptVersion is the "<!-- ais-prompt: work v1 -->" a prompt starts with.
func promptVersion(text string) (role, version string) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// promptData is what a prompt template can use.
type promptData struct {
	Workspace    string
	Proposal     string
	Tickets      []string
	TicketsText  string
	Ticket       string // the refine role's one ticket
	Repos        []repoView
	Notes        string
	Ceiling      string
	Assessed     string
	ReadyStatus  string
	RefineStatus string
	MaxRounds    int
	Rubric       string
	Instructions string
}

type repoView struct{ Name, Path, Branch string }

// rolePrompt is the role's configuration in w (nil: no workspace).
func roleConf(w *Workspace, role string) RoleConf {
	if w == nil {
		return RoleConf{}
	}
	switch role {
	case "planner":
		return w.Planner.RoleConf
	case "work":
		return w.Session.RoleConf
	case "refine":
		return w.Refine.RoleConf
	}
	return RoleConf{}
}

// renderRole builds the prompt a role starts with, and any warning about
// it (an override written against an older built-in prompt).
func renderRole(w *Workspace, role string, d promptData) (string, string, error) {
	rc := roleConf(w, role)
	if w != nil {
		d.Workspace = orDefault(d.Workspace, w.Name)
		d.ReadyStatus, d.RefineStatus, d.MaxRounds = w.Source.Ready, w.Refine.Status, w.Refine.MaxRounds
		d.Instructions = strings.TrimSpace(strings.Join(nonEmpty(w.Instructions, rc.Instructions), "\n\n"))
	} else {
		d.ReadyStatus, d.RefineStatus, d.MaxRounds = "ready", "refinement", 3
	}
	d.Rubric = strings.TrimSpace(stripVersion(builtinPrompt("rubric")))
	if d.Ceiling == "" {
		d.Ceiling = "L2"
	}
	if d.TicketsText == "" {
		d.TicketsText = strings.Join(shortTickets2(d.Tickets), ", ")
	}
	// A literal prompt from config: the way it was before prompts were built
	// in (a skill named in it, say). Only the work role's has placeholders.
	if rc.Prompt != "" {
		text := strings.ReplaceAll(rc.Prompt, "{tickets}", strings.Join(d.Tickets, " "))
		text = strings.ReplaceAll(text, "{notes}", d.Notes)
		text = strings.ReplaceAll(text, "{autonomy}", d.Ceiling)
		return strings.TrimRight(text, " \n"), "", nil
	}
	text, warning := builtinPrompt(role), ""
	if rc.PromptFile != "" {
		data, err := os.ReadFile(rc.PromptFile)
		if err != nil {
			return "", "", fmt.Errorf("%s prompt_file: %w", role, err)
		}
		text = string(data)
		_, theirs := promptVersion(text)
		_, ours := promptVersion(builtinPrompt(role))
		switch {
		case theirs == "":
			warning = fmt.Sprintf("%s does not say which built-in %s prompt it replaces; start it with <!-- ais-prompt: %s %s -->", ais.Tilde(rc.PromptFile), role, role, ours)
		case theirs != ours:
			warning = fmt.Sprintf("%s replaces the %s prompt %s, but the built-in one is %s now: ais flow prompt show %s --builtin shows it", ais.Tilde(rc.PromptFile), role, theirs, ours, role)
		}
	}
	tmpl, err := template.New(role).Option("missingkey=error").Parse(stripVersion(text))
	if err != nil {
		return "", warning, fmt.Errorf("%s prompt: %w", role, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, d); err != nil {
		return "", warning, fmt.Errorf("%s prompt: %w", role, err)
	}
	return strings.TrimSpace(b.String()), warning, nil
}

func stripVersion(text string) string {
	text = strings.TrimLeft(text, "\n")
	if versionRE.MatchString(text) {
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			return rest
		}
	}
	return text
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimSpace(x))
		}
	}
	return out
}

// workData is the work role's data for a proposal's tickets and repos: the
// ceiling from the proposal, else the board, else the workspace, and what
// refinement found.
func workData(w *Workspace, db *DB, p *Proposal) promptData {
	d := promptData{Proposal: p.ID, Tickets: p.Tickets, Notes: p.Notes}
	for _, name := range p.Repos {
		if path, ok := w.Repos[name]; ok {
			dir := filepath.Base(ais.RepoRoot(path))
			if dir == "." || dir == "" {
				dir = filepath.Base(path)
			}
			d.Repos = append(d.Repos, repoView{Name: name, Path: "./" + dir})
		}
	}
	d.Ceiling = p.Autonomy
	var assessed []string
	if db != nil {
		board := map[string]Item{}
		if d.Ceiling == "" {
			if items, err := sourceFor(w, db).Find(p.Tickets); err == nil {
				for _, it := range items {
					board[it.URL] = it
				}
			}
		}
		for _, t := range p.Tickets {
			if d.Ceiling == "" {
				if l := strings.ToUpper(board[t].Autonomy); levelRank[l] > 0 {
					d.Ceiling = minLevel(d.Ceiling, l) // tickets in one session: the strictest
				}
			}
			if a, err := assessmentOf(db, t); err == nil && a != nil && a.Level != "" {
				s := shortTickets([]string{t}) + " " + a.Level
				if a.Flag != "" {
					s += " (" + a.Flag + ": its questions are still open)"
				}
				assessed = append(assessed, s)
			}
		}
	}
	d.Ceiling = orDefault(d.Ceiling, w.Session.Autonomy)
	d.Assessed = strings.Join(assessed, ", ")
	return d
}

const promptUsage = `usage: ais flow prompt <command>

  show ROLE [--ws W] [--proposal ID | --tickets REF,...] [--ticket REF]
       [--autonomy L2|L3] [--builtin]
                   the prompt ROLE (planner, work, refine) starts with, as a
                   session in W would get it: the built-in one with the
                   workspace's instructions added, or its prompt_file.
                   --builtin prints the built-in text, unrendered.
  rubric           the autonomy levels and the readiness check
`

func cmdPrompt(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, promptUsage)
		return nil
	}
	switch args[0] {
	case "rubric":
		fmt.Fprintln(stdout, strings.TrimSpace(stripVersion(builtinPrompt("rubric"))))
		return nil
	case "show":
	default:
		return usagef("unknown prompt command %q; try show, rubric", args[0])
	}
	o, err := parseOpts(args[1:], []string{"--ws", "--proposal", "--tickets", "--ticket", "--autonomy", "--notes"},
		[]string{"--builtin"}, map[string]string{"-w": "--ws"})
	if err != nil {
		return err
	}
	if len(o.pos) != 1 || !contains(roles, o.pos[0]) {
		return usagef("show which prompt: %s", strings.Join(roles, ", "))
	}
	role := o.pos[0]
	if o.flags["--builtin"] {
		fmt.Fprint(stdout, builtinPrompt(role))
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w, err := ticketWorkspace(cfg, o.str("--ws"))
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	var d promptData
	switch role {
	case "work":
		p := &Proposal{Notes: o.str("--notes"), Autonomy: strings.ToUpper(o.str("--autonomy"))}
		if ref := o.str("--proposal"); ref != "" {
			if p, err = findProposal(db, ref); err != nil {
				return err
			}
			if w, err = cfg.workspace(p.Workspace); err != nil {
				return err
			}
		} else {
			for _, ref := range o.list("--tickets") {
				u, err := ticketURL(ref)
				if err != nil {
					return err
				}
				p.Tickets = append(p.Tickets, u)
			}
			if len(p.Tickets) == 0 {
				return usagef("whose work: --proposal ID or --tickets REF,...")
			}
		}
		if w != nil {
			d = workData(w, db, p)
		} else {
			d = promptData{Tickets: p.Tickets, Notes: p.Notes, Ceiling: orDefault(p.Autonomy, "L2")}
		}
	case "refine":
		d.Ticket = "<ticket>"
		if ref := o.str("--ticket"); ref != "" {
			if d.Ticket, err = ticketURL(ref); err != nil {
				return err
			}
		}
		if w != nil {
			d.Repos = workspaceRepos(w)
		}
	case "planner":
		if w == nil {
			return usagef("the planner prompt is per workspace: --ws W")
		}
	}
	text, warning, err := renderRole(w, role, d)
	if warning != "" {
		note("%s", warning)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, text)
	return nil
}

// workspaceRepos is every repo of w, for a role that reads rather than
// changes them: their main checkouts.
func workspaceRepos(w *Workspace) []repoView {
	var out []repoView
	for _, name := range repoNames(w) {
		out = append(out, repoView{Name: name, Path: ais.Tilde(w.Repos[name])})
	}
	return out
}
