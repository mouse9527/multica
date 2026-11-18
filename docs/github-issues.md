# GitHub 原生 issue（已撤回）

本文件属于 `mouse9527/multica` fork。

fork 曾经加过「GitHub 原生 issue」：issue 和评论双写到 GitHub、轮询 GitHub 的新评论和新 issue、`owner/repo#N` 标识、PR 轮询、导入命令。**这些功能已在 2026-11-12 撤回**（决定见 Multica 任务 M-70，撤回的代码改动见 M-72）。现在 fork 的 issue 行为与上游一致，所有 issue 都是 local。

## 撤回后的分工

- 使用者日常工作在 Multica。
- GitHub 是公开的证据档案，只放关键决定、设计结论、最终审查和验收结论，由智能体用 `gh` 按规则发布；不再由服务端双写。
- 他人在 GitHub 上的新评论和新 issue，由巡检智能体用 `gh` 增量拉进 Multica；不再由服务端轮询。
- 等 CI 由智能体用 `gh pr checks <PR> --watch` 完成，不依赖 Multica。

## 撤掉了什么

- 出方向：issue / 评论写入 GitHub 的路径、`github_write_intent` 的使用、状态标签映射、adopt / reconcile、上游文件里的双写钩子、新建 issue 时的仓库下拉框、CLI 的 `issue create --repo` / `--provider`。
- 入方向：issue / 评论轮询、每日对账、按需刷新、`multica issue sync`、导入接口（`POST /api/issues/import`、`multica issue import`）、外部评论只读、外部作者身份、PR 轮询（M-30）和 PR 快照的静态令牌来源。
- 标识与展示：`owner/repo#N` 的解析和渲染、响应里的 `provider` / `github` 字段、详情页的 GitHub 链接行。
- 配置：`RepoClient`、仓库白名单、`MULTICA_GITHUB_ISSUES_ENABLED`、`MULTICA_GITHUB_POLL_ENABLED`、`MULTICA_GITHUB_TOKEN`、`MULTICA_GITHUB_TOKEN_EXPIRES_AT` 与令牌状态提醒。服务端不再读取这些环境变量，也不再需要 GitHub 令牌。

## 还留着什么

- **表和迁移**：迁移 `9000`–`9003` 建的 5 张表（`issue_external`、`comment_external`、`github_write_intent`、`github_repo_sync`、`project_github_settings`）留在库里，没有代码读写，只在删除工作区时随工作区清掉（`DeleteWorkspaceGithubIssueData`）。删表要单独的迁移，需要 Owner 对具体 HEAD 批准，部署前先备份数据库并在副本上演练。
- `server/cmd/migrate/github_issue_migrations.go`：这几个迁移的并发建索引清理钩子，迁移文件还在，所以保留。
- `server/internal/migrations/github_issue_tables_migration_test.go`：迁移的 up / down 往返测试和工作区清理测试。
- 已经同步进来的外部评论保留为普通评论；原来托管在 GitHub 的 issue 按 local 渲染，标识仍是 `M-N`。

fork 对上游文件的修改见 [fork 对上游文件的修改清单](github-issues-upstream-changes.md)（分发时附上）。
