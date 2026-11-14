# 容器生命周期性能调查（2026-11-14）

结论：主要差距来自磁盘同步策略。用户确认后，已将虚拟磁盘与 saveJSON 状态文件的默认同步策略改为 fsync，尚未部署。正式实现及同步策略差异见本次提交的源码。Go 子进程启动不是本轮最大的耗时来源。

## 环境与口径

Fabo 源码、机器、工具链和产物哈希见 environment.json。Kiri 是本机安装的 0.2.0 二进制；本地上游源码用于解释行为，不保证与安装二进制逐行一致。两引擎使用相同 SHA256 的 Linux 内核、2 vCPU、2048 MiB、独立 VM 和独立数据盘，均不共享现用应用状态。镜像固定为 alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf。

Python 客户端使用相同请求链：create → 注册 wait removed → start → 读完 removed 响应并验证退出码。每组 5 次预热 + 100 次采样。镜像提前拉取，不计下载和 VM 冷启动；保留全部样本。顺序测试，宿主仍有其他应用运行，不能作为跨机器性能承诺。阶段可能重叠，不能把各阶段 p50 相加当总 p50。

```mermaid
flowchart LR
    A[Create: metadata sync] --> B[Wait registration]
    B --> C[Start: metadata sync and guest setup]
    C --> D[Process exit and filesystem teardown]
    D --> E[Remove: metadata and volumes]
```

| 方案 | network none p50 | p99 | 说明 |
| --- | ---: | ---: | --- |
| Fabo 原版 | 34.92 ms | 43.77 ms | phases-baseline.json |
| 原版加临时计时 | 32.77 ms | 36.53 ms | 30 次，不用于性能排名 |
| 改为父进程挂载 OverlayFS | 32.21 ms | 43.21 ms | 等待从 wait 转移到 unmount，无实质收益，已撤回 |
| Cached + Full 虚拟磁盘 | 33.23 ms | 41.37 ms | 保持强同步，缓存本身不是主要收益来源 |
| Cached + Fsync 虚拟磁盘 | 14.98 ms | 20.24 ms | 状态文件仍为原有同步策略 |
| 虚拟磁盘 + 状态文件均 fsync | 3.15 ms | 5.71 ms | 候选补丁；复测 p50 2.91 ms、p99 8.11 ms |
| Kiri 0.2.0 | 1.29 ms | 2.04 ms | kiri-none.json |

bridge：原版 Go CLI 自带 API 基准 p50 64.09 ms；Python 客户端候选 32.18 ms，Kiri 31.98 ms。原版 bridge 与其他两者客户端不同，不能据此给出精确加速比。原版同一 Go CLI 的 none API p50 33.14 ms，接近 Python 的 34.92 ms。

## 归因

- host created / starting / running 保存各执行 `os.File.Sync()`。本机 Go 的 Darwin 实现使用 F_FULLFSYNC，而不是普通 fsync。临时计时显示每次约数毫秒；原始分段统计见 trace-summary.json。
- guest wait 耗时约 20 ms，显式 cleanup + remove 仅约 0.2–0.4 ms。改为父进程挂载后，同一耗时转移到了 unmount，说明不是单纯 Go Wait/调度开销。
- 仅把虚拟磁盘从 Cached + Full 改成 Cached + Fsync，退出尾部大幅缩短；再将宿主状态文件 Sync 改成 syscall.Fsync，生命周期降至约 3 ms。
- 两项策略均对齐到 fsync 后仍比 Kiri 慢约 1–2 ms，后续才值得拆解 Go re-exec、guest mount 和 RPC。

## 持久性边界

Apple Virtualization SDK 头文件明确：Full 保证已同步数据进入永久介质；Fsync 不保证数据离开磁盘内部缓存。Go 元数据从 F_FULLFSYNC 改为 fsync 也有对应的掉电保证差异。这不是“免费优化”，必须由产品策略选择。实验没有采用无同步模式，没有跳过删除完成通知，也没有删除 sync 调用。

用户明确选择将默认值也改为 fsync，因此未新增配置开关。macOS 虚拟磁盘使用 Cached + Fsync；saveJSON 使用 syscall.Fsync，保留临时文件写入、同步、关闭、原子替换及失败返回流程。Linux 的 saveJSON 仍使用 fsync 级别同步。现有状态文件与数据盘格式不变，无需迁移；虚拟磁盘策略在新 daemon / VM 启动后生效。

## 验证与复现

`smoke.json` 包含 8 项实际检查：退出码 0/1/42、输出 drain、重复启动、重启后状态/文件保留及清理。常规重启检查不能证明断电安全。正式实现的构建、make check 和真实 VM 回归结果见下方最终验收。

在专用引擎已启动、镜像已存在时，从仓库根目录运行：

```sh
python3 scripts/perf/lifecycle.py \
  --socket /path/to/private/docker.sock \
  --image alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf \
  --runs 100 --network none --out /tmp/lifecycle.json --max-p50-ms 10
```

10 ms 是本机本次实验的回归门槛，不是跨机器保证。原版实测命令退出 1（threshold-baseline.json/log），fsync 实验版退出 0（threshold-experiment.json）。脚本只创建 AutoRemove 测试容器；失败时只清理它创建的容器，不拉镜像或改 Docker context。

正式代码不含诊断输出。全部 `[DEBUG-perf]` 临时计时已从引擎源码清除。隔离实例在交付前停止；日常 Fabo 未重启。

## 最终验收：默认 fsync 已实现

从当前性能工作区重新编译了无诊断输出的 daemon、guest、initramfs。签名检查通过；产物唯一绝对路径、mtime、SHA256 见 final-artifacts.json。未打包或替换已安装应用，未提交或合并。

- `make check` 全部通过：engine vet/race 测试、desktop vet/测试、Linux ARM64 vet 与测试编译。交叉检查不等于 Linux 运行时验收。
- `TestSaveJSONReplacement` 覆盖成功替换、序列化失败保留原记录、rename 失败及临时文件清理；原有状态恢复测试通过。
- 正式构建真实 VM 的 12 项检查全部通过，见 final-smoke.json：退出码 0/1/42、缺失命令、输出完整、bridge 地址、重复启动、退出状态、重启恢复、可写层、卷数据及测试资源清理。
- 同一客户端、镜像和 VM 资源下，100 次 + 5 次预热：none p50 **4.0947 ms**、p99 **21.5733 ms**，bridge p50 **33.7481 ms**、p99 **55.8483 ms**。相较首次原版 none p50 34.9162 ms 降低约 88%；本机存在其他负载，尾延迟仍有波动，未声明固定倍数保证。正式 none 回归命令带 10 ms p50 门槛并通过。
- 未运行全量 `tests/e2e.sh`，未验证真实断电或 Windows 宿主。无持久化 schema 变更。隔离实例和测试容器/卷已停止或清理。

## 后续：bridge 退出阶段的 veth 删除改为后台进行

在 guest 里逐步打点后发现，fsync 改动之后 bridge 网络退出阶段还剩约 27 ms，其中 `containerNetwork.close()` 删除 host 端 veth 占 25 ms。在 VM 里单独测试：即使开着 `rcu_expedited=1`，删除一个 veth 也固定要约 20 ms，用 `ip -batch` 批量删除每个仍要 16.5 ms；而「摘离网桥 + 置为 down + 改名」只要 0.25 ms。Kiri 并没有消除这笔开销，只是把删除挪到下一个容器启动时，所以它的 bridge start 要 29 ms。

现在的做法：容器退出时同步把 veth 摘离网桥、置为 down，并改名为 `kr<pid><序号>` 墓碑名，随后立即上报 Exited；`LinkDel` 按 index 在后台执行，`Runtime.Close` 会等后台删除全部完成。改名之后，同一个容器立即重启时不会撞上 `kv<id>` 这个名字；按 index 删除，也就不会误删新建的同名 veth。容器 netns 销毁时内核本身也会回收这对 veth，所以 agent 异常退出时墓碑也能自行消失。

同一私有引擎（e14b3aa 的 daemon，加上本改动的 guest），2 vCPU / 2048 MiB，100 次 + 5 次预热：

| 网络 | 改动前 p50 | 改动后 p50 | 改动后 p99 | 原始数据 |
| --- | ---: | ---: | ---: | --- |
| bridge | 31.61 ms | **3.92 ms** | 6.97 ms | async-veth-bridge.json |
| none | 2.48 ms | 4.10 ms | 7.25 ms | async-veth-none.json，none 路径未改动，差异来自本机负载波动 |

回归验证：`TestRetireVethFreesNameAndBridge`、`TestRetireVethAlreadyGone`、`TestTombstoneName` 在特权容器里对真实内核连续跑 20 轮全部通过，guestagent 包的全部 Linux 测试也通过；`make check` 通过。私有引擎上依次验证了：200 次 bridge 生命周期后没有遗留 `kr`/`kv` 接口；同一个容器连续 `restart` 20 次全部成功，地址保持不变；地址被快速复用后 30 次 TCP 连通全部成功；自定义网络按名字访问 20/20 成功；`network connect`/`disconnect` 正常；有待删 veth 时 daemon 收到 SIGTERM 能在 0.1 s 内正常退出，控制台没有 veth 相关错误。全量 `tests/e2e.sh` 未运行。

## 后续：缩短 start 阶段（提前上报 Started）

在宿主和 guest 两侧分别打点后，none 网络下 start 阶段约 2.0 ms 的分布是：宿主写 starting 记录 0.16 ms；`HasLayer` RPC 0.05 ms；等 guest 回 Started 1.51 ms；重新获取 `e.mu` 0.20 ms（锁正被 Started 事件处理占着，里面在写 running 记录）。guest 内部：fork+exec Go 子进程 0.31 ms，子进程初始化 0.77 ms（其中挂载、seccomp、切换身份等实际设置约 0.39 ms，其余是 Go 运行时启动），设备过滤 BPF 0.11 ms，网络 0.08 ms。Kiri 的 start 只有 0.41 ms，因为它在 `clone3` 之后就回 Started，不等容器进程完成初始化和 exec。

经用户确认，改为和 Kiri 一致的语义，并加了三项不改变语义的优化：

- 非 TTY 容器在网络就绪后就上报 Started。子进程初始化的结果由退出 goroutine 在后台读取：初始化失败时容器以 127 退出（命令存在但不能执行时为 126），原因写到容器 stderr，格式为 `fabo: cannot start container: …`。TTY 容器要等子进程在初始化结束时把终端传回来，所以保留原来的等待，失败时 start 仍然报错。`docker exec` 不受影响。
- 默认规则的设备过滤 BPF 程序只加载一次，之后所有容器的 cgroup 都挂同一个程序。
- `--rm` 容器在 start 时不落盘 `starting`，进入 running 时也不重写记录（`--rm` 不能配重启策略，退出时记录会被删除）。其他容器照旧落盘：codex review 指出，否则带重启策略的容器在启动过程中遇到 daemon 崩溃，就会丢失重启意图。

### codex review 后的修复

review 发现的两个 P1 都来自提前上报 Started：子进程这时可能还没完成 `pivot_root`、也还没加入共享网络，但已经被当成完整的容器对待。

现在子进程在挂载和 `pivot_root` 完成后，会往状态管道写一个就绪字节（`setupReady`）。agent 收到这个字节，才把该容器标记为初始化完成；管道没有这个字节就关闭（比如子进程在初始化中途被 kill），一律视为失败。需要从容器进程读取根目录或命名空间的操作（exec、`docker cp` 和导出、文件浏览、监听端口查询、加入 `--network container:`），都必须等它初始化完成之后才能拿 fd；初始化失败就拒绝。stats 在初始化完成之前不报告网络计数。第一版是在 guest 里用 `lockSettled` 等待，等待期间不持有 runtime 锁。最终版本只有走独立连接的 `docker cp` 和导出还这样等，其余请求的处理方式见下文。

codex 第二、三轮复核又找到两条由「在控制通道上等待初始化」引出的问题。guest 的控制通道是单循环串行分发的，宿主侧的 session 也一次只发一个请求。只要通道上有请求在等某个容器初始化完成，而初始化又被外部因素卡住，就会死锁：初始化中被 pause（cgroup 冻结），或者共享 PID 命名空间的另一个容器直接 `kill -STOP` 了它，就写不出就绪字节，等待的请求一直占着通道，能让它恢复的 unpause / CONT 就永远进不来。中间一版用「冻结前先等初始化、停止类信号延后投递」堵住了经过 API 的路径，但仍挡不住容器内部直接发出的 STOP，而且延后的 STOP 会覆盖之后的 CONT（复核 P2）。

最终做法是：guest 的控制通道上**永远不等待已上报 Started 的容器的初始化**（TTY 容器和 exec 子进程自身的启动仍是同步的，和 HEAD 一样）。文件浏览和监听端口查询在容器还没就绪时直接回 still starting。guest 读到就绪字节后，通过事件流发一个 `Settled` 事件（`guestwire.SettledMessage`，帧号 81，只带容器 ID）；不走提前回 Started 的容器，在 Started 之后紧接着发。宿主给每次运行建一个 `settled` 通道，收到事件就关闭。exec 在拿容器的 `c.operation` **之前**、不持有任何锁时等这个通道；加入 `--network container:` 时，等的是网络所有者的通道。所以初始化被卡住时，同一个容器的 CONT、kill、pause 和删除都不受影响。中间一版是在宿主上对 still starting 错误做轮询重试，codex 第四轮指出了两个问题：重试期间一直持有 `c.operation`，同一容器的 CONT、kill 会返回 409；按子串匹配错误文本，可能把用户输入里恰好含有这段文字的永久错误当成可重试。现在的设计不轮询、不做字符串匹配，这两个问题都不存在了。guest 侧的 `requireSettled` 保留，作为兜底检查。加入共享网络的容器在等待所有者时，持有的是它自己的 `c.operation`，不是所有者的，所以所有者的恢复、终止和删除不受影响。新增了必需的 Settled 事件，协议版本从 4 升到 5：codex 第五轮指出，如果新 daemon 配上旧 guest，握手会通过，exec 却会一直等一个不会来的 Settled。现在版本不一致的另一方会在 Hello 时被明确拒绝。`docker cp` 和导出走独立连接，继续在 guest 里等待。`freeze` 和 `Signal` 与 HEAD 完全一致。

在真实 VM 上的对照验证：让新卷首次复制 `python:3-alpine` 的 `/usr/local`（1913 个文件，启动多花约 70 ms），紧接着发出 `docker exec` 和 `docker cp`。去掉屏障的对照构建两项都是 0/20，exec 进入了 agent 自己的根目录；修复后的构建都是 20/20。链式 A→B→C 共享网络两边都是 20/20：B 加入网络是子进程初始化的第一步，窗口很窄，这次没有撞上，但屏障同样覆盖了这个竞态。

本来还考虑过 none 网络由子进程自己启用 lo、省掉网络对象，但 none 模式下 guest 会在容器 netns 里提供 127.0.0.11 解析器，省掉会改变行为，所以没有做。

同一私有引擎（本分支构建的 daemon 和 guest），100 次 + 5 次预热，p50（p99）：

| 网络 | start | exit_remove | total | 原始数据 |
| --- | ---: | ---: | ---: | --- |
| none | **0.72**（1.77） | 1.86（4.47） | 3.00（5.92） | early-start-none.json |
| bridge | **1.06**（3.05） | 1.94（4.44） | 3.43（6.30） | early-start-bridge.json |

start 从约 2.0 ms 降到约 0.65 ms。子进程初始化的时间挪到了退出阶段，所以 total 基本不变。表中数据是全部 review 修复之后重新测的，屏障没有拖慢普通的 start。

回归验证（私有引擎）：缺失命令以 127 退出并输出 `executable file not found`，对应 e2e 用例 `exit-127-missing` / `missing-msg` 的断言；无执行权限以 126 退出；`docker start` 一个命令不存在的容器时返回成功，`inspect` 显示 exited 127，`docker logs` 里有原因；TTY 正常运行，TTY 加缺失命令时 start 照旧报错；`--init` 正常运行，`--init` 加缺失命令以 127 退出；在 exec 之前写入的 stdin 能被容器读到；退出码 42 透传；启动后立即 `stop` 正常；`--restart always` 正常重启；exec 到初始化失败的容器会被拒绝。新增 `TestStartRecordsIntentBeforeGuest`：用模拟 guest 卡住 Started，确认非 `--rm` 容器在磁盘上已经是 starting、`manualStopped` 已清除，恢复后 `unless-stopped` 仍会被拉起；`--rm` 容器的记录不变。临时撤掉修复时，这个测试会失败。新增 `TestReadSetup` 和 `TestLockSettled`，覆盖就绪字节的解析，以及等待期间不持锁、失败时拒绝、等待期间容器被同 ID 重启的情况；新增 `TestRequireSettled`（不等待，未就绪时回 still starting）、`TestSettledEventRoundTrip`，以及宿主侧的 `TestExecWaitsForSettled`：exec 在 Settled 之前不会发往 guest，等待期间 `c.operation` 是空闲的，同一容器的 CONT 能到达 guest；运行在等待期间结束时，exec 返回 409。临时去掉「拿锁之前先等待」时，这个测试会失败。最终构建在真实 VM 上的场景回归（新卷挂到 `/usr`，初始化窗口约 960 ms，每步都设超时）：初始化中立即 exec 10/10、`docker cp` 10/10；初始化中 pause → 监听查询 → unpause → exec → `docker ps` 10/10；共享 PID 命名空间时 A 直接 `kill -STOP` 初始化中的 B（10 轮都确实撞上了窗口），B 停住期间 `docker ps` 和 exec 到 A 都在 3 s 内返回 10/10，A 发出 CONT 后排队的 exec 完成 10/10；初始化中先 `docker kill -s STOP` 再 `-s CONT`，之后容器仍在运行 10/10；B 被 A 直接停住、已有 exec 在排队时，通过 B 自己的 API 发 CONT 10/10 成功，排队的 exec 随后完成 10/10；同样情况下 `rm -f` 10/10 成功。对 daemon 发 SIGKILL 模拟崩溃后重启：`--restart always` 的容器自动拉起，普通容器显示 Exited (255)，`--rm` 容器显示 Created（原来是 Exited (255)），三者都能正常删除。`TestDefaultDeviceFilterLoadedOnce` 在特权容器里对真实 cgroup 通过，guestagent 包的全部 Linux 测试通过，`make check` 通过。全量 `tests/e2e.sh` 未运行。

## 与 Kiri 的同时段对照（最终版）

同一台机器、同一时段依次测量，两个引擎都用私有实例，配置都是 2 vCPU / 2048 MiB；Kiri 是本机安装的 0.2.0。API 列用 `scripts/perf/lifecycle.py`（100 次 + 5 次预热，取 p50）；CLI 列是同一个 `docker` CLI 执行 `docker run --rm alpine true`（30 次 + 5 次预热，取中位数）。镜像固定为同一个 alpine digest。

| | API none | API bridge | CLI none | CLI bridge |
| --- | ---: | ---: | ---: | ---: |
| Kiri 0.2.0（本机实测） | **1.30 ms** | 35.49 ms | **11.50 ms** | 35.94 ms |
| Fabo（最终） | 2.76 ms | **3.43 ms** | 14.17 ms | **15.09 ms** |
| Fabo（原版） | 33.14 ms | 64.09 ms | 39.73 ms | 76.08 ms |
| Kiri README 标称（bridge） | — | 2.55 ms | — | 14.2 ms |

原版 Fabo 那一行取自 `baseline-*.log`，是用 `fabo bench` 在另一个时段测的。Kiri README 的 bridge 数字在本机没有复现：本机 Kiri 的 bridge start 阶段要约 32 ms，耗在删除 veth 上。原始数据见 `compare-*.json`。
