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
var ghRunner = func(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
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

// project is a GitHub Projects (v2) board as a workspace's source.
type project struct {
	w  *Workspace
	db *DB
}

func sourceFor(w *Workspace, db *DB) *project { return &project{w: w, db: db} }

func (p *project) num() string { return strconv.Itoa(p.w.Source.Project) }

// Items lists every issue on the board with its status.
func (p *project) Items() ([]Item, error) {
	out, err := ghRunner("project", "item-list", p.num(), "--owner", p.w.Source.Owner, "--format", "json", "--limit", "1000")
	if err != nil {
		return nil, err
	}
	var data struct {
		Items []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Title   string `json:"title"`
			Content struct {
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
			Title: it.Title, Status: it.Status, ItemID: it.ID,
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
// the board is an error, since its status could not be told.
func (p *project) Find(urls []string) ([]Item, error) {
	items, err := p.Items()
	if err != nil {
		return nil, err
	}
	byURL := map[string]Item{}
	for _, it := range items {
		byURL[it.URL] = it
	}
	var out []Item
	for _, u := range urls {
		it, ok := byURL[u]
		if !ok {
			return nil, fmt.Errorf("%s is not on project %s/%d", u, p.w.Source.Owner, p.w.Source.Project)
		}
		out = append(out, it)
	}
	return out, nil
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

// Comment leaves a note on a ticket.
func (p *project) Comment(url, body string) error {
	_, err := ghRunner("issue", "comment", url, "--body", body)
	return err
}
