package main

// The fast paths are held to the ordinary path: for every request a fast path takes, both must give
// the same bytes (the request id aside), and every request that is not the plain case must be left
// alone.

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "shpat_test_0123456789abcdef"

// coldReply answers one complete raw request the way the event loop's ordinary path does.
func coldReply(t testing.TB, app *App, raw string) []byte {
	t.Helper()
	head, body, found := strings.Cut(raw, "\r\n\r\n")
	if !found {
		t.Fatalf("request has no blank line: %q", raw)
	}
	var req Request
	var fields [24]headerField
	framing, err := parseHead(head, &req, fields[:0])
	if err != nil {
		t.Fatalf("parseHead(%q): %v", head, err)
	}
	if framing.contentLength > 0 {
		req.Body = []byte(body[:framing.contentLength])
	}
	res := answer(app, &req, framing.gzipped)
	return appendResponse(nil, res, framing.keepAlive, req.Method == "HEAD")
}

var requestIDLine = regexp.MustCompile(`X-GitHub-Request-Id: [^\r]*\r\n`)

// sameReply compares two replies with their request ids set aside.
func sameReply(a, b []byte) bool {
	blank := []byte("X-GitHub-Request-Id: -\r\n")
	return bytes.Equal(requestIDLine.ReplaceAll(a, blank), requestIDLine.ReplaceAll(b, blank))
}

func status(reply []byte) string {
	line, _, _ := bytes.Cut(reply, crlf)
	return string(line)
}

func request(method, path, host string, headers []string, body string) string {
	var b strings.Builder
	b.WriteString(method + " " + path + " HTTP/1.1\r\n")
	b.WriteString("host: " + host + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	if body != "" {
		b.WriteString("content-type: application/json\r\ncontent-length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	b.WriteString("\r\n" + body)
	return b.String()
}

var bearer = "authorization: Bearer " + testToken

// forgeWithIssue is a forge holding one repository, widgets, with n issues in it.
func forgeWithIssue(t testing.TB, n int) *App {
	t.Helper()
	app := newApp(testToken, "2026-11-19T00:00:00Z")
	created := coldReply(t, app, request("POST", "/api/v3/user/repos", "forge.test:7100", []string{bearer}, `{"name":"widgets"}`))
	if status(created) != "HTTP/1.1 201 Created" {
		t.Fatalf("creating the repository: %s", created)
	}
	for i := 0; i < n; i++ {
		body := `{"title":"issue ` + strconv.Itoa(i+1) + ` \"quoted\" <tag> & more","body":"line one\nline two\ttabbed é ✓"}`
		made := coldReply(t, app, request("POST", "/api/v3/repos/"+defaultLogin+"/widgets/issues", "forge.test:7100", []string{bearer}, body))
		if status(made) != "HTTP/1.1 201 Created" {
			t.Fatalf("creating issue %d: %s", i+1, made)
		}
	}
	return app
}

func headOf(raw string) []byte {
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	return []byte(head)
}

func TestFastIssueGetMatchesOrdinaryPath(t *testing.T) {
	app := forgeWithIssue(t, 3)
	path := "/api/v3/repos/" + defaultLogin + "/widgets/issues/"
	host := "forge.test:7100"
	plain := [][]string{
		{bearer},
		{"Authorization: Bearer " + testToken},
		{"AUTHORIZATION:   Bearer " + testToken + "  "},
		{"accept: application/vnd.github+json", bearer, "user-agent: node", "accept-language: *", "sec-fetch-mode: cors", "accept-encoding: gzip, deflate"},
		{bearer, "connection: keep-alive"},
		{bearer, "Connection: Keep-Alive"},
		{bearer, "content-length: 0"},
		{bearer, "authorization: Bearer some-other-token"}, // only the first counts, on both paths
		{bearer, "x-no-colon-here", ": empty-name"},
	}
	for _, headers := range plain {
		for _, number := range []string{"1", "2", "3", "03"} {
			raw := request("GET", path+number, host, headers, "")
			fast := app.fastIssueGet(headOf(raw))
			if fast == nil {
				t.Fatalf("the fast path left a plain request alone: %q", raw)
			}
			cold := coldReply(t, app, raw)
			if status(cold) != "HTTP/1.1 200 OK" {
				t.Fatalf("ordinary path: %s", cold)
			}
			if !sameReply(fast, cold) {
				t.Fatalf("replies differ for %q\nfast: %q\ncold: %q", raw, fast, cold)
			}
		}
	}
}

func TestFastIssueGetLeavesEverythingElse(t *testing.T) {
	app := forgeWithIssue(t, 2)
	path := "/api/v3/repos/" + defaultLogin + "/widgets/issues/1"
	host := "forge.test:7100"
	upper := "/api/v3/repos/" + strings.ToUpper(defaultLogin) + "/widgets/issues/1"
	cases := []struct {
		name string
		raw  string
		want string // the ordinary path's status line
	}{
		{"no credential", request("GET", path, host, nil, ""), "HTTP/1.1 401 Unauthorized"},
		{"wrong token", request("GET", path, host, []string{"authorization: Bearer nope"}, ""), "HTTP/1.1 401 Unauthorized"},
		{"token is a prefix", request("GET", path, host, []string{bearer + "x"}, ""), "HTTP/1.1 401 Unauthorized"},
		{"wrong first credential", request("GET", path, host, []string{"authorization: Bearer nope", bearer}, ""), "HTTP/1.1 401 Unauthorized"},
		{"token scheme", request("GET", path, host, []string{"authorization: token " + testToken}, ""), "HTTP/1.1 200 OK"},
		{"two spaces", request("GET", path, host, []string{"authorization: Bearer  " + testToken}, ""), "HTTP/1.1 200 OK"},
		{"connection close", request("GET", path, host, []string{bearer, "connection: close"}, ""), "HTTP/1.1 200 OK"},
		{"query string", request("GET", path+"?per_page=1", host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"trailing slash", request("GET", path+"/", host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"upper-case owner", request("GET", upper, host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"escaped path", request("GET", strings.Replace(path, "widgets", "widget%73", 1), host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"signed number", request("GET", strings.TrimSuffix(path, "1")+"+1", host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"head", request("HEAD", path, host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"no such issue", request("GET", strings.TrimSuffix(path, "1")+"99", host, []string{bearer}, ""), "HTTP/1.1 404 Not Found"},
		{"issue zero", request("GET", strings.TrimSuffix(path, "1")+"0", host, []string{bearer}, ""), "HTTP/1.1 404 Not Found"},
		{"no such repository", request("GET", strings.Replace(path, "widgets", "gadgets", 1), host, []string{bearer}, ""), "HTTP/1.1 404 Not Found"},
		{"another route", request("GET", path+"/comments", host, []string{bearer}, ""), "HTTP/1.1 200 OK"},
		{"two hosts", request("GET", path, host, []string{"host: other.test", bearer}, ""), "HTTP/1.1 200 OK"},
		{"expect", request("GET", path, host, []string{bearer, "expect: 100-continue"}, ""), "HTTP/1.1 200 OK"},
		{"chunked", "GET " + path + " HTTP/1.1\r\nhost: " + host + "\r\n" + bearer + "\r\ntransfer-encoding: identity\r\n\r\n", "HTTP/1.1 200 OK"},
		{"http 1.0", "GET " + path + " HTTP/1.0\r\nhost: " + host + "\r\n" + bearer + "\r\n\r\n", "HTTP/1.1 200 OK"},
	}
	for _, c := range cases {
		if fast := app.fastIssueGet(headOf(c.raw)); fast != nil {
			t.Errorf("%s: the fast path answered %q", c.name, status(fast))
		}
		if got := status(coldReply(t, app, c.raw)); got != c.want {
			t.Errorf("%s: the ordinary path answered %q, want %q", c.name, got, c.want)
		}
	}
	// No token configured: every request belongs to the ordinary path.
	open := newApp("", "2026-11-19T00:00:00Z")
	if fast := open.fastIssueGet(headOf(request("GET", path, host, []string{bearer}, ""))); fast != nil {
		t.Errorf("a forge without a token took the fast path")
	}
}

func TestStoredIssueReplyFollowsTheIssue(t *testing.T) {
	app := forgeWithIssue(t, 1)
	path := "/api/v3/repos/" + defaultLogin + "/widgets/issues/1"
	get := request("GET", path, "forge.test:7100", []string{bearer}, "")
	before := app.fastIssueGet(headOf(get))
	if before == nil {
		t.Fatalf("a new issue has no stored reply")
	}
	// A change drops the stored reply; the ordinary path renders the next one.
	patched := coldReply(t, app, request("PATCH", path, "forge.test:7100", []string{bearer}, `{"state":"closed"}`))
	if status(patched) != "HTTP/1.1 200 OK" {
		t.Fatalf("closing the issue: %s", patched)
	}
	if stale := app.fastIssueGet(headOf(get)); stale != nil {
		t.Fatalf("the stored reply outlived a change to the issue")
	}
	cold := coldReply(t, app, get)
	if !bytes.Contains(cold, []byte(`"state":"closed"`)) {
		t.Fatalf("the ordinary path did not show the change: %s", cold)
	}
	after := app.fastIssueGet(headOf(get))
	if after == nil || !sameReply(after, cold) {
		t.Fatalf("the reply stored after the change is not the ordinary path's\nfast: %q\ncold: %q", after, cold)
	}
	// A reply names the origin it was rendered for; another origin gets its own. The lean issue JSON
	// carries no absolute URL, so both origins' replies agree in their bytes; what the check below
	// pins is that the ordinary path still answered and the stored reply follows it.
	elsewhere := request("GET", path, "127.0.0.1:9", []string{bearer}, "")
	if wrong := app.fastIssueGet(headOf(elsewhere)); wrong != nil {
		t.Fatalf("a reply rendered for one origin answered another")
	}
	other := coldReply(t, app, elsewhere)
	if !bytes.Contains(other, []byte(`"state":"closed"`)) {
		t.Fatalf("the ordinary path did not answer: %s", other)
	}
	if moved := app.fastIssueGet(headOf(elsewhere)); moved == nil || !sameReply(moved, other) {
		t.Fatalf("the reply stored for the new origin is not the ordinary path's")
	}
}

// createFast runs a POST through the create fast path as the event loop does.
func createFast(app *App, raw string) []byte {
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	reply, used := app.fastIssueCreate([]byte(head), []byte(body), nil)
	if reply != nil && used != len(body) {
		panic("the fast path consumed " + strconv.Itoa(used) + " of " + strconv.Itoa(len(body)) + " body bytes")
	}
	return reply
}

func TestFastIssueCreateMatchesOrdinaryPath(t *testing.T) {
	// Two forges fed the same requests: one creates through the fast path, the other through the
	// ordinary path. Every reply, and every issue read back afterwards, must be the same bytes.
	fast, cold := forgeWithIssue(t, 1), forgeWithIssue(t, 1)
	path := "/api/v3/repos/" + defaultLogin + "/widgets/issues"
	host := "forge.test:7100"
	long := strings.Repeat("a long line of body text with \\\"quotes\\\" and a\\nnewline; ", 40)
	taken := []string{
		`{"title":"a","body":"b"}`,
		`{"body":"b first","title":"a second"}`,
		`{"title":"only a title"}`,
		" {\n\t\"title\" : \"spaced out\" ,\r\n \"body\" : \"b\" } \n",
		`{"title":"q\"uote \\ back \/ slash","body":"l1\nl2\ttab\rcr\bbs\fff"}`,
		`{"title":"é ✓ 日本語 🎉","body":"<tag> & 'x' ` + "\x7f" + `"}`,
		`{"title":"  padded  ","body":""}`,
		`{"title":"long","body":"` + long + `"}`,
		`{"title":"a title [bench 17]","body":"## Steps\n\n1. one\n2. two\n\nExpected: it works.\n"}`,
	}
	for _, body := range taken {
		raw := request("POST", path, host, []string{bearer}, body)
		got := createFast(fast, raw)
		if got == nil {
			t.Fatalf("the fast path left a plain create alone: %q", body)
		}
		want := coldReply(t, cold, raw)
		if status(want) != "HTTP/1.1 201 Created" {
			t.Fatalf("ordinary path: %s", want)
		}
		if !sameReply(got, want) {
			t.Fatalf("replies differ for %q\nfast: %q\ncold: %q", body, got, want)
		}
	}
	left := []struct{ body, want string }{
		{`{"title":"a","labels":[]}`, "HTTP/1.1 201 Created"},
		{`{"title":"a","labels":["no-such-label"]}`, "HTTP/1.1 422 Unprocessable Entity"},
		{`{"title":"a","assignees":[]}`, "HTTP/1.1 201 Created"},
		{`{"title":"a","body":null}`, "HTTP/1.1 201 Created"},
		{`{"title":123}`, "HTTP/1.1 201 Created"},
		// A unicode escape, with its backslash put in here so the source spells none out.
		{strings.ReplaceAll(`{"title":"~u00e9 escaped"}`, "~", "\\"), "HTTP/1.1 201 Created"},
		{`{"title":"a","title":"b"}`, "HTTP/1.1 201 Created"},
		{"{\"title\":\"bad utf8 \xff\"}", "HTTP/1.1 201 Created"},
		{`{"title":""}`, "HTTP/1.1 422 Unprocessable Entity"},
		{`{"title":"   "}`, "HTTP/1.1 422 Unprocessable Entity"},
		{`{"body":"no title"}`, "HTTP/1.1 422 Unprocessable Entity"},
		{`{}`, "HTTP/1.1 422 Unprocessable Entity"},
		{`[1]`, "HTTP/1.1 422 Unprocessable Entity"},
		{`{"title":"a"} trailing`, "HTTP/1.1 400 Bad Request"},
		{`{"title":"a",}`, "HTTP/1.1 400 Bad Request"},
		{"{\"title\":\"raw\nnewline\"}", "HTTP/1.1 400 Bad Request"},
		{`{"title":"unterminated`, "HTTP/1.1 400 Bad Request"},
		{`{"title":"bad \x escape"}`, "HTTP/1.1 400 Bad Request"},
		{`not json`, "HTTP/1.1 400 Bad Request"},
	}
	for _, c := range left {
		raw := request("POST", path, host, []string{bearer}, c.body)
		issues, ticks := len(fast.repos[defaultLogin+"/widgets"].Issues), fast.ticks
		if got := createFast(fast, raw); got != nil {
			t.Fatalf("the fast path answered %q with %q", c.body, status(got))
		}
		if len(fast.repos[defaultLogin+"/widgets"].Issues) != issues || fast.ticks != ticks {
			t.Fatalf("the fast path changed the store before leaving %q alone", c.body)
		}
		got, want := coldReply(t, fast, raw), coldReply(t, cold, raw)
		if status(want) != c.want {
			t.Errorf("%q: the ordinary path answered %q, want %q", c.body, status(want), c.want)
		}
		if !sameReply(got, want) {
			t.Fatalf("the two forges diverged at %q\none: %q\nother: %q", c.body, got, want)
		}
	}
	// A request the head rules out is left alone whatever its body.
	plainBody := `{"title":"a","body":"b"}`
	for name, raw := range map[string]string{
		"no credential":    request("POST", path, host, nil, plainBody),
		"wrong token":      request("POST", path, host, []string{"authorization: Bearer nope"}, plainBody),
		"connection close": request("POST", path, host, []string{bearer, "connection: close"}, plainBody),
		"unknown repo":     request("POST", strings.Replace(path, "widgets", "gadgets", 1), host, []string{bearer}, plainBody),
		"query string":     request("POST", path+"?x=1", host, []string{bearer}, plainBody),
		"body not all in":  strings.TrimSuffix(request("POST", path, host, []string{bearer}, plainBody), `"}`),
		"gzip body":        request("POST", path, host, []string{bearer, "content-encoding: gzip"}, plainBody),
	} {
		if got := createFast(fast, raw); got != nil {
			t.Errorf("%s: the fast path answered %q", name, status(got))
		}
	}
	// Both forges now hold the same issues, and each reads back the same from either path.
	total := len(cold.repos[defaultLogin+"/widgets"].Issues)
	if got := len(fast.repos[defaultLogin+"/widgets"].Issues); got != total {
		t.Fatalf("the forges hold %d and %d issues", got, total)
	}
	for n := 1; n <= total; n++ {
		get := request("GET", path+"/"+strconv.Itoa(n), host, []string{bearer}, "")
		want := coldReply(t, cold, get)
		if got := coldReply(t, fast, get); !sameReply(got, want) {
			t.Fatalf("issue %d reads back differently\none: %q\nother: %q", n, got, want)
		}
		if got := fast.fastIssueGet(headOf(get)); got == nil || !sameReply(got, want) {
			t.Fatalf("issue %d has no matching stored reply", n)
		}
	}
}

func TestNewIssueTemplateMatchesRenderer(t *testing.T) {
	app := forgeWithIssue(t, 0)
	repo := app.repos[defaultLogin+"/widgets"]
	v := &view{a: app, base: "http://forge.test:7100"}
	template := v.newIssueTemplate(repo, app.viewer)
	if template == nil {
		t.Fatalf("no template passed its check for an ordinary repository")
	}
	// The lean issue JSON carries four fields that differ from one new issue to the next: id, number,
	// title and body, in that order. (created_at and node_id, whose slots the fuller shape also
	// had, are no longer rendered.)
	if string(template.slots) != "intb" {
		t.Fatalf("the template has slots %q: the probe's markers were not all found, or found more than once", template.slots)
	}
	at := app.base.Add(90 * time.Minute)
	texts := []string{"", "plain", `"`, `\`, "a\nb\r\n\tc", "\x00\x01\x1f\x7f", "é ✓ 日本語 🎉", "<script>&amp;</script>", "922222229 8111111111111111118 2001-02-03T04:05:06Z", "\x02title\x03 \x02body\x03", strings.Repeat("long ", 2000), "bad utf8 \xff\xfe"}
	number := 0
	for _, title := range texts {
		for _, body := range texts {
			number++
			issue := &Issue{ID: int64(1000 + number*7919), Number: number, Title: title, Body: body, HasBody: true, State: "open", User: app.viewer, CreatedAt: at, UpdatedAt: at}
			if !issue.untouched() {
				t.Fatalf("a new issue does not count as untouched")
			}
			want := appendJSON(nil, v.issue(repo, issue))
			if got := template.append(nil, issue); !bytes.Equal(got, want) {
				t.Fatalf("template and renderer differ for title %q body %q\ntemplate: %s\nrenderer: %s", title, body, got, want)
			}
		}
	}
	// The template is cut again when what its constant text names has changed.
	if again := v.newIssueTemplate(repo, app.viewer); again != template {
		t.Fatalf("an unchanged repository had its template cut again")
	}
	elsewhere := &view{a: app, base: "http://127.0.0.1:9"}
	moved := elsewhere.newIssueTemplate(repo, app.viewer)
	if moved == nil || moved == template {
		t.Fatalf("another origin reused the template")
	}
	repo.Name = "gadgets"
	renamed := elsewhere.newIssueTemplate(repo, app.viewer)
	if renamed == nil || renamed == moved {
		t.Fatalf("a renamed repository reused the template")
	}
	issue := &Issue{ID: 5, Number: 6, Title: "t", Body: "b", HasBody: true, State: "open", User: app.viewer, CreatedAt: at, UpdatedAt: at}
	if got, want := renamed.append(nil, issue), appendJSON(nil, elsewhere.issue(repo, issue)); !bytes.Equal(got, want) {
		t.Fatalf("the recut template differs from the renderer\ntemplate: %s\nrenderer: %s", got, want)
	}
	// Anything that has happened to an issue takes it out of the template's reach.
	touched := []func(i *Issue){
		func(i *Issue) { i.State = "closed" },
		func(i *Issue) { i.StateReason = "completed" },
		func(i *Issue) { i.Labels = []*Label{{Name: "bug"}} },
		func(i *Issue) { i.Assignees = []*User{app.viewer} },
		func(i *Issue) { i.Comments = []*Comment{{Body: "c"}} },
		func(i *Issue) { i.Locked = true },
		func(i *Issue) { i.ClosedAt = at },
		func(i *Issue) { i.UpdatedAt = at.Add(time.Second) },
		func(i *Issue) { i.Pull = &Pull{} },
	}
	for n, touch := range touched {
		changed := &Issue{ID: 5, Number: 6, Title: "t", State: "open", User: app.viewer, CreatedAt: at, UpdatedAt: at}
		if touch(changed); changed.untouched() {
			t.Errorf("change %d left the issue looking untouched", n)
		}
	}
}

func TestRequestIDShape(t *testing.T) {
	for _, n := range []uint64{0, 1, 9, 10, 255, 4096, 65535, 1 << 20, 1<<36 - 1, 1 << 36, 1<<63 + 12345, 1<<64 - 0x1000000001} {
		hex := strconv.FormatUint(0x1000000000+n, 16)
		want := "F0E1:" + strings.ToUpper(hex[:5]) + ":" + strings.ToUpper(hex[5:]) + ":" + strconv.FormatUint(n, 10)
		if got := requestID(n); got != want {
			t.Errorf("requestID(%d) = %q, want %q", n, got, want)
		}
	}
}

// suiteCreate is an issue create shaped like the write workload's.
func suiteCreate(n int) string {
	body := `{"title":"Intermittent 502 from the artifact proxy under load [bench ` + strconv.Itoa(n) + `]","body":"## What happened\n\nUploads over 64 MiB fail about one time in five.\n\n## Steps\n\n1. Push a large artifact.\n2. Watch the proxy log.\n\n## Expected\n\nThe upload completes, or the client is told to retry.\n"}`
	return request("POST", "/api/v3/repos/"+defaultLogin+"/widgets/issues", "127.0.0.1:44165", []string{
		"connection: keep-alive",
		"accept: application/vnd.github+json",
		"user-agent: makefun-suite",
		bearer,
		"accept-language: *",
		"sec-fetch-mode: cors",
		"accept-encoding: gzip, deflate",
	}, body)
}

// benchCreates times one create per iteration, starting a fresh forge every few thousand so the
// store's size stays what a trial's is.
func benchCreates(b *testing.B, create func(app *App, raw string, head, body []byte)) {
	const batch = 2000
	raws := make([]string, batch)
	heads, bodies := make([][]byte, batch), make([][]byte, batch)
	for i := range raws {
		raws[i] = suiteCreate(i + 1)
		head, body, _ := strings.Cut(raws[i], "\r\n\r\n")
		heads[i], bodies[i] = []byte(head), []byte(body)
	}
	var app *App
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%batch == 0 {
			b.StopTimer()
			app = forgeWithIssue(b, 0)
			b.StartTimer()
		}
		create(app, raws[i%batch], heads[i%batch], bodies[i%batch])
	}
}

func BenchmarkIssueCreateFast(b *testing.B) {
	var out []byte
	benchCreates(b, func(app *App, raw string, head, body []byte) {
		reply, _ := app.fastIssueCreate(head, body, out[:0])
		if reply == nil {
			b.Fatal("the fast path stood aside")
		}
		out = reply
	})
}

func BenchmarkIssueCreateOrdinary(b *testing.B) {
	var req Request
	var fields [24]headerField
	var out []byte
	benchCreates(b, func(app *App, raw string, head, body []byte) {
		framing, err := parseHead(string(head), &req, fields[:0])
		if err != nil {
			b.Fatal(err)
		}
		req.Body = body
		out = appendResponse(out[:0], answer(app, &req, framing.gzipped), framing.keepAlive, false)
	})
}

func BenchmarkIssueGetFast(b *testing.B) {
	app := forgeWithIssue(b, 1)
	coldReply(b, app, suiteGet()) // stores the reply for the origin the benchmark dials
	head := headOf(suiteGet())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if app.fastIssueGet(head) == nil {
			b.Fatal("the fast path stood aside")
		}
	}
}

func BenchmarkIssueGetOrdinary(b *testing.B) {
	app := forgeWithIssue(b, 1)
	raw := []byte(suiteGet())
	headEnd := bytes.Index(raw, headTerminator)
	var req Request
	var fields [24]headerField
	var out []byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		framing, err := parseHead(string(raw[:headEnd]), &req, fields[:0])
		if err != nil {
			b.Fatal(err)
		}
		out = appendResponse(out[:0], answer(app, &req, framing.gzipped), framing.keepAlive, false)
	}
}

// suiteGet is a GET of an issue with the header lines the suite's client sends.
func suiteGet() string {
	return request("GET", "/api/v3/repos/"+defaultLogin+"/widgets/issues/1", "127.0.0.1:44165", []string{
		"connection: keep-alive",
		"accept: application/vnd.github+json",
		"user-agent: makefun-suite",
		bearer,
		"accept-language: *",
		"sec-fetch-mode: cors",
		"accept-encoding: gzip, deflate",
	}, "")
}
