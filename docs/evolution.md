# 进化（Evolution）：内置系统 agent

本文件属于 `mouse9527/multica` fork（M-135，设计见 M-132）。「进化」是和上游 Mika 同一机制的内置系统 agent：定期分析工作区里的对话和评论，针对 agent、工作流、团队提出改进提案，由所有者或管理员批准后再执行。

**当前阶段是 P1**：只有 agent 的身份与一键启用。不读取任何工作区数据、不注册任何定时任务、没有迁移。数据通道、游标与调度（P2）、提案（P3）、apply 与回滚（P4）、效果度量（P5）在后续任务里做。

## 开关

- `evolution_v1`，默认关闭。用 `FF_EVOLUTION_V1=true` 打开（或 `MULTICA_FEATURE_FLAGS_FILE` 里的规则），每次请求时读取。
- 关闭时：`/api/evolution/*` 返回 404，前端不显示设置入口、也不发请求；不注册任何任务。行为与上游一致。
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
- P1 的提示词明确要求：在定时分析上线前，不主动扫描工作区的 chat、评论和运行记录，只处理成员在当前对话或 issue 里直接给出的内容。

## 接口

都在 `/api/evolution` 下，需要工作区成员身份；开关关闭时 404。

| 方法 | 路径 | 谁可以 | 说明 |
|---|---|---|---|
| `GET` | `/api/evolution` | 任何成员 | `{"agent": <agent>}`，未启用时 `agent` 为 `null` |
| `POST` | `/api/evolution/enable` | 所有者、管理员（人类账号；以 agent 身份或用 task token、cloud PAT 调用一律 403） | 请求 `{"runtime_id", "language": "en"/"zh"/"ko"/"ja", "model"?}`，拒绝未知字段。新建返回 201，已存在返回 200 和同一个 agent |

启用的并发安全与 Mika 相同：先按 `system_key` 快速查重，再在事务里取工作区级 advisory lock（`evolution:<workspace_id>`）后复查，所以并发启用只会建一个。runtime 权限沿用 `canUseRuntimeForAgent`，并在锁内复核。工作区里已有同名 agent 时返回 409，提示先改名。

## 前端

设置 → 工作区 →「进化」（只在 `evolution_v1` 打开时出现，标记为管理员页）。所有者和管理员选 runtime（和可选的模型）后点「启用进化」；已启用时显示 agent 并链接到它的详情页；普通成员只看到「请联系所有者或管理员启用」。

代码：`packages/core/evolution/`（接口、zod 校验、查询与变更）；`packages/views/evolution/`（设置页和挂载钩子）；文案在 `packages/views/locales/*/evolution.json`。runtime 与模型选择复用 Mika 的 `MikaRuntimeChoice`。

## 上游触点

见 [github-issues-upstream-changes.md](github-issues-upstream-changes.md)。后端 5 个上游文件各 1 块：`daemon.go` 拼接提示词、`mika_agent.go` 的 `systemInstructionsFor` 展示提示词（两处都调用 fork 的 `service.ComposeForkSystemInstructions` / `ForkSystemInstructions`，以后新增 fork 系统 agent 不用再改这两个文件）、`builtin_skills.go` 的技能映射、`router.go` 挂载路由、`keys.go` 发布开关。前端 11 个上游文件：`package.json` 导出、设置页 3 块、i18n 类型和 8 个语言入口各 2 块，除设置页工作区分组那一处外都紧挨着 Router 已有的 fork 行。
