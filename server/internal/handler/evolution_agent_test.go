// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func withEvolutionFlag(t *testing.T, on bool) {
	t.Helper()
	provider := featureflag.NewStaticProvider()
	provider.Set(evolution.FlagV1, featureflag.Rule{Default: on})
	orig := testHandler.FeatureFlags
	testHandler.FeatureFlags = featureflag.NewService(provider)
	t.Cleanup(func() { testHandler.FeatureFlags = orig })
}

func cleanupEvolution(t *testing.T) {
	t.Helper()
	del := func() {
		testPool.Exec(context.Background(),
			`DELETE FROM agent_invocation_target WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1 AND system_key = $2)`,
			testWorkspaceID, service.EvolutionSystemKey)
		testPool.Exec(context.Background(),
			`DELETE FROM agent WHERE workspace_id = $1 AND system_key = $2`,
			testWorkspaceID, service.EvolutionSystemKey)
	}
	del()
	t.Cleanup(del)
}

// evolutionCall routes a request through MountEvolution as the given
// workspace member, the way the workspace middleware would deliver it.
func evolutionCall(t *testing.T, userID string, req *http.Request) *testutil.Response {
	t.Helper()
	req.Header.Set("X-User-ID", userID)
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      util.MustParseUUID(userID),
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load member row: %v", err)
	}
	req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, member))
	r := chi.NewRouter()
	r.Route("/api/evolution", testHandler.MountEvolution)
	return testutil.Call(t, r.ServeHTTP, req)
}

func enableBody(t *testing.T, language string) map[string]any {
	return map[string]any{"runtime_id": handlerTestRuntimeID(t), "language": language}
}

func countEvolutionAgents(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent WHERE workspace_id = $1 AND system_key = $2`,
		testWorkspaceID, service.EvolutionSystemKey).Scan(&n); err != nil {
		t.Fatalf("count evolution agents: %v", err)
	}
	return n
}

func TestEvolution_FlagOffIsNotFound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, false)

	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution", nil)).Want(http.StatusNotFound)
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))).Want(http.StatusNotFound)
	if n := countEvolutionAgents(t); n != 0 {
		t.Fatalf("flag off must create nothing, found %d evolution agents", n)
	}
}

func TestEnableEvolution_ServerOwnsTheDefinition(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	var before evolutionResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution", nil)).Want(http.StatusOK).JSON(&before)
	if before.Agent != nil {
		t.Fatalf("agent before enable = %+v, want null", before.Agent)
	}

	var resp evolutionResponse
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "zh"))).
		Want(http.StatusCreated).JSON(&resp)
	a := resp.Agent
	if a == nil {
		t.Fatal("enable returned no agent")
	}
	if a.SystemKey != service.EvolutionSystemKey {
		t.Fatalf("system_key = %q, want %q", a.SystemKey, service.EvolutionSystemKey)
	}
	if a.Name != "进化" {
		t.Fatalf("name = %q, want the zh name", a.Name)
	}
	if a.PermissionMode != evolutionAgentPermissionMode {
		t.Fatalf("permission_mode = %q, want %q", a.PermissionMode, evolutionAgentPermissionMode)
	}
	if a.Instructions != "" {
		t.Fatalf("instructions must start empty (the system half is never stored), got %q", a.Instructions)
	}
	if !strings.Contains(a.SystemInstructions, "Evolution agent") {
		t.Fatalf("system_instructions should carry the product prompt, got %q", a.SystemInstructions)
	}
	var kind string
	if err := testPool.QueryRow(context.Background(), `SELECT kind FROM agent WHERE id = $1`, a.ID).Scan(&kind); err != nil {
		t.Fatalf("load kind: %v", err)
	}
	if kind != "user" {
		t.Fatalf("kind = %q, want user", kind)
	}

	var after evolutionResponse
	evolutionCall(t, testUserID, newRequest("GET", "/api/evolution", nil)).Want(http.StatusOK).JSON(&after)
	if after.Agent == nil || after.Agent.ID != a.ID {
		t.Fatalf("GET after enable = %+v, want agent %s", after.Agent, a.ID)
	}
}

func TestEnableEvolution_IsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	var first, second evolutionResponse
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))).
		Want(http.StatusCreated).JSON(&first)
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "ja"))).
		Want(http.StatusOK).JSON(&second)
	if first.Agent.ID != second.Agent.ID {
		t.Fatalf("second enable returned %s, want %s", second.Agent.ID, first.Agent.ID)
	}
	if n := countEvolutionAgents(t); n != 1 {
		t.Fatalf("expected 1 evolution agent, got %d", n)
	}
}

// TestEnableEvolution_ConcurrentEnablesCreateOne is what the advisory lock is
// for: without it every request misses the fast-path lookup and inserts.
func TestEnableEvolution_ConcurrentEnablesCreateOne(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	const n = 8
	runtimeID := handlerTestRuntimeID(t)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := withChatTestWorkspaceCtx(t, newRequest("POST", "/api/evolution/enable",
				map[string]any{"runtime_id": runtimeID, "language": "en"}))
			w := httptest.NewRecorder()
			testHandler.EnableEvolution(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	if created != 1 {
		t.Fatalf("expected exactly one 201, got %d (%v)", created, codes)
	}
	if got := countEvolutionAgents(t); got != 1 {
		t.Fatalf("expected 1 evolution agent, got %d", got)
	}
}

func TestEnableEvolution_OnlyOwnersAndAdmins(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	member := addSecondWorkspaceMember(t, "evolution-member@multica.test")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1`, member)
	})
	evolutionCall(t, member, newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))).Want(http.StatusForbidden)
	// Any member may still read the state.
	evolutionCall(t, member, newRequest("GET", "/api/evolution", nil)).Want(http.StatusOK)

	// An agent acting through a task token cannot enable it either.
	machine := newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))
	machine.Header.Set("X-Actor-Source", "task_token")
	evolutionCall(t, testUserID, machine).Want(http.StatusForbidden)

	if n := countEvolutionAgents(t); n != 0 {
		t.Fatalf("refused enables created %d agents", n)
	}
}

func TestEnableEvolution_RejectsBadInput(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "fr"))).Want(http.StatusBadRequest)
	// Unknown fields are refused: system_key and kind are server-owned.
	body := enableBody(t, "en")
	body["system_key"] = "mika"
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", body)).Want(http.StatusBadRequest)
}

func TestEnableEvolution_NameTakenIsConflict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	withEvolutionFlag(t, true)

	createHandlerTestAgent(t, "Evolution", []byte("[]"))
	evolutionCall(t, testUserID, newRequest("POST", "/api/evolution/enable", enableBody(t, "en"))).Want(http.StatusConflict)
	if n := countEvolutionAgents(t); n != 0 {
		t.Fatalf("conflict created %d agents", n)
	}
}

// TestCreateAgent_CannotForgeEvolutionSystemKey pins that only the enable
// endpoint can mint the agent: the public create API ignores system_key.
func TestCreateAgent_CannotForgeEvolutionSystemKey(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanupEvolution(t)
	const name = "Forged Evolution"
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
	})

	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, newRequest("POST", "/api/agents", map[string]any{
		"name":       name,
		"runtime_id": handlerTestRuntimeID(t),
		"system_key": service.EvolutionSystemKey,
		"kind":       "system",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeAgent(t, w); resp.SystemKey != "" || resp.SystemInstructions != "" {
		t.Fatalf("forged agent carries system_key=%q system_instructions=%q", resp.SystemKey, resp.SystemInstructions)
	}
	if n := countEvolutionAgents(t); n != 0 {
		t.Fatalf("public create minted %d evolution agents", n)
	}
}

func TestSystemInstructionsFor_Evolution(t *testing.T) {
	a := db.Agent{Name: "进化", SystemKey: pgtype.Text{String: service.EvolutionSystemKey, Valid: true}}
	got := systemInstructionsFor(a)
	if !strings.HasPrefix(got, "You are 进化") {
		t.Fatalf("evolution system instructions = %q, want the prompt under the current name", got)
	}
}

// TestClaim_EvolutionLayersSystemPromptOverNotes pins the daemon.go hook: the
// claim carries the product prompt with the workspace's notes under it, and
// the stored notes are left as they were.
func TestClaim_EvolutionLayersSystemPromptOverNotes(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Evolution claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Evolution claim agent")
	const notes = "Focus on the review workflow."
	dbfx.Exec(t, `UPDATE agent SET system_key = $2, instructions = $3 WHERE id = $1`, agentID, service.EvolutionSystemKey, notes)
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)

	var taskID string
	dbfx.QueryRow(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0) RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	got, instr, _, raw := claimAgentInstructionsForTest(t, runtimeID)
	if got != taskID {
		t.Fatalf("claimed %q, want %q: %s", got, taskID, raw)
	}
	if want := service.ComposeEvolutionInstructions("Evolution claim agent", notes); instr != want {
		t.Fatalf("claim instructions =\n%s\nwant\n%s", instr, want)
	}
	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, agentID).Scan(&stored)
	if stored != notes {
		t.Fatalf("stored notes changed to %q", stored)
	}
}
