# 跨工作区只读授权

本文件属于 `mouse9527/multica` fork（M-307，GitHub #207）。同一个实例上，工作区 B（目标）的 owner 或 admin 可以授权工作区 A（来源）的智能体只读 B 的内容。智能体用自己这次运行的任务令牌（`mat_`）读取，每次读取都留审计。

## 开关

- `cross_workspace_read`，默认关闭。用 `FF_CROSS_WORKSPACE_READ=true` 打开（或 `MULTICA_FEATURE_FLAGS_FILE` 里的规则），每次请求时读取。
- 关闭时与上游一致：任务令牌指向别的工作区一律 403（GitHub #9403），授权接口返回 404。已建的授权留在表里，但不生效。
- 没有前端界面，开关不发布给前端。

## 授权和撤销

只有目标工作区的 owner 或 admin 能操作，而且必须是人（任务令牌、cloud PAT 都会被拒绝）。

```
multica workspace read-grant create --source-workspace <来源工作区 ID> [--agent <智能体 ID>] [--expires-in 720h | --expires-at <RFC 3339>] [--note "..."] [目标工作区]
multica workspace read-grant list [目标工作区]
multica workspace read-grant revoke <授权 ID> [目标工作区]
multica workspace read-grant audit [--limit 50] [目标工作区]
```

接口：`/api/workspaces/{id}/cross-read-grants`（`GET` 列出、`POST` 创建、`DELETE /{grantId}` 撤销、`GET /audit` 审计）。

- 不写 `--agent` 就是来源工作区的全部智能体。同时有「指定智能体」和「全部智能体」两条授权时，审计记在指定智能体那条上。
- 授权没有项目范围（Owner 2026-11-26：目前没有权限控制需求），覆盖目标工作区里全部非私有项目。
- 每次请求都查库，不缓存：撤销或过期后，下一次请求就被拒绝。过期按数据库时钟判断。
- 撤销只设置 `revoked_at`，不删除行，审计记录始终能找到对应的授权。

## 智能体怎么读

在来源工作区运行的智能体，给命令加上目标工作区：

```
multica issue get <目标工作区里的 issue 编号> --workspace-id <目标工作区 ID> --output json
multica issue comment list <issue> --workspace-id <目标工作区 ID> --output json
```

服务端按下面的顺序处理带任务令牌的请求：

1. 请求里所有工作区选择器（`X-Workspace-ID`、`X-Workspace-Slug`、`?workspace_id`、`?workspace_slug`、`/api/workspaces/{id}` 路径）都指向同一个工作区，并且不是令牌绑定的工作区；
2. 目标工作区有一条有效授权，覆盖令牌的来源工作区和智能体；
3. 请求的方法和路径在只读白名单里（`server/internal/middleware/cross_ws_read_fork.go` 的 `crossWorkspaceReadRoutes`，默认拒绝）；
4. 先写审计，写入失败返回 503；再按合成的只读成员处理请求。

第 1、2 步任一条不满足，请求交回上游的检查，和以前一样返回 403。第 3 步不满足返回 403，同样记一条审计（`allowed = false`）。

## 能读到什么

| 数据 | 是否可读 |
|---|---|
| issue（列表、查询、搜索、详情、子任务、标签、metadata、PR、附件、订阅者、wakeup） | 可读 |
| 评论、时间线 | 可读 |
| 运行记录和日志（`task-runs`、`/api/tasks/{id}/messages`、`context-receipt`、`usage`、`active-task`） | 可读；chat 运行的记录不可读 |
| 项目、文档、标签、状态、workflow、自定义属性、成员名单 | 可读 |
| 智能体、技能、squad、autopilot 及其运行记录 | 可读；`mcp_config`、webhook 令牌等 secret 仍然脱敏 |
| 私有项目和其中的 issue | 不可读（返回 404，和不存在一样） |
| chat、inbox、通知、pins、视图偏好 | 不可读 |
| 环境和环境日志、智能体 env、MCP 和应用配置、autopilot deliveries | 不可读 |
| 一切写操作 | 拒绝 |

只读成员的身份：`X-User-ID` 是授权 ID（永远不是真实用户），角色 `cross_ws_reader`，`X-Actor-Source` 是 `cross_ws_reader`。所以：

- 按用户的数据都是空的；私有项目按非成员处理，全部隐藏；
- 需要 owner/admin 角色的路由组拒绝；`RequireHumanActor` 把它当机器凭据拒绝；
- 不带来源工作区的 `X-Agent-ID`、`X-Task-ID`，处理函数不会把它当成目标工作区的智能体。

## 审计

表 `cross_workspace_read_audit`，每个被授权匹配到的请求一行：授权 ID、目标和来源工作区、智能体、运行（task）、方法、路由（白名单模式，比如 `GET /api/issues/*`；被拒绝的写路径原样记录，最长 512 字符）、是否放行、时间。不记录查询串和请求体。目前没有自动清理，保留期需要时另做。

## 令牌泄露的影响

授权读取用的是智能体这次运行的 `mat_` 令牌，运行结束令牌就失效。泄露期间，持有者能读到授权范围内的数据，不能写。来源工作区不保存任何长期凭据。

## 迁移

- `9028_cross_workspace_read`：两张新表，不改已有表，没有外键；DDL 放在 `DO` 块里，sqlc 看不到，`pkg/db/generated` 不变。
- `9029`、`9030`：两个索引，`CONCURRENTLY`，登记在 `server/cmd/migrate/crossread_migrations.go`。
- 回滚按 9030、9029、9028 的顺序，删除索引和表（授权和审计一起删除）。
