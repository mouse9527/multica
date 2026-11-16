# fork 的 CI 检查与证据

本文件属于 `mouse9527/multica` fork（上游没有）。从 M-100 起，研发验证「CI 优先」：编译和测试交给 GitHub Actions，开发、审查、验收引用 PR 上**这个 HEAD** 的检查结果作为证据，本机只做 CI 覆盖不到的事。

PR 上有两个工作流：

- **`CI`**（`.github/workflows/ci.yml`）：上游的工作流，fork 不改它，同步上游时不会冲突。多数检查按改动路径选择，`backend`、`frontend` 两个汇总检查会核对「该跑的都跑过、跳过的确实不该跑」。
- **`CI (fork)`**（`.github/workflows/ci-fork.yml`）：fork 补的检查，每个 PR、每次推 `main` 都跑，不按路径跳过；每天定时跑一次，也可以手动触发。

## 本机验证项和对应的检查

下表盘点了 #83、#84、#85、#86 里开发、审查、验收在本机跑过的验证。

| 本机跑过的验证 | 对应的检查（PR 上的名字） | 说明 |
|---|---|---|
| Go 编译（`go build ./...`） | `backend-tests` | `go build -race ./...`，整个模块 |
| `go vet` | `backend-tests` | `go vet -race -tags agentintegration ./...`，整个模块（打了 tag 的文件也查） |
| 单元测试和集成测试（含 `DATABASE_URL` 的数据库用例） | `backend-tests`、`backend-agent-tests` | 全量 `go test`，`-count=1`；CI 先在 Postgres 17（pgvector 镜像）上 `migrate up` 再跑 |
| 数据库用例**没有被 skip** | **`backend-db-no-skip`**（新增） | 见下文。上游没有这项：数据库连不上时，用例 skip、包照样显示 `ok` |
| `-race` | `backend-tests`、`backend-agent-tests`、`backend-db-no-skip` | 都开 `-race` |
| sqlc 一致性（`make sqlc` 后无差异） | `sqlc-check` | 只在改了 `server/pkg/db/queries`、`generated`、`server/migrations`、`sqlc.yaml` 等路径时跑；没跑时 `backend` 汇总会确认它本来就不该跑 |
| 迁移从头 `up` | `backend-tests`（`Run migrations` 步骤）、`migrations-roundtrip` | |
| 迁移 up → down → up 往返，全新库和「旧 main 先 up、再补跑新迁移」两条路径，`pg_dump -s` 比对 | **`migrations-roundtrip`**（新增） | 见下文。原来只有 `internal/migrations` 里的 Go 测试在沙箱 schema 里做往返，不比对完整结构 |
| 迁移测试（`go test ./internal/migrations ./cmd/migrate`） | `backend-tests` | 全量测试的一部分 |
| 真实 HTTP 端到端的 Go 测试（`cmd/server` 的 `TestRouter*` 等） | `backend-tests` | 同上 |
| 前端 typecheck、lint、build | `frontend-build` | `turbo build typecheck lint`（不含 docs、mobile） |
| 前端 knip、脚本测试、依赖 catalog | `frontend-build`（只改质量相关路径时是 `frontend-quality`） | 共用 `.github/actions/frontend-quality` |
| vitest | `frontend-test`、`frontend-views-test (1/2)`、`frontend-views-test (2/2)` | views 分两片 |
| mobile lint、typecheck | `mobile`（`Mobile Verify` 工作流） | 只在改了 mobile 依赖的路径时出现 |
| `git diff --check` | 没有 | 不用编译，本机跑就行 |

前端检查只在改了前端路径时跑，后端同理；没跑时看 `frontend`、`backend` 两个汇总检查是否通过。

## `migrations-roundtrip`

脚本：`scripts/fork-migrations-roundtrip.sh`。用真实的迁移程序（`server/cmd/migrate`）和服务端同版本的 `pg_dump --schema-only` 比对结构，比对前只去掉 `pg_dump` 每次都不同的行（`\restrict` 密钥、版本横幅）。

- **base** 是 PR 合并提交的第一个父提交（即当前 `main`）；推 `main` 时是上一个提交。
- **升级路径**：库 A 用 base 的迁移 `up`，再用 head 的迁移 `up`；库 B 用 head 从头 `up`。A 和 B 的结构必须一致（`upgrade-matches-fresh`）。上游同步带来的新迁移也由这一项覆盖。
- **往返**：只对 fork 的迁移做（编号 ≥ 9000；上游有故意不可回滚的迁移，例如 494）。PR 新增的 fork 迁移在库 B 上 `down`，结构必须等于「head 去掉这些迁移后从头 `up`」的库（`down-matches-without`）；再 `up`，结构必须回到库 B（`reup-matches-fresh`）。新增的 fork 迁移没有 down 文件也算失败。
- 定时和手动触发时，往返覆盖**全部** fork 迁移，不只是新增的。
- 失败时，结构 dump 和 diff 作为构件 `migrations-roundtrip` 上传，保留 7 天。日志开头列出新增迁移、被改动的已有迁移、参与往返的迁移。

本机复现（需要 Go 和一个可写的 Postgres）：

```bash
git worktree add --detach /tmp/base origin/main
DB_URL_PREFIX=postgres://multica:multica@localhost:5432/ \
  bash scripts/fork-migrations-roundtrip.sh --base /tmp/base --head . --out /tmp/roundtrip
```

`PG_CLIENT='docker exec -i <容器>'` 可以让 `psql`、`pg_dump` 在数据库容器里跑，避免客户端和服务端版本不一致。

这项检查不替代**真实数据副本上的迁移演练**：它只用空库，验证不了存量数据。部署前的演练仍由运维在集群做。

## `backend-db-no-skip`

在 Postgres 和 Redis 就绪、`migrate up` 之后，用 `go test -race -json` 跑和 `backend-tests` 相同的包（不含不读数据库的 `pkg/agent`），再由 `scripts/fork-go-test-skips.mjs` 读 JSON：

- 任何 skip 原因提到数据库（`database`、`postgres`、`DATABASE_URL`、`DB`、`not migrated`）就失败；唯一放行的是 pgvector 镜像没有的 `pg_bigm` 扩展。
- 包的 `TestMain` 连不上数据库时会打印 `Skipping … database not reachable` 后直接退出，这种也失败。
- 一个用例都没通过（输出为空或被截断）也失败。
- 运行摘要写进这次 run 的 Summary：有测试的包数、通过、失败、跳过的数量，数据库相关的 skip 数，以及其他 skip 的列表。失败时上传构件 `backend-db-no-skip`（原始 JSON）。

## 审查和验收怎么引用

1. 先确认 PR 的 HEAD：`gh pr view <PR> --json headRefOid`。
2. 列出这个 HEAD 上的检查：`gh pr checks <PR>`（名字、结果、run 链接）；需要按提交核对时用 `gh api repos/mouse9527/multica/commits/<SHA>/check-runs --jq '.check_runs[] | [.name, .conclusion, .html_url] | @tsv'`。
3. 评论里写检查名和 run 链接，例如「`migrations-roundtrip` pass，<run 链接>」。只有当前 HEAD 上的结果算数；推了新提交就要重新核对。
4. `skipped` 只有在对应的汇总检查（`backend`、`frontend`）通过时才算正常。

## 必需检查

`main` 目前没有分支保护，所有检查都不是「必需」。建议由 Owner 在本 PR 合并后加一条 ruleset，把下面这些设为必需（都会出现在每个 PR 上，路径跳过的子检查由汇总检查兜底）：

- `backend`、`frontend`（`CI`）
- `migrations-roundtrip`、`backend-db-no-skip`（`CI (fork)`）

要先合并再加：在 `main` 上还没有 `CI (fork)` 时就设为必需，所有 PR 都会卡住。

## CI 覆盖不到、仍在本机或集群做的事

- 测试环境上的端到端实测（真实实例、真实 daemon 与智能体）。
- 真实数据副本上的迁移演练（运维在集群做）。
- UI 截图（本次不做自动截图）。
- `git diff --check` 这类不用编译的小检查。
