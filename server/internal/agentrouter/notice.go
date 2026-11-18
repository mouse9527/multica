// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router notices in the Router chat (M-111), and no late redelivery of pending notices (M-116).

package agentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The owner notice is the Router's one outlet to a person: the sender writes
// one owner_notice row and never knows the channel; each Adapter delivers it
// and records its result in channel_states. The dispatcher has the chat
// adapter (chat_notice.go) and the inbox adapter as its fallback; everyone
// else has the inbox adapter only.

const (
	UrgencyNormal = "normal"
	UrgencyHigh   = "high"

	// Delivery states. "unknown" means the attempt started but its result
	// was not recorded (the process died, or the adapter could not tell). A
	// notice is attempted once per channel; unknown is never retried, because
	// a send is not idempotent.
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
	DeliverySkipped = "skipped"
	DeliveryUnknown = "unknown"

	// InboxNoticeType is the inbox_item type of a delivered notice. It is
	// not in the notification group table, so group mutes and focus mode do
	// not hide it.
	InboxNoticeType = "router_notice"

	maxNoticeTitle = 200
	maxNoticeBody  = 4000
	maxNoticeRefs  = 10
	maxDedupeKey   = 200
)

var ErrNoticeInvalid = errors.New("invalid notice")

// NoticeInput is a notice to send.
type NoticeInput struct {
	WorkspaceID     string
	RecipientUserID string
	Title           string
	Body            string
	// Refs are issue ids the notice is about; the first one is the inbox
	// item's issue.
	Refs                 []string
	Urgency              string
	DedupeKey            string
	SenderType           string
	SenderID             string
	SourceSubscriptionID string
	SourceTaskID         string
}

// Validate trims and checks the input.
func (in *NoticeInput) Validate() error {
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	in.DedupeKey = strings.TrimSpace(in.DedupeKey)
	if in.Urgency == "" {
		in.Urgency = UrgencyNormal
	}
	switch {
	case in.Title == "":
		return fmt.Errorf("%w: title is required", ErrNoticeInvalid)
	case len(in.Title) > maxNoticeTitle:
		return fmt.Errorf("%w: title is longer than %d bytes", ErrNoticeInvalid, maxNoticeTitle)
	case len(in.Body) > maxNoticeBody:
		return fmt.Errorf("%w: body is longer than %d bytes", ErrNoticeInvalid, maxNoticeBody)
	case len(in.Refs) > maxNoticeRefs:
		return fmt.Errorf("%w: at most %d refs", ErrNoticeInvalid, maxNoticeRefs)
	case in.Urgency != UrgencyNormal && in.Urgency != UrgencyHigh:
		return fmt.Errorf("%w: urgency must be normal or high", ErrNoticeInvalid)
	case len(in.DedupeKey) > maxDedupeKey:
		return fmt.Errorf("%w: dedupe_key is longer than %d bytes", ErrNoticeInvalid, maxDedupeKey)
	}
	return nil
}

// Notice is a stored notice.
type Notice struct {
	ID                   string                     `json:"id"`
	WorkspaceID          string                     `json:"workspace_id"`
	RecipientUserID      string                     `json:"recipient_user_id"`
	Title                string                     `json:"title"`
	Body                 string                     `json:"body"`
	Refs                 []string                   `json:"refs"`
	Urgency              string                     `json:"urgency"`
	DedupeKey            *string                    `json:"dedupe_key"`
	SenderType           string                     `json:"sender_type"`
	SenderID             *string                    `json:"sender_id"`
	SourceSubscriptionID *string                    `json:"source_subscription_id"`
	SourceTaskID         *string                    `json:"source_task_id"`
	ChannelStates        map[string]ChannelDelivery `json:"channel_states"`
	CreatedAt            time.Time                  `json:"created_at"`
}

// ChannelDelivery is one channel's delivery result.
type ChannelDelivery struct {
	State  string `json:"state"`
	Ref    string `json:"ref,omitempty"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at,omitempty"`
}

// Adapter delivers a notice to one channel.
type Adapter interface {
	// Channel names the adapter; it keys channel_states.
	Channel() string
	// Deliver sends the notice and reports the state (sent, failed, skipped
	// or unknown) and a channel reference such as the inbox item id.
	Deliver(ctx context.Context, n Notice) ChannelDelivery
}

// FallbackAdapter is an adapter that delivers a notice only when no channel
// before it has sent it; otherwise its state is recorded as skipped.
type FallbackAdapter interface {
	Adapter
	Fallback() bool
}

// Notifier stores notices and hands them to its adapters.
type Notifier struct {
	Store    *Store
	Adapters []Adapter
	Logger   *slog.Logger
}

func NewNotifier(store *Store, adapters ...Adapter) *Notifier {
	return &Notifier{Store: store, Adapters: adapters, Logger: slog.Default()}
}

const noticeColumns = `
    id::text, workspace_id::text, recipient_user_id::text, title, body, refs, urgency,
    dedupe_key, sender_type, sender_id::text, source_subscription_id::text,
    source_task_id::text, channel_states, created_at`

func scanNotice(row pgx.Row) (Notice, error) {
	var n Notice
	var refs, states []byte
	err := row.Scan(&n.ID, &n.WorkspaceID, &n.RecipientUserID, &n.Title, &n.Body, &refs, &n.Urgency,
		&n.DedupeKey, &n.SenderType, &n.SenderID, &n.SourceSubscriptionID,
		&n.SourceTaskID, &states, &n.CreatedAt)
	if err != nil {
		return Notice{}, err
	}
	_ = json.Unmarshal(refs, &n.Refs)
	_ = json.Unmarshal(states, &n.ChannelStates)
	if n.Refs == nil {
		n.Refs = []string{}
	}
	if n.ChannelStates == nil {
		n.ChannelStates = map[string]ChannelDelivery{}
	}
	return n, nil
}

// noSender stands in for the missing sender_id of system notices in the
// dedupe index (migration 9013).
const noSender = "00000000-0000-0000-0000-000000000000"

// insertNotice writes a notice row through q, which may be a transaction. A
// notice whose dedupe key the same sender already used for the same
// recipient is not written again; that row is returned with created false.
func insertNotice(ctx context.Context, q Querier, in NoticeInput) (Notice, bool, error) {
	if err := in.Validate(); err != nil {
		return Notice{}, false, err
	}
	refs := in.Refs
	if refs == nil {
		refs = []string{}
	}
	refsJSON, _ := json.Marshal(refs)
	n, err := scanNotice(q.QueryRow(ctx, `
		INSERT INTO owner_notice (
		    workspace_id, recipient_user_id, title, body, refs, urgency, dedupe_key,
		    sender_type, sender_id, source_subscription_id, source_task_id
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, NULLIF($7, ''),
		          $8, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid, NULLIF($11, '')::uuid)
		ON CONFLICT (workspace_id, sender_type, (COALESCE(sender_id, '`+noSender+`'::uuid)), recipient_user_id, dedupe_key)
		    WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING `+noticeColumns,
		in.WorkspaceID, in.RecipientUserID, in.Title, in.Body, refsJSON, in.Urgency, in.DedupeKey,
		in.SenderType, in.SenderID, in.SourceSubscriptionID, in.SourceTaskID))
	if err == nil {
		return n, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Notice{}, false, fmt.Errorf("insert notice: %w", err)
	}
	n, err = scanNotice(q.QueryRow(ctx, `
		SELECT `+noticeColumns+` FROM owner_notice
		WHERE workspace_id = $1::uuid AND dedupe_key = $2 AND sender_type = $3
		  AND COALESCE(sender_id, '`+noSender+`'::uuid) = COALESCE(NULLIF($4, '')::uuid, '`+noSender+`'::uuid)
		  AND recipient_user_id = $5::uuid`,
		in.WorkspaceID, in.DedupeKey, in.SenderType, in.SenderID, in.RecipientUserID))
	return n, false, err
}

// Send stores a notice and delivers it. A repeated dedupe key returns the
// first notice and delivers nothing.
func (nf *Notifier) Send(ctx context.Context, in NoticeInput) (Notice, bool, error) {
	n, created, err := insertNotice(ctx, nf.Store.DB, in)
	if err != nil || !created {
		return n, created, err
	}
	return nf.Deliver(ctx, n.ID), true, nil
}

// Deliver hands a stored notice to every adapter that has not attempted it
// yet and returns the notice as it stands after. Each channel is claimed
// with an "unknown" state first, so two deliverers never both send it, and
// a delivery cut short stays unknown instead of being sent twice.
func (nf *Notifier) Deliver(ctx context.Context, id string) Notice {
	for _, a := range nf.Adapters {
		if fb, ok := a.(FallbackAdapter); ok && fb.Fallback() {
			skip, _ := json.Marshal(map[string]ChannelDelivery{a.Channel(): {
				State: DeliverySkipped, Reason: "sent through another channel", At: time.Now().UTC().Format(time.RFC3339)}})
			tag, err := nf.Store.DB.Exec(ctx, `
				UPDATE owner_notice SET channel_states = channel_states || $2::jsonb
				WHERE id = $1::uuid AND NOT channel_states ? $3
				  AND EXISTS (SELECT 1 FROM jsonb_each(channel_states) c WHERE c.value->>'state' = 'sent')`,
				id, skip, a.Channel())
			if err != nil {
				nf.Logger.Warn("router: check notice fallback failed", "notice_id", id, "channel", a.Channel(), "error", err)
			} else if tag.RowsAffected() > 0 {
				continue
			}
		}
		claim, _ := json.Marshal(map[string]ChannelDelivery{a.Channel(): {State: DeliveryUnknown, At: time.Now().UTC().Format(time.RFC3339)}})
		n, err := scanNotice(nf.Store.DB.QueryRow(ctx, `
			UPDATE owner_notice SET channel_states = channel_states || $2::jsonb
			WHERE id = $1::uuid AND NOT channel_states ? $3
			RETURNING `+noticeColumns, id, claim, a.Channel()))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			nf.Logger.Warn("router: claim notice delivery failed", "notice_id", id, "channel", a.Channel(), "error", err)
			continue
		}
		res := a.Deliver(ctx, n)
		res.At = time.Now().UTC().Format(time.RFC3339)
		state, _ := json.Marshal(map[string]ChannelDelivery{a.Channel(): res})
		if _, err := nf.Store.DB.Exec(ctx, `
			UPDATE owner_notice SET channel_states = channel_states || $2::jsonb WHERE id = $1::uuid`,
			id, state); err != nil {
			nf.Logger.Warn("router: record notice delivery failed", "notice_id", id, "channel", a.Channel(), "error", err)
		}
		if res.State != DeliverySent {
			nf.Logger.Warn("router: notice not delivered", "notice_id", id, "channel", a.Channel(),
				"state", res.State, "reason", res.Reason)
		}
	}
	last, err := nf.Get(ctx, "", id)
	if err != nil {
		nf.Logger.Warn("router: reload notice failed", "notice_id", id, "error", err)
	}
	return last
}

// PendingNoticeWindow is how old a notice may be and still be delivered by
// DeliverPending. A notice tells the state of a moment (a pause, a busy
// Router); past this window that state may be over, so the notice is left
// as it is rather than delivered late.
const PendingNoticeWindow = 15 * time.Minute

// DeliverPending delivers notices written inside another transaction (the
// dispatcher's pause notices) that some adapter has not claimed yet, for
// instance because the process stopped right after the commit. It returns how
// many it handed over. An empty workspaceID covers every workspace.
//
// A notice some channel has already sent reached its recipient and is not
// pending: a channel added by a later release (or turned on later) must not
// deliver it a second time.
func (nf *Notifier) DeliverPending(ctx context.Context, workspaceID string) (int, error) {
	channels := make([]string, 0, len(nf.Adapters))
	for _, a := range nf.Adapters {
		channels = append(channels, a.Channel())
	}
	if len(channels) == 0 {
		return 0, nil
	}
	rows, err := nf.Store.DB.Query(ctx, `
		SELECT id::text FROM owner_notice
		WHERE created_at > now() - make_interval(secs => $3)
		  AND NOT (channel_states ?& $1::text[])
		  AND NOT EXISTS (SELECT 1 FROM jsonb_each(channel_states) c WHERE c.value->>'state' = 'sent')
		  AND ($2 = '' OR workspace_id = NULLIF($2, '')::uuid)
		ORDER BY created_at LIMIT 50`, channels, workspaceID, PendingNoticeWindow.Seconds())
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		nf.Deliver(ctx, id)
	}
	return len(ids), nil
}

// Get returns one notice; an empty workspaceID matches any workspace.
func (nf *Notifier) Get(ctx context.Context, workspaceID, id string) (Notice, error) {
	n, err := scanNotice(nf.Store.DB.QueryRow(ctx, `
		SELECT `+noticeColumns+` FROM owner_notice
		WHERE id = $1::uuid AND ($2 = '' OR workspace_id = NULLIF($2, '')::uuid)`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notice{}, ErrNotFound
	}
	return n, err
}

// NoticeFilter narrows ListNotices.
type NoticeFilter struct {
	WorkspaceID     string
	RecipientUserID string
	SenderID        string
	SubscriptionID  string
	Limit           int
}

// ListNotices returns notices newest first.
func (nf *Notifier) ListNotices(ctx context.Context, f NoticeFilter) ([]Notice, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := nf.Store.DB.Query(ctx, `
		SELECT `+noticeColumns+` FROM owner_notice
		WHERE workspace_id = $1::uuid
		  AND ($2 = '' OR recipient_user_id = NULLIF($2, '')::uuid)
		  AND ($3 = '' OR sender_id = NULLIF($3, '')::uuid)
		  AND ($4 = '' OR source_subscription_id = NULLIF($4, '')::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $5`, f.WorkspaceID, f.RecipientUserID, f.SenderID, f.SubscriptionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notice{}
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountRecentNotices counts what one sender sent in the last hour.
func (nf *Notifier) CountRecentNotices(ctx context.Context, workspaceID, senderID string) (int, error) {
	var n int
	err := nf.Store.DB.QueryRow(ctx, `
		SELECT count(*) FROM owner_notice
		WHERE workspace_id = $1::uuid AND sender_id = $2::uuid
		  AND created_at > now() - interval '1 hour'`, workspaceID, senderID).Scan(&n)
	return n, err
}

// InboxAdapter delivers a notice as an inbox item of type router_notice to
// its recipient, through the same eligibility check as every other inbox
// write, and announces it on the bus so open clients refresh.
type InboxAdapter struct {
	Queries *db.Queries
	Bus     *events.Bus
}

func (InboxAdapter) Channel() string { return "inbox" }

// Fallback: a notice already sent to the Router chat does not also go to the
// inbox.
func (InboxAdapter) Fallback() bool { return true }

func (a InboxAdapter) Deliver(ctx context.Context, n Notice) ChannelDelivery {
	recipient, err := util.ParseUUID(n.RecipientUserID)
	if err != nil {
		return ChannelDelivery{State: DeliveryFailed, Reason: "recipient is not a uuid"}
	}
	var issueID pgtype.UUID
	issueStatus := ""
	if len(n.Refs) > 0 {
		if id, err := util.ParseUUID(n.Refs[0]); err == nil {
			if issue, err := a.Queries.GetIssue(ctx, id); err == nil && util.UUIDToString(issue.WorkspaceID) == n.WorkspaceID {
				issueID, issueStatus = issue.ID, issue.Status
			}
		}
	}
	severity := "info"
	if n.Urgency == UrgencyHigh {
		severity = "action_required"
	}
	var actorID pgtype.UUID
	if n.SenderID != nil {
		actorID, _ = util.ParseUUID(*n.SenderID)
	}
	details, _ := json.Marshal(map[string]any{
		"notice_id": n.ID, "refs": n.Refs, "urgency": n.Urgency,
		"subscription_id": n.SourceSubscriptionID, "task_id": n.SourceTaskID,
	})
	// Announced like every other inbox row: a row that joins the recipient's
	// snooze on the issue goes out as inbox:snoozed, so no banner or push.
	item, inserted, err := inboxwriter.CreateAndAnnounce(ctx, a.Queries, a.Bus, db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: util.MustParseUUID(n.WorkspaceID), RecipientType: "member", RecipientID: recipient,
		Type: InboxNoticeType, Severity: severity, IssueID: issueID, Title: n.Title,
		Body:      pgtype.Text{String: n.Body, Valid: n.Body != ""},
		ActorType: pgtype.Text{String: n.SenderType, Valid: true}, ActorID: actorID, Details: details,
	}, &issueStatus)
	if err != nil {
		return ChannelDelivery{State: DeliveryFailed, Reason: err.Error()}
	}
	if !inserted {
		return ChannelDelivery{State: DeliverySkipped, Reason: "recipient is not eligible for this inbox item"}
	}
	return ChannelDelivery{State: DeliverySent, Ref: util.UUIDToString(item.ID)}
}
