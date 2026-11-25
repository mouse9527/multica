# 同步上游时 fork 侧要注意的钩子

本文件属于 `mouse9527/multica` fork（上游没有）。这里记录同步上游时 fork 侧的钩子：上游新加了某个文件或接口，fork 要在同步 PR 里补的 fork 文件，以及历次同步里文本合并不报、但语义会断的地方。

做法：从 fork `main` 切同步分支，`git merge --no-ff upstream/main`，保留合并提交（2026-11-12 #78 起）。合并提交里只解决冲突，语义修复各自单独提交。

## 私有项目路由登记（M-166，M-277 已接上）

上游 `server/cmd/server/private_project_routes_test.go`（`TestEveryRouteByProjectObjectIDHidesPrivateProjects`）遍历全部路由，要求每个路由参数都登记过——**包括它不探测的路由**：参数没登记就先报错，不看「不探测」名单。fork 的登记写在 `server/cmd/server/fork_private_project_routes_test.go`，不进上游的表，由 `server/cmd/server/fork_private_project_routes_hook_test.go` 的 `init` 合并进去：

- `forkRouteParamsOutsideProjects` → `routeParamsOutsideProjects`：不在项目内的对象。`subscriptions/{id}`（Super Agent 订阅是工作区级对象，`issue_tree` 范围可以跨多个项目；它触达的 issue 和事件对项目外的调用者按 issue 过滤掉，`TestRouterSubscriptionFiltersPrivateProjects`），以及 MCP OAuth 的 `mcp-oauth/{name}`、`mcp-oauth/{serverId}`。上游已有的键（`agents/{id}`、`workspaces/{id}`）以上游为准，钩子不覆盖。
- `forkIssueRouteParams` → `projectScopedRouteParams`：指向 issue 的参数，解析成探测夹具里的 issue。现在只有 Evolution 的 `proposals/{issueId}`。
- `forkPrivateProjectRoutesNotProbed` → `privateProjectRoutesNotProbed`：带项目内参数、但上游探测判断不了的路由。上游的探测路由不开功能开关，`/api/router`、`/api/evolution` 全部 404，项目成员那一侧的对照必然失败；分别由 `TestRouterIssueCoveringHidesPrivateProjects`、`TestEvolutionProposalInPrivateProject` 在开着开关时做同样的探测。

`TestForkPrivateProjectRouteRegistrations` 按上游同样的规则检查 fork 路由：每个参数要么在 fork 表里、要么上游已登记；带项目内参数的 fork 路由必须列为「不探测」；每条登记都还在用。包级变量在所有 `init` 之前初始化，所以钩子改到的是上游已经建好的表；上游文件一行不改。上游如果改了这几个表的名字或类型，按新名字改钩子文件即可。

## `pkg/remotemcp` 的内网 origin 放行（M-230）

上游文件 `server/pkg/remotemcp/client.go` 里有两处 fork 钩子（#184 加的），都标着 `Modified for the mouse9527/multica fork (M-230)`：

- `ValidatePublicHTTPSEndpoint`：HTTPS、userinfo、query、fragment 的检查之后，localhost 和公网地址的检查之前，`originAllowedByContext(ctx, endpoint)` 为真（并且 `allowedHosts` 为空或包含这个 host）就直接返回。
- `NewSecureHTTPClient` 的 `DialContext`：拒绝非公网地址的条件里多一个 `!originAllowedByContext(ctx, endpoint)`。

其余代码在 fork 文件 `server/pkg/remotemcp/allowed_origins_fork.go`（`AllowedOrigins`、`WithAllowedOrigins`、`originAllowedByContext`）。放行名单只放在 context 里传递，目前只有 `server/internal/mcpoauth` 的 `withAllowedOrigins` 会挂上它（环境变量 `MULTICA_MCP_OAUTH_ALLOWED_ORIGINS`），Plugin 发现和 daemon broker 不受影响。

同步时：上游改了这两个函数，文本冲突会落在这两处。解冲突时要保证：放行只跳过「地址必须是公网」这一条，HTTPS、证书校验、主机钉住、不跟随重定向、不走代理都照旧；拨号器仍对每个候选地址逐个判断。上游如果把这些检查挪到别的函数或文件，钩子要跟着搬过去，这种情况文本合并可能不报冲突，这时 `allowed_origins_fork_test.go` 里的 `TestValidatePublicHTTPSEndpointHonorsContextOrigins`、`TestSecureClientDialHonorsContextOrigins` 会先失败。

## 2026-11-25 同步（M-277）

合并基 `cd969f848`（M-131），上游 `1fda78b0d`。

**文本冲突 8 个文件，每个一处，都在 fork 钩子上**：

| 文件 | 解法 |
|---|---|
| `server/go.mod`、`go.sum` | fork 的 `golang.org/x/crypto`（密码登录）和上游新加的 `golang.org/x/image` 并存 |
| `server/internal/handler/handler.go` | import 并存（fork `passwordauth`，上游 `projectaccess`） |
| `server/internal/daemon/client.go` | 能力列表并存：上游 `DaemonCapabilityDocCommentV1`，fork `DaemonCapabilityMCPOAuthProxyV1` |
| `server/cmd/multica/cmd_daemon.go` | 保留 fork 的 `logsToTerminal` 判断（launchd 下 stderr 是空设备时写 daemon.log），终端分支改用上游带日志级别的 `NewWriterLoggerDefault` |
| `server/internal/service/delegated_failure_recovery_settled_test.go` | 取上游：触发器改在隔离数据库里建，fork 用 `testutil.ExecDDL` 做的清理不再需要 |
| `server/pkg/db/generated/models.go` | 两个新 struct 按 sqlc 的表名顺序并存（fork `CommentExternal`，上游 `CommentIdempotencyReceipt`）；CI 的 `sqlc-check` 确认与重新生成一致 |
| `packages/views/issues/components/issue-detail.tsx` | 上游把侧栏「详情」段换成一行创建信息页脚（已含「代为」），fork 的 Super Agent 行移到属性网格里 Evolution 行的后面 |

**文本合并不报、需要单独修的**：

- **`mcpoauth` 刷新失败的判定**：上游 #11162 让 `pkg/remotemcp` 把令牌端点的非 2xx 答复报成 `*remotemcp.OAuthError`，文本带上错误码（`HTTP 400: invalid_grant`）。fork 原来按 `HTTP 400` / `HTTP 401` 文本匹配，会把被拒的刷新当成暂时错误、永远不提示重连。现在用 `errors.As` 和 `Rejected()`；`TestTokenEndpointRejectionNeedsReconnect` 用真实的 OAuth 错误体验证。以后 fork 依赖 `remotemcp` 的行为时只用它导出的类型，不匹配错误文本。
- **lean brief（M-259）**：`runtime_config_lean.go` 是上游 brief 的手写精简版，上游每改一次 `runtime_config_sections.go` 就要跟一次。这次补了 `issue edit-description` 与 `--description-base-revision`、docs 命令与 doc 提及链接、doc 讨论任务、项目 workflow 状态映射规则、开头并行读取、Background Task Safety 的 chat wakeup 和 antigravity 分支。标题防漂移测试只看得到标题，段内新增规则要对着 `git diff <合并基> upstream/main -- server/internal/daemon/execenv/runtime_config_sections.go` 逐条看。
- **私有项目路由登记**：见上一节，Evolution 的 `proposals/{issueId}` 原先只列在「不探测」里，在上游测试下会失败，补进 `forkIssueRouteParams`。

- **上游 main 自己红的测试**：`packages/views/agents/components/agent-detail-inspector.description.test.tsx` 的「saves 128 emoji」期望自动保存同时发 `name`，但 #11495 之后只发改过的字段（上游 main 的 CI 在 `6d54300f3`、`4c6c33f78` 上也是这条红）。fork 把期望改成只有 `description`；上游修了以后，下次同步取上游的版本即可。
- **fork 的事件发布绕过了上游的脱敏**：上游在 `handler.publish` 里去掉指向私有项目的阻塞项，fork 的 `publishWithActorTask`（M-193）直接 `Bus.Publish`，UpdateIssue 和 BatchUpdateIssues 的 `issue:updated` 会带出私有阻塞项。现在同样脱敏，`TestPublishWithActorTaskRedactsLikePublish` 比对两者的输出。以后上游在 `publish` 里加处理，这个测试会先红。
- **接口签名变化**：`mergeRuntimeAndAgentMcpConfig` 多了 `cliVersion`（fork 的 OAuth MCP 挂载跟着传）；`paths.issueDetail` 改收 `IssueRouteRef`（Evolution 运行列表、Super Agent 看板链接改用 `useIssueRouteRef`）；上游新加了 `renderHelp`，fork 的 CLI 帮助测试辅助函数改名 `renderRouterHelp`。
- **上游的文案规范测试**对 fork 的 locale 生效：en 和 zh-Hans 用 ASCII 省略号，zh-Hans 用直引号、`skill` 不译。
- **上游的「操作触发智能体即归档」**（`auto_archive_on_agent_trigger`，默认开）会在 owner 自己改派时先归档 owner 的条目；`TestInboxResolveThroughBatchUpdate` 关掉这项偏好，只测 Super Agent 的 resolver。

**看过、不用改的**：

- 上游新的 inbox 类型 `doc_comment`、`doc_mentioned`、`app_tool_calls_quota_exceeded` 不带 issue，Super Agent 的 hold（`agentrouter/hold.go`）从不接；`blockers_done` 带 issue，和已有的 `children_done` 一样按影子模式记录。inbox 规则（`agentrouter/inbox_rules.go`）对未命中的类型判给 owner，新类型不会被误判为「规则处理」。
- 上游迁移 685–754 与 fork 的 9000 段不冲突，也没有改 fork 的表。
