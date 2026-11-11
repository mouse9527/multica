package githubissues

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// fakeWrites is the single-item part of fakeIssuesAPI: create, read, edit
// and delete of one issue or comment, with one-shot fault injection. Items
// it writes land in the same maps the lists serve, so a poll sees them.
type fakeWrites struct {
	nextNumber    int
	nextCommentID int64
	// fault, when set, is applied to the next non-GET request and cleared.
	fault *writeFault
	// sent counts non-GET requests by "METHOD path".
	sent map[string]int
	// bodies are the JSON bodies of non-GET requests, in order.
	bodies []map[string]any
	// now stamps updated_at of written items. It is a copy of the test
	// clock set under mu (setNow): the race detector cannot see the HTTP
	// round trip as synchronization.
	now time.Time
	// author is the login written items get: the token's account.
	author string
}

// writeFault is how a write request fails.
type writeFault struct {
	// kind: "500", "429", "disconnect" or "timeout".
	kind string
	// apply makes GitHub carry out the write before failing, the case that
	// makes a result unknown.
	apply bool
}

var (
	itemIssuePath    = regexp.MustCompile(fmt.Sprintf(`^/repositories/%d/issues/(\d+)$`, pollRepoID))
	itemCommentsPath = regexp.MustCompile(fmt.Sprintf(`^/repositories/%d/issues/(\d+)/comments$`, pollRepoID))
	itemCommentPath  = regexp.MustCompile(fmt.Sprintf(`^/repositories/%d/issues/comments/(\d+)$`, pollRepoID))
	issuesPath       = fmt.Sprintf("/repositories/%d/issues", pollRepoID)
)

func (f *fakeIssuesAPI) setNow(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes.now = now
}

func (f *fakeIssuesAPI) failNext(kind string, apply bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes.fault = &writeFault{kind: kind, apply: apply}
}

func (f *fakeIssuesAPI) sentCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes.sent[key]
}

func (f *fakeIssuesAPI) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes.bodies) == 0 {
		return nil
	}
	return f.writes.bodies[len(f.writes.bodies)-1]
}

func (f *fakeIssuesAPI) commentBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		out = append(out, c["body"].(string))
	}
	sort.Strings(out)
	return out
}

// serveItem answers the single-item routes and reports whether it did. The
// lists (GET /issues, GET /issues/comments) are left to serve.
func (f *fakeIssuesAPI) serveItem(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	isList := r.Method == http.MethodGet && (path == issuesPath || path == issuesPath+"/comments")
	isItem := itemIssuePath.MatchString(path) || itemCommentsPath.MatchString(path) || itemCommentPath.MatchString(path) ||
		(r.Method == http.MethodPost && path == issuesPath)
	if isList || !isItem {
		return false
	}

	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+path+"?"+r.URL.RawQuery)
	var body map[string]any
	var fault *writeFault
	if r.Method != http.MethodGet {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.writes.sent == nil {
			f.writes.sent = map[string]int{}
		}
		f.writes.sent[r.Method+" "+path]++
		f.writes.bodies = append(f.writes.bodies, body)
		fault, f.writes.fault = f.writes.fault, nil
	}
	status, out := http.StatusOK, any(nil)
	if fault == nil || fault.apply {
		status, out = f.applyItem(r, path, body)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if fault != nil {
		switch fault.kind {
		case "500":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
		case "429":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		case "disconnect":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case "timeout":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		return true
	}
	w.WriteHeader(status)
	if out != nil {
		_ = json.NewEncoder(w).Encode(out)
	}
	return true
}

// applyItem carries out one single-item request on the stored items. The
// caller holds f.mu.
func (f *fakeIssuesAPI) applyItem(r *http.Request, path string, body map[string]any) (int, any) {
	stamp := f.writes.now.UTC().Format(time.RFC3339)
	author := f.writes.author
	switch {
	case r.Method == http.MethodPost && path == issuesPath:
		f.writes.nextNumber++
		n := 900 + f.writes.nextNumber
		item := map[string]any{
			"id": 1000 + n, "node_id": "I_" + strconv.Itoa(n), "number": n,
			"title": body["title"], "body": body["body"], "state": "open", "state_reason": nil,
			"labels": labelObjects(body["labels"]), "updated_at": stamp, "user": map[string]any{"login": author},
		}
		f.issues[n] = item
		return http.StatusCreated, item
	case itemIssuePath.MatchString(path):
		n, _ := strconv.Atoi(itemIssuePath.FindStringSubmatch(path)[1])
		item, ok := f.issues[n]
		if !ok {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		if r.Method == http.MethodPatch {
			for _, key := range []string{"title", "body", "state", "state_reason"} {
				if v, set := body[key]; set {
					item[key] = v
				}
			}
			if state, set := body["state"]; set && state == "open" {
				item["state_reason"] = "reopened"
			}
			if labels, set := body["labels"]; set {
				item["labels"] = labelObjects(labels)
			}
			item["updated_at"] = stamp
		}
		return http.StatusOK, item
	case itemCommentsPath.MatchString(path):
		n, _ := strconv.Atoi(itemCommentsPath.FindStringSubmatch(path)[1])
		if r.Method == http.MethodGet {
			var items []map[string]any
			for _, c := range f.comments {
				if c["issue_url"] == fmt.Sprintf("https://api.github.com/repos/acme/widgets/issues/%d", n) {
					items = append(items, c)
				}
			}
			sort.Slice(items, func(i, j int) bool { return fmt.Sprint(items[i]["id"]) < fmt.Sprint(items[j]["id"]) })
			return http.StatusOK, items
		}
		f.writes.nextCommentID++
		id := 7000 + f.writes.nextCommentID
		item := map[string]any{
			"id": id, "body": body["body"], "updated_at": stamp,
			"issue_url": fmt.Sprintf("https://api.github.com/repos/acme/widgets/issues/%d", n),
			"user":      map[string]any{"login": author, "avatar_url": "https://avatars.example.com/" + author},
		}
		f.comments[id] = item
		return http.StatusCreated, item
	case itemCommentPath.MatchString(path):
		id, _ := strconv.ParseInt(itemCommentPath.FindStringSubmatch(path)[1], 10, 64)
		item, ok := f.comments[id]
		if !ok {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		switch r.Method {
		case http.MethodPatch:
			item["body"] = body["body"]
			item["updated_at"] = stamp
		case http.MethodDelete:
			delete(f.comments, id)
			return http.StatusNoContent, nil
		}
		return http.StatusOK, item
	}
	return http.StatusNotFound, map[string]any{"message": "Not Found"}
}

func labelObjects(v any) []map[string]any {
	out := []map[string]any{}
	names, _ := v.([]any)
	for _, name := range names {
		out = append(out, map[string]any{"name": name})
	}
	return out
}
