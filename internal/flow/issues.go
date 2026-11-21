package flow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Issue is one ticket as the task layer shows it to an agent.
type Issue struct {
	URL         string    `json:"url"`
	Repo        string    `json:"repo"` // owner/name
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"` // OPEN or CLOSED
	StateReason string    `json:"state_reason,omitempty"`
	Type        string    `json:"type,omitempty"`
	Labels      []string  `json:"labels"`
	Comments    []Comment `json:"comments"`
	Parent      *Ref      `json:"parent,omitempty"`
	SubIssues   []Ref     `json:"sub_issues"`
	BlockedBy   []Ref     `json:"blocked_by"`
	ClosedBy    []Ref     `json:"closed_by_prs"` // pull requests that close it when merged
}

// Ref is another ticket or pull request, as much as a list needs.
type Ref struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	State string `json:"state,omitempty"`
}

type Comment struct {
	Author string `json:"author"`
	At     string `json:"at"`
	Body   string `json:"body"`
}

// NewIssue is a ticket to create: in Repo, or the parent's repo.
type NewIssue struct {
	Repo      string
	Title     string
	Body      string
	Parent    string   // ticket URL
	BlockedBy []string // ticket URLs
	Type      string   // the tracker's issue type, if it has them
}

// githubIssues is tickets as GitHub issues, through gh.
type githubIssues struct{}

const issueFields = "url,number,title,body,state,stateReason,issueType,labels,comments,parent,subIssues,blockedBy,closedByPullRequestsReferences"

func (githubIssues) Issue(url string) (*Issue, error) {
	out, err := ghRunner("issue", "view", url, "--json", issueFields)
	if err != nil {
		return nil, err
	}
	var data struct {
		URL         string `json:"url"`
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		State       string `json:"state"`
		StateReason string `json:"stateReason"`
		IssueType   *struct {
			Name string `json:"name"`
		} `json:"issueType"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt string `json:"createdAt"`
			Body      string `json:"body"`
		} `json:"comments"`
		Parent    *Ref `json:"parent"`
		SubIssues struct {
			Nodes []Ref `json:"nodes"`
		} `json:"subIssues"`
		BlockedBy struct {
			Nodes []Ref `json:"nodes"`
		} `json:"blockedBy"`
		ClosedBy []Ref `json:"closedByPullRequestsReferences"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("gh issue view %s: %w", url, err)
	}
	is := &Issue{URL: orDefault(data.URL, url), Number: data.Number, Title: data.Title, Body: data.Body,
		State: data.State, StateReason: data.StateReason, Parent: data.Parent,
		Labels: []string{}, Comments: []Comment{}, SubIssues: data.SubIssues.Nodes, BlockedBy: data.BlockedBy.Nodes,
		ClosedBy: data.ClosedBy}
	if u, err := ticketURL(is.URL); err == nil {
		is.URL = u
	}
	is.Repo, is.Number = repoOf(is.URL), orZero(data.Number, numberOf(is.URL))
	if data.IssueType != nil {
		is.Type = data.IssueType.Name
	}
	if is.Parent != nil && is.Parent.URL == "" {
		is.Parent = nil
	}
	for _, l := range data.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	for _, c := range data.Comments {
		is.Comments = append(is.Comments, Comment{Author: c.Author.Login, At: c.CreatedAt, Body: c.Body})
	}
	for _, list := range []*[]Ref{&is.SubIssues, &is.BlockedBy, &is.ClosedBy} {
		if *list == nil {
			*list = []Ref{}
		}
	}
	return is, nil
}

func (githubIssues) Comment(url, body string) error {
	_, err := ghRunner("issue", "comment", url, "--body", body)
	return err
}

func (githubIssues) SetBody(url, body string) error {
	_, err := ghRunnerIn(body, "issue", "edit", url, "--body-file", "-")
	return err
}

func (githubIssues) Close(url string, notPlanned bool, comment string) error {
	args := []string{"issue", "close", url, "--reason", "completed"}
	if notPlanned {
		args[4] = "not planned"
	}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	_, err := ghRunner(args...)
	return err
}

func (githubIssues) Create(n NewIssue) (string, error) {
	repo := n.Repo
	if repo == "" && n.Parent != "" {
		repo = repoOf(n.Parent)
	}
	if repo == "" {
		return "", fmt.Errorf("say which repo the ticket goes in (--repo owner/name), or give it a parent")
	}
	args := []string{"issue", "create", "-R", repo, "--title", n.Title, "--body-file", "-"}
	if n.Parent != "" {
		args = append(args, "--parent", n.Parent)
	}
	if len(n.BlockedBy) > 0 {
		args = append(args, "--blocked-by", strings.Join(n.BlockedBy, ","))
	}
	if n.Type != "" {
		args = append(args, "--type", n.Type)
	}
	out, err := ghRunnerIn(n.Body, args...)
	if err != nil {
		return "", err
	}
	// gh prints the new issue's URL last.
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", fmt.Errorf("gh issue create printed no URL")
	}
	return ticketURL(lines[len(lines)-1])
}

// repoOf is owner/name of an issue URL.
func repoOf(url string) string {
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	if !ok {
		return ""
	}
	repo, _, _ := strings.Cut(rest, "/issues/")
	repo, _, _ = strings.Cut(repo, "/pull/")
	return repo
}

func numberOf(url string) int {
	i := strings.LastIndex(url, "/")
	n, _ := strconv.Atoi(url[i+1:])
	return n
}

func orZero(n, fallback int) int {
	if n != 0 {
		return n
	}
	return fallback
}

// issueSource is the task layer for a ticket outside any workspace: the
// issue operations, with no board to tell.
type issueSource struct{ githubIssues }

func (issueSource) Items() ([]Item, error)         { return nil, nil }
func (issueSource) Ready() ([]Item, error)         { return nil, nil }
func (issueSource) HasStatus(string) (bool, error) { return false, nil }
func (issueSource) SetStatus(items []Item, _ string) error {
	if len(items) > 0 {
		return fmt.Errorf("this ticket is in no workspace, so it has no board status to set (--ws)")
	}
	return nil
}
func (issueSource) Find(urls []string) ([]Item, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("%s is in no workspace (--ws), so it has no board status", shortTickets(urls))
}
