# FabriBay 功能对齐基线

记录于 2026-11-11。FabriBay 是独立项目，源码里只有 Go。上游 [libracoder/kiri](https://github.com/libracoder/kiri) 是功能对齐的参考，本仓库的起点是上游提交 `4418009e3f9f8cd234e37531348dd900f78e84e1`。

目标是保留 Kiri 的 Docker API、现有客户端工作流和数据迁移能力，容器引擎与 guest agent 用 Go 实现，MyGo 承担主窗口和托盘。Docker CLI 的交互与流式能力属于引擎兼容范围。上游由 Rust 提供的其余功能按下面的「待对齐功能」逐步补回。架构和构建方式见[开发说明](development.md)和[桌面与引擎说明](desktop-overview.md)。

## Fabo 品牌入口（2026-11-13）

应用显示名和图标已切换为 Fabo，公开命令入口为 `fabo`。下文历史验证记录保留当时的 `kiri` 拼写；现行构建产物、兼容标识和验证方式见[品牌与命令入口](branding.md)。

## 移除 Rust 之后

2026-11-11 删除了全部 Rust 代码（`crates/`、`xtask/`、Cargo 配置）以及只为它服务的 WebView 页面测试。原来由 Rust 承担的启动和构建改由 Go 完成，并在本机验证过：

| 范围 | 现在的实现 | 验证 |
| --- | --- | --- |
| 启动引擎、打开桌面 | `engine/cmd/kiri`：`kiri daemon`、`kiri` / `kiri ui`、`kiri migrate-rust` | Apple Silicon macOS，`Kiri Dev.app`：引擎启动后 `/version` 返回 `"KiriEngine":"go"`，`docker run --rm alpine` 输出正常，Finder 挂载出现；`kiri ui` 在引擎未运行时把它拉起并打开窗口，关窗后引擎继续运行，SIGTERM 后正常退出并卸载。这次用的是已安装 Kiri 0.1.18 的内核，不是本仓库构建的 |
| 打包 initramfs | `engine/cmd/kiri-mkinitramfs` | 与删除前 `cargo xtask guest-go` 的输出逐字节一致 |
| 构建 app | `Makefile`、`scripts/macos-app.sh`、`scripts/macos-dmg.sh` | `make app-dev` 产出可运行的 `Kiri Dev.app`（ad hoc 签名）。`make dmg`、Developer ID 签名和公证没有跑过 |
| Linux 启动 | `engine/cmd/kiri`（`make linux`） | `tests/linux-host.sh`：引擎在特权容器里启动成功，**8 通过、18 失败**。失败有两个原因，都出在用例环境没按 Go 引擎调整：外层容器在 `172.17.0.0/16` 上，与内层默认网桥冲突；启动时没有委派 cgroup，资源限制和特权容器被拒绝 |

### 首次设置（2026-11-11 对齐）

`kiri setup`、`kiri setup --uninstall`、`kiri build-with` 和从 Finder 打开 app 时的首次设置已用 Go 实现（`engine/cmd/kiri/*_darwin.go`），对照上游 `setup.rs`、`builds.rs`、`buildwith.rs`。

| 做什么 | 验证 |
| --- | --- |
| 写 Docker context | `Kiri Dev.app` 实机：`docker context ls` 认出 `kiri-dev`，`docker --context kiri-dev version` 连通 |
| `~/.kiri/bin` 里的 `kiri`、`docker` 链接；正式安装还写 shell profile、`/usr/local/bin/kiri`、默认 context、`cliPluginsExtraDirs` | Dev 版实机验证了链接。正式安装独有的几项只在沙盒 HOME 的单元测试里验证，没有在真实环境跑 |
| LaunchAgent（登录自启） | 实机：`launchctl` 显示 running；引擎正常退出后不重启；再次打开 app 时由 launchd 拉起 |
| Buildx builder 和以 context 命名的副本 | 实机：`docker --context kiri-dev build` 构建成功，镜像直接可 `docker run` |
| 从 Finder 打开时自动设置 | 实机 `open "Kiri Dev.app"`：以上各项都就位，真实的 `~/.docker/config.json` 没有被改动 |
| `--uninstall` | 实机：LaunchAgent、context、builder、链接都移除，引擎停止，已安装的 Kiri 不受影响 |

与上游的三处差异：

- 带 flavor 的 app 不会在没人选择时接管全机的 `docker build`（上游会）。
- 卸载只移除自己加进 `cliPluginsExtraDirs` 的那一项。上游按 `Kiri.app/...` 子串匹配，会连同另一份 Kiri 的条目一起删掉。
- 设置前先确认引擎能启动。有未导入的 Kiri（Rust）数据时直接报错，不改任何东西。

`make kernel` 和 `make disk` 的命令与原 `xtask` 相同，但这次没能跑完：本机 Kiri 虚拟机的时钟比宿主慢约 3 小时，容器里 `apt-get` 校验签名失败。

### Docker 命令透传（2026-11-13 对齐）

`engine/cmd/kiri/docker.go` 对照上游 `docker.rs`：支持 `kiri run`、`ps`、`exec`、`compose`、`buildx` 等 Docker 子命令。优先使用 PATH 上的 Docker CLI，回退到当前 app 内的 CLI；以当前安装的 socket（含 `KIRI_SOCKET` 覆盖）设置 `DOCKER_HOST`，清除 `DOCKER_CONTEXT`。仅当用户未设置 `BUILDX_BUILDER` 且当前安装的 builder 已登记时，选择该 builder。

命令执行前检查引擎是否在线，不自动启动引擎；通过 `exec` 保留标准输入输出、终端和退出码。`import` 仍为迁移命令保留；macOS 的 `update` 为应用更新保留，容器资源更新使用 `kiri container update`。

验证覆盖命令边界、CLI 查找、三种 Buildx 配置路径、显式 builder，以及独立 Unix socket 上的子进程参数、标准输入输出、退出码、SIGTERM 和引擎离线失败。`make check` 通过，实际编译的启动器通过模拟 Docker CLI 和独立 socket 验证命令分发、端点隔离及退出码；Linux 侧仅交叉编译通过。实际 Docker/VM 和交互终端尚未在本轮验证。上游执行前的 CLI/引擎版本差异提醒及 Kiri Lite 下载 CLI 回退尚未对齐，分别随版本管理和 Lite 补齐。

```mermaid
flowchart LR
    A[kiri COMMAND] --> B{Docker 子命令?}
    B -->|是| C[查找 PATH 或 app 内的 Docker CLI]
    C --> D{当前安装 socket 在线?}
    D -->|是| E[绑定 DOCKER_HOST 并选择 builder]
    E --> F[exec Docker CLI]
    D -->|否| G[报告引擎未启动]
    B -->|否| H[已有 Kiri 子命令或用法提示]
```

### Linux machine 命令行（2026-11-13 对齐）

`engine/cmd/kiri/machine.go` 对照本地上游 `crates/kiri/src/machine.rs`（参考 checkout `a649319`），补齐 `create`、`list` / `ls`、`shell`、`start`、`stop`、`restart`、`delete` / `rm`，支持七种发行版、自定义镜像和 `--systemd`。沿用 `machine-NAME` 容器名与 `dev.kiri.machine` 标签、宿主用户映射、共享 home、免密 sudo、重启策略和 systemd 配置。完整规则和流程见 [machines.md](machines.md)。

与上游的明确差异：参数缺值、未知参数和多余参数直接报错，避免静默使用默认值；镜像 digest 原样传递；重复启动/停止的 HTTP `304` 视为成功，匹配现有端到端用例。宿主用户名进入初始化脚本时按 shell 参数引用。shell 使用进程替换，保留 Docker 的终端、信号和退出码。

验证：`make check` 通过（engine / desktop vet 与测试、engine race、Linux ARM64 交叉编译）。独立 macOS Go VM 中成功创建 Alpine machine，后续 **22 项检查全部通过**，覆盖宿主用户名及 UID/GID、共享文件、工作目录、HOME、hostname、stdin、非零退出码、sudo、真实 TTY、重复启停、重启、删除与错误参数。使用当前源码构建的启动器、daemon、guest agent；内核与空数据盘模板取自已安装 Kiri，只读使用。未连接日常 Kiri 的 socket，也未注册 context 或 LaunchAgent。

Ubuntu `--systemd` 实机也已验证：首次软件包安装约 4 分钟（最初的 120 秒就绪检查未通过，安装完成后复验通过），随后 `systemctl is-system-running` 返回 `running`，PID 1 为 systemd；默认 login shell 与退出码检查通过，另外 **8 项检查全部通过**，包括 sudo、停止、再次启动和删除。测试容器与专用 VM 数据已清理，日志保留在 `target/cli-machine-check/`。

Linux 宿主和其余发行版的真实运行、完整 `tests/e2e.sh` 尚未复验。

### Kubernetes、import、USB、bench、raw（2026-11-13）

五组 CLI 已用 Go 实现，对照本地上游 `a649319`；公开入口为 `fabo`。`k8s` 管理 k3s 节点、设置、CoreDNS 和 kubeconfig；`import` 从指定 Docker context 复制镜像、卷和停止的容器；`usb` 提供 macOS 设备枚举、特权 helper、USB/IP 和软件测试设备，以及 Linux 设备指引；`bench` 测量 API / CLI 的完整容器生命周期；`raw` 独占独立存储并直接管理 guest。

`make check`、当前源码构建通过。隔离 macOS VM 验证了 k3s Ready、TLS API、重建保留 ConfigMap、启停和 kubeconfig 清理；两套独立引擎验证迁入文件所有者/权限、网络、停止状态及源数据保留；USB 软件设备 **41 通过、0 失败**；raw 退出码 37、1 MiB 输出与三种基准路径已实测。详细契约和限制见 [CLI 工具说明](cli-tools.md)。

剩余差异：kubeconfig 暂在 start/status 同步，未接 daemon 启动后台同步；raw 使用独立 Go 存储，镜像列表格式与 Rust 不同。物理 USB 捕获/helper 安装、真实 OrbStack/Docker Desktop 迁移、Linux 宿主尚未实测。未运行全量端到端套件。

### 桌面高级功能（2026-11-14）

对照本地 Kiri `a649319`，MyGo 新增三组操作：容器详情中的环境变量和 TCP / UDP 端口编辑；设置中的仓库登录、退出及凭据拉取；Kubernetes Pods / Services、Pod 日志和删除、集群启停与网络设置。详细规则、接口和失败语义见 [桌面高级操作](desktop-advanced.md)。原有文件浏览、镜像分层与扫描、代理及域名设置继续保留。

验证：`make check`、desktop race（含专用 Keychain 创建 / 更新 / 删除测试）、深浅主题渲染通过；Linux ARM64 与 Windows AMD64 桌面交叉编译通过，仅 macOS 实际运行。当前源码构建的原生窗口、daemon 和 guest agent 配合隔离 VM，**12 项验收通过**，覆盖环境保存后容器 ID / 卷文件保留、即时端口修改及非法输入、真实本地 Registry 鉴权与 helper 存取、k3s 启动、Pod / Service / 日志、删除确认、网络重建后 ConfigMap 保留及集群清理。Pod 尚在 ContainerCreating 时，日志显示原因，运行后手动刷新读取成功。内核复用现有 Dev 包，与本轮 Advanced 包的内核哈希相同。

原生窗口截图及逐项结果保留在 `target/desktop-advanced-check/`。专用 VM、临时 HOME / Docker 配置 / kubeconfig、测试容器和数据卷均已清理，日常 Dev 引擎未重启。`target/dev/Fabo Advanced.app` 已构建并通过 ad hoc 签名检查，未安装或发布。

剩余边界：Pod 日志为尾部 400 行的手动刷新；交互终端仍逐条执行；Linux 引导、多语言、Kubernetes forwards、Import / Diagnostics / Build with、USB / Share 界面尚未补齐。仓库 token 与私有镜像鉴权头由接口测试覆盖，未使用真实私人仓库账号；Linux / Windows 的真实桌面和全量 `tests/e2e.sh` 未在本轮验证。

### 待对齐功能

下面列出尚未补齐的范围。上游对应实现在 `crates/` 下。

| 模块 | 缺什么 | 上游位置 |
| --- | --- | --- |
| 需要管理员密码的设置 | `kiri setup --docker-socket`（把 `/var/run/docker.sock` 链到本引擎，并装开机恢复它的 LaunchDaemon），卸载时也不会清理它。`*.kiri.local` 和 HTTPS 证书信任已对齐（`kiri domains`、`kiri setup --domains`、设置 → 域名），但开启本地域名时不像上游那样用同一次密码顺带链接 `/var/run/docker.sock`；密码框由 `osascript` 和 `security` 弹出，标题不是 Kiri | `crates/kiri/src/setup.rs` 的 `docker_socket` |
| 桌面高级功能 | Linux 首次引导、连续交互式内嵌终端、Pod 连续日志与 Kubernetes forwards、14 种语言、Import / Diagnostics / Build with 桌面入口，以及 USB / Share 页面仍待补齐。文件浏览、镜像分层与扫描、代理与域名、容器环境与端口设置、仓库登录、Kubernetes 管理已有 MyGo 实现；具体验证见下文及 [桌面高级操作](desktop-advanced.md) | `crates/kiri/src/ui/` |
| 自动更新与诊断 | 更新源检查与安装、崩溃报告和日志脱敏 | `crates/kiri/src/update.rs`、`diag/` |
| Windows | `kiri-win` 宿主（named pipe、托盘、设置、更新）、WSL 里的 `kiri bridge`、MSI。`installer/windows`、`contrib/windows`、`contrib/wsl`、`scripts/kiri-linux.sh` 仍按 Rust 二进制编写 | `crates/kiri-win/`、`crates/kiri/src/bridge.rs` |
| Linux 包 | `packaging/linux`、`scripts/ci/linux-build.sh`、`scripts/ci/linux-packages.sh`、`contrib/linux` 仍要求 Rust 的 `kiri`、`kiri-init` 和 GTK 窗口；`kiri host-access` 没有 Go 版本 | `packaging/linux`、`crates/kiri/src/hostaccess.rs` |
| 发版 | 更新源 JSON、发版 tarball、Kiri Lite；`NOTICE` 要按 Go 依赖重新生成 | 上游 `xtask/src/main.rs` 的 `release` |
| 测试 | `tests/e2e.sh` 全量复验（machine、Kubernetes CLI 与 USB 已通过独立专项）、`tests/finder-mount.sh`、`tests/wsl.sh`、`tests/linux-host.sh` 的环境。协议交叉校验（Rust 编解码器对 Go）已随 Rust 删除，现在只有 Go 对 Go | `tests/` |
| CI | 没有跑 Go 测试的工作流，只剩 CodeQL | `.github/workflows` |

下面各节是删除 Rust 之前的引擎回归记录。引擎代码此后只改过两处注释。

## 验证快照

| 范围 | 环境 | 结果 |
| --- | --- | --- |
| 上游默认端到端用例 `tests/e2e.sh` | Apple Silicon macOS，隔离的 `kiri-parity` 实例，存储修复前 | **496 通过、17 失败、6 跳过** |
| 原版网络用例选集 | Apple Silicon macOS | **105 通过、1 失败**。剩余用例是 `default-bridge-no-names`，同环境的 Rust 原版也失败 |
| 原版 Finder 用例 | Apple Silicon macOS | **14 通过、0 失败**。额外验证 1,800 项目录分页及退出后的卸载 |
| 原版存储和绑定挂载用例 | Apple Silicon macOS，存储修复后 | **28 通过、0 失败**。额外验证 Go 重启和 Rust 离线迁移后的匿名卷清理 |
| 存储、运行时和迁移专项 | Debian ARM64 | 验证嵌套卷初始化、匿名卷清理、tmpfs 和 bind 退出码。另有容器网络、日志、exec、服务生命周期和 Rust 数据迁移记录 |
| Kubernetes 专项 | Apple Silicon macOS，Linux ARM64 | macOS 验证 k3s 启停与恢复、应用部署、TCP/UDP LoadBalancer、Pod 与 ClusterIP 转发。Linux ARM64 验证基础能力与转发 |
| USB 专项 | Apple Silicon macOS，软件 USB 设备 | **42 通过、0 失败**。覆盖软件串口和 HID |
| Windows WSL | Linux 上的 WSL 启动脚本验证环境 | Alpine 和 systemd 启停验证通过。真实 Windows 上的安装、更新、named pipe 和代理仍需整机回归 |

各专项覆盖全量用例的一部分，计数有重叠。存储修复之后尚未重跑全量套件，保留原全量结果作为历史基线。

## 已实现范围

| 模块 | 对齐内容 |
| --- | --- |
| 容器与资源 | 生命周期、日志和 attach、exec、健康检查、重启策略、事件、统计、资源更新、暂停与恢复。具备 capabilities、seccomp、只读根、特权模式和设备映射 |
| 网络 | IPv4/IPv6 桥接和 IPAM、静态 IP/MAC、端口发布、动态网络连接、共享和 host 网络、别名 DNS、`--domainname`、`--link`、Mac 宿主服务访问。端口发布支持 TCP/UDP |
| 存储 | 命名和匿名卷、嵌套 bind 下的镜像内容初始化、tmpfs、双向 `docker cp`、`diff`、`export`、`commit`。旧 Go 记录缺少匿名卷归属时保留卷数据 |
| 镜像与构建 | 拉取、标签、归档导入导出、历史、清理、仓库登录和推送、Compose、BuildKit/Buildx、Apple Silicon 上的 Rosetta |
| macOS 与桌面 | Go VM 启停、空闲休眠、资源设置，Finder 只读浏览，MyGo 主窗口（深浅主题、中英文、可折叠侧栏；概览、容器、镜像、卷、网络、构建器、设置；详情坞含日志、逐条执行的终端、文件浏览、环境变量与端口设置、镜像分层与安全扫描；仓库登录与凭据拉取；Kubernetes Pods / Services、Pod 日志与集群管理）、托盘 |
| 安装与数据 | macOS 和 Linux 已验证 Rust 离线导入。Windows WSL 的引擎一侧已实现，平台验证范围见上表 |

## 最近一次全量回归的失败项

| 状态 | 用例 | 记录 |
| --- | --- | --- |
| 修复后专项通过 | `bind-mount-missing`、`vol-copyup-under-bind`、`tmpfs-mount`、`rm-v-anon` | 缺失 bind 源返回退出码 125，空卷从镜像填充，tmpfs 显示与原版一致，匿名卷按归属清理 |
| 同环境 Rust 也失败 | `default-bridge-no-names` | 上游 DNS 对短名称返回了地址 |
| 待修复 | `killed-code` | `docker kill` 后测试得到 0，原版用例期望 137 |
| 待修复或复验 | `domain-ports-same`、`domain-ports-taken`、`domain-ports-unchanged`、`domain-ports-yield` | 涉及冲突提示、设置回滚和端口让渡 |
| 待修复 | `filter-images-ref`、`filter-volume-label-only` | 镜像引用筛选和卷标签筛选 |
| 待复验 | `disconnect-dns-gone` | 断开网络后，客户端报错与用例期望不同。待使用受控上游 DNS 对照 Rust |
| 待实现 | `search`、`search-official` | Go API 的 `/images/search` 返回 404 |
| 修复后专项通过 | `machine-mac-files`、`machine-cwd` | 2026-11-13 使用 Go machine CLI，在真实 `/Users/mike` 下建立临时共享目录，验证文件访问及工作目录一致；原全量套件尚未重跑 |

另有 6 项在隔离实例中跳过，分别是 `domain-http`、`domain-unknown`、`domain-port-yield`、`domain-port-back`、`domain-ports-old-free` 和 `compose-domain`。这些用例涉及标准实例的域名与默认监听端口。

## 复验方式

从仓库根运行快速检查。

```sh
go -C engine test -race ./...
go -C engine vet ./...
go -C desktop test ./...
GOOS=linux GOARCH=arm64 go -C engine test -exec true ./...
```

`make check` 跑的就是这一组。最后一条仅交叉编译 Linux 测试，Linux 行为需要在真实 Linux 环境执行。构建需要 Go 1.27.3 或更新版本。

macOS 全量回归使用与日常实例隔离的 App、HOME、Docker 配置、Docker context、socket 和 VM 数据盘。历史记录使用 App 标识 `dev.kiri.app.parity`、Docker context `kiri-parity`、Buildx builder `kiri-parity-build` 和 `~/Kiri Parity`。先在被测 socket 的 `/version` 中确认 `"KiriEngine":"go"`。将 `DOCKER_CONFIG` 指向隔离 HOME 下的 `.docker`，确保 Registry 用例也能找到隔离的 context。运行域名端口用例时清除 `KIRI_HTTP_PORT` 和 `KIRI_HTTPS_PORT` 覆盖。

```sh
DOCKER_CONTEXT=kiri-parity bash tests/e2e.sh
```

发布前还需在对应平台运行 `tests/linux-host.sh`、`tests/wsl.sh` 和 `tests/kernel-features.sh`，并补齐迁移、Kubernetes、USB、默认域名及实际桌面交互的验证。验收记录按平台列出测试命令、结果、跳过原因和仍存在的差异。当前全量基线尚未达到可直接替换的验收条件。
