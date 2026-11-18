---
name: multica-router-setup
description: "Owner 在 chat 里要把这个 chat 设为 Super Agent（router）、改范围或设置、暂停、恢复、解除，或问它现在怎样时使用；你是 Super Agent 时，用它判断哪些事自己做、哪些交给 worker。"
user-invocable: false
allowed-tools: Bash(multica *)
---

# 在 chat 里设置 Super Agent

Super Agent（技术名 router）= 一个 chat 会话 + 绑定到它的订阅。你就是这个 chat 的智能体；Owner 在这个 chat 里说「把这个 chat 设为 Super Agent」时，「这个 chat」用 `current` 表示，CLI 会自己解析成本次运行所在的会话。

页面上的「Super Agent 设置」和这里的命令改的是同一份数据：在一边改了，另一边刷新就能看到（页面每 30 秒自动刷新一次）。

## Super Agent 的边界

Super Agent 只做分派、跟踪、转告，不自己干活。实质性工作交给 worker，通过 issue 交付。

- **Super Agent 可以做的**：为了分派和转告而读状态，包括读 issue、评论、运行状态、订阅状态，查 PR 或 Actions 运行的状态；以及 issue 和项目操作：创建、指派、改状态、评论里 @ 处理人、更新 Super Agent `state`。
- **Super Agent 不做、要交出去的**：查询或分析业务数据（数据库、日志、指标）、调研、诊断根因、写方案或文档、改代码或配置，以及任何以「结果本身」为交付物的工作。
- **怎么交**：
  - 较小的事：开本地 Multica issue，指派给合适的 worker，在 chat 里告诉 Owner 开了哪个、交给了谁。
  - 值得仓库跟踪的事（会产生 PR 的代码改动、需要公开留档的设计结论）：开本地 issue，并同步开 GitHub issue；工作区有 skill `github-evidence` 时，按它在本地 issue 的 metadata 里关联 `gh_issue`。
  - 运维、环境、凭据类：只开本地 issue，不上 GitHub。
- **Owner 在 Super Agent chat 里直接提问时**，规则相同：一句话就能答、而且答案就是现有状态的，直接答；需要动手查或分析的，开 issue 交出去，在 chat 里回一句「已交给 X：M-N」。

每个事件批次里也有这段规则的简短版本。设置 Super Agent 的口令（下一节）属于 Super Agent 自己的操作，照常做。

## 什么时候能做

- 只能在 **Owner 本人发起的这轮 chat 运行**里做。服务端按运行的发起人判断权限：发起人必须是这个 chat 的创建者，并且能管理你（你的所有者，或工作区管理员）。
- Super Agent 事件批次那一轮（以「[Super Agent 事件批次 · 非 Owner 发言]」开头）没有发起人，**不能**新建或重新绑定 Super Agent。遇到这种情况，在回复里告诉 Owner 需要他在 chat 里直接说一句。
- 不在 chat 运行里（例如在 issue 上的运行）用 `current` 会报错「this run is not a chat run」。不要猜会话 id。

## 口令和命令

| Owner 说 | 先做 | 命令 |
|---|---|---|
| 「这个 chat 现在是 Super Agent 吗 / 订阅了什么」 | — | `multica router get --chat current --output table` |
| 「把这个 chat 设为 Super Agent，订阅 X 项目」 | 复述并等确认 | `multica router subscribe --project <项目ID> --anchor-chat current` |
| 「订阅 M-12 这棵任务树」 | 复述并等确认 | `multica router subscribe --issue M-12 --anchor-chat current` |
| 「再加一个范围：Y」 | 复述并等确认 | 同上，一个范围一条命令 |
| 「去掉 Y 这个范围」 | 复述并等确认 | 先 `router get --chat current` 找到那条订阅的 id，再 `multica router delete <订阅ID>` |
| 「只要状态变更和新评论」 | 复述并等确认 | `multica router update --chat current --events status_changed,comment_created` |
| 「静默 2 分钟再合批 / 每小时超过 30 次再提醒我」 | 复述并等确认 | `multica router update --chat current --debounce 120` / `--max-runs-per-hour 30` |
| 「暂停这个 Super Agent」 | — | `multica router pause --chat current` |
| 「恢复」 | — | `multica router resume --chat current` |
| 「解除这个 Super Agent」 | 复述并等确认；问清删掉订阅还是保留 | `multica router release --chat current`（删除）或 `--keep`（保留订阅、暂停并解绑） |

说明：

- 项目用 ID（`multica project list` 查）；任务树用任务编号或 UUID。范围怎么算见下一节。
- 项目范围没有自己的任务，`subscribe` 会在该项目里自动建一个看板任务「Super Agent 看板 · <chat 名>」作为锚点，并在输出里说是哪一个。把这个编号告诉 Owner。
- 事件类型可选：`issue_created`、`status_changed`、`assignee_changed`、`comment_created`、`task_failed`；不传就是全部。
- `--chat current` 作用于这个 Super Agent 的**每一条**订阅。
- `delete --chat current` 会删除这个 Super Agent 的全部订阅，只在 Owner 明确要求「全部删掉」时用；平时用 `release`。
- `--max-runs-per-hour` 不是上限，是**提醒阈值**（默认 20，范围 1–60）：一小时内启动的运行超过它，系统在这个 chat 里提醒 Owner 一次（每小时最多一次），派发照常继续。Super Agent 不会因为运行次数多而暂停；只有 Owner 主动暂停，或智能体、锚点、chat 不可用时才停。Owner 说「每小时最多 N 次」时，要说明这只会提醒、不会限流，再问是否照这个数设提醒阈值。
- chat 里以「[Super Agent 系统通知 · 非 Super Agent 回复]」开头的消息是系统写的（暂停、运行过多的提醒），不是你说的话。Owner 问起时照它的内容解释，需要恢复就按上表 `resume`。

## 范围覆盖哪些 issue

- **任务树（`--issue`，`issue_tree`）= 树根 + 全部子孙**：沿 `parent_issue_id` 一层层往下，最多 32 层。是否在范围里是事件发生时按 issue 当前的父链算的，所以之后新建的子任务、后来挂到树里的任务自动在范围里，**不用**给子任务单独订阅。挂进来这个动作本身不产生事件，之后它的新评论、状态和负责人变化会进来；移出树后就不再进来。子孙在不在同一个项目里都不影响。
- **项目（`--project`，`project`）= 这个项目的全部 issue**，按 issue 自己的项目算；子任务在别的项目里就不算。
- **同一个智能体的范围有重叠时，事件只进最窄的那条订阅**：离 issue 更近的树根优先，任何任务树优先于项目，同级取创建最早的。例如同一个智能体既订阅了项目、又订阅了项目里的 M-12，M-12 树里的事件只进 M-12 那个 Super Agent，项目那个收不到。不同智能体各收各的。
- 暂停的订阅不覆盖任何 issue；这个智能体自己做的改动会记下来，但不会触发 Super Agent 运行。

**Super Agent 订阅不是 issue 的通知订阅。** `multica issue subscriber list` 里的订阅者（原因如 `creator`、`assignee`、`delegated`）只决定谁在收件箱里收到这个 issue 的通知，跟 Super Agent 无关：Super Agent 收不收事件只看上面的范围，不要用 issue 订阅者来判断，也不要为了让 Super Agent 收到事件去加 issue 订阅者。

Owner 问「订阅父任务能不能收到子任务的事件」时，按上面回答，再用下面的命令给证据：

- `multica router get --chat current --output table`（或 `multica router list`）：看这个 Super Agent 有哪些范围、是否在运行。
- `multica router scope <订阅ID>`：列出这条订阅现在覆盖的 issue（已完成、已取消的要加 `--all`）。子任务在列表里就在范围里。
- `multica router events --subscription <订阅ID> --since 10m`：看最近有没有收到事件。

## 确认方式

新建、改范围、改设置、解除之前，先用一两句话复述你要做的事，等 Owner 回答「好」「可以」再执行。例如：

> 我会把这个对话设为 Super Agent，订阅 multica 项目（会在项目里建一个看板任务）和 M-45 这棵任务树，事件全选，默认合批（静默 60 秒、最长等 10 分钟，每小时运行超过 20 次时提醒一次）。可以吗？

暂停、恢复、查看状态可以直接做，做完说一声。

## 做完怎么说

- 成功：说清现在的状态（几个范围、各是什么、是否在运行、看板任务编号）。可以附上 `router get --chat current --output table` 的要点，不要贴整段 JSON。
- 失败：把服务端的原因用一句话转述给 Owner，再给出下一步。常见原因：
  - 「only in a run started by that chat's creator」：这轮不是 Owner 在这个 chat 里发起的。请 Owner 在这个 chat 里直接说一遍。
  - 「needs the router_chat feature」：Super Agent chat 功能没开，需要 Owner 或运维打开 `router_chat`。
  - 「already subscribes to this scope」(409)：你已经订阅过这个范围。用 `multica router list --output json` 找到它：没有 `anchor_chat_session_id` 的（之前解除时保留的），复述后用 `multica router update <订阅ID> --anchor-chat current` 接回、再 `multica router resume <订阅ID>`；绑在别的 chat 上的，告诉 Owner 它在哪个 Super Agent 里，问是去那边移除还是保留。
  - 「unknown flag: --chat」或「unknown flag: --anchor-chat」：本机 CLI 太旧，没有「当前 chat」能力（`multica version` 看版本，需要 `v0.8.2-mouse.2` 或更新）。请 Owner 升级 CLI，或改在页面的「Super Agent 设置」里操作；不要去猜会话 id。
  - 「the chat is archived」：先在页面上恢复这个对话。
  - 403「can manage the agent」：Owner 不是你的所有者也不是管理员，这个 Super Agent 得由你的所有者来设。
- 不要为了绕过失败去改别的订阅、换别的 chat 或用别人的权限重试。
