# 更新日志

每次发版在最上面加一节 `## <版本> · <日期> · <渠道>`（渠道：`alpha` / `beta` / `rc` / `stable`），面向用户，一行一条。规则见 [docs/release.md](docs/release.md)。

## 未发布

- 新增 Windows 版：引擎运行在 WSL 2，`fabo-win` 在 `\\.\pipe\fabo` 提供 Docker API（没有其他引擎时也服务 `docker_engine`），首次设置可自动安装 WSL、选择 Fabo Linux 或 Ubuntu、装好 docker CLI / Compose / buildx 并设为默认。
- Windows 窗口与托盘：设置向导、引擎启动 / 停止、新版本下载与重启安装，登录信息存入 Windows 凭据管理器。
- Windows 的 `fabo` 命令：docker 子命令、`fabo k8s`、`fabo machine` 等可直接在 PowerShell / cmd 中使用。
- Windows 安装包（MSI，x64 与 Arm64）改为由 `make windows` 与 `scripts/windows-msi.ps1` 构建；尚未在 Windows 上验收，不随本版发布。

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
