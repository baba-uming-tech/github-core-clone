package main

import (
	"encoding/json"
	"strconv"
)

func itoa(n int) string {
	return strconv.Itoa(n)
}

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoJSON struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	Fork          bool     `json:"fork"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	PushedAt      string   `json:"pushed_at"`
	DefaultBranch string   `json:"default_branch"`
	Size          int      `json:"size"`
	URL           string   `json:"url"`
	HTMLURL       string   `json:"html_url"`
	GitURL        string   `json:"git_url"`
	CloneURL      string   `json:"clone_url"`
}

type labelJSON struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Description *string `json:"description"`
}

type issueJSON struct {
	ID         int         `json:"id"`
	NodeID     string      `json:"node_id"`
	Number     int         `json:"number"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	State      string      `json:"state"`
	StateReason *string    `json:"state_reason"`
	Locked     bool        `json:"locked"`
	Labels     []labelJSON `json:"labels"`
	User       userJSON    `json:"user"`
	Assignee   any         `json:"assignee"`
	Assignees  []userJSON  `json:"assignees"`
	Comments   int         `json:"comments"`
	CreatedAt  string      `json:"created_at"`
	UpdatedAt  string      `json:"updated_at"`
	ClosedAt   *string     `json:"closed_at"`
	HTMLURL    string      `json:"html_url"`
	URL        string      `json:"url"`
}

type commentJSON struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type pullRefJSON struct {
	Label string   `json:"label"`
	Ref   string   `json:"ref"`
	Sha   string   `json:"sha"`
	User  userJSON `json:"user"`
}

type pullJSON struct {
	ID             int         `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	State          string      `json:"state"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	User           userJSON    `json:"user"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Mergeable      bool        `json:"mergeable"`
	MergeableState string      `json:"mergeable_state"`
	Draft          bool        `json:"draft"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	ClosedAt       *string     `json:"closed_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
}

func encodeJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func UserJSON(store *Store) []byte {
	return encodeJSON(userJSON{Login: store.Login, ID: 1000, Type: "User"})
}

func RepoJSON(repo *Repo) []byte {
	full := repo.Owner + "/" + repo.Name
	return encodeJSON(repoJSON{
		ID:            repo.ID,
		NodeID:        "repo:" + itoa(repo.ID),
		Name:          repo.Name,
		FullName:      full,
		Owner:         userJSON{Login: repo.Owner, ID: 1000, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		Fork:          false,
		CreatedAt:     repo.CreatedAt,
		UpdatedAt:     repo.CreatedAt,
		PushedAt:      repo.CreatedAt,
		DefaultBranch: defaultBranch(repo),
		Size:          0,
		URL:           "/api/v3/repos/" + full,
		HTMLURL:       "/" + full,
		GitURL:        "/api/v3/repos/" + full + ".git",
		CloneURL:      "/repos/" + full + ".git",
	})
}

func LabelJSON(label *Label) []byte {
	return encodeJSON(labelJSON{
		ID:          label.ID,
		Name:        label.Name,
		Color:       label.Color,
		Description: label.Description,
	})
}

func IssueJSON(repo *Repo, store *Store, issue *Issue) []byte {
	full := repo.Owner + "/" + repo.Name
	labels := make([]labelJSON, 0, len(issue.Labels))
	for i := range issue.Labels {
		labels = append(labels, labelJSON{
			ID:          issue.Labels[i].ID,
			Name:        issue.Labels[i].Name,
			Color:       issue.Labels[i].Color,
			Description: issue.Labels[i].Description,
		})
	}
	return encodeJSON(issueJSON{
		ID:          issue.ID,
		NodeID:      "issue:" + itoa(issue.ID),
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		State:       issue.State,
		StateReason: issue.StateReason,
		Locked:      false,
		Labels:      labels,
		User:        userJSON{Login: store.Login, ID: 1000, Type: "User"},
		Assignee:    nil,
		Assignees:   make([]userJSON, 0),
		Comments:    len(issue.Comments),
		CreatedAt:   issue.CreatedAt,
		UpdatedAt:   issue.UpdatedAt,
		ClosedAt:    issue.ClosedAt,
		HTMLURL:     "/" + full + "/issues/" + itoa(issue.Number),
		URL:         "/api/v3/repos/" + full + "/issues/" + itoa(issue.Number),
	})
}

func CachedIssueJSON(repo *Repo, store *Store, issue *Issue) []byte {
	if issue.JSONCache == nil || issue.JSONCacheVersion != issue.Version {
		issue.JSONCache = IssueJSON(repo, store, issue)
		issue.JSONCacheVersion = issue.Version
	}
	return issue.JSONCache
}

func CommentJSON(store *Store, comment *Comment) []byte {
	return encodeJSON(commentJSON{
		ID:        comment.ID,
		Body:      comment.Body,
		User:      userJSON{Login: store.Login, ID: 1000, Type: "User"},
		CreatedAt: comment.CreatedAt,
		UpdatedAt: comment.UpdatedAt,
	})
}

func PullJSON(repo *Repo, store *Store, pull *Pull) []byte {
	full := repo.Owner + "/" + repo.Name
	user := userJSON{Login: store.Login, ID: 1000, Type: "User"}
	return encodeJSON(pullJSON{
		ID:     pull.ID,
		NodeID: "pull:" + itoa(pull.ID),
		Number: pull.Number,
		State:  pull.State,
		Title:  pull.Title,
		Body:   pull.Body,
		User:   user,
		Head: pullRefJSON{
			Label: pull.HeadOwner + ":" + pull.HeadRef,
			Ref:   pull.HeadRef,
			Sha:   pull.HeadSha,
			User:  user,
		},
		Base: pullRefJSON{
			Label: pull.BaseOwner + ":" + pull.BaseRef,
			Ref:   pull.BaseRef,
			Sha:   pull.BaseSha,
			User:  user,
		},
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSha: pull.MergeCommitSha,
		Mergeable:      true,
		MergeableState: "clean",
		Draft:          pull.Draft,
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.UpdatedAt,
		ClosedAt:       pull.ClosedAt,
		HTMLURL:        "/" + full + "/pull/" + itoa(pull.Number),
		URL:            "/api/v3/repos/" + full + "/pulls/" + itoa(pull.Number),
	})
}

func defaultBranch(repo *Repo) string {
	if repo.DefaultBranch == "" {
		return "main"
	}
	return repo.DefaultBranch
}
