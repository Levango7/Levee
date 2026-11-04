# LEVEE 搁置项清单（2026-10-26 定版；2026-10-28 对账；2026-10-29 增设 F 节「卡在上游发布」）

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
| D-1b 插件破坏性动作声明（已交付） | 已闭环：PR #234（`feat/plugin-destructive-actions`，rebase 落地 `b375ec8`，4 个干净 commit：`bf5a387`/`293694e`/`63ddcf3`/`b375ec8`）；计划期+编译期两侧均消费同一份 `dsl.PluginActionPolicy`（注册表持久化含旧库 `ALTER TABLE` 迁移）；编译期 LE082、计划期 LE082 同源判定（`plan_hash` 不随「谁 plan」漂移）；旧构造器与 `WithInherentIrreversible` 保留为破坏性简写。**强制档（未声明即默认不可逆）仍未做，已由下一条 E-3 取代** | 已合入 master（`b375ec8`，单父线性，§5.4 无违规）；分支已删（EOL） | 见 `docs/blocked-items.md` E-3 与 `CHANGELOG.md` Unreleased 新段 |
| D-1c 运行期通道的一插件多类型 | 只剩「一插件多类型」：一个插件只能提供**一种**传输类型（类型名=插件名），要多种就装多个插件 | 中 | 会话池**已交付**（本笔 `feat/d2-target-caps`，`81886d5` 补测试）：池键 `(插件, host, port, type, 凭据指纹)`，每插件空闲 ≤8/插件、空闲 TTL 2 分钟、闲置会话 age 30 分钟、租约内不回收，挂在 serve ctx 上；`Close` 只归还租约（语义变化：池存活期与 serve ctx 同寿，不再每次 run 新建/销毁子进程）；`publish` 并发拨号由先到者入池、后到者持有胜者租约（同步，非异步）；凭据指纹由每进程随机密钥 HMAC + 长度前缀生成，不可离线推算。多类型按真实消费方出现再排 |
| D-2 运营层资产（demo 库/角色视频/品牌 GIF） | **半边已交付（2026-11-01，demo 库的「场景脚本」）**：四个场景各有可跑工作流与演示脚本——索引 `deploy/lab/DEMOS.md`（证明什么能力 / 前置 / 怎么跑 / 验收看哪条证据 + 诚实边界）+ `deploy/lab/workflows/` 下 6 个文件，CI 强制编译并强制索引↔文件双向对齐。**追加本地容器靶场（2026-11-02）**：配方 `deploy/lab/container-lab/`（3 台 alpine+sshd+nginx 容器顶 VM），执行期路径与快照采集/还原**已在该靶场实跑**。**剩下的仍需人手**：在实验室里把四个场景录成视频（demo 库录像）、三段角色视频、品牌资产（堤坝符号 + 状态色板 + GIF） | 需人手（脚本已交付；拍摄与设计仍需人） | 仍是**面向客户签单的最大缺口**——但"最大缺口"里的**可自动化那半边已经不在这一行里了**。脚本半边的实测边界见 `deploy/lab/DEMOS.md` §4（执行路径与快照采集/还原已在本地容器靶场实跑，见 `deploy/lab/container-lab/`；**仍未实跑**的是真 VM 实验室、MySQL 主从切换与 PG 集群接管） |

> **编号消歧（2026-11-01）**：本表的 **D-2** 指**运营层资产**；`CHANGELOG.md` `[Unreleased]` 段里那笔 **"D-2 目标组边界检查整理提交"**（`feat/d2-target-caps`、`internal/plan/targetcaps.go`、LE105/LE106/LE107）是**另一件已交付的事**，与本行无关。同一个编号指两件事，按"一个结论写两遍就会漏改一半"的口径登记在此，避免下一个读者把两笔对起来。分支名 `feat/d2-target-caps` 里的 `d2` 取的是后者（目标组边界检查），而该分支同时交付了 D-1c 的会话池半边——这正是编号撞车的来源。

## E. 卡在产品决策/需求分母（明确等触发，不焦虑不硬做）

| 项 | 定档依据 |
| --- | --- |
| E-1 自然语言双语 zh/en | 定档“现在不做”：无真实多语言客户诉求，投入只有审美分母 |
| E-2 `notify.webhook` HMAC 签名 / 变更记录结构化导出等按需项 | 2026-09-06 拍板：只在真实消费方出现时做 |
| E-4 `levee schedule`（cron 定时触发变更） | **2026-10-28 定档：不做**。定位明言「不是调度器」（`product-positioning.md` §2）；触发归平台/CI（OpsMesh schedules / K8s CronJob / CI schedule），LEVEE 拥有的是**受治理的变更**；治理那一半（`window` 窗口阻断 + `calendar` 冻结）已实现。先例：`internal/scheduler` 因与已接入的 `dispatch` 职责重叠被整包退役。详见 `levee-design.md` §4.6.1。 |

## F. 卡在上游发布（第三方工具链还没提供可装版本，不是本仓工作量）

| 项 | 卡在哪 | 复测触发条件 |
| --- | --- | --- |
| F-1 `typescript` 5.9 → 7 | **本仓已实测确认无解**：TS 7（原生编译器）移除了 `./lib/tsc` 子路径，`vue-tsc` 在 `resolveTscPath` 直接抛 `ERR_PACKAGE_PATH_NOT_EXPORTED`，类型检查门跑不起来。上游的支持在 Vue Language Tools **4.0** 这条线上——`@vue/language-core@4.0.0-alpha.2` 的 peer 已写成 `typescript: ">=7.1.0-beta"`，而 TS 侧当前稳定版是 7.0.2、7.1 只有 beta/dev 构建；`vue-tsc` 的已发布版本停在 3.3.14，没有 4.x 也没有 alpha tag。⇒ 2026-10-29 的 PR #247 把 `typescript` 钉在 `^5.9.3`（同时满足 vitest 5 与 vue-tsc 3 的 peer 范围） | **`vue-tsc` 出现 4.x（或其 alpha）且 peer 允许已发布的稳定 TS** 时，单独一个 PR 处理：按 #247 的同一条证据路径验（本机重建 + `node:<ver>-alpine` 容器内构建后 `diff -r internal/web/dist` 为空，再让 CI 的 `frontend` 与 `browser-check` 四条腿定案）。收益只有类型检查速度（本仓 `frontend` 腿现为 27–34 秒），所以不值得为它换掉检查器或放宽门禁 |


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
- service_factory：**已接线（2026-11-02）**——`version` / `system version`、`template list` / `template show`、`target list`、`audit verify` 两种模式都经工厂（local 读二进制变量/文件库/store，remote 问服务端 `GetVersion` / `ListTemplates` / `GetTemplate` / `ListTargets` / `VerifyHashChain`），`--remote` 白名单随之扩到六条；工厂的本地构建改成**按需**（不再在门口强制要 store，所以 `version` 不会顺带创建数据库）。`template list` 能接是因为先补了线上字段：`pb.Template` 新增 `id`（7）与 `tags`（8），并把 `serve` 从 `NewTemplateService(store, nil)` 改成读 **CLI 写的那份文件库**——否则远端会对着有模板的部署回答「没有模板」，而且 RPC 建过的模板重启即失忆。`target list` 则是 `pb.Target` 补了 `created_at`（10）/`last_checked_at`（11）/`tenant_id`（12）、`ListTargetsRequest` 补了 `group_name`（7）——本地 `--json` 的三列与 `--group` 的按名字解析此前在线上都不存在，未知分组现在两种模式都报同一句 `group "x" not found`。`audit verify` 是 `pb.RunVerification` 补了 `repeated AuditChainFailure failures`（trace_id/index/expected/actual/prev_*/type）——线上原先只回 `failures[0]`，多行篡改在远端会看起来像单行；另外「run 没有 trace」线上被 `verify all` 的跳过逻辑吞掉，CLI 现在把空 `runs` 还原成本地那句 `ErrNoTraces` 拒绝。`template show` 是 `pb.Template` 补了 `repeated TemplateParameter parameters`（name/description/required/default/type）——`required_params` 只带必填参数的**名字**，可选参数、类型、默认值与描述此前线上都不存在；CLI 导出 `template.FormatShow` 作为唯一人类渲染，两种模式共用。**已收口（2026-11-04）**：`system status` 的两种模式按「远程显示更少」定案——本地仍报 `config_path` / `db_*`（本机配置指向的库），远程只报 `GetStatus` 的运行事实（`status` / `active_runs` / `paused_runs` / `uptime_seconds` / `store_type` / `warnings`），并用 `local_only` 明确列出不报的字段。理由：`GetStatus` 与 REST `/system/status` 都只做认证、不做策略判定，配置路径与数据库端点是部署拓扑，`GetConfig` 对此有 `ActionAdmin` 门禁；加宽 proto 等于把拓扑放上监控面。`--remote` 白名单随之扩到十条（新增 `system status`）。
