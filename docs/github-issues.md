# GitHub 原生 issue（fork 专有）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。代码在 `server/internal/githubissues/`。

## 功能开关与环境变量

| 变量 | 取值 | 说明 |
|---|---|---|
| `MULTICA_GITHUB_ISSUES_ENABLED` | `true` 才开启 | 默认关闭。写法同 `MULTICA_VCS_INTEGRATION_ENABLED` |
| `MULTICA_GITHUB_TOKEN` | classic PAT（`repo` 范围） | 未设置时功能整体关闭，不报错 |
| `MULTICA_GITHUB_TOKEN_EXPIRES_AT` | `YYYY-MM-DD` 或 `never` | 令牌在 GitHub 上登记的到期日；缺失或格式不对时功能关闭 |

- 三个条件同时满足才开启。不满足时服务照常启动，只打一行 `github issues disabled (<原因>)`。原因只写变量名，不回显变量值。
- 关闭时服务行为与上游一致：对上游文件的改动只有 `server/cmd/server/router.go` 里读配置并打日志的一行钩子。
- 令牌只保存在 `githubissues.Config` 的未导出字段里。`Config` 在 `fmt`（含 `%#v`）、`slog`、`encoding/json` 下都不输出令牌，测试见 `config_test.go`。
- 不建 `github_connection` 表，不需要主密钥。轮换令牌要改环境变量并重启。

## 迁移号段规则

### 规则

1. fork 自有迁移使用 **9000–9999** 号段，四位数，从 `9000` 起按顺序分配。
2. 文件名 `9NNN_githubissues_<描述>.up.sql` / `.down.sql`，必须成对。
3. 已发布的迁移**不改号、不改名**。
4. fork 迁移只新建自己的表和索引，不修改上游表结构，DDL 不引用上游 900 号及以后迁移才创建的对象。
5. 其余照 `AGENTS.md`「Database and Migration Rules」：无外键；每个 `CONCURRENTLY` 索引单独一个迁移文件，并在 `server/cmd/migrate/main.go` 的 `concurrentIndexCleanups`（down 方向重建索引时是 `concurrentDownIndexCleanups`）登记，否则上游测试 `TestEveryConcurrentUpBuildHasCleanup` 会失败。这是每个索引迁移对上游文件的一行钩子。

`server/internal/githubissues/migrations_rule_test.go` 检查第 1、2 条和下面的排序结论。

### 依据（基于上游 `f68c891d3` 的代码）

| 事实 | 位置 |
|---|---|
| 执行顺序是对完整路径做**字符串排序**（`sort.Strings`），不是按数值；down 方向逆序 | `server/internal/migrations/migrations.go` `Files` |
| 版本号是去掉 `.up.sql` / `.down.sql` 后的完整文件名，例如 `629_user_star_prompt` | `migrations.go` `ExtractVersion` |
| 执行器逐个文件查 `schema_migrations` 里有没有这个版本，没有就执行。不要求连续，不比较大小，所以乱序插入、号段空洞都会被补跑 | `server/cmd/migrate/main.go` `runMigrations` |
| 就绪检查要求磁盘上每个 up 版本都已记录，不只看最后一个 | `server/cmd/server/health.go` `readinessQuery`、`migrations.AllVersions` |
| 129 号起数字前缀必须唯一 | `server/internal/migrations/migrations_lint_test.go` `TestMigrationNumericPrefixesAreUnique` |
| 每个迁移必须有 up 和 down | 同上 `TestMigrationFilesHaveMatchingDirections` |
| sqlc 把整个 `migrations/` 当 schema 读 | `server/sqlc.yaml` |

### 推导

- **不能沿用上游下一个号（630、631…）。** 上游下次同步大概率也用这些号，`TestMigrationNumericPrefixesAreUnique` 会失败；而事后改号会让已应用的数据库把改名后的文件当成新迁移再跑一遍，因为版本是完整文件名。
- **高位号段可行。** 字符串排序下：
  - `9000_…` 排在上游 100–899 之后（首字符 `9` 大于 `1`–`8`），所以 fork 迁移在全新安装时晚于目前所有上游迁移执行。
  - 上游将来的 900–999 号会在 fork 迁移**之后**执行：`9000_…` 排在 `900_…` 前面，因为第四个字符 `0` 小于 `_`。fork 迁移只建自己的表，与上游迁移互不依赖，相对顺序不影响结果（规则第 4 条保证这一点）。
  - fork 号段内都是四位数，字符串序与数值序一致。
  - 已有数据库上，执行器只看版本是否已记录，所以任何时候加入的 fork 迁移都会被执行，同步上游后新增的上游迁移也会被执行。
- **余量。** 上游约 8 个月写到 629 号。按这个速度，上游要多年后才会到 9000。

### 同步上游时要检查

- 上游最大号是否接近 9000（`migrations_rule_test.go` 会在号段被占用时失败）。
- 上游到 1000 号后是否改了排序方式或命名（例如改成数值排序）。数值排序下 fork 号段仍在最后，规则不受影响；如果改成别的方案，重新评估本规则。
- 上游是否新增了「号必须连续」之类的检查。
