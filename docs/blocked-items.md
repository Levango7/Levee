# LEVEE 搁置项清单（2026-10-26 定版；2026-10-28 对账；2026-10-29 增设 F 节「卡在上游发布」；2026-11-07 账本卫生订正）

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
| A-4 L0 四环境安装演练 | 云账号（阿里云/华为云）或信创硬件 | 现存安装面：`deploy/baremetal/install.sh`（幂等安装器）+ `levee doctor` + `docs/deployment.md` §15 交付检查单；拿到环境即按检查单做四 profile 演练。（2026-11-07 订正：原登记的 `scripts/preflight-deploy.sh` **从未建过**——`git log --all` 对该路径零记录、订正前全仓 `preflight` 唯一命中就是本行自身；不再以不存在的脚本声明「已就绪」。） |
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
| D-2 运营层资产（demo 库/角色视频/品牌 GIF） | **半边已交付（2026-11-01，demo 库的「场景脚本」）**：四个场景各有可跑工作流与演示脚本——索引 `deploy/lab/DEMOS.md`（证明什么能力 / 前置 / 怎么跑 / 验收看哪条证据 + 诚实边界）+ `deploy/lab/workflows/` 下 6 个文件，CI 强制编译并强制索引↔文件双向对齐。**追加本地容器靶场（2026-11-02）**：配方 `deploy/lab/container-lab/`（3 台 alpine+sshd+nginx 容器顶 VM），执行期路径与快照采集/还原**已在该靶场实跑**。**剩下的仍需人手**：在实验室里把四个场景录成视频（demo 库录像）、三段角色视频、品牌资产（堤坝符号 + 状态色板 + GIF） | 需人手（脚本已交付；拍摄与设计仍需人） | 仍是**面向客户签单的最大缺口**——但"最大缺口"里的**可自动化那半边已经不在这一行里了**。脚本半边的实测边界见 `deploy/lab/DEMOS.md` §4（执行路径与快照采集/还原已在本地容器靶场实跑，见 `deploy/lab/container-lab/`；**仍未实跑**的是真 VM 实验室、MySQL 主从切换与 PG 集群接管） |
| D-3 账本时刻的方言归一（SQLite=UTC，PostgreSQL=会话时区） | 还差**一个口径拍板**：库里的 `DATETIME`/`TIMESTAMP` 在两套方言里解出来的 `*time.Time` 时区不同（实测见「已交付」段的 service_factory 条目），后果是所有带时刻的命令（`audit list` / `audit export` / `audit show`、`target list`、`target history`、`group list`）在 PostgreSQL 部署上把同一个 Unix 时刻渲染成**另一个日期**，而 `--remote` 侧只有一种重建方式，无法两边都对上。可选做法：在 store 解码层统一成 UTC（改动面最小、但会让已按本地时区读过账本的 PG 用户看到日期变化），或在 CLI 层加显式 `--tz` 旗标（新增契约）。**不在本系列里顺手做**：它会改已发布输出，且 `--remote` 等价测试只能覆盖 SQLite | 中（改动集中在一层，但需要跨命令重跑等价证据） | 触发条件：真实 PostgreSQL 部署把「远程和本地显示不同一天」当问题报上来，或下一次接线再引入一个新的时刻字段。届时先按本行做取证，再选归一层 |

> **2026-11-07 账本卫生**：原 **D-1b**（插件破坏性动作声明，已交付）按文件头「每完成一项就从本清单移除」的口径移出本表；其登记内容（PR #234、`b375ec8`、四个落地 commit）已在「已交付、不再登记」段与 `CHANGELOG.md` 2026-10-28 段留痕，「强制档（未声明即默认不可逆）」的边界由「已交付」段 **E-3**（动作三态分类）承接。

> **编号消歧（2026-11-01）**：本表的 **D-2** 指**运营层资产**；`CHANGELOG.md` `[Unreleased]` 段里那笔 **"D-2 目标组边界检查整理提交"**（`feat/d2-target-caps`、`internal/plan/targetcaps.go`、LE105/LE106/LE107）是**另一件已交付的事**，与本行无关。同一个编号指两件事，按"一个结论写两遍就会漏改一半"的口径登记在此，避免下一个读者把两笔对起来。分支名 `feat/d2-target-caps` 里的 `d2` 取的是后者（目标组边界检查），而该分支同时交付了 D-1c 的会话池半边——这正是编号撞车的来源。

## E. 卡在产品决策/需求分母（明确等触发，不焦虑不硬做）

| 项 | 定档依据 |
| --- | --- |
| E-1 自然语言双语 zh/en | 定档“现在不做”：无真实多语言客户诉求，投入只有审美分母 |
| E-2 `notify.webhook` HMAC 签名 / 变更记录结构化导出等按需项 | 2026-09-06 拍板：只在真实消费方出现时做 |
| E-4 `levee schedule`（cron 定时触发变更） | **2026-10-28 定档：不做**。定位明言「不是调度器」（`product-positioning.md` §2）；触发归平台/CI（OpsMesh schedules / K8s CronJob / CI schedule），LEVEE 拥有的是**受治理的变更**；治理那一半（`window` 窗口阻断 + `calendar` 冻结）已实现。先例：`internal/scheduler` 因与已接入的 `dispatch` 职责重叠被整包退役。详见 `levee-design.md` §4.6.1。 |

## G. 2026-11-06 四维自查的遗留项（工程侧可做但各自是设计取舍，均在审计中实证成立）

（当前无。G-1 / G-3 于 2026-11-07 定档（维持现状 / 不提供人工修复入口），
G-2 / G-5 已交付，G-4 已于 2026-11-07 随 wire 契约夹具补齐交付——四项全部出清。）

## F. 卡在上游发布（第三方工具链还没提供可装版本，不是本仓工作量）

| 项 | 卡在哪 | 复测触发条件 |
| --- | --- | --- |
| F-1 `typescript` 5.9 → 7 | **本仓已实测确认无解**：TS 7（原生编译器）移除了 `./lib/tsc` 子路径，`vue-tsc` 在 `resolveTscPath` 直接抛 `ERR_PACKAGE_PATH_NOT_EXPORTED`，类型检查门跑不起来。上游的支持在 Vue Language Tools **4.0** 这条线上——`@vue/language-core@4.0.0-alpha.2` 的 peer 已写成 `typescript: ">=7.1.0-beta"`，而 TS 侧当前稳定版是 7.0.2、7.1 只有 beta/dev 构建；`vue-tsc` 的已发布版本停在 3.3.14，没有 4.x 也没有 alpha tag。⇒ 2026-10-29 的 PR #247 把 `typescript` 钉在 `^5.9.3`（同时满足 vitest 5 与 vue-tsc 3 的 peer 范围） | **`vue-tsc` 出现 4.x（或其 alpha）且 peer 允许已发布的稳定 TS** 时，单独一个 PR 处理：按 #247 的同一条证据路径验（本机重建 + `node:<ver>-alpine` 容器内构建后 `diff -r internal/web/dist` 为空，再让 CI 的 `frontend` 与 `browser-check` 四条腿定案）。收益只有类型检查速度（本仓 `frontend` 腿现为 27–34 秒），所以不值得为它换掉检查器或放宽门禁 |


## 已交付、不再登记（防混淆）

- 原 **G-4**（wire 契约夹具未覆盖 5 个前端实调端点）：`internal/grpc/wire_snapshot_test.go` 现在驱动 `/agents`、`/groups`、`/audit/traces`、`/system/version`、`/conversation/sessions` 五条路径，快照从 10 条涨到 15 条（外加 PR #304 补的 `POST /changes/{id}/rollback`）。`/agents` 与 `/groups` 返回**裸数组**，快照新增 `items`（元素键集合）并让它与 `keys`/`rows` 一样进入逐键对账与 PascalCase 防线；TS 侧新增 `rootDto`，裸数组的读取集直接从接口声明推导（不再维护第二份手写名单）。于是 `HostGroup`、`AgentRecord`、`TraceEntry`、`VersionInfo`、`ConversationSessionDTO` 五个 DTO 的拼写第一次被服务端产物验证过——这正是历史上三次 PascalCase 泄漏（ClusterNode / AssignmentSummary 同型）钻过来的那种夹具盲区。本笔只加夹具、**不改服务端行为**：五个端点的键集是记录下来的既有产物。
- 原 **D-1c**（运行期通道的「一插件多类型」）：manifest 新增可选 `channel_types: [别名…]`——一个 channel 插件除自己的名字外，可声明任意多个传输类型。registry 持久化（含旧库 `ALTER TABLE` 迁移；坏列解析为空 = 退回旧行为）；serve 启动时把**同一**插件工厂按插件名 + 每个声明类型注册进 `channel.DefaultRegistry()`，任一类型撞内建传输 / 别的插件则在注册前**整体拒绝**（不留半装）；`PluginMeta.Name` 仍总是注册，所以空声明 = D-1c 之前的行为。校验（仅限 channel 型、token 合法、不重复、不重复插件名）在 manifest 载入与 registry 落库两处。
- 原 **G-3**（`plan.Plan` 持久化无 json tag）：**定档「维持现状」（2026-11-07，是决策不是交付）**。`runs.plan_json` 是 Go 内部工件——写入是 `internal/wiring/plan.go` 的 `json.Marshal(p)`，**全部**读取方 `json.Unmarshal` 回同一 struct，且 `plan_hash` 走独立的 canonical 结构，所以「混合拼写」当前无损；台账原先的触发条件「有非 Go 消费方要读 plan_json」**未到**。补 tag 会改变已落库字节，在滚动升级期间让新旧进程**互读不了**（带 tag 的代码按 snake_case 读、读不到旧的 PascalCase；旧代码读不到新的 snake_case），换来只是「好看」＋一个不存在的消费方。故**不改格式**，只在写入点加注释说明这是有意为之（`internal/wiring/plan.go`）。将来真出现非 Go 消费方，做法是加 tag **并**自定义 `UnmarshalJSON` 同时接受旧 PascalCase（dual-key），不是裸加 tag。
- 原 **G-1**（audit `BuildForce` / `Rebuild` 无生产调用方、无 operator 命令）：**定档「不提供人工修复入口」（2026-11-07，是决策不是交付）**。审计链对**合法写**造成的断裂由下一次 `Seal` 自愈，对**内容篡改**由 `Seal` / `Verify` 报 `ErrChainBroken` 而**不静默重链**；`Rebuild` / `BuildForce` 是刻意另起的方法名、write path 一个都不调用，保留作内部 / 测试与「确认篡改后的恢复」原语。**不新增 `levee audit verify --rebuild`**——那是一个会动 WORM 邻域的新操作面，而该邻域已有 WORM 触发器 + Seal + Verify 三道防线；用一个不可达、又无法从 CLI 触发的“修复”路径去补洞，比洞本身更糟（`Seal` 的注释早已记下这条）。将来真需要人工恢复，按 G-1 行内取证另立产品项。决定写进 `internal/audit/auditchain.go`（`Rebuild`）与 `hashchain.go`（`BuildForce`）的注释。
- 原 **G-2**（assignment 陈旧度判定的时钟源）：新增可选接口 `state.ClockReader`（`StoreNow`）——PostgreSQL 返回服务器 `NOW()`、SQLite 返回本机时钟（与它落的库同机）；`dispatch.reclaimStale` 改用它作为比较用 `now`（经 `state.Underlying` 找到底层 store，拿不到就回退进程时钟并 WARN）。这样「写 `updated_at` 的时钟」与「算 age 的时钟」在异构部署下是同一个，reclaim 判定不再被库/应用时钟差整体平移。：本笔
- 原 **G-5**（`RunEventStore.UpdateRun` 的读-改-写竞态）：新增可选接口 `state.RunStatusPrevReader`——`UpdateRunReturningPrevStatus` 在**同一步**里写全行并回报被覆盖的旧 status（SQLite 走 `BEGIN IMMEDIATE` 单连接事务，PostgreSQL 走 `SELECT … FOR UPDATE`），`RunEventStore.UpdateRun` 优先走它、无能力的 store 保留读-再-写回退。于是并发 `UpdateRun` 不可能再 emit 一个它从未覆盖过的 `from`：本笔
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
- service_factory：**已接线（2026-11-02）**——`version` / `system version`、`template list` / `template show`、`target list`、`audit verify` 两种模式都经工厂（local 读二进制变量/文件库/store，remote 问服务端 `GetVersion` / `ListTemplates` / `GetTemplate` / `ListTargets` / `VerifyHashChain`），`--remote` 白名单随之扩到六条；工厂的本地构建改成**按需**（不再在门口强制要 store，所以 `version` 不会顺带创建数据库）。`template list` 能接是因为先补了线上字段：`pb.Template` 新增 `id`（7）与 `tags`（8），并把 `serve` 从 `NewTemplateService(store, nil)` 改成读 **CLI 写的那份文件库**——否则远端会对着有模板的部署回答「没有模板」，而且 RPC 建过的模板重启即失忆。`target list` 则是 `pb.Target` 补了 `created_at`（10）/`last_checked_at`（11）/`tenant_id`（12）、`ListTargetsRequest` 补了 `group_name`（7）——本地 `--json` 的三列与 `--group` 的按名字解析此前在线上都不存在，未知分组现在两种模式都报同一句 `group "x" not found`。`audit verify` 是 `pb.RunVerification` 补了 `repeated AuditChainFailure failures`（trace_id/index/expected/actual/prev_*/type）——线上原先只回 `failures[0]`，多行篡改在远端会看起来像单行；另外「run 没有 trace」线上被 `verify all` 的跳过逻辑吞掉，CLI 现在把空 `runs` 还原成本地那句 `ErrNoTraces` 拒绝。`template show` 是 `pb.Template` 补了 `repeated TemplateParameter parameters`（name/description/required/default/type）——`required_params` 只带必填参数的**名字**，可选参数、类型、默认值与描述此前线上都不存在；CLI 导出 `template.FormatShow` 作为唯一人类渲染，两种模式共用。**已收口（2026-11-04）**：`system status` 的两种模式按「远程显示更少」定案——本地仍报 `config_path` / `db_*`（本机配置指向的库），远程只报 `GetStatus` 的运行事实（`status` / `active_runs` / `paused_runs` / `uptime_seconds` / `store_type` / `warnings`），并用 `local_only` 明确列出不报的字段。理由：`GetStatus` 与 REST `/system/status` 都只做认证、不做策略判定，配置路径与数据库端点是部署拓扑，`GetConfig` 对此有 `ActionAdmin` 门禁；加宽 proto 等于把拓扑放上监控面。`--remote` 白名单随之扩到十条（新增 `system status`）。`audit list` / `audit export` **已接（2026-11-05，白名单现 12 条）**——但接线前必须先修服务端两处：`ListAuditTraces` 原来把存储读取钉在 `Limit: maxPageSize`（=1000）再在这份切片上分页，超 1000 行的 run 在远端**静默少给且 `total_size` 也报截断值**（本地无上限，所以两边不等价而无告警），已改成同文件 `GetAuditLog` 的流式页游走，`state.TraceFilter` 因此新增 `Offset`（SQLite 侧与 LIMIT 同进同出）；`pb.TraceEntry` 新增 `stored_detail`（13，入库原文＝封链覆盖的那份 JSON）与 `tenant_id`（14），因为 `detail`（12）是给人看的**投影**，按投影导出等于导出与自己账本不一致的字节。`audit show` **已接（2026-11-05，白名单现 13 条）**——它不能复用 `ListAuditTraces`：那条只按 **run** 过滤，而 `show` 的入参是一个 **trace ID**，照现有面走等于把一次点查做成"枚举所有 run、逐 run 翻页"的全表扫描，故新增 `AuditService.GetAuditTrace(GetAuditTraceRequest) returns (TraceEntry)`。服务端按 `GetRunReport` 的既有次序**存在性先于可见性**：行不在报 `NotFound`，行在但属于视野外的 run 报 `PermissionDenied` 而**不报"不存在"**（否则这条错误就是探测哪些 trace ID 存在的探针）；空 ID 是 `InvalidArgument` 而不是"不过滤"。作用域取该行所属 run——`trace.run_id` 在两套方言里都是 `NOT NULL` + `FOREIGN KEY … ON DELETE CASCADE`，孤儿 trace 不可能存在，所以没有为它写兜底分支。CLI 两条路径都交回 `*state.Trace`，`--json` / `--quiet` / 人类三个渲染分支共用；远程 `NotFound` 翻译成本地那句 `trace "x" not found`。**顺带修掉上一笔的一个真缺陷**：`pb.TraceEntry` 新增 `timestamp_nanos`（15，字段 8 那个 int64 秒的亚秒余数，即 `google.protobuf.Timestamp` 的秒/纳秒拆分；字段 8 语义不动，web 的 `timestamp: number` 一个都不用改），两个生产者（`traceEntryToPB` 与 `auditToTraceEntry`）都填。此前 CLI 的 `traceFromPB` 用 `time.Unix(sec, 0)` 重建行，而 recorder 盖的是 `time.Now().UTC()`、SQLite 往返保留完整纳秒、PostgreSQL 保留微秒，于是远程 `audit list` / `audit export --json` 对任何真实行都与本地不同——**它一直是绿的，因为每份夹具都写了 `Truncate(time.Second)`：那批"逐字节相同"的断言没有射程，不是正确。** 夹具已换成故意不对齐的瞬间并在比较前先自证本地输出真有 `.123456789`；`cli-reference.md` §7.2 / §7.3 里由上一笔写下的两条"远程只到秒"随之订正。`group list` **已接（2026-11-05，白名单现 14 条）**——按 owner 的口径走**逐字节相同**这一档，而不是 `template list` / `template show` / `target list` 那三处「如实标注的远程只到秒」。缺口是实的：本地 `group list --json` 直接 marshal `state.InventoryGroup`，`created_at` 没有 `omitempty`（所以每行都打，且 `UpsertInventoryGroup` 盖的是 `time.Now()`，带纳秒）、`tenant_id` 有 `omitempty`（真带租户时才出现）；而 `pb.Group` 原来只有 `id`/`name`/`parent_id`，照它重建会给每一行打 `0001-01-01T00:00:00Z` 并丢掉租户列。故 `pb.Group` 补 `created_at`（4，int64 秒）＋ `created_at_nanos`（5，纳秒余数，与 `TraceEntry.timestamp_nanos` 同一种秒/纳秒拆分）＋ `tenant_id`（6）；`groupToPB` 三处都填（`ListGroups` 与 `CreateGroup` 共用这一个映射），CLI 的 `groupFromPB` 用 `time.Unix(sec, nanos).UTC()` 重建。库存适配器**镜像全部 6 条 RPC**（ListGroups/CreateGroup/DeleteGroup/ImportTargets/SetTargetStatus/TargetHistory）而不只接 ListGroups——`targetServiceAdapter` 连 AddTarget/RemoveTarget 都镜像了，半镜像的接口正是下一个命令只写一种模式的原因；`TestInventoryAdapterMirrorsTheWholeRPCSet` 用反射把这条口径变成断言。分组读权限按**部署范围的命名空间**判定（分组不声明环境），所以带策略的部署里远程可能被拒绝，本地不判权。**但这条「逐字节相同」的适用范围要如实收窄**：2026-11-06 实测（postgres:16-alpine，server `TZ=Asia/Shanghai`，宿主 `+08`，写入时刻 `2024-03-17T23:59:30Z`）——SQLite 把库里的 DATETIME 解成 `Location="UTC"`，**PostgreSQL 解成 `Location="Local"`**。同一个 Unix 时刻，本地 `--json` 在 PG 上打 `2024-03-18T07:59:30+08:00`、在 SQLite 上打 `2024-03-17T23:59:30Z`，人类输出的日期也差一天。远程侧只能选一种重建方式（`time.Unix(sec, nanos).UTC()`），所以**两边逐字节相同只在 SQLite 部署上成立**；不存在同时匹配两种方言的按秒重建。同一事实适用于 `audit list` / `audit export` / `audit show` 与 `target list`，登记为 D-3。`target history` **已接（2026-11-06，白名单现 15 条）**——它是这一系列里**唯一**一条远程比本地少字段的命令，按 owner 选的「远程显示更少 + `local_only`」方案做，而不是硬把线上加宽：`InventoryService.TargetHistory` 早已存在（不是新 RPC），但它的 `TargetHistoryEntry` 只带 5 个字段，而本地 `--json` marshal 的是整个 `state.Run`（15 个 json 键）。**「补齐」这条路被本仓自己写下的边界否掉**：`proto/levee.proto` 的 `Change.plan` 注释明言计划正文「never for ListChanges」，`params` / `plan_json` 正是同类大字段，列表 RPC 不携带；反过来**收窄本地**等于把已经交出去的 `--json` 形状改契约。所以远程显示 5 个键、并用 `meta.local_only` 点名缺的 10 个（`template_name`/`params`/`plan_hash`/`plan_json`/`approval_status`/`approval_level`/`priority`/`updated_at`/`incident_id`/`tenant_id`），形状沿用 `system status` 的既有先例。**两个容易看漏的点**：保留的键沿用**本地的名字与形态**（主键仍叫 `id` 而不是线上的 `run_id`；`created_at` 仍是 RFC3339 字符串而不是裸秒计数），否则消费方要按模式分支而不是容忍少键；`--limit` 是 `int` 而线上是 `int32`，本仓 CLI 此前没有任何整型窄化，不做钳位时 `--limit 3000000000` 会静默变成一个不同的（截断成负数则被服务端兜成 20 的）值——正是这一系列要消灭的「操作员输入的和服务端执行的不是一回事」，现按 `math.MaxInt32` 钳。`remoteTargetHistoryLocalOnlyFields` 不是靠注释自觉的：`TestTargetHistoryRemoteJSONIsDeclaredSubsetOfLocal` 读两边的**实际输出**求键集差，声明与实测不符就红（夹具必须带 `tenant_id`，因为它 `omitempty`，空租户会让本地也不输出那个键、于是声明少一个也测不出来）。另有两处**未修、如实登记**：① 本地路径的 `--json` 仍是 15 键全量且 `meta` 为 `null`，本笔不动本地契约；② `TargetHistoryEntry.workflow_name` 存的是工作流**原文**（`runs.workflow_name` 即渲染后的 YAML），所以这条列表 RPC 会把整份文档发给每一个 `target history --remote` 调用方——CLI 两侧都过 `workflowDisplay` 所以显示无碍，但线上载荷偏重，与上面 `Change.plan` 的边界是同一条理由，将来该换成只带声明名。
