# GitHub 原生 issue：Owner 环境实测清单

本文件属于 `mouse9527/multica` fork。功能说明见 [github-issues.md](github-issues.md)。

下面这些行为只能用真实 GitHub 与真实令牌确认，自动化测试用的是假 GitHub，智能体也不碰真实令牌，所以由 Owner 照做并记录结果。每一项写了步骤、去哪里看、期望结果；和期望不一致时，把实际结果贴到对应的 issue 里，代码按实际行为调整。

## 准备

- **测试仓库**：两个专用仓库，不用生产仓库。下文记作 `OWNER/REPO-A`、`OWNER/REPO-B`（B 用来测转移）。在 A 里先建一个 issue（下文 `#1`）并写两条评论。
- **测试实例**：一个不放真实数据的 Multica 实例，设置环境变量后重启服务端：
  - `MULTICA_GITHUB_ISSUES_ENABLED=true`
  - `MULTICA_GITHUB_TOKEN=<classic PAT，repo 范围>`
  - `MULTICA_GITHUB_TOKEN_EXPIRES_AT=<YYYY-MM-DD 或 never>`
  - 可选：`MULTICA_GITHUB_POLL_ENABLED=true`、`MULTICA_GITHUB_REFRESH_ON_VIEW=true`（默认都关闭，GitHub 只在手动同步时读取；B 节里依赖轮询或打开即刷新的步骤，关闭时改用 `multica issue sync`）
- **命令行变量**（只在自己的终端里，不要贴进任何评论或日志）：

  ```bash
  export T='<同一个 PAT>'
  gh_api() { curl -sS -i -H "Authorization: Bearer $T" -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" "$@"; }
  RID_A=$(gh_api https://api.github.com/repos/OWNER/REPO-A | sed -n '/^{/,$p' | jq .id)
  RID_B=$(gh_api https://api.github.com/repos/OWNER/REPO-B | sed -n '/^{/,$p' | jq .id)
  ```

  下文 `gh_api` 输出带响应头；只看正文时在后面接 `| sed -n '/^{/,$p'` 或 `| sed -n '/^\[/,$p'`。

## A. GitHub API 的行为（curl）

### A1 `GET /user` 是否带 `X-OAuth-Scopes`

```bash
gh_api https://api.github.com/user | grep -i -E '^(x-oauth-scopes|x-ratelimit-(limit|remaining))'
```

- 期望：有 `x-oauth-scopes`，值里含 `repo`；`x-ratelimit-limit` 为 5000。
- 用途：令牌校验在这个头缺失时跳过范围检查（「令牌状态 · 校验」）。记下它是否出现、格式如何。

### A2 带 `If-None-Match` 的 304 是否计入限额

```bash
gh_api https://api.github.com/repositories/$RID_A/issues/1 | grep -i -E '^(etag|x-ratelimit-remaining)'
# 把上面的 ETag 原样填进去（含引号和 W/ 前缀）
gh_api -H 'If-None-Match: <ETag>' https://api.github.com/repositories/$RID_A/issues/1 | grep -i -E '^(HTTP|x-ratelimit-remaining)'
gh_api https://api.github.com/rate_limit | sed -n '/^{/,$p' | jq .resources.core.remaining
```

- 期望：第二个请求是 `304`。记下 304 前后 `remaining` 是否减少（GitHub 文档说 304 不计入，未验证）。
- 用途：如果不计入，以后可以给轮询加条件请求省限额；目前代码没有用 ETag。

### A3 按仓库 id 寻址的路径

```bash
for p in "issues?state=all&per_page=1" "issues/1" "issues/1/comments?per_page=1&page=1" \
         "issues/comments?sort=updated&direction=asc&per_page=1" "issues/1/timeline?per_page=1" "pulls?state=all&per_page=1"; do
  printf '%s -> ' "$p"; gh_api "https://api.github.com/repositories/$RID_A/$p" | head -1
done
```

- 期望：每一行都是 `HTTP/2 200`。代码所有请求都发到 `/repositories/{id}/...`（「访问仓库 · 固定 repo_id」）。
- 另外把 A 改个名，再跑一遍：仍然 200（id 不变）。改回原名。

### A4 评论新增、编辑、删除后，issue 的 `updated_at` 与 `issues?since`

```bash
issue_updated() { gh_api https://api.github.com/repositories/$RID_A/issues/1 | sed -n '/^{/,$p' | jq -r .updated_at; }
T0=$(issue_updated); echo "$T0"; sleep 2
# 新增评论
gh_api -X POST -d '{"body":"checklist A4"}' https://api.github.com/repositories/$RID_A/issues/1/comments | sed -n '/^{/,$p' | jq '{id, updated_at}'
issue_updated
gh_api "https://api.github.com/repositories/$RID_A/issues?state=all&sort=updated&direction=asc&since=$T0" | sed -n '/^\[/,$p' | jq '[.[].number]'
gh_api "https://api.github.com/repositories/$RID_A/issues/comments?sort=updated&direction=asc&since=$T0" | sed -n '/^\[/,$p' | jq '[.[].id]'
```

然后对这条评论分别做「编辑」（`-X PATCH -d '{"body":"checklist A4 edited"}' .../issues/comments/<id>`）和「删除」（`-X DELETE .../issues/comments/<id>`），每次之后重复后三条命令。

- 记录：每一步之后 issue 的 `updated_at` 是否变化；`issues?since` 是否返回 `#1`；`issues/comments?since` 是否返回这条评论（删除后应不再返回）。
- 用途：代码不依赖「评论会推进 issue 的 `updated_at`」，评论单独轮询；删除的评论两条列表都发现不了（「轮询 · 已知限制」）。

### A5 `since` 是否包含边界

```bash
U=$(issue_updated)
gh_api "https://api.github.com/repositories/$RID_A/issues?state=all&since=$U" | sed -n '/^\[/,$p' | jq '[.[].number]'
```

- 记录：结果里有没有 `#1`（`updated_at` 正好等于 `since`）。代码按「可能不包含」处理（翻页时减 1 秒），两种结果都能工作，记下实际行为即可。

### A6 删除 issue 后的表现

在 GitHub 网页上新建 issue `#D`，然后删除它（issue 页右侧「Delete issue」，需要仓库管理员权限）。

```bash
gh_api https://api.github.com/repositories/$RID_A/issues/<D> | head -1
gh_api https://api.github.com/repos/OWNER/REPO-A/issues/<D> | head -1
```

- 期望：`404` 或 `410`。记下是哪个，以及删除后 `issues?since` 是否还会返回它。
- 用途：每日对账把 404、410 都当作已删除（「每日对账 · 结果」）。

### A7 转移 issue 后的表现

在 GitHub 网页上新建 issue `#X`，用「Transfer issue」转到 `OWNER/REPO-B`，记下新编号 `M`。

```bash
gh_api https://api.github.com/repositories/$RID_A/issues/<X> | grep -i -E '^(HTTP|location)'
gh_api https://api.github.com/repos/OWNER/REPO-A/issues/<X> | grep -i -E '^(HTTP|location)'
```

- 期望：`301`，`Location` 形如 `https://api.github.com/repositories/<RID_B>/issues/<M>`。
- 用途：对账按 `Location` 里的仓库 id 和编号改挂（代码只认这个形状，`/repos/owner/name/issues/N` 形状会被当作「已转出」）。形状不同时贴出实际值。

### A8 正文里的隐藏标记是否原样保留、能否被搜到

```bash
MARK='<!-- multica:v1 actor=member:00000000-0000-0000-0000-000000000000 op=11111111-1111-1111-1111-111111111111 -->'
BODY=$(jq -n --arg b "$(printf 'line 1\n\n  indented line\ntrailing spaces   \n\n%s' "$MARK")" '{body:$b}')
gh_api -X POST -d "$BODY" https://api.github.com/repositories/$RID_A/issues/1/comments | sed -n '/^{/,$p' | jq -r .body | od -c | tail -5
sleep 30
gh_api "https://api.github.com/search/issues?q=repo:OWNER/REPO-A+11111111-1111-1111-1111-111111111111+in:comments" | sed -n '/^{/,$p' | jq .total_count
```

- 期望：返回的 `body` 与发出的一字不差（换行、缩进、行尾空格、末尾的 HTML 注释都在）。
- 记录：搜索能否找到（`total_count`）。代码找「结果未知」的写入时**不用搜索**，而是读写入时间之后更新的 issue 与该 issue 的评论、按标记比对，所以搜不到不影响功能；正文若被改写（例如去掉行尾空格或注释），就会影响标记识别与「GitHub 是否已执行」的判断，需要报告。
- 做完删掉这条评论。

### A9 PR 与 CI 的读取

先在 A 里开一个 PR（正文写 `Closes #1`），等 CI 跑起来，记下 head SHA `$SHA`。

```bash
gh_api "https://api.github.com/repositories/$RID_A/pulls?state=all&since=2020-01-01T00:00:00Z&per_page=100" | sed -n '/^\[/,$p' | jq length
gh_api https://api.github.com/repos/OWNER/REPO-A/commits/$SHA/check-runs | head -1
gh_api https://api.github.com/repos/OWNER/REPO-A/commits/$SHA/status | head -1
gh_api "https://api.github.com/repos/OWNER/REPO-A/actions/runs?head_sha=$SHA" | head -1
gh_api "https://api.github.com/repositories/$RID_A/issues/1/timeline?per_page=100" | sed -n '/^\[/,$p' | jq '[.[] | select(.event=="cross-referenced") | .source.issue.number]'
```

- 记录：`pulls` 是否认 `since`（与不带 `since` 的结果比较；文档没有这个参数，代码也不用它）；`check-runs`、`status`、`actions/runs` 对这个令牌是否 200（PR 卡片的 CI 状态靠它们）；时间线里是否有这个 PR。
- 再从 `OWNER/REPO-B` 开一个正文写 `Closes OWNER/REPO-A#1` 的 PR，重复最后一条：跨仓库 PR 是否也出现在时间线里。

## B. 在 Multica 里端到端走一遍

下文「issue 页」指 Multica 里这个 issue 的详情页。需要 issue 的 UUID 时用 `multica issue get <标识> --output json` 里的 `id`。

### B1 开启与令牌状态

1. 重启后看服务端日志：有 `github issues enabled`，没有 `github issues disabled`；随后有 `github token accepted`（带 `account_login`），两行都不含令牌本身。
2. 以 owner 身份请求 `GET /api/workspaces/<工作区 id>/github-issues/token-status`。
   - 期望：`status` 为 `ok`，`account_login` 是令牌所属账号，`expires_at` 与环境变量一致。
3. 在某个项目的资源里登记 `https://github.com/OWNER/REPO-A` 和 `https://github.com/OWNER/REPO-B`。

### B2 导入与外部评论

0. **只轮询被跟踪的仓库（#57）**：B1 登记了 A、B 之后、导入任何 issue 之前，等两个轮询周期，看令牌的限额消耗（`gh_api https://api.github.com/rate_limit | sed -n '/^{/,$p' | jq .resources.core`）或服务端日志。期望：轮询不对 A、B 发请求，`github_repo_sync` 里没有它们的行。导入 A 的 issue 后，下一个周期起只轮询 A；B 仍然不发请求，直到 B 也有了已跟踪的 issue。

1. `multica issue import OWNER/REPO-A#1`。
   - 期望：返回 201；issue 页标识是 `OWNER/REPO-A#1`，描述开头是「由 @… 在 GitHub 创建（外部、不可信…）」，已有的两条评论都在，作者显示为系统、正文带外部前缀。
   - 再执行一次：返回 200，不出现第二个 issue、不出现重复评论。
2. 用另一个 GitHub 账号在 `#1` 下评论，再编辑这条评论。
   - 期望：两个轮询周期（约 2 分钟）内评论出现在 issue 页，编辑后正文跟着变；这条评论在 Multica 里不能编辑、删除；挂在这个 issue 上的智能体不会因此开始运行。
3. **已跟踪的 issue 在 GitHub 上被更新后，标题和描述如何跟随**（离线演练没有触发过这一条，请逐项记录）：
   1. 在 GitHub 上改导入的 `#1` 的标题。期望：2 分钟内 issue 页标题更新；在这之前打开 issue 页，几秒内就更新（按需刷新）。
   2. 在 GitHub 上改 `#1` 的描述。期望：Multica 里的描述变成「由 @<原作者> 在 GitHub 创建（外部、不可信…）」前缀加**新的**原文，前缀只有一个，不会叠加；在 Multica 里改这个描述被拒绝（403，他人 issue 的描述只读）。
   3. 在 B4 第 2 步从 Multica 新建的那个 issue 上，在 GitHub 网页里改描述（保留或删掉末尾的隐藏标记各试一次）。期望：Multica 里的描述跟随 GitHub 的新文本，没有外部前缀；保留了标记时标记不显示在 Multica 里，删掉时原样显示 GitHub 的文本。
   4. 在 GitHub 上把 `#1` 关闭再重新打开；如果项目配了状态标签（`project_github_settings.status_label_map`，目前没有界面和接口，只能直接写表，例如 `{"in_progress":"status:in-progress"}`），再加上这个标签。期望：2 分钟内 Multica 的状态跟着变（关闭 → done 类；重开 → 入口状态；状态标签 → 映射的状态），不改标题、描述的更新不会动状态。
   5. 把同一个 issue 的标题在 GitHub 上连续改两次（间隔几秒）。期望：最终是第二次的标题，不会被较早的快照改回去。
4. 导入入口目前只有 API 和 CLI：界面里没有「导入 GitHub issue」的输入框，这是已知的，不算失败。没导入的 issue 在 GitHub 上更新、评论，Multica 里都不应出现。

### B3 从 Multica 写到 GitHub

1. 在 issue 页发一条评论。期望：GitHub 上出现同样的评论，末尾有隐藏标记（网页上看不到，`gh_api .../issues/comments/<id>` 的 `body` 里有 `<!-- multica:v1 ... -->`）；Multica 里只有一条，不会在下一轮轮询后变成两条。
2. 让一个智能体在这个 issue 上发评论。期望：GitHub 上的评论末尾有一行「— <智能体名>（智能体），经 Multica」。
3. 编辑、删除第 1 步的评论。期望：GitHub 上同步编辑、删除。
4. 在 Multica 里把 issue 改成 done。期望：GitHub 上 `#1` 关闭（completed）。在 GitHub 上重新打开它，2 分钟内 Multica 里回到项目入口状态（通常是 backlog）。
5. 改成一个 GitHub 表达不了的状态（项目没配状态标签时，例如 in_progress）。期望：被拒绝，提示去项目的 GitHub 设置加 `status:` 标签。

### B4 新建 issue 选仓库

1. 在挂了两个仓库的项目里打开新建 issue 对话框。期望：有仓库下拉，最后一项是「本地（不同步 GitHub）」；多个仓库且没有默认时不预选。
2. 选 `OWNER/REPO-A` 创建。期望：GitHub 上出现新 issue；Multica 里标识是 `OWNER/REPO-A#<新编号>`。
3. `multica issue create --title test --project <项目 id>`（不带 `--repo`）以智能体身份执行时被拒绝并列出候选；带 `--provider local` 时建成 local issue。

### B5 PR 卡片

用 A9 里 `Closes #1` 的 PR。期望：两个轮询周期内 issue 页的 PR 卡片显示这个 PR 与 CI 状态；合并到默认分支后，GitHub 关闭 `#1`，下一轮轮询 Multica 里变成 done。

### B6 每日对账：删除与转移

每日对账每个 issue 一天读一次。为了不等一天，在测试实例的数据库里把要测的 issue 的同步时间往前拨：

```sql
UPDATE issue_external SET external_synced_at = now() - interval '25 hours'
WHERE issue_id = '<issue 的 UUID>';
```

1. **删除**：导入一个新 issue `#D2`，在 GitHub 上删除它，拨回同步时间，等一个轮询周期（约 1 分钟）。
   - 期望：`SELECT state_reason FROM issue_external WHERE issue_id = '<UUID>'` 是 `multica:deleted`；服务端日志有一条 `github issue no longer on GitHub; no longer synced`；Multica 里这个 issue 和评论都还在；改标题、发评论都成功且不再发 GitHub 请求（GitHub 上什么也不发生）。
2. **转到登记过的仓库**：导入 `#X2`，转移到 `OWNER/REPO-B`，拨回同步时间，等一个周期。
   - 期望：issue 页标识变成 `OWNER/REPO-B#<新编号>`；之后在 B 上评论，2 分钟内出现在这个 issue 上。
3. **转到没登记的仓库**：导入 `#X3`，转到一个没有登记在项目上的仓库，拨回同步时间，等一个周期。
   - 期望：`state_reason` 是 `multica:transferred`，issue 保留。
4. 期望 `github_repo_sync.last_full_reconcile_at` 有值。

### B7 令牌失效、日志里没有令牌

1. 在 GitHub 上撤销（或换掉）这个令牌，等一个轮询周期。期望：token-status 变成 `invalid`；轮询停止（日志不再有轮询请求）；Multica 里对 github issue 的写入返回 503 `github_unavailable`，本地不变。
2. 在服务端日志里搜令牌的前几位（例如 `ghp_` 加前 4 个字符）。期望：搜不到。
3. 换回有效令牌并重启：恢复正常。

### B8 关闭开关与上游一致

1. 去掉 `MULTICA_GITHUB_ISSUES_ENABLED`，重启。
   - 期望：日志有一行 `github issues disabled (...)`；`token-status`、`/api/issues/import` 返回 404；导入过的 issue 标识显示为 `PREFIX-N`；新建对话框没有仓库下拉；没有发往 GitHub 的请求。
2. 重新打开开关并重启：标识恢复成 `OWNER/REPO-A#1`，轮询接着之前的游标继续。

### B9 默认手动同步（#63）

开关取默认值（不设 `MULTICA_GITHUB_POLL_ENABLED`、`MULTICA_GITHUB_REFRESH_ON_VIEW`）重启。

1. 日志有一行 `github issues poll off`。记下 `gh_api https://api.github.com/rate_limit | sed -n '/^{/,$p' | jq .resources.core.remaining`，什么都不做等 5 分钟（期间也打开几次已导入的 issue 页），再看一次。期望：两次只差你自己的 `rate_limit` 查询之外的消耗（服务端不发请求；`rate_limit` 接口本身不计入额度）。
2. 用另一个 GitHub 账号在已导入的 `#1` 下评论，再把 `#1` 关闭。Multica 里不会自己出现。执行 `multica issue sync OWNER/REPO-A#1`。期望：返回 `new_comments: 1`、`issue_updated: true`、`requests_used` 约 2–3，`rate_limit.remaining` 与 `gh_api .../rate_limit` 报的接近；issue 页出现这条评论、状态跟着变。再执行一次：`new_comments: 0`，没有重复评论。
3. `multica issue sync --project <项目 id> --output table`。期望：逐个列出项目里的 GitHub issue，最后一行汇总同步了几个、有变化几个、用了多少请求。
4. 额度守卫：用 `MULTICA_GITHUB_SYNC_MIN_REMAINING=5001` 重启（高于任何剩余额度），再同步一次。期望：命令失败，错误是 429 `github_quota_low`，带 `retry_after_seconds`；`--project` 时输出 `stopped` 与 `not_attempted`，不发同步请求。测完去掉这个变量。

## 记录表

| 项 | 结果 | 与期望一致 | 备注 |
|---|---|---|---|
| A1 `X-OAuth-Scopes` | | | |
| A2 304 是否计入限额 | | | |
| A3 按 id 寻址 | | | |
| A4 评论与 `updated_at`、`since` | | | |
| A5 `since` 边界 | | | |
| A6 删除后的状态码 | | | |
| A7 转移后的 `Location` | | | |
| A8 标记原样保留 / 能否搜到 | | | |
| A9 `pulls` 的 `since`、CI 读取、跨仓库时间线 | | | |
| B1–B8 | | | |
| B9 手动同步与额度守卫 | | | |
