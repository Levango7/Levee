# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 修复（台账-代码一致性门禁扩面 + 陈旧声明订正）

- **缺口**：2026-11-13 全仓普查复核出 9 处陈旧/矛盾声明；`check_doc_anchors.py` 的 CLAIMS 只覆盖 37 条，且 **README 不在 LIVE_DOCS 里**——而 README 正是读者（与 agent）实际进入的第一页。
- **修法**：① CLAIMS 扩到 42 条（`MIN_CLAIMS` 33→38），新增第四轮五条，钉住「审核闸门已接进计划前检（不是『下一批』）」「执行器 AST 主判已落地」「mysql 模块已交付」「SQL 审核 verdict 面」「k8s 变更通道 S2」；② `README.md` 进 LIVE_DOCS（今日仅 1 条 `path:line` 锚点且可解析；docgen 块由 `docs regenerate check` 负责，不靠本门禁）；③ 订正四处陈旧声明：README 服务数 9→10、README 模块清单补 mysql、`levee-api.md` 审核闸门「下一批」改为已落地、k8s 设计文档「k8s 不能当变更目标」行标注「提案基线，已被 S2 推翻」。
- **验证**：`check_doc_anchors.py` PASSED（30 anchors / 7 docs；42 claims 全 hold）；**变异实测**（把某 claim 的关键词换成不存在的符号 → 该行当场判红）；`test_doc_anchors.py` 24 例 OK；roadmap 门禁 PASSED。
- **边界**：`docs/project-status-2026-11.md` §4.1/§5.2 的陈旧读数属**自认陈旧**（§15 已声明「以 §15 为准」），未改；CodeQL 开放告警数（release-notes 时点快照）未改。

### 修复（断网策略 Q1 落地：分区期拒绝新 apply / 新审批）

- **缺口**：`docs/design-disconnect-policy.md` 的 Q1–Q4 已拍板（2026-11-10），但生产代码 0 行——分区期 serve 仍会受理新的 apply 与审批，而那时它无法证明执行权（无租约、无 epoch、无围栏），正是整套设计要避免的物理双跑面。
- **修法**：新增 `internal/partition`（`Checker` 对控制面数据库做有界探测，默认 3s；每次受护调用现测而非缓存——缓存的「可达」恰好会在数据库掉线的窗口里变成「拒绝拒绝」）。`ChangeService` 新增 `WithPartitionChecker`，`ApplyChange` / `ApproveChange` 在首次读库前调用 `preflightGuard`，不可达时返回 `FailedPrecondition` 并在消息里点名条件与文档；读路径刻意不受护（它们本就依赖数据库，会如实降级）。serve 仅在 PostgreSQL 集群态装配（单节点 SQLite 无库可失，行为逐字不变）；`state` 的两个具体 store 各加一个 `Ping`。
- **验证**：`internal/partition` 6 例（nil checker / nil pinger / 可达 / 不可达 / 超时上限 / 默认超时兜底）；`internal/grpc` 5 例（apply 拒绝且 **GetRun 零调用**、approve 拒绝、可达不误拒、nil checker 保持历史行为、读路径不受护）。`go test ./...` 全绿；`golangci-lint` 0 issues。
- **边界**：本笔只落地 Q1（停新任务 A 全拒）；Q2 维持现状（无代码）；**Q3 只读白名单标记与 Q4 恢复四步对账工具未落地**，见设计文档 §3 验收门槛第 2、3 条。

### 修复（dbchange — 发布单把「读不到」当成「没有」——诚实三态、审批三态、校验错误分流）

- **缺口**：只读复核 PR #370（feat/dbchange 数据库变更发布单）时发现四处诚实问题：① 计划制品不可读时，发布单把「读不到」说成「没有」；② 审计链「完好」没有用例承重；③ legacy 审批行被读成「绑定当前计划: true」；④ 审计「未验证」不区分「没有 trace 记录」与「校验出错」。
- **修法**：① `ChangeSection.PlanState` 三态：`none` / `readable` / `unreadable` —— 不可读时 SQL / 审核结论 / 回滚三段如实说「未知」；CLI 新增「计划状态」行；② 审批绑定三态：`MatchesPlan` 只在真绑定时为真，新增 `LegacyApproval` / `StaleApproval`，CLI 出提示行；③ `AuditSection.VerifyError` 记录校验失败原因（无 trace 记录不算错误）；④ 补篡改链用例（`TestCollect_TamperedChainIsInvalid`）。
- **验证**：新增 8 例（不可读计划 / 无计划 / legacy 审批 / 陈旧审批 / 篡改链 invalid / 校验出错 / CLI 三条渲染）；**变异 5 处全红**（PlanState 不标 unreadable / legacy 当成绑定 / 不记 VerifyError / Valid 恒真 / CLI 措辞回退）；文档同步 `docs/cli-reference.md` 第30章（输出分段表 + 计划状态三态表）。
- **边界**：只补状态与措辞，不加 RPC、不改 #370 的读模型结构；CLI 旗标零新增。

### 新增（dbchange — 数据库变更发布单：读模型与 CLI）

- **缺口**：v1.2 把对外第一身份定为「数据库变更发布与治理核」，但一个数据库变更的事实散在四处——`levee show`（状态/证据）、`audit report`（审计）、审批读取、控制台详情——「给我这个变更的单」要开四个面再脑内 join。
- **修法**：新增 `internal/dbchange`（纯读模型，`Collect(ctx, store, runID)`）：Change（身份/状态，Label 用 `dsl.DisplayName` 与 API 同源）、SQL（`mysql.query` 步骤 + 计划里的审核结论）、Rollback（运行级策略 + 逐步补偿）、Window（如实声明不随 run 持久化）、Approval（级别/状态/计划绑定/决定）、Execution（复用 `evidence.Classify` 同一词表）、Audit（`audit.ChainVerifier`）。CLI 新增 `levee dbchange show <change-id>`（本地，`--json` 标准信封；空段说出自己的意思而不是消失）。
- **验证**：`internal/dbchange` 10 例（not found / 无计划 / SQL+审核 / 审批链 / 陈旧审批不误判为匹配 / 执行证据+审计 / 无 trace 时审计不冒充通过 / 窗口诚实 / 计划不可读时降级不失败 / nil store）；`cmd/levee` 3 例（分段渲染 / 空段自述 / JSON 信封）。`go test ./...` 全绿；golangci-lint 0 issues。
- **边界**：本笔只做**读模型 + CLI**，未加 RPC（REST/前端/wire 契约留作下一片）；窗口无法回放「这次按哪个窗口放行」是如实说明而非缺陷（run 行确实没有该列）。

### 文档（SQL 审核链演示手册：四幕离线复跑「高危拦截＋强制审批＋事后审计追溯」）

- **缺口**：§8.6 第 4 步的演示面此前只有脚本（`examples/demo/sql-review-chain.sh`，未随版本库提交，后由并行线收进 `feat/sql-review-demo` 分支、PR #356 在途），没有文档化的跑法、验收口径与话术。
- **修法**：`docs/demo-sql-review-chain.md`——前置（离线、无库、throwaway 目录）→ 跑法 → 四幕逐幕「屏幕看到 / 治理语义 / 验收证据」表 → 三句自撰话术 → 与九步链路的如实映射（覆盖前五步＋审计报告；分批执行 / 验证门禁 / 失败自动回滚需真 target，不在离线演示面）→ 边界（status-only 不冒充执行、本地 `--as` 的既有 WARN 保留、规则文案自撰有专测）。
- **验证**：脚本以当前树二进制实跑四幕全 PASS、RC=0（act0 SR-02 fail / act1 `FailedPrecondition` 具名 / act2 双签＋未达法定人数 `apply` 退出码 4 / act3 报告含结论＋plan_hash＋审批链、空链如实报空）；文档引用断言与产物逐条对上。
- **边界**：纯文档笔，零代码改动；脚本本体未随本分支提交，由并行线收进 `feat/sql-review-demo`（PR #356 在途，归属登记在 agent-board，本线不擅自代提交）。

### 修复（`levee plugin --help` 仍称 channel 插件「registry-only」，与 serve 装载的事实矛盾）

- **缺口**：`cmd/levee/cmd_plugin.go` 的 `newPluginCmd` Long 文案仍写 channel 插件「registry-only (no engine consumption)」，而 serve 启动即装入已启用的通道插件（`installPluginChannels`）、CLI 一次性 apply 路径不装——文案与行为相反（上方注释同病）。
- **修法**：改为如实口径——「Channel plugins are loaded by serve at startup for enabled registry entries; the CLI apply path does not load them; channel_types must match the target's channel_type (lowercase plugin type name). See docs/channel-plugins.md.」注释同步。
- **验证**：`go build ./...` 绿；本机实跑 `levee plugin --help`，Long 全文即新口径（serve 启动装入 / CLI apply 不装 / channel_types 须匹配 target 的 channel_type）；gofmt 干净。

### 新增（k8spolicy 门禁 DSL 接线：YAML 今天起写得出这个门禁）

- **为什么做**：S1（纯函数策略引擎＋门禁适配器）与 #349 的引擎侧预接（`gateFromCheck` 的 `case "k8spolicy"`）都已进 master，但 `internal/dsl` 的 YAML 词表没有 `k8spolicy`，`convertGateCheck` 落 default 报 LE051——YAML 写不出这个门禁，S1 是「引擎认、门禁写不出」的悬空面。opencode 交接单点名归本线（internal/dsl owns）。
- **改动**：`yamlGateRaw` 加 `K8sPolicy *yamlK8sPolicyRaw`（`manifests` 字符串数组 / `manifest` 单串两形态）；`convertGateCheck` 新 case 把类型字段种成 `params.manifests` / `params.manifest`（与引擎 `k8sPolicyManifests` 契约对齐；声明字段在键冲突时获胜、用户 params 同行）；函数尾 `gc.Params = g.Params` 无条件覆盖改为「gc.Params 为 nil 时才透传」，其他 check 类型语义逐字不变；`ast.go` GateCheck 词表与 LE051 文案补 k8spolicy。
- **验证**：新 `internal/dsl/k8spolicy_gate_test.go`（列表形态逐字保留＋用户 params 同行 / 单文档形态 / 冲突时声明字段获胜 / 空块 Type 如实且 Params 为空交引擎 fail-closed）；dsl/engine/verify/cmd-levee 四包 go test 全绿；变异抽查（yaml tag 改名→测试当场抓红→还原回绿）；gofmt 干净；提交前全量门禁实跑——`go test ./...` RC=0、`golangci-lint` 全仓 0 issues（私有 TMPDIR+cache 绕同机锁）、`go vet` 干净。
- **边界（如实）**：空 `k8spolicy: {}` 解析通过、无 manifests，由引擎材料化时 fail-closed 拒（不在 parser 层假装拦截）；`docs/gates.md` / `docs/leveelang-spec.md` 语法节按交接单约定归 opencode 补，本笔不动。

### 新增（SQL 审核链演示脚本：离线跑通「高危拦截 → 强制审批 → 事后审计追溯」）

- **为什么做**：§8.6 第 4 步要求一份可直接跑的演示，把 SQL 审核楔子串成完整链路——fail 级规则在 plan 期拒掉高危批文（审批链从未生成）、warn 级放行但结论随 plan_hash 落进计划产物、high 档强制双人会签（发起人除外）、事后审计报告一页给出规则命中·计划哈希·审批链。公开选型话术里的「阈值分档」只复刻概念，演示与规则文案全部仓库自撰（copyleft 红线：零字抄录外部材料）。
- **改动**：新 `examples/demo/sql-review-chain.sh`（自包含：`mktemp -d` 临时数据目录、heredoc 夹具 hosts.yaml / 两份模板、`assert_exit` / `assert_contains` 助手、`set -euo pipefail` + ERR 陷阱报行号）＋ `examples/demo/README.md`（运行法、四幕证明表带代码指针、如实态说明、文案边界、踩坑记录）。
- **验证**：本机离线实跑整脚本 6 段断言全过、退出码 0（产物 /tmp/tmp.EmQFb1ndrT）——act 0 `sqlreview` 判 SR-02 fail 退出 1；act 1 `plan` 退出 1 并点名 SR-02「DELETE without WHERE」；act 2 warn 模板 plan 落库 hash-bound、未批 apply 两次退出 4、alice 一票仍 pending、bob 第二票后 apply 退出 0 且如实标注 status-only；act 3 HTML 报告含 SR-07 行、`v2:` 计划哈希、alice/bob 审批与 `exclude_initiator`，零 trace 的 run 如实写「no trace records」、`audit verify` 退出 1 报「no traces found」（空链不说完好）。
- **边界（如实）**：演示只走 CLI 一次性路径（new/plan/approve/apply/audit report），本构建未接执行引擎，apply 是 status-only（`cmd/levee/cmd_apply.go:39` 的 WARNING 标记）；模板 SQL 是写死的两份夹具（DELETE 全表 / DROP TABLE），无参数化入口；`LEVEE_AUDIT_HMAC_KEY` 必须从第一次审计写入就设置——中途加 key 会「hash chain exists but is broken」（2026-11-12 实测，README 记为踩坑）。

### 新增（k8s 变更通道：S2——通道插件，核心零行改动）

- **为什么做**：D-4（`docs/design-k8s-change-governance.md`）的第二片。S1 给了"改 k8s 工作负载之前该查什么"，本笔补**执行面**：让一个 Kubernetes 集群成为 LEVEE 可拨的 target，使工作负载变更走与 SSH 主机变更完全相同的审批·快照回滚·WORM 审计链。
- **改动**：新插件 `examples/plugins/k8s`（`type: channel`，`channel_types: [k8s, kubernetes]`，协议与 `tcp_console` 同族）。**核心零行改动**：注册走 serve 既有的 `installPluginChannels`；快照回滚走核心快照器既有的 `Exec("base64 <路径>")`/`Upload(路径, 内容)` 惯用语——本插件把"路径"解释成资源引用，`base64` → `kubectl get -o json`（剥 resourceVersion/uid/generation/creationTimestamp/managedFields/status 后 base64），`Upload` → `kubectl apply -f -`。
- **安全下限（传输自身的地板，审批/门禁在其上只能收紧）**：目标钉死（kubeconfig/context 来自插件 config 与 target 行，调用方命令里的 `--kubeconfig/--context/--server/--namespace/-n/--token/--as` 等改写旗标一律**拒绝**）；动词白名单（默认只放行改/读已声明工作负载的动词；`exec/port-forward/proxy/cp/attach/debug/run/auth/config/certificate` 等逃生口动词**不在默认集**，放行是运维在 config 里的显式决定）；无 shell（引号感知 tokenizer 切 argv 直跑，shell 元字符拒绝）；明文凭据不过管道（kubeconfig 在插件自己的 config 里，宿主只传 `credential_ref` 名）。
- **验证**：契约单测 22 例（fake kubectl 断言参数向量：钉死旗标前置、白名单、改写旗标拒绝、抓取剥字段、还原逐字节透传、协议环回）＋ **kind 实测**（`TestK8sChannelDrill`，env 门控 `LEVEE_K8S_DRILL_CONTEXT`，真集群：快照抓取→scale→按快照还原断言全绿）；`go vet`/`gofmt`/`golangci-lint` 干净；`go build ./...` 绿；DSL examples 门绿（插件 yaml 按 `/plugins/` 豁免，计数仍 6）；`go mod tidy` 无漂移。
- **边界（如实）**：① 插件通道只在 **serve 启动时装入**，CLI 一次性路径 `levee apply --engine-enabled` 不装（`cmd/levee` 他人认领区）——三场景的引擎半边（审批拒绝→不执行、审计链）是引擎既有行为且传输无关，走 serve 得完整链路；② `levee target check` 的 `echo ok` 探针集群答不了（仅诊断命令）；③ exec 无 stdin，manifest 应用走 upload。不做 Operator、不做声明式同步、不做 GitOps。

### 新增（k8s 工作负载策略门禁：S1——纯函数策略引擎 ＋ 门禁适配器，fail-closed）

- **为什么做**：定位 v1.2 把对外第一身份改成「数据库变更发布与治理核」之后，云原生侧真正空着的是「高危动作级变更的**审批·回滚·举证**」——声明式交付归 ArgoCD/Flux、观测归 Prometheus/Zabbix/ELK，没人管谁批谁回滚。`docs/design-k8s-change-governance.md`（D-4）把这件事拆成三片，**本笔是 S1**。
- **改动**：新包 `internal/k8spolicy`（不引 K8s 依赖，把 manifest 当通用 YAML map 读；11 条策略：privileged / allowPrivilegeEscalation / hostPath / hostNamespace / runAsUser=0 / 加 ALL·SYS_ADMIN / 镜像未钉版 / 无 resources.limits / 未声明 runAsNonRoot / 自动挂载 SA token / hostPort，每条给**字段定位 + 原因 + 补救**）；`internal/verify` 新增 `K8sPolicyGate`（critical 阻断、warning 放行但仍报出、**解析失败按拒绝处理**——读不到不等于没问题）。
- **验证**：新包 11 例（每条策略一例 ＋ 安全清单零命中 ＋ 非工作负载跳过 ＋ 多文档 ＋ CronJob 模板 ＋ 解析失败即错误）＋ 门禁 4 例（阻断 / 告警放行 / 读不到即拒 / 默认阶段）全绿；`go vet` 干净；`gofmt` 干净；内部包可达性门绿（新包经 `internal/verify` 可达）。
- **边界**：本笔**只做策略与门禁本体**，不接执行面（改 k8s 工作负载的通道是 S2），也不改任何既有命令行为；门禁的注册与 DSL 接线待 S2 与 `internal/dsl`/`cmd/levee` 的认领者协调。

### 文档（云原生变更治理面设计提案 ＋ 平台适配矩阵；账本新增 D-4）

- **缺口**：定位 v1.2 把对外第一身份改成「数据库变更发布与治理核」之后，"信创 / ARM / 云原生"三类落地条件一直**没有成文清单**——全仓对麒麟/统信/达梦/人大金仓/国密**零提及**，账本 A-4 只有"信创硬件"四个字；同时 K8s 只出现在**部署面**（Helm chart），作为**被治理的变更目标**没有任何执行面（执行通道只有 local/ssh/winrm，全仓无 k8s 客户端）。
- **改动**：`docs/design-k8s-change-governance.md`（设计提案：为什么走「治理面延伸到云原生」而不是「补通用运维面」，以及 S1 策略门禁／S2 k8s 变更通道／S3 适配矩阵三片的范围与本地验收方式）；`docs/platform-support-matrix.md`（逐格标「已验证／未验证／不支持」，只登记本仓自己跑过的结论）；`docs/blocked-items.md` 新增 **D-4** 行，并给 A-4 加矩阵指针。
- **验证**：事实基线逐条给证据（CI 构建矩阵含 linux/macos/windows × amd64/arm64；Helm chart 模板清单；执行通道仅 local/ssh/winrm；DB 驱动仅 sqlite/postgres；无任何 k8s 客户端代码）。四道文档门全过：check_blocked_items PASSED（16 items，D=3）、check_doc_anchors PASSED（29 anchors / 37 claims）、check_roadmap_status PASSED（55 rows）、test_changelog_structure OK。
- **边界**：本笔是**提案 ＋ 清单**，零代码；S1–S3 待 owner 拍板范围与先后后逐片实施。文档里的市场判断来自本仓定位与产品结构，**不是**来自真实标书。

### 修复（`go` 指令 1.26.10 → 1.26.11：收口新发布的标准库公告 GO-2026-6859，并升 `golang.org/x/crypto` 到修复版）