// Added by the mouse9527/multica fork: Evolution P2, saving the configuration (M-136).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNoticeRequired: the request would start reading chats, which every
// member must be told about first, and it did not confirm that they will be.
var ErrNoticeRequired = errors.New("reading chats notifies every member first; confirm with acknowledge_member_notice")

// ErrWorkspaceNotFound: the workspace row is gone.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// State is a workspace's configuration and what is being read.
type State struct {
	Config Config `json:"config"`
	// ChatActive: members were told and chats are being read.
	ChatActive bool `json:"chat_active"`
	// CommentsActive: comments are being read.
	CommentsActive bool `json:"comments_active"`
}

// LoadState reads a workspace's configuration and cursors.
func LoadState(ctx context.Context, q Querier, workspaceID string) (State, error) {
	var settings []byte
	err := q.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1::uuid`, workspaceID).Scan(&settings)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrWorkspaceNotFound
	}
	if err != nil {
		return State{}, err
	}
	st := State{Config: ConfigFromSettings(settings)}
	_, hasComment, err := GetCursor(ctx, q, workspaceID, SourceComment)
	if err != nil {
		return State{}, err
	}
	_, hasChat, err := GetCursor(ctx, q, workspaceID, SourceChat)
	if err != nil {
		return State{}, err
	}
	st.CommentsActive = hasComment && st.Config.CommentsWanted()
	st.ChatActive = hasChat && st.Config.ChatWanted()
	return st, nil
}

// SaveRequest is one PUT of the configuration.
type SaveRequest struct {
	WorkspaceID string
	Config      Config
	// AcknowledgeMemberNotice confirms that turning chat reading on sends
	// every member the notice. Without it such a request is refused.
	AcknowledgeMemberNotice bool
	// Actor is the admin saving; the member notice comes from them.
	Actor Actor
	Now   time.Time
}

// SaveConfig validates and stores a configuration. When it starts chat
// reading it first notifies every member, and only then starts the chat
// cursor, so chats are never read before members were told. When it stops a
// source it deletes the source's cursor, so turning it back on reads only
// from then.
func SaveConfig(ctx context.Context, database Database, queries *db.Queries, pub inboxwriter.Publisher, req SaveRequest) (State, error) {
	if err := req.Config.Validate(); err != nil {
		return State{}, err
	}
	cfg := req.Config.normalized()
	prev, err := LoadState(ctx, database, req.WorkspaceID)
	if err != nil {
		return State{}, err
	}
	now := req.Now.UTC().Truncate(time.Microsecond)

	startChat := cfg.ChatWanted() && !prev.ChatActive
	var noticed Position
	if startChat {
		if !req.AcknowledgeMemberNotice {
			return State{}, ErrNoticeRequired
		}
		noticed, _, err = notifyMembersAfter(ctx, database, queries, pub, req.WorkspaceID, req.Actor, Position{})
		if err != nil {
			return State{}, err
		}
	}

	patch, err := json.Marshal(map[string]any{WorkspaceSettingKey: cfg})
	if err != nil {
		return State{}, err
	}
	tx, err := database.Begin(ctx)
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback(ctx)
	if err := db.New(tx).MergeWorkspaceSettings(ctx, db.MergeWorkspaceSettingsParams{
		ID: util.MustParseUUID(req.WorkspaceID), Patch: patch,
	}); err != nil {
		return State{}, err
	}
	switch {
	case startChat:
		if err := SetCursor(ctx, tx, req.WorkspaceID, SourceChat, Position{TS: now}); err != nil {
			return State{}, err
		}
		if noticed.TS.IsZero() {
			noticed = Position{TS: now}
		}
		if err := SetCursor(ctx, tx, req.WorkspaceID, SourceMemberNotice, noticed); err != nil {
			return State{}, err
		}
	case !cfg.ChatWanted():
		if err := DeleteCursor(ctx, tx, req.WorkspaceID, SourceChat, SourceMemberNotice); err != nil {
			return State{}, err
		}
	}
	if cfg.CommentsWanted() {
		if err := StartCursorIfMissing(ctx, tx, req.WorkspaceID, SourceComment, Position{TS: now}); err != nil {
			return State{}, err
		}
	} else if err := DeleteCursor(ctx, tx, req.WorkspaceID, SourceComment); err != nil {
		return State{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return State{}, err
	}
	return LoadState(ctx, database, req.WorkspaceID)
}
