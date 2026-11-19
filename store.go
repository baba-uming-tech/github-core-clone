package main

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

type ObjectType string

const (
	ObjectTypeBlob   ObjectType = "blob"
	ObjectTypeTree   ObjectType = "tree"
	ObjectTypeCommit ObjectType = "commit"
	ObjectTypeTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType `json:"type"`
	Data []byte     `json:"data"`
}

type Label struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Description *string `json:"description"`
}

type Comment struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type Issue struct {
	ID               int       `json:"id"`
	Number           int       `json:"number"`
	Title            string    `json:"title"`
	Body             string    `json:"body"`
	State            string    `json:"state"`
	StateReason      *string   `json:"stateReason"`
	Labels           []Label   `json:"labels"`
	CreatedAt        string    `json:"createdAt"`
	UpdatedAt        string    `json:"updatedAt"`
	ClosedAt         *string   `json:"closedAt"`
	Comments         []Comment `json:"comments"`
	Version          int       `json:"version"`
	JSONCache        []byte    `json:"-"`
	JSONCacheVersion int       `json:"-"`
}

type Pull struct {
	ID             int     `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	HeadOwner      string  `json:"headOwner"`
	HeadRef        string  `json:"headRef"`
	HeadSha        string  `json:"headSha"`
	BaseOwner      string  `json:"baseOwner"`
	BaseRef        string  `json:"baseRef"`
	BaseSha        string  `json:"baseSha"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"mergedAt"`
	MergeCommitSha *string `json:"mergeCommitSha"`
	Draft          bool    `json:"draft"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	ClosedAt       *string `json:"closedAt"`
}

type Repo struct {
	Owner         string
	Name          string
	ID            int
	Description   string
	Private       bool
	CreatedAt     string
	DefaultBranch string
	Refs          map[string]string
	Objects       map[string]GitObject
	Labels        map[string]Label
	Issues        []Issue
	IssueSeq      int
	CommentSeq    int
	Pulls         []Pull
	PullSeq       int
	Rev           int
}

func NewRepo(owner, name string, id int, description string, isPrivate bool, createdAt string) *Repo {
	return &Repo{
		Owner:         owner,
		Name:          name,
		ID:            id,
		Description:   description,
		Private:       isPrivate,
		CreatedAt:     createdAt,
		DefaultBranch: "main",
		Refs:          make(map[string]string),
		Objects:       make(map[string]GitObject),
		Labels:        make(map[string]Label),
		Issues:        make([]Issue, 0),
		Pulls:         make([]Pull, 0),
	}
}

func (r *Repo) FindIssue(number int) *Issue {
	for i := range r.Issues {
		if r.Issues[i].Number == number {
			return &r.Issues[i]
		}
	}
	return nil
}

func (r *Repo) FindPull(number int) *Pull {
	for i := range r.Pulls {
		if r.Pulls[i].Number == number {
			return &r.Pulls[i]
		}
	}
	return nil
}

func (r *Repo) PutObject(objectType ObjectType, data []byte) string {
	sha := hashObject(objectType, data)
	if _, ok := r.Objects[sha]; !ok {
		r.Objects[sha] = GitObject{Type: objectType, Data: data}
		r.Rev++
	}
	return sha
}

func (r *Repo) GetObject(sha string) *GitObject {
	obj, ok := r.Objects[sha]
	if !ok {
		return nil
	}
	return &obj
}

type Store struct {
	Login         string
	Repos         map[string]*Repo
	NextRepoID    int
	NextIssueID   int
	NextLabelID   int
	NextCommentID int
	NextPullID    int
	NowCounter    int
}

func NewStore() *Store {
	return &Store{
		Login:         "adaiversen",
		Repos:         make(map[string]*Repo),
		NextRepoID:    1,
		NextIssueID:   1,
		NextLabelID:   1,
		NextCommentID: 1,
		NextPullID:    1,
		NowCounter:    0,
	}
}

func (s *Store) RepoKey(owner, name string) string {
	return strings.ToLower(owner + "/" + name)
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.GetRepoByKey(s.RepoKey(owner, name))
}

func (s *Store) GetRepoByKey(key string) *Repo {
	repo, ok := s.Repos[key]
	if !ok {
		return nil
	}
	if repo.Owner != s.Login {
		return nil
	}
	return repo
}

func (s *Store) FindIssue(owner, name string, number int) *Issue {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return nil
	}
	return repo.FindIssue(number)
}

func (s *Store) FindPull(owner, name string, number int) *Pull {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return nil
	}
	return repo.FindPull(number)
}

func (s *Store) NowISO() string {
	base := time.Now().UnixMilli() + int64(s.NowCounter)
	s.NowCounter++
	return time.UnixMilli(base).UTC().Format("2006-01-02T15:04:05.000Z")
}

func hashObject(objectType ObjectType, data []byte) string {
	header := []byte(string(objectType) + " " + strconv.Itoa(len(data)) + "\x00")
	h := sha1.New()
	h.Write(header)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
