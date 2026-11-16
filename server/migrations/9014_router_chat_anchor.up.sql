-- Modified for the mouse9527/multica fork: Router v6, slice 1 (M-96).
--
-- One new table binding a Router subscription to a chat session: with the
-- router_chat feature on, dispatch writes each batch into that chat instead
-- of starting an anchor run on the anchor issue. router_subscription and
-- router_dispatch stay as they are; dispatch rows of a chat-bound
-- subscription still carry its anchor issue, and their task_id is the chat
-- task. Deleting the row switches the subscription back to anchor runs.
--
-- No foreign keys (repo rule); the primary key is the only index, so there is
-- no concurrent index file. As in 9004, the DDL runs through EXECUTE inside a
-- DO block so sqlc does not see the table.

DO $router$
BEGIN
    EXECUTE $ddl$
    CREATE TABLE IF NOT EXISTS router_chat_anchor (
        subscription_id UUID PRIMARY KEY,
        workspace_id    UUID NOT NULL,
        chat_session_id UUID NOT NULL,
        created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    $ddl$;
END
$router$;
