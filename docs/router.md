# Router：事件捕获（S1）

本文件属于 `mouse9527/multica` fork（M-85，设计见 M-45 v5）。Router 让一个智能体订阅一个范围（一棵 issue 树或一个项目），把范围内的事件写进持久的事件队列。**S1 只做捕获**：不派发任何运行，不合批，不写 `state`，不发 `owner_notice`，这些是 S2。

## 开关

- `router_v1`，默认关闭。用 `FF_ROUTER_V1=true` 打开（或 `MULTICA_FEATURE_FLAGS_FILE` 里的规则）。
- 关闭时：`/api/router/*` 返回 404，`events.Bus` 监听器和对账任务都不注册，行为与上游一致。开关在启动时读一次，改了要重启服务。
- `MULTICA_ROUTER_RECONCILE_INTERVAL`：对账周期（Go duration，如 `5m`），默认 5 分钟，最小 30 秒。

## 数据（迁移 9004–9007，只新增表）

| 表 | 用途 |
|---|---|
| `router_subscription` | 一个智能体订阅一个范围。`(agent_id, scope_type, scope_id)` 唯一。S2 用到的列（`anchor_issue_id`、合批参数、`state`）S1 只存默认值 |
| `router_event` | 事件队列。`(agent_id, event_key)` 唯一，监听器和对账都按它幂等写入；`source` 为 `bus`（监听器写的）或 `reconcile`（对账补的） |
| `router_issue_seen` | 每个订阅对每个 issue 最后见到的状态和处理人，对账按它做水平比较 |

回滚：按 9007→9004 的顺序执行 down，三张表连同数据一起删除；上游表不受影响。

9004 的建表语句放在 `DO` 块里用 `EXECUTE` 执行：sqlc 把迁移目录当 schema 读，直接写的 `CREATE TABLE` 会让 `make sqlc` 往共享的 `pkg/db/generated/models.go` 加 Router 的结构体。Router 只用手写 SQL，不需要这些结构体。`TestRouterTablesStayInvisibleToSqlc` 守住这一点。

## 捕获什么

| 事件 | 来源 | `event_key` |
|---|---|---|
| `issue_created` | 范围内新建的 issue（含新子 issue） | `issue:{id}:created` |
| `status_changed` | 状态与 `router_issue_seen` 不同 | `issue:{id}:status_changed:{updated_at}` |
| `assignee_changed` | 处理人与 `router_issue_seen` 不同 | `issue:{id}:assignee_changed:{updated_at}` |
| `comment_created` | 新评论 | `comment:{id}` |
| `task_failed` | 运行最终失败（还要自动重试的失败不算） | `task:{id}:failed` |

- **范围**：`issue_tree` 是根 issue 及其后代（最多 32 层），`project` 是项目里的 issue（按 issue 自己的 `project_id`）。
- **最窄范围**：同一个智能体的多条订阅都覆盖某个 issue 时，事件只记到最窄的那条：离 issue 更近的树根优先，任何树优先于项目，同级取创建最早的。不同智能体各记各的。
- **self**：事件的 actor 就是订阅的智能体时 `self=true`（S2 不会因为它派发运行）。对账补到的状态和处理人变化不知道 actor，`self=false`。
- 事件 `payload` 只放引用和变化的字段，不放评论正文。

## 两条路径

1. **监听器**（`events.Bus`，同步、进程内）：只在该工作区有启用的订阅时工作。订阅在进程内缓存 30 秒，本进程改订阅时立即失效；缓存过期时只有一个发布者去重新加载，其他发布者照用旧缓存、不等待；加载失败按 1 秒起、翻倍、最多 30 秒退避。每个事件（含可能的缓存加载）共用一个 500ms 的超时，做一次祖先链查询和一个短事务；失败只记日志和计数，不影响原请求。
2. **对账**（DB 调度器，全局一个租约）：每个周期对每个有启用订阅的工作区，
   - 按时间窗口补新 issue、评论、最终失败的运行（窗口下界是上次上界减 10 分钟，且不早于订阅开始捕获的时间），按 `event_key` 去重；
   - 比较 issue 当前的状态、处理人和 `router_issue_seen`，补出差异；
   - 跳过最近 30 秒内的变化，避免和还在写的监听器抢。

对账补进来的每一行都是一次 **`capture_miss`**：监听器漏掉、对账补上的事件数。

已知局限：

- 状态在一个对账周期内 A→B→A 翻回去，对账只看到当前状态，这次抖动会丢（设计上接受）。
- 多副本部署时，新订阅在其他副本最多 30 秒看不见，这段时间的事件由对账补，也会计入 `capture_miss`。
- 订阅开始后才创建、又在一个对账周期内被移进范围的 issue，监听器只把它当作移入（不记事件），对账会补一条 `issue_created`，同样计入 `capture_miss`。

## 订阅与暂停

- 新建订阅时，把范围内每个 issue 的当前状态记为基线，所以已有的状态不会被当成漏掉的变化。
- 暂停：停止捕获，清掉这条订阅的基线。恢复：从恢复那一刻重新捕获并重建基线，暂停期间的变化不补、也不算 `capture_miss`。
- 权限：智能体只能订阅自己；成员要能管理该智能体（智能体所有者或工作区管理员）。暂停、恢复、删除：订阅的智能体本身、创建它的成员、工作区所有者或管理员。读取对工作区成员开放，私有项目里的 issue 按现有的可见性规则过滤。

## API 与 CLI

| CLI | API |
|---|---|
| `multica router subscribe --agent <名或ID> (--issue <键> \| --project <ID>) [--events a,b]` | `POST /api/router/subscriptions` |
| `multica router list [--agent]` | `GET /api/router/subscriptions` |
| `multica router get <id>` | `GET /api/router/subscriptions/{id}`（含 `event_count`、`capture_miss`） |
| `multica router pause\|resume <id>` | `POST /api/router/subscriptions/{id}/pause\|resume` |
| `multica router delete <id>` | `DELETE /api/router/subscriptions/{id}` |
| `multica router events [--subscription] [--since 2h\|RFC3339] [--state pending\|consumed] [--source bus\|reconcile]` | `GET /api/router/events` |
| `multica router scope <id> [--all]` | `GET /api/router/subscriptions/{id}/scope` |

订阅 ID 可以用前缀。智能体在运行里调用 `subscribe` 时可以省略 `--agent`，默认订阅自己。

## 怎么看 capture_miss

- `multica router get <id>` 或 `multica router list` 的 `CAPTURE MISS` 列：这条订阅累计的对账补录数。
- `multica router events --source reconcile`：逐条看补了哪些。
- 对账任务每次运行把本次补录数写进 `sys_cron_executions`（`job_name='router_reconcile'` 的 `result`），补到时也打一行 `router: reconcile added events the listener missed` 日志。

持续为 0，说明 Bus 覆盖了这几类写入路径，可以考虑放宽对账周期；持续非 0，按 `kind` 找没有发布 Bus 事件的写入路径。
