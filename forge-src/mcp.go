package main

// MCP: JSON-RPC 2.0 over Streamable HTTP at POST /mcp.
// Modern era preferred (revision 2026-07-28 via server/discover), with the
// legacy initialize flow as fallback. Tools act on the same store as REST.

import (
	"strconv"
	"strings"
)

// Precomputed tools array JSON: byte-identical on every tools/list call.
var mcpToolsJSON = []byte(`[{"name":"create_repo","description":"Create a new repository for the viewer.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"},"owner":{"type":"string","description":"Repository owner login"}},"required":["name"]}},{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state: open, closed, or all"}},"required":["owner","repo"]}},{"name":"create_pull_request","description":"Create a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},{"name":"merge_pull_request","description":"Merge a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}]`)

var mcpRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func (s *Server) handleMCP(req *Request) Response {
	if req.Method == "DELETE" {
		return Response{Status: 200, ContentType: "application/json", Body: []byte(`{}`)}
	}
	if req.Method != "POST" {
		return errJSON(405, "Method Not Allowed")
	}
	if !s.requireAuth(req) {
		return errJSON(401, "Requires authentication")
	}
	v, err := parseJSON(req.Body)
	if err != nil {
		// Parse error: HTTP 400 with -32700, echoing the id if recoverable.
		id := extractRPCID(req.Body)
		var out []byte
		out = append(out, `{"jsonrpc":"2.0","id":`...)
		writeJSONValue(&out, id)
		out = append(out, `,"error":{"code":-32700,"message":"Parse error"}}`...)
		return jsonResp(400, out)
	}
	m := asObj(v)
	method := asStr(m["method"])
	id := m["id"]
	// Notifications (no id) get no JSON-RPC response body.
	if id == nil {
		if method == "notifications/initialized" {
			return Response{Status: 202, ContentType: "application/json", Body: nil}
		}
		return Response{Status: 202, ContentType: "application/json", Body: nil}
	}
	switch method {
	case "server/discover":
		var res []byte
		res = append(res, `{"supportedVersions":[`...)
		for i, r := range mcpRevisions {
			if i > 0 {
				res = append(res, ',')
			}
			escapeJSONString(&res, r)
		}
		res = append(res, `],"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"},"instructions":"GitHub-compatible git forge over MCP.","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"forge","version":"1.0.0"}}}`...)
		return rpcResult(id, jsonRaw(res))
	case "initialize":
		params := asObj(m["params"])
		ver := asStr(params["protocolVersion"])
		ok := false
		for _, r := range mcpRevisions {
			if r == ver {
				ok = true
			}
		}
		if !ok {
			ver = "2025-11-25"
		}
		res := `{"protocolVersion":` + jsonString(ver) + `,"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"forge","version":"1.0.0"}}}`
		return rpcResult(id, jsonRaw(res))
	case "ping":
		return rpcResult(id, jsonRaw(`{}`))
	case "tools/list":
		res := `{"tools":` + string(mcpToolsJSON) + `}`
		return rpcResult(id, jsonRaw(res))
	case "tools/call":
		return s.mcpCall(id, m)
	default:
		return rpcError(id, -32601, "Method not found")
	}
}

func rpcResult(id any, result jsonRaw) Response {
	var out []byte
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	writeJSONValue(&out, id)
	out = append(out, `,"result":`...)
	out = append(out, result...)
	out = append(out, '}')
	return jsonResp(200, out)
}

func rpcError(id any, code int, msg string) Response {
	var out []byte
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	writeJSONValue(&out, id)
	out = append(out, `,"error":{"code":`...)
	out = strconv.AppendInt(out, int64(code), 10)
	out = append(out, `,"message":`...)
	escapeJSONString(&out, msg)
	out = append(out, `}}`...)
	return jsonResp(200, out)
}

// extractRPCID recovers the "id" from a malformed JSON-RPC body (best effort).
func extractRPCID(body []byte) any {
	s := string(body)
	i := strings.Index(s, `"id"`)
	if i < 0 {
		return nil
	}
	rest := s[i+4:]
	j := strings.IndexByte(rest, ':')
	if j < 0 {
		return nil
	}
	rest = rest[j+1:]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return nil
		}
		return rest[1 : 1+end]
	}
	k := 0
	for k < len(rest) && (rest[k] >= '0' && rest[k] <= '9' || rest[k] == '-') {
		k++
	}
	if k == 0 {
		return nil
	}
	n, err := strconv.Atoi(rest[:k])
	if err != nil {
		return nil
	}
	return n
}

func (s *Server) mcpCall(id any, m map[string]any) Response {
	params := asObj(m["params"])
	name := asStr(params["name"])
	args := asObj(params["arguments"])
	// Tool implementations. Each returns (structuredContent, isError, errMsg).
	var content jsonRaw
	var isErr bool
	var errMsg string
	var errText string
	need := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := args[k]; !ok {
				errMsg = "Missing required argument: " + k
				return false
			}
		}
		return true
	}
	lookup := func() (*Repo, bool) {
		owner := asStr(args["owner"])
		if owner == "" {
			owner = viewerLogin
		}
		repo := s.lookupRepo(owner, asStr(args["repo"]))
		return repo, repo != nil
	}
	switch name {
	case "create_repo":
		if !need("name") {
			return rpcError(id, -32602, errMsg)
		}
		rname := asStr(args["name"])
		owner := asStr(args["owner"])
		if owner == "" {
			owner = viewerLogin
		}
		if owner != viewerLogin {
			return rpcError(id, -32602, "Cannot create a repository for another owner")
		}
		if existing := s.store.getRepo(viewerLogin, rname); existing != nil {
			content = existing.repoJSON()
		} else {
			r := s.store.createRepo(rname, asBool(args["private"], false), asStr(args["description"]))
			content = r.repoJSON()
		}
	case "get_repo":
		if !need("owner", "repo") {
			return rpcError(id, -32602, errMsg)
		}
		repo, ok := lookup()
		if !ok {
			isErr = true
			errText = "Repository not found"
		} else {
			content = repo.repoJSON()
		}
	case "create_issue":
		if !need("owner", "repo", "title") {
			return rpcError(id, -32602, errMsg)
		}
		repo, ok := lookup()
		if !ok {
			isErr = true
			errText = "Repository not found"
		} else {
			is := s.newIssue(repo, asStr(args["title"]), asStr(args["body"]), nil)
			content = is.cached
		}
	case "list_issues":
		if !need("owner", "repo") {
			return rpcError(id, -32602, errMsg)
		}
		repo, ok := lookup()
		if !ok {
			isErr = true
			errText = "Repository not found"
		} else {
			state := asStr(args["state"])
			if state == "" {
				state = "open"
			}
			var items [][]byte
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
				}
				items = append(items, is.cached)
			}
			content = jsonRaw(`{"issues":` + string(joinJSON(items)) + `}`)
		}
	case "create_pull_request":
		if !need("owner", "repo", "title", "head", "base") {
			return rpcError(id, -32602, errMsg)
		}
		repo, ok := lookup()
		if !ok {
			isErr = true
			errText = "Repository not found"
		} else {
			head := asStr(args["head"])
			base := asStr(args["base"])
			headSha, ok1 := repo.refTip("refs/heads/" + head)
			baseSha, ok2 := repo.refTip("refs/heads/" + base)
			if !ok1 || !ok2 {
				isErr = true
				errText = "Head or base branch does not exist"
			} else {
				pr := s.newPull(repo, asStr(args["title"]), asStr(args["body"]), head, base, headSha, baseSha)
				content = repo.renderPull(pr)
			}
		}
	case "merge_pull_request":
		if !need("owner", "repo", "pull_number") {
			return rpcError(id, -32602, errMsg)
		}
		repo, ok := lookup()
		if !ok {
			isErr = true
			errText = "Repository not found"
		} else {
			num := asInt(args["pull_number"], 0)
			pr, ok := repo.pullsByNo[num]
			if !ok {
				isErr = true
				errText = "Pull request not found"
			} else if !pr.merged {
				if _, conflict := repo.mergePR(pr); conflict {
					isErr = true
					errText = "Merge conflict"
				} else {
					content = repo.renderPull(pr)
				}
			} else {
				content = repo.renderPull(pr)
			}
		}
	default:
		return rpcError(id, -32602, "Unknown tool: "+name)
	}
	if isErr {
		// isError result: content text carries the message.
		var out []byte
		out = append(out, `{"content":[{"type":"text","text":`...)
		escapeJSONString(&out, errText)
		out = append(out, `}],"isError":true}`...)
		return rpcResult(id, jsonRaw(out))
	}
	// Success: content text is the JSON, structuredContent is the object.
	var out []byte
	out = append(out, `{"content":[{"type":"text","text":`...)
	escapeJSONString(&out, string(content))
	out = append(out, `}],"structuredContent":`...)
	out = append(out, content...)
	out = append(out, `,"isError":false}`...)
	return rpcResult(id, jsonRaw(out))
}
