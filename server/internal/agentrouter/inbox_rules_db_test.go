// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).

package agentrouter

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ruleWrite is one inbox write for the rule tests.
type ruleWrite struct {
	issue, typ, severity, details string
}

func (e *env) writeRuleItem(recipient string, w ruleWrite) string {
	e.t.Helper()
	p := db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: util.MustParseUUID(e.ws), RecipientType: "member",
		RecipientID: util.MustParseUUID(recipient), IssueID: util.MustParseUUID(w.issue),
		Type: w.typ, Severity: w.severity, Title: "item " + w.typ, Details: []byte(w.details),
	}
	p.ActorType.String, p.ActorType.Valid = "agent", true
	p.ActorID = util.MustParseUUID(e.agentB)
	item, inserted, err := inbox.CreateAndAnnounce(e.ctx, db.New(pool), nil, p, nil)
	if err != nil || !inserted {
		e.t.Fatalf("inbox write %s: inserted=%v err=%v; shadow mode must not keep items out", w.typ, inserted, err)
	}
	return util.UUIDToString(item.ID)
}

// ruleEnv is an issue tree the owner's chat-bound subscription covers.
func newRuleEnv(t *testing.T) (*env, string) {
	e := newEnv(t)
	root := e.issue("rules root")
	e.bindChat(e.subscribe(e.agentA, ScopeIssueTree, root))
	return e, root
}

func (e *env) setDefaultRules() {
	e.t.Helper()
	cfg, err := DefaultInboxRules([]string{"started_2"}).Normalize()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := SetInboxRulesConfig(e.ctx, pool, e.ws, &cfg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) holdByItem() map[string]Hold {
	e.t.Helper()
	out := map[string]Hold{}
	for _, h := range e.holds() {
		out[h.InboxItemID] = h
	}
	return out
}

var ruleWrites = []ruleWrite{
	{typ: "status_changed", severity: "info", details: `{"from":"in_progress","to":"started_2"}`},
	{typ: "status_changed", severity: "info", details: `{"from":"started_2","to":"in_review"}`},
	{typ: "new_comment", severity: "info", details: `{"comment_id":"c1"}`},
	{typ: "mentioned", severity: "info", details: `{"comment_id":"c1"}`},
	{typ: "task_failed", severity: "action_required", details: `{}`},
	{typ: "issue_assigned", severity: "action_required", details: `{}`},
	{typ: "priority_changed", severity: "info", details: `{"from":"low","to":"high"}`},
	{typ: "issue_created", severity: "info", details: `{}`},
}

// inboxSnapshot is what the owner sees of their items, ids and times aside.
func (e *env) inboxSnapshot() []string {
	e.t.Helper()
	rows, err := pool.Query(e.ctx, `
		SELECT type || '|' || severity || '|' || title || '|' || COALESCE(details::text, '') || '|' || read || '|' || archived
		FROM inbox_item WHERE workspace_id = $1 ORDER BY created_at, id`, e.ws)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestInboxRulesRecordAVerdictPerItem(t *testing.T) {
	e, root := newRuleEnv(t)
	e.setDefaultRules()
	h := e.shadowHold()
	h.ClassifyByRules()

	want := map[string]InboxVerdict{}
	verdicts := []InboxVerdict{
		{VerdictOwner, "enter_waiting_status"}, {VerdictHandle, "status_progress"}, {VerdictHandle, "comment"},
		{VerdictGray, "mention"}, {VerdictGray, "failure"}, {VerdictOwner, "action_required"},
		{VerdictHandle, "field_update"}, {VerdictOwner, RuleUnmatched},
	}
	for i, w := range ruleWrites {
		w.issue = root
		want[e.writeRuleItem(e.user, w)] = verdicts[i]
	}
	got := e.holdByItem()
	if len(got) != len(want) {
		t.Fatalf("holds = %d, want %d", len(got), len(want))
	}
	for id, v := range want {
		x := got[id]
		if x.Verdict != v.Verdict || x.Rule != v.Rule || x.Mode != HoldModeShadow || x.Status != HoldStatusShadow || !x.InInbox {
			t.Fatalf("hold for %s = %+v, want %+v", id, x, v)
		}
	}
	var decided int
	if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM router_hold WHERE workspace_id = $1 AND decided_at IS NOT NULL`, e.ws).Scan(&decided); err != nil || decided != len(want) {
		t.Fatalf("rows with decided_at = %d (%v)", decided, err)
	}
	st := h.Stats()
	if st.Rules == nil || st.Rules.Failures != 0 || st.Rules.Verdicts[VerdictHandle] != 3 ||
		st.Rules.Verdicts[VerdictOwner] != 3 || st.Rules.Verdicts[VerdictGray] != 2 || st.Errors != 0 {
		t.Fatalf("stats = %+v rules = %+v", st, st.Rules)
	}
}

// The owner's inbox is the same with rules on and off; only the shadow rows
// differ.
func TestInboxRulesLeaveTheInboxUnchanged(t *testing.T) {
	e, root := newRuleEnv(t)
	e.setDefaultRules()
	write := func() []string {
		for _, w := range ruleWrites {
			w.issue = root
			e.writeRuleItem(e.user, w)
		}
		snap := e.inboxSnapshot()
		e.fx.Exec(t, `DELETE FROM inbox_item WHERE workspace_id = $1`, e.ws)
		e.fx.Exec(t, `DELETE FROM router_hold WHERE workspace_id = $1`, e.ws)
		return snap
	}
	e.shadowHold()
	off := write()
	h := e.shadowHold()
	h.ClassifyByRules()
	on := write()
	if fmt.Sprint(off) != fmt.Sprint(on) || len(on) != len(ruleWrites) {
		t.Fatalf("inbox with rules off:\n%v\nwith rules on:\n%v", off, on)
	}
}

func TestInboxRulesOff(t *testing.T) {
	t.Run("flag off", func(t *testing.T) {
		e, root := newRuleEnv(t)
		e.setDefaultRules()
		h := e.shadowHold() // no ClassifyByRules: router_inbox_rules off
		e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "new_comment", severity: "info", details: `{}`})
		for _, x := range e.holds() {
			if x.Verdict != "" || x.Rule != "" {
				t.Fatalf("hold = %+v, want no verdict", x)
			}
		}
		if h.Stats().Rules != nil {
			t.Fatalf("rules stats = %+v, want nil", h.Stats().Rules)
		}
		if _, ok := CurrentInboxRulesStats(); ok {
			t.Fatal("CurrentInboxRulesStats reports rules on")
		}
	})
	t.Run("workspace without rules", func(t *testing.T) {
		e, root := newRuleEnv(t)
		h := e.shadowHold()
		h.ClassifyByRules()
		e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "new_comment", severity: "info", details: `{}`})
		got := e.holds()
		if len(got) != 1 || got[0].Verdict != "" {
			t.Fatalf("holds = %+v, want one without a verdict", got)
		}
		if st := h.Stats().Rules; st.Failures != 0 || len(st.Verdicts) != 0 {
			t.Fatalf("rules stats = %+v", st)
		}
	})
}

func TestInboxRulesFailureOnlyCounts(t *testing.T) {
	e, root := newRuleEnv(t)
	e.setDefaultRules()
	h := e.shadowHold()
	h.ClassifyByRules()
	h.rules.rule = func(InboxRulesConfig, db.CreateInboxItemParams) (InboxVerdict, error) {
		return InboxVerdict{}, errors.New("boom")
	}
	e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "new_comment", severity: "info", details: `{}`})
	h.rules.rule = func(InboxRulesConfig, db.CreateInboxItemParams) (InboxVerdict, error) { panic("boom") }
	e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "new_comment", severity: "info", details: `{}`})
	h.rules.rule = ClassifyInboxItem
	// Details that cannot say which status the issue moved to.
	e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "status_changed", severity: "info", details: `{"from":"todo"}`})

	got := e.holds()
	if len(got) != 3 {
		t.Fatalf("holds = %+v, want all three items recorded", got)
	}
	for _, x := range got {
		if x.Verdict != "" || !x.InInbox {
			t.Fatalf("hold = %+v, want in the inbox without a verdict", x)
		}
	}
	st := h.Stats()
	if st.Rules.Failures != 3 || st.Errors != 0 || st.Panics != 0 || st.WouldHold != 3 {
		t.Fatalf("stats = %+v rules = %+v: a rule failure is counted apart from interceptor failures", st, st.Rules)
	}
}

func TestInboxRulesConfigChangeTakesEffectAtOnce(t *testing.T) {
	e, root := newRuleEnv(t)
	e.setDefaultRules()
	h := e.shadowHold()
	h.ClassifyByRules()
	first := e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "issue_created", severity: "info", details: `{}`})
	cfg := InboxRulesConfig{Rules: []InboxRule{{Name: "new_issue", Types: []string{"issue_created"}, Verdict: VerdictHandle}}}
	if err := SetInboxRulesConfig(e.ctx, pool, e.ws, &cfg); err != nil {
		t.Fatal(err)
	}
	second := e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "issue_created", severity: "info", details: `{}`})
	if err := SetInboxRulesConfig(e.ctx, pool, e.ws, nil); err != nil {
		t.Fatal(err)
	}
	third := e.writeRuleItem(e.user, ruleWrite{issue: root, typ: "issue_created", severity: "info", details: `{}`})
	got := e.holdByItem()
	if got[first].Rule != RuleUnmatched || got[second].Rule != "new_issue" || got[third].Verdict != "" {
		t.Fatalf("first %+v, second %+v, third %+v", got[first], got[second], got[third])
	}
}

func TestInboxRulesReport(t *testing.T) {
	e, root := newRuleEnv(t)
	e.setDefaultRules()
	h := e.shadowHold()
	h.ClassifyByRules()
	other := e.fx.User(t, "rules other", fmt.Sprintf("rules-other-%d@example.test", time.Now().UnixNano()))
	e.fx.Member(t, e.ws, other, "member")

	issues := make([]string, 6)
	for i := range issues {
		issues[i] = e.issue(fmt.Sprintf("rules %d", i), testutil.Cols{"parent_issue_id": root})
	}
	// at moves an item (and its hold row) hoursAgo into the past.
	at := func(item string, hoursAgo int) {
		e.fx.Exec(t, `UPDATE inbox_item SET created_at = now() - make_interval(hours => $2) WHERE id = $1`, item, hoursAgo)
		e.fx.Exec(t, `UPDATE router_hold SET created_at = now() - make_interval(hours => $2) WHERE inbox_item_id = $1`, item, hoursAgo)
	}
	ownerComment := func(issue string, hoursAgo int) {
		e.comment(issue, testutil.Cols{"author_type": "member", "author_id": e.user,
			"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%d hours'", hoursAgo))})
	}
	ownerChange := func(issue string, hoursAgo int) {
		e.fx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
			VALUES ($1, $2, 'member', $3, 'status_changed', '{"from":"started_2","to":"todo"}', now() - make_interval(hours => $4))`,
			e.ws, issue, e.user, hoursAgo)
	}
	statusTo := func(to string) string { return `{"from":"in_progress","to":"` + to + `"}` }

	// 0: handled status change the owner commented on within the window.
	at(e.writeRuleItem(e.user, ruleWrite{issues[0], "status_changed", "info", statusTo("in_review")}), 48)
	ownerComment(issues[0], 47)
	// 1: handled comment, no owner action.
	at(e.writeRuleItem(e.user, ruleWrite{issues[1], "new_comment", "info", `{}`}), 48)
	// An agent's comment is not the owner acting.
	e.comment(issues[1], testutil.Cols{"author_type": "agent", "author_id": e.agentB,
		"created_at": testutil.Raw("now() - interval '47 hours'")})
	// 2: a handled priority change, then the issue enters the waiting
	// status; the owner's change after that belongs to the second item.
	at(e.writeRuleItem(e.user, ruleWrite{issues[2], "priority_changed", "info", `{}`}), 48)
	at(e.writeRuleItem(e.user, ruleWrite{issues[2], "status_changed", "info", statusTo("started_2")}), 47)
	ownerChange(issues[2], 46)
	// 3: handled comment the owner answered only after the window.
	at(e.writeRuleItem(e.user, ruleWrite{issues[3], "new_comment", "info", `{}`}), 48)
	ownerComment(issues[3], 20)
	// 4: handled comment still inside its window.
	e.writeRuleItem(e.user, ruleWrite{issues[4], "new_comment", "info", `{}`})
	ownerComment(issues[4], 0)
	// 5: a gray mention, no action.
	at(e.writeRuleItem(e.user, ruleWrite{issues[5], "mentioned", "info", `{}`}), 30)
	// Another member commenting is not the owner acting.
	e.comment(issues[1], testutil.Cols{"author_type": "member", "author_id": other,
		"created_at": testutil.Raw("now() - interval '47 hours'")})
	// copyRow copies one of the owner's rows with a new item id that never
	// reached the inbox.
	copyRow := func(recipient, note any) {
		e.fx.Exec(t, `INSERT INTO router_hold (workspace_id, subscription_id, agent_id, recipient_user_id, inbox_item_id, issue_id,
		        item_type, severity, title, item, mode, status, decide_micros, decision_note)
		    SELECT workspace_id, subscription_id, agent_id, $2::uuid, gen_random_uuid(), issue_id,
		        item_type, severity, title, item, mode, status, decide_micros, $3::text
		    FROM router_hold WHERE workspace_id = $1 AND recipient_user_id = $4 LIMIT 1`, e.ws, recipient, note, e.user)
	}
	copyRow(e.user, "rule:handle:comment")
	// Another member's row, recorded without a verdict.
	copyRow(other, nil)

	rep, err := e.store.InboxRulesReport(e.ctx, RulesReportFilter{WorkspaceID: e.ws, RecipientUserID: e.user})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RuleReport{}
	for _, r := range rep.Rules {
		got[r.Verdict+":"+r.Rule] = r
	}
	for key, want := range map[string][3]int64{ // samples, acted, pending
		"handle:status_progress":     {1, 1, 0},
		"handle:comment":             {2, 0, 1},
		"handle:field_update":        {1, 0, 0},
		"owner:enter_waiting_status": {1, 1, 0},
		"gray:mention":               {1, 0, 0},
	} {
		r := got[key]
		if [3]int64{r.Samples, r.Acted, r.Pending} != want {
			t.Errorf("%s = %+v, want samples/acted/pending %v", key, r, want)
		}
	}
	if len(rep.Rules) != 5 {
		t.Errorf("rules = %+v, want 5", rep.Rules)
	}
	if len(rep.Verdicts) == 0 || rep.Verdicts[0].Verdict != VerdictHandle || rep.Verdicts[0].Samples != 4 ||
		rep.Verdicts[0].Acted != 1 || rep.Verdicts[0].Rate == nil || *rep.Verdicts[0].Rate != 0.25 {
		t.Errorf("verdicts = %+v, want handle 1 of 4 first", rep.Verdicts)
	}
	if rep.NotDelivered != 1 || rep.Unclassified != 0 || rep.Definition == "" || rep.Window != "24h0m0s" {
		t.Errorf("report = %+v", rep)
	}

	// The other member sees only their own row, unclassified.
	theirs, err := e.store.InboxRulesReport(e.ctx, RulesReportFilter{WorkspaceID: e.ws, RecipientUserID: other})
	if err != nil || len(theirs.Rules) != 0 || theirs.Unclassified != 1 {
		t.Fatalf("other member's report = %+v (%v)", theirs, err)
	}
	// A longer window takes in the late comment.
	wide, err := e.store.InboxRulesReport(e.ctx, RulesReportFilter{WorkspaceID: e.ws, RecipientUserID: e.user, Window: 30 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range wide.Rules {
		if r.Rule == "comment" && (r.Samples != 2 || r.Acted != 1) {
			t.Fatalf("comment with a 30h window = %+v, want 1 of 2", r)
		}
	}
	for _, f := range []RulesReportFilter{
		{WorkspaceID: e.ws, Window: 30 * time.Minute},
		{WorkspaceID: e.ws, Window: 8 * 24 * time.Hour},
		{WorkspaceID: e.ws, Since: 12 * time.Hour},
		{WorkspaceID: e.ws, Since: 31 * 24 * time.Hour},
	} {
		if _, err := e.store.InboxRulesReport(e.ctx, f); !errors.Is(err, ErrInvalid) {
			t.Errorf("filter %+v: err = %v, want ErrInvalid", f, err)
		}
	}
}
