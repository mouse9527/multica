// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/multica-ai/multica/server/internal/crossread"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A task token normally acts only in the workspace it is bound to (GitHub
// #9403, MUL-2600). When the cross_workspace_read flag is on and another
// workspace (the target) granted the token's workspace (the source) read
// access, a request that names the target is served instead as a synthetic
// reader:
//
//   - only the routes on crossWorkspaceReadRoutes, deny by default;
//   - X-User-ID is the grant id, which is never a real user: per-user data
//     (inbox, subscriptions, chat) is empty, private projects are hidden, and
//     secret-bearing fields stay redacted because the reader is neither an
//     owner/admin nor the owner of anything;
//   - X-Agent-ID and X-Task-ID are left unset: they name the source
//     workspace's agent and run, which mean nothing in the target;
//   - X-Actor-Source is cross_ws_reader, a machine credential for
//     RequireHumanActor;
//   - every request a grant matched is audited before it is served, and a
//     failed audit write refuses the request.
//
// Without a live grant nothing changes: the upstream workspace check refuses
// the request as before.

// ActorSourceCrossWorkspaceReader is the X-Actor-Source of a request served
// under a cross-workspace read grant.
const ActorSourceCrossWorkspaceReader = "cross_ws_reader"

// RoleCrossWorkspaceReader is the role of the synthetic member such a request
// runs as. No route or handler grants anything to this role.
const RoleCrossWorkspaceReader = "cross_ws_reader"

const errCrossWorkspaceReadOnly = "cross-workspace read grant allows read-only routes only"

// CrossWorkspaceReadStore is what Auth needs from crossread.Store.
type CrossWorkspaceReadStore interface {
	Enabled(ctx context.Context) bool
	ActiveGrant(ctx context.Context, targetWorkspaceID, sourceWorkspaceID, agentID string) (crossread.Grant, bool, error)
	RecordAudit(ctx context.Context, entry crossread.AuditEntry) error
}

type crossReadContextKey int

const (
	ctxKeyCrossReadStore crossReadContextKey = iota
	ctxKeyCrossReadGrant
)

// WithCrossWorkspaceRead hands store to the Auth middleware mounted after it.
// Route groups without it keep the upstream behavior.
func WithCrossWorkspaceRead(store CrossWorkspaceReadStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyCrossReadStore, store))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// crossWorkspaceReadRoutes is the reader's allowlist: reads only, and only
// routes behind the workspace membership middleware, so every one of them
// runs as the synthetic member. Some reads are POST (filter sets too large
// for a query string). Chat, inbox, environments and their logs, agent env,
// MCP and app configuration, autopilot deliveries and every write stay out.
var crossWorkspaceReadRoutes = compileGuestRoutes([]string{
	"GET /api/workspaces/*",
	"GET /api/workspaces/*/members",

	"GET /api/issues",
	"POST /api/issues/query",
	"POST /api/issues/table/groups",
	"POST /api/issues/table/rows",
	"POST /api/issues/table/facets",
	"GET /api/issues/search",
	"GET /api/issues/child-progress",
	"GET /api/issues/children",
	"GET /api/issues/grouped",
	"GET /api/issues/pull-requests",
	"GET /api/issues/*",
	"GET /api/issues/*/comments",
	"GET /api/issues/*/timeline",
	"GET /api/issues/*/subscribers",
	"GET /api/issues/*/attachments",
	"GET /api/issues/*/children",
	"GET /api/issues/*/duplicates",
	"GET /api/issues/*/labels",
	"GET /api/issues/*/metadata",
	"GET /api/issues/*/pull-requests",
	"GET /api/issues/*/active-task",
	"GET /api/issues/*/task-runs",
	"GET /api/issues/*/usage",
	"GET /api/issues/*/wakeups",
	"GET /api/issues/*/wakeups/*/runs",
	"GET /api/issues/*/system-wakeups",

	// Run transcripts; a chat run's stays with the chat's creator.
	"GET /api/tasks/*/messages",
	"GET /api/tasks/*/context-receipt",

	"GET /api/projects",
	"GET /api/projects/search",
	"GET /api/projects/*",
	"GET /api/projects/*/progress",
	"GET /api/projects/*/activity",
	"GET /api/projects/*/members",
	"GET /api/projects/*/resources",

	"GET /api/docs",
	"GET /api/docs/search",
	"GET /api/docs/*",
	"GET /api/docs/*/versions",
	"GET /api/docs/*/versions/*",
	"GET /api/docs/*/task-runs",
	"GET /api/docs/*/comments",

	"GET /api/labels",
	"GET /api/labels/*",
	"GET /api/issue-statuses",
	"GET /api/issue-workflows",
	"GET /api/issue-workflows/*",
	"GET /api/properties",
	"GET /api/properties/*",

	"GET /api/agents",
	"GET /api/agents/*",
	"GET /api/agents/*/skills",
	"GET /api/agents/*/labels",
	"GET /api/agents/*/tasks",

	"GET /api/skills",
	"GET /api/skills/search",
	"GET /api/skills/*",
	"GET /api/skills/*/labels",
	"GET /api/skills/*/files",
	"GET /api/skills/*/versions",
	"GET /api/skills/*/versions/*",

	"GET /api/squads",
	"GET /api/squads/*",
	"GET /api/squads/*/members",
	"GET /api/squads/*/briefs",

	"GET /api/autopilots",
	"GET /api/autopilots/*",
	"GET /api/autopilots/*/runs",
	"GET /api/autopilots/*/runs/*",

	"GET /api/attachments/*",
	"GET /api/attachments/*/content",
})

// matchCrossWorkspaceReadRoute returns the allowlist pattern a request
// matches; the pattern is what the audit records as its route.
func matchCrossWorkspaceReadRoute(r *http.Request) (string, bool) {
	// Match the path chi routes on: the escaped form when the request has
	// one, so an encoded "/" cannot make the allowlist and the router see
	// different segments.
	path := r.URL.Path
	if r.URL.RawPath != "" {
		path = r.URL.RawPath
	}
	return CrossWorkspaceReadRoute(r.Method, path)
}

// CrossWorkspaceReadRoute returns the allowlist pattern that admits method on
// path (as chi routes it), if any. Exported for the router-wide test.
func CrossWorkspaceReadRoute(method, path string) (string, bool) {
	segments := splitPath(path)
	for _, route := range crossWorkspaceReadRoutes {
		if route.matches(method, segments) {
			return route.pattern(), true
		}
	}
	return "", false
}

// CrossWorkspaceReadPatterns returns the allowlist as written.
func CrossWorkspaceReadPatterns() []string {
	out := make([]string, 0, len(crossWorkspaceReadRoutes))
	for _, route := range crossWorkspaceReadRoutes {
		out = append(out, route.pattern())
	}
	return out
}

func (route guestRoute) pattern() string {
	out := route.method + " "
	for _, s := range route.segments {
		out += "/" + s
	}
	return out
}

// crossWorkspaceReadTarget returns the one workspace every workspace selector
// on the request names, when that is not the bound workspace. Selectors that
// are malformed, unknown or disagree return ok=false and leave the request to
// checkTaskTokenWorkspaceTarget, which refuses it exactly as upstream does.
func crossWorkspaceReadTarget(r *http.Request, queries *db.Queries, boundWorkspaceID string) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", false
	}
	var targets []string
	for _, values := range [][]string{r.Header.Values("X-Workspace-ID"), query["workspace_id"]} {
		for _, v := range values {
			if v == "" {
				continue
			}
			id, err := util.ParseUUID(v)
			if err != nil {
				return "", false
			}
			targets = append(targets, util.UUIDToString(id))
		}
	}
	// /api/workspaces/{id}/... names its workspace in the path.
	if segments := splitPath(r.URL.Path); len(segments) >= 3 && segments[0] == "api" && segments[1] == "workspaces" {
		if id, err := util.ParseUUID(segments[2]); err == nil {
			targets = append(targets, util.UUIDToString(id))
		}
	}
	for _, values := range [][]string{r.Header.Values("X-Workspace-Slug"), query["workspace_slug"]} {
		for _, slug := range values {
			if slug == "" {
				continue
			}
			if queries == nil {
				return "", false
			}
			ws, err := queries.GetWorkspaceBySlug(r.Context(), slug)
			if err != nil {
				return "", false
			}
			targets = append(targets, util.UUIDToString(ws.ID))
		}
	}
	if len(targets) == 0 {
		return "", false
	}
	for _, t := range targets[1:] {
		if t != targets[0] {
			return "", false
		}
	}
	if targets[0] == boundWorkspaceID {
		return "", false
	}
	return targets[0], true
}

// serveCrossWorkspaceRead is the Auth hook for a task token. It returns false
// to let Auth continue as upstream (no store, flag off, no other workspace
// named, no live grant) and true when it has answered or served the request.
func serveCrossWorkspaceRead(w http.ResponseWriter, r *http.Request, next http.Handler, queries *db.Queries, boundWorkspaceID, agentID, taskID string) bool {
	ctx := r.Context()
	store, _ := ctx.Value(ctxKeyCrossReadStore).(CrossWorkspaceReadStore)
	if store == nil || !store.Enabled(ctx) {
		return false
	}
	target, ok := crossWorkspaceReadTarget(r, queries, boundWorkspaceID)
	if !ok {
		return false
	}
	grant, found, err := store.ActiveGrant(ctx, target, boundWorkspaceID, agentID)
	if err != nil {
		slog.Warn("auth: cross-workspace read grant lookup failed",
			"path", r.URL.Path, "target_workspace_id", target, "source_workspace_id", boundWorkspaceID, "error", err)
		writeError(w, http.StatusServiceUnavailable, errAuthBackendUnavailable)
		return true
	}
	if !found {
		return false
	}

	pattern, allowed := matchCrossWorkspaceReadRoute(r)
	entry := crossread.AuditEntry{
		GrantID:           grant.ID,
		TargetWorkspaceID: target,
		SourceWorkspaceID: boundWorkspaceID,
		AgentID:           agentID,
		TaskID:            taskID,
		Method:            r.Method,
		Route:             pattern,
		Allowed:           allowed,
	}
	if !allowed {
		entry.Route = r.Method + " " + r.URL.Path
	}
	if err := store.RecordAudit(ctx, entry); err != nil {
		slog.Warn("auth: cross-workspace read audit failed",
			"path", r.URL.Path, "grant_id", grant.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, errAuthBackendUnavailable)
		return true
	}
	if !allowed {
		slog.Warn("auth: cross-workspace read refused a route outside the allowlist",
			"path", r.URL.Path, "method", r.Method, "grant_id", grant.ID, "agent_id", agentID, "task_id", taskID)
		writeError(w, http.StatusForbidden, errCrossWorkspaceReadOnly)
		return true
	}

	r.Header.Set("X-User-ID", grant.ID)
	r.Header.Set("X-Workspace-ID", target)
	r.Header.Del("X-Agent-ID")
	r.Header.Del("X-Task-ID")
	r.Header.Set("X-Actor-Source", ActorSourceCrossWorkspaceReader)
	next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyCrossReadGrant, grant)))
	return true
}

func crossWorkspaceReadGrant(ctx context.Context) (crossread.Grant, bool) {
	g, ok := ctx.Value(ctxKeyCrossReadGrant).(crossread.Grant)
	return g, ok
}

// CrossWorkspaceReaderMember returns the synthetic member of a request served
// under a cross-workspace read grant, when userID and workspaceID are the
// grant's reader and target. Handlers that look members up by user id get it
// instead of a member row the reader does not have.
func CrossWorkspaceReaderMember(ctx context.Context, userID, workspaceID string) (db.Member, bool) {
	grant, ok := crossWorkspaceReadGrant(ctx)
	if !ok || userID != grant.ID || !sameUUID(workspaceID, grant.TargetWorkspaceID) {
		return db.Member{}, false
	}
	return readerMember(grant)
}

func readerMember(grant crossread.Grant) (db.Member, bool) {
	reader, err := util.ParseUUID(grant.ID)
	if err != nil {
		return db.Member{}, false
	}
	target, err := util.ParseUUID(grant.TargetWorkspaceID)
	if err != nil {
		return db.Member{}, false
	}
	return db.Member{ID: reader, WorkspaceID: target, UserID: reader, Role: RoleCrossWorkspaceReader}, true
}

func sameUUID(a, b string) bool {
	ua, err := util.ParseUUID(a)
	if err != nil {
		return false
	}
	ub, err := util.ParseUUID(b)
	if err != nil {
		return false
	}
	return ua == ub
}

// serveCrossWorkspaceReaderMember is the workspace middleware hook. It
// returns false for every request not served under a grant; otherwise it
// answers: the workspace must be the grant's target, role-gated groups
// refuse, and everything else runs as the synthetic member.
func serveCrossWorkspaceReaderMember(w http.ResponseWriter, r *http.Request, next http.Handler, workspaceID string, roles []string) bool {
	if r.Header.Get("X-Actor-Source") != ActorSourceCrossWorkspaceReader {
		return false
	}
	grant, ok := crossWorkspaceReadGrant(r.Context())
	if !ok || !sameUUID(workspaceID, grant.TargetWorkspaceID) {
		writeError(w, http.StatusForbidden, "cross-workspace read grant does not cover this workspace")
		return true
	}
	if len(roles) > 0 {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return true
	}
	member, ok := readerMember(grant)
	if !ok {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return true
	}
	ctx := SetMemberContext(r.Context(), grant.TargetWorkspaceID, member)
	next.ServeHTTP(w, r.WithContext(ctx))
	return true
}
