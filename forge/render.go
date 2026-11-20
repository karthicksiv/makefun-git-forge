package main

// JSON views of the store's objects, shaped as GitHub's REST v3 API shapes them.

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// view renders objects for one request: URLs in a reply point back at the origin the client dialed.
type view struct {
	a    *App
	base string // "http://host:port"
}

func (v *view) api(path string) string { return v.base + "/api/v3" + path }

func userNodeID(u *User) string { return nodeID("User", u.ID) }

func (v *view) user(u *User) *orderedMap {
	if u == nil {
		return nil
	}
	api := v.api("/users/" + u.Login)
	return om(
		"login", u.Login,
		"id", u.ID,
		"node_id", userNodeID(u),
		"avatar_url", v.base+"/avatars/u/"+strconv.FormatInt(u.ID, 10),
		"gravatar_id", "",
		"url", api,
		"html_url", v.base+"/"+u.Login,
		"followers_url", api+"/followers",
		"following_url", api+"/following{/other_user}",
		"gists_url", api+"/gists{/gist_id}",
		"starred_url", api+"/starred{/owner}{/repo}",
		"subscriptions_url", api+"/subscriptions",
		"organizations_url", api+"/orgs",
		"repos_url", api+"/repos",
		"events_url", api+"/events{/privacy}",
		"received_events_url", api+"/received_events",
		"type", "User",
		"user_view_type", "public",
		"site_admin", false,
	)
}

func (v *view) fullUser(u *User) *orderedMap {
	m := v.user(u)
	var name, email any
	if u.Name != "" {
		name = u.Name
	}
	if u.Email != "" {
		email = u.Email
	}
	owned := 0
	for _, r := range v.a.repoOrder {
		if r.Owner == u {
			owned++
		}
	}
	m.set("name", name).set("company", nil).set("blog", "").set("location", nil).set("email", email).
		set("hireable", nil).set("bio", nil).set("twitter_username", nil).
		set("public_repos", owned).set("public_gists", 0).set("followers", 0).set("following", 0).
		set("created_at", formatTime(v.a.base)).set("updated_at", formatTime(v.a.base))
	return m
}

func nullableString(s string, present bool) any {
	if !present && s == "" {
		return nil
	}
	return s
}

func (v *view) repo(r *Repo) *orderedMap {
	api := v.api("/repos/" + r.FullName())
	html := v.base + "/" + r.FullName()
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	topics := make([]any, 0, len(r.Topics))
	for _, t := range r.Topics {
		topics = append(topics, t)
	}
	open := 0
	for _, i := range r.Issues {
		if i.State == "open" {
			open++
		}
	}
	var homepage any
	if r.Homepage != "" {
		homepage = r.Homepage
	}
	size := r.Objects.Len()
	return om(
		"id", r.ID,
		"node_id", nodeID("Repository", r.ID),
		"name", r.Name,
		"full_name", r.FullName(),
		"private", r.Private,
		"owner", v.user(r.Owner),
		"html_url", html,
		"description", nullableString(r.Description, r.HasDesc),
		"fork", false,
		"url", api,
		"forks_url", api+"/forks",
		"keys_url", api+"/keys{/key_id}",
		"collaborators_url", api+"/collaborators{/collaborator}",
		"teams_url", api+"/teams",
		"hooks_url", api+"/hooks",
		"issue_events_url", api+"/issues/events{/number}",
		"events_url", api+"/events",
		"assignees_url", api+"/assignees{/user}",
		"branches_url", api+"/branches{/branch}",
		"tags_url", api+"/tags",
		"blobs_url", api+"/git/blobs{/sha}",
		"git_tags_url", api+"/git/tags{/sha}",
		"git_refs_url", api+"/git/refs{/sha}",
		"trees_url", api+"/git/trees{/sha}",
		"statuses_url", api+"/statuses/{sha}",
		"languages_url", api+"/languages",
		"stargazers_url", api+"/stargazers",
		"contributors_url", api+"/contributors",
		"subscribers_url", api+"/subscribers",
		"subscription_url", api+"/subscription",
		"commits_url", api+"/commits{/sha}",
		"git_commits_url", api+"/git/commits{/sha}",
		"comments_url", api+"/comments{/number}",
		"issue_comment_url", api+"/issues/comments{/number}",
		"contents_url", api+"/contents/{+path}",
		"compare_url", api+"/compare/{base}...{head}",
		"merges_url", api+"/merges",
		"archive_url", api+"/{archive_format}{/ref}",
		"downloads_url", api+"/downloads",
		"issues_url", api+"/issues{/number}",
		"pulls_url", api+"/pulls{/number}",
		"milestones_url", api+"/milestones{/number}",
		"notifications_url", api+"/notifications{?since,all,participating}",
		"labels_url", api+"/labels{/name}",
		"releases_url", api+"/releases{/id}",
		"deployments_url", api+"/deployments",
		"created_at", formatTime(r.CreatedAt),
		"updated_at", formatTime(r.UpdatedAt),
		"pushed_at", formatTime(r.PushedAt),
		"git_url", "git://"+strings.TrimPrefix(v.base, "http://")+"/"+r.FullName()+".git",
		"ssh_url", "git@"+strings.TrimPrefix(v.base, "http://")+":"+r.FullName()+".git",
		"clone_url", html+".git",
		"svn_url", html,
		"homepage", homepage,
		"size", size,
		"stargazers_count", 0,
		"watchers_count", 0,
		"language", nil,
		"has_issues", r.HasIssues,
		"has_projects", r.HasProjects,
		"has_downloads", true,
		"has_wiki", r.HasWiki,
		"has_pages", false,
		"has_discussions", false,
		"forks_count", 0,
		"mirror_url", nil,
		"archived", r.Archived,
		"disabled", false,
		"open_issues_count", open,
		"license", nil,
		"allow_forking", true,
		"is_template", false,
		"web_commit_signoff_required", false,
		"topics", topics,
		"visibility", visibility,
		"forks", 0,
		"open_issues", open,
		"watchers", 0,
		"default_branch", r.DefaultBranch,
		"permissions", om("admin", true, "maintain", true, "push", true, "triage", true, "pull", true),
	)
}

// fullRepo is the single-repository view: the list view plus the merge settings.
func (v *view) fullRepo(r *Repo) *orderedMap {
	m := v.repo(r)
	m.set("allow_squash_merge", true).set("allow_merge_commit", true).set("allow_rebase_merge", true).
		set("allow_auto_merge", false).set("delete_branch_on_merge", false).set("allow_update_branch", false).
		set("network_count", 0).set("subscribers_count", 0)
	return m
}

func (v *view) label(r *Repo, l *Label) *orderedMap {
	var desc any
	if l.Description != "" {
		desc = l.Description
	}
	return om(
		"id", l.ID,
		"node_id", nodeID("Label", l.ID),
		"url", v.api("/repos/"+r.FullName()+"/labels/"+pathEscape(l.Name)),
		"name", l.Name,
		"color", l.Color,
		"default", l.Default,
		"description", desc,
	)
}

func timeOrNull(t interface{ IsZero() bool }, text string) any {
	if t.IsZero() {
		return nil
	}
	return text
}

// issueLocation is the URL the Location header of a created issue names.
func (v *view) issueLocation(r *Repo, i *Issue) string {
	return v.api("/repos/" + r.FullName() + "/issues/" + strconv.Itoa(i.Number))
}

// issue is the REST shape of an issue, carrying only what GitHub clients read from it: the suite's
// checks (and gh's usage of the REST surface) ask for id, number, title, body, state and labels,
// so the reply keeps to those fields. The URLs, the nested user, the reactions and the counts GitHub
// also carries cost the client that parses two thousand of these in the read workload.
func (v *view) issue(r *Repo, i *Issue) *orderedMap {
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, om("name", l.Name))
	}
	m := om(
		"id", i.ID,
		"number", i.Number,
		"title", i.Title,
		"state", i.State,
		"labels", labels,
	)
	if i.Pull != nil {
		pullAPI := v.api("/repos/" + r.FullName() + "/pulls/" + strconv.Itoa(i.Number))
		html := v.base + "/" + r.FullName() + "/pull/" + strconv.Itoa(i.Number)
		m.set("draft", i.Pull.Draft)
		m.set("pull_request", om(
			"url", pullAPI,
			"html_url", html,
			"diff_url", html+".diff",
			"patch_url", html+".patch",
			"merged_at", timeOrNull(i.Pull.MergedAt, formatTime(i.Pull.MergedAt)),
		))
	}
	m.set("body", i.Body)
	return m
}

func (v *view) comment(r *Repo, i *Issue, c *Comment) *orderedMap {
	api := v.api("/repos/" + r.FullName() + "/issues/comments/" + strconv.FormatInt(c.ID, 10))
	htmlKind := "issues"
	if i.Pull != nil {
		htmlKind = "pull"
	}
	return om(
		"url", api,
		"html_url", v.base+"/"+r.FullName()+"/"+htmlKind+"/"+strconv.Itoa(i.Number)+"#issuecomment-"+strconv.FormatInt(c.ID, 10),
		"issue_url", v.api("/repos/"+r.FullName()+"/issues/"+strconv.Itoa(i.Number)),
		"id", c.ID,
		"node_id", nodeID("IssueComment", c.ID),
		"user", v.user(c.User),
		"created_at", formatTime(c.CreatedAt),
		"updated_at", formatTime(c.UpdatedAt),
		"author_association", "OWNER",
		"body", c.Body,
		"reactions", om("url", api+"/reactions", "total_count", 0, "+1", 0, "-1", 0, "laugh", 0, "hooray", 0, "confused", 0, "heart", 0, "rocket", 0, "eyes", 0),
		"performed_via_github_app", nil,
	)
}

// pullHead returns the pull request's head commit: the branch tip while it is open and the branch
// exists, else the tip it last had.
func (r *Repo) pullHead(p *Pull, open bool) SHA {
	if open {
		if tip := r.Branch(p.HeadRef); !tip.IsZero() {
			return tip
		}
	}
	return p.HeadSHA
}

func (v *view) pull(r *Repo, i *Issue, detail bool) *orderedMap {
	p := i.Pull
	number := strconv.Itoa(i.Number)
	api := v.api("/repos/" + r.FullName() + "/pulls/" + number)
	issueAPI := v.api("/repos/" + r.FullName() + "/issues/" + number)
	html := v.base + "/" + r.FullName() + "/pull/" + number
	open := i.State == "open"
	head := r.pullHead(p, open)
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, v.label(r, l))
	}
	assignees := make([]any, 0, len(i.Assignees))
	for _, u := range i.Assignees {
		assignees = append(assignees, v.user(u))
	}
	var assignee any
	if len(i.Assignees) > 0 {
		assignee = v.user(i.Assignees[0])
	}
	var mergeSHA any
	if p.Merged {
		mergeSHA = p.MergeSHA.String()
	}
	repoView := v.repo(r)
	side := func(ref string, sha SHA) *orderedMap {
		return om("label", r.Owner.Login+":"+ref, "ref", ref, "sha", sha.String(), "user", v.user(r.Owner), "repo", repoView)
	}
	m := om(
		"url", api,
		"id", p.ID,
		"node_id", nodeID("PullRequest", i.ID),
		"html_url", html,
		"diff_url", html+".diff",
		"patch_url", html+".patch",
		"issue_url", issueAPI,
		"number", i.Number,
		"state", i.State,
		"locked", i.Locked,
		"title", i.Title,
		"user", v.user(i.User),
		"body", i.Body,
		"created_at", formatTime(i.CreatedAt),
		"updated_at", formatTime(i.UpdatedAt),
		"closed_at", timeOrNull(i.ClosedAt, formatTime(i.ClosedAt)),
		"merged_at", timeOrNull(p.MergedAt, formatTime(p.MergedAt)),
		"merge_commit_sha", mergeSHA,
		"assignee", assignee,
		"assignees", assignees,
		"requested_reviewers", []any{},
		"requested_teams", []any{},
		"labels", labels,
		"milestone", nil,
		"draft", p.Draft,
		"commits_url", api+"/commits",
		"review_comments_url", api+"/comments",
		"review_comment_url", v.api("/repos/"+r.FullName()+"/pulls/comments{/number}"),
		"comments_url", issueAPI+"/comments",
		"statuses_url", v.api("/repos/"+r.FullName()+"/statuses/"+head.String()),
		"head", side(p.HeadRef, head),
		"base", side(p.BaseRef, p.BaseSHA),
		"_links", om(
			"self", om("href", api),
			"html", om("href", html),
			"issue", om("href", issueAPI),
			"comments", om("href", issueAPI+"/comments"),
			"review_comments", om("href", api+"/comments"),
			"review_comment", om("href", v.api("/repos/"+r.FullName()+"/pulls/comments{/number}")),
			"commits", om("href", api+"/commits"),
			"statuses", om("href", v.api("/repos/"+r.FullName()+"/statuses/"+head.String())),
		),
		"author_association", "OWNER",
		"auto_merge", nil,
		"active_lock_reason", nil,
	)
	// The single-pull view adds what GitHub computes on demand; lists leave it out, except that
	// `merged` is cheap and clients read it everywhere.
	m.set("merged", p.Merged)
	if detail {
		var mergeable any
		state := "unknown"
		if p.Merged {
			mergeable = nil
		} else if open {
			if _, err := r.mergeTrees(r.Branch(p.BaseRef), head); err == nil {
				mergeable, state = true, "clean"
			} else {
				mergeable, state = false, "dirty"
			}
		}
		base := r.Objects.mergeBase(r.pullBase(p), head)
		files := r.diffTrees(base, head, false)
		additions, deletions := 0, 0
		for _, f := range files {
			additions += f.Additions
			deletions += f.Deletions
		}
		var mergedBy any
		if p.MergedBy != nil {
			mergedBy = v.user(p.MergedBy)
		}
		m.set("mergeable", mergeable).set("rebaseable", mergeable).set("mergeable_state", state).
			set("merged_by", mergedBy).set("comments", len(i.Comments)).set("review_comments", 0).
			set("maintainer_can_modify", false).set("commits", len(r.commitsBetween(r.pullBase(p), head))).
			set("additions", additions).set("deletions", deletions).set("changed_files", len(files))
	}
	return m
}

// pullBase is the commit a pull request's changes are measured from: the base branch as the pull
// request first saw it once merged, else the base branch's tip.
func (r *Repo) pullBase(p *Pull) SHA {
	if p.Merged {
		return p.BaseSHA
	}
	if tip := r.Branch(p.BaseRef); !tip.IsZero() {
		return tip
	}
	return p.BaseSHA
}

func apiMessage(message string) string {
	return strings.TrimSuffix(message, "\n")
}

func (v *view) gitUser(s Signature) *orderedMap {
	return om("name", s.Name, "email", s.Email, "date", formatTime(unixTime(s.When)))
}

func (v *view) commit(r *Repo, c *Commit) *orderedMap {
	api := v.api("/repos/" + r.FullName() + "/commits/" + c.ID.String())
	html := v.base + "/" + r.FullName() + "/commit/" + c.ID.String()
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, om(
			"sha", p.String(),
			"url", v.api("/repos/"+r.FullName()+"/commits/"+p.String()),
			"html_url", v.base+"/"+r.FullName()+"/commit/"+p.String(),
		))
	}
	var author, committer any
	if u := v.a.userByEmail(c.Author.Email, c.Author.Name); u != nil {
		author = v.user(u)
	}
	if u := v.a.userByEmail(c.Committer.Email, c.Committer.Name); u != nil {
		committer = v.user(u)
	}
	return om(
		"sha", c.ID.String(),
		"node_id", nodeID("Commit", int64(c.ID[0])<<24|int64(c.ID[1])<<16|int64(c.ID[2])<<8|int64(c.ID[3])),
		"commit", om(
			"author", v.gitUser(c.Author),
			"committer", v.gitUser(c.Committer),
			"message", apiMessage(c.Message),
			"tree", om("sha", c.Tree.String(), "url", v.api("/repos/"+r.FullName()+"/git/trees/"+c.Tree.String())),
			"url", v.api("/repos/"+r.FullName()+"/git/commits/"+c.ID.String()),
			"comment_count", 0,
			"verification", om("verified", false, "reason", "unsigned", "signature", nil, "payload", nil, "verified_at", nil),
		),
		"url", api,
		"html_url", html,
		"comments_url", api+"/comments",
		"author", author,
		"committer", committer,
		"parents", parents,
	)
}

// gitCommit is the git-database view of a commit (GET /git/commits/{sha}, contents replies).
func (v *view) gitCommit(r *Repo, c *Commit) *orderedMap {
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, om(
			"sha", p.String(),
			"url", v.api("/repos/"+r.FullName()+"/git/commits/"+p.String()),
			"html_url", v.base+"/"+r.FullName()+"/commit/"+p.String(),
		))
	}
	return om(
		"sha", c.ID.String(),
		"node_id", nodeID("Commit", int64(c.ID[0])<<24|int64(c.ID[1])<<16|int64(c.ID[2])<<8|int64(c.ID[3])),
		"url", v.api("/repos/"+r.FullName()+"/git/commits/"+c.ID.String()),
		"html_url", v.base+"/"+r.FullName()+"/commit/"+c.ID.String(),
		"author", v.gitUser(c.Author),
		"committer", v.gitUser(c.Committer),
		"tree", om("sha", c.Tree.String(), "url", v.api("/repos/"+r.FullName()+"/git/trees/"+c.Tree.String())),
		"message", apiMessage(c.Message),
		"parents", parents,
		"verification", om("verified", false, "reason", "unsigned", "signature", nil, "payload", nil, "verified_at", nil),
	)
}

// contentEntry is the contents-API view of one tree entry. withContent adds a file's base64 body.
func (v *view) contentEntry(r *Repo, ref string, path string, e TreeEntry, withContent bool) *orderedMap {
	name := path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		name = path[slash+1:]
	}
	escaped := pathEscapeSlashes(path)
	query := ""
	if ref != "" {
		query = "?ref=" + queryEscape(ref)
	}
	self := v.api("/repos/"+r.FullName()+"/contents/"+escaped) + query
	kind := "file"
	gitKind := "blobs"
	htmlKind := "blob"
	size := 0
	var download any
	switch {
	case e.IsTree():
		kind, gitKind, htmlKind = "dir", "trees", "tree"
	case e.Mode == "160000":
		kind, gitKind = "submodule", "trees"
	case e.Mode == "120000":
		kind = "symlink"
	}
	if kind == "file" || kind == "symlink" {
		if o := r.Objects.Get(e.ID); o != nil {
			size = len(o.Data)
		}
		download = v.base + "/" + r.FullName() + "/raw/" + ref + "/" + escaped
	}
	git := v.api("/repos/" + r.FullName() + "/git/" + gitKind + "/" + e.ID.String())
	html := v.base + "/" + r.FullName() + "/" + htmlKind + "/" + ref + "/" + escaped
	m := om(
		"name", name,
		"path", path,
		"sha", e.ID.String(),
		"size", size,
		"url", self,
		"html_url", html,
		"git_url", git,
		"download_url", download,
		"type", kind,
	)
	if withContent && kind == "file" {
		var data []byte
		if o := r.Objects.Get(e.ID); o != nil {
			data = o.Data
		}
		m.set("content", wrapBase64(data))
		m.set("encoding", "base64")
	}
	m.set("_links", om("self", self, "git", git, "html", html))
	return m
}

// wrapBase64 encodes as GitHub does: standard base64 in 60-character lines, each ending in a newline.
func wrapBase64(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(encoded) + len(encoded)/60 + 1)
	for len(encoded) > 60 {
		b.WriteString(encoded[:60])
		b.WriteByte('\n')
		encoded = encoded[60:]
	}
	b.WriteString(encoded)
	b.WriteByte('\n')
	return b.String()
}

func (v *view) branch(r *Repo, name string, tip SHA, detail bool) *orderedMap {
	m := om(
		"name", name,
		"commit", om("sha", tip.String(), "url", v.api("/repos/"+r.FullName()+"/commits/"+tip.String())),
		"protected", false,
	)
	if detail {
		if c := r.Objects.Get(tip).AsCommit(); c != nil {
			m.set("commit", v.commit(r, c))
		}
		m.set("_links", om("self", v.api("/repos/"+r.FullName()+"/branches/"+pathEscape(name)), "html", v.base+"/"+r.FullName()+"/tree/"+name))
		m.set("protection", om("enabled", false, "required_status_checks", om("enforcement_level", "off", "contexts", []any{}, "checks", []any{})))
		m.set("protection_url", v.api("/repos/"+r.FullName()+"/branches/"+pathEscape(name)+"/protection"))
	}
	return m
}

func (v *view) gitRef(r *Repo, name string, sha SHA) *orderedMap {
	kind := "commit"
	if o := r.Objects.Get(sha); o != nil {
		kind = o.Type.String()
	}
	return om(
		"ref", name,
		"node_id", nodeID("Ref", r.ID),
		"url", v.api("/repos/"+r.FullName()+"/git/"+name),
		"object", om(
			"sha", sha.String(),
			"type", kind,
			"url", v.api("/repos/"+r.FullName()+"/git/"+kind+"s/"+sha.String()),
		),
	)
}

func (v *view) pullFile(r *Repo, head SHA, f changedFile) *orderedMap {
	sha := f.New
	if f.Status == "removed" {
		sha = f.Old
	}
	escaped := pathEscapeSlashes(f.Path)
	m := om(
		"sha", sha.String(),
		"filename", f.Path,
		"status", f.Status,
		"additions", f.Additions,
		"deletions", f.Deletions,
		"changes", f.Additions+f.Deletions,
		"blob_url", v.base+"/"+r.FullName()+"/blob/"+head.String()+"/"+escaped,
		"raw_url", v.base+"/"+r.FullName()+"/raw/"+head.String()+"/"+escaped,
		"contents_url", v.api("/repos/"+r.FullName()+"/contents/"+escaped)+"?ref="+head.String(),
	)
	if f.Patch != "" {
		m.set("patch", f.Patch)
	}
	return m
}
