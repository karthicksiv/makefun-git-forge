package main

// REST v3: labels, issues, comments and pull requests. The same operations back GraphQL mutations
// and the MCP tools, so each lives in an App method that the REST handler only wraps.

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// dropIssueCaches forgets every rendered issue of a repository, after a change (a label renamed, the
// repository renamed) that shows in all of them.
func (repo *Repo) dropIssueCaches() {
	for _, issue := range repo.Issues {
		issue.cached.Store(nil)
	}
}

// apiError is a refusal an operation hands back: the HTTP reply REST sends, which GraphQL and MCP
// turn into their own error shapes.
type apiError struct {
	res     *Response
	message string
}

func refuse(res *Response, message string) *apiError { return &apiError{res: res, message: message} }

// ---------------------------------------------------------------- labels

func (c *rc) labels(repo *Repo, method string, rest []string) *Response {
	a, v := c.a, c.v
	if len(rest) == 0 {
		switch method {
		case "GET":
			items := make([]any, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				items = append(items, v.label(repo, l))
			}
			return c.list(items)
		case "POST":
			name, _ := c.str("name")
			name = strings.TrimSpace(name)
			if name == "" {
				return validationFailed("Label", "name", "missing_field", "")
			}
			if repo.label(name) != nil {
				return validationFailed("Label", "name", "already_exists", "")
			}
			color, _ := c.str("color")
			if color == "" {
				color = "ededed"
			}
			description, _ := c.str("description")
			l := &Label{ID: a.nextLabelID, Name: name, Color: strings.TrimPrefix(color, "#"), Description: description}
			a.nextLabelID++
			repo.Labels = append(repo.Labels, l)
			res := jsonResponse(201, v.label(repo, l))
			return res.setHeader("Location", v.api("/repos/"+repo.FullName()+"/labels/"+pathEscape(l.Name)))
		}
		return nil
	}
	l := repo.label(strings.Join(rest, "/"))
	if l == nil {
		return notFound()
	}
	switch method {
	case "GET":
		return jsonResponse(200, v.label(repo, l))
	case "PATCH":
		if name, ok := c.str("new_name"); ok && name != "" {
			l.Name = name
		} else if name, ok := c.str("name"); ok && name != "" {
			l.Name = name
		}
		if color, ok := c.str("color"); ok {
			l.Color = strings.TrimPrefix(color, "#")
		}
		if description, ok := c.str("description"); ok {
			l.Description = description
		}
		repo.dropIssueCaches()
		return jsonResponse(200, v.label(repo, l))
	case "DELETE":
		repo.dropIssueCaches()
		for i, other := range repo.Labels {
			if other == l {
				repo.Labels = append(repo.Labels[:i], repo.Labels[i+1:]...)
				break
			}
		}
		for _, issue := range repo.Issues {
			issue.removeLabel(l)
		}
		return &Response{Status: 204}
	}
	return nil
}

func (i *Issue) removeLabel(l *Label) {
	for idx, other := range i.Labels {
		if other == l {
			i.Labels = append(i.Labels[:idx:idx], i.Labels[idx+1:]...)
			return
		}
	}
}

// labelNames reads a labels value from a body: strings or {name} objects.
func labelNames(raw any) ([]string, bool) {
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		switch l := item.(type) {
		case string:
			names = append(names, l)
		case map[string]any:
			if name, ok := l["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names, true
}

// resolveLabels maps names to the repository's labels; an unknown name is a validation failure.
func (repo *Repo) resolveLabels(names []string) ([]*Label, *apiError) {
	out := make([]*Label, 0, len(names))
	for _, name := range names {
		l := repo.label(name)
		if l == nil {
			res := jsonResponse(422, om(
				"message", "Validation Failed",
				"errors", []any{om("value", name, "resource", "Label", "field", "name", "code", "invalid")},
				"documentation_url", "https://docs.github.com/rest/issues/issues#create-an-issue",
				"status", "422",
			))
			return nil, refuse(res, "Validation Failed: label "+strconv.Quote(name)+" does not exist")
		}
		duplicate := false
		for _, have := range out {
			if have == l {
				duplicate = true
			}
		}
		if !duplicate {
			out = append(out, l)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- issues

// issueInput is what creating an issue takes, whatever surface asked.
type issueInput struct {
	Title     string
	Body      string
	HasBody   bool
	Labels    []string
	Assignees []string
}

// createIssue adds an issue. The caller holds a.mu for writing.
func (a *App) createIssue(repo *Repo, in issueInput) (*Issue, *apiError) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, refuse(validationFailed("Issue", "title", "missing_field", ""), "Validation Failed: title is required")
	}
	labels, failure := repo.resolveLabels(in.Labels)
	if failure != nil {
		return nil, failure
	}
	now := a.now()
	issue := &Issue{
		ID:        a.nextIssueID,
		Number:    repo.nextNumber,
		Title:     in.Title,
		Body:      in.Body,
		HasBody:   in.HasBody,
		State:     "open",
		Labels:    labels,
		User:      a.viewer,
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, login := range in.Assignees {
		if u := a.users[strings.ToLower(login)]; u != nil {
			issue.Assignees = append(issue.Assignees, u)
		}
	}
	a.nextIssueID++
	repo.nextNumber++
	repo.Issues = append(repo.Issues, issue)
	repo.UpdatedAt = now
	return issue, nil
}

// setIssueState opens or closes an issue or pull request.
func (a *App) setIssueState(issue *Issue, state, reason string) {
	if state != "open" && state != "closed" {
		return
	}
	now := a.now()
	if state == "closed" {
		if issue.State != "closed" {
			issue.ClosedAt = now
		}
		if reason == "" {
			reason = "completed"
		}
		issue.StateReason = reason
	} else {
		if issue.State == "closed" {
			issue.StateReason = "reopened"
		}
		issue.ClosedAt = time.Time{}
	}
	if issue.Pull != nil {
		// A pull request carries no state reason.
		issue.StateReason = ""
	}
	issue.State = state
	issue.UpdatedAt = now
	issue.cached.Store(nil)
}

// addComment appends a comment to an issue or pull request.
func (a *App) addComment(repo *Repo, issue *Issue, body string) *Comment {
	now := a.now()
	comment := &Comment{ID: a.nextCommentID, Body: body, User: a.viewer, CreatedAt: now, UpdatedAt: now}
	a.nextCommentID++
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedAt = now
	issue.cached.Store(nil)
	return comment
}

type issueFilter struct {
	State     string // open, closed, all
	Labels    []string
	Sort      string // created, updated, comments
	Direction string // asc, desc
	PullsOnly bool
	NoPulls   bool
	Head      string
	Base      string
	Creator   string
	Assignee  string
}

// filterIssues selects and orders a repository's issues.
func (repo *Repo) filterIssues(f issueFilter) []*Issue {
	var out []*Issue
	for _, issue := range repo.Issues {
		if f.PullsOnly && issue.Pull == nil {
			continue
		}
		if f.NoPulls && issue.Pull != nil {
			continue
		}
		switch f.State {
		case "", "open":
			if issue.State != "open" {
				continue
			}
		case "closed":
			if issue.State != "closed" {
				continue
			}
		}
		matches := true
		for _, want := range f.Labels {
			found := false
			for _, l := range issue.Labels {
				if strings.EqualFold(l.Name, want) {
					found = true
				}
			}
			if !found {
				matches = false
			}
		}
		if !matches {
			continue
		}
		if f.Creator != "" && !strings.EqualFold(issue.User.Login, f.Creator) {
			continue
		}
		if f.Assignee != "" && f.Assignee != "*" {
			found := false
			for _, u := range issue.Assignees {
				if strings.EqualFold(u.Login, f.Assignee) {
					found = true
				}
			}
			if !found && f.Assignee != "none" {
				continue
			}
			if f.Assignee == "none" && len(issue.Assignees) > 0 {
				continue
			}
		}
		if issue.Pull != nil {
			if f.Head != "" {
				head := f.Head
				if _, branch, ok := strings.Cut(head, ":"); ok {
					head = branch
				}
				if issue.Pull.HeadRef != head {
					continue
				}
			}
			if f.Base != "" && issue.Pull.BaseRef != f.Base {
				continue
			}
		}
		out = append(out, issue)
	}
	desc := f.Direction != "asc"
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		var less, equal bool
		switch f.Sort {
		case "updated":
			less, equal = x.UpdatedAt.Before(y.UpdatedAt), x.UpdatedAt.Equal(y.UpdatedAt)
		case "comments":
			less, equal = len(x.Comments) < len(y.Comments), len(x.Comments) == len(y.Comments)
		default:
			less, equal = x.Number < y.Number, false
		}
		if equal {
			less = x.Number < y.Number
		}
		if desc {
			return !less && x != y
		}
		return less
	})
	return out
}

func (c *rc) issueFilterFromQuery() issueFilter {
	q := c.req.Query()
	f := issueFilter{State: q.Get("state"), Sort: q.Get("sort"), Direction: q.Get("direction"), Creator: q.Get("creator"), Assignee: q.Get("assignee"), Head: q.Get("head"), Base: q.Get("base")}
	if labels := q.Get("labels"); labels != "" {
		for _, name := range strings.Split(labels, ",") {
			if name = strings.TrimSpace(name); name != "" {
				f.Labels = append(f.Labels, name)
			}
		}
	}
	return f
}

func (c *rc) issues(repo *Repo, method string, rest []string) *Response {
	a, v := c.a, c.v
	if len(rest) == 0 {
		switch method {
		case "GET":
			filter := c.issueFilterFromQuery()
			filter.NoPulls = c.issuesOnly
			found := repo.filterIssues(filter)
			items := make([]any, 0, len(found))
			for _, issue := range found {
				items = append(items, v.issue(repo, issue))
			}
			return c.list(items)
		case "POST":
			in := issueInput{}
			if raw, present := c.body["title"]; present {
				switch title := raw.(type) {
				case string:
					in.Title = title
				case float64:
					in.Title = strconv.FormatFloat(title, 'f', -1, 64)
				}
			}
			in.Body, in.HasBody = c.str("body")
			if raw, present := c.body["labels"]; present {
				in.Labels, _ = labelNames(raw)
			}
			if list, ok := c.body["assignees"].([]any); ok {
				for _, item := range list {
					if login, ok := item.(string); ok {
						in.Assignees = append(in.Assignees, login)
					}
				}
			}
			if login, ok := c.str("assignee"); ok && login != "" {
				in.Assignees = append(in.Assignees, login)
			}
			issue, failure := a.createIssue(repo, in)
			if failure != nil {
				return failure.res
			}
			// The new issue is rendered once: its JSON answers this request and stays on the
			// issue as the reply to the GETs that follow.
			res := &Response{Status: 201, ContentType: jsonType, Body: v.storeIssueReply(repo, issue, c.req.ordinal).json()}
			return res.setHeader("Location", v.issueLocation(repo, issue))
		}
		return nil
	}
	if rest[0] == "comments" {
		return c.issueCommentByID(repo, method, rest[1:])
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return nil
	}
	issue := repo.issue(number)
	if issue == nil {
		return notFound()
	}
	if len(rest) == 1 {
		switch method {
		case "GET":
			return c.issueResponse(repo, issue)
		case "PATCH":
			if title, ok := c.str("title"); ok {
				if strings.TrimSpace(title) == "" {
					return validationFailed("Issue", "title", "missing_field", "")
				}
				issue.Title = title
			}
			if raw, present := c.body["body"]; present {
				body, isString := raw.(string)
				issue.Body, issue.HasBody = body, isString
			}
			if raw, present := c.body["labels"]; present {
				names, _ := labelNames(raw)
				labels, failure := repo.resolveLabels(names)
				if failure != nil {
					return failure.res
				}
				issue.Labels = labels
			}
			if list, ok := c.body["assignees"].([]any); ok {
				issue.Assignees = issue.Assignees[:0]
				for _, item := range list {
					if login, ok := item.(string); ok {
						if u := a.users[strings.ToLower(login)]; u != nil {
							issue.Assignees = append(issue.Assignees, u)
						}
					}
				}
			}
			reason, _ := c.str("state_reason")
			if state, ok := c.str("state"); ok {
				if state != "open" && state != "closed" {
					return validationFailed("Issue", "state", "invalid", "")
				}
				a.setIssueState(issue, state, reason)
			} else {
				issue.UpdatedAt = a.now()
			}
			issue.cached.Store(nil)
			return jsonResponse(200, v.issue(repo, issue))
		}
		return nil
	}
	switch rest[1] {
	case "comments":
		if len(rest) != 2 {
			return nil
		}
		switch method {
		case "GET":
			items := make([]any, 0, len(issue.Comments))
			for _, comment := range issue.Comments {
				items = append(items, v.comment(repo, issue, comment))
			}
			return c.list(items)
		case "POST":
			body, ok := c.str("body")
			if !ok || body == "" {
				return validationFailed("IssueComment", "body", "missing_field", "")
			}
			comment := a.addComment(repo, issue, body)
			res := jsonResponse(201, v.comment(repo, issue, comment))
			return res.setHeader("Location", v.api("/repos/"+repo.FullName()+"/issues/comments/"+strconv.FormatInt(comment.ID, 10)))
		}
	case "labels":
		listLabels := func(status int) *Response {
			items := make([]any, 0, len(issue.Labels))
			for _, l := range issue.Labels {
				items = append(items, v.label(repo, l))
			}
			return jsonResponse(status, items)
		}
		if len(rest) == 2 {
			var names []string
			if raw, present := c.body["labels"]; present {
				names, _ = labelNames(raw)
			} else if raw, present := c.body["__array"]; present {
				names, _ = labelNames(raw)
			}
			switch method {
			case "GET":
				return listLabels(200)
			case "POST", "PUT":
				labels, failure := repo.resolveLabels(names)
				if failure != nil {
					return failure.res
				}
				if method == "PUT" {
					issue.Labels = labels
				} else {
					for _, l := range labels {
						issue.removeLabel(l)
						issue.Labels = append(issue.Labels, l)
					}
				}
				issue.UpdatedAt = a.now()
				issue.cached.Store(nil)
				return listLabels(200)
			case "DELETE":
				issue.Labels = nil
				issue.UpdatedAt = a.now()
				issue.cached.Store(nil)
				return &Response{Status: 204}
			}
		} else if method == "DELETE" {
			l := repo.label(strings.Join(rest[2:], "/"))
			if l == nil {
				return jsonMessage(404, "Label does not exist")
			}
			issue.removeLabel(l)
			issue.UpdatedAt = a.now()
			issue.cached.Store(nil)
			return listLabels(200)
		}
	case "events", "timeline", "reactions":
		if method == "GET" {
			return jsonResponse(200, []any{})
		}
	}
	return nil
}

// issueResponse answers GET /issues/{n}, the hottest read there is, from the reply kept on the
// issue until it next changes. A stored reply is tied to the origin its URLs name.
func (c *rc) issueResponse(repo *Repo, issue *Issue) *Response {
	reply := issue.cached.Load()
	if reply == nil || reply.origin != c.v.base {
		reply = c.v.storeIssueReply(repo, issue, c.req.ordinal)
	}
	return &Response{Status: 200, ContentType: jsonType, Body: reply.json()}
}

// issueReplyHeadRoom is the space left ahead of an issue's JSON for the reply head, which can only
// be written once the JSON's length is known.
const issueReplyHeadRoom = 192

// storeIssueReply renders an issue for the view's origin and keeps the whole GET reply on the
// issue: the JSON goes straight into the reply's own buffer, and the head is laid in front of it.
// The head comes from appendReplyHead with the headers a REST reply carries, so the stored bytes
// are what the ordinary path would send, under the id of the request (its ordinal) that rendered
// them.
func (v *view) storeIssueReply(repo *Repo, issue *Issue, ordinal uint64) *issueReply {
	return v.storeIssueReplyFrom(repo, issue, requestID(ordinal), nil)
}

// storeIssueReplyFrom is storeIssueReply given the request id itself, with the JSON written from a
// new-issue template when the caller has one for this issue and by the generic renderer when not.
func (v *view) storeIssueReplyFrom(repo *Repo, issue *Issue, id string, template *issueTemplate) *issueReply {
	var buf []byte
	if template != nil {
		// A template knows its size but for the title and body, which may grow a little escaped.
		buf = make([]byte, issueReplyHeadRoom, issueReplyHeadRoom+template.size+len(issue.Title)+len(issue.Body)+96)
		buf = template.append(buf, issue)
	} else {
		buf = make([]byte, issueReplyHeadRoom, issueReplyHeadRoom+2048)
		buf = appendJSON(buf, v.issue(repo, issue))
	}
	var room [issueReplyHeadRoom]byte
	headers := [2]string{"X-GitHub-Request-Id", id}
	head := appendReplyHead(room[:0], 200, jsonType, headers[:], len(buf)-issueReplyHeadRoom, true)
	var wire []byte
	if len(head) <= issueReplyHeadRoom {
		wire = buf[issueReplyHeadRoom-len(head):]
		copy(wire, head)
	} else {
		wire = append(head, buf[issueReplyHeadRoom:]...)
	}
	reply := &issueReply{origin: v.base, head: len(head), wire: wire}
	issue.cached.Store(reply)
	return reply
}

func (c *rc) issueCommentByID(repo *Repo, method string, rest []string) *Response {
	if len(rest) == 0 {
		if method != "GET" {
			return nil
		}
		var items []any
		for _, issue := range repo.Issues {
			for _, comment := range issue.Comments {
				items = append(items, c.v.comment(repo, issue, comment))
			}
		}
		return c.list(items)
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || len(rest) != 1 {
		return nil
	}
	for _, issue := range repo.Issues {
		for idx, comment := range issue.Comments {
			if comment.ID != id {
				continue
			}
			switch method {
			case "GET":
				return jsonResponse(200, c.v.comment(repo, issue, comment))
			case "PATCH":
				body, ok := c.str("body")
				if !ok || body == "" {
					return validationFailed("IssueComment", "body", "missing_field", "")
				}
				comment.Body = body
				comment.UpdatedAt = c.a.now()
				return jsonResponse(200, c.v.comment(repo, issue, comment))
			case "DELETE":
				issue.Comments = append(issue.Comments[:idx], issue.Comments[idx+1:]...)
				issue.cached.Store(nil)
				return &Response{Status: 204}
			}
			return nil
		}
	}
	return notFound()
}

// ---------------------------------------------------------------- pull requests

type pullInput struct {
	Title   string
	Body    string
	HasBody bool
	Head    string
	Base    string
	Draft   bool
}

// createPull opens a pull request. The caller holds a.mu for writing.
func (a *App) createPull(repo *Repo, in pullInput) (*Issue, *apiError) {
	invalid := func(field, detail string) *apiError {
		res := jsonResponse(422, om(
			"message", "Validation Failed",
			"errors", []any{om("resource", "PullRequest", "field", field, "code", "invalid")},
			"documentation_url", "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
			"status", "422",
		))
		return refuse(res, "Validation Failed: "+detail)
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, refuse(validationFailed("PullRequest", "title", "missing_field", ""), "Validation Failed: title is required")
	}
	head := in.Head
	if _, branch, ok := strings.Cut(head, ":"); ok {
		head = branch
	}
	head = strings.TrimPrefix(head, "refs/heads/")
	base := strings.TrimPrefix(in.Base, "refs/heads/")
	if base == "" {
		base = repo.DefaultBranch
	}
	if head == "" {
		return nil, refuse(validationFailed("PullRequest", "head", "missing_field", ""), "Validation Failed: head is required")
	}
	headTip := repo.Branch(head)
	if headTip.IsZero() {
		return nil, invalid("head", "head branch "+strconv.Quote(head)+" does not exist")
	}
	baseTip := repo.Branch(base)
	if baseTip.IsZero() {
		return nil, invalid("base", "base branch "+strconv.Quote(base)+" does not exist")
	}
	if head == base {
		return nil, invalid("head", "head and base are the same branch")
	}
	for _, other := range repo.Issues {
		if other.Pull != nil && other.State == "open" && other.Pull.HeadRef == head && other.Pull.BaseRef == base {
			res := jsonResponse(422, om(
				"message", "Validation Failed",
				"errors", []any{om("resource", "PullRequest", "code", "custom", "message", "A pull request already exists for "+repo.Owner.Login+":"+head+".")},
				"documentation_url", "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
				"status", "422",
			))
			return nil, refuse(res, "A pull request already exists for "+repo.Owner.Login+":"+head+".")
		}
	}
	now := a.now()
	issue := &Issue{
		ID:        a.nextIssueID,
		Number:    repo.nextNumber,
		Title:     in.Title,
		Body:      in.Body,
		HasBody:   in.HasBody,
		State:     "open",
		User:      a.viewer,
		CreatedAt: now,
		UpdatedAt: now,
		Pull:      &Pull{ID: a.nextPullID, HeadRef: head, BaseRef: base, HeadSHA: headTip, BaseSHA: baseTip, Draft: in.Draft},
	}
	a.nextIssueID++
	a.nextPullID++
	repo.nextNumber++
	repo.Issues = append(repo.Issues, issue)
	repo.UpdatedAt = now
	return issue, nil
}

// mergePull merges a pull request into its base branch. method is merge, squash or rebase.
func (a *App) mergePull(repo *Repo, issue *Issue, method, title, message, expectHead string) (SHA, *apiError) {
	p := issue.Pull
	notMergeable := func(text string) *apiError {
		return refuse(jsonMessage(405, text), text)
	}
	if p.Merged {
		return zeroSHA, notMergeable("Pull Request is not mergeable")
	}
	if issue.State != "open" {
		return zeroSHA, notMergeable("Pull Request is not mergeable")
	}
	headTip := repo.pullHead(p, true)
	baseTip := repo.Branch(p.BaseRef)
	if headTip.IsZero() || baseTip.IsZero() {
		return zeroSHA, notMergeable("Pull Request is not mergeable")
	}
	if expectHead != "" && !strings.EqualFold(expectHead, headTip.String()) {
		text := "Head branch was modified. Review and try the merge again."
		return zeroSHA, refuse(jsonMessage(409, text), text)
	}
	tree, err := repo.mergeTrees(baseTip, headTip)
	if err != nil {
		return zeroSHA, notMergeable("Pull Request is not mergeable")
	}
	switch method {
	case "", "merge", "squash", "rebase":
	default:
		text := "Invalid merge method: " + method
		return zeroSHA, refuse(unprocessable(text), text)
	}
	sig := a.signature()
	var merged SHA
	switch method {
	case "squash", "rebase":
		subject := title
		if subject == "" {
			subject = issue.Title + " (#" + strconv.Itoa(issue.Number) + ")"
		}
		text := subject
		if message != "" {
			text += "\n\n" + message
		} else if method == "squash" && issue.Body != "" {
			text += "\n\n" + issue.Body
		}
		merged = repo.commit(tree, []SHA{baseTip}, sig, sig, text)
	default:
		subject := title
		if subject == "" {
			subject = "Merge pull request #" + strconv.Itoa(issue.Number) + " from " + repo.Owner.Login + "/" + p.HeadRef
		}
		text := subject
		if message != "" {
			text += "\n\n" + message
		} else {
			text += "\n\n" + issue.Title
		}
		if repo.Objects.isAncestor(headTip, baseTip) {
			// Nothing to bring over: record the merge with the base's own tree.
			merged = repo.commit(tree, []SHA{baseTip}, sig, sig, text)
		} else {
			merged = repo.commit(tree, []SHA{baseTip, headTip}, sig, sig, text)
		}
	}
	repo.setRef("refs/heads/"+p.BaseRef, merged)
	now := a.now()
	p.Merged = true
	p.MergedAt = now
	p.MergedBy = a.viewer
	p.MergeSHA = merged
	p.HeadSHA = headTip
	p.BaseSHA = baseTip
	issue.State = "closed"
	issue.ClosedAt = now
	issue.UpdatedAt = now
	issue.cached.Store(nil)
	repo.PushedAt = now
	repo.UpdatedAt = now
	return merged, nil
}

func (c *rc) pulls(repo *Repo, method string, rest []string) *Response {
	a, v := c.a, c.v
	if len(rest) == 0 {
		switch method {
		case "GET":
			f := c.issueFilterFromQuery()
			f.PullsOnly = true
			found := repo.filterIssues(f)
			items := make([]any, 0, len(found))
			for _, issue := range found {
				items = append(items, v.pull(repo, issue, false))
			}
			return c.list(items)
		case "POST":
			in := pullInput{}
			in.Title, _ = c.str("title")
			in.Body, in.HasBody = c.str("body")
			in.Head, _ = c.str("head")
			in.Base, _ = c.str("base")
			in.Draft, _ = c.boolean("draft")
			if raw, ok := c.body["issue"].(float64); ok && in.Title == "" {
				if source := repo.issue(int(raw)); source != nil {
					in.Title, in.Body, in.HasBody = source.Title, source.Body, source.HasBody
				}
			}
			issue, failure := a.createPull(repo, in)
			if failure != nil {
				return failure.res
			}
			res := jsonResponse(201, v.pull(repo, issue, true))
			return res.setHeader("Location", v.api("/repos/"+repo.FullName()+"/pulls/"+strconv.Itoa(issue.Number)))
		}
		return nil
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		if method == "GET" && rest[0] == "comments" {
			return jsonResponse(200, []any{})
		}
		return nil
	}
	issue := repo.issue(number)
	if issue == nil || issue.Pull == nil {
		return notFound()
	}
	p := issue.Pull
	if len(rest) == 1 {
		switch method {
		case "GET":
			return jsonResponse(200, v.pull(repo, issue, true))
		case "PATCH":
			if title, ok := c.str("title"); ok && strings.TrimSpace(title) != "" {
				issue.Title = title
			}
			if raw, present := c.body["body"]; present {
				body, isString := raw.(string)
				issue.Body, issue.HasBody = body, isString
			}
			if base, ok := c.str("base"); ok && base != "" && !p.Merged {
				tip := repo.Branch(base)
				if tip.IsZero() {
					return validationFailed("PullRequest", "base", "invalid", "")
				}
				p.BaseRef, p.BaseSHA = base, tip
			}
			if state, ok := c.str("state"); ok {
				if state != "open" && state != "closed" {
					return validationFailed("PullRequest", "state", "invalid", "")
				}
				if p.Merged && state == "open" {
					return unprocessable("Validation Failed: a merged pull request cannot be reopened")
				}
				if state == "closed" && issue.State == "open" {
					p.HeadSHA = repo.pullHead(p, true)
				}
				a.setIssueState(issue, state, "")
			} else {
				issue.UpdatedAt = a.now()
			}
			issue.cached.Store(nil)
			return jsonResponse(200, v.pull(repo, issue, true))
		}
		return nil
	}
	head := repo.pullHead(p, issue.State == "open")
	switch rest[1] {
	case "files":
		if method != "GET" {
			return nil
		}
		base := repo.Objects.mergeBase(repo.pullBase(p), head)
		files := repo.diffTrees(base, head, true)
		items := make([]any, 0, len(files))
		for _, f := range files {
			items = append(items, v.pullFile(repo, head, f))
		}
		return c.list(items)
	case "commits":
		if method != "GET" {
			return nil
		}
		commits := repo.commitsBetween(repo.pullBase(p), head)
		items := make([]any, 0, len(commits))
		for _, commit := range commits {
			items = append(items, v.commit(repo, commit))
		}
		return c.list(items)
	case "merge":
		switch method {
		case "GET":
			if p.Merged {
				return &Response{Status: 204}
			}
			return notFound()
		case "PUT":
			mergeMethod, _ := c.str("merge_method")
			title, _ := c.str("commit_title")
			message, _ := c.str("commit_message")
			expect, _ := c.str("sha")
			merged, failure := a.mergePull(repo, issue, mergeMethod, title, message, expect)
			if failure != nil {
				return failure.res
			}
			return jsonResponse(200, om("sha", merged.String(), "merged", true, "message", "Pull Request successfully merged"))
		}
	case "comments", "reviews", "requested_reviewers":
		if method == "GET" {
			return jsonResponse(200, []any{})
		}
	case "update-branch":
		if method == "PUT" {
			return jsonResponse(202, om("message", "Updating pull request branch.", "url", v.api("/repos/"+repo.FullName()+"/pulls/"+strconv.Itoa(issue.Number))))
		}
	}
	return nil
}

// ---------------------------------------------------------------- search

// searchQuery is a search string split into free terms and key:value qualifiers.
type searchQuery struct {
	terms      []string
	qualifiers map[string][]string
}

func parseSearch(query string) searchQuery {
	sq := searchQuery{qualifiers: map[string][]string{}}
	for _, word := range strings.Fields(query) {
		if key, value, ok := strings.Cut(word, ":"); ok && key != "" && value != "" {
			sq.qualifiers[key] = append(sq.qualifiers[key], strings.Trim(value, `"`))
		} else {
			sq.terms = append(sq.terms, strings.ToLower(strings.Trim(word, `"`)))
		}
	}
	return sq
}

func (sq searchQuery) matches(text string) bool {
	lower := strings.ToLower(text)
	for _, term := range sq.terms {
		if !strings.Contains(lower, term) {
			return false
		}
	}
	return true
}

// searchRepos finds repositories whose name or description holds every term.
func (a *App) searchRepos(query string) []*Repo {
	sq := parseSearch(query)
	var out []*Repo
	for _, repo := range a.repoOrder {
		if owners := sq.qualifiers["user"]; len(owners) > 0 && !strings.EqualFold(owners[0], repo.Owner.Login) {
			continue
		}
		if sq.matches(repo.Name + " " + repo.Description) {
			out = append(out, repo)
		}
	}
	return out
}

// searchIssues finds issues and pull requests by terms and the repo:, is: and state: qualifiers.
func (a *App) searchIssues(query string) []issueRef {
	sq := parseSearch(query)
	var out []issueRef
	for _, repo := range a.repoOrder {
		if wanted := sq.qualifiers["repo"]; len(wanted) > 0 && !strings.EqualFold(wanted[0], repo.FullName()) {
			continue
		}
		for _, issue := range repo.Issues {
			skip := false
			for _, value := range sq.qualifiers["is"] {
				switch value {
				case "issue":
					skip = skip || issue.Pull != nil
				case "pr", "pull-request":
					skip = skip || issue.Pull == nil
				case "open", "closed":
					skip = skip || issue.State != value
				}
			}
			for _, value := range sq.qualifiers["state"] {
				skip = skip || issue.State != value
			}
			if skip || !sq.matches(issue.Title+" "+issue.Body) {
				continue
			}
			out = append(out, issueRef{repo, issue})
		}
	}
	return out
}

// search answers GET /search/issues and /search/repositories with simple term matching.
func (c *rc) search(kind string) *Response {
	query := c.req.Query().Get("q")
	var items []any
	switch kind {
	case "repositories":
		for _, repo := range c.a.searchRepos(query) {
			items = append(items, c.v.repo(repo))
		}
	case "issues":
		for _, ref := range c.a.searchIssues(query) {
			items = append(items, c.v.issue(ref.repo, ref.issue))
		}
	default:
		return nil
	}
	total := len(items)
	paged, headers := c.page(items)
	if paged == nil {
		paged = []any{}
	}
	res := jsonResponse(200, om("total_count", total, "incomplete_results", false, "items", paged))
	res.Headers = append(res.Headers, headers...)
	return res
}
