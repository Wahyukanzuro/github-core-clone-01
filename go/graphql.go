package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------- GraphQL ----------

// Value kinds: string, int64, float64, bool, nil, gqlEnum, []interface{},
// map[string]interface{}, gqlVar.

type gqlEnum struct{ Val string }
type gqlVar struct{ Name string }

type gqlField struct {
	Alias    string
	Name     string
	Args     map[string]interface{}
	Sel      []gqlField
	Fragment bool
	TypeCond string
}

type gqlDoc struct {
	OpType  string // "query" or "mutation"
	OpName  string
	Sel     []gqlField
}

// --- tokenizer ---

type gqlTok struct {
	kind string // "name", "string", "int", "float", "punct", "spread"
	val  string
}

func tokenizeGQL(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"':
			// block string """..."""
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, gqlTok{"string", src[i+3 : i+3+end]})
				i += 3 + end + 3
				continue
			}
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					switch src[j+1] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					case 'u':
						if j+5 < len(src) {
							if v, err := strconv.ParseUint(src[j+2:j+6], 16, 32); err == nil {
								sb.WriteRune(rune(v))
								j += 4
							}
						}
					default:
						sb.WriteByte(src[j+1])
					}
					j += 2
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c == '.' && strings.HasPrefix(src[i:], "..."):
			toks = append(toks, gqlTok{"spread", "..."})
			i += 3
		case (c >= '0' && c <= '9') || (c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			j := i
			for j < len(src) && ((src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		case strings.ContainsRune("!$()[]{}:=@|&", rune(c)):
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		default:
			return nil, fmt.Errorf("unexpected char %q", c)
		}
	}
	return toks, nil
}

// --- parser ---

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() (gqlTok, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return gqlTok{}, false
}

func (p *gqlParser) next() (gqlTok, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

func (p *gqlParser) expectPunct(v string) error {
	t, ok := p.next()
	if !ok || t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %q", v)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	t, ok := p.next()
	if !ok || t.kind != "name" {
		return "", fmt.Errorf("expected name")
	}
	return t.val, nil
}

func parseGQLDocument(src string) (*gqlDoc, error) {
	toks, err := tokenizeGQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{OpType: "query"}
	// optional operation type
	if t, ok := p.peek(); ok && t.kind == "name" && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
		doc.OpType = t.val
		p.next()
		// optional operation name
		if t2, ok2 := p.peek(); ok2 && t2.kind == "name" {
			doc.OpName = t2.val
			p.next()
		}
		// optional variable definitions
		if t2, ok2 := p.peek(); ok2 && t2.kind == "punct" && t2.val == "(" {
			if err := p.skipVarDefs(); err != nil {
				return nil, err
			}
		}
		// optional directives
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	doc.Sel = sel
	return doc, nil
}

func (p *gqlParser) skipVarDefs() error {
	// consume balanced parens
	depth := 0
	for {
		t, ok := p.next()
		if !ok {
			return fmt.Errorf("unterminated variable definitions")
		}
		if t.kind == "punct" && t.val == "(" {
			depth++
		} else if t.kind == "punct" && t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) skipDirectives() error {
	for {
		t, ok := p.peek()
		if !ok || t.kind != "punct" || t.val != "@" {
			return nil
		}
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if t2, ok2 := p.peek(); ok2 && t2.kind == "punct" && t2.val == "(" {
			if err := p.skipArgs(); err != nil {
				return err
			}
		}
	}
}

func (p *gqlParser) skipArgs() error {
	depth := 0
	for {
		t, ok := p.next()
		if !ok {
			return fmt.Errorf("unterminated arguments")
		}
		if t.kind == "punct" && t.val == "(" {
			depth++
		} else if t.kind == "punct" && t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var fields []gqlField
	for {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (gqlField, error) {
	var f gqlField
	t, ok := p.next()
	if !ok {
		return f, fmt.Errorf("unexpected end of document")
	}
	if t.kind == "spread" {
		// inline fragment: ... on Type { ... } or ... { ... }
		f.Fragment = true
		if t2, ok2 := p.peek(); ok2 && t2.kind == "name" && t2.val == "on" {
			p.next()
			cond, err := p.expectName()
			if err != nil {
				return f, err
			}
			f.TypeCond = cond
		}
		if err := p.skipDirectives(); err != nil {
			return f, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return f, err
		}
		f.Sel = sel
		return f, nil
	}
	if t.kind != "name" {
		return f, fmt.Errorf("expected field name, got %v", t)
	}
	name := t.val
	// alias?
	if t2, ok2 := p.peek(); ok2 && t2.kind == "punct" && t2.val == ":" {
		p.next()
		real, err := p.expectName()
		if err != nil {
			return f, err
		}
		f.Alias = name
		f.Name = real
	} else {
		f.Name = name
	}
	// arguments
	if t2, ok2 := p.peek(); ok2 && t2.kind == "punct" && t2.val == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return f, err
		}
		f.Args = args
	}
	if err := p.skipDirectives(); err != nil {
		return f, err
	}
	// sub-selection
	if t2, ok2 := p.peek(); ok2 && t2.kind == "punct" && t2.val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return f, err
		}
		f.Sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseArguments() (map[string]interface{}, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]interface{}{}
	for {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated arguments")
		}
		if t.kind == "punct" && t.val == ")" {
			p.next()
			return args, nil
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
}

func (p *gqlParser) parseValue() (interface{}, error) {
	t, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end of value")
	}
	switch t.kind {
	case "string":
		return t.val, nil
	case "int":
		v, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "float":
		v, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum{Val: t.val}, nil
		}
	case "punct":
		switch t.val {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return gqlVar{Name: name}, nil
		case "[":
			var list []interface{}
			for {
				t2, ok2 := p.peek()
				if !ok2 {
					return nil, fmt.Errorf("unterminated list")
				}
				if t2.kind == "punct" && t2.val == "]" {
					p.next()
					return list, nil
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
		case "{":
			obj := map[string]interface{}{}
			for {
				t2, ok2 := p.peek()
				if !ok2 {
					return nil, fmt.Errorf("unterminated input object")
				}
				if t2.kind == "punct" && t2.val == "}" {
					p.next()
					return obj, nil
				}
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[name] = v
			}
		}
	}
	return nil, fmt.Errorf("unexpected token %v", t)
}

// ---------- execution ----------

type gqlNode struct {
	kind string
	repo *Repo
	issue *Issue
	pull *Pull
	comment *Comment
	file *FileChange
	// connection payload
	nodes []gqlNode
	cursors []string
	hasNext bool
	total int
	// mutation payload
	payload map[string]gqlNode
	// scalar
	strVal string
	intVal int64
	boolVal bool
	// introspection
	typeInfo *gqlTypeInfo
}

type gqlTypeInfo struct {
	Name   string
	Kind   string // OBJECT, ENUM, INPUT_OBJECT, SCALAR
	Fields []gqlFieldInfo
	Enums  []string
}

type gqlFieldInfo struct {
	Name string
	Type string
}

type gqlExecutor struct {
	s      *Server
	vars   map[string]interface{}
	errors []J
}

func (s *Server) dispatchGraphQL(req *Request) *Response {
	if req.Method != "POST" {
		return s.restErr(404, "Not Found")
	}
	if !s.checkAuth(req) {
		return jsonResponse(401, J{"message": "Bad credentials"})
	}
	// batch or single
	var docs []map[string]interface{}
	trimmed := strings.TrimSpace(string(req.Body))
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]interface{}
		if err := jsonUnmarshal(req.Body, &arr); err != nil {
			return jsonResponse(400, J{"message": "Problems parsing JSON"})
		}
		docs = arr
	} else {
		var one map[string]interface{}
		if err := jsonUnmarshal(req.Body, &one); err != nil {
			return jsonResponse(400, J{"message": "Problems parsing JSON"})
		}
		docs = []map[string]interface{}{one}
	}
	results := make([]interface{}, 0, len(docs))
	for _, d := range docs {
		query := strField(d, "query")
		vars := map[string]interface{}{}
		if v, ok := d["variables"]; ok {
			if vm, ok := v.(map[string]interface{}); ok {
				vars = vm
			}
		}
		results = append(results, s.executeGraphQL(query, vars))
	}
	if strings.HasPrefix(trimmed, "[") {
		return jsonResponse(200, results)
	}
	return jsonResponse(200, results[0])
}

func (s *Server) executeGraphQL(query string, vars map[string]interface{}) J {
	doc, err := parseGQLDocument(query)
	if err != nil {
		return J{"data": nil, "errors": []J{{"message": err.Error()}}}
	}
	// One lock for the whole operation: write lock for mutations, read lock
	// for queries. Resolvers assume the lock is held.
	if doc.OpType == "mutation" {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
	} else {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
	}
	e := &gqlExecutor{s: s, vars: vars}
	root := gqlNode{kind: "Query"}
	if doc.OpType == "mutation" {
		root.kind = "Mutation"
	}
	data := e.selectFields(root, doc.Sel)
	out := J{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}

// resolve substitutes variables in a value.
func (e *gqlExecutor) resolve(v interface{}) interface{} {
	switch t := v.(type) {
	case gqlVar:
		if val, ok := e.vars[t.Name]; ok {
			return val
		}
		return nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = e.resolve(item)
		}
		return out
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, item := range t {
			out[k] = e.resolve(item)
		}
		return out
	default:
		return v
	}
}

func (e *gqlExecutor) arg(f gqlField, name string) interface{} {
	if f.Args == nil {
		return nil
	}
	if v, ok := f.Args[name]; ok {
		return e.resolve(v)
	}
	return nil
}

func toInt(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
	}
	return 0
}

func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case gqlEnum:
		return t.Val
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func toStringList(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		} else if en, ok := item.(gqlEnum); ok {
			out = append(out, en.Val)
		}
	}
	return out
}

// selectFields resolves a selection set against a node.
func (e *gqlExecutor) selectFields(n gqlNode, sel []gqlField) map[string]interface{} {
	out := map[string]interface{}{}
	for _, f := range sel {
		if f.Fragment {
			if f.TypeCond != "" && !typeMatches(n.kind, f.TypeCond) {
				continue
			}
			sub := e.selectFields(n, f.Sel)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.Name
		if f.Alias != "" {
			key = f.Alias
		}
		val := e.resolveField(n, f)
		out[key] = val
	}
	return out
}

func typeMatches(kind, cond string) bool {
	if kind == cond {
		return true
	}
	// Node interface: Repository, Issue, PullRequest all implement Node
	if cond == "Node" {
		switch kind {
		case "Repository", "Issue", "PullRequest", "IssueComment":
			return true
		}
	}
	return false
}

// resolveField resolves one field on a node. Unknown fields record an error
// and return nil.
func (e *gqlExecutor) resolveField(n gqlNode, f gqlField) interface{} {
	// __typename is always available
	if f.Name == "__typename" {
		return n.kind
	}
	switch n.kind {
	case "Query", "Mutation":
		return e.resolveRoot(n, f)
	case "CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload":
		return e.resolvePayload(n, f)
	case "Repository":
		return e.resolveRepository(n, f)
	case "Issue":
		return e.resolveIssue(n, f)
	case "PullRequest":
		return e.resolvePullRequest(n, f)
	case "User":
		return e.resolveUser(n, f)
	case "Ref":
		return e.resolveRefNode(n, f)
	case "IssueConnection", "PullRequestConnection", "IssueCommentConnection", "PullRequestFileConnection":
		return e.resolveConnection(n, f)
	case "IssueEdge", "PullRequestEdge", "IssueCommentEdge", "PullRequestFileEdge":
		return e.resolveEdge(n, f)
	case "PageInfo":
		return e.resolvePageInfo(n, f)
	case "IssueComment":
		return e.resolveComment(n, f)
	case "PullRequestFile":
		return e.resolveFile(n, f)
	case "__Type":
		return e.resolveType(n, f)
	case "__Field":
		return e.resolveTypeField(n, f)
	case "__EnumValue":
		return e.resolveEnumValue(n, f)
	}
	e.errField(n.kind, f.Name)
	return nil
}

func (e *gqlExecutor) errField(typeName, field string) {
	e.errors = append(e.errors, J{"message": fmt.Sprintf("Cannot query field %q on type %q.", field, typeName)})
}

// --- root ---

func (e *gqlExecutor) resolveRoot(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "viewer":
		return e.selectFields(gqlNode{kind: "User"}, f.Sel)
	case "repository":
		owner := toString(e.arg(f, "owner"))
		name := toString(e.arg(f, "name"))
		repo, ok := e.s.store.getRepo(owner, name)
		if !ok {
			return nil
		}
		return e.selectFields(gqlNode{kind: "Repository", repo: repo}, f.Sel)
	case "node":
		id := toString(e.arg(f, "id"))
		kind, parts, ok := parseGID(id)
		if !ok {
			return nil
		}
		switch kind {
		case "Repository":
			if len(parts) < 1 {
				return nil
			}
			dbID := toInt(parseIntOrZero(parts[0]))
			for _, r := range e.s.store.order {
				if r.DatabaseID == dbID {
					return e.selectFields(gqlNode{kind: "Repository", repo: r}, f.Sel)
				}
			}
			return nil
		case "Issue":
			if len(parts) < 2 {
				return nil
			}
			dbID := toInt(parseIntOrZero(parts[0]))
			num := int(toInt(parseIntOrZero(parts[1])))
			for _, r := range e.s.store.order {
				if r.DatabaseID == dbID {
					if iss := r.getIssue(num); iss != nil {
						return e.selectFields(gqlNode{kind: "Issue", repo: r, issue: iss}, f.Sel)
					}
				}
			}
			return nil
		case "PullRequest":
			if len(parts) < 2 {
				return nil
			}
			dbID := toInt(parseIntOrZero(parts[0]))
			num := int(toInt(parseIntOrZero(parts[1])))
			for _, r := range e.s.store.order {
				if r.DatabaseID == dbID {
					if pr := r.getPull(num); pr != nil {
						return e.selectFields(gqlNode{kind: "PullRequest", repo: r, pull: pr}, f.Sel)
					}
				}
			}
			return nil
		}
		return nil
	case "__type":
		name := toString(e.arg(f, "name"))
		ti := schemaType(name)
		if ti == nil {
			return nil
		}
		return e.selectFields(gqlNode{kind: "__Type", typeInfo: ti}, f.Sel)
	case "__schema":
		return e.selectFields(gqlNode{kind: "__Schema"}, f.Sel)
	}
	// mutations
	if n.kind == "Mutation" {
		return e.resolveMutation(f)
	}
	e.errField(n.kind, f.Name)
	return nil
}

func parseIntOrZero(s string) interface{} {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func (e *gqlExecutor) resolveMutation(f gqlField) interface{} {
	input, _ := e.arg(f, "input").(map[string]interface{})
	switch f.Name {
	case "createIssue":
		repoID := toString(input["repositoryId"])
		kind, parts, ok := parseGID(repoID)
		if !ok || kind != "Repository" || len(parts) < 1 {
			e.errors = append(e.errors, J{"message": "invalid repositoryId"})
			return nil
		}
		dbID := toInt(parseIntOrZero(parts[0]))
		title := toString(input["title"])
		body := toString(input["body"])
		var repo *Repo
		for _, r := range e.s.store.order {
			if r.DatabaseID == dbID {
				repo = r
				break
			}
		}
		if repo == nil {
			e.errors = append(e.errors, J{"message": "repository not found"})
			return nil
		}
		iss, _ := e.s.store.createIssue(repo, title, body, nil)
		if iss == nil {
			e.errors = append(e.errors, J{"message": "could not create issue"})
			return nil
		}
		return e.selectFields(gqlNode{kind: "CreateIssuePayload", payload: map[string]gqlNode{"issue": {kind: "Issue", repo: repo, issue: iss}}}, f.Sel)
	case "closeIssue":
		issueID := toString(input["issueId"])
		kind, parts, ok := parseGID(issueID)
		if !ok || kind != "Issue" || len(parts) < 2 {
			e.errors = append(e.errors, J{"message": "invalid issueId"})
			return nil
		}
		dbID := toInt(parseIntOrZero(parts[0]))
		num := int(toInt(parseIntOrZero(parts[1])))
		var iss *Issue
		var repo *Repo
		for _, r := range e.s.store.order {
			if r.DatabaseID == dbID {
				repo = r
				iss = r.getIssue(num)
				break
			}
		}
		if iss == nil {
			e.errors = append(e.errors, J{"message": "issue not found"})
			return nil
		}
		iss.State = "closed"
		iss.ClosedAt = nextTime()
		iss.UpdatedAt = nextTime()
		return e.selectFields(gqlNode{kind: "CloseIssuePayload", payload: map[string]gqlNode{"issue": {kind: "Issue", repo: repo, issue: iss}}}, f.Sel)
	case "createPullRequest":
		repoID := toString(input["repositoryId"])
		kind, parts, ok := parseGID(repoID)
		if !ok || kind != "Repository" || len(parts) < 1 {
			e.errors = append(e.errors, J{"message": "invalid repositoryId"})
			return nil
		}
		dbID := toInt(parseIntOrZero(parts[0]))
		baseRef := toString(input["baseRefName"])
		headRef := toString(input["headRefName"])
		title := toString(input["title"])
		body := toString(input["body"])
		var repo *Repo
		for _, r := range e.s.store.order {
			if r.DatabaseID == dbID {
				repo = r
				break
			}
		}
		if repo == nil {
			e.errors = append(e.errors, J{"message": "repository not found"})
			return nil
		}
		pr, errKind := e.s.store.createPull(repo, title, headRef, baseRef, body)
		if pr == nil {
			if errKind == "duplicate" {
				// tolerate: find the existing open PR
				for _, p := range repo.Pulls {
					if p.State == "open" && p.HeadBranch == headRef && p.BaseBranch == baseRef {
						pr = p
						break
					}
				}
			}
			if pr == nil {
				e.errors = append(e.errors, J{"message": "could not create pull request: " + errKind})
				return nil
			}
		}
		return e.selectFields(gqlNode{kind: "CreatePullRequestPayload", payload: map[string]gqlNode{"pullRequest": {kind: "PullRequest", repo: repo, pull: pr}}}, f.Sel)
	case "mergePullRequest":
		prID := toString(input["pullRequestId"])
		kind, parts, ok := parseGID(prID)
		if !ok || kind != "PullRequest" || len(parts) < 2 {
			e.errors = append(e.errors, J{"message": "invalid pullRequestId"})
			return nil
		}
		dbID := toInt(parseIntOrZero(parts[0]))
		num := int(toInt(parseIntOrZero(parts[1])))
		var pr *Pull
		var repo *Repo
		for _, r := range e.s.store.order {
			if r.DatabaseID == dbID {
				repo = r
				pr = r.getPull(num)
				break
			}
		}
		if pr == nil {
			e.errors = append(e.errors, J{"message": "pull request not found"})
			return nil
		}
		_, status := e.s.store.mergePull(repo, pr, "squash")
		if status != 200 {
			e.errors = append(e.errors, J{"message": "pull request cannot be merged"})
			return nil
		}
		return e.selectFields(gqlNode{kind: "MergePullRequestPayload", payload: map[string]gqlNode{"pullRequest": {kind: "PullRequest", repo: repo, pull: pr}}}, f.Sel)
	case "addComment":
		subjectID := toString(input["subjectId"])
		body := toString(input["body"])
		kind, parts, ok := parseGID(subjectID)
		if !ok {
			e.errors = append(e.errors, J{"message": "invalid subjectId"})
			return nil
		}
		var iss *Issue
		var repo *Repo
		switch kind {
		case "Issue":
			if len(parts) >= 2 {
				dbID := toInt(parseIntOrZero(parts[0]))
				num := int(toInt(parseIntOrZero(parts[1])))
				for _, r := range e.s.store.order {
					if r.DatabaseID == dbID {
						repo = r
						iss = r.getIssue(num)
						break
					}
				}
			}
		}
		if iss == nil {
			e.errors = append(e.errors, J{"message": "subject not found"})
			return nil
		}
		c := &Comment{
			ID:         nextInternalID(),
			Body:       body,
			UserLogin:  ViewerLogin,
			UserID:     ViewerID,
			CreatedSeq: e.s.store.nextCounter(),
			CreatedAt:  nextTime(),
			UpdatedAt:  nextTime(),
		}
		iss.Comments = append(iss.Comments, c)
		edge := gqlNode{kind: "IssueCommentEdge", comment: c, repo: repo, issue: iss}
		return e.selectFields(gqlNode{kind: "AddCommentPayload", payload: map[string]gqlNode{"commentEdge": edge}}, f.Sel)
	}
	e.errField("Mutation", f.Name)
	return nil
}

// --- payload nodes ---

func (e *gqlExecutor) resolvePayload(n gqlNode, f gqlField) interface{} {
	if sub, ok := n.payload[f.Name]; ok {
		return e.selectFields(sub, f.Sel)
	}
	e.errField(n.kind, f.Name)
	return nil
}

// --- repository ---

func (e *gqlExecutor) resolveRepository(n gqlNode, f gqlField) interface{} {
	r := n.repo
	switch f.Name {
	case "id":
		return gid("Repository", r.DatabaseID)
	case "databaseId":
		return r.DatabaseID
	case "name":
		return r.Name
	case "nameWithOwner":
		return r.Owner + "/" + r.Name
	case "owner":
		return e.selectFields(gqlNode{kind: "User"}, f.Sel)
	case "defaultBranchRef":
		_, ok := r.Refs["refs/heads/"+r.DefaultBranch]
		if !ok {
			return nil
		}
		return e.selectFields(gqlNode{kind: "Ref", strVal: r.DefaultBranch, repo: r}, f.Sel)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "hasWikiEnabled":
		return false
	case "description":
		return r.Description
	case "isPrivate":
		return r.Private
	case "createdAt":
		return r.CreatedAt
	case "updatedAt":
		return r.UpdatedAt
	case "url":
		return "http://example.invalid/" + r.Owner + "/" + r.Name
	case "issue":
		num := int(toInt(e.arg(f, "number")))
		iss := r.getIssue(num)
		if iss == nil {
			return nil
		}
		return e.selectFields(gqlNode{kind: "Issue", repo: r, issue: iss}, f.Sel)
	case "issues":
		return e.issueConnection(r, f)
	case "pullRequest":
		num := int(toInt(e.arg(f, "number")))
		pr := r.getPull(num)
		if pr == nil {
			return nil
		}
		return e.selectFields(gqlNode{kind: "PullRequest", repo: r, pull: pr}, f.Sel)
	case "pullRequests":
		return e.pullConnection(r, f)
	}
	// mutation payloads share this resolver via kind check
	if n.kind == "CreateIssuePayload" || n.kind == "CloseIssuePayload" || n.kind == "CreatePullRequestPayload" || n.kind == "MergePullRequestPayload" || n.kind == "AddCommentPayload" {
		return e.resolvePayload(n, f)
	}
	e.errField("Repository", f.Name)
	return nil
}

func (e *gqlExecutor) issueConnection(r *Repo, f gqlField) interface{} {
	first := int(toInt(e.arg(f, "first")))
	after := toString(e.arg(f, "after"))
	states := toStringList(e.arg(f, "states"))
	orderBy, _ := e.arg(f, "orderBy").(map[string]interface{})
	// filter
	var list []*Issue
	for _, iss := range r.Issues {
		if len(states) > 0 {
			match := false
			for _, st := range states {
				if strings.EqualFold(st, iss.State) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, iss)
	}
	// order
	field := "CREATED_AT"
	dir := "DESC"
	if orderBy != nil {
		if v, ok := orderBy["field"]; ok {
			field = toString(v)
		}
		if v, ok := orderBy["direction"]; ok {
			dir = toString(v)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if field == "CREATED_AT" {
			if dir == "ASC" {
				if list[i].CreatedSeq != list[j].CreatedSeq {
					return list[i].CreatedSeq < list[j].CreatedSeq
				}
				return list[i].Number < list[j].Number
			}
			if list[i].CreatedSeq != list[j].CreatedSeq {
				return list[i].CreatedSeq > list[j].CreatedSeq
			}
			return list[i].Number > list[j].Number
		}
		return list[i].Number > list[j].Number
	})
	// cursor pagination
	start := 0
	if after != "" {
		if idx := cursorIndex(after); idx >= 0 {
			start = idx + 1
		}
	}
	total := len(list)
	hasNext := false
	end := total
	if first > 0 && start+first < total {
		end = start + first
		hasNext = true
	}
	page := list[start:end]
	nodes := make([]gqlNode, 0, len(page))
	cursors := make([]string, 0, len(page))
	for i, iss := range page {
		nodes = append(nodes, gqlNode{kind: "Issue", repo: r, issue: iss})
		cursors = append(cursors, cursorFor(start+i))
	}
	conn := gqlNode{kind: "IssueConnection", nodes: nodes, cursors: cursors, hasNext: hasNext, total: total, repo: r}
	return e.selectFields(conn, f.Sel)
}

func (e *gqlExecutor) pullConnection(r *Repo, f gqlField) interface{} {
	first := int(toInt(e.arg(f, "first")))
	after := toString(e.arg(f, "after"))
	states := toStringList(e.arg(f, "states"))
	var list []*Pull
	for _, pr := range r.Pulls {
		if len(states) > 0 {
			match := false
			for _, st := range states {
				if strings.EqualFold(st, pr.State) || (st == "MERGED" && pr.Merged) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, pr)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
	start := 0
	if after != "" {
		if idx := cursorIndex(after); idx >= 0 {
			start = idx + 1
		}
	}
	total := len(list)
	hasNext := false
	end := total
	if first > 0 && start+first < total {
		end = start + first
		hasNext = true
	}
	page := list[start:end]
	nodes := make([]gqlNode, 0, len(page))
	cursors := make([]string, 0, len(page))
	for i, pr := range page {
		nodes = append(nodes, gqlNode{kind: "PullRequest", repo: r, pull: pr})
		cursors = append(cursors, cursorFor(start+i))
	}
	conn := gqlNode{kind: "PullRequestConnection", nodes: nodes, cursors: cursors, hasNext: hasNext, total: total, repo: r}
	return e.selectFields(conn, f.Sel)
}

func cursorFor(i int) string { return b64enc([]byte("cursor:" + strconv.Itoa(i))) }

func cursorIndex(c string) int {
	b, err := b64dec(c)
	if err != nil {
		return -1
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return -1
	}
	v, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return -1
	}
	return v
}

// --- issue ---

func (e *gqlExecutor) resolveIssue(n gqlNode, f gqlField) interface{} {
	iss := n.issue
	switch f.Name {
	case "id":
		return gid("Issue", n.repo.DatabaseID, iss.Number)
	case "databaseId":
		return iss.ID
	case "number":
		return iss.Number
	case "title":
		return iss.Title
	case "body":
		return iss.Body
	case "state":
		if iss.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "createdAt":
		return iss.CreatedAt
	case "updatedAt":
		return iss.UpdatedAt
	case "closedAt":
		if iss.ClosedAt == "" {
			return nil
		}
		return iss.ClosedAt
	case "author":
		return e.selectFields(gqlNode{kind: "User"}, f.Sel)
	case "labels":
		return nil
	case "comments":
		nodes := make([]gqlNode, 0, len(iss.Comments))
		cursors := make([]string, 0, len(iss.Comments))
		for i, c := range iss.Comments {
			nodes = append(nodes, gqlNode{kind: "IssueComment", repo: n.repo, issue: iss, comment: c})
			cursors = append(cursors, cursorFor(i))
		}
		conn := gqlNode{kind: "IssueCommentConnection", nodes: nodes, cursors: cursors, hasNext: false, total: len(iss.Comments), repo: n.repo}
		return e.selectFields(conn, f.Sel)
	case "repository":
		return e.selectFields(gqlNode{kind: "Repository", repo: n.repo}, f.Sel)
	}
	e.errField("Issue", f.Name)
	return nil
}

// --- pull request ---

func (e *gqlExecutor) resolvePullRequest(n gqlNode, f gqlField) interface{} {
	pr := n.pull
	switch f.Name {
	case "id":
		return gid("PullRequest", n.repo.DatabaseID, pr.Number)
	case "databaseId":
		return pr.ID
	case "number":
		return pr.Number
	case "title":
		return pr.Title
	case "body":
		return pr.Body
	case "state":
		if pr.Merged {
			return "MERGED"
		}
		if pr.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "headRefName":
		return pr.HeadBranch
	case "baseRefName":
		return pr.BaseBranch
	case "mergeable":
		if pr.State != "open" {
			return "UNKNOWN"
		}
		if e.s.store.pullMergeable(n.repo, pr) {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	case "merged":
		return pr.Merged
	case "mergedAt":
		if pr.MergedAt == "" {
			return nil
		}
		return pr.MergedAt
	case "mergeCommit":
		if pr.MergeCommitSHA == "" {
			return nil
		}
		return e.selectFields(gqlNode{kind: "Ref", strVal: pr.BaseBranch, repo: n.repo}, f.Sel)
	case "createdAt":
		return pr.CreatedAt
	case "updatedAt":
		return pr.UpdatedAt
	case "author":
		return e.selectFields(gqlNode{kind: "User"}, f.Sel)
	case "repository":
		return e.selectFields(gqlNode{kind: "Repository", repo: n.repo}, f.Sel)
	case "files":
		first := int(toInt(e.arg(f, "first")))
		changes := e.s.store.pullFiles(n.repo, pr)
		if first > 0 && first < len(changes) {
			changes = changes[:first]
		}
		nodes := make([]gqlNode, 0, len(changes))
		cursors := make([]string, 0, len(changes))
		for i := range changes {
			fc := changes[i]
			nodes = append(nodes, gqlNode{kind: "PullRequestFile", file: &fc})
			cursors = append(cursors, cursorFor(i))
		}
		conn := gqlNode{kind: "PullRequestFileConnection", nodes: nodes, cursors: cursors, hasNext: false, total: len(changes), repo: n.repo}
		return e.selectFields(conn, f.Sel)
	}
	e.errField("PullRequest", f.Name)
	return nil
}

// --- user / ref / comment / file ---

func (e *gqlExecutor) resolveUser(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "login":
		return ViewerLogin
	case "id":
		return gid("User", ViewerID)
	case "databaseId":
		return ViewerID
	case "name":
		return ViewerName
	case "email":
		return ViewerEmail
	case "avatarUrl":
		return "http://example.invalid/avatar/" + ViewerLogin
	}
	e.errField("User", f.Name)
	return nil
}

func (e *gqlExecutor) resolveRefNode(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "name":
		return n.strVal
	case "id":
		return gid("Ref", n.repo.DatabaseID, n.strVal)
	case "target":
		return nil
	}
	e.errField("Ref", f.Name)
	return nil
}

func (e *gqlExecutor) resolveComment(n gqlNode, f gqlField) interface{} {
	c := n.comment
	switch f.Name {
	case "id":
		return gid("IssueComment", n.repo.DatabaseID, c.ID)
	case "databaseId":
		return c.ID
	case "body":
		return c.Body
	case "createdAt":
		return c.CreatedAt
	case "updatedAt":
		return c.UpdatedAt
	case "author":
		return e.selectFields(gqlNode{kind: "User"}, f.Sel)
	}
	e.errField("IssueComment", f.Name)
	return nil
}

func (e *gqlExecutor) resolveFile(n gqlNode, f gqlField) interface{} {
	fc := n.file
	switch f.Name {
	case "path":
		return fc.Filename
	case "additions":
		return fc.Additions
	case "deletions":
		return fc.Deletions
	case "changes":
		return fc.Changes
	case "status":
		return strings.ToUpper(fc.Status)
	}
	e.errField("PullRequestFile", f.Name)
	return nil
}

// --- connections / edges / pageInfo ---

func (e *gqlExecutor) resolveConnection(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "nodes":
		out := make([]interface{}, 0, len(n.nodes))
		for _, sub := range n.nodes {
			out = append(out, e.selectFields(sub, f.Sel))
		}
		return out
	case "edges":
		edgeKind := "IssueEdge"
		switch n.kind {
		case "PullRequestConnection":
			edgeKind = "PullRequestEdge"
		case "IssueCommentConnection":
			edgeKind = "IssueCommentEdge"
		case "PullRequestFileConnection":
			edgeKind = "PullRequestFileEdge"
		}
		out := make([]interface{}, 0, len(n.nodes))
		for i, sub := range n.nodes {
			edge := sub
			edge.kind = edgeKind
			edge.cursors = []string{n.cursors[i]}
			out = append(out, e.selectFields(edge, f.Sel))
		}
		return out
	case "totalCount":
		return n.total
	case "pageInfo":
		return e.selectFields(gqlNode{kind: "PageInfo", hasNext: n.hasNext, cursors: n.cursors}, f.Sel)
	}
	e.errField(n.kind, f.Name)
	return nil
}

func (e *gqlExecutor) resolveEdge(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "node":
		inner := n
		inner.kind = strings.TrimSuffix(n.kind, "Edge")
		if inner.kind == "PullRequestFile" {
			inner.kind = "PullRequestFile"
		}
		inner.cursors = nil
		return e.selectFields(inner, f.Sel)
	case "cursor":
		if len(n.cursors) > 0 {
			return n.cursors[0]
		}
		return ""
	}
	e.errField(n.kind, f.Name)
	return nil
}

func (e *gqlExecutor) resolvePageInfo(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "hasNextPage":
		return n.hasNext
	case "hasPreviousPage":
		return false
	case "startCursor":
		if len(n.cursors) > 0 {
			return n.cursors[0]
		}
		return nil
	case "endCursor":
		if len(n.cursors) > 0 {
			return n.cursors[len(n.cursors)-1]
		}
		return nil
	}
	e.errField("PageInfo", f.Name)
	return nil
}

// --- introspection ---

func (e *gqlExecutor) resolveType(n gqlNode, f gqlField) interface{} {
	ti := n.typeInfo
	switch f.Name {
	case "name":
		return ti.Name
	case "kind":
		return ti.Kind
	case "fields":
		includeDeprecated := false
		if v, ok := e.arg(f, "includeDeprecated").(bool); ok {
			includeDeprecated = v
		}
		_ = includeDeprecated
		out := make([]interface{}, 0, len(ti.Fields))
		for _, fi := range ti.Fields {
			out = append(out, e.selectFields(gqlNode{kind: "__Field", strVal: fi.Name}, f.Sel))
		}
		return out
	case "enumValues":
		includeDeprecated := false
		if v, ok := e.arg(f, "includeDeprecated").(bool); ok {
			includeDeprecated = v
		}
		_ = includeDeprecated
		out := make([]interface{}, 0, len(ti.Enums))
		for _, en := range ti.Enums {
			out = append(out, e.selectFields(gqlNode{kind: "__EnumValue", strVal: en}, f.Sel))
		}
		return out
	case "description":
		return nil
	}
	e.errField("__Type", f.Name)
	return nil
}

func (e *gqlExecutor) resolveTypeField(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "name":
		return n.strVal
	case "description":
		return nil
	case "args":
		return []interface{}{}
	case "type":
		return J{"name": "String", "kind": "SCALAR"}
	case "isDeprecated":
		return false
	case "deprecationReason":
		return nil
	}
	e.errField("__Field", f.Name)
	return nil
}

func (e *gqlExecutor) resolveEnumValue(n gqlNode, f gqlField) interface{} {
	switch f.Name {
	case "name":
		return n.strVal
	case "description":
		return nil
	case "isDeprecated":
		return false
	case "deprecationReason":
		return nil
	}
	e.errField("__EnumValue", f.Name)
	return nil
}

// ---------- schema (introspection data) ----------

func fieldNames(names ...string) []gqlFieldInfo {
	out := make([]gqlFieldInfo, 0, len(names))
	for _, n := range names {
		out = append(out, gqlFieldInfo{Name: n})
	}
	return out
}

func schemaType(name string) *gqlTypeInfo {
	switch name {
	case "Issue":
		return &gqlTypeInfo{Name: "Issue", Kind: "OBJECT", Fields: fieldNames(
			"id", "databaseId", "number", "title", "body", "state", "createdAt", "updatedAt",
			"closedAt", "author", "labels", "comments", "repository", "url",
		)}
	case "PullRequest":
		return &gqlTypeInfo{Name: "PullRequest", Kind: "OBJECT", Fields: fieldNames(
			"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName",
			"mergeable", "merged", "mergedAt", "mergeCommit", "createdAt", "updatedAt",
			"author", "repository", "files", "url",
		)}
	case "Repository":
		return &gqlTypeInfo{Name: "Repository", Kind: "OBJECT", Fields: fieldNames(
			"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
			"viewerPermission", "hasIssuesEnabled", "hasWikiEnabled", "description", "isPrivate",
			"createdAt", "updatedAt", "url", "issue", "issues", "pullRequest", "pullRequests",
		)}
	case "SearchType":
		return &gqlTypeInfo{Name: "SearchType", Kind: "ENUM", Enums: []string{
			"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT",
		}}
	case "User":
		return &gqlTypeInfo{Name: "User", Kind: "OBJECT", Fields: fieldNames(
			"id", "databaseId", "login", "name", "email", "avatarUrl",
		)}
	case "IssueState":
		return &gqlTypeInfo{Name: "IssueState", Kind: "ENUM", Enums: []string{"OPEN", "CLOSED"}}
	case "PullRequestState":
		return &gqlTypeInfo{Name: "PullRequestState", Kind: "ENUM", Enums: []string{"OPEN", "CLOSED", "MERGED"}}
	case "MergeableState":
		return &gqlTypeInfo{Name: "MergeableState", Kind: "ENUM", Enums: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}}
	case "RepositoryPermission":
		return &gqlTypeInfo{Name: "RepositoryPermission", Kind: "ENUM", Enums: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}}
	}
	return nil
}
