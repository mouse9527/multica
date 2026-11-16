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
| `server/cmd/multica/cmd_daemon_autostart.go` | M-101 | macOS LaunchAgent 的 `ProcessType` 默认 `Standard`（上游写死 `Background`），可用 `MULTICA_DAEMON_PROCESS_TYPE` 改；非默认值同时写进 plist 的 `EnvironmentVariables` | 有 |
| `server/cmd/multica/cmd_update.go` | M-58 | Homebrew 安装且更新源不是上游时报错，不跑 `brew upgrade` | 有 |
| `server/internal/cli/update.go` | M-58 | Release 仓库可配置、默认 fork；替换的可执行文件路径经变量解析（测试用） | 有 |
| `server/internal/daemon/auto_update.go` | M-58 | 版本判断接受 `-mouse.N`（两行） | 有 |
| `server/internal/daemon/daemon.go` | M-58 | `runUpdate` 前的 Homebrew 检查 | 有 |
| `server/cmd/server/main.go` | M-85、M-86 | 调用 `agentrouter.Register`（M-86 起多传 `taskSvc`）：`router_v1` 打开时注册 Router 监听器和对账任务，`router_dispatch` 也打开时再注册派发任务；关闭时什么都不注册 | 有 |
| `server/cmd/server/router.go` | M-85 | 挂载 `/api/router`（开关关闭时返回 404） | 有 |
| `server/internal/featureflags/keys.go` | M-85、M-89、M-86、M-96 | 新开关 `router_v1` 与 `RouterV1Enabled`；`handoff_reply_wake` 与 `HandoffReplyWakeEnabled`；`router_dispatch` 与 `RouterDispatchEnabled`；`router_chat` 与 `RouterChatEnabled` | 有 |
| `server/internal/handler/comment.go` | M-89 | `computeCommentAgentTriggers` 改名为 `computeUpstreamCommentAgentTriggers`（一行），由 fork 的同名包装函数调用，见 `handoff_reply_wake.go` | 有 |
| `server/internal/handler/workspace.go` | M-20、M-85 | 删除工作区时清理迁移 9000 建的 5 张表（功能撤回后表仍在，见 [github-issues.md](github-issues.md)），以及 Router 的表（迁移 9004、9008，由 `agentrouter.DeleteWorkspaceData` 删除；M-86 新增的 3 张表不再改这个文件） | 有 |
| `server/pkg/db/generated/models.go` | M-20 | sqlc 按迁移 9000 的表生成的模型（生成文件，不手改） | 无（生成） |
| `server/pkg/protocol/messages.go` | M-96 | 新常量 `ChatMessageKindRouterBatch`（`router_batch`） | 有 |
| `server/internal/handler/chat.go` | M-96 | `normalizeMessageKind` 原样透出 `router_batch`（一个 case） | 有 |
| `server/pkg/db/queries/chat.sql` | M-96 | `ListChatSessionsByCreator`、`ListAllChatSessionsByCreator`：预览排除 Router 批次和批次轮次的 `no_response`，未读数不计后者 | 有 |
| `server/pkg/db/generated/chat.sql.go` | M-96 | sqlc 按上面的 `chat.sql` 重新生成（生成文件，不手改） | 有（来自 SQL 注释） |
| `packages/core/api/schemas.ts` | M-96 | 两处 `message_kind` 枚举加 `router_batch` | 有 |
| `packages/core/types/chat.ts` | M-96 | `ChatMessageKind` 加 `router_batch` | 有 |
| `packages/views/chat/components/chat-message-list.tsx` | M-96 | 引入 `router-batch.tsx`：批次消息折叠显示，批次轮次的 `no_response` 不显示（一个 import、一个过滤、一个分支） | 有 |

### 只改测试的上游文件

| 文件 | 任务 | 改动 | 文件内说明 |
|---|---|---|---|
| `server/internal/cli/update_source_test.go` | M-58 | 适配可配置的 Release 仓库 | 有 |
| `server/internal/handler/workspace_delete_manifest_test.go` | M-20、M-85、M-86、M-96 | 登记迁移 9000 的 5 张表、迁移 9004 的 4 张表、迁移 9008 的 3 张表和迁移 9014 的 1 张表 | 有 |
| `server/internal/handler/autopilot_subscriber_test.go` | M-24 | 删触发器的 DDL 改走 `testutil.ExecDDL` | 有 |
| `server/internal/handler/daemon_runtime_access_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/task_supplement_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/webhook_delivery_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/workspace_delete_fence_test.go` | M-24 | 同上 | 有 |
| `server/internal/handler/worktree_claim_gate_test.go` | M-24 | 同上 | 有 |
| `server/internal/service/autopilot_quota_test.go` | M-24 | 同上 | 有 |
| `server/internal/service/delegated_failure_recovery_settled_test.go` | M-24 | 同上 | 有 |

合计：修改过的上游文件 29 个（含 10 个测试文件），其中 1 个（生成的 `models.go`）文件内没有修改说明，以本清单为准。M-96 新增的 7 个上游触点（前端 3 个、`chat.sql` 与生成文件 2 个、`messages.go`、`chat.go`）集中在同一个提交里，同步上游时可以整块重放。

## fork 新增的文件

下面这些都是 fork 新写的文件，不是对上游文件的修改，许可证意义上属于 fork 自己的代码（仍按上游许可证分发）：

- `server/migrations/9000_*`–`9003_*`：GitHub 原生 issue 的建表迁移（功能已撤回，表暂留）；`server/cmd/migrate/github_issue_migrations.go`：这几个迁移的并发建索引清理；`server/internal/migrations/github_issue_tables_migration_test.go`：迁移往返与工作区清理测试；`server/pkg/db/queries/github_issue.sql` 与 `server/pkg/db/generated/github_issue.sql.go`：只剩工作区清理查询。
- `server/internal/testutil/ddl.go` 及测试：测试里的 DDL 不卡住并行运行的其他测试包（M-24）。
- `.github/workflows/build-image.yml`：fork 的镜像构建（M-58 起 `latest` 只随最新的 `-mouse.N` tag 移动）。
- `.github/workflows/release-fork.yml`、`.goreleaser.fork.yml`、`scripts/fork-release-is-newest*.sh`：fork 的二进制发布与 `latest` 判断（M-58）；`scripts/fork-macos-sign*.sh`、`scripts/fork-install-rcodesign.sh`、`.github/workflows/fork-macos-sign.yml`：发布时给 darwin 二进制签名（M-99）；`scripts/fork-install-macos*.sh`：macOS 上核对签名身份后安装 Release 二进制（M-99）；`server/internal/cli/update_fork*.go`、`server/internal/daemon/auto_update_fork_test.go`、`server/cmd/multica/cmd_update_fork_test.go`：从 fork Release 更新（M-58）。
- `docs/fork-release.md`：发布与 runner 更新说明（M-58）。
- Router 捕获（M-85，见 [router.md](router.md)）：`server/internal/agentrouter/`；`server/internal/handler/agent_router.go` 及测试；`server/cmd/multica/cmd_router.go` 及测试；`server/cmd/server/agent_router_integration_test.go`；`server/migrations/9004_*`–`9007_*`；`server/cmd/migrate/router_migrations.go`；`server/internal/migrations/router_tables_migration_test.go`；`docs/router.md`。
- Router 派发与出口（M-86，见 [router.md](router.md)）：`server/internal/agentrouter/` 下的 `dispatch.go`、`notice.go`、`run.go` 及测试；`server/internal/service/router_run.go`（上游包里的新文件，只导出 `EnqueueRouterRun`）；`server/internal/handler/agent_router_dispatch.go` 及测试；`server/cmd/multica/cmd_router_dispatch.go`；`server/cmd/server/agent_router_dispatch_integration_test.go`；`server/migrations/9008_*`–`9013_*`。
- Router chat 载体（M-96，v6 切片 1，开关 `router_chat`，默认关闭，见 [router.md](router.md)）：`server/internal/agentrouter/chat.go` 及测试；`server/internal/service/router_chat_run.go`（上游包里的新文件，只导出 `EnqueueRouterChatRun`、`RouterChatPriority`、`ErrRouterChatUnavailable`）；`server/internal/handler/agent_router_chat_test.go`；`server/migrations/9014_*`；`packages/views/chat/components/router-batch.tsx` 及测试。
- 回复叫醒修正（M-89，开关 `handoff_reply_wake`，默认关闭）：`server/internal/handler/handoff_reply_wake.go` 及测试；`server/internal/issuestatus/handoff_fork.go`；`server/cmd/server/handoff_reply_wake_parity_test.go`。开关打开时，issue 处于「交还给人」的状态（`issueStatusIsHandoff` 为真，例如自定义的 started 状态、`in_review`、`blocked`、`done`）时，成员的任何评论（含讨论串里的回复，不含 note）在上游路由之外再叫醒处理人；智能体的评论不走这条路。
- daemon LaunchAgent 的 ProcessType（M-101）：`server/cmd/multica/cmd_daemon_autostart_process_type.go` 及测试。
- `docs/github-issues*.md`。
