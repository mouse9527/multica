// Added by the mouse9527/multica fork: Evolution P2, the signal channel (M-136).

package evolution

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MaxContentRunes caps one signal's content; longer content is cut and
// marked truncated.
const MaxContentRunes = 4000

// Page sizes for GET /api/evolution/signals.
const (
	DefaultPageSize = 100
	MaxPageSize     = 200
)

// Signal is one comment or chat message, redacted. It carries ids, not
// names: the agent links to the source and never names the person.
type Signal struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	Content    string    `json:"content"`
	Redactions int       `json:"redactions"`
	Truncated  bool      `json:"truncated"`

	// Comments.
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueTitle      string `json:"issue_title,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	ParentID        string `json:"parent_id,omitempty"`
	AuthorType      string `json:"author_type,omitempty"`
	AuthorID        string `json:"author_id,omitempty"`

	// Chat messages.
	ChatSessionID string `json:"chat_session_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	Role          string `json:"role,omitempty"`

	pos Position
}

// Page is one page of a source for a run.
type Page struct {
	Source string   `json:"source"`
	Items  []Signal `json:"items"`
	// NextAfter continues from the last item; empty when the page is empty.
	NextAfter string `json:"next_after"`
	// HasMore: another page is waiting in this run's window.
	HasMore bool `json:"has_more"`
	// LimitReached: this run has read its max_items_per_window; the rest is
	// left for a catch-up run.
	LimitReached bool `json:"limit_reached"`
	// WindowUntil is the upper bound of this run's window for the source.
	WindowUntil time.Time `json:"window_until"`
}

// PageRequest asks for one page.
type PageRequest struct {
	WorkspaceID string
	// IssueID is the analysis issue of the calling task; it selects the run.
	IssueID string
	Source  string
	// After, when set, continues from a previous page's NextAfter. It is
	// clamped to the run's range: a client can re-read what it was served,
	// never skip ahead of it.
	After *Position
	Limit int
	// Config is the workspace's configuration at request time.
	Config Config
	// HiddenProjects is projectaccess.Hidden.Param() for the run: nil when no
	// project is hidden.
	HiddenProjects []pgtype.UUID
	// SelfAgentID is the Evolution agent: its own comments and chats are not
	// signals.
	SelfAgentID string
}

// ServePage returns the next page of a source for the open run on
// req.IssueID and records how far the run was served. The cursor itself does
// not move here; it moves to the served position only when the run succeeds.
func ServePage(ctx context.Context, db Database, req PageRequest) (Page, error) {
	if req.Source == SourceComment && !req.Config.CommentsWanted() {
		return Page{}, ErrSourceOff
	}
	if req.Source == SourceChat && !req.Config.ChatWanted() {
		return Page{}, ErrSourceOff
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback(ctx)

	run, err := openRunForIssueForUpdate(ctx, tx, req.WorkspaceID, req.IssueID)
	if err != nil {
		return Page{}, err
	}
	cursor, ok, err := GetCursor(ctx, tx, req.WorkspaceID, req.Source)
	if err != nil {
		return Page{}, err
	}
	if !ok {
		// Chat without a cursor was never announced to members.
		return Page{}, ErrSourceOff
	}
	served, wasServed := run.Served[req.Source]
	if !wasServed || served.Before(cursor) {
		served = cursor
	}
	start := served
	if req.After != nil {
		start = *req.After
		if start.Before(cursor) {
			start = cursor
		}
		if served.Before(start) {
			start = served
		}
	}
	until := run.WindowUntil
	if capped := cursor.TS.Add(time.Duration(req.Config.MaxWindowDays) * 24 * time.Hour); capped.Before(until) {
		until = capped
	}
	page := Page{Source: req.Source, Items: []Signal{}, WindowUntil: until}

	budget := req.Config.MaxItemsPerWindow - run.ServedItems
	var items []Signal
	if until.After(start.TS) || (until.Equal(start.TS) && start.ID != "") {
		excluded := append([]string{req.SelfAgentID}, req.Config.ExcludeAgents...)
		if req.Source == SourceComment {
			items, err = readComments(ctx, tx, req.WorkspaceID, start, until, req.HiddenProjects, req.Config.ExcludeProjects, excluded, limit+1)
		} else {
			items, err = readChats(ctx, tx, req.WorkspaceID, start, until, req.HiddenProjects, req.Config.ExcludeProjects, excluded, limit+1)
		}
		if err != nil {
			return Page{}, err
		}
	}
	if len(items) > limit {
		items = items[:limit]
		page.HasMore = true
	}
	// Only items past what the run was already served count toward its
	// budget; a re-read is free.
	fresh := 0
	for i, it := range items {
		if !served.Before(it.pos) {
			continue
		}
		if fresh == max(budget, 0) {
			items = items[:i]
			page.HasMore = false
			page.LimitReached = true
			break
		}
		fresh++
	}
	page.Items = items

	reached := served
	if len(items) > 0 {
		last := items[len(items)-1].pos
		page.NextAfter = last.Token()
		if reached.Before(last) {
			reached = last
		}
	}
	if !page.HasMore && !page.LimitReached && reached.Before(Position{TS: until}) {
		// The window is exhausted: every row up to until was considered.
		reached = Position{TS: until}
	}
	if served.Before(reached) || fresh > 0 || !wasServed {
		if err := recordServed(ctx, tx, run.ID, req.Source, reached, fresh); err != nil {
			return Page{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	for i := range page.Items {
		page.Items[i].redact()
	}
	return page, nil
}

func (s *Signal) redact() {
	content, n := Redact(s.Content)
	if utf8.RuneCountInString(content) > MaxContentRunes {
		content = string([]rune(content)[:MaxContentRunes])
		s.Truncated = true
	}
	s.Content = content
	s.Redactions = n
	if s.IssueTitle != "" {
		title, n := Redact(s.IssueTitle)
		s.IssueTitle = title
		s.Redactions += n
	}
}

// Keyset predicate over (created_at, id) for start = ($2 ts, $3 id); $3 NULL
// means everything at $2 was read. $4 is the window's upper bound.
const keysetSQL = `(%[1]s.created_at > $2 OR (%[1]s.created_at = $2 AND $3::uuid IS NOT NULL AND %[1]s.id > $3::uuid))
	AND %[1]s.created_at <= $4`

// readComments returns members' and agents' ordinary comments in the
// window. Comments on issues in hidden (private) or excluded projects, and
// comments by excluded agents, are filtered here, in SQL, so they are never
// served.
func readComments(ctx context.Context, q Querier, workspaceID string, start Position, until time.Time,
	hidden []pgtype.UUID, excludeProjects, excludeAgents []string, limit int) ([]Signal, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id::text, c.created_at, c.content, c.issue_id::text,
		       w.issue_prefix || '-' || i.number::text, i.title, COALESCE(i.project_id::text, ''),
		       COALESCE(c.parent_id::text, ''), c.author_type, c.author_id::text
		FROM comment c
		JOIN issue i ON i.id = c.issue_id AND i.workspace_id = $1::uuid
		JOIN workspace w ON w.id = i.workspace_id
		WHERE c.workspace_id = $1::uuid
		  AND c.deleted_at IS NULL
		  AND c.type = 'comment'
		  AND c.author_type IN ('member', 'agent')
		  AND `+sprintfKeyset("c")+`
		  AND ($5::uuid[] IS NULL OR i.project_id IS NULL OR NOT (i.project_id = ANY($5::uuid[])))
		  AND (i.project_id IS NULL OR NOT (i.project_id = ANY(COALESCE($6::uuid[], '{}'))))
		  AND NOT (c.author_type = 'agent' AND c.author_id = ANY(COALESCE($7::uuid[], '{}')))
		ORDER BY c.created_at, c.id
		LIMIT $8`,
		workspaceID, start.TS, start.idArg(), until, hidden, excludeProjects, excludeAgents, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(rows pgx.Rows) (Signal, error) {
		s := Signal{Source: SourceComment}
		err := rows.Scan(&s.ID, &s.CreatedAt, &s.Content, &s.IssueID, &s.IssueIdentifier, &s.IssueTitle,
			&s.ProjectID, &s.ParentID, &s.AuthorType, &s.AuthorID)
		return s, err
	})
}

// readChats returns ordinary chat messages, from every member, with agents
// whose chats any member may open: an agent that is private, or public only
// to some members, is skipped, as are excluded agents and chats filed under
// hidden or excluded projects.
func readChats(ctx context.Context, q Querier, workspaceID string, start Position, until time.Time,
	hidden []pgtype.UUID, excludeProjects, excludeAgents []string, limit int) ([]Signal, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.created_at, m.content, s.id::text, s.agent_id::text, m.role
		FROM chat_message m
		JOIN chat_session s ON s.id = m.chat_session_id AND s.workspace_id = $1::uuid
		JOIN agent a ON a.id = s.agent_id AND a.workspace_id = $1::uuid
		WHERE m.message_kind = 'message'
		  AND `+sprintfKeyset("m")+`
		  AND a.permission_mode = 'public_to'
		  AND EXISTS (SELECT 1 FROM agent_invocation_target t
		              WHERE t.agent_id = a.id AND t.target_type = 'workspace')
		  AND NOT (s.agent_id = ANY(COALESCE($7::uuid[], '{}')))
		  AND ($5::uuid[] IS NULL OR s.project_id IS NULL OR NOT (s.project_id = ANY($5::uuid[])))
		  AND (s.project_id IS NULL OR NOT (s.project_id = ANY(COALESCE($6::uuid[], '{}'))))
		ORDER BY m.created_at, m.id
		LIMIT $8`,
		workspaceID, start.TS, start.idArg(), until, hidden, excludeProjects, excludeAgents, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(rows pgx.Rows) (Signal, error) {
		s := Signal{Source: SourceChat}
		err := rows.Scan(&s.ID, &s.CreatedAt, &s.Content, &s.ChatSessionID, &s.AgentID, &s.Role)
		return s, err
	})
}

func sprintfKeyset(alias string) string {
	return fmt.Sprintf(keysetSQL, alias)
}

func collect(rows pgx.Rows, scan func(pgx.Rows) (Signal, error)) ([]Signal, error) {
	defer rows.Close()
	out := []Signal{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		s.pos = Position{TS: s.CreatedAt, ID: s.ID}
		out = append(out, s)
	}
	return out, rows.Err()
}
