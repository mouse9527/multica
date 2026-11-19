# 密码登录（fork）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。代码在 `server/internal/passwordauth/`。实施跟踪见 #42。

本文件随实施阶段补充；当前内容对应阶段 1（#37）：数据表、凭据存取、哈希封装，以及阶段 2 的管理员重置命令（#39）。两者都不含 HTTP 接口，不改任何上游行为。

## 数据表

迁移 `9018_passwordauth_user_credential` 新建 `user_credential`，不改现有表（包括 `user`）。fork 的迁移编号从 9000 起，取当时最大号加一；CI 的 `migrations-roundtrip` 会对所有 fork 迁移做 up/down 往返，见 [fork-ci.md](fork-ci.md)。

| 列 | 说明 |
|---|---|
| `user_id` | 主键：一个用户最多一条凭据；没有行就是没设密码 |
| `password_hash` | PHC 字符串 `$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>`，自带盐和参数，校验只读它 |
| `algo` | 目前只允许 `argon2id`（CHECK 约束） |
| `params` | 生成该哈希时的参数（JSON），便于运维统计还有多少哈希停在旧参数，不必解析哈希 |
| `failed_count`、`locked_until` | 按账号的失败计数与锁定，策略在阶段 2 实现；`locked_until` 为 NULL 表示未锁 |

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

## 管理员重置密码

忘记密码、又没有邮件服务可发验证码时，由管理员在服务端给**已存在的用户**设置或重置密码。命令是镜像里的独立二进制 `reset_password`（源码 `server/cmd/reset_password/`），与 `server`、`migrate` 在同一目录 `/app`。

```bash
# 交互输入（推荐）：提示输入两次，不回显
kubectl exec -it deploy/<后端> -- ./reset_password --email user@example.com

# 非交互：stdin 只放密码一行（可带一个换行）
kubectl exec -i deploy/<后端> -- ./reset_password --email user@example.com < password.txt
```

- 密码**只从 stdin 读**，没有 `--password` 之类的参数，不会进 shell 历史和进程列表。stdin 是终端时关闭回显、输入两次核对（Linux、macOS）；不是终端时整个 stdin 就是密码，只允许末尾一个换行，多行报错。
- 先按邮箱找用户（与登录相同：去首尾空白、转小写），找不到就报错退出，**不创建用户**；找到后才提示输入密码。
- 密码规则与用户自己设置时相同（最短 12 个字符、最长 1024 字节）。成功后失败计数清零、解除锁定（`UpsertUserCredential` 一条语句完成）。
- 读环境变量 `DATABASE_URL`（必需）和 `MULTICA_PASSWORD_ARGON2_*`，在容器里与服务端一致；参数不同也不要紧，用户下次登录时会自动重哈希。
- 输出只有用户 ID，不回显密码。审计：在 stderr 写一行 JSON，`msg` 为 `passwordauth: audit: password reset by admin command`，含 `user_id`、`email`、`at`（UTC 时间），不含密码和哈希。这一行不走业务日志，`LOG_LEVEL` 设成 `warn` 或 `error` 也照样输出；写不出去时命令报错、退出码 1（此时密码已经改了，按报错提示另行留档）。注意 `kubectl exec` 的输出只回到执行人的终端，不进容器日志；需要留档时由执行人保存这条输出。
- 退出码：0 成功；1 失败（用户不存在、密码不合规、数据库错误、审计记录写不出去）；2 用法错误。
- 这个命令不看功能开关：开关只控制登录接口；关闭时设置的密码在打开后生效。
