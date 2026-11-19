package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (a *App) routeIssuesPulls(w http.ResponseWriter, r *http.Request, method string, rest []string, query string, body map[string]any, repo *Repo) bool {
	store := a.Store

	if len(rest) == 1 && rest[0] == "labels" {
		switch method {
		case http.MethodGet:
			labels := make([]*Label, 0, len(repo.Labels))
			for i := range repo.Labels {
				label := repo.Labels[i]
				labels = append(labels, &label)
			}
			sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
			out := make([]labelJSON, 0, len(labels))
			for _, label := range labels {
				out = append(out, labelJSON{ID: label.ID, Name: label.Name, Color: label.Color, Description: label.Description})
			}
			a.sendJSON(w, http.StatusOK, encodeJSON(out))
			return true
		case http.MethodPost:
			name, _ := body["name"].(string)
			if name == "" {
				a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
				return true
			}
			color, _ := body["color"].(string)
			if color == "" {
				color = "ededed"
			}
			color = strings.TrimPrefix(color, "#")
			label := Label{ID: store.NextLabelID, Name: name, Color: color, Description: nil}
			store.NextLabelID++
			repo.Labels[name] = label
			a.sendJSON(w, http.StatusCreated, LabelJSON(&label))
			return true
		}
		return false
	}

	if len(rest) == 2 && rest[0] == "labels" && method == http.MethodDelete {
		delete(repo.Labels, rest[1])
		a.sendJSON(w, http.StatusNoContent, nil)
		return true
	}

	if len(rest) == 1 && rest[0] == "issues" {
		switch method {
		case http.MethodPost:
			title, _ := body["title"].(string)
			if title == "" {
				a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
				return true
			}
			labels, ok := resolveIssueLabels(repo, body["labels"])
			if !ok {
				a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
				return true
			}
			issueBody, _ := body["body"].(string)
			ts := store.NowISO()
			repo.IssueSeq++
			store.NextIssueID++
			issue := Issue{
				ID:        store.NextIssueID - 1,
				Number:    repo.IssueSeq,
				Title:     title,
				Body:      issueBody,
				State:     "open",
				StateReason: nil,
				Labels:    labels,
				CreatedAt: ts,
				UpdatedAt: ts,
				ClosedAt:  nil,
				Comments:  []Comment{},
				Version:   0,
			}
			repo.Issues = append(repo.Issues, issue)
			added := &repo.Issues[len(repo.Issues)-1]
			added.Version++
			a.sendJSON(w, http.StatusCreated, IssueJSON(repo, store, added))
			return true
		case http.MethodGet:
			state := queryParam(query, "state")
			if state == "" {
				state = "open"
			}
			sortKey := queryParam(query, "sort")
			if sortKey == "" {
				sortKey = "created"
			}
			direction := queryParam(query, "direction")
			if direction == "" {
				direction = "desc"
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for i := range repo.Issues {
				if state == "all" || repo.Issues[i].State == state {
					list = append(list, &repo.Issues[i])
				}
			}
			sort.SliceStable(list, func(i, j int) bool {
				if sortKey == "updated" {
					return list[i].UpdatedAt < list[j].UpdatedAt
				}
				return list[i].Number < list[j].Number
			})
			if direction != "asc" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			items := make([]json.RawMessage, 0, len(list))
			for _, issue := range list {
				items = append(items, IssueJSON(repo, store, issue))
			}
			perPage, page := issuePagination(query)
			start, end := issuePageBounds(len(items), perPage, page)
			if start+perPage < len(items) {
				host := r.Host
				if host == "" {
					host = "localhost"
				}
				w.Header().Set("Link", "<http://"+host+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues?per_page="+itoa(perPage)+"&page="+itoa(page+1)+">; rel=\"next\"")
			}
			a.sendJSON(w, http.StatusOK, encodeJSON(items[start:end]))
			return true
		}
		return false
	}

	if len(rest) >= 2 && rest[0] == "issues" {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		issue := repo.FindIssue(number)
		if issue == nil {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		if len(rest) == 2 {
			switch method {
			case http.MethodGet:
				a.sendJSON(w, http.StatusOK, CachedIssueJSON(repo, store, issue))
				return true
			case http.MethodPatch:
				var labels []Label
				if value, exists := body["labels"]; exists {
					resolved, ok := resolveIssueLabels(repo, value)
					if !ok {
						a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
						return true
					}
					labels = resolved
					issue.Labels = labels
				}
				if title, ok := body["title"].(string); ok {
					issue.Title = title
				}
				if issueBody, ok := body["body"].(string); ok {
					issue.Body = issueBody
				}
				switch body["state"] {
				case "closed":
					issue.State = "closed"
					closedAt := store.NowISO()
					issue.ClosedAt = &closedAt
				case "open":
					issue.State = "open"
					issue.ClosedAt = nil
				}
				issue.UpdatedAt = store.NowISO()
				issue.Version++
				a.sendJSON(w, http.StatusOK, IssueJSON(repo, store, issue))
				return true
			}
			return false
		}
		if len(rest) == 3 && rest[2] == "comments" {
			switch method {
			case http.MethodGet:
				items := make([]json.RawMessage, 0, len(issue.Comments))
				for i := range issue.Comments {
					items = append(items, CommentJSON(store, &issue.Comments[i]))
				}
				a.sendJSON(w, http.StatusOK, encodeJSON(items))
				return true
			case http.MethodPost:
				commentBody, _ := body["body"].(string)
				ts := store.NowISO()
				comment := Comment{
					ID:        store.NextCommentID,
					Body:      commentBody,
					CreatedAt: ts,
					UpdatedAt: ts,
				}
				store.NextCommentID++
				issue.Comments = append(issue.Comments, comment)
				issue.UpdatedAt = ts
				issue.Version++
				a.sendJSON(w, http.StatusCreated, CommentJSON(store, &issue.Comments[len(issue.Comments)-1]))
				return true
			}
			return false
		}
		return false
	}

	if len(rest) == 1 && rest[0] == "pulls" {
		switch method {
		case http.MethodPost:
			headRaw, _ := body["head"].(string)
			baseRef, _ := body["base"].(string)
			headOwner := repo.Owner
			headRef := headRaw
			if colon := strings.IndexByte(headRaw, ':'); colon >= 0 {
				headOwner = headRaw[:colon]
				headRef = headRaw[colon+1:]
			}
			headSha := repo.Refs["refs/heads/"+headRef]
			baseSha := repo.Refs["refs/heads/"+baseRef]
			if headSha == "" || baseSha == "" {
				a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
				return true
			}
			for i := range repo.Pulls {
				existing := &repo.Pulls[i]
				if existing.State == "open" && existing.HeadRef == headRef && existing.BaseRef == baseRef {
					a.sendJSON(w, http.StatusCreated, PullJSON(repo, store, existing))
					return true
				}
			}
			pullTitle, _ := body["title"].(string)
			pullBody, _ := body["body"].(string)
			draft, _ := body["draft"].(bool)
			ts := store.NowISO()
			repo.PullSeq++
			store.NextPullID++
			pull := Pull{
				ID:        store.NextPullID - 1,
				Number:    repo.PullSeq,
				Title:     pullTitle,
				Body:      pullBody,
				State:     "open",
				HeadOwner: headOwner,
				HeadRef:   headRef,
				HeadSha:   headSha,
				BaseOwner: repo.Owner,
				BaseRef:   baseRef,
				BaseSha:   baseSha,
				Merged:    false,
				MergedAt:  nil,
				MergeCommitSha: nil,
				Draft:     draft,
				CreatedAt: ts,
				UpdatedAt: ts,
				ClosedAt:  nil,
			}
			repo.Pulls = append(repo.Pulls, pull)
			a.sendJSON(w, http.StatusCreated, PullJSON(repo, store, &repo.Pulls[len(repo.Pulls)-1]))
			return true
		case http.MethodGet:
			state := queryParam(query, "state")
			if state == "" {
				state = "open"
			}
			list := make([]*Pull, 0, len(repo.Pulls))
			for i := range repo.Pulls {
				if state == "all" || repo.Pulls[i].State == state {
					list = append(list, &repo.Pulls[i])
				}
			}
			items := make([]json.RawMessage, 0, len(list))
			for _, pull := range list {
				items = append(items, PullJSON(repo, store, pull))
			}
			perPage, page := issuePagination(query)
			start, end := issuePageBounds(len(items), perPage, page)
			a.sendJSON(w, http.StatusOK, encodeJSON(items[start:end]))
			return true
		}
		return false
	}

	if len(rest) >= 2 && rest[0] == "pulls" {
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		pull := repo.FindPull(number)
		if pull == nil {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		if len(rest) == 2 && method == http.MethodGet {
			a.sendJSON(w, http.StatusOK, PullJSON(repo, store, pull))
			return true
		}
		if len(rest) == 3 && rest[2] == "files" && method == http.MethodGet {
			files := PullChangedFiles(repo, pull)
			var items []json.RawMessage
			if err := json.Unmarshal(encodeJSON(files), &items); err != nil {
				items = []json.RawMessage{}
			}
			perPage, page := issuePagination(query)
			start, end := issuePageBounds(len(items), perPage, page)
			a.sendJSON(w, http.StatusOK, encodeJSON(items[start:end]))
			return true
		}
		if len(rest) == 3 && rest[2] == "merge" {
			switch method {
			case http.MethodGet:
				if pull.Merged {
					a.sendJSON(w, http.StatusNoContent, nil)
				} else {
					a.sendError(w, http.StatusNotFound, "Not Found")
				}
				return true
			case http.MethodPut:
				mergeMethod, _ := body["merge_method"].(string)
				result := MergePull(store, repo, pull, mergeMethod)
				if !result.OK {
					if result.Conflict {
						a.sendError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
					} else {
						a.sendError(w, http.StatusUnprocessableEntity, "Validation Failed")
					}
					return true
				}
				a.sendJSON(w, http.StatusOK, encodeJSON(struct {
					SHA     string `json:"sha"`
					Merged  bool   `json:"merged"`
					Message string `json:"message"`
				}{SHA: result.SHA, Merged: true, Message: "Pull Request successfully merged"}))
				return true
			}
			return false
		}
		return false
	}

	return false
}

func resolveIssueLabels(repo *Repo, value any) ([]Label, bool) {
	if value == nil {
		return []Label{}, true
	}
	var names []string
	switch list := value.(type) {
	case []string:
		names = list
	case []any:
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, false
			}
			names = append(names, name)
		}
	default:
		return nil, false
	}
	out := make([]Label, 0, len(names))
	for _, name := range names {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, false
		}
		out = append(out, label)
	}
	return out, true
}

func issuePagination(query string) (int, int) {
	perPage, page := 30, 1
	if v := queryParam(query, "per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	if v := queryParam(query, "page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if page < 1 {
		page = 1
	}
	return perPage, page
}

func issuePageBounds(total, perPage, page int) (int, int) {
	start := (page - 1) * perPage
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return start, end
}
