// Added by the mouse9527/multica fork: Evolution P2 (M-136), P4 (M-138).

package handler

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// enableEvolutionAgent enables the workspace's Evolution agent through the
// P1 endpoint and returns its id. Everything the agent's runs leave behind is
// removed when the test ends.
func enableEvolutionAgent(t *testing.T) string {
	t.Helper()
	cleanupEvolution(t)
	withEvolutionFlag(t, true)
	var resp evolutionResponse
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))).
		Want(http.StatusCreated).JSON(&resp)
	agentID := resp.Agent.ID
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN (SELECT issue_id FROM evolution_run WHERE workspace_id = $1)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM evolution_run WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM evolution_cursor WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM evolution_proposal WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM evolution_config_version WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, evolution.NoticeInboxType)
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'evolution' WHERE id = $1`, testWorkspaceID)
	})
	return agentID
}

func evolutionTestConfig() evolution.Config {
	cfg := evolution.DefaultConfig()
	cfg.Enabled = true
	return cfg
}

func putEvolutionConfig(t *testing.T, cfg evolution.Config, ack bool) *testutil.Response {
	t.Helper()
	return evolutionCall(t, testUserID, newRequest("PUT", "/api/evolution/config", map[string]any{
		"config": cfg, "acknowledge_member_notice": ack,
	}))
}

func evolutionCursorCount(t *testing.T, source string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM evolution_cursor WHERE workspace_id = $1 AND source = $2`, testWorkspaceID, source)
}

// backdateEvolutionCursors moves the read position two days back, so fixture
// rows written an hour ago are inside the next run's window.
func backdateEvolutionCursors(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE evolution_cursor SET ts = now() - interval '2 days', item_id = NULL
		WHERE workspace_id = $1 AND source IN ('comment', 'chat')`, testWorkspaceID)
}

func evolutionScheduler(now func() time.Time) *evolution.Scheduler {
	return &evolution.Scheduler{
		DB: testPool, Queries: testHandler.Queries, Issues: testHandler.IssueService,
		Inbox: testHandler.Bus, Flags: testHandler.FeatureFlags, Now: now,
	}
}

func tickEvolution(t *testing.T, now func() time.Time) {
	t.Helper()
	if _, err := evolutionScheduler(now).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

// openEvolutionRun returns the open run's task, marked running the way a
// daemon claim would leave it.
func openEvolutionRunTask(t *testing.T, agentID string) (runID, taskID string) {
	t.Helper()
	var issueID string
	dbfx.QueryRow(t, `SELECT id::text, issue_id::text FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`,
		testWorkspaceID).Scan(&runID, &issueID)
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 ORDER BY created_at DESC LIMIT 1`,
		issueID, agentID).Scan(&taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', dispatched_at = now(), started_at = now() WHERE id = $1`, taskID)
	return runID, taskID
}

func signalsPage(t *testing.T, agentID, taskID, source, after string, limit int) *testutil.Response {
	t.Helper()
	q := url.Values{"source": {source}, "limit": {strconv.Itoa(evolution.MaxPageSize)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		q.Set("after", after)
	}
	req := newRequest("GET", "/api/evolution/signals?"+q.Encode(), nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return evolutionCall(t, testUserID, req)
}

// readAllSignals pages through a source and returns the ids it was served.
func readAllSignals(t *testing.T, agentID, taskID, source string) (map[string]evolution.Signal, evolution.Page) {
	t.Helper()
	seen := map[string]evolution.Signal{}
	after := ""
	for i := 0; i < 50; i++ {
		var page evolution.Page
		signalsPage(t, agentID, taskID, source, after, 0).Want(http.StatusOK).JSON(&page)
		for _, s := range page.Items {
			seen[s.ID] = s
		}
		if !page.HasMore {
			return seen, page
		}
		after = page.NextAfter
	}
	t.Fatalf("%s: too many pages", source)
	return nil, evolution.Page{}
}

// TestEvolutionScheduledRunSeesOnlyWhatItMay is the visibility contract of a
// scheduled run (M-132 decision of 2026-11-18): it reads comments in public
// projects and outside any project, and chats with agents open to every
// member. It never sees private projects (even ones the agent's owner is a
// member of), private agents' chats, excluded projects, its own comments and
// chats, or deleted comments; and what it reads is redacted.
func TestEvolutionScheduledRunSeesOnlyWhatItMay(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	evo := enableEvolutionAgent(t)
	runtimeID := handlerTestRuntimeID(t)

	public := dbfx.Project(t, "evolution public")
	private := dbfx.Project(t, "evolution private", testutil.Cols{"visibility": "private"})
	addProjectMemberRow(t, private, testUserID)
	excluded := dbfx.Project(t, "evolution excluded")

	cfg := evolutionTestConfig()
	cfg.Sources.Chat = true
	cfg.ExcludeProjects = []string{excluded}
	putEvolutionConfig(t, cfg, true).Want(http.StatusOK)
	backdateEvolutionCursors(t)

	hourAgo := testutil.Cols{"created_at": testutil.Raw("now() - interval '1 hour'")}
	withCols := func(extra testutil.Cols) testutil.Cols {
		out := testutil.Cols{"created_at": testutil.Raw("now() - interval '1 hour'")}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	inPublic := dbfx.Issue(t, "evolution signal public", testutil.Cols{"project_id": public})
	inPrivate := dbfx.Issue(t, "evolution signal private", testutil.Cols{"project_id": private})
	inExcluded := dbfx.Issue(t, "evolution signal excluded", testutil.Cols{"project_id": excluded})
	noProject := dbfx.Issue(t, "evolution signal no project")

	cPublic := dbfx.Comment(t, inPublic, "public comment, mail alice@example.com", hourAgo)
	cNoProject := dbfx.Comment(t, noProject, "comment outside any project", hourAgo)
	cPrivate := dbfx.Comment(t, inPrivate, "private project comment", hourAgo)
	cExcluded := dbfx.Comment(t, inExcluded, "excluded project comment", hourAgo)
	cSelf := dbfx.Comment(t, noProject, "the Evolution agent's own report", withCols(testutil.Cols{"author_type": "agent", "author_id": evo}))
	cDeleted := dbfx.Comment(t, noProject, "deleted comment", withCols(testutil.Cols{"deleted_at": testutil.Raw("now()")}))

	publicAgent := dbfx.Agent(t, "evolution signal public agent", runtimeID, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": publicAgent, "target_type": "workspace", "target_id": testWorkspaceID,
	})
	privateAgent := dbfx.Agent(t, "evolution signal private agent", runtimeID)
	message := func(session, role, content string) string {
		return dbfx.Insert(t, "chat_message", withCols(testutil.Cols{
			"chat_session_id": session, "role": role, "content": content,
		}))
	}
	publicChat := dbfx.ChatSession(t, publicAgent)
	mUser := message(publicChat, "user", "my token is mul_abcdefghijklmnop1234")
	mReply := message(publicChat, "assistant", "noted")
	mPrivateAgent := message(dbfx.ChatSession(t, privateAgent), "user", "private agent chat")
	mPrivateProject := message(dbfx.ChatSession(t, publicAgent, testutil.Cols{"project_id": private}), "user", "chat in a private project")
	mSelf := message(dbfx.ChatSession(t, evo), "user", "chat with Evolution itself")

	tickEvolution(t, nil)
	_, taskID := openEvolutionRunTask(t, evo)

	comments, _ := readAllSignals(t, evo, taskID, evolution.SourceComment)
	for _, id := range []string{cPublic, cNoProject} {
		if _, ok := comments[id]; !ok {
			t.Errorf("comment %s should be served", id)
		}
	}
	for name, id := range map[string]string{"private project": cPrivate, "excluded project": cExcluded, "own": cSelf, "deleted": cDeleted} {
		if _, ok := comments[id]; ok {
			t.Errorf("%s comment %s must not be served", name, id)
		}
	}
	if got := comments[cPublic]; strings.Contains(got.Content, "alice@example.com") || got.Redactions == 0 {
		t.Errorf("public comment was not redacted: %+v", got)
	}

	chats, _ := readAllSignals(t, evo, taskID, evolution.SourceChat)
	for _, id := range []string{mUser, mReply} {
		if _, ok := chats[id]; !ok {
			t.Errorf("chat message %s should be served", id)
		}
	}
	for name, id := range map[string]string{"private agent": mPrivateAgent, "private project": mPrivateProject, "own": mSelf} {
		if _, ok := chats[id]; ok {
			t.Errorf("%s chat message %s must not be served", name, id)
		}
	}
	if got := chats[mUser]; strings.Contains(got.Content, "mul_abcdefghijklmnop1234") {
		t.Errorf("chat message was not redacted: %q", got.Content)
	}
}

// TestEvolutionSignalsOnlyForTheRunTask: members, other agents, and the
// Evolution agent outside its open run's task get nothing.
func TestEvolutionSignalsOnlyForTheRunTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	evo := enableEvolutionAgent(t)
	putEvolutionConfig(t, evolutionTestConfig(), false).Want(http.StatusOK)

	member := newRequest("GET", "/api/evolution/signals?source=comment", nil)
	evolutionCall(t, testUserID, member).Want(http.StatusForbidden)

	// Active tasks need a runtime (agent_task_queue_active_requires_runtime).
	runtimeID := handlerTestRuntimeID(t)
	other := dbfx.Agent(t, "evolution signals other agent", runtimeID)
	otherTask := dbfx.Task(t, other, testutil.Cols{
		"status": "running", "runtime_id": runtimeID, "issue_id": dbfx.Issue(t, "evolution other"),
	})
	signalsPage(t, other, otherTask, evolution.SourceComment, "", 0).Want(http.StatusForbidden)

	looseTask := dbfx.Task(t, evo, testutil.Cols{
		"status": "running", "runtime_id": runtimeID, "issue_id": dbfx.Issue(t, "evolution loose"),
	})
	signalsPage(t, evo, looseTask, evolution.SourceComment, "", 0).Want(http.StatusConflict)
}

// TestEvolutionCursorMovesOnlyAfterSuccess: a failed run leaves the cursor,
// so the next run reads the same rows; a successful run that hit its item
// budget moves the cursor to what it read and gets a catch-up run.
func TestEvolutionCursorMovesOnlyAfterSuccess(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	evo := enableEvolutionAgent(t)
	cfg := evolutionTestConfig()
	cfg.MaxItemsPerWindow = evolution.MinItemsPerWindow
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	backdateEvolutionCursors(t)
	var before evolution.Position
	dbfx.QueryRow(t, `SELECT ts FROM evolution_cursor WHERE workspace_id = $1 AND source = 'comment'`, testWorkspaceID).Scan(&before.TS)

	issue := dbfx.Issue(t, "evolution backlog")
	dbfx.Exec(t, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at)
		SELECT $1, $2, 'member', $3, 'backlog comment ' || g, 'comment', now() - interval '1 hour' + g * interval '1 second'
		FROM generate_series(1, $4::int) g`, issue, testWorkspaceID, testUserID, evolution.MinItemsPerWindow+10)

	// Run 1 reads its budget, then fails.
	tickEvolution(t, nil)
	run1, task1 := openEvolutionRunTask(t, evo)
	var first evolution.Page
	signalsPage(t, evo, task1, evolution.SourceComment, "", evolution.MaxPageSize).Want(http.StatusOK).JSON(&first)
	if len(first.Items) != evolution.MinItemsPerWindow || !first.LimitReached || first.HasMore {
		t.Fatalf("first page: %d items, limit_reached=%v, has_more=%v; want the run's whole budget", len(first.Items), first.LimitReached, first.HasMore)
	}
	var again evolution.Page
	signalsPage(t, evo, task1, evolution.SourceComment, "", 0).Want(http.StatusOK).JSON(&again)
	if len(again.Items) != 0 || !again.LimitReached {
		t.Fatalf("after the budget: %d items, limit_reached=%v; want nothing more", len(again.Items), again.LimitReached)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', completed_at = now() WHERE id = $1`, task1)
	tickEvolution(t, nil)
	if got := dbfx.Count(t, `SELECT count(*) FROM evolution_run WHERE id = $1 AND status = 'failed'`, run1); got != 1 {
		t.Fatal("run 1 should be failed")
	}
	cur, ok, err := evolution.GetCursor(context.Background(), testPool, testWorkspaceID, evolution.SourceComment)
	if err != nil || !ok || !cur.TS.Equal(before.TS) || cur.ID != "" {
		t.Fatalf("a failed run moved the cursor: %+v (ok=%v, err=%v), want %v", cur, ok, err, before.TS)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`, testWorkspaceID); n != 0 {
		t.Fatalf("a failed run must not be retried before the next slot, found %d open runs", n)
	}

	// Run 2, at a later day's slot, reads the same rows again and succeeds.
	// Two days ahead is past the next slot whatever the time of day.
	tomorrow := func() time.Time { return time.Now().Add(48 * time.Hour) }
	tickEvolution(t, tomorrow)
	run2, task2 := openEvolutionRunTask(t, evo)
	var second evolution.Page
	signalsPage(t, evo, task2, evolution.SourceComment, "", evolution.MaxPageSize).Want(http.StatusOK).JSON(&second)
	if len(second.Items) == 0 || second.Items[0].ID != first.Items[0].ID {
		t.Fatalf("run 2 should start where run 1 started; got %d items", len(second.Items))
	}
	last := second.Items[len(second.Items)-1]
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, task2)
	tickEvolution(t, tomorrow)
	if got := dbfx.Count(t, `SELECT count(*) FROM evolution_run WHERE id = $1 AND status = 'succeeded'`, run2); got != 1 {
		t.Fatal("run 2 should be succeeded")
	}
	cur, _, _ = evolution.GetCursor(context.Background(), testPool, testWorkspaceID, evolution.SourceComment)
	if cur.ID != last.ID || !cur.TS.Equal(last.CreatedAt) {
		t.Fatalf("cursor = %+v, want the last item run 2 read (%s)", cur, last.ID)
	}
	var kind string
	dbfx.QueryRow(t, `SELECT kind FROM evolution_run WHERE workspace_id = $1 AND status = 'open'`, testWorkspaceID).Scan(&kind)
	if kind != evolution.RunCatchUp {
		t.Fatalf("open run kind = %q, want a catch-up for the rest of the window", kind)
	}
}

// TestEvolutionConfigChatNeedsTheMemberNotice: chat reading starts only with
// the confirmation, which notifies every member first; turning it off
// forgets the position, and turning it on again needs a new confirmation.
// Enabling the agent never turns chat on.
func TestEvolutionConfigChatNeedsTheMemberNotice(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)

	var st evolutionConfigResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.Config.Sources.Chat || st.ChatActive || st.Config.Enabled {
		t.Fatalf("enabling the agent must not start any reading: %+v", st.State)
	}

	cfg := evolutionTestConfig()
	cfg.Sources.Chat = true
	putEvolutionConfig(t, cfg, false).Want(http.StatusConflict)
	if evolutionCursorCount(t, evolution.SourceChat) != 0 {
		t.Fatal("a refused request must not start chat reading")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, evolution.NoticeInboxType); n != 0 {
		t.Fatalf("a refused request sent %d notices", n)
	}

	putEvolutionConfig(t, cfg, true).Want(http.StatusOK).JSON(&st)
	if !st.ChatActive {
		t.Fatal("chat should be read after the confirmation")
	}
	members := dbfx.Count(t, `SELECT count(*) FROM member WHERE workspace_id = $1`, testWorkspaceID)
	notified := dbfx.Count(t, `SELECT count(DISTINCT recipient_id) FROM inbox_item WHERE workspace_id = $1 AND type = $2`, testWorkspaceID, evolution.NoticeInboxType)
	if notified != members {
		t.Fatalf("%d of %d members notified", notified, members)
	}

	// Saving again with chat still on needs no new confirmation.
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)

	cfg.Sources.Chat = false
	putEvolutionConfig(t, cfg, false).Want(http.StatusOK)
	if evolutionCursorCount(t, evolution.SourceChat) != 0 {
		t.Fatal("turning chat off must drop its cursor")
	}
	cfg.Sources.Chat = true
	putEvolutionConfig(t, cfg, false).Want(http.StatusConflict)
}

// TestEvolutionChatWrittenAroundTheEndpointIsNotRead: a configuration that
// asks for chat but reached workspace.settings without the confirmation
// (the upstream workspace update writes settings whole) reads no chat.
func TestEvolutionChatWrittenAroundTheEndpointIsNotRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{evolution}',
		'{"enabled": true, "sources": {"comments": true, "chat": true}}'::jsonb) WHERE id = $1`, testWorkspaceID)
	tickEvolution(t, nil)
	if evolutionCursorCount(t, evolution.SourceChat) != 0 {
		t.Fatal("chat must not be read without the member notice")
	}
	var st evolutionConfigResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.ChatActive || !st.Config.Sources.Chat {
		t.Fatalf("state = %+v; want chat asked for but not active", st.State)
	}
}

// TestEvolutionConfigLimitsAreServerEnforced: out-of-range values are
// refused on PUT and clamped when read from settings written some other way.
func TestEvolutionConfigLimitsAreServerEnforced(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableEvolutionAgent(t)
	for name, mutate := range map[string]func(*evolution.Config){
		"items":    func(c *evolution.Config) { c.MaxItemsPerWindow = evolution.MaxItemsPerWindow + 1 },
		"days":     func(c *evolution.Config) { c.MaxWindowDays = 0 },
		"runs":     func(c *evolution.Config) { c.MaxRunsPerDay = evolution.MaxRunsPerDay + 1 },
		"interval": func(c *evolution.Config) { c.Schedule.IntervalDays = 8 },
		"time":     func(c *evolution.Config) { c.Schedule.Time = "25:00" },
		"timezone": func(c *evolution.Config) { c.Schedule.Timezone = "Mars/Olympus" },
		"exclude":  func(c *evolution.Config) { c.ExcludeAgents = []string{"not-a-uuid"} },
	} {
		cfg := evolutionTestConfig()
		mutate(&cfg)
		if resp := putEvolutionConfig(t, cfg, false); resp.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.Code)
		}
	}

	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{evolution}',
		'{"max_items_per_window": 999999, "max_window_days": 365, "max_runs_per_day": 0}'::jsonb) WHERE id = $1`, testWorkspaceID)
	var st evolutionConfigResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution/config", nil)).Want(http.StatusOK).JSON(&st)
	if st.Config.MaxItemsPerWindow != evolution.MaxItemsPerWindow || st.Config.MaxWindowDays != evolution.MaxWindowDays ||
		st.Config.MaxRunsPerDay != evolution.MinRunsPerDay {
		t.Fatalf("stored values were not clamped: %+v", st.Config)
	}

	agentReq := newRequest("PUT", "/api/evolution/config", map[string]any{"config": evolutionTestConfig()})
	agentReq.Header.Set("X-Actor-Source", "task_token")
	evolutionCall(t, testUserID, agentReq).Want(http.StatusForbidden)
}
