package githubissues

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// repoName is a GitHub repository name as "owner/name". GitHub compares
// names case-insensitively, so matching uses sameAs.
type repoName struct {
	owner string
	name  string
}

func (n repoName) String() string { return n.owner + "/" + n.name }

func (n repoName) sameAs(other repoName) bool {
	return strings.EqualFold(n.owner, other.owner) && strings.EqualFold(n.name, other.name)
}

var (
	// GitHub logins: alphanumerics and single inner hyphens, up to 39 chars.
	// Older accounts may carry a trailing hyphen, so only the shape matters.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// newRepoName validates owner and name. The check keeps anything that is not
// a plain GitHub name (slashes, dot segments, escapes) out of request paths.
func newRepoName(owner, name string) (repoName, bool) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !ownerPattern.MatchString(owner) || !repoPattern.MatchString(name) || name == "." || name == ".." {
		return repoName{}, false
	}
	return repoName{owner: owner, name: name}, true
}

// repoNameFromResourceRef reads the repository a project_resource of type
// github_repo points at. Only github.com URLs count: the token is for
// github.com, and a resource on any other host never enters the allowlist.
// Accepted forms are the ones GitHub's "Code" menu offers, with or without
// ".git" and a trailing slash:
//
//	https://github.com/owner/name
//	ssh://git@github.com/owner/name.git
//	git@github.com:owner/name.git
func repoNameFromResourceRef(ref []byte) (repoName, bool) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(ref, &payload); err != nil {
		return repoName{}, false
	}
	raw := strings.TrimSpace(payload.URL)

	var host, path string
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "ssh", "git":
		default:
			return repoName{}, false
		}
		if u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
			return repoName{}, false
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp-like shorthand: [user@]host:owner/name
		if strings.Contains(raw, "://") || strings.ContainsAny(raw, " ?#") {
			return repoName{}, false
		}
		hostPart, rest, ok := strings.Cut(raw, ":")
		if !ok {
			return repoName{}, false
		}
		if _, h, hasUser := strings.Cut(hostPart, "@"); hasUser {
			hostPart = h
		}
		host, path = hostPart, rest
	}
	if !strings.EqualFold(host, "github.com") && !strings.EqualFold(host, "www.github.com") {
		return repoName{}, false
	}

	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(name, "/") {
		return repoName{}, false
	}
	return newRepoName(owner, name)
}
