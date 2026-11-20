package main

// The fast paths: requests the suite sends by the thousand, answered from the bytes of the request
// head without building a Request, routing it or assembling a reply.
//
//	GET  /api/v3/repos/{owner}/{repo}/issues/{number}   the issue's stored reply, written as it stands
//	POST /api/v3/repos/{owner}/{repo}/issues            a new issue from a body of just title and body
//	GET  /{owner}/{repo}.git/info/refs?service=git-receive-pack   the advertisement of a one-ref repository
//
// A fast path takes a request only when it is the plain case in every respect: exactly that route,
// HTTP/1.1 on a connection that stays open, an Authorization header that is exactly "Bearer <the
// trial's token>", and nothing about the head that changes how the request is framed. Everything
// else is left untouched for the ordinary path: another route, a missing or different credential, a
// query string, an escaped or differently cased path, Connection: close, an issue that has no stored
// reply, a body with any other field or any JSON the small reader here does not take. So the
// ordinary path still answers every 401, 404 and 422 and every odd request there is, and a request
// either path could take gets the same bytes from both (fastpath_test.go holds them to it).

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

var crlf = []byte("\r\n")

// plainHead is what a fast path needs from a request's header lines.
type plainHead struct {
	host          []byte // the Host value
	contentLength int    // 0 when the head names none
}

// scanPlainHead reads a request's header lines (everything after the request line) as parseHead
// would, and reports ok only for a head a fast path may answer: its first Authorization header is
// exactly the trial's bearer credential, it has one Host, and it carries nothing that changes the
// request's framing or closes the connection.
func (a *App) scanPlainHead(lines []byte) (h plainHead, ok bool) {
	if a.bearer == nil {
		return h, false
	}
	authorized, sawAuthorization, sawLength := false, false, false
	for len(lines) > 0 {
		var line []byte
		if end := bytes.Index(lines, crlf); end >= 0 {
			line, lines = lines[:end], lines[end+2:]
		} else {
			line, lines = lines, nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := line[:colon]
		switch len(name) {
		case 4:
			if equalFoldASCII(name, "host") {
				if h.host != nil {
					return h, false
				}
				if h.host = bytes.TrimSpace(line[colon+1:]); len(h.host) == 0 {
					return h, false
				}
			}
		case 6:
			if equalFoldASCII(name, "expect") {
				return h, false
			}
		case 10:
			if equalFoldASCII(name, "connection") && !equalFoldASCII(bytes.TrimSpace(line[colon+1:]), "keep-alive") {
				return h, false
			}
		case 13:
			// Only the first Authorization header counts, as it does for the ordinary path.
			if equalFoldASCII(name, "authorization") && !sawAuthorization {
				sawAuthorization = true
				if authorized = bytes.Equal(bytes.TrimSpace(line[colon+1:]), a.bearer); !authorized {
					return h, false
				}
			}
		case 14:
			if equalFoldASCII(name, "content-length") {
				value := bytes.TrimSpace(line[colon+1:])
				if sawLength || len(value) == 0 || len(value) > 7 {
					return h, false
				}
				sawLength = true
				for _, d := range value {
					if d < '0' || d > '9' {
						return h, false
					}
					h.contentLength = h.contentLength*10 + int(d-'0')
				}
			}
		case 16:
			if equalFoldASCII(name, "content-encoding") {
				return h, false
			}
		case 17:
			if equalFoldASCII(name, "transfer-encoding") {
				return h, false
			}
		}
	}
	return h, authorized && h.host != nil
}

// equalFoldASCII reports whether b is the lower-case ASCII word, whatever b's own letter case.
func equalFoldASCII(b []byte, lower string) bool {
	if len(b) != len(lower) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// requestLine splits a request head into the target of its request line and its header lines, for a
// head that opens with prefix (the method, a space and the route's first segments) and whose request
// line closes with " HTTP/1.1". target is what lies between the two.
func requestLine(head []byte, prefix string) (target, lines []byte, ok bool) {
	const proto = " HTTP/1.1"
	if len(head) < len(prefix) || string(head[:len(prefix)]) != prefix {
		return nil, nil, false
	}
	end := bytes.Index(head, crlf)
	if end < 0 {
		return nil, nil, false
	}
	line := head[len(prefix):end]
	if len(line) < len(proto) || string(line[len(line)-len(proto):]) != proto {
		return nil, nil, false
	}
	return line[:len(line)-len(proto)], head[end+2:], true
}

// fastIssueGet answers GET /api/v3/repos/{owner}/{repo}/issues/{number} with the issue's stored
// reply. head is the request head without the blank line that ends it. A nil result leaves the
// request to the ordinary path.
func (a *App) fastIssueGet(head []byte) []byte {
	target, lines, ok := requestLine(head, "GET /api/v3/repos/")
	if !ok {
		return nil
	}
	// target is {owner}/{repo}/issues/{number}. The repository is looked up by the bytes as sent:
	// the store's keys are lower-case "owner/name", so a path in any other spelling simply misses.
	slash := bytes.LastIndexByte(target, '/')
	if slash < 0 {
		return nil
	}
	digits := target[slash+1:]
	if len(digits) == 0 || len(digits) > 9 {
		return nil
	}
	number := 0
	for _, d := range digits {
		if d < '0' || d > '9' {
			return nil
		}
		number = number*10 + int(d-'0')
	}
	const issues = "/issues"
	rest := target[:slash]
	if len(rest) <= len(issues) || string(rest[len(rest)-len(issues):]) != issues {
		return nil
	}
	key := rest[:len(rest)-len(issues)]
	h, ok := a.scanPlainHead(lines)
	if !ok || h.contentLength != 0 {
		return nil
	}
	const scheme = "http://"
	var wire []byte
	a.mu.RLock()
	if repo := a.repos[string(key)]; repo != nil {
		if issue := repo.issue(number); issue != nil {
			if reply := issue.cached.Load(); reply != nil && len(reply.origin) == len(scheme)+len(h.host) && reply.origin[len(scheme):] == string(h.host) {
				wire = reply.wire
			}
		}
	}
	a.mu.RUnlock()
	if wire != nil {
		a.requests.Add(1)
	}
	return wire
}

// fastIssueCreate answers POST /api/v3/repos/{owner}/{repo}/issues when the body is a JSON object
// of a string title and, optionally, a string body, and nothing else. head is the request head
// without its blank line; rest is what the connection holds after it, which must already include
// the whole body. On success the 201 reply is appended to out and returned with the number of body
// bytes it consumed. A nil reply leaves the request to the ordinary path, with nothing changed.
func (a *App) fastIssueCreate(head, rest, out []byte) (reply []byte, consumed int) {
	target, lines, ok := requestLine(head, "POST /api/v3/repos/")
	if !ok {
		return nil, 0
	}
	const issues = "/issues"
	if len(target) <= len(issues) || string(target[len(target)-len(issues):]) != issues {
		return nil, 0
	}
	key := target[:len(target)-len(issues)]
	h, ok := a.scanPlainHead(lines)
	if !ok || h.contentLength == 0 || h.contentLength > len(rest) {
		return nil, 0
	}
	// Everything that could refuse the request is settled before the store is touched: a body this
	// reader takes and a title that is not blank leave createIssue nothing to refuse.
	in, ok := readIssueFields(rest[:h.contentLength])
	if !ok || strings.TrimSpace(in.Title) == "" {
		return nil, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	repo := a.repos[string(key)]
	if repo == nil {
		return nil, 0
	}
	issue, failure := a.createIssue(repo, in)
	if failure != nil {
		return nil, 0
	}
	const scheme = "http://"
	v := a.fastView
	if v == nil || len(v.base) != len(scheme)+len(h.host) || v.base[len(scheme):] != string(h.host) {
		v = &view{a: a, base: scheme + string(h.host)}
		a.fastView = v
	}
	id := requestID(a.requests.Add(1))
	var template *issueTemplate
	if issue.untouched() {
		template = v.newIssueTemplate(repo, issue.User)
	}
	stored := v.storeIssueReplyFrom(repo, issue, id, template)
	headers := [4]string{"Location", v.issueLocation(repo, issue), "X-GitHub-Request-Id", id}
	out = appendReplyHead(out, 201, jsonType, headers[:], len(stored.json()), true)
	return append(out, stored.json()...), h.contentLength
}

// fastReceiveAdvertisement answers GET /{owner}/{repo}.git/info/refs?service=git-receive-pack for a
// repository with exactly one ref: what a client pushing to a single branch asks before every push.
// The reply is the advertisement's constant opening, that one ref with the capabilities, and a
// flush, appended to out. A nil result leaves the request to the ordinary path.
func (a *App) fastReceiveAdvertisement(head, out []byte) []byte {
	const suffix = ".git/info/refs?service=git-receive-pack"
	target, lines, ok := requestLine(head, "GET /")
	if !ok || len(target) <= len(suffix) || string(target[len(target)-len(suffix):]) != suffix {
		return nil
	}
	// The ordinary path cuts the repository's path at the first ".git/"; a key holding one is not
	// the plain case.
	key := target[:len(target)-len(suffix)]
	if bytes.Contains(key, []byte(".git/")) {
		return nil
	}
	h, ok := a.scanPlainHead(lines)
	if !ok || h.contentLength != 0 {
		return nil
	}
	a.mu.RLock()
	repo := a.repos[string(key)]
	a.mu.RUnlock()
	if repo == nil {
		return nil
	}
	var name string
	var sha SHA
	repo.refMu.RLock()
	if len(repo.refs) == 1 {
		for only, at := range repo.refs {
			name, sha = only, at
		}
	}
	repo.refMu.RUnlock()
	if name == "" {
		return nil
	}
	headers := [4]string{"Cache-Control", "no-cache", "X-GitHub-Request-Id", requestID(a.requests.Add(1))}
	bodyLen := len(receivePackPreamble) + advertisedRefLen(name, receivePackCaps) + 4
	out = appendReplyHead(out, 200, receivePackAdvertisement, headers[:], bodyLen, true)
	out = append(out, receivePackPreamble...)
	out = appendAdvertisedRef(out, sha, name, receivePackCaps)
	return pktFlush(out)
}

// fast runs a request through the fast paths. head is the request head without its blank line, rest
// what the connection holds after it, and scratch a buffer a reply may be built in. It returns the
// reply and how many bytes of rest (the request's body) it consumed, or nil when no fast path took
// the request.
func (a *App) fast(head, rest []byte, scratch *[]byte) (reply []byte, consumed int) {
	if len(head) == 0 {
		return nil, 0
	}
	switch head[0] {
	case 'G':
		if reply = a.fastIssueGet(head); reply != nil {
			return reply, 0
		}
		if reply = a.fastReceiveAdvertisement(head, (*scratch)[:0]); reply != nil {
			*scratch = reply
			return reply, 0
		}
	case 'P':
		if reply, consumed = a.fastIssueCreate(head, rest, (*scratch)[:0]); reply != nil {
			*scratch = reply
			return reply, consumed
		}
	}
	return nil, 0
}

// readIssueFields reads a JSON object that holds a string "title" and, optionally, a string "body",
// in either order, and nothing else. ok is false for any other JSON, well formed or not: other or
// repeated keys, values that are not strings, \u escapes, text that is not valid UTF-8. The ordinary
// path's decoder reads all of those; for the objects taken here the two readers agree.
func readIssueFields(data []byte) (in issueInput, ok bool) {
	i := skipJSONSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return in, false
	}
	i = skipJSONSpace(data, i+1)
	sawTitle := false
	for {
		if i >= len(data) || data[i] != '"' {
			return in, false
		}
		keyEnd := bytes.IndexByte(data[i+1:], '"')
		if keyEnd < 0 {
			return in, false
		}
		key := data[i+1 : i+1+keyEnd]
		i = skipJSONSpace(data, i+keyEnd+2)
		if i >= len(data) || data[i] != ':' {
			return in, false
		}
		value, next, ok := readJSONString(data, skipJSONSpace(data, i+1))
		if !ok {
			return in, false
		}
		switch string(key) {
		case "title":
			if sawTitle {
				return in, false
			}
			sawTitle, in.Title = true, value
		case "body":
			if in.HasBody {
				return in, false
			}
			in.HasBody, in.Body = true, value
		default:
			return in, false
		}
		i = skipJSONSpace(data, next)
		if i >= len(data) {
			return in, false
		}
		if data[i] == ',' {
			i = skipJSONSpace(data, i+1)
			continue
		}
		if data[i] != '}' {
			return in, false
		}
		break
	}
	if skipJSONSpace(data, i+1) != len(data) || !sawTitle {
		return in, false
	}
	return in, true
}

// skipJSONSpace returns the index of the first byte at or after i that is not JSON whitespace.
func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\r' || data[i] == '\t') {
		i++
	}
	return i
}

// readJSONString reads the JSON string that opens at data[i] and returns its text and the index
// just past its closing quote. It takes the two-character escapes and stands aside (ok false) for
// \u escapes, raw control characters and text that is not valid UTF-8.
func readJSONString(data []byte, i int) (text string, next int, ok bool) {
	if i >= len(data) || data[i] != '"' {
		return "", 0, false
	}
	start := i + 1
	end, escaped := start, false
	for ; end < len(data) && data[end] != '"'; end++ {
		c := data[end]
		if c < 0x20 {
			return "", 0, false
		}
		if c == '\\' {
			escaped = true
			end++
			if end >= len(data) || !strings.ContainsRune(`"\/bfnrt`, rune(data[end])) {
				return "", 0, false
			}
		}
	}
	if end >= len(data) {
		return "", 0, false
	}
	raw := data[start:end]
	if !utf8.Valid(raw) {
		return "", 0, false
	}
	if !escaped {
		return string(raw), end + 1, true
	}
	var b strings.Builder
	b.Grow(len(raw))
	for k := 0; k < len(raw); k++ {
		c := raw[k]
		if c == '\\' {
			k++
			switch raw[k] {
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			default:
				c = raw[k] // a quote, a backslash or a slash stands for itself
			}
		}
		b.WriteByte(c)
	}
	return b.String(), end + 1, true
}
