-- Modified for the mouse9527/multica fork: password login.
--
-- The password of a user, kept apart from "user": no existing table is
-- altered, and a user without a row here has no password. user_id is the
-- primary key, so a user has at most one credential and lookups need no
-- other index.
--
-- No foreign key (repo rule). Upstream never deletes users; if that changes,
-- the deleting code must remove the row here too.
--
-- password_hash is the PHC string "$argon2id$v=19$m=..,t=..,p=..$salt$hash";
-- it carries the salt and is what verification reads. algo and params repeat
-- the algorithm and parameters it was made with, so an operator can see how
-- many hashes are still on old parameters without parsing hashes.
-- failed_count and locked_until back the per-account lockout; NULL
-- locked_until means not locked.
CREATE TABLE IF NOT EXISTS user_credential (
    user_id       UUID PRIMARY KEY,
    password_hash TEXT NOT NULL,
    algo          TEXT NOT NULL CHECK (algo IN ('argon2id')),
    params        JSONB NOT NULL,
    failed_count  INTEGER NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    locked_until  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
