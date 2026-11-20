# 密码登录：开关、上线与实测（fork）

本文件属于 `mouse9527/multica` fork，上游 `multica-ai/multica` 没有这部分功能。这里写打开、上线、验证密码登录要做的事；表结构、哈希、接口与错误码等设计细节见 [password-auth.md](password-auth.md)。实施跟踪见 #42，本文件对应收尾阶段 #41。

## 开关与环境变量

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `MULTICA_PASSWORD_AUTH_ENABLED` | 关 | `true` 或 `1` 为开。关闭时不注册密码路由（请求得到 404），`/api/config` 不含 `password_login_enabled`，登录页与设置页和上游一致 |
| `MULTICA_PASSWORD_ARGON2_MEMORY_KIB` | 65536 | argon2id 内存（KiB），不低于 19456 |
| `MULTICA_PASSWORD_ARGON2_ITERATIONS` | 3 | argon2id 迭代次数；「内存 × 迭代」不低于 38912 |
| `MULTICA_PASSWORD_ARGON2_PARALLELISM` | 4 | argon2id 并行度，1–255 |
| `MULTICA_PASSWORD_ARGON2_MEMORY_BUDGET_KIB` | 262144 | 同一进程同时进行的哈希计算合计可占的内存（默认 256 MiB，约 4 个并发） |
| `MULTICA_PASSWORD_IP_LIMIT` | 开 | 按 IP 限流的开关。`0` 或 `false` 为关：不再按地址计数，只剩按账号锁定，启动日志有 `password login: per-IP limit off`。关之前读下文「已接受的风险」 |
| `RATE_LIMIT_TRUSTED_PROXIES` | 空 | 上游已有的变量。服务在反向代理或 Ingress 后面时**必须**配上代理的地址段，见下文「上线前检查清单」 |
| `DATABASE_URL` | — | 管理员重置命令 `reset_password` 用，容器里已有 |

- 不设 argon2id 变量就用默认值，一般不用设。设了但不合规（低于下限、超出预算等）时，服务照常启动，记一条 error 日志 `password login disabled: invalid argon2id settings`，密码登录保持关闭。
- 关掉：去掉 `MULTICA_PASSWORD_AUTH_ENABLED` 并重启。已设的密码留在 `user_credential` 表里，再打开时仍然有效。
- 打开、关闭都只影响密码登录；验证码登录和 Google 登录不受影响，始终可以作为后备。

## 锁定规则

- **按账号**：连续输错 5 次锁 1 分钟；此后每多错一次，锁定时长翻倍（2、4、8 …分钟），最长 1 小时。锁定期间密码正确也被拒绝。
- 失败次数只在三种情况下清零：登录成功、在设置页设置新密码、管理员重置。锁过期后再猜错一次会锁得更久。
- **按 IP**：每个地址每分钟最多 10 次登录尝试（IPv6 按 /64 计），存在进程内。设 `MULTICA_PASSWORD_IP_LIMIT=0` 可以关掉，按账号锁定不受影响。
- 不存在的邮箱和没设密码的账号按同样规则"锁"，响应与真实账号相同，不能据此判断账号是否存在、是否设了密码。
- 页面上：密码错误、邮箱不存在、没设密码都显示「邮箱或密码不正确。」（HTTP 401 `invalid_credentials`）；锁定和 IP 限流（开着时）都显示「尝试次数过多，请等几分钟再试。」（HTTP 429 `too_many_attempts`）。
- 多副本：IP 限流和"无密码邮箱"的计数只在单个进程内，N 个副本时配额约为 N 倍；按账号锁定存在数据库里，不受影响。现网是单副本。

细节（排队时如何判断锁定、内存预算等）见 [password-auth.md](password-auth.md)「失败锁定与限流」。

## 管理员重置密码

用户忘了密码、或者要在用户自己设置之前先给他一个密码时，由管理员在服务端执行。只能给**已存在的用户**设置，不会创建用户；不看功能开关。

1. 确认用户的登录邮箱。
2. 在后端容器里执行（`<后端>` 换成后端的 Deployment 名）：

   ```bash
   kubectl exec -it deploy/<后端> -- ./reset_password --email user@example.com
   ```

   命令先按邮箱找用户（找不到就报错退出，这时还没让你输密码），再提示输入新密码两次，输入不回显。密码至少 12 个字符。
3. 成功时输出 `Password set for user <用户 ID>; failed attempts and lock cleared.`，并在 stderr 打出一行 JSON 审计记录（`passwordauth: audit: password reset by admin command`，含 `user_id`、`email`、`at`）。这行只回到你的终端，不进容器日志；需要留档就自己保存。
4. 新密码立即生效，原来的锁定和失败次数同时清除。把新密码通过安全渠道交给用户，并请他登录后在「设置 → 个人资料 → 密码」改成自己的。

非交互方式（例如脚本）：`kubectl exec -i deploy/<后端> -- ./reset_password --email user@example.com < password.txt`，文件里只放密码一行。不要把密码写在命令参数里，命令也不接受。

退出码：0 成功；1 失败（用户不存在、密码不合规、数据库错误、审计记录写不出去）；2 用法错误。

## 上线前检查清单

在真实实例打开开关之前逐项确认：

- [ ] **先在测试库演练**：在测试实例上把下面的「Owner 实测清单」完整走一遍。
- [ ] **备份数据库**，并在副本上验证迁移 `9018_passwordauth_user_credential` 的 up 和 down（运维做）。迁移只新建 `user_credential` 表，不改现有表；回滚会删掉所有已设的密码，用户仍可用验证码登录。
- [ ] 部署的镜像包含 `reset_password`：`kubectl exec deploy/<后端> -- ls /app` 能看到它。
- [ ] **`RATE_LIMIT_TRUSTED_PROXIES`**（按 IP 限流开着时）：服务在反向代理或 Ingress 后面时，把代理的地址段配进去。否则所有用户看起来来自同一个代理地址，全站共用每分钟 10 次的配额，任何人连续输错 10 次就能让所有人收到「尝试次数过多」。打开后用实测清单第 7 步核对：日志里 `user logged in with password` 的 `ip` 应是真实客户端地址，不是代理地址。
- [ ] argon2id 变量要么不设，要么合规：重启后日志里没有 `password login disabled: invalid argon2id settings`，`/api/config` 返回 `"password_login_enabled": true`。
- [ ] 后端容器的内存上限为哈希预算（默认 256 MiB）留出余量。
- [ ] **保留验证码登录作为后备**：打开后登录页的「改用邮箱验证码」能走通；不要因为有了密码登录就停掉验证码。
- [ ] 知道怎么关：去掉 `MULTICA_PASSWORD_AUTH_ENABLED` 并重启即恢复上游行为。
- [ ] 读过下文「已接受的风险」。

## Owner 实测清单

在**测试实例**上照做，不要在真实实例上先做。需要：一个能收验证码的测试账号邮箱、对测试实例后端的 `kubectl` 权限、一台手机（或桌面浏览器的手机模式）。下文 `<测试实例>` 换成测试实例的地址。

| # | 操作 | 预期结果 |
|---|---|---|
| 1 | 开关关闭时打开登录页；执行 `curl -s https://<测试实例>/api/config`；`curl -s -o /dev/null -w '%{http_code}\n' -X POST https://<测试实例>/auth/password-login` | 登录页只有验证码登录，与上游一致；config 里没有 `password_login_enabled`；POST 返回 `404` |
| 2 | 给后端加环境变量 `MULTICA_PASSWORD_AUTH_ENABLED=true`，重启；再执行第 1 步的 `curl` | 启动日志没有 `password login disabled`；config 含 `"password_login_enabled": true`；POST 不带请求体返回 `400` |
| 3 | 打开登录页 | 第一步是邮箱加密码表单，下面有「改用邮箱验证码」，Google 登录按钮照旧 |
| 4 | **首次设置密码**：点「改用邮箱验证码」，用验证码登录；进入「设置 → 个人资料」，页面底部的「密码」区块 | 只要求填新密码和确认，不要求当前密码 |
| 5 | 先输一个 11 个字符的密码 | 提示「密码至少需要 12 个字符。」，不提交 |
| 6 | 输一个 12 个字符以上的密码并确认，点「设置密码」 | 提示「密码已设置」；之后这一块多出「当前密码」，按钮变成「修改密码」 |
| 7 | 退出登录，用邮箱加刚设的密码登录 | 登录成功；后端日志有 `user logged in with password`，其中 `ip` 是你的真实地址（不是代理地址） |
| 8 | 退出，用一个不存在的邮箱加任意密码登录；再用测试账号加错误密码登录 | 两次都显示「邮箱或密码不正确」，文案完全相同 |
| 9 | **触发锁定**：用测试账号连续输错密码，直到出现「尝试次数过多」（前面第 8 步已错 1 次，再错 4 次后第 6 次尝试即出现）；然后输入正确密码 | 正确密码也显示「尝试次数过多」 |
| 10 | 等 1 分钟后输入正确密码 | 登录成功（锁过期，失败次数清零） |
| 11 | **管理员重置**：距第 10 步至少等 1 分钟（见表后说明），再按第 9 步把账号锁上；然后执行 `kubectl exec -it deploy/<后端> -- ./reset_password --email <测试账号邮箱>`，输入一个新密码两次 | 输出 `Password set for user …; failed attempts and lock cleared.` 和一行审计 JSON（不含密码） |
| 12 | 不用等，立刻用第 11 步的新密码登录；再试一次旧密码 | 新密码登录成功（锁已解除）；旧密码显示「邮箱或密码不正确」 |
| 13 | 在「设置 → 个人资料 → 密码」修改密码：先把当前密码填错 | 提示「当前密码不正确。」；填对后提示「密码已修改」，之后用新密码能登录 |
| 14 | **手机宽度**：用手机浏览器（或桌面浏览器开发者工具的手机模式，宽度约 375px）打开登录页和「设置 → 个人资料 → 密码」 | 输入框和按钮都在屏幕内、页面不横向滚动、都能点中；登录和设置密码都能完成；密码管理器能提示保存或自动填充 |
| 15 | **验证码后备**：在登录页点「改用邮箱验证码」，走完验证码登录 | 照常登录成功 |
| 16 | （可选）在本机执行 `multica login`，在打开的网页里用密码登录 | CLI 登录成功，和验证码登录一样 |
| 17 | 检查后端日志：`kubectl logs deploy/<后端> \| grep -i password` | 能看到上述登录、锁定、设置事件；找不到你输过的任何密码明文，也没有 `$argon2id$` 开头的哈希 |
| 18 | **关闭**：去掉 `MULTICA_PASSWORD_AUTH_ENABLED`，重启；重复第 1 步 | 回到上游行为（无密码表单、config 无字段、POST 404）；验证码登录正常 |

**第 10–12 步与按 IP 限流**（设了 `MULTICA_PASSWORD_IP_LIMIT=0` 时不存在这个问题）：管理员重置只清账号的失败次数和锁定，**不清按 IP 的配额**（同一地址每分钟 10 次，从这一分钟的第一次尝试起算）。第 11 步锁账号要 6 次尝试、第 12 步再 2 次，和前面的尝试挤在同一分钟里就会超过 10 次，这时新密码也显示「尝试次数过多」，看起来像重置没生效。所以第 11 步前先等满 1 分钟。如果第 12 步仍出现「尝试次数过多」，那是 IP 配额：等 1 分钟后从第 11 步重做（等过 1 分钟账号锁也过期了，直接重试证明不了是重置解的锁）。

全部符合预期后，再按「上线前检查清单」在真实实例上打开。实测中任何一步不符合，记下步骤号和看到的现象，关掉开关即可恢复。

## 安全测试汇总

下面的测试都在 CI 里跑（Go 测试在 `backend-tests` 与 `backend-db-no-skip`，后者保证需要数据库的用例没有被跳过；前端测试在前端检查里）。

| 关注点 | 测试 | 覆盖了什么 |
|---|---|---|
| 爆破锁定 | `passwordauth`：`TestLoginLockoutAndRecovery`、`TestLockDuration` | 5 次错后锁定，锁定中正确密码也拒绝，过期后再错一次翻倍，成功后清零 |
| | `passwordauth`：`TestQueuedGuessesStopAtTheLock`、`TestQueuedLoginRefusedAfterLock`、`TestQueuedLoginCancelled` | 并发猜测排队等哈希预算时，锁上之后不再计算；锁定中排队的正确密码不签发会话；断开的请求不计算、不计失败 |
| | `passwordauth`：`TestLoginIPLimit`、`TestIPLimiter`、`TestIPLimiterFullTable`；`handler`：`TestPasswordLoginLockout` | 按 IP 每分钟 10 次；IPv6 按 /64；表满时放行；HTTP 层锁定与限流返回相同的 429 |
| | `passwordauth`：`TestIPLimitEnabled`、`TestLoginWithoutIPLimit`；`handler`：`TestPasswordIPLimitSwitch`（#136） | 只有 `0`、`false` 关掉按 IP 限流，不设或其他值保持开启；关掉后同一地址可以连续尝试，真实账号和无密码邮箱仍在 5 次后锁定；开关经 `MountPasswordAuth` 生效，关掉时启动日志有提示 |
| 防枚举 | `passwordauth`：`TestLoginAnswersDoNotRevealAccounts`、`TestPhantomLocks` | 不存在的邮箱、没设密码、密码错误走完全相同的应答序列，含第 5 次后的"锁定"；日志不记尝试的邮箱 |
| | `handler`：`TestPasswordLogin`、`TestPasswordLoginLockout` | 三种失败的 401 响应体逐字节相同；锁定与 IP 限流的 429 相同 |
| | 前端 `password-login.test.tsx` | 只显示「邮箱或密码不正确」「尝试次数过多」，不显示服务端原文 |
| 时序 | `passwordauth`：`TestLoginHashesOncePerInvalidAnswer`（M-44 新增） | 计数：所有 401 路径（含超长密码、不存在的邮箱）恰好做 1 次 argon2id，所有 429 路径（账号锁定、无密码邮箱"锁定"、IP 限流）做 0 次 |
| | `passwordauth`：`TestLoginTimingRough`（M-44 新增） | 粗测：用较慢的哈希参数交替测量 15 轮，密码错误、没设密码、不存在的邮箱三者耗时中位数相差不超过 3 倍；锁定账号（不做哈希）的中位数明显更快，证明这个测量能发现漏掉的哈希 |
| | `passwordauth`：`TestVerifyDummyNeverMatches`、`TestVerifyOverlongPasswordNeverMatches`、`TestLoginRefusesHashOverBudget` | 无密码账号的替身校验永不通过；超长密码只哈希前 1024 字节；超预算哈希按错误密码处理并做一次替身校验 |
| 开关 | `handler`：`TestPasswordAuthSwitch`；`cmd/server`：`TestPasswordRoutesFollowTheSwitch`；`passwordauth`：`TestEnabled` | 关闭（空、`false`）时三个路由 404、`/api/config` 无字段；参数不合规时保持关闭；打开后在生产路由上可用 |
| | 前端 `password-login.test.tsx`、`password-section.test.tsx`、`api.test.ts` | 开关关闭时登录页与上游一致、设置页不渲染也不请求；只认显式的 `true` |
| 密码与哈希不外泄 | `passwordauth`：`TestSetPasswordErrorHidesPasswordAndHash`、`TestRehashFailureLogHidesPasswordAndHash`、`TestCheckPasswordErrorsHidePassword`、`TestCredentialNeverPrintsHash` | 数据库错误（含 Postgres 的 `Detail`）、日志、`fmt`/`slog`/JSON 输出都不含密码与哈希 |
| | `passwordauth` 与 `handler` 的数据库用例（`assertNoSecrets`） | 响应体、响应头、日志不含测试密码，也不含库里任何一个哈希或其片段 |
| | `cmd/reset_password`：`TestRunAgainstDatabase`、`TestReadPiped`、`TestPromptTwice`；`passwordauth`：`TestAdminResetAgainstDatabase` | 重置命令的输出、日志、审计记录不含密码和哈希 |
| | 前端 `api.test.ts` | 前端错误对象不含密码 |
| 只用会话、不建账号 | `handler`：`TestPasswordManagementRefusesPATs`；`middleware`：`TestAuthenticatedBySession` | 个人访问令牌（含放进 Cookie 并带上可自算的 CSRF 值）、任务令牌、云节点令牌都不能读写密码 |
| | `handler`：`TestSetMyPasswordNeedsExistingUser`；`cmd/reset_password`：`TestRunAgainstDatabase` | 设置接口和重置命令都不会创建用户或给不存在的邮箱建凭据 |
| 管理员重置后登录 | `cmd/server`：`TestAdminResetThenPasswordLogin`（M-44 新增） | 在生产路由上：管理员给没密码的用户设密码后可经 `POST /auth/password-login` 登录，返回的会话能访问 `/api/me`；账号锁定后重置，新密码立即可登录，旧密码不再可用 |
| 哈希与参数 | `passwordauth`：`TestHashRoundTrip`、`TestNeedsRehashAfterParameterUpgrade`、`TestCheckPasswordRehashesAfterParameterUpgrade`、`TestRehashDoesNotOverwriteNewerPassword`、`TestParamsFromEnv`、`TestValidatePassword` 及 `budget_test.go` | 加盐、参数升级后自动重哈希且不覆盖新密码、弱参数拒绝启动、12 字符下限按 Unicode 字符计、内存预算 |

## 已接受的风险

- **个人访问令牌（PAT）可变成密码**（Owner 2026-11-19 对 M-41 选 B）：上游的 `POST /api/cli-token` 允许用 PAT 换一个普通会话 JWT，用这个会话可以给**还没设过密码**的账号设第一个密码。所以 PAT 泄露后，攻击者可以给没密码的账号设一个自己知道的密码；之后只吊销 PAT **撤不掉这个密码**。发现 PAT 泄露时：吊销 PAT，并用该账号登录网页查看「设置 → 个人资料 → 密码」是否显示为已设密码（或调用 `GET /api/me/password`）；如果用户自己没设过，用管理员重置命令改掉。
- **多副本**：IP 限流和"无密码邮箱"的计数只在进程内，见上文「锁定规则」。现网单副本。
- **关掉按 IP 限流**（`MULTICA_PASSWORD_IP_LIMIT=0`，#136）：
  - 不变的：单个账号能被猜的次数（5 次后锁，之后翻倍，最长 1 小时；锁定期间不做哈希）；不存在的邮箱同样会"锁"；验证码登录不受账号锁定影响，始终可用。
  - 失去的：对总请求量的限制。攻击者从一个地址换着邮箱（或对很多账号各试几个常见密码）不断请求，每个没锁的邮箱都要做一次 argon2id。
  - 已有的上限：哈希内存预算（默认 256 MiB）把同时进行的 argon2id 限制在 4 个，内存不会超；默认参数每个用 4 个线程，最多约 16 个线程在算，每个进程每秒约 80 次。所以不会无限占内存，但**能把后端的 CPU 占满**（容器核数少于 16 时），拖慢同一进程里的其他接口；排队等预算的请求没有上限，正常用户的密码登录会排在攻击请求后面，变得很慢甚至超时（这时验证码登录仍可用）。
  - 也会更容易被人故意把别人的账号锁住（对账号连续输错），不过开着按 IP 限流时换地址同样做得到。
  - 可选的缓解（是否做由 Owner 决定）：一是不改代码，把 `MULTICA_PASSWORD_ARGON2_MEMORY_BUDGET_KIB` 调到 `65536`，同时只算 1 个哈希，最多约 4 个线程，代价是攻击期间正常登录排队更久；二是改代码，给排队设上限，排队的请求超过一定数量就直接返回 429、不做哈希；三是改代码，加一个全进程的速率上限（例如每分钟 N 次，不分地址），超出返回 429。二和三在被攻击时都会让正常用户也收到「尝试次数过多」，但 CPU 和排队都有上限。
- **"无密码邮箱"计数会遗忘**：24 小时没有新失败、或服务重启后清空；真实账号的计数不会自己清零。所以长时间后两者可能不同，但每探测一次要等一天。
- **IP 表满时放行**：同时跟踪的地址超过 1 万且都未过期时，新地址不计数（不因地址洪水挡住所有人）；按账号锁定仍然有效。
- **锁上那一刻正在计算的猜测**：最多与哈希并发数相同（默认 4 个）的猜测在锁定前已开始计算，它们照样计入失败次数。
- **时序只做到粗略一致**：所有 401 路径都做一次相同参数的哈希（默认约 50 ms），但数据库查询次数不同（不存在的邮箱少几次查询，约为亚毫秒到毫秒级）。`TestLoginTimingRough` 只证明差距远小于一次哈希，不是严格的旁路分析。
- **桌面端**与网页共用登录组件，开关打开时同样可用，但不专门适配也不测试；原生 mobile app 不包含。
- **重置命令的审计记录**只回到执行人的终端，不进容器日志，需要执行人自己留档。
