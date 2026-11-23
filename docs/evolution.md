# 进化（Evolution）：内置系统 agent

本文件属于 `mouse9527/multica` fork（M-135、M-136、M-137、M-138，设计见 M-132）。「进化」是和上游 Mika 同一机制的内置系统 agent：定期分析工作区里的对话和评论，针对 agent、工作流、团队提出改进提案，由所有者或管理员批准后再执行。

**当前阶段是 P4**：P1 的身份与一键启用、P2 的配置、游标、每日调度和数据通道、P3 的提案接口之上，所有者或管理员可以在提案 issue 上**应用**提案、再**回滚**，每次都追加一条版本记录。提案本身仍不会改动任何配置，进化 agent 也调不了 apply；效果度量（P5）在后续任务里做。

## 开关

- `evolution_v1`，默认关闭。用 `FF_EVOLUTION_V1=true` 打开（或 `MULTICA_FEATURE_FLAGS_FILE` 里的规则），每次请求时读取。
- 关闭时：`/api/evolution/*` 返回 404，前端不显示设置入口、也不发请求；不注册定时任务（服务启动时读开关；任务每次运行也会再读一次，运行中关闭开关即停止调度）。行为与上游一致。
- 开关常量在 fork 文件 `server/internal/featureflags/evolution_fork.go`；`keys.go` 只多一行，把它发布给前端。
- 开关打开期间已启用的「进化」agent，在开关关闭后照常存在、照常可用（它的系统提示词仍随二进制下发），只是不能再通过接口启用新的。

## 身份

| 项 | 值 |
|---|---|
| `agent.system_key` | `evolution` |
| `agent.kind` | `user`（与 Mika 相同：可见、可分派、不随 runtime 硬删） |
| 名称 | 按启用时的界面语言：`Evolution` / `进化` / `진화` / `進化`，之后可改名 |
| 可见性 / 调用 | `visibility=workspace`、`permission_mode=public_to`，调用目标是整个工作区（全员可对话、可指派） |
| 并发 | `max_concurrent_tasks=1` |
| 归档 | 和所有系统 agent 一样不能归档 |

公开的 `POST /api/agents` 不接受 `system_key` 和 `kind`（忽略未知字段），所以客户端不能伪造「进化」。

## 系统提示词与技能

- 系统半段是随二进制发布的 `server/internal/service/builtin_agents/evolution/INSTRUCTIONS.md`（`go:embed`），在领取任务时与工作区自己写的 `agent.instructions`（notes）拼接，notes 放在「Workspace notes」小节下。发版即对所有工作区生效，不写库，所以发版不会覆盖 notes。
- 技能 `multica-evolution`（提案格式、归因方法、护栏）只发给 `system_key=evolution` 的 agent。
- 提示词要求：只在分析 issue 上通过 `multica evolution signals` 读数据，发一条报告评论后结束；不开提案、不改任何东西。在分析 issue 之外，不主动扫描工作区的 chat、评论和运行记录。
- 引用规则写进提示词和技能：chat 不贴原文，只写归纳并指向会话；评论最多摘一句、不超过 120 字；不写成员姓名。

## 接口

都在 `/api/evolution` 下，需要工作区成员身份；开关关闭时 404。

| 方法 | 路径 | 谁可以 | 说明 |
|---|---|---|---|
| `GET` | `/api/evolution` | 任何成员 | `{"agent": <agent>}`，未启用时 `agent` 为 `null` |
| `POST` | `/api/evolution/enable` | 所有者、管理员（人类账号；以 agent 身份或用 task token、cloud PAT 调用一律 403） | 请求 `{"runtime_id", "language": "en"/"zh"/"ko"/"ja", "model"?}`，拒绝未知字段。新建返回 201，已存在返回 200 和同一个 agent。只建 agent，不改配置，所以不会打开任何读取 |
| `GET` | `/api/evolution/config` | 任何成员 | 配置、实际在读什么（`comments_active`、`chat_active`）、服务端上限、最近 10 次运行 |
| `PUT` | `/api/evolution/config` | 所有者、管理员（人类账号） | 请求 `{"config": {...}, "acknowledge_member_notice": bool}`，整块替换配置，拒绝未知字段；超出上限返回 400；要开始读 chat 却没带确认返回 409 |
| `GET` | `/api/evolution/signals` | 只限进化 agent 的分析运行（见下） | `?source=comment\|chat&after=<next_after>&limit=1..200`，返回一页数据 |
| `POST` | `/api/evolution/proposals` | 只限进化 agent 的分析运行（同数据通道的授权） | 提交一条结构化提案，见下文「提案（P3）」 |
| `GET` | `/api/evolution/proposals/{issueId}` | 任何成员 | 提案的状态：改动、`state`（pending / applied / rolled_back）、`stale`、`changed_since_apply`、版本列表；`can_apply` / `can_rollback` 只对所有者、管理员为 true。不是可应用的提案时 404 |
| `POST` | `/api/evolution/proposals/{issueId}/apply` | 所有者、管理员（人类账号；agent 身份、task token、cloud PAT 一律 403） | 应用提案，见下文「应用与回滚（P4）」 |
| `POST` | `/api/evolution/proposals/{issueId}/rollback` | 同上 | 回滚这条提案的应用 |

启用的并发安全与 Mika 相同：先按 `system_key` 快速查重，再在事务里取工作区级 advisory lock（`evolution:<workspace_id>`）后复查，所以并发启用只会建一个。runtime 权限沿用 `canUseRuntimeForAgent`，并在锁内复核。工作区里已有同名 agent 时返回 409，提示先改名。

## 配置（P2）

存在 `workspace.settings.evolution`，由 `PUT /api/evolution/config` 整块写入。没有新表、不改上游表结构。

| 配置 | 默认 | 服务端上限 |
|---|---|---|
| `enabled`（定时分析开关；和 agent 是否启用无关） | false | — |
| `schedule.time` / `schedule.timezone` / `schedule.interval_days` | `03:00` / `Asia/Shanghai` / 1 | HH:MM；已知时区；1–7 |
| `sources.comments` / `sources.chat` | true / false | — |
| `exclude_projects` / `exclude_agents` | 空 | 各最多 200 个 UUID |
| `max_items_per_window`（一次运行最多读多少条，两个数据源合计） | 2000 | 50–5000 |
| `max_window_days`（一次运行从游标往后最多读几天） | 7 | 1–30 |
| `max_runs_per_day`（含补跑） | 3 | 1–10 |
| `max_proposals_per_run`（一次运行最多新开几条提案；合并到已有提案不计） | 5 | 1–20 |
| `min_evidence_count`（一条提案至少要几个独立来源：不同 issue 或不同会话） | 2 | 1–10 |
| `rejected_cooldown_days`（同一提案被取消后多少天内不再提） | 30 | 1–365 |

P3 的三项设置页暂不展示，用 `PUT /api/evolution/config` 修改；设置页保存时原样带回这三项；请求里不带或为 0 时取默认值。

`workspace.settings` 也能经上游的工作区更新接口整块写入，所以读取时一律按上限夹紧，写入时超限直接拒绝。

## 告知成员（P2）

Owner 选了「读全部成员的 chat」，前提是对成员明示：

1. **确认框**：设置页保存一个会开始读 chat 的配置时，先弹确认框；确认后请求带 `acknowledge_member_notice: true`。服务端也检查：没有确认就返回 409，什么都不改。
2. **全员收件箱通知**：确认后，服务端**先**给每个成员发一条 `evolution_notice` 收件箱通知（中英双语：读什么、不读什么、提案怎么引用、在哪里关），**再**建 chat 游标。之后新加入的成员由定时任务补发。
3. **简介**：进化 agent 的描述写明它读什么（新启用的 agent 生效；P1 期间已启用的 agent 描述不会自动改，可手动改）。设置页「进化」分区写同样的范围说明，任何成员可见。

chat 游标是「已告知」的唯一凭证：只有上面第 2 步会建它。关闭 chat（或关闭定时分析）时删除游标，重新打开要再确认、再通知，并且只从那一刻起读，不回补关闭期间的内容。如果有人绕过接口直接改 `workspace.settings` 把 chat 打开，因为没有游标，chat 不会被读，设置页显示「待告知」；定时任务看到配置关闭了 chat 也会删除游标。

## 数据通道（P2）

`GET /api/evolution/signals` 的授权条件同时满足：

- 用 task token 调用（`X-Actor-Source=task_token`，由认证中间件写入）；
- token 上的 agent 是本工作区 `system_key='evolution'` 的 agent；
- token 上的任务正在运行（`dispatched` / `running`），并且它所在的 issue 是本工作区**当前打开的分析运行**的 issue。

成员、其他 agent、进化 agent 在别的 issue 上的任务一律拿不到（403 / 409）。

返回的内容：

| 数据源 | 读 | 不读 |
|---|---|---|
| `comment` | 成员和 agent 写的普通评论（`type='comment'`） | 已删除的评论；系统和状态变更评论；**私有项目**里的 issue 的评论；排除的项目；排除的 agent 写的评论；进化自己写的评论 |
| `chat` | 普通消息（`message_kind='message'`），所有成员 | **私有 agent**（不是 `public_to` 加工作区目标）的对话；挂在私有项目或排除项目下的会话；排除的 agent；和进化自己的对话 |

- 私有项目按已定决定「默认不读」：用 `projectaccess.ForPrincipals` 不带任何人算出隐藏集（即**所有**私有项目都隐藏，不管谁触发、agent 的 owner 是不是项目成员），SQL 里用 `hidden.Param()` 过滤，和上游其他接口同一个函数。测试 `TestEvolutionScheduledRunSeesOnlyWhatItMay` 固定了定时运行能看到和看不到的内容。
- 不读运行日志、`task_message`、工具调用。
- 每条内容出库前脱敏（`internal/evolution/redact.go`）：私钥、JWT、常见令牌前缀、Authorization 头、`password=`/`token:` 这类键值、URL 里的账号密码和查询串、邮箱、手机号、内网 IP 和内网域名，替换成 `[redacted:…]`，并给出替换次数；单条超过 4000 字截断。
- 返回成员和 agent 的 id，不返回姓名。

## 游标与调度（P2）

新增两张表（迁移 `9019_evolution_tables`，只新增表、有 down、DO 块建表让 sqlc 看不到、无外键，工作区删除时由 `evolution.DeleteWorkspaceData` 清理）：

- `evolution_cursor(workspace_id, source, ts, item_id)`：每个数据源分析到的位置，键集 `(created_at, id)`。`source='member_notice'` 是成员加入时间上的同类游标，记录已经告知到哪位成员。
- `evolution_run`：每次分析运行，记录分析 issue、窗口上界、每个数据源已经发给这次运行的位置、已发条数、结果。

定时任务 `evolution_tick` 每 5 分钟跑一次（`evolution_v1` 关闭时不注册）：

1. 让游标跟配置一致：关闭的数据源删除游标；评论打开但没有游标时从当前时刻开始。
2. chat 在读时，给新成员补发告知。
3. 结算打开的运行：分析 issue 上进化 agent 的任务全部结束后，最新一个是 `completed` 则**成功**，把每个数据源的游标推进到这次运行实际读到的位置；`failed`/`cancelled` 则**失败**，游标不动，下次从同一位置重读，不丢数据。还在排队或运行就继续等。
4. 没有打开的运行时，判断是否该开新的：
   - **定时**：最近一个计划时刻（今天的，或今天还没到就用昨天的）距上次定时运行的日期满 `interval_days` 天就开。服务停机错过的时刻，恢复后马上补跑一次，从游标读到现在（受 `max_window_days` 限制）。
   - **补跑**：上一次运行成功但没读完窗口（到了 `max_items_per_window` 或 `max_window_days`），并且当天运行次数没到 `max_runs_per_day`，立即再开一次。失败的运行不立即重试，等下一个计划时刻。
5. 开运行：在工作区级 advisory lock 下确认没有打开的运行，写运行行，再用上游的 `IssueService.Create` 建一个指派给进化 agent 的分析 issue（标题「进化分析 · 日期」，按 agent 名称选语言），由上游的指派流程排队任务。

运行的窗口上界是创建时刻减 5 分钟，避免漏掉晚提交的行。

## 提案（P3）

分析运行用 `multica evolution propose --file <proposal.json>` 调 `POST /api/evolution/proposals`。agent 不直接建 issue，由服务端校验后建，所以下面的规则都由服务端强制，不靠提示词自觉。

**请求**（拒绝未知字段）：`target{type,id}`、`field`、`topic`、`title`、`problem`、`change{before,after}`、`evidence[{type,id,note,quote}]`、`expected_effect`、`risk`（low/medium/high）、`risk_reason`、`rollback`。

**拒绝**（返回 `{"code","error"}`，`error` 是给 agent 看的原因；内容类 422，配额与冷却 409，请求体不合法 400）：

| code | 什么时候 |
|---|---|
| `target_not_allowed` | 目标类型不是 agent / workflow / squad；目标不存在、已归档、是内部系统 agent（`kind=system`）、被排除；**目标是进化自己** |
| `field_not_allowed` | 字段不在白名单：agent 只限 `instructions`、`description`、`skills`；workflow 只限 `steps`、`name`、`description`；squad 只限 `instructions`、`description`、`members`、`leader`（squad 提案只出文本）。`custom_env`、`custom_args`、`mcp_config`、runtime、权限、可见性、密钥类字段单独给出原因 |
| `change_does_not_match_target` | `change.before` 不在目标当前的文本里（比较时忽略空白排版）；`before` 为空时，`after` 已经在目标里 |
| `change_is_ambiguous` | `change.before` 在目标当前的文本里出现不止一处（P4 起：apply 要写的位置必须唯一） |
| `insufficient_evidence` | 没有证据；独立来源数低于 `min_evidence_count`（同一 issue 下的多条评论算一个来源）；证据指向进化读不到的内容（私有项目、私有 agent 的会话、被排除的项目和 agent、进化自己的评论和会话、进化自己的分析和提案 issue） |
| `quote_not_allowed` | 在 chat 或 issue 证据上带 `quote`；`quote` 超过 120 字；`quote` 不是那条评论的原文摘录；任何一个发布出去的字段（标题、问题、改动后的文本、预期效果、风险、回滚、证据说明和摘录）里出现某条 chat 消息中连续 20 个字符（或整条 8 字以上的短消息），或某条评论中连续 121 个字符以上。比对范围是所引的证据**加上本次运行通过数据通道读到的全部评论和 chat**（按游标到已读位置重读，过滤规则与数据通道相同） |
| `invalid_proposal` | 请求不合法；P4 起也包括：改动后工作流的 `steps` 不再是合法 JSON |
| `sensitive_content` | 任何文本经脱敏规则（与数据通道同一套）会被替换；任何文本含工作区成员的姓名（与目标同名的除外） |
| `run_limit_reached` | 本次运行新开的提案已达 `max_proposals_per_run` |
| `recently_rejected` | 同一提案在 `rejected_cooldown_days` 内被取消过 |

比对是逐字段的连续子串比对（忽略大小写和空白排版）。拦不住的情况：
- 改写过的原话；
- 拆到两个字段里各放一段（每段都不超过阈值）；
- 本次运行没读到、也没被引用的内容（例如以前运行读过的）；
- `change.before`：它必须是目标当前文本里的原文，由 `change_does_not_match_target` 约束，不做这项比对。

所以提示词仍要求只写归纳，审查时抽查。

**查重**：`fingerprint = sha256(target_type, target_id, field, 归一化 topic)` 的前 32 位（topic 忽略大小写、标点和空白）。在工作区级 advisory lock 下：

1. 有同 fingerprint 且状态属于未开始 / 进行中分类的提案：**不新建**，把它还没引用的证据以进化的评论追加到原 issue（`outcome: merged`）；没有新证据就什么都不写（`outcome: duplicate`）。原 issue 的 metadata 不动。
2. 否则若同 fingerprint 的提案在冷却期内被取消（状态分类为 closed，以 `updated_at` 计）：拒绝。
3. 否则检查本次运行的配额后新建。

**新建的提案 issue**：标题「进化提案 · …」（按 agent 启用时的语言）；不指派、状态 `backlog`（工作流不含 backlog 时由上游改为 todo），所以不会触发任何运行；标签 `evolution`（首次自动创建）；正文按模板：对象（含基线哈希）、问题与归因、改动（diff）、证据（链接、日期、评论摘录；不写作者）、预期效果、风险档位和回滚、来源分析运行。metadata 写扁平字符串键（只写在这个新 issue 上）：`evolution_fingerprint`、`evolution_target_type`、`evolution_target_id`、`evolution_field`、`evolution_base_hash`（目标白名单字段的内容哈希，P4 apply 时据此判断目标是否被改过）、`evolution_risk`、`evolution_run_id`。

**不自动 apply**：提案接口不写任何 agent、工作流、squad；apply 与回滚是 P4，只有人类管理员能调用。

P4 起，新建提案时服务端同时在 `evolution_proposal` 表里存下这条提案改什么：目标、字段、基线哈希、`change.before/after`、字段原值和改动后的值（`before` 为空时把 `after` 另起一行追加在末尾）。apply 只读这张表，不读 issue 正文和 metadata——这两处成员和 agent 都能编辑。

## 应用与回滚（P4）

**谁能调用**：工作区所有者、管理员的人类账号。路由带 `RequireHumanActor`，处理函数里再判一次：以 agent 身份（`X-Agent-ID` + `X-Task-ID`）、task token、cloud PAT 调用一律 403，普通成员 403。进化 agent 没有任何写入通道。

**写什么（白名单）**：agent 只写 `instructions`、`description`、`skills`；workflow 只写 `steps`、`name`、`description`。其余一律拒绝：`custom_env`、`custom_args`、`mcp_config`、runtime、权限与可见性、模型、密钥类字段返回 `field_not_allowed`；squad 提案、新建或合并 agent 的提案只出文本，返回 `target_not_allowed`（由人手工执行）。进化自己、内部系统 agent、已归档的 agent 不能被写。

**apply 流程**（一个事务）：

1. 工作流目标先取与 `UpdateIssueWorkflow` 相同的状态目录锁；再锁提案 issue 行（同一提案的两次操作串行），再锁目标行（`FOR NO KEY UPDATE`）。
2. 读目标白名单字段算内容哈希，与提案的基线哈希比较。**不一致**：什么都不写，返回 409 `proposal_stale`，并把提案标为过期（`evolution_state=stale`，只在第一次时留一条评论说明）。已应用且未回滚：409 `already_applied`。
3. 用上游接口同样的查询写入字段，并做同样的校验：agent 描述不超过 255 字；技能按名称在工作区里解析，启用列表变为提案里的名单（多的删绑定、少的补上、停用的重新启用；名单外停用的绑定不动；Qoder agent 不能有技能）；工作流走 `Normalize`、`Validate`、交接处理人校验，若会让项目里的 issue 停在被删掉的状态上就拒绝（需要人选迁移目标，到工作流设置里手工改）；工作流名称重复、默认工作流改名都拒绝。校验不过返回 422 `cannot_apply`，什么都不写。
4. 写完重读：只有这个字段变了、且变成提案里的值（技能按名单集合比较、`steps` 按 JSON 值比较），否则整体回滚，返回 422 `cannot_apply`。
5. 追加一条版本行（`action=apply`，前后快照和哈希），把 `evolution_state` 设为 `applied`，以操作人的身份在提案 issue 上留一条评论；提交后照常广播 agent / 工作流变更。

**回滚**：只回滚这条提案当前生效的那次 apply。当前哈希必须等于那次 apply 之后的哈希，否则返回 409 `changed_since_apply`、不写任何东西——**回滚不会覆盖 apply 之后别人做的修改**，这种情况请手工改。通过后写回 apply 前的字段值，重读确认整个快照回到 apply 前的哈希，再追加一条 `action=rollback`、`reverted_of=<那次 apply 的版本行>` 的版本行，`evolution_state` 设为 `rolled_back`，留评论。回滚后可以再次 apply。

**版本表** `evolution_config_version`：每次 apply、每次回滚一行，只追加、从不修改或删除（工作区删除时随工作区删掉）。`version` 是同一目标的递增序号（在目标行锁下取最大值加一）。上游的 `UpdateAgent` 等更新路径不动，所以人在界面上的修改不写版本行，但会被哈希检出。

**拒绝**（`{"code","error"}`）：

| code | 状态码 | 什么时候 |
|---|---|---|
| `proposal_stale` | 409 | 提案之后目标被改过 |
| `already_applied` | 409 | 已应用、未回滚 |
| `not_applied` | 409 | 回滚时没有生效的 apply |
| `changed_since_apply` | 409 | 回滚时目标在 apply 之后被改过 |
| `field_not_allowed` / `target_not_allowed` | 422 | 白名单外的字段、squad、进化自己、系统 agent、已归档 agent |
| `cannot_apply` | 422 | 目标已删除；上游接口的校验不通过；写完与提案不一致 |

## 前端

设置 → 工作区 →「进化」（只在 `evolution_v1` 打开时出现，标记为管理员页）。所有者和管理员选 runtime（和可选的模型）后点「启用进化」；已启用时显示 agent 并链接到它的详情页；普通成员只看到「请联系所有者或管理员启用」。

P2 在已启用的 agent 下面加「定时分析」：范围说明、开关、数据源、时间表、上限、排除的项目和 agent、最近的运行（链接到分析 issue）。所有者和管理员可改；普通成员只读（成员能看到进化读什么）。保存会开始读 chat 的配置时先弹确认框。

P4：提案 issue 的侧栏属性里多一行「提案」：显示未应用 / 已应用第 N 版 / 已回滚 / 已过期 / 需手动执行；所有者和管理员看到「应用」「回滚」按钮，点击后弹确认框，应用时显示服务端存下的改动前后文本。只对带 `evolution_fingerprint` 的 issue 请求，开关关闭时不显示也不请求。

代码：`packages/core/evolution/`（接口、zod 校验、查询与变更）；`packages/views/evolution/evolution-proposal-row.tsx`（提案行）；`packages/views/evolution/`（设置页、`evolution-config.tsx` 配置区和挂载钩子）；文案在 `packages/views/locales/*/evolution.json`。runtime 与模型选择复用 Mika 的 `MikaRuntimeChoice`。

## CLI

`multica evolution signals --source comment|chat [--after <next_after>] [--limit N]`：分析运行读数据用；`multica evolution propose --file <path|->`：分析运行提交提案用。都在 fork 新文件 `server/cmd/multica/cmd_evolution.go`。

## 上游触点

见 [github-issues-upstream-changes.md](github-issues-upstream-changes.md)。P4 没有新增上游文件：在已是触点的 `issue-detail.tsx` 里，紧挨 M-194 的两处 fork 行（导入块后的 fork 导入、侧栏属性里的 Super Agent 行）各加一行，不新增独立冲突块；后端路由仍在 fork 文件 `evolution_agent.go`，`workspace_delete_manifest_test.go` 在 P2 那块里登记两张新表。迁移 `9020_evolution_apply`（两张新表，DO 块建表，含 down）。P3 没有新的上游触点：路由加在 fork 文件 `evolution_agent.go` 的 `MountEvolution` 里，其余都是 fork 新文件。P2 没有新的上游文件，在已有 fork 改动的 3 个上游文件上各追加一块：`main.go` 一行注册定时任务（紧挨 `agentrouter.Register`）；`workspace.go` 的删除步骤（紧挨 Router 那块）；`workspace_delete_manifest_test.go` 登记两张新表。前端没有新的上游触点（只改 fork 自己的文件）。

P1：后端 5 个上游文件各 1 块：`daemon.go` 拼接提示词、`mika_agent.go` 的 `systemInstructionsFor` 展示提示词（两处都调用 fork 的 `service.ComposeForkSystemInstructions` / `ForkSystemInstructions`，以后新增 fork 系统 agent 不用再改这两个文件）、`builtin_skills.go` 的技能映射、`router.go` 挂载路由、`keys.go` 发布开关。前端 11 个上游文件：`package.json` 导出、设置页 3 块、i18n 类型和 8 个语言入口各 2 块，除设置页工作区分组那一处外都紧挨着 Router 已有的 fork 行。
