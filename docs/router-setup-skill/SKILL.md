---
name: router-setup
description: Owner 在 chat 里让你把这个 chat 设为 Router、改它的范围或设置、暂停/恢复、解除，或者问这个 Router 现在怎么样时使用。说明口令对应的 multica router 命令、改动前怎么跟 Owner 确认、失败时怎么说。
---

<!-- 草案（mouse9527/multica fork，M-105）。由 Mika 在 Owner 确认后作为 workspace skill 挂到相关智能体上；本文件只是内容来源。 -->

# 在 chat 里设置 Router

Router = 一个 chat 会话 + 绑定到它的订阅。你就是这个 chat 的智能体；Owner 在这个 chat 里说「把这个 chat 设为 Router」时，「这个 chat」用 `current` 表示，CLI 会自己解析成本次运行所在的会话。

页面上的「Router 设置」和这里的命令改的是同一份数据：在一边改了，另一边刷新就能看到（页面每 30 秒自动刷新一次）。

## 什么时候能做

- 只能在 **Owner 本人发起的这轮 chat 运行**里做。服务端按运行的发起人判断权限：发起人必须是这个 chat 的创建者，并且能管理你（你的所有者，或工作区管理员）。
- Router 事件批次那一轮（以「[Router 事件批次 · 非 Owner 发言]」开头）没有发起人，**不能**新建或重新绑定 Router。遇到这种情况，在回复里告诉 Owner 需要他在 chat 里直接说一句。
- 不在 chat 运行里（例如在 issue 上的运行）用 `current` 会报错「this run is not a chat run」。不要猜会话 id。

## 口令和命令

| Owner 说 | 先做 | 命令 |
|---|---|---|
| 「这个 chat 现在是 Router 吗 / 订阅了什么」 | — | `multica router get --chat current --output table` |
| 「把这个 chat 设为 Router，订阅 X 项目」 | 复述并等确认 | `multica router subscribe --project <项目ID> --anchor-chat current` |
| 「订阅 M-12 这棵任务树」 | 复述并等确认 | `multica router subscribe --issue M-12 --anchor-chat current` |
| 「再加一个范围：Y」 | 复述并等确认 | 同上，一个范围一条命令 |
| 「去掉 Y 这个范围」 | 复述并等确认 | 先 `router get --chat current` 找到那条订阅的 id，再 `multica router delete <订阅ID>` |
| 「只要状态变更和新评论」 | 复述并等确认 | `multica router update --chat current --events status_changed,comment_created` |
| 「静默 2 分钟再合批 / 每小时最多 3 次」 | 复述并等确认 | `multica router update --chat current --debounce 120` / `--max-runs-per-hour 3` |
| 「暂停这个 Router」 | — | `multica router pause --chat current` |
| 「恢复」 | — | `multica router resume --chat current` |
| 「解除这个 Router」 | 复述并等确认；问清删掉订阅还是保留 | `multica router release --chat current`（删除）或 `--keep`（保留订阅、暂停并解绑） |

说明：

- 项目用 ID（`multica project list` 查）；任务树用任务编号或 UUID，树根和它的全部子任务都在范围里。
- 项目范围没有自己的任务，`subscribe` 会在该项目里自动建一个看板任务「Router 看板 · <chat 名>」作为锚点，并在输出里说是哪一个。把这个编号告诉 Owner。
- 事件类型可选：`issue_created`、`status_changed`、`assignee_changed`、`comment_created`、`task_failed`；不传就是全部。
- `--chat current` 作用于这个 Router 的**每一条**订阅。
- `delete --chat current` 会删除这个 Router 的全部订阅，只在 Owner 明确要求「全部删掉」时用；平时用 `release`。

## 确认方式

新建、改范围、改设置、解除之前，先用一两句话复述你要做的事，等 Owner 回答「好」「可以」再执行。例如：

> 我会把这个对话设为 Router，订阅 multica 项目（会在项目里建一个看板任务）和 M-45 这棵任务树，事件全选，默认合批（静默 60 秒、最长等 10 分钟、每小时最多 6 次）。可以吗？

暂停、恢复、查看状态可以直接做，做完说一声。

## 做完怎么说

- 成功：说清现在的状态（几个范围、各是什么、是否在运行、看板任务编号）。可以附上 `router get --chat current --output table` 的要点，不要贴整段 JSON。
- 失败：把服务端的原因用一句话转述给 Owner，再给出下一步。常见原因：
  - 「only in a run started by that chat's creator」：这轮不是 Owner 在这个 chat 里发起的。请 Owner 在这个 chat 里直接说一遍。
  - 「needs the router_chat feature」：Router chat 功能没开，需要 Owner 或运维打开 `router_chat`。
  - 「already subscribes to this scope」(409)：你已经订阅过这个范围。用 `multica router list --output json` 找到它：没有 `anchor_chat_session_id` 的（之前解除时保留的），复述后用 `multica router update <订阅ID> --anchor-chat current` 接回、再 `multica router resume <订阅ID>`；绑在别的 chat 上的，告诉 Owner 它在哪个 Router 里，问是去那边移除还是保留。
  - 「the chat is archived」：先在页面上恢复这个对话。
  - 403「can manage the agent」：Owner 不是你的所有者也不是管理员，这个 Router 得由你的所有者来设。
- 不要为了绕过失败去改别的订阅、换别的 chat 或用别人的权限重试。
