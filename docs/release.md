# 版本号与发版

Fabo 是 Go 项目，版本号不存在源码里，只来自 git 标签 `vX.Y.Z`。

## 版本号从哪来

- `Makefile` 用 `git describe --tags --dirty` 取版本，去掉前缀 `v`，传给构建：
  `-ldflags "-X main.engineRelease=<版本>"` 写进 `fabo-daemon`（Docker API 的 `/version`、桌面关于页），
  `-X main.release=<版本>` 写进启动器 `fabo`（`fabo --version`），`scripts/macos-app.sh` 写进 `Info.plist`。
  直接 `go build` 不注入时，`fabo --version` 显示 `0.0.0-dev`。
- 两次发版之间的构建自动带开发标记，例如 `0.1.3-2-g1a2b3c4-dirty`：上个标签之后第 2 个提交，工作区有未提交改动。
  不带后缀的版本号一定是从标签构建出来的包。
- 仓库目前还没有标签，这时构建出来的版本是 `0.0.0-dev`。首个版本号由用户定。
- 只认数字的字段（`Info.plist` 的 `CFBundleShortVersionString` / `CFBundleVersion`）在脚本里去掉后缀再写，
  `0.1.3-2-g1a2b3c4` 写成 `0.1.3`。
- 版本号只有标签这一处。安装包文件名、About、`/version` 接口都在构建时读它，不手抄。
- `git remote` 里的 `upstream` 是本项目的仓库 `OpenFabrica/Fabo`，主分支 `master`。
  不要把上游 Kiri 仓库的标签拉进来：它的 `v0.1.x` 会被 `git describe` 当成本项目的版本。

## 发版步骤

1. `CHANGELOG.md` 顶上加 `## <版本> · <日期> · <渠道>`，渠道是 `alpha` / `beta` / `rc` / `stable`。
   更新日志手写，面向用户，一行一条。先按上个标签以来的 `git log` 起草，给用户看过再定稿。
2. 提交 `chore(release): <版本>`。
3. 打注释标签 `v<版本>`，推送分支和标签。**打标签和推送前先问用户。**
4. 门禁：`make release-check TAG=v<版本>`（`scripts/release-check.sh`），不过就停。
5. 从标签构建：`make guest kernel disk app dmg`。随包的 Go 二进制（`fabo`、`fabo-desktop`、`fabo-daemon`、guest 的 `fabo-init`，以及 `make linux`）都用 `-s -w` 去掉符号表和 DWARF，体积约少三分之一，panic 调用栈仍有函数名和行号。单独调试引擎的 `make daemon` 保留调试信息。
6. 生成内核源码包 `scripts/ci/kernel-source.sh target/Fabo-<版本>-kernel-source.tar.xz`，和 DMG 一起附到 GitHub Release `v<版本>`。
   `guest/kernel/SOURCE_OFFER.md` 承诺了这个下载地址（GPL 要求），不能漏。公证时传 `FABO_NOTARY_PROFILE`。

没有「推进到下一个开发版」的提交：标签之后的构建自动变成开发版。日常 push 不动版本。

## 门禁

`make release-check TAG=v<版本>` 逐条检查，任一不过就停（主分支默认 `upstream/master`，可用 `RELEASE_REMOTE` / `RELEASE_BRANCH` 改）：

- 标签是 `vX.Y.Z` 或 `vX.Y.Z-(alpha|beta|rc).N`，并且是注释标签。
- `CHANGELOG.md` 里有 `## <版本> · ` 这一节。
- `NOTICE` 与 `scripts/notice.py` 的输出一致。

- `git describe --tags --dirty` 的输出正好是要发的标签，没有 `-N-g<哈希>` 和 `-dirty` 后缀。
- 标签在主分支上：`git merge-base --is-ancestor <标签> upstream/master`。
- 已跟踪的文件没有未提交改动：`git status --porcelain --untracked-files=no` 为空。

## 不可变

- 开发版不打标签，不建 Release。
- 已推送的标签不移动、不重打；已发布的 Release 不改回草稿。发错了就发下一个版本。

## 现状

- 只有 macOS 的 app 和 DMG 能从本仓库构建。更新源（feed）、Linux 包和 Windows 安装器的发版流程还没有迁移到 Go，
  见 [功能对齐基线](compatibility-baseline.md)。
- `docs/releases/` 是上游 Kiri 0.1.x 的发版说明，留作对照。
- `NOTICE` 由 `python3 scripts/notice.py` 生成：随包 Go 二进制实际链接的模块与 Go 运行时的许可证、
  `packaging/notice/bundled.txt`（docker CLI、Compose、buildx、Grype、Simple Icons、内核 GPL 全文，手工维护，
  脚本核对版本与 `packaging/cli-pins.env` 和扫描器一致），以及内核源码提供说明。依赖变动后重新生成；门禁会检查它是否最新。
