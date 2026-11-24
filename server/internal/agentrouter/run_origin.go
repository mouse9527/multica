// Added by the mouse9527/multica fork: "your own actions" means the Super
// Agent's own runs, not every run of its agent (M-193), and only those runs:
// an action whose run is unknown is someone else's (M-233).

package agentrouter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ownRunSQL is the SQL condition that an action of the subscribed agent came
// from one of the Super Agent's own runs: a turn in the chat the subscription
// is bound to (batch turns and turns the owner starts alike), or a run
// dispatch started for the subscription, system retries included.
//
// Nothing else is the Super Agent's own (M-233): the same agent working an
// issue task (the subscription's anchor issue included) or another chat, the
// server writing in the agent's name (Evolution's issues and comments, an
// autopilot's issue), and an action whose run is unknown (NULL, or not a task
// of this agent) all reach the batch like anyone else's. Every write a Super
// Agent turn makes through the API records its run (comment source_task_id,
// issue origin_id, X-Task-ID on issue events), which is what keeps those
// turns from waking their own Super Agent; the dispatcher's self-loop brake
// (selfLoopBrake) bounds the damage if one ever does not.
//
// The arguments are SQL expressions for the subscription id, the agent id and
// the id of the task the action came from.
//
// The caller still checks that the actor is the subscribed agent.
func ownRunSQL(subID, agentID, taskID string) string {
	return `EXISTS (
	    WITH RECURSIVE own_chain AS (
	        SELECT own_t.id, own_t.agent_id, own_t.chat_session_id, own_t.retry_of_task_id, 0 AS depth
	        FROM agent_task_queue own_t WHERE own_t.id = ` + taskID + `
	        UNION ALL
	        SELECT own_r.id, own_r.agent_id, own_r.chat_session_id, own_r.retry_of_task_id, own_c.depth + 1
	        FROM own_chain own_c JOIN agent_task_queue own_r ON own_r.id = own_c.retry_of_task_id
	        WHERE own_c.depth < 32
	    )
	    SELECT 1 FROM own_chain own_x
	    WHERE own_x.depth = 0 AND own_x.agent_id = ` + agentID + `
	      AND (COALESCE(own_x.chat_session_id = (
	          SELECT own_a.chat_session_id FROM router_chat_anchor own_a WHERE own_a.subscription_id = ` + subID + `), false)
	        OR EXISTS (
	          SELECT 1 FROM own_chain own_y JOIN router_dispatch own_d ON own_d.task_id = own_y.id
	          WHERE own_d.subscription_id = ` + subID + `)))`
}

// TaskChatSession returns the chat session a task ran in, or "" when it is not
// a chat task or does not exist. It is how a request's X-Task-ID tells whether
// the agent is acting from a chat, such as a Super Agent's.
func TaskChatSession(ctx context.Context, q Querier, taskID string) (string, error) {
	var chat *string
	err := q.QueryRow(ctx, `SELECT chat_session_id::text FROM agent_task_queue WHERE id = $1::uuid`, taskID).Scan(&chat)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil || chat == nil {
		return "", err
	}
	return *chat, nil
}
