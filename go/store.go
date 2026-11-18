package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ---------- viewer identity (fixed by the scenario) ----------

const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"

// ---------- domain types ----------

type Label struct {
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID         int64
	Body       string
	UserLogin  string
	UserID     int64
	CreatedSeq int64
	CreatedAt  string
	UpdatedAt  string
}

type Issue struct {
	ID         int64
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	UserLogin  string
	UserID     int64
	CreatedSeq int64
	CreatedAt  string
	UpdatedAt  string
	ClosedAt   string
	Comments   []*Comment
}

type Pull struct {
	ID             int64
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadBranch     string
	BaseBranch     string
	HeadSHA        string // at creation
	BaseSHA        string // at creation
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	CreatedSeq     int64
	CreatedAt      string
	UpdatedAt      string
}

type Repo struct {
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	DatabaseID    int64
	CreatedSeq    int64
	CreatedAt     string
	UpdatedAt     string
	Objects       map[string]Object
	Refs          map[string]string // full ref -> oid
	Issues        []*Issue
	NextIssueNum  int
	Pulls         []*Pull
	NextPullNum   int
	Labels        map[string]*Label
}

func (r *Repo) key() string { return r.Owner + "/" + r.Name }

// ---------- store ----------

type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo // "owner/name" -> repo
	order     []*Repo          // creation order
	nextDBID  int64
	nextSeq   int64
	viewer    string
	viewerID  int64
	token     string
}

func NewStore(token string) *Store {
	return &Store{
		repos:    map[string]*Repo{},
		viewer:   ViewerLogin,
		viewerID: ViewerID,
		token:    token,
	}
}

func (s *Store) nextCounter() int64 {
	s.nextSeq++
	return s.nextSeq
}

// ---------- objects ----------

func (s *Store) getObject(oid string) (Object, bool) {
	// objects live per-repo; search all repos (small store, fine)
	for _, r := range s.repos {
		if o, ok := r.Objects[oid]; ok {
			return o, true
		}
	}
	return Object{}, false
}

func (r *Repo) getObject(oid string) (Object, bool) {
	o, ok := r.Objects[oid]
	return o, ok
}

func (r *Repo) putObject(o Object) string {
	oid := hashObject(o.Type, o.Data)
	r.Objects[oid] = o
	return oid
}

// putObject stores on the repo (used by tree building).
func (s *Store) putObject(r *Repo, o Object) string { return r.putObject(o) }

// ---------- repos ----------

func (s *Store) createRepo(owner, name string, private bool, description string) (*Repo, bool) {
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	s.nextDBID++
	seq := s.nextCounter()
	r := &Repo{
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		DatabaseID:    s.nextDBID,
		CreatedSeq:    seq,
		CreatedAt:     nextTime(),
		UpdatedAt:     nextTime(),
		Objects:       map[string]Object{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		NextIssueNum:  1,
		NextPullNum:   1,
	}
	s.repos[key] = r
	s.order = append(s.order, r)
	return r, true
}

func (s *Store) getRepo(owner, name string) (*Repo, bool) {
	r, ok := s.repos[owner+"/"+name]
	return r, ok
}

func (s *Store) deleteRepo(owner, name string) bool {
	key := owner + "/" + name
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	for i, r := range s.order {
		if r.key() == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

func (s *Store) listRepos(owner string) []*Repo {
	var out []*Repo
	for _, r := range s.order {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out
}

// ---------- refs ----------

// resolveRef resolves a branch name, full ref, or oid to a commit oid.
func (r *Repo) resolveRef(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	if len(ref) == 40 && isHex(ref) {
		if _, ok := r.Objects[ref]; ok {
			return ref, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, "refs/") {
		oid, ok := r.Refs[ref]
		return oid, ok
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if oid, ok := r.Refs["refs/tags/"+ref]; ok {
		return oid, true
	}
	return "", false
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ensureBranch returns the tip of branch, creating the branch at the default
// branch tip if it does not exist yet (and the default exists). Returns "" if
// the repo is unborn.
func (r *Repo) ensureBranch(branch string) string {
	ref := "refs/heads/" + branch
	if tip, ok := r.Refs[ref]; ok {
		return tip
	}
	if defTip, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		r.Refs[ref] = defTip
		return defTip
	}
	return ""
}

// ---------- commits ----------

type HistoryEntry struct {
	OID     string
	Commit  CommitInfo
}

func (r *Repo) history(tip string) []HistoryEntry {
	var out []HistoryEntry
	seen := map[string]bool{}
	// DFS pre-order: newest first (parents after children)
	var walk func(oid string)
	walk = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		seen[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			return
		}
		c := parseCommit(obj.Data)
		out = append(out, HistoryEntry{OID: oid, Commit: c})
		for _, p := range c.Parents {
			walk(p)
		}
	}
	walk(tip)
	return out
}

// ---------- contents ----------

// serverIdent is the ident line for server-built commits.
func serverIdent() string {
	return fmt.Sprintf("%s <%s> %s", ViewerName, ViewerEmail, gitTime(trialNowSec))
}

// commitContents creates a commit on branch with the given flat file map
// change applied. files is the new full flat map (after the change).
// parentTip is the current tip ("" for a root commit).
func (s *Store) commitContents(r *Repo, branch, parentTip, message string, files map[string]FileEntry) string {
	treeOID := buildTreeFromFiles(r, files)
	parents := []string{}
	if parentTip != "" {
		parents = append(parents, parentTip)
	}
	data := buildCommit(treeOID, parents, serverIdent(), serverIdent(), message)
	oid := r.putObject(Object{Type: ObjCommit, Data: data})
	r.Refs["refs/heads/"+branch] = oid
	return oid
}

// putContent implements PUT contents: create or update a file.
// Returns (commitOID, status, errKind) where status is 201/200 and errKind is
// "" or "stale" (409) or "notfound".
func (s *Store) putContent(r *Repo, branch, path string, content []byte, message string, expectedSHA string, hasSHA bool) (string, int, string) {
	tip := r.ensureBranch(branch)
	files := map[string]FileEntry{}
	var parentTree string
	if tip != "" {
		obj, ok := r.Objects[tip]
		if !ok || obj.Type != ObjCommit {
			return "", 0, "notfound"
		}
		parentTree = parseCommit(obj.Data).Tree
		files = flattenTree(r, parentTree)
	}
	blobOID := r.putObject(Object{Type: ObjBlob, Data: content})
	existing, exists := files[path]
	if hasSHA {
		if !exists || existing.OID != expectedSHA {
			return "", 0, "stale"
		}
	}
	status := 201
	if exists {
		status = 200
	}
	files[path] = FileEntry{Mode: "100644", OID: blobOID}
	commitOID := s.commitContents(r, branch, tip, message, files)
	return commitOID, status, ""
}

// deleteContent implements DELETE contents. Returns (commitOID, errKind).
func (s *Store) deleteContent(r *Repo, branch, path, message, expectedSHA string) (string, string) {
	tip, ok := r.Refs["refs/heads/"+branch]
	if !ok {
		return "", "notfound"
	}
	obj, ok := r.Objects[tip]
	if !ok || obj.Type != ObjCommit {
		return "", "notfound"
	}
	files := flattenTree(r, parseCommit(obj.Data).Tree)
	existing, exists := files[path]
	if !exists {
		return "", "notfound"
	}
	if expectedSHA != "" && existing.OID != expectedSHA {
		return "", "stale"
	}
	delete(files, path)
	commitOID := s.commitContents(r, branch, tip, message, files)
	return commitOID, ""
}

// ---------- issues ----------

func (s *Store) createIssue(r *Repo, title, body string, labels []string) (*Issue, string) {
	for _, l := range labels {
		if _, ok := r.Labels[l]; !ok {
			return nil, l
		}
	}
	seq := s.nextCounter()
	iss := &Issue{
		ID:         nextInternalID(),
		Number:     r.NextIssueNum,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string{}, labels...),
		UserLogin:  ViewerLogin,
		UserID:     ViewerID,
		CreatedSeq: seq,
		CreatedAt:  nextTime(),
		UpdatedAt:  nextTime(),
	}
	r.NextIssueNum++
	r.Issues = append(r.Issues, iss)
	return iss, ""
}

func (r *Repo) getIssue(n int) *Issue {
	for _, iss := range r.Issues {
		if iss.Number == n {
			return iss
		}
	}
	return nil
}

// ---------- pulls ----------

func (s *Store) createPull(r *Repo, title, head, base, body string) (*Pull, string) {
	headTip, ok := r.Refs["refs/heads/"+head]
	if !ok {
		return nil, "head branch not found"
	}
	baseTip, ok := r.Refs["refs/heads/"+base]
	if !ok {
		return nil, "base branch not found"
	}
	for _, p := range r.Pulls {
		if p.State == "open" && p.HeadBranch == head && p.BaseBranch == base {
			return nil, "duplicate"
		}
	}
	seq := s.nextCounter()
	pr := &Pull{
		ID:         nextInternalID(),
		Number:     r.NextPullNum,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadBranch: head,
		BaseBranch: base,
		HeadSHA:    headTip,
		BaseSHA:    baseTip,
		CreatedSeq: seq,
		CreatedAt:  nextTime(),
		UpdatedAt:  nextTime(),
	}
	r.NextPullNum++
	r.Pulls = append(r.Pulls, pr)
	return pr, ""
}

func (r *Repo) getPull(n int) *Pull {
	for _, p := range r.Pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}

// pullMergeable computes whether a pull merges cleanly (3-way merge).
func (s *Store) pullMergeable(r *Repo, pr *Pull) bool {
	_, conflict := s.mergePreview(r, pr)
	return !conflict
}

// mergePreview computes the merged flat file map and whether it conflicts.
func (s *Store) mergePreview(r *Repo, pr *Pull) (map[string]FileEntry, bool) {
	headTip, ok := r.Refs["refs/heads/"+pr.HeadBranch]
	if !ok {
		return nil, true
	}
	baseTip, ok := r.Refs["refs/heads/"+pr.BaseBranch]
	if !ok {
		return nil, true
	}
	headObj, ok := r.Objects[headTip]
	if !ok || headObj.Type != ObjCommit {
		return nil, true
	}
	baseObj, ok := r.Objects[baseTip]
	if !ok || baseObj.Type != ObjCommit {
		return nil, true
	}
	mb := mergeBase(r, headTip, baseTip)
	if mb == "" {
		// no common ancestor: treat as conflict unless one side is empty
		return nil, true
	}
	mbObj, ok := r.Objects[mb]
	if !ok || mbObj.Type != ObjCommit {
		return nil, true
	}
	baseFiles := flattenTree(r, parseCommit(mbObj.Data).Tree)
	headFiles := flattenTree(r, parseCommit(headObj.Data).Tree)
	tipFiles := flattenTree(r, parseCommit(baseObj.Data).Tree)
	merged := map[string]FileEntry{}
	conflict := false
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range tipFiles {
		paths[p] = true
	}
	for p := range paths {
		b, hasB := baseFiles[p]
		h, hasH := headFiles[p]
		t, hasT := tipFiles[p]
		// head changed?
		headChanged := !(hasH == hasB && (!hasH || h.OID == b.OID))
		tipChanged := !(hasT == hasB && (!hasT || t.OID == b.OID))
		if !headChanged {
			if hasT {
				merged[p] = t
			}
			continue
		}
		if !tipChanged {
			if hasH {
				merged[p] = h
			}
			continue
		}
		// both changed
		if hasH == hasT && (!hasH || h.OID == t.OID) {
			if hasH {
				merged[p] = h
			}
			continue
		}
		conflict = true
	}
	return merged, conflict
}

// mergePull performs a squash merge. Returns (mergeCommitOID, status)
// where status is 200 ok, 404 unknown, 405 conflict.
func (s *Store) mergePull(r *Repo, pr *Pull, method string) (string, int) {
	if pr.State != "open" {
		return "", 404
	}
	merged, conflict := s.mergePreview(r, pr)
	if conflict {
		return "", 405
	}
	headTip := r.Refs["refs/heads/"+pr.HeadBranch]
	baseTip := r.Refs["refs/heads/"+pr.BaseBranch]
	treeOID := buildTreeFromFiles(r, merged)
	msg := pr.Title
	if pr.Body != "" {
		msg += "\n\n" + pr.Body
	}
	data := buildCommit(treeOID, []string{baseTip}, serverIdent(), serverIdent(), msg)
	mergeOID := r.putObject(Object{Type: ObjCommit, Data: data})
	r.Refs["refs/heads/"+pr.BaseBranch] = mergeOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = nextTime()
	pr.MergeCommitSHA = mergeOID
	pr.UpdatedAt = nextTime()
	_ = headTip
	return mergeOID, 200
}

// pullFiles diffs merge-base tree vs head tree.
type FileChange struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
}

func (s *Store) pullFiles(r *Repo, pr *Pull) []FileChange {
	headTip, ok := r.Refs["refs/heads/"+pr.HeadBranch]
	if !ok {
		return nil
	}
	baseTip, ok := r.Refs["refs/heads/"+pr.BaseBranch]
	if !ok {
		return nil
	}
	mb := mergeBase(r, headTip, baseTip)
	var baseFiles map[string]FileEntry
	if mb != "" {
		if mbObj, ok := r.Objects[mb]; ok && mbObj.Type == ObjCommit {
			baseFiles = flattenTree(r, parseCommit(mbObj.Data).Tree)
		}
	}
	if baseFiles == nil {
		baseFiles = map[string]FileEntry{}
	}
	headObj, ok := r.Objects[headTip]
	if !ok || headObj.Type != ObjCommit {
		return nil
	}
	headFiles := flattenTree(r, parseCommit(headObj.Data).Tree)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	var out []FileChange
	for p := range paths {
		b, hasB := baseFiles[p]
		h, hasH := headFiles[p]
		if hasB == hasH && (!hasB || b.OID == h.OID) {
			continue
		}
		fc := FileChange{Filename: p}
		switch {
		case !hasB && hasH:
			fc.Status = "added"
			if obj, ok := r.Objects[h.OID]; ok {
				fc.Additions = lineCount(string(obj.Data))
				if fc.Additions < 1 {
					fc.Additions = 1
				}
			} else {
				fc.Additions = 1
			}
		case hasB && !hasH:
			fc.Status = "removed"
			if obj, ok := r.Objects[b.OID]; ok {
				fc.Deletions = lineCount(string(obj.Data))
			}
		default:
			fc.Status = "modified"
			var oldS, newS string
			if obj, ok := r.Objects[b.OID]; ok {
				oldS = string(obj.Data)
			}
			if obj, ok := r.Objects[h.OID]; ok {
				newS = string(obj.Data)
			}
			fc.Additions, fc.Deletions = diffLineCounts(oldS, newS)
		}
		fc.Changes = fc.Additions + fc.Deletions
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// diffLineCounts counts added/removed lines via multiset difference.
func diffLineCounts(oldS, newS string) (int, int) {
	oldLines := strings.Split(oldS, "\n")
	newLines := strings.Split(newS, "\n")
	count := func(lines []string) map[string]int {
		m := map[string]int{}
		for _, l := range lines {
			m[l]++
		}
		return m
	}
	om, nm := count(oldLines), count(newLines)
	add, del := 0, 0
	for l, c := range nm {
		if c > om[l] {
			add += c - om[l]
		}
	}
	for l, c := range om {
		if c > nm[l] {
			del += c - nm[l]
		}
	}
	return add, del
}

// ---------- sorted helpers ----------

func sortedRepoNames(repos []*Repo) []string {
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}
