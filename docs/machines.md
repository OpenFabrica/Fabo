# Linux machine 命令

`fabo machine` 提供持久运行的 Linux 开发环境。machine 使用普通 Docker 容器实现，宿主用户可在相同路径访问 home 下的文件。Go 入口位于 `engine/cmd/fabo/machine.go`，行为对照本机 Kiri 源码的 `crates/kiri/src/machine.rs`。

```sh
fabo machine create dev --distro alpine
fabo machine list
fabo machine shell dev
fabo machine shell dev -- sh -c 'id; pwd'
fabo machine stop dev
fabo machine start dev
fabo machine restart dev
fabo machine delete dev
```

## 核心规则

- 名称使用 ASCII 字母、数字和连字符，以字母或数字开头。容器名是 `machine-NAME`，标签为 `dev.fabo.machine=NAME`。`list` 仅列出带该标签的容器，包含停止状态；`ls` 和 `rm` 分别是 `list` 和 `delete` 的别名。
- 默认镜像为 `ubuntu:24.04`。`--distro` 支持 `ubuntu`、`debian`、`alpine`、`fedora`、`arch`、`rocky`、`opensuse`；`--image IMAGE` 支持自定义镜像，包括带 registry 端口或 digest 的引用。沿用上游行为，发行版简称在两个选项中均可使用，两个选项均可接受自定义镜像。
- 创建时拉取镜像，写入容器配置并启动，然后最多等待约 30 秒确认宿主用户已建立。HTTP 错误、拉取流中的错误和用户初始化失败都返回非零退出码。启动或初始化失败时保留容器，供查看日志或删除，不自动销毁其数据。
- 容器中使用宿主的 `USER`、UID、GID；UID 为 0 时使用 `root`。初始化为其配置免密 sudo；精简镜像缺少 sudo 时在后台安装，因此刚创建成功时 sudo 可能尚未就绪。自定义镜像需要支持相应的 shell、账户及包管理命令。
- macOS 绑定 `/Users:/Users`，Linux 绑定 `/home:/home`。shell 在宿主工作目录位于共享目录内时沿用该目录，否则进入 `/Users/USER`、`/home/USER` 或 `/root`。这与宿主 `HOME` 环境变量的任意覆盖不同。
- shell 复用 Docker CLI；有终端时使用 `exec -it`，管道输入使用 `exec -i`。默认启动 bash 或 sh 的 login shell；`--` 后的参数原样传入。标准输入输出、信号和退出码由现有 Docker 透传路径保留。
- 所有操作使用当前安装的 socket，支持 `FABO_SOCKET`。shell 设置 `DOCKER_HOST` 并清除 `DOCKER_CONTEXT`，避免误连其他实例。引擎离线时报错，不自动启动或修改 Docker context。
- 停止保留可写层；启动和停止对 Docker 的 `304` 返回成功。删除强制移除容器及其匿名卷；共享的宿主文件不会删除。
- `--systemd` 启用 privileged、`/run`、`/run/lock`、`/tmp` tmpfs、`container=docker` 和 `SIGRTMIN+3` 停止信号；镜像缺少 systemd 时先安装。创建成功仅表示容器与用户已就绪，systemd 服务初始化可能仍在继续。

```mermaid
flowchart TD
    A[fabo machine] --> B[校验参数并检查实例 socket]
    B --> C{操作}
    C -->|create| D[拉取镜像并检查流中错误]
    D --> E[创建 machine 容器并启动]
    E --> F[等待宿主用户就绪]
    C -->|shell| G[计算用户与共享工作目录]
    G --> H[绑定实例 socket 并 exec Docker CLI]
    C -->|list / start / stop / restart / delete| I[调用 Docker API 并检查状态码]
```

## 桌面端

侧栏的"Linux 机器"页单独列出 machine，它们不再出现在容器列表里（调试会话、打包和导入时用的临时 helper 容器也不出现）。每行显示发行版图标、发行版、状态和创建时间；行上的按钮打开终端、启动或停止，"更多"菜单里有重启、复制终端命令和删除。详情面板显示镜像、用户、共享目录和终端命令。

"新建机器"对话框选择名称、发行版和是否运行 systemd，交给 `fabo machine create`。删除前会确认，并说明共享目录下的文件留在宿主上。启动、停止和删除直接调用引擎，和 CLI 等价。

终端按钮打开这台机器自己的终端页：页头是返回箭头、机器名和发行版，下面每个 shell 一个标签（终端 1、终端 2…），+ 按钮再开一个 `fabo machine shell NAME`。关闭一个标签就结束对应的 shell，在 shell 里输入 `exit` 也一样；最后一个关掉后，页面回到机器列表。点返回箭头回到列表时 shell 保持打开，再点这台机器的终端按钮会回到原来的标签。机器停止或删除后，它的 shell 会全部关闭。

有终端时，`fabo machine shell` 传 `TERM=xterm-256color`，并在机器里写入 `/etc/profile.d/fabo-color.sh`：交互登录 shell 用彩色提示符（绿色的 `用户@主机`、蓝色的路径），`ls`、`grep` 带颜色，另有 `ll`。各发行版的登录 shell 都会在自己的提示符之后读取这个文件，所以以前创建的机器也会生效；用户自己 `~/.profile` 里的设置仍然优先。

终端背景默认黑色，在"设置 › 外观 › 终端背景"里可以改成石墨、墨蓝、白色，或跟随窗口外观。这个设置作用于容器、Linux 机器和宿主终端，已经打开的终端会立即换色。

daemon 需要共享 `/Users`（macOS）或 `/home`（Linux）才能创建 machine。`fabo` 启动 daemon 时传 `-standard-shares`；手动运行 `fabo-daemon` 做测试时也要带上它，否则创建会报 `VM bind mounts require a configured -share directory`。

## 验证

```sh
go -C engine test -race ./cmd/fabo
make check
```

自动测试覆盖发行版与参数、镜像引用、HTTP 请求和错误、拉取流失败、创建中断、用户就绪、systemd 配置、列表过滤、重复启停、删除失败、shell 工作目录、子进程标准输入输出和退出码，以及超时与非法响应。

实机验证使用独立的 Go 引擎 socket、状态目录、数据盘和 Docker 配置；不能对日常使用的 Fabo 实例运行这些生命周期测试。当前验证结果和平台限制见[功能对齐基线](compatibility-baseline.md)。
