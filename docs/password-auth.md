# 密码登录（fork）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。代码在 `server/internal/passwordauth/`。实施跟踪见 #42。

本文件随实施阶段补充；当前内容对应阶段 1（#37）：数据表、凭据存取、哈希封装。阶段 1 不含 HTTP 接口，不改任何上游行为。

## 数据表

迁移 `9004_passwordauth_user_credential` 新建 `user_credential`，不改现有表（包括 `user`）。号段规则见 `docs/github-issues.md`「迁移号段规则」。

| 列 | 说明 |
|---|---|
| `user_id` | 主键：一个用户最多一条凭据；没有行就是没设密码 |
| `password_hash` | PHC 字符串 `$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>`，自带盐和参数，校验只读它 |
| `algo` | 目前只允许 `argon2id`（CHECK 约束） |
| `params` | 生成该哈希时的参数（JSON），便于运维统计还有多少哈希停在旧参数，不必解析哈希 |
| `failed_count`、`locked_until` | 按账号的失败计数与锁定，策略在阶段 2 实现；`locked_until` 为 NULL 表示未锁 |

- 无外键（`AGENTS.md`）。上游目前没有删除用户的代码；以后若有，删除方要一并删这张表的行。
- 唯一性由主键保证，没有其他索引，所以没有 `CONCURRENTLY` 索引迁移，也不需要在 `server/cmd/migrate/main.go` 登记钩子。
- 回滚：`9004_…down.sql` 只 `DROP TABLE user_credential`，会删除所有已设密码；回滚后用户仍可用验证码登录。

## 哈希

argon2id（`golang.org/x/crypto/argon2`）。

| 参数 | 默认值 | 环境变量 |
|---|---|---|
| 内存 | 65536 KiB（64 MiB） | `MULTICA_PASSWORD_ARGON2_MEMORY_KIB` |
| 迭代 | 3 | `MULTICA_PASSWORD_ARGON2_ITERATIONS` |
| 并行度 | 4 | `MULTICA_PASSWORD_ARGON2_PARALLELISM` |
| 盐 | 16 字节 | 不可配 |
| 输出 | 32 字节 | 不可配 |

依据：默认值取 RFC 9106 第 4 节第二推荐方案（t=3、64 MiB、4 路并行，用于每次哈希不能占 2 GiB 内存的场景）；盐与输出长度取 RFC 9106 推荐的 128 位与 256 位。它高于 OWASP Password Storage Cheat Sheet 的 argon2id 最低配置（19 MiB、t=2、p=1）。

覆盖规则（`ParamsFromEnv`）：内存不低于 19456 KiB 且「内存 × 迭代」不低于 38912（即不低于 OWASP 最低配置的成本）；内存不超过 1 GiB，迭代不超过 64，并行度 1–255。不合规时返回错误，由调用方在启动时失败，不会悄悄用弱参数。

- 校验用常量时间比较（`crypto/subtle`）。
- `NeedsRehash`：存储的哈希参数与当前参数有任何不同（含盐、输出长度）即为真。`Store.CheckPassword` 在密码正确时自动用当前参数重哈希；重哈希只在库里仍是刚校验过的那个哈希时生效，并发改过密码则放弃，新密码优先。重哈希失败只记日志，不影响本次登录。
- 没设密码的账号也做一次等价的哈希计算（`VerifyDummy`），响应时间不暴露「是否设了密码」。

## 密码规则

- 最短 12 个字符（按 Unicode 字符计，不按字节）。
- 最长 1024 字节：防止超大输入消耗 CPU；登录时超长输入直接判错，不做哈希。
- 只在设置密码时校验（`ValidatePassword`、`Hasher.Hash`、`Store.SetPassword`）；登录时按存储的哈希原样比对。

## 不泄露密码与哈希

- 本包返回的错误都是固定值，不含密码、哈希或其片段；数据库错误按 `%w` 包装，`pgconn.PgError.Error()` 不含 `Detail`（Postgres 会把失败行放在 `Detail` 里）。
- `Credential` 的哈希是未导出字段；`fmt`（所有动词，含 `%#v`）、`slog`、`encoding/json` 都不输出它。
- 日志只记 `user_id` 和错误。测试见 `store_test.go`（伪造数据库返回带密码与哈希的 `Detail`，断言错误与日志都不含）。
