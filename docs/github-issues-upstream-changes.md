# fork 对上游文件的修改清单

本文件属于 `mouse9527/multica` fork。上游 `multica-ai/multica` 的许可证（`LICENSE`，Apache 2.0 加附加条件）要求：被修改的文件要带有显著的修改说明。分发 fork 的源码或镜像时，请把本清单和 `LICENSE`、`NOTICE` 一起附上。

- **基线**：上游 `f68c891d3`（fork 开始时的上游 `main`）。下表是 fork `main`（含 M-31）相对这个基线**修改过的上游文件**。fork 新增的文件不在表里，见文末。
- **「文件内说明」** 一列：「有」表示文件里有 fork 的修改说明（形如 `Modified by the mouse9527/multica fork`、`mouse9527/multica fork` 或 `GitHub-native issues (fork)` 的注释）；「无」表示文件里没有，**以本清单作为它的修改说明**。JSON 文件不能写注释；`Dockerfile` 那一行升级按 M-49 的已定决定没有加说明；其余几处是合并时漏掉的。按 Owner 的要求，本任务**不改任何已合并文件的内容**，只在这里补齐。
- **重新生成**：`git diff --name-status f68c891d3..origin/main | grep '^M'`；每个文件涉及的任务：`git log --format=%s f68c891d3..origin/main -- <文件>`。同步上游之后，基线改为那次同步的上游提交。fork 改动又碰到上游文件时，在同一个 PR 里更新本表。

## 修改过的上游文件

| 文件 | 任务 | 改动 | 文件内说明 |
|---|---|---|---|
| `.github/workflows/release.yml` | M-58 | `verify` 只在 `multica-ai` 运行，整个上游发布在 fork 上不跑（fork 用 `release-fork.yml`） | 有 |
| `Dockerfile` | M-49 | 构建镜像升级到 `golang:1.27.4-alpine`（一行）。上游 `6ccbdd178` 已是同一行，下次同步后不再是修改 | 无 |
| `packages/core/api/client.ts` | M-28 | `getProjectGitHubSettings`（404 返回 `null`） | 有 |
| `packages/core/package.json` | M-28 | 导出 `./github-issues` | 无 |
| `packages/core/types/api.ts` | M-28 | `CreateIssueRequest.repo`、`provider` | 有 |
| `packages/views/locales/{en,es,fr,ja,ko,ru,zh-Hans,zh-Hant}/modals.json`（8 个） | M-28 | 新建 issue 对话框的仓库下拉文案 `create_issue.github_repo` | 无（JSON） |
| `packages/views/modals/create-issue.tsx` | M-28 | 仓库下拉、状态调整、提交前检查、请求体带上选择 | 有 |
| `packages/views/modals/create-issue.test.tsx` | M-28 | 上面这些行为的测试 | 有 |
| `server/cmd/multica/cmd_update.go` | M-58 | Homebrew 安装且更新源不是上游时报错，不跑 `brew upgrade` | 有 |
| `server/cmd/multica/cmd_id_resolver.go` | M-22 | 接受 `owner/repo#N` | 有 |
| `server/cmd/multica/cmd_issue.go` | M-22、M-28 | `--fields` 加 `provider`；`issue create` 的 `--repo`、`--provider` 与 422 改写 | 有 |
| `server/cmd/server/activity_listeners.go` | M-22 | 活动记录里的 issue 标识 | 有 |
| `server/cmd/server/main.go` | M-19、M-22、M-23、M-24 | 读配置建 `githubissues.Service`、令牌校验与到期提醒、标识开关、启动轮询 | 有 |
| `server/cmd/server/router.go` | M-18、M-19、M-23、M-26、M-27、M-28、M-29、M-30、M-46、M-56 | 功能开启时注册路由并把 `Service` 接到 handler 的各个钩子字段 | 有 |
| `server/internal/cli/update.go` | M-58 | Release 仓库可配置、默认 fork；替换的可执行文件路径经变量解析（测试用） | 有 |
| `server/internal/daemon/auto_update.go` | M-58 | 版本判断接受 `-mouse.N`（两行） | 有 |
| `server/internal/daemon/daemon.go` | M-58 | `runUpdate` 前的 Homebrew 检查 | 有 |
| `server/internal/handler/activity.go` | M-29 | 时间线接口载入 issue 后触发按需刷新 | 有 |
| `server/internal/handler/comment.go` | M-23、M-27、M-29 | 外部评论只读；评论新建、编辑、删除的双写；评论列表触发按需刷新 | 有 |
| `server/internal/handler/daemon.go` | M-22 | 认领任务时的 issue 标识 | 有 |
| `server/internal/handler/github.go` | M-30 | PR 镜像入参归一化（`prMirrorInput`）、关联 GitHub issue、返回写入错误 | 有 |
| `server/internal/handler/handler.go` | M-22、M-23、M-26、M-27、M-29、M-30 | `Handler` 的 fork 钩子字段；`owner/repo#N` 解析 | 有 |
| `server/internal/handler/issue.go` | M-22、M-26、M-27、M-28、M-29 | `provider` 字段与标识改写；删除时删 `issue_external`；状态映射拒绝钩子；issue 新建、更新的双写；新建选仓库；详情触发按需刷新 | 有 |
| `server/internal/handler/issue_duplicate.go` | M-22 | 构造完一页后改写标识 | 有 |
| `server/internal/handler/issue_system_wakeup.go` | M-22 | 等待中的子 issue 的标识 | 有 |
| `server/internal/handler/issue_table_rows.go` | M-22 | 构造完一页后改写标识 | 有 |
| `server/internal/handler/issue_workflow_entry.go` | M-22 | 工作流提示词里的标识 | 有 |
| `server/internal/handler/plugin_action.go` | M-22 | 构造完一页后改写标识 | 有 |
| `server/internal/handler/pr_auto_complete.go` | M-30 | GitHub issue 的 PR 合并不改本地状态（新决定 `github`） | 有 |
| `server/internal/handler/search_index.go` | M-22 | 构造完一页后改写标识 | 有 |
| `server/internal/handler/squad.go` | M-22 | squad 活跃 issue 的标识 | 有 |
| `server/internal/handler/vcs_webhook.go` | M-30 | 调 `reconcileAutoLinks` 时忽略新增的错误返回值（1 行） | 无 |
| `server/internal/handler/workspace.go` | M-20 | 删除工作区时清理 fork 表 | 有 |
| `server/internal/integrations/ghsnapshot/client.go` | M-25 | PR 快照的静态令牌来源 | 有 |
| `server/internal/service/issue.go` | M-27 | `IssueCreateOpts.InTx`：提交前在创建事务里执行 | 有 |
| `server/internal/service/source_context.go` | M-22 | 来源快照里的 issue 标识改用 `LookupIssueIdentifier`（1 行） | 无 |
| `server/internal/service/task.go` | M-22 | `IssueToMap` 带 `provider`，`IssueToMapResolved` 改写标识 | 有 |
| `server/pkg/db/generated/models.go` | M-20 | sqlc 按 fork 新表生成的模型（生成文件，不手改） | 无（生成） |

### 只改测试的上游文件

这些文件只为让上游测试在 fork 的 CI 里稳定运行（M-24：删触发器的 DDL 改走 `testutil.ExecDDL`；M-30：适配 `mirrorPullRequestForWorkspace` 的新入参；M-20：登记 fork 新表）：

| 文件 | 任务 | 文件内说明 |
|---|---|---|
| `server/internal/cli/update_source_test.go` | M-58 | 有 |
| `server/internal/handler/autopilot_subscriber_test.go` | M-24 | 有 |
| `server/internal/handler/daemon_runtime_access_test.go` | M-24 | 有 |
| `server/internal/handler/task_supplement_test.go` | M-24 | 有 |
| `server/internal/handler/webhook_delivery_test.go` | M-24 | 有 |
| `server/internal/handler/workspace_delete_fence_test.go` | M-24 | 有 |
| `server/internal/handler/worktree_claim_gate_test.go` | M-24 | 有 |
| `server/internal/service/autopilot_quota_test.go` | M-24 | 有 |
| `server/internal/service/delegated_failure_recovery_settled_test.go` | M-24 | 有 |
| `server/internal/handler/webhook_status_resolver_test.go` | M-30 | 无 |
| `server/internal/handler/workspace_delete_manifest_test.go` | M-20 | 有 |

合计：修改过的上游文件 56 个（含 8 个翻译文件与 11 个测试文件），其中 14 个文件内没有修改说明，以本清单为准。

## fork 新增的文件

下面这些都是 fork 新写的文件，不是对上游文件的修改，许可证意义上属于 fork 自己的代码（仍按上游许可证分发）：

- `server/internal/githubissues/`（含 `issuekey/`）：功能主体。
- `server/internal/handler/githubissues_*.go`、`issue_key.go`：handler 侧的钩子实现与测试。
- `server/internal/integrations/ghsnapshot/`：`ratelimit_export.go`、`static_source.go` 及其测试。
- `server/cmd/multica/`：`cmd_issue_github.go`、`cmd_issue_import.go` 及测试；`server/cmd/server/githubissues_*_test.go`；`server/cmd/migrate/github_issue_migrations.go`；`server/internal/service/issue_key.go`；`server/internal/testutil/ddl.go`；`server/internal/migrations/` 下的 fork 测试。
- `server/migrations/9000_*`–`9003_*`：fork 迁移；`server/pkg/db/queries/` 下的 `github_issue.sql`、`githubissues.sql`、`githubissues_write.sql` 与 `server/pkg/db/generated/` 里对应的生成文件。
- `packages/core/github-issues/`、`packages/views/modals/create-issue-github-repo*.tsx`。
- `.github/workflows/build-image.yml`：fork 的镜像构建（M-58 起 `latest` 只随最新的 `-mouse.N` tag 移动）。
- `.github/workflows/release-fork.yml`、`.goreleaser.fork.yml`、`scripts/fork-release-is-newest*.sh`：fork 的二进制发布与 `latest` 判断（M-58）；`server/internal/cli/update_fork*.go`、`server/internal/daemon/auto_update_fork_test.go`、`server/cmd/multica/cmd_update_fork_test.go`：从 fork Release 更新（M-58）。
- `docs/fork-release.md`：发布与 runner 更新说明（M-58）。
- `docs/github-issues*.md`。
