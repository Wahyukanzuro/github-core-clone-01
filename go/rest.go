package main

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------- REST v3 ----------

func (s *Server) dispatchRest(req *Request) *Response {
	if !s.checkAuth(req) {
		return s.restErr(401, "Bad credentials")
	}
	path := strings.TrimPrefix(req.Path, "/api/v3")
	if path == "" || path == "/" {
		if req.Method != "GET" {
			return s.restErr(404, "Not Found")
		}
		resp := jsonResponse(200, J{
			"current_user_url": "http://example.invalid/api/v3/user",
			"repository_url":   "http://example.invalid/api/v3/repos/{owner}/{repo}",
		})
		resp.Headers["X-OAuth-Scopes"] = "repo, read:org"
		return s.withReqID(req, resp)
	}
	segments := splitPath(path)
	// /user, /user/repos
	if len(segments) >= 1 && segments[0] == "user" {
		if len(segments) == 1 {
			if req.Method != "GET" {
				return s.restErr(404, "Not Found")
			}
			return s.withReqID(req, jsonResponse(200, userJSON()))
		}
		if len(segments) == 2 && segments[1] == "repos" {
			if req.Method == "GET" {
				return s.withReqID(req, s.restListUserRepos(req))
			}
			if req.Method == "POST" {
				return s.withReqID(req, s.restCreateRepo(req))
			}
			return s.restErr(404, "Not Found")
		}
		return s.restErr(404, "Not Found")
	}
	// /meta
	if len(segments) == 1 && segments[0] == "meta" {
		if req.Method != "GET" {
			return s.restErr(404, "Not Found")
		}
		return s.withReqID(req, jsonResponse(200, J{"installed_version": "1.0.0"}))
	}
	// /repos/{owner}/{repo}[/...]
	if len(segments) >= 3 && segments[0] == "repos" {
		owner, repoName := segments[1], segments[2]
		rest := segments[3:]
		return s.withReqID(req, s.dispatchRepo(req, owner, repoName, rest))
	}
	return s.restErr(404, "Not Found")
}

func (s *Server) withReqID(req *Request, resp *Response) *Response {
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers["X-GitHub-Request-Id"] = nextRequestIDFast()
	return resp
}

// nextRequestIDFast renders "req-%08x" without fmt.Sprintf.
func nextRequestIDFast() string {
	n := atomic.AddInt64(&reqCounter, 1)
	var buf [12]byte
	copy(buf[0:], "req-")
	const hexd = "0123456789abcdef"
	for i := 0; i < 8; i++ {
		buf[11-i] = hexd[n&0xf]
		n >>= 4
	}
	return string(buf[:])
}

func (s *Server) restErr(status int, msg string) *Response {
	return s.withReqID(nil, errResponse(status, msg))
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if u, err := url.PathUnescape(seg); err == nil {
			out = append(out, u)
		} else {
			out = append(out, seg)
		}
	}
	return out
}

func userJSON() J {
	return J{
		"login":      ViewerLogin,
		"id":         ViewerID,
		"name":       ViewerName,
		"email":      ViewerEmail,
		"type":       "User",
		"avatar_url": "http://example.invalid/avatar/" + ViewerLogin,
		"html_url":   "http://example.invalid/" + ViewerLogin,
	}
}

func repoJSON(r *Repo) J {
	return J{
		"id":             r.DatabaseID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"private":        r.Private,
		"owner":          J{"login": r.Owner, "id": ViewerID},
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
		"created_at":     r.CreatedAt,
		"updated_at":     r.UpdatedAt,
		"pushed_at":      r.UpdatedAt,
		"html_url":       "http://example.invalid/" + r.Owner + "/" + r.Name,
		"clone_url":      "http://example.invalid/" + r.Owner + "/" + r.Name + ".git",
		"has_issues":     true,
		"has_wiki":       false,
		"has_pages":      false,
		"forks_count":    0,
		"watchers_count": 0,
	}
}

// issueJSON renders one issue. A typed struct marshals far faster than the
// generic map the other renderers use, and the parsed JSON is identical.
type issueLabelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueUserJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type issueJSONOut struct {
	ID        int64            `json:"id"`
	Number    int              `json:"number"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	State     string           `json:"state"`
	Labels    []issueLabelJSON `json:"labels"`
	User      issueUserJSON    `json:"user"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
	ClosedAt  interface{}      `json:"closed_at"`
	Comments  int              `json:"comments"`
	HTMLURL   string           `json:"html_url"`
}

func issueJSON(iss *Issue, r *Repo) issueJSONOut {
	labels := make([]issueLabelJSON, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		color := "ededed"
		desc := ""
		if l, ok := r.Labels[name]; ok {
			color = l.Color
			desc = l.Description
		}
		labels = append(labels, issueLabelJSON{Name: name, Color: color, Description: desc})
	}
	closedAt := interface{}(nil)
	if iss.ClosedAt != "" {
		closedAt = iss.ClosedAt
	}
	return issueJSONOut{
		ID:        iss.ID,
		Number:    iss.Number,
		Title:     iss.Title,
		Body:      iss.Body,
		State:     iss.State,
		Labels:    labels,
		User:      issueUserJSON{Login: iss.UserLogin, ID: iss.UserID},
		CreatedAt: iss.CreatedAt,
		UpdatedAt: iss.UpdatedAt,
		ClosedAt:  closedAt,
		Comments:  len(iss.Comments),
		HTMLURL:   "http://example.invalid/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(iss.Number),
	}
}

// getIssueFast finds an issue by number in O(1): issue numbers are dense
// from 1 in creation order, so number n lives at index n-1; anything else
// falls back to the linear scan.
func getIssueFast(r *Repo, n int) *Issue {
	if n >= 1 && n <= len(r.Issues) {
		if iss := r.Issues[n-1]; iss.Number == n {
			return iss
		}
	}
	return r.getIssue(n)
}

func commentJSON(c *Comment, r *Repo, issueNum int) J {
	return J{
		"id":         c.ID,
		"body":       c.Body,
		"user":       J{"login": c.UserLogin, "id": c.UserID},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
		"html_url":   "http://example.invalid/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(issueNum) + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
	}
}

func pullJSON(pr *Pull, r *Repo) J {
	headSHA := pr.HeadSHA
	if tip, ok := r.Refs["refs/heads/"+pr.HeadBranch]; ok {
		headSHA = tip
	}
	baseSHA := pr.BaseSHA
	if tip, ok := r.Refs["refs/heads/"+pr.BaseBranch]; ok {
		baseSHA = tip
	}
	mergedAt := interface{}(nil)
	if pr.MergedAt != "" {
		mergedAt = pr.MergedAt
	}
	mergeSHA := interface{}(nil)
	if pr.MergeCommitSHA != "" {
		mergeSHA = pr.MergeCommitSHA
	}
	return J{
		"id":               pr.ID,
		"number":           pr.Number,
		"state":            pr.State,
		"title":            pr.Title,
		"body":             pr.Body,
		"user":             J{"login": ViewerLogin, "id": ViewerID},
		"head":             J{"ref": pr.HeadBranch, "sha": headSHA, "label": r.Owner + ":" + pr.HeadBranch, "user": J{"login": r.Owner, "id": ViewerID}},
		"base":             J{"ref": pr.BaseBranch, "sha": baseSHA, "label": r.Owner + ":" + pr.BaseBranch, "user": J{"login": r.Owner, "id": ViewerID}},
		"merged":           pr.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"mergeable":        true,
		"created_at":       pr.CreatedAt,
		"updated_at":       pr.UpdatedAt,
		"html_url":         "http://example.invalid/" + r.Owner + "/" + r.Name + "/pull/" + strconv.Itoa(pr.Number),
		"draft":            false,
	}
}

// ---------- pagination ----------

type pageParams struct {
	page    int
	perPage int
}

func parsePage(req *Request) pageParams {
	p := pageParams{page: 1, perPage: 30}
	if v := req.Query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			p.page = n
		}
	}
	if v := req.Query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > 100 {
				n = 100
			}
			p.perPage = n
		}
	}
	return p
}

// paginate slices items and builds a Link header when a next page exists.
func (s *Server) paginate(req *Request, total int, pp pageParams) (start, end int, link string) {
	start = (pp.page - 1) * pp.perPage
	if start > total {
		start = total
	}
	end = start + pp.perPage
	if end > total {
		end = total
	}
	if end < total {
		q := url.Values{}
		q.Set("page", strconv.Itoa(pp.page+1))
		q.Set("per_page", strconv.Itoa(pp.perPage))
		// preserve other query params
		for k, vs := range req.Query {
			if k == "page" || k == "per_page" {
				continue
			}
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		host := req.Headers["host"]
		scheme := "http"
		abs := scheme + "://" + host + req.Path + "?" + q.Encode()
		link = "<" + abs + ">; rel=\"next\""
	}
	return start, end, link
}

func (s *Server) listResponse(req *Request, items []interface{}) *Response {
	pp := parsePage(req)
	start, end, link := s.paginate(req, len(items), pp)
	page := items[start:end]
	resp := jsonResponse(200, page)
	if link != "" {
		resp.Headers["Link"] = link
	}
	return resp
}

// ---------- /user/repos ----------

func (s *Server) restListUserRepos(req *Request) *Response {
	s.store.mu.RLock()
	repos := s.store.listRepos(ViewerLogin)
	s.store.mu.RUnlock()
	items := make([]interface{}, 0, len(repos))
	for _, r := range repos {
		items = append(items, repoJSON(r))
	}
	return s.listResponse(req, items)
}

func (s *Server) restCreateRepo(req *Request) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	name := strField(body, "name")
	if name == "" {
		return s.restErr(422, "name is required")
	}
	private := boolField(body, "private")
	desc := strField(body, "description")
	s.store.mu.Lock()
	_, created := s.store.createRepo(ViewerLogin, name, private, desc)
	s.store.mu.Unlock()
	if !created {
		return s.restErr(422, "name already exists on this account")
	}
	s.store.mu.RLock()
	repo, _ := s.store.getRepo(ViewerLogin, name)
	s.store.mu.RUnlock()
	return jsonResponse(201, repoJSON(repo))
}

// ---------- /repos/{owner}/{repo} ----------

func (s *Server) dispatchRepo(req *Request, owner, repoName string, rest []string) *Response {
	// load repo (read lock released before handler runs; handlers lock as needed)
	s.store.mu.RLock()
	repo, exists := s.store.getRepo(owner, repoName)
	s.store.mu.RUnlock()
	if !exists {
		return s.restErr(404, "Not Found")
	}
	if len(rest) == 0 {
		switch req.Method {
		case "GET":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			return jsonResponse(200, repoJSON(repo))
		case "PATCH":
			body, ok := parseJSONBody(req.Body)
			if !ok {
				return s.restErr(400, "Problems parsing JSON")
			}
			s.store.mu.Lock()
			if v, present := body["description"]; present {
				if str, isStr := v.(string); isStr {
					repo.Description = str
				}
			}
			if v, present := body["default_branch"]; present {
				if str, isStr := v.(string); isStr && str != "" {
					repo.DefaultBranch = str
				}
			}
			repo.UpdatedAt = nextTime()
			s.store.mu.Unlock()
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			return jsonResponse(200, repoJSON(repo))
		case "DELETE":
			s.store.mu.Lock()
			s.store.deleteRepo(owner, repoName)
			s.store.mu.Unlock()
			return &Response{Status: 204, Headers: map[string]string{}}
		}
		return s.restErr(404, "Not Found")
	}
	switch rest[0] {
	case "branches":
		if len(rest) == 1 && req.Method == "GET" {
			return s.restListBranches(req, repo)
		}
		if len(rest) == 2 && req.Method == "GET" {
			return s.restGetBranch(req, repo, rest[1])
		}
	case "commits":
		if len(rest) == 1 && req.Method == "GET" {
			return s.restListCommits(req, repo)
		}
	case "contents":
		if len(rest) >= 2 {
			path := strings.Join(rest[1:], "/")
			switch req.Method {
			case "GET":
				return s.restGetContents(req, repo, path)
			case "PUT":
				return s.restPutContents(req, repo, path)
			case "DELETE":
				return s.restDeleteContents(req, repo, path)
			}
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 && req.Method == "POST" {
				return s.restCreateRef(req, repo)
			}
			if len(rest) >= 4 && rest[2] == "heads" && req.Method == "GET" {
				return s.restGetRef(req, repo, "refs/heads/"+strings.Join(rest[3:], "/"))
			}
			if len(rest) >= 4 && rest[2] == "tags" && req.Method == "GET" {
				return s.restGetRef(req, repo, "refs/tags/"+strings.Join(rest[3:], "/"))
			}
		}
	case "labels":
		if len(rest) == 1 && req.Method == "POST" {
			return s.restCreateLabel(req, repo)
		}
		if len(rest) == 1 && req.Method == "GET" {
			return s.restListLabels(req, repo)
		}
	case "issues":
		if len(rest) == 1 {
			if req.Method == "GET" {
				return s.restListIssues(req, repo)
			}
			if req.Method == "POST" {
				return s.restCreateIssue(req, repo)
			}
		}
		if len(rest) == 2 && req.Method == "GET" {
			return s.restGetIssue(req, repo, rest[1])
		}
		if len(rest) == 2 && req.Method == "PATCH" {
			return s.restPatchIssue(req, repo, rest[1])
		}
		if len(rest) == 3 && rest[2] == "comments" {
			if req.Method == "GET" {
				return s.restListComments(req, repo, rest[1])
			}
			if req.Method == "POST" {
				return s.restCreateComment(req, repo, rest[1])
			}
		}
	case "pulls":
		if len(rest) == 1 {
			if req.Method == "GET" {
				return s.restListPulls(req, repo)
			}
			if req.Method == "POST" {
				return s.restCreatePull(req, repo)
			}
		}
		if len(rest) == 2 && req.Method == "GET" {
			return s.restGetPull(req, repo, rest[1])
		}
		if len(rest) == 3 && rest[2] == "files" && req.Method == "GET" {
			return s.restPullFiles(req, repo, rest[1])
		}
		if len(rest) == 3 && rest[2] == "merge" && req.Method == "PUT" {
			return s.restMergePull(req, repo, rest[1])
		}
	}
	return s.restErr(404, "Not Found")
}

// ---------- branches / commits / refs ----------

func (s *Server) restListBranches(req *Request, repo *Repo) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	names := sortedRefNames(repo.Refs)
	var items []interface{}
	for _, ref := range names {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		items = append(items, J{"name": strings.TrimPrefix(ref, "refs/heads/"), "commit": J{"sha": repo.Refs[ref]}, "protected": false})
	}
	return jsonResponse(200, items)
}

func (s *Server) restGetBranch(req *Request, repo *Repo, branch string) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	tip, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		return s.restErr(404, "Branch not found")
	}
	return jsonResponse(200, J{"name": branch, "commit": J{"sha": tip}, "protected": false})
}

func (s *Server) restListCommits(req *Request, repo *Repo) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	sha := req.Query.Get("sha")
	var tip string
	if sha == "" {
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else {
		var ok bool
		tip, ok = repo.resolveRef(sha)
		if !ok {
			return s.restErr(404, "No commit found for SHA: "+sha)
		}
	}
	hist := repo.history(tip)
	items := make([]interface{}, 0, len(hist))
	for _, h := range hist {
		name, email := identNameEmail(h.Commit.Author)
		date := formatGitDate(h.Commit.AuthorTS)
		items = append(items, J{
			"sha": h.OID,
			"commit": J{
				"message": strings.TrimRight(h.Commit.Message, "\n"),
				"author":  J{"name": name, "email": email, "date": date},
				"committer": J{"name": name, "email": email, "date": date},
			},
			"author":    J{"login": ViewerLogin, "id": ViewerID},
			"committer": J{"login": ViewerLogin, "id": ViewerID},
			"parents":   parentsJSON(h.Commit.Parents),
		})
	}
	return s.listResponse(req, items)
}

func parentsJSON(parents []string) []interface{} {
	out := make([]interface{}, 0, len(parents))
	for _, p := range parents {
		out = append(out, J{"sha": p})
	}
	return out
}

func formatGitDate(ts int64) string {
	if ts <= 0 {
		return baseTime()
	}
	return timeFormatRFC3339(ts)
}

func (s *Server) restCreateRef(req *Request, repo *Repo) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	ref := strField(body, "ref")
	sha := strField(body, "sha")
	if ref == "" || sha == "" {
		return s.restErr(422, "ref and sha are required")
	}
	if _, exists := repo.Objects[sha]; !exists {
		return s.restErr(422, "Object does not exist")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if cur, exists := repo.Refs[ref]; exists {
		if cur == sha {
			return jsonResponse(201, refJSON(ref, sha, repo))
		}
		return s.restErr(422, "Reference already exists")
	}
	repo.Refs[ref] = sha
	return jsonResponse(201, refJSON(ref, sha, repo))
}

func refJSON(ref, sha string, repo *Repo) J {
	typ := "commit"
	if obj, ok := repo.Objects[sha]; ok {
		switch obj.Type {
		case ObjTag:
			typ = "tag"
		case ObjTree:
			typ = "tree"
		case ObjBlob:
			typ = "blob"
		}
	}
	return J{"ref": ref, "object": J{"sha": sha, "type": typ}}
}

func (s *Server) restGetRef(req *Request, repo *Repo, ref string) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	oid, ok := repo.Refs[ref]
	if !ok {
		return s.restErr(404, "Not Found")
	}
	return jsonResponse(200, refJSON(ref, oid, repo))
}

// ---------- contents ----------

func (s *Server) restGetContents(req *Request, repo *Repo, path string) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	ref := req.Query.Get("ref")
	var tip string
	if ref == "" {
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else {
		var ok bool
		tip, ok = repo.resolveRef(ref)
		if !ok {
			return s.restErr(404, "No commit found for SHA: "+ref)
		}
	}
	if tip == "" {
		return s.restErr(404, "This repository is empty.")
	}
	obj, ok := repo.Objects[tip]
	if !ok || obj.Type != ObjCommit {
		return s.restErr(404, "Not Found")
	}
	treeOID := parseCommit(obj.Data).Tree
	files := flattenTree(repo, treeOID)
	// exact file?
	if fe, ok := files[path]; ok {
		blob, _ := repo.Objects[fe.OID]
		return jsonResponse(200, J{
			"type":     "file",
			"encoding": "base64",
			"size":     len(blob.Data),
			"name":     baseName(path),
			"path":     path,
			"sha":      fe.OID,
			"content":  b64enc(blob.Data),
		})
	}
	// directory listing: immediate children of path
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]J{}
	for p, fe := range files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			continue
		}
		seg := strings.SplitN(rest, "/", 2)[0]
		if _, dup := seen[seg]; dup {
			continue
		}
		if strings.Contains(rest, "/") {
			// a directory; find its tree oid by walking the tree
			seen[seg] = J{"type": "dir", "name": seg, "path": prefix + seg, "sha": dirTreeOID(repo, treeOID, prefix+seg)}
		} else {
			seen[seg] = J{"type": "file", "name": seg, "path": prefix + seg, "sha": fe.OID, "size": len(repo.Objects[fe.OID].Data)}
		}
	}
	if len(seen) == 0 {
		return s.restErr(404, "Not Found")
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]interface{}, 0, len(names))
	for _, n := range names {
		items = append(items, seen[n])
	}
	return jsonResponse(200, items)
}

func dirTreeOID(repo *Repo, treeOID, path string) string {
	parts := strings.Split(path, "/")
	cur := treeOID
	for _, part := range parts {
		obj, ok := repo.Objects[cur]
		if !ok || obj.Type != ObjTree {
			return ZeroOID
		}
		found := ""
		for _, e := range parseTree(obj.Data) {
			if e.Name == part && e.IsDir {
				found = e.OID
				break
			}
		}
		if found == "" {
			return ZeroOID
		}
		cur = found
	}
	return cur
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (s *Server) restPutContents(req *Request, repo *Repo, path string) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	message := strField(body, "message")
	contentB64 := strField(body, "content")
	content, err := b64dec(contentB64)
	if err != nil {
		return s.restErr(422, "content is not valid base64")
	}
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha, hasSHA := body["sha"].(string)
	s.store.mu.Lock()
	commitOID, status, errKind := s.store.putContent(repo, branch, path, content, message, sha, hasSHA)
	if errKind == "stale" {
		s.store.mu.Unlock()
		return s.restErr(409, "sha does not match")
	}
	if errKind != "" {
		s.store.mu.Unlock()
		return s.restErr(404, "Not Found")
	}
	// recompute blob oid for the response
	blobOID := hashObject(ObjBlob, content)
	s.store.mu.Unlock()
	return jsonResponse(status, J{
		"content": J{"name": baseName(path), "path": path, "sha": blobOID, "size": len(content), "type": "file", "encoding": "base64"},
		"commit": J{
			"sha":     commitOID,
			"message": message,
			"author":  J{"name": ViewerName, "email": ViewerEmail, "date": baseTime()},
			"committer": J{"name": ViewerName, "email": ViewerEmail, "date": baseTime()},
		},
	})
}

func (s *Server) restDeleteContents(req *Request, repo *Repo, path string) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	message := strField(body, "message")
	sha := strField(body, "sha")
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	s.store.mu.Lock()
	commitOID, errKind := s.store.deleteContent(repo, branch, path, message, sha)
	s.store.mu.Unlock()
	if errKind == "stale" {
		return s.restErr(409, "sha does not match")
	}
	if errKind != "" {
		return s.restErr(404, "Not Found")
	}
	return jsonResponse(200, J{"commit": J{"sha": commitOID, "message": message}})
}

// ---------- labels ----------

func (s *Server) restCreateLabel(req *Request, repo *Repo) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	name := strField(body, "name")
	if name == "" {
		return s.restErr(422, "name is required")
	}
	color := strField(body, "color")
	if color == "" {
		color = "ededed"
	}
	desc := strField(body, "description")
	s.store.mu.Lock()
	repo.Labels[name] = &Label{Name: name, Color: color, Description: desc}
	s.store.mu.Unlock()
	return jsonResponse(201, J{"name": name, "color": color, "description": desc})
}

func (s *Server) restListLabels(req *Request, repo *Repo) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	names := make([]string, 0, len(repo.Labels))
	for n := range repo.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]interface{}, 0, len(names))
	for _, n := range names {
		l := repo.Labels[n]
		items = append(items, J{"name": l.Name, "color": l.Color, "description": l.Description})
	}
	return jsonResponse(200, items)
}

// ---------- issues ----------

func (s *Server) restListIssues(req *Request, repo *Repo) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	state := req.Query.Get("state")
	if state == "" {
		state = "open"
	}
	sortDir := req.Query.Get("direction")
	if sortDir == "" {
		sortDir = "desc"
	}
	issues := append([]*Issue{}, repo.Issues...)
	var filtered []*Issue
	for _, iss := range issues {
		if state == "all" || iss.State == state {
			filtered = append(filtered, iss)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if sortDir == "asc" {
			return filtered[i].CreatedSeq < filtered[j].CreatedSeq
		}
		return filtered[i].CreatedSeq > filtered[j].CreatedSeq
	})
	items := make([]interface{}, 0, len(filtered))
	for _, iss := range filtered {
		items = append(items, issueJSON(iss, repo))
	}
	return s.listResponse(req, items)
}

// createIssueBody is the typed decode for issue creation: a struct decode
// avoids the generic map the other endpoints use.
type createIssueBody struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

func (s *Server) restCreateIssue(req *Request, repo *Repo) *Response {
	var body createIssueBody
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return s.restErr(400, "Problems parsing JSON")
		}
	}
	title := body.Title
	issueBody := body.Body
	labels := body.Labels
	s.store.mu.Lock()
	iss, badLabel := s.store.createIssue(repo, title, issueBody, labels)
	s.store.mu.Unlock()
	if iss == nil {
		return s.restErr(422, "Label does not exist: "+badLabel)
	}
	return jsonResponse(201, issueJSON(iss, repo))
}

func (s *Server) restGetIssue(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	iss := getIssueFast(repo, n)
	if iss == nil {
		return s.restErr(404, "Not Found")
	}
	return jsonResponse(200, issueJSON(iss, repo))
}

func (s *Server) restPatchIssue(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	s.store.mu.Lock()
	iss := getIssueFast(repo, n)
	if iss == nil {
		s.store.mu.Unlock()
		return s.restErr(404, "Not Found")
	}
	if v, present := body["state"]; present {
		if str, isStr := v.(string); isStr && (str == "open" || str == "closed") {
			iss.State = str
			if str == "closed" {
				iss.ClosedAt = nextTime()
			} else {
				iss.ClosedAt = ""
			}
		}
	}
	if v, present := body["title"]; present {
		if str, isStr := v.(string); isStr {
			iss.Title = str
		}
	}
	if v, present := body["body"]; present {
		if str, isStr := v.(string); isStr {
			iss.Body = str
		}
	}
	if v, present := body["labels"]; present {
		if arr, isArr := v.([]interface{}); isArr {
			var labs []string
			for _, item := range arr {
				if str, isStr := item.(string); isStr {
					labs = append(labs, str)
				}
			}
			iss.Labels = labs
		}
	}
	iss.UpdatedAt = nextTime()
	s.store.mu.Unlock()
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return jsonResponse(200, issueJSON(iss, repo))
}

func (s *Server) restListComments(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	iss := getIssueFast(repo, n)
	if iss == nil {
		return s.restErr(404, "Not Found")
	}
	items := make([]interface{}, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		items = append(items, commentJSON(c, repo, n))
	}
	return s.listResponse(req, items)
}

func (s *Server) restCreateComment(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	commentBody := strField(body, "body")
	s.store.mu.Lock()
	iss := getIssueFast(repo, n)
	if iss == nil {
		s.store.mu.Unlock()
		return s.restErr(404, "Not Found")
	}
	c := &Comment{
		ID:         nextInternalID(),
		Body:       commentBody,
		UserLogin:  ViewerLogin,
		UserID:     ViewerID,
		CreatedSeq: s.store.nextCounter(),
		CreatedAt:  nextTime(),
		UpdatedAt:  nextTime(),
	}
	iss.Comments = append(iss.Comments, c)
	s.store.mu.Unlock()
	return jsonResponse(201, commentJSON(c, repo, n))
}

// ---------- pulls ----------

func (s *Server) restListPulls(req *Request, repo *Repo) *Response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	state := req.Query.Get("state")
	if state == "" {
		state = "open"
	}
	var filtered []*Pull
	for _, pr := range repo.Pulls {
		if state == "all" || pr.State == state {
			filtered = append(filtered, pr)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedSeq > filtered[j].CreatedSeq })
	items := make([]interface{}, 0, len(filtered))
	for _, pr := range filtered {
		items = append(items, pullJSON(pr, repo))
	}
	return s.listResponse(req, items)
}

func (s *Server) restCreatePull(req *Request, repo *Repo) *Response {
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	title := strField(body, "title")
	head := strField(body, "head")
	base := strField(body, "base")
	pullBody := strField(body, "body")
	if head == "" || base == "" {
		return s.restErr(422, "head and base are required")
	}
	s.store.mu.Lock()
	pr, errKind := s.store.createPull(repo, title, head, base, pullBody)
	s.store.mu.Unlock()
	if pr == nil {
		if errKind == "duplicate" {
			return s.restErr(422, "A pull request already exists for "+head)
		}
		return s.restErr(422, errKind)
	}
	return jsonResponse(201, pullJSON(pr, repo))
}

func (s *Server) restGetPull(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	pr := repo.getPull(n)
	if pr == nil {
		return s.restErr(404, "Not Found")
	}
	return jsonResponse(200, pullJSON(pr, repo))
}

func (s *Server) restPullFiles(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	pr := repo.getPull(n)
	if pr == nil {
		return s.restErr(404, "Not Found")
	}
	changes := s.store.pullFiles(repo, pr)
	items := make([]interface{}, 0, len(changes))
	for _, fc := range changes {
		items = append(items, J{
			"filename":  fc.Filename,
			"status":    fc.Status,
			"additions": fc.Additions,
			"deletions": fc.Deletions,
			"changes":   fc.Changes,
		})
	}
	return jsonResponse(200, items)
}

func (s *Server) restMergePull(req *Request, repo *Repo, numStr string) *Response {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.restErr(404, "Not Found")
	}
	body, ok := parseJSONBody(req.Body)
	if !ok {
		return s.restErr(400, "Problems parsing JSON")
	}
	_ = body
	s.store.mu.Lock()
	pr := repo.getPull(n)
	if pr == nil {
		s.store.mu.Unlock()
		return s.restErr(404, "Not Found")
	}
	mergeOID, status := s.store.mergePull(repo, pr, strField(body, "merge_method"))
	s.store.mu.Unlock()
	if status == 405 {
		return s.restErr(405, "Merge conflict")
	}
	if status != 200 {
		return s.restErr(404, "Not Found")
	}
	return jsonResponse(200, J{
		"sha":     mergeOID,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}
