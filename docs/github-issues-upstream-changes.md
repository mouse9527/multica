# fork 对上游文件的修改清单

本文件属于 `mouse9527/multica` fork。上游 `multica-ai/multica` 的许可证（`LICENSE`，Apache 2.0 加附加条件）要求：被修改的文件要带有显著的修改说明。分发 fork 的源码或镜像时，请把本清单和 `LICENSE`、`NOTICE` 一起附上。

- **基线**：上游 `5b8a15082`（#73 同步进 fork 的上游 `main`）。下表是 fork `main` 相对这个基线**修改过的上游文件**。fork 新增的文件不在表里，见文末。
- **「文件内说明」** 一列：「有」表示文件里有 fork 的修改说明（形如 `Modified by the mouse9527/multica fork` 或 `mouse9527/multica fork` 的注释）；「无」表示文件里没有，**以本清单作为它的修改说明**。
- **重新生成**：`git diff --name-status 5b8a15082 origin/main | grep '^M'`（两个提交之间用空格、不用 `..`：基线已并入 `main`，要比较两棵树，不是取提交区间）。同步上游之后，基线改为那次同步的上游提交。fork 改动又碰到上游文件时，在同一个 PR 里更新本表。
- **GitHub 原生 issue 已撤回**（M-70，代码改动见 M-72）：它在上游文件里的钩子已全部还原成上游版本，fork 对上游文件的修改从 77 个降到 17 个。说明见 [github-issues.md](github-issues.md)。

## 修改过的上游文件

| 文件 | 任务 | 改动 | 文件内说明 |
|---|---|---|---|
| `.github/workflows/release.yml` | M-58 | `verify` 只在 `multica-ai` 运行，整个上游发布在 fork 上不跑（fork 用 `release-fork.yml`） | 有 |
| `server/cmd/multica/cmd_update.go` | M-58 | Homebrew 安装且更新源不是上游时报错，不跑 `brew upgrade` | 有 |
| `server/internal/cli/update.go` | M-58 | Release 仓库可配置、默认 fork；替换的可执行文件路径经变量解析（测试用） | 有 |
| `server/internal/daemon/auto_update.go` | M-58 | 版本判断接受 `-mouse.N`（两行） | 有 |
| `server/internal/daemon/daemon.go` | M-58 | `runUpdate` 前的 Homebrew 检查 | 有 |
| `server/cmd/server/main.go` | M-85 | 调用 `agentrouter.Register`：`router_v1` 打开时注册 Router 监听器和对账任务，关闭时什么都不注册 | 有 |
| `server/cmd/server/router.go` | M-85 | 挂载 `/api/router`（开关关闭时返回 404） | 有 |
| `server/internal/featureflags/keys.go` | M-85 | 新开关 `router_v1` 与 `RouterV1Enabled` | 有 |
| `server/internal/handler/workspace.go` | M-20、M-85 | 删除工作区时清理迁移 9000 建的 5 张表（功能撤回后表仍在，见 [github-issues.md](github-issues.md)），以及 Router 的 3 张表（迁移 9004） | 有 |
| `server/pkg/db/generated/models.go` | M-20 | sqlc 按迁移 9000 的表生成的模型（生成文件，不手改） | 无（生成） |

### 只改测试的上游文件

| 文件 | 任务 | 改动 | 文件内说明 |
|---|---|---|---|
| `server/internal/cli/update_source_test.go` | M-58 | 适配可配置的 Release 仓库 | 有 |
| `server/internal/handler/workspace_delete_manifest_test.go` | M-20、M-85 | 登记迁移 9000 的 5 张表和迁移 9004 的 3 张表 | 有 |
| `server/internal/handler/autopilot_subscriber_test.go` | M-24 | 删触发器的 DDL 改走 `testutil.ExecDDL` | 有 |
| `server/internal/handler/daemon_runtime_access_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/task_supplement_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/webhook_delivery_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/workspace_delete_fence_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/worktree_claim_gate_test.go` | M-24 | 同上 | 有 |
| `server/internal/service/autopilot_quota_test.go` | M-24 | 同上 | 有 |
| `server/internal/service/delegated_failure_recovery_settled_test.go` | M-24 | 同上 | 有 |

合计：修改过的上游文件 20 个（含 10 个测试文件），其中 1 个（生成的 `models.go`）文件内没有修改说明，以本清单为准。

## fork 新增的文件

下面这些都是 fork 新写的文件，不是对上游文件的修改，许可证意义上属于 fork 自己的代码（仍按上游许可证分发）：

- `server/migrations/9000_*`–`9003_*`：GitHub 原生 issue 的建表迁移（功能已撤回，表暂留）；`server/cmd/migrate/github_issue_migrations.go`：这几个迁移的并发建索引清理；`server/internal/migrations/github_issue_tables_migration_test.go`：迁移往返与工作区清理测试；`server/pkg/db/queries/github_issue.sql` 与 `server/pkg/db/generated/github_issue.sql.go`：只剩工作区清理查询。
- `server/internal/testutil/ddl.go` 及测试：测试里的 DDL 不卡住并行运行的其他测试包（M-24）。
- `.github/workflows/build-image.yml`：fork 的镜像构建（M-58 起 `latest` 只随最新的 `-mouse.N` tag 移动）。
- `.github/workflows/release-fork.yml`、`.goreleaser.fork.yml`、`scripts/fork-release-is-newest*.sh`：fork 的二进制发布与 `latest` 判断（M-58）；`server/internal/cli/update_fork*.go`、`server/internal/daemon/auto_update_fork_test.go`、`server/cmd/multica/cmd_update_fork_test.go`：从 fork Release 更新（M-58）。
- `docs/fork-release.md`：发布与 runner 更新说明（M-58）。
- Router 捕获（M-85，见 [router.md](router.md)）：`server/internal/agentrouter/`；`server/internal/handler/agent_router.go` 及测试；`server/cmd/multica/cmd_router.go` 及测试；`server/cmd/server/agent_router_integration_test.go`；`server/migrations/9004_*`–`9007_*`；`server/cmd/migrate/router_migrations.go`；`server/internal/migrations/router_tables_migration_test.go`；`docs/router.md`。
- `docs/github-issues*.md`。
