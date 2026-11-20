package main

// REST v3 under /api/v3: the router, authentication, pagination, and the repository, git-data and
// contents resources. Issues, labels and pull requests are in rest_issues.go.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const jsonType = "application/json"

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func unixTime(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func pathEscape(s string) string  { return url.PathEscape(s) }
func queryEscape(s string) string { return url.QueryEscape(s) }

// pathEscapeSlashes escapes each segment of a path and keeps its slashes.
func pathEscapeSlashes(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func jsonResponse(status int, value any) *Response {
	return &Response{Status: status, ContentType: jsonType, Body: appendJSON(make([]byte, 0, 256), value)}
}

func jsonMessage(status int, message string) *Response {
	return jsonResponse(status, om("message", message, "documentation_url", "https://docs.github.com/rest", "status", strconv.Itoa(status)))
}

func notFound() *Response { return jsonMessage(404, "Not Found") }

// validationFailed is GitHub's 422 body: a message and the fields at fault.
func validationFailed(resource, field, code string, detail string) *Response {
	e := om("resource", resource, "field", field, "code", code)
	if detail != "" {
		e.set("message", detail)
	}
	message := "Validation Failed"
	return jsonResponse(422, om("message", message, "errors", []any{e}, "documentation_url", "https://docs.github.com/rest", "status", "422"))
}

func unprocessable(message string) *Response { return jsonMessage(422, message) }

// credential pulls the token out of an Authorization header: Bearer, token, or Basic with the token
// as either half.
func credential(req *Request) (tokens []string, present bool) {
	header := strings.TrimSpace(req.Header.Get("Authorization"))
	if header == "" {
		return nil, false
	}
	sp := strings.IndexByte(header, ' ')
	if sp < 0 {
		return []string{header}, true
	}
	scheme, rest := strings.ToLower(header[:sp]), strings.TrimSpace(header[sp+1:])
	switch scheme {
	case "bearer", "token":
		return []string{rest}, true
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, true
		}
		user, pass, _ := strings.Cut(string(raw), ":")
		return []string{pass, user}, true
	}
	return []string{rest}, true
}

// authorized reports whether the request carries the trial's token, and whether it carried any.
func (a *App) authorized(req *Request) (ok bool, present bool) {
	tokens, present := credential(req)
	if !present {
		return false, false
	}
	for _, t := range tokens {
		if t == "" {
			continue
		}
		if a.token == "" || t == a.token {
			return true, true
		}
	}
	return false, true
}

func unauthorized(present bool) *Response {
	message := "Requires authentication"
	if present {
		message = "Bad credentials"
	}
	res := jsonMessage(401, message)
	return res
}

// handle answers one request on any surface.
func (a *App) handle(req *Request) (res *Response) {
	n := a.requests.Add(1)
	req.ordinal = n
	defer func() {
		// A handler that panics must cost one reply, never the server: the suite keeps going.
		if failure := recover(); failure != nil {
			fmt.Fprintf(os.Stderr, "[forge] %s %s panicked: %v\n", req.Method, req.Path, failure)
			res = jsonMessage(500, "Internal Server Error")
		}
		res.Headers = append(res.Headers, "X-GitHub-Request-Id", requestID(n))
	}()
	return a.route(req)
}

func requestID(n uint64) string {
	// Shaped like GitHub's: four groups, derived from the request's ordinal. Every reply carries
	// one, so it is put together in place: F0E1, the ordinal offset and in upper-case hex, split
	// after five digits, then the ordinal in decimal.
	var digits [20]byte
	hex := strconv.AppendUint(digits[:0], 0x1000000000+n, 16)
	for i, c := range hex {
		if c >= 'a' {
			hex[i] = c - ('a' - 'A')
		}
	}
	var buf [48]byte
	id := append(buf[:0], "F0E1:"...)
	id = append(id, hex[:5]...)
	id = append(id, ':')
	id = append(id, hex[5:]...)
	id = append(id, ':')
	id = strconv.AppendUint(id, n, 10)
	return string(id)
}

var healthBody = []byte(`{"ok":true}`)

func (a *App) route(req *Request) *Response {
	path := req.Path
	switch {
	case path == "/healthz":
		return &Response{Status: 200, ContentType: jsonType, Body: healthBody}
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		return a.handleREST(req, "/api/v3")
	case path == "/api/graphql" || path == "/graphql":
		return a.handleGraphQL(req)
	case path == "/mcp" || path == "/mcp/":
		return a.handleMCP(req)
	}
	if i := strings.Index(path, ".git/"); i > 0 {
		ownerRepo := strings.Trim(path[:i], "/")
		if owner, name, ok := strings.Cut(ownerRepo, "/"); ok && !strings.Contains(name, "/") {
			if authed, present := a.authorized(req); !authed {
				res := gitText(401, "Authentication required\n")
				if present {
					res = gitText(401, "Bad credentials\n")
				}
				return res.setHeader("WWW-Authenticate", `Basic realm="forge"`)
			}
			return a.handleGit(req, owner, name, path[i+len(".git/"):])
		}
	}
	return notFound()
}

// rc is one REST request being answered.
type rc struct {
	a    *App
	req  *Request
	v    *view
	body map[string]any
	// issuesOnly keeps pull requests out of GET /issues (the MCP list_issues tool asks for that).
	issuesOnly bool
}

func (c *rc) str(key string) (string, bool) {
	s, ok := c.body[key].(string)
	return s, ok
}

func (c *rc) boolean(key string) (bool, bool) {
	b, ok := c.body[key].(bool)
	return b, ok
}

// page cuts a list to the requested page and writes the Link header for its neighbours.
func (c *rc) page(items []any) ([]any, []string) {
	q := c.req.Query()
	perPage := 30
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		perPage = n
		if perPage > 100 {
			perPage = 100
		}
	}
	page := 1
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		page = n
	}
	total := len(items)
	last := (total + perPage - 1) / perPage
	if last < 1 {
		last = 1
	}
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	link := func(n int, rel string) string {
		values := url.Values{}
		for key, vals := range q {
			if key != "page" {
				values[key] = vals
			}
		}
		values.Set("page", strconv.Itoa(n))
		return "<" + c.v.base + c.req.RawPath + "?" + values.Encode() + `>; rel="` + rel + `"`
	}
	var links []string
	if page > 1 {
		links = append(links, link(page-1, "prev"))
	}
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"))
	}
	var headers []string
	if len(links) > 0 {
		headers = []string{"Link", strings.Join(links, ", ")}
	}
	return items[start:end], headers
}

func (c *rc) list(items []any) *Response {
	paged, headers := c.page(items)
	if paged == nil {
		paged = []any{}
	}
	res := jsonResponse(200, paged)
	res.Headers = append(res.Headers, headers...)
	return res
}

// baseURL is the origin the client dialed, which every URL in a reply must stay on.
func baseURL(req *Request) string {
	host := req.Host
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

func (a *App) handleREST(req *Request, prefix string) *Response {
	if ok, present := a.authorized(req); !ok {
		return unauthorized(present)
	}
	raw := strings.Trim(strings.TrimPrefix(req.RawPath, prefix), "/")
	var segs []string
	if raw != "" {
		for _, part := range strings.Split(raw, "/") {
			decoded, err := url.PathUnescape(part)
			if err != nil {
				decoded = part
			}
			segs = append(segs, decoded)
		}
	}
	c := &rc{a: a, req: req, v: &view{a: a, base: baseURL(req)}}
	mutating := req.Method != "GET" && req.Method != "HEAD"
	if mutating {
		trimmed := strings.TrimSpace(string(req.Body))
		if trimmed != "" {
			var parsed any
			if err := json.Unmarshal(req.Body, &parsed); err != nil {
				return jsonMessage(400, "Problems parsing JSON")
			}
			switch body := parsed.(type) {
			case map[string]any:
				c.body = body
			case []any:
				// A bare array is a body some label routes take.
				c.body = map[string]any{"__array": body}
			default:
				return jsonMessage(400, "Body should be a JSON object")
			}
		}
		if c.body == nil {
			c.body = map[string]any{}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
	} else {
		a.mu.RLock()
		defer a.mu.RUnlock()
	}
	res := c.dispatch(segs)
	if res == nil {
		return notFound()
	}
	// X-OAuth-Scopes is asked of the API root alone; carrying it on the reply every request makes
	// the client parse one header more for nothing, so only the root states it.
	if len(segs) == 0 {
		res.Headers = append(res.Headers, "X-OAuth-Scopes", oauthScopes)
	}
	return res
}

// oauthScopes is the X-OAuth-Scopes value every REST reply carries.
const oauthScopes = "repo, read:org"

func (c *rc) dispatch(segs []string) *Response {
	method := c.req.Method
	if method == "HEAD" {
		method = "GET"
	}
	a, v := c.a, c.v
	if len(segs) == 0 {
		if method != "GET" {
			return nil
		}
		return jsonResponse(200, om(
			"current_user_url", v.api("/user"),
			"current_user_repositories_url", v.api("/user/repos{?type,page,per_page,sort}"),
			"repository_url", v.api("/repos/{owner}/{repo}"),
			"user_url", v.api("/users/{user}"),
			"user_repositories_url", v.api("/users/{user}/repos{?type,page,per_page,sort}"),
			"issues_url", v.api("/issues"),
			"rate_limit_url", v.api("/rate_limit"),
			"emojis_url", v.api("/emojis"),
			"meta_url", v.api("/meta"),
		))
	}
	switch segs[0] {
	case "meta":
		if len(segs) == 1 && method == "GET" {
			return jsonResponse(200, om(
				"verifiable_password_authentication", false,
				"installed_version", "3.14.0",
			))
		}
	case "rate_limit":
		if len(segs) == 1 && method == "GET" {
			reset := a.base.Add(time.Hour).Unix()
			core := om("limit", 5000, "used", 0, "remaining", 5000, "reset", reset)
			return jsonResponse(200, om("resources", om("core", core, "graphql", core, "search", om("limit", 30, "used", 0, "remaining", 30, "reset", reset)), "rate", core))
		}
	case "zen":
		if len(segs) == 1 && method == "GET" {
			return &Response{Status: 200, ContentType: jsonType, Body: []byte(`"Keep it logically awesome."`)}
		}
	case "user":
		switch {
		case len(segs) == 1 && method == "GET":
			return jsonResponse(200, v.fullUser(a.viewer))
		case len(segs) == 2 && segs[1] == "repos" && method == "GET":
			return c.listRepos(a.viewer)
		case len(segs) == 2 && segs[1] == "repos" && method == "POST":
			return c.createRepo()
		case len(segs) == 2 && (segs[1] == "orgs" || segs[1] == "teams" || segs[1] == "issues") && method == "GET":
			return jsonResponse(200, []any{})
		}
	case "users":
		if len(segs) >= 2 && method == "GET" {
			u := a.users[strings.ToLower(segs[1])]
			if u == nil {
				return notFound()
			}
			if len(segs) == 2 {
				return jsonResponse(200, v.fullUser(u))
			}
			if len(segs) == 3 && segs[2] == "repos" {
				return c.listRepos(u)
			}
		}
	case "repos":
		if len(segs) >= 3 {
			repo := a.repos[repoKey(segs[1], segs[2])]
			if repo == nil {
				return notFound()
			}
			return c.repoRoute(repo, method, segs[3:])
		}
	case "repositories":
		if len(segs) == 2 {
			if id, err := strconv.ParseInt(segs[1], 10, 64); err == nil {
				if repo := a.repoByID[id]; repo != nil && method == "GET" {
					return jsonResponse(200, v.fullRepo(repo))
				}
			}
		}
	case "search":
		if len(segs) == 2 && method == "GET" {
			return c.search(segs[1])
		}
	}
	return nil
}

// ---------------------------------------------------------------- repositories

func (c *rc) listRepos(owner *User) *Response {
	var items []any
	for _, r := range c.a.repoOrder {
		if r.Owner == owner {
			items = append(items, c.v.repo(r))
		}
	}
	return c.list(items)
}

func validRepoName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

// newRepo creates a repository for the viewer from API-style fields; it is shared by REST, GraphQL
// and MCP. The caller holds a.mu for writing.
func (a *App) newRepo(name, description string, hasDesc, private, autoInit bool) (*Repo, *Response) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, validationFailed("Repository", "name", "missing_field", "")
	}
	if !validRepoName(name) {
		return nil, validationFailed("Repository", "name", "custom", "name can only contain ASCII letters, digits, and the characters ., -, and _")
	}
	if a.repos[repoKey(a.viewer.Login, name)] != nil {
		return nil, jsonResponse(422, om(
			"message", "Repository creation failed.",
			"errors", []any{om("resource", "Repository", "code", "custom", "field", "name", "message", "name already exists on this account")},
			"documentation_url", "https://docs.github.com/rest/repos/repos#create-a-repository-for-the-authenticated-user",
			"status", "422",
		))
	}
	repo := a.createRepoLocked(a.viewer, name)
	repo.Description, repo.HasDesc = description, hasDesc
	repo.Private = private
	if autoInit {
		readme := "# " + name + "\n"
		if description != "" {
			readme += "\n" + description + "\n"
		}
		blob := repo.Objects.Put(ObjBlob, []byte(readme))
		tree := repo.treeWith(map[string]flatFile{}, []fileChange{{Path: "README.md", Blob: blob.ID}})
		sig := a.signature()
		repo.setRef(repo.headRef(), repo.commit(tree, nil, sig, sig, "Initial commit"))
	}
	return repo, nil
}

// signature is the viewer as a commit author, stamped with the forge's clock.
func (a *App) signature() Signature {
	name := a.viewer.Name
	if name == "" {
		name = a.viewer.Login
	}
	email := a.viewer.Email
	if email == "" {
		email = strconv.FormatInt(a.viewer.ID, 10) + "+" + a.viewer.Login + "@users.noreply.github.com"
	}
	return Signature{Name: name, Email: email, When: a.now().Unix(), Zone: "+0000"}
}

// userByEmail maps a commit signature back to an account, when one matches.
func (a *App) userByEmail(email, name string) *User {
	for _, u := range a.users {
		if (u.Email != "" && u.Email == email) || strings.HasPrefix(email, strconv.FormatInt(u.ID, 10)+"+"+u.Login+"@") {
			return u
		}
	}
	return nil
}

func (c *rc) createRepo() *Response {
	name, _ := c.str("name")
	description, hasDesc := c.str("description")
	private, _ := c.boolean("private")
	if visibility, ok := c.str("visibility"); ok {
		private = visibility == "private"
	}
	autoInit, _ := c.boolean("auto_init")
	repo, failure := c.a.newRepo(name, description, hasDesc, private, autoInit)
	if failure != nil {
		return failure
	}
	if homepage, ok := c.str("homepage"); ok {
		repo.Homepage = homepage
	}
	for _, flag := range []struct {
		key    string
		target *bool
	}{{"has_issues", &repo.HasIssues}, {"has_wiki", &repo.HasWiki}, {"has_projects", &repo.HasProjects}} {
		if value, ok := c.boolean(flag.key); ok {
			*flag.target = value
		}
	}
	res := jsonResponse(201, c.v.fullRepo(repo))
	return res.setHeader("Location", c.v.api("/repos/"+repo.FullName()))
}

func (c *rc) repoRoute(repo *Repo, method string, rest []string) *Response {
	a, v := c.a, c.v
	if len(rest) == 0 {
		switch method {
		case "GET":
			return jsonResponse(200, v.fullRepo(repo))
		case "PATCH":
			if name, ok := c.str("name"); ok && name != repo.Name {
				if !validRepoName(name) {
					return validationFailed("Repository", "name", "custom", "name is invalid")
				}
				if a.repos[repoKey(repo.Owner.Login, name)] != nil {
					return validationFailed("Repository", "name", "custom", "name already exists on this account")
				}
				delete(a.repos, repoKey(repo.Owner.Login, repo.Name))
				repo.Name = name
				a.repos[repoKey(repo.Owner.Login, name)] = repo
				repo.dropIssueCaches()
			}
			if raw, present := c.body["description"]; present {
				description, isString := raw.(string)
				repo.Description, repo.HasDesc = description, isString
			}
			if homepage, ok := c.str("homepage"); ok {
				repo.Homepage = homepage
			}
			if private, ok := c.boolean("private"); ok {
				repo.Private = private
			}
			if visibility, ok := c.str("visibility"); ok {
				repo.Private = visibility == "private"
			}
			if archived, ok := c.boolean("archived"); ok {
				repo.Archived = archived
			}
			if branch, ok := c.str("default_branch"); ok && branch != "" {
				if repo.Branch(branch).IsZero() && len(repo.sortedRefs()) > 0 {
					return validationFailed("Repository", "default_branch", "invalid", "The branch "+branch+" was not found.")
				}
				repo.DefaultBranch = branch
				repo.gen.Add(1)
			}
			for _, flag := range []struct {
				key    string
				target *bool
			}{{"has_issues", &repo.HasIssues}, {"has_wiki", &repo.HasWiki}, {"has_projects", &repo.HasProjects}} {
				if value, ok := c.boolean(flag.key); ok {
					*flag.target = value
				}
			}
			repo.UpdatedAt = a.now()
			return jsonResponse(200, v.fullRepo(repo))
		case "DELETE":
			a.deleteRepoLocked(repo)
			return &Response{Status: 204}
		}
		return nil
	}
	switch rest[0] {
	case "branches":
		if method != "GET" {
			return nil
		}
		if len(rest) == 1 {
			var items []any
			for _, ref := range repo.sortedRefs() {
				if name, ok := strings.CutPrefix(ref.Name, "refs/heads/"); ok {
					items = append(items, v.branch(repo, name, ref.SHA, false))
				}
			}
			return c.list(items)
		}
		name := strings.Join(rest[1:], "/")
		tip := repo.Branch(name)
		if tip.IsZero() {
			return jsonMessage(404, "Branch not found")
		}
		return jsonResponse(200, v.branch(repo, name, tip, true))
	case "commits":
		if method != "GET" {
			return nil
		}
		if len(rest) == 1 {
			return c.listCommits(repo)
		}
		return c.getCommit(repo, strings.Join(rest[1:], "/"))
	case "git":
		if len(rest) >= 2 {
			return c.gitData(repo, method, rest[1], rest[2:])
		}
	case "contents":
		return c.contents(repo, method, strings.Join(rest[1:], "/"))
	case "readme":
		if method == "GET" && len(rest) == 1 {
			tip, ok := repo.resolveRev(c.req.Query().Get("ref"))
			if ok {
				if commit := repo.Objects.Get(tip).AsCommit(); commit != nil {
					for _, e := range repo.Objects.Get(commit.Tree).AsTree() {
						if strings.HasPrefix(strings.ToLower(e.Name), "readme") && !e.IsTree() {
							return jsonResponse(200, v.contentEntry(repo, repo.DefaultBranch, e.Name, e, true))
						}
					}
				}
			}
			return notFound()
		}
	case "labels":
		return c.labels(repo, method, rest[1:])
	case "issues":
		return c.issues(repo, method, rest[1:])
	case "pulls":
		return c.pulls(repo, method, rest[1:])
	case "compare":
		if method == "GET" && len(rest) >= 2 {
			return c.compare(repo, strings.Join(rest[1:], "/"))
		}
	case "tags":
		if method == "GET" && len(rest) == 1 {
			var items []any
			for _, ref := range repo.sortedRefs() {
				if name, ok := strings.CutPrefix(ref.Name, "refs/tags/"); ok {
					target := repo.peel(ref.SHA)
					items = append(items, om("name", name, "commit", om("sha", target.String(), "url", v.api("/repos/"+repo.FullName()+"/commits/"+target.String())), "node_id", nodeID("Ref", repo.ID)))
				}
			}
			return c.list(items)
		}
	case "topics":
		if len(rest) == 1 {
			if method == "PUT" {
				if names, ok := c.body["names"].([]any); ok {
					repo.Topics = repo.Topics[:0]
					for _, n := range names {
						if s, ok := n.(string); ok {
							repo.Topics = append(repo.Topics, s)
						}
					}
				}
			}
			if method == "GET" || method == "PUT" {
				names := make([]any, 0, len(repo.Topics))
				for _, t := range repo.Topics {
					names = append(names, t)
				}
				return jsonResponse(200, om("names", names))
			}
		}
	case "languages":
		if method == "GET" && len(rest) == 1 {
			return jsonResponse(200, om())
		}
	case "collaborators", "assignees", "contributors":
		if method == "GET" && len(rest) == 1 {
			return c.list([]any{v.user(repo.Owner)})
		}
	case "milestones", "releases", "forks", "hooks", "deployments", "stargazers", "subscribers", "events", "teams":
		if method == "GET" && len(rest) == 1 {
			return jsonResponse(200, []any{})
		}
	}
	return nil
}

// ---------------------------------------------------------------- commits

func (c *rc) listCommits(repo *Repo) *Response {
	q := c.req.Query()
	tip, ok := repo.resolveRev(q.Get("sha"))
	if !ok {
		if q.Get("sha") == "" {
			return jsonMessage(409, "Git Repository is empty.")
		}
		return notFound()
	}
	path := strings.Trim(q.Get("path"), "/")
	var items []any
	for _, commit := range repo.Objects.commitLog(tip, 0) {
		if path != "" && !repo.touches(commit, path) {
			continue
		}
		items = append(items, c.v.commit(repo, commit))
	}
	return c.list(items)
}

// touches reports whether a commit changed a path relative to its first parent.
func (repo *Repo) touches(commit *Commit, path string) bool {
	mine, mineOK := repo.Objects.lookupPath(commit.Tree, path)
	if len(commit.Parents) == 0 {
		return mineOK
	}
	parent := repo.Objects.Get(commit.Parents[0]).AsCommit()
	if parent == nil {
		return mineOK
	}
	theirs, theirsOK := repo.Objects.lookupPath(parent.Tree, path)
	return mineOK != theirsOK || mine.ID != theirs.ID
}

func (c *rc) getCommit(repo *Repo, rev string) *Response {
	tip, ok := repo.resolveRev(rev)
	if !ok {
		return jsonMessage(422, "No commit found for SHA: "+rev)
	}
	commit := repo.Objects.Get(tip).AsCommit()
	if commit == nil {
		return jsonMessage(422, "No commit found for SHA: "+rev)
	}
	m := c.v.commit(repo, commit)
	var parent SHA
	if len(commit.Parents) > 0 {
		parent = commit.Parents[0]
	}
	files := repo.diffTrees(parent, commit.ID, true)
	additions, deletions := 0, 0
	list := make([]any, 0, len(files))
	for _, f := range files {
		additions += f.Additions
		deletions += f.Deletions
		list = append(list, c.v.pullFile(repo, commit.ID, f))
	}
	m.set("stats", om("total", additions+deletions, "additions", additions, "deletions", deletions))
	m.set("files", list)
	return jsonResponse(200, m)
}

func (c *rc) compare(repo *Repo, spec string) *Response {
	baseRev, headRev, ok := strings.Cut(spec, "...")
	if !ok {
		baseRev, headRev, ok = strings.Cut(spec, "..")
	}
	if !ok {
		return notFound()
	}
	strip := func(rev string) string {
		if _, branch, found := strings.Cut(rev, ":"); found {
			return branch
		}
		return rev
	}
	base, okBase := repo.resolveRev(strip(baseRev))
	head, okHead := repo.resolveRev(strip(headRev))
	if !okBase || !okHead {
		return notFound()
	}
	ancestor := repo.Objects.mergeBase(base, head)
	ahead := repo.commitsBetween(base, head)
	behind := repo.commitsBetween(head, base)
	status := "diverged"
	switch {
	case len(ahead) == 0 && len(behind) == 0:
		status = "identical"
	case len(behind) == 0:
		status = "ahead"
	case len(ahead) == 0:
		status = "behind"
	}
	commits := make([]any, 0, len(ahead))
	for _, commit := range ahead {
		commits = append(commits, c.v.commit(repo, commit))
	}
	files := repo.diffTrees(ancestor, head, true)
	list := make([]any, 0, len(files))
	for _, f := range files {
		list = append(list, c.v.pullFile(repo, head, f))
	}
	m := om(
		"url", c.v.api("/repos/"+repo.FullName()+"/compare/"+spec),
		"html_url", c.v.base+"/"+repo.FullName()+"/compare/"+spec,
		"status", status,
		"ahead_by", len(ahead),
		"behind_by", len(behind),
		"total_commits", len(ahead),
		"commits", commits,
		"files", list,
	)
	if bc := repo.Objects.Get(base).AsCommit(); bc != nil {
		m.set("base_commit", c.v.commit(repo, bc))
	}
	if mc := repo.Objects.Get(ancestor).AsCommit(); mc != nil {
		m.set("merge_base_commit", c.v.commit(repo, mc))
	}
	return jsonResponse(200, m)
}

// ---------------------------------------------------------------- git data

func validRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || strings.Count(name, "/") < 2 || strings.HasSuffix(name, "/") {
		return false
	}
	return !strings.Contains(name, "..") && !strings.ContainsAny(name, " ~^:?*[\\") && !strings.HasSuffix(name, ".lock")
}

func (c *rc) gitData(repo *Repo, method, kind string, rest []string) *Response {
	a, v := c.a, c.v
	switch kind {
	case "refs", "ref", "matching-refs":
		name := "refs"
		if len(rest) > 0 {
			name = "refs/" + strings.Join(rest, "/")
		}
		switch method {
		case "GET":
			if sha := repo.Ref(name); !sha.IsZero() && kind != "matching-refs" {
				return jsonResponse(200, v.gitRef(repo, name, sha))
			}
			if kind == "ref" {
				return notFound()
			}
			var items []any
			for _, ref := range repo.sortedRefs() {
				if name == "refs" || strings.HasPrefix(ref.Name, name) {
					items = append(items, v.gitRef(repo, ref.Name, ref.SHA))
				}
			}
			if len(items) == 0 && kind == "refs" && len(rest) > 0 {
				return notFound()
			}
			return c.list(items)
		case "POST":
			if kind != "refs" || len(rest) != 0 {
				return nil
			}
			ref, _ := c.str("ref")
			shaText, _ := c.str("sha")
			if !validRefName(ref) {
				return unprocessable("Reference name must start with 'refs/' and contain at least two slashes")
			}
			sha, ok := parseSHA(shaText)
			if !ok || !repo.Objects.Has(sha) {
				return unprocessable("Object does not exist")
			}
			if !repo.Ref(ref).IsZero() {
				return unprocessable("Reference already exists")
			}
			repo.setRef(ref, sha)
			repo.PushedAt = a.now()
			res := jsonResponse(201, v.gitRef(repo, ref, sha))
			return res.setHeader("Location", v.api("/repos/"+repo.FullName()+"/git/"+ref))
		case "PATCH":
			if kind != "refs" || len(rest) == 0 {
				return nil
			}
			current := repo.Ref(name)
			if current.IsZero() {
				return unprocessable("Reference does not exist")
			}
			shaText, _ := c.str("sha")
			sha, ok := parseSHA(shaText)
			if !ok || !repo.Objects.Has(sha) {
				return unprocessable("Object does not exist")
			}
			force, _ := c.boolean("force")
			if !force && !repo.Objects.isAncestor(current, sha) {
				return unprocessable("Update is not a fast forward")
			}
			repo.setRef(name, sha)
			repo.PushedAt = a.now()
			return jsonResponse(200, v.gitRef(repo, name, sha))
		case "DELETE":
			if kind != "refs" || len(rest) == 0 {
				return nil
			}
			if repo.Ref(name).IsZero() {
				return unprocessable("Reference does not exist")
			}
			repo.setRef(name, zeroSHA)
			return &Response{Status: 204}
		}
	case "commits":
		if method == "GET" && len(rest) == 1 {
			sha, ok := parseSHA(rest[0])
			commit := repo.Objects.Get(sha).AsCommit()
			if !ok || commit == nil {
				return notFound()
			}
			return jsonResponse(200, v.gitCommit(repo, commit))
		}
		if method == "POST" && len(rest) == 0 {
			message, _ := c.str("message")
			treeText, _ := c.str("tree")
			tree, ok := parseSHA(treeText)
			if !ok || !repo.Objects.Has(tree) {
				return unprocessable("Tree SHA does not exist")
			}
			var parents []SHA
			if list, ok := c.body["parents"].([]any); ok {
				for _, item := range list {
					text, _ := item.(string)
					parent, ok := parseSHA(text)
					if !ok || !repo.Objects.Has(parent) {
						return unprocessable("Parent SHA does not exist or is not a commit object")
					}
					parents = append(parents, parent)
				}
			}
			sig := a.signature()
			author := c.signatureFrom("author", sig)
			committer := c.signatureFrom("committer", author)
			id := repo.commit(tree, parents, author, committer, message)
			return jsonResponse(201, v.gitCommit(repo, repo.Objects.Get(id).AsCommit()))
		}
	case "trees":
		if method == "GET" && len(rest) >= 1 {
			rev := strings.Join(rest, "/")
			var treeID SHA
			if sha, ok := parseSHA(rev); ok && repo.Objects.Has(sha) {
				treeID = sha
			} else if tip, ok := repo.resolveRev(rev); ok {
				treeID = tip
			} else {
				return notFound()
			}
			o := repo.Objects.Get(treeID)
			if commit := o.AsCommit(); commit != nil {
				treeID = commit.Tree
				o = repo.Objects.Get(treeID)
			}
			if o == nil || o.Type != ObjTree {
				return notFound()
			}
			recursive := c.req.Query().Get("recursive") != ""
			var entries []any
			var walk func(id SHA, prefix string)
			walk = func(id SHA, prefix string) {
				for _, e := range repo.Objects.Get(id).AsTree() {
					mode := e.Mode
					if mode == "40000" {
						mode = "040000"
					}
					item := om("path", prefix+e.Name, "mode", mode, "type", e.TypeName(), "sha", e.ID.String())
					if !e.IsTree() {
						if blob := repo.Objects.Get(e.ID); blob != nil {
							item.set("size", len(blob.Data))
						}
						item.set("url", v.api("/repos/"+repo.FullName()+"/git/blobs/"+e.ID.String()))
					} else {
						item.set("url", v.api("/repos/"+repo.FullName()+"/git/trees/"+e.ID.String()))
					}
					entries = append(entries, item)
					if recursive && e.IsTree() {
						walk(e.ID, prefix+e.Name+"/")
					}
				}
			}
			walk(treeID, "")
			if entries == nil {
				entries = []any{}
			}
			return jsonResponse(200, om("sha", treeID.String(), "url", v.api("/repos/"+repo.FullName()+"/git/trees/"+treeID.String()), "tree", entries, "truncated", false))
		}
		if method == "POST" && len(rest) == 0 {
			files := map[string]flatFile{}
			if baseText, ok := c.str("base_tree"); ok {
				if base, ok := parseSHA(baseText); ok {
					var flat []flatFile
					repo.Objects.flattenTree(base, "", &flat)
					for _, f := range flat {
						files[f.Path] = f
					}
				}
			}
			var changes []fileChange
			list, _ := c.body["tree"].([]any)
			for _, item := range list {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				path, _ := entry["path"].(string)
				mode, _ := entry["mode"].(string)
				change := fileChange{Path: path, Mode: mode}
				if content, ok := entry["content"].(string); ok {
					change.Blob = repo.Objects.Put(ObjBlob, []byte(content)).ID
				} else if shaText, ok := entry["sha"].(string); ok {
					sha, ok := parseSHA(shaText)
					if !ok {
						return unprocessable("tree.sha " + shaText + " is not a valid blob")
					}
					change.Blob = sha
				} else {
					change.Delete = true
				}
				changes = append(changes, change)
			}
			treeID := repo.treeWith(files, changes)
			return jsonResponse(201, om("sha", treeID.String(), "url", v.api("/repos/"+repo.FullName()+"/git/trees/"+treeID.String()), "tree", []any{}, "truncated", false))
		}
	case "blobs":
		if method == "GET" && len(rest) == 1 {
			sha, ok := parseSHA(rest[0])
			o := repo.Objects.Get(sha)
			if !ok || o == nil || o.Type != ObjBlob {
				return notFound()
			}
			return jsonResponse(200, om(
				"sha", sha.String(),
				"node_id", nodeID("Blob", int64(sha[0])<<16|int64(sha[1])<<8|int64(sha[2])),
				"size", len(o.Data),
				"url", v.api("/repos/"+repo.FullName()+"/git/blobs/"+sha.String()),
				"content", wrapBase64(o.Data),
				"encoding", "base64",
			))
		}
		if method == "POST" && len(rest) == 0 {
			content, _ := c.str("content")
			data := []byte(content)
			if encoding, _ := c.str("encoding"); encoding == "base64" {
				decoded, err := decodeBase64(content)
				if err != nil {
					return unprocessable("content is not valid Base64")
				}
				data = decoded
			}
			o := repo.Objects.Put(ObjBlob, data)
			return jsonResponse(201, om("sha", o.ID.String(), "url", v.api("/repos/"+repo.FullName()+"/git/blobs/"+o.ID.String())))
		}
	}
	return nil
}

// signatureFrom reads an {name, email, date} object from the body, falling back to a default.
func (c *rc) signatureFrom(key string, fallback Signature) Signature {
	obj, ok := c.body[key].(map[string]any)
	if !ok {
		return fallback
	}
	sig := fallback
	if name, ok := obj["name"].(string); ok && name != "" {
		sig.Name = name
	}
	if email, ok := obj["email"].(string); ok && email != "" {
		sig.Email = email
	}
	if date, ok := obj["date"].(string); ok {
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			sig.When = t.Unix()
		}
	}
	return sig
}

func decodeBase64(text string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, text)
	if data, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(cleaned, "="))
}

// ---------------------------------------------------------------- contents

func (c *rc) contents(repo *Repo, method, path string) *Response {
	a, v := c.a, c.v
	path = strings.Trim(path, "/")
	switch method {
	case "GET":
		ref := c.req.Query().Get("ref")
		tip, ok := repo.resolveRev(ref)
		if !ok {
			if ref == "" && len(repo.sortedRefs()) == 0 {
				return jsonMessage(404, "This repository is empty.")
			}
			if ref != "" {
				return jsonMessage(404, "No commit found for the ref "+ref)
			}
			return notFound()
		}
		if ref == "" {
			ref = repo.DefaultBranch
		}
		commit := repo.Objects.Get(tip).AsCommit()
		if commit == nil {
			return notFound()
		}
		entry, ok := repo.Objects.lookupPath(commit.Tree, path)
		if !ok {
			return notFound()
		}
		if !entry.IsTree() {
			return jsonResponse(200, v.contentEntry(repo, ref, path, entry, true))
		}
		children := repo.Objects.Get(entry.ID).AsTree()
		sorted := make([]TreeEntry, len(children))
		copy(sorted, children)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		items := make([]any, 0, len(sorted))
		for _, child := range sorted {
			childPath := child.Name
			if path != "" {
				childPath = path + "/" + child.Name
			}
			items = append(items, v.contentEntry(repo, ref, childPath, child, false))
		}
		return jsonResponse(200, items)
	case "PUT", "DELETE":
		if path == "" {
			return notFound()
		}
		message, hasMessage := c.str("message")
		if !hasMessage || message == "" {
			if method == "PUT" {
				return validationFailed("Commit", "message", "missing_field", "")
			}
			message = "Delete " + path
		}
		branch, _ := c.str("branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := repo.Branch(branch)
		if tip.IsZero() && len(repo.sortedRefs()) > 0 {
			return jsonMessage(404, "Branch "+branch+" not found")
		}
		files := repo.filesOf(tip)
		existing, exists := files[path]
		suppliedSHA, hasSHA := c.str("sha")
		var change fileChange
		status := 200
		if method == "PUT" {
			encoded, ok := c.str("content")
			if !ok {
				return validationFailed("Commit", "content", "missing_field", "")
			}
			data, err := decodeBase64(encoded)
			if err != nil {
				return unprocessable("content is not valid Base64")
			}
			if exists {
				if !hasSHA {
					return unprocessable(`Invalid request.` + "\n\n" + `"sha" wasn't supplied.`)
				}
				if !strings.EqualFold(suppliedSHA, existing.ID.String()) {
					return jsonMessage(409, path+" does not match "+suppliedSHA)
				}
			} else {
				status = 201
				// A path cannot be both a file and a directory.
				for other := range files {
					if strings.HasPrefix(other, path+"/") || strings.HasPrefix(path, other+"/") {
						return unprocessable("path contains a malformed path component")
					}
				}
			}
			change = fileChange{Path: path, Blob: repo.Objects.Put(ObjBlob, data).ID}
		} else {
			if !exists {
				return notFound()
			}
			if !hasSHA {
				return unprocessable(`Invalid request.` + "\n\n" + `"sha" wasn't supplied.`)
			}
			if !strings.EqualFold(suppliedSHA, existing.ID.String()) {
				return jsonMessage(409, path+" does not match "+suppliedSHA)
			}
			change = fileChange{Path: path, Delete: true}
		}
		tree := repo.treeWith(files, []fileChange{change})
		sig := a.signature()
		author := c.signatureFrom("author", sig)
		committer := c.signatureFrom("committer", author)
		var parents []SHA
		if !tip.IsZero() {
			parents = []SHA{tip}
		}
		id := repo.commit(tree, parents, author, committer, message)
		repo.setRef("refs/heads/"+branch, id)
		repo.PushedAt = a.now()
		repo.UpdatedAt = repo.PushedAt
		commitView := v.gitCommit(repo, repo.Objects.Get(id).AsCommit())
		if method == "DELETE" {
			return jsonResponse(200, om("content", nil, "commit", commitView))
		}
		entry := TreeEntry{Mode: "100644", Name: path, ID: change.Blob}
		return jsonResponse(status, om("content", v.contentEntry(repo, branch, path, entry, true), "commit", commitView))
	}
	return nil
}
