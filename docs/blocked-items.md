# LEVEE 搁置项清单（2026-10-26 定版；2026-10-28 对账）

> 本文件是**待办搁置区**：工程侧当前无法独立完成的项目，按“卡在哪”分类，
> 每项写明解锁条件。每完成一项就从本清单移除；新发现的搁置项追加于此。
> 判定口径沿用本仓账本惯例：区分“已修/不存在/有意保留/待拍板”，
> 不重复登记已交付能力。

## A. 卡在外部凭据/环境（部署侧给资源，代码零阻碍）

| 项 | 缺什么 | 解锁后动作 |
| --- | --- | --- |
| A-1 ChatOps 真机验收 | Slack App / 钉钉机器人 / 飞书应用的 secret | 填 `notify.chatops.*` 配置即可端到端跑通审批卡片（代码全链路已备） |
| A-2 SSO/OIDC 真实启用 | 真实 IdP（Keycloak/Zitadel/企业微信扫码等）或 GitHub OAuth App 注册 | 填 `auth.oidc` / `auth.github` 配置（默认关闭，代码已落地） |
| A-3 移动端原生推送 | APNs 证书 / FCM 项目凭据 | 深链审批链路已通（PR #141），只差推送通道接入 |
| A-4 L0 四环境安装演练 | 云账号（阿里云/华为云）或信创硬件 | `scripts/preflight-deploy.sh` 已就绪，拿到环境即四 profile 演练 |
| A-5 租约锁生产浸泡 | 一个真实多节点部署跑 4-6 周 | 浸泡数据是 C-1（Raft 决策）的输入 |

## B. 卡在工作所有权（他人活跃工作，按 AGENTS.md 规矩不代合）

（当前无。原 B-1「Firefox 跨浏览器矩阵」已随 PR #208 于 2026-10-28T07:39Z 合入 master，
`feat/browser-matrix-firefox` 分支已删——`git ls-remote --heads` 实测无此分支。）

## C. 卡在未复现/无数据（证据面已建，等触发）

| 项 | 现状 | 解锁条件 |
| --- | --- | --- |
| C-1 Raft/真故障转移 | 已有分析：共识已外包给 PG，Raft 会造成双真值来源；等租约锁浸泡数据再议 PG lease-claim 工作队列 | A-5 |
| C-2 windows calendar `0xc0000005` 偶发崩溃 | 未复现；CI 已自动留档崩溃事件（Event ID 1000/1001）与包重跑证据 | 复发时按自动留档定性 |
| C-3 browser-check windows 模式 A（settle 超时） | 未复现；超时自诊断已上线（指出 mounted/inflight/quiet 哪条没满足并截图） | 复发时读 settleDiagnosis 输出 |

## D. 卡在工作量/立项（代码侧可做，建议独立排期）

| 项 | 还差什么 | 量级 | 备注 |
| --- | --- | --- | --- |
| D-1c 运行期通道的一插件多类型 | 只剩「一插件多类型」：一个插件只能提供**一种**传输类型（类型名=插件名），要多种就装多个插件 | 中 | 会话池**已交付**（本笔）：池键 (插件, host, port, type, 凭据指纹)，每插件空闲 ≤8、空闲 2 分钟回收、闲置 ≤30 分钟、租约内不回收，挂在 serve ctx 上；upload 动词已交付（PR #237，单笔 ≤512 KiB 帧上限）。多类型按真实消费方出现再排 |
| D-2 运营层资产（demo 库/角色视频/品牌 GIF） | 需要真人录制与设计，非代码工作 | 需人手 | **面向客户签单的最大缺口，建议优先安排人手** |

## E. 卡在产品决策/需求分母（明确等触发，不焦虑不硬做）

| 项 | 定档依据 |
| --- | --- |
| E-1 自然语言双语 zh/en | 定档“现在不做”：无真实多语言客户诉求，投入只有审美分母 |
| E-2 `notify.webhook` HMAC 签名 / 变更记录结构化导出等按需项 | 2026-09-06 拍板：只在真实消费方出现时做 |
| E-4 `levee schedule`（cron 定时触发变更） | **2026-10-28 定档：不做**。定位明言「不是调度器」（`product-positioning.md` §2）；触发归平台/CI（OpsMesh schedules / K8s CronJob / CI schedule），LEVEE 拥有的是**受治理的变更**；治理那一半（`window` 窗口阻断 + `calendar` 冻结）已实现。先例：`internal/scheduler` 因与已接入的 `dispatch` 职责重叠被整包退役。详见 `levee-design.md` §4.6.1。 |

## 已交付、不再登记（防混淆）

- 审批票判定全链（服务路径 + gate/深链 DecisionGuard）：PR #214
- 仓库卫生（删 gosec 产物/内网探测报告/.zcode）：PR #214
- drift 常驻巡检 + 告警收件人真缺陷修复：PR #217
- plugin `plugin test` 最小真执行 + notifier 执行面：PR #219
- plugin gate 执行面（DSL 语法 + verify 门禁 + plan hash + engine 可执行性 + wiring 注入 + serve resolver）：PR #221
- plugin module 执行面（协议 `validate`/`execute` + `plugin.PluginModule` 适配器 + `executor.ChannelFree` + serve 注册进进程级 executor + 同名保护）：PR #230
- plugin channel 执行面（会话子进程 + `connect`/`exec`/`collect`/`disconnect` + `plugin.PluginChannel`/工厂 + `channel.CredentialRefNamer` 的明文边界 + serve 注册进通道注册表 + 同名保护）：PR #232
- plugin channel 跨 run 会话池（原 D-1c 的池化半：池键含凭据指纹、空闲 ≤8/插件、空闲 TTL 2 分钟、闲置会话 age 30 分钟、租约内不回收、serve ctx 收尾；`Close` 只归还租约）：本笔（PR #241）
- plugin 破坏性动作声明（原 D-1b：manifest `destructive_actions` → registry 持久化含旧库 `ALTER TABLE` 迁移 → 编译期 `dsl.NewValidator(extra...)` 与计划期 `plan.NewGenerator(extra...)` 消费同一份词表，`plan_hash` 不随「谁 plan」漂移；**当时「未声明即默认不可逆」的强制档仍未做**，已由下一条取代）：PR #234
- plugin 动作三态分类（原 E-3，取代上一条的「仍未做」：manifest 新增 `reversible_actions`，动作落破坏性/可逆/未分类，未分类对已安装模块按不可逆处理；`dsl.PluginActionPolicy` 单一来源，编译期/计划期/对话草案三处闸门同源，判定进 `plan_hash`；旧构造器与 `WithInherentIrreversible` 保留为破坏性简写）：本笔
- 跨浏览器矩阵（原 B-1：Chrome + Firefox × ubuntu/windows，mock 网关补两个 SSE 端点，`BROWSER_CHECK_BROWSER` 选引擎）：PR #208
- service_factory：并行会话已改良（remote=产品方向、local 委托单一构造点+drift guard），维持现状
