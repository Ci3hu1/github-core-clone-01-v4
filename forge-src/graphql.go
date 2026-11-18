package main

// GraphQL v4: a small lexer/parser/executor for exactly the documents the
// conformance clients send (queries, mutations, batch, __type introspection,
// node(id:) with inline fragments, cursor pagination).

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// --- lexer ---------------------------------------------------------------------

type gqlTok struct {
	kind string // "name" | "string" | "int" | "punct" | "var" | "eof"
	val  string
}

func gqlLex(src string) ([]gqlTok, error) {
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
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"punct", "..."})
			i += 3
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlTok{"var", src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
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
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, errBadJSON
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			toks = append(toks, gqlTok{"int", src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		default:
			return nil, errBadJSON
		}
	}
	toks = append(toks, gqlTok{"eof", ""})
	return toks, nil
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

// --- parser ----------------------------------------------------------------------

type gqlArgVal struct {
	kind string // "var" | "string" | "int" | "bool" | "null" | "enum" | "list" | "object"
	s    string
	list []gqlArgVal
	obj  map[string]gqlArgVal
}

type gqlField struct {
	alias    string
	name     string
	args     map[string]gqlArgVal
	children []*gqlField
	fragOn   string // non-empty for inline fragments "... on Type"
}

type gqlOp struct {
	kind   string // "query" | "mutation"
	fields []*gqlField
}

type gqlParser struct {
	toks []gqlTok
	i    int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.i] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.i]; p.i++; return t }
func (p *gqlParser) expect(kind, val string) (gqlTok, bool) {
	t := p.peek()
	if t.kind == kind && (val == "" || t.val == val) {
		p.i++
		return t, true
	}
	return t, false
}

func parseGraphQL(src string) ([]*gqlOp, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	var ops []*gqlOp
	for p.peek().kind != "eof" {
		op, err := p.operation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) operation() (*gqlOp, error) {
	op := &gqlOp{kind: "query"}
	if t := p.peek(); t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = t.val
		p.next()
		// optional operation name
		if t := p.peek(); t.kind == "name" {
			p.next()
		}
		// optional variable definitions: skip balanced parens
		if t := p.peek(); t.kind == "punct" && t.val == "(" {
			if err := p.skipBalanced("(", ")"); err != nil {
				return nil, err
			}
		}
	}
	fields, err := p.selectionSet()
	if err != nil {
		return nil, err
	}
	op.fields = fields
	return op, nil
}

func (p *gqlParser) skipBalanced(open, close string) error {
	depth := 0
	for {
		t := p.next()
		if t.kind == "eof" {
			return errBadJSON
		}
		if t.kind == "punct" && t.val == open {
			depth++
		} else if t.kind == "punct" && t.val == close {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) selectionSet() ([]*gqlField, error) {
	if _, ok := p.expect("punct", "{"); !ok {
		return nil, errBadJSON
	}
	var fields []*gqlField
	for {
		t := p.peek()
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return fields, nil
		}
		if t.kind == "eof" {
			return nil, errBadJSON
		}
		f, err := p.field()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) field() (*gqlField, error) {
	// inline fragment: ... on Type { ... }
	if t := p.peek(); t.kind == "punct" && t.val == "..." {
		p.next()
		f := &gqlField{}
		if t := p.peek(); t.kind == "name" && t.val == "on" {
			p.next()
			nt, ok := p.expect("name", "")
			if !ok {
				return nil, errBadJSON
			}
			f.fragOn = nt.val
		}
		children, err := p.selectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
		return f, nil
	}
	nameTok, ok := p.expect("name", "")
	if !ok {
		return nil, errBadJSON
	}
	f := &gqlField{name: nameTok.val}
	// alias: name ':' name
	if t := p.peek(); t.kind == "punct" && t.val == ":" {
		p.next()
		realTok, ok := p.expect("name", "")
		if !ok {
			return nil, errBadJSON
		}
		f.alias = f.name
		f.name = realTok.val
	}
	// arguments
	if t := p.peek(); t.kind == "punct" && t.val == "(" {
		p.next()
		args := make(map[string]gqlArgVal)
		for {
			t := p.peek()
			if t.kind == "punct" && t.val == ")" {
				p.next()
				break
			}
			if t.kind == "eof" {
				return nil, errBadJSON
			}
			nt, ok := p.expect("name", "")
			if !ok {
				return nil, errBadJSON
			}
			if _, ok := p.expect("punct", ":"); !ok {
				return nil, errBadJSON
			}
			v, err := p.argValue()
			if err != nil {
				return nil, err
			}
			args[nt.val] = v
		}
		f.args = args
	}
	// selection set
	if t := p.peek(); t.kind == "punct" && t.val == "{" {
		children, err := p.selectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
	}
	return f, nil
}

func (p *gqlParser) argValue() (gqlArgVal, error) {
	t := p.next()
	switch t.kind {
	case "var":
		return gqlArgVal{kind: "var", s: t.val}, nil
	case "string":
		return gqlArgVal{kind: "string", s: t.val}, nil
	case "int":
		return gqlArgVal{kind: "int", s: t.val}, nil
	case "name":
		switch t.val {
		case "true":
			return gqlArgVal{kind: "bool", s: "true"}, nil
		case "false":
			return gqlArgVal{kind: "bool", s: "false"}, nil
		case "null":
			return gqlArgVal{kind: "null"}, nil
		default:
			return gqlArgVal{kind: "enum", s: t.val}, nil
		}
	case "punct":
		if t.val == "[" {
			var list []gqlArgVal
			for {
				nt := p.peek()
				if nt.kind == "punct" && nt.val == "]" {
					p.next()
					break
				}
				v, err := p.argValue()
				if err != nil {
					return gqlArgVal{}, err
				}
				list = append(list, v)
			}
			return gqlArgVal{kind: "list", list: list}, nil
		}
		if t.val == "{" {
			obj := make(map[string]gqlArgVal)
			for {
				nt := p.peek()
				if nt.kind == "punct" && nt.val == "}" {
					p.next()
					break
				}
				kt, ok := p.expect("name", "")
				if !ok {
					return gqlArgVal{}, errBadJSON
				}
				if _, ok := p.expect("punct", ":"); !ok {
					return gqlArgVal{}, errBadJSON
				}
				v, err := p.argValue()
				if err != nil {
					return gqlArgVal{}, err
				}
				obj[kt.val] = v
			}
			return gqlArgVal{kind: "object", obj: obj}, nil
		}
	}
	return gqlArgVal{}, errBadJSON
}

// resolveArg turns a parsed argument value into a plain Go value using vars.
func resolveArg(v gqlArgVal, vars map[string]any) any {
	switch v.kind {
	case "var":
		if vars != nil {
			return vars[v.s]
		}
		return nil
	case "string", "enum":
		return v.s
	case "int":
		n, _ := strconv.Atoi(v.s)
		return n
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, resolveArg(e, vars))
		}
		return out
	case "object":
		out := make(map[string]any)
		for k, e := range v.obj {
			out[k] = resolveArg(e, vars)
		}
		return out
	}
	return nil
}

func (f *gqlField) arg(name string, vars map[string]any) any {
	if f.args == nil {
		return nil
	}
	v, ok := f.args[name]
	if !ok {
		return nil
	}
	return resolveArg(v, vars)
}

func (f *gqlField) argStr(name string, vars map[string]any) string {
	return asStr(f.arg(name, vars))
}

func (f *gqlField) argInt(name string, vars map[string]any, def int) int {
	return asInt(f.arg(name, vars), def)
}

// --- executor --------------------------------------------------------------------

type gqlErr struct{ msg string }

type gqlConn struct {
	nodes     []any // *Issue or *Pull
	nodeKind  string
	hasNext   bool
	endCursor string
}

type gqlTypeInfo struct {
	name string
}

type gqlViewer struct{}

type gqlUser struct{ login string }

type gqlRef struct {
	name string
	oid  string
}

type gqlEdge struct{ node any }

func gqlCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(idx)))
}

func gqlParseCursor(c string) int {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return -1
	}
	s := string(raw)
	if !strings.HasPrefix(s, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

var gqlIssueFields = []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "url", "author", "comments", "labels", "assignees", "milestone", "closedAt", "repository", "authorAssociation"}
var gqlPullFields = []string{"id", "number", "title", "state", "headRefName", "baseRefName", "mergeable", "merged", "files", "body", "createdAt", "updatedAt", "url", "author", "headRef", "baseRef", "repository", "isDraft", "additions", "deletions"}
var gqlRepoFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt", "homepageUrl", "isPrivate", "isArchived"}

func (s *Server) handleGraphQL(req *Request) Response {
	if req.Method != "POST" {
		return errJSON(404, "Not Found")
	}
	if !s.requireAuth(req) {
		return errJSON(401, "Requires authentication")
	}
	v, err := parseJSON(req.Body)
	if err != nil {
		return errJSON(400, "Invalid JSON")
	}
	switch body := v.(type) {
	case []any:
		var parts [][]byte
		for _, item := range body {
			parts = append(parts, s.execGraphQLRequest(item))
		}
		var out []byte
		out = append(out, '[')
		for i, p := range parts {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, p...)
		}
		out = append(out, ']')
		return jsonResp(200, out)
	default:
		return jsonResp(200, s.execGraphQLRequest(v))
	}
}

func (s *Server) execGraphQLRequest(v any) []byte {
	m := asObj(v)
	query := asStr(m["query"])
	vars := asObj(m["variables"])
	ops, err := parseGraphQL(query)
	if err != nil || len(ops) == 0 {
		return []byte(`{"data":null,"errors":[{"message":"GraphQL parse error"}]}`)
	}
	op := ops[0]
	rootKind := "Query"
	if op.kind == "mutation" {
		rootKind = "Mutation"
	}
	var errs []gqlErr
	data := s.execSelection(op.kind, rootKind, nil, op.fields, vars, &errs)
	var out []byte
	out = append(out, `{"data":`...)
	writeJSONValue(&out, data)
	if len(errs) > 0 {
		out = append(out, `,"errors":[`...)
		for i, e := range errs {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, `{"message":`...)
			escapeJSONString(&out, e.msg)
			out = append(out, '}')
		}
		out = append(out, ']')
	}
	out = append(out, '}')
	return out
}

// execSelection executes fields against a parent object of the given kind.
// kind: "Query" | "Mutation" | "Repository" | "Issue" | "PullRequest" |
// "Viewer" | "User" | "Ref" | "IssueConnection" | "PullRequestConnection" |
// "__Type" | "Comment" | "CommentEdge" | "Node".
func (s *Server) execSelection(opKind, kind string, obj any, fields []*gqlField, vars map[string]any, errs *[]gqlErr) map[string]any {
	out := make(map[string]any)
	for _, f := range fields {
		if f.fragOn != "" {
			// Inline fragment: applies only when the parent type matches.
			if f.fragOn != kind && !(kind == "Node" && f.fragOn == nodeKindOf(obj)) {
				continue
			}
			sub := s.execSelection(opKind, kind, obj, f.children, vars, errs)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		val, ok := s.execField(opKind, kind, obj, f, vars, errs)
		if !ok {
			*errs = append(*errs, gqlErr{msg: `Cannot query field "` + f.name + `" on type "` + kind + `".`})
			continue
		}
		out[key] = val
	}
	return out
}

func nodeKindOf(obj any) string {
	switch obj.(type) {
	case *Repo:
		return "Repository"
	case *Issue:
		return "Issue"
	case *Pull:
		return "PullRequest"
	case *Comment:
		return "IssueComment"
	}
	return ""
}

// execField resolves one field. Returns ok=false when the field is unknown.
func (s *Server) execField(opKind, kind string, obj any, f *gqlField, vars map[string]any, errs *[]gqlErr) (any, bool) {
	switch kind {
	case "Query":
		switch f.name {
		case "viewer":
			return s.execSelection(opKind, "Viewer", &gqlViewer{}, f.children, vars, errs), true
		case "repository":
			repo := s.lookupRepo(f.argStr("owner", vars), f.argStr("name", vars))
			if repo == nil {
				return nil, true
			}
			return s.execSelection(opKind, "Repository", repo, f.children, vars, errs), true
		case "node":
			id := f.argStr("id", vars)
			target, _ := s.resolveNode(id)
			if target == nil {
				return nil, true
			}
			return s.execSelection(opKind, "Node", target, f.children, vars, errs), true
		case "__type":
			name := f.argStr("name", vars)
			if name == "" {
				return nil, true
			}
			return s.execSelection(opKind, "__Type", &gqlTypeInfo{name: name}, f.children, vars, errs), true
		}
		return nil, false
	case "Mutation":
		return s.execMutation(f, vars, errs)
	case "Viewer":
		switch f.name {
		case "login":
			return viewerLogin, true
		case "id":
			return globalID("User", "1000"), true
		case "name":
			return viewerName, true
		case "email":
			return viewerEmail, true
		}
		return nil, false
	case "User":
		u := obj.(*gqlUser)
		switch f.name {
		case "login":
			return u.login, true
		case "id":
			return globalID("User", u.login), true
		case "name":
			return viewerName, true
		case "email":
			return viewerEmail, true
		}
		return nil, false
	case "Repository":
		return s.execRepoField(obj.(*Repo), f, vars, errs, opKind)
	case "Issue":
		return s.execIssueField(obj.(*Issue), f, vars, errs)
	case "PullRequest":
		return s.execPullField(obj.(*Pull), f, vars, errs, opKind)
	case "Ref":
		r := obj.(*gqlRef)
		switch f.name {
		case "name":
			return r.name, true
		case "target":
			return map[string]any{"oid": r.oid}, true
		}
		return nil, false
	case "IssueConnection", "PullRequestConnection":
		c := obj.(*gqlConn)
		switch f.name {
		case "nodes":
			var nodes []any
			for _, n := range c.nodes {
				switch nn := n.(type) {
				case *Issue:
					nodes = append(nodes, s.execSelection(opKind, "Issue", nn, f.children, vars, errs))
				case *Pull:
					nodes = append(nodes, s.execSelection(opKind, "PullRequest", nn, f.children, vars, errs))
				}
			}
			if nodes == nil {
				nodes = []any{}
			}
			return nodes, true
		case "pageInfo":
			pi := map[string]any{"hasNextPage": c.hasNext}
			if c.endCursor == "" {
				pi["endCursor"] = nil
			} else {
				pi["endCursor"] = c.endCursor
			}
			return pi, true
		case "totalCount":
			return len(c.nodes), true
		case "edges":
			return []any{}, true
		}
		return nil, false
	case "__Type":
		t := obj.(*gqlTypeInfo)
		switch f.name {
		case "name":
			return t.name, true
		case "kind":
			if t.name == "SearchType" {
				return "ENUM", true
			}
			return "OBJECT", true
		case "fields":
			var names []string
			switch t.name {
			case "Issue":
				names = gqlIssueFields
			case "PullRequest":
				names = gqlPullFields
			case "Repository":
				names = gqlRepoFields
			default:
				names = []string{}
			}
			var out []any
			for _, n := range names {
				out = append(out, map[string]any{"name": n})
			}
			return out, true
		case "enumValues":
			if t.name == "SearchType" {
				var out []any
				for _, n := range []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "LABEL", "TOPIC", "CODE"} {
					out = append(out, map[string]any{"name": n})
				}
				return out, true
			}
			return []any{}, true
		}
		return nil, false
	case "Node":
		switch f.name {
		case "id":
			switch o := obj.(type) {
			case *Repo:
				return globalID("Repository", strconv.Itoa(o.id)), true
			case *Issue:
				if r := s.repoOfIssue(o); r != nil {
					return globalID("Issue", strconv.Itoa(r.id)+":"+strconv.Itoa(o.number)), true
				}
				return globalID("Issue", strconv.Itoa(o.number)), true
			case *Pull:
				if r := s.repoOfPull(o); r != nil {
					return globalID("PullRequest", strconv.Itoa(r.id)+":"+strconv.Itoa(o.number)), true
				}
				return globalID("PullRequest", strconv.Itoa(o.number)), true
			}
			return nil, false
		}
		// Other fields resolve against the concrete type.
		ck := nodeKindOf(obj)
		return s.execField(opKind, ck, obj, f, vars, errs)
	case "Comment":
		c := obj.(*Comment)
		switch f.name {
		case "id":
			return globalID("IssueComment", strconv.Itoa(c.id)), true
		case "body":
			return c.body, true
		}
		return nil, false
	case "CommentEdge":
		e := obj.(*gqlEdge)
		if f.name == "node" {
			return s.execSelection(opKind, "Comment", e.node, f.children, vars, errs), true
		}
		return nil, false
	}
	return nil, false
}

func (s *Server) execRepoField(repo *Repo, f *gqlField, vars map[string]any, errs *[]gqlErr, opKind string) (any, bool) {
	switch f.name {
	case "id":
		return globalID("Repository", strconv.Itoa(repo.id)), true
	case "databaseId":
		return repo.id, true
	case "name":
		return repo.name, true
	case "nameWithOwner":
		return repo.owner + "/" + repo.name, true
	case "owner":
		return s.execSelection(opKind, "User", &gqlUser{login: repo.owner}, f.children, vars, errs), true
	case "defaultBranchRef":
		tip, ok := repo.defaultTip()
		if !ok {
			return nil, true
		}
		return s.execSelection(opKind, "Ref", &gqlRef{name: "main", oid: tip}, f.children, vars, errs), true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "description":
		return repo.description, true
	case "private":
		return repo.private, true
	case "url":
		return "https://example.invalid/" + repo.owner + "/" + repo.name, true
	case "issue":
		num := f.argInt("number", vars, 0)
		is, ok := repo.issuesByNo[num]
		if !ok {
			return nil, true
		}
		return s.execSelection(opKind, "Issue", is, f.children, vars, errs), true
	case "issues":
		return s.execIssuesConn(repo, f, vars, errs, opKind), true
	case "pullRequest":
		num := f.argInt("number", vars, 0)
		pr, ok := repo.pullsByNo[num]
		if !ok {
			return nil, true
		}
		return s.execSelection(opKind, "PullRequest", pr, f.children, vars, errs), true
	case "pullRequests":
		return s.execPullsConn(repo, f, vars, errs, opKind), true
	}
	return nil, false
}

func (s *Server) execIssuesConn(repo *Repo, f *gqlField, vars map[string]any, errs *[]gqlErr, opKind string) any {
	// states filter
	states := map[string]bool{}
	for _, sv := range asArr(f.arg("states", vars)) {
		states[asStr(sv)] = true
	}
	var list []*Issue
	for _, is := range repo.issues {
		if len(states) > 0 {
			want := "OPEN"
			if is.state == "closed" {
				want = "CLOSED"
			}
			if !states[want] {
				continue
			}
		}
		list = append(list, is)
	}
	// orderBy
	desc := false
	if ob := asObj(f.arg("orderBy", vars)); ob != nil {
		if asStr(ob["direction"]) == "DESC" {
			desc = true
		}
	}
	if desc {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	return s.pageConn(f, vars, list, "Issue", opKind, errs)
}

func (s *Server) execPullsConn(repo *Repo, f *gqlField, vars map[string]any, errs *[]gqlErr, opKind string) any {
	var list []*Pull
	for _, pr := range repo.pulls {
		list = append(list, pr)
	}
	return s.pageConn(f, vars, list, "PullRequest", opKind, errs)
}

// pageConn applies first/after over an ordered list and returns the
// connection object rendered through the requested subfields.
func (s *Server) pageConn(f *gqlField, vars map[string]any, list any, nodeKind, opKind string, errs *[]gqlErr) any {
	first := f.argInt("first", vars, 0)
	after := f.argStr("after", vars)
	start := 0
	if after != "" {
		if idx := gqlParseCursor(after); idx >= 0 {
			start = idx + 1
		}
	}
	var nodes []any
	var total int
	switch l := list.(type) {
	case []*Issue:
		total = len(l)
		if first <= 0 {
			first = total
		}
		end := start + first
		if end > total {
			end = total
		}
		if start > total {
			start = total
		}
		for _, is := range l[start:end] {
			nodes = append(nodes, is)
		}
	case []*Pull:
		total = len(l)
		if first <= 0 {
			first = total
		}
		end := start + first
		if end > total {
			end = total
		}
		if start > total {
			start = total
		}
		for _, pr := range l[start:end] {
			nodes = append(nodes, pr)
		}
	}
	hasNext := start+len(nodes) < total
	endCursor := ""
	if len(nodes) > 0 {
		endCursor = gqlCursor(start + len(nodes) - 1)
	}
	conn := &gqlConn{nodes: nodes, nodeKind: nodeKind, hasNext: hasNext, endCursor: endCursor}
	kind := "IssueConnection"
	if nodeKind == "PullRequest" {
		kind = "PullRequestConnection"
	}
	return s.execSelection(opKind, kind, conn, f.children, vars, errs)
}

func (s *Server) execIssueField(is *Issue, f *gqlField, vars map[string]any, errs *[]gqlErr) (any, bool) {
	// Need the repo for global ids; find it by scanning (cheap: few repos).
	repo := s.repoOfIssue(is)
	switch f.name {
	case "id":
		if repo != nil {
			return globalID("Issue", strconv.Itoa(repo.id)+":"+strconv.Itoa(is.number)), true
		}
		return globalID("Issue", strconv.Itoa(is.number)), true
	case "number":
		return is.number, true
	case "title":
		return is.title, true
	case "body":
		return is.body, true
	case "state":
		if is.state == "closed" {
			return "CLOSED", true
		}
		return "OPEN", true
	case "createdAt", "updatedAt":
		return fixedTimeISO, true
	case "closedAt":
		if is.state == "closed" {
			return fixedTimeISO, true
		}
		return nil, true
	case "url":
		if repo != nil {
			return "https://example.invalid/" + repo.owner + "/" + repo.name + "/issues/" + strconv.Itoa(is.number), true
		}
		return "", true
	case "author":
		return map[string]any{"login": viewerLogin}, true
	}
	return nil, false
}

func (s *Server) execPullField(pr *Pull, f *gqlField, vars map[string]any, errs *[]gqlErr, opKind string) (any, bool) {
	repo := s.repoOfPull(pr)
	switch f.name {
	case "id":
		if repo != nil {
			return globalID("PullRequest", strconv.Itoa(repo.id)+":"+strconv.Itoa(pr.number)), true
		}
		return globalID("PullRequest", strconv.Itoa(pr.number)), true
	case "number":
		return pr.number, true
	case "title":
		return pr.title, true
	case "body":
		return pr.body, true
	case "state":
		if pr.merged {
			return "MERGED", true
		}
		if pr.closed {
			return "CLOSED", true
		}
		return "OPEN", true
	case "headRefName":
		return pr.headRef, true
	case "baseRefName":
		return pr.baseRef, true
	case "headRef":
		return map[string]any{"name": pr.headRef}, true
	case "baseRef":
		return map[string]any{"name": pr.baseRef}, true
	case "mergeable":
		if repo != nil && repo.mergeConflict(repo.liveBaseTip(pr), pr.headSha) {
			return "CONFLICTING", true
		}
		return "MERGEABLE", true
	case "merged":
		return pr.merged, true
	case "createdAt", "updatedAt":
		return fixedTimeISO, true
	case "url":
		if repo != nil {
			return "https://example.invalid/" + repo.owner + "/" + repo.name + "/pull/" + strconv.Itoa(pr.number), true
		}
		return "", true
	case "author":
		return map[string]any{"login": viewerLogin}, true
	case "files":
		if repo == nil {
			return s.execSelection(opKind, "PullRequestConnection", &gqlConn{nodes: []any{}}, f.children, vars, errs), true
		}
		baseTree, headTree := "", ""
		if o := repo.objects[pr.baseSha]; o != nil {
			baseTree = parseCommit(o.data).tree
		}
		if o := repo.objects[pr.headSha]; o != nil {
			headTree = parseCommit(o.data).tree
		}
		changes := repo.diffTrees(baseTree, headTree)
		first := f.argInt("first", vars, 0)
		if first <= 0 {
			first = len(changes)
		}
		if first > len(changes) {
			first = len(changes)
		}
		var nodes []any
		for _, c := range changes[:first] {
			nodes = append(nodes, map[string]any{"path": c.path})
		}
		// Build the connection shape the query asks for.
		result := map[string]any{}
		for _, cf := range f.children {
			if cf.name == "nodes" {
				var rows []any
				for _, n := range nodes {
					m := n.(map[string]any)
					row := map[string]any{}
					for _, nf := range cf.children {
						if nf.name == "path" {
							row["path"] = m["path"]
						}
					}
					rows = append(rows, row)
				}
				if rows == nil {
					rows = []any{}
				}
				result["nodes"] = rows
			} else if cf.name == "pageInfo" {
				result["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": nil}
			} else if cf.name == "totalCount" {
				result["totalCount"] = len(changes)
			}
		}
		return result, true
	}
	return nil, false
}

func (s *Server) repoOfIssue(is *Issue) *Repo {
	for _, r := range s.store.reposList {
		if r.issuesByNo[is.number] == is {
			return r
		}
	}
	return nil
}

func (s *Server) repoOfPull(pr *Pull) *Repo {
	for _, r := range s.store.reposList {
		if r.pullsByNo[pr.number] == pr {
			return r
		}
	}
	return nil
}

func (s *Server) resolveNode(id string) (any, string) {
	kind, payload, ok := decodeGlobalID(id)
	if !ok {
		return nil, ""
	}
	switch kind {
	case "Repository":
		rid, err := strconv.Atoi(payload)
		if err != nil {
			return nil, ""
		}
		for _, r := range s.store.reposList {
			if r.id == rid {
				return r, "Repository"
			}
		}
	case "Issue":
		parts := strings.SplitN(payload, ":", 2)
		if len(parts) != 2 {
			return nil, ""
		}
		rid, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		for _, r := range s.store.reposList {
			if r.id == rid {
				if is, ok := r.issuesByNo[num]; ok {
					return is, "Issue"
				}
			}
		}
	case "PullRequest":
		parts := strings.SplitN(payload, ":", 2)
		if len(parts) != 2 {
			return nil, ""
		}
		rid, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		for _, r := range s.store.reposList {
			if r.id == rid {
				if pr, ok := r.pullsByNo[num]; ok {
					return pr, "PullRequest"
				}
			}
		}
	}
	return nil, ""
}

// --- mutations ---------------------------------------------------------------------

func (s *Server) execMutation(f *gqlField, vars map[string]any, errs *[]gqlErr) (any, bool) {
	input := asObj(f.arg("input", vars))
	switch f.name {
	case "createIssue":
		repoID := asStr(input["repositoryId"])
		kind, payload, ok := decodeGlobalID(repoID)
		if !ok || kind != "Repository" {
			*errs = append(*errs, gqlErr{msg: "Could not resolve repository id"})
			return map[string]any{}, true
		}
		rid, _ := strconv.Atoi(payload)
		var repo *Repo
		for _, r := range s.store.reposList {
			if r.id == rid {
				repo = r
				break
			}
		}
		if repo == nil {
			*errs = append(*errs, gqlErr{msg: "Could not resolve repository id"})
			return map[string]any{}, true
		}
		is := s.newIssue(repo, asStr(input["title"]), asStr(input["body"]), nil)
		// The mutation payload is {issue: {...}}; children are the
		// fields of createIssue's payload, so re-dispatch them.
		return s.execMutationPayload("createIssue", is, repo, f, vars, errs), true
	case "closeIssue":
		issueID := asStr(input["issueId"])
		target, _ := s.resolveNode(issueID)
		is, ok := target.(*Issue)
		if !ok {
			*errs = append(*errs, gqlErr{msg: "Could not resolve issue id"})
			return map[string]any{}, true
		}
		is.state = "closed"
		if repo := s.repoOfIssue(is); repo != nil {
			repo.renderIssue(is)
			repo.bump()
		}
		return s.execMutationPayload("closeIssue", is, nil, f, vars, errs), true
	case "createPullRequest":
		repoID := asStr(input["repositoryId"])
		kind, payload, ok := decodeGlobalID(repoID)
		if !ok || kind != "Repository" {
			*errs = append(*errs, gqlErr{msg: "Could not resolve repository id"})
			return map[string]any{}, true
		}
		rid, _ := strconv.Atoi(payload)
		var repo *Repo
		for _, r := range s.store.reposList {
			if r.id == rid {
				repo = r
				break
			}
		}
		if repo == nil {
			*errs = append(*errs, gqlErr{msg: "Could not resolve repository id"})
			return map[string]any{}, true
		}
		head := asStr(input["headRefName"])
		base := asStr(input["baseRefName"])
		headSha, ok1 := repo.refTip("refs/heads/" + head)
		baseSha, ok2 := repo.refTip("refs/heads/" + base)
		if !ok1 || !ok2 {
			*errs = append(*errs, gqlErr{msg: "Head or base branch does not exist"})
			return map[string]any{}, true
		}
		pr := s.newPull(repo, asStr(input["title"]), asStr(input["body"]), head, base, headSha, baseSha)
		return s.execMutationPayload("createPullRequest", pr, repo, f, vars, errs), true
	case "mergePullRequest":
		prID := asStr(input["pullRequestId"])
		target, _ := s.resolveNode(prID)
		pr, ok := target.(*Pull)
		if !ok {
			*errs = append(*errs, gqlErr{msg: "Could not resolve pull request id"})
			return map[string]any{}, true
		}
		repo := s.repoOfPull(pr)
		if repo == nil {
			*errs = append(*errs, gqlErr{msg: "Could not resolve repository"})
			return map[string]any{}, true
		}
		if !pr.merged {
			if _, conflict := repo.mergePR(pr); conflict {
				*errs = append(*errs, gqlErr{msg: "Merge conflict"})
				return map[string]any{}, true
			}
		}
		return s.execMutationPayload("mergePullRequest", pr, repo, f, vars, errs), true
	case "addComment":
		subjectID := asStr(input["subjectId"])
		target, _ := s.resolveNode(subjectID)
		is, ok := target.(*Issue)
		if !ok {
			*errs = append(*errs, gqlErr{msg: "Could not resolve subject id"})
			return map[string]any{}, true
		}
		c := &Comment{id: s.store.nextGlobalID(), body: asStr(input["body"]), issueNo: is.number}
		is.comments = append(is.comments, c)
		if repo := s.repoOfIssue(is); repo != nil {
			repo.renderIssue(is)
			repo.bump()
		}
		return s.execMutationPayload("addComment", c, nil, f, vars, errs), true
	}
	return nil, false
}

// execMutationPayload renders the payload object of a mutation: its
// children name the payload fields (issue, pullRequest, commentEdge).
func (s *Server) execMutationPayload(mut string, obj any, repo *Repo, f *gqlField, vars map[string]any, errs *[]gqlErr) map[string]any {
	out := map[string]any{}
	for _, cf := range f.children {
		switch cf.name {
		case "issue":
			is := obj.(*Issue)
			out["issue"] = s.execSelection("mutation", "Issue", is, cf.children, vars, errs)
		case "pullRequest":
			pr := obj.(*Pull)
			out["pullRequest"] = s.execSelection("mutation", "PullRequest", pr, cf.children, vars, errs)
		case "commentEdge":
			c := obj.(*Comment)
			edge := &gqlEdge{node: c}
			out["commentEdge"] = s.execSelection("mutation", "CommentEdge", edge, cf.children, vars, errs)
		case "clientMutationId":
			out["clientMutationId"] = nil
		}
	}
	return out
}
