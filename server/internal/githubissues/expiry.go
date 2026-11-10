package githubissues

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	expiryJobName       = "github_token_expiry"
	inboxTypeExpiry     = "github_token_expiry"
	expiryInboxSeverity = "attention"
)

// reminderThresholds are the days before expiry that each get one reminder,
// ascending.
var reminderThresholds = []int{1, 7, 14}

// ExpiryReminderJob is the daily scheduler job that reminds workspace owners
// and admins 14, 7 and 1 days before the token expires.
//
// A reminder is due once the token is within its threshold and the member has
// not had it yet, so a day the job did not run delays a reminder instead of
// dropping it, and a retried run sends nothing twice. Expiry itself needs no
// job: every process derives the expired status from the date.
func (s *Service) ExpiryReminderJob(queries *db.Queries, bus *events.Bus) scheduler.JobSpec {
	return scheduler.JobSpec{
		Name: expiryJobName, Cadence: 24 * time.Hour, CatchUpMode: scheduler.CatchUpLatestOnly, CatchUpWindow: 24 * time.Hour,
		RunTimeout: time.Minute, StaleTimeout: 5 * time.Minute, HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 3, RetryBackoff: []time.Duration{10 * time.Minute},
		Scopes: scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, _ scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			sent, err := s.sendExpiryReminders(ctx, queries, bus, s.now())
			return scheduler.HandlerResult{RowsAffected: int64(sent)}, err
		},
	}
}

// daysUntil counts whole days from now's UTC date to the expiry date.
func daysUntil(now, expiresAt time.Time) int {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return int(expiresAt.Sub(today).Hours() / 24)
}

// reminderThreshold returns the reminder due with daysLeft days to go: the
// smallest threshold not below daysLeft. None is due once the token expired.
func reminderThreshold(daysLeft int) (threshold int, due bool) {
	if daysLeft < 1 {
		return 0, false
	}
	for _, t := range reminderThresholds {
		if daysLeft <= t {
			return t, true
		}
	}
	return 0, false
}

func (s *Service) sendExpiryReminders(ctx context.Context, queries *db.Queries, bus *events.Bus, now time.Time) (int, error) {
	expiresAt, ok := s.cfg.TokenExpiresAt()
	if !ok {
		return 0, nil
	}
	daysLeft := daysUntil(now, expiresAt)
	threshold, due := reminderThreshold(daysLeft)
	if !due {
		return 0, nil
	}
	recipients, err := queries.ListGitHubTokenExpiryRecipients(ctx)
	if err != nil {
		return 0, fmt.Errorf("list github token expiry recipients: %w", err)
	}
	if len(recipients) == 0 {
		s.logger.Warn("github token expires soon but no workspace has a GitHub repository to notify",
			"token_expires_at", s.cfg.expiryString(), "days_left", daysLeft)
		return 0, nil
	}

	expiry := s.cfg.expiryString()
	details, err := json.Marshal(map[string]string{
		"expires_at":     expiry,
		"threshold_days": strconv.Itoa(threshold),
		"days_left":      strconv.Itoa(daysLeft),
	})
	if err != nil {
		return 0, err
	}
	title := fmt.Sprintf("GitHub token expires in %d days", daysLeft)
	if daysLeft == 1 {
		title = "GitHub token expires tomorrow"
	}
	body := fmt.Sprintf("The GitHub token in %s expires on %s. GitHub issues stop syncing on that date. "+
		"Create a new classic token with the repo scope, set %s and %s, and restart the server.",
		EnvToken, expiry, EnvToken, EnvTokenExpiresAt)

	sent := 0
	for _, r := range recipients {
		already, err := queries.GitHubTokenExpiryReminderSent(ctx, db.GitHubTokenExpiryReminderSentParams{
			WorkspaceID: r.WorkspaceID, RecipientID: r.UserID,
			ExpiresAt: expiry, ThresholdDays: strconv.Itoa(threshold),
		})
		if err != nil {
			return sent, fmt.Errorf("check github token expiry reminder: %w", err)
		}
		if already {
			continue
		}
		item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: r.WorkspaceID,
			RecipientType: "member", RecipientID: r.UserID,
			Type: inboxTypeExpiry, Severity: expiryInboxSeverity, IssueID: pgtype.UUID{},
			Title: title, Body: pgtype.Text{String: body, Valid: true},
			ActorType: pgtype.Text{String: "system", Valid: true}, ActorID: pgtype.UUID{},
			Details: details,
		})
		if err != nil {
			return sent, fmt.Errorf("create github token expiry reminder: %w", err)
		}
		if !inserted {
			continue
		}
		sent++
		publishInboxItem(bus, item)
	}
	return sent, nil
}

func publishInboxItem(bus *events.Bus, item db.InboxItem) {
	if bus == nil {
		return
	}
	bus.Publish(events.Event{
		Type: protocol.EventInboxNew, WorkspaceID: util.UUIDToString(item.WorkspaceID),
		ActorType: "system",
		Payload: map[string]any{"item": map[string]any{
			"id": util.UUIDToString(item.ID), "workspace_id": util.UUIDToString(item.WorkspaceID),
			"recipient_type": item.RecipientType, "recipient_id": util.UUIDToString(item.RecipientID),
			"type": item.Type, "severity": item.Severity, "issue_id": nil,
			"issue_status": nil, "issue_priority": nil,
			"title": item.Title, "body": util.TextToPtr(item.Body), "read": item.Read,
			"archived": item.Archived, "created_at": util.TimestampToString(item.CreatedAt),
			"actor_type": util.TextToPtr(item.ActorType), "actor_id": nil,
			"details": json.RawMessage(item.Details),
		}},
	})
}
