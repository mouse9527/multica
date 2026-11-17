# Router：事件捕获（S1）、派发与出口（S2）、chat 载体（v6 切片 1）、暂扣影子模式（v6 切片 2）

本文件属于 `mouse9527/multica` fork（S1 是 M-85，S2 是 M-86，设计见 M-45 v5）。Router 让一个智能体订阅一个范围（一棵 issue 树或一个项目），把范围内的事件写进持久的事件队列（S1）；再把攒下的事件合成一批，在锚点 issue 上给这个智能体起一次运行，并给它一个通知 Owner 的出口（S2，见后文「派发（S2）」）。v6 切片 1（M-96）让订阅可以绑定一个 chat 会话：批次写进这个会话，Router 在会话里回话（见后文「chat 载体（v6 切片 1）」）。v6 切片 2（M-97）加上暂扣的影子模式：只记录「本来会扣」的收件箱条目，不真的扣（见后文「暂扣影子模式（v6 切片 2）」）。M-105 让 Owner 在页面上自己设置 Router（见后文「页面设置（M-105）」）。

## 开关

- `router_v1`，默认关闭。用 `FF_ROUTER_V1=true` 打开（或 `MULTICA_FEATURE_FLAGS_FILE` 里的规则）。
- 关闭时：`/api/router/*` 返回 404，`events.Bus` 监听器和对账任务都不注册，行为与上游一致。开关在启动时读一次，改了要重启服务。
- `router_dispatch`（S2），默认关闭，`FF_ROUTER_DISPATCH=true` 打开，**只在 `router_v1` 也打开时生效**。关闭时不注册派发任务，`/api/router/subscriptions/{id}/state` 和 `/api/router/notices` 返回 404，行为与 S1 一致。订阅的修改（`PATCH`）、审计记录和运行列表只要 `router_v1`。
- `router_chat`（v6 切片 1），默认关闭，`FF_ROUTER_CHAT=true` 打开，**只在 `router_dispatch` 也打开时生效**。注意两个读取时点不同：派发任务在**启动时**读一次（决定绑定了 chat 的订阅是否走 chat），`PATCH` 接口在**每次请求时**读（决定是否接受 `anchor_chat_session_id`）。所以不重启就在线打开开关的话，绑定能立刻存下来，但派发要等重启后才会写进会话。关闭时：`PATCH` 里的 `anchor_chat_session_id` 返回 400；已有的绑定保留但不用，所有订阅照旧走锚点运行，行为与 S2 一致。
- `router_hold_shadow`（v6 切片 2），默认关闭，`FF_ROUTER_HOLD_SHADOW=true` 打开，**只在 `router_chat` 也打开时生效**，启动时读一次。关闭时不注册收件箱拦截器，`CreateIfEligible` 与上游完全一致；批次里也没有「Hold preview」一节。
- `MULTICA_ROUTER_RECONCILE_INTERVAL`：对账周期（Go duration，如 `5m`），默认 5 分钟，最小 30 秒。

## 数据（迁移 9004–9007，只新增表）

| 表 | 用途 |
|---|---|
| `router_subscription` | 一个智能体订阅一个范围。`(agent_id, scope_type, scope_id)` 唯一。S2 用到的列（`anchor_issue_id`、合批参数、`state`）S1 只存默认值 |
| `router_event` | 事件队列。`(agent_id, event_key)` 唯一，监听器和对账都按它幂等写入；`source` 为 `bus`（监听器写的）或 `reconcile`（对账补的） |
| `router_issue_seen` | 每个订阅对每个 issue 最后见到的状态和处理人，对账按它做水平比较 |
| `router_reconcile_cursor` | 每个工作区每类边事件（新 issue、评论、最终失败）对账读到了哪里：（时间, 行 id, 订阅）键集 |

`router_subscription.reconciled_through` 是该工作区最近一次**完整跑完**的对账的上界。

S2 的迁移 9008–9013 只新增 3 张表（`router_subscription` 上 S2 用的列 9004 已经建好，不需要改表）：

| 表 | 用途 |
|---|---|
| `router_dispatch` | 每次锚点运行一行：哪个订阅、哪个任务、带了几个事件；运行（连同系统重试）结束后记 `outcome` |
| `router_subscription_change` | 订阅的审计记录：谁改的（`actor`）、凭谁的权限（`authorized_by_user_id`）、经哪个智能体的哪次运行代办（`via_agent_id`、`via_task_id`）；订阅删掉后保留 |
| `owner_notice` | 「通知 Owner」：收件人、标题、正文、关联 issue、紧急程度、`dedupe_key`（同一发送者、同一收件人内唯一）、各渠道投递结果 `channel_states` |

回滚：按 9013→9004 的顺序执行 down（只回滚 S2 就到 9008 为止），表连同数据一起删除；上游表不受影响。

9004 的建表语句放在 `DO` 块里用 `EXECUTE` 执行：sqlc 把迁移目录当 schema 读，直接写的 `CREATE TABLE` 会让 `make sqlc` 往共享的 `pkg/db/generated/models.go` 加 Router 的结构体，而对 sqlc 看不见的表写 `ALTER TABLE` 会让生成直接失败。Router 只用手写 SQL，不需要这些结构体。**以后的 Router 迁移（包括 S2）也要把建表、改表放进 `DO` 块**；`CREATE INDEX CONCURRENTLY` 不能放进 `DO` 块，sqlc 也不读索引语句，照常单独成文件。`TestRouterTablesStayInvisibleToSqlc` 检查所有 `*_router_*.up.sql`。

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
- **状态和处理人的 actor**：只有当事件负载里的 issue（状态、处理人、`updated_at`）就是记录变化时数据库里的状态，才记上事件的 actor；否则（这次变化之后又被别人改了）actor 留空。负载时间只精确到秒，同一秒内两次写入、字段又相同的，记到先处理的那个事件上。
- 事件 `payload` 只放引用和变化的字段，不放评论正文。

## 两条路径

1. **监听器**（`events.Bus`，同步、进程内）：只在该工作区有启用的订阅时工作。订阅在进程内缓存 30 秒，本进程改订阅时立即失效；缓存过期时只有一个发布者去重新加载，其他发布者照用旧缓存、不等待；加载失败按 1 秒起、翻倍、最多 30 秒退避。每个事件（含可能的缓存加载）共用一个 500ms 的超时，做一次祖先链查询和一个短事务；失败只记日志（日志带进程内累计的 `failures_total`），不影响原请求。
2. **对账**（DB 调度器，全局一个租约）：每个周期对每个有启用订阅的工作区（每个工作区单独 1 分钟预算），
   - 先比较 issue 当前的状态、处理人和 `router_issue_seen`，补出差异（逐个 issue 提交）；
   - 再按（时间, 行 id, 订阅）顺序分批（每批 500 行）补新 issue、评论、最终失败的运行，从 `router_reconcile_cursor` 记的位置往后读，**每批提交后游标就前进**，所以预算用完被打断的一轮也保留已做的部分，下一轮接着做；不早于订阅开始捕获的时间；按 `event_key` 去重；
   - 三类都追上之后，再把最近 10 分钟重读一遍，补晚提交的行（只补、不动游标）；
   - 跳过最近 30 秒内的变化，避免和还在写的监听器抢。
   - 全部做完才更新 `reconciled_through`。

**并发**：同一个（订阅, issue）的监听器和对账都先锁住 `router_issue_seen` 那一行，再在同一事务里读 issue 当前状态来比较。所以它们按加锁顺序、对着最新已提交的状态比较，拿着旧快照的一方不会把 seen 改回去，也不会写出方向相反的事件。

对账补进来的每一行都是一次 **`capture_miss`**：监听器漏掉、对账补上的事件数。

已知局限：

- 状态在一个对账周期内 A→B→A 翻回去，对账只看到当前状态，这次抖动会丢（设计上接受）。
- 多副本部署时，新订阅在其他副本最多 30 秒看不见，这段时间的事件由对账补，也会计入 `capture_miss`。
- 订阅开始后才创建、又在一个对账周期内被移进范围的 issue，监听器只把它当作移入（不记事件），对账会补一条 `issue_created`，同样计入 `capture_miss`。
- 写事件时会确认订阅仍是启用的，所以拿着暂停或删除前缓存的监听器不会再写进来；但暂停或删除恰好在这次写入的事务进行中提交时，仍可能留下一条，它照样能按订阅 id 查到，删工作区时清掉。
- 如果一个工作区每 10 分钟里的新行多到一轮预算重读不完，回看阶段总被打断，`reconciled_through` 就不前进（前进阶段不受影响）。`reconciled_through` 停在过去，就说明对账没跑完。

## 订阅与暂停

- 新建订阅时，把范围内每个 issue 的当前状态记为基线，所以已有的状态不会被当成漏掉的变化。
- 暂停：停止捕获，清掉这条订阅的基线；已经暂停的订阅再暂停不改原来的原因。恢复：从恢复那一刻重新捕获并重建基线，暂停期间的变化不补、也不算 `capture_miss`。
- 权限（S2 起）：
  - **成员**：能管理该智能体（智能体所有者或工作区所有者、管理员）就能给它建订阅；修改、暂停、恢复、删除：创建它的成员、被订阅智能体的所有者、工作区所有者或管理员。
  - **智能体订阅自己**：照旧允许，不需要成员授权。
  - **智能体代建、代改**（给别的智能体）：只看这次运行的发起人（任务的 `originator_user_id`），不看提示词。运行必须还没结束，发起人必须是工作区成员，并且按上面成员的规则有权操作目标智能体或订阅。没有人类发起人的运行（autopilot 的规则所有者、owner_fallback 归责）没有这个权限。代建的订阅记为该成员创建（`created_by_type=member`、`owner_user_id` 也是该成员），代办的智能体和运行记在 `via_agent_id`、`via_task_id`。
  - 每次创建、修改、暂停、恢复、删除和自动暂停都写一条 `router_subscription_change`，`multica router changes <id>` 可查。
  - 读取对工作区成员开放，私有项目里的 issue 按现有的可见性规则过滤。

## API 与 CLI

| CLI | API |
|---|---|
| `multica router subscribe --agent <名或ID> (--issue <键> \| --project <ID>) [--events a,b]` | `POST /api/router/subscriptions` |
| `multica router list [--agent]` | `GET /api/router/subscriptions` |
| `multica router get <id>` | `GET /api/router/subscriptions/{id}`（含 `event_count`、`capture_miss`、`reconciled_through`） |
| `multica router pause\|resume <id>` | `POST /api/router/subscriptions/{id}/pause\|resume` |
| `multica router delete <id>` | `DELETE /api/router/subscriptions/{id}` |
| `multica router events [--subscription] [--since 2h\|RFC3339] [--state pending\|consumed] [--source bus\|reconcile]` | `GET /api/router/events` |
| `multica router scope <id> [--all]` | `GET /api/router/subscriptions/{id}/scope` |
| `multica router update <id> [--anchor] [--events] [--debounce] [--max-wait] [--max-runs-per-hour]` | `PATCH /api/router/subscriptions/{id}` |
| `multica router changes <id>` | `GET /api/router/subscriptions/{id}/changes` |
| `multica router release --chat <current\|会话> [--keep]` | 多次 `DELETE`，或 `pause` 后 `PATCH anchor_chat_session_id=none` |
| （`--anchor-chat current`、`--chat current` 的解析） | `GET /api/router/chats/current` |
| `multica router runs [--subscription]` | `GET /api/router/dispatches` |
| `multica router state get <id>` | `GET /api/router/subscriptions/{id}/state` |
| `multica router state set <id> --expect-revision N (--state \| --state-file \| --state-stdin \| --clear)` | `PUT /api/router/subscriptions/{id}/state` |
| `multica notify owner --title … [--body…] [--ref <issue>]… [--urgency normal\|high] [--dedupe-key] [--subscription]` | `POST /api/router/notices` |
| `multica notify list [--subscription]` | `GET /api/router/notices` |

`subscribe` 也接受 `--anchor`、`--debounce`、`--max-wait`、`--max-runs-per-hour`。`POST /api/router/subscriptions` 还接受 `anchor_chat_session_id`，建订阅时直接绑定 chat（M-105，需要 `router_chat`）。

订阅 ID 可以用前缀。智能体在运行里调用 `subscribe` 时可以省略 `--agent`，默认订阅自己。

## 怎么看 capture_miss

- `multica router get <id>` 或 `multica router list` 的 `CAPTURE MISS` 列：这条订阅累计的对账补录数；`RECONCILED THROUGH` 列：它覆盖到什么时候。
- `multica router events --source reconcile`：逐条看补了哪些。
- 对账任务每次运行把本次补录数写进 `sys_cron_executions`（`job_name='router_reconcile'` 的 `result`），补到时也打一行 `router: reconcile added events the listener missed` 日志。

**只有对账确实跑完了，`capture_miss` 才能当覆盖证据**：先看 `reconciled_through` 是否接近现在（落后不超过一两个对账周期）。它停在过去或是 `never`，说明对账没跑完，这时 `capture_miss` 为 0 不说明任何问题；去看 `router: reconcile did not finish` 日志和 `sys_cron_executions` 里 `router_reconcile` 的失败记录。

`reconciled_through` 正常前进、`capture_miss` 持续为 0，说明 Bus 覆盖了这几类写入路径，可以考虑放宽对账周期；持续非 0，按 `kind` 找没有发布 Bus 事件的写入路径。

## 派发（S2）

### 合批

- 派发任务每 15 秒跑一次（DB 调度器，全局一个租约；并发跑也安全）。
- 订阅下**别人造成的**（非 `self`）未消费事件满足任一条件就派发：最后一个事件之后 `debounce_seconds`（默认 60，最小 10）内没有新事件；或最早的那个已经等了 `max_wait_seconds`（默认 600）。
- 一批最多 200 个事件，按捕获顺序；自己的事件（`self`）不会触发派发，但会跟着下一批一起被标为已消费，不写进提示词。
- 没有锚点的订阅只捕获、不派发：`issue_tree` 默认锚点是树根；`project` 订阅要用 `--anchor` 指定（S1 建的项目订阅没有锚点，所以打开 `router_dispatch` 后也不会派发，直到设置锚点）。

### 锚点运行

- 走与「提及一个智能体」相同的入队路径（`service.EnqueueRouterRun`），所以归责、私有项目、triage 守卫和 `(issue, agent)` 唯一索引都照旧；没有成员是这次运行的发起人。daemon 不用改：批次写进任务的 `handoff_note`，渲染进提示词。
- 提示词里有：本批事件（时间、issue、类型、变化、谁做的）、范围内未关闭的 issue（最多 40 条，按最近更新）、订阅的 `state` 和 revision，以及继续查询、写 state、通知 Owner 的命令。
- **按 Router 串行**（见下面「并行与串行」）：同一条订阅同时只有一个运行；同一个智能体的不同订阅可以并行。
- 这个智能体在锚点 issue 上已有排队的运行，或锚点 issue 在 triage 中：本轮不派发，下一轮再试。所以同一个智能体的两条订阅用同一个锚点 issue 时，实际上还是排队；要并行就各用各的锚点 issue。

### 并行与串行（M-104）

Owner 2026-11-16 定：Router 是实例（像 issue），智能体只是执行模板。同一个智能体可以配多个 Router（多条订阅、多个 chat），不同 Router 并行，取代 S2 的「按智能体加锁、同一智能体同时只有一个在途运行」。

**串行单位是 Router 实例**：

| 订阅 | Router 实例 | 派发时拿的锁（事务级 advisory，拿不到就跳过这一轮） | 有在途运行就不派发 |
|---|---|---|---|
| 没绑 chat（锚点运行），或 `router_chat` 关闭 | 订阅 | `router:sub:<订阅>` | 这条订阅有没结算的派发 |
| 绑了 chat（`router_chat` 打开） | chat 会话 | `router:sub:<订阅>` 加 `router:chat:<会话>` | 这条订阅有没结算的派发，或这个会话里有任何订阅没结算的批次轮次 |

- 多条订阅绑同一个 chat 时按会话串行：它们共享会话上下文，一个会话同时只有一个批次轮次。
- 订阅之前有一个没结算的锚点运行、后来绑了 chat：等那个运行结算后才往 chat 写批次。
- 绑定是在订阅的行锁下读的；改绑定（`router update --anchor-chat`）也先锁这一行，所以派发事务里读到的绑定在提交前不会变。
- 不同 Router 不互相等待。并发上限交给认领：智能体的 `max_concurrent_tasks`、runtime 的上限（`agent_runtime.max_concurrent_tasks`），以及上游认领本来就有的「同一 issue 同一智能体」「同一 chat 会话」串行。超过上限时，任务在队列里等，批次已经交给这个任务（事件 `consumed_task_id` 指向它），不会丢；它最终失败或被取消时，结算照旧把事件退回队列。
- 不需要迁移：会话串行靠 `router_dispatch.task_id` 关联到任务的 `chat_session_id` 判断。

**原来按智能体加锁保护的东西，现在怎么保证**：

- **事件消费**：事件按订阅存。派发在订阅行锁下读批次，写 `consumed_task_id` 时还带 `consumed_task_id IS NULL`，同一事件不会进两批。
- **`state`**：按订阅存、带 revision。同一订阅串行，所以同一时间只有一个运行读写它；不同订阅的 state 互不相干。写入照旧要带 revision，冲突返回 409。
- **结算与退回**：按派发行处理，只退回这个任务消费的事件（`consumed_task_id = 任务`），不涉及别的订阅。
- **每小时上限**：按订阅计数（`router_dispatch.subscription_id`），在订阅行锁下数，和派发写入在同一事务，不会被并发派发绕过。
- **暂停**：按订阅（`UPDATE … WHERE paused_reason IS NULL`），通知按「订阅、原因、小时」去重。
- **chat 会话**：会话锁加「会话内没有在途批次」保证一个会话同时只有一个批次轮次；上游认领也按会话串行；Owner 的发言优先级仍高于批次。
- **多副本、滚动升级**：订阅行锁 `FOR UPDATE` 始终在，旧版本（拿智能体锁）和新版本同时派发同一订阅时也由它串行，后到的看到在途派发就跳过。

### 运行结束

- 每轮先结算：沿 `retry_of_task_id` 跟踪系统重试，整条链都结束才结算。有一次 `completed` 就记 `completed`；否则记 `failed`，把这批事件的 `consumed_task_id` 清空，**重新排队**，下一批再带上（至少一次投递；Router 的动作要能重复执行，这是对提示词的要求）。运行行被删掉的也按失败处理。

### 暂停（不静默）

以下情况把订阅的 `paused_reason` 设为 `rate` 或 `agent_unavailable`，**捕获继续**，只停派发，并给 Owner 发一条紧急通知（每个订阅、原因、小时只发一条）：

- 最近一小时的派发次数达到 `max_runs_per_hour`（默认 6，最大 60）→ `rate`；
- 智能体被归档、没有 runtime，锚点 issue 被删，或入队被拒（私有项目无权限、工作区归责 fail-closed）→ `agent_unavailable`。

`multica router resume <id>` 清掉原因，暂停期间攒下的事件作为一批派发。手动 `pause` 仍是 S1 的含义（停止捕获）。通知的收件人是订阅的 `owner_user_id`，没有时是智能体所有者。

### state

`router_subscription.state`，文本，最多 8KB（超出返回 413），带 `state_revision`。写入必须带上读到的 revision，不一致返回 409，由调用方重读、合并再写。只有被订阅的智能体本身和工作区所有者、管理员能写；工作区成员能读。各 issue 正文的「已定决定」仍是唯一事实，`state` 只是 Router 自己的索引。

## 出口：owner_notice

- `multica notify owner` 写一行 `owner_notice`，再交给各渠道适配器；Router 只调用这一个原语，不知道渠道。
- 收件人：带 `--subscription` 时是该订阅的 `owner_user_id`（没有时是智能体所有者），智能体只能用自己的订阅；不带时，智能体通知自己的所有者，成员通知自己。
- `--dedupe-key` 只在同一发送者（成员、智能体或系统）发给同一收件人的通知之间去重：重复发送返回第一条（HTTP 200，`created=false`），不会再投递。别人用同一个键发的通知互不影响，也读不到。智能体每小时最多发 30 条（429）。
- **适配器**（`agentrouter.Adapter`）：MVP 只有收件箱。投递前先把该渠道记为 `unknown` 再发送，发完写 `sent`、`failed` 或 `skipped`；**`unknown` 不自动重试**（发送不幂等）。派发在事务里写的暂停通知，提交后立即投递；进程在这之间退出的，下一轮派发补投。
- **收件箱适配器**：写一条 `type=router_notice` 的收件箱条目，`severity` 为 `action_required`（`--urgency high`）或 `info`，issue 是第一个 `--ref`；走现有的收件箱资格检查（私有项目不可见的成员会得到 `skipped`）。这个类型不在通知分组里，不会被静音或专注模式过滤。前端把它当未知类型：列表和详情显示标题和正文，详情头部没有类型名称（本切片不改前端）。chat、IM 适配器只留接口。

## chat 载体（v6 切片 1）

设计见 M-95「已定决定」2026-11-16。订阅绑定一个 chat 会话后（`router_chat` 打开时），派发不再在锚点 issue 上起运行，而是把批次写进这个会话、由一个 chat 任务读它。Owner 在同一个会话里和 Router 说话，Router 也在这里回话。合批、每小时上限、暂停、`state`、结算与重派都和锚点运行一样；串行单位是会话（见「并行与串行」）。

### 绑定

- 迁移 9014 新增 `router_chat_anchor(subscription_id, workspace_id, chat_session_id, …)`，不改 `router_subscription`、`router_dispatch`。订阅的 JSON 多一个 `anchor_chat_session_id`。
- `multica router update <id> --anchor-chat <session-uuid>`（API：`PATCH /api/router/subscriptions/{id}`，`anchor_chat_session_id`）。服务端校验：会话在本工作区、没有归档；会话的智能体就是订阅的智能体；会话的创建者就是订阅的 Owner（`owner_user_id`，没有时是智能体所有者）；订阅仍有锚点 issue（派发记录照旧写它，锚点 issue 继续当看板）。
- `--anchor-chat none` 解除绑定，回到锚点运行。绑定、解绑都记在审计里（`detail.anchor_chat_session_id`）。

### 批次怎么进会话

- `service.EnqueueRouterChatRun`（`server/internal/service/router_chat_run.go`）在一个事务里做和成员发消息相同的事：锁会话 → 建 chat 任务 → 任务拥有自己的输入 → 写一条 `role=user`、`message_kind=router_batch` 的消息。daemon 不用改：认领时这条消息就是本轮的输入。
- 批次正文第一行是「[Router 事件批次 · 非 Owner 发言]」，因为认领时多条输入直接拼接、不带作者；正文告诉 Router：有话要 Owner 知道或决定才回文字，否则不输出文字。
- 批次任务优先级 1，Owner 的 chat 是 2：排队中的 Owner 发言先被认领；正在跑的批次不会被打断。批次任务没有发起人（`originator_user_id` 为空，Router 在其中没有任何成员的授权），归责照旧回落到智能体所有者，`initiator_user_id` 是会话创建者。
- **私有项目围栏与锚点运行一致**（MUL-8472）：批次里有 issue 标题和评论内容，所以写入之前先对锚点 issue 做 `guardRunProjectAccess`。这里要求两个人都能看到这个项目：一是会话创建者（批次写进的是他的会话），二是智能体所有者（在他的 runtime 上执行）。任一方看不到，就返回 `ErrProjectAccessDenied`，映射为 `ErrRunRefused`：派发暂停（`agent_unavailable`），并发收件箱通知，消息和任务都不写。
- 和锚点运行的一个差别：锚点 issue 进入 triage 时，锚点运行会一直等待；chat 载体不受影响，因为 chat 模式下锚点 issue 只是派发记录和看板，不是运行的位置。
- 派发记录（`router_dispatch`）照旧写锚点 issue，`task_id` 是 chat 任务。
- **批次轮次不做系统自动重试**（任务的 `max_attempts` 设为 1）。上游会把 chat 重试的优先级提到至少 3，那样重试中的批次会排到 Owner 排队的发言前面。批次轮次失败或被取消时，会话里照上游规则留一条失败消息（不弹通知）；派发结算为 `failed`，把事件退回队列，下一轮作为新批次（优先级 1）重新写进会话，所以整条链都排在 Owner 之后，事件也不会丢。

### 通知与显示

- 批次轮次没有文字输出时，上游照常写一条 `no_response`，它不弹通知；有文字输出就是一条普通回复，走 chat 回复通知（#10424），这就是 Router 找 Owner 的方式。
- 会话列表的预览不显示批次消息和批次轮次的 `no_response`，未读数也不算后者（`chat.sql` 两个列表查询）。
- 前端：批次消息显示为折叠的卡片（标题是正文前两行），批次轮次的 `no_response` 不显示：按输入归属识别，即 `no_response` 的任务就是写这条批次的任务（批次轮次没有系统重试，回复总是带着这个任务 ID）（`packages/views/chat/components/router-batch.tsx`）。批次写入时广播的 `chat:message` 不带 `message_id`，客户端只刷新、不先插一条不带类型的用户气泡。

### chat 不可用时

会话被归档、删除，转给了别的智能体，或会话创建者已不是工作区成员：每轮派发都会检查已绑定会话的订阅，发现了就把订阅的派发暂停（`agent_unavailable`，捕获继续），并用 `owner_notice` 走收件箱通知 Owner（不走这个已经不可用的会话），正文写明原因和恢复办法（重新绑定或 `--anchor-chat none`，再 `resume`）。入队时才发现会话已归档（竞态）也同样暂停。

这类暂停沿用 `agent_unavailable`（9004 的 CHECK 只允许 `rate`、`manual`、`agent_unavailable`，本切片不改表），所以 `multica router list` 和按原因统计的告警分不出「智能体不可用」「项目权限拒绝」和「chat 不可用」。具体原因看 `router changes` 里 `auto_pause` 记录的 `detail.why`，或收件箱通知的正文。

## 暂扣影子模式（v6 切片 2）

设计见 M-95「已定决定」2026-11-16（D1–D4）和研析 07:47 修订稿第 2 节。暂扣的目标是：Owner 收件箱里落在 Router 范围内的条目，先过 Router，再决定进不进收件箱。本切片只做**影子模式**：判定照做、记录照写，条目照常写进收件箱，收件箱的行为不变。真扣是切片 3。

### 钩子

- 上游只改一处：`inbox.CreateIfEligible` 在写入前调用 `intercept`（fork 新文件 `server/internal/inbox/intercept.go`）。注册方式是 `inbox.SetInterceptor(i, budget)`；没有注册时直接写入，与上游一致。
- **fail-open**：拦截函数返回错误、panic，或超过预算（`DefaultHoldBudget`，250 毫秒），都照常写入。只有在预算内干净地返回 `hold=true` 才会不写（切片 3 才会用到；影子模式永远返回 `false`）。
- 拦截函数在**独立的 goroutine** 里跑，写入方最多等预算这么久，拦截函数不响应 context（例如卡在不受 context 控制的调用里）也一样；超时后才返回的结果一律作废。同时在跑的调用最多 `inbox.InterceptorSlots`（64）个，超时未返回的调用继续占着名额；名额用完时后续写入直接跳过拦截，不等待。这两种情况分别计入 `hook.abandoned`、`hook.saturated`（见「观测」），日志第一次和之后每 100 次各打一条。
- 注册了拦截器时，没有 id 的条目在钩子里先生成 id（UUIDv7），拦截器和写入看到的是同一个 id。现有 12 个调用点都已自带 id。

### 判定（D4）

同时满足才算「本来会扣」：

- 收件人是成员，条目有 `issue_id`；
- 收件人是某个订阅的 Owner，这个订阅启用、未暂停，并绑定了一个 `active`、智能体相同的 chat 会话；
- issue 在这个订阅的范围内（祖先链加项目）。收件人有多个这样的订阅时取最窄的那个（与 `pickNarrowest` 同一规则，跨智能体比较）；
- 不在不扣名单里：`router_notice`（Router 自己的通知，含暂停、故障通知）、连接应用的四类条目（`app_action_requested`、`app_action_reminder`、`app_connect_requested`、`app_action_result`）、`autopilot_paused`、`autopilot_quota_exceeded`、`quick_create_failed`、`quick_create_unconfirmed`；
- 条目的 actor 不是这个订阅的智能体（Router 自己的动作不扣）。

订阅集合按 30 秒缓存（与捕获相同），订阅变更会让本进程的缓存立刻失效；派发自动暂停不改代数，最多 30 秒后生效。判定只对「收件人有绑定 chat 的订阅」的条目查库（一条递归查询取祖先链和项目），其他条目只读内存。

### 记录

- 迁移 9015 新增 `router_hold`（9016、9017 两个并发索引）。影子模式写 `mode='shadow'`、`status='shadow'` 的行：订阅、智能体、收件人、条目 id、issue、类型、标题、actor、完整的写入内容（`item`，切片 3 放行时原样写入用）、判定耗时（`decide_micros`）。切片 3 要用的列（`deadline_at`、`decided_*`、`dispatch_task_id` 等）已经建好，切片 3 不需要改表。
- 记录**走连接池，不进写入方的事务**：写入方的事务里失败一条语句会中止整个事务，违反 fail-open。代价是写入方的事务回滚时，影子行还在；`in_inbox`（关联 `inbox_item`）区分这种情况，以及收件人不合资格没写进去的情况。
- `inbox_item_id` 唯一，同一条目重复写只记一次。影子行保留 30 天，派发任务每小时清理一次。

### 批次里的清单

`router_chat` 下的批次多一节「Hold preview (shadow mode, nothing to do)」：自上一批以来本来会扣的条目（最多 30 条、3KB），写明它们已经照常进了收件箱，Router 不需要处理。列过的行记下批次任务（`dispatch_task_id`），批次失败时清空，下一批再列。影子行不单独触发批次，跟着下一批走。

### 观测

- `multica router holds [--subscription <id>] [--limit N]`（API：`GET /api/router/holds`，需要 `router_dispatch`）：成员看到发给自己的条目，智能体看到自己订阅的条目。
- `multica router holds --stats`：回答这个请求的服务进程自启动以来的拦截统计——调用次数、本来会扣的次数、错误、超时、panic、issue 不可见（写入方事务里的新 issue）、订阅重载失败、平均与最大耗时、耗时分桶（≤1ms、≤5ms、≤20ms、≤100ms、>100ms），以及钩子层面的 `hook.abandoned`（写入没等到答复就照常写了）和 `hook.saturated`（名额用完、没有调用拦截函数）。统计在内存里，重启清零，多副本时每个进程各算各的；持久的数据是 `router_hold` 的行和其中的 `decide_micros`。
- 拦截失败时日志打一次，之后每 100 次打一次（`router hold: interceptor failed; the inbox write went ahead`）。

### 调用点核对（切片 3 前提）

`CreateIfEligible` 共 12 个调用点。影子模式下钩子永远不扣，所以都不受影响；下表看的是切片 3 真扣时（返回 `inserted=false`）是否安全：

| 调用点 | 条目 | `inserted=false` 时 | 切片 3 |
|---|---|---|---|
| `cmd/server/notification_listeners.go` `createMemberInboxItem`（issue 订阅者，经 `notifyIssueSubscribers`） | 订阅者通知 | 不发 `inbox:new`；**但返回 false 会让该成员不进 `notified`**，于是父 issue 订阅者、项目订阅者两轮又各写一次同一事件 | **不安全**：同一事件会被暂扣 2–3 次。切片 3 要把「被扣」当作已通知（例如钩子返回单独的信号，或这一处把扣下也计入 `notified`） |
| 同文件 `notifyDirect` | 直接通知（指派等） | 不发 `inbox:new`，直接返回 | 安全 |
| 同文件 `notifyMentionedMembers` | 提及 | 跳过这个人 | 安全 |
| `cmd/server/autopilot_failure_monitor.go` | `autopilot_paused` | 跳过 | 不扣（不扣名单，且没有 issue） |
| `internal/connectedapps/requests.go` | 连接应用请求、结果 | 不发 `inbox:new` | 不扣（不扣名单） |
| `internal/service/issue_wakeup_system.go` | `children_done` | 在写入方事务里；`inserted=false` 时活动记录写成 `outcome=none`（而不是 `notified`） | **审计失真**：被扣的会记成「没人通知」。切片 3 要让这一处知道是被扣（记 `held`），否则活动记录误导 |
| `internal/service/autopilot.go` | `issue_subscribed` | 跳过 | 安全 |
| `internal/service/task.go`（委派失败恢复耗尽） | `task_failed`（`action_required`） | 在写入方事务里；只是不发 `inbox:new` | 安全；但切片 3 若在事务里写暂扣行，要用保存点或照影子模式走连接池 |
| `internal/service/task.go` `quick_create_done` | 快速创建完成 | 跳过 | 安全 |
| `internal/service/task.go` `writeQuickCreateOutcomeInbox` | 快速创建失败/未确认 | 跳过 | 不扣（不扣名单，且没有 issue） |
| `internal/service/autopilot_quota_notifications.go` | `autopilot_quota_exceeded` | 跳过 | 不扣（不扣名单，且没有 issue） |
| `internal/agentrouter/notice.go` `InboxAdapter` | `router_notice` | 会把投递记成 `skipped` | 不扣（不扣名单） |

全仓库写 `inbox_item` 只有 `CreateIfEligible` 这一条路（`CreateInboxItem` 没有其他调用方，也没有手写 `INSERT INTO inbox_item`），所以钩子覆盖了全部收件箱写入。

## 已知局限（v6 切片 2）

- 统计只在进程内存里（见「观测」）；要跨重启、跨副本的数字，用 `router_hold` 的行。
- 写入方事务里新建的 issue，判定时还看不到（记 `issue_missing`，不记影子行）。现在的调用点都是在提交之后写收件箱，所以实际不会出现；切片 3 若要覆盖，判定要进写入方事务。
- 订阅被派发自动暂停（`rate`、`agent_unavailable`）后，最多 30 秒内仍可能记影子行。
- 「Hold preview」只出现在 chat 批次里；没有绑定 chat 的订阅不判定、不记录。

## 页面设置（M-105）

Owner 不用 CLI，在页面上（或在 chat 里对智能体下指令，见下文「在 chat 里用指令设置」）把自己的一个 chat 设为 Router、选范围、暂停和恢复、解除，并在设置页看到自己的全部 Router。**Router 是一个 chat 会话加上绑定到它的订阅**，智能体就是这个 chat 的智能体；服务端只认订阅，前端按 `anchor_chat_session_id` 把订阅归成 Router。

### 入口与开关

- 只在 `router_chat` 真正生效（`router_v1`、`router_dispatch`、`router_chat` 都打开）时显示。服务端把这个组合结果作为前端开关 `router_chat` 发到 `/api/config`；关闭时页面上没有任何 Router 入口，chat 头部和设置页与上游一致。
- chat 头部 ⋯ 菜单：「设为 Router」（还不是 Router）或「Router 设置」（已是 Router）。只出现在自己创建的 chat 上；归档的 chat 不能新设为 Router，但已有的 Router 仍能暂停、解除。
- 设置页「个人」组下的「Router」：列出自己的全部 Router（chat 名、智能体、范围、状态、最近一次运行），点一行进入对应的 chat。下面一节列出自己的、没有绑定任何 chat 的订阅（「自己的」= `owner_user_id` 是自己，没有 owner 的按智能体所有者算），可以二次确认后删除。「自己的」= 绑定的 chat 在自己的 chat 列表里（服务端要求 chat 创建者就是订阅的 Owner，所以这就是向自己汇报的那些 Router）。

### 设为 Router

- 范围可以加多个：项目（`project`），或一棵任务树（选根任务，`issue_tree`）。项目列表和任务搜索沿用现有接口，看不到的私有项目不会出现。
- 事件类型默认全选；合批参数（静默期、最长等待、每小时运行上限）默认 60 / 600 / 6，前端按服务端同样的范围校验。
- 每个范围一条订阅，**创建时直接绑定这个 chat**：`POST /api/router/subscriptions` 新增 `anchor_chat_session_id`（需要 `router_chat`），绑定和建订阅在同一个事务里，校验与 `PATCH` 相同（会话在本工作区、未归档、智能体相同、创建者是订阅 Owner）。所以订阅不会有「已建好、还没绑定」而去起锚点运行的窗口；绑定校验失败时什么都不建。
- **锚点 issue**：任务树用树根。项目范围没有自己的 issue，而派发记录和私有项目围栏都按锚点 issue 走，所以页面会在**该项目里**新建一个看板任务「Router 看板 · <chat 名>」作为锚点，并在 Router 设置里显示「看板 ACME-12」的链接。每个项目各建一个（围栏检查的是锚点所在项目）。订阅创建失败时，刚建的看板任务会被删掉。没有为「不要锚点」改后端：那要改 `router_dispatch` 等表，属于迁移。

### Router 设置

- 状态：运行中，或已暂停并列出原因（手动暂停 / 达到每小时运行上限 / 智能体、对话或项目不可用）；自动暂停（`agent_unavailable`）时再显示服务端审计里记的具体原因（`router changes` 里 `auto_pause` 的 `detail.why`）。每个范围也单独标出暂停原因。
- 健康：最近一次运行（时间、结果、事件数；按订阅各查最新一条，不受工作区里其他 Router 运行多少影响）、对账推进到（各订阅 `reconciled_through` 里最早的那个）、漏捕获（`capture_miss` 合计）。
- 修改事件类型、合批参数时写到这个 Router 的每一条订阅；可以继续加范围，或移除某个范围（删除那条订阅；最后一个范围要用「解除 Router」）。
- **同一智能体同一范围只能有一条订阅**（`(agent_id, scope_type, scope_id)` 唯一）。所以加范围时：这个智能体已有一条**未绑定**的同范围订阅（例如解除时选了「保留」），页面直接接回它——先绑到这个 chat 并写入表单的设置，再恢复，不新建、也不再建看板；已经在**别的 chat 的 Router** 里的范围不出现在选择器里，万一撞上（数据过期）就提示先去那边解除或移除。
- 暂停 / 恢复：暂停是手动暂停（停捕获）；恢复同时清掉自动暂停。
- 解除 Router（二次确认）：二选一，「删除它的订阅」，或「保留订阅并暂停」（先暂停、再解绑，所以中间不会退回锚点运行）。保留的订阅出现在设置页「Router」的「不在 Router 里的订阅」一节，可以在那里删除；把任一 chat 设为 Router 并选同样的范围，就会按上一条接回。
- 「经 Router 处理的条目」：给切片 2、3 的暂扣层留的位置，现在只有一句说明。
- Router 状态没有实时事件，打开的设置对话框和列表每 30 秒刷新一次。

### 在 chat 里用指令设置

Owner 也可以在 chat 里直接跟这个 chat 的智能体说「把这个 chat 设为 Router，订阅 X」「暂停这个 Router」「再加一个范围」。智能体用 CLI 完成，和页面改的是同一份数据。

- **当前 chat**：`GET /api/router/chats/current` 返回调用方这次 chat 运行所在的会话（id、标题、智能体、创建者）。只认智能体自己的、还没结束的、带 `chat_session_id` 的运行；成员请求、issue 上的运行、已结束的运行都返回 400，`router_chat` 关闭时也是 400。CLI 的 `current` 就是用它解析的。
- **授权沿用 S2 的代建规则**：智能体绑定 chat 时（`POST` 带 `anchor_chat_session_id`，或 `PATCH` 绑到某个 chat），这次运行的成员发起人必须就是这个 chat 的创建者，否则 403；建订阅时发起人还要能管理这个智能体。即使智能体订阅的是自己，也按「为发起人代建」记：`created_by_type=member`、`owner_user_id` 是发起人，智能体和运行记在 `via_agent_id`、`via_task_id`。Router 批次轮次没有发起人，不能新建或改绑 Router；别人发起的运行也不能替 Owner 建 Router。暂停、恢复、删除仍按原来的规则（智能体可以改自己的订阅）。
- `GET /api/router/subscriptions?chat_session_id=<id>`：只列绑定到这个 chat 的订阅，即一个 Router。
- CLI：
  - `multica router subscribe (--issue | --project) --anchor-chat current|<会话>`：建订阅时直接绑定；`--project` 不带 `--anchor` 时自动在该项目里建看板任务「Router 看板 · <chat 名>」作锚点，订阅被拒时删掉它。
  - `router get|pause|resume|delete|update --chat current|<会话>`：作用于这个 Router 的每一条订阅（`get` 列出它们）。
  - `router update <订阅> --anchor-chat current`：把订阅绑到当前 chat。
  - `router release --chat current|<会话> [--keep]`：解除 Router；默认删除订阅，`--keep` 先暂停再解绑。
- 给智能体的说明：`docs/router-setup-skill/SKILL.md`（`router-setup` skill 草案：口令对应的命令、改动前先复述并等确认、失败时怎么告诉 Owner）。由 Mika 在 Owner 确认后挂到相关智能体上。

### 代码位置

- 前端：`packages/core/router/`（API、zod 校验、查询与变更、分组）；`packages/views/router/`（对话框、范围选择、列表页、两个挂载钩子）；文案在 `packages/views/locales/*/router.json`。
- 上游触点：`packages/core/api/client.ts` 新增一个公开方法 `forkRequest`（fork 模块借用共享的请求路径），`packages/core/package.json` 导出 `./router`，`chat-session-header.tsx` 与 `settings-page.tsx` 各加一个挂载钩子，8 个语言入口和 `resources-types.ts` 登记 `router` 命名空间，`featureflags/keys.go` 公开 `router_chat`。见 [github-issues-upstream-changes.md](github-issues-upstream-changes.md)。

## 已知局限（v6 切片 1）

- 会话创建者离开工作区时，Owner 就是这个人，收件箱不再收他的通知：通知记为 `skipped`，订阅仍然暂停，用 `multica router list` 可以看到。
- Owner 在界面上取消一个排队中的批次轮次时，上游会把这条输入当成被撤回的草稿放回输入框；事件仍会退回队列重派。
- 移动端没有改：批次消息在移动端显示成一条普通的用户气泡（`message_kind` 未知时退化为 `message`），每个静默的批次轮次还会成对显示一条「本轮无文字回复」的 `no_response` 占位（移动端照常渲染它）。网页端没有这些噪音；移动端是否做同样的隐藏，留给后续切片决定。
- 批次轮次因基础设施原因失败（runtime 离线、超时等）时不会原地重试，而是作为新批次重派，所以会话里会留一条失败消息和一条新的批次消息；重派计入每小时上限，一直失败会触发 `rate` 暂停并通知 Owner。

## 已知局限（并行，M-104）

- `self` 仍按智能体判定：同一个智能体的 Router 1 在 Router 2 的范围里做的事，对 Router 2 也算 `self`，不会触发 Router 2 的批次（只跟着下一批被标为已消费）。范围通常不交叉（事件只投最窄订阅，Owner 已认可）；需要交叉时，用不同的智能体。
- 同一个智能体的两条锚点订阅共用一个锚点 issue 时，受上游「同一 issue 同一智能体」唯一索引和认领串行限制，仍然排队。

## 已知局限（页面设置，M-105）

- 只做了网页端（含手机浏览器的窄屏布局）。原生移动 App（`apps/mobile`）没有 Router 设置入口。
- 看板任务是普通任务，会出现在项目看板里；解除 Router 或删除范围时不会自动删它。
- 页面上改设置是逐条订阅提交的，不是一个事务：中途失败时，已提交的那几条保留新值，刷新后能看到实际状态。

## 已知局限（S2）

- 入队成功但随后的提交失败（极少见）时，这批事件仍是未消费，之后会再派发一次：至少一次，不是恰好一次。
- 已消费的事件不清理（设计里的 14 天清理没做，免得改变 `capture_miss` 的计数口径）；事件多了再加。
- 锚点 issue 被删、入队被拒都归到 `agent_unavailable`（9004 的 CHECK 只允许 `rate`、`manual`、`agent_unavailable`，S2 不改表）；通知正文写了具体原因。

## 部署清单（S2）

1. 备份数据库，在真实数据副本上演练迁移 9008–9013（up、down、再 up）。
2. **把本机 daemon 给智能体用的 CLI 更新到 fork 最新 `main`**：现在的版本没有 `router`、`notify` 子命令，智能体在运行里调用 `multica router state …`、`multica notify owner` 会失败。
3. 部署后先保持 `router_dispatch` 关闭，确认迁移和 S1 捕获正常；再打开 `FF_ROUTER_DISPATCH=true`（需要重启）。
4. 打开前用 `multica router list` 检查现有订阅：`issue_tree` 订阅默认有锚点，打开后就会开始派发；只想继续观察的，先 `router update` 调大参数或暂停。
5. 打开后用 `multica router runs`、`multica notify list` 和 `sys_cron_executions`（`job_name='router_dispatch'`）观察。

## 部署清单（v6 切片 1）

1. 备份数据库，在真实数据副本上演练迁移 9014（up、down、再 up）。
2. 更新 daemon 用的 CLI（`router update --anchor-chat`）。
3. 部署后先保持 `router_chat` 关闭，确认行为不变；再打开 `FF_ROUTER_CHAT=true`（需要重启）。
4. Owner 在界面里和 Router 智能体建一个会话，用 `multica router update <id> --anchor-chat <session>` 绑定；在范围内改状态、发评论，确认批次进会话、Router 回话或保持安静。

## 部署清单（v6 切片 2）

1. 备份数据库，在真实数据副本上演练迁移 9015–9017（up、down、再 up）。
2. 更新 daemon 用的 CLI（`router holds`）。
3. 部署后先保持 `router_hold_shadow` 关闭，确认行为不变；再打开 `FF_ROUTER_HOLD_SHADOW=true`（需要重启，且 `router_chat` 已打开）。
4. 在范围内 @Owner、改状态，确认收件箱照常收到；`multica router holds` 能看到对应的影子行，下一批 chat 批次里有「Hold preview」。
5. 观察几天：`multica router holds --stats` 的耗时分桶和错误、超时计数；在副本上压测写入延迟的结论一起交给切片 3。
