# 运行时 brief 的 lean 版（M-259）

本文件属于 `mouse9527/multica` fork（上游没有）。

## 是什么

daemon 每次运行都会把一份运行时 brief 写进工作目录（Claude 是 `CLAUDE.md`，Codex 等是 `AGENTS.md`）。lean 版是同一份 brief 的精简写法：段落标题、段落顺序、各任务类型（issue / chat / quick-create / autopilot）带哪些段都和上游一致，只把最长的几段改短，规则本身不删。

- 代码：`server/internal/daemon/execenv/runtime_config_lean.go`。上游文件只有 `runtime_config.go` 的 `buildMetaSkillContent` 里一处钩子。
- 改短的段：Background Task Safety、Available Commands（含 quick-create 版）、Issue Body Formatting、Comment Formatting、Workflow 的 step 1、2、4 和内置状态规则、Sub-issue Creation、Mentions、Attachments、Always Use CLI，以及 issue 类的 Output。
- 原样沿用上游：开头、Agent Identity、Requesting User、Workspace Context、Project Context、Instruction Precedence、项目 workflow 的状态规则、Skills、chat / quick-create / autopilot 的 Workflow 和 Output、状态命令那一行。工作区说明、项目说明、智能体说明逐字透传，不做任何压缩。
- 仓库列表（只在 lean 版里生效）：项目绑了 `github_repo` 资源时照旧全部列出；没有项目，或者项目没绑 repo（这时服务端给的是工作区全部 repo）时最多列 5 个，其余写成「…and N more in this workspace; `multica repo list` shows them all.」。服务端的 `task.Repos` 没动，所以没列出的 repo 照样能 `multica repo checkout`。
- 「KEPT checkout」那段说明从 lean 版 brief 里拿掉，改由 `multica repo checkout` 在真的保留了旧 checkout 时输出（`server/cmd/multica/cmd_repo_kept_note.go`）。这一处对所有智能体生效：上游版 brief 里仍有这段，所以用上游版的智能体在保留 checkout 时会读到两遍，内容一致。

## 开关

daemon 进程的环境变量 `MULTICA_BRIEF_LEAN_AGENTS`，值是逗号分隔的智能体 ID（UUID，大小写不敏感，允许空格）。

- 不设或为空：所有智能体都用上游 brief，逐字节一致。
- 列入的智能体：用 lean 版；没列入的照旧。
- 变量在 daemon 启动时读入进程环境，改了要重启 daemon 才生效；正在跑的任务不受影响，下一次运行才换。
- 回滚：从变量里删掉 ID（或清空变量），重启 daemon。

## 同一个 daemon 跑多个工作区

名单只按智能体 ID 匹配，不看工作区。智能体 ID 是全局唯一的 UUID，所以只列工作区 A 某个智能体的 ID，不会让同一 daemon 上工作区 B 的智能体切到 lean 版。以工作区 B 为例：

- 开关关着，或名单里没有工作区 B 的智能体：工作区 B 的 brief 和现在完全一样，仓库列表照旧全部列出。
- 名单里有工作区 B 的智能体：那个智能体用 lean 版。它的任务如果没有项目、或者项目没绑 repo，仓库列表只列前 5 个，加一行「…and N more」，需要别的 repo 时用 `multica repo list` 查，再 `multica repo checkout`。
- 不论开关如何，`multica repo checkout` 在保留旧 checkout 时都会多输出那段说明，所有工作区都一样（要等带这次改动的 CLI 发版并装上后才有）。

## 同步上游时

- `TestLeanBriefHeadingsMatchUpstream`：对一组覆盖所有任务类型的上下文，比较上游和 lean 两个装配器输出的标题（`#` 到 `###`）序列，不一致就失败。上游加了、改了、删了或挪了一个段落，这个测试会挂；按上游的改动在 `runtime_config_lean.go` 里补上对应段落（新段落一般先直接调用上游的 writer）。
- `TestLeanBriefKeepsHardRules`：钉住 lean 版不能丢的规则原句（turn 结束即终止、不 background-and-yield、不 watch/poll CI 及唯一例外、不按名字杀 multica 进程、评论是唯一交付、`--content-file` 写在工作目录、step 2 必须扫评论、何时拆子任务等）。
- 上游改了 lean 版沿用的 writer，lean 版自动跟着变；改了 lean 版已经重写的段落，上游的新规则要手工搬进 lean 版。同步 PR 里用 `git diff <旧 main>..<新 main> -- server/internal/daemon/execenv/runtime_config_sections.go` 看上游改了哪些段。
- 钩子冲突只可能出现在 `runtime_config.go` 的 `buildMetaSkillContent` 和 `cmd_repo.go` 的 `repoCheckoutOutcome` 各一行。
