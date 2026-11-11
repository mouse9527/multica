package githubissues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The write path (docs/github-issues.md, "双写"): a change to a GitHub issue
// made in Multica is checked, recorded as a github_write_intent, sent to
// GitHub through RepoClient, and only after GitHub accepted it written to the
// local tables. Fields only Multica has (assignee, project, priority,
// parent) are written locally only and never reach this file.

// Intent ops, as github_write_intent.op.
const (
	opCreateIssue   = "create_issue"
	opUpdateIssue   = "update_issue"
	opCreateComment = "create_comment"
	opUpdateComment = "update_comment"
	opDeleteComment = "delete_comment"
)

// Intent states, as github_write_intent.state.
const (
	intentPending  = "pending"
	intentGitHubOK = "github_ok"
	intentApplied  = "applied"
	intentUnknown  = "unknown"
	intentFailed   = "failed"
)

const (
	// needsConfirmation prefixes the error of an intent handed to a person:
	// its result could not be established and it is never retried.
	needsConfirmation = "needs_confirmation: "
	// unknownGrace is how long a write GitHub never answered is looked for
	// before it is handed to a person: two poll cycles, and at least five
	// minutes.
	unknownGrace   = max(2*pollInterval, 5*time.Minute)
	maxIntentError = 500
)

// WriteErrorKind classifies a write that did not go through.
type WriteErrorKind int

const (
	// WriteRejected means GitHub answered 4xx: nothing was written.
	WriteRejected WriteErrorKind = iota + 1
	// WriteRateLimited means GitHub throttled the write: nothing was written.
	WriteRateLimited
	// WriteUnknown means the request timed out, the connection dropped or
	// GitHub answered 5xx: the write may or may not exist on GitHub. It is
	// never retried automatically; the next polls look for it by its marker.
	WriteUnknown
	// WriteUnavailable means nothing was sent: the token is not usable or the
	// repository is not registered in the workspace.
	WriteUnavailable
	// WriteNotAllowed means the change was refused before anything was sent.
	WriteNotAllowed
)

// WriteError is returned by the Begin* methods when a write did not go
// through. It never carries the token or a GitHub response body.
type WriteError struct {
	Kind WriteErrorKind
	// StatusCode is GitHub's status for WriteRejected.
	StatusCode int
	// RetryAfter is GitHub's wait for WriteRateLimited.
	RetryAfter time.Duration
	msg        string
}

func (e *WriteError) Error() string { return "githubissues: " + e.msg }

// HTTPResponse is how a handler answers a write that did not go through:
// status, JSON body and, for throttling, a Retry-After header value.
func (e *WriteError) HTTPResponse() (status int, body map[string]any, retryAfter string) {
	switch e.Kind {
	case WriteRejected:
		return http.StatusUnprocessableEntity, map[string]any{
			"code": "github_write_rejected", "error": e.msg, "github_status": e.StatusCode,
		}, ""
	case WriteRateLimited:
		secs := int(e.RetryAfter.Round(time.Second) / time.Second)
		return http.StatusTooManyRequests, map[string]any{
			"code": "github_rate_limited", "error": e.msg, "retry_after_seconds": secs,
		}, strconv.Itoa(secs)
	case WriteUnknown:
		return http.StatusBadGateway, map[string]any{"code": "github_write_unconfirmed", "error": e.msg}, ""
	case WriteNotAllowed:
		return http.StatusForbidden, map[string]any{"code": "github_write_not_allowed", "error": e.msg}, ""
	default:
		return http.StatusServiceUnavailable, map[string]any{"code": "github_unavailable", "error": e.msg}, ""
	}
}

const unconfirmedMessage = "the change may or may not have reached GitHub; " +
	"it is not retried automatically and the next polls look for it, so do not resend it yet"

// Write is a write GitHub accepted. The caller writes the local copy and then
// reports the outcome with Finish (or, for a new issue, InTx and Abandon).
// A nil *Write means there was nothing to send to GitHub; its methods are
// no-ops.
type Write struct {
	svc    *Service
	intent db.GithubWriteIntent
	repo   *Repo
	// issue is GitHub's answer for issue writes.
	issue *githubIssue
	// comment is GitHub's answer for comment writes.
	comment *githubComment
}

// CommentID is the id the new local comment must be created with, chosen
// before the request so a comment healed by the poller and the one the caller
// writes are the same row. Zero for other writes.
func (w *Write) CommentID() pgtype.UUID {
	if w == nil {
		return pgtype.UUID{}
	}
	return w.intent.CommentID
}

// writeRequest is one GitHub write.
type writeRequest struct {
	op     string
	actor  Actor
	repo   *Repo
	method string
	path   string
	// payload builds the JSON body; op is the intent id for the marker.
	payload func(op pgtype.UUID) map[string]any
	// target columns of the intent
	issueID   pgtype.UUID
	number    int32
	commentID pgtype.UUID
	githubID  int64
}

// send records the intent, sends the request and classifies the answer. On
// success the intent is github_ok and the response is returned.
func (s *Service) send(ctx context.Context, req writeRequest) (db.GithubWriteIntent, *Response, error) {
	if !req.actor.valid() {
		return db.GithubWriteIntent{}, nil, errors.New("githubissues: write without a member or agent actor")
	}
	q, err := s.queries()
	if err != nil {
		return db.GithubWriteIntent{}, nil, err
	}
	id := dbid.NewV7()
	payload := req.payload(id)
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return db.GithubWriteIntent{}, nil, err
		}
	}
	params := db.InsertGithubWriteIntentParams{
		ID: id, WorkspaceID: req.repo.workspaceID, Op: req.op,
		RepoID: req.repo.id, ExternalRepo: req.repo.FullName(),
		IssueID: req.issueID, CommentID: req.commentID,
		PayloadDigest: payloadDigest(payload),
		ActorType:     req.actor.Type, ActorID: req.actor.ID,
	}
	if req.number > 0 {
		params.ExternalNumber = pgtype.Int4{Int32: req.number, Valid: true}
	}
	if req.githubID > 0 {
		params.GithubID = pgtype.Int8{Int64: req.githubID, Valid: true}
	}
	intent, err := q.InsertGithubWriteIntent(ctx, params)
	if err != nil {
		return db.GithubWriteIntent{}, nil, fmt.Errorf("githubissues: record write intent: %w", err)
	}

	// The request outlives a caller that hangs up: once it may have reached
	// GitHub, its answer must be recorded.
	sendCtx := context.WithoutCancel(ctx)
	resp, err := req.repo.Do(sendCtx, req.method, req.path, body)
	var limited *ghsnapshot.RateLimitError
	switch {
	case err == nil:
	case errors.Is(err, ErrTokenNotUsable):
		s.settleIntent(sendCtx, intent, intentFailed, "token not usable")
		return intent, nil, &WriteError{Kind: WriteUnavailable, msg: "the GitHub token is not usable"}
	case errors.As(err, &limited):
		s.settleIntent(sendCtx, intent, intentFailed, "rate limited")
		return intent, nil, &WriteError{Kind: WriteRateLimited, RetryAfter: limited.RetryAfter, msg: "GitHub rate limit reached; try again later"}
	default:
		s.settleIntent(sendCtx, intent, intentUnknown, err.Error())
		s.logger.Warn("github write result unknown", "intent_id", intent.ID, "op", req.op, "repo", req.repo, "error", err)
		return intent, nil, &WriteError{Kind: WriteUnknown, msg: unconfirmedMessage}
	}

	accepted := resp.StatusCode >= 200 && resp.StatusCode < 300
	if req.method == http.MethodDelete && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		// Already gone: deleting again changes nothing.
		accepted = true
	}
	switch {
	case accepted:
		return intent, resp, nil
	case resp.StatusCode >= 500:
		s.settleIntent(sendCtx, intent, intentUnknown, "github status "+strconv.Itoa(resp.StatusCode))
		s.logger.Warn("github write result unknown", "intent_id", intent.ID, "op", req.op, "repo", req.repo, "status", resp.StatusCode)
		return intent, nil, &WriteError{Kind: WriteUnknown, msg: unconfirmedMessage}
	default:
		s.settleIntent(sendCtx, intent, intentFailed, "github status "+strconv.Itoa(resp.StatusCode))
		return intent, nil, &WriteError{Kind: WriteRejected, StatusCode: resp.StatusCode,
			msg: "GitHub rejected the change (HTTP " + strconv.Itoa(resp.StatusCode) + ")"}
	}
}

// accept records GitHub's acceptance and the id it assigned.
func (s *Service) accept(ctx context.Context, intent *db.GithubWriteIntent, githubID int64, number int32) {
	params := db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: intentGitHubOK,
	}
	if githubID > 0 {
		params.GithubID = pgtype.Int8{Int64: githubID, Valid: true}
	}
	if number > 0 {
		params.ExternalNumber = pgtype.Int4{Int32: number, Valid: true}
	}
	if updated, err := db.New(s.db).UpdateGithubWriteIntentState(context.WithoutCancel(ctx), params); err != nil {
		// GitHub has the write; the poller heals the local copy from it, and
		// Finish moves the intent on from pending.
		s.logger.Warn("github write: record acceptance", "intent_id", intent.ID, "error", err)
		intent.State, intent.GithubID, intent.ExternalNumber = intentGitHubOK, params.GithubID, params.ExternalNumber
	} else {
		*intent = updated
	}
}

// settleIntent moves an intent to state with an error note. Errors are only
// logged: the caller already knows the outcome and reports it.
func (s *Service) settleIntent(ctx context.Context, intent db.GithubWriteIntent, state, note string) {
	if _, err := db.New(s.db).UpdateGithubWriteIntentState(ctx, db.UpdateGithubWriteIntentStateParams{
		ID: intent.ID, WorkspaceID: intent.WorkspaceID, State: state, Error: errorText(note),
	}); err != nil {
		s.logger.Warn("github write: record intent state", "intent_id", intent.ID, "state", state, "error", err)
	}
}

// localFailed notes that GitHub has the write but the local copy failed. The
// intent stays github_ok; the next poll heals the local copy from GitHub.
func (w *Write) localFailed(ctx context.Context, cause error) {
	w.svc.logger.Warn("github write accepted but local write failed; the next poll heals it",
		"intent_id", w.intent.ID, "op", w.intent.Op, "repo", w.repo, "error", cause)
	w.svc.settleIntent(context.WithoutCancel(ctx), w.intent, intentGitHubOK, "local write failed: "+cause.Error())
}

func errorText(note string) pgtype.Text {
	if note == "" {
		return pgtype.Text{}
	}
	if len(note) > maxIntentError {
		note = strings.ToValidUTF8(note[:maxIntentError], "")
	}
	return pgtype.Text{String: note, Valid: true}
}

// payloadDigest is "<sorted keys>:<sha256 of the canonical JSON>". The keys
// say which fields the write set, so a reconciler can recompute the digest
// from GitHub's current values of the same fields.
func payloadDigest(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// encoding/json writes map keys sorted, so the encoding is canonical.
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return strings.Join(keys, ",") + ":" + hex.EncodeToString(sum[:])
}

// repoForIssue returns the client for the repository of a GitHub issue.
func (s *Service) repoForIssue(ctx context.Context, ext db.IssueExternal) (*Repo, error) {
	owner, name, ok := strings.Cut(ext.ExternalRepo, "/")
	if !ok {
		return nil, &WriteError{Kind: WriteUnavailable, msg: "the issue's repository name is malformed"}
	}
	repo, err := s.RepoClient(ctx, ext.WorkspaceID, owner, name)
	if err != nil {
		return nil, repoError(err)
	}
	if repo.ID() != ext.ExternalRepoID {
		return nil, &WriteError{Kind: WriteUnavailable, msg: "the issue's repository is registered under a name that now points elsewhere"}
	}
	return repo, nil
}

// repoError turns a RepoClient failure into a WriteError: nothing was sent.
func repoError(err error) error {
	var limited *ghsnapshot.RateLimitError
	switch {
	case errors.As(err, &limited):
		return &WriteError{Kind: WriteRateLimited, RetryAfter: limited.RetryAfter, msg: "GitHub rate limit reached; try again later"}
	case errors.Is(err, ErrRepoNotRegistered):
		return &WriteError{Kind: WriteUnavailable, msg: "the repository is not registered on any project of this workspace"}
	case errors.Is(err, ErrRepoAmbiguous):
		return &WriteError{Kind: WriteUnavailable, msg: "the repository is registered under two names in this workspace"}
	case errors.Is(err, ErrTokenNotUsable):
		return &WriteError{Kind: WriteUnavailable, msg: "the GitHub token is not usable"}
	default:
		return &WriteError{Kind: WriteUnavailable, msg: "could not reach GitHub"}
	}
}
