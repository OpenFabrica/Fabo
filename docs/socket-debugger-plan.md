# Socket 调试器实现方案

设计稿：`designs/fabo-socket-debugger/index.html`（截图在同目录 `shots/`）。
范围：TCP / UDP / WebSocket 的客户端与服务端、HTTP 客户端、HTTP Mock；不做二维码。会话配置持久化，服务端「启动时自动监听」默认关闭。

## 0. 已确认的事实（方案的前提）

| 事实 | 证据 |
|---|---|
| 本机 / 局域网会话不需要引擎，桌面进程直接 `net.Dial` / `net.Listen` | — |
| 引擎连容器内部地址，TCP 与 UDP 都已有 | `guestwire/relay.go`：`RelayFramedTCPPort=1030`、`RelayUDPPort=1029`；`localengine/port_relay.go:291 dialPublishedPort(ctx, relay, network, relayPort, dst)`；Linux 上 `relay == nil` 直接拨号 |
| 容器 → 宿主（`host.docker.internal`）只走 TCP | `fabo-init/hostbound_linux.go:20` 只监听 TCP；`guestagent/forward_linux.go` 桥接网络上 UDP 只重定向 53 端口给 DNS，其余只重定向 TCP |
| 宿主中转只带目标端口，不带来源地址 | `hostbound_linux.go` `forwardHost` 写 `127.0.0.1:port`；`fabo-daemon/hostbound_darwin.go forward` 只读目标后连回环 |
| UDP 拿不到 `SO_ORIGINAL_DST` | 内核 `getorigdst` 只支持 TCP / SCTP，所以 UDP 不能照搬 TCP 的 REDIRECT 做法 |
| guest 内核已有 TPROXY 与 socket 匹配 | `guest/kernel/fabo.config`：`CONFIG_NFT_TPROXY`、`CONFIG_NF_TPROXY_IPV4/6`、`CONFIG_NFT_SOCKET`、`CONFIG_IP_MULTIPLE_TABLES`（x86_64 版由它派生） |
| guest 与 host 版本必须完全一致 | `guestwire/wire.go:147` Hello 校验 `h.Version != Version` 即拒绝 |
| 中转头部已有「全零头部 + 扩展」的先例 | `relay.go:99` IPv6 扩展头 |
| 桌面端控件都有 | MyGo：`TextArea`、`List`+`FollowEnd`、`Split`、`Table`、`Tabs`、`Checkbox`、`Switch`、`Select`、`PopoverMenuButton`、`DialogBase` |
| 两个 `go.mod` 都没有 WebSocket 库 | 需新增依赖 |
| 非 GET 请求会先唤醒虚拟机，处理期间计入 `inFlight`，`inFlight` 不为零时虚拟机不休眠 | `fabo-daemon/supervisor.go` 的 `requiresGuest`、`ServeHTTP` 的 wake 分支、`sleepIfIdle` |
| `/fabo/share` 在唤醒逻辑之前处理，**不适合**照搬它的挂载位置 | `supervisor.go:399-409` |
| 上游 Kiri 没有宿主方向的 UDP 中转，0.6 的新头部是本项目自定义 | 在 `/Users/mike/source/project/tmp/kiri/crates` 里检索 tproxy / UDP，未发现相关实现 |
| 容器详情「信息」页已有「端口」一行，可以挂调试入口 | `TestScreenshots` 的 `containers-tab0` |

## 1. 功能边界

调试器是独立工具，容器只是它的目标来源，它不管理容器。

- **桌面端**负责会话配置、消息、界面，以及本机 / 局域网的全部 socket。引擎不保存任何调试状态。
- **引擎**只新增两个通用能力：
  1. 连接容器的某个端口（TCP / UDP）；
  2. 查询「宿主中转连接是哪个容器发起的」。

  两者都不知道「调试会话」的存在。
- **会话引用容器**用「容器名 + 端口 + 协议」，每次连接时再解析 IP。容器停止或删除后会话保留并显示原因。调试器不会启动容器，也不会映射端口。
- **容器页**只提供端口旁的调试入口，不显示调试会话。
- **分工**：终端、日志看容器内部；调试器从外部模拟对端；共享把服务公开到公网；HTTP Mock 只在本机。

## 2. 架构

```
桌面端 debug_*.go ──本机/局域网── net.Dial / net.Listen
      │
      ├─ 容器网络直连 ── POST /fabo/debug/dial (Upgrade: tcp) ── dialPublishedPort ── vsock 1030 / 1029 ── 容器
      │
      └─ 来源识别 ──── GET /fabo/debug/peers ── 宿主中转登记表

容器 ── host.docker.internal:port ── guest 中转（TCP: REDIRECT；UDP: TPROXY）── vsock 1026（0.6 新头部：协议 + 来源 + 端口）
     ── fabo-daemon hostBound ── 127.0.0.1:port（登记 回环本地端口 → 来源容器）── 桌面端监听
```

## 3. 分期

### M1 桌面端本机会话（不改引擎）

新文件放在 `desktop/`：

- `debug.go`：页面、会话列表、各类工作区、新建会话对话框。新增导航项 `debugPage`，放在「共享」之后（`shell.go` 的 `navItems`）。
- `debug_session.go`：会话模型与持久化。
- `debug_net.go`：socket 工作者，包括 TCP / UDP / WS 客户端与服务端、HTTP 客户端、HTTP Mock。
- `debug_test.go`：Tester 测试，并把新页面加进 `TestScreenshots`。

要点：

- **并发模型**：每个活动会话一个 goroutine 组，负责读、写和定时发送。收到的消息先放进带锁的环形缓冲（每个会话上限 5000 行，超出丢最旧的），再 `win.Update` 触发重绘。视图只读快照，不在主线程做网络 IO。停止或删除会话时用 `context` 取消，并等工作者退出，退出后才允许以同一端口重新监听。
- **日志视图**：`ui.List` + `ListState.FollowEnd`；16 进制视图在渲染时转换，不额外存一份。
- **输入**：文本模式解析 `\r\n\t` 转义；16 进制模式校验后转字节，出错时内联提示。⌘↩ 用 `OnTextKey` 拦截。
- **定时发送**：`time.Ticker`，间隔下限 100ms；断开或切换会话时停止。
- **WebSocket**：新增依赖 `github.com/coder/websocket`。它基于 context，客户端和服务端都支持，没有间接依赖。
- **wss**：在桌面配置目录生成一份自签名证书（首次用到时生成）。不复用域名 HTTPS 的证书，因为那份证书归启动器管理，并且涉及钥匙串与管理员授权。
- **HTTP 客户端**：用 `net/http`，有历史记录（每个会话保留 50 条）。`*.fabo.local` 直接可用。
- **HTTP Mock**：路由按「方法 + 路径」匹配，支持 `:param` 和 `*`。模板只支持 `{{path.x}}`、`{{uuid}}`、`{{rand}}`。还支持延迟和静态目录。请求记录保留 200 条。
- **服务端监听范围**：每个会话一个字段，可选「仅本机」（默认）或「所有网卡」（让局域网设备也能连，与原版 Socket Debugger 一致）。
  - macOS 选「仅本机」时绑定 `127.0.0.1`，容器经宿主中转仍能连进来，因为中转连的就是回环。
  - Linux 选「仅本机」时绑定 `127.0.0.1` 加上各桥接网络的网关地址（从 Docker API 的网络列表取），容器能连进来，又不暴露到局域网。
  - 选「所有网卡」时绑定 `0.0.0.0`，界面上同时显示局域网地址。
- **持久化**：`debug-sessions.json` 放在 `prefsFile` 同目录。
  - 内容是带版本号的会话配置、快捷输入、Mock 路由和 `autoListen`，不保存消息。
  - 用临时文件加 rename 原子写入。
  - 与安装包共用配置的开发窗口会读写同一份文件，按「后写入者生效」处理。
- **启动时的行为**：只恢复配置。`autoListen` 为真的服务端尝试监听一次，端口被占用就把会话标成错误，不重试。
- **与原版 Socket Debugger 对齐的细项**（都在 M1 内）：
  - 服务端「初始消息」：客户端刚连上时自动发送；
  - 自定义 TLS 证书和私钥（PEM），不填时用自签名证书；
  - 编码：UTF-8、GBK、GB18030、Big5、Shift_JIS、ISO-8859-1，用 `golang.org/x/text/encoding`；
  - 「不处理转义字符」开关，按会话保存；
  - 会话备注；
  - 日志导出为文本文件；
  - HTTP Mock 的「在浏览器中打开」。
- **不做**：二维码工具；原版的内购与解锁。
- **文案**一律 `tr("中文", "English")`；颜色只从 `palette` 取。

### M2 引擎：容器网络直连（不改线协议）

新端点 `POST /fabo/debug/dial?container=<名或ID>&port=<n>&proto=tcp|udp`：

- 路由走 `fabo-daemon/supervisor.go` `ServeHTTP` 的通用路径，**不能**像 `/fabo/share` 那样放在唤醒逻辑之前。POST 会经 `requiresGuest` 先唤醒虚拟机；handler hijack 后一直阻塞到流结束，期间 `inFlight` 不为零，虚拟机就不会休眠。这与 attach 的做法相同。实现放进 `localengine`。
- 处理时先解析容器（不存在返回 404，未运行返回 409），再取桥接地址，然后调用 `dialPublishedPort`：TCP 用 `RelayFramedTCPPort`，UDP 用 `RelayUDPPort`。
- 拿到连接后 `Hijack`，做法与 `attach.go:153` 一致。之后：
  - TCP 是原始流，保留半关闭；
  - UDP 在这条流上用现有的 `WriteRelayDatagram` / `ReadRelayDatagram` 分帧。
- 请求不带 `X-Fabo-Background`，所以会刷新活动时间。连接断开时 `inFlight` 减一，空闲计时从那时重新开始。
- 桌面端：
  - 用 `api.go:657` 已有的 Upgrade 写法拨号，再包装成 `net.Conn`；
  - HTTP 客户端的 `Transport.DialContext` 对容器目标走这个端点；
  - 容器详情「信息」页的端口行加调试图标，点开时预填目标打开新建会话。
- **兼容开发模式**：与安装包共用引擎的开发窗口，连的是旧引擎，没有这个端点。收到 404 时，「容器网络」选项置灰并提示需要更新引擎，「映射端口」照常可用。
- 更新 `docs/desktop-overview.md`，新增「Debug dial」小节。

### M3 线协议 0.6：容器 → 宿主的 UDP 与来源识别

> **实施结果（2026-11-17）**：TPROXY 原型在 VM 里失败。VM 的网桥开着 `br_netfilter`（`bridge-nf-call-iptables = 1`），prerouting 在网桥层就执行了，TPROXY 分配的 socket 进入 IP 层时被丢弃，数据报落到 `NoPorts`。因此改用本节提到的退路：UDP 和 TCP 一样走 nat REDIRECT 到 1026，guest 从 conntrack 读原目标端口，回程由 conntrack 把源地址改回 `198.19.248.254:原端口`，不需要透明 socket 和策略路由。
>
> 代价是同一个容器源端口同时发往两个宿主端口时，只有第一个能通（两个流的回程地址相同）。host 网络模式的工作负载也一并加了 UDP 重定向规则，但还没有实测。下文 TPROXY 相关的描述只作记录。

**guestwire**

- `ProtocolMinor` 从 5 升到 6。
- 新增宿主中转头部 `HostBoundHeader{Proto, Source netip.AddrPort, Port}`，用保留目标 `0.0.0.0:2` 作标记，与 `EngineRelayTarget = 0.0.0.0:1` 区分开，后面跟扩展字段。
- 写 `Write/ReadHostBoundHeader` 及往返单元测试。
- 旧的 6 字节格式保留给引擎 API 路径（`engine_socket_linux.go` 用的 `EngineRelayTarget`）。

**guest（fabo-init / guestagent）**

- TCP：`forwardHost` 改写新头部，带上 `client.RemoteAddr()`。REDIRECT 会保留来源地址。
- UDP（**先做验证原型**，这是整个方案风险最大的一步）：
  - 桥接入口：现有的 `host_bound` 是 nat 类型的链，只对每个流的第一个包求值，TPROXY 不能放进去。要在 `forward_linux.go` 里新建一条 filter 类型、mangle 优先级的 prerouting 链。它先于 nat 链执行，所以**必须显式排除目标端口 53**，DNS 才能继续走现有的重定向。
  - 这条链把发往 `198.19.248.254` 的 UDP 交给 `HostRelayPort` 上的透明 UDP socket（`IP_TRANSPARENT` + `IP_RECVORIGDSTADDR`），guest 从每个数据报的控制消息里取原目标端口。
  - 还要配套 fwmark 策略路由（`ip rule fwmark → local table`）。
  - 每个（来源，目标端口）建一个会话：开一条 vsock 1026 连接，写入新头部，之后用 `RelayDatagram` 分帧双向转发，空闲 60 秒回收（照 `port_relay.go serveUDP` 的写法）。
  - 回包：用绑定到 `198.19.248.254:原端口` 的透明 socket 发回容器，这样容器看到的来源才正确。
  - 和 `startHostBound` 的 TCP 共用 128 个槽位。
- host 网络模式的工作负载走的是 output 钩子，TPROXY 不适用。这一期先不支持它们的 UDP，在规格文档里写明。

**fabo-daemon（hostbound_darwin.go）**

- 读新头部。TCP 连 `tcp4 127.0.0.1:port`；UDP 连 `udp4 127.0.0.1:port` 并按帧转发。
- **来源登记表**：拨号成功后、开始转发前，登记「本地回环地址:端口 + 协议 → 来源 IP:端口」，连接关闭时删除。
- 新端点 `GET /fabo/debug/peers?proto=&addr=127.0.0.1:53012`：返回来源地址，引擎再根据自己的容器状态补上容器名。

**桌面端**

- 服务端每收到一个新连接或新的 UDP 来源，就按「本地 → 对端」的地址查一次 peers 并缓存。
- 存在一个竞态：桌面端 accept 可能早于引擎登记完成。处理办法是首次查不到时退避重试 3 次（50ms 起），仍查不到就显示 `127.0.0.1:端口`。
- Linux 上没有宿主中转，来源就是真实的容器 IP，直接与容器列表匹配，不调用 peers。
- 旧引擎（共用引擎的开发模式）对 peers 返回 404 时，一律显示原始地址。

**规格文档**：更新 `docs/desktop-overview.md` 的「Network proxy and host loopback」，写明 UDP、来源识别和 host 网络模式的限制。

## 4. 平台

| 平台 | 本机会话 | 容器网络直连 | 容器 → 宿主 UDP | 来源识别 |
|---|---|---|---|---|
| macOS | M1 | M2（vsock） | M3 | M3（peers） |
| Linux | M1 | M2（直接拨号） | 预期原生支持（容器经网关直达宿主），**待真机验证** | 预期原生支持（真实来源 IP），**待真机验证** |
| Windows / WSL | M1 | 待验证：WSL 引擎是否走 `relay == nil` 分支 | 不在本期 | 不在本期 |

## 5. 验证

- 每一步结束都跑 `make check`（engine 与 desktop 的 vet + test，加 Linux 交叉编译）。
- guestwire：新头部往返、标记冲突、非法输入。
- localengine：dial 端点的 404 / 409 / 正常路径，用 fake relay 测；peers 登记表的增删与并发。
- desktop：Tester 覆盖新建会话、发送、16 进制切换、群发目标、持久化读写、404 降级；`TestScreenshots` 加上浅色和深色截图。
- 端到端：用 `FABO_OWN_ENGINE=1 FABO_SIGN_IDENTITY=- make app-dev` 起自己的引擎（`~/.fabo-dev`）。
  - 起一个 alpine 容器，用 `nc -u host.docker.internal 5514` 发 UDP，确认本机服务端收到、来源显示容器名、回复能回到容器。
  - 用 TCP 直连一个未映射端口（postgres 5432）。
  - 停止容器后会话显示「容器已停止」。
- 每个 M 完成后补 `CHANGELOG.md` 的未发布条目。

## 6. 影响面

- M3 改的是 guest 的网络规则。**所有容器**发往 `host.docker.internal` 的 UDP 都会经过新的中转，不只是调试会话。之前这类 UDP 发不到宿主，改完后会被投递到宿主的回环地址。
- 来源识别和 UDP 中转在同一次版本升级（0.6）里完成。如果只要 UDP、不要来源识别，可以去掉头部里的来源字段和 peers 端点，但版本照样要升。

## 7. 风险

1. **guest 侧 UDP TPROXY**：涉及透明 socket、策略路由和回包源地址。M3 先做一个最小原型，在 `make app-dev` 的虚拟机里跑通再写正式代码；跑不通就退回 conntrack netlink 查原目标（内核有 `CONFIG_NF_CT_NETLINK`）。
2. **来源登记的竞态**：靠「先登记再转发」加桌面端重试兜底，并写并发单元测试覆盖。
3. **UDP 流占用 vsock 连接**：每个来源一条，与 TCP 共用 128 个槽位，满了就丢。这个限制写进规格文档。
4. **开发模式连旧引擎**：所有新端点都必须容忍 404。

## 8. 执行步骤

1. `git worktree add ../Fabo-socket-debugger -b feat/socket-debugger`。之后所有编辑、构建、测试都在 `../Fabo-socket-debugger` 里做。
2. M1 → `make check` → 截图自查 → 提交。
3. M2 → `make check` → 端到端 → 提交。
4. M3 先做 UDP TPROXY 原型并验证，再写正式代码 → `make check`（含 Linux 交叉编译）→ 端到端 → 提交。
5. 更新 `docs/desktop-overview.md` 与 `CHANGELOG.md`。
6. 合并后执行 `git worktree remove ../Fabo-socket-debugger`。
