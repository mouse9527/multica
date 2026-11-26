# fork 的发布与 runner 自动更新

本文件属于 `mouse9527/multica` fork（上游没有）。说明 fork 怎么用一个 tag 同时发布 runner 用的 `multica` 二进制包和 server/web 镜像，以及 `multica update` 和 daemon 怎么从 fork 的 Release 更新。

## 版本号

- tag 写成 `v<上游版本>-mouse.<N>`，例如 `v0.8.2-mouse.1`。前半段是 fork 基于的上游版本，`N` 是在同一个上游版本上的追加序号，从 1 开始；跟进到新的上游版本后 `N` 从 1 重新计。
- 上游版本取 fork `main` 已合入的最新上游 tag。例：`main` 基于上游 v0.8.1 时，第一次发布是 `v0.8.1-mouse.1`。
- 不会和上游 tag 撞名；fork 不推送上游的 tag。
- 新旧比较：先比 `X.Y.Z`，再按数字比 `N`；没有后缀的上游版本视为 `N = 0`。所以 `v0.8.2 < v0.8.2-mouse.1 < v0.8.2-mouse.9 < v0.8.2-mouse.10 < v0.8.3-mouse.1`。
- `multica --version` 显示去掉 `v` 的版本（`0.8.2-mouse.1`）。
- `git describe` 风格的开发版本（`v0.8.2-mouse.1-3-gabc1234`）、`dev`、上游的 `-rc` 之类都不算 release：自动更新对它们不生效。

## 发布一个版本

发版的人工决定只有一个：**Owner 在 Multica 里批准发版**（具体提交和版本号）。GitHub 上不再有人工审批，`release-fork.yml` 的 `verify` job 用自动检查代替（见下文「发布闸门」）。

1. 确认要发布的提交已经在 `main` 上，它在 `main` 上的 CI、CI (fork)、Build Image 三个运行都已经成功结束（还在跑时推 tag，闸门会失败）。
2. 版本号：前半段取这个提交已合入的最新上游 tag，`N` 比同一上游版本已有的最大 `N` 大（通常加 1）。
3. Owner 在 Multica 里批准后，打 annotated tag 并推送：

   ```bash
   git tag -a v0.8.1-mouse.1 <commit> -m "v0.8.1-mouse.1"
   git push origin v0.8.1-mouse.1
   ```

4. 推送后两个工作流同时运行，都会先检查 tag，不满足就什么都不发布：
   - `.github/workflows/release-fork.yml`：`verify` job 先过发布闸门，再跑更新相关测试、签名脚本测试和 `govulncheck`；全部通过后发布 job 直接开始，用 `.goreleaser.fork.yml` 构建、给 darwin 二进制签名（见下文「macOS 签名」）并发布 Release。产物是 `multica-cli-<版本>-{darwin,linux,windows}-{amd64,arm64}.{tar.gz,zip}` 和 `checksums.txt`；每个压缩包带 `LICENSE`、`NOTICE`、`README*` 和修改清单 `docs/github-issues-upstream-changes.md`。最后核对两个 darwin 包里的签名、6 个平台的包和 `checksums.txt` 都在、Release 不是 pre-release、并且（最新版本时）是 latest。
   - `.github/workflows/build-image.yml`：检查 tag 格式和 tag 指向的提交在 `main` 上，然后构建并推送 `ghcr.io/mouse9527/multica-backend:<tag>` 和 `multica-web:<tag>`。

### 发布闸门

`scripts/fork-release-verify.sh` 在 `verify` job 第一步运行（测试：`scripts/fork-release-verify.test.sh`，`verify` 先跑一遍，CI (fork) 的 `fork-ci-config` 在每个 PR 上也跑）。任一条不满足，脚本失败，`verify` 失败，发布 job 不运行：

| 检查 | 怎么查 | 不满足的典型原因 |
|---|---|---|
| tag 格式 `vX.Y.Z-mouse.N`，`N` ≥ 1、不带前导零 | 正则 | 打错名字 |
| tag 是 annotated tag，直接指向这次运行的提交 | 从 fork 重新拉取 tag 到 `refs/fork-release-verify/`，看对象类型和目标 | 用了 `git tag` 没加 `-a`；tag 推送后又被移动 |
| 提交在 `main` 上 | `git merge-base --is-ancestor <提交> main`（完整 clone，浅 clone 直接失败） | 在分支或 PR 的提交上打了 tag |
| `X.Y.Z` 是这个提交已合入的最新上游 tag | `git ls-remote --tags` 读上游的 `vX.Y.Z` tag，从新到旧找第一个是这个提交祖先的 | 同步上游后还用旧前缀，或用了还没合入的上游版本 |
| `N` 大于同一上游版本已有的最大 `N`（不算这个 tag 自己） | fork 的 tag 列表 | 序号重复或倒退 |
| 这个提交在 `main` 上的 CI、CI (fork)、Build Image 都是 success | Actions API 按工作流文件（`ci.yml`、`ci-fork.yml`、`build-image.yml`）、提交、`push` 事件、`main` 分支查，取最新一次运行 | 还在跑、失败、被取消（CI (fork) 在 `main` 上会被下一次推送取消），或者查不到；API 出错也算失败 |

`verify` job 的 token 只有 `contents: read`、`actions: read`。

闸门失败后：等检查跑完再在 Actions 里重跑这次 Release (fork) 运行；提交不对就删掉 tag，在正确的提交上按规则重新打。Build Image 的 tag 运行不经过这个闸门，闸门失败时镜像可能已经推了 `<tag>`（是最新的还会移动 `latest`），要么修好后重跑 Release (fork)，要么把 `latest` 镜像移回。

### `latest` 的规则

| 事件 | 镜像 | GitHub Release |
|---|---|---|
| 合并到 `main` | 只推 `sha-<完整提交>`，不动 `latest` | 不产生 |
| 推 `-mouse.N` tag，且它是最新的 `-mouse.N` | 推 `<tag>`，并把 `latest` 移到这次构建 | 发布，标为 latest |
| 推 `-mouse.N` tag，但已有更新的 `-mouse.N`（比如重跑旧 tag） | 只推 `<tag>`，不动 `latest` | 闸门拒绝（`N` 不大于已有最大值），不发布 |

「最新」由 `scripts/fork-release-is-newest.sh` 按上面的版本比较规则判断（测试：`scripts/fork-release-is-newest.test.sh`，两个工作流用它之前都会先跑）。它通过 GitHub API 读仓库的 tag 列表；读取失败时脚本报错退出，所在步骤失败，`latest` 不会移动，Release 也不会发布。这样 server 取 `latest` 镜像、runner 取最新 Release，两者指向同一个版本。

同一 tag 重跑会替换同名产物。

**只成功了一半**（两个工作流互相独立）：
- Release 成功、镜像失败：runner 会先升到新版本，server 还在旧的 `latest` 镜像上。在 Actions 里重跑这个 tag 的 Build Image 运行；短时间内修不好就先把这个 Release 改回草稿，runner 停在旧版本，等镜像修好再发布。
- 镜像成功、Release 失败：server 先更新，runner 不变。重跑这个 tag 的 Release (fork) 运行。
- 判断「是否最新」时 GitHub API 出错导致的失败，直接重跑即可。

撤回一个坏版本：把 Release 删掉或改回草稿，`/releases/latest` 回到上一个版本；`latest` 镜像要手动移回（或发一个新的 `-mouse.N`）；已经更新的 runner 需要手动装回旧版本（见下文）。

### 上游发布流程里关掉的部分

runner 包只发布到 fork 的 GitHub Release，不发布到任何第三方包管理渠道。上游的 `release.yml` 在 fork 上整体不运行（`verify` 加了 `github.repository_owner == 'multica-ai'`，其余 job 都依赖它）；上游 `.goreleaser.yml` 只被这个工作流使用，所以它的 Homebrew 配置在 fork 上也不会执行：

| 上游步骤 | 为什么关掉 |
|---|---|
| GoReleaser 的 Homebrew tap（`brews`） | 第三方渠道；要 `HOMEBREW_TAP_GITHUB_TOKEN`，写的是 `multica-ai/homebrew-tap` |
| 多架构镜像（`docker-*`） | 和 `build-image.yml` 推同一组 GHCR 标签会互相覆盖；fork 用 `build-image.yml` |
| Helm chart | 上游本来就只在 `multica-ai` 运行 |
| Desktop 安装包 | 不在本 fork 的范围 |
| 旧压缩包名 `multica_{os}_{arch}` | 只给上游老版本 CLI 兼容用，fork 的 CLI 先找新名字 |

## macOS 签名

macOS 按代码签名的 designated requirement（DR）记住隐私授权（照片、iCloud 等 TCC 授权）。没签名或 ad-hoc 签名的二进制，DR 就是它的 cdhash，每次升级都算新程序，授权全部丢失、重新弹窗。所以发布的 darwin 二进制都用**同一张 CI 专用证书、同一个 identifier** 签名，DR 固定为：

```
identifier "ai.multica.cli" and certificate leaf = H"<证书 SHA-1>"
```

这和 `codesign` 对自签名证书默认生成的 DR 一样。升级以后 DR 不变，之前的「允许」「不允许」继续有效。

### 证书与密钥

- 自签名证书，CN `multica-ci`，扩展用途只有 Code Signing，有效期 10 年。它不是 Apple 签发的，不做公证（notarization）；只解决「身份不变」，不解决 Gatekeeper。命令行下载的二进制没有隔离属性（quarantine），Gatekeeper 不拦。
- 私钥只在 GitHub `release` Environment 的两个 Secret 里：`MACOS_SIGN_P12_BASE64`（PKCS#12，base64）和 `MACOS_SIGN_P12_PASSWORD`。生成时用管道直接产出 PKCS#12 写进 Secret，不落盘；只保留公开的证书。
- PKCS#12 必须用 `PBE-SHA1-3DES` 加密（`-keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1`）：rcodesign 0.29 读不了 OpenSSL 3 默认的 AES 加密，会报密码错误。
- 证书的 SHA-1（`706a5367ebdf9a6deebae02d1301fe303e0287e7`，有效期到 2036-11-13）固定在 `scripts/fork-macos-sign.sh` 的 `PINNED_CERT_SHA1`。Secret 里换成别的证书，签名后的校验对不上，发布失败。
- 换证书（泄露或到期）：生成新证书、替换两个 Secret、在 PR 里更新 `PINNED_CERT_SHA1`。新证书是新身份，所有机器会再弹一次窗。

### 签名怎么做

- 工具：[rcodesign](https://github.com/indygreg/apple-platform-rs)（apple-codesign 0.29.0），在 Linux runner 上运行，由 `scripts/fork-install-rcodesign.sh` 下载并校验 SHA-256。不用 macOS runner + `codesign`：那要在 runner 上建临时钥匙串、导入 PKCS#12、处理不受信任的自签名证书，步骤多、私钥多经过一层；而且整个发布 job 已经在 Linux 上。rcodesign 直接读 PKCS#12。它产出的签名在 macOS 上用 `codesign -d -r-`、`codesign --verify --strict` 检查和 `codesign` 签出来的一致（`scripts/fork-macos-sign.test.sh` 在 macOS 上会做这一步）。
- rcodesign 不能编译 DR 表达式（Apple 的 `csreq` 只在 macOS 上），`scripts/fork-macos-sign.sh` 自己拼出编译后的 DR；测试把它和 `csreq` 的输出逐字节比较。
- GoReleaser 的 post-build 钩子（`.goreleaser.fork.yml`）对每个 darwin 二进制调用 `scripts/fork-macos-sign.sh sign`：签名，再立即校验 identifier、DR 和 CMS 里的证书是否等于固定值。没有密钥、签名失败、校验对不上，钩子失败，GoReleaser 在打包和发布之前就停下，什么都不发布。其他平台的二进制原样通过。
- 打包和 `checksums.txt` 都在签名之后，校验和对应的是签过名的二进制。发布后 `release-fork.yml` 再从 `dist/` 的两个 darwin 包里取出二进制校验一遍。
- 不加时间戳（`--timestamp-url none`）：证书自签名、不公证，时间戳不影响身份。

### 谁拿得到私钥

- `release` Environment：Secret 只放在这里；只允许 `v*.*.*-mouse.*` 形式的 tag 部署；没有 required reviewer，发布 job 能否开始由 `verify` 的发布闸门决定（Owner 2026-11-26 决定，M-297）。
- 只有 `release-fork.yml` 的发布 job 引用这个 Environment，它只由 `-mouse.N` tag 触发。PR、普通 push 和 `fork-macos-sign.yml` 都拿不到；`fork-macos-sign.yml` 在 PR 上用一次性证书跑同样的签名流程，只为验证配置。
- fork 是公开仓库，外部贡献者的 PR 来自他们自己的 fork：`pull_request` 事件下 GitHub 不给任何 Secret，也推不了 tag。
- 还剩下的途径（主要是我们自己的智能体）：
  - 智能体用的是本机登录的 `gh`，它带 Owner 的仓库权限，推 tag 也是 Owner 的身份，GitHub 分不出是谁推的。闸门挡的是推错：不在 `main` 上的提交、CI 没绿、版本号不对、轻量 tag。它挡不住故意绕过：工作流文件取自 tag 指向的提交，在一个改过 `release-fork.yml`（删掉闸门）的提交上打 tag，跑的就是改过的工作流；拿着 Owner 凭据的进程也能改 Environment 的规则。约束靠规则：智能体不创建、不修改 Environment 和 Secret；只有 Owner 在 Multica 里批准了具体提交和版本号才打 tag；发布后看一眼 `verify` job 的输出，确认闸门的每一项都是 `ok`。
  - 闸门通过之后，发布 job 里运行的代码都能读到密钥：仓库里 `main` 的代码（GoReleaser 钩子、脚本、`go build` 时的依赖）、固定到 SHA 的 Actions、rcodesign。改这些的 PR 都要审查；工作流改动属于高档。
  - Secret 写进去之后从 GitHub 读不回来；runner 用完即毁，密钥文件在 GoReleaser 之后立即删除。

### 本机过渡

- 现在（第一个签名版本发布前）：本机 runner 的二进制是本机编译、用本机证书 `multica-local` 签名的，靠本机的签名脚本 `sign.sh`（在 runner 机器上，不在仓库里）。这些二进制都没有构建记录。
- 本机 runner 的二进制装在 `~/.local/bin/multica`（和 `scripts/install.sh` 无 sudo 时的位置一致），签名脚本在 `~/.local/share/multica/sign.sh`，默认目标就是这个二进制（M-107）。`~/.local/bin/multica` 必须是真实文件：`scripts/fork-install-macos.sh` 和 `multica daemon autostart enable` 都会先解析软链，目标是软链时，替换和登记的都是它指向的旧文件。daemon 每次启动都会把 LaunchAgent 的程序路径改成自己的路径，所以换安装位置要先改 plist，再在没有任务时 `launchctl bootout` + `launchctl bootstrap`。
- 第一个签名版本发布后，**本机只用 fork Release 里的 darwin 产物，不再在本机编译 multica**。证书从 `multica-local` 换成 `multica-ci`，DR 变了，**会再弹一次窗**；按意愿选「允许」或「不允许」后会被记住，以后升级不再弹。
- **怎么拿二进制：由运维用 `scripts/fork-install-macos.sh` 下载并替换**，不用 daemon 自动更新。自动更新只校验 `checksums.txt`，不检查签名身份；这台机器上保持关闭（不设 `MULTICA_DAEMON_AUTO_UPDATE=true`）。

  ```bash
  # 在 fork main 的检出里运行；<target> 省略时取 PATH 上的 multica（解析软链）
  bash scripts/fork-install-macos.sh v0.8.1-mouse.1 [<target>]
  ```

  脚本按顺序核对，任何一项不过都**不替换**，原二进制不动：
  1. 包的 SHA-256 和 Release 的 `checksums.txt` 一致；
  2. `codesign --verify --strict` 通过，`codesign -d -r-` 等于 `designated => identifier "ai.multica.cli" and certificate leaf = H"<CI 证书 SHA-1>"`（期望值来自 `scripts/fork-macos-sign.sh expected-requirement`，和发布时用的是同一个固定值）；
  3. 二进制 `--version` 报告的版本等于 tag、提交是 tag 指向的提交；
  4. 这个 tag、这个提交有一次成功的 Release (fork) 运行。

  通过后原子替换，旧二进制留在 `<target>.prev`，并打印提交和 Release 运行链接：**每个版本都能对应到具体提交和 CI 运行记录**。daemon 在空闲时 re-exec 进新二进制（auto-reload），也可以在没有任务时手动 `multica daemon restart`。装回旧版本：同样的命令换成旧 tag，或把 `.prev` 移回去。
- `sign.sh` 只留作应急（比如必须在本机临时编译一个版本）。本机编译、用 `multica-local` 签名的版本是**另一个身份**：切过去按 `multica-local` 的授权走（没有记录就弹窗），切回 CI 版本也一样，而且它没有构建记录。不要来回换。
- 自动更新（`multica update` 和 daemon）下载的也是 Release 里签过名的包：校验 `checksums.txt`、解压出 `multica`、原样写入临时文件、改权限、改名覆盖，不改动二进制内容，签名和身份保持不变。别的 runner 可以照常用它；只是它不额外核对签名身份。

## runner 怎么更新

### 更新源

`multica update` 和 daemon 读 `mouse9527/multica` 的最新 Release，下载前先取 `checksums.txt`，SHA-256 不一致就不解压、不替换。相关环境变量：

| 变量 | 作用 | 默认 |
|---|---|---|
| `MULTICA_RELEASE_REPO` | 读哪个仓库的 Release，格式 `owner/name` | `mouse9527/multica` |
| `MULTICA_RELEASE_API_BASE_URL` | GitHub API 兼容的镜像地址（上游已有） | `https://api.github.com` |
| `MULTICA_RELEASE_DOWNLOAD_BASE_URL` | 产物镜像地址，按 `/<tag>/<文件名>` 取（上游已有） | Release 自带的下载地址 |

用 Homebrew 装的 `multica` 跟的是上游 tap：更新源不是上游时，`multica update` 和 daemon 会报错并提示改用 Release 里的二进制，不会去跑 `brew upgrade`。

### daemon 自动更新

daemon 定期读 fork 的最新 Release，比自己新就在空闲时安装并重启进新版本（只升不降）。开关和间隔沿用上游：

| 变量 / 参数 | 作用 | 默认 |
|---|---|---|
| `MULTICA_DAEMON_AUTO_UPDATE` | 开关；`--no-auto-update` 或 `config.json` 的 `disable_auto_update` 只能关 | 自建 server 上关（和上游一致），设 `true` 打开 |
| `MULTICA_DAEMON_AUTO_UPDATE_INTERVAL` | 检查间隔 | `6h`，启动后 2 分钟先查一次 |

- 有任务在跑或正在认领时推迟到下一次检查，不打断任务。
- 下载失败、校验和不一致、Release 不存在时保留旧二进制，继续接任务，下一次检查再试。
- 只有安装成功后才重启进新版本。
- 本身是开发构建（`make daemon` 之类）的 daemon 不自动更新。
- Desktop 管理的 daemon 不自动更新（上游行为）。

要让开机自启动的 daemon 也打开自动更新，需要把 `MULTICA_DAEMON_AUTO_UPDATE=true` 写进它的启动环境（launchd plist 或 systemd unit）。

### 第一次装 fork 的二进制

现在跑上游二进制的 runner 只会从上游更新，要手动装一次 fork 版本：

```bash
TAG=v0.8.1-mouse.1; VER=${TAG#v}
OS=$(uname -s | tr A-Z a-z); ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
F=multica-cli-$VER-$OS-$ARCH.tar.gz
curl -fLO https://github.com/mouse9527/multica/releases/download/$TAG/$F
curl -fLO https://github.com/mouse9527/multica/releases/download/$TAG/checksums.txt
grep " $F\$" checksums.txt | shasum -a 256 -c -
tar -xzf "$F" multica && install -m 755 multica "$(dirname "$(command -v multica)")/multica"
multica --version
```

仓库自带的 `scripts/install.sh`、`scripts/install.ps1` 仍从上游下载，装出来的是上游二进制，不要用它们安装 fork 版本。Homebrew 装的需要先 `brew uninstall multica`，再把二进制放到 `PATH` 里的目录。daemon 会在空闲时自己重启进新二进制（auto-reload），或者手动 `multica daemon restart`。装回旧版本也用同样的步骤换成旧 tag。

## 不做的

- 不发布到 Homebrew、npm、Scoop、winget 等第三方渠道，不做桌面端安装包。
- Web 端运行时页面的「有新版本」提示仍读上游的最新 Release；fork 版本号带后缀，它不会提示升级，不影响 daemon 的自动更新。
