// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package agentrouter

import (
	"fmt"
	"testing"
)

func TestParseInboxResolveConfig(t *testing.T) {
	owner := "6F1C2D1E-6A55-4C5B-9E1B-2B7F6B8A9C10"
	for name, tc := range map[string]struct {
		settings string
		want     InboxResolveConfig
		active   bool
	}{
		"no settings":    {settings: `{}`, want: InboxResolveConfig{}},
		"unreadable":     {settings: `not json`, want: InboxResolveConfig{}},
		"removed (null)": {settings: `{"router_inbox_resolve": null}`, want: InboxResolveConfig{WaitingStatuses: []string{}}},
		"bad owner":      {settings: `{"router_inbox_resolve": {"owner_user_id": "me"}}`, want: InboxResolveConfig{}},
		"normalized": {
			settings: fmt.Sprintf(`{"router_inbox_resolve": {"owner_user_id": %q, "waiting_statuses": [" Started_2 ", "started_2", ""]}}`, owner),
			want:     InboxResolveConfig{OwnerUserID: "6f1c2d1e-6a55-4c5b-9e1b-2b7f6b8a9c10", WaitingStatuses: []string{"started_2"}},
			active:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseInboxResolveConfig([]byte(tc.settings))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) || got.Active() != tc.active {
				t.Fatalf("got %+v (active %v), want %+v (active %v)", got, got.Active(), tc.want, tc.active)
			}
		})
	}
}

func TestIssueUpdateLeaveReason(t *testing.T) {
	cfg := InboxResolveConfig{OwnerUserID: "owner", WaitingStatuses: []string{"started_2"}}
	member, agent, owner, other := "member", "agent", "owner", "other"
	update := func(status string, statusChanged bool, prev string, aType, aID, pType, pID *string) issueUpdate {
		var u issueUpdate
		u.Issue.Status, u.StatusChanged, u.PrevStatus = status, statusChanged, prev
		u.Issue.AssigneeType, u.Issue.AssigneeID = aType, aID
		u.AssigneeChanged = pType != nil || pID != nil
		u.PrevAssigneeType, u.PrevAssigneeID = pType, pID
		return u
	}
	for name, tc := range map[string]struct {
		u    issueUpdate
		want string
	}{
		"left the waiting status":      {update("todo", true, "started_2", nil, nil, nil, nil), ArchiveReasonLeftStatus},
		"entered the waiting status":   {update("started_2", true, "todo", nil, nil, nil, nil), ""},
		"between other statuses":       {update("in_review", true, "in_progress", nil, nil, nil, nil), ""},
		"status flag off":              {update("todo", false, "started_2", nil, nil, nil, nil), ""},
		"unassigned from the owner":    {update("todo", false, "", &agent, &other, &member, &owner), ArchiveReasonUnassigned},
		"cleared from the owner":       {update("todo", false, "", nil, nil, &member, &owner), ArchiveReasonUnassigned},
		"from another member":          {update("todo", false, "", &agent, &other, &member, &other), ""},
		"from an agent with owner id":  {update("todo", false, "", nil, nil, &agent, &owner), ""},
		"assigned to the owner (gain)": {update("todo", false, "", &member, &owner, &agent, &other), ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.u.leaveReason(cfg); got != tc.want {
				t.Fatalf("leaveReason = %q, want %q", got, tc.want)
			}
		})
	}
}
