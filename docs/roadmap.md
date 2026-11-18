# Roadmap

功能对齐上游 Kiri 的进度在 [compatibility-baseline.md](compatibility-baseline.md)；这里记录 Fabo 自己的功能和它们的验证状态。

## 公网共享：套餐与收费（edge.share.talkwork.vip）

规格见 [share-billing.md](share-billing.md)，实现细节见 [share-billing-internals.md](share-billing-internals.md)，界面设计稿在 `designs/fabo-share-billing/`。

### 已完成（2026-11-15，`ec2d19f..9e08f91`，已合并到 master）

- [x] 套餐、计量与限额：`fabo-edge -billing billing.json`。免费版同时 1 个连接、每月 5 GB；付费三档同时 5 个连接，按月流量分档。80% 时提醒，用完后断开并拒绝新连接，下月 1 日重置。不加 `-billing` 时行为与原来一致。
- [x] 权益与订单：建单时保存套餐快照；续费、升级、乱序到账都按秒折算实际付款；`store.update` 统一事务；订单审计写入 `orders.jsonl`，结束 90 天后归档；超出套餐的保留名暂停使用。
- [x] 支付：支付宝电脑网站支付、微信 Native APIv3，包括验签、查单、关单和补偿扫描。相同的购买复用原订单，每个账号最多 3 笔未结束订单；微信退款的订单直接关闭。
- [x] 中继网页：`/account`、`/pay/{id}`、支付结果页，一次性 ticket、cookie 会话、CSRF。
- [x] 管理命令：`fabo-edge plan show|set`、`fabo-edge orders`。
- [x] 客户端：额度不足时链接进入 `waiting`，额度恢复后自动重连；新增 `POST /fabo/share/billing`；`fabo share status` 显示套餐和用量，新增 `fabo share upgrade`。
- [x] 桌面共享页：套餐标签和用量条、流量用完的横幅、连接已满对话框、「升级」入口。
- [x] 验证：`make check`、相关包 `-race` 测试，以及 `TestScreenshots` 渲染的各状态截图。
- [x] 本机安装版 `/Applications/Fabo.app` 已升级到 `0.5.1-14-g9e08f91`（Developer ID 签名，未公证）；旧版备份在 `target/backup/Fabo-0.5.1-5-g425fa8b.app`。

### 已完成（2026-11-16）

- [x] 中继网页管理后台 `/admin`：`fabo-edge admin` 生成一次性登录链接；概览、用户、用户详情、订单、在线隧道、操作记录；修改套餐、发放 / 撤销 token、释放保留名、关闭隧道；运维操作写入 `admin.jsonl`。
- [x] 每条隧道的实时双向流量（不依赖计费）；`whoami` 增加 `usage.history`（近 6 个月）。
- [x] 桌面：共享表格新增「流量」列；点击用量条打开流量详情对话框。
- [x] 管理后台：流量页（按月、CSV 导出）、套餐页（目录、用户数、收入）、订单汇总与手动查单。

### 待验证（下次）

- [ ] 管理后台在真实中继上走一遍：`fabo-edge admin` 登录、修改套餐后桌面刷新、关闭隧道、撤销 token。

- [ ] 本地端到端：起 `fabo-edge -dev -billing <测试配置>`，支付宝用沙箱网关；`Fabo Dev.app` 用 `FABO_SHARE_CA` / `FABO_SHARE_CONNECT` 连接本地中继。依次走：
  - 共享页显示用量
  - 开第 2 条链接，弹出「连接已满」
  - 点升级进入网页，在沙箱付款
  - 窗口刷新为付费套餐，等待中的链接自动恢复
  - 用 `curl` 把流量刷满，出现断开和横幅
- [ ] 跨月与到期：用 `fabo-edge plan set --until` 和假时钟，验证到期降级（只保留最早的一条隧道）和跨月后自动恢复。
- [x] 部署（2026-11-16）：edge.share.talkwork.vip（lisahost）运行 `0.5.1-16-gacbacdd`，`-billing /opt/fabo-edge/billing.json` 已开启，只配套餐、未配支付；旧二进制、systemd unit 和状态目录备份带 `20261116-022551` 时间戳。
- [ ] 生产联调：用 `testAccounts` 限定的 1 分钱测试套餐，在线上真实走一次支付宝和一次微信，确认两个回调地址从公网可达、能入账。

### 上线前提（需要准备，不是代码问题）

- [ ] 支付宝开放平台应用开通电脑网站支付，微信支付商户号开通 Native 支付。两者都要求企业主体，域名要已做 ICP 备案并与商户资料一致。
- [ ] 确认面向公众收费提供隧道转发服务涉及的增值电信业务许可和内容合规要求。
- [ ] 定价：`billing.json` 里目前是建议价（基础 ¥9 / 50 GB，专业 ¥29 / 200 GB，高级 ¥99 / 1 TB，免费 5 GB），上线前确认最终价格。
