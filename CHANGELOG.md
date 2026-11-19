# 更新日志

每次发版在最上面加一节 `## <版本> · <日期> · <渠道>`（渠道：`alpha` / `beta` / `rc` / `stable`），面向用户，一行一条。规则见 [docs/release.md](docs/release.md)。

## 未发布

- 「构建」页改为显示 BuildKit 的构建历史：按状态筛选和搜索，查看每一步的耗时、缓存和输出、失败原因与 Dockerfile 行、lint 警告，产出镜像的分层和安全扫描，可删除单条记录或清理所有失败的构建；构建器设置留在设置 → 构建。
- 新增 Windows 版：引擎运行在 WSL 2，`fabo-win` 在 `\\.\pipe\fabo` 提供 Docker API（没有其他引擎时也服务 `docker_engine`），首次设置可自动安装 WSL、选择 Fabo Linux 或 Ubuntu、装好 docker CLI / Compose / buildx 并设为默认。
- Windows 窗口与托盘：设置向导、引擎启动 / 停止、新版本下载与重启安装，登录信息存入 Windows 凭据管理器。
- Windows 的 `fabo` 命令：docker 子命令、`fabo k8s`、`fabo machine` 等可直接在 PowerShell / cmd 中使用。
- Windows 容器域名：设置 → 域名可切到 `*.fabo.local`（需管理员同意一次）。
- Windows 上的本机代理（Clash 等监听 127.0.0.1 的代理）现在会经中继给拉镜像和容器使用。
- Windows 新增 USB 页：经 usbipd-win 把 USB 设备交给容器或还回 Windows，未安装时可一键安装。
- Windows 设置新增「资源」（WSL 的 CPU 与内存，写入 `.wslconfig`）和「诊断」（打开日志文件夹、生成脱敏报告）。
- Windows 设置 → 构建可选普通 `docker build` 用 Docker 还是 Fabo；退出或卸载时还原。
- Windows 安装包（MSI，x64 与 Arm64）改为由 `make windows` 与 `scripts/windows-msi.ps1` 构建；尚未在 Windows 上验收，暂未发布。
- Apple 芯片上的 amd64 容器在 macOS 14 / 15 的 Rosetta 下能正常启动；特权容器里可以用 `losetup` 和 `mount -o loop`。
- Testcontainers（Java、.NET、Mongo 等）、Compose 5.7 和 k3d 能在 Fabo 上运行：引擎报 Docker 27.5.1，`Cmd: []` 使用镜像的命令，exec 不带 Upgrade 头时回 200，`/info` 带 `SystemTime`，名称过滤按正则匹配。
- `docker kill` 等容器真正退出后才返回；`docker images` 按创建时间从新到旧排列；`docker volume ls` 支持过滤。
- 容器里向整个进程组发信号（如 Apache 停止时）不会再影响引擎和其他容器。
- `-u` 非 root 用户或 `--read-only` 容器在工作目录不存在时也能启动；tmpfs 卷和空卷重启后保留原来的属主和权限。
- 用 Nix 构建的镜像（如 Supabase Postgres）能正常拉取。
- 在 Finder 里打开 `/proc/kmsg` 这类文件不会再卡住引擎。
- 状态文件损坏时引擎照常启动，坏文件改名为 `*.corrupt-*` 保留；VM 启动失败时引擎不再反复退出，而是在后台重试。
- shell 配置文件是失效链接或只读（Nix home-manager）时，首次打开 Fabo 不再失败，也不会截断这些文件。
- 安全：导入卷时只用官方 alpine / busybox 镜像做中转；Compose 页只读写本机的 `.yml` / `.yaml` 文件。
- 运行中的 VM 用内存气球限制文件缓存的增长，大量读写文件时占用的 Mac 内存更少；已经用过的内存仍要等 VM 休眠或停止才会归还。

## 0.6.0 · 2026-11-18 · stable

- 新增 macOS 和 Linux 的 USB 页：macOS 上列出本机 USB 设备并一键交给容器或还回，Linux 上列出设备和 `--device` 路径。
- 新增「调试」页：TCP / UDP / WebSocket 的客户端和服务端、HTTP 客户端和 HTTP Mock，支持文本 / 16 进制收发、多种编码、快捷输入、定时发送、自动回复、群发和日志导出；会话自动保存。
- 调试客户端可直接连容器端口，未映射的端口也能经容器网络连上；容器详情里可一键调试某个端口。
- 容器现在可以经 `host.docker.internal` 向本机发 UDP（宿主与虚拟机协议升到 0.6，需同时更新引擎）。
- 侧栏的调试菜单列出会话和连接状态，可在会话之间切换。
- 主窗口底部新增状态条，可查看应用和引擎版本、运行容器数、内存容量和 Docker 当前连接的引擎，也可打开 GitHub 反馈。
- 标题栏左侧移除品牌标识，侧栏导航行距缩小。Linux 虚拟机卡片默认收起，点击可展开详情和电源操作。

## 0.5.2 · 2026-11-16 · stable

- 新增公网共享：`fabo share 端口|容器` 和窗口的「共享」页给本机服务一个公网 HTTPS 地址。目前内测，名额需要申请：给仓库点 Star 并发邮件，收到 token 后登录。
- 容器列表可直接共享运行中的容器；已共享的容器可在列表里复制地址、打开或停止共享。
- 共享页显示每条链接的状态、请求数和流量，链接上线时提示并可复制地址，停止前会确认。
- 共享支持套餐和月流量额度，可查看用量、升级续费；额度恢复后等待中的链接自动重连。
- 虚拟机联网方式可选，默认改为兼容 VPN 的用户态网络。
- 容器域名可在容器的设置里修改。
- 容器列表不再常驻复选框，鼠标移到行上或开始选择后才显示。
- 设置页新增 GitHub 反馈和作者 X 账号入口，关于页新增联系方式。
- 修复：清除容器日志后离开再回来，不再重现旧日志。
- 修复：保留名字因网络失败时，不再误报「名字不可用」。
