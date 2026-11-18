package main

import (
	"strconv"
	"strings"
	"sync"
)

// ---------- MCP (JSON-RPC 2.0 over Streamable HTTP) ----------

const mcpRevision = "2026-07-28"

var mcpToolsJSON []byte
var mcpToolsOnce sync.Once

func mcpToolsBytes() []byte {
	mcpToolsOnce.Do(func() {
		tools := []J{
			{
				"name":        "create_repo",
				"description": "Create a new repository owned by the authenticated user.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"name":        J{"type": "string", "description": "Repository name"},
						"description": J{"type": "string", "description": "Repository description"},
						"private":     J{"type": "boolean", "description": "Whether the repository is private"},
					},
					"required": []string{"name"},
				},
			},
			{
				"name":        "get_repo",
				"description": "Get a repository by owner and name.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"owner": J{"type": "string", "description": "Repository owner login"},
						"repo":  J{"type": "string", "description": "Repository name"},
					},
					"required": []string{"owner", "repo"},
				},
			},
			{
				"name":        "create_issue",
				"description": "Create an issue in a repository.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"owner": J{"type": "string", "description": "Repository owner login"},
						"repo":  J{"type": "string", "description": "Repository name"},
						"title": J{"type": "string", "description": "Issue title"},
						"body":  J{"type": "string", "description": "Issue body"},
					},
					"required": []string{"owner", "repo", "title"},
				},
			},
			{
				"name":        "list_issues",
				"description": "List issues in a repository.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"owner": J{"type": "string", "description": "Repository owner login"},
						"repo":  J{"type": "string", "description": "Repository name"},
						"state": J{"type": "string", "description": "Issue state: open, closed, or all"},
					},
					"required": []string{"owner", "repo"},
				},
			},
			{
				"name":        "create_pull_request",
				"description": "Create a pull request in a repository.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"owner": J{"type": "string", "description": "Repository owner login"},
						"repo":  J{"type": "string", "description": "Repository name"},
						"title": J{"type": "string", "description": "Pull request title"},
						"head":  J{"type": "string", "description": "Head branch name"},
						"base":  J{"type": "string", "description": "Base branch name"},
						"body":  J{"type": "string", "description": "Pull request body"},
					},
					"required": []string{"owner", "repo", "title", "head", "base"},
				},
			},
			{
				"name":        "merge_pull_request",
				"description": "Merge a pull request with a squash merge.",
				"inputSchema": J{
					"type": "object",
					"properties": J{
						"owner":       J{"type": "string", "description": "Repository owner login"},
						"repo":        J{"type": "string", "description": "Repository name"},
						"pull_number": J{"type": "integer", "description": "Pull request number"},
					},
					"required": []string{"owner", "repo", "pull_number"},
				},
			},
		}
		mcpToolsJSON = jsonBytes(J{"tools": tools})
	})
	return mcpToolsJSON
}

func (s *Server) dispatchMCP(req *Request) *Response {
	if req.Method == "DELETE" {
		return jsonResponse(200, J{"jsonrpc": "2.0", "id": nil, "result": J{}})
	}
	if req.Method != "POST" {
		return jsonResponse(405, J{"jsonrpc": "2.0", "id": nil, "error": J{"code": -32601, "message": "Method not allowed"}})
	}
	// Auth before anything else.
	if !s.checkAuth(req) {
		return jsonResponse(401, J{"message": "Bad credentials"})
	}
	// Parse the JSON-RPC envelope.
	var envelope map[string]interface{}
	if err := jsonUnmarshal(req.Body, &envelope); err != nil {
		return &Response{
			Status:  400,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    jsonBytes(J{"jsonrpc": "2.0", "id": nil, "error": J{"code": -32700, "message": "Parse error"}}),
		}
	}
	method := strField(envelope, "method")
	id, hasID := envelope["id"]
	if !hasID {
		// Notification: no response body.
		return &Response{Status: 202, Headers: map[string]string{}}
	}
	var params map[string]interface{}
	if p, ok := envelope["params"].(map[string]interface{}); ok {
		params = p
	} else {
		params = map[string]interface{}{}
	}
	result, rpcErr := s.handleMCPMethod(method, params)
	resp := J{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	out := jsonResponse(200, resp)
	// Echo a session id if the client sent one.
	if sid := req.Headers["mcp-session-id"]; sid != "" {
		out.Headers["Mcp-Session-Id"] = sid
	}
	return out
}

func (s *Server) handleMCPMethod(method string, params map[string]interface{}) (interface{}, J) {
	switch method {
	case "server/discover":
		return J{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      J{"tools": J{}},
			"serverInfo":        J{"name": "forge", "version": "1.0.0"},
			"_meta":             J{"io.modelcontextprotocol/serverInfo": J{"name": "forge", "version": "1.0.0"}},
		}, nil
	case "initialize":
		version := "2025-11-25"
		if v := strField(params, "protocolVersion"); v != "" {
			for _, supported := range []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
				if v == supported {
					version = v
					break
				}
			}
		}
		return J{
			"protocolVersion": version,
			"capabilities":    J{"tools": J{}},
			"serverInfo":      J{"name": "forge", "version": "1.0.0"},
		}, nil
	case "ping":
		return J{}, nil
	case "tools/list":
		// Byte-identical across calls: return the pre-marshaled constant.
		return rawJSON(mcpToolsBytes()), nil
	case "tools/call":
		return s.mcpCall(params)
	case "resources/list":
		return J{"resources": []interface{}{}}, nil
	case "prompts/list":
		return J{"prompts": []interface{}{}}, nil
	case "notifications/initialized", "notifications/cancelled":
		return J{}, nil
	}
	return nil, J{"code": -32601, "message": "Method not found: " + method}
}

// rawJSON is a marker for a pre-marshaled JSON value.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// mcpCall executes a tools/call request.
func (s *Server) mcpCall(params map[string]interface{}) (interface{}, J) {
	name := strField(params, "name")
	args, _ := params["arguments"].(map[string]interface{})
	if args == nil {
		args = map[string]interface{}{}
	}
	// normalize: find the tool by suffix, case-insensitively
	tool := ""
	for _, candidate := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"} {
		if strings.EqualFold(name, candidate) || strings.HasSuffix(strings.ToLower(name), candidate) {
			tool = candidate
			break
		}
	}
	if tool == "" {
		return nil, J{"code": -32601, "message": "Unknown tool: " + name}
	}
	// argument reader: canonical name first, then synonyms
	getStr := func(names ...string) string {
		for _, n := range names {
			if v, ok := args[n]; ok {
				if str, ok := v.(string); ok && str != "" {
					return str
				}
			}
		}
		return ""
	}
	getInt := func(names ...string) int64 {
		for _, n := range names {
			if v, ok := args[n]; ok {
				switch num := v.(type) {
				case float64:
					return int64(num)
				case int64:
					return num
				case string:
					if i, err := strconv.ParseInt(num, 10, 64); err == nil {
						return i
					}
				}
			}
		}
		return 0
	}
	owner := getStr("owner", "owner_login", "ownerLogin")
	repoName := getStr("repo", "repository", "repository_name", "repo_name")
	// a full_name property is filled with "owner/repo" only when no repo
	// property exists; split it if needed
	if repoName == "" {
		if full := getStr("full_name", "fullName"); full != "" {
			if parts := strings.SplitN(full, "/", 2); len(parts) == 2 {
				owner = parts[0]
				repoName = parts[1]
			}
		}
	}
	isErr := func(msg string) (interface{}, J) {
		return J{
			"content":        []J{{"type": "text", "text": msg}},
			"structuredContent": J{"error": msg},
			"isError":        true,
		}, nil
	}
	toolResult := func(v interface{}) (interface{}, J) {
		return J{
			"content":           []J{{"type": "text", "text": jsonString(v)}},
			"structuredContent": v,
		}, nil
	}
	switch tool {
	case "create_repo":
		name := getStr("name")
		if name == "" {
			return isErr("missing required argument: name")
		}
		desc := getStr("description")
		private := false
		if v, ok := args["private"]; ok {
			if b, ok := v.(bool); ok {
				private = b
			}
		}
		s.store.mu.Lock()
		_, created := s.store.createRepo(ViewerLogin, name, private, desc)
		s.store.mu.Unlock()
		if !created {
			return isErr("name already exists on this account")
		}
		s.store.mu.RLock()
		repo, _ := s.store.getRepo(ViewerLogin, name)
		s.store.mu.RUnlock()
		return toolResult(repoJSON(repo))
	case "get_repo":
		if owner == "" || repoName == "" {
			return isErr("missing required argument: owner and repo")
		}
		s.store.mu.RLock()
		repo, ok := s.store.getRepo(owner, repoName)
		s.store.mu.RUnlock()
		if !ok {
			return isErr("Not Found")
		}
		return toolResult(repoJSON(repo))
	case "create_issue":
		title := getStr("title")
		if owner == "" || repoName == "" || title == "" {
			return isErr("missing required argument: owner, repo, title")
		}
		body := getStr("body")
		s.store.mu.Lock()
		repo, ok := s.store.getRepo(owner, repoName)
		if !ok {
			s.store.mu.Unlock()
			return isErr("Not Found")
		}
		iss, _ := s.store.createIssue(repo, title, body, nil)
		s.store.mu.Unlock()
		if iss == nil {
			return isErr("could not create issue")
		}
		return toolResult(issueJSON(iss, repo))
	case "list_issues":
		if owner == "" || repoName == "" {
			return isErr("missing required argument: owner and repo")
		}
		state := getStr("state")
		if state == "" {
			state = "open"
		}
		s.store.mu.RLock()
		repo, ok := s.store.getRepo(owner, repoName)
		if !ok {
			s.store.mu.RUnlock()
			return isErr("Not Found")
		}
		var items []interface{}
		for _, iss := range repo.Issues {
			if state != "all" && iss.State != state {
				continue
			}
			items = append(items, issueJSON(iss, repo))
		}
		s.store.mu.RUnlock()
		if items == nil {
			items = []interface{}{}
		}
		return toolResult(items)
	case "create_pull_request":
		title := getStr("title")
		head := getStr("head", "head_ref", "headRef")
		base := getStr("base", "base_ref", "baseRef")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return isErr("missing required argument: owner, repo, title, head, base")
		}
		body := getStr("body")
		s.store.mu.Lock()
		repo, ok := s.store.getRepo(owner, repoName)
		if !ok {
			s.store.mu.Unlock()
			return isErr("Not Found")
		}
		pr, errKind := s.store.createPull(repo, title, head, base, body)
		s.store.mu.Unlock()
		if pr == nil {
			if errKind == "duplicate" {
				return isErr("A pull request already exists for " + head)
			}
			return isErr(errKind)
		}
		return toolResult(pullJSON(pr, repo))
	case "merge_pull_request":
		pullNum := getInt("pull_number", "pullNumber", "number")
		if owner == "" || repoName == "" || pullNum == 0 {
			return isErr("missing required argument: owner, repo, pull_number")
		}
		s.store.mu.Lock()
		repo, ok := s.store.getRepo(owner, repoName)
		if !ok {
			s.store.mu.Unlock()
			return isErr("Not Found")
		}
		pr := repo.getPull(int(pullNum))
		if pr == nil {
			s.store.mu.Unlock()
			return isErr("Not Found")
		}
		mergeOID, status := s.store.mergePull(repo, pr, "squash")
		s.store.mu.Unlock()
		if status == 405 {
			return isErr("Merge conflict")
		}
		if status != 200 {
			return isErr("Not Found")
		}
		out := pullJSON(pr, repo)
		out["sha"] = mergeOID
		return toolResult(out)
	}
	return nil, J{"code": -32601, "message": "Unknown tool: " + name}
}
