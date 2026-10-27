# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 新增（插件系统的第四个引擎消费点：module→channel，`channel` 类型接进通道注册表——"执行面未启用"声明四次分型，四类全接）

- **声明分型**：上笔把 module 接进执行器，边界声明为"notifier + gate + module = 活的执行面；channel = 仍是管理面"。本笔兑现 channel：**四类全是活的执行面**。help/README/cli-reference 三处同步改写；`docs/channel-plugins.md` 第 7 节从"通道注册是进程级的、动态插件加载在设计 backlog（V2）"改写为"两种接入方式（编译期/运行期）"对照表——那句 backlog 现在已经不成立了。
- **这是唯一一种「会话」消费点**：门禁是单次检查、模块是单次执行，通道是 connect 一次、exec/collect 多次、disconnect/close 收尾。前者用一次性子进程是对的；通道用一次性子进程会把 NETCONF 对端/跳板机刚建立的会话状态丢掉。所以新增 `internal/plugin/session.go`：常驻子进程 + 三条不变量——**请求串行化**（一条管道一次一条；Channel 契约明确允许并发 Exec，所以加锁而不是让写入交错）、**失败即判死**（超时/取消/协议违例后流无法再同步，下一次读会拿到上一题的答案，所以杀进程、IsConnected 转 false、由调用方重拨）、**收尾不等在途请求**（两把锁：请求等轮次，teardown 绝不等请求——否则一次永不回答的执行会让 Close 挂死）。
- **协议扩展（向后兼容）**：动作集加 `connect`/`exec`/`collect`/`disconnect`；`PluginResponse` 加 `exec`（复用 `types.go` 已声明的 `ChannelExecResult`）与 `collect`（base64 载荷）。回 ok:true 却不带对应载荷 = 无 verdict，判失败。
- **`plugin.PluginChannel`（channel.Channel）+ `PluginChannelFactory`（channel.ChannelFactory）**：Connect = 起会话 → init(安装期配置) → connect(寻址)；Exec 映射 exit/stdout/stderr/duration，**非零退出码是结果不是错误**（与 SSH 通道同口径），插件拒投/无载荷与 ctx 取消返回 `ExitCode -1`（绝不把失败伪装成 0）；Download 走 collect 解码；**Upload 按名拒绝**（声明的 ChannelPlugin 契约没有 upload 动词，`file.copy` 类步骤在运行期通道上不可用——明说，不假装）；Close 幂等且永不因收尾失败而报错。
- **明文凭据永不过管道**：新增 `channel.CredentialRefNamer` 可选接口（`liveTarget` 实现，暴露 inventory 行的引用**名**），插件拿到的 connect 参数只有 host/port/type + `credential_ref` 名字，扩写后的 `CredentialRef`（含口令）一个字段都不传。密钥走插件自己的安装期 `config.yaml`——这正是"凭据零泄露/传输原生凭据"规则对子进程形态的落地。子进程人格在测试里对 `credentials` 键直接拒连，边界被钉住。
- **serve 装配**：`installPluginChannels` 加入既有的 `installPluginExecutionSurfaces` 接缝（`buildServeServices` 的圈复杂度预算仍不许第三个独立分支）——注册表缺失=未配置；meta 握手失败/二进制消失=拒绝启动点名 + 逃生门；**插件名撞已注册类型（ssh/winrm/local…）=拒绝启动**（`Register` 按设计覆盖同名项，静默替换 `ssh` 会把所有写它的 workflow 引到插件上）。
- **测试**：`internal/plugin/channel_test.go` 13 例（记录三态拒绝 / 二进制消失 / 类型名=插件名 / 寻址含引用名且不含明文 / Connect 幂等 / 未连接即拒活 / 结果映射含非零退出 / 拒投浮出并带 -1 / 无载荷拒绝 / upload 按名拒绝 / collect 解码 / Close 幂等 / **取消即判死** / **进程中途死亡即断开** / 并发 Exec 串行化且各得各答）；`protocol_test.go` 加 `channel` 子进程人格（会话型：多请求一进程）；`cmd/levee/serve_plugin_channel_test.go` 5 例；`internal/wiring` 钉住 `CredentialRefName` 暴露的是引用名而非扩写凭据。
- **验证**：build/vet/gofmt/golangci-lint 干净；plugin/channel（含三传输）/wiring/cmd/levee 全量绿。

### 新增（插件系统的第三个引擎消费点：gate→module，`module` 类型接进执行器——"执行面未启用"声明三次分型）

- **声明分型**：上笔把 gate 接进门禁体系，边界声明为"notifier + gate = 活的执行面；module/channel = 仍是管理面"。本笔兑现 module：**notifier + gate + module = 活的执行面；channel = 仍是管理面**。help/README/cli-reference 三处同步改写。
- **协议扩展（向后兼容）**：`PluginResponse` 新增 `module` 字段承载 `ModuleResult`（`types.go` 里早已声明的插件侧契约：exit_code/stdout/stderr/changed/duration_ms），与 `result`（meta/check）并列；`HostRequest` 的动作集加入 `"validate"` 与 `"execute"`。**execute 回 ok:true 却不带 module 结果 = 无 verdict，判失败**——空成功不是成功（同 gate 空 verdict 的同类，fail-closed 一以贯之）。
- **module 声明走 meta**：`details.actions`（必填，动作表）与 `details.idempotent`（可省，缺省 false）——沿用协议里 meta 的既有位置（示例插件把身份放在 `result.details`），零新增封装。**未声明动作表的模块被拒绝**：一个不接受任何动词的模块注册了也只会失败，接受它等于登记一个必然失败的能力；幂等缺省 false 是 `executor.IsIdempotent` 的既定保守口径。
- **`plugin.PluginModule` 适配器**：把启用的 module 记录适配成 `executor.Module`——`Name()` 用注册表名、`Actions()` 用 meta 声明（未知动词在执行前被 executor 拒绝）、`Idempotent()` 用声明值、`Execute()` 起一次短命子进程（init 交安装期配置 → **validate 交动作与参数**（声明接口使 Validate 必选，插件不支持就在此拒绝而不是静默跳过）→ execute → close），5 分钟死限（比通知的 30 秒长，因为模块动作会改基础设施；仍是上限不是目标）。
- **channel-free 模块**：`executor.ChannelFree` 可选接口（`NeedsChannel() bool`）。插件模块自持传输（厂商 API / 云控制面），`runExec.dispatchTarget` 在拨号前询问，声明 false 时**只取库存行作为寻址数据、不建会话、不解析凭据**——为一个不会用到的会话去拨号，成本是连接，风险是为一桩无关原因让步骤失败。内建模块不实现该接口，契约零变化。
- **serve 装配**：`installPluginModules` 在 `buildServeServices` 里紧随 notifier 装配——注册表缺失=未配置态（no-op）；打不开=拒绝启动；启用的 module 插件 meta 握手失败 / 二进制消失=拒绝启动点名 + 逃生门（`levee plugin disable <名>`）。**注册是 boot 期一次性的**：executor 是静态注册表，运行中出现的模块对已声明它的 step 仍是未知模块（与 gate resolver 的"每次现读注册表"刻意不同，理由在实现里写明）。
- **同名保护**：插件名与内建模块同名（如 `shell`）**拒绝启动**。`RegisterModule` 按设计覆盖同名项（测试靠它换模块），所以碰撞检查必须在注册之前——静默替换 `shell` 会换掉所有 workflow 赖以书写的能力。
- **两条明写的边界**：①插件模块不在编译期「固有不可逆」词表（`internal/dsl/inherent_irreversible.go` 是静态表，插件动作在运行期才知道），**破坏型插件步骤须由作者显式 `irreversible: true`**，否则不触发不可逆级别的审批要求——这是本笔 knowingly 留下的缺口，已登记为后续项；②`agent` 型目标在远端 agent 进程里执行模块，serve 侧注册的插件模块在那里不可达。
- **测试**：`internal/plugin/module_test.go` 8 例（记录三态拒绝 / meta 声明面读取 / 未声明动作表拒绝 / 二进制消失拒绝 / 结果映射含 changed+duration / 插件拒投浮出 / validate 前置拒绝 / 参数线格式含 target 与空输入）；`protocol_test.go` 加 `module` 与 `module-noactions` 子进程人格（init 前置条件让配置交接可观测）；`internal/wiring/exec_channel_free_test.go` 钉住三分支（声明 free / 无接口 / 未知模块）；`cmd/levee/serve_plugin_module_test.go` 5 例（缺注册表 no-op / 启用注册且声明 channel-free / 其他类型在碰二进制前被过滤 / 同名拒绝 / 二进制消失拒绝带逃生门，新增 TestMain 子进程人格）。
- **验证**：build/vet/gofmt/golangci-lint 干净；plugin/executor/wiring/cmd/levee 全量绿。

## [v1.23.0] - 2026-10-27

> **发布状态：未切版。** 本节共 **38** 个小节，六条主线：① **插件系统**走到两个真执行面（notifier 装配进通知管理器、gate 接进验证链与 plan 期可执行性判定），`plugin test` 给出最小活证据，文档与 CLI 的执行面声明二次分型；② **通知与告警链路**补齐——drift 常驻巡检接进 serve、run 生命周期全量 webhook 镜像、告警每源入口身份与按归属可见、审批超时第一次生效、门禁失败进聊天；③ **授权与安全**——`DecisionGuard` 补上两处绕行、CLI/ChatOps 投票走服务层且匿名被拒、清单 env 归属有事实源、ChatOps 重放防护、被服务而未判定的三个面穷举收口；④ **供应链与工具链**——Go 1.26.10 收口 GO-2026-6809、codeql 锁步、前端生产依赖 high 清零、vitest 3→5；⑤ **词表单一来源与控制台/CLI**——docgen 的生成表从 2 张长到 12 张（两笔增量 + 四套取值 + 告警 severity）、控制台字段收成严格联合、详情页存量计划、密度档位、`--as` 本地可判路径；⑥ **正确性、验证基建与协作口径**——`{target}` 展开、审批与结果列的数据卫生、未链入包守卫、windows 崩溃证据自动留档、浏览器假红自证、§5.4 线性落地守卫与 rebase 落地口径、账本两处订正。

### 新增（插件系统的第二个引擎消费点：notifier→gate，`plugin` 门禁类型接进验证链——"执行面未启用"声明二次分型）

- **声明分型**：上笔把 notifier 接到通知管理器，边界声明同步改写为"notifier=活的执行面；gate/module/channel=仍是注册表管理面"。本笔兑现 gate：**notifier + gate = 活的执行面；module/channel = 仍是管理面**。help/README/cli-reference 三处边界声明同步更新。
- **DSL 语法**：`yamlGateRaw` 新增 `plugin: {name: <插件名>}` 声明块；`convertGateCheck` 产出 `GateCheck{Type:"plugin", Plugin:<名>}`；LE051 文案加 plugin。门禁参数（params）与 probe/slo/human 同构——声明原样透传，gate 实现按自身契约 fail-closed 校验。
- **plan hash 纳入插件名**：`canonicalGateCheck` 新增 `Plugin` 字段（omitempty，历史哈希字节不变）。审批对的是"哪个插件执行检查"——改插件名或插件后批准，计划哈希不同，`ApproveChange` 的 plan-hash 匹配不会误放行。
- **verify.PluginGate**：实现 `verify.Gate`，`Check` 时经 resolver 解析插件名 → `plugin.Probe` 起一次子进程（init 交注册表配置 + check 交声明参数 + close），响应映射 `Result.Passed` → `GateResult.Passed`，消息/细节透传。**无 verdict 的 ok:true 判为失败**（同 cmd 门禁空输出的同类）——fail-closed 是这条链的铁律。`timeout` 声明注入 `timeout_seconds` 参数，插件自决。
- **engine 可执行性判定**：`GateRuntime.Plugins`（resolver）是 plan 期拒绝的判据——无 resolver 时声明 plugin 门禁的计划在 GeneratePlan 即被拒（`ErrGateNotExecutable`，命名 gate 与缺失配置），同 slo 无 PrometheusURL / human 无 approver 的既有形状。`gateFromCheck` 新增 plugin 分支；default 拒绝文案加 plugin。
- **wiring 注入**：`WithGatePluginResolver` Option + `gateRuntime()` 传参。serve 侧 `pluginGateResolverFromRegistry` 在 `buildEngineServeOptions` 装配——每次解析读注册表（插件可在计划与执行之间被启用/禁用，解析失败即按次失败，不拿 boot 时快照）。
- **serve resolver 语义**：注册表文件缺失 = 该部署无法解析任何插件名（错误点名原因）；记录存在但类型非 gate / 未启用 / 二进制消失 = 逐项可读拒绝并给逃生门（`levee plugin enable/disable`）。这是"注册表文件即开关"的延伸——安装+启用=选择加入，与 notifier 通道同构。
- **测试**：`internal/verify/plugin_gate_test.go` 7 例（通过/失败映射 / 拒投浮出 / 解析失败 / nil resolver / 垃圾输出拒绝 / timeout 注入钉死）；`internal/engine` 表驱动 + materialize 2 例（无 resolver 拒绝、有 resolver 注册）；`internal/dsl` plugin 门禁解析；`internal/plan` 插件名进哈希；`internal/wiring` 拒绝 + 接通对偶。protocol_test 加 checker 子进程模式。
- **验证**：build/vet/gofmt/golangci-lint 干净；dsl/plan/verify/engine/wiring/plugin/cmd/levee 全量绿（cmd/levee 179s）。

### 新增（插件系统的第一个引擎消费点：notifier 插件接进 serve 的通知管理器——"执行面未启用"的声明从此分型）

- **兑现上笔的承诺**：上一笔把"执行面未启用"写成明说的边界，并注明"接入时本声明同步撤销"。本笔接入四类消费点里最薄的一类（notifier），声明同步分型：**notifier=活的执行面；gate/module/channel=仍是注册表管理面**（help/README/cli-reference 三处同步改写，不再是笼统的"未启用"）。
- **接缝与语义**：serve 启动时 `buildServePluginNotifiers` 从注册表读出全部**启用状态**的 `notify` 型插件，经 `plugin.PluginNotifier` 适配成 `notify.Notifier`，注册进与 webhook 同一个管理器——run 生命周期镜像、漂移告警、回滚评级走同一路由，操作者装的通知渠道与内建渠道平权。**注册表文件本身就是开关**（安装+启用=选择加入），不另设配置节——与 alert-sources.yaml"admin 管理的文件即开关"同构。注册表文件缺失=未配置态（nil）；存在但打不开=拒绝启动（静默跳过会让投递断得无声无息）。
- **交付一次 = 一次子进程**：每次 Send 起一个固定 argv 的短命进程（init 交注册表里存的安装时配置 → notify 交消息 → close），30 秒死限（比握手长，因为插件可能有自己的上游网络 I/O），ctx 取消与超时都会杀进程。**刻意不用 Sandbox**：沙箱的价值（崩溃监控/重启/cgroup）属于常驻进程，一次性投递用不上——与 `plugin test` 的 Probe 同一判断，本轮已把两者共用一套 `pluginProc` 运行器（protocol.go 重构，Probe 行为零变化、其测试原样通过）。
- **boot 期与投递期的失败分级**：boot 时二进制已消失=渠道不可用，按 webhook 规则**拒绝启动并点名 + 给逃生门**（`levee plugin disable`）；boot 后的失败（上游 5xx、协议违例、进程崩溃）=**按次投递错误**，由管理器聚合上报，不拖垮 serve。gate 型插件永远不会被当成通知渠道装配（类型过滤，装配测试钉住）。
- **消息映射**：`notify.Message` → `NotifyMessage`（插件接口的 JSON 契约原样：event/run_id/level/title/body/recipients/metadata，recipients 折叠为 ID 列表，level 用词表字符串）。
- **测试**：`internal/plugin/notifier_test.go` 5 例（真子进程交付+config 交接 / 插件拒投浮出为错误且带插件原话 / 记录校验三态 / 二进制消失=按次错误 / 线格式映射钉死）；`cmd/levee/serve_plugin_notify_test.go` 4 例（注册表缺失=未配置 / 启用通知插件→按名装配 / gate 型被过滤 / 二进制消失→拒绝启动点名+逃生门）；protocol 重构回归 9 例全绿。
- **验证**：`go build`/`vet`/`gofmt` 干净；`internal/plugin`、`cmd/levee` 全量绿。

### 新增（drift 常驻巡检接进 serve：`drift.schedule.enabled`——CLI 写的任务文件第一次有人在后台跑）

- **补的缺口**：`DriftScheduler` 与 `levee drift schedule add/list/remove/run` 早已齐备（含 cron 解析、任务文件持久化、e2e），但 serve 从不 import drift——任务文件躺在 `<dataDir>/drift/jobs`，**没有常驻进程去跑它们**：serve 部署的漂移只在有人手动跑 CLI 时才被检测，"定时巡检"在有服务端的部署里是空转的。
- **一个开关，一个语义**：`drift.schedule.enabled`（默认 false，既有部署逐字节不变）。开启后本 serve 进程在后台跑这些任务；**任务本身不进配置**——继续由 `levee drift schedule add` 管理，CLI 与 serve 共用同一份任务文件（one job store, two consumers），两边不可能对"调度了什么"产生分歧。这与 `ai.llm`、`diagnosis.topology` 的"配置节=开关，数据=磁盘文件"先例同构。
- **装配三态**（`buildServeDriftScheduler` + runServe 装配点）：关闭 → 不构造（历史姿态）；开启有任务 → `Start` + defer `Stop`（进程退出前优雅停）+ 启动日志报任务数；**开启但零任务 → 调度器不启动**，WARN 点名"去 `levee drift schedule add` 建任务"——空跑一个永不触发的 goroutine 是在假装巡检。
- **顺带修掉一个藏在集成处的真缺陷**：`DriftDetector.sendDriftAlert` 构建的告警消息**没有 Recipients**，而 `NotificationManager.Notify/NotifyAsync` 的校验对空收件人直接拒投（`ErrNoRecipients`）——即装上 notifier，任何 drift 告警也到不了任何渠道（本笔接线测试当场抓出）。修法取 `run_lifecycle` 同款先例：渠道广播语义（`RecipientChannel("drift","drift")`），漂移告警面向渠道而非个人。
- **测试**：`cmd/levee/drift_serve_wiring_test.go` 4 例钉装配（关闭=不构造 / 开启=CLI 任务文件全部加载且 Start/Stop 真活 / 开启零任务=空调度器交由调用方决定 / **notifier 端到端**：真 NotificationManager + 计数渠道，RunOnce 一次必达）。调度器自身 cron 行为不动（`internal/drift` 已有覆盖）。
- **验证**：`go build`/`vet`/`gofmt` 干净；cmd/levee、internal/drift、internal/config 全绿；`config.example.yaml` 新增 `drift:` 段。

### 新增（`levee plugin test`：插件链路的最小真执行——一次活的协议握手，把"装了→启用了→真的能跑"钉成可验证事实）

- **与"插件执行面未启用声明"那条的关系**：那条把执行面缺位从暗雷变成明说的边界；本条在同一前提下给出最小的**真**执行——引擎消费点（gate/notifier 接入）仍是独立决定，但"安装→启用→执行"链路从此有命令可验，不再只有注册表里的元数据。
- **协议落地为代码**：示例插件（`examples/plugins/http_probe`）声明的 JSON 行 stdio 协议有了宿主侧实现（`internal/plugin/protocol.go`：`HostRequest`/`PluginResponse` + `Probe`）。`Probe` 用**固定 argv** 的子进程（`&exec.Cmd{Path, Args}` 字面量、无 shell、无命令行参数——插件指令只经 stdin 传输），路径先经 `resolvePluginBinary` 收紧（绝对化 + 必须是已存在的普通文件，拒绝目录/空串）；10 秒死限，超时杀进程并报"未应答"而非挂死。
- **刻意不用 Sandbox**：沙箱假设常驻 gRPC 形态（它丢弃 stdout）且其价值（崩溃监控/重启/cgroup）属于长生命周期进程；一次握手用固定 argv + 管道 + 死限才是诚实的工具。注释写明。
- **CLI**：`levee plugin test <name>`——仅对 `enabled` 状态的插件（否则点名先 enable）；报告插件身份应答（meta details）与往返耗时，`--json` 同形。
- **测试**：`internal/plugin/protocol_test.go` 4 例，其中活的握手用**测试二进制自我重执行**当子进程（环境变量选分支，与 agent 注册表双进程 E2E 同款先例）：真握手往返 / 非 JSON 输出必须被拒（不是解析成误导性零值）/ 三种不可用路径拒绝 / 握手请求形状钉死。
- **验证**：`internal/plugin` 全包绿；`go build`/`vet`/`gofmt` 干净。

### 新增（审批票判定补上服务路径之外的缺口：`approval.DecisionGuard` 管 gate 投票与移动深链）

- **与"CLI 投票走 serve 的同一条路"那条的关系（同题分工，不是重复实现）**：CLI/ChatOps 的投票已改走 `ChangeService.ApproveChange`/`RejectChange`（服务层判定 + quorum 服务端强制）。但仍有两处投票面**不走服务层**：`levee gate approve/reject`（human 门禁决定，直调领域服务）与**移动深链**（serve 进程内 `MobileApprovalService` 直调 `approvalSvc`——它的 REST 路由豁免网关鉴权，token 证明持有人，而绑定的 userID 此前不经注册表判定就能投出有效票）。本笔给领域服务补一个可选判定缝，把这两面收进同一张矩阵。
- **接缝**：`approval.Service` 新增可选 `DecisionGuard` 窄接口（依赖倒置——approval 包不 import authz），`decide()` 在记录级检查**之前**先问 guard，nil guard = 逐字旧行为。guard 装在 serve 的 `approvalSvc` 上（`authz.Load` 装配点，与 `changeSvc.WithAuthorizer` 同处；钉子测试 `TestServeInstallsTheApprovalDecisionGuard` 结构断言装配行不丢——丢了不影响任何行为测试，这正是要钉子的原因），深链与 gRPC `ApproveChange` 内层的领域调用随之受判（后者是纵深防御，RPC 层判定保留在前）。gate 的 CLI 构造点改走 `newApprovalService`（helpers.go，`--as` 生效带 guard）。
- **gate 的判定姿态与 approve/reject 刻意不同（opt-in 而非强制）**：`gate approve/reject` 是 run 执行中的人在环决定——操作者已在引擎会话内，且强制 `--as` 会 breaking 现存 gate 脚本；治理动作（approve/reject）已按前条定为强制。差异写在 `newApprovalService` 的注释里。
- **适配器 `grpc.ApprovalDecisionGuard`**：判**投票人**于**变更的环境**（`envOf(run)` 与 `authorizeResource` 同口径；run 缺失按 `default_env` 判，不是绕过）；拒绝走 `RecordDenial`（SA-007：只存在于客户端报错里的拒绝是没法审计的拒绝）并带 `levee authz explain` 指引；store 读失败 fail-closed；`approval.Action*` 与 `permission.Action*` 同拼法零映射。
- **行为变更（迁移说明）**：开 `permissions.yaml` 的部署里，**移动深链审批人必须进注册表**（`users.yaml`），注册表不认识的深链用户投票被拒并得到 explain 指引——这正是矩阵语义，此前是该语义的漏判。未开矩阵、CLI gate 不带 `--as` 的部署零变化。
- **测试**：`internal/approval/service_guard_test.go` 6 例（拒绝否决且记录不动 / guard 先于名单——不在名单的人拿到的也是策略拒绝 / nil=旧行为 / 放行且参数正确 / Reject 独立动作可单独拒 / `WithDecisionGuard(nil)` 可拆装）；`internal/grpc/approval_guard_test.go` 6 例（矩阵放行 / 环境未授权拒绝带 explain 指引 / 未注册拒绝 / nil Auth=无策略姿态 / run 缺失按 default_env 判且不成为绕过 / store 读失败 fail-closed）；`cmd/levee/approval_guard_wiring_test.go` 2 例（同一 fixture 唯一差异是 `--as` 旗标）。
- **验证**：`go build`/`go vet`/`gofmt` 干净；approval、grpc、cmd/levee 相关测试全绿。

### 修复（文档与 CLI 对插件系统说了它做不到的话——"执行面未启用"从此是明说的，不是推断的）

- **不实承诺的三个位置**：`levee plugin --help` 原文"Plugins extend the engine with custom channels, gates, modules and notifiers"、`docs/cli-reference.md` 第 16 章"管理 LEVEE 插件（Channel/Gate/Module/Notifier 四类接口）"、`README` "插件注册表开放第三方通道接入"——而引擎（engine/executor/channel/notify/verify 五个消费侧）没有任何插件消费点，`plugin install` 之后引擎行为**不变**。管理面（注册表/沙箱/签名验证，`internal/plugin` 2,148 行）是真实的，执行面不存在：用户可以装、可以 enable、可以在 list 里看到它，然后它什么都不做——"表面可用、实际空转"正是本仓账本反复批判的装饰性字段形态。
- **三处改法**：CLI help 与 16 章开头改为如实声明边界（管理面可用、执行面未启用、装了不变），并把"今天就要扩展引擎"的用户指向**编译期通道插件**（`docs/channel-plugins.md`——`channel.ChannelFactory` + init 注册，那是另一套机制且真实可用）；README 那句原本就把"运行时插件注册表"与"编译期通道插件"混在一句里并引错文档，现按两套机制分述。
- **刻意不做**：不给 help 文本加"即将支持"之类的时点承诺（执行面接入是独立决定，接入时本声明同步撤销）；不改 `internal/plugin` 代码本身——它是为执行面就绪的管理面，声明边界即可，退役与否是另一个决定。
- **验证**：`go build ./cmd/levee/` 干净；help 文本变更不影响行为测试。

### 修复（CLI 投票走 serve 的同一条路：`--as` 现在真正约束 approve/reject，ChatOps 投票获得服务级审计）

- **账本登记的最后一条边界收口**：#203 写明"`--as` 只到走服务层的动作；**approve/reject 的实际投票走的是领域服务 `approval.Service.Decide`，那里没有 authorizer 参数，因此不受 `--as` 约束**——属接口变更，另行决定"。本笔按"不加接口参数"的定案收口：**CLI 投票改走 `ChangeService.ApproveChange` / `RejectChange`**（serve 的同一条路），本地上下文带 `ContextWithSubject(currentActor())` 的已验证主体——subject.go 的本地模式条款明确允许这一点（"the process itself is the authority on who typed the command"）。
- **三处行为变化（账本预告过，全部落地）**：① 本地投票**要求已验证主体**——不给 `--as` 时投票被拒，报错与远程调用方看到的逐字相同（"a shared --token authenticates the deployment, not a person"）；**这是 breaking**：依赖匿名 `levee approve` 的脚本要么给 `--as`，要么改用远程身份。② 结算与审计**由服务完成**——旧的"decide + 第二次 settle pass"去掉了（`settleApprovalFromCLI` 不再被 approve/reject 调用，只剩 ChatOps 用），**服务级 audit 行第一次有 CLI 投票**（旧路径只有 CLI 自己的输出）。③ **`--level` 保留为预检**：服务自己选第一条 pending approval、不认档位，operator 点名档位却被静默投到别的档位上比精准拒绝更糟——预检在投票前拒绝。
- **ChatOps 同步改造**：`chatops approve/reject` 也改走同一条路。它的身份来自平台 `identity_map`（MAC 验签的回调），**不需要 `--as`**——回调用 `ContextWithSubject` 立主体（subject.go 本地条款同款），策略接 `chatopsAuthorizer()`（矩阵缺失=WARN+不判，加载失败=WARN+不判：**让紧急 ChatOps 否决因辅助文件损坏而不可用，等于用一次有审计的决定换一次不可用**；这个取舍写在函数注释里）。输出契约保留（`approval_id` 仍先解析、`--quiet` 仍打它）。
- **E2E 抓到一个我引入的真 bug（这条测试的价值现场）**：改造首版给 `ChangeService` 的 approval 参数传了 nil，`ApproveChange` 于是走 **legacy 直改路径——绕过 quorum 直接 approved**，`TestApproveE2E_PartialQuorumKeepsRunPending`（1/2 票不得批准）当场红。修复：CLI 与 ChatOps 都传 `approval.NewService(newApprovalStoreAdapter(store))`（与 serve/`newCLIChangeService` 相同）。**quorum 门禁从此在这条路上被服务端强制，而不是靠调用方自觉。**
- **验证**：`go build ./cmd/levee` rc=0；`vet` rc=0；`gofmt` 干净；`cmd/levee` 全包 ok（124s，含 Approve/Reject/Settle/Chatops/Quorum E2E 全家）。`--level` 预检、quorum stderr 提示（改从返回的 run 状态判：partial quorum 留在 pre-decision 状态）、`--quiet`/`--json` 输出契约全部保留。

### 新增（清单侧 env 归属的事实源：`target-ownership.yaml`——"这台机器真的属于这个环境"从此是配置，不是声称）

- **登记的产品决定已定形**：归属采用**主机清单式**（而非凭据归属式），形态与本仓自己的先例同构——`alert-sources.yaml` 用"每源凭据证明谁在说话、登记项决定它属于哪个 env"，本笔是"清单决定这台主机属于哪个 env，而不由载荷自报"。选清单式的理由：**迁移是一份文件**（存量主机一次性导入），而凭据式要重新配发每台机器的凭据；且归属文件是 admin 面，一次审计一目了然。
- **语义（三条，都写进了文件注释）**：
  1. **优先级**：registry 里的归属**压过** `labels["env"]`——它是唯一一个请求中任何东西都无法断言的输入，这正是这个文件存在的全部理由；labels 降级为"registry 未登记主机时的声明"。
  2. **未登记主机**：回落现状（labels → default_env），**不拒绝**——否则存量部署一升级就大面积失败。
  3. **文件**：`<dataDir>/target-ownership.yaml`（与 `alert-sources.yaml` 同目录同风格）。缺失 = labels-only（可选文件姿态）；**坏文件拒绝启动**（与矩阵/策略同口径：选择了加入又写坏，静默降级正是要避免的结果）。键是 **hostname** 而非 target id：id 是生成物（`tgt-…`）、重注册会变，hostname 是操作员知道的、告警 `instance` 标签携带的、跨导入存活的那个标识。同一 hostname 声明两次是错误——"对一个主机有两个事实"正是这个文件要终结的歧义。
- **接入缝是一个点，不散改**：`grpc.TargetEnvResolver(store, ownership)` 加了第二参（diagnosis 门禁与可见性判定的 env 来源）。解析顺序：registry（raw target 先查一次，hostname 拼写的请求不必先落库）→ 行的 labels → authz 的 default_env。`nil` registry 就是**逐字**的旧 resolver（现有测试零改语义，仅补 `nil` 实参）。
- **serve 侧**：加载失败拒绝启动；加载成功打一行 host 数。`InventoryService` 的写路径**不变**——本笔给的是**读侧的归属事实**，不改谁能登记主机（那是 #200 已收口的另一条线）。
- **测试与变异**：`inventory` 5 例（缺失=空 / 解析+大小写归一 / 三种残缺拒绝 / 不可解析拒绝 / nil 安全）；`grpc` 4 例——**"registry 胜过 labels"**（含 id 拼写与 hostname 拼写殊途同归）、**未登记主机回落 labels**（registry 不能把它不知道的行变成未知）、**nil registry = 逐字旧 resolver**（含 unknown host 仍是 known=false 的拒绝场景）、**registry 能回答 store 里还没有的主机**（operator 先写归属、后导入清单——"还没入库"不是回落到自报的许可）。**变异**：反转优先级（labels 压过 registry）后，恰好只有"registry 胜过 labels"一条红、其余三条仍绿——正是这条守卫承重的形状。
- **验证**：`go build ./...` rc=0；`inventory` + `grpc` 两包全绿；`gofmt` 干净。

### 新增（“未链入包 8→0”从手工做到变成机器守着）+ 订正一个我自己的假结论

- **为何要守一个已经做到的事**：2026-09-29 那次审计找出 **8 个** internal 包没被任何东西链进 `./cmd/levee`——4,691 行非测试代码、自己的测试还都过（90.4–95.4% 覆盖），而没有任何调用方。当天清到 0，账本把这条件记作已完成。**但没有任何东西在检查它**，所以下一个“停止被链接”或“新增且从未链接”的包会静静到达，而账本会继续声称 0。
- **守的是“可达性”，不是“包内死代码”**（这两件事常被混为一谈，而本仓对后者已有定案）：文件级“生产不调它”是另一个问题——`golangci` 的 `unused` 开着，且 `.golangci.yml` 明确豁免 test-only 助手，所以“只有测试在用”是**被设计允许的**。`go list -deps` 回答的正好是这个守卫要问的：**编译器到底碰不碰得到这个包**。
- **落地**：`cmd/levee/every_internal_package_is_reachable_test.go`。用**导入路径**而不是相对模式（`go test` 的工作目录是包目录，`./cmd/levee` 在那里会解析不到——那正是一条扫描静默变空扫描的途径）。
  - 断言的差集**恰好等于**一张**带理由的白名单**（而非一个数字：数字会让“一个离开、另一个进来”看不见）。当前仅 2 条：`internal/agentboard` 与 `internal/docgen`，两者都是 `go run` 工具，链进二进制等于给每个部署白送代码。
  - **两个方向都断言**：新出现未链接包 → 红；白名单里的包**已变成被链接** → 也红（**陈旧的豁免就是豁免表失去意义的开始**）。
  - **非空性下限**（依赖 >400、internal 包 >40，实测 744 / 68）防的是“列表被截断后看起来全部已链接”。
  - **变异已验证**：新建一个无人 import 的 `internal/zzz_probe` 后守卫立刻红，报错点名该包并给出修法（改完即删）。
- **订正一个我自己的假结论（比没有结论贵）**：同一批我在 CHANGELOG、账本与 PR 正文里写“`cmd/levee/service_factory.go` **整份是死代码**（640 行，文件外零调用）”——**那是错的**，依据是一条 `grep -v _test`，而**这个 grep 恰好藏掉测试的使用**：`cmd/levee/grpc_client_test.go` 在用（local + remote 两模式都测）。仓库的 `unused` 检查器开着且 CI 绿，本来就是“不存在真未使用符号”的证据。已按实测改正行数与措辞；**“生产路径不经过它”这个结论不变**，它仍是那批最初接在那里不生效的原因。
- **把两条学费写进 `CONTRIBUTING.md` 的自检清单**（都来自本批，不是一般性提醒）：
  1. 改动使某条**既有声称**不再成立时，**全文复核该结论出现过的每一处**，不只改 grep 命中的那一处——本批一共订正 **6 处**过期账本，其中一处就是“同一结论在一行里写了两遍、改了一半”，于是新写的订正与旧句子在同一个单元格里互相矛盾。
  2. 下结论前检查**取证手法本身**：`grep -v _test` **不能**用来推出“某符号无人使用”。
- **验证**：新守卫本机 `--- PASS (6.46s)`；变异后 `--- FAIL` 并点名 `internal/zzz_probe`；`CONTRIBUTING.md` 与账本均为追加，账本表格列数逐行与 HEAD 对比零变化（新行为 4 列，与表头一致）。

### 新增（windows 崩溃的证据面：把“复发时要取的”从手取变成自动留档，不动判定）

- **背景**：账本里那条 `internal/calendar` 在 windows-latest 上以 `exit status 0xc0000005` 偶发崩溃的记录，自己就写着痛处：崩溃时 **Go runtime 一个字都没打**（无 panic、无 goroutine dump、FAIL 行前后无输出），所以前两次排查都是**按 attempt 手工取日志**重建的。
- **两件它确实留下、而测试日志里根本没有的东西**：
  1. **Windows 的 Application Error 事件**（Event ID 1000/1001）——它记录**故障模块与异常码**。取结构化 `Properties` 而**不是 `Message`**：后者是**本地化文本**（runner 是 en-US，开发机不一定），前者在任何语言下都是同一组字段。已在本机实跑验证命令可用。
  2. **出事的那个包的重跑**。账本点名要 `go test -count=5 -v ./internal/calendar/`，因为“是不是确定性的”是第一个问题，而别的东西回答不了它。本机实跑 2.8s 通过，所以这是一条便宜且安全的证据步骤。
- **刻意不是什么**：**不是重试、不是跳过、不是放宽断言**。它只在**失败之后**跑（`if: failure() && matrix.os == 'windows-latest'`），`continue-on-error: true` 保证它**不改变判定**，也不动那条腿断言的任何东西。
- **为何这笔不算“改配置凑绿”**（账本明列不做的三件事之一）：三件事分别是不把驱动升级当修复、不加重试、不把 windows 腿从 `-cover` 摘掉——本笔三件都没做，只是**把一次复发后要取的证据变成自动的**。"未复现"这个结论本身一字未改。
- **验证**：`ci.yml` YAML 合法且两条新步骤的 `if` / `continue-on-error` 按预期解析；`go test -count=5 -v ./internal/calendar/` 本机 rc=0（2.8s）。

### 修复（浏览器检查在 windows 上红了：真因是 **Chrome 没起来**，不是页面没稳定）

- **拿到了一次带自诊断的真实现场**（`#197` 加的那套第一次真的用上）：master `7e7b505` 的 push CI 在 `browser-check (Chrome on windows-latest)` 变红。但现场是**与账本记的不同的失败模式**——
  - 无截图、无 `settle note`、也没走到任何断言；从 `launching Chrome` 到作业结束只过了 **38 秒**（完整跑要几分钟），日志尾部是：
    ```
    TimeoutError: Timed out after 30000 ms while waiting for the WS endpoint URL to appear in stdout!
        at ChromeLauncher.launch (.../BrowserLauncher.js:181)
        at async browser-check.mjs:379:17
    ```
  - 也就是说：**Chrome 进程本身没在 30 秒内就绪**，而账本登记的那个 flake 是“页面 30 秒没稳定下来后断言读到空 DOM”。**两者根因不同、处置也不同**。
- **为何敢把超时改大，而 `SETTLE_DEADLINE_MS` 不改**：这两个预算的性质不同，而区别恰好就是理由——
  - `SETTLE_DEADLINE_MS` 决定“何时停止观察页面并对眼前的东西下断言”，改大它等于削弱断言的含义；它是**判定代理**，不动。
  - `CHROME_LAUNCH_TIMEOUT_MS` 只是在等一个**外部进程**打印它的 websocket 端点，没有任何断言依赖这个耗时，改大**不可能**把错的结论变成对的。
  - puppeteer 默认 30s（为闲置开发机定的）；本机实测启动 **0.4–1.2s**。取 90s，是对“冷文件系统”的余量，不是对“竞争机器”的估值；可用 `BROWSER_CHECK_LAUNCH_MS` 覆盖。
- **失败时把“这是哪一种失败”说出来**：启动失败被 `catch` 住并显式打印“这是 runner/浏览器启动失败，**不是**控制台断言失败”，因为启动器自己那行错误读起来像断言失败，而两者的应对完全不同（一个意味着控制台回归，另一个意味着 runner 启浏览器慢）。
- **刻意不做**：不给启动加自动重试（与本仓不给 CI 加自动重试同口径：会把一次真问题洗成一次绿）。本笔把失败模式分开并说清，而不是把它涂掉。
- **新增币账本一行**（原文那行讲的是另一个模式，保留不动）：两个失败模式分别登记，并各自记下“复发时要取的”。
- **验证**：`node --check` 干净；本机完整跑一遍 `browser-check` **ALL PASS**（含 `[4c]` 与新的启动预算）。

### 新增（`--as`：本地模式给出可判路径，且本轮记下两件改变做法的事实）

- **登记在账本上的缺口**：`--local` 本地模式不设策略判定（那些门禁全在 `internal/grpc/*.go`）。本笔按“加法”补：新增 `--as <subject>`，**不给就零变化**（本地照旧不判，`subject.go` 已定稿的信任边界不被推翻），给了就加载策略注册表并以该身份判定。
- **侦察得出的两条事实，它们改变了做法（原文照排期会低估工作量）**：
  1. **`cmd/levee/service_factory.go` 在生产路径上无人使用**：584 行；`serviceFactory` / `buildLocalServices` / `localDeps` / `GetChangeService` 在**非测试代码里零调用**，生产本地命令各自直接构造服务（`cmd_approve.go` / `conversation_change_bridge.go` / `engine_cli.go`），而这个工厂**只被 `cmd/levee/grpc_client_test.go` 使用**（local + remote 两种模式都测）。我最初把 `--as` 接在这里，发现生产路径根本不会走到，已**撤掉**。
     **订正（2026-10-24）**：本小节初稿把它写成“**整份是死代码**（640 行，文件外零调用）”，**那是错的**——当时的依据是一条 `grep -v _test`，而**这个 grep 恰好把测试的使用藏掉了**，于是“只有测试在用”被读成了“没人用”。仓库的 `unused` 检查器开着且 CI 绿，本来就说明不存在真未使用的符号。行数与措辞按实测改正；**“生产路径不经过它”这个结论不变**，它仍是 `--as` 接在那里不生效的原因。
     顺带一个仍然成立的观察（比“死代码”准确、也更有用）：这是一个**被测试覆盖、但生产未采用**的缝——测试在维护一条没人走的路径。接不接、退不退是决定，不是缺陷。
  2. **本地命令不走 gRPC 层**：它们直接构造服务，并且把 authorizer 参数传 `nil`（`grpc.NewChangeService(store, nil, nil, nil)`）。所以“本地判权”不是开关一个开关，而是要在本地路径上找到真正的咽喉。
- **缝**：`newCLIChangeService()`（`engine_cli.go:45`）是本地 `plan`/`apply` 的**单一构造点**，两处调用点均改为接 authorizer。为了让本地 96 处 `context.Background()` 不必逐一改，在 `authz` 加了一个**默认主体**缝：`WithDefaultSubject`（默认空 = 旧行为，`Decide`/`AdmitsRead`/`ReadFilteringActive` **共用一个替换点**，以免“按某人判定”与“为无人过滤”在同一请求里打架）；注册表**加载失败是错误而非静默退回不判**（操作员明确要求被判定，静默降级正是他要避免的结果）。
- **边界写清楚，不宣称“本地已全面判权”**：`--as` 只到**走服务层**的动作（`plan` / `apply` / CLI 投票后的 `SettleApproval`）。**`approve`/`reject` 的实际投票走的是领域服务 `approval.Service.Decide`，那里没有 authorizer 参数，因此不受 `--as` 约束**——这句写在 `cmd_approve.go` 的函数注释里；账本同一处也登记了“要让投票也纳入判定需要给该服务加 authorizer 参数，属接口变更，另行决定”。
- **顺带一处一致性**：`currentActor()` 现在**优先取 `--as`**（再是 `LEVEE_ACTOR`）。判定用的身份与审计记的身份必须是同一个，这与 `grpc.ContextWithSubject` 把审计标签同步成已验证主体是同一条理由，只是高一层。
- **测试与变异**：`internal/authz` 新增 4 例——只在 ctx 无主体时替换（并断言真主体**优先**：默认是回退不是覆盖）、**读路径同样替换**（`AdmitsRead`/`ReadFilteringActive` 必须答同一个问题）、**空默认就是旧行为**（这条防的是“悄悄改变每个既有部署”）、nil 授权器安全。**变异**：把替换去掉后，两条替换用例立刻红、而“空默认”两条仍绿——正是应有的形状。`go build ./cmd/levee/` 与 `go vet` rc=0；`gofmt` 干净。
- **过程中我弄坏过一次又改回来**：`tokens.css` 曾有括号失衡（已修）；`authz.go` 的一次 `git checkout --` 误删了未提交的改动（已按原文重做，此后改用“先复制再变异”）。

### 新增（密度档位：两档纵向节奏，实测校准并由两个密度下的真机测量钉住）

- **补的是长期值守场景的诉求**（账本 P2 那条：“它是长期值守场景的诉求”），但做法刻意不是“把设计缩小一号”：
  - 只动**列表型面**（表格、地图、表单控件）的纵向留白。**不缩放 `--lv-space-*`**——它被每个页面组合消费，减半会重排只在宽松尺寸下量过的布局，那是第二套设计而不是更密的一套；**也不动字号**——密度花的是留白，不是可读性。
  - 宽松档 = **没有 `density-compact` 这个类**，所以默认渲染与加这个功能之前**逐字相同**，不打开开关的人零变化。
- **实测值（1600px 视口、dev 构建，2026-10-23）**：

  | | 宽松 | 紧凑 |
  |---|---|---|
  | 表格行高 | 56px | **48px** |
  | 地图行距 | 27px | **23px** |
  | 表单控件（输入框 / 默认按钮） | 32px | **28px** |
  | 地图滚动高（300 台） | 8147 | 6944 |

- **途中查出一个真实的“半改陷阱”**：Element Plus 把**默认按钮高度写死成字面量 32px**（只有 `--large`/`--small` 读变量）。所以仅改 `--el-component-size` 会把输入框降到 28px、按钮留在 32px——实测确认过（变量读 28px、input 28px、button 32px）。**那比不改更糟**：工具栏上 28px 输入框紧邻 32px 按钮看起来是坏的，不是密的。已补一条同变量的显式规则，两条 `:not()` 臂保住库自己的尺寸变体。
- **落地**：`composables/useDensity.ts`（choice / 存储键 / 解析器 / `applyDensity`，对齐 `useTheme` 的防御式写法）+ `index.html` 首屏预应用（同主题同一套做法，避免首帧闪一次重排）+ 标题栏开关（**独立 handler**：共用 `handleCommand` 的兑底会把主题设成 `compact`）+ `tokens.css` 旋钮与 `html.density-compact` 单一块。地图的窗口化数学改用 `mapRowPitch()`，**跟随实际生效的密度**，否则滚动条会描述一个不存在的布局。
- **为什么必须真机测**：`MAP_ROW_PITCH` 是**实测常量**不是算式（CSS 盒模型算 21、实测 27），所以单测只能拿常量比自己。`browser-check` 新增第 `[4c]` 节，在**两个密度下各测一次**真实 DOM：单元格 padding、行距、滚动高仍描述全部 300 台主机、以及该节结束后**把密度恢复默认**（localStorage 是按配置而非按页面的，不恢复会让后面所有块跟着跑紧凑）。CSS 一旦把节奏改动就在那里变红。
- **测试与验证**：`useDensity.spec.ts` 10 例（含**直接读 `index.html` 断言两处重复拼写一致**，比主题那半边只钉常量更紧；含 localStorage 抛异常仍能启动）；`targetMap.spec.ts` 加一条钉住两个常量及解析器；`coldImport` 守卫**抓到了本 spec 重导入模块图而未领 `COLD_IMPORT_TIMEOUT`**，已按规矩补上并把它加进 `KNOWN_REIMPORTING`。`vue-tsc --noEmit` rc=0；`vitest` **37 文件 / 325 测试全绿**；`browser-check` **ALL PASS**（含 `[4c]`）。
- **顺带订正一处规范不实**：`docs/ui-blueprint.md` 原写“密度：表格行高 36px”，实测 56px（14px 字号 ×1.55 行高 + 9px 上下内边距 + 边框）。规范与实现不一致时以实测为准，并写明差在哪里。

### 文档（账本第五处“待做”也是过期：docgen 三组早已收口——我把它当待办排了一轮）

- **这一处最值得记，因为它刚真的误导了一次排期**：账本 D-3 词表行写“`step`（5 个）/ `approval_status`（3 个）/ `AssignResult*` 三组没有前端抄件，`StepRowDTO.status` 现在是开放 `string` 加注释……属界面行为变更、需单独定”，我据此把它当作待办报给了产品并准备开做。**现测三条全不成立**：
  - `STEP_STATES` / `RUN_APPROVAL_STATUSES` / `APPROVAL_STATUSES` / `ASSIGN_RESULTS` 全在**生成物**里（`web/src/types/vocabulary.ts:131/143/153/164`）；
  - 由 `internal/docgen` 从 `internal/state` 的 `StepStatusNames()` / `ApprovalStatusNames()` / `AssignResultNames()`（`internal/state/vocabulary.go:111/117/123`）生成；
  - `StepRowDTO.status` **已是联合类型** `StepStatus`（`web/src/api/index.ts` 的注释就写着 Generated from internal/state.StepStatusNames()）。
  ⇒ **没有任何“界面行为变更”待定**，只是记账没跟上代码。原文保留（它是登记当时的事实），旁边补上现态取证。
- **顺带修一句同样过期的生成注释**：`internal/docgen/webvocabulary.go` 里 `APPROVAL_STATUSES` 的说明写着“expired 目前只由 CheckExpiry 产生，而它**没有生产调用方**”——现测 `internal/approval/expiry.go:46` 就是那个生产调用方（serve 侧的过期扫描器），`internal/approval/vocabulary.go` 早已按此事改写而生成源没跟。已改成与之一致的说法，并按生成规则**同步改了生成物**。
- **验证**：这是本批唯一的“手改生成物”处，所以以机器判据收口——`go run ./internal/docgen -check -root .` 报 `all 2 generated targets are up to date`（rc=0）；`./internal/docgen/` 与 `./internal/approval/` 两组测试全绿；账本表格列数逐行与 HEAD 对比零变化。

### 修复（清单侧 env 归属：重新导入不再能把已登记的 prod 主机改标成 dev）

- **登记过的缺口，实测比登记写的更尖锐**。账本里那一条写的是“`labels["env"]` 对 add/import 是调用方声明，策略只能回答‘你能不能在这个环境登记主机’，回答不了‘这台机器真的属于这个环境’”。现测发现不止“新建时”：`ImportTargets` 的逐环境循环取的是**来件 row** 的 env，**从不查已存行**，而导入复用 `existing.ID` 走的是 UPDATE——于是 **admin of dev 只需重新导一份 `env: dev` 的清单，就能把一台已登记的 prod 主机改标成 dev**。
- **为何这是安全属性而非标签卫生**：目标的环境正是策略用来判**读可见性**（`ListTargets` 按可见 env 收窄）与 **`Diagnose` 的 `apply`** 的，而后者的通道用的是**存在库里的凭据**。一棵 dev 管理员 → dev `apply` 主体 → 在 prod 机器上带平台凭据执行，就这么接上了。
- **落地（拒绝先于任何写，且预览与写入同一套规则）**：
  - `ImportTargets` 在逐环境循环之后、**任何写之前**跑一遍 `inventory.Importer.LabelChanges`；凡会把已存目标移出原 env 的行，要求调用方**在**那个 env 上也是 `admin`，拒绝时点名**被移出的环境**（不是被声明的那个）。
  - 预览必须先于写入：refuse 发生在一半会让清单**半落地**，而这个端点原本就承诺“文件里有一台不可达的主机，则一台也不导入”。
  - **合并规则只有一份**：把原先内联在 `importTarget` 里的 re-import 合并逻辑抽成 `mergedLabels`（地址归一抽成 `targetAddressFor`），预览与写入**都调它**——否则预览会与真正落地的结果分歧，而那正是这类守卫最容易腐化的地方。
- **残余已收窄，并写在代码里而非畵着**：`(hostname, port)` 唯一，所以 `AddTarget` **抢不走**平台已持有的地址（`AlreadyExists`）；能从外部改写已有行的路径只有导入，而它现在需要原 env 的 admin。残余是“对平台从未持有的地址自称 env”——见 `target_service.go` 的注释与账本。
- **测试与变异**：`internal/inventory` 新增 6 例（移动 / 纯重声明不报 / 新主机不报 / **清空 env 也算变更** / 同标签不报 / **预览与写入结果一致**）；`internal/grpc` 新增 1 例，带**对照组**——同一主体、同一文件、同一声明环境，**只因库里有没有该主机而结果不同**，这样才能证明是**新检查**在拦而不是原有的逐环境循环。**变异**：把判定从 stored env 换成 incoming env（即旧行为）后新用例立刻红（`must not be wide open`）。
- **验证**：`go test ./internal/inventory/ ./internal/grpc/ -count=1` 全绿；`gofmt -l` 干净；`go vet` rc=0。（本机 Go 1.26.6 + `GOTOOLCHAIN=local` 跑，临时降工作树 `go.mod`——**未进提交**，CI 仍按 1.26.10 复核。）

### 修复（remote http 探针的 `{target}` 此前不展开：一个按文档写就永远过不了的门禁）

- **现象**：`probe` 门禁在 `mode: remote` + `kind: http` 下把 `url` **原样**交给目标机上的 curl。参数表（`docs/gates.md`）把 `{target}` 当作 `url` 支持的占位符来宣传，于是按文档写的 `url: "http://{target}:9090/health"` 会把字面量 `{target}` 交给 curl，curl 拿它当主机名解析——DNS 失败，**这个探针永远不可能通过**，而且错得完全不指向真正的毛病。
- **为何算缺陷而非语义**：同样是 `remote` 模式、同一个占位符，**remote tcp 会解析**（`port_from_target` 与 `{target}` 都对着那一台解析）、**direct 会展开**（`expandTargets`）——只有 remote http 漏调了那个已经存在的辅助函数。原登记里也把它写作“新登记的**不一致**”而非设计。
- **定夺与方向**：**按目标展开，与 remote tcp / direct 同口径**。依据是本仓已有同形先例——“命令门禁在 run 路径中拿不到通道，因此不可执行”（原状态：任何声明 cmd 门禁的工作流必然回滚；后改为按真实退出码判定）。两者都是把一个“无论如何都失败”的检查改成**测到它声称要测的东西**，方向是让门禁可信而不是放宽它。
- **落地**：remote http 分支改走 `expandTargets`。`input.TargetIDs` 在 run 路径已被 `checkAcrossTargets` 收窄成单台（`single.TargetIDs = []string{host}`），所以正常路径下就是一条 URL，且判定/文案/证据仍由 `judgeRemoteHTTP` 原样给出；持**预拨通道 + 列了多台**的旧形状逐台按本机地址检查、**全过才算过**（与 remote tcp 同形），失败时点名是哪一台。`docs/gates.md` 那条“两条不对称”已改写。
- **测试**：`internal/verify` 新增 4 例——按目标展开（命令里出现 `http://web-1:9090/health` 且**不含** `{target}`）；旧形状多台逐台检查；多台时失败点名 `web-2`；**无占位符时命令逐字节不变**（回归钉子，并钉住单 URL 仍用 `judgeRemoteHTTP` 自己的文案）。**3 项变异全被抓**：把 `expandTargets` 换回 `[]string{g.url}` 后三条新用例立刻变红，报出的正是 “should not contain \"{target}\"”。
- **验证**：`go test ./internal/verify/ -count=1` 全包绿（本机 Go 1.26.6 + `GOTOOLCHAIN=local`，临时把工作树 `go.mod` 降到 1.26.6 才能跑——本机拉不到 1.26.10 工具链；**该改动未进提交**，CI 仍按 1.26.10 复核）。

### 文档（账本四处“未解决”已过期：订正为已收口，并各自附取证）

- **本仓的头号缺陷类是“过期账本误导排期”，这次又抓到四处**。全部先取证再改，不靠照推：
  1. **`isValidTransition` 的裸状态字面量**（D-3 词表行尾登记的“同族残留”）：现测 `internal/grpc/change_service.go:1665` 起整个 switch **裸字符串零命中**、通篇 `runstatus.*`——已由 `feat/vocab-constants-and-linear-guard`（PR #176）收口。
  2. **`runToPB` 把整份源当 `Label`**（`levee new` 行登记的“登记不修①”）：现测 `change_service.go:283` 是 `Label: dsl.DisplayName(r.WorkflowName)`，源码逐字留在 `WorkflowFile`，注释写明了这次换的理由。
  3. **`input:` 块零读取点**（同行的“登记不修②”）：现已有两个读取点——`compile_warnings.go:184` 发 **LE004**（“声明了或引用了 input 却没人供给/替换它们，`{{input.x}}` 会以字面量进模块”）、`emit.go:71` 拒绝发射带 input 的工作流。**登记里仍然成立的那一半已保留写明**：`input:` 的值供给本身仍未实现（LE004 只说真话、不阻断），与模板 `parameters` 仍是两套并行声明，真源问题待产品定形态。
  4. **browser-check “仍未做：CI 里跑它”**：现测它早就是 `check (all jobs passed)` 的必需腿（`ci.yml:821` 的 `needs` 点名 `browser-check`，`BROWSER_CHECK_REQUIRE=1` 让“没跑成”不能算绿）。仍真待做的两项已分开写：跳浏览器矩阵（只有 Chrome 验证过）、windows 腿的**偶发红根因**。
- **为何四处都是“已收口但账本没跟着改”**：那些修正当时都发在其他行的 PR 里（或刻意只补代码不补账），而账本的形状是“一行一个主题”，于是“某主题下挂的旁路项被移走”在账本里不可见。本笔只改“现态声称”，**历史诊断原文一律保留**（它们是登记当时的事实，也是举证链的一部分）——这与本仓“不对 master 重写历史”的口径同源。
- **验证**：逐条用 `grep`/`sed` 取的行号与代码摘录均在改动说明里；表格列数逐行与 `HEAD` 对比**零变化**（只改单元格文本，不动表格结构）；`## [Unreleased]` 仍恰好一个、无冲突标记。

### 测试基建（浏览器检查的假红从“盲猜”变成“自证”：超时说清三条原因之一 + 失败截图随 CI 上传）

- **背景：这个 job 一轮里红了三次**（master `733e84d`、#184、#196），每次都在**互不相关**的提交上，而同提交的 ubuntu 腿是绿的；单次红是 `empty list renders the empty tier — title=""` 配一行 `NOTE settle timeout after 30000ms`。账本对它的要求正是“复发时要取到证据”，但那行 NOTE **什么原因都没说**，紧随其后的断言又只报自己的读数——于是造成超时的那个 DOM 在有人读日志时早已不存在。前两次排查都只能用断言文本反推。
- **落地（超时自证）**：新增 `settleDiagnosis(page)`，读回 `settled()` 消费的三个输入并**指出是哪一个没被满足**：`mounted=false`（视图根本没渲染，属应用故障不是时序）、`inflight>0`（REST 调用没完成，可能挂起或重试）、`quiet<=150ms`（DOM 还在变，动画或轮询）。除原因外一并打出 `url` / `h1`（带元素类型）/ 可见的 `.state__title` / 表格行数 / 页面报错，并在 `--shots` 时**对导致超时的那个页面截图**——失败区域（test [2] 的空态）**不在** test [1] 的截图清单里，这正是前两次看不到现场的直接原因。
- **落地（CI 保留现场）**：`ci.yml` 的 browser-check 加一步 `if: always()` 上传截图 artifact（按 OS 分名，复用仓内已钉的 `upload-artifact` SHA，`if-no-files-found: ignore`）。`always()` 而非 `failure()`：取消与超时的运行同样留下一张值得看的页面。
- **顺带修掉两处“声明了但没人消费”（同一缺陷类）**：① `open()` 只收两个参数，而三处调用仍在传第三个（`1800`）——sleep 时代的 per-call 预算，等待改成观测后它就再也不被读，一个看着像配置、实际无效的数字；已删并在 `open()` 上写明“预算只有一个全局旋钮”。② `notes` 数组**只写不读**（收了一个失败的整个会话从来没人打印）；现在在结尾连同 FAIL 清单一起输出，因为“某条断言读到了未稳定的页面”正是判断一个红是不是产品回归的第一步。
- **证据（本机隔离实例 mock:4699 / vite:5188，未复用任何现有端口）**：
  - `BROWSER_CHECK_SETTLE_MS=1` 强制超时下，三类原因**逐一被正确打印**：`a REST call never finished (inflight=6, …)` / `no route title was ever rendered (the view did not mount)` / `the DOM was still changing (last mutation 6ms ago, needs >150ms of quiet)`，截图逐个落地。
  - 正常预算：**ALL PASS**（无回归）。
  - **A/B 对照（验证“等待仍承重”）**：原版 `exit=1 ok=4 FAIL=17 notes=15`，本笔 `exit=1 ok=3 FAIL=18 notes=15`——两者都判红且 notes 数一致，本笔没有救活任何断言。CHANGELOG 旧文里那个“7 条 FAIL”是当时断言总数下的读数，列表长到 38 条后数字自然变大，性质不变。
- **仍未做**：跨浏览器矩阵（只有 Chrome 验证过）、以及这个 flake 本身的**根因**——本笔把“下次复发能看到什么”做成了事实，但没声称修好了它（没复现路径就不声称）。

### 修复（`go` 指令 1.26.9 → 1.26.10：收口新发布的标准库公告 GO-2026-6809）

- **一刀切在所有人头上的红**：2026-10-21，PR #192（codeql-action 锁步，与 Go 毫无关系）的 `govulncheck` 失败，报 `Vulnerability #1: GO-2026-6809 — Distrust compromised intermediate certificates in crypto/x509`，`Found in: crypto/x509@go1.26.9 / Fixed in: crypto/x509@go1.26.10`，实例跟踪直指本仓代码：`internal/web/server.go:147:25: web.Start calls http.Server.Serve, which eventually calls x509.Certificate.Verify`。同日 master 上最后一次 push（`46243d7`，10-20 12:31Z）这条腿是 success ⇒ 公告是在那之后发布的，**不是任何进行中改动的回归**，但它会把此后每个 PR 都卡在 `check (all jobs passed)` 上。
- **为什么是升 `go` 指令而不是别的**：本仓的 `govulncheck` 作业刻意按 `go.mod` 声明的工具链扫描（`docs/release-notes/v1.21.0.md` 已把这条写死：`go 1.26.6 → 1.26.9` 收的就是同类标准库公告），所以标准库类公告**只能**靠升指令收口——没有别的旋钮。改动面因此只有一行：`go 1.26.9 → 1.26.10`。
- **全仓核过没有第二处吃补丁号的地方**：CI 六个作业都用 `go-version-file: go.mod`（这正是 v1.22.0 那次"stale alias"事故后换的读法），`Dockerfile` 的 `GO_VERSION=1.26` 是 major.minor 不吃补丁号；剩下两处字面 1.26.9 是 `.github/workflows/ci.yml` 里描述当时经过的注释，属于历史不该改。
- **另一条公告不拦路，一并记录免得下次误判**：`GO-2026-5932`（`golang.org/x/crypto/openpgp` 无维护）同场出现，但 govulncheck 自己判定 "your code doesn't appear to call these" ⇒ 不可达，不进阻断集，故本笔不升 `x/crypto`。
- **验证**：本机 `goenv` 在 WSL 下 `GOSUMDB=off` 无法自动拉 1.26.10（`verifying module: checksum database disabled`，纯本机网络限制，CI 的 setup-go 不受影响），所以本地只做了 `scripts/` 59 套件与结构守卫；编译器侧结论交给 CI。合入后以 master 的 push CI 为准。
### 修复（codeql-action 四条引用统一升到 v4.39.0，并补上锁步守卫）

- **登记过的配置债，第二次犯**：`.github/workflows/codeql.yml` 的 init / autobuild / analyze 三条与 `ci.yml` 的 `upload-sarif` 各自钉着不同的 commit SHA（三条 v4.38.3、一条 v4.38.2），而 CodeQL 硬性要求同一个 workflow 内所有 `github/codeql-action/*` 步骤版本一致。2026-10-19 Dependabot 因此开出 **四个各自只升一条** 的 PR（#183/#185/#186/#187），其中 #185 的 `analyze (go)` 直接以 CodeQL 的原始报错失败：`Loaded a configuration file for version '4.38.3', but running version '4.39.0'. All steps in a workflow that use github/codeql-action must use the same version to work correctly.`
  - #164 那一笔做的是同一类锁步，但只覆盖了 `codeql.yml`，漏了 `ci.yml` 里那条 `upload-sarif`，且没有守卫——所以今天历史原样重演。
- **落地（同一 SHA + 守卫 + dependabot group 三件）**：
  1. 四处引用全部指向 `6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee`（v4.39.0），版本注释同步。dockerd 那条 #184 的 `proto regenerate check` 是 `proxy.golang.org` 网络抖动（`stream error: stream ID 25; INTERNAL_ERROR`），非代码问题，本笔不动它。
  2. `scripts/check_codeql_action_lockstep.py` 新守卫：扫 `.github/workflows/*.yml` 的所有 `github/codeql-action/<name>@<sha>` 引用，断言只钉一个 SHA、只标一个版本号，并在你只升一条时把不一致的两行原文打进报错（不是只说"不一致"）。零引用也判红——那意味着守卫读错路径或 workflow 被人删空，两种都不许静默通过。
  3. 挂进 `ci.yml` 的 `release-gate` job 作第 3 步（在 `landing shape` 之前），并加进发现式套件点名表——这张表本身就是本仓踩过两次的地方，漏加新套件会让它"收集了空"却 exit 0。
- **让 Dependabot 下次不再拆开**：`.github/dependabot.yml` 的 `github-actions` 段新增 `codeql-action` group（`patterns: github/codeql-action/*`）。group 减少来源、守卫钉住结果——人手动改 workflow 的场景 group 管不到，所以两处都要有。
- **测试**：`scripts/test_check_codeql_action_lockstep.py` 6 例（本仓现状绿、全同绿、混 SHA 红且报错含两个 sha、混版本注释红、零引用红、解析 action/sha/版本三元组）。跑法：`python scripts/check_codeql_action_lockstep.py` + `python -m unittest discover -s scripts -p 'test_*.py'`（65/65）。

### 修复（approvals.comment 归属收口：supersession 不再把链 JSON 盖成人读字符串）

- **登记已久的真 bug**：上一批收口审批超时时发现的——`change_service` 的审批 supersession 路径把 `approvals.comment` 列直接写成人读字符串（`"superseded by re-plan"`），而该列由 `internal/approval/staterow.go` 独占，里面是审批链的 JSON（approvers/decisions/history）。守卫只写了「列为空才写」，而经 `approval.Create` 落库的行 comment 从不空，所以这条写路径要么是死的、要么（一旦命中）会让 `ApprovalFromState` 解析失败——`comment` 非 JSON 时它返回 `unparsable metadata` 错误，那笔已过期记录连同「谁被问过、谁投过票」一起从所有读路径消失。
- **落地（归属协议写死，不是加一个判空）**：`comment` 列 = 链 JSON，且**只能**是链 JSON；任何要记录的原因都必须是在 JSON 里长出来的字段。`staterow.go` 新增 `MarkSuperseded(sa, reason)`——解码现有 blob、写入新字段 `superseded_reason`、按同一形状重编码；`Approval` 域对象同步暴露 `SupersededReason` 并进 `StateApproval` / `ApprovalFromState` 往返。`change_service.go` 的 supersession 改为 `old.Status="expired"` + `ActedAt` + `MarkSuperseded`，status/时间戳仍由调用方写，`MarkSuperseded` 只碰 JSON。
- **corrupt blob 是「报告」不是「覆盖」**：`MarkSuperseded` 遇到非 JSON comment 返回错误（不静默改写），调用方记录原因丢失但**仍能把行标成 expired**——过期是安全属性，原因注解是审计属性，二者不能互相阻塞。
- **测试**：`staterow_test.go` 4 例（superseded 后链仍可读且带原因、legacy 空 comment 也能标、corrupt blob 被拒且不被改写、nil 接收者安全）；`TestApprovalRouting_RePlanSupersedesPending` 加断言：supersede 之后 `ApprovalFromState` 必须成功且 `Approvers` 非空、原因落在 `SupersededReason`。

### 修复（assignments.result 写入面收窄：非终态的 run 状态不再透传进结果列）

- **登记过的数据卫生缺口**：`cmd/levee/grpc_change_executor.go` 的 `retry()` 在 default 分支把 `resp.GetStatus()` 原样当 assignment 结果返回。该列的词表（`state.AssignResult*`）冻结在 `completed | failed | rolled_back`，而 `running` / `interrupted` / `paused` / ... 本不是判定——于是 `state=done` 的行能带一个下游不认识的结果。设计文档 `docs/design-cluster-dispatch.md` 早就写明映射点在这里且 `apply()` 走 default 落 `failed`，只有 `retry()` 漏了。
- **落地（两道）**：
  1. **映射点**：`retry()` 的 default 归为 `state.AssignResultFailed`，并 `log.Warn` 点名真实的非终态 run 状态（run 行仍保留精确状态，需要区分时 JOIN 回来）；三个分支改用 `state.AssignResult*` 常量而不是字面量，`apply()` 同步。
  2. **存储层兜底**：`SQLiteStore.SetAssignmentResult` / `PGStore.SetAssignmentResult` 在写之前用新的 `state.isAssignResult` 校验，不在词表内的值（含空串——列默认 `''` 是“尚无结果”而非第四个判定）直接返回错误。这样即便将来又有一个调用方忘了映射，坏值也进不了列。
- **测试**：`cmd/levee` 新增 `TestExecute_RetryNonTerminalStatusMapsToFailed`（running/interrupted/paused/pending/planned/approved 六态均落 `failed`）；`internal/state` 新增 `assignment_result_write_surface_test.go`（非法值被拒且错文可读、三个合法判定不被守卫拦截）。

### 新增（run 生命周期全量 webhook 镜像：chat 房间只收命名时刻，机器收全流）

- **接口在、渠道在、没人投**：run 生命周期漏斗（`wiring.RunEventStore`）上一批收口后只接了一个消费者——ChatOps 房间投影，而且刻意只发 `running` 与终态（“中间跳一跳一卡是噪音”）。那条注释把 webhook 全量镜像登记为后续，并写着“serve ships no channel yet”——其实 `notify.webhook` 渠道已经接进 serve（`serve_notify.go`），只是没人给它事件。
- **落地**：`internal/notify/run_lifecycle.go` 新增 `RunLifecycleMirror`。它按 `wiring.RunEventSink` 的形状（不 import wiring，免得回环）接收每一笔成功迁移，映射成通知消息：
  - `paused → running` = `resumed`（先于 `run_started` 判，否则恢复会被误报成新启动）、`→ running` = `run_started`、`→ completed` = `run_completed`、其余终态（failed / 三种回滚判定 / cancelled / rejected / interrupted）= `run_failed`（LevelError，**精确状态保留在 metadata**）、`→ paused` = `paused`（LevelWarning）、其余（draft→planned / approved / archived…）= 新触发器 `run_status_changed`。
  - 投递走 `NotificationManager.NotifyAsync`：漏斗的 sink 契约禁止在 store 写上阻塞外部 I/O，而 webhook POST 带重试正是这种 I/O。
- **单一漏斗、两个消费者**：`wiring.CombineRunEventSinks` 把 chat 投影与 webhook 镜像扇出，各自 isolate panic（一个坏消费者不能静默另一个）。serving：`notify.webhook` 管理器从 `buildEngineServeOptions` 提到 `buildServeServices` 构造，一份 manager 同时给回滚 Grader 和镜像用；engine 关闭时镜像照常工作（PlanChange/ApproveChange/pause/resume/archive 都在写 run 状态），`warnNotifyTransportWithoutEngine` 的措辞相应订正（不再是“no notification will ever be sent”）。
- **测试**：`internal/notify` 5 例（命名时刻+时间戳/收件人、七种终态保真、中间跳与 resume 区分、nil 守卫不投递）；`internal/wiring` 3 例（扇出顺序、全 nil 返回 nil、单 sink panic 不阻断后续）。`go test ./internal/notify/ ./internal/wiring/ ./cmd/levee/` 全绿。

### 新增/加固（告警接入：每源入口身份 + 按归属判可见性，例外表清空）

- **登记已久的例外**：`AlertService` 自登记进 serve 起就没有策略判定，是一直写在 `docs/identity-deployment.md` 与 `docs/product-roadmap.md` 里的“刻意不接”——原因是真的：`AlertMessage` 只有 id/source/severity/ends_at，没有 team、没有 env、也没有能解析成 env 的 target，环境键控的矩阵没有可判的输入；两条捷径都在说谎（按 `permission.default_env` 判等于把监控源归给恰好拥有默认环境那支队；新增 `alert` 动作 token 在每份既有矩阵上 fail-open）。
- **归属来自配置，不来自载荷**：新增 `<dataDir>/alert-sources.yaml`（`internal/alert/sources.go`），每条 `{name, environment, secret}`。监控源是机器、拥有一个环境；凭据证明“哪个源在说话”，登记项决定“它的告警属于哪个环境”。`ReceiveAlert` 在配置了源时要求 `x-levee-alert-source` + `x-levee-alert-token`（gRPC metadata；REST 同名头由网关桥接进同一 metadata），未登记/错凭据/无凭据一律 `Unauthenticated`（机器不是矩阵能判的主体），且被拒的告警**不进 ring、不进流**。告警的环境写进 `alert.Alert.Environment`（新增字段，仅内存域对象，不动 proto）。
- **可见性按告警自己的环境**：`GetAlertStatus` 与 `SubscribeAlerts` 按 `alert.Environment` 要 `view`（复用 `authorizeResourceRead` / `resourceVisibility`）——流按可见环境**收窄**而非整条拒绝，与 ListChanges 同口径；调用方不能靠知道 id 扩大可见面。未配 `alert-sources.yaml` 时保持历史姿态（只认证，启动 WARN）；配置了却写坏则拒绝启动，与矩阵/策略同一规则。
- **例外表清空**：`cmd/levee/serve_policy_wiring_test.go` 的 `exemptWiringCalls` 返回空表（机制保留、内容清空，棘轮 1→0），`AlertService` 进入受控集，探针用 prod 源发告警 + 无权主体读（`view`）、dev 源 + 有权主体读。
- **测试**：`internal/alert` 6 例（缺文件=空注册表、解析+trim、缺 name/env/secret 各自拒绝、重名拒绝、坏 YAML 报错）；`internal/grpc` 3 例（入口身份四种情形+环境归因+拒件不入 ring、读按归属环境判且无关主体被拒、无源时保持旧姿态且告警无归属）；`cmd/levee` census/ingress 测试全绿。

### 新增（审批超时闭环 + run 生命周期事件漏斗：两笔登记已久的缺口一次收口）

- **审批超时从未生效过**：`approval.CheckExpiry`（扫描过期 pending 并落成 expired）自落库起**零生产调用方**——只有测试在调。三层超时配置（`approval.standard_timeout/high_timeout/emergency_supplement`，4h/8h/24h）同样是**零消费方**的装饰品；kickoff 审批一律硬编码 24h。也就是说：配置里写了 4h，线上一个超时审批永远读作 pending，`expired` 是词表里的死字。棘轮测试 `TestCheckExpiryIsTheOnlyProducerOfExpired` 为此埋了数周——本批接上驱动器时**它如约变红**，注释与断言按它要求同步更新（现在钉的是“恰好一个调用方=清扫器”，而不是当年的“零调用方”）。
- **落地（两半）**：
  1. **超时声明**：`ChangeService.WithApprovalExpiryResolver`——kickoff 的 `ExpiresAt` 从硬编码 24h 改为按层解析（standard→standard_timeout、high→high_timeout、emergency→emergency_supplement，即 config 注释里一直承诺的语义）。未装 resolver 的构造保持 24h 兼容（错误但已知的值，不是静默无超时）；层超时配置为非正值 → 零值 → CheckExpiry 按文档口径跳过（“想要不自动过期”是显式配置项，不是猜出来的缺省）。
  2. **超时驱动**：`internal/approval/expiry.go` 的 `ExpirySweeper`——serve 按 `approval.expiry_sweep_interval`（新键，默认 1m，≤0 整体禁用）周期调 CheckExpiry，每批落定后：ChatOps 卡片（新事件 `approval_expired`）+ run 时间线的 `approval_expired` trace 行（控制台看得见“变更为什么停了”）。清扫是检测不是门禁：扫漏一拍只是晚一拍说真话，apply 门禁本来就不放行无有效审批的变更。
- **run 生命周期事件漏斗（`internal/wiring/runeventstore.go`）**：run 状态写入散在四个包的六个调用点（apply 漏斗、retry、接管中断、pause/resume、CLI），这正是四个 chatops 事件类型（`run_started/completed/failed/state_change`）零 producer 的结构性原因——每条新流程都得记得播报，而没人记得。不逐点审计，漏斗下沉到 store 缝：`RunEventStore` 装饰器拦截三条状态写路径（`UpdateRunStatusIf` / `UpdateRunApprovalStatusIf` / 全行 `UpdateRun` 含状态变化时 diff 前值），每次成功迁移交给 sink。装饰器选择与 TenantStore 同缝同理；sink 的 panic 就地収容（GateObserver 同一姿态：通知路径不能成为 run 的第二种失败源），nil sink 全透传。
- **终态播报（chatops 半边）**：`bridge.OnRunTransition` 消费漏斗——`→running` 发 run_started、`→completed` 发 run_completed、其余终态（failed / rolled_back / rolled_back_partial / rolled_back_incomplete / interrupted）发 run_failed 且**卡片携带真实状态**（部分回滚不得读作干净失败或干净完成）。**分工是产品决定**：中间跳（paused / pending_approval）不上聊天——一跳一卡是噪音，且可行动的时刻已有自己的卡片；webhook 全量镜像登记为后续（Notifier 接口在、serve 尚未配置渠道，与“无 secret 不挂入向”同一姿态：说出来而不是假装已接）。
- **顺带登记一个真 bug（本批不修）**：`change_service` 的审批 supersession 路径直接把 state 行的 `comment` 列写成人读字符串（"superseded by re-plan"），而该列由 `staterow.go` 独占——里面是审批链的 JSON（approvers/decisions/history）。后续任何经 `ApprovalFromState` 的读取会把这笔已过期记录解码成空链：审计上“谁投过票”被抹掉。修复需要先定 comment 列的归属协议，单独立项。
- **边界**：LEVEELang 门禁（`gate_approver`）的审批记录刻意**不带** ExpiresAt（零值 = 清扫跳过）——在途门禁阻塞的是 run 本身，过期它需要 run 级语义，另行设计；Jira 镜像暂不播报过期（登记）；`--local` 模式不装 sweeper（与 authz 的信任边界口径一致）。
- **测试**：approval 6 例（清扫器落定+回调收全记录、空扫不触发回调、幂等、禁用区间立即返回、ctx 取消停扫、nil 接收者安全）；chatopsbridge 4 例（过期广播+卡片渲染、零过期时间不渲染零值时间、迁移映射五态、中间跳/自迁移/nil 全不播）；wiring 7 例（CAS 成功发/拒绝不发、审批结算 CAS、全行写仅真状态变化、nil sink 透传、sink panic 容纳、未知 run 透明）；chatops 卡片分发 1 例（approval_expired → Notification）；cmd 3 例（层→超时映射、零值豁免、清扫间隔默认值）。**变异咬合**：棘轮测试从“零调用方”改判“恰好一个调用方=清扫器”，第二个绕过漏斗的过期写入方会红。
- 验证：`go test ./...` EXIT=0、`go vet` 干净、golangci-lint 0 issues、gofmt 干净。前端未涉及、dist 未动。

### 升级（前端工具链 vitest 3→5：清掉剩余 5 条 dev-only 漏洞；连锁 vite 5→7）

- **动机**：非破坏 audit fix（上一条）清完生产依赖后，剩余 5 条全在 vitest 工具链（@vitest/mocker moderate、tinypool critical——worker options 原型污染 RCE，只在跑测试时存在）。修复需要 vitest 5，破坏性升级单独立项——就是这个变更。
- **连锁升级**：vitest 5 peer 要求 vite ≥6.4 → vite 5.2.6→7.3.7、@vitejs/plugin-vue 5→6.0.10、@types/node 20→^22（对齐运行时 Node 22）。装完 npm audit **0 vulnerabilities**。
- **两个坑（都在 worker 启动语义上）**：
  1. **默认池 threads→forks**：forks worker 在 drvfs/9p 挂载（WSL /mnt/f 工作区）上启动慢到撞 60s 超时，36 个测试文件全军覆没。配置钉 `pool: 'threads'`（恢复 vitest 3 行为），注释写明原因。
  2. **TZ 钉子失效**：formatTimestamp 的 UTC 断言此前靠 spec 顶部 `process.env.TZ = 'UTC'`——vitest 3 下有效，vitest 5 的 worker 预热（环境装配/内部代码）先碰 Date，ICU 把时区缓存成宿主值（Asia/Shanghai），后设不生效；`test.env` 同样太晚。最终钉在 `vite.config.ts` 顶部（主进程求值、先于 worker 生成，threads worker 创建时快照父进程 env，必然带上）——与 CI（ubuntu, TZ=UTC）一致，也符合 roadmap 约定：UTC 存储、展示层按用户时区转换（mvp-plus1-roadmap.md「时区处理」）。
- **dist**：vite 7 重新构建，32 个资产换哈希（vendor `Dnq1n2I3`→`sGgvVIPI`），`diff -r` 一致、embed 测试绿。
- 验证：vitest **300/300**（36 文件）、vue-tsc 干净、`vite build` 成功、`go test ./...` EXIT=0。

### 加固（工程查漏补缺：前端生产依赖漏洞与行尾/依赖门禁）

- **前端生产依赖 4 个 high 漏洞（npm audit）**：axios 1.x 12 条（原型污染/头注入/ReDoS/重定向 SSRF——控制台全部 HTTP 走它）、vue/@vue/server-renderer XSS、brace-expansion、source-map-js。`npm audit fix` 非破坏升级（axios 1.19→1.20.0、vue 3.5.41→3.5.44），按 Makefile 同一命令重建并镜像 `internal/web/dist`（vendor 块 `C7FidRup`→`Dnq1n2I3`）：`diff -r` 一致、internal/web embed 测试绿、前端 300 例全绿、vue-tsc 干净。
- **遗留（dev-only，不随产物发布）**：vitest 工具链 5 条（@vitest/mocker moderate、tinypool critical——worker options 原型污染 RCE，只在跑测试时存在）。修复需要 vitest 5 破坏性升级，单独立项，不夹带在非破坏 `audit fix` 里。
- **`.gitattributes` 漏钉 `go.mod`/`go.sum`**：其余源码全部 `text eol=lf`，这两个文件落到 `text=auto`，Windows 检出为 CRLF——`go mod tidy -diff` 报整文件 diff，把真实依赖漂移埋进行尾噪声里。补钉 + 本地工作区已归一（内容零差异）。
- **CI vet 作业补 `go mod tidy -diff`**：vet 作业定位是「cheap structural checks, fail fast」，tidy -diff 正是这一类——依赖漂移（加了 import 没 pin checksum、删了 import 留孤儿条目）在 build/test 掩盖之前拦住；CI 检出是 Linux/LF，不受上述行尾问题影响。

### 加固（ChatOps 入向重放防护：钉钉/飞书的签名只盖时间戳，截获的 (ts, sign) 在新鲜度窗口内可换 body 重放——现已单次有效）

- **问题**：三平台的回调验签强度并不相同，而 `ingress.go` 的包注释笼统写成「MAC over the request body」——那只对 Slack 成立。钉钉/飞书的构造是 `sign = HMAC(secret, ts+"\n"+secret)`，**body 不参与签名**：在 1 小时新鲜度窗口内，截获一对合法 `(ts, sign)` 后可以携带**任意 body** 重放——换成别的 run、别的 user，就是一次合法入站。Slack 的 body 入 MAC，重放只能重放同一份内容，但语义上同样是「同一签名被用第二次」。
- **落地（`internal/chatops/replay.go` 新）**：TTL+LRU 单次有效守卫——每个已验证的 `(platform, ts, sig)` 三元组在「最长新鲜度窗口 + 1 分钟钟差余量」内第二次出现一律 401（新哨兵 `ErrCallbackReplay`），无论 body 是否原样。三平台统一适用：Slack 重放被拒的代价是平台重投递拿 401，这是安全的——首次投递要么已 ack（平台不会再投）、要么中途崩溃（崩掉的命令点第二次就是，审批状态机 `ErrDuplicateDecision` 本就幂等）。
- **验证先行是结构性质不是约定**：守卫只在验签通过后消费签名位——未验证的洪水进不了 LRU，不能把真实条目挤出去；容量 4096 兜底内存，条目 TTL 到期即视为未见过（届时时间戳新鲜度检查反正会拒）。
- **顺带**：抽了 `callbackSignature` 帮助函数（三个 verify\* 与守卫的键必须读到同一对 header，收拢到一处）；包注释改为逐平台写清「MAC 覆盖了什么、为什么差异重要」；修正两处 `NewCallbackHandler` → `CallbackHandler` 的过时引用。
- **测试**：ingress 5 例（钉钉同签名**换 body** 重放 401 且只派发一次——本条防护的核心用例；同 body 重放 401；飞书重放 401 且被换的 reject 不派发；Slack 重放 401；验签失败不消费签名位）；`replayGuard` 单元 4 例（首次真/重放假、不同签名与平台互不干扰、TTL 过期视为未见过、LRU 驱逐最旧、nil 守卫全放行）。
- 验证：`go test ./...` EXIT=0、`go vet` 干净。`-race` 在本机 Windows 交叉编译环境不可用，由 CI 的 Linux leg 覆盖。前端未涉及、dist 未动。

### 新增（四套取值有了单一来源：steps.status / 两张审批表 / assignments.result 接进 docgen，控制台字段收成严格联合）

- **补上的是 #170 / #172 那条线上的最后一块**：`web/src/types/vocabulary.ts` 此前生成 7 张表，仍有四套取值散在 owner 常量与前端手抄件之间。现在 `internal/state` 导出 `StepStatusNames()` / `ApprovalStatusNames()` / `AssignResultNames()`，`internal/approval` 导出 `StatusNames()`，docgen 的表从 7 张长到 **11 张**（`go run ./internal/docgen` 一次生成，`-check` 报 `all 2 generated targets are up to date`）。
- **两张审批表刻意不合并**，因为它们是两列不同东西：`runs.approval_status` 三个值（pending/approved/rejected），`approvals.status` 四个（多一个 `expired`）。schema.sql 对本列的注释写着 `timeout|skipped`，那两个拼写在**任何一列都没有写入方**——清单是按写入方清点的，不是抄注释抄来的，这点连同分歧一起写进 owner 的注释，并由 `TestRunApprovalListStaysApartFromTheApprovalsTable` 钉住（它断言常量组恰为三个、且清单里不许长出 `expired`）。合并成一张表会让一列能渲染出它永远取不到的值。
- **前端的手抄件删掉了，而它指着的是一个不存在的守卫**：`web/src/utils/targetMap.ts` 原来自己写着五个字符串，注释说"由 `internal/state/step_status_vocabulary_test.go` 对着 `state.StepStatus*` 钉住"——**那个文件不存在**（真实文件是 `step_approval_status_vocabulary_test.go`，而且它钉的是写入方，不是这份抄件）。抄件被"解析抄件"的守卫看着，正是 #153 的形状：几处产物互相自洽、一起和服务器不一致，全程没有一处报错。现在 `STEP_STATES` / `StepStatus` 由 owner 生成，targetMap 走与 `./batch.ts`、`./assignment.ts` 同样的 import + re-export 写法（不是 `export … from`，因为下面的 keyed `Record` 要用这个名字）。
- **严格联合是实测出来的，不是自我感觉**：临时放一个 `status: 'definitely_not_a_step_status'` 的 `StepRowDTO` 进 `web/src` ⇒ `vue-tsc` 报 `TS2322: … is not assignable to type '"pending" | "running" | "failed" | "success" | "skipped"'`（错误文本里那五个成员就是生成表的内容），删掉探针后 `type-check` 回到 rc=0。`StepRowDTO.status` 与 `ApprovalChain.status` 从 `string` 收成对应联合；`STEP_TONE` / `STEP_LABEL` / `SEVERITY` 三张 keyed `Record` 因此变成穷举检查。落到产物里能看见：多出一个 `vocabulary-*.js` chunk，`MonitorView` chunk 里 `未开始` 文案仍在。
- **4 项变异全部被抓**（跑完 sha256 逐字节还原，`residue=none`）：store.go 声明一个常量而不进清单 ⇒ owner 守卫红；从 `webVocabularies()` 删掉一张表 ⇒ 名册守卫红；owner 换个顺序而生成的 `.ts` 没重跑 ⇒ `-check` 报 `web\src\types\vocabulary.ts is out of date.`；从 `STEP_TONE` 删一个成员 ⇒ `TS2741: Property 'skipped' is missing … required in type 'Record<…, CellTone>'`。其中第二项第一次跑是**编译不过**（注释掉三行字面量的首行留下悬空 `}`）——本会话第二次栽在同一件事上，改成正则删整条目后才成为有效变异：编译失败不是被抓。
- **顺手登记两件真实缺口（本批不改，改了就是行为变更）**：① `assignments.result` 的写入面比词表宽——`cmd/levee/grpc_change_executor.go:82-85` 的 `default:` 分支把**任意 run 状态**（`interrupted` / `rolled_back_partial` / `rollback_incomplete` / `running`）原样透传进这一列，而该列的 SQL 默认还是 `''`，所以"执行器结果"这列今天能被写成"运行状态"；把它接进渲染之前应当先收窄写入面。② `StatusExpired` 只由 `CheckExpiry` 产生，而 `CheckExpiry` **没有任何生产调用方**（全仓只有 `service_test.go` 调它）——也就是说超时的审批在跑动的系统里仍读作 `pending`。这条写成 `TestCheckExpiryIsTheOnlyProducerOfExpired`，将来谁接上驱动器它会红，并逼着注释一起更新；词表仍列出 `expired`，是为了接上那一路时线上格式已就位。
- **验证**：`go build ./...` 0、`go vet ./...` 0、`gofmt -l internal/ cmd/ scripts/` 空、`go test ./...` **68 包 ok / 0 FAIL**、`golangci-lint run --timeout 12m ./...` **0 issues**（第一次用默认超时被 1m 掐断，报的是工具自己的 timeout，不是代码问题）、`python -m unittest discover -s scripts` **59 tests OK**、`docgen -check` 两目标均新。前端：`npm run type-check` rc=0（并跑过上面的正对照）、`npm run test` **36 文件 / 314 例全绿**、`npm run build` rc=0；`internal/web/dist` 按 CI 同法重拷后**两次构建产物逐字节相同**（`git diff --numstat` 无非重命名差异、未跟踪项 0）。零 proto、零 schema 变更。

### 门禁（§5.4 的线性落地第一次有了机器守卫，而且是在浅克隆下验过的）

- `CONTRIBUTING.md` §5.4 把"PR 用 `--rebase` 落地"写成规则后，它只活在文档里：谁来合、用什么旗标合，既不在 PR 的 diff 里，`pull_request` 事件也看不见。这次补的是**唯一能证明的时刻**——push 到 master 之后，HEAD 就是落地那一笔提交。
- **步骤挂在 `release-gate` job 上，条件写在 step 而不是 job 上**，这一点是设计核心而不是风格：该 job 已经在扇入 `check (all jobs passed)` 的 `needs` 里，而聚合逻辑把 `skipped` 归到"produced no verdict — UNVERIFIED"并 `rc=1`（那段注释就是 2026-10-06 饿在队列里的四个 job 换来的）。把条件写成 job 级 ⇒ 每条 PR 都因一道"只在 master 才有意义"的检查变红。`scripts/test_landing_shape_step.py::test_job_level_condition_detected` 专门把这条死法钉住。
- **差点推上去一条必红的检查**：第一版用 `git rev-list --parents -1 HEAD | awk '{print NF-1}'` 数父提交，本机全克隆下 1/2 两种形状区分得干干净净——但 `release-gate` 用默认 `fetch-depth: 1`，**浅克隆里 rev-list 对真正的双父提交报 0 个父**（实测 `385d772`：rev-list 0 / cat-file 2），于是那条比较会把每一次正常 master 推送判成违规。现在改成读提交对象本身（`git cat-file commit HEAD | grep -c '^parent '`），与历史抓取深度无关，并**新建两个浅克隆分别跑通两种判决**（merge 提示父 ⇒ rc=1 且把两个 parent 的 sha 打出来；线性提示 ⇒ rc=0）。这条教训正是本仓那条老规矩的又一形态：**CI 断言必须在 CI 环境下可满足**，本机绿不算。
- **守卫自己的守卫**：新测试 `scripts/test_landing_shape_step.py` 断言步骤存在、`if` 仍同时限定 push 与 `refs/heads/master`、父数取自提交对象、比较对象是 `"1"`、失败路径 `exit 1`、`release-gate` 没有 job 级 `if`、并且仍在扇入 `needs` 里；`DetectorIsNotVacuousTest` 用合成文本跑同一组表达式（删步骤 / 拿掉 `if` / 放宽事件 / 改成只打印不退出 / 换成 rev-list / 加 job 级 `if`，六种都能看见）。同时把 `test_landing_shape_step` 加进 release-gate 的逐套件点名表——那张表存在是因为"discovery 收集到空套件也会 exit 0"。
- **两处自纠，都写进代码注释而不是悄悄改掉**：① 第一版探针依赖历史深度（见上）；② 点名表断言里我最初用全文件第一个 `needs: [...]` 去做包含检查，那命中的是 CodeQL 的 `needs: [vet, lint, test]`——断言会以错误的理由通过，改为先取 `check` job 的块再读它自己的 `needs`。步骤里那句解释性注释本身含有 `git rev-list --parents` 字样，曾让新测试把自己的注释当成违规（误报），因此 `step_code()` 只取 `run:` 体内**去掉注释行**的代码来投票：注释既不能满足必需项，也不能触发禁止项。
- **验证**：`python -m unittest discover -s scripts` **59 tests OK**（新增 11 条）；`yaml.safe_load` 能解析改后的 `ci.yml`（16 个 job）；抽取出的 run 体 `bash -n` 通过、带 `#!/usr/bin/env bash` 时 `shellcheck -S info` 干净（SC2148 只在无 shebang 的片段上出现，runner 实际以 `bash -e {0}` 执行）；两个浅克隆给出 rc=1 / rc=0 的正确区分。零 Go / 前端变更。

### 修复（`isValidTransition` 半转换的裸状态字面量收进 `runstatus`，并补两条判据）

- `internal/grpc/change_service.go:1630` 的 `isValidTransition` 里，九个 run 状态是**裸字面量**（`"paused"` / `"running"` / `"pending"` / `"approved"` / `"draft"` / `"planned"` / `"cancelled"` / `"completed"` / `"archived"`），而紧挨着它的 `archived` 那一支早已用 `runstatus.Status*` —— 一个函数里两种写法，正是"词表有第二个家"的形状（#153 与 `runs.status` 注释都为此付过账）。现在九处全部引用 `runstatus` 常量，**零行为变更**（同名字符串逐一替换）。
- **为什么既有全仓棘轮没看见这里**：`internal/runstatus/vocabulary_guard_test.go:190` 的 `TestNoBareRunStatusLiteralsOutsideRunstatus` 刻意只扫两条下划线判决（`rolled_back_partial` / `rollback_incomplete`），因为 `draft` / `running` 这类短名会在无关上下文里冒出来。这个盲区不是缺陷而是那条注释写下的取舍——所以补的不是"把全仓扫宽"（会造噪声、逼人加豁免），而是**一条定点棘轮**：`internal/grpc/status_transition_ratchet_test.go` 直接把产品源文件里的 `isValidTransition` 函数体读出来，断言 ① 体内不含任何 `runstatus.All` 成员的引号字面量，② 体内 `runstatus.Status` 引用数 ≥ 12（这条是防空壳：只删比较臂会让 ① 变成"没有字面量的真空"，② 立刻红），③ 抽取锚点找不到函数或函数体过短就大声失败（防止签名移动后守卫读到空串还全绿）。
- **顺手补上行为表**：该函数此前只有四对状态转移被测（`interrupted` 的四条），一次拼写改动不会惊动任何人。`TestIsValidTransition_TruthTable` 把 15 个源状态 × 8 个目标（含 `completed` / `interrupted` / 非状态串 / 空串）逐一列出期望值，**期望值是按状态机手写出来的、不是调用被测函数算出来的**（否则改代码就是改测试）。其中 `cancelled` 一支代码本身写成"拒绝列表"，所以测试里对同一支用两种读法交叉核对，而不是只信表格。
- **变异 4 项全部被抓**（跑完 sha256 逐字节还原，`residue=False`）：把 `"paused"` 字面量放回 ⇒ 形状守卫红；把比较臂掏空（无字面量也无常量）⇒ 常量计数与行为表同时红；`approved → paused` 这条**没有任何字面量、也没有缺常量**的行为改动 ⇒ 只有行为表看得见；把签名改成 `(to, from string)` ⇒ 锚点失效即报错（同时暴露参数顺序调换本身）。
- **验证**：`go vet ./internal/grpc/` 0、`gofmt -l` 空、`go test ./internal/grpc/ -run 'TestIsValidTransition|TestInterrupted'` 3 条全绿（含既有 `TestInterrupted_Transitions` 未受改动影响）。零 proto、零 schema、零前端变更。

### 变更（第一笔 rebase 落地后回填实测：`mergeCommit` 不为 null，而祖先关系断言失效）

- **#174 已用 `--rebase` 落地**，master tip `4ab1b10`，`git rev-list --parents -1 origin/master` 实测父数 = **1** —— 线性历史从这一笔起成立，本节是它的实测回填。
- **上一节里的一条"预期"被实测否掉，收回原因写在这里而不是静默改掉**：`gh pr view 174 --json mergeCommit` 返回的是 `4ab1b10`，**不是 null**。原文按"GitHub 不造合并提交"推出"字段为空"，推错了对象——`mergeCommit` 报的是**落到 master 上的那笔提交**，与它是不是合并提交无关。影响面现查为零（`scripts/`、`.github/`、`internal/` 无任何地方读 `mergeCommit` / `baseRefOid` / `mergedAt`），所以没有门禁被这条误判掩护；但"rebase ⇒ mergeCommit 为空"这个假设本身是错的，已在 §5.4 改写。
- **实测出来的真坑是 sha 重写**：同一笔改动 PR 侧 `841d7f0`、master 侧 `4ab1b10`，`git diff --stat 841d7f0 4ab1b10` 为空 —— **树相同、sha 不同**。于是 ① `gh pr view --json commits` 给的是 rebase 前的 sha，它不在 master 谱系上；② 本仓反复使用的 `git merge-base --is-ancestor <PR头> origin/master`（今天核 #172 / #173 用的就是它）在 rebase 落地后会**假阴性**，判据必须换成 `mergeCommit.oid` / `git log --grep` / 比树三选一。这条对本仓库的取证习惯影响比对新代码更大，因此同时写进 `CONTRIBUTING.md` §5.4。
- **验证**：`CONTRIBUTING.md` 结构未破（`### 5.4` 与 `## 第6章 测试要求` 各计数 1）；CHANGELOG 结构守卫在内 `python -m unittest discover -s scripts` 48 tests OK；纯文档改动，无 Go / CI / 前端文件命中。

### 变更（协作口径：PR 改用 rebase 落地，既有历史不改写）

- **落地方式从 merge commit 换成 `gh pr merge <n> --rebase`**，规则写在 `CONTRIBUTING.md` §5.4（新增一节）。此前每个 PR 都是一个双父的 `Merge pull request #N`，而这种提交在这里不携带决策——它只是把分支名再抄一遍；squash 也不取，它会把"一笔提交＝一个决定"压掉，而分笔正是各批次把守卫与产品码分开的用意。**历史不改写**：master 上 `385d772` / `ff669ed` / `5139395f` 等合并提交仍是当时的事实，两种形状并存是预期而不是脏。
- **这条与 §5.1 的"切 base 不补跑"是同一问题的两半**：#170 与 #172 的两份绿 CI 都不在合并那一刻的 master 上（分别跑在昨天的 base 与 `d02f379`），最后是靠 #172 合并后 master 的 push CI（24 job 全绿，含 `docs regenerate check`）才把"合并树"这件事补成证据。rebase 之后 master 上那笔提交就是 PR 里验过的同一笔内容，PR 级证据与主干级证据不再靠人去对齐。
- **写之前先现查**：`scripts/`、`.github/`、`internal/` 里**零处**读 `mergeCommit` / `baseRefOid` / `mergedAt`，所以这一改不会静默弄坏发布或取证脚本；该负结果带正对照——同一条 grep 在 `docs/release-notes/*.md` 命中 **20** 处 `PR #` 写法（第一次只看了 `head -20` 的截断输出就写下"14 处"，复读时按实测改回），证明不是目录或 glob 写错造成的假阴性。仓库设置侧 `allow_rebase_merge=true`（现查 `gh api repos/Levango7/Levee`），不需要谁去改配置这条口径才成立。**"rebase 落地后 `mergeCommit` 返回 null"写成待实测断言**，等第一笔落地校正，不当定理用；分支保护里唯一的 required context 仍是 `check (all jobs passed)`（`enforcement_level=non_admins`，现查 `.../branches/master`）。
- **这条口径目前没有机器守卫**，并且这是决定而不是疏漏：可以在 `ci.yml` 的 master-push 路径上断言"标记提交之后不得再出现合并提交"，但那要同步扇入的 `needs` 清单与判决字符串——本仓在扇入清单上踩过两次（`docs` 曾出现在 `needs` 而不在判决串里），所以留作另一笔单独决定。
- **验证**：纯文档改动，零 Go 代码、零 CI 拓扑变更；`grep -c '^## 第6章 测试要求' CONTRIBUTING.md` = 1 且 `### 5.4` 落在 §5.3 与第6章之间（标题锚点改动后复读，防"编辑吃掉标题"这一类自伤）、`python -m unittest discover -s scripts` 的 CHANGELOG 结构守卫通过（`## [Unreleased]` 仍唯一且在行 5）。

### 词表单一来源的第二增量（集群 / 批次 / 派发 / 会话四套取值改由 owner 数据生成，前端与守卫都不再解析抄件）

- **接上增量 1 留下的前提**：那笔登记的话是"cluster / batch / step / session 四套词表的 owner 只导出散常量、没有清单可导出，硬接 docgen 会让生成器回去解析 Go 源码＝把刚消灭的弱点搬进生成器内部"。这一笔先满足前提再接生成：`internal/cluster/vocabulary.go`（`NodeStatusNames()` / `NodeRoleNames()`）、`internal/state/vocabulary.go`（`BatchStateNames()` / `AssignmentStateNames()`）、`internal/conversation/vocabulary.go`（`SessionStateNames()`）各导出一份**带顺序的数据清单**，docgen 的 vocabulary 目标从 2 张表长到 **7 张 / 43 个取值**，`web/src/utils/{node,batch,assignment,session}.ts` 里那四份手抄数组删除、改为取值。**常量留在原地**：它们出现在 switch case 里，Go 的 case 只认常量，所以"清单只有一份"不等于"把常量改成变量"。
- **解析没有消失，是搬回 owner 包里、只搬一次**：三个 `vocabulary_ownership_test.go` 检查"本包的清单 == 本包常量块的全部声明"。conversation 那份不另立清单，而是**从 `String()` 现取**，并钉两件解析也救不了的事：枚举上界 `sessionStateLast` 是否跟着最后一个声明走、有没有两个状态共用同一个拼写（漏写一个 `case` 就落到兜底分支变成 `unknown`，于是清单里出现两个 unknown）。跨语言那一侧从此只比**数据与产物**。`String()` 的兜底拼写同时提成具名常量 `SessionStateUnknownName`：它进清单是因为越界的行确实会以这个词到达控制台，但它不是状态机里的一个状态——这一点写进生成物的文档注释。
- **四条守卫改的是判定对象**：`internal/cluster/node_vocabulary_test.go`、`internal/state/{batch,assignment}_status_vocabulary_test.go`、`internal/conversation/session_state_vocabulary_test.go` 各删掉"解析 TS 抄件"与"解析 Go 常量"两半（合计 5 个解析辅助函数没了），换成①产物逐项等于 owner 数据（含顺序）；②消费文件只接线不重抄（`export const X = [` 与 `export type X =` 两种手抄形状都判红）；③**没有 Go owner 的那一半原样保留**：中文标签表逐状态覆盖、`TONE_BY_STATUS` 的键集与注册表同集、每个 tone 在 `web/src/styles/base.css` 里真有 `.lv-dot--<tone>` 规则（`warning` 与真名 `warn` 只差一个字母，后果是一个没有背景的圆点）。批次那条的行为测试（`BatchSummary` 按写入端字面量种数据）不动。
- **顺手补掉我自己在增量 1 里写下的第三份抄件**：四个 vitest 用例原本各自写死成员清单（例如 `expect([...BATCH_STATES].sort()).toEqual([七个字面量])`）。改接线之后那条断言的含义已经变了——它比的是"我抄的清单"与"生成物"，正是本笔要消灭的形状——所以换成这一层还能证明的事：**同一个对象从 `./batch.ts` 出来**（`expect(BATCH_STATES).toBe(FROM_OWNER)` + 非空 + 无重复）。这个替换让判红面变大而不是变小：变异 N3 把 `batch.ts` 换回一份同值的本地数组，**旧的成员清单断言是绿的**，而新的 identity 断言与 Go 侧接线断各自判红。
- **变异 6 条，其中一条刻意"应该绿"**：N1 在 `node.go` 声明第四个状态而不进 `nodeStatusOrder` → owner 完整性测试红、`docgen -check` **按分工为绿**（漏登记的常量不改产物字节；把这条如实写成"预期绿"才让汇总行的"0 逃逸"有意义）；N2 清单漏一个常量 → owner 红 + `-check` 红；N3 见上一条；N4 让两个会话状态同名 → owner 红（两条既有测试同时红）；N5 在枚举顶上加状态而不放宽上界 → owner 红；N6 手改生成物一个取值 → `-check` 点名该文件 + 集群守卫红。三条基线先验全绿才注入，逐条 `finally` 还原并核 md5 与原字节相同。驱动脚本自己在两跑之间崩过一次（把 vitest 在仓库根跑、解析不到 `node_modules`，报出与源码无关的"基线红"）——**基线红不等于产品红**，这条按事实记录而不是抹掉。
- **CI 覆盖面这件事是先发现、后补的**：本笔开 PR 时把 base 指向 #170 的分支（stacked，为让评审面只剩这一笔）。而 `ci.yml` 的 `pull_request` 触发只列 `main` / `master` / `develop`，那种 base 下 **24 个 check run 一个都没跑**（含 `check (all jobs passed)`——它就是 master 分支保护里唯一的 required context），PR 上只剩 CodeQL 两条与一条按设计 skipped 的第三方检查：「没有红」在界面上长得像「绿」，但它不等于验证。已把 base 切回 `master`；**切 base 本身不会补跑**（GitHub 记为 `edited`，不在 `ci.yml` 声明的默认事件类型里），所以这一笔同时把这条约束写进 `CONTRIBUTING.md` §5.1，并靠那次真实推送把 CI 起起来。
- **与新 master 提交 `d02f379e`（#171 门禁失败进聊天）的合并后状态是本机复现的，不是推断**：`663d018..d02f379e` 改 11 个文件，`663d018..be56fa6` 改 56 个，交集只有 `CHANGELOG.md`；临时工作树里真做一次 merge，合并树上门禁全绿（`go build ./...` 0、`go vet ./...` 0、`gofmt` 空、`go test ./...` **68 包 ok / 0 FAIL**、`docgen -check` up to date、`python -m unittest discover -s scripts` 48 OK——CHANGELOG 结构守卫对「两侧都往 `## [Unreleased]` 顶部插整节」不敏感，这条当时最怀疑所以专门跑过）。merge 对 `web/` 与 `internal/web/dist` **零命中**，故前端三道门（314 例、`vue-tsc`、dist 逐字节）对合并树同样成立——同一份字节，不是「应该没问题」。夹具工作树用完即删。
- **验证**：`go build ./...` 0、`go vet ./...` 0、`go test ./...` **68 包 ok / 0 FAIL**、`gofmt -l internal cmd tests` 空；`vue-tsc --noEmit` 干净、`npm run test` **314 例 / 36 文件全绿**。产物这一笔**不需要回填**：按 CI 同法重跑 `npm run build`，49 个文件全部新写入（mtime 与构建同分钟）却与入库的 `internal/web/dist` **逐字节相同**（`diff -r` 0 行、`git diff HEAD -- internal/web/dist` 空）——rollup 把再导出解析进同一个共享块，"本地数组"换成 `import`+`export` 不改变最终块内容。
- **登记不修（同族但这一笔不动）**：① `internal/state` 的 `StepStatus*`（5 个）与 `ApprovalStatus*`（3 个）以及 `AssignResult*` 结果组**没有 TypeScript 镜像**——`StepRowDTO.status` 现在是开放 `string` 加一句注释，把它们接进 docgen 会顺手把它变成联合类型、要求每个消费点处理未知拼写，那是界面行为变更，要单独定；② `internal/diagnosis` / `internal/grpc` 的健康与裁决词表仍由"解析 Go 源码"的守卫盯着（如 `system_health_vocabulary_test.go`），它们没有前端抄件，所以不在"消灭第二份手抄"这条线上，但"owner 导出清单"这个判据对它们同样适用；③ `change_service.go:1630` 的 `isValidTransition` 仍混用裸状态字面量与 `runstatus` 常量（增量 1 已登记，本笔未扩大棘轮扫描面）。

### 词表单一来源的第一增量（run 状态与优先级两张表改由 `internal/docgen` 生成）

- **动的是哪两份抄件**：`web/src/types/levee.ts` 的 `ChangeStatus`（15 个）与 `Priority`（4 个）联合——本仓"Go 侧有 owner、TS 侧靠手抄"的词表里，这两张是**唯一被写成清单**的那两份（其余四张见下一节，同日收口）。守它们的测试是 **Go 侧解析那份手抄再与 owner 比对**——这正是 #153 的失败形状（`wireContract.spec.ts` 的 `DTO_READS` 是手抄表，守卫核对的是自己的抄写，于是集群页三个读数恒为默认值而全程不报错）。这次删掉抄件而不是继续给它加门禁：`internal/docgen` 从单目标扩成 target 列表（每个目标自带 marker 语法与 locate 指纹），新目标 `web/src/types/vocabulary.ts` 由 `runstatus.All` 与 `state.PriorityNames()` 直接生成，`levee.ts` 改为取值于它、`utils/format.ts` 的 `PRIORITIES` 数组改为再导出（既有 import 路径一条不破）。顺序也是契约的一部分：优先级按紧迫度从低到高，是实例化向导下拉的呈现顺序。
- **CI 不需要改动**：`.github/workflows/ci.yml` 早有 `go run ./internal/docgen -root .` 与 `-check` 两步（342 / 346 行），遍历目标列表 ⇒ 新目标自动被钉住。**加一个状态的流程因此变成**：改 `internal/runstatus` → `go run ./internal/docgen` → 前端与规范文档一起跟上；忘了第二步就是 CI 红，而不是界面静默少一项。
- **顺带换来一扇类型门**：`STATUS_LABEL: Record<ChangeStatus, string>` 此前钉在一份手抄联合上，上游加了状态而前端没配标签，两边可以一起错下去；现在 `ChangeStatus` 由生成的数组派生，**漏配标签会直接是 vue-tsc 编译错误**。中文标签与颜色表刻意仍留在 `utils/format.ts`——它们没有 Go owner，生成器产不出措辞（"生成器凭无有中造出内容"就是编造）。
- **实付的两条教训**：① `export type { X } from './vocabulary'` 单独一句**不会**把 `X` 带进本文件作用域，`Change` 接口当场找不到名字；`npm run build` 报出 4 条错（`types/levee.ts:17`/`:18` 的 `Cannot find name`，加上下游 `useRollbackRemediation.ts:49` 与 `MobileApprovalView.vue:259` 两条 TS7053 级联），改为 `import type` + `export type` 两句。② **单包复跑绿、全包红**：两条新写的"接线"断言把 re-export 写死成那一种拼写，而修作用域的改动就在两跑之间发生——判据挂在拼写上，而不是挂在"名字从哪儿来"这个性质上。断言因此改成 `import|export … from './vocabulary'` 两形皆认，另补一条 `NotRegexp` 钉住"不许在消费文件里重新声明联合"，**判红面没有因为放宽而变小**。
- **守卫改的是判定对象，不是删断言**：`TestWebStatusMirrorMatchesGo` 从"解析 levee.ts 的联合"改为"比对生成的数组与 `runstatus.All` 逐项相等（含顺序）"；新增 `TestWebStatusVocabularyIsWiredNotCopied`（生成的联合必须是派生形式 + `levee.ts` 只接线不重抄 + 逐个状态断言它不在 `levee.ts` 里以字面量出现）；`internal/state` 侧同构（`webUnionMembers` → `webConstListMembers`、新增 `TestWebPriorityVocabularyIsDerivedNotReSpelled`、`PRIORITIES` 数组断言改读生成物，因此"owner 加了值而忘记重新生成"会在这里以长度不匹配出现）；`priorityDisplay.spec.ts` 里那条"format.ts 仍然拥有 PRIORITIES"改成"每种形态一个家"，并对 format.ts 里重新出现的字面数组判红。docgen 自带 17 条测试（本批新增 8 条），其中阳性对照是往 `runstatus.All` 植入一个状态、断言已提交的 `vocabulary.ts` 不含它且 `replaceBlock` 产出不同字节——否则"生成器不再产出差异"这件事本身坏了也没人知道。
- **变异 6 条全部咬住**（先验基线三条全绿才允许注入，逐条 `finally` 还原并核 md5 与原字节相同）：M1 owner 加状态不重新生成 → `-check` 同时点名 spec 与 vocabulary.ts，`TestWebStatusMirrorMatchesGo`、`TestRenderedBlockEqualsTheCommittedFile` 各自判红；M2 手工重排生成的状态顺序 → `-check` 红 + 顺序断言红；M3 在 `levee.ts` 恢复手抄的 `ChangeStatus` 联合（含历史上的幻影值 `pending_approval`）→ `TestWebStatusVocabularyIsWiredNotCopied` 红，而值比对那条**看不见**——这正是新增断言的射程；M3b 同构地恢复 `Priority` 手抄 → state 侧红；M4 调换 `priorityOrder` 顺序 → state 的数组顺序断言 + `-check` 双红；M5 在 `format.ts` 把 `PRIORITIES` 重新抄成字面数组 → vitest 结构守卫红。
- **验证**：`go build ./...` 0、`go vet ./...` 0、`go test ./...` **68 包 ok / 0 FAIL**、`gofmt -l internal cmd tests` 空；前端 `npm run test` **314 例 / 36 文件全绿**、`vue-tsc --noEmit` 干净、`vite build` 成功；嵌入产物按 CI 同法回填后 `diff -r web/dist internal/web/dist` **0 行**、`git ls-files --others -- internal/web/dist` **0**。**工具链等价性另做了一次对照**：本机 node v26.8.2 而 CI 钉 20，所以在 `663d018` 的分离工作树里重跑 `npm ci && npm run build`，重建结果与该提交已入库的 `internal/web/dist` **逐字节相同**（`diff -rq` 0 行）——"产物新鲜"这句因此是对 CI 那一步成立，而不只是对本机成立。`python -m unittest discover -s scripts` 48 tests OK。
- **登记不修（下一笔的前提，不是遗漏）**：① `cluster` / `batch` / `step` / `session` 四套词表仍是"Go 测试解析 TS 抄件"的形状，根因是它们的 owner 只导出散落的常量、没有可导出的清单——把 docgen 接上去之前，owner 必须先导出数据（`All` 那样的切片），否则生成器只能回去解析源码，那等于把本批刚消灭的弱点搬进生成器内部；**这一条已由上一节（同窗口的第二增量）落地**，`cluster` / `batch` / `assignment` / `session` 四套都改成了 owner 导出清单 + 生成物，剩下的 `step` / `approval_status` / `AssignResult*` 三组是因为**根本没有前端抄件**（把开放 `string` 收成联合类型属于界面行为变更，另定），理由记在那节的"登记不修"里；② `internal/grpc/change_service.go:1630` 的 `isValidTransition` 仍混用裸字面量（`"paused"` / `"running"` / `"cancelled"`）与 `runstatus` 常量，而 `TestNoBareRunStatusLiteralsOutsideRunstatus` 刻意只扫两个最有辨识度的回滚判定拼写，短名会撞上无关上下文——这是既有的扫描面边界，本批没有扩大它。

### 新增（详情页显示存量计划：只读取那一条入口，并顺手把 wire 从没发过的三个字段从类型里摘掉）

- **#18 的最后一环（前端侧）**。#155 让 `GetChange` 真能带回计划，但详情页仍调 `changesApi.get(id)` 不带 `includePlan`、也没有渲染面——后端能力为真而界面看不见。现在 `changesApi.get(id, { includePlan: true })` + 「执行计划」面板（批数 / 目标数 / 每批主机 / 并发上限 / impact summary），无计划的 draft 说"还没有存量计划（草稿阶段的常态）"而不是留白。**刻意只用这条只读入口**：`POST /changes/:id/plan` 是**生成**计划，一个读页面去造产物会改变它声称只是显示的东西——而这一页的全部意义是"这就是被批准的那一份"。
- **现查抓到的类型谎言（三条，零写入方）**：`Plan.estimatedDuration`、`Plan.potentialConflicts`、`Batch.waitAfter` 在 Go 侧**没有任何赋值**（`grep EstimatedDuration|PotentialConflicts|WaitAfter --include=*.go` 连测试都零命中；`plan.Batch` 里根本没有对应数据来源），protojson 于是把它们从每个响应里省掉。TS 却把三者声明为**必填** ⇒ 任何读者拿到的都是 `undefined` 而类型说不是。已删除，并在类型处写明"若网关将来开始填，快照会先长出键、`wireContract.spec.ts` 会要求前端重新声明"。删除是安全的：全仓对这三个名字**零读取方**（只有声明本身）。
- **契约补齐补在两层，不是顶层**：`internal/grpc/wire_snapshot_test.go` 新增 `change-with-plan` 条目，`objects.plan` 与 `objects['plan.batches']` 两层键集都记录在案（顶层之外的这一层正是当年 cluster DTO 漂移处），`web/src/api/wireContract.spec.ts` 注册 `objectDtos: { plan: 'Plan', 'plan.batches': 'Batch' }` 做双向核对。**反向对照实跑**：往 `Plan` 里塞回 `estimatedDuration` ⇒ 守卫精确报 `change-with-plan: Plan declares estimatedDuration but the wire does not send it`，随后逐字节复原。同一轮还补了源解析的一处盲区：`Change`/`Plan`/`Batch` 声明在 `types/levee.ts` 而非 `api/index.ts`，故 `fieldsOf()` 先查 `api/index.ts`（**先命中者胜出**，避免同名接口悄悄换了一份被扫描的声明）、查不到才回落 `types/levee.ts`。
- **两条"声明了但没人执行"的既有缺陷顺手坐实**：① `forbid` 列表此前**两侧都无人读**——Go 不核对、TS 的 `WireRow` 里连这个字段都没解析，于是它一直是装饰；现于 `TestWireSnapshot_MatchesContract` 逐条断言"被禁键不出现在 keys / rows / objects 任一层"。② `arrayKeys` 只看 `rows[0]`，而 protojson 省零值 ⇒ 首批的 `index`（值为 0）会被漏记成"wire 不发 index"；新增的 `nestedArrayKeys` 跨行取并集，且为此把契约夹具的计划改成两批（**这不是为了让测试好看**：单批夹具下 `index` 真的看不见，而它确实在 wire 上）。顶层 `arrayKeys` 的既有行为原样保留——八个条目都是在它之下快照的，重驱它们是另一批的事。
- **默认不回显这条性质现在是实测而非前提**：契约夹具先把计划存进 run，再断言不带参数的响应**不含** `plan`、带参数的**含**——此前 `change` 那条快照"没有 plan"只是因为 fixture 根本没计划。
- **protojson 省零值这条，渲染层必须接住**：第 0 批在 JSON 里**根本没有 `index` 键**、不限并发的批没有 `maxConcurrency` 键。模板若按数字读，格子就是空的（与"客户端读到 wire 没有的键 ⇒ 一片空白但不报错"同族）。故渲染写成 `b.index ?? 0` 与 `b.maxConcurrency || '不限'`（Go 侧注释本就写着 zero means unlimited）。这一条也是**先写完再回看时抓到的**：最初的实现用的是 `b.index` 与 `b.maxConcurrency === 0`，前者对 undefined 无感、后者对 undefined 判 false ⇒ 两处都会静默落空。
- **前端结构守卫六条，各配一条变异**：不带 `includePlan` 取数 / 改调生成端点 / 删掉 draft 文案 / 面板去读 `estimatedDuration` / 把 `index` 当必有键直接读 / 把"缺 `maxConcurrency`"当成 0 而非不限——六条都各自把对应断言打红（`OK red -> …` 六行），跑完逐字节复原（`ChangeDetailView.vue` md5 `515d66a9…` 前后同值，22,546 字节）。变异脚本第一版自身崩过一次：python `subprocess` 直接跑 `npx` 触发 `WinError 2`（npx 是 `.cmd`，CreateProcess 不认），崩在产品码已变异、还原未走到 —— 已改为 `node node_modules/vitest/vitest.mjs` 起测、加"先验基线必须绿才允许注入"和 `finally` 还原，现场随即复原。
- **dist 按 CI 同法回填，并在最终 markup 上重跑一遍**：`npm ci`（真目录，非 junction）⇒ `npm run build`（`vue-tsc --noEmit` + vite）⇒ 清空 `internal/web/dist` 后拷回。第一次回填发生在零值渲染修正**之前**，所以那轮的"产物已核对"对最终代码不成立；重建后 `diff -r web/dist internal/web/dist` 为 0 行、`git ls-files --others -- internal/web/dist` 为 0（即 CI 那两条检查的等价形态本机先跑过），并在产物里 `grep` 到面板文案（`并发上限`）与 draft 文案，证明嵌入的 UI 不是旧版。
- **文档与头注释同步**：`docs/levee-api.md` 那段原写"前端未接（本批只做后端能力）"，已改为如实说明控制台走的是只读入口，并把"proto 声明六个、网关只填四个（`Batch` 四填三）"写进契约口径。详情页头注释同理——它原来宣称本页汇聚四类指纹却显示不了 plan/gates，如今两条都各有真来源（plan 来自存量 artifact，gate 判定来自 #154 落库的 `gate_check` trace 行，由既有时间线原样印出）。
- **验证**：`node node_modules/vitest/vitest.mjs run` **314 例全绿（36 文件）**、`npm run build` 退出 0、`go test ./...` **68 包 ok / 0 FAIL**、`go build ./...` 0、`go vet ./...` 0、`gofmt -l internal/ cmd/` 空、`golangci-lint run ./...`（单实例、`--timeout 12m`）**0 issues**、`docgen -check` 通过、`python -m unittest discover -s scripts` 48 tests OK（含 CHANGELOG 结构守卫）。不动后端行为、不动 proto。


### 新增（门禁失败进聊天：EventGateResult 终于有了发送方，范围刻意只做失败）

- **#38 收口**。`chatops.EventGateResult`（`bot.go:88` 定义、`card.go:229` 有渲染分支、`card_test.go:114-127` 有用例）**全仓零发送方**——卡片写得漂漂亮亮，没人把事件投进去。这正是 #154 那类"类型存在"与"调用点存在"长得很像的缺陷，所以这一批的验收口径从一开始就不是"能不能渲染"，而是"生产路径里有没有人 produce"。
- **口径两条（已定档）**：① **只广播失败的判定**，通过的不发；② **接受全局扇出**——ChatOps 没有"按变更路由到某个群"的数据面（`feishu/slack/dingtalk` 三个适配器都**丢弃** `channel` 参数，webhook 本身绑一个群；`internal/state/schema.sql` 里没有任何存房间/群的列，`runs` 也没有），所以收件人只能是"全部已配置 bot"。在这种扇出下把每批每门禁的绿色判定都发出去会淹掉这个群真正承载的审批请求，而最近的先例（回滚 escalate，`rollback_grade.go`）同样只在**不健康**时说话。通过的判定不是无处可查：它们逐条落 `gate_check` trace 行（#154），控制台时间线原样显示。
- **接缝放在 engine、绑定放在 wiring、策略放在 bridge、安装放在 serve**，四处都不成环（现查 `go list -deps`：`internal/chatops` 的内部依赖只有 `internal/log`；`internal/wiring` 已 import `chatops`+`notify`；`chatopsbridge` 已 import `engine`；`engine` 不碰 chatops）。engine 侧新增 `GateVerdict` 类型 + `GateObserver`（函数类型、**不返回 error**，为的就是"通知路径不能变成 run 的第二种失败原因"）+ `WithGateObserver`；wiring 用 `changeBoundGateObserver(changeID, obs)` 把**变更身份**绑上去（engine 只有自己那个临时 id，卡片若印它操作员无从下手——这与 #154 修的是同一个身份缺口）；`chatopsbridge.OnGateVerdict` 只做"失败才发 + 组 Event + 广播失败只 Warn"。
- **顺手把审计行与广播收成同一份真值**：原先 engine 在调用处手搓 `detail map[string]any`，广播若再读同一个 map 就要按字符串键取值（本仓反复付学费的"按名字取口径"）。现在 `recordGateVerdicts` 只产 `GateVerdict`，audit 的 detail 由 `v.AuditDetail()` 派生、广播读字段，**同一份判定只有一个来源**；`extra map[string]any` 参数一并换成显式 `batchID string`。行形状未变（`AuditDetailKeepsRowShape` 五种组合逐一断言，含零延迟仍出 `latency_ms` 这条"键的存在才是契约"）。
- **两个 sink 是独立的，这一点专门写了测试**：第一版的 `recordGateVerdicts` 有 `if cr.gateRecorder == nil { return }` 的早退——照搬下去会静默变成"没装审计 = 也不广播"。`ObserverFiresWithoutRecorder` 断言无 recorder 时观察者仍收到 4 条、`FailingGateStillRollsBackWithObserverInstalled` 用**引擎自己的 VerifyResults** 做基线对照（而不是拿"没装观察者时那份必然空"的切片去比长度——那是一条恒真的假对照）。
- **最后一跳用顺序断言钉住**：`cmd/levee` 里 `WithGateObserver(...)` 必须出现在 `eng = wiring.NewEngine(store, opts...)` **之前**——写在构造之后就是"算了但丢了"。三条源码守卫（安装位置 / bot 先于 engine 构造 / `serveServices` 仍带回 mgr 且 `defer stopServeChatOps` 仍在）都带 `require.Greater(idx, -1)` 防"锚点挪走导致守卫空跑"。为此把 bot 构造从 `changeSvc` 之后**上提**到 engine 之前，审批镜像仍留在原位（它需要 changeSvc），两处共用同一个 bridge 实例。
- **卡片以"渲染出来能读"为验收**：`CardActuallyRendersTheGate` 不断言字段填了就算，而是调 `chatops.BuildCardForEvent(evt)` 后在渲染文本里找 变更ID / 门禁名 / "失败" / phase / 原因，并断言 `card.ChangeID` 是变更 id 而 `card.RunID` 保留 exec id。`gateStatus` 依 `v.Passed` 出词而不是硬编码"未通过"——将来放宽范围时它不会说谎。
- **本轮新测试 15 条全绿**（engine 5、bridge 6、wiring 2、cmd 守卫 3 中的观察面），并做了**探针活性自查**：统计"哪些事件类型仍零发送方"时先用宽探针（不排除任何包）复核，确认那四个 0 不是被我的过滤条件滤掉的。**由此登记的下一步缺口**：`EventStateChange` / `EventRunStarted` / `EventRunCompleted` / `EventRunFailed` 四个类型仍只有定义 + 共用 `card.go:229` 那一个渲染分支，**生产侧零 producer**（非测试命中仅 `bot.go:85-91` 与 `card.go:229`），而 `approval_requested` / `approval_decision` / `gate_result` 三个有。变更终态没人播报这件事属于另一个决定（发哪些终态、跟 notify 的 webhook 渠道如何分工），本批不动。
- **文档同步**：`config.example.yaml` 的 chatops 出向范围原写"审批创建/决策事件"，已补上门禁失败，并把"为什么只发失败 / 为什么只能全局扇出"两段理由写进注释（不是留个开关让人猜）。`docs/release-notes/v1.22.0.md:181` 那句"门禁判定的 chatops 广播同样未接，已单独登记"**保持原样**——它是发布当时的事实，按"带日期的报告只追加不改写"处理，本小节即为它的后续。
- **验证**：`go build ./...` 0、`go vet ./...` 0、`gofmt -l internal/ cmd/` 空、`go test ./...` 全绿、`-race` 于 `engine`+`wiring`+`chatopsbridge`+`cmd/levee`、`golangci-lint run ./...` 0 issues、`docgen -check` 通过、`python -m unittest discover -s scripts` 48 OK。零前端、零 dist、零 proto、零 schema 变更。

### 新增（被服务而未判定的三个面收口，接线守卫从"点名"改"穷举"）

- **抓的是"清单写错了但门禁全绿"这一类**。`cmd/levee/serve_policy_wiring_test.go` 原先用**点名清单**描述受控集（文件注释写 "seven services"、roadmap 那行写"六个服务"），于是"少了一个服务"在测试里根本不可见。现查 `AlertService` / `DiagnosisService` / `ConversationService` 自登记进 `serve` 起就没有策略判定，而门禁始终全绿。形状与 #154 同族（"类型存在"与"调用点存在"长得很像），方向相反：那次的洞是**加法**（发了事件没人收），这次是**减法**（受控集写小了没人报）——减法更危险，因为它让"漏判"读起来像"设计如此"。
- **诊断按目标主机自己的环境判**：`Diagnose` 要 `apply`、`GetDiagnosis` 要 `view`，环境由新增的 `grpc.TargetEnvResolver(store)` 从主机清单的 `labels["env"]` 解析（id 优先、其次裸主机名——控制台的主机选择器传 id，告警驱动的 `instance` 标签传主机名，两种都会来）。三种情形分开处理且各有各的理由：未登记的主机**拒**（没有可归属的环境，就没有"用平台凭据在它上面执行采集"的许可）；已登记但没打标签的落回 `permission.default_env`（配置里的答案，不是猜）；装了 authorizer 却没装 resolver ⇒ `FailedPrecondition`——半接的接线必须被点名，悄悄退回默认环境会产出"关于没人问过的环境的授权结论"。`Diagnose` 的判定**先于任何采集**，被拒的三条后果各有一条断言（不执行、不花凭据、不留缓存报告）。resolver 读**租户包装后**的 store 而不是 `state.Underlying(store)`：矩阵不知道租户这回事，能解析出真实环境就等于让 dev 持 `apply` 的主体跨租户诊断；代价是拒绝文案说"清单里没有这台主机"，而更准的说法是"这台不是你的"——两处都写进了注释。`GetDiagnosis` 要等 id 解析出来才知道该判哪个环境，于是报告 id 成为一个**弱存在预言**（能区分"没有这份报告"与"不是你能读的"），这条取舍写进方法注释与交付边界，没有当实现细节咽下。
- **对话两面 + REST 四端点接现有 token，不新增动作词**。`SendMessage` 要 `plan`、`SubscribeConversation` 按读要 `view`；REST `/conversation/sessions*` 那四个端点绕过 gRPC 服务层，`SendMessage` 的门禁按构造覆盖不到，于是收在 `dispatchConversation` 判一次（GET→`view`，POST/DELETE→`plan`）——不接就留下一条"能开会话、能发消息"的绕行面。对话请求里没有环境，按 `default_env` 判并写明这是运维刻意配的那个回退；它也不是唯一一次判定，对话唯一能触发的变更动作仍带着**变更自己的环境**在 `ChangeService.CreateChange` 再判一次。否掉"新增 `alert` / `diagnose` 动作 token"的理由记进文档：未登记的动作在每份既有矩阵上都没有授权，而本部署口径是"无矩阵即不强制"，加 token 就是 fail-open。
- **`AlertService` 刻意保持不判，并把"不判"做成可观测事实**。载荷（`AlertMessage`）只有 id / source / severity / ends_at，无 team、无 env、无可解析成 env 的 target，环境键控的矩阵没有可判的输入；两条捷径都说谎（按默认环境判＝把监控源归给恰好拥有默认环境的那支队；加 token＝fail-open）。待办（每源入向凭据或签名、告警流可见性规则）登记在 `ReceiveAlert` 的注释里，`docs/identity-deployment.md` §6 第 6 条把它列为交付时要主动说明的边界。
- **守卫改造：受控集由 proto 枚举，例外表自身被断言**。服务全集取自编译进二进制的描述符（`protoregistry.RangeFilesByPackage("levee")`，实测 **10** 个），每个服务必须落在受控集或例外表之一，两者不相交、并集与全集**逐字节相等**；例外表钉条数（棘轮，改动必须是这一行的决定）、理由必须非空、必须带至少一个探针。探针判据是"未授权主体与已授权主体得到**同一个答案**"——那才是"不受控"的可观测形式，哪天给它加了门禁这条就红，例外不许悄悄过期。另有非空对照 `TestServeAlertIngressReallyWorks`：mallory 的告警确实被 `accepted` 且随后可读，否则"两个主体答案相同"对一台拒绝所有人的服务同样成立。
- **每个受控服务两条腿，且拒绝文案里的动作名是判据的一半**：`deny`（mallory 必 `PermissionDenied`，且报错必须带上它判的那个动作 `apply`/`view`/`plan`/`admin`）与 `allow`（dave 在 dev 持 `admin`，任何调用都不许回到策略答复）。`via` 不是装饰——"把写按读判"这条变异在"403 是否还出现"上完全看不出来，是文案里 `plan`→`view` 抓住的；REST 那条同样靠 `Contains(body, "plan")` 而不是只看状态码。
- **顺手核掉"还有没有直读 store 而不受判的 REST 面"**：`rest.go` 里直接用 `gw.store` 的 handler 共三处（集群面板 / 批次进度 / 步骤与趋势），批次与步骤两条带 run 级环境判定，集群面板那条在 `internal/grpc/rest.go:1888` 带自己的 `authorizeResourceRead`（现查，不是引用旧记录）；对话四个补上后这一片不再有已知缺口。
- **变异脚本自己贡献三条教训（都是"工具会静默骗人"的形状，都已修）**：① 第一版把"删掉门禁调用"当变异，`permission` 于是成了未用导入 ⇒ **包编译不过**，脚本按 broken 拒收——编译失败不是被抓，变异必须保持类型正确（改用"把传给门禁的 authorizer 换成 nil"这类等价削弱）。② `SubscribeConversation` 的 carol 探针原本用 `context.Background()`：门禁一旦被删，这条用例**挂死十分钟**而不是变红（harness 把它记成 broken，我第一版差点把它当成"没抓到"）。改成进门即已取消的 context（`endedStream`）：门禁在则立刻拒、门禁不在则 `require.Error` 立刻红——把挂死换成快失败，这是测试设计的缺陷而不是变异的问题。③ 第二版脚本 baseline 门里有一个**早先被我改掉名字**的用例名，`-run` 匹配不到任何东西，"want 8 / pass 7"因此在注入任何变异之前就挡住了整个脚本：先验基线不只是"证明能绿"，也是"证明这些用例真的存在"。
- **计数**：新增用例 internal/grpc **16** 条（诊断 7、对话 9）；`cmd/levee` 接线守卫重写为 **7** 条（5 新 + 2 保留，受控集改为 proto 穷举）。变异 **17** 项全部被抓（一轮 7、二轮 9、三轮 1），三份脚本各自带 sha256 逐字节还原断言且都报 `residue: none`。
- **验证**：`go build ./...` 0、`go vet ./...` 0、`gofmt -l internal/ cmd/` 空、`go test ./... -count=1` **68 包 ok / 0 FAIL**（rc 0）、`-race` 于 `internal/grpc`（100.6s ok）与 `cmd/levee` 的策略用例（14.0s ok）、`golangci-lint run ./...` **0 issues**、`go run ./internal/docgen -check -root .` 输出 `spec is up to date`、`python -m unittest discover -s scripts` **48 tests OK**。零前端、零 dist、零 proto、零 schema 变更（`git status --porcelain -- internal/web/dist web/` 为空）。**并发写者老毛病复发一次并如实记账**：后台那次全量套件尚未结束我又跑了前台一遍，两次写同一个输出文件 ⇒ 那份文件不可用，上面所有数字取自重跑的**单次**干净运行。


## [v1.22.0] - 2026-10-16

> **发布状态：已切版。** 附注 tag `v1.22.0`（tag 对象 `b0f54d33`，两阶段剥离后指向合并提交 `5139395f`，即 PR #168 的合并）于 2026-10-16 推送；`release.yml` 三段（CI gate / goreleaser / container image，run `38839860583`）全部 success：GitHub Release `draft=false`、`publishedAt=2026-10-16T02:34:38Z`（10:34:38 +0800），资产 7 个（6 个平台包 + `checksums.txt`）；镜像 `ghcr.io/levango7/levee:v1.22.0` 按外部事实核过——`manifests/v1.22.0` 返回 **200**（同一次探测里 `v1.21.0` → 200、不存在的 tag → 404，证明这个探针有分辨力），`tags/list` = `[v1.18.0, latest, v1.19.0, v1.20.0, v1.21.0, v1.22.0]`。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.22.0` / 逐字 `v1.22.0`（顺序不能反：规则②要求它逐字出现在 tag 集合里）。 本节共 **48** 个小节，五条主线：① **交互层 P0 全部落地**——变更详情页、模板实例化三步向导、部分回滚补救（先 dry_run 摊证据）、审批中心卡片化（为此新增审批链读路径 `GET /changes/{id}/approvals`）、`/monitor` 三视图（目标地图 + SSE 事件流）、审计证据片段卡与时间线、看板第一张趋势图、列个性化、全键盘路径、空/失败/无权限三档；② **词表与判据收口**——`runs.status` 行内注释（4 个幻影拼写、owner 的 15 个里缺 10 个）、`approval_level` 两处失实、`runs.priority` 拆成独立列（schema v9）并在入口校验、`audit.action`/`result` 两对孪生拼写与三处散文、错误码目录双向漂移（撤 11 补 1）、门禁判定第一次进审计链；③ **授权与安全**——8 个写 RPC 从零准入到接权限矩阵、2 个直读 store 的 REST-only 端点接门禁、重试上限改由服务端强制、ChatOps 入向决策闭环（平台验签 + 身份映射）、移动深链重复点击返回收据、`levee tenant purge` 只回收 WORM 允许的那一半；④ **契约与交付物**——DTO 双向守卫（`wire.json` 由服务端写下、含嵌套对象键集）、`cluster_enabled` 位上 wire、节点能力申报三段管道、Helm 单节点 PostgreSQL 成为一等公民、`make web` 拒绝 CRLF 工作副本；⑤ **CI 与协作**——浏览器级检查进 CI（ubuntu + windows 双 OS）并把它假红的根因坐实为「判据挂在时钟上而不是挂在渲染上」、多 agent 协作留言板与本仓第一份 `AGENTS.md`。**切版顺序（按既有约束执行）**：先折叠 CHANGELOG 与 release notes、chart 不动（`check_release_versions.py` 规则②要求 `image.tag` **逐字**出现在 tag 集合里，tag 存在之前不能升），推 tag 并确认 `release.yml` 三段 success 之后，再随下一笔把 chart 两处升到 `1.22.0` / 逐字 `v1.22.0`。release notes 见 [`docs/release-notes/v1.22.0.md`](docs/release-notes/v1.22.0.md)。

### 依赖与 CI 工具链（三条 Go bump 合并成一笔重算，CodeQL 三条 pin 锁步）

- **为什么重算而不是手抄**：dependabot 的三条 Go bump——#147 `golang.org/x/crypto v0.57.0 → v0.58.0`、#148 `github.com/aws/aws-sdk-go-v2/config v1.33.4 → v1.33.9`、#149 `github.com/aws/aws-sdk-go-v2/service/kms v1.61.1 → v1.61.4`——都在旧基线 `af1d706` 上算出，三条彼此又与并行分支相撞；它们当时各红在 browser-check 的**不同**断言上（根因见下一节），三条全部 CLOSED 未合。收进 #165 时按工具链重算（`go get <mod>@<目标版本> && go mod tidy`），不是把冲突块抄过来；产物与三条 PR 的目标版本逐字一致，锁文件其余 14 项的差异方向全是「更新」。**这三条 PR 正文与标签里都没有公告号**（`labels=[]`，无 `GO-20xx-xxxx` / CVE 引用），所以本节只记为版本 bump，不写成安全补丁。AWS SDK 在本仓的唯一消费点是 `internal/credential`。
- **CodeQL 三条 pin 必须同版（#164）**：dependabot 把同一个 release 拆成两条单边 PR（#145 只升 `init`、#144 只升 `analyze`），而 `init` 在 job 内安装 CLI、`autobuild`/`analyze` 复用同一 runner 上已装的 CLI 与其配置版本 ⇒ 两条各自的红点都是版本对不上：#145 在 autobuild 步报 `Loaded a configuration file for version '4.38.3', but running version '4.38.2'`，#144 在 Post analyze 报反向那条。合成为一笔把 `init`/`autobuild`/`analyze` 一起钉到 `24c54180a607b1449ed407dd24f251e4e9147c8d`（v4.38.3）；tag→SHA 从上游现取且做两阶段剥离（annotated tag 的 ref 给的是 tag 对象 `cee97f8…`，再问 `git/tags/` 才拿到提交——只看第一层会以为对不上）。
- **CI 动作 pin 同批**：`actions/checkout 4.4.0 → 7.0.1`、`actions/setup-python 5.6.0 → 7.0.0`、`actions/upload-artifact 7.0.1 → 7.0.2`。**未被一起升的一处，如实登记**：`github/codeql-action/upload-sarif` 在 ci.yml 里仍钉 v4.38.2——它不在 codeql.yml 那次 CLI 安装的同 job 闭包内（ci.yml 只用这一步传 sarif），因此不受上面那条锁步约束；写下是为了免得下一次把它当成漏项「顺手对齐」。
- **切版基线的扫描面（取 CI 检查结论，非本机复算）**：`go 1.26.9` 未变；`govulncheck`、`trivy`、`gosec`、`lint` 四条腿在 `91a63ef` 上均 success。

### 文档（一处歧义订正 + 双语机制的定档）

- **「多语言」这个词在账本里指错了东西**：`internal/docgen` 那行登记的原话是"多语言实现（把 `web/src/types/levee.ts` 那张表也生成为 TS）"，它的"语言"指的是**生成目标**，不是自然语言。实测本仓没有任何 i18n 设施：`web/src` 的 23 个 `.vue` / 62 个 `.ts` 里有 19 个文件写死中文文案，`grep` 的 `i18n/locale/intl/LanguagePack` 只命中 `main.ts` 与 `targetMap.ts` 两处无关词。措辞改成"多目标生成"，免得下一次按"国际化"排期——同一类过期/歧义账本今年已经误导过一次排期（A2 就是照过期账本排出来的幻影项）。
- **自然语言双语定档：只做 zh/en，且现在不做**，依据是产品定位（国内使用为主，英文不构成发布门槛）。同时把"要做必须先满足"的四条保守约束写进账本，防止将来接的时候走成第二份手抄表：① 词条只有一个源（Go 侧一份 catalog，TS 与文档由 docgen 生成）；② **缺译文判红而不是回退**——运行时把中英混排当降级，形状与 #153 的"读不到键就用默认值"完全相同，页面会说谎；③ 语言取**显式状态**（`?lang=` + 本地持久化，默认 `zh`），不做浏览器语言自动探测——`browser-check` 的分支断言按中文文案字面区分（"节点注册表为空" vs "未启用集群协调"），自动探测会当场摧毁这套可复现性；④ 一次只翻一个自带"能红"门禁的竖切片，不做运行期热切换、不预先搭 i18n 抽象层。启动条件写明：出现真实多语言客户诉求；在那之前这笔投入只有审美分母、没有可测分母。
- 生成目标那一行的判据补全：**词表唯一源必须在 Go 侧**，TS 由 docgen 产出。依据是同一缺陷类今年两次实付——#153 的 `DTO_READS` 手抄表让契约守卫比的是自己的抄写（集群页三个读数恒为默认值、全程无报错），以及本轮合并时 mock 的 `cluster-empty` 分支又冒出一处 camelCase `totalActive`。

### CI（浏览器检查假红的根因坐实：判据挂在时钟上，不是挂在渲染上）

- **背景**：#136 把这套检查请进 CI 时如实记了一句「**根因没能坐实**……没有声称预热是修复——证据不足」。三天后它开始付账：三条互不相关的 Go 依赖 bump（#147/#148/#149）在同一段窗口里各红在**不同**断言上——`normal mode shows rows — rows=0`、`403 renders the forbidden tier — title=""`、`Enter opens the cursor row`——且只在 `windows-latest` 红，同一提交的 ubuntu 腿全绿，页面自己 `errors=[]`。后果是安全依赖合不进去，而"红"长得像产品回归。
- **根因（实测得出，不是推断）**：`open()` 用 `await sleep(settleMs)`（900/1200/1500/1600/2200ms 各点不一）作为"导航已完成"的唯一同步手段，11 个调用点后挂着全部断言。把这些 sleep 归零、冷缓存重跑，本机**精确复现** CI 的两条失败签名（`rows=0`、`title=""`）。断言读的是"等了多久"，不是"页面到了什么状态"。
- **落地**：`open()` 改为等内容——每个页面在 `evaluateOnNewDocument` 里装一个只数 `/api` 的 XHR/fetch 在途计数（SSE 走 EventSource 因而不计数，理由与既有的"永不等 networkidle2"同源），外加一个 `MutationObserver` 记录最后一次 DOM 变更；就绪判据＝路由标题已挂载 **且** 在途请求归零 **且** 两者都静默 150ms。加观察器是因为请求计数不够：`loadend` 早于 axios 的 promise 落地、更早于 Vue 下一帧重渲染，只数请求仍会读到未画完的 DOM。`SETTLE_DEADLINE_MS`（默认 30s，`BROWSER_CHECK_SETTLE_MS` 可覆盖）是**上限而非等待时长**，超时不改变判据、只打一行 `NOTE settle timeout` 后照常断言。键盘与虚拟滚动两处固定 sleep 各换成对可观测状态的等待，并刻意让等待信号弱于断言：光标「先等到存在、再断言恰好一个」（否则"出现两个光标"会被等待吞掉），滚动「先等到首行主机真的变了、再断言底部留白归零」（避免同义反复）。
- **断言没有被等待洗白（两条变异）**：① `BROWSER_CHECK_SETTLE_MS=1` → 退出 1，7 条 FAIL + 15 条 `NOTE settle timeout`——等待确实承重，且可疑处点名而不是静默读过；② 把两个期望值改错（`offline === 99`、`segs > 99999`）→ 各自判红并如实回显真实读数 `offline=1`、`segments=24`。
- 验证：38 条断言（含循环内展开）在**冷缓存**（`rm -rf node_modules/.vite` + `vite --force`）下 ALL PASS，热态连跑两轮各 `ok=38 fail=0 note=0`。条件：本机 Chrome + node v26.8.2（CI 钉 node 20）＋自起隔离实例 mock:4699 / vite:5183，**未复用**主工作树上 10-13 起的 :5173/:4599。`go vet ./...`、`internal/docgen -check`、`scripts/test_changelog_structure.py`、`test_check_release_versions.py`、`test_ci_aggregate.py`、`test_release_workflow_budget.py` 全过（无 Go 改动，pb 未触碰）。
- **仍未做**：runner 速度差是这份检查的固有条件，本次改的是判据不是预算。将来若有断言真的依赖"在多久之内"发生（例如 SSE 首帧），它该有一个按属性写的显式 deadline，而不是回到固定 sleep。

### 修复（集群页三个读数在真机上恒为默认值：DTO 拼写与 wire 不一致，而契约守卫核对的是手抄表）

- **现象**：`/system/cluster-status` 的手写镜像 DTO 把四个键拼成 camelCase——`lastHeartbeat`/`joinedAt`/`nodeLoad`/`totalActive`。该端点由 gateway 用 `encoding/json` 直接编 `state.ClusterNode` / `state.AssignmentSummary`，wire 上是 struct tag 的 snake_case。真机实测（af1d706 自编二进制 + `serve --insecure --engine-enabled`，curl `:9098/api/v1/system/cluster-status`）：`{"backend":"sqlite","nodes":null,"summary":{"counts":{},"node_load":{},"total_active":0}}`。读不存在的键得 `undefined`，而三处消费点各有一个默认值，于是集群页把「活跃分配」渲染成 0、「Worker 负载」恒为"无活跃分配"、节点「心跳」恒为 `—`——**没有一处报错**。
- **为什么现有的门没响**：四个产物互相自洽、一起与服务器不一致。`web/scripts/mock-gateway.mjs` 发的正是 camelCase（还把 `lastHeartbeat` 发成秒级 epoch 数字，而 Go 的 `time.Time` 落 wire 是 RFC3339 串，且完全没有 `node_load`），`cluster.spec.ts` 的 fixture 照抄 mock，浏览器检查又跑在这个 mock 上——两侧测试都绿。该响的是 `wireContract.spec.ts`，但它的 `DTO_READS` 是**手抄表**：表里写 `last_heartbeat`、接口里声明 `lastHeartbeat`，守卫拿手抄去比 wire，核对的不是代码。同文件那句「a table naming a DTO nobody declared is a lie the test would still pass」的落地形式是 `expect(apiSource).toContain('export interface')`——恒真。
- **落地**：① DTO / 视图 / spec fixture / mock 四处改成 wire 的真实拼写（mock 同时改发 RFC3339 并补上 `node_load`）；② Go 快照新增 `objects` 字段，记录**嵌套对象**的键集（`summary: [counts, node_load, total_active]`）——此前只记顶层键与数组行，嵌套一层是契约盲区，而这次漂移正发生在那一层；③ TS 守卫改为**从接口声明本身提取字段**再与 wire 比对（`interfaceFields()` 做括号配平读取；提取到 0 个字段即判红，不让它空跑通过），覆盖 batch-status / change-trend / cluster-status 三个手写端点及其行类型与嵌套类型。
- **验证（变异都打在缺陷类上，不是打在样式上）**：把 `ClusterNodeDTO` 改回 camel → 新守卫点名 `ClusterNodeDTO declares lastHeartbeat but the wire does not send it`，而**旧的 4 条检查带着这个 bug 全绿**；只把 `AssignmentSummaryDTO.total_active` 改回 camel → 同样判红，证明嵌套层现在有料；把视图读点改成 `n.lastHeartbeat` → `vue-tsc` 报 `Property 'lastHeartbeat' does not exist … Did you mean 'last_heartbeat'?`，故视图侧由类型门兜住，本轮不再加浏览器断言（也避免与 #152 在同一脚本上相撞）。全量：`vitest 36 文件 / 303 测`、`vue-tsc --noEmit` + `vite build`、`go test ./internal/grpc -run TestWireSnapshot` 两条、重建后 `diff -r web/dist internal/web/dist` 无差异（48 个文件）。
- **同类先例与边界**：`StepRowDTO.batch_no` 的注释写着"this field was camelCase for a while and read back as undefined"——同一缺陷类第二次发生，第一次只改了那个字段、没补判据。proto 派生的端点（changes / audit-log）**不在**派生检查里：protojson 省略零值字段，「声明 ⊆ 已发」会误报，且它们的 DTO 住在 `types/levee.ts` 而非 `api/index.ts`；takeover-events 的客户端类型是内联匿名类型，提取器按具名接口工作，暂未纳入（实测其五个键与 wire 一致）。

### 新增（`levee tenant purge`：租户数据的回收，只回收证据链允许的那一半）

- **背景**：`tenant delete` 一直是软删除——状态置 `deleted`、释放配额、记录留着让历史 run 仍有身份。roadmap 上挂了很久的"级联清理"写着"无从实现"，理由是 `TenantManager` 没有反查接口。真去试的时候发现的却是另一件事：**"清空一个租户的数据"在这套 schema 上是做不到的，而且失败方式会把缺陷伪装成 bug**。
- **实测前提（不是推断）**：`trace`/`audit` 有 `BEFORE DELETE` 的 WORM 触发器 `RAISE(ABORT)`，而 `trace`/`approvals`/`batches`/`steps` 都带 `ON DELETE CASCADE FROM runs`，驱动又确实开着 `PRAGMA foreign_keys`（`sqlite.go:84`/`:110`）。于是"删掉一个有 trace 的 run"得到的不是删证据，而是**整条语句回滚、一行都没删**——操作者看到的是 purge 失灵，实际是保护机制生效。`run_assignment` 反过来：`run_id` 是它的主键而不是外键，没有任何级联边，不显式删就会比它指向的 run 活得久。
- **落地（`state.PurgeTenantData` + `levee tenant purge <tenant-id>`）**：删**终态且从未产生 trace** 的 run（连带级联的 batches/steps/approvals）与它们的 `run_assignment` 行；**有 trace 的 run、非终态的 run 一律保留并计入报告**。一个事务做完——半途失败的租户比没清理的更糟。
- **三面门禁**：① 注册表状态必须是 `deleted`，否则命令失败且一行不动（`delete` 才是"这些数据不再属于任何人"的意图记录点）；② 空 tenant id 直接拒绝——`tenant.enabled: false` 时全库行的 `tenant_id` 都是空串，按空串清理等于把整个单租户库删掉；③ 不可逆动词在 `--json` 下没有终端可问，必须显式 `--yes`。
- **输出面把保留项说出来**：`purged` 之后紧跟"删了 N 行 / 因 WORM 保留 M 行 / 非终态保留 K 行"，且一行都没删时明确打印 `Nothing was removable`。**判据：一个只显示成功字样的清理命令，会让运维以为数据已经没了**——真实情况多半恰恰相反。
- **判据两条（可复用）**：① **purge 做成独立动词而不是 `delete --force`**——`delete` 的既有语义是"可预期、不动数据"，把不可逆删除挂成它的旗标，就等于让一条命令的破坏半径取决于一个没人读的开关；且分开的动词才能表达"三个月前删的租户今天清理"。② **前提也要有测试**：`TestWormTriggerIsWhatForcesTheTraceGuard`（直删有 trace 的 run → 被触发器拒绝并验证整条回滚）、`TestPurgeGuardIsLoadBearing`（去掉 `NOT EXISTS` 的同一谓词当场报错）、`TestAssignmentTableHasNoCascadeEdgeFromRuns`（`pragma_foreign_key_list` 计数为 0，且删 run 后 assignment 行仍在）。这三条把"注释里的理由"变成 CI 会因由红的事实——将来谁动了触发器或级联边，`NOT EXISTS` 这道 guard 是保命还是装饰会立刻见分晓。
- **词汇表不抄第二份**：终态集合由 `runstatus.Terminal` 在 Go 侧展开成绑定参数，SQL 里只有占位符；方言差异由本文件的 `bind()` 处理，它只把 `?` 改写成 `$n`，值仍走参数绑定（gosec 的 G202 豁免注释写在拼装行上并说明理由）。装饰过的 store（`tenant.TenantStore`）由函数**内部** `state.Underlying` 解出，调用方没有忘记的机会——这是 #47 那条教训的形状。
- **登记不修（要用户点头）**：`credentials`/`targets`/`inventory_groups` 不在授权范围内，本命令一律不碰；"连证据一起清"（purge trace/audit）是另一个产品决定，它要把 WORM 从默认承诺改成可选豁免，而 `verifyStoredPlanHash` 与审计链的取信前提会随之改变；原文提到的"`Delete` 没有必须先 suspend 的前置检查"仍未加，那是新增行为。
- **清理动作自己也留痕**：purge 成功后写一条 `audit.action = tenant_purge`（actor = 本地 actor、target = 租户 ID），走 `audit.Record` 即随写封链。**判据：没有这条行，"这批 run 三月被清掉了"和"这个租户从来没有过 run"在库里是同一个形状**——正是本仓四个 P0 一直在堵的洞。写失败只 WARN 不回滚：删除已经提交，没有回滚可给。计数不落审计行（`audit` 无 detail 列，链哈希覆盖固定字段集）。
- **删除能达到的边界由外键决定，这条边界要写明**：`audit.run_id` 没有外键（只有 trace/approvals/batches/steps 有 `ON DELETE CASCADE FROM runs`），所以被删 run 的审计行留在库里——purge 因此可行，且"变更存在过"这件事永不被回收。`TestPurgeTenantDataRemovesOnlyWhatTheChainPermits` 同时钉住这两侧（run 消失、它的 audit 行还在）。新增动作令牌 `tenant_purge` 过 `TestAuditActionCommentMatchesConstants` 的双向核对，故 SQLite 与 PostgreSQL 两份 DDL 注释同批改。
- **验证**：`internal/state` 10 例（可删子集/空 id 拒绝/租户隔离/装饰 store/无 handle/未知租户 + 上面三条前提 + **一条活库 PostgreSQL 用例**）；`cmd/levee` 5 例（命令已注册且裸参数不动作、活租户被拒且行不动、`--json` 无 `--yes` 被拒、端到端删除+保留+人类输出三条计数+审计留痕、无事可做时明说）；CLI 走 `openStoreFromConfig`，因此 purge 打到 config.yaml 指定的库而不是本机默认文件。
- **`bind()` 的方言分支必须有活库证据**：把 `?` 改写成 `$n` 这件事在 SQLite 上永远看不见错——本机起一个临时 `postgres:16-alpine`（`levee-a5a-pg`，测后已 `docker rm -f`）跑通整条 purge 并复现"WORM 拒绝删有 trace 的 run"（SQLSTATE P0001），然后把 `bind()` 改成"postgres 也返回原串"——该用例当场红（`ERROR: syntax error at end of input`）。CI 的 postgres job 已含 `go test -race ./internal/state/...`（`.github/workflows/ci.yml:258`），因此这条活库用例不是"本机跑过一次"而是会复跑、会因漂移而红。
- **`#nosec` 不是装饰**：初版在两条拼接语句上写了 G202 豁免注释，实测该版本 gosec 对这两行**零命中**——豁免注释压住一个不存在的问题，代价是将来真出现同码问题时它会被一起静默掉。注释已删，`bind()` 自身的安全性说明留在函数注释里（它只改占位符、值仍全部绑定）。
- **变异 11 项，全部由断言抓红、无一项以编译失败冒充**：去掉 `NOT EXISTS`（被 WORM 判错）、把非终态当可删、跳过 assignment 删除、**把 assignment 删除改序到 run 之后**（证明"先删 assignment"是承重的而非修辞）、接受空 id、不解包装饰器、抹掉 held-trace 计数、CLI 三条门禁与审计写入各一（活租户门 / `--yes` 门 / `audit.Record` 调用）、以及 `bind()` 的方言分支。

### 修复（Agent 摘除的第二扇门没有守卫：两条门共用同一个判定，覆盖权只留一处）

- **背景**：#85 给 `RemoveAgent`（运维侧）接了"记录仍报在途任务就拒绝、`force` 显式覆盖"。`DeregisterAgent`（Agent 自己的告别）当时**没有守卫**：拿到 id 直接 `DeleteAgent`。两个调用效果相同、判定不同——而它是注册在 proto 服务面上、admin 权限门后的公开 RPC，任何 admin 客户端都能选那扇没守卫的门。
- **实测**（`af1d706`）：`grep -rn "DeregisterAgent" --include=*_test.go` 全仓只有**一条授权拒绝用例**，功能面零覆盖——即"这条门会不会删掉在途记录"从来没有被测过，所以缺陷不是"测出来没修"，而是"没人看过"。
- **落地**：`DeregisterAgent` 先 `GetAgent`，`ActiveTasks > 0` 时回 `FailedPrecondition` 并在文案里点名改用 `RemoveAgent + force`；记录不存在时保持 `removed=false` 的既有可读拒绝语义（不是服务端故障）。**没有给 `DeregisterAgent` 加 force**：`DeregisterAgentRequest` 里本来就没有这个字段，加它等于让被摘除的对象给自己发豁免——覆盖权刻意只留在运维那侧。
- **判据（值得复用）**：**同一个破坏效果有几扇门，判定就必须有几处断言**。除各自的行为用例外，加了一条 `TestBothRemovalDoorsShareTheInFlightJudgement`——同一条记录、同一个状态，先问运维门（拒绝）、再问告别门（拒绝）、确认库里记录还在，最后证明 `force` 只在一条门上生效。只测单侧，"日后把某一侧阈值改掉"依然全绿。
- **两条登记项经实测否定**：① 「守卫应改读 pending/unclaimed assignments 而非心跳报的 `ActiveTasks`」是**身份空间混淆**——`run_assignment.owner_node` 写的是 `cluster.Node.ID`（`internal/dispatch/dispatch.go:344 workerLoads` 取自 `mgr.ActiveMastersAndWorkers()`），与 `agents` 表的 agent id 不是同一套标识，跨表核对没有共同的键。② 「本地模式 `--force` 惰性」不修——进程内 `AgentRegistry` 只在同一进程里被填充，`levee agent remove` 作为独立进程调用时对任何在册 agent 都返回 not found，在那里加在途拒绝等于加一条不可达分支（本仓惯例：测不出来的守卫是假守卫）。两条都写进了 CLI 参考 20.5，避免下次又被当成待办。
- **顺带查出、登记不修**：`agent.MasterClient.Deregister` 的接口注释承诺"master 应把该 Agent 从注册表移除**并重新分配在途任务**"，而唯一实现 `InProcessMasterClient.Deregister` 只做 `registry.Deregister`，没有任何重分配（进程内注册表也不持有 assignment 存储）。这与 #85 记录的"`agent start` 仍为进程内、proto 无任务下发通道"是同一条边界——要让那句注释成立，先要有任务下发通道，不是补一句代码的事。注释保留原样（它写的是目标语义），因为它描述的字段与实现差距已在 roadmap 与本节登记，不能再被当成"已经做了"。
- **验证**：`go build` / `go vet` / `gofmt` 干净，`go test ./...` 全绿，docgen up to date，`internal/grpc/pb/` 未改（无 proto 变更）。新增 4 例。变异三项、均为断言失败而非编译失败：抽掉 `DeregisterAgent` 的在途判定 → **2 条红**（`…RefusesInFlight`（含"记录仍在"那条断言）与 `TestBothRemovalDoorsShareTheInFlightJudgement`）；阈值从 `> 0` 改成 `> 1` → **1 条红**（双门同判，心跳报 1 的情形——单侧阈值漂移正是那条结构测试存在的理由）；把"缺记录"从 `removed=false` 改成服务端错误 → **1 条红**（`…AbsentIsNotAFault`）。

### 新增（Helm 单节点 PostgreSQL 成为一等公民：DSN 走 Secret→env，凭据不必再进 ConfigMap）

- **背景**：#47 让"单节点跑 PostgreSQL"成为合法形态——CLI 与 serve 都经 `database.driver` + `database.dsn` 取同一个库，`serve` 的报错原文甚至就写着"单节点请改用 database.driver + database.dsn"。但 chart 里所有 PostgreSQL 管道整块挂在 `mode == cluster` 上，单节点用户只剩一条路：把 DSN 明文写进 `config.yaml`，也就是写进 ConfigMap（`kubectl get cm -o yaml` 谁都能读）。
- **根因在配置侧，不在模板里**：`database.dsn` 是 `DatabaseConfig` 里唯一没登记进 `LEVEE_*` 环境绑定表（`internal/config: allKeys`）的叶子。`bindEnv` 只对显式绑定过的键生效（viper 的 `Unmarshal` 走 `AllSettings`），所以 `LEVEE_DATABASE_DSN` **静默不可达**——不是"不推荐这么用"，是读不到。`ai.llm.api_key` 这类凭据早已在表里，可见这不是"凭据不走 env"的策略，是漏了一项。
- **落地四面**：① 把 `database.dsn` 登记进绑定表；② PG 管道改为按"操作者有没有要求 PostgreSQL"启用（新 helper `levee.wantsPostgres`：`enabled` / `dsn` / `externalSecretName` 任一），不再看 `mode`；单节点注入 `LEVEE_DATABASE_DSN`，cluster 模式**原样保留** `--pg-dsn`——若两边都注入，绑定的 env 会悄悄覆盖操作者写在 `values.config` 里的 `database.dsn`；内嵌演示实例同步解禁到单节点；③ `config.yaml` 只写驱动不写凭据，没显式写驱动时按 `wantsPostgres` 补 `driver: postgres`，**显式写了就不改**；④ CI 从"能渲染"升级到"渲染内容对不对"。
- **ConfigMap 与 rollout 哈希同取一次计算**（`levee.effectiveConfig`）：`checksum/config` 原来按原始 `values.config` 求哈希，于是"Chart 补写了 database.driver"不改变哈希、已运行的 Pod 不会因此滚动重启——同一份配置在两个地方说法不同。
- **判据（值得复用）**：
  - ** Chart 不替操作者改写已经写明的取值**。`driver: sqlite` 又启用 PostgreSQL 是一份自相矛盾，Chart 保持 sqlite，由 `config.Validate` 大声拒绝启动（`database.dsn is only used when database.driver = postgres`）；Chart 侧不再抄一份校验规则，免得两处判定漂移。
  - **" Secret 的名字出现在渲染里"不等于"Secret 被渲染出来**：Deployment 的 env 引用同名对象时字符串照样出现，所以断言改成"在 `kind: Secret` 文档内找这个名字"（`require_secret`）。
  - **出现次数而非存在性**：cluster 的 `--pg-dsn` 由两份模板分别传入（master Deployment 与 worker StatefulSet）。"字符串存在"的断言在变异删掉 master 那份后仍然绿——改成 `require_count … 2` 才抓得住。
  - **`externalSecretName` 与 `postgres.dsn` 同时给出时外部完全优先**：既不渲染同名 Secret（覆盖现场拥有的对象比少一个对象更糟），也不把 values 侧 DSN 写到任何地方。
- **顺带纠正一条记账不足**：NOTES.txt 里的后端说明无法在 CI 渲染（实测 helm 3.14 的 `helm template` 无 `--notes`，`helm install --dry-run=client` 仍需集群连接）。因此"这个 Pod 连的是哪种库"改为同时写成 Deployment 注解 `levee.qoder/database-backend: sqlite|postgres`——`kubectl get po -o yaml` 一眼可见，且被 CI 断言覆盖。
- **验证**：`internal/config` 新增 6 例（reflect 遍历字段要求逐个绑定 + "至少扫到 4 项"的下限、env 覆盖文件、纯 env 选驱动、无 DSN 拒载、sqlite+DSN 拒绝、以及 Chart 不需重复校验的依据）；`go build` / `vet` / `gofmt` / `go test ./...` 全绿、`docgen -check` up to date、`internal/grpc/pb/` 未改、钉版 gosec 0 命中。`bash scripts/validate_delivery.sh` 新增 5 个渲染 profile（`single-postgres` / `-embedded` / `-contradiction` / `-external-secret` / `-external-conflict`）与四种断言原语；**变异九项逐项命中预期断言**：单节点丢 DSN env、Secret 退回 cluster-only、ConfigMap 不再补 driver、Chart 改写显式驱动、DSN 泄进 ConfigMap、cluster 丢 `--pg-dsn`、内嵌实例不在单节点渲染、DSN 管道漏进默认渲染、外部 Secret 被同名覆盖。

### 文档（roadmap 账本四行过期已订正——防止下一次照它排期）

- **动机**：本会话已经为过期账本付过一次代价（按 7 天前的基线排出的 A2 整项是幻影，`SystemView.vue` 的六个字段自 Phase 2 就在全渲染）。这四行同族：**代码已落地，账本仍写着待办**。
- **逐行按当前 HEAD 复测后订正，并把取证位置写进账本**：
  1. `idempotent: true` 的存量 Lint → 已落地为 **LE103**（`internal/dsl/compile_warnings.go:195-216`，warning 而非 error）；与 #79 那条口径并排写清：**前瞻步上的 `idempotent` 是 advisory，undo step 上的才是治理字段**。
  2. 集群拓扑 + 接管事件时间线 → 已落地（`0635e76`，PR #139）：接管结算此前**零读者**，现有 gRPC 读路径与 `ClusterView.vue:42-54` 的时间线（loading/empty/error 三档齐）。遗留的 `cluster_enabled` 位单独留 A3b，别用 `backend` 当代理指标。
  3. 深色模式、密度档位、可配置列 → **三项里两项已落地**（`useTheme.ts` 的 `light/dark/system` 挂 `<html>.dark`；`tablePrefs.ts` 的列显隐 + 筛选记忆），**只剩密度档位**，并给出判空命令（`web/src` 内 `density` 与「紧凑」零命中）。
  4. 移动审批卡深链证据 → 已落地（PR #141），实际形态比原描述完整：`409 + {decided, decision, run_id, decided_by, decided_at}` 走"收据"而非错误；前端以 `decided === true` 为准绳（不走 401 分支，那会清 token 跳登录）；未签发过的 token 仍只得扁平 401——披露面不扩大由 `assert.Len(out, 1)` 钉住。
- **判据**：账本里"待办"这类进度型条目**不是证据**，排期前逐项在当前 HEAD 复测；订正时把**取证位置（file:line 或 commit）**一起写进去，否则下一次还得重查一遍。契约型数字（如 schema 版本）可写死，进度型数字必须带"截至某 commit + 现测命令"。

### 修复（集群页把「用哪个库」当成「有没有集群」：单节点 PostgreSQL 被叫去启动 worker）

- **问题**：`GET /system/cluster-status` 只报 `backend`，`ClusterView.vue` 就用它回答了两个问题——`backend === 'sqlite'` 说「单节点模式，集群协调未启用」，`backend === 'postgres' && nodes.length === 0` 用**同一句话**覆盖两种部署：「未注册任何 worker 节点。集群模式下请启动 worker 加入；单节点 PostgreSQL 部署无需 worker」。而这两种部署该做的动作正好相反：一个是「什么都不用做」，另一个是「协调开着但注册表空了——这是事故」。页面拿不到区分所需的量，就只能把事故说成常态。
- **为什么不能从 `backend` 推**：单节点 PostgreSQL（`database.driver: postgres`，无成员/无租约/无 fencing）自 #158 起是受支持形态，`backend` 只回答「哪个库在答」。反过来说 SQLite 确实永远不可能是集群，但那是 `openServeStore` 的构造结果（`--cluster` 必须有 `--pg-dsn`），不是一个字符串标签的语义。
- **落地**：`ServeGatewayConfig.ClusterEnabled` → payload `cluster_enabled`。取值放在 `newServeGateway(..., clusterMgr)` 里按 `clusterMgr != nil` 计算，**不**写在 `runServe` 的字面量里——把「协调开没开」留成一个可以被忘记赋值的字段，正是本仓反复踩的「门在结构体里≠门在进程里」。
- **两面测试**：`internal/grpc/rest_cluster_status_test.go` 用四例在**同一个 `backend`、同一份 registry 数据**下让 `cluster_enabled` 取到两个值（任何从 `backend` 或从 `len(nodes)` 推导的写法都会红）+ 一例钉住 payload 键集；`cmd/levee/serve_gateway_wiring_test.go` 三例咬住赋值来源（含「调用方自己写了 true 但 manager 为 nil → 必须被覆盖」）。
- **接线也要被测到，且写清为什么只能这样测**：`runServe` 是长启动序列，单测驱动不了，所以第二个文件对 `cmd_serve.go` 做定点读取——要求 `newServeGateway` 的最后一个实参就是那个喂给 takeover/dispatch 两个循环的 `clusterMgr` 标识符，并要求它来自 `openServeStore`。这是文本断言而不是行为断言，**原因与被它放过什么**都写在文件头注释里，不留「我们有了接线测试」的错觉。
- **前端**：DTO `ClusterStatus.cluster_enabled`（照 wire 的 snake_case 拼写——#153 的教训：客户端自创 camelCase 只会读到 undefined，而 `undefined === true` 是 false，于是每个真集群都会被永久报成「协调未启用」）；两段提示各说各的，且都要求 `answered`（`data !== null`），首帧与请求失败时不再凭空断言部署形态（旧实现在 `data` 为空时把 `backend` 默认成 `unknown` 并落进仪表盘，渲染出一屏读起来像实数据的零）。`ClusterView.spec.ts` 新增 4 例，其中「两句不许互相串」在**剥掉注释后的模板区间**上断言——本文件自己的注释引用了被退役的 `backend === 'sqlite'`，不剥就会验在散文上。
- **浏览器级检查补上第三种部署**：`mock-gateway.mjs` 原来只有 ok/empty/forbidden/cluster 四个模式，`cluster_enabled` 加进 fixture 后新增 `cluster-empty` 模式（coordination 开 + 注册表空），`browser-check.mjs` 加 3 例：这种部署要说「注册表为空」、不许借用协调未启用那句、不渲染节点卡。默认模式的断言同时收紧——旧断言是 `单节点 || 未启用`，而新文案里两种部署都含「单节点」，宽松词表会让错误分支蒙过去。
- **兼容性**：只加键不改键、不删键，`backend` 原样保留；控制台与二进制同仓编译（`internal/web/dist` 由 `make web` 刷新、CI 逐字节验新鲜），不存在新版页面对旧版服务的读取分歧；schema 无变化，无迁移。
- 验证：`go test ./...` EXIT=0；`npm run test` 36 files / 306 tests 全绿；`vue-tsc --noEmit && vite build` 通过；`internal/web/dist` 按 `make web` 的删除+拷贝两步刷新（43 条路径变化：21 个改名的 view chunk + 21 个新增 + index.html）；26 个同名 chunk 与刷新前**逐字节相同**（含 `vendor-C7FidRup.js`），证明本机 Windows 构建与 CI 的 ubuntu 构建口径一致——master 的 CI 前端新鲜度门是绿的，故等价链条成立。`npm run test:browser` 本机全通过（自建 mock :4611 + vite :5188；默认端口上跑的是别人 checkout 里的 dev server，第一次的三条红是它的旧代码，不是这里的）。变异 6 个逐一咬红：payload 写死 false、从 `backend` 推、从 `len(nodes)` 推、`newServeGateway` 信任字面量、调用点传 `nil`、前端 `clusterEnabled` 改读 `backend`。

### 新增（节点能力申报接通：`--node-capabilities` 报得上去、重连会刷新、也查得动——但派发刻意不看它）

- **背景**：`cluster_nodes.capabilities` 是 schema 收敛时保留下来的一列，账本登记为"写侧与读侧都没用到，要么用起来要么退役"。**现场复核把它拆成三件不同的事**：列不是没读者（agents 侧 `ListAgents` 的 `wantCap` 就是服务端按能力过滤的现役先例，且 agents 的 upsert 一直在刷新 capabilities），缺的是集群侧的**生产者、刷新与查询面**。语义有先例、管道缺三段，那就补管道而不是拆列——按"接线而非退役"落地。
- **落地（五段）**：
  1. **`internal/capability`（新，只依赖标准库的叶子包）**成为能力编码的唯一所有者：`Normalize` 去重+排序+JSON 并拒空名，`Names` 只接受 JSON 字符串数组（`{}`、`shell,pkg`、`[1,2]` 一律报错而不是宽容解析），`Has` 在读到不可解析的行时返回 false（宁可筛不到，不猜）。叶子是为了让 `cluster`、`grpc`、`cmd/levee` 都能用而不互相形成 import 环——同 `dbschema` 的先例。
  2. **`--node-capabilities`（StringSlice）只在 `--cluster` 下有意义**：单独给出即启动失败并点名原因，绝不静默收下。单机 SQLite 进程没有 `cluster_nodes` 表可写，"收下但无处落"正是这一列长期空转的成因，不能再复制一次。
  3. **心跳 upsert 的 `ON CONFLICT DO UPDATE` 补 `capabilities = EXCLUDED.capabilities`**：此前是一次性写，节点改了档位库里永远停在第一次申报的值。写入前经 `Normalize`，非规范形态直接拒绝整个 Join 而非落一条读者解析不了的行。
  4. **`GET /system/cluster-status?capability=<名>` 在服务端收窄 `nodes`**，控制台集群页加筛选输入框，并把"注册表为空"与"没有节点声明这个能力"分成两段话说（后者明写"这不是注册表为空"），`/cluster?capability=<名>` 深链可用。
  5. **Helm 侧 `cluster.nodeCapabilities`（默认 `[]`）**同时注入 master 与 worker 两个 Deployment——只挂 master 是个陷阱：承接派发运行的是 worker Pod，而整份渲染里 `--node-capabilities` 照样出现，看起来像配好了。留空时两处 args 与加这个值之前逐字一致。
- **边界四处同写，因为这正是一个字段变成装饰品的路径**：派发**不**按能力路由——`internal/dispatch` 仍按负载 / `--cluster-dispatch-capacity` / epoch 选节点。理由（按能力路由会把不具备该能力的节点饿死在队列里，而"声明了什么"今天没有权威来源可校验）写进了旗标说明、`docs/deployment.md` §12、`docs/levee-api.md` 与控制台"分配统计"标题。这一列的作用域是**人与工具筛节点**，不是调度器的输入。
- **顺带修一处形状漂移**：DDL 默认值 `'{}'` 与写侧实际发送的 `'[]'` 长期不一致（一个是对象、一个是数组，而 `Names` 对 `{}` 必须报错）——即"从未申报的旧行"在读侧是解析失败而不是空集合。默认值改成 `'[]'`，并加 `TestCapabilitiesColumnDefaultMatchesWhatTheWriterSends` 钉住"默认值必须等于写侧的空值"（`internal/dbschema` 仍是唯一定义点）。
- **测试**：capability 5 组（含"同集合不同顺序归一后字节相同"、幂等、`{}` 必须报错、`Has` 遇不可读行不匹配）；活库 4 例（Join 落 `["pkg","shell"]`、重连刷新成新集合、不申报落 `[]`、`shell,pkg` 这类非 JSON 被拒且不留行）；`cmd/levee` 接线 3 组（旗标归一进 `Node`、不给 `--cluster` 即拒绝**且不创建数据文件**、两个活库用例实证 SELECT 回来的值）；REST 2 组 5 例（不带参数与空参数给出同一批节点——空参数不等于"筛选空名字"、命中/多命中/零命中、`summary`/`backend` 不随过滤变、带参数不绕过鉴权仍 403；默认响应的形状本就由 `wire_snapshot_test.go` 的 cluster-status 快照钉住，本批未动它）；前端 `cluster.spec.ts` +1（query 参数真的发出）、`ClusterView.spec.ts` +4 源码锚点例、mock 网关与 browser-check 各加能力筛选路径。**CI 新增隔离步骤** `go test -race ./cmd/levee/ -run 'TestServeClusterJoin'`：这两个用例没有 `LEVEE_PG_TEST_DSN` 会 skip，不进步骤就等于"活库实证"会静默腐烂。
- **变异 7 项逐一咬住**：删掉 REST 过滤 → 过滤用例红（`filters to…` 与 `nothing matches…` 两条）；撤掉 upsert 的 `SET capabilities` → 活库重连刷新用例红；把筛选改回前端本地做 → 源码锚点例红；去掉空注册表分支的 `!appliedCapability` 守卫 → "没声明 ≠ 空"分支红；DDL 默认值改回 `'{}'` → 默认值守卫红；`Normalize` 里 `sort.Strings` 变成排空切片 → "同集合不同顺序字节相同"红；摘掉 worker 模板的 `{{- with }}` 块 → `validate_delivery.sh` 按 Deployment 逐份断言时报 `cluster-worker.yaml rendered no --node-capabilities`（整份文件 grep 是抓不到这条的，master 那段还在）。
- 验证：`go build ./...`、`go vet ./...`、`gofmt` 干净，`go test ./...` EXIT=0，`golangci-lint` 0 issues（本机 2.14.0，CI 钉 v2.13——版本差以 CI 为准）；gosec（pinned，`-quiet -exclude=G104,G115,G304,G307,G501,G505,G601`）EXIT=0；`go run ./internal/docgen -check -root .` 报 spec is up to date；活库用例在容器 `levee-a5c-pg`（127.0.0.1:5500）实测且 `-v` 确认是 PASS 不是 SKIP；`npm test` 36 文件 / 307 例全绿、`vue-tsc --noEmit` 干净，`vite build` 后 `internal/web/dist` 已刷新（43 个路径变更、47 个同名 chunk 逐字节相同、differing=0）；`scripts/validate_delivery.sh` PASSED（新 `cluster-caps` profile 下 master 与 worker 两份 Deployment 都渲染出 `--node-capabilities "shell,pkg"`，默认 `cluster` profile 下不出现）；`browser-check` ALL PASS（自建端口 4611/5188，mock 与 vite 的进程命令行核对过归属，非借用他人 checkout）。

### 新增（多 agent 协作留言板：一条事实通告，带有效期、带校验器，但不是命令通道）

- **背景（都是本仓真实付过的代价，不是假想敌）**：同一台机器上多个会话并行处理这个仓库时，碰撞全部来自"缺一条事实"而不是"缺代码"——一个会话建的工作树被另一个会话批量清理（`F:/Nexus/levee-wt-*` 曾被标成 prunable）；两次把活干进错的 checkout；浏览器级检查连到别人 checkout 的 dev server（默认 5173/4599 在本机就是别人的，跑出三条假红）；两条 PR 同时改 `internal/grpc/rest.go` 却互相不知情。
- **为什么不在仓库里**：留言板曾在设计中途被推到"提交进 git 的一条公告板"，实测两条理由把它否掉——① 提交在分支上的声明，别人要先 fetch 再 checkout 才读得到，那时候碰撞已经发生；② 合进 master 的条目会开始过期，之后每个无关构建都因一条没人认领的过期条目变红。所以**板在共享文件系统上（`-dir` 或 `LEVEE_AGENT_BOARD_DIR`），仓库只携带它的格式定义与校验器**。刻意**没有默认路径**：凭空造一个默认值，会让每个 agent 读不同目录却以为自己协调过了。
- **一个 agent 一个文件**，不是"大家往同一块板末尾追加"：后者在每条并发分支上都会撞 git，协调工具本身不该成为冲突来源。键是封闭词表（`agent/branch/state/updated/ttl-minutes` 必填，`worktree/owns/notes` 可选），**未知键按错误处理而不是忽略**——把 `ttl-minutes` 打成 `ttl_minutes` 若被忽略，这条条目就永远不会过期，下一个人会当它是有效声明。
- **到期即判红**是这套东西唯一的抗腐烂手段：`active` 过期＝失效声明，`done` 过期＝噪音，两者都 exit 1，报文里直接写处置（"refresh it or delete the file"）；`updated` 写进未来也判红（那是在谎报新鲜度），但容忍 5 分钟以内的跨机时钟偏差——要求每台机器对时间达成一致会把板子搞成不可用。谁都能删过期条目（它按定义已不再断言任何东西），但那条工作若还在进行，正确处置是由原作者续期。
- **重叠只报不拦，这是刻意的**：两条 `active` 未过期条目声明同一路径会被打印出来，但不判失败——两条分支改同一个文件在评审队列里是正常状态（#160 与 #161 就都改 `rest.go`），把这种事变成硬失败，结果只会是大家绕过留言板。需要"不可能"的是板子对什么是活的撒谎；需要"看得见"的是这个文件别人也在动。
- **三条边界写进文档，因为它最容易被越过去**：① 不是命令通道（没有优先级、没有派活、没有请求）；② 别人的声明不是证据（"他验过了"不等于你不用自己重跑）；③ 合并与推送的决定权仍属于仓库的人，看到一条 `active` 不构成去合并/rebase/关闭/删除别人分支的理由。**不做指挥层级**：让一个 agent 给别人派活，实质是把用户的授权挪到某个 peer 身上，而且会让"对方结论我自己重跑"这条失效。
- **落地**：`internal/agentboard`（`package main`，同 `internal/docgen` 的先例；标准库实现）+ `docs/agent-board.md`（格式与规则）+ `AGENTS.md`（本仓第一份面向 agent 的指令文件：动手前读板、干活时写自己那条、收尾删掉；另把已付过代价的机器事实钉在同一页——主 checkout 是别人的、工作树会被并行清理所以改完即提交推送、`F:\` 禁 `git stash`、5173/4599 归属、推送只走 ssh 443、PG 测试包必须 `-p 1`）+ `CONTRIBUTING.md` §4.4 与 README 文档索引各一处指引。
- **诚实说明强制力的边界**：参与本身无法用 CI 强制（板子在仓库外，别人的构建看不见）。能被自动拦下的是**格式与文档漂移**——`TestFormatDocMatchesTheParser` 从 `docs/agent-board.md` 的表格里把键集合抽出来与解析器的 `knownKeys` 逐个比对，并核对文件名正则与 TTL 上下界在文档里就是代码里那份；这条守卫自己也被测过不空转（`require.NotEmpty(t, declared)`，写它的时候正因为解析方式错了而真实报红一次）。参与靠 `AGENTS.md`（Qoder 明确把 AGENTS.md/QODER.md 当作长期指令来源，别的 agent CLI 是否自动加载本仓未实测）+ 一条 `exit 1` 的可运行校验。
- **测试**：10 组 / 30 例（含子用例）——合法条目、9 类"无法判定"的畸形条目各自点名（未知键、缺必填、重复键、词表外 state、无偏移时间戳、TTL 越上下界、无冒号散文、含空格的分支名）、文件名规则三类反例、过期 `active`/过期 `done`/未来时间戳/5 分钟偏差容忍/板目录不存在、同分支被两个文件声明、重叠的三种情形（活对活报、对已过期的不报、对 `done` 的不报）、`PathsInUse` 只算活条目（已过期的那条**刻意只声明一个别人没声明的路径**，否则"把过期也算进去"这个变异看不出来）、`README.md` 不当条目、`-dir` 优先于环境变量且无默认值。
- **变异 11 项逐一咬住**：未知键改为忽略（`ttl_minutes` 那条子用例红）；去掉到期判定（`active`/`done` 两条子用例红）；未来时间戳不再拒绝；`MinTTL` 下界放宽到 1 分钟；重叠检查接受已过期的邻居；重叠检查把 `done` 当活的；同分支重复声明不再拒绝；`README.md` 被当条目；`PathsInUse` 不再过滤过期条目；从文档表格删掉一个键；文档里的文件名正则从 `{0,80}` 漂成 `{0,40}`。**其中一项是变异自己暴露的弱断言**：`PathsInUse` 那条最初写的过期条目与活条目声明同样路径，于是"不过滤过期"这个变异**照样绿**——补了一个只有过期条目声明的路径才真正抓得住；测试改的是断言的数据形状，不是放宽判定。另有一条无效变异（把 `len(files) > 1` 改成 `if false` 造成 `files` 未使用而编译失败）被当场判为不算证据，换成 `> 99` 重做。
- 验证：`go build ./...`、`go vet ./...`、`go test ./...` EXIT=0、`gofmt -l` 干净、`golangci-lint` 0 issues、pinned gosec EXIT=0、`go run ./internal/docgen -check -root .` 报 spec is up to date、`python scripts/test_changelog_structure.py` 7 例 OK；前端未涉及、`internal/web/dist` 未动。命令行三种退出码实测：二进制 `-dir` 缺失=2、有条目过期=1、板子诚实=0；**`go run` 会把 2 压平成 1**（只在 stderr 打 `exit status 2`），这个差别已写进 `docs/agent-board.md`，按退出码分支的调用方要用二进制。
