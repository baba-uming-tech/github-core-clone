package main

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type TreeEntry struct {
	Mode string
	Name string
	SHA  string
}

type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

type CommitInfo struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

type Tag struct {
	Object  string
	Type    string
	Name    string
	Tagger  string
	Message string
}

type DirEntry struct {
	Name string
	Path string
	Type string
	SHA  string
	Size int
}

var tagObjectPattern = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})`)
var identityPattern = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

var commitCounter int64

func GetObject(repo *Repo, sha string) *GitObject {
	if repo == nil {
		return nil
	}
	return repo.GetObject(sha)
}

func PutObject(repo *Repo, objectType ObjectType, data []byte) string {
	return repo.PutObject(objectType, data)
}

func decodeHexLoose(s string) []byte {
	if len(s)%2 != 0 {
		s = s[:len(s)-1]
	}
	raw, _ := hex.DecodeString(s)
	return raw
}

func treeSortName(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortName(sorted[i]) < treeSortName(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(decodeHexLoose(e.SHA))
	}
	return buf.Bytes()
}

func EncodeTree(entries []TreeEntry) []byte {
	return BuildTree(entries)
}

func ParseTree(data []byte) []TreeEntry {
	out := make([]TreeEntry, 0, 8)
	i := 0
	for i < len(data) {
		relSpace := bytes.IndexByte(data[i:], ' ')
		if relSpace < 0 {
			break
		}
		sp := i + relSpace
		relNul := bytes.IndexByte(data[sp+1:], 0)
		if relNul < 0 {
			break
		}
		nul := sp + 1 + relNul
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		end := nul + 21
		if end > len(data) {
			end = len(data)
		}
		sha := hex.EncodeToString(data[nul+1 : end])
		out = append(out, TreeEntry{Mode: mode, Name: name, SHA: sha})
		i = nul + 21
	}
	return out
}

func ParseCommit(data []byte) *Commit {
	text := string(data)
	sep := strings.Index(text, "\n\n")
	head, message := text, ""
	if sep >= 0 {
		head = text[:sep]
		message = text[sep+2:]
	}
	c := &Commit{Parents: make([]string, 0), Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	return c
}

func BuildCommit(c *Commit) []byte {
	if c == nil {
		c = &Commit{}
	}
	var buf strings.Builder
	buf.WriteString("tree ")
	buf.WriteString(c.Tree)
	buf.WriteByte('\n')
	for _, p := range c.Parents {
		buf.WriteString("parent ")
		buf.WriteString(p)
		buf.WriteByte('\n')
	}
	buf.WriteString("author ")
	buf.WriteString(c.Author)
	buf.WriteByte('\n')
	buf.WriteString("committer ")
	buf.WriteString(c.Committer)
	buf.WriteByte('\n')
	buf.WriteByte('\n')
	message := c.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	buf.WriteString(message)
	return []byte(buf.String())
}

func BuildCommitParts(tree string, parents []string, author, committer, message string) []byte {
	return BuildCommit(&Commit{Tree: tree, Parents: parents, Author: author, Committer: committer, Message: message})
}

func ParseTag(data []byte) *Tag {
	text := string(data)
	sep := strings.Index(text, "\n\n")
	head, message := text, ""
	if sep >= 0 {
		head = text[:sep]
		message = text[sep+2:]
	}
	t := &Tag{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = line[7:]
		case strings.HasPrefix(line, "type "):
			t.Type = line[5:]
		case strings.HasPrefix(line, "tag "):
			t.Name = line[4:]
		case strings.HasPrefix(line, "tagger "):
			t.Tagger = line[7:]
		}
	}
	return t
}

func TagObjectSHA(data []byte) string {
	m := tagObjectPattern.FindSubmatch(data)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

func ParseTagObject(data []byte) string {
	return TagObjectSHA(data)
}

func ParseIdentityLine(line string) (name, email, date string) {
	m := identityPattern.FindStringSubmatch(line)
	if m == nil {
		return line, "", "1970-01-01T00:00:00.000Z"
	}
	ts, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return line, "", "1970-01-01T00:00:00.000Z"
	}
	return m[1], m[2], time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func NextRestTimestamp() int64 {
	ts := 1735689600 + commitCounter
	commitCounter++
	return ts
}

func ResetCommitCounterForTest() {
	commitCounter = 0
}

func CollectReachable(tips []string, lookup func(string) *GitObject) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == "" || seen[sha] {
			continue
		}
		obj := lookup(sha)
		if obj == nil {
			continue
		}
		seen[sha] = true
		switch obj.Type {
		case ObjectTypeCommit:
			c := ParseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case ObjectTypeTree:
			for _, e := range ParseTree(obj.Data) {
				stack = append(stack, e.SHA)
			}
		case ObjectTypeTag:
			if object := TagObjectSHA(obj.Data); object != "" {
				stack = append(stack, object)
			}
		}
	}
	return seen
}

func ResolveRef(repo *Repo, ref string) string {
	if ref == "" || repo == nil {
		return ""
	}
	if sha, ok := repo.Refs[ref]; ok {
		return sha
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
	if sha, ok := repo.Refs["refs/heads/"+ref]; ok {
		return sha
	}
	if _, ok := repo.Objects[ref]; ok {
		return ref
	}
	return ""
}

func peelCommit(repo *Repo, sha string) *Commit {
	visited := make(map[string]bool)
	for sha != "" && !visited[sha] {
		visited[sha] = true
		obj := repo.GetObject(sha)
		if obj == nil {
			return nil
		}
		switch obj.Type {
		case ObjectTypeCommit:
			return ParseCommit(obj.Data)
		case ObjectTypeTag:
			sha = TagObjectSHA(obj.Data)
		default:
			return nil
		}
	}
	return nil
}

func treeForCommitish(repo *Repo, sha string) string {
	visited := make(map[string]bool)
	for sha != "" && !visited[sha] {
		visited[sha] = true
		obj := repo.GetObject(sha)
		if obj == nil {
			return ""
		}
		switch obj.Type {
		case ObjectTypeCommit:
			return ParseCommit(obj.Data).Tree
		case ObjectTypeTree:
			return sha
		case ObjectTypeTag:
			sha = TagObjectSHA(obj.Data)
		default:
			return ""
		}
	}
	return ""
}

func splitPath(path string) []string {
	raw := strings.Split(path, "/")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func lookupTreePath(repo *Repo, treeOid, path string) (string, string, bool) {
	if path == "" || path == "/" || path == "." {
		return "", "", false
	}
	parts := splitPath(path)
	if len(parts) == 0 {
		return "", "", false
	}
	cur := treeOid
	for i, part := range parts {
		obj := repo.GetObject(cur)
		if obj == nil || obj.Type != ObjectTypeTree {
			return "", "", false
		}
		entries := ParseTree(obj.Data)
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return "", "", false
		}
		if i == len(parts)-1 {
			return found.SHA, found.Mode, true
		}
		if found.Mode != "40000" {
			return "", "", false
		}
		cur = found.SHA
	}
	return "", "", false
}

func LookupPath(repo *Repo, commitSHA, path string) (string, string, bool) {
	treeOid := treeForCommitish(repo, ResolveRef(repo, commitSHA))
	if treeOid == "" {
		return "", "", false
	}
	return lookupTreePath(repo, treeOid, path)
}

func ReadFile(repo *Repo, treeOid, path string) string {
	sha, mode, ok := lookupTreePath(repo, treeOid, path)
	if !ok || mode == "40000" {
		return ""
	}
	return sha
}

func ListDir(repo *Repo, treeOid, dirPath string) []DirEntry {
	target := treeOid
	dp := strings.Trim(dirPath, "/")
	if dp != "" {
		for _, part := range splitPath(dp) {
			obj := repo.GetObject(target)
			if obj == nil || obj.Type != ObjectTypeTree {
				return []DirEntry{}
			}
			var found *TreeEntry
			for _, e := range ParseTree(obj.Data) {
				if e.Name == part {
					copy := e
					found = &copy
					break
				}
			}
			if found == nil || found.Mode != "40000" {
				return []DirEntry{}
			}
			target = found.SHA
		}
	}
	obj := repo.GetObject(target)
	if obj == nil || obj.Type != ObjectTypeTree {
		return []DirEntry{}
	}
	entries := ParseTree(obj.Data)
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		isDir := e.Mode == "40000"
		p := e.Name
		if dp != "" {
			p = dp + "/" + e.Name
		}
		size := 0
		if !isDir {
			blob := repo.GetObject(e.SHA)
			if blob != nil && blob.Type == ObjectTypeBlob {
				size = len(blob.Data)
			}
		}
		typeName := "file"
		if isDir {
			typeName = "dir"
		}
		out = append(out, DirEntry{Name: e.Name, Path: p, Type: typeName, SHA: e.SHA, Size: size})
	}
	return out
}

func WriteBlob(repo *Repo, data []byte) string {
	return repo.PutObject(ObjectTypeBlob, data)
}

func writeTreeAtPath(repo *Repo, baseTree string, parts []string, idx int, blobSHA string) string {
	name := parts[idx]
	isLast := idx == len(parts)-1
	byName := make(map[string]TreeEntry)
	if baseTree != "" {
		obj := repo.GetObject(baseTree)
		if obj != nil && obj.Type == ObjectTypeTree {
			for _, e := range ParseTree(obj.Data) {
				byName[e.Name] = e
			}
		}
	}
	if isLast {
		if blobSHA == "" {
			delete(byName, name)
		} else {
			if existing, ok := byName[name]; ok && existing.Mode == "100644" && existing.SHA == blobSHA {
				return baseTree
			}
			byName[name] = TreeEntry{Mode: "100644", Name: name, SHA: blobSHA}
		}
	} else {
		child := byName[name]
		childTree := ""
		if child.Mode == "40000" {
			childTree = child.SHA
		}
		newChild := writeTreeAtPath(repo, childTree, parts, idx+1, blobSHA)
		if newChild == "" {
			delete(byName, name)
		} else {
			byName[name] = TreeEntry{Mode: "40000", Name: name, SHA: newChild}
		}
	}
	newEntries := make([]TreeEntry, 0, len(byName))
	for _, e := range byName {
		newEntries = append(newEntries, e)
	}
	if len(newEntries) == 0 {
		return ""
	}
	return repo.PutObject(ObjectTypeTree, BuildTree(newEntries))
}

func WriteFile(repo *Repo, baseTreeOid, path, blobOid string) string {
	parts := splitPath(path)
	if len(parts) == 0 {
		return repo.PutObject(ObjectTypeTree, []byte{})
	}
	result := writeTreeAtPath(repo, baseTreeOid, parts, 0, blobOid)
	if result == "" {
		return repo.PutObject(ObjectTypeTree, []byte{})
	}
	return result
}

func WriteCommitUpdatingPath(repo *Repo, branch, path string, data []byte, message string) string {
	refName := "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")
	parent := repo.Refs[refName]
	parents := make([]string, 0, 1)
	baseTree := ""
	if parent != "" {
		parents = append(parents, parent)
		if c := peelCommit(repo, parent); c != nil {
			baseTree = c.Tree
		}
	}
	blobSHA := WriteBlob(repo, data)
	newTree := WriteFile(repo, baseTree, path, blobSHA)
	ident := "ada iversen <adaiversen@example.invalid> " + strconv.FormatInt(NextRestTimestamp(), 10) + " +0000"
	commitSHA := repo.PutObject(ObjectTypeCommit, BuildCommit(&Commit{
		Tree:      newTree,
		Parents:   parents,
		Author:    ident,
		Committer: ident,
		Message:   message,
	}))
	repo.Refs[refName] = commitSHA
	repo.Rev++
	return commitSHA
}

func CollectCommits(repo *Repo, startSHA string) []CommitInfo {
	out := make([]CommitInfo, 0)
	seen := make(map[string]bool)
	cur := ResolveRef(repo, startSHA)
	for cur != "" && !seen[cur] {
		obj := repo.GetObject(cur)
		if obj == nil || obj.Type != ObjectTypeCommit {
			break
		}
		seen[cur] = true
		c := ParseCommit(obj.Data)
		parents := c.Parents
		if parents == nil {
			parents = []string{}
		}
		out = append(out, CommitInfo{
			SHA:       cur,
			Tree:      c.Tree,
			Parents:   parents,
			Author:    c.Author,
			Committer: c.Committer,
			Message:   c.Message,
		})
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out
}

func IsAncestor(repo *Repo, ancestor, descendant string) bool {
	a := ResolveRef(repo, ancestor)
	d := ResolveRef(repo, descendant)
	if a == "" || d == "" {
		return false
	}
	if a == d {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{d}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == "" || seen[sha] {
			continue
		}
		seen[sha] = true
		if sha == a {
			return true
		}
		obj := repo.GetObject(sha)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjectTypeCommit:
			stack = append(stack, ParseCommit(obj.Data).Parents...)
		case ObjectTypeTag:
			if object := TagObjectSHA(obj.Data); object != "" {
				stack = append(stack, object)
			}
		}
	}
	return false
}
