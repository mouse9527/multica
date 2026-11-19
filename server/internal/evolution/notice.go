// Added by the mouse9527/multica fork: Evolution P2, telling members (M-136).

package evolution

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// NoticeInboxType is the inbox item every member gets when Evolution starts
// reading the workspace's chats, and every member who joins while it does.
const NoticeInboxType = "evolution_notice"

// Members cannot be assumed to read one language, so the notice carries both.
const (
	noticeTitle = "进化会读取本工作区的对话和评论 · Evolution reads this workspace's chats and comments"
	noticeBody  = `本工作区启用了内置智能体「进化」。它每天读取一次工作区内的 issue 评论和所有成员与智能体的对话，用来发现反复出现的问题，并给智能体、工作流和团队提出改进提案。

不读的内容：运行日志和工具调用；私有项目里的评论；私有智能体（不对全员公开）的对话；管理员排除的项目和智能体。读取前会去掉疑似密钥、令牌、邮箱、手机号和内网地址。

提案以 issue 的形式出现，每项改动都要等所有者或管理员批准。提案里不贴对话原文，也不写成员姓名；评论只摘一句、不超过 120 字。所有者或管理员可以随时在「设置 → 进化」里关闭对话读取或整个分析。

The workspace has enabled its built-in Evolution agent. Once a day it reads the workspace's issue comments and every member's chats with agents, to find recurring problems and propose improvements to agents, workflows, and the team.

Not read: run logs and tool calls; comments in private projects; chats with private agents (agents not open to every member); projects and agents an admin excluded. Secrets, tokens, email addresses, phone numbers, and internal hosts are removed before reading.

Proposals arrive as issues, and every change waits for an owner's or admin's approval. Proposals never paste chat messages or name members, and quote comments by one short excerpt of at most 120 characters. An owner or admin can turn chat reading, or the whole analysis, off at any time in Settings → Evolution.`
)

// Actor is who a notice is from.
type Actor struct {
	Type string
	ID   string
}

type memberRow struct {
	ID     string
	UserID string
	Joined time.Time
}

// listMembersAfter returns members who joined after p, oldest first.
func listMembersAfter(ctx context.Context, q Querier, workspaceID string, p Position, limit int) ([]memberRow, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.user_id::text, m.created_at FROM member m
		WHERE m.workspace_id = $1::uuid
		  AND (m.created_at > $2 OR (m.created_at = $2 AND $3::uuid IS NOT NULL AND m.id > $3::uuid))
		ORDER BY m.created_at, m.id
		LIMIT $4`, workspaceID, p.TS, p.idArg(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []memberRow{}
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.ID, &m.UserID, &m.Joined); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// notifyMembersAfter sends the notice to every member who joined after p and
// returns the position of the last one notified (p when there was nobody).
// It stops at the first failure, returning the position reached so far.
func notifyMembersAfter(ctx context.Context, q Querier, queries *db.Queries, pub inboxwriter.Publisher, workspaceID string, from Actor, p Position) (Position, int, error) {
	wsID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return p, 0, err
	}
	actorID, err := util.ParseUUID(from.ID)
	if err != nil {
		return p, 0, err
	}
	details, _ := json.Marshal(map[string]any{"kind": "chat_reading"})
	sent := 0
	for {
		members, err := listMembersAfter(ctx, q, workspaceID, p, 200)
		if err != nil {
			return p, sent, err
		}
		for _, m := range members {
			recipient, err := util.ParseUUID(m.UserID)
			if err != nil {
				return p, sent, err
			}
			if _, _, err := inboxwriter.CreateAndAnnounce(ctx, queries, pub, db.CreateInboxItemParams{
				ID: dbid.NewV7(), WorkspaceID: wsID, RecipientType: "member", RecipientID: recipient,
				Type: NoticeInboxType, Severity: "info", Title: noticeTitle,
				Body:      pgtype.Text{String: noticeBody, Valid: true},
				ActorType: pgtype.Text{String: from.Type, Valid: true}, ActorID: actorID, Details: details,
			}, nil); err != nil {
				return p, sent, fmt.Errorf("notify member %s: %w", m.UserID, err)
			}
			p = Position{TS: m.Joined, ID: m.ID}
			sent++
		}
		if len(members) < 200 {
			return p, sent, nil
		}
	}
}
