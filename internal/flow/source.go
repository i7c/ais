package flow

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Item is a ticket as a source knows it.
type Item struct {
	URL    string `json:"url"`
	Repo   string `json:"repo"` // owner/name
	Number int    `json:"number"`
	Title  string `json:"title"`
	Status string `json:"status"`
	ItemID string `json:"item_id"` // the project item, for setting its status
	// Autonomy is the board's "Autonomy" field (L1/L2/L3), when it has one:
	// the highest level a session may work at.
	Autonomy string `json:"autonomy,omitempty"`
}

// ticketURL normalises a ticket reference — an issue URL or owner/repo#n —
// to the issue URL, the form flow stores.
func ticketURL(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if m := regexp.MustCompile(`^https://github\.com/([^/\s]+/[^/\s]+)/(?:issues|pull)/(\d+)/?$`).FindStringSubmatch(ref); m != nil {
		return "https://github.com/" + m[1] + "/issues/" + m[2], nil
	}
	if m := regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`).FindStringSubmatch(ref); m != nil {
		return "https://github.com/" + m[1] + "/issues/" + m[2], nil
	}
	return "", fmt.Errorf("%q is not a ticket: use its URL or owner/repo#number", ref)
}

// ghRunner runs gh and returns its stdout; a variable for tests.
var ghRunner = func(args ...string) ([]byte, error) { return ghRunnerIn("", args...) }

// ghRunnerIn runs gh with stdin, for the bodies it reads from "-".
var ghRunnerIn = func(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), msg)
	}
	return out.Bytes(), nil
}

// Source is the task layer: where a workspace's tickets live, how their
// status is told, and everything an agent does to a ticket. Agents reach it
// only through "ais flow ticket", never through the tracker's own tools, so
// a second source is one more implementation of this, and no prompt changes.
type Source interface {
	// The board.
	Items() ([]Item, error)
	Ready() ([]Item, error)
	Find(urls []string) ([]Item, error)
	SetStatus(items []Item, status string) error
	HasStatus(status string) (bool, error)
	// Tickets.
	Issue(url string) (*Issue, error)
	Comment(url, body string) error
	SetBody(url, body string) error
	Close(url string, notPlanned bool, comment string) error
	Create(n NewIssue) (string, error)
}

// project is a GitHub Projects (v2) board as a workspace's source; the
// tickets on it are GitHub issues.
type project struct {
	githubIssues
	w  *Workspace
	db *DB
}

func sourceFor(w *Workspace, db *DB) Source { return &project{w: w, db: db} }

func (p *project) num() string { return strconv.Itoa(p.w.Source.Project) }

// Items lists every issue on the board with its status.
func (p *project) Items() ([]Item, error) {
	out, err := ghRunner("project", "item-list", p.num(), "--owner", p.w.Source.Owner, "--format", "json", "--limit", "1000")
	if err != nil {
		return nil, err
	}
	var data struct {
		Items []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Autonomy string `json:"autonomy"` // an "Autonomy" field, if the board has one
			Title    string `json:"title"`
			Content  struct {
				Type       string `json:"type"`
				URL        string `json:"url"`
				Number     int    `json:"number"`
				Repository string `json:"repository"`
			} `json:"content"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("gh project item-list: %w", err)
	}
	var items []Item
	for _, it := range data.Items {
		if it.Content.Type != "Issue" {
			continue // draft items and pull requests are not tickets to plan
		}
		items = append(items, Item{
			URL: it.Content.URL, Repo: it.Content.Repository, Number: it.Content.Number,
			Title: it.Title, Status: it.Status, ItemID: it.ID, Autonomy: it.Autonomy,
		})
	}
	return items, nil
}

// Ready is the tickets whose status is the workspace's ready value.
func (p *project) Ready() ([]Item, error) {
	items, err := p.Items()
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range items {
		if it.Status == p.w.Source.Ready {
			out = append(out, it)
		}
	}
	return out, nil
}

// Find is the board's items for these ticket URLs, in order; a ticket not on
// the board is an error, since its status could not be told. It asks about
// each ticket rather than listing the board: agents read tickets one after
// the other, and GitHub's hourly limit is shared by everything you run.
func (p *project) Find(urls []string) ([]Item, error) {
	var out []Item
	for _, u := range urls {
		it, err := p.item(u)
		if err != nil {
			return nil, err
		}
		if it == nil {
			return nil, fmt.Errorf("%s is not on project %s/%d", u, p.w.Source.Owner, p.w.Source.Project)
		}
		out = append(out, *it)
	}
	return out, nil
}

const itemQuery = `query($url: URI!, $field: String!) { resource(url: $url) { ... on Issue {
  title
  projectItems(first: 50, includeArchived: false) { nodes {
    id
    project { number owner { ... on Organization { login } ... on User { login } } }
    status: fieldValueByName(name: $field) { ... on ProjectV2ItemFieldSingleSelectValue { name } }
    autonomy: fieldValueByName(name: "Autonomy") {
      ... on ProjectV2ItemFieldSingleSelectValue { name }
      ... on ProjectV2ItemFieldTextValue { text }
    }
  } }
} } }`

// item is the board's item for one issue, or nil when it is not on it.
func (p *project) item(url string) (*Item, error) {
	out, err := ghRunner("api", "graphql", "-f", "query="+itemQuery, "-f", "url="+url, "-f", "field="+p.w.Source.Field)
	if err != nil {
		return nil, err
	}
	var data struct {
		Data struct {
			Resource *struct {
				Title        string `json:"title"`
				ProjectItems struct {
					Nodes []struct {
						ID      string `json:"id"`
						Project struct {
							Number int `json:"number"`
							Owner  struct {
								Login string `json:"login"`
							} `json:"owner"`
						} `json:"project"`
						Status *struct {
							Name string `json:"name"`
						} `json:"status"`
						Autonomy *struct {
							Name string `json:"name"`
							Text string `json:"text"`
						} `json:"autonomy"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"resource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	if data.Data.Resource == nil {
		return nil, nil
	}
	for _, n := range data.Data.Resource.ProjectItems.Nodes {
		if n.Project.Number != p.w.Source.Project || !strings.EqualFold(n.Project.Owner.Login, p.w.Source.Owner) {
			continue
		}
		it := &Item{URL: url, Repo: repoOf(url), Number: numberOf(url), Title: data.Data.Resource.Title, ItemID: n.ID}
		if n.Status != nil {
			it.Status = n.Status.Name
		}
		if n.Autonomy != nil {
			it.Autonomy = orDefault(n.Autonomy.Name, n.Autonomy.Text)
		}
		return it, nil
	}
	return nil, nil
}

type ghIDs struct {
	Project string            `json:"project"`
	Field   string            `json:"field"`
	Options map[string]string `json:"options"` // status name -> option id
}

// ids resolves the board, Status field and option ids once, then caches them.
func (p *project) ids() (*ghIDs, error) {
	var cached string
	err := p.db.QueryRow(`SELECT data FROM gh_ids WHERE workspace = ?`, p.w.Name).Scan(&cached)
	if err == nil {
		var ids ghIDs
		if json.Unmarshal([]byte(cached), &ids) == nil && ids.Project != "" {
			return &ids, nil
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	out, err := ghRunner("project", "view", p.num(), "--owner", p.w.Source.Owner, "--format", "json")
	if err != nil {
		return nil, err
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &view); err != nil || view.ID == "" {
		return nil, fmt.Errorf("gh project view: no project id")
	}
	out, err = ghRunner("project", "field-list", p.num(), "--owner", p.w.Source.Owner, "--format", "json")
	if err != nil {
		return nil, err
	}
	var fields struct {
		Fields []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Options []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"options"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out, &fields); err != nil {
		return nil, fmt.Errorf("gh project field-list: %w", err)
	}
	ids := &ghIDs{Project: view.ID, Options: map[string]string{}}
	for _, f := range fields.Fields {
		if f.Name == p.w.Source.Field {
			ids.Field = f.ID
			for _, o := range f.Options {
				ids.Options[o.Name] = o.ID
			}
		}
	}
	if ids.Field == "" {
		return nil, fmt.Errorf("project %s/%d has no field %q", p.w.Source.Owner, p.w.Source.Project, p.w.Source.Field)
	}
	for _, want := range []string{p.w.Source.Ready, p.w.Source.Scheduled, p.w.Source.InProgress} {
		if ids.Options[want] == "" {
			return nil, fmt.Errorf("field %q of project %s/%d has no option %q", p.w.Source.Field, p.w.Source.Owner, p.w.Source.Project, want)
		}
	}
	p.db.Exec(`INSERT OR REPLACE INTO gh_ids (workspace, data) VALUES (?, ?)`, p.w.Name, toJSON(ids))
	return ids, nil
}

// SetStatus moves items to the named status.
func (p *project) SetStatus(items []Item, status string) error {
	ids, err := p.ids()
	if err != nil {
		return err
	}
	opt := ids.Options[status]
	if opt == "" {
		return fmt.Errorf("no status %q on the board", status)
	}
	for _, it := range items {
		if _, err := ghRunner("project", "item-edit", "--id", it.ItemID, "--project-id", ids.Project,
			"--field-id", ids.Field, "--single-select-option-id", opt); err != nil {
			return fmt.Errorf("setting %s to %q: %w", it.URL, status, err)
		}
	}
	return nil
}

// HasStatus is whether the board's status field has an option of this name.
func (p *project) HasStatus(status string) (bool, error) {
	ids, err := p.ids()
	if err != nil {
		return false, err
	}
	return ids.Options[status] != "", nil
}

// forgetIDs drops the cached board ids, so the next use reads them again:
// for an option added to the board since they were cached.
func (p *project) forgetIDs() { p.db.Exec(`DELETE FROM gh_ids WHERE workspace = ?`, p.w.Name) }

// Create opens a ticket and puts it on the board too: a board whose
// automation adds sub-issues already has it, and adding it again is a no-op.
func (p *project) Create(n NewIssue) (string, error) {
	url, err := p.githubIssues.Create(n)
	if err != nil {
		return "", err
	}
	if _, err := ghRunner("project", "item-add", p.num(), "--owner", p.w.Source.Owner, "--url", url); err != nil {
		return url, fmt.Errorf("created %s, but could not put it on the board: %w", url, err)
	}
	return url, nil
}
