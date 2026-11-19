package main

import (
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type commitIdentityJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitDetailJSON struct {
	Message   string             `json:"message"`
	Author    commitIdentityJSON `json:"author"`
	Committer commitIdentityJSON `json:"committer"`
}

type commitParentJSON struct {
	SHA string `json:"sha"`
}

type commitJSON struct {
	SHA     string             `json:"sha"`
	Commit  commitDetailJSON   `json:"commit"`
	Parents []commitParentJSON `json:"parents"`
}

type refObjectJSON struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type refJSON struct {
	Ref    string        `json:"ref"`
	Object refObjectJSON `json:"object"`
	URL    string        `json:"url,omitempty"`
}

type branchCommitJSON struct {
	SHA string `json:"sha"`
}

type branchJSON struct {
	Name      string           `json:"name"`
	Commit    branchCommitJSON `json:"commit"`
	Protected bool             `json:"protected"`
}

type branchSingleJSON struct {
	Name   string           `json:"name"`
	Commit branchCommitJSON `json:"commit"`
}

type contentFileJSON struct {
	Type        string `json:"type"`
	Encoding    string `json:"encoding"`
	Size        int    `json:"size"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	SHA         string `json:"sha"`
	Content     string `json:"content"`
	GitURL      string `json:"git_url"`
	HTMLURL     string `json:"html_url"`
	DownloadURL string `json:"download_url"`
}

type contentDirEntryJSON struct {
	Type string `json:"type"`
	Size int    `json:"size"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

type contentWriteJSON struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type contentPutCommitJSON struct {
	SHA       string             `json:"sha"`
	Message   string             `json:"message"`
	Author    commitIdentityJSON `json:"author"`
	Committer commitIdentityJSON `json:"committer"`
}

type putContentsResponse struct {
	Content contentWriteJSON     `json:"content"`
	Commit  contentPutCommitJSON `json:"commit"`
}

type deleteCommitJSON struct {
	SHA string `json:"sha"`
}

type deleteContentsResponse struct {
	Content any              `json:"content"`
	Commit  deleteCommitJSON `json:"commit"`
}

func (a *App) RouteAPIV3(w http.ResponseWriter, r *http.Request, method, pathname string, segs []string, query string, body map[string]any) bool {
	store := a.Store

	if (pathname == "/api/v3" || pathname == "/api/v3/") && method == http.MethodGet {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		a.sendJSON(w, http.StatusOK, []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`))
		return true
	}

	if len(segs) == 3 && segs[0] == "api" && segs[1] == "v3" && segs[2] == "meta" && method == http.MethodGet {
		a.sendJSON(w, http.StatusOK, []byte(`{"installed_version":"github-core-clone-01 1.0.0"}`))
		return true
	}

	if len(segs) == 3 && segs[0] == "api" && segs[1] == "v3" && segs[2] == "user" && method == http.MethodGet {
		a.sendJSON(w, http.StatusOK, UserJSON(store))
		return true
	}

	if len(segs) == 4 && segs[0] == "api" && segs[1] == "v3" && segs[2] == "user" && segs[3] == "repos" {
		if method == http.MethodGet {
			a.listUserRepos(w, r, query)
			return true
		}
		if method == http.MethodPost {
			a.createUserRepo(w, body)
			return true
		}
		return false
	}

	if len(segs) >= 5 && segs[0] == "api" && segs[1] == "v3" && segs[2] == "repos" {
		repo := store.GetRepo(segs[3], segs[4])
		if repo == nil {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}

		if len(segs) == 5 {
			switch method {
			case http.MethodGet:
				a.sendJSON(w, http.StatusOK, RepoJSON(repo))
				return true
			case http.MethodPatch:
				a.patchRepo(w, repo, body)
				return true
			case http.MethodDelete:
				delete(store.Repos, store.RepoKey(repo.Owner, repo.Name))
				a.sendJSON(w, http.StatusNoContent, nil)
				return true
			}
			return false
		}

		return a.routeRepoResources(w, r, method, segs[5:], query, body, repo)
	}

	return false
}

func (a *App) listUserRepos(w http.ResponseWriter, r *http.Request, query string) {
	perPage := 30
	if v := queryParam(query, "per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	page := 1
	if v := queryParam(query, "page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	if page < 1 {
		page = 1
	}

	owned := make([]*Repo, 0, len(a.Store.Repos))
	for _, repo := range a.Store.Repos {
		if repo.Owner == a.Store.Login {
			owned = append(owned, repo)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID > owned[j].ID })

	start := (page - 1) * perPage
	end := start + perPage
	if start > len(owned) {
		start = len(owned)
	}
	if end > len(owned) {
		end = len(owned)
	}
	slice := owned[start:end]

	if start+perPage < len(owned) {
		host := r.Host
		if host == "" {
			host = "localhost"
		}
		w.Header().Set("Link", "<http://"+host+"/api/v3/user/repos?per_page="+strconv.Itoa(perPage)+"&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
	}

	buf := make([]byte, 0, 2+len(slice)*512)
	buf = append(buf, '[')
	for i, repo := range slice {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, RepoJSON(repo)...)
	}
	buf = append(buf, ']')
	a.sendJSON(w, http.StatusOK, buf)
}

func (a *App) createUserRepo(w http.ResponseWriter, body map[string]any) {
	name, _ := body["name"].(string)
	if name == "" {
		a.sendError(w, http.StatusUnprocessableEntity, "Repository creation failed")
		return
	}
	key := a.Store.RepoKey(a.Store.Login, name)
	if _, exists := a.Store.Repos[key]; exists {
		a.sendError(w, http.StatusUnprocessableEntity, "Repository creation failed: name already exists")
		return
	}
	description, _ := body["description"].(string)
	isPrivate, _ := body["private"].(bool)
	repo := NewRepo(a.Store.Login, name, a.Store.NextRepoID, description, isPrivate, a.Store.NowISO())
	a.Store.NextRepoID++
	a.Store.Repos[key] = repo
	a.sendJSON(w, http.StatusCreated, RepoJSON(repo))
}

func (a *App) patchRepo(w http.ResponseWriter, repo *Repo, body map[string]any) {
	if v, ok := body["description"].(string); ok {
		repo.Description = v
	}
	if v, ok := body["private"].(bool); ok {
		repo.Private = v
	}
	if name, ok := body["name"].(string); ok && name != "" && name != repo.Name {
		delete(a.Store.Repos, a.Store.RepoKey(repo.Owner, repo.Name))
		repo.Name = name
		a.Store.Repos[a.Store.RepoKey(repo.Owner, repo.Name)] = repo
	}
	a.sendJSON(w, http.StatusOK, RepoJSON(repo))
}

func (a *App) routeRepoResources(w http.ResponseWriter, r *http.Request, method string, rest []string, query string, body map[string]any, repo *Repo) bool {
	if len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" {
		if method == http.MethodGet {
			a.listRefs(w, query, repo)
			return true
		}
		if method == http.MethodPost {
			a.createRef(w, body, repo)
			return true
		}
		return false
	}

	if len(rest) >= 4 && rest[0] == "git" && rest[1] == "refs" && rest[2] == "heads" && method == http.MethodGet {
		branch := strings.Join(rest[3:], "/")
		refName := "refs/heads/" + branch
		sha, ok := repo.Refs[refName]
		if !ok {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		a.sendJSON(w, http.StatusOK, encodeJSON(refJSON{
			Ref:    refName,
			Object: refObjectJSON{Type: "commit", SHA: sha},
		}))
		return true
	}

	if len(rest) == 1 && rest[0] == "branches" && method == http.MethodGet {
		a.listBranches(w, repo)
		return true
	}

	if len(rest) >= 2 && rest[0] == "branches" && method == http.MethodGet {
		branch := strings.Join(rest[1:], "/")
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return true
		}
		a.sendJSON(w, http.StatusOK, encodeJSON(branchSingleJSON{
			Name:   branch,
			Commit: branchCommitJSON{SHA: sha},
		}))
		return true
	}

	if len(rest) == 1 && rest[0] == "commits" && method == http.MethodGet {
		a.listCommits(w, query, repo)
		return true
	}

	if len(rest) >= 2 && rest[0] == "commits" && method == http.MethodGet {
		a.getCommit(w, strings.Join(rest[1:], "/"), repo)
		return true
	}

	if len(rest) >= 2 && rest[0] == "contents" {
		path := strings.Join(rest[1:], "/")
		switch method {
		case http.MethodGet:
			a.handleGetContents(w, repo, path, queryParam(query, "ref"))
			return true
		case http.MethodPut:
			a.handlePutContents(w, repo, path, body)
			return true
		case http.MethodDelete:
			a.handleDeleteContents(w, repo, path, body)
			return true
		}
		return false
	}

	if a.routeIssuesPulls(w, r, method, rest, query, body, repo) {
		return true
	}

	return false
}

func (a *App) listRefs(w http.ResponseWriter, query string, repo *Repo) {
	filter := queryParam(query, "ref")
	prefix := ""
	if filter != "" {
		if strings.HasPrefix(filter, "refs/") {
			prefix = filter
		} else {
			prefix = "refs/" + filter
		}
	}
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		if prefix == "" || strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	refs := make([]refJSON, 0, len(names))
	for _, name := range names {
		refs = append(refs, refJSON{
			Ref:    name,
			Object: refObjectJSON{Type: "commit", SHA: repo.Refs[name]},
		})
	}
	a.sendJSON(w, http.StatusOK, encodeJSON(refs))
}

func (a *App) createRef(w http.ResponseWriter, body map[string]any, repo *Repo) {
	ref, refOK := body["ref"].(string)
	sha, shaOK := body["sha"].(string)
	if !refOK || !shaOK {
		a.sendError(w, http.StatusUnprocessableEntity, "Reference update failed")
		return
	}
	if _, ok := repo.Objects[sha]; !ok {
		a.sendError(w, http.StatusUnprocessableEntity, "Reference update failed")
		return
	}
	if _, ok := repo.Refs[ref]; ok {
		a.sendError(w, http.StatusUnprocessableEntity, "Reference update failed")
		return
	}
	repo.Refs[ref] = sha
	repo.Rev++
	a.sendJSON(w, http.StatusCreated, encodeJSON(refJSON{
		Ref:    ref,
		Object: refObjectJSON{Type: "commit", SHA: sha},
		URL:    "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/" + ref,
	}))
}

func (a *App) listBranches(w http.ResponseWriter, repo *Repo) {
	branches := make([]branchJSON, 0, len(repo.Refs))
	for ref, sha := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, branchJSON{
				Name:      ref[len("refs/heads/"):],
				Commit:    branchCommitJSON{SHA: sha},
				Protected: false,
			})
		}
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	a.sendJSON(w, http.StatusOK, encodeJSON(branches))
}

func (a *App) listCommits(w http.ResponseWriter, query string, repo *Repo) {
	shaParam := queryParam(query, "sha")
	perPage := 0
	hasPerPage := false
	if v := queryParam(query, "per_page"); v != "" {
		hasPerPage = true
		perPage = 30
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			perPage = n
		}
		if perPage < 1 {
			perPage = 1
		}
	}
	tip := resolveCommitRef(repo, shaParam)
	if tip == "" {
		if shaParam != "" {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return
		}
		a.sendJSON(w, http.StatusOK, []byte(`[]`))
		return
	}
	shas := walkCommitsFirstParent(repo, tip)
	if hasPerPage && len(shas) > perPage {
		shas = shas[:perPage]
	}
	commits := make([]commitJSON, 0, len(shas))
	for _, sha := range shas {
		if c := commitToJSON(repo, sha); c != nil {
			commits = append(commits, *c)
		}
	}
	a.sendJSON(w, http.StatusOK, encodeJSON(commits))
}

func (a *App) getCommit(w http.ResponseWriter, ref string, repo *Repo) {
	tip := resolveCommitRef(repo, ref)
	if tip == "" {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := commitToJSON(repo, tip)
	if c == nil {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	a.sendJSON(w, http.StatusOK, encodeJSON(c))
}

func (a *App) handleGetContents(w http.ResponseWriter, repo *Repo, path, refParam string) {
	tip := resolveCommitRef(repo, refParam)
	if tip == "" {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	commitObj := repo.GetObject(tip)
	if commitObj == nil || commitObj.Type != ObjectTypeCommit {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	treeOid := ParseCommit(commitObj.Data).Tree

	blobSHA := ReadFile(repo, treeOid, path)
	if blobSHA != "" {
		blob := repo.GetObject(blobSHA)
		if blob == nil || blob.Type != ObjectTypeBlob {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return
		}
		ref := refParam
		if ref == "" {
			ref = defaultBranch(repo)
		}
		a.sendJSON(w, http.StatusOK, encodeJSON(contentFileJSON{
			Type:        "file",
			Encoding:    "base64",
			Size:        len(blob.Data),
			Name:        nameForPath(path),
			Path:        path,
			SHA:         blobSHA,
			Content:     base64.StdEncoding.EncodeToString(blob.Data),
			GitURL:      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/blobs/" + blobSHA,
			HTMLURL:     "/" + repo.Owner + "/" + repo.Name + "/blob/" + ref + "/" + path,
			DownloadURL: "/raw/" + repo.Owner + "/" + repo.Name + "/" + ref + "/" + path,
		}))
		return
	}

	if _, mode, ok := lookupTreePath(repo, treeOid, path); ok && mode == "40000" {
		entries := ListDir(repo, treeOid, path)
		out := make([]contentDirEntryJSON, 0, len(entries))
		for _, e := range entries {
			size := 0
			if e.Type == "file" {
				size = e.Size
			}
			out = append(out, contentDirEntryJSON{
				Type: e.Type,
				Size: size,
				Name: e.Name,
				Path: e.Path,
				SHA:  e.SHA,
			})
		}
		a.sendJSON(w, http.StatusOK, encodeJSON(out))
		return
	}

	a.sendError(w, http.StatusNotFound, "Not Found")
}

func (a *App) handlePutContents(w http.ResponseWriter, repo *Repo, path string, body map[string]any) {
	message, msgOK := body["message"].(string)
	content, contentOK := body["content"].(string)
	if !msgOK || !contentOK {
		a.sendError(w, http.StatusUnprocessableEntity, "Invalid request")
		return
	}

	branchName, _ := body["branch"].(string)
	if branchName == "" {
		branchName = defaultBranch(repo)
	}
	refName := "refs/heads/" + branchName

	baseTip := repo.Refs[refName]
	baseTree := ""
	if baseTip == "" {
		defaultTip := repo.Refs["refs/heads/"+defaultBranch(repo)]
		if defaultTip != "" {
			repo.Refs[refName] = defaultTip
			repo.Rev++
			baseTip = defaultTip
		}
	}
	if baseTip != "" {
		if cobj := repo.GetObject(baseTip); cobj != nil && cobj.Type == ObjectTypeCommit {
			baseTree = ParseCommit(cobj.Data).Tree
		}
	}

	contentBuf := decodeBase64Loose(content)
	existingBlob := ""
	if baseTree != "" {
		existingBlob = ReadFile(repo, baseTree, path)
	}
	isCreate := existingBlob == ""
	if sha, ok := body["sha"].(string); ok {
		if existingBlob == "" || existingBlob != sha {
			a.sendError(w, http.StatusConflict, "sha does not match")
			return
		}
	}

	blobOid := PutObject(repo, ObjectTypeBlob, contentBuf)
	newTree := WriteFile(repo, baseTree, path, blobOid)
	ident := fixedIdentity(body["committer"])
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	parents := make([]string, 0, 1)
	if baseTip != "" {
		parents = append(parents, baseTip)
	}
	commitBuf := BuildCommit(&Commit{
		Tree:      newTree,
		Parents:   parents,
		Author:    ident.line(),
		Committer: ident.line(),
		Message:   msg,
	})
	commitSHA := PutObject(repo, ObjectTypeCommit, commitBuf)
	repo.Refs[refName] = commitSHA
	if branchName == defaultBranch(repo) {
		repo.DefaultBranch = "main"
	}
	repo.Rev++

	resp := putContentsResponse{
		Content: contentWriteJSON{
			Name:     nameForPath(path),
			Path:     path,
			SHA:      blobOid,
			Size:     len(contentBuf),
			Encoding: "base64",
			Content:  base64.StdEncoding.EncodeToString(contentBuf),
		},
	}
	if c := commitToJSON(repo, commitSHA); c != nil {
		resp.Commit = contentPutCommitJSON{
			SHA:       c.SHA,
			Message:   c.Commit.Message,
			Author:    c.Commit.Author,
			Committer: c.Commit.Committer,
		}
	} else {
		resp.Commit = contentPutCommitJSON{SHA: commitSHA}
	}

	status := http.StatusOK
	if isCreate {
		status = http.StatusCreated
	}
	a.sendJSON(w, status, encodeJSON(resp))
}

func (a *App) handleDeleteContents(w http.ResponseWriter, repo *Repo, path string, body map[string]any) {
	message, ok := body["message"].(string)
	if !ok {
		a.sendError(w, http.StatusUnprocessableEntity, "Invalid request")
		return
	}

	branchName, _ := body["branch"].(string)
	if branchName == "" {
		branchName = defaultBranch(repo)
	}
	refName := "refs/heads/" + branchName
	tip, ok := repo.Refs[refName]
	if !ok || tip == "" {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	cobj := repo.GetObject(tip)
	if cobj == nil || cobj.Type != ObjectTypeCommit {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	baseTree := ParseCommit(cobj.Data).Tree
	existingBlob := ReadFile(repo, baseTree, path)
	if existingBlob == "" {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if sha, ok := body["sha"].(string); ok && sha != existingBlob {
		a.sendError(w, http.StatusConflict, "sha does not match")
		return
	}

	newTree := WriteFile(repo, baseTree, path, "")
	ident := fixedIdentity(body["committer"])
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commitBuf := BuildCommit(&Commit{
		Tree:      newTree,
		Parents:   []string{tip},
		Author:    ident.line(),
		Committer: ident.line(),
		Message:   msg,
	})
	commitSHA := PutObject(repo, ObjectTypeCommit, commitBuf)
	repo.Refs[refName] = commitSHA
	repo.Rev++

	a.sendJSON(w, http.StatusOK, encodeJSON(deleteContentsResponse{
		Content: nil,
		Commit:  deleteCommitJSON{SHA: commitSHA},
	}))
}

func resolveCommitRef(repo *Repo, shaParam string) string {
	if shaParam == "" {
		return repo.Refs["refs/heads/"+defaultBranch(repo)]
	}
	if strings.HasPrefix(shaParam, "refs/") {
		return repo.Refs[shaParam]
	}
	if sha, ok := repo.Refs["refs/heads/"+shaParam]; ok {
		return sha
	}
	if _, ok := repo.Objects[shaParam]; ok {
		return shaParam
	}
	return ""
}

func walkCommitsFirstParent(repo *Repo, tip string) []string {
	out := make([]string, 0, 16)
	seen := make(map[string]bool)
	cur := tip
	for cur != "" && !seen[cur] {
		obj := repo.GetObject(cur)
		if obj == nil || obj.Type != ObjectTypeCommit {
			break
		}
		seen[cur] = true
		out = append(out, cur)
		c := ParseCommit(obj.Data)
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out
}

func commitToJSON(repo *Repo, sha string) *commitJSON {
	obj := repo.GetObject(sha)
	if obj == nil || obj.Type != ObjectTypeCommit {
		return nil
	}
	c := ParseCommit(obj.Data)
	authorName, authorEmail, authorDate := ParseIdentityLine(c.Author)
	committerName, committerEmail, committerDate := ParseIdentityLine(c.Committer)
	msg := c.Message
	if strings.HasSuffix(msg, "\n") {
		msg = msg[:len(msg)-1]
	}
	parents := make([]commitParentJSON, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, commitParentJSON{SHA: p})
	}
	return &commitJSON{
		SHA: sha,
		Commit: commitDetailJSON{
			Message:   msg,
			Author:    commitIdentityJSON{Name: authorName, Email: authorEmail, Date: authorDate},
			Committer: commitIdentityJSON{Name: committerName, Email: committerEmail, Date: committerDate},
		},
		Parents: parents,
	}
}

type identity struct {
	name      string
	email     string
	timestamp int64
}

func fixedIdentity(committer any) identity {
	if m, ok := committer.(map[string]any); ok {
		name, nameOK := m["name"].(string)
		email, emailOK := m["email"].(string)
		if nameOK && emailOK {
			ts := NextRestTimestamp()
			if dateStr, ok := m["date"].(string); ok {
				if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
					ts = t.Unix()
				}
			}
			return identity{name: name, email: email, timestamp: ts}
		}
	}
	return identity{name: "ada iversen", email: "adaiversen@example.invalid", timestamp: NextRestTimestamp()}
}

func (id identity) line() string {
	return id.name + " <" + id.email + "> " + strconv.FormatInt(id.timestamp, 10) + " +0000"
}

func nameForPath(path string) string {
	parts := splitPath(path)
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

func decodeBase64Loose(s string) []byte {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '+' || r == '/' || r == '=' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	raw := b.String()
	if raw == "" {
		return []byte{}
	}
	if m := len(raw) % 4; m != 0 {
		raw += strings.Repeat("=", 4-m)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return []byte{}
	}
	return data
}
