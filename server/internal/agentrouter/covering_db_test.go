// Added by the mouse9527/multica fork: which Super Agents cover an issue (M-194).

package agentrouter

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func (e *env) identifier(issueID string) string {
	e.t.Helper()
	var n int
	if err := pool.QueryRow(e.ctx, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return fmt.Sprintf("RT-%d", n)
}

func (e *env) covering(issueID, reader string, hidden ...string) []Covering {
	e.t.Helper()
	got, err := e.store.Covering(e.ctx, e.ws, issueID, reader, hidden)
	if err != nil {
		e.t.Fatalf("covering: %v", err)
	}
	return got
}

func TestCoveringListsEveryScopeAndWhichOneDelivers(t *testing.T) {
	e := newEnv(t)
	project := e.fx.Project(t, "router project")
	root := e.issue("root", testutil.Cols{"project_id": project})
	child := e.issue("child", testutil.Cols{"project_id": project, "parent_issue_id": root})
	outside := e.issue("outside")
	aProject := e.subscribe(e.agentA, ScopeProject, project)
	aTree := e.subscribe(e.agentA, ScopeIssueTree, root)
	bProject := e.subscribe(e.agentB, ScopeProject, project)
	chat := e.bindChat(aTree, testutil.Cols{"title": "triage chat"})

	got := e.covering(child, e.user)
	ids := make([]string, len(got))
	for i, c := range got {
		ids[i] = c.SubscriptionID
	}
	// The ones that deliver first, by agent name; then the superseded one.
	if want := []string{aTree.ID, bProject.ID, aProject.ID}; !slices.Equal(ids, want) {
		t.Fatalf("covering = %v, want %v", ids, want)
	}
	tree, b, wide := got[0], got[1], got[2]
	if !tree.Effective || tree.ScopeName != e.identifier(root) || tree.AgentName != "router-a" {
		t.Fatalf("tree = %+v", tree)
	}
	if !tree.ChatBound || tree.ChatSessionID == nil || *tree.ChatSessionID != chat || tree.ChatTitle == nil || *tree.ChatTitle != "triage chat" {
		t.Fatalf("the chat's creator must see the chat: %+v", tree)
	}
	if !b.Effective || b.ScopeName != "router project" || b.SupersededBy != nil {
		t.Fatalf("agent B's project = %+v, want it delivering too", b)
	}
	if wide.Effective || wide.SupersededBy == nil || *wide.SupersededBy != aTree.ID {
		t.Fatalf("agent A's project = %+v, want it superseded by the tree", wide)
	}

	// Someone else sees that a chat is bound, not which one.
	other := e.fx.User(t, "router reader", fmt.Sprintf("router-reader-%d@example.test", time.Now().UnixNano()))
	if c := e.covering(child, other)[0]; !c.ChatBound || c.ChatSessionID != nil || c.ChatTitle != nil {
		t.Fatalf("another member's view = %+v, want the chat hidden", c)
	}
	// Scopes in a project the reader cannot see go unnamed.
	for _, c := range e.covering(child, e.user, project) {
		if c.ScopeName != "" {
			t.Fatalf("hidden project: %+v still names its scope", c)
		}
	}

	// A paused subscription still covers the issue but delivers nothing.
	if _, err := e.store.Pause(e.ctx, e.ws, bProject.ID, testChange); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.covering(child, e.user) {
		if c.SubscriptionID == bProject.ID && (c.Enabled || c.Effective || c.SupersededBy != nil) {
			t.Fatalf("paused = %+v", c)
		}
	}

	if got := e.covering(outside, e.user); len(got) != 0 {
		t.Fatalf("an issue in no scope: covering = %+v", got)
	}
}

func TestCreatedIssueScopeTellsTheChatWhereTheIssueGoes(t *testing.T) {
	e := newEnv(t)
	project := e.fx.Project(t, "router project")
	root := e.issue("root")
	aTree := e.subscribe(e.agentA, ScopeIssueTree, root)
	aProject := e.subscribe(e.agentA, ScopeProject, project)
	chat := e.bindChat(aTree)
	// A chat-bound project scope needs an anchor issue of its own.
	board := e.issue("board", testutil.Cols{"project_id": project})
	projectChat := e.fx.ChatSession(t, e.agentA)
	if _, err := e.store.Update(e.ctx, e.ws, aProject.ID, UpdateParams{
		AnchorIssueID: &board, AnchorChatSessionID: &projectChat, Change: testChange,
	}); err != nil {
		t.Fatalf("bind the project scope: %v", err)
	}

	check := func(chatID, issueID string) *CreatedIssueScope {
		t.Helper()
		got, err := e.store.CreatedIssueScope(e.ctx, e.ws, e.agentA, chatID, issueID, e.identifier(issueID))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A sub-issue of the subscribed tree, in another project or none, is in scope.
	child := e.issue("child", testutil.Cols{"parent_issue_id": root})
	if got := check(chat, child); got == nil || !got.InScope || got.Hint != "" || got.SubscriptionID == nil || *got.SubscriptionID != aTree.ID {
		t.Fatalf("child of the tree = %+v, want in scope without a hint", got)
	}

	// The agent's other Super Agent chat receives an issue of its project.
	inProject := e.issue("in project", testutil.Cols{"project_id": project})
	got := check(chat, inProject)
	if got == nil || got.InScope || got.OtherChatSessionID == nil || *got.OtherChatSessionID != projectChat ||
		!strings.Contains(got.Hint, "bound to another chat") {
		t.Fatalf("issue of the other chat's project = %+v", got)
	}

	// An issue in no scope gets the command to add it, after asking.
	loose := e.issue("loose")
	got = check(chat, loose)
	if got == nil || got.InScope || got.SubscriptionID != nil ||
		!strings.Contains(got.Hint, "multica router subscribe --issue "+e.identifier(loose)+" --anchor-chat current") ||
		!strings.Contains(got.Hint, "only after they agree") {
		t.Fatalf("issue in no scope = %+v", got)
	}

	// A chat no subscription is bound to is not a Super Agent: no answer.
	plain := e.fx.ChatSession(t, e.agentA)
	if got := check(plain, loose); got != nil {
		t.Fatalf("plain chat = %+v, want nil", got)
	}

	// A paused tree covers nothing.
	if _, err := e.store.Pause(e.ctx, e.ws, aTree.ID, testChange); err != nil {
		t.Fatal(err)
	}
	if got := check(chat, child); got == nil || got.InScope {
		t.Fatalf("child of a paused tree = %+v, want out of scope", got)
	}
}

func TestOverlapsNameOtherAgentsOnly(t *testing.T) {
	e := newEnv(t)
	project := e.fx.Project(t, "router project")
	root := e.issue("root", testutil.Cols{"project_id": project})
	child := e.issue("child", testutil.Cols{"project_id": project, "parent_issue_id": root})
	e.issue("closed child", testutil.Cols{"project_id": project, "parent_issue_id": child, "status": "done"})
	bProject := e.subscribe(e.agentB, ScopeProject, project)

	overlaps := func(agent, scopeType, scopeID string, hidden ...string) []Overlap {
		t.Helper()
		got, err := e.store.Overlaps(e.ctx, e.ws, agent, scopeType, scopeID, hidden)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := overlaps(e.agentA, ScopeIssueTree, root)
	if len(got) != 1 || got[0].SubscriptionID != bProject.ID || got[0].AgentName != "router-b" ||
		got[0].ScopeName != "router project" || got[0].IssueCount != 3 {
		t.Fatalf("A's tree inside B's project: overlaps = %+v", got)
	}
	// Open issues are named first.
	if want := []string{e.identifier(root), e.identifier(child)}; len(got[0].Issues) != 3 || !slices.Equal(got[0].Issues[:2], want) {
		t.Fatalf("issues = %v, want %v first", got[0].Issues, want)
	}

	// The agent's own scopes never overlap: the narrowest one wins.
	if got := overlaps(e.agentB, ScopeIssueTree, root); len(got) != 0 {
		t.Fatalf("B's own tree: overlaps = %+v", got)
	}

	// Issues the reader cannot see are counted, not named.
	if got := overlaps(e.agentA, ScopeIssueTree, root, project); len(got) != 1 || got[0].IssueCount != 3 || len(got[0].Issues) != 0 {
		t.Fatalf("hidden project: overlaps = %+v", got)
	}

	// The same project overlaps even before it has an issue.
	empty := e.fx.Project(t, "router empty")
	bEmpty := e.subscribe(e.agentB, ScopeProject, empty)
	if got := overlaps(e.agentA, ScopeProject, empty); len(got) != 1 || got[0].SubscriptionID != bEmpty.ID || got[0].IssueCount != 0 {
		t.Fatalf("empty project: overlaps = %+v", got)
	}

	// A paused subscription receives nothing, so it does not overlap.
	if _, err := e.store.Pause(e.ctx, e.ws, bProject.ID, testChange); err != nil {
		t.Fatal(err)
	}
	if got := overlaps(e.agentA, ScopeIssueTree, root); len(got) != 0 {
		t.Fatalf("paused: overlaps = %+v", got)
	}
}
