# 密码登录（fork）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。代码在 `server/internal/passwordauth/`。实施跟踪见 #42。

本文件随实施阶段补充；当前内容对应阶段 1（#37：数据表、凭据存取、哈希封装）和阶段 2（#38：开关、接口、失败锁定、限流）。

## 数据表

迁移 `9018_passwordauth_user_credential` 新建 `user_credential`，不改现有表（包括 `user`）。fork 的迁移编号从 9000 起，取当时最大号加一；CI 的 `migrations-roundtrip` 会对所有 fork 迁移做 up/down 往返，见 [fork-ci.md](fork-ci.md)。

| 列 | 说明 |
|---|---|
| `user_id` | 主键：一个用户最多一条凭据；没有行就是没设密码 |
| `password_hash` | PHC 字符串 `$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>`，自带盐和参数，校验只读它 |
| `algo` | 目前只允许 `argon2id`（CHECK 约束） |
| `params` | 生成该哈希时的参数（JSON），便于运维统计还有多少哈希停在旧参数，不必解析哈希 |
| `failed_count`、`locked_until` | 按账号的失败计数与锁定，策略见下文「失败锁定与限流」；`locked_until` 为 NULL 表示未锁 |

- 无外键（`AGENTS.md`）。上游目前没有删除用户的代码；以后若有，删除方要一并删这张表的行。
- 唯一性由主键保证，没有其他索引，所以没有 `CONCURRENTLY` 索引迁移，也不需要在 `server/cmd/migrate/main.go` 登记钩子。
- 回滚：`9018_…down.sql` 只 `DROP TABLE user_credential`，会删除所有已设密码；回滚后用户仍可用验证码登录。

## 哈希

argon2id（`golang.org/x/crypto/argon2`）。

| 参数 | 默认值 | 环境变量 |
|---|---|---|
| 内存 | 65536 KiB（64 MiB） | `MULTICA_PASSWORD_ARGON2_MEMORY_KIB` |
| 迭代 | 3 | `MULTICA_PASSWORD_ARGON2_ITERATIONS` |
| 并行度 | 4 | `MULTICA_PASSWORD_ARGON2_PARALLELISM` |
| 同时计算的内存预算 | 262144 KiB（256 MiB） | `MULTICA_PASSWORD_ARGON2_MEMORY_BUDGET_KIB`，见「失败锁定与限流」 |
| 盐 | 16 字节 | 不可配 |
| 输出 | 32 字节 | 不可配 |

依据：默认值取 RFC 9106 第 4 节第二推荐方案（t=3、64 MiB、4 路并行，用于每次哈希不能占 2 GiB 内存的场景）；盐与输出长度取 RFC 9106 推荐的 128 位与 256 位。它高于 OWASP Password Storage Cheat Sheet 的 argon2id 最低配置（19 MiB、t=2、p=1）。

覆盖规则（`ParamsFromEnv`）：内存不低于 19456 KiB 且「内存 × 迭代」不低于 38912（即不低于 OWASP 最低配置的成本）；内存不超过 1 GiB，迭代不超过 64，并行度 1–255。不合规时返回错误，由调用方在启动时失败，不会悄悄用弱参数。

- 校验用常量时间比较（`crypto/subtle`）。
- `NeedsRehash`：存储的哈希参数与当前参数有任何不同（含盐、输出长度）即为真。`Store.CheckPassword` 在密码正确时自动用当前参数重哈希；重哈希只在库里仍是刚校验过的那个哈希时生效，并发改过密码则放弃，新密码优先。重哈希失败只记日志，不影响本次登录。
- 没设密码的账号也做一次等价的哈希计算（`VerifyDummy`），响应时间不暴露「是否设了密码」。

## 密码规则

- 最短 12 个字符（按 Unicode 字符计，不按字节）。
- 最长 1024 字节：防止超大输入消耗 CPU。登录时超长输入一定判错，但仍对前 1024 字节做一次哈希，耗时与普通错误密码、没设密码的账号相同。
- 只在设置密码时校验（`ValidatePassword`、`Hasher.Hash`、`Store.SetPassword`）；登录时按存储的哈希原样比对。

## 不泄露密码与哈希

- 本包返回的错误都是固定值，不含密码、哈希或其片段；数据库错误按 `%w` 包装，`pgconn.PgError.Error()` 不含 `Detail`（Postgres 会把失败行放在 `Detail` 里），`%#v` 打印包装后的错误时内层只显示指针。调用方不要用 `errors.As` 取出 `*pgconn.PgError` 再整体打印或记日志。
- sqlc 生成的 `db.UserCredential` 的 `PasswordHash` 带 `json:"password_hash"`。本包只对外返回 `Credential`；阶段 2 写接口时不要直接序列化生成的 model。
- `Credential` 的哈希是未导出字段；`fmt`（所有动词，含 `%#v`）、`slog`、`encoding/json` 都不输出它。
- 日志只记 `user_id` 和错误。测试见 `store_test.go`（伪造数据库返回带密码与哈希的 `Detail`，断言错误与日志都不含）。

## 开关

环境变量 `MULTICA_PASSWORD_AUTH_ENABLED`，`true` 或 `1` 为开，默认关。

- 关：不注册任何密码路由（请求得到 404），`/api/config` 不含 `password_login_enabled`，行为与上游一致。
- 开：注册下面三个路由，`/api/config` 返回 `"password_login_enabled": true`。
- 开但 argon2id 环境变量不合规：服务照常启动，记一条 error 日志，密码登录保持关闭（同上「关」），不会用弱参数存哈希。

上游文件里的钩子：`server/cmd/server/router.go` 一行 `h.MountPasswordAuth(...)`，`server/internal/handler/handler.go` 的 `PasswordAuth` 字段，`server/internal/handler/config.go` 的 `PasswordLoginEnabled` 字段和一行赋值。其余在 `server/internal/handler/passwordauth.go`、`server/internal/middleware/client_ip_fork.go` 和本包。

## 接口

| 路由 | 鉴权 | 说明 |
|---|---|---|
| `POST /auth/password-login` | 无 | 请求 `{"email","password"}`。成功返回与验证码登录相同的 `{"token","user"}`，并设置相同的 Cookie（`issueJWT`、`SetAuthCookies`、CloudFront Cookie）。不创建用户。CLI 的 `cli_callback` 流程不变：网页登录后照旧换 CLI token |
| `GET /api/me/password` | 登录会话 | `{"has_password": bool}`，设置页据此决定要不要填当前密码 |
| `POST /api/me/password` | 登录会话 | 请求 `{"current_password","new_password"}`。第一次设置只需会话；已有密码时必须给对当前密码。新密码最短 12 个字符。只作用于会话里的现有用户，用户不存在返回 401，不建用户、不建凭据 |

`/api/me/password` 只接受本人的登录会话 JWT：判断的是鉴权中间件实际用的那个凭据（先看 `Authorization`，没有再看 Cookie，与中间件一致），不看它放在哪里。任务令牌（`mat_`）、云节点令牌（`mcn_`）和个人访问令牌（`mul_`）都返回 403，放进 `multica_auth` Cookie 也一样（持有 PAT 的人能自己算出与它绑定的 CSRF 值，所以 CSRF 挡不住），避免一个交给脚本的令牌变成密码。判断在 fork 文件 `server/internal/middleware/session_credential_fork.go`。

错误响应（`code` 供前端翻译）：

| 情况 | 状态码 | `code` |
|---|---|---|
| 邮箱不存在、账号没设密码、密码错误 | 401 | `invalid_credentials`（三者响应体逐字节相同） |
| 账号锁定中、按 IP 限流 | 429 | `too_many_attempts`（两者相同） |
| 新密码太短或太长 | 400 | `invalid_password` |
| 修改时没给当前密码 | 400 | `current_password_required` |
| 修改时当前密码错误 | 403 | `current_password_incorrect` |

上游临时停用名单（`server/internal/auth/temporary_disabled_users.go`）里的用户，密码正确时也返回与验证码登录相同的 403，签发不了会话。

## 失败锁定与限流

不依赖 Redis（现网没有 Redis，上游按 IP 的限流在没有 Redis 时关闭，`router.go` 那段不动）。

- **按账号锁定**（存 `user_credential`）：连续失败 5 次后锁 1 分钟，此后每多失败一次时长翻倍，最长 1 小时。失败次数只在登录成功或设置新密码时清零，所以等锁过期后再猜一次就会锁更久。锁定期间不校验密码，正确密码也返回 429。修改密码时当前密码输错同样计数。
- **没有密码的邮箱也"锁"**：不存在的邮箱和没设密码的账号在进程内按同样规则计数并返回同样的 429，否则"猜 5 次后会不会锁"就能判断某个邮箱有没有密码。这张表只存邮箱的 SHA-256，最多 1 万条；24 小时没有新失败就忘掉（真实账号的计数不会自己清零，因此停一天后两者可能再次不同，探测一次要等一天）；服务重启后清空。
- **按 IP 限流**（进程内）：每个地址每分钟 10 次登录尝试，IPv6 按 /64 计。客户端地址的取法与上游 `RateLimit` 相同（`RATE_LIMIT_TRUSTED_PROXIES` 里的代理才看 `X-Forwarded-For`）。表最多 1 万个地址，满了且都未过期时新地址不计数直接放行（不因地址洪水挡住所有人；按账号锁定仍然有效）。
- **多副本的局限**：IP 限流和"无密码邮箱"的计数都在单个进程内。现网单副本；N 个副本时每个地址最多可得 N 倍配额，同一个无密码邮箱也要在每个副本上分别累计才会"锁"，不再与真实账号完全一致。按账号锁定存在数据库里，不受影响。
- **哈希内存预算**：同一进程里同时进行的 argon2id 计算（登录校验、`VerifyDummy`、设密码、重哈希）占用的内存合计不超过预算 `MULTICA_PASSWORD_ARGON2_MEMORY_BUDGET_KIB`（默认 262144，即 256 MiB；可设 19456 到 67108864）。每次计算按**它实际用的参数**占预算：校验旧哈希时按旧哈希自带的内存计，不按当前配置；预算不够就排队。默认参数下一次约 50 ms、64 MiB，最多 4 个并发。
  - 当前参数的内存必须不超过预算，否则启动时记错误日志、密码登录保持关闭。
  - **单次超预算**：库里某个哈希的内存大于整个预算（运维把预算调到旧参数以下）时不计算它，按密码错误处理：返回 401、做一次 `VerifyDummy` 拉平耗时、不计失败次数，并记一条 error 日志提示调大预算或重置该账号的密码。所以**调低参数时**，预算要先保持不低于旧参数的内存，等活跃账号登录后自动重哈希，再调低预算。

所有失败路径耗时一致：返回 401 的路径都恰好做一次 argon2id 校验（不存在的账号用 `VerifyDummy`），返回 429 的路径都不做。

## 日志

记录事件，不记密码、哈希，也不记尝试的邮箱（攻击者输入，甚至可能是填错位置的密码）：

- 登录成功：`user logged in with password`，带 `user_id`。
- 失败：`password login: wrong password`（带 `user_id`）、`password login: no password for this email`（只带 IP）、`password login: account locked`、`password login: account locked after failures`（带次数和时长）、`password login: rate limited`（带 IP）。
- 设置或修改：`password set`、`password change: wrong current password`，带 `user_id`。

测试对响应体、响应头和日志断言不含测试用的密码与库里的任何哈希（`server/internal/passwordauth/service_db_test.go`、`server/internal/handler/passwordauth_test.go`）。
