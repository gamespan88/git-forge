// The forge's data store: repositories, each with its own git object store and refs, plus the
// issue / pull-request / label records the REST, GraphQL and MCP surfaces share. Everything is
// in memory: a trial starts from an empty forge and the suite builds every fixture itself.
//
// The one identity: the bearer token the suite presents belongs to the trial scenario's viewer
// (wire.ts's buildScenario() runs on one fixed seed in the sealed env, so the viewer is a
// constant of the contest): adaiversen, id 1000. A repository belongs to that login; the same
// name under any other owner does not exist for this token.
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type ViewerInfo struct {
	ID    int
	Login string
	Name  string
	Email string
}

var VIEWER = ViewerInfo{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

type LabelRec struct {
	Name  string
	Color string
}

type CommentRec struct {
	ID   int
	Body string
}

type IssueRec struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*CommentRec
	Seq      int

	viewCache []byte // the issue's serialized REST view, valid while viewKey matches
	viewKey   string
}

type PullRec struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	HeadRef        string
	BaseRef        string
	Seq            int
}

type Repo struct {
	Objects       *ObjectStore
	Refs          map[string]string // full ref names ("refs/heads/main") to commit ids
	Labels        map[string]*LabelRec
	LabelOrder    []string // label names in creation order (list responses keep it)
	Issues        map[int]*IssueRec
	Pulls         map[int]*PullRec
	CreatedAt     string
	ID            int
	Name          string
	Owner         string
	Description   string
	IsPrivate     bool
	DefaultBranch string
	NextNumber    int
	seqCounter    int

	viewCache []byte // the repo's serialized REST view, valid while viewKey matches
	viewKey   string
}

func NewRepo(id int, name, owner, description string, isPrivate bool) *Repo {
	return &Repo{
		Objects:       NewObjectStore(),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*LabelRec),
		Issues:        make(map[int]*IssueRec),
		Pulls:         make(map[int]*PullRec),
		CreatedAt:     isoNow(),
		ID:            id,
		Name:          name,
		Owner:         owner,
		Description:   description,
		IsPrivate:     isPrivate,
		DefaultBranch: "main",
		NextNumber:    1,
	}
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (r *Repo) NextSeq() int {
	r.seqCounter++
	return r.seqCounter
}

func (r *Repo) HeadRef() string {
	return "refs/heads/" + r.DefaultBranch
}

// TipOf returns the tip commit of a branch, or "" when the branch does not exist.
func (r *Repo) TipOf(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

func (r *Repo) TreeOfCommit(commitSha string) string {
	obj := r.Objects.Get(commitSha)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

// TipTree returns the tree of a branch's tip, or "" when there is none.
func (r *Repo) TipTree(branch string) string {
	tip := r.TipOf(branch)
	if tip == "" {
		return ""
	}
	return r.TreeOfCommit(tip)
}

// ------------------------------------------------------------------ tree helpers

type TreeHit struct {
	Sha  string
	Mode string
}

// treeLookup walks a tree by path segments; returns the entry's sha and mode, or nil.
func treeLookup(store *ObjectStore, treeSha string, segments []string) *TreeHit {
	if treeSha == "" {
		return nil
	}
	current := treeSha
	for i, segment := range segments {
		obj := store.Get(current)
		if obj == nil || obj.Type != "tree" {
			return nil
		}
		var entry *TreeEntry
		for _, candidate := range parseTree(obj.Data) {
			if candidate.Name == segment {
				c := candidate
				entry = &c
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(segments)-1 {
			return &TreeHit{Sha: entry.Sha, Mode: entry.Mode}
		}
		if entry.Mode != "40000" {
			return nil
		}
		current = entry.Sha
	}
	return nil
}

// flattenTree flattens a tree into a map of full path -> blob sha (files only).
func flattenTree(store *ObjectStore, treeSha string) map[string]string {
	out := make(map[string]string)
	var walk func(sha, prefix string)
	walk = func(sha, prefix string) {
		obj := store.Get(sha)
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, entry := range parseTree(obj.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Sha, path)
			} else {
				out[path] = entry.Sha
			}
		}
	}
	if treeSha != "" {
		walk(treeSha, "")
	}
	return out
}

type flatNode struct {
	files map[string]string
	dirs  map[string]*flatNode
}

// buildTreeFromFlat builds (and stores) nested trees from a flat path -> blob sha map.
func buildTreeFromFlat(store *ObjectStore, flat map[string]string) string {
	root := &flatNode{files: make(map[string]string), dirs: make(map[string]*flatNode)}
	for path, sha := range flat {
		segments := strings.Split(path, "/")
		node := root
		for _, segment := range segments[:len(segments)-1] {
			child, ok := node.dirs[segment]
			if !ok {
				child = &flatNode{files: make(map[string]string), dirs: make(map[string]*flatNode)}
				node.dirs[segment] = child
			}
			node = child
		}
		node.files[segments[len(segments)-1]] = sha
	}
	var build func(node *flatNode) string
	build = func(node *flatNode) string {
		var entries []TreeEntry
		for name, sha := range node.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, Sha: sha})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Sha: build(child)})
		}
		return store.Write("tree", serializeTree(entries))
	}
	return build(root)
}

// updateTree returns a new tree sha with segments set to blobSha (or removed when blobSha is "").
func updateTree(store *ObjectStore, treeSha string, segments []string, blobSha string) string {
	var entries []TreeEntry
	if treeSha != "" {
		if obj := store.Get(treeSha); obj != nil {
			entries = parseTree(obj.Data)
		}
	}
	head, rest := segments[0], segments[1:]
	index := -1
	for i, entry := range entries {
		if entry.Name == head {
			index = i
			break
		}
	}
	removeAt := func() {
		if index >= 0 {
			entries = append(entries[:index], entries[index+1:]...)
		}
	}
	if len(rest) == 0 {
		if blobSha == "" {
			removeAt()
		} else if index >= 0 {
			entries[index] = TreeEntry{Mode: "100644", Name: head, Sha: blobSha}
		} else {
			entries = append(entries, TreeEntry{Mode: "100644", Name: head, Sha: blobSha})
		}
	} else {
		childTree := ""
		if index >= 0 && entries[index].Mode == "40000" {
			childTree = entries[index].Sha
		}
		newChild := updateTree(store, childTree, rest, blobSha)
		childEmpty := false
		if obj := store.Get(newChild); obj != nil && len(obj.Data) == 0 {
			childEmpty = true
		}
		if childEmpty {
			removeAt()
		} else if index >= 0 {
			entries[index] = TreeEntry{Mode: "40000", Name: head, Sha: newChild}
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: head, Sha: newChild})
		}
	}
	return store.Write("tree", serializeTree(entries))
}

// ------------------------------------------------------------------ the forge

type StaleShaError struct{ msg string }

func (e *StaleShaError) Error() string { return e.msg }

type NotFoundError struct{ msg string }

func (e *NotFoundError) Error() string { return e.msg }

type Forge struct {
	Repos         map[string]*Repo
	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	Mu            sync.Mutex // guards every surface: the store serves one request at a time
}

func NewForge() *Forge {
	return &Forge{Repos: make(map[string]*Repo), nextRepoID: 1, nextIssueID: 1, nextCommentID: 1}
}

func (f *Forge) repoKey(owner, name string) string {
	return owner + "/" + name
}

// GetRepo returns the repository this token may see: only the viewer's own; anything else does
// not exist.
func (f *Forge) GetRepo(owner, name string) *Repo {
	if owner != VIEWER.Login {
		return nil
	}
	return f.Repos[f.repoKey(owner, name)]
}

// CreateRepo creates a repository, returning nil plus false when the name is taken... it returns
// (repo, true) on success and (nil, false) when a repo of that name already exists.
func (f *Forge) CreateRepo(name, description string, isPrivate bool) (*Repo, bool) {
	key := f.repoKey(VIEWER.Login, name)
	if _, exists := f.Repos[key]; exists {
		return nil, false
	}
	repo := NewRepo(f.nextRepoID, name, VIEWER.Login, description, isPrivate)
	f.nextRepoID++
	f.Repos[key] = repo
	return repo, true
}

func (f *Forge) DeleteRepo(owner, name string) bool {
	if owner != VIEWER.Login {
		return false
	}
	key := f.repoKey(owner, name)
	if _, exists := f.Repos[key]; !exists {
		return false
	}
	delete(f.Repos, key)
	return true
}

func (f *Forge) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(f.Repos))
	for _, repo := range f.Repos {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *Forge) AllocIssueID() int {
	id := f.nextIssueID
	f.nextIssueID++
	return id
}

func (f *Forge) AllocCommentID() int {
	id := f.nextCommentID
	f.nextCommentID++
	return id
}

// WriteFile creates or deletes the file at path on branch as a new commit. When the branch does
// not exist yet it is started from createFrom's tip (or as a root commit when the repository is
// empty). expectSha, when non-nil, is the blob id the caller believes the file has now: a
// mismatch is a StaleShaError, and a missing file with an expectation is stale too. A nil content
// deletes the file. Returns (commitSha, blobSha or "", created, err).
func (f *Forge) WriteFile(repo *Repo, branch, path string, content []byte, message string, expectSha *string, createFrom string) (string, string, bool, error) {
	ref := "refs/heads/" + branch
	tip, hasTip := repo.Refs[ref]
	if !hasTip && createFrom != "" {
		tip, hasTip = repo.Refs["refs/heads/"+createFrom]
	}
	tipTree := ""
	if hasTip {
		tipTree = repo.TreeOfCommit(tip)
	}
	segments := strings.Split(path, "/")
	existing := treeLookup(repo.Objects, tipTree, segments)
	if expectSha != nil {
		existingSha := ""
		if existing != nil {
			existingSha = existing.Sha
		}
		if existingSha != *expectSha {
			return "", "", false, &StaleShaError{msg: fmt.Sprintf("the file's sha is %s, not %s", existingSha, *expectSha)}
		}
	}
	if content == nil && existing == nil {
		return "", "", false, &NotFoundError{msg: "no file at " + path}
	}
	blobSha := ""
	if content != nil {
		blobSha = repo.Objects.Write("blob", content)
	}
	newTree := updateTree(repo.Objects, tipTree, segments, blobSha)
	now := time.Now().Unix()
	who := ident(VIEWER.Name, VIEWER.Email, now)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitSha := repo.Objects.Write("commit", serializeCommit(CommitInfo{
		Tree:      newTree,
		Parents:   parents,
		Author:    who,
		Committer: who,
		Message:   message,
	}))
	repo.Refs[ref] = commitSha
	return commitSha, blobSha, existing == nil, nil
}

// ------------------------------------------------------------------ merges and diffs

// mergeBase returns the best common ancestor of two commits, or "" when they share no history.
func mergeBase(store *ObjectStore, a, b string) string {
	ancestorsOfA := make(map[string]bool)
	for _, sha := range ancestors(store, a) {
		ancestorsOfA[sha] = true
	}
	for _, candidate := range ancestors(store, b) {
		if ancestorsOfA[candidate] {
			return candidate
		}
	}
	return ""
}

type MergeOutcome struct {
	Conflict bool
	TreeSha  string
}

// threeWayMerge merges flattened trees path-wise.
func threeWayMerge(store *ObjectStore, baseTree, ourTree, theirTree string) MergeOutcome {
	base := flattenTree(store, baseTree)
	ours := flattenTree(store, ourTree)
	theirs := flattenTree(store, theirTree)
	result := make(map[string]string)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	for path := range paths {
		b, o, t := base[path], ours[path], theirs[path]
		switch {
		case o == t:
			if o != "" {
				result[path] = o
			}
		case b == o:
			if t != "" {
				result[path] = t
			}
		case b == t:
			if o != "" {
				result[path] = o
			}
		default:
			return MergeOutcome{Conflict: true}
		}
	}
	return MergeOutcome{TreeSha: buildTreeFromFlat(store, result)}
}

// pullMergeable reports whether the pull request (head into base, at their current tips) merges
// cleanly.
func pullMergeable(repo *Repo, pull *PullRec) bool {
	headTip := repo.TipOf(pull.HeadRef)
	baseTip := repo.TipOf(pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return false
	}
	if isAncestor(repo.Objects, headTip, baseTip) {
		return true // already contained
	}
	base := mergeBase(repo.Objects, baseTip, headTip)
	baseTree := ""
	if base != "" {
		baseTree = repo.TreeOfCommit(base)
	}
	outcome := threeWayMerge(repo.Objects, baseTree, repo.TreeOfCommit(baseTip), repo.TreeOfCommit(headTip))
	return !outcome.Conflict
}

// squashMerge squash-merges the pull request onto its base branch: one commit whose tree is the
// three-way merge, parented on the base tip. Returns the new commit sha, or "" on conflict.
func squashMerge(repo *Repo, pull *PullRec) string {
	headTip := repo.TipOf(pull.HeadRef)
	baseTip := repo.TipOf(pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return ""
	}
	base := mergeBase(repo.Objects, baseTip, headTip)
	baseTree := ""
	if base != "" {
		baseTree = repo.TreeOfCommit(base)
	}
	outcome := threeWayMerge(repo.Objects, baseTree, repo.TreeOfCommit(baseTip), repo.TreeOfCommit(headTip))
	if outcome.Conflict || outcome.TreeSha == "" {
		return ""
	}
	now := time.Now().Unix()
	who := ident(VIEWER.Name, VIEWER.Email, now)
	commitSha := repo.Objects.Write("commit", serializeCommit(CommitInfo{
		Tree:      outcome.TreeSha,
		Parents:   []string{baseTip},
		Author:    who,
		Committer: who,
		Message:   fmt.Sprintf("%s (#%d)\n", pull.Title, pull.Number),
	}))
	repo.Refs["refs/heads/"+pull.BaseRef] = commitSha
	return commitSha
}

type FileChange struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
}

// lineDiff counts the changed lines between two texts with a longest-common-subsequence diff.
func lineDiff(oldText, newText string) (int, int) {
	if oldText == newText {
		return 0, 0
	}
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	// LCS over lines; the texts the suite diffs are small, and a cap keeps a pathological pair cheap.
	if len(a)*len(b) > 4_000_000 {
		return len(b), len(a)
	}
	width := len(b) + 1
	dp := make([]uint32, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i*width+j] = dp[(i+1)*width+j+1] + 1
			} else if dp[(i+1)*width+j] > dp[i*width+j+1] {
				dp[i*width+j] = dp[(i+1)*width+j]
			} else {
				dp[i*width+j] = dp[i*width+j+1]
			}
		}
	}
	common := int(dp[0])
	return len(b) - common, len(a) - common
}

// pullFiles returns the files a pull request changes: merge-base tree versus head tree.
func pullFiles(repo *Repo, pull *PullRec) []FileChange {
	headTip := repo.TipOf(pull.HeadRef)
	baseTip := repo.TipOf(pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return nil
	}
	base := mergeBase(repo.Objects, baseTip, headTip)
	fromTree := ""
	if base != "" {
		fromTree = repo.TreeOfCommit(base)
	}
	from := flattenTree(repo.Objects, fromTree)
	to := flattenTree(repo.Objects, repo.TreeOfCommit(headTip))
	pathSet := make(map[string]bool)
	for p := range from {
		pathSet[p] = true
	}
	for p := range to {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		before, after := from[path], to[path]
		if before == after {
			continue
		}
		oldText, newText := "", ""
		if before != "" {
			if obj := repo.Objects.Get(before); obj != nil {
				oldText = string(obj.Data)
			}
		}
		if after != "" {
			if obj := repo.Objects.Get(after); obj != nil {
				newText = string(obj.Data)
			}
		}
		var additions, deletions int
		var status string
		switch {
		case before == "":
			status = "added"
			if newText != "" {
				additions = len(strings.Split(newText, "\n"))
			}
		case after == "":
			status = "removed"
			if oldText != "" {
				deletions = len(strings.Split(oldText, "\n"))
			}
		default:
			status = "modified"
			additions, deletions = lineDiff(oldText, newText)
		}
		changes = append(changes, FileChange{
			Filename:  path,
			Status:    status,
			Additions: additions,
			Deletions: deletions,
			Changes:   additions + deletions,
		})
	}
	return changes
}
