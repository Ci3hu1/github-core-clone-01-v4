package main

// REST v3 surface: repos, branches, commits, git refs, contents, labels,
// issues, pulls. Every reply is JSON with X-GitHub-Request-Id (added by
// the transport) and error bodies {"message": string}.

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// --- dispatch ---------------------------------------------------------------

func (s *Server) dispatch(req *Request) Response {
	// /healthz is unauthenticated.
	if req.Path == "/healthz" {
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	// Git smart HTTP.
	if strings.HasSuffix(req.Path, ".git/info/refs") || strings.Contains(req.Path, ".git/git-") {
		return s.dispatchGit(req)
	}
	if req.Path == "/api/graphql" || strings.HasPrefix(req.Path, "/api/graphql/") {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		return s.handleGraphQL(req)
	}
	if req.Path == "/mcp" || strings.HasPrefix(req.Path, "/mcp/") {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		return s.handleMCP(req)
	}
	if req.Path == "/api/v3" || strings.HasPrefix(req.Path, "/api/v3/") {
		readOnly := req.Method == "GET" || req.Method == "HEAD"
		if readOnly {
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
		} else {
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
		}
		return s.dispatchRest(req)
	}
	return errJSON(404, "Not Found")
}

func (s *Server) requireAuth(req *Request) bool {
	return strings.TrimSpace(req.Auth) != ""
}

func (s *Server) parseBody(req *Request) (map[string]any, bool) {
	if len(req.Body) == 0 {
		return map[string]any{}, true
	}
	v, err := parseJSON(req.Body)
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

// --- git dispatch -----------------------------------------------------------

func (s *Server) dispatchGit(req *Request) Response {
	// Path: /{owner}/{repo}.git/info/refs or /{owner}/{repo}.git/git-*-pack
	parts := splitPath(req.Path)
	if len(parts) < 3 {
		return errJSON(404, "Not Found")
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	if !strings.HasSuffix(parts[1], ".git") {
		return errJSON(404, "Not Found")
	}
	s.store.mu.RLock()
	repo := s.store.getRepo(owner, repoName)
	s.store.mu.RUnlock()
	if repo == nil || owner != viewerLogin {
		return errJSON(404, "Not Found")
	}
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" {
		if req.Method != "GET" {
			return errJSON(405, "Method Not Allowed")
		}
		service := req.QueryParam("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return errJSON(404, "Not Found")
		}
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		var body []byte
		if service == "git-upload-pack" && req.Header("git-protocol") == "version=2" {
			body = advertisementV2()
		} else {
			var b []byte
			b = append(b, pktLineStr("# service="+service+"\n")...)
			b = append(b, pktFlush...)
			for _, l := range repo.refLinesV0(service) {
				b = append(b, l...)
			}
			b = append(b, pktFlush...)
			body = b
		}
		return Response{Status: 200, ContentType: "application/x-" + service + "-advertisement", Body: body}
	}
	if len(parts) == 3 && parts[2] == "git-upload-pack" {
		if req.Method != "POST" {
			return errJSON(405, "Method Not Allowed")
		}
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		var out []byte
		var err error
		if req.Header("git-protocol") == "version=2" {
			out, err = repo.uploadPackV2(req.Body)
		} else {
			out, err = repo.uploadPackV0(req.Body)
		}
		if err != nil {
			return Response{Status: 400, ContentType: "text/plain", Body: nil}
		}
		return Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out}
	}
	if len(parts) == 3 && parts[2] == "git-receive-pack" {
		if req.Method != "POST" {
			return errJSON(405, "Method Not Allowed")
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		out, err := repo.receivePack(req.Body)
		if err != nil {
			return Response{Status: 400, ContentType: "text/plain", Body: nil}
		}
		return Response{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: out}
	}
	return errJSON(404, "Not Found")
}

// --- helpers ------------------------------------------------------------------

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (s *Server) lookupRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.store.getRepo(owner, name)
}

// paginate slices items (already ordered) per per_page/page query params and
// returns the page plus a Link header value ("" when there is no next page).
func paginate(req *Request, total int) (start, end int, link string) {
	perPage := atoiDef(req.QueryParam("per_page"), 30)
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	page := atoiDef(req.QueryParam("page"), 1)
	if page < 1 {
		page = 1
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	if end < total {
		link = "<" + nextPageURL(req, page+1, perPage) + `>; rel="next"`
	}
	return start, end, link
}

func nextPageURL(req *Request, page, perPage int) string {
	host := req.Host
	if host == "" {
		host = "127.0.0.1"
	}
	var q []string
	for _, kv := range strings.Split(req.Query, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if k == "page" || k == "per_page" {
			continue
		}
		q = append(q, kv)
	}
	q = append(q, "per_page="+strconv.Itoa(perPage), "page="+strconv.Itoa(page))
	return "http://" + host + req.Path + "?" + strings.Join(q, "&")
}

func joinJSON(items [][]byte) []byte {
	var b []byte
	b = append(b, '[')
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	b = append(b, ']')
	return b
}

// --- REST dispatch ------------------------------------------------------------

func (s *Server) dispatchRest(req *Request) Response {
	p := req.Path
	if p == "/api/v3" || p == "/api/v3/" {
		if req.Method != "GET" {
			return errJSON(404, "Not Found")
		}
		return Response{Status: 200, ContentType: "application/json",
			Body:   []byte(`{"current_user_url":"https://example.invalid/api/v3/user","message":"GitHub API"}`),
			Headers: map[string]string{"X-OAuth-Scopes": "repo"}}
	}
	if p == "/api/v3/meta" {
		if req.Method != "GET" {
			return errJSON(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"installed_version":`+jsonString(metaVersion)+`}`))
	}
	if !s.requireAuth(req) {
		return errJSON(401, "Requires authentication")
	}
	if p == "/api/v3/user" {
		if req.Method != "GET" {
			return errJSON(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"login":"adaiversen","id":1000,"node_id":`+jsonString(globalID("User", "1000"))+`,"name":"ada iversen","email":"adaiversen@example.invalid","type":"User","site_admin":false,"avatar_url":"https://example.invalid/avatar/adaiversen"}`))
	}
	parts := splitPath(p) // ["api","v3",...]
	if len(parts) < 3 {
		return errJSON(404, "Not Found")
	}
	rest := parts[2:]
	if len(rest) >= 2 && rest[0] == "user" && rest[1] == "repos" {
		if len(rest) != 2 {
			return errJSON(404, "Not Found")
		}
		if req.Method == "GET" {
			return s.listUserRepos(req)
		}
		if req.Method == "POST" {
			return s.createRepo(req)
		}
		return errJSON(404, "Not Found")
	}
	if len(rest) >= 3 && rest[0] == "repos" {
		return s.dispatchRepo(req, rest[1], rest[2], rest[3:])
	}
	return errJSON(404, "Not Found")
}

func (s *Server) listUserRepos(req *Request) Response {
	var items [][]byte
	for _, r := range s.store.reposList {
		items = append(items, r.repoJSON())
	}
	start, end, link := paginate(req, len(items))
	resp := jsonResp(200, joinJSON(items[start:end]))
	if link != "" {
		resp.Headers = map[string]string{"Link": link}
	}
	return resp
}

func (s *Server) createRepo(req *Request) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	name := asStr(body["name"])
	if name == "" {
		return errJSON(422, "Repository name is required")
	}
	if s.store.getRepo(viewerLogin, name) != nil {
		return errJSON(422, "Repository already exists")
	}
	r := s.store.createRepo(name, asBool(body["private"], false), asStr(body["description"]))
	return jsonResp(201, r.repoJSON())
}

// dispatchRepo handles /api/v3/repos/{owner}/{name}/...
func (s *Server) dispatchRepo(req *Request, owner, name string, rest []string) Response {
	repo := s.lookupRepo(owner, name)
	if repo == nil {
		return errJSON(404, "Not Found")
	}
	if len(rest) == 0 {
		switch req.Method {
		case "GET":
			return jsonResp(200, repo.repoJSON())
		case "PATCH":
			body, ok := s.parseBody(req)
			if !ok {
				return errJSON(400, "Invalid JSON")
			}
			if d, ok := body["description"]; ok {
				repo.description = asStr(d)
			}
			if pv, ok := body["private"]; ok {
				if b, isBool := pv.(bool); isBool {
					repo.private = b
				}
			}
			repo.bump()
			return jsonResp(200, repo.repoJSON())
		case "DELETE":
			delete(s.store.repos, owner+"/"+name)
			for i, r := range s.store.reposList {
				if r == repo {
					s.store.reposList = append(s.store.reposList[:i], s.store.reposList[i+1:]...)
					break
				}
			}
			return Response{Status: 204, ContentType: "application/json", Body: nil}
		}
		return errJSON(404, "Not Found")
	}
	switch rest[0] {
	case "branches":
		if len(rest) == 1 && req.Method == "GET" {
			return s.listBranches(repo)
		}
		if len(rest) >= 2 && req.Method == "GET" {
			return s.getBranch(repo, strings.Join(rest[1:], "/"))
		}
	case "commits":
		if len(rest) == 1 && req.Method == "GET" {
			return s.listCommits(req, repo)
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 {
				if req.Method == "POST" {
					return s.createRef(req, repo)
				}
				if req.Method == "GET" {
					return s.listRefs(repo)
				}
			}
			if len(rest) >= 3 && req.Method == "GET" {
				return s.getRef(repo, strings.Join(rest[2:], "/"))
			}
		}
	case "contents":
		if len(rest) >= 1 {
			path := ""
			if len(rest) > 1 {
				path = strings.Join(rest[1:], "/")
			}
			switch req.Method {
			case "PUT":
				return s.putContents(req, repo, path)
			case "GET":
				return s.getContents(req, repo, path)
			case "DELETE":
				return s.deleteContents(req, repo, path)
			}
		}
	case "labels":
		if len(rest) == 1 {
			if req.Method == "POST" {
				return s.createLabel(req, repo)
			}
			if req.Method == "GET" {
				return s.listLabels(repo)
			}
		}
	case "issues":
		if len(rest) == 1 {
			if req.Method == "POST" {
				return s.createIssue(req, repo)
			}
			if req.Method == "GET" {
				return s.listIssues(req, repo)
			}
		}
		if len(rest) >= 2 {
			num, err := strconv.Atoi(rest[1])
			if err != nil {
				return errJSON(404, "Not Found")
			}
			if len(rest) == 2 {
				if req.Method == "GET" {
					return s.getIssue(repo, num)
				}
				if req.Method == "PATCH" {
					return s.patchIssue(req, repo, num)
				}
			}
			if len(rest) == 3 && rest[2] == "comments" {
				if req.Method == "POST" {
					return s.createComment(req, repo, num)
				}
				if req.Method == "GET" {
					return s.listComments(repo, num)
				}
			}
		}
	case "pulls":
		if len(rest) == 1 {
			if req.Method == "POST" {
				return s.createPull(req, repo)
			}
			if req.Method == "GET" {
				return s.listPulls(req, repo)
			}
		}
		if len(rest) >= 2 {
			num, err := strconv.Atoi(rest[1])
			if err != nil {
				return errJSON(404, "Not Found")
			}
			if len(rest) == 2 && req.Method == "GET" {
				return s.getPull(repo, num)
			}
			if len(rest) == 3 && rest[2] == "files" && req.Method == "GET" {
				return s.listPullFiles(repo, num)
			}
			if len(rest) == 3 && rest[2] == "merge" && req.Method == "PUT" {
				return s.mergePull(req, repo, num)
			}
		}
	}
	return errJSON(404, "Not Found")
}

// --- branches / commits / refs --------------------------------------------------

func (s *Server) listBranches(repo *Repo) Response {
	var items [][]byte
	for _, b := range repo.branchNames() {
		tip, _ := repo.refTip("refs/heads/" + b)
		items = append(items, []byte(`{"name":`+jsonString(b)+`,"commit":{"sha":`+jsonString(tip)+`},"protected":false}`))
	}
	return jsonResp(200, joinJSON(items))
}

func (s *Server) getBranch(repo *Repo, branch string) Response {
	tip, ok := repo.refTip("refs/heads/" + branch)
	if !ok {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, []byte(`{"name":`+jsonString(branch)+`,"commit":{"sha":`+jsonString(tip)+`},"protected":false}`))
}

func (s *Server) resolveRefTip(repo *Repo, ref string) (string, bool) {
	if ref == "" {
		return repo.defaultTip()
	}
	if tip, ok := repo.refTip("refs/heads/" + ref); ok {
		return tip, true
	}
	if tip, ok := repo.refTip(ref); ok {
		return tip, true
	}
	if o := repo.objects[ref]; o != nil && o.typ == "commit" {
		return ref, true
	}
	return "", false
}

func (s *Server) listCommits(req *Request, repo *Repo) Response {
	tip, ok := s.resolveRefTip(repo, req.QueryParam("sha"))
	if !ok {
		return errJSON(404, "Not Found")
	}
	oids := repo.listCommits(tip)
	var items [][]byte
	for _, oid := range oids {
		o := repo.objects[oid]
		if o == nil {
			continue
		}
		c := parseCommit(o.data)
		msg := strings.TrimSuffix(c.message, "\n")
		items = append(items, []byte(`{"sha":`+jsonString(oid)+`,"commit":{"message":`+jsonString(msg)+`,"author":{"name":"ada iversen","email":"adaiversen@example.invalid","date":"`+fixedTimeISO+`"},"committer":{"name":"ada iversen","email":"adaiversen@example.invalid","date":"`+fixedTimeISO+`"}},"author":{"login":"adaiversen","id":1000},"committer":{"login":"adaiversen","id":1000},"parents":[]}`))
	}
	start, end, link := paginate(req, len(items))
	resp := jsonResp(200, joinJSON(items[start:end]))
	if link != "" {
		resp.Headers = map[string]string{"Link": link}
	}
	return resp
}

func (s *Server) createRef(req *Request, repo *Repo) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	ref := asStr(body["ref"])
	sha := asStr(body["sha"])
	if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/") {
		return errJSON(422, "Invalid ref")
	}
	if _, exists := repo.refs[ref]; exists {
		return errJSON(422, "Reference already exists")
	}
	o := repo.objects[sha]
	if o == nil || (strings.HasPrefix(ref, "refs/heads/") && o.typ != "commit") {
		return errJSON(422, "Object does not exist")
	}
	repo.setRef(ref, sha)
	repo.bump()
	return jsonResp(201, []byte(`{"ref":`+jsonString(ref)+`,"object":{"sha":`+jsonString(sha)+`},"url":"https://example.invalid/api/v3/repos/`+repo.owner+`/`+repo.name+`/git/refs/`+ref+`"}`))
}

func (s *Server) listRefs(repo *Repo) Response {
	var items [][]byte
	names := make([]string, 0, len(repo.refs))
	for n := range repo.refs {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		items = append(items, []byte(`{"ref":`+jsonString(n)+`,"object":{"sha":`+jsonString(repo.refs[n])+`}}`))
	}
	return jsonResp(200, joinJSON(items))
}

func (s *Server) getRef(repo *Repo, refPath string) Response {
	// refPath is like "heads/<branch>" or "tags/<tag>".
	full := "refs/" + refPath
	oid, ok := repo.refs[full]
	if !ok {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, []byte(`{"ref":`+jsonString(full)+`,"object":{"sha":`+jsonString(oid)+`}}`))
}

// --- contents ---------------------------------------------------------------------

func (s *Server) putContents(req *Request, repo *Repo, path string) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	if path == "" {
		return errJSON(422, "Path is required")
	}
	contentB64 := asStr(body["content"])
	raw, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return errJSON(422, "Invalid base64 content")
	}
	branch := asStr(body["branch"])
	if branch == "" {
		branch = "main"
	}
	message := asStr(body["message"])
	if message == "" {
		message = "Update " + path
	}
	tip, tipOK := repo.refTip("refs/heads/" + branch)
	if !tipOK {
		// Branch does not exist yet: start from the default branch tip
		// (or nothing for an unborn repo).
		tip, _ = repo.defaultTip()
	}
	parts := strings.Split(path, "/")
	blobOID := repo.putObject("blob", raw)
	created := true
	var curTree string
	var parents []string
	if tip != "" {
		if o := repo.objects[tip]; o != nil {
			curTree = parseCommit(o.data).tree
		}
		parents = []string{tip}
		flat := repo.flattenTree(curTree)
		if cur, exists := flat[path]; exists {
			created = false
			if sha := asStr(body["sha"]); sha != "" && sha != cur {
				return errJSON(409, "Conflict: sha does not match")
			}
		}
	}
	newTree, _ := repo.setFileInTree(curTree, parts, blobOID)
	commitOID := repo.makeCommit(newTree, parents, message)
	repo.setRef("refs/heads/"+branch, commitOID)
	repo.bump()
	status := 201
	if !created {
		status = 200
	}
	var b []byte
	b = append(b, `{"content":{"name":`...)
	escapeJSONString(&b, baseName(path))
	b = append(b, `,"path":`...)
	escapeJSONString(&b, path)
	b = append(b, `,"sha":`...)
	escapeJSONString(&b, blobOID)
	b = append(b, `,"size":`...)
	b = strconv.AppendInt(b, int64(len(raw)), 10)
	b = append(b, `,"type":"file","encoding":"base64","content":`...)
	escapeJSONString(&b, base64.StdEncoding.EncodeToString(raw))
	b = append(b, `},"commit":{"sha":`...)
	escapeJSONString(&b, commitOID)
	b = append(b, `,"message":`...)
	escapeJSONString(&b, message)
	b = append(b, `}}`...)
	return jsonResp(status, b)
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *Server) getContents(req *Request, repo *Repo, path string) Response {
	treeOID := repo.treeOIDAt(req.QueryParam("ref"))
	if treeOID == "" {
		return errJSON(404, "Not Found")
	}
	flat := repo.flattenTree(treeOID)
	if path == "" {
		return jsonResp(200, s.dirListing(repo, treeOID, ""))
	}
	if blobOID, ok := flat[path]; ok {
		o := repo.objects[blobOID]
		if o == nil {
			return errJSON(404, "Not Found")
		}
		var b []byte
		b = append(b, `{"type":"file","name":`...)
		escapeJSONString(&b, baseName(path))
		b = append(b, `,"path":`...)
		escapeJSONString(&b, path)
		b = append(b, `,"sha":`...)
		escapeJSONString(&b, blobOID)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, int64(len(o.data)), 10)
		b = append(b, `,"encoding":"base64","content":`...)
		escapeJSONString(&b, base64.StdEncoding.EncodeToString(o.data))
		b = append(b, `}`...)
		return jsonResp(200, b)
	}
	// Directory? Find the subtree for path.
	sub := s.subtreeOID(repo, treeOID, path)
	if sub == "" {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, s.dirListing(repo, sub, path))
}

func (s *Server) subtreeOID(repo *Repo, treeOID, path string) string {
	parts := strings.Split(path, "/")
	cur := treeOID
	for _, part := range parts {
		o := repo.objects[cur]
		if o == nil || o.typ != "tree" {
			return ""
		}
		found := ""
		for _, e := range parseTree(o.data) {
			if e.name == part && e.dir {
				found = e.oid
				break
			}
		}
		if found == "" {
			return ""
		}
		cur = found
	}
	return cur
}

func (s *Server) dirListing(repo *Repo, treeOID, prefix string) []byte {
	o := repo.objects[treeOID]
	var items [][]byte
	if o != nil && o.typ == "tree" {
		entries := parseTree(o.data)
		sortTreeEntries(entries)
		for _, e := range entries {
			p := e.name
			if prefix != "" {
				p = prefix + "/" + e.name
			}
			typ := "file"
			size := 0
			if e.dir {
				typ = "dir"
			} else if bo := repo.objects[e.oid]; bo != nil {
				size = len(bo.data)
			}
			var b []byte
			b = append(b, `{"type":`...)
			escapeJSONString(&b, typ)
			b = append(b, `,"name":`...)
			escapeJSONString(&b, e.name)
			b = append(b, `,"path":`...)
			escapeJSONString(&b, p)
			b = append(b, `,"sha":`...)
			escapeJSONString(&b, e.oid)
			b = append(b, `,"size":`...)
			b = strconv.AppendInt(b, int64(size), 10)
			b = append(b, `}`...)
			items = append(items, b)
		}
	}
	return joinJSON(items)
}

func (s *Server) deleteContents(req *Request, repo *Repo, path string) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	branch := asStr(body["branch"])
	if branch == "" {
		branch = "main"
	}
	tip, tipOK := repo.refTip("refs/heads/" + branch)
	if !tipOK || tip == "" {
		return errJSON(404, "Not Found")
	}
	curTree := parseCommit(repo.objects[tip].data).tree
	flat := repo.flattenTree(curTree)
	cur, exists := flat[path]
	if !exists {
		return errJSON(404, "Not Found")
	}
	if sha := asStr(body["sha"]); sha != "" && sha != cur {
		return errJSON(409, "Conflict: sha does not match")
	}
	message := asStr(body["message"])
	if message == "" {
		message = "Delete " + path
	}
	newTree, _ := repo.setFileInTree(curTree, strings.Split(path, "/"), "")
	commitOID := repo.makeCommit(newTree, []string{tip}, message)
	repo.setRef("refs/heads/"+branch, commitOID)
	repo.bump()
	return jsonResp(200, []byte(`{"commit":{"sha":`+jsonString(commitOID)+`,"message":`+jsonString(message)+`}}`))
}

// --- labels -------------------------------------------------------------------------

func (s *Server) createLabel(req *Request, repo *Repo) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	name := asStr(body["name"])
	if name == "" {
		return errJSON(422, "Label name is required")
	}
	if lb, exists := repo.labels[name]; exists {
		return jsonResp(201, s.labelJSON(repo, lb))
	}
	color := asStr(body["color"])
	if color == "" {
		color = "ededed"
	}
	lb := &Label{id: s.store.nextGlobalID(), name: name, color: color}
	repo.labels[name] = lb
	repo.labelOrder = append(repo.labelOrder, name)
	repo.bump()
	return jsonResp(201, s.labelJSON(repo, lb))
}

func (s *Server) labelJSON(repo *Repo, lb *Label) []byte {
	return []byte(`{"id":` + strconv.Itoa(lb.id) + `,"node_id":` + jsonString(globalID("Label", strconv.Itoa(lb.id))) + `,"name":` + jsonString(lb.name) + `,"color":` + jsonString(lb.color) + `,"description":"","default":false}`)
}

func (s *Server) listLabels(repo *Repo) Response {
	var items [][]byte
	for _, n := range repo.labelOrder {
		items = append(items, s.labelJSON(repo, repo.labels[n]))
	}
	return jsonResp(200, joinJSON(items))
}

// --- issues ---------------------------------------------------------------------------

func (s *Server) createIssue(req *Request, repo *Repo) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	title := asStr(body["title"])
	if title == "" {
		return errJSON(422, "Title is required")
	}
	var labels []string
	for _, lv := range asArr(body["labels"]) {
		name := asStr(lv)
		if _, exists := repo.labels[name]; !exists {
			return errJSON(422, "Validation Failed: label "+name+" does not exist")
		}
		labels = append(labels, name)
	}
	is := s.newIssue(repo, title, asStr(body["body"]), labels)
	return jsonResp(201, is.cached)
}

func (s *Server) newIssue(repo *Repo, title, body string, labels []string) *Issue {
	is := &Issue{
		id:     s.store.nextGlobalID(),
		number: repo.nextNumber,
		title:  title,
		body:   body,
		state:  "open",
		labels: labels,
	}
	repo.nextNumber++
	repo.issues = append(repo.issues, is)
	repo.issuesByNo[is.number] = is
	repo.renderIssue(is)
	repo.bump()
	return is
}

func (s *Server) listIssues(req *Request, repo *Repo) Response {
	state := req.QueryParam("state")
	if state == "" {
		state = "open"
	}
	direction := req.QueryParam("direction")
	if direction == "" {
		direction = "desc"
	}
	var filtered []*Issue
	for _, is := range repo.issues {
		switch state {
		case "open":
			if is.state != "open" {
				continue
			}
		case "closed":
			if is.state != "closed" {
				continue
			}
		case "all":
		default:
			if is.state != "open" {
				continue
			}
		}
		filtered = append(filtered, is)
	}
	// sort=created: order by creation (number). direction asc/desc.
	if direction == "asc" {
		// filtered is already in creation order
	} else {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	var items [][]byte
	for _, is := range filtered {
		items = append(items, is.cached)
	}
	start, end, link := paginate(req, len(items))
	resp := jsonResp(200, joinJSON(items[start:end]))
	if link != "" {
		resp.Headers = map[string]string{"Link": link}
	}
	return resp
}

func (s *Server) getIssue(repo *Repo, num int) Response {
	is, ok := repo.issuesByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, is.cached)
}

func (s *Server) patchIssue(req *Request, repo *Repo, num int) Response {
	is, ok := repo.issuesByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	if st, present := body["state"]; present {
		switch asStr(st) {
		case "closed":
			is.state = "closed"
		case "open":
			is.state = "open"
		}
	}
	if t, present := body["title"]; present {
		is.title = asStr(t)
	}
	if b, present := body["body"]; present {
		is.body = asStr(b)
	}
	repo.renderIssue(is)
	repo.bump()
	return jsonResp(200, is.cached)
}

func (s *Server) createComment(req *Request, repo *Repo, num int) Response {
	is, ok := repo.issuesByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	c := &Comment{id: s.store.nextGlobalID(), body: asStr(body["body"]), issueNo: num}
	is.comments = append(is.comments, c)
	repo.renderIssue(is)
	repo.bump()
	return jsonResp(201, repo.renderComment(c, num))
}

func (s *Server) listComments(repo *Repo, num int) Response {
	is, ok := repo.issuesByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	var items [][]byte
	for _, c := range is.comments {
		items = append(items, repo.renderComment(c, num))
	}
	return jsonResp(200, joinJSON(items))
}

// --- pulls ------------------------------------------------------------------------------

func (s *Server) createPull(req *Request, repo *Repo) Response {
	body, ok := s.parseBody(req)
	if !ok {
		return errJSON(400, "Invalid JSON")
	}
	head := asStr(body["head"])
	base := asStr(body["base"])
	headSha, ok1 := repo.refTip("refs/heads/" + head)
	baseSha, ok2 := repo.refTip("refs/heads/" + base)
	if !ok1 || !ok2 {
		return errJSON(422, "Head or base branch does not exist")
	}
	pr := s.newPull(repo, asStr(body["title"]), asStr(body["body"]), head, base, headSha, baseSha)
	return jsonResp(201, repo.renderPull(pr))
}

func (s *Server) newPull(repo *Repo, title, body, head, base, headSha, baseSha string) *Pull {
	pr := &Pull{
		id:      s.store.nextGlobalID(),
		number:  repo.nextNumber,
		title:   title,
		body:    body,
		headRef: head,
		baseRef: base,
		headSha: headSha,
		baseSha: baseSha,
	}
	repo.nextNumber++
	repo.pulls = append(repo.pulls, pr)
	repo.pullsByNo[pr.number] = pr
	repo.bump()
	return pr
}

func (s *Server) listPulls(req *Request, repo *Repo) Response {
	state := req.QueryParam("state")
	if state == "" {
		state = "open"
	}
	var items [][]byte
	for _, pr := range repo.pulls {
		isOpen := !pr.merged && !pr.closed
		switch state {
		case "open":
			if !isOpen {
				continue
			}
		case "closed":
			if isOpen {
				continue
			}
		case "all":
		default:
			if !isOpen {
				continue
			}
		}
		items = append(items, repo.renderPull(pr))
	}
	start, end, link := paginate(req, len(items))
	resp := jsonResp(200, joinJSON(items[start:end]))
	if link != "" {
		resp.Headers = map[string]string{"Link": link}
	}
	return resp
}

func (s *Server) getPull(repo *Repo, num int) Response {
	pr, ok := repo.pullsByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, repo.renderPull(pr))
}

func (s *Server) listPullFiles(repo *Repo, num int) Response {
	pr, ok := repo.pullsByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	baseTree, headTree := "", ""
	if o := repo.objects[pr.baseSha]; o != nil {
		baseTree = parseCommit(o.data).tree
	}
	if o := repo.objects[pr.headSha]; o != nil {
		headTree = parseCommit(o.data).tree
	}
	changes := repo.diffTrees(baseTree, headTree)
	var items [][]byte
	for _, c := range changes {
		status := c.status
		items = append(items, []byte(`{"filename":`+jsonString(c.path)+`,"status":`+jsonString(status)+`,"additions":`+strconv.Itoa(c.additions)+`,"deletions":`+strconv.Itoa(c.deletions)+`,"changes":`+strconv.Itoa(c.additions+c.deletions)+`}`))
	}
	return jsonResp(200, joinJSON(items))
}

func (s *Server) mergePull(req *Request, repo *Repo, num int) Response {
	pr, ok := repo.pullsByNo[num]
	if !ok {
		return errJSON(404, "Not Found")
	}
	if pr.merged {
		return jsonResp(200, []byte(`{"sha":`+jsonString(pr.mergeCommitSha)+`,"merged":true,"message":"Pull request successfully merged"}`))
	}
	commitOID, conflict := repo.mergePR(pr)
	if conflict {
		return errJSON(405, "Merge conflict")
	}
	return jsonResp(200, []byte(`{"sha":`+jsonString(commitOID)+`,"merged":true,"message":"Pull request successfully merged"}`))
}
