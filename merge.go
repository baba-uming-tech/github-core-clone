package main

import (
	"sort"
	"strconv"
)

type MergeResult struct {
	OK       bool
	Conflict bool
	SHA      string
}

type mergeTreeFile struct {
	SHA  string
	Mode string
}

type mergeFileSet struct {
	files map[string]mergeTreeFile
	order []string
}

func newMergeFileSet() *mergeFileSet {
	return &mergeFileSet{files: make(map[string]mergeTreeFile), order: []string{}}
}

func (s *mergeFileSet) get(path string) (mergeTreeFile, bool) {
	entry, ok := s.files[path]
	return entry, ok
}

func (s *mergeFileSet) has(path string) bool {
	_, ok := s.files[path]
	return ok
}

func (s *mergeFileSet) set(path string, entry mergeTreeFile) {
	if _, ok := s.files[path]; !ok {
		s.order = append(s.order, path)
	}
	s.files[path] = entry
}

func mergeCommitTree(repo *Repo, sha string) string {
	obj := GetObject(repo, sha)
	if obj == nil || obj.Type != ObjectTypeCommit {
		return ""
	}
	c := ParseCommit(obj.Data)
	if c == nil {
		return ""
	}
	return c.Tree
}

func mergeTreeFiles(repo *Repo, treeOid string) *mergeFileSet {
	out := newMergeFileSet()
	if treeOid == "" {
		return out
	}
	type frame struct {
		oid    string
		prefix string
	}
	stack := []frame{{oid: treeOid, prefix: ""}}
	seen := make(map[string]bool)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := cur.prefix + "\x00" + cur.oid
		if seen[key] {
			continue
		}
		seen[key] = true
		obj := GetObject(repo, cur.oid)
		if obj == nil || obj.Type != ObjectTypeTree {
			continue
		}
		for _, entry := range ParseTree(obj.Data) {
			path := entry.Name
			if cur.prefix != "" {
				path = cur.prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				stack = append(stack, frame{oid: entry.SHA, prefix: path})
			} else {
				out.set(path, mergeTreeFile{SHA: entry.SHA, Mode: entry.Mode})
			}
		}
	}
	return out
}

type mergeDistances struct {
	dist  map[string]int
	order []string
}

func commitDistances(repo *Repo, tip string) *mergeDistances {
	out := &mergeDistances{dist: make(map[string]int), order: []string{}}
	type queueItem struct {
		oid string
		d   int
	}
	queue := []queueItem{{oid: tip, d: 0}}
	for qi := 0; qi < len(queue); qi++ {
		cur := queue[qi]
		if _, exists := out.dist[cur.oid]; exists {
			continue
		}
		obj := GetObject(repo, cur.oid)
		if obj == nil || obj.Type != ObjectTypeCommit {
			continue
		}
		out.dist[cur.oid] = cur.d
		out.order = append(out.order, cur.oid)
		for _, parent := range ParseCommit(obj.Data).Parents {
			queue = append(queue, queueItem{oid: parent, d: cur.d + 1})
		}
	}
	return out
}

func mergeBase(repo *Repo, a, b string) string {
	da := commitDistances(repo, a)
	db := commitDistances(repo, b)
	best := ""
	bestScore := int(^uint(0) >> 1)
	for _, oid := range da.order {
		other, ok := db.dist[oid]
		if !ok {
			continue
		}
		score := da.dist[oid] + other
		if score < bestScore {
			bestScore = score
			best = oid
		}
	}
	return best
}

func mergeLineCount(repo *Repo, sha string) int {
	obj := GetObject(repo, sha)
	if obj == nil || obj.Type != ObjectTypeBlob || len(obj.Data) == 0 {
		return 0
	}
	text := string(obj.Data)
	n := 1 + stringsCountByte(text, '\n')
	if len(text) > 0 && text[len(text)-1] == '\n' {
		n--
	}
	if n < 1 {
		n = 1
	}
	return n
}

func stringsCountByte(s string, b byte) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			n++
		}
	}
	return n
}

func PullChangedFiles(repo *Repo, pull *Pull) []map[string]any {
	baseTip := repo.Refs["refs/heads/"+pull.BaseRef]
	if baseTip == "" {
		baseTip = pull.BaseSha
	}
	headTip := repo.Refs["refs/heads/"+pull.HeadRef]
	if headTip == "" {
		headTip = pull.HeadSha
	}
	mb := mergeBase(repo, baseTip, headTip)
	mbTree := ""
	if mb != "" {
		mbTree = mergeCommitTree(repo, mb)
	}
	baseFiles := newMergeFileSet()
	if mbTree != "" {
		baseFiles = mergeTreeFiles(repo, mbTree)
	}
	headTree := mergeCommitTree(repo, headTip)
	headFiles := newMergeFileSet()
	if headTree != "" {
		headFiles = mergeTreeFiles(repo, headTree)
	}

	pathSet := make(map[string]bool)
	paths := make([]string, 0, len(baseFiles.files)+len(headFiles.files))
	for _, path := range baseFiles.order {
		if !pathSet[path] {
			pathSet[path] = true
			paths = append(paths, path)
		}
	}
	for _, path := range headFiles.order {
		if !pathSet[path] {
			pathSet[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	files := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		headEntry, hasHead := headFiles.get(path)
		baseEntry, hasBase := baseFiles.get(path)
		var status string
		var additions, deletions int
		var sha string
		if hasHead && !hasBase {
			status = "added"
			sha = headEntry.SHA
			additions = mergeLineCount(repo, sha)
		} else if hasHead && hasBase && headEntry.SHA != baseEntry.SHA {
			status = "modified"
			sha = headEntry.SHA
			additions = mergeLineCount(repo, sha)
		} else if !hasHead && hasBase {
			status = "removed"
			sha = baseEntry.SHA
			deletions = mergeLineCount(repo, sha)
		} else {
			continue
		}
		files = append(files, map[string]any{
			"sha":       sha,
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
			"blob_url":  "/" + repo.Owner + "/" + repo.Name + "/blob/" + pull.HeadRef + "/" + path,
			"raw_url":   "/raw/" + repo.Owner + "/" + repo.Name + "/" + pull.HeadRef + "/" + path,
		})
		if len(files) >= 300 {
			break
		}
	}
	return files
}

type mergePlan struct {
	baseRefName string
	baseTip     string
	baseTree    string
	headTree    string
	baseFiles   *mergeFileSet
	headFiles   *mergeFileSet
	conflict    bool
}

func planMerge(repo *Repo, pull *Pull) *mergePlan {
	baseRefName := "refs/heads/" + pull.BaseRef
	baseTip := repo.Refs[baseRefName]
	baseTree := ""
	if baseTip != "" {
		baseTree = mergeCommitTree(repo, baseTip)
	}
	headTip := repo.Refs["refs/heads/"+pull.HeadRef]
	if headTip == "" {
		headTip = pull.HeadSha
	}
	headTree := mergeCommitTree(repo, headTip)
	if baseTip == "" || baseTree == "" || headTree == "" {
		return nil
	}
	mb := mergeBase(repo, baseTip, headTip)
	mbTree := ""
	if mb != "" {
		mbTree = mergeCommitTree(repo, mb)
	}
	baseFiles := newMergeFileSet()
	if mbTree != "" {
		baseFiles = mergeTreeFiles(repo, mbTree)
	}
	headFiles := mergeTreeFiles(repo, headTree)
	tipFiles := mergeTreeFiles(repo, baseTree)

	conflict := false
	for _, path := range headFiles.order {
		headEntry := headFiles.files[path]
		mbEntry, hasMB := baseFiles.get(path)
		if hasMB && mbEntry.SHA == headEntry.SHA {
			continue
		}
		tipEntry, hasTip := tipFiles.get(path)
		if hasTip && (!hasMB || mbEntry.SHA != tipEntry.SHA) && tipEntry.SHA != headEntry.SHA {
			conflict = true
			break
		}
	}
	if !conflict {
		for _, path := range baseFiles.order {
			if headFiles.has(path) {
				continue
			}
			mbEntry := baseFiles.files[path]
			tipEntry, hasTip := tipFiles.get(path)
			if hasTip && tipEntry.SHA != mbEntry.SHA {
				conflict = true
				break
			}
		}
	}
	return &mergePlan{
		baseRefName: baseRefName,
		baseTip:     baseTip,
		baseTree:    baseTree,
		headTree:    headTree,
		baseFiles:   baseFiles,
		headFiles:   headFiles,
		conflict:    conflict,
	}
}

func MergePull(store *Store, repo *Repo, pull *Pull, method string) MergeResult {
	plan := planMerge(repo, pull)
	if plan == nil {
		return MergeResult{}
	}
	if plan.conflict {
		return MergeResult{Conflict: true}
	}

	tree := plan.baseTree
	for _, path := range plan.headFiles.order {
		tree = WriteFile(repo, tree, path, plan.headFiles.files[path].SHA)
	}
	for _, path := range plan.baseFiles.order {
		if !plan.headFiles.has(path) {
			tree = WriteFile(repo, tree, path, "")
		}
	}

	ident := "ada iversen <adaiversen@example.invalid> " + strconv.FormatInt(NextRestTimestamp(), 10) + " +0000"
	message := "Merge pull request #" + itoa(pull.Number) + " from " + pull.HeadOwner + ":" + pull.HeadRef
	commitSha := PutObject(repo, ObjectTypeCommit, BuildCommit(&Commit{
		Tree:      tree,
		Parents:   []string{plan.baseTip},
		Message:   message,
		Author:    ident,
		Committer: ident,
	}))
	repo.Refs[plan.baseRefName] = commitSha

	mergedAt := store.NowISO()
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &commitSha
	pull.ClosedAt = &mergedAt
	pull.UpdatedAt = mergedAt
	repo.Rev++

	return MergeResult{OK: true, SHA: commitSha}
}
