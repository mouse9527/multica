-- Added by the mouse9527/multica fork: queries of password login
-- (server/internal/passwordauth). Upstream has no such queries. They touch only
-- the fork table user_credential.

-- name: GetUserCredential :one
SELECT * FROM user_credential
WHERE user_id = $1;

-- name: UpsertUserCredential :exec
-- Sets a user's password. A new password also clears the failure count and
-- any lock: whoever set it (the user after a verification-code login, or an
-- admin reset) has proved control of the account another way.
INSERT INTO user_credential (user_id, password_hash, algo, params)
VALUES (@user_id, @password_hash, @algo, @params)
ON CONFLICT (user_id) DO UPDATE SET
    password_hash = EXCLUDED.password_hash,
    algo          = EXCLUDED.algo,
    params        = EXCLUDED.params,
    failed_count  = 0,
    locked_until  = NULL,
    updated_at    = now();

-- name: RehashUserCredential :execrows
-- Replaces the hash after a successful login when the hashing parameters
-- changed. It applies only while the stored hash is still the one that was
-- verified, so a password set concurrently is never overwritten with the old
-- password. Zero rows means the password changed in between.
UPDATE user_credential SET
    password_hash = @new_password_hash,
    algo          = @algo,
    params        = @params,
    updated_at    = now()
WHERE user_id = @user_id
  AND password_hash = @old_password_hash;

-- name: RecordUserCredentialFailure :one
-- Counts one failed password attempt and returns the new count.
UPDATE user_credential SET
    failed_count = failed_count + 1,
    updated_at   = now()
WHERE user_id = $1
RETURNING failed_count;

-- name: LockUserCredential :exec
UPDATE user_credential SET
    locked_until = @locked_until,
    updated_at   = now()
WHERE user_id = @user_id;

-- name: ResetUserCredentialFailures :exec
-- Clears the failure count and lock after a successful login.
UPDATE user_credential SET
    failed_count = 0,
    locked_until = NULL,
    updated_at   = now()
WHERE user_id = $1
  AND (failed_count <> 0 OR locked_until IS NOT NULL);
