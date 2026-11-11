# GitHub 原生 issue（fork 专有）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。代码在 `server/internal/githubissues/`。

## 功能开关与环境变量

| 变量 | 取值 | 说明 |
|---|---|---|
| `MULTICA_GITHUB_ISSUES_ENABLED` | `true` 才开启 | 默认关闭。写法同 `MULTICA_VCS_INTEGRATION_ENABLED` |
| `MULTICA_GITHUB_TOKEN` | classic PAT（`repo` 范围） | 未设置时功能整体关闭，不报错 |
| `MULTICA_GITHUB_TOKEN_EXPIRES_AT` | `YYYY-MM-DD` 或 `never` | 令牌在 GitHub 上登记的到期日；缺失或格式不对时功能关闭 |
| `MULTICA_GITHUB_SYNC_ACCOUNT` | 可选：`member:<user-uuid>` 或 `agent:<agent-uuid>` | 他人在 GitHub 新建的 issue 记在谁名下，见「外部作者」。不设时用各工作区最早的 owner；格式不对时功能关闭 |

- 三个条件同时满足才开启。不满足时服务照常启动，只打一行 `github issues disabled (<原因>)`。原因只写变量名，不回显变量值。
- 关闭时服务行为与上游一致：`githubissues.New` 返回 nil，不发任何 GitHub 请求，不注册路由，不注册调度任务。对上游文件的钩子见下面「对上游文件的钩子」。
- 令牌只保存在 `githubissues.Config` 的未导出字段里。`Config` 在 `fmt`（含 `%#v`）、`slog`、`encoding/json` 下都不输出令牌，测试见 `config_test.go`。
- 不建 `github_connection` 表，不需要主密钥。轮换令牌要改环境变量并重启。

## 令牌状态、校验与到期提醒

### 状态

每个服务进程在内存里保存令牌状态，不建表：

| 状态 | 含义 |
|---|---|
| `ok` | 最近一次 `GET /user` 被 GitHub 接受，且未到期 |
| `invalid` | GitHub 拒绝了令牌，或本进程启动后还没拿到 GitHub 的结论 |
| `expired` | 已到 `MULTICA_GITHUB_TOKEN_EXPIRES_AT` 登记的日期（UTC）。**到期日当天即视为到期**，按日期推算，不依赖调度任务 |

只有 `ok` 时 `Service.Usable()` 为真。轮询、按需刷新、双写都必须先看它，为假就停。

### 校验

- 启动后在后台调 `GET /user`，之后每天再调一次。失败不影响服务启动。
- 401 → `invalid`，类别 `unauthorized`。
- 403 且是限流（`X-RateLimit-Remaining: 0`、`Retry-After`、secondary rate limit）或 429 → 按响应头退避后重试，不改变已有结论。判断与退避时长复用 `ghsnapshot`（`IsRateLimited`、`RateLimitFromResponse`，等待时长限制在 1 秒到 5 分钟）。
- 403 非限流 → `invalid`，类别 `sso_required`（带 `X-GitHub-SSO` 头）或 `forbidden`。
- 响应带 `X-OAuth-Scopes` 且不含 `repo` → `invalid`，类别 `missing_repo_scope`。**这个头是否总会出现未验证**，缺失时跳过此项检查。
- 网络错误、5xx、其他状态码：说明不了令牌好坏，保留上一次结论，按 30 秒起翻倍、最长 10 分钟重试。
- 已到期的令牌不再发给 GitHub。
- 日志和错误只写类别或状态码，不写响应正文，也不写令牌。

### 状态接口

`GET /api/workspaces/{id}/github-issues/token-status`，只有该工作区的 owner、admin 能读；功能关闭时这个路由不存在（404）。令牌是部署级的，返回的是整个部署的状态，与路径里的工作区无关：**本部署任何工作区的 owner、admin 都能看到令牌所属的 GitHub 账号名和令牌状态**，即使该工作区没有登记 GitHub 仓库。响应只有四个字段：

```json
{"account_login": "octocat", "status": "ok", "expires_at": "2027-01-31", "validated_at": "2027-01-10T09:30:00Z"}
```

- `account_login`：GitHub 接受过令牌后才有，否则为 `null`。
- `expires_at`：登记的到期日；`never` 时为 `null`。
- `validated_at`：GitHub 最近一次给出结论（接受或拒绝）的时间；还没有结论时为 `null`。

### 到期提醒

- 调度任务 `github_token_expiry`，每天一次，写法同 `scheduler/jobs_issue_wakeup.go` 的 `JobSpec`。
- 到期前 14、7、1 天各提醒一次，走收件箱，类型 `github_token_expiry`，严重度 `attention`。
- 收件人：项目上登记了 GitHub 仓库（`project_resource.resource_type = 'github_repo'`，即任务 C 白名单的来源）的工作区里的 owner 和 admin。没有这样的工作区时只打一行警告。
- 某天任务没跑时，下一次运行补发当前阶段的提醒（例如第 12 天补发 14 天提醒）。这只对 14 天和 7 天两档成立：如果到期前最后一天（UTC）整天都没有运行，1 天那条提醒就丢了，因为下一次运行时令牌已经是 `expired`，不再发「即将到期」的提醒。状态接口和启动日志仍会显示已到期。
- 同一到期日、同一阶段对同一人只发一次，以收件箱里已有的记录为准，所以正常的重试和多副本不会重复发。去重是先查再插，没有唯一约束兜底（不改现有表）：如果一次运行卡住超过租约超时（5 分钟）被另一个副本接手，两边可能各发一条，**极端情况下同一个人可能收到重复的一次提醒**。
- 到期本身不靠这个任务：每个进程按日期自己算出 `expired`。

### 对上游文件的钩子

| 文件 | 改动 |
|---|---|
| `server/cmd/server/main.go` | 读配置建 `githubissues.Service`，传给路由；开启时启动校验并注册 `github_token_expiry` |
| `server/cmd/server/router.go` | `RouterOptions.GitHubIssues` 字段；开启时注册状态接口 |
| `server/internal/integrations/ghsnapshot/ratelimit_export.go` | 新文件，导出限流判断与退避，供本功能复用 |
| `server/pkg/db/queries/githubissues.sql` | 新文件：提醒收件人与去重查询、`RepoClient` 白名单查询（只读现有表）与绑定的条件写入（只写 fork 表 `github_repo_sync`），不改表结构 |

## 访问仓库：`RepoClient` 与白名单

本功能对 GitHub 仓库的所有请求（轮询、按需刷新、双写、PR 轮询）都经过 `Service.RepoClient(ctx, workspaceID, owner, repo)`，这是唯一入口。

### 白名单

- 白名单 = 该工作区所有项目上登记的 `github_repo` 资源（`project_resource`）的并集，每次调用现查，删掉资源立即生效。
- 只认 `github.com`（含 `www.github.com`）的地址：`https://github.com/owner/name`、`ssh://git@github.com/owner/name.git`、`git@github.com:owner/name.git`，可带 `.git` 与结尾 `/`。其他主机、带端口、带查询串、多于两段路径的地址都不进白名单。名字比较不区分大小写。
- 不在白名单（含名字不合法）：返回 `ErrRepoNotRegistered`，**不发任何请求**，也不写库。
- 令牌不是 `ok`（失效或到期）：返回 `ErrTokenNotUsable`，不发请求。先查白名单，再看令牌。

### 固定 `repo_id`

- 某个登记名第一次使用时，调 `GET /repos/{owner}/{repo}` 取得仓库 id，写入 `github_repo_sync`（以 `(workspace_id, repo_id)` 为键，`owner`、`name` 记登记名）。之后同名调用直接用这个 id，不再按名字查。
- `Repo.Do` 的请求一律发到 `/repositories/{repo_id}/...`，不按名字寻址。所以：
  - 仓库改名或转移后，仍登记旧名的资源继续命中原仓库（同一个 id）；
  - 有人在旧名下新建仓库，也不会被命中；
  - Owner 把资源改成新名后，新名解析出同一个 id，沿用原来那一行（游标不丢），只更新名字。
- 新名在资源改过来之前不在白名单，会被拒绝。
- **一行只记一个名字，仍登记的名字的固定绑定不会被别的名字顶掉。** 如果某个名字解析出的 id 已经固定在另一个仍登记的名字上（例如项目 A 留着旧名、项目 B 登记了新名；或者新名已固定，又有人登记了仍被 GitHub 重定向过来的旧名），后来的这个名字返回 `ErrRepoAmbiguous`，不改那一行，并打一条警告。原来的名字继续命中原 id；即使旧名已被别人新建的仓库占用，也不会转到那个仓库。删掉其中一个资源后即恢复：留下的名字若是新名，下次调用时接管这一行（游标不丢）。
- **绑定写入是条件写，并发也成立。** 新绑定用 `InsertGithubRepoPin`（`ON CONFLICT DO NOTHING`：这个 id 已有绑定就不写）；改名迁移用 `MoveGithubRepoPin`（只在这一行仍是刚读到的名字时才改）。写入落空说明别的调用者先写了，`RepoClient` 重新读取绑定再判断（最多 3 次），所以两个名字同时首次使用时，先写入的那个保留，另一个被拒绝（`ErrRepoAmbiguous`）。M-20 的 `UpsertGithubRepoSync` 会无条件覆盖名字，`RepoClient` 不用它，后续任务也不要用它改绑定。
- 删除资源后该仓库立即不可用；`github_repo_sync` 里的那一行保留，不再作为授权依据。
- 首次解析时跟随 GitHub 的改名重定向（只读的 GET），所以登记的是改名前的旧名也能解析到原仓库。

### `Repo.Do`

- 只允许 `GET`、`POST`、`PATCH`、`PUT`、`DELETE`。路径必须以 `/` 开头，可带查询串；拒绝 `..`、`.` 段（含 `%2e%2e`）、反斜杠、`//` 开头、带协议或主机、带 `#`。被拒的路径不发请求。
- 不跟随重定向（按 id 寻址的仓库不应重定向；跟随会把写请求变成 GET，或落到别的仓库），3xx 原样交给调用方。
- 每次发送前再看一次令牌状态。401 → 令牌置为 `invalid`，返回 `ErrTokenNotUsable`（可用 `errors.Is` 判断），之后所有 `RepoClient` 停止，直到每日校验重新接受令牌。限流 → `*ghsnapshot.RateLimitError`。非限流的 403 不改令牌状态，原样返回：在仓库接口上它可能只是这个仓库拒绝（SSO、权限）。
- 响应体上限 8 MiB；错误里只有方法、路径和状态码，不带响应体，也不带令牌。

### 令牌不出包

- 构造请求、读令牌只在 `transport.go` 一个文件里（`newRequest`、`exchange`，都不导出）；从环境变量读令牌只在 `config.go`。`Repo`、`Service` 不导出任何字段，导出方法不返回 `*http.Client`、`*http.Request` 或 `http.RoundTripper`，唯一的例外是受限的 `SnapshotTransport`（见下文「PR 快照：静态令牌来源」）；`Repo` 在 `fmt`、`slog` 下只显示 id 和名字。
- `scan_test.go` 用语法树扫描本包及其子包（如 `issuekey`）的全部非测试文件，以及 `ghsnapshot` 目录下所有带 fork 修改声明（`mouse9527/multica fork`）的文件，发现下面任何一项就失败：`http.NewRequest*`、`http.Get/Post/DefaultClient` 等默认客户端、`.token` 字段读取、`"Authorization"` 头、`EnvToken` 或 `"MULTICA_GITHUB_TOKEN"`、`os.Getenv/LookupEnv/Environ`、对 `*httpClient` 直接调用 `Do`。前三类与 `.Do` 只允许出现在本包顶层的 `transport.go`，令牌变量只允许出现在 `config.go`；`ghsnapshot` 里的文件没有文件级例外。以后修改上游 `ghsnapshot` 文件时，按许可证要加修改声明，这个文件就会自动进入扫描。上游文件里以 GitHub App 身份认证的函数按函数、按规则豁免（`scan_test.go` 的 `upstreamAppPath`）：`client.go` 的 `NewClientFromEnv`（读 App 的环境变量）、`cachedToken`（读 App 安装令牌）、`mintInstallationToken`（换取安装令牌）、`restGet`（构造请求、给 App 请求加安装令牌；不豁免读 `.token` 和直接调用 `httpClient.Do`）。这些函数只处理 App 的凭据，不接触本功能的令牌；文件里其余部分照常扫描。

### 已知限制

- classic PAT 的 `repo` 范围在 GitHub 一侧不能限定仓库。白名单是服务端的自我约束，挡不住令牌泄露后的外部使用。
- 同一工作区同时登记同一个仓库的两个名字时，后来的名字不可用（`ErrRepoAmbiguous`），要删掉其中一个资源。「这个名字是否仍登记」读的是项目资源，与绑定写入不在同一条 SQL 里：只有在读和写之间恰好有人删改项目资源时才可能判断过时，不加锁。
- `/repositories/{id}/...` 这种按 id 寻址的写法与 GitHub 对改名仓库返回的重定向地址一致，但没有在真实 GitHub 上验证全部子路径（issues、comments、pulls），需要 Owner 实测。

## PR 快照：静态令牌来源

`ghsnapshot` 读取 PR 的 CI 与可合并状态（`FetchPRSnapshot`），原来只能用 GitHub App 的安装令牌。现在 `ghsnapshot.Client` 另有一个静态令牌来源，用本功能的令牌读取；`snapshot.go`、`refresh.go`、`issue_pull_request`、前端 PR 卡片都不变，`handler/github.go` 的 `isGitHubConfigured()` 也不变。

- **合成键：** 静态来源的安装 id 是 `ghsnapshot.StaticInstallationID`（`-1`）。刷新队列、限流暂停、PR 行都像真实安装 id 一样用它做键；GitHub 的安装 id 都是正数，不会冲突。用这个 id 的请求只走静态来源，不会回退到 App；其他 id 只走 App。
- **令牌不进 `ghsnapshot`：** `githubissues` 导出 `Service.SnapshotTransport()`（一个 `http.RoundTripper`），`ghsnapshot` 用 `Client.WithStaticSource(transport)` 接上。`ghsnapshot` 只构造不带凭据的请求，由这个 transport 在放行时给请求的副本加上 `Authorization` 头；返回的响应里 `Response.Request` 指回调用方传入的原请求（`net/http` 默认会指向带令牌的那份副本），所以从响应上也拿不到令牌。功能关闭（nil `Service`）时返回 nil，`WithStaticSource(nil)` 不改变客户端。
- **放行条件**（不满足就返回错误，不发请求）：
  - 只放行 `GET`，主机与协议必须是 GitHub API 根地址，不能带用户信息；
  - 路径只能是 `FetchPRSnapshot` 读的几种：`/repos/{owner}/{repo}/pulls/{n}`、`/repos/{owner}/{repo}/commits/{sha}/check-runs`、`.../commits/{sha}/status`、`/repos/{owner}/{repo}/actions/runs`（`sha` 只接受十六进制）；
  - 令牌必须可用（`ok`），否则 `ErrTokenNotUsable`；
  - 仓库必须登记为某个项目的 `github_repo` 资源，否则 `ErrRepoNotRegistered`。刷新队列不知道 PR 属于哪个工作区（只有安装 id、owner、repo、号），所以这里的白名单是**所有工作区的并集**（新查询 `ListGitHubRepoRefs`）；按工作区的约束由把 PR 放进队列的一方负责（任务 M 的 PR 轮询）。
- **重定向：** transport 自己不跟随重定向，3xx 原样交回 `ghsnapshot` 的客户端；客户端跟随时新请求再次经过 transport，按同样条件检查。所以改名仓库重定向到 `/repositories/{id}/...` 时会被拒绝，快照失败，不会把令牌带到别的地址。
- **权限：** `tokenCanRead` 对静态令牌恒为真（PAT 的权限无法逐项得知），所以存在失败的 Actions 检查时也会读 `actions/runs`。调用返回 403 就当不可用：这一次快照失败，保留上次的结果，错误里只有状态码；非限流的 403 不改令牌状态。401 → 令牌置为 `invalid`，与 `RepoClient` 相同。
- **接线：** 本节只提供能力。把静态来源接到服务端的 `ghsnapshot` 客户端、并把 PR 以 `StaticInstallationID` 放进刷新队列，是任务 M（PR 轮询与关联）的范围。

## 标识

### 规则

- 有 `issue_external` 行的 issue 是 github issue，标识为 `owner/repo#N`（`external_repo` 加 GitHub 上的编号）；没有的就是 local issue，标识仍是 `PREFIX-N`，与上游一致。
- issue 响应（HTTP 与事件载荷）多一个 `provider` 字段：`local` 或 `github`。`number` 仍是 Multica 内的编号，github issue 的 `PREFIX-N` 也能继续用来查找。
- 标识跟随功能开关：`main.go` 启动时按 `githubissues.Config.Enabled()` 调 `issuekey.SetEnabled`。开关关闭（默认）时不读 `issue_external`、不解析 `owner/repo#N`，所有 issue 按 local 渲染，与上游一致；开启期间写下的关联行保留，重新开启后恢复 `owner/repo#N`。
- 解析（仅开关开启时）：`/api/issues/{id}` 接受 `owner/repo#N`（含百分号转义的 `owner%2Frepo%23N`，CLI 就是这样发的），仓库名不区分大小写；CLI 的 `multica issue ... owner/repo#N` 交给服务端解析。
- 删除 issue 时在同一事务里删除它的 `issue_external` 行（表没有外键）；这一步不看开关，免得关闭期间删除的 issue 留下孤儿行。工作区删除由 `DeleteWorkspaceGithubIssueData` 清理。

### 实现

- 格式、解析与批量读取在 `server/internal/githubissues/issuekey/`。它只依赖生成的查询代码，`service`、`handler`、CLI 都能引用，不会形成循环依赖（`githubissues` 经 `scheduler` 依赖 `service`）。
- `service.IssueIdentifier` 仍只渲染 local 标识，不改签名；按是否存在 `issue_external` 分支的是 `service.IssueKey`（已读好的一批）和 `service.LookupIssueIdentifier`（单条，多一次查询）。
- 列表与详情：纯函数构造器先按 local 渲染，`handler.fillIssueKeys` 再用**一页一次** `ListIssueExternalsByIssueIDs` 改写 `identifier`、`provider` 和 `duplicate_of.identifier`。列表接口因此只多 1 次查询，`issue_key_test.go` 用计数的 `DBTX` 断言。
- 读取 `issue_external` 失败时记一条警告、按 local 渲染，与标签加载失败时的处理一致。

### 对上游文件的钩子

| 文件 | 改动 |
|---|---|
| `server/internal/handler/issue.go` | `IssueResponse.Provider`；三个构造器填 `local`；`fillStatusCategory(ies)`、搜索、open 列表、分组、子 issue、创建、批量更新后调 `fillIssueKeys`；删除事务删 `issue_external` |
| `server/internal/handler/handler.go` | `resolveIssueByIdentifier` 先试 `owner/repo#N` |
| `server/internal/handler/{issue_table_rows,search_index,issue_duplicate,plugin_action}.go` | 构造完一页后调 `fillIssueKeys` |
| `server/internal/handler/{daemon,issue_workflow_entry,issue_system_wakeup,squad}.go` | 认领、工作流提示词、等待中的子 issue、squad 活跃 issue 的标识改用 `issueKey` / `service.IssueKey` |
| `server/internal/service/{task,source_context}.go` | `IssueToMap` 带 `provider`，`IssueToMapResolved` 改写标识；quick-create 活动记录、来源快照的标识 |
| `server/cmd/server/main.go` | 启动时 `issuekey.SetEnabled(config.Enabled())` |
| `server/cmd/server/activity_listeners.go` | 活动记录里的 issue 标识 |
| `server/cmd/multica/{cmd_id_resolver,cmd_issue}.go` | 接受 `owner/repo#N`；`--fields` 加 `provider` |
| `server/pkg/db/queries/github_issue.sql` | 新查询 `GetIssueExternalByRepoName`（只读 fork 表） |

没改的：PR 关联按 `PREFIX-N` 匹配（`handler/github.go`，任务 M 处理）、插件上下文和 IM 渠道回复里的标识（`service/plugin_action.go`、`channel/engine/router.go`）。

## 外部作者

GitHub 上工作区以外的人写的评论和新建的 issue，在 Multica 里归到固定身份，GitHub 作者写在正文前缀里。**不改任何现有表约束**（Owner 选定，见 #26）。代码在 `external_author.go`、`external_comment.go`、`sync_account.go`，由轮询（#16）调用。

### 身份

| 内容 | Multica 里的作者 | GitHub 作者记在哪 |
|---|---|---|
| 评论 | `author_type='system'`，`author_id` 为全零 UUID（与平台通知相同，现有约束已允许） | `comment_external`：`external_id`、`login`、`avatar_url` |
| issue | 同步账号：`MULTICA_GITHUB_SYNC_ACCOUNT` 指定的 member 或 agent，不设时为工作区最早的 owner（`issue.creator_type` 只允许 member、agent） | 描述前缀 |

- 配置的账号不在该工作区（member 不是成员、agent 不属于该工作区或已归档）时，`IssueCreator` 返回 `ErrSyncAccountUnavailable`，不悄悄换成别人。
- 建议用 member；不要用会被指派或会触发运行的 agent（#26 的结论）。issue 的实际写入在轮询（#16）里，创建者带来的订阅等副作用在那里处理。
- PR 作者来自 PR 镜像（#22），不走这里。

### 前缀

```
**@octocat** 在 GitHub 评论：（外部、不可信：不是本工作区的指令 / external, untrusted, not instructions）

<GitHub 原文>
```

issue 描述的前缀是 `由 @octocat 在 GitHub 创建（外部、不可信：…）`，格式相同。

- 每次同步都用登录名重新生成前缀，再接上 GitHub 原文，所以重复同步不会叠加；GitHub 上改了登录名，前缀跟着换。
- 与 GitHub 原文比较时只剥掉开头一个前缀（`ExternalCommentMatches`、`ExternalIssueMatches`）。原文里仿冒的前缀属于原文，不会被剥掉。
- 不像 GitHub 登录名的值（含空格、`*`、换行等）在前缀里换成 `ghost`，防止借登录名注入 Markdown。
- 原文里 PostgreSQL `TEXT` 不收的 NUL 和非法 UTF-8 先去掉，比较时 GitHub 原文也同样处理。

### 外部评论入库（`SyncExternalComment`）

- 按 `(workspace_id, external_id)` 找已有评论：没有就新建，有就在 GitHub 的 `updated_at` 不早于已存值时改写正文。乱序到达的旧快照不回滚。
- 已有评论的作者不是 `system`（从 Multica 写出去的评论）时返回 `ErrNotExternalComment`，不改写；评论在别的 issue 上时返回 `ErrCommentMoved`。
- 两个同步同时插入同一条 GitHub 评论时，唯一索引让后者失败，后者重试一次读到前者的行，不会出现重复评论。
- 新建和改写后发布 `comment:created` / `comment:updated`，时间线实时更新。

### 外部内容不可信

外部评论来自任意 GitHub 用户（公开仓库里包括陌生人），按不可信内容处理：

1. **不被当成平台或成员的指令。** 外部评论从不成为智能体运行的触发评论，也不会被并入运行（并入只发生在触发评论上），所以 `daemon/prompt.go` 里把 `system` 作者标成「The platform」的那段不会作用到它。智能体只会在读评论列表时看到它，看到的作者是 `system`，正文开头就是前缀。`agent_access.go` 对 `system` 的宽权限只在 `system` 发起调用时生效，外部评论不发起任何调用。
2. **前缀标明「外部、不可信」**，中英文各一遍，智能体读到的正文里就有。
3. **不唤醒、不指派智能体。** 入库不走评论触发流程（`handler.CreateComment` 的指派触发、@提及、线程回复都不经过），正文里的 `mention://agent/...` 不起作用。数据库触发器 `capture_comment_wakeup` 会为 issue 上订阅了 `comment.created` / `comment.updated` 的 wakeup 记一条回执，`SyncExternalComment` 在同一事务里删掉只由这条评论产生的回执，提交前调度器看不到。如果回执和更早一个未处理的事件合并了（`coalesced_count > 1`），保留它，因为那次运行本来就会发生。
   - 通知和订阅监听器本来就跳过 `system` 作者，所以外部评论不进收件箱、不加订阅者。
   - 目前没有 Owner 可配置的触发条件，外部评论一律不唤醒智能体。工作区装的插件订阅了 `comment.created` 时仍会收到事件，插件是 Owner 安装的，算 Owner 的配置。

### 只读

外部评论在 Multica 里只读：`PUT`、`DELETE /api/comments/{id}` 对「`system` 作者且有 `comment_external` 行」的评论返回 403，owner、admin 也一样。其他评论（成员、智能体、平台通知）不受影响。功能关闭时不做这项检查，与上游一致。

### 对上游文件的钩子（本节）

| 文件 | 改动 |
|---|---|
| `server/cmd/server/main.go` | 建 `githubissues.Service` 时传入数据库连接池和事件总线（`RepoClient` 也用这个连接池查白名单、写绑定） |
| `server/cmd/server/router.go` | 功能开启时把 `Service` 设为 `Handler.ExternalComments` |
| `server/internal/handler/handler.go` | `Handler.ExternalComments` 字段 |
| `server/internal/handler/comment.go` | `UpdateComment`、`DeleteComment` 各一行只读检查 |
| `server/internal/handler/githubissues_hook.go` | 新文件：只读检查 |
| `server/pkg/db/queries/githubissues.sql` | 新查询：按 GitHub 评论 id 加锁、删除外部评论产生的 wakeup 回执、默认同步账号 |

## 轮询

把 GitHub 上的 issue 与评论拉进 Multica。代码在 `poll.go`、`poll_issues.go`、`poll_comments.go`，所有请求经 `RepoClient`。

### 调度

- 调度任务 `github_issues_poll`，`JobSpec` 写法同其他任务，周期 60 秒，全局一个任务（不按仓库拆）。数据库租约保证多副本同一时刻只有一个周期在跑，所以同一个令牌同时最多 2 个请求：2 个 worker，每个 worker 一次只处理一个仓库，同一仓库的请求串行。
- 任务跑在**单独的调度器**上（`Service.StartPolling`，每 10 秒检查一次）。调度器按顺序执行任务，一个周期最长 50 秒，放在主调度器上会拖住 issue wakeup 等其他任务。
- 每个周期列出所有工作区登记的 `github_repo` 资源（同一工作区同名只算一次），按上次轮询时间从早到晚处理；周期用完 50 秒还没轮到的仓库，下个周期排在最前面。`github_repo_sync.status = 'paused'` 的仓库跳过（目前没有界面会设置它）。
- 令牌不是 `ok` 时整个周期不发请求。

### 两个游标

| 流 | 请求 | 游标 |
|---|---|---|
| issue | `GET /repositories/{id}/issues?state=all&sort=updated&direction=asc&since=…` | `issues_cursor` |
| 评论 | `GET /repositories/{id}/issues/comments?sort=updated&direction=asc&since=…` | `comments_cursor` |

- 游标是已处理项的最大 `updated_at`，每页处理完就写库，只前进不后退。每个周期从游标回退 2 分钟开始读，重叠部分靠 `(workspace, repo_id, number)` 与 `(workspace, 评论 id)` 唯一键去重，不会产生重复行。
- **不依赖「评论是否推进 issue 的 `updated_at`」**（文档没写），评论单独轮询。
- 翻页按游标而不是页码：下一页请求「上一页最大 `updated_at` 减 1 秒」之后的项（GitHub 时间戳精确到秒，`since` 是否含边界未验证，减 1 秒两种情况都覆盖）。这样读的过程中有项被更新，它会挪到后面，不会把没读的项挤到已读过的页。整页都在同一秒时才按页码往下翻。
- 每个流每个周期最多 10 页（每页 100 条），没读完下个周期从游标接着读。首次轮询（游标为空）从头导入仓库里全部 issue 和评论。
- 读取进度可以跨周期接续：一个流没读到末尾就停下时（10 页用完、周期超时、某一项失败），记下下一个请求的 `since` 和页码，下个周期从这里接着读，而不是又从「游标减 2 分钟」开始。游标只存时间戳，同一秒的项多于一个周期的预算时（比如批量导入），如果每个周期都从游标重来，就会一直重读同一批，后面的项和评论流永远轮不到；接续保证无论这批有多大，流都在前进（每周期约 1000 条）。读到末尾时清掉记录。
- 进度只存在进程内存里。服务重启后、或在还没读过这个流的副本上，会从游标重读一次，之后接着往前。记录的位置落在重叠窗口之外时（别的副本已经读得更远），丢弃它，改用游标。
- issue 流里的 PR（带 `pull_request` 键）跳过。评论流只在 issue 流本周期读到末尾后才读，所以评论不会先于它所属的 issue 到达；找不到本地 issue 的评论（PR 上的评论）跳过。
- 某一项处理失败时，这个流停在这一项，游标不越过它，下个周期重试；错误写进 `github_repo_sync.last_error`，下一次干净的轮询清空它。其他仓库照常轮询。

### 他人新建的 issue

- 本地没有对应 `issue_external` 的 GitHub issue 新建为本地 issue：创建者是同步账号（`IssueCreator`，见「外部作者」），描述是带前缀的原文，不指派任何人，项目是最早登记该仓库的项目，优先级 `none`。同一事务写 `issue_external`，并用 advisory lock 串行化同一 GitHub issue 的写入。工作区编号冲突时按上游的做法修复计数器后重试一次。
- 初始状态：按「状态映射」的规则从 GitHub 的状态与状态标签得出。项目工作流不包含该状态时用工作流的第一步（与其他创建入口一致）。新建 issue 不走 `UpdateIssue`，所以直接落在交接步上时**不触发交接**；之后的状态变化见「状态映射」。
- 配置的同步账号不可用时（`ErrSyncAccountUnavailable`），这个 issue 和它之后的项都不导入，直到配置修正。
- 本地删除的 github issue 在 GitHub 上再次更新时会重新导入（GitHub 是唯一写入方）。

### 已有 issue 的更新与乱序

- GitHub 快照的 `updated_at` 早于已存的 `external_updated_at` 时什么都不改（乱序到达的旧快照不回滚）；不早于时更新标题、`issue_external` 的状态字段与仓库名。描述只在它带外部前缀（由轮询写入）时改写；从 Multica 写出去的 issue 的描述留给双写任务（#19）。
- 评论的乱序与去重沿用 `SyncExternalComment`；从 Multica 写出去的评论（作者不是 `system`）不改写。

### 限流

- 任何请求遇到限流（429、带限流信号的 403），整个周期立即停止，按 GitHub 给的等待时长（`ghsnapshot.RateLimitFromResponse`，限制在 1 秒到 5 分钟）暂停轮询，期间的周期不发请求。暂停记在进程内存里；多副本时另一副本下一次请求也会被限流并自行暂停。
- 401 使令牌变为 `invalid`（见 `RepoClient`），周期停止。

### 事件与外部文本

- 新建 issue 发 `issue:created`，载荷是完整的 issue，但 **`description` 置空**：订阅监听器会从 `issue:created` 的描述里解析 mention 链接并加订阅者，GitHub 上的文本不能借此订阅或通知任何人。同步账号照常作为创建者被加为订阅者。客户端打开详情时从接口读到描述。
- 已有 issue 变化时发 `issue:updated`，载荷只有 `id` 和 `workspace_id`、不带 `revision`，客户端因此重新拉取；需要完整 issue 的通知、活动、订阅监听器都会跳过它。
- 事件的 `actor_type` 是 `system`，活动记录里显示为系统创建。
- 改写标题、描述时，数据库触发器照常为订阅了 `issue.updated` 的 wakeup 记回执（载荷只有字段名，不含正文），没有像外部评论那样丢弃。

### 已知限制

- 删除、转移的 issue 与删除的评论，`since` 列表发现不了，留给每日对账与按需刷新（#21、#23）。
- `since` 的边界语义、评论列表的排序参数、`/repositories/{id}/...` 路径在真实 GitHub 上未验证，见 PR 的 Owner 实测步骤。

### 对上游文件的钩子（本节）

| 文件 | 改动 |
|---|---|
| `server/cmd/server/main.go` | 功能开启时调 `gitHubIssues.StartPolling(sweepCtx, pool)` |
| `server/pkg/db/queries/githubissues.sql` | 新查询：跨工作区列出 `github_repo` 资源与轮询状态（只读 `project_resource`）、记录一次轮询结果（只写 `github_repo_sync`）、改写 github issue 的标题与描述 |

## 状态映射

GitHub 只有 open / closed（加 `state_reason`）和标签；Multica 是工作区状态目录加项目工作流。代码在 `status.go`。

### GitHub → Multica

| GitHub | Multica |
|---|---|
| closed，`state_reason` 为 `not_planned` 或 `duplicate` | cancelled 类：当前已是 cancelled 类就保持，否则 `cancelled` |
| closed，其他（`completed`、无原因） | done 类：当前已是 done 类就保持，否则 `done` |
| open，有一个映射到的状态标签 | 标签映射到的状态 |
| open，没有映射到的状态标签 | 项目默认入口状态：`backlog`；项目工作流不含 `backlog` 时用工作流起始状态（`todo`） |
| open，有多个映射到的状态标签 | 见下 |

- 状态标签用保留前缀 `status:`，每个项目在 `project_github_settings.status_label_map` 里配置，形如 `{"in_progress": "status:in-progress", "in_review": "status:review"}`（状态 key → 标签名）。标签名比较不区分大小写（GitHub 的标签名不区分大小写唯一）。不以 `status:` 开头的条目、和别的 key 重复的标签忽略并记告警。没有配置时只有入口状态、done 类、cancelled 类可以表达。
- 带 `status:` 前缀但不在映射里的标签、映射到 done 类或 cancelled 类状态的标签（这两类只由关闭表达）忽略。
- **同一 issue 出现多个状态标签：** 取 GitHub 标签顺序里的最后一个，并记告警 `several status labels; took the last`。哪个标签最新无法核实：GitHub 的 issue 列表不带每个标签的添加时间，之前见过的标签也没有存。结果只取决于这份快照本身、不看当前状态，所以轮询在重叠窗口里重读同一份快照时不会再改状态（不会来回改派、反复触发运行）。不回写清理多余标签（写 GitHub 属于双写任务）。
- 算出的状态与当前状态相同时什么都不做，所以只改标题、正文的 GitHub 更新不会动状态；本地状态与 GitHub 等价（例如当前是自定义的 done 类状态而 GitHub 是 closed completed）也不动。

### 走与普通写入相同的入口

- 已有 issue 的状态变化在 issue 事务提交后，由 `StatusWriter.WriteGitHubStatus` 执行。它的实现是 handler 的 `WriteGitHubStatus`：在进程内构造一个 `PUT /api/issues/{id}` 请求、直接调用 `UpdateIssue`，所以和人在 Multica 里改状态是同一条路径：项目工作流校验、进入交接步时改派给步骤处理人并启动运行、`issue:updated` 事件与活动记录、子任务规则。
- 以谁的身份：同步账号是成员时用它；同步账号是智能体时用工作区最早的所有者（智能体只能在自己的运行里写状态）；同步账号配置了但不可用时报错，不悄悄换人。
- 写入请求的 context 带标记，`githubissues.IsSyncedWrite(ctx)` 为真，双写不应把这次变化再写回 GitHub。
- `UpdateIssue` 拒绝（4xx，例如工作流不含该状态）：记告警，不重试（重试结果一样）。其他失败（5xx、409 写入竞争、数据库错误）：这一项失败，流停在这里，下个周期重读这个 issue 再试。
- 没有接上 `StatusWriter` 时（只在测试里会发生）不同步状态并记告警。

### Multica → GitHub：无法表达的状态拒绝写入

- 对 github issue 改状态时，目标状态必须能在 GitHub 上表达：done 类、cancelled 类、项目入口状态，或在 `status_label_map` 里有标签。否则 `UpdateIssue` 返回 422，`code` 为 `github_status_unmapped`，`allowed_statuses` 列出能用的状态，错误文本提示去项目的 GitHub 设置加 `status:<name>` 标签。不拒绝的话，这个状态会在下一次 GitHub 更新时被覆盖，等于静默丢失。
- 批量更新里，这样的 issue 和其他逐项校验失败一样被跳过，其余照常更新。
- 本地 issue、不改状态的写入不受影响。功能关闭时没有这个钩子，行为与上游一致。
- 在双写（#19）合并之前，Multica 里改的状态不会写到 GitHub；GitHub 上下一次更新这个 issue 时，按上面的规则以 GitHub 为准。

### 已知限制

- 新建 issue（他人在 GitHub 上新建）直接落在交接步时不触发交接，见「他人新建的 issue」。
- 项目移动、工作流切换时的状态重映射（`MoveTarget`）不经过本节的拒绝钩子。
- 多个状态标签时不判定哪个最新（需要读 GitHub 的标签事件时间线，多一次请求，本期不做），固定取最后一个。

### 对上游文件的钩子（本节）

| 文件 | 改动 |
|---|---|
| `server/internal/handler/handler.go` | `Handler` 增加字段 `GitHubStatuses` |
| `server/internal/handler/issue.go` | `UpdateIssue` 在工作流校验之后调 `rejectUnmappedGitHubStatus`（3 行）；`BatchUpdateIssues` 同处调 `gitHubStatusRejected`（3 行） |
| `server/cmd/server/router.go` | 功能开启时设置 `h.GitHubStatuses` 并 `SetStatusWriter(h)` |

钩子实现与 `WriteGitHubStatus` 在 fork 自己的 `server/internal/handler/githubissues_hook.go`。

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
