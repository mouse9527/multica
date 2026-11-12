# fork 的发布与 runner 自动更新

本文件属于 `mouse9527/multica` fork（上游没有）。说明 fork 怎么发布 runner 用的 `multica` 二进制包、镜像怎么和它对齐，以及 `multica update` 和 daemon 怎么从 fork 的 Release 更新。

## 版本号

- tag 写成 `vX.Y.Z-fork.N`：`X.Y.Z` 是 fork 当前合入的上游版本（`git describe --tags --abbrev=0` 在合入了上游 tag 的 `main` 上得到的值），`N` 是在这个上游版本上的第几次 fork 发布，从 1 开始。例：fork 的 `main` 基于上游 v0.8.1，第一次发布是 `v0.8.1-fork.1`。
- 同步到新的上游版本后，`N` 重新从 1 开始：`v0.8.2-fork.1`。
- 不会和上游 tag 撞名（上游没有 `-fork.` 后缀）；fork 不推送上游的 tag。
- 新旧比较：先比 `X.Y.Z`，再比 `N`；没有后缀的上游版本视为 `N = 0`。所以 `v0.8.1 < v0.8.1-fork.1 < v0.8.1-fork.2 < v0.8.2-fork.1`。
- `multica --version` 显示去掉 `v` 的版本（`0.8.1-fork.1`）；镜像里 server 报告的 `server_version` 是 tag 原样（`v0.8.1-fork.1`），两者比较时忽略 `v`。
- `git describe` 风格的开发版本（`v0.8.1-fork.1-3-gabc1234`）、`dev`、上游的 `-rc` 之类都不算 release：自动更新对它们不生效。

## 发布一个版本

1. 确认要发布的提交已经在 `main` 上，`main` 的 CI 是绿的。
2. 打 tag 并推送（第一次发布要 Owner 对具体 HEAD 批准）：

   ```bash
   git tag -a v0.8.1-fork.1 <commit> -m "v0.8.1-fork.1"
   git push origin v0.8.1-fork.1
   ```

3. 推送后两个工作流同时运行：
   - `.github/workflows/release-fork.yml`：检查 tag 格式和提交在 `main` 上，跑更新相关测试和 `govulncheck`，再用 `.goreleaser.fork.yml` 构建并发布 Release。产物：`multica-cli-<版本>-{darwin,linux,windows}-{amd64,arm64}.{tar.gz,zip}` 和 `checksums.txt`；每个压缩包带 `LICENSE`、`NOTICE`、`README*` 和修改清单 `docs/github-issues-upstream-changes.md`。最后一步核对 4 个必需平台的包和 `checksums.txt` 都在。
   - `.github/workflows/build-image.yml`：为同一个 tag 构建 `ghcr.io/mouse9527/multica-backend:<tag>` 和 `multica-web:<tag>`（不移动 `latest`）。
4. Release 不标成 pre-release，并设为 latest，因为 runner 读的是 `/releases/latest`。

同一 tag 重跑会替换同名产物。要撤回一个坏版本：在 GitHub 上把 Release 删掉或改回草稿，`/releases/latest` 就回到上一个版本；已经更新过的 runner 需要手动装回旧版本（见下文）。

### 上游发布流程里关掉的部分

上游的 `release.yml` 在 fork 上整体不运行（`verify` 加了 `github.repository_owner == 'multica-ai'`，其余 job 都依赖它）：

| 上游步骤 | 为什么关掉 |
|---|---|
| GoReleaser 的 Homebrew tap（`brews`） | 要 `HOMEBREW_TAP_GITHUB_TOKEN`，写的是 `multica-ai/homebrew-tap` |
| 多架构镜像（`docker-*`） | 和 `build-image.yml` 推同一组 GHCR 标签会互相覆盖；fork 用 `build-image.yml` |
| Helm chart | 上游本来就只在 `multica-ai` 运行 |
| Desktop 安装包 | 不在本 fork 的范围 |
| 旧压缩包名 `multica_{os}_{arch}` | 只给上游老版本 CLI 兼容用，fork 的 CLI 先找新名字 |

## runner 怎么更新

### 更新源

`multica update` 和 daemon 默认读 `mouse9527/multica` 的 Release，下载前先取 `checksums.txt`，SHA-256 不一致就不解压、不替换。相关环境变量：

| 变量 | 作用 | 默认 |
|---|---|---|
| `MULTICA_RELEASE_REPO` | 读哪个仓库的 Release，格式 `owner/name` | `mouse9527/multica` |
| `MULTICA_RELEASE_API_BASE_URL` | GitHub API 兼容的镜像地址（上游已有） | `https://api.github.com` |
| `MULTICA_RELEASE_DOWNLOAD_BASE_URL` | 产物镜像地址，按 `/<tag>/<文件名>` 取（上游已有） | Release 自带的下载地址 |

用 Homebrew 装的 `multica` 跟的是上游 tap：更新源不是上游时，`multica update` 和 daemon 会报错并提示改用 Release 里的二进制，不会去跑 `brew upgrade`。

### daemon 自动更新

| 变量 / 参数 | 作用 | 默认 |
|---|---|---|
| `MULTICA_DAEMON_AUTO_UPDATE` | 开关；`--no-auto-update` 或 `config.json` 的 `disable_auto_update` 只能关 | 自建 server 上关（和上游一致），设 `true` 打开 |
| `MULTICA_DAEMON_AUTO_UPDATE_TARGET` | `server`：对齐到 server 报告的版本；`latest`：跟最新 Release，只升不降 | `server` |
| `MULTICA_DAEMON_AUTO_UPDATE_INTERVAL` | 检查间隔 | `6h`，启动后 2 分钟先查一次 |

`server` 模式：daemon 读 `GET /api/config` 的 `server_version`。它是 release 版本且和自己不同，就安装同名 tag 的 Release（升或降都会做，目的是和 server 跑同一个构建）。server 跑的是 `main` 的镜像（版本是 `dev`，`server_version` 为空）时什么都不做。

不论哪种模式：

- 有任务在跑或正在认领时推迟到下一次检查，不打断任务。
- 下载失败、校验和不一致、Release 不存在时保留旧二进制，继续接任务，下一次检查再试。
- 只有安装成功后才重启进新版本。
- 本身是开发构建（`make daemon` 之类）的 daemon 不自动更新。
- Desktop 管理的 daemon 不自动更新（上游行为）。

要让通过开机自启动的 daemon 也打开自动更新，需要把 `MULTICA_DAEMON_AUTO_UPDATE=true` 写进它的启动环境（launchd plist 或 systemd unit）。

### 第一次装 fork 的二进制

现在跑上游二进制的 runner 只会从上游更新，要手动装一次 fork 版本：

```bash
TAG=v0.8.1-fork.1; VER=${TAG#v}
OS=$(uname -s | tr A-Z a-z); ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
F=multica-cli-$VER-$OS-$ARCH.tar.gz
curl -fLO https://github.com/mouse9527/multica/releases/download/$TAG/$F
curl -fLO https://github.com/mouse9527/multica/releases/download/$TAG/checksums.txt
grep " $F\$" checksums.txt | shasum -a 256 -c -
tar -xzf "$F" multica && install -m 755 multica "$(dirname "$(command -v multica)")/multica"
multica --version
```

Homebrew 装的需要先 `brew uninstall multica`，再把二进制放到 `PATH` 里的目录。daemon 会在空闲时自己重启进新二进制（auto-reload），或者手动 `multica daemon restart`。装回旧版本也用同样的步骤换成旧 tag。

## 不做的

- 不发布到 Homebrew、npm，不做桌面端安装包。
- Web 端运行时页面的「有新版本」提示仍读上游的最新 Release；fork 版本号带后缀，它不会提示升级，不影响 daemon 的自动更新。
