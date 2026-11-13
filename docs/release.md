# 版本号与发版

FabriBay 是 Go 项目，版本号不存在源码里，只来自 git 标签 `vX.Y.Z`。

## 版本号从哪来

- `Makefile` 用 `git describe --tags --dirty` 取版本，去掉前缀 `v`，传给构建：
  `-ldflags "-X main.engineRelease=<版本>"` 写进 `kiri-go-daemon`，`scripts/macos-app.sh` 写进 `Info.plist`。
- 两次发版之间的构建自动带开发标记，例如 `0.1.3-2-g1a2b3c4-dirty`：上个标签之后第 2 个提交，工作区有未提交改动。
  不带后缀的版本号一定是从标签构建出来的包。
- 仓库目前还没有标签，这时构建出来的版本是 `0.0.0-dev`。首个版本号由用户定。
- 只认数字的字段（`Info.plist` 的 `CFBundleShortVersionString` / `CFBundleVersion`）在脚本里去掉后缀再写，
  `0.1.3-2-g1a2b3c4` 写成 `0.1.3`。
- 版本号只有标签这一处。安装包文件名、About、`/version` 接口都在构建时读它，不手抄。
- `git remote` 里的 `upstream` 是上游 Kiri。拉取时加 `--no-tags`，上游的 `v0.1.x` 标签进了本仓库会被 `git describe` 当成本项目的版本。

## 发版步骤

1. `CHANGELOG.md` 顶上加 `## <版本> · <日期> · <渠道>`，渠道是 `alpha` / `beta` / `rc` / `stable`。
   更新日志手写，面向用户，一行一条。先按上个标签以来的 `git log` 起草，给用户看过再定稿。
2. 提交 `chore(release): <版本>`。
3. 打注释标签 `v<版本>`，推送分支和标签。**打标签和推送前先问用户。**
4. 从标签构建：`make guest kernel disk app dmg`。公证时传 `KIRI_NOTARY_PROFILE`。

没有「推进到下一个开发版」的提交：标签之后的构建自动变成开发版。日常 push 不动版本。

## 门禁

发版构建前逐条检查，任一不过就停：

- `git describe --tags --dirty` 的输出正好是要发的标签，没有 `-N-g<哈希>` 和 `-dirty` 后缀。
- 标签在主分支上：`git merge-base --is-ancestor <标签> origin/main`。
- 已跟踪的文件没有未提交改动：`git status --porcelain --untracked-files=no` 为空。

## 不可变

- 开发版不打标签，不建 Release。
- 已推送的标签不移动、不重打；已发布的 Release 不改回草稿。发错了就发下一个版本。

## 现状

- 只有 macOS 的 app 和 DMG 能从本仓库构建。更新源（feed）、Linux 包和 Windows 安装器的发版流程还没有迁移到 Go，
  见 [功能对齐基线](compatibility-baseline.md)。
- `docs/releases/` 是上游 Kiri 0.1.x 的发版说明，留作对照。
- `NOTICE` 仍是上游按 Rust 依赖生成的，发版前要按 Go 依赖重新生成。
