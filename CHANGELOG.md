# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 修复（断网策略 Q1 落地：分区期拒绝新 apply / 新审批）

- **缺口**：`docs/design-disconnect-policy.md` 的 Q1–Q4 已拍板（2026-11-10），但生产代码 0 行——分区期 serve 仍会受理新的 apply 与审批，而那时它无法证明执行权（无租约、无 epoch、无围栏），正是整套设计要避免的物理双跑面。
- **修法**：新增 `internal/partition`（`Checker` 对控制面数据库做有界探测，默认 3s；每次受护调用现测而非缓存——缓存的「可达」恰好会在数据库掉线的窗口里变成「拒绝拒绝」）。`ChangeService` 新增 `WithPartitionChecker`，`ApplyChange` / `ApproveChange` 在首次读库前调用 `preflightGuard`，不可达时返回 `FailedPrecondition` 并在消息里点名条件与文档；读路径刻意不受护（它们本就依赖数据库，会如实降级）。serve 仅在 PostgreSQL 集群态装配（单节点 SQLite 无库可失，行为逐字不变）；`state` 的两个具体 store 各加一个 `Ping`。
- **验证**：`internal/partition` 6 例（nil checker / nil pinger / 可达 / 不可达 / 超时上限 / 默认超时兜底）；`internal/grpc` 5 例（apply 拒绝且 **GetRun 零调用**、approve 拒绝、可达不误拒、nil checker 保持历史行为、读路径不受护）。`go test ./...` 全绿；`golangci-lint` 0 issues。
- **边界**：本笔只落地 Q1（停新任务 A 全拒）；Q2 维持现状（无代码）；**Q3 只读白名单标记与 Q4 恢复四步对账工具未落地**，见设计文档 §3 验收门槛第 2、3 条。

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

### 新增（k8s 变更通道：S2——通道插件，核心零行改动）

- **为什么做**：D-4（`docs/design-k8s-change-governance.md`）的第二片。S1 给了"改 k8s 工作负载之前该查什么"，本笔补**执行面**：让一个 Kubernetes 集群成为 LEVEE 可拨的 target，使工作负载变更走与 SSH 主机变更完全相同的审批·快照回滚·WORM 审计链。
- **改动**：新插件 `examples/plugins/k8s`（`type: channel`，`channel_types: [k8s, kubernetes]`，协议与 `tcp_console` 同族）。**核心零行改动**：注册走 serve 既有的 `installPluginChannels`；快照回滚走核心快照器既有的 `Exec("base64 <路径>")`/`Upload(路径, 内容)` 惯用语——本插件把"路径"解释成资源引用，`base64` → `kubectl get -o json`（剥 resourceVersion/uid/generation/creationTimestamp/managedFields/status 后 base64），`Upload` → `kubectl apply -f -`。
- **安全下限（传输自身的地板，审批/门禁在其上只能收紧）**：目标钉死（kubeconfig/context 来自插件 config 与 target 行，调用方命令里的 `--kubeconfig/--context/--server/--namespace/-n/--token/--as` 等改写旗标一律**拒绝**）；动词白名单（默认只放行改/读已声明工作负载的动词；`exec/port-forward/proxy/cp/attach/debug/run/auth/config/edit/certificate` 等逃生口动词**不在默认集**，放行是运维在 config 里的显式决定）；无 shell（引号感知 tokenizer 切 argv 直跑，shell 元字符拒绝）；明文凭据不过管道（kubeconfig 在插件自己的 config 里，宿主只传 `credential_ref` 名）。
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

- **一刀切在所有人头上的红**：2026-11-11，本仓 `govulncheck` 作业失败，报 `Vulnerability #1: GO-2026-6859 — Standard library`，`Found in: net/http@go1.26.10 / Fixed in: net/http@go1.26.11`，实例跟踪直指本仓代码（经 `net/http` 的服务端路径）。同一批扫描另报 `GO-2026-5932`（`golang.org/x/crypto@v0.58.0`，公告给出的修复版为 `v0.58.1-0.20261009181718-2f1f834330b2`）——本仓代码**不可达**该漏洞，但它仍在依赖图里。
- **为什么只能升 `go` 指令**：本仓的 `govulncheck` 作业刻意按 `go.mod` 声明的工具链扫描（`ci.yml` 的 `go-version-file: go.mod`），标准库类公告没有别的旋钮——这与 2026-10-21 收口 GO-2026-6809（`go 1.26.9 → 1.26.10`）是同一条路径。
- **改动面**：`go.mod` 一行 `go 1.26.10 → 1.26.11`，外加 `golang.org/x/crypto v0.58.0 → v0.58.1-0.20261009181718-2f1f834330b2`（公告指定的修复版；本仓不可达，先收口以免公告把它升级为可达）。`AGENTS.md` 与 `docs/project-status-2026-11.md` 里「`go.mod` 要求 1.26.10」的当前声称同步订正；`CHANGELOG.md` 与 `docs/release-notes/*` 里的历史记录**不改写**。
- **验证**：本机按 `go.mod` 拉到 go1.26.11 后 `govulncheck ./...` = **No vulnerabilities found**（同一命令在修复前报 stdlib 1 处命中）；`go build ./...` rc=0；`gofmt -l` 干净；`go test ./...` 本机全绿；CI 侧以 master push CI 为准。
- **边界**：本笔只升工具链与一个模块，不改任何产品代码；Go 补丁版本升级不引入语言或语义变化。`Dockerfile` 的 `ARG GO_VERSION=1.26` 刻意用 major.minor 别名（会取到 1.26.11），本笔不改。

### 文档（产品对外楔子 v1.2 的第一接触面：README/quickstart 补 SQL 审核，proto 规模改由 docgen 生成）

- **缺口**：定位 v1.2（2026-11-10 市场扫描裁决）把 `levee sqlreview` 定为读者最先接触的一面，
  但 README 与 quickstart 对它零提及（`git grep sqlreview -- '*.md'` 只命中 CHANGELOG /
  cli-reference / product-positioning / product-roadmap）；README 还带着过期读数（当前版本写
  v1.13.0，实际 v1.25.0；RPC 计数写 53 个 = 40+13、22 个挂 `ChangeService`、九个服务）。
- **修法**：README 与 quickstart 补 SQL 审核楔子（指向 `cli-reference.md` 第 29 章，不复制规则清单）；
  `docs/levee-api.md` 新增 §2.11 SQL 审核命令（附录 A/B 同步），补上设计文档对楔子的缺席；
  README 与 `product-positioning.md` 的 proto 规模数字改由 `internal/docgen` 从**已编译的 proto
  描述符**生成（两个文档嵌同一块，单一来源；CI 的 docs regenerate check 兜底），消灭手抄副本；
  订正 README 版本行。
- **边界**：零代码行为变更；不碰 `internal/sqlreview` 的规则实现，规则清单仍以 cli-reference 为准。

### 新增（接链三项：SQL 审核裁决进计划漏斗、执行器 AST 白名单、审计报告接审核结论）

- **缺口**：SQL 审核引擎首版只到 CLI 演示面——`levee sqlreview` 的裁决没有卡进任何生产路径：计划照常生成、`mysql.query` / `pt_osc` 的破坏性语句只靠正则白名单、`levee audit report` 里看不到审核结论。
- **修法**：
  - ① 计划生成前预检接审核：`wiring.GeneratePlan` 在窗口门禁之后对**已解析输入**的 SQL 面跑审核，fail 即拒绝（`sqlreview.ErrReviewFailed` 哨兵错误，gRPC 侧传 `FailedPrecondition` 并写 `failed` 的 sql_review 审计行；成功路径写 `passed` 行，Target=计划哈希）；审核结论进 `plan.SQLReview` 并入 canonicalPlanV2（`omitempty` 保旧计划哈希不变）；dry-run 不落证据；回滚重载存量计划刻意不过门，单一强制点由结构测试钉住。
  - ② 执行器白名单 regex→AST：`rejectDestructiveSQL` 先切分语句、逐条解析，DDL drop/truncate 与无 WHERE 的 DELETE 由 AST 判决（含正则漏掉的尾随注释写法）；正则白名单保留作兜底（解析失败与语法盲区如 DROP INDEX / DROP USER），且对语法可读的非破坏语句照跑——正则原来拦住的现在照样拦，行为零回退。`pt_osc` 的 alter 条款白名单不变（正则条款是实质守卫），AST 只作软确认：语法盲区恒真，只拒正面误读。
  - ③ 审计报告＝审核结论+计划哈希+审批链：`levee audit report` 每 run 显示计划哈希与计划产物里的审核结论，三种如实状态——有结论 / 无结论（产物早于门禁或无可审 SQL 面，无结论≠通过）/ 有哈希无产物（漂移嫌疑，标红）。
- **验证**：`internal/wiring` 新增 13 例（含 RPC 形状两例：拒答传 `FailedPrecondition` 且写 failed 审计行、通过写 passed 行且 Target=计划哈希；回滚路径不过门的结构测试）；`internal/plan` 追加 2 例（SQLReview 进 canonical v2、nil 结论保旧哈希）；`internal/executor/modules/mysql` 新增 3 例（AST 主判尾随注释 DELETE、正则兜底语法盲区、alterShapeIsSingle）；`cmd/levee` 审计报告 E2E 新增 1 例（结论+哈希+审批链聚合与三种诚实状态），并修掉 `--output` 旗标跨测试泄漏（每例复位旗标）；`go test ./...` 全量绿（internal/plugin 一次环境抖动复跑绿），四包 `go test -race` 绿，golangci-lint 全仓 0 issues（goimports 抓的 import 分组 1 条已修；首跑冷缓存超默认时限，加 `--timeout 15m` 复跑全仓绿），go mod tidy 无漂移，四道 python 门禁（roadmap 55 行 / CHANGELOG 骨架 / doc 锚点 29 锚点 37 claims）绿。

### 修复（`levee cluster status` 文档口径与二进制不符）

- **缺口**：`cmd_cluster_status.go` 把子命令的 `Use` 写成 `"cluster status"`，
  cobra 只认第一个词作命令名，文档与 DEMOS 里承诺的 `levee cluster status`
  实际以 `unknown command "status"` 收场——又一个「文档写了、二进制没有」
  的口径洞。
- **修法**：按 target/agent 既有的父子登记法，`levee cluster` 作父命令注册、
  `status` 作子命令，文档口径即二进制行为。
- **验证**：灾备验收现场实跑 `levee cluster status --pg-dsn ...` 输出
  Nodes / Unplaceable Approved Runs / Worker Load / Active Assignments 四段
  （证据 `run/cluster/evidence/s1/cluster-status-after-kill.txt`，本地产物
  不入库）。

### 修复（serve 集群态 pgStoreDB 直接断言 `*state.PGStore` 落空，围栏 / 接管循环拿 nil db）

- **缺口**：`cmd_serve.go` 的 `pgStoreDB` 直接类型断言 `*state.PGStore`；
  集群态下传给接管 / 围栏循环的是漏斗装饰器（RunEventStore），断言落空
  ⇒ 每一轮 sweep 都报 `cluster: expired executions: nil db`，围栏与接管
  形同虚设。
- **修法**：沿 `state.Underlying` Unwrapper 缝走到最内层真后端（与
  StoreDriver / calendarFor 同一走法），单节点 SQLite 仍返回 nil（围栏是
  集群专属，不变）。
- **验证**：`cmd/levee/serve_pgstore_unwrap_test.go` 源码级回归门，变异
  验证过（改回直接断言即 FAIL）；容器靶场三场景实跑中围栏 / 接管均生效
  （见下条新增）。

### 修复（REST 网关 `WriteTimeout: 30s` 砍掉同步长操作的响应）

- **缺口**：`internal/grpc/rest.go` 的 `http.Server` 设了连接级
  `WriteTimeout: 30s`，而数据面是**同步**长操作——`/changes/:id/retry`
  （与 `apply`）执行整个变更计划（仅 hold 步就 120s+）后一次性写响应。
  实测：灾备验收 S1 里服务端全跑完（lock preempted → released、run
  completed、账本 exactly-once），客户端却收 `HTTP 0 / Remote end closed
  connection without response`——run 已收敛，调用方无从知晓。
- **修法**：`WriteTimeout` 置 0（构造提出为 `newHTTPServer(mux)` 缝）；
  逐请求约束归 request context，连接级写截止日期与同步长操作互斥。
  `ReadHeaderTimeout` / `IdleTimeout` 两个 slowloris / keep-alive 守卫保留。
- **验证**：新增 `rest_server_write_timeout_test.go` 钉死
  `WriteTimeout == 0` 且两守卫不连带撤掉；变异验证过（加回 30s 门即
  FAIL）；S1 终态树 retry 实收 `HTTP 200 … "status":"completed"`。
- **如实边界**：置 0 后响应侧不再有连接级兜底，慢客户端可长连——数据面
  全量响应都是一次性小 JSON，风险可接受；真需要流式 / 大响应再评估
  按路由分级。

### 新增（裸机灾备验收：三场景容器靶场 + 机器断言 + 诚实契约）

- **缺口**：灾备能力（集群接管 / 围栏 / 恢复）停在设计文档与 VM 剧本，
  无现场证据；旧 DEMOS A4 口径与实现三处冲突（`levee retry` 对
  interrupted 是错指令、「接管不重跑副作用」对硬杀在途轮次不成立、
  「第一步仍是 completed」对零行账本不成立）。
- **修法**：`deploy/lab/container-lab/` 新增 `clusterdrive.py` 驱动 +
  `workflows/lab-cluster-takeover.yaml`（write-marker + hold 120s 的执行体）
  + `Dockerfile.node` 节点镜像；三场景全机器断言：S1 `docker kill -s KILL`
  worker、S2 `docker network disconnect` worker（进程活真分区）、S3
  `docker stop/start` PG（命名卷进程级重启）。证据落
  `run/cluster/evidence/s{1,2,3}/`（`run/` 已被 .gitignore 覆盖）。
- **实测终态**（2026-11-10 终态树全链）：S1 `VERDICT: PASS (10/10)`、
  S2 `VERDICT: PASS (9/9)`、S3 `VERDICT: PASS (8/8)`。共同事实：run 收敛
  `interrupted` 不卡 running；接管事件可读且属本 run；在途轮次账本零行
  （步级行本就在 closure 结束一次性落盘）；围栏拦住愈合 / DB 恢复后旧执行
  者的晚期落盘（trace `evidence_discarded` ≥ 1、steps 仍零行）；REST retry
  后 `completed` 且每步恰一行 success（账本 exactly-once）；标记文件 mtime
  推后（物理 at-least-once，设计事实）。
- **诚实边界**：① 硬杀 / 分区丢在途证据是设计事实，不是缺陷；② S3 是单
  PG 进程级重启 + 命名卷，**不是**副本提升；③ 恢复时延上界 = 目标机锁
  TTL（300s = exec_timeout），硬杀后孤儿锁等 TTL 自然过期是防物理双跑的
  安全网，产品行为合理不改；④ 容器靶场 ≠ 真 VM 实验室，DEMOS A4 的 VM
  形态仍未实跑（已在 DEMOS 如实登记）；⑤ #97 断网策略提案需收录「恢复
  时延上界 = 锁 TTL」这条实测事实。

### 修复（新落地特性文档漂移清扫：region 派发 / 运行期通道池化 / push 旗标 / webhook 镜像四处口径）

- **缺口**：四批已落地特性在 docs 里的口径停在实现前——region 派发只写三种拒绝（漏 `plan_unreadable`）且把「某机房无人声明」误并入 `no_single_node_spans_regions`；运行期通道仍写「无跨 run 连接池」（D-1c 起已池化，cli-reference / README / tcp_console 示例三处同病）；`levee push` 的 `--platform` 默认 `ios`、`--body` 非必填、不带选项报错 exit=2、失败聚合 `ErrPushFailed` 无部分成功，与 cli-reference 的「必填 / 显示配置状态 / 部分成功」全相反；webhook 镜像已随 run 生命周期全量投递，而 mvp-tasks D-20 / blocked-items E-2 / config.example.yaml / CHANGELOG 旧节仍写「唯一事件源是回滚分级」「serve 尚未配置渠道」「HMAC 未做」。
- **修法**：逐特性对照代码订正（判据与错误码以 `internal/dispatch/placement.go` 的四个 Reason 常量为准；池化口径以 `internal/plugin/pool.go` 为准；push 旗标以 `cmd/levee/cmd_push.go` 为准；镜像边界以 `internal/notify/run_lifecycle.go` 为准），共 10 个文件 21 处；只改叙述不改锚点标记。
- **验证**：`scripts/check_doc_anchors.py` PASSED（29 anchors / 29 claims）；`python -m unittest discover -s scripts -p 'test_*.py'` 132 OK；`go run ./internal/agentboard -dir …` 仅剩两条他人 active 过期（按板纪律只报不改）；逐行复读全量 diff（23+/25-）无语义误伤。

### 新增（跨机房拒绝诊断：点名 offending batch + 给可操作补救）

- **缺口**：上一笔跨机房派发把混合机房的 run 如实拒在派发侧，但拒绝摘要只写「需要 dc-a,dc-b」——操作员知道涉及哪些机房，却不知道**哪一批**把它们混在一起，也不知道该拆 run 还是补节点申报。
- **修法（判定不动、诊断上车）**：`forRun` 的结论仍只读聚合 `Regions`，与上一笔逐字节一致；每批的机房足迹以未导出字段 `batches []batchFact` 随行携带（诊断载荷，不进判定）。`Placement.Diagnosis(reason)` 组合三段：需求（既有 `String()`）＋批次足迹（`batches: batch 0: dc-a; batch 1: dc-b`，封顶 5 批，超出写「N more batches」）＋按原因给的补救。`recordRefusals` 两处（首次分配与 reclaim）改打 `Diagnosis`；`levee cluster status` 的 stuck runs 表新增 `DETAIL` 列、`--json` 行内新增 `detail` 字段——**纯增量**，原 `placement`/`reason` 字段不动，现有消费者不受影响。
- **补救文案取自 `docs/scenario-cross-region-ops.md` 的诚实边界建议**（拆成每机房一条 run，或用 `--node-regions` 桥接节点），文档与 CLI 不各说各话。
- **验证**：`internal/dispatch` 3 例（拒绝点名 batch 0/batch 1 与补救文案；同机房两批的批次事实不改变放置结论、仍落 leader——守住「诊断不进判定」；`Diagnosis` 四原因合成＋未知原因无 fix＋7 批封顶写「2 more batches」）；`cmd/levee` 2 例（stuck run 的 `Detail` 含批次事实与补救、表格渲染出 `DETAIL` 列）。**变异 2 条**：`batchLine` 恒空、`remedy` 恒空各让这 4 例红；还原后 `cmp` 逐字节核对。
- **如实边界**：① 批次事实是诊断载荷、不是新判据——没有任何代码路径按 batch 决定放置（有专门用例守）；② reason 词表与指标标签不变，监控面板不必改；③ 拆 run 是人工操作，本笔不提供拆分工具——那正是被成本核实证伪的 A 案。

### 修复（`target list --json` 空库存返回 `[]` 而非 `null`）

- **缺口**：local store 无行时返回 nil slice，remote 分页器以 `var targets []*state.Target` 起始，两条路径都直接作为 `data` 输出 `null`。
- **修法**：`printTargetList` 在打印前归一化为 `[]*state.Target{}`，local 与 remote 一并覆盖。
- **验证**：新增回归用例（fresh store，**修复前实测 `"data": null`**）；`go test ./cmd/levee` 定向子集全绿；`go vet ./cmd/levee` rc=0。

### 新增（执行结果不确定：把「跑过但没记下来」变成一条可读的事实，而不是空 step 列表）

- **缺口（不是推测，是三条代码路径）**：执行证据（batch/step 行）在批次闭包返回后**一次性写入**（`internal/wiring/run.go` → `persistClosureResults`）。于是三种情况会让 run 的状态说有动作、证据表却什么也没有：① 执行节点死在批次中途——闭包没返回，一行都没写，接管把 run 收成 `interrupted`；② 执行者在写证据前丢掉租约——它已执行的证据被**故意丢弃**（`run.go` 的围栏注释写明「脏证据比缺失证据更糟」）；③ 证据持久化失败但步骤已跑——run 落在 `failed`，行数却不全。三种情况下「这台变更到底动没动过我的主机」的诚实答案是**不知道**，而平台把它渲染成空 step 列表——读起来和「什么都没跑」一模一样。两种读法导致相反的操作：一个是安全重试，另一个是在未知的半应用状态上重试。
- **形态（B 案：事实独立于状态词表）**：不动 run 状态机，也不加列。把事实写成 **run 自己的 trace 事件**（append-only、按 run 封链、接管本来就用这条通道记 `interrupted_by_takeover`）：新增 `state.EventEvidenceDiscarded`（围栏丢弃，在 `run.go` 的围栏分支写）与 `state.EventEvidenceIncomplete`（持久化失败，在持久化失败分支写），外加 `state.EventRetryOverUnknownOutcome`（重试决定）。为什么用 trace 不用新列：`runs` 被多条路径整行覆写，一列要每个写方都记得保留，漏一个事实就静默消失——正是本笔要终结的那类缺陷。
- **判定收成一处（`internal/evidence`，新包）**：`Classify` 把「状态 + trace 事件 + batch/step 行数 + 是否有派发分配行」判成 `not_started` / `in_flight` / `complete` / `partial` / `missing` / `unrecorded` 六种之一，每种对应一个**不同的操作动作**；`ForRun` 是唯一的取数实现，CLI 与执行器共用——「能不能安全重试」不是某个输出面的意见。两个判据值得点名：**`interrupted` 永远不是 `complete`**（有行也只是一次更早尝试的），**「是否派发过」只认派发分配行**（单机模式的 `cancelled` 没有分配行，把它叫「派发过且为空」会对每个取消的草稿报假警）。
- **呈现**：`levee show` 在状态下面固定打一行 `Evidence:`（`complete` 时也打，否则「没有这行」本身就成了需要读代码才知道的含义），`--json` 给 `data.evidence = {kind, reason, unsafe_to_retry}`；`levee retry` / `retry-host` 在 `partial`/`missing`/`unrecorded` 时先往 stderr 告警，并把 `retry_over_unknown_outcome` 写进该 run 的 trace 链；引擎侧 `retryChange` 同样在 CAS 之前判定并记录（覆盖 RPC 与 Web 控制台使用的同一条路径）。**重试不被拒绝**——`interrupted` 的续跑本就依赖「跳过幂等模块已完成的批次」，`failed` 用整计划重跑；把读侧的事实变成卡死的 run，只会把操作员逼去手改状态列。文档如实写了这条取舍。
- **验证**：`internal/evidence` 15 例（六种判定的每个入口，含「缺口事件压过状态」「丢弃压过不完整」「`interrupted` 有行仍是 partial」「`cancelled` 无派发记录不叫 missing」；另两条守卫：**判定理由不得读成安抚**——凡 `UnsafeToBlindRetry` 的理由必须说清「可能改了主机」而不是只说「没有证据」）；`internal/wiring` 5 例（**持久化失败**：用一个只在 `CreateStep` 上失败的嵌入 store 驱动真实执行，断言执行确实发生、step 行确实没有、`evidence_incomplete` 确实写下，且**按 batch 行单独判定会误报 complete**——这条把「事件为什么必须有」钉住了；**围栏丢弃**：沿用既有 fake guard 场景断言 `evidence_discarded`；**取消的 ctx 仍要记录**；引擎重试对无证据的 run 记录、对有证据的 run 不记录）；`cmd/levee` 3 例（`show` 的 `missing` 与 `complete` 两面对照、CLI 重试对无据 run 记录该事件、有据 run 不记录）。
- **如实边界**：① **服务端/前端尚未呈现**——`levee show` 是本地只读命令，集群部署的操作员要看到这行判定需要给线上消息加字段（proto + TS 词汇生成），本笔不含；② 重试**只告警不拒绝**（上文的取舍）；③ 阈值/风控不介入——平台只回答「记没记下来」，不判断「该不该回滚」；④ 本笔不动 `docs/blocked-items.md`（opencode 持 D-3 行）。

### 新增（inventory `region` 标签闭环：导入期校验 + `target label set/unset` 逐台编辑）

- **缺口（上一笔派发能力的两条未闭环处）**：`internal/dispatch` 按 `targets.labels["region"]` 决定 run 落在哪个机房，但 ① **没有任何地方校验这个值**——`region: "cn north"` / `region: "dc-b "` 会原样入库，与节点申报的 `region=<名>` 逐字节比较时永远不匹配，run 在派发时被拒、日志却在说节点（`no_eligible_node`），把操作员送去查节点而错误其实在清单里；② **唯一的写入方是 `target import`，而导入会替换该行的整套标签**——给一台已上线的机器补一个 `region`，得把它的 `env`/`role` 等全部重抄一遍，漏写一个键就是静默丢掉一个标签。
- **修法（一个校验函数，两个写入路径）**：`internal/capability` 新增 `RegionLabel` 与 `ValidateRegion`——`RegionLabel` 是标签键的唯一拼写（`dispatch.TargetRegionLabel` 直接别名它，不再是两份字符串），`ValidateRegion` 是名字语法的唯一实现，节点申报的 `Normalize` 与 inventory `region` 标签都走它：首尾空白去掉（YAML 值与 shell 参数都会招来空白），中间空白与 `,` 拒绝。**两个校验器会让一个名字在两侧合法性不同，那正是「节点在、但到不了任何地方」的形状。** 导入路径（YAML 与 `ImportTargets` RPC 共用 `Importer.Import`）按行校验并把规范值写回，不合法则该行失败且错误点名 `label "region"`；`LabelChanges` 预览对同一行跳过（与既有的 `invalid status` 跳过同构——它会失败的行走不到写入，预览就不该为它申请标签变更授权）。
- **`levee target label set <address> key=value … [--port N]` / `unset <address> key …`**：语义是**编辑而非替换**（未点名的键保持原样），幂等（设为已有值、或 `unset` 不存在的键都是 no-op，人类输出写 `unchanged`、`--json` 的 `changed=false`，不谎称写过）。**`env` 明确不可经此编辑**：目标机的环境决定策略把读与远程执行授权到哪，`target-ownership.yaml` 在导入路径上管着它（`EnvConflicts`），这条命令直接改它等于绕开该检查，因此拒绝并指回 `levee target import`（那里矛盾会被拒、除非 `--allow-env-conflict` 且写审计）。库侧实现在 `internal/inventory/labels.go`（`SetTargetLabels` + `FormatLabels`），CLI 只做参数解析与渲染；`target list` 的 LABELS 列改用同一个 `FormatLabels`，**操作员编辑的那一行就是刚才列出的那一行**，不再靠两处巧合的排序实现保持一致。
- **仅本地，且是「打开库之前」就拒绝**：没有能改标签的 RPC，`--remote` 由既有的 `validateServiceMode` 在 `PersistentPreRunE` 拒绝（`[exit=2]`）——命令不会加载配置、不会打开库，所以「操作员以为在跟 server 说话、实际写了本地库」在这里不可能发生。这是该守卫第一次被一个新写的**写**命令继承，e2e 直接断言了这条拒绝。
- **验证（17 个新用例 + 4 条变异全红在预期位）**：`internal/capability` 2 例（`ValidateRegion` 表：规范化与四种非法名；`RegionLabel` 与 `RegionPrefix` 必须点名同一条事实）；`internal/inventory` 10 例（非法 region 行失败且**没有一次写入**、规范值落库、非 region 标签原样通过、编辑保留未点名键、幂等且不重写、`env` 的 set/unset 双双被拒、region 值校验、目标不存在、`ParseAddress` 与导入同优先级、`FormatLabels` 排序）；`internal/dispatch` 1 例（YAML → importer → store → `DispatchOnce` 全链：标签进得来、派发按同一键读得到）；`cmd/levee` 4 例（`set` 保留其余标签并持久化、四类拒绝**且拒绝后该行逐字节未变**、`--remote` 在开库前被拒、参数解析）。**变异**：① 去掉导入期校验 → 2 例红；② `env` 守卫失效 → 库侧与 CLI e2e 同时红（证明 e2e 真在打守卫，不是复述库侧）；③ 编辑改成替换 → 库侧与 e2e 同时红；④ 去掉幂等判据 → 幂等用例两处红。
- **本笔自己抓到的一处测试注释与射程不符（已订正，不是自述）**：`TestDispatchObeysTheRegionLabelAnImportWrote` 的初稿注释声称「导入存下未规范化的值也会在这里失败」，但实测**不会**——派发侧 `targetsFor` 也做了 `TrimSpace`，所以带空白的值两边都能匹配。该用例真正承重的是「标签被导入写进去了、派发按同一个键读到了」（丢掉标签则 run 变成不受约束、leader 赢得平局，断言 OwnerNode 必红）。注释按实测改写，规范化这一条留在它真正该在的地方（`internal/inventory` 的规范化用例）。
- **如实边界**：① 标签编辑**不写审计链**（与 `target freeze/unfreeze/retire` 一致；审计链没有标签事件，本笔不新造一个语义）；② 读-改-写之间不加锁，两个并发编辑同一行时以最后写入者为准（与 `target import` 的整行替换同风险）；③ `env` 仍只能经导入路径改；④ 本笔不动 `docs/blocked-items.md`（opencode 持 D-3 行），也不改 `target import` 的既有替换语义——两种语义各有其位，文档已分别写明。

### 架构（跨机房派发：`region` 放置——把「区域 worker 就近执行」从文档承诺变成派发判据）

- **缺口（不是推测，是两条事实对不上）**：`docs/scenario-cross-region-ops.md` 写「集群 worker 就近执行：各区域一台 worker，leader 统一派发」，而 `internal/dispatch/dispatch.go` 的 `workerLoads`/`sortByLoad` 只按在途分配数排序、`claim` 直接取最闲的活跃节点——**目标机在哪个机房与派发给谁无关**。同一份文档的部署图又写着「区域 worker 就近执行、本区 SSH 到目标机」。于是北京机房的目标可能被交给上海机房的 worker：网络通就跨区执行（违背安全域），不通就如实失败。`internal/capability` 的包注释当时已把这个边界写明（「dispatch 按负载放置、不知道能力声明；在 run 没有声明自己需要什么之前应保持如此」）——本笔就是补上那个前提。
- **设计（不新增列、不新增 DSL 关键字、无迁移）**：两台都已存在的事实各自归位——**目标在哪个机房**是 inventory `targets.labels["region"]`，**节点能到哪些机房**是 `cluster_nodes.capabilities` 里的保留 token `region=<名>`（新旗标 `serve --node-regions dc-north,dc-south`，操作员不必手写 token）。`internal/capability` 新增 `RegionPrefix`/`Regions`/`Reaches`，并把「region token 必须在申报时就是合法的」放进 `Normalize`：`region=` 与 `region=cn north` 会被拒绝启动，而不是存成一个读不出任何机房的字符串（那种值在派发看来与「没声明」无法区分，正是要消灭的静默降级）。`Normalize` 同时把 `region= dc-b` 规范成 `region=dc-b`——同一机房的两种写法必须落成同一串字节，否则「存储文本是集合的函数」这条既有承诺就破了。
- **派发（`internal/dispatch/placement.go`，新文件）**：每个 sweep 一次性列出本租户 inventory（不是每条 run 一次查询），从 `runs.plan_json` 的批次目标名反查 `region` 标签，得出该 run 的机房需求；`pickWorker` 只在**有容量且覆盖全部所需机房**的节点里取最闲的。三条判据：① **全部目标不带标签＝不约束**——未使用本能力的部署，放置结果与引入前逐字节相同（有专门用例守住这条）；② **部分带标签/目标行已从 inventory 删除＝拒绝**（`region_unresolved`），不做猜测——半应用的放置正是「演示看着绿、生产跨了边界」的形状；③ **跨两个机房而没有任何节点同时声明两者＝拒绝**（`no_single_node_spans_regions`）；某个机房没人声明＝`no_eligible_node`。**reclaim 走同一判据**——回收是分区机房里的 run 必经的路径，只约束首次分配会在这里漏一个洞（该结论由一条独立变异实测：把 reclaim 改回 `workers[0]` 时**只有**回收那条用例红）。
- **不静默**：`run` 留在 `approved` 等下一次 sweep；新增指标标签 `levee_dispatched_runs_total{result="unplaceable"}`；leader 日志**只在结论变化时**打一行（`reportRefusals` 记住上一次的摘要）——卡住的放置必须可见，但不能每 10 秒刷一次；`levee cluster status` 新增 `Nodes`（各节点声明的机房）与 `Unplaceable Approved Runs`（run / 需求 / 原因）两段，`--json` 同步加 `nodes`/`unplaceable`。`cluster status` 与派发共用同一个分类函数 `Assignability`：**两个面用不同的词解释同一条卡住的 run，是让操作员两边都不信的最快办法**。
- **测试（17 个新用例 + 5 条变异全红在预期位）**：`internal/dispatch/placement_test.go` 10 个——region 亲和力压过负载（那个用例的判别条件是「最闲的节点恰是错的机房」，去掉判据则 leader 赢平局、断言必红）、无人声明该机房、跨机房不可拆、未打标签保持旧行为、半标签拒绝、容量饱和是背压而非放置失败、reclaim 遵守机房、逐 run 而非逐 sweep 求需求、损坏 plan 拒绝、`Assignability` 三种分类；`internal/capability` 5 个（申报期拒非法 region、规范化与排序、只取 region token、不可读声明要报错而不是当空、`Reaches` 全覆盖语义）；`cmd/levee` 2 个（节点机房渲染、原因优先级）。**变异 5 条**：① `pickWorker` 去掉机房判据 → 5 条红（含 reclaim）；② reclaim 回退 `workers[0]` → **只 1 条红**（证明回收路径有独立承重）；③ 跨机房判据恒假 → 只剩专门那条红；④ 去掉申报期校验 → capability 那条红；⑤ 损坏 plan 当空处理 → 对应那条红。每条变异前用 `sha256sum` 备份、还原后逐文件核对与注入前相同。
- **CI 首跑抓到的真缺陷（同一 PR 内修掉）**：`workerLoads` 的输入来自 `registry.List()`——它迭代 map，**顺序不确定**；`sortByLoad` 是稳定插入排序，于是「两节点负载相同」时谁先被选中取决于 map 迭代顺序：同一集群状态每次 sweep 可能把同一 run 放到不同节点，日志与 `cluster status` 不可复现，而「最闲的节点赢」这句断言其实是在断言 Go 的 map 顺序。本机连跑 8 次全绿只是运气，CI 的 windows 腿首跑就红在 `TestDispatchOnce_UnlabelledRunIsUnconstrained`（期望 node-a、实得 node-b）。修法**不是放宽断言**，而是给排序加**按节点 ID 的二级键**（负载仍优先），并补 `TestSortByLoadTiesByID`——夹具故意不是字母序输入。
- **如实边界**：① **一次变更整条交给一个执行节点**，拆分会同时增加在途执行者与审批覆盖面，故跨机房 run 必须有节点同时声明两个机房，否则拒绝——这不是 bug，已写进 `scenario-cross-region-ops.md` §7 诚实边界；② 判据只读 `runs.plan_json` 里的目标主机名，plan 为空的历史行保持旧行为（`Apply` 本来也拒绝执行无 plan 的 run）；③ 机房名的**连通性**不被验证，声明的是「我声称能到达」，网络是否真的通仍由执行期如实报错；④ 本笔不动 `docs/blocked-items.md`（opencode 正持 D-3 行）；⑤ `levee cluster status` 在 `cli-reference.md` 里**一直没有独立章节**，本笔只补了 `serve` 的旗标行与放置语义，章节缺位如实登记、不顺手扩写。

### 修复（`trace --verify` / `diff` / `team list` 的 JSON 空集合返回 `[]` 而非 `null`）

- **缺口**：三处 nil slice 进入 JSON：① `trace <run-id> --verify --json` 的 `verify.failures`（链有效且 0 失败）；② `diff <run-a> <run-b> --json` 的 `diffs`（两个 run 完全一致时 `computeRunDiffs` 返回 nil）；③ `team list --json` 行内 `environments`（team 无具体环境，如 wildcard-only 授权）。
- **修法**：① `buildTraceOutput` 归一化 `[]audit.ChainFailure{}`；② `runDiff` 归一化 `[]diffEntry{}`；③ `runTeamList` 循环内改用 `make([]map[string]any, 0, len(envs))`。
- **验证**：三例各补回归并做失败取证（**修复前分别实测 `"failures": null`、`"diffs": null`、`"environments": null`**）；`go test ./cmd/levee -run 'TestTrace|TestDiff|TestTeam|TestBuildTraceOutput'` 全绿；`go vet ./cmd/levee` rc=0。

### 修复（`group list` / `audit export` / `audit verify` 的 JSON 空集合返回 `[]` 而非 `null`）

- **缺口**：三处 nil slice 直接进入 `data` / `failures`：① `group list --json` 空组（local store 可能返回 nil）；② `audit export <run-id>`（默认 json，run 无 trace；local 与 remote 共用出口）；③ `audit verify --json` 链有效且 0 失败（`VerifyResult.Failures` 为 nil）。
- **修法**：① `runGroupList` 归一化为 `[]*state.InventoryGroup{}`；② `exportTracesJSON` 归一化为 `[]*state.Trace{}`；③ `renderAuditVerify` 归一化为 `[]audit.ChainFailure{}`。
- **验证**：三例各补回归并做失败取证（**修复前分别实测 `"data": null`、`"data": null`、`"failures": null`**）；`go test ./cmd/levee` 定向子集全绿；`go vet ./cmd/levee` rc=0。

### 修复（`target history --json` 空历史返回 `[]` 而非 `null`）

- **缺口**：local 路径在无历史时 `entries` 为 nil slice，`json.Marshal` 输出 `"data": null`；与 #304 确立的「空集合输出为 `[]`」契约不一致，破坏直接遍历 `data` 的消费者。remote 路径已用 `make(...,0,...)`，无需改。
- **修法**：`runLocalTargetHistory` 在 JSON 输出前归一化为 `[]*state.Run{}`。
- **验证**：新增回归用例（fresh store + 不存在的主机名，**修复前实测 `"data": null`**）；`go test ./cmd/levee` 定向子集通过；`go vet ./cmd/levee` rc=0。


### 修复（`levee serve` 真的能发推送：APNs / FCM 凭据接入守护进程，A-3 的第二半）

- **缺口**：`cmd/levee/cmd_serve.go` 里的 `NewMobileApprovalService` 把 push manager **硬编码成 `nil`**，所以守护进程的审批请求**永远**不产生任何通知——哪怕部署已经配好了 APNs / FCM 凭据。`getPushManager`（唯一会构造 PushManager 的地方）只被 `levee push` 的 5 个子命令调用，`serve` 从不读 `<dataDir>/push.json`。台账 `docs/blocked-items.md` 把 A-3 归在「卡在外部凭据/环境（**代码零阻碍**）」节、写「只差凭据」，与代码不符。
- **修法**：
  - 抽出 `newPushRuntimeFromDataDir(dataDir)`：**CLI 与守护进程走同一条路**，凭据与设备注册表不可能各说各话。它返回 manager、公网 base URL，以及 `configured`（是否至少配了一个通道）。
  - `installServeMobileApproval` 是守护进程的接线点：**配了凭据才把 manager 交出去**；没配（或配置损坏）就传 nil，移动审批服务如实记「push disabled; skipping notification」——一个没有通道的 manager 会**报发送成功却什么都没送到**。
  - **推送配置损坏不拖垮控制面**：`push.json` 解析不了、密钥读不了，都只记 ERROR 并关闭推送，守护进程照常启动（推送是尽力而为的旁路，审批本身已经落库；为一个通知密钥拒绝启动等于把通知问题变成一次停机）。
  - `push.json` 新增 `base_url`（公网 origin），`levee push config --base-url` 写入。**web（show）深链**指向它；approve/reject 深链走 `levee://` scheme，不需要它。未配置时沿用原占位符 `https://levee.local`（零行为变更）。
- **验证**：`cmd/levee` 新增 9 例（无凭据→`configured=false` / 有 APNs 凭据→`configured=true` / `base_url` 被读到 / 损坏的 `push.json` 报错 / 无凭据→守护进程不持有 manager / 配了凭据→守护进程拿到 manager / 守护进程与 CLI 共用同一份设备注册表 / 损坏配置**不**拖垮守护进程 / 密钥读不了**不**拖垮守护进程）+ 1 例 e2e（`levee push config --base-url` 落盘且**再次保存凭据不会把它抹掉**）。**变异验证 4 条全红**：① serve 永远不接 manager → 「配了凭据拿到 manager」红；② `configured` 永远 false → 「有凭据」红；③ 损坏的 `push.json` 读成空 → 「损坏报错」红；④ `base_url` 被忽略 → 两条 base_url 用例红。`go build ./...` rc=0；`go test ./internal/push/ ./cmd/levee/` 全绿；`golangci-lint v2.13.2` 0 issues。
- **如实边界**：本笔只交付**守护进程接线 + base_url**。本仓的证据止于「凭据齐备时守护进程**持有**那个 manager」与传输层的本地 HTTP 服务器用例。**（2026-11-08 订正）**：本笔**没有**触发点——`MobileApprovalService.RequestApproval` 当时无生产调用方，审批创建不会发通知；「使用」二字是过度声明，触发接线见上方「审批创建即通知审批人」一节。


### 文档（账本卫生：A-4 的假引用订正、D-1b 残留行移除）

- **缺口**：盘点 `docs/blocked-items.md` 剩余 A–F 项、逐条对引用物时，发现两处账本与仓库不一致。① **A-4** 声称 `scripts/preflight-deploy.sh` 已就绪，但该文件**从未存在**——`git log --all -- scripts/preflight-deploy.sh` 零记录，订正前全仓 `preflight` 唯一命中就是本行自身；真正存在的安装面是 `deploy/baremetal/install.sh`（幂等安装器）、`levee doctor` 与 `docs/deployment.md` §15 交付检查单。② **D-1b** 自带「（已交付）」标记，且「已交付、不再登记」段早已登记 PR #234，却仍留在 D 待办表里被 `check_blocked_items` 计为一行——违反文件头「每完成一项就从本清单移除」的口径。
- **改动**：A-4 的「解锁后动作」改为现存安装面，并在单元格里写明日期与取证方式；D-1b 行移出待办表，D 节加留存说明（登记内容在已交付段与 `CHANGELOG.md` 2026-10-28 段留痕，「强制档」边界由已交付段的 E-3 承接）；文件标题的修订日期追加本笔；`docs/project-status-2026-11.md` 按追加式口径新增 §14（§5.2 / §12 原文不动）。
- **验证**：`check_blocked_items` PASSED（**14** 项 — A=5 B=0 C=3 D=2 E=3 F=1 G=0，rc=0；下限 12 未动）；`scripts/check_blocked_items.py` 的气囊注释按实测重写（原写 16，在 master `829f4ae` 上实测已是 15，本笔后 14）；`check_doc_anchors` / `check_roadmap_status` / `check_release_versions` / `test_changelog_structure` / `python -m unittest discover -s scripts` 全过。零代码行为变更。
- **边界**：只订正「引用的文件不存在 / 已交付行仍计为待办」这两件事；「四 profile 演练」的 profile 清单与安装面是否覆盖该演练**未核**，A–F 的解锁条件一条未变。§5.2 的「唯一未开工的代码项是 D-1c」在本笔之前已过期（D-1c 随 #308 合入），按追加式口径只在 §14 登记、不改写原文。
### 修复（`levee push` 的设备注册表跨进程可见：持久化到 `<dataDir>/push_devices.json`）

- **缺口**：设备注册表只活在 `PushManager` 的内存 map 里。`levee push register` 把设备写进去、命令退出就没了；随后的 `levee push devices` 是**另一个进程**，拿到的是空注册表，于是永远回答「No devices registered」；`levee push send` / `levee push test` 则以 `device not found` 失败。**整个 `levee push` 命令面跨进程不可用**——而 `docs/cli-reference.md` 第 23 章把它当可用特性写着。台账 `docs/blocked-items.md` 把 A-3 归在「卡在外部凭据/环境（**代码零阻碍**）」节、写「只差凭据」，与代码不符：注册表不落地与凭据无关。
- **为什么既有测试没发现**：`cmd/levee/cmd_push_e2e_test.go` 的用例把**进程内**的 manager singleton 跨命令留着（注释明写「the push commands share the in-process device registry」），于是注册看起来是持久的——而生产里每条命令都是独立进程。
- **修法**：新增 `push.DeviceStore` 接口与 `push.FileDeviceStore`（JSON 文件，**原子写**（同目录临时文件 + rename），权限 `0600`；损坏的文件**报错**而不是读成空——读成空会让所有已注册设备看起来凭空消失，正是本条要修的那个症状）。`PushManager` 新增 `NewPushManagerWithStore`：构造时载入已有注册表，`RegisterDevice` / `UnregisterDevice` 成功后落盘；**落盘失败则回滚内存改动**（否则会留下「内存说已注册、文件里没有」的设备——现在会推到、下次重启就没了）。`NewPushManager` 行为不变（进程内注册表）。`levee push` 的 manager 改用 `<dataDir>/push_devices.json`（与 `push.json` 同目录），**不新增操作面**。
- **验证**：`internal/push` 新增 10 例（文件 store 往返 / 缺文件=空 / 损坏文件=报错 / 空文件=空 / 原子写且不留临时文件 / 权限 0600 / 空用户被丢 / Save 覆盖旧内容；manager 跨进程可见 / unregister 也持久 / 落盘失败回滚 / 注册表读不出来时构造报错 / nil store 保持旧行为）；`cmd/levee` 新增 1 例（**复位 manager singleton 模拟进程边界**：register → devices 仍能看到 → unregister → devices 仍看不到）。**变异验证 4 条全红**：① CLI 改回进程内注册表 → 跨进程用例红；② `persistLocked` 变成 no-op → manager 跨进程用例红；③ 损坏文件读成空 → 损坏用例红；④ 去掉落盘失败回滚 → 回滚用例红。`go build ./...` rc=0；`go test ./internal/push/ ./cmd/levee/` 全绿；`golangci-lint v2.13.2` 0 issues。
- **如实边界**：本笔只交付**注册表持久化 + CLI 接线**。**`levee serve` 仍未接推送**——`cmd_serve.go` 的 `NewMobileApprovalService` 硬编码 `pushMgr=nil`，守护进程的审批请求仍不会产生任何通知；深链 base URL 仍硬编码占位符 `https://levee.local`。这两条是 A-3 剩下的代码侧缺口，本笔不含。


### 修复（`diagnose --json` 的空集合序列化为 `[]`：`DiagnosticReport` 自带 `MarshalJSON` 归一化）

- **缺口**：`DiagnosticReport` 的集合字段（`health.network.tcp`、`health.node.disks`、`health.service.services`、`health.data.disks`、`findings`、`recommendations`）都没有 `omitempty`，但 nil slice 会被 `encoding/json` 编码成 `null`。默认 `levee diagnose <host> --json` 就命中：CLI 的探针配置不声明任何服务，`ProbeService` 对空列表提前返回 `ServiceHealth{Status: StatusHealthy}`，`services` 于是是 `null`；`--logs` 模式跳过健康探针时整份 `health` 都是零值，六个集合一起变 `null`。消费者遍历这些字段时要先处理 `null` 与 `[]` 的差异，而「探针跑了、没有条目」与「字段没送达」在文档里无法区分。
- **修法**：`DiagnosticReport` 增加值接收者的 `MarshalJSON`——用别名类型复制报告、把六个空集合归一化为 `[]` 后再编码（别名阻止方法递归）。只改 JSON 编码，不改结构体本身；`json.Unmarshal` 与包内使用不变。`errors` 保持 `omitempty`（键省略是显式设计，未动）。
- **验证**：`internal/diagnosis` 新增 1 例（零值报告 marshal 后六个集合逐一声明为 `[]`；**修复前实测六个全为 `null`**）。CLI 实跑（本机）同一命令修复前 `"tcp": null` / `"services": null`，修复后 `"tcp": []` / `"services": []`。`go test ./internal/diagnosis` 全绿；`go vet ./internal/diagnosis` rc=0；`go build ./...` rc=0。
- **边界**：只归一化 `DiagnosticReport` 的 JSON 编码；`HealthReport` 等嵌套类型被单独 marshal 时行为不变（当前无此消费者）。与 #304/#314/#316/#317/#318 同属「空集合不能是 null」契约族。


### 修复（审批创建即通知审批人：把 A-3 的推送触发点接上）

- **缺口（对上一笔 #320 的订正）**：#320 让 `levee serve` 从 `push.json` 构造出 PushManager 并交进移动审批服务，但**没有任何生产代码会调用发送推送的 `MobileApprovalService.RequestApproval`**——它此前只有测试在调。真正的审批创建路径 `ChangeService.kickoffApproval`（`internal/grpc/change_service.go`）只建审批链 + 记审计；REST 网关只挂了 `/changes/deeplink/approve|reject`。所以配好凭据、登记好手机之后，**新审批依然不会推送到任何人**。#320 的如实边界写「持有并使用那个 manager」是过度声明，一并订正。
- **修法**：
  - `MobileApprovalService` 新增 `NotifyApprovalRequest(ctx, *Approval)`：把 `RequestApproval` 里的推送那段抽成可复用的「通知半边」，**逐个审批人**发一键审批深链（`approve`），逐个失败聚合返回，best-effort 不改变审批结果。`RequestApproval` 改为调它（行为不变：推送失败仍不影响已创建的审批）。
  - serve 用既有的 `ChangeService.WithApprovalCreateObserver` 缝把通知接到**审批创建那一刻**（与 ChatOps / Jira 镜像同一个 observer 扇出，互不顶替）；抽成 `wireServeMobileApprovalPush`，只在**配了通道**时挂（`pushMgr == nil` 直接返回，避免无凭据部署每次审批都记一行「push disabled」）。
- **验证**：`internal/approval` 新增 6 例（nil push / nil approval / 空 run id / 空审批人 均 no-op；三个审批人全无设备 → 聚合错误同时点名 alice/bob/carol，钉住**逐个尝试**而不是首错即停；一个审批人有设备、另一个没有 → 循环不中断）；`cmd/levee` 新增组合守卫（文本）：断言 serve 建了 mobile 服务、调了 `wireServeMobileApprovalPush`、且 helper 里 `WithApprovalCreateObserver`→`NotifyApprovalRequest`→`if pushMgr == nil` 三段都在。`go build`/`go vet` rc=0；`go test ./internal/approval/ ./cmd/levee/ ./internal/grpc/` 全绿；`golangci-lint v2.13.2` 0 issues。**变异 2 条先红后还原**：① serve 的接线调用换成 `_ = servePushMgr`（能编译、永不接线）→ 组合守卫红；② 通知只发第一个审批人 → 扇出用例红。
- **如实边界**：本笔把**触发点**接上；能否真送达仍取决于外部凭据与真机（见 A-3）。本仓证据止于「审批创建 → 每个审批人各调用一次 Send」，不包含真实 APNs/FCM 投递。

### 文档（板约定收口：本机钟会跳变 → `gh-clock` 是唯一锚点；板目录只允许 `README.md` 作非条目）

- **背景**：同伴把双钟约定写成了板目录里的 `board-conventions.md`——它不是条目，校验器把每行当 claim 解，板因此判红；而 `gh-clock` / `clock-source` 已在 #322 入词表，键的语义也已在 `docs/agent-board.md` 的键表里。
- **改动**：把约定写进官方 `docs/agent-board.md`（唯一事实源）：①新增「时间戳：本机钟会跳变，`gh-clock` 才是锚点」；②在「一个 agent 一个文件」里写明 **`README.md` 是板目录里唯一允许的非条目文件**、约定走 `docs/agent-board.md` 而非再开共享文件；③新增「开 PR 前的自检」纪律（板能报重叠、报不了漏声明）。
- **验证**：`go test ./internal/agentboard/`（含 `TestFormatDocMatchesTheParser`，文档键表 ↔ 解析器一致）全绿；`check_doc_anchors` / `test_doc_anchors`（24 例）/ `test_changelog_structure` 全过。
- **如实边界**：本笔只改文档；`gh-clock` 仍**不**作为 TTL 判据（过期仍按 `updated` + `ttl-minutes`），要变判据需要 GitHub 的当前时刻，超出解析器范围。板目录里那个 `board-conventions.md` 的删除归作者（只报不改）。

### 测试（账本断言机器化扩容：A-3 推送 / G-2 / G-5 / D-1c 各配机器可判的锚点）

- **背景**：`scripts/check_doc_anchors.py` 的声明式 `CLAIMS` 表已把「台账说已落地 / 未做」的断言机器化（29 条，下限 25）。本仓头号缺陷类是「一个结论写两遍漏改一半」——台账里的「已就绪 / 已交付」句子会在代码搬走后继续读作现状。
- **改动**：`CLAIMS` 增加 8 条，覆盖最近交付、也最容易被搬走的四组：**A-3**（`NotifyApprovalRequest` / `wireServeMobileApprovalPush` / `newPushRuntimeFromDataDir` / `FileDeviceStore` / `GenerateApprovalLink`）、**G-2**（`ClockReader`）、**G-5**（`RunStatusPrevReader`）、**D-1c**（`ChannelTypes`）；气囊下限 `MIN_CLAIMS` 25 → **33**（ratchet）。
- **验证**：`python scripts/check_doc_anchors.py` ⇒ 28 anchors / **37 claims** 全 hold（rc=0）；`python scripts/test_doc_anchors.py` 24 例 OK。**变异**：把任一新关键字改成不存在的符号 → 该行判红（已验）。
- **如实边界**：`CLAIMS` 仍是**子集**——它证明「关键词还在」，不证明台账那句话的**措辞**仍然准确（`what` 只是失败信息）；扩表是持续的活。

### 安全（依赖：`golang.org/x/net` v0.60.0 → v0.62.0，修 CVE-2026-78661）

- **背景**：2026-11-06 晚起，CI 的 `trivy` 腿（`check (all jobs passed)` 的必需腿）扫出 blocking finding **CVE-2026-78661**（HIGH）：`usr/local/bin/levee` 里的 `golang.org/x/net` v0.60.0（indirect）——`golang.org/x/net/html: Denial of Service via crafted HTML input`，fixed version **0.62.0**。这条腿红的这段时间，**任何** PR 与 master push 都合不进去（与具体改动无关）。
- **改动**：`go get golang.org/x/net@v0.62.0` + `go mod tidy`，只动 `go.mod` 一行与 `go.sum` 两处。其余 `golang.org/x/*` 版本不变。
- **验证**：`go build ./...` rc=0；`go vet ./...` rc=0；`internal/state` / `internal/auth` / `internal/notify` / `internal/chatops` 全绿；trivy 复扫以本 PR 的 CI 为准。

### 新增（运行期通道插件「一插件多类型」：manifest 声明 `channel_types`，D-1c）

- **缺口**：一个运行期 channel 插件只能提供**一种**传输类型，且类型名就是插件名——同一个二进制要服务几个 target 词汇（如一个 TCP 控制台桥同时是设备控制台和终端服务器）时，只能把它按不同名字装多份，注册表行、config、enable/disable 都跟着翻倍，运行时毫无好处。这正是 `docs/blocked-items.md` 的 **D-1c**（也是项目现状里“唯一未开工的代码项”）。
- **修法**：`PluginMeta` 新增可选 `channel_types: [别名…]`。`serve` 启动时把一个插件工厂按**插件名 + 每个声明类型**全部注册进 `channel.DefaultRegistry()`（共享同一个插件进程与会话池）；任一类型与内建传输（ssh/winrm/local…）或别的插件重名，则在**注册之前整体拒绝**——先查全部再注册，不留半装。`PluginMeta.Name` 仍无条件注册，所以**空声明＝D-1c 之前的“一插件一类型”行为**，既有 manifest 与旧注册表行不受影响。插件从 `Target.Type()` 拿到被声明的那个类型，需要按类型分派时据此分支。
- **持久化**：`plugin_registry` 新增 `channel_types TEXT NOT NULL DEFAULT '[]'`（与 `destructive_actions`/`reversible_actions` 同一套幂等 `ALTER TABLE` 迁移）；`Registry.Install`/`Get`/`List` 全链携带。坏列解析为空 → 退回“只注册插件名”。校验（仅限 channel 型、`^[a-z][a-z0-9-]*$`、不重复、不重复插件名）在 manifest 载入与 `Registry.Install` 两处，直接调 Registry 也绕不过。
- **验证**：`registry` 新增 4 例（往返 / 空声明为空 / `validateChannelTypes` 六表 / 非 channel 型被拒）、`serve` 新增 2 例（多类型都注册到同一工厂 / 别名撞内建则整体拒绝且插件名也不注册）、`manager` 新增 4 例（manifest 载入路径：非 channel 型 / 别名重名 / 非法 token 被拒 + 合法声明一路带到 registry 行），并修正既有的撞名断言与 `pluginMetaFor` 测试夹具。`go build` + `go test ./internal/plugin/ ./cmd/levee/`（定向）全绿；`golangci-lint v2.13.2` 0 issues。示例 `examples/plugins/tcp_console/plugin.yaml` 与 `docs/channel-plugins.md` §7/§8 同步。
- **变异验证（7 条，全红）**：① serve 不注册别名 → 2 例红；② serve 先注册插件名再查冲突 → 「自己的名字不能被注册」红（**这条断言原先是空转的**：撞名用例没设 `serveModuleChildEnv`，工厂根本建不起来，于是断言永远不可能触发——已补上 child persona 让它真承重）；③ `validateChannelTypes` 整个短路 → 2 例红；④ registry 落库丢掉 `channel_types` → 往返例红；⑤ 迁移列表去掉 `channel_types` → 旧表迁移守卫红（`no such column: channel_types`）；⑥ 去掉 manifest 载入处的校验 → 3 例红（**注**：一开始这三条走 `Install()` 写，registry 那道校验会兜底，变异不红——改为直接调 `loadManifest` 才真正隔离 manifest 这一层）。
- **依赖**：本笔 rebase 在 `golang.org/x/net` v0.62.0（CVE-2026-78661 修复）之上；不引入新依赖。
- **如实边界**：只扩了**运行期**通道插件（D-1c 的原文范围）；编译期通道插件不受影响；类型别名只是一个“同一二进制可被多个名字解析到”的声明，不改变插件协议或沙箱，也不做类型→行为的自动分派（那由插件读 `Target.Type()` 自己决定）。


### 测试（wire 契约夹具补齐 5 个前端实调端点：裸数组有了 `items` 形状，DTO 对账也覆盖它们）

- **缺口**：`internal/grpc/wire_snapshot_test.go` 只驱动 10 条路径，而控制台实际调用 `/agents`、`/groups`、`/audit/traces`、`/system/version`、`/conversation/sessions` 时没有任何形状记录——`web/src/api/wireContract.spec.ts` 的两向守卫对它们全盲，`HostGroup`、`AgentRecord`、`TraceEntry`、`VersionInfo`、`ConversationSessionDTO` 五个 DTO 的拼写从未被服务端产物验证过。历史上三次 PascalCase 泄漏（ClusterNode / AssignmentSummary 同型）都是从这种夹具盲区钻过来的。
- **改动**：夹具播种补齐（一条 agent 记录、父子两个 group、两条带 Detail 的 trace、一个会话）。`/agents` 与 `/groups` 返回**裸数组**，快照新增 `items`（元素键集合），并让它和 `keys`/`rows` 一样进入逐键对账与 PascalCase 防线；`/audit/traces` 用 `pageSize=1` 对两条 trace 逼出 `nextPageToken`（protojson 会省略空 token，而控制台的 `toPageResult` 要读它）。TS 侧新增 `rootDto` 与 5 个 `DTO_READS` 条目；裸数组的读取集直接从接口声明推导，不再维护第二份手写名单。
- **验证**：`UPDATE_WIRE_CONTRACT=1` 重生成快照后，快照断言与 PascalCase 扫描双绿，TS 6 例全绿。**红样本四条（接管后独立复现，不采信上一轮的自述）**：① 把 `conversation-sessions` 的 forbid 列表加上 `UserId`（它确实在线上）→ Go 侧 `JSONEq` 红并点名 `UserId`；② 往 `agents.items` 塞 `Id` → TS 的 items PascalCase 断言红（`agents: item key Id is PascalCase`），且同时触发「declared nowhere」；③ 把快照里的 `user_id` 改成 `userId` → TS **三个方向同时红**（客户端读了线上没有的键 / 线上有键没人声明 / 接口声明了线上没发的键）；④ 把**服务端** `sessToDTO` 的 `user_id` 改成 `userId` → Go 快照红并给出「the wire shape changed: re-run with UPDATE_WIRE_CONTRACT=1」。四条均已还原，`gofmt` 干净、快照与提交版逐字节一致。另外第一次跑 TS 时它确实红在 `agents: item key address is declared nowhere`，证明裸数组元素的逐键对账不是装饰。
- **边界**：本笔**只加夹具、不改服务端行为**——五个端点的键集是**记录**下来的既有产物，不是新造的形状。`/audit/traces` 的 `input`/`output`/`detail` 由 trace 的 Detail JSON 解码而来，夹具已播种真实形状；Detail 为空的行仍会省略这些键（protojson 语义），夹具不替它伪造。`docs/blocked-items.md` 的 G-4 行在本笔中从待办表**移出**、进「已交付、不再登记」节（沿用 #307 对 G-1 / G-3 的同一写法）；G 类因此空了，按 `check_blocked_items` 的口径显式写「当前无」——空分类必须说明是空的，否则「少了表」会静默通过。


### 决策（`blocked-items.md` 的 G-1 / G-3 / D-3 定档：均为「不做行为变更」，只落账与注释）

- **原 G-3 — `plan.Plan` 持久化无 json tag → 定档「维持现状」**：`runs.plan_json` 是 Go **内部**工件，写入是 `internal/wiring/plan.go` 的 `json.Marshal(p)`、读取方全部 `json.Unmarshal` 回同一 struct，且 `plan_hash` 走独立的 canonical 结构，所以「混合拼写」当前无损。原触发条件「有非 Go 消费方」未到；补 tag 会改变已落库字节，在滚动升级期间让新旧进程**互读不了**，换来只是一个不存在的消费方。故**不改格式**，只在写入点加注释（同一条也提醒：将来真要做，是加 tag **并**自定义 `UnmarshalJSON` 兼容旧 PascalCase，不是裸加 tag）。
- **原 G-1 — audit `BuildForce` / `Rebuild` 无生产调用方 → 定档「不提供人工修复入口」**：链对合法写的断裂由下一次 `Seal` 自愈，对内容篡改由 `Seal` / `Verify` 报错而不静默重链；`Rebuild` / `BuildForce` 保留作内部 / 测试与确认篡改后的恢复原语，**不新增 `levee audit verify --rebuild`**（不新增动 WORM 邻域的操作面）。决定写进 `internal/audit/auditchain.go`（`Rebuild`）与 `hashchain.go`（`BuildForce`）的注释。
- **D-3 — 账本时刻方言归一（SQLite=UTC / PostgreSQL=会话时区）→ 定档「O3 暂不改，触发后预定 O1」**：当前未收到真实 PostgreSQL 部署报障，且 `--remote` 等价测试仅覆盖 SQLite；无真实证据下动存储/展示口径易引入第二形态。决策：维持触发式；触发后默认实施 **O1**（store 解码层统一归一为 UTC，账本 UTC 存/读、展示层按需转换），**O2**（CLI `--tz` 展示旗标）仅作备选。触发条件与实施前证据要求（SQLite+PostgreSQL 双方言下重跑 `audit list/export/show`、`target list/history`、`group list` 的本地与 `--remote` 等价、发布说明）记入 `docs/blocked-items.md` D-3。**零行为变更**（纯账本）。
- **验证**：两项都是**零行为变更**——G-3 不动序列化，G-1 不新增命令；改动为 `docs/blocked-items.md` + 三处代码注释。`go build ./internal/wiring/ ./internal/audit/` rc=0；`check_blocked_items`（19 → 17，G 表只剩 G-4）/ `check_doc_anchors` / `test_changelog_structure` / `check_roadmap_status` 全过。


### 修复（dispatch 的 assignment 陈旧度改按 store 时钟比较，消除库/应用时钟差对 reclaim 判定的平移）

- **缺陷**：`run_assignment.updated_at` 由**后端时钟**盖章——PostgreSQL 的 `CreateAssignment` / `UpdateAssignmentState*` / `ReclaimAssignment` / `SetAssignmentResult` 写的是 `NOW()`；而 `dispatch.reclaimStale` 算 `age` 用的是**进程时钟** `time.Now().UTC()`。集群里库主机与应用主机时钟一旦有偏差，同一个 `age` 就被整体平移：一端会把还在被认领的 assignment 当陈旧抢走（`epoch` fencing 兜住迟到的 owner，不至于双执行），另一端永不复现的真孤儿则不回收。**同机测试环境永不复现**，只在异构部署显形。这是 `docs/blocked-items.md` 的 **G-2**。
- **修法**：新增可选接口 `state.ClockReader`（`StoreNow(ctx) (time.Time, error)`）——**PostgreSQL** 返回服务器 `SELECT NOW()`，**SQLite** 返回本机时钟（它与数据库同机，本就是同一时钟）。`dispatch.reclaimStale` 用它作为比较用的 `now`（经 `state.Underlying` 穿透 `TenantStore`/`RunEventStore` 装饰器找到底层 store）；store 没有这个能力（测试替身）或读取失败时回退到进程时钟并 `WARN`，reclaim 不会因读时钟失败而停摆。
- **历史行不回填**：不需要。PostgreSQL 的历史行本来就是库时钟落的，改用库时钟比较反而与它们一致；SQLite 的库时钟即本机时钟。所以没有一次性迁移，只把“比较用的时钟”对齐到“盖章用的时钟”。
- **验证**：新增 3 例——`state` 的 SQLite / PostgreSQL `StoreNow`（后者在真容器 `postgres:16-alpine` 上跑过），`dispatch` 的 **时钟分离**（1 分钟旧的 assignment 在进程时钟下不陈旧、在 store 时钟`+30min` 下陈旧且被 reclaim；**变异**：把 `l.storeNow(ctx)` 改回 `time.Now().UTC()` → 该用例红，还原后绿）。既有 `internal/dispatch` 全包与 `internal/state`（含 PG）全绿；`golangci-lint v2.13.2` 0 issues。
- **如实边界**：只改了 **staleness 比较**的时钟；fresh claim 路径传给 `CreateAssignment` 的 `AssignedAt/UpdatedAt` 未动（PostgreSQL 那边本就忽略它们、一律 `NOW()`；SQLite 那边本机时钟即库时钟）。`state.Store` 接口未加宽，只有 SQLite / PostgreSQL 两个实现提供该能力。


### 修复（`RunEventStore.UpdateRun` 的读-改-写竞态：并发全行写会把错的 `from` 播给 ChatOps/webhook）

- **缺陷**：`internal/wiring/runeventstore.go` 的 `UpdateRun` 拦截全行写（pause/resume、archive 这些路径带状态变化），做法是「先 `GetRun` 读旧 status → `UpdateRun` 写 → 用读到的旧 status 当 `from` 播事件」。读与写是两条语句：两个并发的全行写交错时，后完成者的 `from` 可以是它**从未覆盖过**的状态——run 实际走了 `running → paused → resumed`，而事件里会出现一条 `running → resumed`（ChatOps 卡片与 webhook 全量镜像直接照抄这个 `from`）。测试只在单线程跑，所以一直绿。这正是 `docs/blocked-items.md` 的 **G-5**。
- **修法（store 层原子化，不是再补一条 CAS）**：新增**可选**接口 `state.RunStatusPrevReader`，方法 `UpdateRunReturningPrevStatus(ctx, run) (prevStatus string, err error)` 在**同一步**里写全行并回报被覆盖的旧 status：
  - **SQLite**：在 `db.Conn` 上单连接 `BEGIN IMMEDIATE` → `SELECT status` → `UPDATE` → `COMMIT`。刻意用 IMMEDIATE 而非默认 deferred：deferred 在共享快照上读完再写会被并发写者逼成 `SQLITE_BUSY_SNAPSHOT`（`busy_timeout` 也重试不掉），IMMEDIATE 让并发写者**排队**而不是报错。
  - **PostgreSQL**：`BEGIN` → `SELECT status … FOR UPDATE`（行锁，跨集群节点也成立）→ `UPDATE` → `COMMIT`。
  - `RunEventStore.UpdateRun` **优先**走它；store 没这个能力（测试替身、非 SQL 后端）时保留原来的读-再-写回退，所以 `state.Store` 接口不被加宽。
- **验证**：新增 5 例——SQLite 的顺序 / 未找到 / nil + **并发链**（24 个 goroutine 对同一行做读-改-写，断言被覆盖的 status 串成一条不丢不重的链）；PostgreSQL 一例（真容器 `postgres:16-alpine` 上跑过）；wiring 两例钉住「有能力时走原子路径、无能力时回退」。既有 `RunEventStore` 用例全绿；`golangci-lint v2.13.2` 0 issues；`internal/state` 全包（含 PG）与 `internal/wiring` / `internal/pause` / `internal/takeover` 全绿。
- **如实边界**：只保证**经该 decorator** 的全行写的 `from` 正确；未把全部全行写改造成 CAS（那要动十几个调用点，风险远大于收益）。`state.Store` 接口未变，只有 SQLite / PostgreSQL 两个实现提供原子路径。


### 文档（`CONTRIBUTING.md` §5.1 的 `ci.yml` check-run 计数订正：`24` → 实测 `26`）

- **缺口**：§5.1 讲「stacked PR 的 base 不在 `main`/`master`/`develop` 时 `ci.yml` 一条 check 都不生成」时，引用的计数是 **24 个 check run**——那是更早的 job 集留在文本里的旧数。上一笔（`e0b3353`）自己留了话：「`ci.yml` 的 **24 个** check run 这个计数本笔没核实（顶层 job 实测 16 个），登记给下一次专门核计数的人」。
- **实测（2026-11-06）**：读 PR `#300`（head `89c120ef`）的 check-runs API，该 PR 共 **30 条** check run = `ci.yml` **26** + `codeql.yml` 2（`CodeQL` / `analyze (go)`）+ 第三方 2（`Macroscope - Correctness Check` skipped、`Trivy`）。`ci.yml` 的 26 = **16 个顶层 job**（13 个单实例 + `test`×3 + `build`×6 + `browser-check`×4）。
- **改动**：只订正 §5.1 那一句，把 `24` 换成 `26` 并写出 16→26 的矩阵展开与实测出处，另把「只有 CodeQL 两条与第三方检查」补成能对上的口径（`codeql.yml` 两条 + 第三方）。**不改 CHANGELOG 里已发布小节的同形旧数**（那是当时的事实快照，按本仓「发布说明不重写」的口径保留）。
- **验证**：`python scripts/test_changelog_structure.py`、`python scripts/check_doc_anchors.py`、`python scripts/test_doc_anchors.py` 全过；纯文档改动，不动 Go/前端，故不跑构建与测试。
- **如实边界**：这个数是**拼写说明、不是门禁**——没有脚本从 `ci.yml` 反算它（矩阵形态多样，反算器一旦漏看一处就静默少数；宁可不设这道门）。所以 `ci.yml` 增删 job 时仍要靠人同步，文本里已写清怎么重算。


### 测试（`blocked-items.md` 结构与分类计数门禁：每类几项从文件本身算，不再只有快照）

- **缺口**：`docs/blocked-items.md` 把搁置项分成 A–F 六类，「每类有几项」只出现在 `docs/project-status-2026-11.md` 的**快照**里（§12 写 `A5 / B0 / C3 / D3 / E3 / F1`）——没有任何门禁从文件本身算，所以快照会悄悄过期；分类结构（项归错类、ID 重复、某类被清空）也无人守。
- **实测当下就是过期的**：本门禁算出 **16 项 — A=5 B=0 C=3 D=4 E=3 F=1**，而 §12 快照写的是 `D3`——**D 类现在有 4 行**。
- **改动**：新增 `scripts/check_blocked_items.py`，从文件解析 A–F 六节，打印每类计数，并校验四条不变量：① 每行 ID 的前缀必须等于其所在节的字母（`C-9` 落在 D 类 → 红）；② 全文件 ID 不重复；③ 每节要么有表、要么显式写「当前无」（少了表变静默 → 红）；④ 行数下限（12/16）。`scripts/test_blocked_items.py` 11 例；`ci.yml` 的 release-gate 点名表加 `test_blocked_items`。
- **验证**：`check_blocked_items` PASSED（16 项）；本套件 11 例全绿；`check_roadmap_status`、`check_doc_anchors`、`test_changelog_structure`、`unittest discover -s scripts` 全过。**变异三条**：① 一行 ID 改成 `Z-1` → 红并点名「letter must match its category」；② 复制 `C-1` → 红并指出两处行号；③ 去掉 B 节的「当前无」注 → 红并说「must be either populated or explicitly empty」。
- **如实边界**：本门禁只校验**结构与计数**，不判「活跃 vs 已交付」——D-1b 带「（已交付）」标记仍算一行。要分开那两类需要给 blocked-items 也加状态列，是另一件事。

### 修复（`RollbackChange` 的 `dry_run` 从未被兑现：控制台的回滚「预览」会真的执行回滚）

- **缺陷**：proto 的 `RollbackRequest.dry_run` 写着「只模拟回滚并报告将要发生什么」，REST 层也老老实实把 `dryRun` 解出来传进了请求——但 `ChangeService.RollbackChange` **从头到尾没有读过 `req.GetDryRun()`**。于是控制台「部分回滚补救」向导（`web/src/composables/useRollbackRemediation.ts`）那一步 `changesApi.rollback(id, { dryRun: true })` 拿到的不是预览，而是**一次真的回滚**：补偿派发到生产主机、run 状态被改写成 `rolled_back`、审计链留下一条 `rollback` 记录。操作员随后看到的对话框把**刚刚已经执行掉的主机**列成「待补偿」，而真正点「现在补」的那一次又因为状态已变成 `rolled_back`（不在 `runstatus.RollbackAdmitted` 里）被准入检查拒绝。**净效果：破坏性动作在操作员确认之前就发生了，之后还报一个他读不懂的错。**
- **为什么能发布出去**：这个向导是**已交付**功能——`docs/product-roadmap.md` 的 P0 条目写着「先 `dry_run` 摊证据（待补偿/将跳过/服务端消息）再问『现在补吗』」，`docs/release-notes/v1.22.0.md` 也记着同一条。证据只到过 vitest（对着 mock 的 API）与人工点击，没有一条端到端路径把「预览」和「真的执行」分开：**修复前** `web/scripts/mock-gateway.mjs` **根本没有 rollback 端点**，所以浏览器级检查（`browser-check.mjs`）覆盖不到这条流；后端则**连一个 `dry_run` 的测试都没有**。
- **同类的另外两个洞（一起收口，否则下一次还会漏）**：proto 里共 5 个 `dry_run` 字段——兑现 2 个（`PlanChange`、`InstantiateTemplate`）、**空转但契约成立** 1 个（`CreateChange`：它只建 draft 等审批，本来就不应用，所以「只计划并返回、不应用」这条契约无论如何都成立）、**被忽略** 2 个（`ApplyChange`：proto 写着「只打印计划并退出」，REST `/changes/:id/apply` 又把 `dryRun` 透传进去，于是「我要预览」得到的是一次**真的 apply**；`RollbackChange`：本文的主角）。忽略一个声明过的字段，比没有这个字段更糟：调用方会以为自己在预览。
- **修法（三层，各自有闸）**：
  - **回滚管理器自己知道它在预览**：`internal/rollback` 新增 `WithDryRun` 与 `StepRollbackResult.DryRun`。补偿步骤照走（这才算得出「会动哪些」），但**快照恢复被拒绝**——`strategy: "snapshot"` 的步骤会把采集到的前状态写回目标机，是回滚能做的最破坏性动作，而回调明明是接好的；`execFn == nil` 的既有分支（manager 早就写着「useful for dry-run invocations」）补上 `DryRun` 标记，让「本来会跑」与「因证据被跳过」这两件事在结果里分得开。
  - **接缝变成一个带参数的闭合**：`EngineAdapter.Rollback` 加 `dryRun bool`，返回 `RollbackOutcome{RunID, RolledBackHosts, SkippedHosts, Gaps}`。预览与提交**共用同一段实现**（`wiring.Engine.rollbackChange`）：一个可能与提交对不上的预览比没有预览更糟。
  - **服务层兑现契约**：`RollbackChange` 在 `dry_run` 时**什么都不落盘**（不改 run 状态、不写审计、不发事件、不铸 run id），原样回报两个主机列表与一条说明三类主机的 message；`skipped_hosts` 这个 proto 里早就存在、却**从未被赋值**的字段现在第一次有值。`ApplyChange` 则**明确拒绝** `dry_run=true`（`FailedPrecondition`，并点名「要预览请用 `PlanChange`，它的 `dry_run` 是兑现的」）——宁可拒绝，也不能在操作员说「预览」的时候执行。
- **三类主机分开报**：待补偿（会动）、将跳过（前向没跑 / 已被补偿，不需要）、**缺口**（有步骤没有可用补偿动作：没声明补偿、被策略拒绝、undo 无法安全重排）。第三类**不是**「将跳过」——它是需要人的洞，所以单列并在 message 里点名，而不是塞进那两个列表之一。**「待补偿」与「缺口」可以重叠**：一台主机的某个步骤有可用补偿、另一个步骤没有补偿动作时，回滚**会动它**、同时**也补不全它**——两个事实操作员都要知道，所以两栏都列；「将跳过」则是独占的（一台有缺口的主机绝不会被说成「不需要处理」，那正好是反话）。
- **验证（Go 侧）**：新增 **13 例**（`internal/rollback/manager_dryrun_test.go` 3、`internal/wiring/rollback_dryrun_test.go` 5、`internal/grpc/rollback_dryrun_test.go` 5）。wiring 层用真的通道记录器断言「预览一条 undo 命令都没派发」「预览没把基线写回目标机」「已补偿的主机落在 skipped 而不是待补偿」「既有可补偿步骤又有无补偿步骤的主机**同时**落在待补偿与缺口两栏」；grpc 层断言「flag 确实传到了引擎」「run 状态与审计链零变化」「真实回滚仍然执行并留证」「无引擎时预览不把 run 标成 `rolled_back`」「`ApplyChange` 拒绝 `dry_run` 且不派发」，外加一条**线协议级**的断言：驱动真的 REST handler，把 JSON 解析出来，钉住 `rolledBackHosts` / `skippedHosts` 两个键**存在且拼写正确**（且预览里没有 `rollbackRunId`）——已提交的 wire 契约快照只覆盖 GET 端点，而 rollback 是 POST，所以这两个键此前**没有任何一处被机器校验过**；`skippedHosts` 更是一个服务端从未发过的键。
- **验证（浏览器级，补上这条缺陷得以发布的根因）**：`web/scripts/mock-gateway.mjs` 新增 rollback 端点，**把预览与提交分成两次不同的调用**并记录调用日志（`GET /__rollback-log`）；`web/scripts/browser-check.mjs` 新增场景 **[8]**：在一条 `rolled_back_partial` 的变更上点「补救未完成的回滚」，断言对话框点名了待补偿与将跳过的主机、**预览没有提交**、取消也不提交、确认后**恰好提交一次**且提交发生在预览之后。没有这一步，mock 根本没有 rollback 端点，浏览器检查永远覆盖不到这条流。
- **变异（证明新检查有牙，不是装饰）**：① 把客户端的 `dryRun: true` 去掉（模拟「预览即提交」）→ 场景 [8] **5 条断言变红**（`calls=["commit"]`）；② 把服务端的 `SkippedHosts` 赋值去掉 → **2 条 Go 测试变红**（`expected ["10.0.1.12"], actual nil`）。两处都已还原，`gofmt` 干净。
- **同类洞的全文复核（CONTRIBUTING §5.2：一条声称被推翻时，它的每一处都要重新审）**：本笔使「部分回滚补救已先 `dry_run` 摊证据再确认」这条声称**从假变真**——凡是写着我这条结论的地方，全部重新审过，共 4 处命中：① `docs/product-roadmap.md:84`（活账本 P0 行）——现在标注了「2026-10-10 落地时**只有 UI 那一半**」，否则读者会以为那天起预览就是安全的；② `docs/release-notes/v1.22.0.md:25`（v1.22.0 发布说明）——按仓里既有的勘误写法（`v1.20.0.md:48` 的「更正一处」）加了一段 `> 更正一处（2026-11-06 补）`；③ `CHANGELOG.md` 的 v1.22.0 小节摘要（按 v1.23.0 小节「订正（账本对账）」的写法）加了同一条订正；④ `docs/leveelang-spec.md:1324`（V16 补偿声明，LE091 已撤回、状态为「规划」）——说的是**编译期不阻断**与 plan dry-run 的告警，与本笔的 `RollbackChange.dry_run` 不是同一条路，**不改**。另两处是描述而非声称，不动：`docs/ui-blueprint.md:181`（设计蓝图里的补救闭环）与 `docs/product-positioning.md:47`（角色定位「接管 partial rollback 的补救」）。**发布说明不应声称 tag 里没有的行为**——v1.22.0 发布当时服务端没有这个能力，说明却写着有，这三处订正就是补这笔账。
- **如实边界**：**proto 一行未改**（`dry_run` 与 `skipped_hosts` 字段本来就存在，这笔是让实现追上契约，不是加字段），所以线协议、生成代码与 `proto regenerate check` 都不受影响；**前端业务代码一行未改**——它本来就是照着正确的契约写的，是后端违约；本笔改的前端文件只有测试夹具（`web/scripts/mock-gateway.mjs`、`web/scripts/browser-check.mjs`）。**当时没做的两件，后一笔补齐**（2026-11-07，见下面「`levee rollback` 从不回滚」那条）：本笔当时只把预览留在控制台暴露，**CLI 没有 `rollback --dry-run`**；已提交的 wire 契约快照**只覆盖 GET 端点**，rollback 是 POST，故本笔改用上面那条线协议级 Go 测试达到同一目的。

### 修复（`levee rollback` 从来没有回滚过：它只把 run 写进一个没人消费的状态；顺带补上 `rollback --dry-run` 与 POST 的 wire 契约）

- **缺陷**：`cmd/levee/cmd_rollback.go` 的 `levee rollback <run-id>` **一次补偿都没有派发过**。它做的事是：把 run 状态改写成 `rolling_back`、写一条审计行、打印「rollback triggered / Post-rollback verification: pending」——然后退出。快照一步没恢复，post-rollback 验证一次没跑，回滚步骤一步没执行。
- **`rolling_back` 是一个没人消费的状态**（全仓按 `.go/.ts/.vue/.yaml/.md/.proto` 逐文件搜过）：除 CLI 自己与它的测试外，只有 `docs/cli-reference.md` 一处提及；它**不在** `internal/runstatus` 的词表里（`All` 15 个状态、`Terminal` 9 个状态里都没有它），前端生成词表 `web/src/types/vocabulary.ts` 也没有；调度器 `internal/dispatch/dispatch.go:311` 只捡 `approved` 的 run，没有任何 worker 或 reaper 扫 `rolling_back`。
- **后果（它能潜伏这么久的原因）**：run 被卡进一个**任何命令都出不去**的状态——`levee logs <run-id>` 跟随到终态为止（`cmd/levee/cmd_logs.go:134` 判 `isTerminalStatus`），而 `rolling_back` 不是终态，所以操作员回滚之后最自然的下一个动作「看日志」会**一直等下去**；`levee cancel` 只放行 pending/draft，`levee retry` 与 `levee apply` 也不放行它，服务端的 `runstatus.RollbackAdmitted` 更不含它。**一次回滚请求，代价是一条永久卡住的变更。**
- **文档早就写着它做不到的事**：`docs/cli-reference.md` 与 `docs/levee-api.md` 都写着「回滚按执行 batch 的逆序执行 rollback 步骤」「回滚完成后运行 post-rollback 验证确认目标状态健康」——CLI 一行都没做；`docs/levee-api.md` 的选项表还列着 `--batch <n>` 与 `--force` 两个**二进制里从未注册**的 flag（`cmd/levee/cmd_rollback.go` 里没有，`git log -S` 显示自初始提交起就没有）。
- **修法（一条路径，不做两套）**：`levee rollback` 改走服务端同一套 `ChangeService.RollbackChange`（经 `cmd/levee/engine_cli.go` 的引擎桥 `newCLIChangeService`），于是准入检查、风险门禁、补偿派发、终态与审计行与控制台完全一致：按执行证据逆序 batch、逆序步骤派发补偿，结论写进 `rolled_back` / `rolled_back_partial` / `rollback_incomplete`，审计行由服务层写（带 WORM 哈希链，actor 是 CLI 用户）。
- **`--dry-run` 预览**：CLI 现在有 `levee rollback <id> --dry-run`，与控制台**同一个** `dry_run` 语义——只报告「会补偿哪些主机、会跳过哪些主机」，不补偿主机、不改写状态、不写审计行、不恢复快照。
- **被旧 CLI 卡住的存量 run 有了出路**：`rolling_back` **不能**加进 `runstatus.All`（前端词表是从 `All` 生成的，而 `pending_approval` 那个坑正是「后端从不产生的状态出现在词表里」，结果是筛选永远空、无处报错）。它被登记成 `runstatus.StatusRollingBackLegacy`——只作为**输入**被识别、明确不进 `All`；CLI 遇到这样的 run 会先把它标成 `rollback_incomplete`（「请求过回滚、一次补偿都没完成」——这是对它真实现状的陈述，而不是编造），再真正执行回滚。
- **`--force` 移除**：旧 `--force` 只绕过 CLI 自己那份状态检查，而放行后的唯一结局就是把 run 写进 `rolling_back`。服务端的准入检查**不可绕过**（回滚一个从未执行、或仍在飞行中的 run 不是回滚，是另一件事），这个 flag 没有对应的服务端语义；保留它只会是一个装饰性的假承诺，所以拿掉并写进 CHANGELOG。
- **wire 契约快照纳入 POST**（上一笔列在「没做的」里，本笔做掉）：`web/src/api/__contract__/wire.json` 新增 `rollback` 条目（`method: POST`、`path: /changes/{id}/rollback`），键集取**预览与提交两个响应的并集**——protojson 省略未设置字段，所以预览没有 `rollbackRunId`、提交有；并集就是「浏览器能从这个端点读到什么」。TS 侧把 `web/src/api/index.ts` 里内联的返回类型提成具名接口 `RollbackResponseDTO`，并进 `web/src/api/wireContract.spec.ts` 的对账表（读集、`SERVER_ONLY`、接口声明逐键对账三处都过）。**预览自己的子集**由 `internal/grpc/rollback_dryrun_test.go` 的线协议用例钉住。
- **验证（CLI）**：`cmd/levee/cmd_rollback_test.go` 重写为 **13 例端到端**（此前该命令**一条端到端测试都没有**，全是拼字符串的断言——这正是它能潜伏的原因）：预览不改状态、不写审计行、flag 确实传到引擎；真回滚把状态改写成 `rolled_back`、留下唯一一条带 CLI actor 的审计行；不在准入集内的状态被服务端拒绝（exit 4）且 run 不动；被旧 CLI 卡住的 run 被释放并真正回滚；**预览不做那次修复**（预览不落盘）；**预览的措辞不能说成「已补偿」**（预览输出写「Would compensate hosts」并注明状态「unchanged by the preview」——把没做的事说成做了，与文档承诺一场没跑的回滚是同一类毛病）；JSON 信封带两个主机列表（空列表是 `[]` 不是 `null`——消费方不必区分「没有主机」与「这个字段不存在」）；退出码映射（4 状态 / 3 权限 / 1 其他）。为此给 CLI 的引擎桥留了一个测试接缝 `cliChangeServiceFactory`（生产恒为 `newCLIChangeService`），`levee apply --engine-enabled` 也走它。
- **验证（wire 契约）**：`internal/grpc/wire_snapshot_test.go` 新增 `rollback` 条目与一个驱动真 REST handler 的夹具（预览 + 提交两次调用，引擎用 stub，所以不会有主机被碰）；`TestWireSnapshot_NoPascalCaseLeaks` 的行走也覆盖了 rollback 的两个响应。
- **变异（证明新检查有牙）**：① 把 CLI 的 `DryRun: rollbackOptDryRun` 改成恒 `false` → 预览用例变红（run 状态被改写）；② 去掉存量 run 的修复分支 → 「被旧 CLI 卡住」用例变红（状态仍是 `rolling_back`，服务端的准入检查直接拒绝）；③ 从 wire 快照里删掉 `rollback` 条目 → Go 侧 `JSONEq` 变红；④ 把 TS 侧的 `RollbackResponseDTO` 字段拼错 → `wireContract.spec.ts` 变红；⑤ 去掉 JSON 信封里主机列表的 `[]` 归一化 → 「空列表是数组」用例变红（`expected "[]", actual "null"`）。
- **同类声称的全文复核**（CONTRIBUTING §5.2）：本笔推翻了「CLI 的 rollback 会执行回滚」「CLI 的 rollback 会跑 post-rollback 验证」「CLI 的 rollback 有 `--force`/`--batch` 选项」「回滚后 run 状态是 `rolling_back`」这四条声称，逐处重审后：`docs/cli-reference.md` §3.10 重写；`docs/levee-api.md` §2.8 重写并加订正块（`--batch`/`--force` 从未存在、post-rollback 验证不成立）；`docs/mvp-tasks.md` T074 行加 2026-11-07 复核订正；`docs/review-report.md` 里「levee rollback | 2.8 | 一致」那行加订正（当时判「一致」，其实两端都不符）。**未改**：`docs/leveelang-spec.md:1239`（手动回滚路径与闭包路径共用快照 store、同样恢复一次 run 基线——这条**成立**，回滚管理器确实是逆序 batch、逆序步骤）；`docs/release-notes/v1.3.0.md` 的命令存在性记载（命令确实存在）；`docs/levee-api.md` 附录 A 的命令速查（回滚确实覆盖该 run 的全部批次）。
- **如实边界**：**proto 一行未改**（CLI 走的是已有的 `RollbackRequest`/`RollbackResponse`）；**前端业务代码一行未改**（`useRollbackRemediation.ts` 本来就照着正确的契约写，是后端违约）；**服务端也一行未改**——本笔的缺陷全在 CLI 侧，服务端的 `RollbackChange` 本来就是对的（上一笔修的）；`--force` 是**移除**（破坏性变更），已在本文与 `docs/cli-reference.md` 写明原因；**没有**给 CLI 加 `--as` 之外的授权面（策略判定仍由 `localAuthorizer` 负责，与其它本地命令一致）。

### 文档（`product-roadmap.md` 表格规整化：5 处畸形行修好，`状态` 列现在真的会渲染 + 门禁要求格数匹配）

- **背景**：上一笔给 4 张 item 表加了 `状态` 列，但有 **5 行表格是畸形的**，在 GFM 下那一行的状态格**不会渲染**——多格行被丢掉溢出格（正是末列的状态），少格行状态格留空，跨物理行的行整行坏。所以这不是美化，是那道列收尾。
- **5 处修法（内容零丢失，逐处）**：① L27 —— 一个 code span 里的**未转义 `|`**（`` `levee authz status|explain` ``）破坏分格 → 转义为 `\|`；② L34 —— 标题被并进了优先级格 → 拆成「优先级 | 项」；③ L50–53 —— **跨 4 个物理行**的一行 → join 成一行，并补上缺失的「依赖」格（`—`）；④ L68 —— 「理由」后多出一个理由格 → 合并（以 `；` 分隔）；⑤ L69 —— 一个独立的 `无` 格与紧随的 `无（…）` 格**语义重复** → 吸收进后者。
- **门禁收紧**：`scripts/check_roadmap_status.py` 现在要求**每条数据行的格数等于其表头格数**，理由写在判据里（多格=丢末格=丢状态）。`scripts/test_roadmap_status.py` 11 → **13** 例（新增「多格行被抓」「转义 `|` 仍算一格」）。
- **验证**：`check_roadmap_status` PASSED（53 行，计数不变：closed=47 / decided=1 / open=2 / blocked-on:人手=3）；本套件 13 例全绿；`check_doc_anchors`、`test_changelog_structure`、`unittest discover -s scripts` 全过。**变异**：往一条真实行注入多余格 → 门禁红并点名该行。
- **如实边界**：**只动这 5 行，其余 48 行逐字节未变**（`git diff` = 8 删 / 5 插）；L69 处吸收的那个独立 `无` 与后一格 `无（…）` 重复，未丢信息。表格现在是规整的，门禁会保持它规整。

### 文档（`CONTRIBUTING.md` §5.1 补 CI 等待的第二种「零 check」来源：base 前进导致的 DIRTY）

- **缺口**：§5.1 已有的 callout 只写了一条触发路径——把 PR 的 **base 设成别的分支**时 `ci.yml` 一条 check 都不生成，并落下原则「"这个 PR 没有红"并不等于"CI 绿了"」。但**同一个症状还有第二条路径没被登记**：base 自己往前走路导致 PR 变 `CONFLICTING`（`mergeStateStatus=DIRTY`）时，`ci.yml` 同样不生成 check。缺这一条的代价不是文档不好看，而是**等待循环会读错**：未完成的 check 数停住不动，与 runner 积压的形状完全相同。
- **两笔实测**（就是本条存在的理由）：`#285` 的哨兵只数 `pending`，磨到自设的 90 轮上限仍未收口，报的是 `TIMEOUT`——而那时候候选早已 DIRTY；`#295` 的循环每轮同时采 `mergeable` / `mergeStateStatus`，第 16 轮就报出 `NEEDS_REBASE`，其间该 SHA 上的 check run 总数与未完成数都卡住不动。
- **改动**：只往既有 callout 追加一段，写清第二种触发条件、判据（**每轮同时读两个字段，读到 DIRTY 就停下来先 rebase、再在新头上重跑**）与上面两笔实测。**不重述「可合 = 哪个 context 绿」**——那条口径已经在 §5.4 第 3 条，本条只以引用衔接（同一个结论写两遍就会漏改一半）。也不新增 `path:line` 式锚点：PR 编号与章节号稳定，行号会腐烂。
- **验证**：`check_doc_anchors` 27 锚点 + 29 claims PASSED，但**这条只是回归哨，不是对本段的覆盖证明**——`CONTRIBUTING.md` 虽在它扫的 6 本活账本里，全书却只有裸文件路径、**0 个 `path:line` 形状锚点**（实测数过），所以本段的追加不会动那 27 这个数。`test_changelog_structure` / `test_doc_anchors` / `check_roadmap_status` 全过；追加的 10 行每行都带 `> ` 前缀，逐行核过没把 blockquote 断成普通段落。
- **顺带核实与顺带存疑**：本段赖以成立的「`ci.yml` 的 `pull_request` 触发只列 `main` / `master` / `develop`」现查 `.github/workflows/ci.yml` 仍在（`branches: [main, master, develop]`），未被这段改动影响；而 DIRTY 那条机制**不是从触发器推出来的**，是今天两笔 PR 上直接观测到的。**但**同一句里既有的「`ci.yml` 的 **24 个** check run」这个计数我**没能核实**：`ci.yml` 顶层 job 实测是 16 个，加上 matrix 展开与第三方检查后与 24 对不对得上没算过——那是另一处文档漂移，不在本笔顺手改，登记给下一次专门核计数的人。
- **如实边界**：本条讲的是「为什么等不到 check 收敛」，**不给**新的执法机制。把 DIRTY 识别变成机器门禁（例如让等待脚本本身成为被检对象）是另一件事，本笔不做。

### 文档（`project-status-2026-11.md` 追加 §13：订正 §12 的「47 行表」读数口径，并记录 #293 已消解判据分歧）

- **订正的是什么**：§12 写「同一份 **47** 行表」——47 是 `^| P` 判据下的行数，读起来却像表的规模。路线图**逻辑数据行实为 53**：47 + 2 条 `~~P1~~`/`~~P2~~` 开头 + 1 条身份层 `| — |` + 运营层表 3 条（正是 §12 点名的结构性漏项）。权威出处是实测：`python scripts/check_roadmap_status.py` ⇒ `ROADMAP STATUS CHECK PASSED: 53 rows, blocked-on:人手=3, closed=47, decided=1, open=2`（跑在 `d1a3d76`）。
- **我自己数错的过程也写进去**：先用朴素 `split("|")` 数出 **59**，换个写法又得 **52**——错在解析器：逻辑行可跨多物理行、单元格里允许未转义 `|`、有的行列数与表头不同（`scripts/check_roadmap_status.py:26-30` 把这三条写明了）。教训：**计数口径要连着"畸形行怎么解析"一起写，否则同一个数会由不同人给出不同答案。**
- **§12 的判据分歧已被 #293 消解**：闭合状态收成一列「状态」+ 门禁钉形状（`MIN_ROWS = 50` 是气囊，整张表被漏数就红）。§12 那张 A=11 / B=1 / C=1 / 人工=6 表用同一实现在 `e848f79` 与 `d1a3d76` 各跑一遍仍是 **11 / 1 / 1** ⇒ 「三判据各自可复现、彼此不一致」那句没写错；变的是现在有了唯一权威口径。
- **锚点复核**：§12 引用的 13 处 roadmap 行号在 #293 重写整张表（122 行 diff）后**逐行回读仍成立**（状态加在行尾，行号不动）⇒ 无需为锚点另开一笔。
- **一条边界**：#293 的门禁保证「每行有且只有一个规范状态」，**不保证状态填得对**——填错的 `closed` 依然是 `closed`；`docs/blocked-items.md` 的分类计数仍是另一套口径，未被任何门禁止住。
- **验证**：`python scripts/test_changelog_structure.py` OK；`python scripts/check_doc_anchors.py` PASSED（27 锚点 + 29 claims）；`python scripts/check_roadmap_status.py` PASSED；零产品代码改动 ⇒ 不动测试套件。§12 已随 **v1.25.0**（折叠提交 `334bf2c`）发布，故本节按追加式口径只订正、不回改旧节。


### 新增（`target history` 接 `--remote`：本系列里唯一一处「远程显示更少」，缺的键由输出自己点名）

- **方案由 owner 2026-11-06 拍板**：`InventoryService.TargetHistory` **早就存在**，本笔不新增 RPC；要做的是把 CLI 接上去，并处理「线上的字段比本地少」这件事。三条路里选了一条：
  - **补齐线上**（加宽 `TargetHistoryEntry` 到 15 个字段）——被本仓自己写下的边界否掉。`proto/levee.proto` 的 `Change.plan` 注释明言计划正文「never for `ListChanges`」，而缺的 10 个键里 `params` 与 `plan_json` 正是同类大字段；`runToPB` 也不映射 `PlanJSON`。把这条边界为了一致性推翻，代价比收益大。
  - **收窄本地**（本地也只打 5 个键）——等于把已经发出去的 `--json` 形状改契约，现有消费方会掉字段。
  - **远程显示更少 + 公开点名**（选这条）——沿用 `system status` 远程模式已经落地的先例（`remoteLocalOnlyStatusFields`）：远程打出线上真带的那 5 个键，并把这个缺口写进输出，而不是静默省略。
- **缺口是量出来的，不是估计的**：`state.Run` 有 15 个 json 标签，`TargetHistoryEntry` 有 5 个字段，差 10 个 —— `template_name` / `params` / `plan_hash` / `plan_json` / `approval_status` / `approval_level` / `priority` / `updated_at` / `incident_id` / `tenant_id`。这份名单写进 `remoteTargetHistoryLocalOnlyFields`，并出现在 `--json` 的 `meta.local_only`（同批还有 `meta.mode` / `meta.server`；本地的 `meta` 仍是 `null`，所以「服务器什么也没告诉我」与「服务器告诉我这是子集」两种情形分得开）。
- **保留的键沿用本地的名字与形态，这一点比少几个键更要紧**：主键叫 `id`（不是线上字段名 `run_id`），`created_at` 是 RFC3339 字符串（不是裸的 int64 秒计数）。否则消费方要按 `--remote` 分支处理键名与时间格式，而「少几个键」本来只需容忍即可。
- **人类表格仍逐字节相同**：两个模式填同一张 `targetHistoryRow`、共用 `printTargetHistoryHuman`，所以这一档的差异只可能出现在 `--json`，且出现在那里是申报过的。渲染逻辑与 `8ef53b8` 上的原实现逐字对齐（`%-22s %-20s %-10s %-12s` + `workflowDisplay` + `2006-01-02`），本地路径的行为一个字节都没动。
- **顺手堵掉一个本仓 CLI 里第一次出现的窄化**：`--limit` 是 `int`，`TargetHistoryRequest.limit` 是 `int32`。此前 CLI 没有任何整型窄化（`grep int32( cmd/levee/*.go` 只有这一处），不做钳位时 `--limit 3000000000` 会**静默变成一个不同的数**——截断成负数还会被服务端兜成 20，等于操作员输入的数字和服务端执行的数字不是一回事，正是这一系列要消灭的那类缺陷。现按 `math.MaxInt32` 钳。
- **`local_only` 不靠注释自觉**：`TestTargetHistoryRemoteJSONIsDeclaredSubsetOfLocal` 读两边的**实际输出**求键集差，声明与实测不符就红。夹具因此必须带 `tenant_id`——它 `omitempty`，空租户会让**本地**也不打这个键，于是「声明里多写了 `tenant_id`」这种错就测不出来（第一版就是这么红的，报的是 9 个而不是 10 个）。反向也断言：远程不许出现本地没有的键。
- **证据（8 个新用例）**：人类表格逐字节相同（并单独断言远程行真过了 `workflowDisplay`——两边同时是垃圾也能通过相等比较，所以另断言表格里不出现 YAML 原文）；空答案两边同一条 `No changes recorded on <host>.`；`--json` 子集契约（键集差＝声明、共有键同值、`created_at` 按时刻比较）；`meta.local_only` / `mode` / `server` 三项且先断言 `local_only` 非空；`--limit 1` 确实穿到服务端（前置断言默认 limit 下本地有 2 行，否则这条只在测 limit 本身）；**钳位走线上时确实是 `MaxInt32`**（用一个记录请求的 `InventoryServiceServer` 假实现读 `req.GetLimit()`，不靠读代码）；死端点必须报错（等价用例两边读同一份库，分不出走的是哪条路）；远程不建本地库。测试复用 ② 的 `startGroupTestServer`（`TargetHistory` 与 `ListGroups` 同属 `InventoryService`），没有第三次注册这个服务，也没有改共享的 `startTestServer`。
- **`--remote` 白名单 14 → 15**：`root.go` 与 `TestRemoteCapabilityListIsExactlyThese` 同步；文档全局选项表由 `TestRemoteConsumersInCliReferenceMatchTheList` 逼着同步（第一次跑就红在「文档没写 `target history`」，是这条守卫在本笔的第二次真实射程）。`docs/cli-reference.md` 补了 **§6.4**——`target history` 此前在参考手册里**完全没有条目**（第 6 章只有 list / import / check；`freeze` / `unfreeze` / `retire` 同样缺，本笔不代做，如实登记）。
- **两处未修、如实登记**：① `TargetHistoryEntry.workflow_name` 存的是工作流**原文**（`runs.workflow_name` 即渲染后的 YAML），所以这条列表 RPC 会把整份文档发给每个调用方——显示两侧都过 `workflowDisplay` 所以无碍，但载荷偏重，与 `Change.plan` 那条边界同一理由，将来该只带声明名；② 本地 PostgreSQL 路径把库里的时刻解成**会话时区**、SQLite 解成 UTC，远程只能固定按 UTC 渲染，所以在 PG 部署上同一 run 的 CREATED 列可能与本地**差一天**——已登记为 `docs/blocked-items.md` 的 **D-3**，并据此把 ② 那句「逐字节相同」的适用范围收窄为「SQLite 部署上成立」。
- **变异（6 条全部先红在预期位，还原后逐文件 `sha256` 与注入前相同）**：① 声明表去掉 `tenant_id` → 子集用例红（键集差＝9 而非 10）；② 远程主键 `id` 改回 `run_id` → 红（既撞「远程不许有本地没有的键」，也撞「主键仍叫 id」）；③ `created_at` 退回裸 int64 秒 → 红（`time.Time` 解不开一个数）；④ **钳位掏空**（`_ = math.MaxInt32`，其余接线不动）→ **只有那条记录请求的用例红**，证明承重的是那条用例而不是别的顺带效应；⑤ **远程分支短路回本地读取** → 5 条红，但**人类表格与空答案两条等价用例仍然绿**——这是「双模式等价测试必须配死端点对照」的又一次实测：两边读同一份库时，短路成本地根本分不出来；⑥ 服务端不再把 `run.Creator` 放上线 → 本笔的 CLI 子集用例红。**⑥ 的另一半结论要如实说**：`internal/grpc` 里既有那两条服务端用例（`TestInventoryTargetHistory`、`TestServicePolicy_TargetHistoryShowsOnlyVisibleRuns`）对这条变异**没有反应**——它们不检查 `creator`，所以线上第 5 个字段的承重证据目前只在 CLI 侧。本笔不改别人的测试文件，只登记。
- **门禁**（在下面这棵 rebase 后的树上复跑，而不是改动前那棵）：`gofmt -l ./cmd ./internal` 空输出；`go build ./...` / `go vet ./...` rc=0；`go test ./... -count=1` 全包 rc=0；`go test ./cmd/levee/ ./internal/grpc/ -count=1` 变异还原后双 ok；`check_doc_anchors` 27 锚点 + **29 claims** PASSED；`check_roadmap_status` PASSED（53 行）；`test_changelog_structure` 14 tests OK；`test_doc_anchors` 24 tests OK；`python -m unittest discover -s scripts` **119** tests OK。claims / 例数比上一笔记下的 25 与 18 多，是 master 这两轮自己长的（`CLAIMS` 第三轮 + 命令树负断言守卫），本笔没有新增 `path:line` 引用，也没有改动任何下限。

### 文档（`product-roadmap.md` 闭合状态收成一列 `状态` + 门禁：把「未决行数」从五种写法变成一个答案）

- **缺口（qoder 在 `docs/project-status-2026-11.md` §12 登记为「只记不改」）**：roadmap 的「未决行数」**没有唯一答案**——同一张表，按优先级格有无闭合标记数是 **11**、按任一格是否有标记是 **1**、再加标题删除线是 **1**，人工回读是 **6**，三个判据各自可复现。根因是闭合状态有 **5 种互不兼容写法**（优先级格 `✅已落地` / 尾列 `✅已落地` / 标题 `~~划掉~~` / 优先级格 `已修`·`已定案并落地`·`已接线` / 正文 `已完成`）。
- **实测比登记更糟：是 53 条数据行，不是 47。** `^| P` 扫描漏掉 2 条 `~~P1~~` 开头行、1 条 `| — |` 身份层订正行、以及**整张运营层表**（`| 项 | 目标 |`，行首没有 `P`）——即登记里点名的「结构性漏项」。
- **改动**：
  - 4 张 item 表各加一列 **`状态`**，取值**单一来源**：`closed`（已完成，或已定档不做）/ `open`（未完成，含「未复现·待复发观察」）/ `decided`（产品决定已下且记了结果）/ `blocked-on:<谁>`（等一个点名的外部对象）。状态**只从行末格读**——因为表格本身是 ragged 的（有的行少一格、有的正文里有未转义的 `|`、有的行跨 4 个物理行）。
  - 新增门禁 `scripts/check_roadmap_status.py`：**每条数据行必须恰有一格且取值合法**（第五种写法再也进不来），并**打印计数**——「唯一答案」由此而来。含两个防空转断言：行数下限（50/53）与「**运营层表必须被识别**」（它正是被漏掉的那张，识别不到就 ScopeError 而不是静默少算）。
  - `scripts/test_roadmap_status.py` 11 例（多行行、缺列、坏取值、空 `blocked-on:`、运营层计数、缺运营层表→ScopeError、下限）+ 点名断言；ci.yml 的 release-gate 点名表加 `test_roadmap_status`。
- **现状计数**：**closed=47 / open=2（L56 calendar `0xc0000005`、L59 browser-check windows，均「未复现」）/ decided=1（L35 双语定档不做）/ blocked-on:人手=3（运营层 demo 录像、角色视频、品牌资产）**。
- **验证**：`check_roadmap_status` PASSED（53 行）；本套件 11 例全绿；`unittest discover -s scripts` 全绿；`check_doc_anchors`（27 锚点 + 29 claims）、`test_changelog_structure` 全过。**变异三条**：① 状态改 `done` → 红并点名行；② 删掉运营层表 → **ScopeError（exit 2）**；③ 删掉某行状态格 → 红并读出原末格。
- **如实边界**：本笔**只加列与门禁，正文一字未改**（某格若判错，改一格即可，信息零丢失）；**不修表格本身的 raggedness**（少格/未转义 `|`/多行行），那是另一件事。状态词表当前无 `blocked-on` 之外的子类。

### 测试（命令树负断言守卫：`levee schedule` / `drift scan` / `drift show` 这类「必须不存在」的命令路径变成机器可查）

- **缺口**：`cli_reference_flags_test.go` 只查文档→树的一个方向——文档点名的命令/旗标必须存在。账本里还有另一种断言：**命令路径必须不存在**，此前无任何门禁。其中两条是**关键词搜索做不到**的（同一个词在别处合法存在）：`levee schedule`（`docs/blocked-items.md` E-4 定档不做）——但 `schedule` 作为 `drift schedule` 存在；`drift show <report-id>`（`docs/levee-api.md` §6 从未注册）——但 `show` 作为 `drift baseline show` 存在。只有 cobra 树能区分顶层与嵌套。
- **改动**：新增 `cmd/levee/command_tree_test.go`——一张声明的 `commandClaims` 表（ledger/what/path/absent），用同包既有的 `childByName` 逐段解析 `rootCmd`：present 断言路径必须解析得到、absent 必须解析不到。含**成对样本自检**（同一词嵌套存在、顶层不存在）与条数下限，让解析器恒真/恒假也被抓住。
- **覆盖 9 条**：present 4（`drift detect` / `drift report` / `drift baseline list` / `drift schedule run`）；absent 5（顶层 `schedule`、`delegate`、`rollback-all`，`drift scan`，`drift show`）。
- **验证**：两条测试全绿；`gofmt` 干净、`go vet ./cmd/levee/` rc=0。**变异**：① present 路径改成不存在的子命令 → 红并点名；② absent 路径改成 `drift schedule`（存在）→ 红。
- **如实边界**：本表只管**命令路径**的存在性；与 Python `CLAIMS` 里 `cmd/levee` 目录级的 absent（`"rollback-all"` / `"delegate"` 任何出现即红）互补——前者查注册、后者查字符串出现。

### 修复（全仓四维自查：接口面门禁、判/读 ID 一致化、多租户 assignment、run 状态词表、前端三态与防重提交）

**怎么来的**：用户问「项目做了好几个月，到现在仍然不能转化为生产力吗」并要求一套无微不至的排查。按四个维度并行只读审计（接口表面 / 前端页面 / 数据流 / 构建基线），agent 报告逐条回读源码复核后才动手——三条最重的都是「认证之后没有第二道闸」这一类。

**安全（两条，均可从普通 token 触达）**：
1. `POST /gates/verify` 是**认证后零策略的远程命令执行器**：`GateService` 结构体没有 authz 字段、整个文件 grep `authorize` 为 0，任何合法 bearer（含仅 dev view 的团队）可对任意在册主机（含 prod 标签机）以平台凭据执行任意 shell。修法照 `TargetService` 同款：`WithAuthorizer` + 拨号前按目标自身 `env` 判 `apply`；未注册主机在闸上拒绝而非拨号后才报。nil authorizer 保持全仓一致的「无策略部署」缺省。
2. `GetLogs` / `StreamLogs` / `GetTrace` / `GetDiff` **判 change_id、读 run_id**：请求可同时指定两者，数据按胜出的 run id 读，而门禁只判了 change——把 run_id 换成另一环境的 run，一次请求即跨环境读出 step 的 stdout/stderr（命令原文与凭据回显）或审计轨迹。修法：**请求里点名的每个 id 都判**（判/读一致化）；`GetDiff` 回显的 run 过滤器同样受审，一个不能读的 id 不值得一个回显。

**数据正确性（三条）**：
3. dispatch 的 claim 写 `run_assignment` **不带 `TenantID`**——而 leader 是该行唯一写入方，于是多租户集群下该行对租户侧读写面整体不存在：全部 assignment CAS 回答 `ErrCrossTenantAccess`、purge 永不回收，表象是「控制台有数、API 无数」。`candidates` 改为回传整行（id 无法重建 tenant），claim 落库时镜像 run 的 tenant；补回归测试。
4. 后台 loop 拿**裸 store**（`cmd_serve.go` 的注释只为租户包装说明，与 funnel 无关），把 takeover loop 整个移出了 run 事件 funnel：settleOne 的 `running → interrupted` 永不 emit，`runeventstore.go` 开头列为六大写入点之一、chatops bridge 明确处理 `interrupted by takeover`——**消费者又零生产者了**，正是 funnel 建成要终结的回归。修法：funnel 构造提到 loop 之前，loop 拿 funnel（观察）但不拿租约包装（隔离）——两个装饰器回答两个问题， seam 上都要。装配合计不可单测直达，用源码级结构守卫钉住（funnel 先于 loop 构造、两处调用点必须传 `runEvents`），变异验证过。
5. `approvals.status` 的 schema 注释三处（`schema.sql` / `pgschema.sql` / `levee.proto`）都写着 `timeout`，常量与真实写入是 `expired`——batches `done/completed` 的同形事故：照注释写查询的人永远匹配不到行。中文注释由 `vocabulary.go` 自己记录了两条拼写并存，但没有任何守卫执行它。修法：三处改为 `expired` 并在注释里点明两种拼写的关系；`status_column_comment_guard_test.go` 扩到 approvals 表（常量从 `internal/approval/service.go` 解析，不重述），变异验证过。同批把 `runstatus` 词表的 CAS `from` 侧从字面量收敛为常量（change_service/takeover/dispatch/wiring 共 12 处）——CAS 的 `from` 是字面量时，常量改名当天它静默不再匹配任何行，而失败形态是「该成功的 CAS 全部失败」。

**前端（用户可见，按严重度）**：
6. **审批不防重复提交**：`ApprovalView` / `MobileApprovalView` 的 approve/reject/delegate 与键盘 a/r/d 全程无 busy 闸——审批是唯一真正不可逆的写操作且直接进审计链，慢网络或连点就是两条审批记录。加 `acting` 闸（按钮 `:disabled` + 处理器首行 return，键盘与弹窗窗口都在 handlers 的首行覆盖）。
7. **审批人身份硬编码 `'operator'`**：该值原样进审计行。新增 `utils/identity.ts` 从会话令牌 `sub` claim 解（解.payload 不是信任决定，服务端每请求自验；这是归属问题）+ 6 例单测；App.vue 侧栏收起后的纯图标按钮补 `aria-label`/`title`。
8. **SSE 静默失败**：`MonitorView` 把 quiet 刷新失败写进一个**全文件零渲染**的 ref——操作员盯着看起来实时的面板，数据停在最后成功快照。改为面板顶部一条 stream-note（含重试），仅当没有更高级错误档时出现。
9. **两处 theme 纪律破坏与两处对比度**：`AlertsView` 用了两个**从未定义**的 token 名（`--lv-warning` / `--lv-good`，权威拼写 `--lv-warn` / `--lv-ok`）+ 亮色 fallback，深色下 3.56:1；`ChangesView` / `MonitorView` 各有一处组件内 `html.dark` 分支——新增 `--lv-neutral-fill` 语义别名（两主题各一侧， tokens.css 单一权威），组件只消费别名、无 dark 分支；`MonitorView` 时间戳列 3.47:1 → 提到与邻列相同的 5.70:1。
10. 顺手：`ChangesView` 的 status 词表第三份逐字抄写改为从 `vocabulary.ts` + `format.ts` 两个 owner 派生；`ChangeDetailView` 两处 `1fr` → `minmax(0, 1fr)`（无它内容列溢出面板而非省略）。

**验证**：`go test ./...` 68 包全绿；`vitest` 333 例全绿（39 文件，含新增 identity 6 例）；`vue-tsc` 0 错；`golangci-lint run ./...` 0 issues；`internal/web/dist` 已随 `web/src` 重建同步（新鲜度门禁口径）。**边界**：本次只修「实证成立」的项；审计报告里另有一批设计取舍类（BuildForce 不可达、PG/SQLite 时钟源分叉、plan.Plan 持久化无 tag、wire 夹具 5 端点未覆盖等）未动，逐条留在 `docs/blocked-items.md` 待产品裁决。

### 新增（SQL 审核规则引擎首版：`levee sqlreview` CLI，20 条自撰规则）

- **缺口**：市场扫描裁决（2026-11-10）认定 LEVEE 对外第一身份是「数据库变更发布与治理核」，楔子是数据库变更标里的 SQL 审核能力（招标词表：SQL审核 / 变更自动执行 / 分级审批 / 回滚 / 审计留痕）；仓库已有 `mysql.query`/`pt_osc` 执行模块却没有审核面，读者拿到的第一条 LEVEE 体验与招标词表对不上。
- **修法**：`internal/sqlreview` 规则引擎 + `levee sqlreview` CLI（`--sql`/`--file` 二选一，`--json` 走标准信封，verdict 为 fail 时退出码 1 供流水线卡点）。解析器层嵌入 Apache-2.0 的 `xwb1989/sqlparser`（零依赖；goproxy.cn 不服务 vitess 现行模块路径，偏离底稿 §8.6 的 vitess 计划如实登记；语法冻结在 MySQL 5.7 时代是已知边界）；规则层 20 条全部自撰（SR-01 解析失败即拒 … SR-20 批次多语句不主张跨语句原子性），severity 只有 fail/warn 两档，聚合 fail > warn > pass。两条诚实规则即产品边界：语法看不到 ALTER 语句体（SR-08 如实说「判不了，人工审」），解析失败是 fail 不是放行（SR-01）。
- **验证**：`internal/sqlreview` 11 例（每条规则有触发用例；规则表完整性 17 条表内 + 3 条表外 = 20；同义句式 1=1 / true / `'x'='x'` / OR 臂恒真；语法盲区 CREATE INDEX / GRANT 如实呈现；静默语句零 findings；批级规则；空输入报错；消息自足性与无 copyleft 工具词 tripwire）；`cmd/levee` 文档旗标与包可达性门禁随命令注册自动覆盖；`go test -race` 绿。
- **许可证归属**：新增根目录 `NOTICE` 登记 xwb1989/sqlparser 的 Apache-2.0 归属（CONTRIBUTORS.md 版权行：Wenbin Xiao 2015 / Andrew Brampton 2017 / vitess 骨干 Copyright Google Inc）；规则层为自撰，NOTICE 写明。
- **如实边界**：本批只到 CLI 演示面；接 V19 计划前检、执行器白名单 AST 升级与审核报告落审计链（审核结论 + plan_hash + 审批链）在下一批；首单合同前由律师过 NOTICE 与规则文案（本笔是工程口径，非法律意见）。

## [v1.25.0] - 2026-11-06

> **发布状态：已切版。** 附注 tag `v1.25.0`（tag 对象 `44749a1c271a`，指向折叠提交 `334bf2c`——单父、以 `--rebase` 落地）于 2026-11-05T20:15Z 推送，且是在该折叠提交自己的 `master` push run 全绿之后才打的：run `42097852339` 26/26 job 全 success，含聚合门 `check (all jobs passed)`。`release.yml` 三段（CI gate / goreleaser / container image，run `42101719313`，20:15:40Z → 21:03:16Z）全部 success：第一段等的是 **tag push 自己触发的** `ci.yml` run `42101719312`（`head_branch=v1.25.0`，20:46:14Z success，26/26）——`require_ci_release.py` 明确不接受同一提交在分支上已经绿的那个 run，只认这个 tag ref 的；GitHub Release `draft=false`、`isPrerelease=false`、`publishedAt=2026-11-05T20:50:45Z`（11-06 04:50:45 +0800），资产 **7** 个（6 个平台包 + `checksums.txt`）；镜像 `ghcr.io/levango7/levee:v1.25.0` 按外部事实核过——`manifests/v1.25.0` 返回 **200**（同一次探测里 `v1.24.0` → 200、不存在的 `v1.25.99` → 404，证明探针有分辨力；`Accept` 必须带多架构 index 媒体类型，否则连已存在的 tag 也会读成 404），index 内是 `linux/amd64` + `linux/arm64` 两个平台（另两条 `unknown/unknown` 是构建 attestation）。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.25.0` / 逐字 `v1.25.0`（顺序不能反：规则②要求它逐字出现在远端 tag 集合里）。本节共 **36** 个小节，主题分布与行为变更见 [`docs/release-notes/v1.25.0.md`](docs/release-notes/v1.25.0.md)。

### 测试（`CLAIMS` 第三轮：目录级扫描 + `gone` 模式，把「从未实现／整包退役」也变成机器断言；下限 20 → 25）

- **起因**：上两轮把表建起来并扩到 25 条，但只加 present。**负断言**仍只有 `drift scan` 一条——稳健的负断言要么要求「若实现，符号一定落在某已知文件里」（命令级断言不满足：实现会落在新文件），要么要求「路径应当不存在」（删除类断言）。本轮把这两种形状都补上。
- **机制（两点）**：① `path` 现在可以是**目录** → 递归搜索，**跳过 `*_test.go`**（关于产品注册/导出了什么的断言，不该被测试夹具满足或推翻；要查测试文件就把该文件写成 `path`）；空目录 = ScopeError（防空转）而不是绿。② 新增 `mode="gone"`：路径必须**不存在**、且**父目录必须存在**（防末段笔误静默变绿）——对应「整包退役删除」那类账本断言。
- **新增 4 条**：`docs/levee-api.md` 的「`levee rollback-all` 从未实现」→ `cmd/levee` 目录 absent `"rollback-all"`；「`levee delegate` 从未实现」→ 同法 absent `"delegate"`；`docs/product-roadmap.md` 的「`internal/scheduler` 整包退役」与「`internal/recommend/rag` 整包退役」→ `gone`。
- **下限**：`MIN_CLAIMS` 20 → **25**（表当前 29 条）。
- **验证**：套件 18 → **24** 例全绿（新增目录扫描 present、目录跳 `_test.go`、空目录 ScopeError、`gone` 三态）；`check_doc_anchors` 27 锚点 + **29 claims**。**变异**：① 新建 `cmd/levee/zz_probe_mut.go` 写 `"rollback-all"` → 负断言红并点名该文件；② 复建 `internal/scheduler/` → `gone` 断言红（「exists again」）。
- **如实边界**：目录扫描跳过 `_test.go` 是刻意的（有专测）；`gone` 的父目录守卫防的是末段笔误、不防整条路径写错（那需要更粗的锚）。仍未覆盖：像 `drift schedule` 那种「同名字符串已存在于别处」的**顶层**命令负断言——关键词区分不了层级，需要更强的作用域表达，留作后续。


### 测试（`CLAIMS` 表第二轮：13 → 25 条，把 roadmap/blocked-items 已在引用的符号变成机器断言；下限抬到 20）

- **起因**：上一笔（`3f60a79`）建了 `CLAIMS` 机制并种 13 条。本轮继续登记——凡账本**已经用 `path:line` 引用、但引用之外没有任何守卫**的断言，只要形状可机器校验，就进表。
- **新增 12 条 present**：① `window.max_concurrency` 是 DSL 接受的字段（`internal/dsl/parser.go`）——正对上一笔刚订正的那句；② `runToPB` 的 Label 用 `dsl.DisplayName`（`internal/grpc/change_service.go`）；③ `buildEngineServeOptions` 是引擎 serve 选项的生产构造点（`cmd/levee/cmd_serve.go`）；④ 取库决策收敛到 `openStoreFromConfig`（`cmd/levee/helpers.go`）；⑤ 窗口关闭以 `ErrWindowClosed` 传出（`internal/dsl/window.go`）；⑥ `CheckWindowForPlan` 有生产调用点（`internal/wiring/calendar_gate.go`）；⑦ 词表 owner 导出 `StepStatusNames`（`internal/state/vocabulary.go`）；⑧ 集群接管时间线消费 `TakeoverEventDTO`（`web/src/views/ClusterView.vue`）；⑨ 插件破坏性动作声明持久化（`internal/plugin/registry.go` 的 `DestructiveActions`，D-1b）；⑩ 运行期通道 `upload` 动词（`internal/plugin/protocol.go`，D-1c）；⑪ human 门禁 quorum 字段 `ExcludeInitiator`（`internal/verify/human_gate.go`）；⑫ docgen 生成 `web/src/types/vocabulary.ts`（`internal/docgen/docgen.go`）。
- **下限**：`MIN_CLAIMS` 10 → **20**（棘轮：表当前位置 25 条，删断言需要连下限一起动）。
- **验证**：`check_doc_anchors` 27 锚点 + **25 claims**；套件 18 例；变异（把新行的 keyword 改错 → 红并点名 `internal/dsl/parser.go` 那条）。
- **如实边界（承上一笔）**：本轮只加 present 断言。负断言仍只有 `drift scan` 一条——稳健的负断言要求“若实现，符号一定落在某已知文件里”，多数“从未实现／定档不做”的命令级断言不满足这个形状（实现可能落在新文件），需目录级扫描才可靠，留作后续。

### 文档（`product-roadmap.md` 订正一处失实断言：`window.max_concurrency` 不是「已不存在」，是「存在但无读取方」）

- **失实**：`docs/product-roadmap.md` 第 37 行记「`Window.MaxConcurrency` **已不存在** … DSL 的 Window 类型里没有这个字段」，并断言原登记的「仍无人读取」是过期信息。
- **复核（2026-11-05，代码级 + 实测）**：`ChangeWindow.MaxConcurrency`（`internal/dsl/ast.go`）自 initial commit 起就在；`internal/dsl/parser.go` 的 `yamlWindowRaw` 读 `max_concurrency` 并赋给 `wf.Window`；`internal/dsl/emit.go` 回写。一份带 `window.max_concurrency: 5` 的工作流 `levee compile` 报 `val_errs=0 type_errs=0`，与不含该字段的对照组逐项相同 ⇒ 字段在 DSL 里存在且被接受。
- **真相**：消费面为空——`ChangeWindow.Check()` 只校验 start/end/timezone/days，plan/apply 都不读它，规范 §4.2 的 window 字段表也没有它。所以原登记的「仍无人读取」**成立、并未过期**；失实的是「已不存在」那半句。
- **改动**：只订正这一句散文，改用**文件 + 符号**级引用（不新增会腐烂的行号）。要不要接线或删除该字段是独立决定，本笔不动产品代码。
- **验证**：`python scripts/check_doc_anchors.py`、`python scripts/test_changelog_structure.py`、`python -m unittest discover -s scripts` 全过。
### 新增（`group list` 接 `--remote`：按「逐字节相同」补三个线上字段）

- **缺口是实的，不是防御性加字段**：本地 `group list --json` 直接 marshal `state.InventoryGroup`，其中 `created_at`（`json` 无 `omitempty`）**每一行都打**、`tenant_id`（有 `omitempty`）在分组真带租户时才打；而 `pb.Group` 原来只有 `id` / `name` / `parent_id`。照线上重建，远程会给**每一行**打出 `"created_at":"0001-01-01T00:00:00Z"` 并丢掉租户列——不是「少一点精度」，是每行都不一样。`UpsertInventoryGroup` 盖的是 `time.Now().UTC()`（`internal/state/inventory_sqlite.go`），所以时间戳带完整纳秒。
- **修法（加宽线上，不按模式分叉输出）**：`pb.Group` 补 `created_at`（4，int64 秒）、`created_at_nanos`（5，纳秒余数）、`tenant_id`（6）。前两个是 `google.protobuf.Timestamp` 那种秒/纳秒拆分，与上一笔给 `TraceEntry.timestamp_nanos` 的口径一致：字段 4 单独看仍是秒，没学会 5 的读者丢失余数的方式与今天完全一致。`groupToPB` 三处都填，`ListGroups` 与 `CreateGroup` 共用这一个映射；CLI 的 `groupFromPB` 用 `time.Unix(sec, nanos).UTC()` 重建——只取秒就和上一笔修掉的 `traceFromPB` 是同一个缺陷。
- **适配器镜像全部 6 条 RPC**（ListGroups / CreateGroup / DeleteGroup / ImportTargets / SetTargetStatus / TargetHistory），不只接用得上的那条。理由与既有写法一致：`targetServiceAdapter` 连 AddTarget / RemoveTarget 都镜像了。半镜像的接口正是下一个命令被写成「只有一种模式能用」的原因，而那种代码在评审里看不出来。`TestInventoryAdapterMirrorsTheWholeRPCSet` 用反射把这条口径变成断言（跳过 `mustEmbedUnimplemented…`，并带「比到的方法数 ≥ 6」的底数，免得底数变成零时断言空转）。
- **可见性照实说**：`ListGroups` 按**部署范围的命名空间**判读权限（分组不声明环境，名字里的 `prod/db` 是命名习惯而不是策略能依据的事实），所以带策略的部署里远程可能被拒；本地不判权。这条不是本笔引入的，是本笔第一次让 CLI 走到它。
- **证据（6 个用例）**：三种输出形状逐字节相同（人类与 `--json`，且 `--json` 那条先自证本地输出真有 `.123456789` 与租户 `t-a`，否则比较只是在测自己）；空分组两边同报 `No groups.`；死端点必须报错（等价用例两边读同一份库，分不出走的是哪条路）；远程不建本地库；解析 `--json` 信封后**点名两个有损映射**（租户列、亚秒余数）——逐字节相等也可能两边同时丢掉同一件东西；以及上面那条反射守卫。测试 server 建在自己的文件里（`pb.RegisterInventoryServiceServer(srv.GrpcServer(), …)`），**没有改共享的 `grpc_client_test.go`**，也没有把这个服务塞进 `startTestServer` 去改变其余远程测试的对话对象。
- **变异（4 条全部先红在预期位，还原后 `sha256` 逐字节相同）**：① `groupToPB` 不填 `created_at_nanos` → `--json` 等价与信封断言同时红；② `groupToPB` 不填 `tenant_id` → 两条都红；③ **CLI 退回 `time.Unix(sec, 0)`** → 等价红（与上一笔同一类缺陷，这次是新代码里没有它）；④ 远程分支短路成本地 → 死端点用例红。
- **口径说明（与仓里另三处不一致，是 owner 的选择）**：`docs/cli-reference.md` 的 §5.1 `template list`、§5.2 `template show`、§6.1 `target list` 仍写着「远程 `--json` 只到秒」——那三处是**如实标注的已知差异**，不是假成功。本笔按 owner 2026-11-05 的指令，把 `group list` 放进「两种模式逐字节相同」这一档。于是同一个 `--json` 精度现在有两档保证，把另三处补齐是另一个决定（要动 `pb.Template` / `pb.Target` 并改三处已成文的契约），本笔不做。
- **门禁**：`gofmt -l ./cmd ./internal` 空输出；`go build ./...` / `go vet ./...` rc=0；`go test ./... -count=1` 全包 ok；`scripts/gen-proto.sh`（钉住的 protoc v27.0）重生成后 `git diff --exit-code -- internal/grpc/pb/` rc=0；`check_doc_anchors` / `test_changelog_structure` / `test_doc_anchors` 全过。
### 新增（`audit show` 接 `--remote`：按 trace ID 取单条的新 RPC；顺带修掉上一笔留下的亚秒丢失）

- **为什么不能复用现成的 RPC**：`ListAuditTraces` 只按 **run** 过滤，而 `audit show` 的入参是一个 **trace ID**。照现有面走，操作者拿着一个已经点名的行 ID，却要先枚举所有 run、逐 run 翻页去找那一行——那不是"接上远程"，是把一次点查做成全表扫描。故新增 `AuditService.GetAuditTrace(GetAuditTraceRequest) returns (TraceEntry)`。
- **服务端（存在性先于可见性，与 `GetRunReport` 同序）**：行不在 ⇒ `NotFound`；行**在**但属于调用方视野外的 run ⇒ `PermissionDenied`，**不报"不存在"**——否则这条错误本身就变成了探测哪些 trace ID 存在的探针。空 ID 是 `InvalidArgument` 而不是"不过滤"（那等于一次全表扫描，且返回任意一行）。作用域取该行所属的 run：`trace.run_id` 在两套方言里都是 `NOT NULL` + `FOREIGN KEY … ON DELETE CASCADE`，孤儿 trace 不可能存在，因此没有为它写不可达的兜底分支；读 run 失败按 `Internal` 报错，而不是猜一个环境。
- **补线上字段**：`TraceEntry.timestamp_nanos`（15）——字段 8 那个 int64 秒的**亚秒余数**，即 `google.protobuf.Timestamp` 那种秒/纳秒拆分。字段 8 语义不动，既有消费者（web 的 `timestamp: number`）一个都不用改；没学会 15 的读者丢失亚秒的方式与今天完全一致。两个生产者都填它：`traceEntryToPB`（trace 行）与 `auditToTraceEntry`（审计行转 trace 形状）——只填一个的话，另一条路径会静默报零，那比不填更坏。
- **这条字段顺带修掉了上一笔（`audit list` / `audit export` 接 `--remote`）的一个真缺陷**：CLI 的 `traceFromPB` 此前用 `time.Unix(sec, 0)` 重建行，于是远程的 `audit list`（人类与 `--json`）与 `audit export --json` 对**任何真实行**都与本地不同——recorder 盖的是 `time.Now().UTC()`，SQLite 往返保留**完整纳秒**、PostgreSQL 保留微秒。**它此前一直是绿的，因为每一份夹具都写了 `Truncate(time.Second)`：那批"逐字节相同"的断言没有射程，不是正确。** 本笔把夹具换成故意不对齐的瞬间（`2024-03-17T08:05:03.123456789Z`），并在比较前先断言本地输出里真有 `.123456789`——两边同时丢掉同一件东西也能被拦下。`cli-reference.md` §7.2 / §7.3 里由上一笔写下的两条"远程只到秒"因此已订正为"两种模式都含亚秒"。
- **接线**：`runAuditShow` 先 `resolveServiceMode()`，两条路径都交回 `*state.Trace`（`fetchTrace` → `getTraceLocal` / `getTraceRemote`），于是 `--json` / `--quiet` / 人类三个渲染分支**共用**、不可能按模式各长一份。远程的 `NotFound` 被翻译成与本地同一条 `trace "x" not found`，不是 `rpc error: code = NotFound desc = …`（与 `template show` 同口径）。`--remote` 白名单扩到 **13** 条，帮助文本与文档全局选项表由既有双向守卫逼着同步。新 RPC **不需要**在 REST 暴露矩阵里加豁免：`TestEveryRPCHasADocumentedRESTAnswer` 是**按族**判的，`AuditService` 整族早已映射到 `audit`；加一条豁免反而是死键（该守卫自己就断言"被豁免的 RPC 不得变成已路由"）。
- **证据（7 个新用例 + 1 处策略扩面）**：服务端 3 条——按 ID 取回**入库原文**（先自证夹具真带亚秒，否则用例只是在测自己）、不可用 ID 的三种形状（nil / 空串 / 不存在）各自报错、`GetAuditLog` 那条路径也带亚秒余数（另一个生产者，独立断言）。CLI 4 条——三种输出形状逐字节相同（并解析 `--json` 信封、点名三个易错的有损映射：入库原文而非 field 12 的投影、租户列、亚秒）、未知 ID 两边同一条错误文本、死端口必须报错（等价用例两边读同一份库，分不出走的是哪条路）、远程不建本地库。策略面：`TestServicePolicy_AuditReadsAreScopedPerRun` 增补按 ID 取行的三支——视野内可读、视野外**拒绝**而非"不存在"、真不存在报 `NotFound`。
- **变异（5 条全部先红在预期位，还原后 `sha256` 逐字节相同）**：① 服务端不填 `timestamp_nanos` → 服务端用例红；② `auditToTraceEntry` 不填 → 审计那条用例红（证明两个生产者各自承重，不是同一处代码）；③ **CLI 还原时退回 `time.Unix(sec, 0)`** → **四处断言同时红**，横跨上一笔与本笔的 CLI 用例：这就是"上一批比较原本空转"的直接证据；④ 服务端不做可见性判定 → 策略用例红；⑤ 空 ID 当成"不过滤" → 服务端用例红。
- **验证**：`gofmt -l ./cmd ./internal` 空输出；`go build ./...` / `go vet ./...` rc=0；`go test ./... -count=1` 全包 ok；`PROTOC=/e/dev-data/protoc/27.0/bin/protoc.exe bash scripts/gen-proto.sh` 后 `git diff --exit-code -- internal/grpc/pb/` 为空（生成物与钉住的工具链一致）；`golangci-lint run --timeout 20m`（本机 v2.13.2，与 CI 钉的 `version: v2.13` 同线；**不用 CI 的 5m**——本机 5m 会打出 `0 issues.` 却以 rc=4 超时结束，那种组合不算通过）。
### 新增（`audit list` / `audit export` 接 `--remote`：先把「1000 行静默截断」修掉）

- **为什么不能直接接（两处）**：① 服务端 `ListAuditTraces` 把存储读取钉在 `Limit: maxPageSize`（=1000）再在这份切片上分页，**超过 1000 行的 run 在线上根本取不到**——本地 `ListTraces` 无上限，于是远程会「少给但不报错」，翻页游标也在截断后的集合上算，第 1001 行永远翻不到；② `pb.TraceEntry.detail` 是给人看的**投影**（`error: …` 或 recorder 的 metadata），而 `audit export` / `audit show` 打的是**入库原文**（正是封链覆盖的那份 JSON），照投影还原等于让导出与自己导出的账本不一致。
- **修法（服务端）**：`ListAuditTraces` 改成同文件 `GetAuditLog` 早已在用的形状——按 `auditStorePageSize` 一块块走存储、在过滤后的流上数出精确的 `total_size`、按游标取页。`state.TraceFilter` 因此新增 `Offset`（SQLite 与 PostgreSQL 双引擎；SQLite 侧 OFFSET 与 LIMIT 同进同出，因为该方言不接受裸 OFFSET）。具名 run 与全局枚举都不再在页上限处静默收尾。
- **补线上字段**：`pb.TraceEntry` 新增 `stored_detail`（13，入库原文）与 `tenant_id`（14），`traceEntryToPB` 填它俩；`detail`（12）保持原投影不动既有消费者。生成仍用钉住的 protoc v27.0 工具链——改前先做「未改即重建」对照，产物零差异才动手。
- **接线**：`runAuditList` / `runAuditExport` 先 `resolveServiceMode()`，远程经工厂 `GetAuditService().ListAuditTraces` 按页拉全量并还原成 `[]*state.Trace`，与本地**共用同一渲染**（人类表格 / `--quiet` 的 ID 清单 / `--json` / CSV 表头与列序）。行序两边都是存储的 `(timestamp, id)`，所以是同一序列而不只是同一集合。`--remote` 白名单扩到 12 条（master 侧刚合的 `system status` 是第 10 条），帮助文本与 `docs/cli-reference.md` 的全局选项表由既有双向守卫逼着同步。
- **精度与可见性差异（照实说）**：`--format csv` 的格式本就精确到秒，两种模式逐字节相同。**`--json` 的 `timestamp` 本笔提交时确实远程只到秒——那是缺陷，已由上面一笔（`audit show`）补 `TraceEntry.timestamp_nanos` 修掉，两种模式现在都含亚秒，且当时"逐字节相同"的断言因夹具全部按秒对齐而没有射程，也已一并加固。** 服务端还按调用方可见环境**收窄** trace 行，本地不判权——带策略的部署里远程可能少几行，那是本来看不到那个环境，不是丢数据。
- **证据（11 个用例）**：服务端 3 条——按 `auditStorePageSize+7` = 1007 行播种，具名 run 与全局枚举各取全量且顺序等于存储序；`total_size` 断言为过滤后的全量而非本页条数；`stored_detail` 与投影分开断言（含 `tenant_id`，空租户列会让这条映射没人查）。存储层 2 条——按页拼接不重不漏（**故意写成有界循环**：若 OFFSET 被忽略，每页都返回同样三行，有界循环立刻报「第 N 页行数不对」，而「循环到页不满」会转圈到测试超时，症状看起来像 hang 不像红）；无 LIMIT 时 OFFSET 不参与构造 SQL。CLI 6 条——三种输出形状**逐字节相同**、未知 run 两边同报 `No traces found.`、export 的 json 与 csv 各逐字节相同且先断言导出里真有入库原文（夹具自证：投影不含 `web-01`，两边同时丢也拦得住）、每页 2 条驱动翻页抓游标交接、死端口必须报错（等价用例两边读同一份库，分不出走的是哪条路）、远程不建本地库。
- **变异（9 条全部先红，还原后 `sha256` 逐字节相同）**：① 服务端只走一个存储页（即修复前的截断）→ 跨上限用例红；② `total_size` 改报本页条数 → 计数用例红；③ 服务端不带 `stored_detail` → 服务端与 CLI export 同时红；④ CLI 还原时取投影字段而非原文 → export 等价红（这条是「字段选错」，与 ③ 的「字段缺失」不同形）；⑤ CLI 不交接游标 → 翻页用例红；⑥ 远程分支短路成本地 → 死端口用例红；⑦⑧ 服务端 / CLI 分别丢掉 `tenant_id` → 两条都红；⑨ SQLite 不认 `Offset` → 存储层用例红。
- **如实边界（CodeQL 报了一条 high，判为假阳性，未改代码也未消警）**：`CodeQL` 检查在 `internal/grpc/audit_service.go:261` 报了 `go/uncontrolled-allocation-size`（"Slice memory allocation with excessive size value"），指的正是本笔新写的 `entries := make([]*pb.TraceEntry, 0, pageSize)`。判定依据三条：① **`pageSize` 在 `make` 之前已被夹住**——`internal/grpc/helpers.go:15` 的 `maxPageSize = 1000`，`int32` 入参先 `<= 0 → defaultPageSize`、再 `> maxPageSize → maxPageSize`，所以容量上界是 1000 个指针（64 位下 8 KB），规则的前提"不受控"在这一处不成立；② **同一文件、同一形状、同一夹法在 master 上早已是一条开放告警**——`code-scanning/alerts` 返回 **#5 = `audit_service.go:162`**（`GetAuditLog` 里逐字相同的 `make([]*pb.TraceEntry, 0, pageSize)`，夹法在 :137–143 与本处 :249–255 同形）与 **#6 = `change_service.go:2811`**，两条同规则同 severity、均未消警，本笔只是这条既有模式的第三个实例，不是新风险类别；③ **`CodeQL` 不是必需检查**——分支保护只要求 `check (all jobs passed)`，其 `needs:` 是 `ci.yml` 的 16 个 job，而 CodeQL 定义在 `codeql.yml`，不在其中。**不改代码的理由**：把这一处的预分配去掉只会让同一文件里出现第三种变体（比三处一致更难读），而省下的是至多约 11 次 append 扩容；**不消警的理由**：master 上还有 6 条开放告警（含 `internal/diagnosis/health_probe.go:774` 的 critical `go/request-forgery` 与 `internal/state/inventory_sqlite.go:246` 的 high `go/sql-injection`），处置口径应由仓库所有者统一定，不在本笔里单点动别人的安全面。

### 测试（账本断言机器化：`check_doc_anchors.py` 增加声明式 `CLAIMS`，把「锚点还指向某处」升级到「那句话还成立」）

- **缺口**：`scripts/check_doc_anchors.py` 只证 **liveness**——锚点能被解析、行号不越过 EOF。`docs/project-status-2026-11.md` §8.4 记的机制建议（「把「台账声称未做/已做」变成可机器校验的东西」）从未落地，所以一条账本句子可以一边锚点解析得到、一边与代码矛盾，而 CI 全绿。
- **改动**：新增声明式 `CLAIMS` 表，每行 = (ledger, what, path, keyword, mode)。`check_claims()` 逐条检查 `keyword` 在 `path` 里 **present**（账本说「已落地」）或 **absent**（账本说「未实现／已删除」）。未知 `mode`、或 `ledger` 不在 `LIVE_DOCS` 里，是 **ScopeError（exit 2）**——那是声明写错、不是代码漂移，读的人该修表而不是修码。加 `MIN_CLAIMS` 安全垫（同 `MIN_ANCHORS` 之意）。`check()` 把两半合流，`main` 的通过行同时报锚点与断言数。
- **种子（13 条，全部现测为真）**：12 条 present + 1 条 absent，取自三本活账本——回滚后验证装配（`internal/wiring/run.go` 的 `NewPostRollbackVerifier`）、registry/labels 冲突检出（`internal/inventory/ownership.go` 的 `LabelConflicts`）、日历装进 serve（`cmd/levee/cmd_serve.go` 的 `WithChangeCalendar`）、日历门禁 plan+apply（`internal/wiring/calendar_gate.go` 的 `AssertNotFrozen`）、回滚豁免结构测试、观察者扇出、LE103、human 门禁 approver、插件执行面接缝、input 取值供给、`drift detect` 已注册；负断言是 `drift scan` 从未注册（`cmd/levee/cmd_drift.go` 不含 `"scan"`）。
- **验证**：`test_doc_anchors.py` 18 例全绿（新增 8 例：present/absent 的满足与违反各有配对样本、target 缺失、未知 mode、非活账本 ledger 是 ScopeError、表下限与真实条数挂钩）；`unittest discover -s scripts` 94→102 例全绿。**变异三条（先红后还原）**：① present 关键字改成不存在 → 红并点名 `docs/product-roadmap.md` 那条与文件/关键字；② 负断言的 keyword 改成 `"detect"` → 红并说 "negative claim is stale"；③ `MIN_CLAIMS` 抬到 999 → 下限红。
- **如实边界**：`CLAIMS` 是**声明式子集**，不是从散文刮出来的（刮会按句子猜关键字，两个方向都可能猜错）；`ledger`/`what` 只进报错文案，不与账本里的 `path:line` 做精确匹配——行号会腐烂，那是 liveness 半部的职责。CI 无需改：套件名不变，release-gate 本就点名 `test_doc_anchors`，而它调用 `check()`。

### 测试（CHANGELOG 结构门补两条：`[Unreleased]` 小节标题查重 + 标题行不许粘行）

- **缺口**：`scripts/test_changelog_structure.py` 的 7 例只查 `## [Unreleased]` 唯一、首个 `## ` 段是 `[Unreleased]`、未发布内容必须分组进 `### ` 小节、`## [tag]` 版本标题唯一——**[Unreleased] 块内 `### ` 小节标题的重复、以及标题行粘行，从来没有检查**。那正是 2026-10-31 双 agent 同锚点手术真实产出过的两个形态（重复节靠人工 python 断言兜住，粘行靠当场目测），两者都能让整套 CI 全绿。
- **为什么是现在**：今天板面上 5 个 agent 同时往同一个 `## [Unreleased]` 插节（opencode ×2、qoder ×2、本笔），同锚点手术面只会更大。重复标题对读者不可见（读起来就是一个正常条目）、对下一个复盘「已经写了什么」的 agent 是纯负债。
- **改动**：① `test_no_duplicate_unreleased_subsection_titles`——块内 `### ` 标题逐个建行号表，重名即报出两处行号（精确匹配：近义标题是作者的判断，不是机器的判决）；② `test_subsection_headings_are_not_glued`——标题行剥掉反引号代码段后不得再含 `### ` / `## `（粘行的孪生节会被读成一个标题叫「甲### 乙」的节，任何按节数做的检查都看不见这种丢失；标题里**示例性引用**标题是合法散文，所以先剥代码段）。
- **验证**：套件 7→14 例全绿；`unittest discover -s scripts` 87→94 例全绿；`check_doc_anchors` 30 锚点 × 6 本活账本 PASSED；真实 CHANGELOG（25 节）零重复、零粘行。**变异（在真实文件上做，做完逐字节还原）**：① 复制一个真实小节标题 → 查重断言红并点名该标题与两处行号；② 把两个真实标题行粘成一行 → 粘行断言红并报行号。合成夹具另覆盖：好夹具过、重复夹具红、**远距重复也红**（块内 200 行之深，对应真实事故里孪生节不相邻的形状）、粘行夹具红、标题里反引号引用 `### 甲` 不算粘行。
- **如实边界**：查重只覆盖 `[Unreleased]` 块——版本段内的历史小节不在手术面上，不纳入；CI 的 release-gate 逐套件点名是按文件名 `grep -q test_changelog_structure`，本笔只加方法、不动套件名，**ci.yml 无需改动**。

### 新增（`system status --remote`：按「远程只报运行事实」定案——GetStatus 不做策略判定，不放部署拓扑）

- **缺口**：`system status` 此前只有本地语义：报 `config_path` 与 `db_status` / `db_driver` / `db_location`，而远端 `GetStatus` 只有运行计数、`store_type` 与 warnings；两者被当成「同一命令的两种模式」会误导——本地探的是 CLI 自己的库，远程问的是 server 进程。
- **定案**：远程只报服务端 `GetStatus` 真正携带的运行事实（`status` / `active_runs` / `paused_runs` / `uptime_seconds` / `store_type` / `warnings`）加服务端 `version`，并用 `local_only` 数组明确列出 `config_path` / `db_status` / `db_driver` / `db_location` / `db_path` 不报。**不加宽 proto**：`GetStatus` 与 REST `/system/status` 都只做认证、不做策略判定，配置路径与数据库端点是部署拓扑，`GetConfig` 对这类数据有 `ActionAdmin` 门禁。
- **接线**：`system status` 进 `remoteCapableCommands`（第 10 条）；远程路径经 service_factory 只调 `GetVersion` + `GetStatus`，不加载本地配置、不打开本地库。`--quiet` 本地打印 `db_status`，远程打印服务端 `status`，文档写明二者语义不同。
- **测试**：`cmd_system_remote_test.go` 4 个用例——人类输出与 JSON 形状（含 `local_only` 五字段）、`--remote` 不创建本地库、连不上 server 必报错；`remote_mode_guard_test.go` 的固定名单同步到十条。
- **文档**：`cli-reference.md` §10.2 与 `--remote` 全局行、`levee-api.md` 第 9 章改为实际本地/远程两套输出，并订正同章两条不可执行的 config 示例（`server.url` 不可读、`output.color` 不在 5 键可写白名单）；`blocked-items.md` 的「仍待决」改为已收口。
- **验证**：`go test ./cmd/levee/ -run 'TestSystemStatusRemote|TestRemoteCapabilityList|TestValidateServiceMode|TestRemoteConsumersInCliReference' -count=1` 全绿。

### 新增（ABAC 清单侧 env 归属的**写侧**强制：默认拒绝，仅 `ImportTargets` 可点名豁免）

- **缺口**：`env` 在清单里有两个身份——策略谈的是哪些主机，也是谁能碰它们。读侧早已是 `target-ownership.yaml` 压过 `labels["env"]`（`internal/grpc/resource_authz.go` 三处 `ownership.Lookup`），但写侧（`AddTarget` / `ImportTargets`）一直按请求/文件**自报**的 env 判定门禁，从不问注册表。后果不是权限逃逸（策略层拿到的 labels 是用被判定的 env 现造的，不读库存行），而是门禁判错了对象、且按 env 过滤的主机视图读到假值。
- **定案（ADR-008，方案 D，P1）**：`AddTarget` 与注册表冲突即 `FailedPrecondition`，**无豁免**；`ImportTargets` 默认同样拒绝，只有 `allow_env_conflict` 点名才按文件写入，并写一条 `audit.action=import_env_conflict`。豁免**不绕过**原有 stored-env admin 检查。只挡新写；存量冲突行普查另开，启动时的冲突 WARN 保留。
- **判定位置**：`internal/inventory` 的 `EnvConflicts` 判的是**这次写入之后行上会留下的** env（对已存在行走 `mergedLabels`），所以「不带 labels 的再导入」也会按保留下来的值判——否则一条假 label 能靠沉默永远续命。地址/状态/通道会让 importer 直接失败的行不参与判定。
- **表面**：`proto/levee_extra.proto` 加 `bool allow_env_conflict = 3` 并重新生成；REST `POST /targets/import` 用同名 JSON 键，`FailedPrecondition` 映射 HTTP 412；CLI `levee target import --allow-env-conflict`（本地导入直连 importer，缺了它就是有记录的绕过口，豁免同样写审计）；serve 把已加载的注册表交给两个服务。
- **作用面上界**：注册表声明过的主机。未声明的主机继续按 labels/默认 env，每个拒绝用例都配了「未声明/无注册表时行为不变」的控制组。
- **文档订正（相邻缺陷）**：`cli-reference.md` §6.2 原写「每行一个主机名 / `#` 注释」，实际解析的是 YAML 清单；`levee-api.md` 的 `target import` 表里 `--merge` / `--replace` / `--dry-run`、`target list` 表里 `--label` / `--channel` / `--limit` 在 CLI 均不存在，`--status` 的合法值也不是 `reachable|unreachable|unknown`——已逐条按 `cmd_target.go` 的真实旗标订正。
- **验证**：`go test ./internal/inventory/ ./internal/grpc/ ./internal/state/ ./cmd/levee/ -count=1` 全绿；`golangci-lint run` 0 issues；`go vet` rc=0；`test_doc_anchors` 10 tests OK、`test_changelog_structure` 7 tests OK。

### 测试（旗标守卫扩面：`identity-deployment.md` 入扫，并修掉一个会让新文档一入扫就假红的行首匹配）

- **缺口**：`cmd/levee/cli_reference_flags_test.go` 的扫描名单收着 cli-reference / quickstart / deployment / levee-api 四本，而 `docs/identity-deployment.md`——SSO 与命名令牌的部署指南，其 §4 自称「三条命令，逐条可复现」——一直在名单外。它与 levee-api.md 同形状（裸 `levee …` 行、操作者照敲），却没有任何 CI 打开过它。
- **入扫后第一条红不是文档错，是门自己的假阳性**：`role_map` 的 YAML 键 `levee-admins: sre` / `levee-operators: operator` / `levee-viewers: viewer`（该文档 41–43 行）被 `^\s*levee(.*)$` 当成命令行，报出三个「文档写了不存在的命令」。命令行的形状是 `levee` 后接空白或行尾——永远不会是 `levee-` 或 `levee:`，故给正则补上尾部边界。
- **顺带修一条陈旧注释**：扫描名单的注释写着「表格行见 TestDocumentedCommandsExist」，而该测试**全仓不存在**——不存在的命令一直由本测试的 `cmd == nil` 分支负责报。留着这个引用只会让下一个人去找一个不存在的测试。
- **验证**：入扫 + 边界修复后 **583 处旗标引用对 cobra 树全绿**（门自带 ≥40 的地板断言，扫不到东西不会是绿）。文档内 5 条命令（`serve --token` / `authz status --json` / `authz explain` ×3）逐条真实注册，**文档一个字都不用改**。
- **变异（两条，均先红后还原）**：① 正则退回无边界形状 → 41–43 行假阳性红复现（证明文档确已入扫、边界修复承重）；② 往文档注入 `--resolution-hint` 假旗标 → 门在该行红并列出 `authz explain` 的真实旗标全集（证明真命令行也在扫，不只是 YAML 键那条路）。
- **如实边界**：续行命令（`levee serve --token <密钥> \` 接下一行 `--auth-token …`）只有首行入扫，续行旗标不在检查面——方向与该门「可以少记、不许记错」的既定口径一致（漏检在安全侧），本笔不收窄。

### 文档（`project-status-2026-11.md` 追加 §12：基线推到 e848f79 重测，并收回我上一批发布的一个排序错误）

- **兑的是自己的账**：§10 结尾写着"下一轮复核应以 `e6f6f34` 为新基线重测"，去兑时 master 已 `e848f79`——自 §10 基线 `9974b44` 前进 **10 笔**（本笔经两轮 rebase，最终 base `4c0b42db` = **15** 笔）。§1–§11 原文一字不改，只追加 §12。
- **复测（全部本轮重跑）**：`go test -count=1 ./...` rc=0、72 包 = 68 `ok` + 4 `[no test files]`、0 FAIL；`golangci-lint run ./...` rc=0 / `0 issues.`；PG（新建容器 `qcn12-pg`，跑完已 `docker rm`）`internal/state` **412** PASS（顶层 217 + 子 195）/ 0 SKIP / 0 FAIL / 48.1s，`internal/cluster` 50 PASS / 8.2s；proto **60** RPC（41 + 19）/ 10 service；blocked-items A5 / B0 / C3 / D3（活跃 2）/ E3 / F1；旗标守卫扫描面已从 `cli-reference.md` 扩到 `levee-api.md`。
- **收回一条**：§10 那行（`docs/project-status-2026-11.md:245`）写"其余均在 9s 以内"，**这个结论是排序方法错的产物**——我用 `sort -r` 对时长列做字符串倒序，于是 `"9.4s"` 排在 `"98.9s"` 之前、`"8.6s"` 能排到顶上。按数值排序的真实前三：`cmd/levee` **118.4s**、`internal/grpc` **113.0s**、**`internal/credential` 66.4s**（各包之和 664.4s，它一家占十分之一；该包密钥派生走 argon2id，见 `internal/credential/store.go:7`，但我没做逐用例计时，这条只到包级）。教训写进 §12：计数文档里"其余都在 X 以内"这类断言必须能按数值自己复算。
- **新登记的缺陷（只记不改，`roadmap.md` 此刻在别人 owns 里）**：`docs/product-roadmap.md` 的"未决行数"**没有唯一答案**——同一份 47 行表，判据 A（优先级格有闭合标记）给 **11**、判据 B（任一格有标记）给 **1**、判据 C（B + 标题删除线视为已闭）给 **1**，人工逐行回读是 **6**；**三个判据各自可复现**（同一实现在 `e848f79` / `4c0b42db` / 本笔 HEAD 上各跑一遍，都是 11/1/1），问题是判据不自明而不是跑不出来。根因是闭合状态有四种互不兼容写法：优先级格 `✅ 已落地（…）`、**尾列** `✅ 已落地`、标题 `~~划掉~~`、以及 `:36` 的「已修」（不在任何词表里）。更严重的是结构性漏项：**运营层那张表行首不是 `| P<n>`**（`:96`–`:98` 是两列 `| 项 | 目标 |`），任何 `^| P` 统计整类看不见这三行——我自己就先按"6 行"报过一次，回读才发现其中三行从来不在统计面上。最小修法（留给后续，需要动的文件正在被并行线改）：闭合状态收成固定一列取值 `open|closed|blocked-on:<谁>`、词表单一来源、脚本门禁数 `open` 并钉"每行必须有这格"，运营层表并入同一形状。
- **状态同步**：ADR-008 的五问由 owner 定案（降 **P1**、方案 **D**、只挡新写、注册表为其声明过的主机的唯一权威、豁免只给 `ImportTargets`），**且实现已落地**——`3be8895`(11-05 02:50+08) + `4c0b42d`(03:54+08)，ADR 状态行现为「已定案并实现」。§12 因此把这条从"实现中"改成"走完 登记 → 材料 → 实现 三段、两天闭环"。我在 roadmap 登记的 `levee-api` 第 6 章缺陷已由 §11 收口。
- **新登记一条环境事实（本笔为此在本地待命 2 小时 55 分，18:57Z→21:53Z）**：CI 的 **macOS arm64** 腿会被池子拒绝领取——job `126767273532` 实测 `conclusion=cancelled` 且 **`runner=null`**，注解 `The job was not acquired by Runner of type hosted even after multiple attempts` + GitHub 的 arm64 容量公告；聚合器把 cancelled 计成 failure，于是 master 的 `41885083482` 与本 PR 的 `41874663901` 都在**内容腿全绿**的情况下变红。本轮用只读看守采 macos 腿判恢复（18:57Z 8 腿 3 cancelled ⇒ 21:53Z 9 腿 0 cancelled 才推送），并写明"先取证再决定等池子还是 `rerun --failed`"。
- **协调面观察**：派单号 `PI-LEVEE-004/005` 既不在仓里也不在板目录里（板目录只有各线自己的声明文件，那两个号只作为引用出现在正文）⇒ **"看板上没有待办"不等于"项目没有待办"**，只读板找活的人会得出错结论。
- **推送前按 base 复算，本节初稿又收回四条（两轮 rebase：`8ab41428` → `4c0b42db`）**：base 从 `e848f79` 挪走之后，把所有**指向别人的坐标**重读两遍——① 旗标守卫的行号 `:49`/`:65` → `:54`/`:73`；② 扫描面实为 **5** 本，`identity-deployment.md` 已入扫，不再是"正在另一条线上接"；③ "`#272` 还没进 master" **是错的**（`74653e18` 于 2026-11-03T23:13Z 合并，已是 `e848f79` 的祖先；但它对 `proto/levee.proto` 的 54 行只加 message 字段、新增 RPC 为 0 ⇒ 41/19/60 三个计数不过期）；④ 板面成员与"板只有三条声明"按复读时刻改写。全量 / lint / PG 三类墙钟数字**没有**在 `8ab41428` 上重跑，§12 的按语已如实区分这两类。
- **还收回一条自己的盲区**：初稿写"浅克隆未 unshallow、谱系判断只靠内容比对"，复算阶段实测 `git rev-parse --is-shallow-repository` 已是 **false**（历史被某次 fetch 补全）⇒ 本节所有祖先判断都以 `git merge-base --is-ancestor` 为准（例如 `#272` 的 `74653e18` 确在 `e848f79` 里）。
- **验证**：`python scripts/test_changelog_structure.py` 7 tests OK；`python scripts/check_doc_anchors.py` PASSED；零产品代码改动 ⇒ 不动测试套件（上表的全量/lint/PG 是**为核对文档数字**而跑的，不是本笔代码的验收）。

### 文档（`levee-api.md` 第 6 章 drift 命令口径收口：正文早已订正，补掉仍在引用旧 `scan/show` 的台账）

- **缺口**：`docs/levee-api.md` 第 6 章正文已于 2026-11-01 按实际 CLI 订正为 `drift detect` / `drift report`，但 `docs/review-report.md` 仍把第 6 章写成 `scan/show` 并判「一致」，`docs/product-roadmap.md` 也还挂着「登记不修」，`docs/mvp-plus1-roadmap.md` 的 F10 行还用 `drift scan` 指代已有能力——同一条旧口径被三处台账继续背书。
- **改动**：① `review-report.md` 的两处旧口径加 2026-11-04 注记（保留 2026-08-15 审核原文）；② `product-roadmap.md` 第 36 行改为「已修」并写明实际命令与旗标；③ `mvp-plus1-roadmap.md` 的 F10 行把 `drift scan` 改为 `drift detect` / `drift report`；④ `levee-api.md` 第 6 章补 `drift report --host/--days` 说明，并把 `drift baseline` / `drift schedule` 的完整旗标指向 `cli-reference.md` 第 22 章；⑤ `project-status-2026-11.md` 追加 §11 收口记录。
- **未做**：`drift scan` / `drift show <report-id>` 没有实现；要新增这两个接口需要报告 ID 存储与读路径，另开接口项。
- **验证**：`go test ./cmd/levee/ -run TestDocumentedFlagsAreRegistered -count=1`、`python scripts/check_doc_anchors.py`、`python scripts/test_changelog_structure.py` 均通过。

### 文档（`AGENTS.md` 补两条本机已付代价的坑：`--unshallow` 后的假红 fsck、浅克隆的假祖先判据）

- **缺口**：`AGENTS.md` 的「本机上已经付过代价的几件事」收了 7 条，但**解浅这一条没写**。2026-11-04 做远端分支对账时按规矩先 `git fetch --unshallow`（浅克隆下 `git rev-list --left-right --count` 会把已完成的分支报成 `ahead=700+`，`git branch --no-merged` 也是假阳性），解浅后 `git fsck` 立刻红：5 个 `Could not read <sha>` 与 `failed to parse commit … from object database for commit-graph`。**症状极像仓库损坏**，最容易被误处置成重新 clone。
- **定性三步（这才是本条真正想留下的东西）**：① `git rev-list --objects HEAD` rc=0 且无 `missing` ⇒ master 全史对象齐；② `git cat-file -e <sha>` rc=1 ⇒ 它本来就不在对象库里；③ `gh api repos/Levango7/Levee/compare/<sha>...master` 报 `diverged` / `behind_by=1` ⇒ 那 5 个全是当年没合进来的 dependabot PR 头，**历史没有洞**。三步走完才能说"这是缓存"，不是"看起来不像丢东西"。
- **修法与边界**：删 `.git/objects/info/commit-graphs/`（split 链）或 `.git/objects/info/commit-graph`，再 `git commit-graph write --reachable`；复验 `git commit-graph verify` 与 `git fsck` 均 rc=0（本机实测如此）。commit-graph 只是加速缓存，删掉后 git 回落到 pack index——代价是日志查询变慢，不是数据风险。
- **顺带把"假祖先判据"独立成一条**：解浅只解决"数得对"，不解决"判得对"。rebase 落地会重写 sha，`git merge-base --is-ancestor` 于是假阴性；**真判据是 `git cherry <base> <branch>` 的 `+`**（补丁等价，改 sha 也不误判）。这一条与 `docs/project-status-2026-11.md` §2.2 的观察同源，但那段是快照、下一个 agent 未必读；`AGENTS.md` 是他动手前必读的那一页。
- **验证**：`python scripts/check_doc_anchors.py` PASSED（30 锚点 × 6 本活账本，`AGENTS.md` 在扫描范围内）；`python scripts/test_changelog_structure.py` 7 tests OK。纯文档，无产品代码与生成物改动。

### 修复（`docs/levee-api.md` 的 20 处 CLI 示例与假命令：照着敲会 unknown command / unknown flag）

- **根因（一个此前没人读它的文档面）**：`cmd/levee/cli_reference_flags_test.go` 的扫描列表只有 `cli-reference.md`、`quickstart.md`、`deployment.md`——**`levee-api.md` 从来不在其中**。于是一整本 API 参考的 CLI 示例章没有任何守卫，它的假命令/假旗标从未被 CI 发现。
- **症状（把该文档纳入守卫后一次暴露 24 处红，24 处全部订正）**：
  - **根本不存在的命令**：`levee delegate`（转授权）与 `levee rollback-all`——grep 全仓、CHANGELOG、blocked-items、cli-reference、`git log -S` **均无**，即它们从未实现过、也从未登记为不做。已在示例、2.4 选项表、附录 A、附录 B 四处标注"未实现"并给出替代做法（`--as` 代签 / 审批人本人 approve / 按 run-id 循环 rollback）。
  - **层级写错**：`levee config get/set` / `levee status` / `levee doctor` 实际是 `levee system config|status|doctor` 子命令。示例、端点表「对应 CLI」列、附录 A 一并订正。
  - **不存在的旗标**：`list --target`、`rollback --batch`、`logs --step`、`link --itsm/--jira`、`template list --limit`、`template create --file`、`target list --label`、`target import --merge`、`target remove [--force]`、`audit list --from/--to/--who/--action`、`audit export --from/--to/-o`、`secret list --type/--team`、`secret add --file/--team`、`user list --team`、`team add --approver`。逐个按真实 flag 订正（`--content` 而非 `--file`、`--value "$(cat …)"` 而非 `--file`、`--group` 而非 `--label`、`--incident` 而非 `--itsm/--jira`、`--default-group` 而非 `--merge`；无对应能力的则删条件并注明，如 audit 过滤无 CLI 入口、重定向代替 `-o`）。
- **落地**：`cli_reference_flags_test.go` 的扫描列表加 `docs/levee-api.md`（附为何它此前在外的注释）。
- **验证**：纳入后从 24 红到 0 红；`go build`/`go vet` rc=0；`gofmt` 干净；`go test -count=1 ./cmd/levee/` 全包 ok（84.9s）；`golangci-lint run ./cmd/levee/`（v2.13.2 与 CI 同版）0 issues；`scripts/test_changelog_structure.py` 与 `test_doc_anchors.py` OK。
- **变异**：把第 114 行改回 `--target group=web` → 守卫立刻红并点名（`levee list: target — documented but not registered` 同时列出 list 的真实旗标全集）；还原后绿。
- **如实边界**：附录 A 的「其他域」表只列 41 条命令，而 cobra 顶层有 54 条（`agent`/`alert`/`system`/`tenant`/`web` 等族整体未列）。本笔**不做反向穷举**——该表自称"速查"，漏列与 cli_reference_flags_test.go 对旗标的口径一致（可以少记，不许记错）；守卫的方向因此是单向的。

### 修复（`--remote` 的帮助文本对操作员说谎：名单早就是六个，帮助还写"当前仅"三个）

- **症状**：`cmd/levee/root.go` 的 `--remote` flag 用法写死「当前仅 agent list/show/remove」，而 `remoteCapableCommands` 早已长到六条（`version` / `system version` / `template list` 于 2026-11-02 两笔落地）。`docs/cli-reference.md` 的全局选项表是对的，`levee --help` 是错的——**操作员先读的是帮助文本**。
- **根因（本仓头号缺陷类的又一实例）**：名单有两条测试死死钉住（`TestRemoteCapabilityListResolves` / `TestRemoteCapabilityListIsExactlyThese`），帮助文本却是一份手写的副本。副本不会跟着名单长。
- **修法（单一来源，不是同步两份）**：`remoteFlagUsage()` 从 `remoteCapableCommands` 渲染用法串，且刻意**不写"当前仅"也不写数量**——集合会长，写死的排他句是今天真、下周假的形状。
- **守卫（三条，把两处现场都钉住）**：① `TestRemoteFlagHelpStaysAlignedWithTheCapabilityList`——渲染串必须点名名单里每一条，且不许点名名单外的命令（解析"当前由 … 消费"段反向对账），并不许回到"当前仅"式措辞；② `TestRemoteConsumersInCliReferenceMatchTheList`——把 `docs/cli-reference.md` 的全局选项表也纳入同一套双向对账（该表是手写的第二现场，#272 落地时它一定会被改，守卫让"忘了同步"变红而不是留一条注释）；③ 解析器自带形状过滤（`--server` / `[exit=2]` / `false` 这些非命令记号不会被当成命令）。
- **顺带订正一处账本漂移**：`docs/product-roadmap.md` 的 `service_factory` 行仍写「P2 维持现状（非阻塞）；『接线 or 退役』仍属可选决定」，而 `docs/blocked-items.md` 同一条已写「已接线（2026-11-02）」。两处对同一事实给出了相反答案——按代码事实（`e5f1587` version/system version、`22a7153` template list 均已经工厂）订正 roadmap 行，并保留真正仍是可选决定的那一半（`buildLocalServices` 覆盖的其余服务尚未全部迁到工厂）。
- **验证**：`go build` / `go vet` rc=0；`gofmt` 干净；`go test -count=1 ./cmd/levee/` 全包 ok（61.5s）；`golangci-lint run ./cmd/levee/`（v2.13.2 与 CI 同版，`--allow-parallel-runners`）0 issues；`scripts/test_changelog_structure.py` 与 `test_doc_anchors.py` OK。
- **变异（四条均先红后还原）**：① 渲染串多提一个 `target list` → 反向断言红；② 渲染串漏掉名单末条 → 正向断言红（点名 `template list`）；③ cli-reference 的共享组 `agent list / show / remove` 改成 `/ show` → 红（点名 `agent remove`）；④ 同表多提一条 `target list` → 红。
- **如实边界**：`--remote` 的消费面还会长（#272 在飞：target list / audit verify / template show）。届时 `remoteCapableCommands` 一改，`root.go` 的渲染**自动跟上**，但 `docs/cli-reference.md` 仍要人改——守卫会把漏改变红，这是它存在的理由，不是遗留缺陷。

### 新增（`template show --remote`：先把参数声明补全，再接线）

- **为什么不能直接接**：本地 `template show` 打印**全部**参数声明——名称、类型、必填与否、默认值、描述，`--json` 直接输出 `template.Template`。而 `pb.Template.required_params` 只带必填参数的**名字**：可选参数、类型、默认值与描述在线上根本不存在。照着渲染会静默少一半内容。
- **补线上字段**：`pb.Template` 新增 `repeated TemplateParameter parameters = 9`，镜像 `template.TemplateParam`（name / description / required / default / type）；`domainTemplateToPB` 填它，`required_params` 保留兼容既有消费者。生成仍用钉住的 protoc v27.0 工具链。
- **渲染收成一份**：`internal/template` 导出 `FormatShow`（`Show` 改调它），CLI 两种模式都走它——远程 `GetTemplate` 的响应经 `templateFromPB` 还原成 `template.Template` 后，与本地 `lib.Get` 的结果共用 JSON / quiet / human 三个渲染分支。空 tags/parameters 归一为 `nil` 保持 `omitempty` 一致。
- **错误也要一致**：服务端对缺失模板回 `NotFound`，CLI 把它映射回 `template.ErrTemplateNotFound`——两种模式都输出 `get template: template: template not found`，而不是让远端多一个 `rpc error: code = NotFound` 前缀。
- **证据**：`template_show_remote_test.go` 4 个用例。① 参数丰富与裸模板各跑 human / `--quiet` / `--json` 六组，**逐字节相同**（裸模板专门抓「空 map/slice 被转成非空」导致的 JSON 差异）；② 缺失模板两边同一条错误；③ 死端口必须报错（等价用例读同一份库内容，分不出走的是哪条路）；④ 远程不建本地库。①里还带一条夹具自证：human 输出必须真的含 `replicas (int, optional) default="2"`，否则「相同」可能只是两边都缺。**变异（还原后 `sha256` 逐字节相同）**：服务端不带 `parameters` → 等价与自证红；丢掉 `updated_at` → JSON 等价红；CLI 去掉 NotFound 映射 → 错误一致性红；把远程分支短路成本地 → 死端口用例红。
- **如实标注**：`--json` 的 `created_at` / `updated_at` 本地含亚秒、远程只到秒；人类输出用 RFC3339，两种模式相同（写进 `docs/cli-reference.md` §5.2）。
- **仍不接**：只剩 `system status`（与 `GetStatus` 语义不同），见 `docs/blocked-items.md`。

### 新增（`audit verify --remote`：线上不再只报第一条篡改）

- **为什么不能直接接（两处）**：① 本地逐条打印每个被篡改记录（`trace` / `index` / `type`，`--json` 还带 recomputed 与 stored 两对哈希），而 `VerifyHashChain` 只把 `failures[0]` 塞进 `broken_entry_id` / `broken_reason`——**多行篡改在远端会看起来像单行**，这是安全判定，不是显示差异；② 服务端「验证全部 run」时会把没有 trace 的 run 直接跳过，对**具名 run** 的请求就退化成 `valid=true, 0 条`，而本地对同一输入报 `ErrNoTraces`——远端等于宣布一条不存在的链是完好的。
- **补线上字段**：`pb.RunVerification` 新增 `repeated AuditChainFailure failures = 6`，字段与 `audit.ChainFailure` 一一对应（`trace_id` / `index` / `expected` / `actual` / `prev_expected` / `prev_actual` / `type`）。`type` 用**数字**而不是字符串，因为本地 `--json` 打印的就是 `ChainFailure.Type` 的整数值（没有自定义 marshaler），字符串会逼出一个有损的反查表。`broken_entry_id` / `broken_reason` 保留为「首条」以兼容既有消费者。生成仍用钉住的 protoc v27.0 工具链。
- **服务端**：为每个 run 填**全部** `Failures`（不再只取 `[0]`）。
- **接线**：`runAuditVerify` 先 `resolveServiceMode()`；远程经工厂 `GetAuditService().VerifyHashChain`，把响应还原成 `audit.VerifyResult`，与本地**共用同一个渲染函数**（JSON 信封、human 行、`[exit=6]` 文案全在一处）。空 `runs` 还原成 `verify chain: audit: no traces found for run`，与本地逐字一致。`--remote` 白名单扩到 8 条。
- **证据**：`audit_remote_test.go` 6 个用例。① 完好链三形状（human / `--quiet` / `--json`）**逐字节相同**；② **同时伪造两行**内容：两边都报 `2 of 3 records tampered`、逐条 failure 逐字节相同、stdout+stderr+error 三方相等——只伪造一行的测试会被「只回首条」的实现骗绿；③ 删除中间一行（`prev_hash_mismatch`，type=1）：覆盖 `type/index/prev_*` 这些在内容篡改里恰好等于零值的字段；④ run 不存在/没有 trace：两边报同一句 `ErrNoTraces`；⑤ 死端口必须报错（等价用例读同一份库，分不出走的是哪条路）；⑥ 远程不建本地库。**变异（还原后 `sha256` 逐字节相同）**：服务端只保留首条失败 → ② 红；丢掉 `Type` → ③ 红（`"Type":1` 变 0）；丢掉 `PrevExpected` → ③ 红；跳过空 `runs` 的拒绝 → ④ 红；把远程分支短路成本地 → ⑤ 红。
- **仍不接**：`template show`、`system status`，阻塞点见 `docs/blocked-items.md`。

### 新增（`target list --remote`：线上补字段 + 分组按名字解析）

- **为什么不能直接接（两类缺口）**：本地 `target list` 渲染的是完整的 `state.Target`——`--json` 里带 `created_at` / `last_checked_at` / `tenant_id`，而 `pb.Target` 三个字段都没有；`--group` 接受的是分组**名字**（`prod/db`），本地用 `GetInventoryGroupByName` 解析成 ID，而 `ListTargetsRequest` 只认 `group_id`，线上根本没有按名字查组的 RPC。硬接会做出「远程少三列、`--group` 只能填内部 ID」的版本。
- **补线上字段**：`pb.Target` 新增 `created_at`（10）、`last_checked_at`（11，0 表示从未探测，对应可空列）、`tenant_id`（12）；`ListTargetsRequest` 新增 `group_name`（7），服务端用 `GetInventoryGroupByName` 解析，**`group_id` 优先**。未知分组返回 `NotFound` 且消息与本地逐字一致（`group "x" not found`）——返回空列表会让拼错的分组名看起来像「这个组没有主机」。生成仍用仓库钉住的 protoc v27.0 工具链，**改前先做对照重建**（未改 proto 时 `git diff` 为空）。
- **接线**：`runTargetList` 先 `resolveServiceMode()`；远程经工厂 `GetTargetService()`、按 `PageSize=1000` 翻页取全量、`--status` 客户端过滤（RPC 无该字段），再经 `stateTargetFromPB` 还原成 `state.Target`，与本地**共用同一个渲染函数**。空 `labels` 映射归一为 `nil`，让 `omitempty` 在两种模式下都省略该键。
- **顺带修正人类输出的不确定性**：标签列表原先按 map 迭代顺序拼接（每次运行都可能不同），现在排序——否则「两种模式逐字节相同」这个断言本身就是随机的，只能靠运气绿。这也让重复执行本地命令的输出稳定。
- **证据**：`target_remote_test.go` 7 个用例（三形状是同一用例里的三个子测试）。①②③ 同一库存、两种模式、**逐字节相同**（human / `--quiet` / `--json`）；④ `--status frozen` 两边同结果；⑤ `--group prod/db` 两边同结果（走 `group_name`）；⑥ 未知分组**两边报同一句话**；⑦ 每页 2 条跑 3 条数据，证明翻页真的交接了 token；⑧ 远程不建本地库；⑨ 指向死端口时**报错且无输出**——两种模式读同一份库时，只有这一条能证明远程分支真的在拨号，而不是悄悄退回本地。**变异（还原后 `sha256` 逐字节相同）**：转换器去掉 `CreatedAt` → JSON 等价用例红；去掉 `LastCheckedAt` → 同上；服务端不解析 `group_name` → 分组用例红；删掉翻页循环里的 token 交接 → 分页用例红；把未知组改成返回空列表 → 拒绝用例红；把远程分支短路成本地（`&& false`）→ 「远程不建本地库」用例红（同时说明等价用例本身分不出这两种模式读同一份库）。
- **如实标注的一处差异**：`--json` 的 `created_at` / `last_checked_at` 本地含亚秒、远程只到秒（线上是 int64 秒）。等价测试用整秒时间戳规避这一维，避免把「精度差异」当成「接线差异」；该差异写进 `docs/cli-reference.md` §6.1。
- **仍不接**：`template show`、`audit verify`、`system status`，阻塞点同前，见 `docs/blocked-items.md`。

### 文档（`project-status-2026-11.md` 追加 §10 复核：基线推到 9974b44，四个数字复测、两处表述要限定）

- **写法**：带日期的快照**只追加不重写**——§1–§9 原文一字未改，新增 §10 写明基线（`482af47` → `9974b44`，前进 **21** 笔）、复测命令与实测值，失准处单列"要限定/收回的表述"。这条写法本身作为机制样本收进 §10 末。
- **复测（都是本轮命令输出，不是引用）**：`go test -count=1 ./...` rc=0，**72** 个包 = 68 `ok` + 4 `[no test files]`，`^FAIL` 0；`golangci-lint run ./...` rc=0 / `0 issues.`（本机 v2.13.2，CI 钉 `v2.13`，权威仍是 CI 的 `lint` job）；RPC 计数独立复算 **60** = `levee.proto` **41** + `levee_extra.proto` **19**、服务 **10**、`ChangeService` **23** ⇒ §7 第 5 条的订正仍成立；浅克隆 `git rev-parse --is-shallow-repository` = true（主检出与本工作树都是）⇒ §2.2 前提未变；blocked-items 逐类 A 5 / B 0 / C 3 / D 活跃 2 / E 3 / F 1 ⇒ §5.2 那张表准确。
- **PG 那条从"自报"升级成实测**：另起容器 `qcn-recheck-pg`（`postgres:16-alpine`，`127.0.0.1:55442`）而不是复用 §9.1 那台共享库——本机此刻跑着别人的一批容器（kind 集群、`levee-lab-1..3`），共用端口或库会把别人的状态卷进我的结论。实测 `internal/state` 单独 **412** 个 PASS（顶层 217 + 子用例 195）、**0 SKIP**、0 FAIL（44.6s），带 `internal/cluster` 合计 462（cluster 9.5s）。§9.1 那句"412 PASS / 0 SKIP"**逐位复现**，含它的计数口径。
- **新增一条以前没记的事实**：全量测试的墙钟几乎全在两个包里——`internal/grpc` **98.9s** + `cmd/levee` **82.7s**，其余包都在 9s 以内。估 CI 等待时别按平均包耗时摊。
- **两处表述要限定（旧条目保留原文）**：① §8.1 的 PR #262 / #263 **均已合入**（`03849c9`、`3386213`），不再是待办；② §5.2 末句"唯一真正未开工的代码项是 D-1c"在 `blocked-items` 的 D 类里成立，**作为全局陈述不成立**——roadmap 第 27 行那条 P0（ABAC **写侧** env 归属强制）从未开工，`#272` 的 `--remote` 系列也在推进同一批文件；准确措辞是"搁置项里唯一未开工的代码项"。
- **顺带把本轮另一件事接上账**：§8.3 那一问「清单写侧是否按 registry 强制」现在有 ADR-008（`docs/adr/ADR-008-abac-write-side-env-ownership.md`）承接四候选与五问，并收回了它的定级基础（条件策略 labels 由被判定 env 现造，`internal/authz/authz.go:435` ⇒ 不构成权限逃逸）。
- **本轮验证盲区照实登记**：没跑前端腿（`web/` vitest 与 `browser-check`）、没跑全量 `-race`、PG 用的是新建空库（覆盖同一套测试代码而非同一份数据）、浅克隆未 unshallow。
- **验证**：`python scripts/check_doc_anchors.py` PASSED（27 锚点 × 6 本活账本）；`python scripts/test_changelog_structure.py` 7 tests OK。零产品代码改动。

### 新增（ADR-008：ABAC 清单侧 env 归属**写侧**强制的裁决材料——四候选、一推荐、五问待 owner）

- **认领的是 roadmap 里那条 P0 的"仍待做"半边**（写入路径不按 `target-ownership.yaml` 强制）。本笔**只出材料、不动产品代码**：`docs/adr/ADR-008-abac-write-side-env-ownership.md`。
- **最重要的发现是这条的定级基础站不住**：本行原暗示"dev 能把 prod 机器接走"级别的逃逸，实测**不成立**——条件策略层的 labels 由被判定的 env 现造（`internal/authz/authz.go:435`），不读库存行，所以假 label 换不到额外许可。真实失真面只有两处：门禁按**自报** env 判定（`internal/grpc/target_service.go:133`），以及清单行可以对平台自己的权威事实说谎（读侧注册表压过 labels，`internal/grpc/resource_authz.go:86`）。另一条限制作用面：写侧强制只在注册表**声明过**的主机上有意义，而注册表覆盖率本机无真实 inventory 可测。
- **已吃掉的一半写清了**：`ImportTargets` 不但判文件声明的每个 env，还先预览再要求"把主机从哪个 env 拿走"需要那个 env 的 admin，且首个写入前拒绝（`internal/grpc/inventory_service.go:151-165`）——所以残余确实只剩"首次写入时的 label 选择"。
- **四候选 + 推荐**：A 写入即拒绝（fail-closed，但会把历史失真变成"今天起改不动"，例行 import 会被挡）；B 按注册表静默纠正（**明确不推荐**，理由写进 ADR：它让 env 同时活在两处，合并规则从读侧一条扩散成读写各一条，正是本仓反复出现的平行词表缺陷）；C 只在写入时告警（把 boot-time 那一次提醒搬到每次写入，适合当第一跳不适合当终态）；**D＝A + 调用方点名的豁免字段**（与本仓既有姿态同源：`RemoveAgent` 的 refusal、E-3 的 `allow_irreversible` 白名单）。
- **五问待答**（不定级不实现）：① P0 还是降 P1；② A 还是 D，C 是否可作第一跳；③ 存量冲突行只挡新写还是同时普查报数；④ 注册表是否为 env 的唯一权威（若是，`labels["env"]` 该不该降级成派生值——那是数据模型变更，ADR 不主张但要表态）；⑤ 豁免给不给单主机 `AddTarget`（倾向只给批量 import）。
- **与并行线的关系写在文档里**：本议题的三处线头中 `internal/grpc/target_service.go` 在 qoder-a26 在途 PR #272 的**实际 diff**里，`cmd/levee/cmd_serve.go:1650`/`:1657` 与 `cmd/levee/service_factory.go:137` 在它的 **owns 声明**里（后者刚被 #270 动过）。所以实现另开 PR、且接线部分等 #272 落地后 rebase。
- **验证**：`python scripts/check_doc_anchors.py` PASSED（30 条锚点 × 6 本活账本仍可解析——本笔往 `docs/product-roadmap.md` 第 27 行追加的那三处 `file:line` 在其中）；ADR 引用的**全部**代码锚点逐条按 `9974b44` 复核行号（初稿是从停在 `dcfa951` 的主检出抄的，`cmd/levee/cmd_serve.go` 与 `cmd/levee/service_factory.go` 三处已漂并改回）。产品代码零改动，因此不跑测试套件。

### 文档（audit 链封链读放大：量化 + 定档「维持现状」，并给出重新立项条件）

- **起因**：`internal/audit/auditchain.go` 的 `walkChain` 注释长期登记「sealing still walks every visible row, and Record calls it per write … a change to what a broken chain means, so it is not folded in here」——这句话**从未被量化**，也没有任何基准或测试钉住它。
- **量化（本机 SQLite，2026-11-02，`internal/audit/benchmark_audit_seal_test.go`）**：稳态链上 `audit.Record`（插入+封链）100 行 **0.53ms** → 1000 行 **3.41ms** → 5000 行 **18.34ms**，线性 ~3.5µs/行；其中 `Seal` 占 51%/70%/**95%**——**瓶颈是封链不是插入**。外推 1 万行 35ms、10 万行 350ms。审计写不是冷路径（审批/apply/pause/登录/凭据轮换/目标机变更都写），所以这是一条「越用越慢」的写路径。
- **为什么不能顺手增量**：`Seal` 的篡改判定 `ComputeAuditHash(a, a.PrevHash) == a.CurrHash` 是**单点性质**（只用行自身 prev，不需顺序累积）⇒ **跳过已封前缀 = 跳过对前缀内每行的写时篡改校验**。WORM 触发器（`internal/audit/worm.go`）是数据库层第一道防线，Seal 是纵深防御；把它从「每次写」降为「只在 Verify 时」属安全姿态变更，不该由一次性能 PR 做掉。
- **顺带排除「窄投影读」方案**：稳态判定第一支是 `a.PrevHash == prev && a.CurrHash == want`，而 `want` 是对**内容字段**算的哈希——不算它就无法判定稳态、也算不出下一行的 prev，所以内容列一行都省不掉。
- **定档**：维持现状。交付 ① `internal/audit/benchmark_audit_seal_test.go`（把数字固化为可复现输出，非 CI 门禁——CI runner 测的是 runner；但其**线性形状**是结论的一部分）；② `docs/audit-chain-scale-design.md`（量化、四方案对比、定档理由、C/D 的 5 条重新立项触发条件、已知边界）；③ `walkChain` 注释改为指向该文档并保留一句话事实，避免两处各自过期。
- **行为变更：无。** `Seal`/`Record`/`Verify`/`Rebuild` 语义逐字未动，仅新增基准与文档、一处注释。
- **验证**：`go vet` rc=0；`gofmt` 干净；`go test -count=1 ./internal/audit/` 全绿；`golangci-lint run ./internal/audit/`（v2.13.2，与 CI 同版，`--allow-parallel-runners` 绕开他人在跑的全局锁）0 issues；`go run ./internal/docgen -check` up to date；`scripts/test_doc_anchors.py` 与 `test_changelog_structure.py` OK。

### 新增（`template list --remote`：先把线上字段补齐，再接线）

- **为什么不能直接接**：本地 `template list` 打印 `id / name / description / created_at`，而 `pb.Template` **不携带 id 与 tags**——ID 列没有来源，`--tag` 没有可过滤的东西。更糟的是 `serve` 用 `NewTemplateService(store, nil)` 建服务，那份「内存注册表」只被 RPC 的 `CreateTemplate` 填过，而 `levee template create` 写的是**文件库**：直接接上去，远端会对着一个有几份模板的部署回答「没有模板」。
- **补线上字段**：`pb.Template` 新增 `string id = 7` 与 `map<string,string> tags = 8`（`proto/levee.proto`），`domainTemplateToPB` 随之带出。生成用仓库钉住的 protoc v27.0 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2 重跑 `scripts/gen-proto.sh`；**先做了对照**：未改 proto 时重新生成 `git diff` 为空，证明本机工具链与签入文件可复现。
- **修服务端读错源**：`serve` 改为 `template.NewTemplateLibrary(templateDir(cfg))` 并交给 `grpc.NewTemplateService(store, tmplLib)`——从此服务端读的就是 CLI 写的那份目录。附带的行为变化：**RPC 建的模板现在落盘**（此前只在内存里，重启即失忆），`InstantiateTemplate` 也走文件库分支（它本来就按 `s.lib != nil` 分支，只是以前永远是 nil）。
- **接线**：`template list` 先 `resolveServiceMode()`；远程分支按 `PageSize=1000` 翻页取全量、`--tag` 在客户端按 `tags` 过滤，三种输出形状（human / `--quiet` / `--json`）与本地**共用同一个渲染函数**，避免两边各写一份。`--remote` 白名单扩到 6 条。
- **证据**：`template_remote_test.go` 4 例。①②③ 同一份文件库、两种模式、**逐字节相同**（human / quiet / json 三种形状各一条）；④ `--tag tier=prod` 两边都只剩同一条。另加两处结构守卫：`TestServeServesTheFileLibrary` 读 `cmd_serve.go`，钉住「必须打开 CLI 那份目录、不许回到 `NewTemplateService(store, nil)`」——等价测试自己给服务器传了 library，看不见 serve 的这一半；`TestTemplateListRemoteCreatesNoLocalDatabase` 断言远程不建本地库。**变异三处（还原后 `sha256` 逐字节相同）**：转换器去掉 `Id` → human/json 等价用例红；去掉 `Tags` → `--tag` 用例红；把 serve 改回 `nil` → 结构守卫红。
- **如实标注的一处差异**：`--json` 的 `created_at` 本地是含亚秒的完整时间戳，远程只到秒（线上是 int64 秒）。等值测试用整秒的 `CreatedAt` 规避这一维，避免把「格式差异」当成「接线差异」；该差异已写进 `docs/cli-reference.md` §5.1。
- **仍不接**：`template show`——线上只有 `required_params`（丢了可选参数、类型、默认值与描述），照着渲染会静默少一半内容；`target list` / `audit verify` / `system status` 的阻塞点同前一笔，一并登记在 `docs/blocked-items.md`。

### 新增（service_factory 接线：`version` / `system version` 两种模式同一个入口）

- **背景**：`cmd/levee/service_factory.go` 是双模式（`--local` / `--remote`）的适配层，但**非测试代码零调用**——它维护着五套适配器而生产一个都没走。本笔把第一个真实调用点接上，并顺手把工厂改成**按需构造**。
- **为什么先接这两条**：它们与线上类型是**无损对应**的——`VersionInfo` 的四个字段正好是 `version` / `build time` / `commit` / `go version`。远程模式回答的也是操作者真正会问的问题：「server 跑的是哪个构建？」其余只读命令族**不是缺工厂而是缺线上字段**（`template list` 读文件库而 serve 的 TemplateService 用 nil lib；`target list` 渲染的字段多于 `pb.Target` 且 RPC 会分页+环境过滤；`audit verify` 本地给每条 failure 的 index/哈希而 `VerifyHashChain` 只回首处断点；`system status` 报 config 路径/库位置而 `GetStatus` 报运行计数）。硬接会做出「远程显示一个更少的版本」而不自知，所以逐条登记在 `docs/blocked-items.md`，不在这笔里凑数。
- **工厂改为按需构造**：`localServices` 不再在 `newServiceFactory` 时把五个服务全建出来，而是每个 `Get*Service` 现建现用；store 只在**真正读它的服务**上要求（`change` / `target` / `audit`），`system` 与「有 library 的 template」不需要。否则接 `version` 会顺带打开并创建本地 SQLite——打印四个字符串不该有数据库副作用。
- **接线**：`fetchVersionInfo(ctx)` 先 `resolveServiceMode()`，再向工厂要 `GetSystemService()`，local 读二进制变量、remote 走 `GetVersion`；根命令 `levee version` 与 `levee system version` 共用它。`--remote` 白名单（`remoteCapableCommands`）相应加上 `levee version` / `levee system version`。
- **证据**：`version_remote_test.go` 4 例 + 工厂 2 例。① 端到端起真实 gRPC server（版本串 `test` / commit `test-commit`），断言 `--remote version` 与 `--remote system version` 报**服务端**的值、且**不含**本地构建变量（测试里是 `unknown`）；② `--json` 逐字段核对；③ `--remote version` 在临时 data_dir 下**不创建** `levee.db`；④ 本地模式仍报本二进制。**变异两处（还原后 `sha256` 逐字节相同）**：把 `fetchVersionInfo` 的 mode 参数硬编码成 `modeLocal` → 远程用例红并打印出本地构建信息；让 `GetSystemService` 也要求 store → `TestServiceFactoryLocal_StoreOnlyWhereNeeded` 红。`go test ./cmd/levee/ -count=1` 全绿（91s）。
- **边界**：`--remote` 的支持面从 3 条扩到 5 条命令，其余命令仍以 `[exit=2]` 拒绝（上一笔的 fail-fast 保持不变）；本笔不碰 change 的写路径，也不改 proto。

### 测试（把 deployment.md 纳入旗标守卫 + 把 conversation 会话路由钉成三重事实）

- **缺口一：一句没有读者的强断言。** `docs/deployment.md` 前言写着「所有旗标与配置键均与当前代码实现核对，未包含未实现的能力」，而 `cmd/levee/cli_reference_flags_test.go` 只扫 `cli-reference.md` 与 `quickstart.md`——那句话无人验证。它的调用就是普通 `levee serve --flag` 行，本来就在 `docUsageRE` 的匹配形态内，扩一个文档路径即可纳入。
- **缺口二：REST-only 端点不在任何 proto 驱动守卫的射程内。** §13.7 的五个 conversation 会话端点不经过 gRPC，因此 `rest_exposure_matrix_test.go`（从 proto 描述符枚举 RPC）与 `rest_endpoint_table_test.go`（对账 §13.2 表与 `restResourceHandlers`）都看不到它们；最近的 `rest_conversation_owner_test.go` 只断言**所有权**结果——路由悄悄不再被路由时它不红（它期望的 403 与路由不存在时的 404 在"没拿到我要的会话"这一点上不可分辨），且它从不读文档。
- **落地**：① flag 守卫的文档列表加 `docs/deployment.md`（纳入后 521 处旗标引用全对得上，证明那句当前不是说谎——**本笔是预防性守卫，不是修漂移**）；② 新增 `internal/grpc/rest_conversation_route_test.go`，三条腿各自独立：**源码腿**（`dispatchConversation` 必须含每条形状的字面量，因为它是路径字符串运算、没有可枚举的路由表）、**文档腿**（§13.7 必须描述每条 backticked 调用）、**线上腿**（对真实 session 断言成功码，再用「未知 sub-path 404 且消息点名路径」+「错误动词 405」这一对把 "routed" 钉住）。
- **写线上腿时发现自己两个错**（已写进测试注释，供下一个人别再犯）：① `docAnchor` 曾把 METHOD 放在反引号外，而 §13.7 实际把整段包在一个反引号里，五条全红；② 断言「非 404 即路由存在」是**错的**——dispatcher 对未知路径答 404，handler 对不存在的 session 也答 404，而 `DELETE /conversation/sessions/{不存在}` 正是会 404 的合法请求；改成对真实 session 断言成功码才成立。
- **验证**：`go vet` rc=0；`go fmt` 干净；`go test -count=1 ./internal/grpc/`（89.7s）与 `./cmd/levee/`（90.9s）全绿；`golangci-lint run`（v2.13.2 与 CI 同版，`--allow-parallel-runners` 绕开他人在跑的全局锁）0 issues；`scripts/test_changelog_structure.py` 与 `test_doc_anchors.py` OK。
- **变异（均先红后还原）**：`dispatchConversation` 的 `/messages` 改成不可达 → 源码腿与线上腿同时红；删文档里 `DELETE /conversation/sessions/{id}` 的描述 → 文档腿红。
- **如实边界**：反向不在守卫范围——在 §13.7 写一条不存在的端点不会变红（实测确认），它只保证「真实路由同时被代码与文档记住」。

### 新增（活台账锚点活性门：`scripts/check_doc_anchors.py` + 6 处死锚点订正）

- **缺口**：台账里的 `path:line` 锚点是下一个 agent 照着走的落点，但它们**没有任何门**。11-01 的一轮复核翻出 5 处账本与代码脱节（`docs/project-status-2026-11.md` §7），全部只能靠人读代码发现——CI 校验代码，不校验"文档指向的那一行还在不在"。第一版扫描口径（全 `docs/`）会大量误报：冻结快照（`docs/quality-hardening-2026-09.md` 等）的锚点本来就该烂，裸文件名（`store.go:12`）在仓里对应十几个包。
- **落地**：① `scripts/check_doc_anchors.py` 把作用域做成**声明式**（`LIVE_DOCS` 六篇：product-roadmap / blocked-items / levee-api / product-positioning / AGENTS / CONTRIBUTING），锚点必须带路径分隔符，校验文件存在 / 行号 ≤ EOF / 区间不倒置；围栏内的示例不算引用；`MIN_ANCHORS=20` 防"扫了个寂寞"；退出码 0/1/2 区分"干净 / 有问题 / 扫不了"（把"读不到文档"报成"全是死锚点"会把人引到错的修法上）。② 修掉 `docs/product-roadmap.md` 的 6 处死锚点：`compat/executor.go:4-7` 与 `rag/embedding.go:7-10` 指向 2026-10-03 已删的包 ⇒ 改写为明确的历史陈述（不带行号，不再冒充锚点）；`approval/service.go:258`、`grpc/change_service.go:116`、`executor/irreversible.go:188`、`wiring/plan.go:119` 是包在、行号漂 ⇒ 重指到当前实现（`internal/approval/service.go:259`、`internal/grpc/change_service.go:90`、`internal/executor/irreversible.go:287`、`internal/wiring/plan.go:150`）。③ 随行订正 `docs/levee-api.md` 第 6 章的 drift 命令：文档写 `drift scan` / `drift show --target`，仓里注册的是 `drift detect` / `drift report`（`cmd/levee/cmd_drift.go:101` / `:754`），目标是 `--host/--hosts`，不存在 `--target` 与 `--depth`——旧写法照着敲直接报 unknown command。
- **证据（变异打在仓库真实文件上，不是夹具）**：把一个锚点改成 `:99999` → 红并点名 `line 99999 is past EOF (cmd/levee/cmd_drift.go has 1287 lines)`；把一个锚点指向不存在的 `plan2.go` → 红并点名 `no such file`；两次都从备份还原，还原后两份文件 `sha256` 与变异前**逐字节相同**、检查器回绿（27 个锚点 / 6 篇活台账）。检测器自证 7 组样本：缺文件、超 EOF、区间倒置、活锚点对照、裸文件名不算锚点、围栏内豁免双向、声明文档缺失是 exit 2 而非通过。`test_this_suite_is_named_in_the_release_gate_list` 有**先红后绿**的实样：加进 ci.yml 点名清单之前它确实报 `'test_doc_anchors' not found`。
- **边界**：本门只证"锚点还指向某处"，**不证锚点周围那句话还对**——语义复核仍归人（§7 那 5 处漂移是读代码定的，不是这门发现的）。作用域固定六篇；`docs/security-audit.md`、`docs/review-report.md` 等 9 月快照刻意不扫，它们的锚点属于冻结记录。`scripts/` 自测 77 → **87** 例。

### 变更（`--remote` 在只有本地语义的命令上 fail-fast，不再静默写本地库）

- **缺陷（静默谎言，且带数据后果）**：`--remote` 是全局旗标，但只有 `agent list / show / remove` 读它。其余命令带着 `--remote` 照常执行——打开**本地** store，把变更、审批、冻结期写进一个 server 永远不读的数据库，然后正常退出。操作者以为在跟 server 说话，实际没有；`docs/cli-reference.md` 当时把这写成"其余命令仍按本地语义执行"，等于把缺陷写成了说明书。
- **`--local` 默认值是这个缺陷藏起来的原因**：它默认 `true`，于是 `levee --remote …` 同时把 `--local` 也置真，`resolveServiceMode()` 一旦接线就会以"两者互斥"拒掉**所有** `--remote` 调用（包括三个真正支持它的命令）。默认值改为 `false`：**两者都不给 = 本地**（行为与今天一致），`--local` / `--remote` 才是真正的二选一。`resolveServiceMode()` 从"零调用点"变成被 `PersistentPreRunE` 调用。
- **落地**：`cmd/levee/root.go` 新增 `remoteCapableCommands` 白名单与 `validateServiceMode(cmd)`，挂在 `rootCmd.PersistentPreRunE` 上——它在任何 `RunE` 之前跑，被拒的命令**不会加载配置、不会打开 store**。拒绝文案点名命令与支持清单，退出码 `[exit=2]`（用法错误，不是笼统的 1）。父命令与 help/completion 不拒（它们只打印）。
- **证据**：`cmd/levee/remote_mode_guard_test.go` 7 例。① 端到端：临时 config（`server.data_dir`/`database.path` 都指到 `t.TempDir()`，并**先建出 data 目录**，否则"没建库"会退化成"建不了库"）跑 `--remote list`，断言 `exit=2`、错误点名 `not wired for \`levee list\``、且 SQLite 文件**不存在**。② 变异（在仓库文件上做，还原后 `sha256` 逐字节相同）：摘掉 `PersistentPreRunE` 接线 → 端到端用例红在"期望报错却 nil"，**同时复现了原始缺陷本身**（命令正常执行，即静默走本地）；从白名单删掉 `levee agent list` → 红。③ 第一版白名单测试是**自证的**：它遍历生产用的那个 slice，删一项只是少跑一圈、照样绿——实测发现后补了独立期望 `TestRemoteCapabilityListIsExactlyTheseThree`（字面量逐项对齐），重跑同一变异才转红；这条与本仓"判据别挂代理指标"同源。④ `TestRemoteCapabilityListResolves` 把白名单条目解析回真实命令树，改名不会让名单指向空。`go test ./cmd/levee/ -count=1` 全绿（40s）。
- **边界**：本笔只把"静默退本地"变成"响亮拒绝"，**没有**给任何新命令接远程语义——`service_factory` 的接线 / 退役仍是待定项（`docs/blocked-items.md` 末行"维持现状"与 `docs/project-status-2026-11.md` §8.3 把它登记为决定）。因此 `--remote` 的支持面仍是 `agent list / show / remove` 三条，与 `docs/cli-reference.md` 订正后的表述一致。

### 修复（发布门禁 G-06：「跑成」与「不超时」是两件事，必须都判）

- **缺陷**：`scripts/release_gate.sh` 的 G-06 写成 `go test ./... -count=1 > /dev/null 2>&1 || true`，然后**只**比较计时与 600s 预算。`|| true` 把退出码丢掉之后，"测试根本没跑起来"与"跑得很快的绿"在门禁眼里是同一件事。
- **本机实测过的假阳性**（2026-10-31 切 v1.24.0 前）：`GOSUMDB=off` 挡住 `go.mod` 声明的 1.26.10 工具链切换，那次报告里 G-01/02/03/07 **全红**（`go: go.mod requires go >= 1.26.10 …`），而同一张表里 G-06 是 **PASS (1s)**。一条会给坏状态背书的门比没有门更糟：它把"没跑到"翻译成了"跑到且不超时"。
- **修法**：`|| RC_G6=$?` 捕获退出码，判定改成三条分支——rc≠0 先判红并在报告里点明"测试没跑成或跑红，先修这个再谈时长"；rc=0 且 `> 600s` 判超时红；否则绿。计时预算那条分支原样保留（它仍量它名字里那件事）。
- **运行证据（替身 `go`，四组样本都要有）**：旧脚本 + `go` 返 1 → **PASS**（缺陷复现，整表只因别的门红而 FAILED）；新脚本 + 返 1 → **FAIL**（`go test rc=1`）；新脚本 + 返 0 → **PASS**（不过度判红）；把预算临时改成 `-gt -1` 的可执行副本 + 返 0 → **FAIL**（`0s > 600s`，证明时长分支仍可达、没被我改成 rc 独占判定后饿死）。
- **回归守卫**（扩在既有套件 `scripts/test_release_gate_step.py` 里，该套件已在 ci.yml 的 release-gate 点名清单中，不新增点名项）：新增 `g06_block()` / `g06_problems()`，对 `release_gate.sh` 的 G-06 段判四件事——不许出现 `|| true`、必须捕获 `|| NAME=$?`、必须在 `if` 里按该变量 `-ne/-eq 0` 分支、必须仍把 `DURATION_G6` 与一个数值预算比较；锚点找不到直接 `AssertionError`（会沉默的守卫等于给损坏背书）。检测器自证 5 组样本：修好的形状绿、`|| true` 旧形状红、捕获了但不判定红、预算丢失红、**注释里引用 `|| true` 不红**（最后这条是被自己咬出来后补的反例：本笔写的说明注释里引用了旧写法，判据一度把一段**解释**当成**缺陷**）。真实变异用整段回退到旧 `release_gate.sh` 做在**仓库文件**上（不是夹具）→ 守卫红，还原后与原文件 sha256 逐字节相同。
- **边界**：G-06 不判"有没有测试被跑到"（全 `t.Skip` 仍算跑成）——那属于覆盖率类门禁，不在本笔；也不改 CI 的 test 腿。`scripts/` 自测由 70 例增至 **77** 例全绿。

### 修复（`/monitor` 实时流账本过期订正 + 告警 SSE 路由分发守卫）

- **缺口类型（本仓头号缺陷类：账本没跟上代码）**：`docs/product-roadmap.md` 的 `/monitor` 三视图联动行登记「日志流是轮询不是真流」「目标地图仍缺」「真流需补网关路由」，而三条**都早已落地**——SSE 路由（`internal/grpc/sse.go` 的 `/changes/{id}/events`、`sse_alerts.go` 的 `/alerts/stream`、`sse_route.go`）、目标地图（#118 的 host × batch 矩阵，`utils/targetMap.ts`）、前端订阅（`MonitorView.vue` 用 `EventSource`，2 秒轮询已删，`MonitorStream.spec.ts` 钉住）。`internal/grpc/rest_exposure_matrix_test.go` 的 `SubscribeAlerts` 豁免理由同样过期（写「SSE route is the ledger's open item」）。
- **真缺口（代码侧）**：`dispatchAlerts` **零测试覆盖**——`sse_alerts_test.go` 此前直接调 `handleAlertsStream`，绕过了 `/alerts/stream` 的路由分发；删掉那个 case 现有测试全绿。（changes 侧的分发已被 `sse_test.go` 的 `dispatchChange` 钉住。）
- **落地**：① 新增 `TestAlertsStreamRouteIsWired`——经 `dispatchAlerts` 走真实 HTTP，断言 `/alerts/stream` 到达流处理（环回放证明）且未知子路径得到指向真路由的 400；② 订正 `rest_exposure_matrix_test.go` 的过期理由；③ 订正 roadmap 的 `/monitor` 行（保留原文仅作历史）；④ `docs/levee-api.md` 补记两条 SSE 路由并新增 13.2b 节（鉴权只判一次 / 保活 / 不缓冲三条公共契约）。
- **验证**：`gofmt` 干净；`go build ./internal/grpc/` rc=0；`go test -count=1 -run 'TestAlertsStream|TestServeChangeEvents|TestHandleChangeEvents|TestEveryRPCHasADocumentedRESTAnswer' ./internal/grpc/` ok；`golangci-lint run ./internal/grpc/`（v2.13.2 与 CI 同版）0 issues。
- **边界**：`levee logs -f`（CLI）仍是每 2 秒轮询（`cmd/levee/cmd_logs.go`），本笔不动它——那是 CLI 的既有实现，不是控制台 SSE 那一条；控制台的实时流才是本行订正的对象。

### 新增（漂移基线命名多版本：主键从 host 改成 (host, name)，`default` 是没被点名时读的那一份）

- **缺口（roadmap:43）**：`BaselineManager` 以 host 为唯一主键，一台主机至多一份基线，`set` / `auto` 都是覆盖写。文档曾承诺 `--name`，后来因为"命名多版本要改的是存储主键和取用规则、不是加旗标"而**删掉了那句承诺**（原登记原话）。本笔按那句话把它做成事实。
- **键与取用规则**：`drift.Baseline` 新增 `Name`，管理器键从 `host` 改成 `(host, name)`。保留名 `default` 是**所有没被点名的消费方**读的那一份——`detect --baseline auto`、`baseline show` / `delete`、以及没钉版本的巡检任务都落在它上面。语义是**同名覆盖、异名并存**：覆盖 `default` 就是"刷新当前期望状态"那条路径，另存一个名字不动它。`Delete` 一次只删一个版本、绝不连带清同主机的其他版本（`--baseline-name` 打错字最多删错一份，不会把该主机当前的期望状态一起带走）；`Get` 未命中时把该主机**实际存在的版本名**写进错误串——"没有基线"是死胡同，"它有的是 cis、prod"才是能照着改的提示。
- **`Set` 的键只能来自记录本身**：旧签名 `Set(host, *Baseline)` 让键和记录各说一遍、两者可以不一致而没人管；改成 `Set(*Baseline)`，host/name 一律从记录取并规范化。本仓反复出现的那类"平行词表"在键上同样是缺陷。
- **兼容性不做迁移**：`default` 的文件名沿用 `<dataDir>/drift/baselines/<host>.json`，命名版本另起一个 `<host>.<name>.json`；旧记录没有 `name` 字段 ⇒ 装载时归 `default`。因此升级不改变任何运维动作，`--json` 只多出一个 `name` 键。
- **名字规则刻意收紧，是拒绝不是巧合**：小写字母/数字开头，其后允许 `._-`，总长 ≤64。**不接受大写**——名字要进文件名，而大小写不敏感的文件系统会把 `Prod` 与 `prod` 合成同一份基线。主机名与版本名两处一起拒路径分隔符与 `.` / `..`（`syncJSONDir` 原有的那道守卫保留，纵深防御）。
- **最容易被顺手做坏的是"整目录对账"**：`saveBaselinesToDisk` 写期望集合、并删掉目录里其余所有 `*.json`。而 (host, name) → 文件名**不是内射**：主机 `web-01` 的版本 `prod` 与主机 `web-01.prod` 的 `default` 都要占 `web-01.prod.json`。碰撞时**整批写入拒绝**、一个字节都不落——否则一次平常的 `baseline set` 会把另一份当陈旧文件删了。
- **钉名是被消费的字段，不是装饰**：`DriftJob.BaselineName` 由 `runDetection` 在探测**任何主机之前**解析；名字不可用 ⇒ 整轮不探测（不允许半张舰队按错误的期望状态被检查）。CLI 在建任务时就校验并规范化，不让它活到每天 02:00 静默失败。`--json` 里的 `baseline_name` 是文件里的原值：旧任务文件是空串（按 `default` 解析），把空串显示成 `default` 等于声称有一份从没被选择过的版本。
- **`--baseline-name` 与 `--baseline <file>` 互斥**：文件本身就是一份完整基线，同时给等于悄悄丢掉一个，因此直接拒绝（`--baseline-name applies only to --baseline auto`）。
- **可观测**：`baseline list` 增加 `NAME` 列（先按主机、再按版本名字典序，`default` 不置顶）；`show` / `delete` 输出点名版本；`delete --json` 除被删版本外还给 `remaining_for_host`，人类可读输出删除后列出该主机仍存着的版本名；`schedule add` 多一行 `Baseline version: <name>`；调度日志的 `get baseline failed` 带上 `baseline_name`。
- **测试**：
  - `internal/drift/baseline_test.go`：命名版本并存且默认版本不被顶掉、空名归一为 `default`、`NormalizeBaselineName` 12 条形状子用例（含大写 / 分隔符 / 内含空格 / 前导标点 / `..` / 65 字符拒 / 64 字符收）、主机形状拒绝（分隔符与点名）、未命中错误列出其他版本且顺序稳定、`Set` 的键取自记录、`ListHost`、`List` 按 (host, name) 稳定排序 + 防御拷贝、删除只动一个版本、并发用例改成两个**不同版本**写入（避免 last-writer-wins 掩盖竞态）。
  - `internal/drift/scheduler_test.go` 2 例 + 一个专用替身：同一个主机同一份实际状态，只有钉的版本不同 ⇒ 未钉报 1 处漂移、钉 `cis` 报 0（"镜像 checks 的 mock"会让每个版本看起来一样，故新写 `pinnedActualProber`）；不可用的钉名 ⇒ `TotalHosts==0` 且**一次都没探测**。
  - `cmd/levee/cmd_drift_baseline_name_test.go`（新文件，7 例）：旧格式文件读作 `default` **并在无关写入后仍存在于磁盘**、两个版本各自落盘且删一个不动另一个（含 `remaining_for_host`）、`detect --baseline-name` 真的决定漂移结论、文件来源与钉名互斥、非法名字写入即拒且磁盘零残留、任务钉名往返 + 旧任务文件空串原样报出、stem 碰撞整批拒绝且两份原文件都还在。
  - 既有 drift 用例（`TestDriftE2E_*`、serve 侧 scheduler 装配 5 例）**一行未改全绿**——兼容性的实际验收。
- **变异咬合（5 条，均先红后还原；三个源文件还原后与改前 `sha256` 逐字节相同）**：① 去掉 `saveBaselinesToDisk` 的碰撞拒绝 → 碰撞用例红（"expected an error, got nil"）；② 让 `default` 不再沿用 `<host>` 文件名 → 升级用例红在"legacy 文件应存活"那条断言上（即真实数据丢失）；③ 去掉版本名校验 → drift 侧 4 条顶层用例红（`TestNormalizeBaselineName` 的 7 条"应拒"子用例全部红、5 条"应收"子用例仍绿），CLI 的写入拒绝用例同时红；④ `runDetection` 忽略任务钉名 → 钉名用例红（漂移结论翻转），"不探测"用例两条断言都红；⑤ `Delete` 改成清掉同主机所有版本 → drift 一条红，CLI 一条红在 `remaining_for_host`（说 1 实际 0）。**诚实标注射程**：③ 之下 drift 的"不探测"用例仍绿——它测的是"不探测"而不是"校验"，两条性质不重叠，不能拿③当它的反证。
- **顺带订正两处文档说谎**：`docs/cli-reference.md` 22.1 原写 `--baseline` 接受"基线 ID"且"必填"（实际默认 `auto`、其余取值是 YAML 文件路径，**从不接受 ID**），22.3 示例漏掉必填的 `--run`（`docs/quickstart.md` 同一处同漏）；`drift baseline show` / `delete` 此前**整章没有章节**，现补齐——第 22 章因此重编号至 22.11（此前无任何外部文档引用过 22.x 小节号，实测 `grep` 无命中）。
- **登记不修**：`docs/levee-api.md` 第 6 章把 `levee drift scan --target …` 与 `levee drift show <report-id>` 写成产品命令并配了选项表，实际注册表里只有 `detect` / `baseline{set,auto,list,show,delete}` / `schedule{add,list,remove,run}` / `report`；`docs/review-report.md:167` 还把这一对判成"一致"。已进 roadmap 新行，改法要单独定夺（那是 API 参考文档的口径问题，不是本笔的键控改动）。
- **清理**：`drift.ErrBaselineExists` 是零消费者的死哨兵（注释承诺"覆盖未明示意图时返回"，代码从来没有那条路径），随重新键控一并删除。
- **边界（仍未做，写在文档里而不是假装覆盖）**：① 没有 `promote` / `export`——要把命名版本变成 `default`，目前只能再 `set --file` 或 `auto` 一次；② 一个任务只有一个版本名、作用于其全部主机，按主机分别钉名要拆成多个任务；③ 版本没有历史链、保留策略与自动淘汰，存几份由操作者自己删；④ 粒度到 `(host, name)` 为止，不做参数级。
- **验证**：`gofmt -l internal/drift cmd/levee` 空输出；`go vet ./internal/drift/... ./cmd/levee/...` rc=0；`go test -count=1 ./...` rc=0（68 个包 `ok`，零 `FAIL`；`cmd/levee` 196.6s、`internal/drift` 23.4s）；`go test -race -count=2 ./internal/drift` rc=0（`CGO_ENABLED=1` + mingw64 在 PATH，覆盖"同一主机两个版本并发写入"那条用例）；`drift` 包 99 条顶层用例 + 12 条子用例全过；`python scripts/test_changelog_structure.py` 7 tests OK（本条目落在唯一且居首的 `[Unreleased]` 下）；`golangci-lint run ./internal/drift/... ./cmd/levee/...` **rc=0 / `0 issues.`**（本机 v2.13.2 与 CI 钉的 `version: v2.13` 同线，私有 `TMP`/`GOLANGCI_LINT_CACHE` 隔离同机并发的全局锁；第一次跑打印了 `0 issues.` 却以 **rc=4 超时**结束——那**不算通过**，故把 `--timeout` 抬到 570s 重跑到 rc=0，冷缓存 67s）；`golangci-lint run ./internal/drift/... ./cmd/levee/...`（本机 v2.13.2，与 CI 钉的 `version: v2.13` 同线；私有 `TMP` + 私有 `GOLANGCI_LINT_CACHE` 绕开全局锁）**`0 issues.` 且 rc=0**（67s）——第一次带 300s 超时跑时冷缓存打出 `0 issues.` 却以 **rc=4（超时）** 结束，那种组合不是"lint 通过"，故重试到真退码为止；**真二进制走通产品路径**（不是只靠单测）：`levee drift baseline set --host web-01` 与 `--baseline-name pre-migration` 之后，磁盘上同时存在 `web-01.json` 与 `web-01.pre-migration.json`，`drift baseline list --host web-01` 出两行、`NAME` 列分别为 `default` / `pre-migration`。

### 修复（D-1b 已交付：插件破坏性动作声明接进编译期与计划期；D-2 目标组边界检查整理提交）

- **D-1b 已闭环（PR #234，`b375ec8`，4 个干净 commit：`bf5a387`/`293694e`/`63ddcf3`/`b375ec8`）**：插件破坏性动作声明（`destructive_actions`）通过 registry 持久化（含旧库 `ALTER TABLE` 迁移，`PRAGMA table_info` 守卫 + 显式 `ALTER`，不吞真错误）；编译期 `dsl.NewValidatorWithPluginPolicy` 与计划期 `plan.NewGeneratorWithPluginPolicy` 消费同一份 `dsl.PluginActionPolicy`（破坏性对 / 可逆对 / 已安装模块名，归一化去重）；判定进 `plan_hash`（`irreversible` / `irreversible_reason`），同一插件在不同部署下可逆与不可逆的差异成为事实，不是漂移。旧构造器 `NewValidator` / `NewGenerator` / `WithInherentIrreversible` 保留为破坏性简写，语义不变。**强制档（未声明即默认不可逆）仍未做，已登记为 `docs/blocked-items.md` E-3，由后续产品决定取代。**
- **D-2 整理提交（本轮 `feat/d2-target-caps`）**：新增 `internal/plan/targetcaps.go`（目标组 `min_count`/`max_count` 边界检查，规范 §4.1）；新增错误码 `LE105`/`LE106`/`LE107`（`internal/errors/errors.go`）；集成到 `generator.go`（`Generate` 阶段调用 `checkTargetCaps`，计划阶段阻断、不进审批）；补提交 `internal/chatops/event_producer_test.go`（守卫测试，7328 字节）。修复 `generator.go` 意外被注释的函数声明（构建验证通过）；提交为单父线性分支（`feat/d2-target-caps`，`fd867f2` + `81886d5`），无 merge commit，遵循 §5.4。
- **本轮无冲突确认**：不触碰 `CHANGELOG.md` 其他已交付条目（`#244`/#246/#247/#250`/`#234` 的记录原样保留）；`docs/blocked-items.md` 已同步更新（D-1b 挪入已交付区并注 PR 号，D-1c 保持已收窄描述）；`docs/blocked-items.md` E-3（强制档）仍留作待定，不与本笔混淆。
- **验证**：`gofmt -l` 干净；`go vet ./...` rc=0；`go test ./internal/dsl/ ./internal/plan/ ./internal/chatops/` 全绿（含变异验证）；构建在临时降 `go.mod`（已还原）后通过；远端 `origin/master`（`023e334`）与本分支无文件冲突。

### 新增（演示库场景脚本：D-2 运营层资产里 agent 能做的那一半）

- **缺口（D-2 拆开看）**：`docs/blocked-items.md` 的 D-2 把「demo 库 / 角色视频 / 品牌 GIF」整条记为「需要真人录制与设计，非代码工作」。拆开看，四个场景（MySQL 主从切换 / nginx 灰度 / web 配置回滚 / 集群接管）里的**场景脚本**这一半是 agent 能独立完成的——人只需要照着跑并录像。实测缺口：nginx 灰度与 web 配置回滚全仓只有 `docs/ui-blueprint.md` 的示意名，**没有可跑工作流**；集群接管只有 README 里的手写步骤。
- **落地**：
  - 新增 `deploy/lab/DEMOS.md`（演示索引）：四个客户场景 A1–A4 + 两个实验室演练 B1/B2，每个场景写明**它证明什么能力 / 跑之前要准备什么 / 验收看哪条证据**，末尾一节**诚实边界**（哪些环节实测过、哪些没有）。
  - 新增 3 个工作流：`lab-nginx-canary.yaml`（percent 50/100 两批 + 批间 HTTP 探针门禁不过就阻断第二批 + `nginx -t` 单独成步判定配置正确性 + 改前快照）、`lab-web-config-rollback.yaml`（失败→门禁红→按改前快照自动还原→回滚后再验证一次）、`lab-cluster-takeover.yaml`（给接管演练一个可被中断的长任务：第一步已完成、第二步长睡，kill -9 执行节点后看收敛与「已完成步骤不重跑」）。
  - `deploy/lab/inventory.example.yaml` 补 web 前端层两台（`.15`/`.16`，`role=web`），并写明 `tier` 标签只是人看的标注（by-tag 批次策略已从词表移除，`docs/leveelang-spec.md` §593）。
  - `deploy/lab/README.md` 的命令序列订正（见下一节）。
- **守卫**：`internal/dsl/shipped_assets_guard_test.go` 七条（G1–G7），其中 **G4 强制 `DEMOS.md` 与 `deploy/lab/workflows/` 双向对齐**——每个工作流文件被恰好一个场景行认领、每个场景行指向的文件真实存在、A1–A4 必须齐。这是本仓头号缺陷类（台账/文档与实物流离）的机器化拦截：索引漏一个文件或指向不存在的文件，CI 直接红。
- **本机实测口径（只跑到计划层，不外推）**：本机没有 4~7 台 VM、没有 PG 集群 ⇒ **apply 执行与快照回滚未实跑**。已实测的是：真二进制 + 真实 SQLite + 假清单下 `template create → new --params → plan` 全链可行；nginx 灰度 plan 出**两批**（先 1 台金丝雀、后 1 台），web 回滚与集群接管各 1 批；同一模板换参数后 **plan_hash 变化**（`v2:1b17ebfe…` vs `v2:271e2c96…`，取值确实进计划哈希）。这条边界已写进 `DEMOS.md` §4，不写成"已跑通"。
- **口径订正（2026-11-02）**：上一条里的「apply 执行与快照回滚未实跑」**已被后来的容器靶场实跑取代**——执行期路径与快照采集/还原现已在本机跑通（见下文「容器靶场」一节），仍然没跑的是**真 VM 实验室**与**集群形态**（MySQL 主从、PG 集群接管）。本条其余内容（计划层口径、plan_hash 随参数变化）不变。

### 修复（已发布工作流的四类"编译绿、计划期/运行期才炸"）

- **取值层混用（3 个文件，5 处引用）**：`examples/workflows/batch-update.yaml`（3 处）、`examples/workflows/dr-switch-orders-db.yaml`（1 处）、`deploy/lab/workflows/lab-replica-switch.yaml`（1 处）用**打包层**的 `{{.x}}` 槽位，而语言层是 `{{input.x}}`（`docs/leveelang-spec.md` §2.1 明写二者"刻意分开"）。探针实测：语言层的取值链不替换 `{{.new_primary}}`——值会原样送到目标机。改法：换成 `{{input.<name>}}`，取值在**计划期**从参数袋解析，解析后的值进 plan hash（换参数即换计划、需重新审批）。`examples/templates/patch-rolling.yaml` 里另有 9 处 `{{.x}}`——**那是模板负载**（它就是模板记录的内容本身），`{{.x}}` 在那里合法，G1 因此把它排除在外。
- **补偿形状被静默丢弃（2 个文件）**：`examples/workflows/batch-update.yaml` 与 `examples/templates/patch-rolling.yaml` 的 step 级 `rollback: {action, args}` 是旧形状；解析器只认 `strategy/on_failure/verify_after/step/steps/snapshot_paths`（`internal/dsl/parser.go` 的 `yamlRollbackRaw`），yaml.v3 **静默丢弃**未知键 ⇒ 补偿被解析成空壳（`strategy=""`、`steps=0`、`snapshotPaths=[]`），而注释却声称"a step-level compensation"。改法：改用 `strategy: undo-action` + `step:`。
- **命令体违反运行期白名单（已发布资产 7 处 + 本笔新增 1 处）**：`shell.exec` 的命令体在**运行期**受白名单约束（只允许字母数字与 `- _ . / =` 和空格；`&`、引号、管道、重定向一律拒，见 `internal/executor/modules/shell/shell.go`），而**编译期不校验**。实测：`nginx -t && systemctl reload nginx` → `shell.exec: unsafe command: disallowed character '&' in command`（连通道都没到）。已发布资产命中 **7** 处（逐条按 `git show HEAD:<file>` 复核过，不是估的）：`examples/workflows/batch-update.yaml`（`uptime && df -h`、`echo 'update complete' >> /var/log/levee.log`）、`examples/workflows/dr-switch-orders-db.yaml` 与 `deploy/lab/workflows/lab-replica-switch.yaml`（`mysql -e "SHOW SLAVE STATUS\G" | grep -E …`）、`examples/gate-templates/mysql.yaml`（`mysql -e 'FLUSH BINARY LOGS'`）、`examples/gate-templates/redis.yaml`（`yum update -y redis && systemctl restart redis`）、`deploy/lab/workflows/lab-batch-restart.yaml`（`systemctl restart lab-app && systemctl is-active lab-app`）。本笔新增的 `lab-nginx-canary.yaml` 里自己也撞上了第 8 处（`nginx -t && systemctl reload nginx`），写的时候就一并改了。改法：需要管道/引号/重定向的走 `shell.script`（脚本体不受该白名单，由目标机 `sh` 执行），需要"先 A 再 B"的**拆成两步**（每步各自留证据，比 `&&` 链更贴治理语义）。
- **高危档没声明审批人（已发布资产 5 处 + 本笔新增 1 处）**：`approval: level: high` 但没写 `approval.approvers` 的工作流，**计划期**就被拒（`internal/grpc/change_service.go` 的 `kickoffApproval`：可投票人数不足即 `ErrApprovalUnderprovisioned`，且不落计划、不建审批行）。实测报错：`approval under-provisioned: high-level change needs 2 independent approver(s) but the workflow declares 0 votable ([])`。已发布资产命中 **5** 处（`examples/workflows/` 三个、`examples/gate-templates/redis.yaml`、`deploy/lab/workflows/lab-replica-switch.yaml`；逐条按 `git show HEAD:<file>` 复核），本笔新增的 `lab-nginx-canary.yaml` 是第 6 处。改法：补 `approvers: [alice, bob]`；**发起人不能投**，所以建 run 的身份要另换一个（`--as`）。全部由 `TestShippedWorkflowsDeclareApproversForHighTier` 守住。
- **顺带修好 `deploy/lab/README.md` 的命令序列**（原文照抄跑不通）：① `levee compile` **不建 run**，README 从没说 run 怎么创建 → 补 `template create` + `new --params`；② README 让人给 `levee plan` 传 input，但 `levee plan` **没有 `--params` 旗标**（参数在 `levee new --params`）；③ `levee plan` 的 `--targets` **必填**（收清单里的主机名，即地址）——工作流里的 `target.query` 目前**不参与选目标**（只是给人看的选择说明；计划期真正切批的是 `--targets` 的顺序，`min_count`/`max_count` 只卡集合大小，见 `internal/plan/targetcaps.go`），README 与三个工作流的注释都说过头了；④ 演练 1 的窗口 `01:00–05:00 Asia/Shanghai` 会让窗外 plan 直接被拒，README 从未提；⑤ `levee audit verify` 要带 `<run-id>`；⑥ `levee cluster_status` 实为 `levee cluster status --pg-dsn`；⑦ 凭据那节写"`mysql.replica_switch` 走 mysql 通道，需要 MySQL 凭据"——mysql 模块是在**目标机上**跑 mysql 客户端、经 ssh 通道下发，`levee secret add` 的类型里根本没有 mysql，要存的是 **SSH 凭据**。
- **新增七条守卫**（`internal/dsl/shipped_assets_guard_test.go`，`package dsl_test`——G5/G6 要读执行器注册表与 shell 模块的运行期策略，而 `internal/executor` 导入 `internal/dsl`，放在内部测试包里会成环）：G1 工作流文档不得出现模板层 `{{.x}}`；G2 声明的 `input` 与 `{{input.x}}` 引用**双向**闭合；G3 step 级 `rollback` 不得含解析器不认识的键；G4 demo 索引 ↔ 工作流文件双向对齐；G5 动作必须在**模块注册表的可执行集**内（词表 ≠ 可执行：`file.write` / `shell.command` 在 `actionSignatures` 里但对应模块没有这两个动作）；G6 `shell.exec` 命令体与 verify 命令必须满足各自运行期策略；G7 高危档必须声明足够审批人。
- **变异验证（7/7，先红后绿，文件逐字节还原）**：① 把 `{{input.package}}` 改回 `{{.package}}` → G1 红；② 引用一个未声明的 `{{input.bogus}}` → G2 红；③ 在 step 级 `rollback` 里塞一个不认识的键 → G3 红；④ 从 `DEMOS.md` 删掉 B1 行 → G4 红；⑤ 把动作改成幽灵动作 `file.write` → G5 红；⑥ 把 `&&` 放回命令体 → G6 红；⑦ 删掉 `approvers` → G7 红。每条还原后守卫复绿，且被改文件与改前逐字节相同。
- **验证**：`gofmt -l internal/ cmd/` 空输出；`go build ./...` rc=0；`go vet ./internal/dsl/... ./internal/executor/...` rc=0；`go test -count=1 ./...` **rc=0，68 个包全 ok、零 FAIL**（`cmd/levee` 93.4s、`internal/grpc` 107.3s、`internal/credential` 67.2s）；`golangci-lint run`（本机 v2.13.2，与 CI 钉的 `version: v2.13` 同线）对改动包 **rc=0 / 0 issues**；真二进制端到端见上。
- **边界（不外推）**：本笔只把 demo 库的场景脚本与已发布工作流的上述四类缺陷做到"可编译、可校验、计划层可跑"。**apply 的执行期路径、快照采集与回滚、集群接管三条没有在本机实跑**（无 VM、无 PG 集群），已逐条登记在 `deploy/lab/DEMOS.md` §4；守卫 G7 覆盖的是**静态可判**的两档（声明档位 + 不可逆红线），风险分带出的更高下限是计划期值，文件级守卫看不见——那一档仍由引擎在计划期拒绝。
- **边界订正（2026-11-02）**：上一条里的「apply 执行期路径、快照采集与回滚没有实跑」已不成立——容器靶场实跑后这两条已跑通（真 SSH 上的模块分发、门禁判定、批次切分、失败回滚、快照采集与还原，证据见下文「容器靶场」一节）。**仍未跑**的是集群接管（需 PG 集群与第二节点）与真 VM 实验室；DEMOS.md §4 已按此改写。

### 新增（容器靶场：把执行期路径在没有 VM 的机器上也实跑一遍）

- **动机**：上一笔交付的诚实边界写着「apply 的执行期路径、快照采集与回滚没有在本机实跑」——因为本机没有 4~7 台 VM。这个缺口不是"加台机器"能补的（那是采购与运维），于是本笔用**Docker 容器顶替实验室 VM**，把执行期路径真正跑通。
- **新增 `deploy/lab/container-lab/`**：
  - `Dockerfile`——靶机镜像（alpine 3.24 + openssh + nginx + openrc）。两个必须记的坑都写进注释：openrc 在没 boot 过的容器里要 `touch /run/openrc/softlevel`；openrc 的 pidfile（`/run/nginx/nginx.pid`）与 nginx 默认写的（`/run/nginx.pid`）不一致时 `service nginx status` 恒报 `crashed`，svc 模块的 `isActive` 判据就废了，所以镜像里把 nginx.conf 的 `pid` 指令改到 openrc 期望的路径。
  - `workflows/` 三条可跑工作流：成功路径（`svc.start`/`file.copy`/`shell.exec`/`svc.reload` + 步骤级门禁 + 批间 HTTP 探针门禁 + `percent 50/100` 分批）、失败路径（退出码非 0 → 自动回滚 → `undo-action` 补偿）、快照路径（`rollback.strategy: snapshot`）。两份站点配置是**故意不同的**（8090 与 8091）——否则"还原成功"与"什么都没发生"分不开。
  - `README.md`——完整配方与七条实跑事实。
  - `run/config.template.yaml` + `run/inventory.yaml` + `run/credential-value.json`——
    配置模板、目标清单与靶场凭据值。`run/config.yaml` 是**生成物**（不入库）：
    `labdrive.py` 首次运行时从模板生成，把 `{{LAB_DIR}}` 换成本目录的绝对路径。
  - `labdrive.py`——驱动脚本。**它不是便利包装，是"能不能复现"的关键**：PowerShell
    传多行 YAML 会把参数按行拆散，传带双引号的 JSON 会把双引号吃掉（不报错，
    解析回退后整段被当成口令 ⇒ 一次莫名其妙的认证失败）；脚本用 Python 的
    `subprocess.run([...])` 列表形式传参，并固定工作目录 = 靶场根目录
    （`file.copy` 的相对 `src` 相对的就是它）、`LEVEE_FILE_MODULE_EXTRA_DIRS`
    与 `LEVEE_MASTER_PASSWORD`。
- **实测证据（本机，3 台容器）**：① 成功路径 8 个步骤全 `success`，输出是目标机真实回显（`uploaded 132 bytes to '/etc/nginx/http.d/lab-site.conf'`、`nginx: the configuration file /etc/nginx/nginx.conf syntax is ok`、`* Reloading nginx configuration ... [ ok ]`），`percent 50/100` 切出 2 批各 1 台，步骤级 cmd 门禁与批间 HTTP 探针门禁均通过；② 失败路径 `deliberate-failure` `exit_code=1` → 补偿步 `undo-start-nginx`（`svc.stop`）`success` → 目标机独立复核 `service nginx status` = **stopped**；③ 快照路径经 `levee serve --engine-snapshot-dir` + REST gateway（`POST /api/v1/changes/<id>/apply`）：正向 `file.copy` 上传 132 字节（写入 `listen 8091`）→ 故意失败 → `snapshot:deploy-site-config` 还原 `success` → 目标机文件回到 **`listen 8090`**，快照库落地 `meta.json` / `paths.json` / 载荷三件套。
  **终态照实写**：成功路径 `completed`；**两条失败路径都是 `rolled_back_partial` 而不是 `rolled_back`**——故意失败的那一步自己没声明补偿契约，它的副作用没被撤销，回滚没完全收敛，引擎于是不声称"干净回滚"（`internal/engine/closure.go:96-101`，引擎自己的回归测试 `internal/wiring/exec_run_test.go:351-357` 钉的就是这一条）。快照那条的还原步是 `success`、文件确实回到 `listen 8090`，但 run 终态仍是 `rolled_back_partial`——**"文件回来了"不等于"回滚干净了"**。
- **入库后按配方复验（干净目录，本机 Docker 靶场）**：把 `deploy/lab/container-lab/`
  原样复制到一个空目录，按 README §5.1 的命令序列重跑——① 成功路径 run
  `run-8b33517b724b7e34` 终态 **`completed`**（2 批各 1 台、8 个步骤全 `success`），
  目标机独立复核 `service nginx status` = started、站点文件 = `listen 8090`；
  ② 失败路径 run `run-9670144f78b0a818` 终态 **`rolled_back_partial`**、exit=1，
  补偿步 `undo-start-nginx`（`svc.stop`）`success`，目标机独立复核
  `service nginx status` = **stopped**。
  **复验抓到配方本身的两个缺口并已补**：① `run/data/` 不存在时 sqlite 报
  `unable to open database file (14)`——报错看着像权限问题，其实是目录没建
  （`labdrive.py` 现在会建 `run/data/` 与 `run/snapshots/`）；② 配置里的绝对路径
  不能入库，所以拆成 `run/config.template.yaml`（占位符 `{{LAB_DIR}}`）+
  运行时生成不入库的 `run/config.yaml`。
- **实跑查出并写进文档的五条操作事实**（`DEMOS.md` §4.1 与 `container-lab/README.md` §4）：① **CLI 本地路径不读 `channel.ssh.*` 配置**——`ssh.SetDefaultConfig(...)` 全仓只有 `cmd/levee/cmd_serve.go:954` 一处调用，`levee plan` / `levee apply --engine-enabled` 一律按包默认值走（严格主机密钥校验 + `~/.ssh/known_hosts`），配置里的 `strict_host_check: false` 对本地路径**无效**；② **快照补偿在 CLI 本地 apply 上不可用**（`--engine-snapshot-dir` 只有 serve 有），声明快照的计划在动任何目标之前 fail-closed 拒绝：`declares snapshot rollback but no snapshot store is configured (--engine-snapshot-dir)`；③ **`file.copy` 的 `src` 不能是绝对路径**，只能在控制端工作目录内（或 `LEVEE_FILE_MODULE_EXTRA_DIRS` 白名单），相对 `src` 是相对**执行 levee 那个进程的工作目录**；④ **别用 `pgrep` 做存活探测**——Alpine 的 busybox 把 `pgrep` 实现成了 `pkill` 的别名，`pgrep nginx` 会按名字杀进程（实测把执行它的 shell 一起杀了），要用 `service nginx status`；⑤ **`levee target check` 在 CLI 本地模式下探不了**（恒返回 `no channel available`），连通性探测要在 serve 下做。
- **守卫覆盖同步**：`shipped_assets_guard_test.go` 的 `shippedDocs` 根目录增加 `deploy/lab/container-lab/workflows`，使这三条新工作流同样受 G1/G2/G3/G5/G6/G7 约束（G4 的索引对齐**不**覆盖它们——G4 映射的是 `DEMOS.md` 的场景行与客户演示工作流，容器靶场有自己的索引，即它 README 里那张表）。
- **边界（不外推）**：容器靶场证明的是**引擎的执行期路径**（SSH 认证、模块分发、门禁判定、批次切分、失败回滚、快照采集与还原、证据落库）。它**不**证明真 VM 上的 systemd 服务语义（靶机走 openrc 的 sysvinit 分支）、MySQL 主从切换（无 MySQL 集群）、集群接管（无 PG 集群与第二节点）、以及真实 nginx 灰度的流量效果（探针是固定 URL，没有真实负载）。这些仍要按 `DEMOS.md` 的实验室环境做。

### 修复（`file.copy` 的编译期签名与运行期契约对不上）

- **原缺陷**：编译期签名表 `internal/dsl/typechecker.go` 的 `actionSignatures["file.copy"]`
  声明的参数名是 **`dst`**，而运行期模块读的是 **`dest`**
  （`internal/executor/modules/file/file.go:182`）。全仓 `grep '"dst"'` **只命中签名表这一行**
  ——没有任何工作流、模块或测试用 `dst`；所有已发布工作流（`deploy/lab/workflows/` 的
  `lab-nginx-canary.yaml`、`lab-web-config-rollback.yaml`，以及本笔新增的三条容器靶场工作流）
  写的都是 `dest`。
- **危害**：签名表在对外宣告一个**运行期不存在的参数名**，而真正的参数名根本不在表里。
  照着签名表写工作流的人会写 `dst:`——它能过严格类型检查（这个键确实在表里），
  然后在目标机上炸：`file.copy: missing argument "dest"`。这正是本仓在猎的
  「编译绿、运行期才炸」那一类。
- **改法**：`dst` → `dest`（一个词的订正）。**没有**动检查器的其他行为。
- **顺带订正一条说谎的注释**：`checkStepArgs` 的注释原写「未知参数在宽松模式被忽略、
  在严格模式被报告」——代码里根本没有这个分支，未知参数在**两种模式下都不报**
  （`checkStepArgs` 只遍历签名表里有的键）。注释已按代码实况改写，并写明为什么不能改成
  「严格模式报未知参数」：签名表是**故意不完整的**（只列了 MVP 动作），而模块还接受表里
  没写的可选参数（`file.copy` 的 `owner`/`group`，运行期对非字符串是跳过而非报错），
  一旦报未知参数就会把运行期本来能跑的工作流挡在编译期。
- **新守卫（两侧都钉住）**：`internal/dsl/typechecker_test.go` 的
  `TestFileCopySignatureUsesRuntimeArgName`（内部测试包，直接断言签名表里有 `dest`、
  没有 `dst`）+ `internal/dsl/action_signature_contract_test.go` 的
  `TestFileCopyRuntimeArgNameMatchesSignature`（外部测试包，断言运行期模块：缺 `dest` 拒、
  给 `dst` 也拒、给 `dest` 才放行）。后者必须放外部包——`internal/executor` 导入
  `internal/dsl`（`executor/irreversible.go`），放在内部测试包里会成环。
- **编译门扩面**：`TestShippedLabWorkflowsCompile` 原来只覆盖
  `deploy/lab/workflows/`（6 个文件），本笔把 `deploy/lab/container-lab/workflows/`
  （3 个文件）也纳入，计数 6 → **9**（计数是钉死的，不是下界——按仓库既有约定，
  新增已发布工作流必须同笔更新这个数）。
- **残留缺口（不外推）**：本笔**没有**堵住「未知参数一律放过」这个口子，所以照旧可以
  写一个 `dst:` 的工作流骗过编译期。要真堵住，得先把签名表补全到每个模块的完整参数集
  （含 `owner`/`group` 这类可选参数），那是另一件事，本笔不做、也没假装做了。

### 本笔的变异验证（7/7，先红后绿，文件逐字节还原）

① 容器靶场工作流里塞打包层槽位 `{{.slot}}` → G1 红；② 命令体放回 `&&` → G5 红；
③ 高危档删掉 `approvers` → G6 红；④ 动作改成幽灵动作 `bogus.action` → G3 红；
⑤ 批次策略改成非法值 `bogus` → 编译门红；⑥ 签名表的 `dest` 退回 `dst` → 签名守卫红；
⑦ file 模块改读 `dst` → 运行期契约守卫红。每条还原后守卫复绿，且被改文件与改前逐字节相同
（容器靶场目录尚未纳入版本控制，所以还原是按字节写回，不是 `git checkout`）。

## [v1.24.0] - 2026-10-31

> **发布状态：已切版。** 附注 tag `v1.24.0`（tag 对象 `7a69ba624f77`，指向折叠提交 `023e334`——单父、以 `--rebase` 落地）于 2026-10-31T14:21Z（22:21 +0800）推送，且是在该折叠提交自己的 `master` push run 全绿之后才打的：27 条 check 全部 `completed/success`，含聚合门 `check (all jobs passed)`。`release.yml` 三段（CI gate / goreleaser / container image，run `41209750308`，14:21:24Z → 15:21:15Z）全部 success：第一段等的是 **tag push 自己触发的** `ci.yml` run `41209750382`（`head_branch=v1.24.0`，14:40:57Z success）——`require_ci_release.py` 明确不接受同一提交在分支上已经绿的那个 run，只认这个 tag ref 的；GitHub Release `draft=false`、`publishedAt=2026-10-31T14:57:35Z`（11-01 06:57:35 +0800），资产 **7** 个（6 个平台包 + `checksums.txt`，那 6 行 sha256 与 6 个包一一对应）；镜像 `ghcr.io/levango7/levee:v1.24.0` 按外部事实核过——`manifests/v1.24.0` 返回 **200**（同一次探测里 `v1.22.0`、`v1.23.0` → 200、不存在的 `v1.24.99` → 404，证明探针有分辨力；`Accept` 必须带多架构 index 媒体类型，否则连已存在的 tag 也会读成 404），`tags/list` = `[v1.18.0, latest, v1.19.0, v1.20.0, v1.21.0, v1.22.0, v1.23.0, v1.24.0]`，index 内是 `linux/amd64` + `linux/arm64` 两个平台（另两条 `unknown/unknown` 是构建 attestation）。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.24.0` / 逐字 `v1.24.0`（顺序不能反：规则②要求它逐字出现在远端 tag 集合里）。 本节共 **16** 个小节，六条主线：① **插件系统**四类全部接进引擎消费点（gate→module 执行器、module→channel 通道注册表），动作不可逆改成**三态分类**（未声明即按不可逆处理、判定进 `plan_hash`、编译期/计划期/对话草案三处同源）并透出到 `plugin info` / `plugin test`；② **编排与通道**——`depends_on` 从「被解析却没人消费」变成稳定拓扑排序（新增 LE104、LE071 重新登记）、通道会话跨 run 池化、`upload` 动词让 `file.copy` 这类推文件的步骤第一次能在插件通道上跑；③ **数据与并发契约**——`run_assignment.result` 的词表随迁移 v10 下沉到数据库（回填 + 双触发/CHECK）、`approval.CheckExpiry` 的写回改成 CAS；④ **供应链与工具链**——Node 24 + Vite 8（rolldown）+ vue-tsc 3 + jsdom 30 换代，TypeScript 实测钉在 5.9；⑤ **验证基建与假红根因**——`target-ownership.yaml` 的 serve 侧第一次有测试（含接线守卫）、`#244` 的单步验证补齐「判决 ≠ 拒绝」的行为覆盖、console 行为第一次在第二个浏览器引擎上被验证、`internal/diagnosis` 的耗时断言不再判时钟分辨率；⑥ **发版纪律**——`landing shape` 在 tag 路径上从 skipped 变成机器断言。

### 修复（step `depends_on` 从「被解析却没人消费」变成真依赖顺序 + 未知/成环拒绝）

- **缺口（本仓头号缺陷类的又一实例）**：`depends_on` 被解析（`parser.go`）、登上 AST/IR、被 `emit.go` 回写、被交付示例（`examples/workflows/batch-update.yaml` 等三份）使用——但**没有任何代码消费它**。执行总是按声明顺序，于是 `steps: [b(depends_on a), a]` 会先跑 b 再跑 a，**静默违反声明的依赖**；错误码表甚至把撤回的 LE071 描述成「no depends_on in the language」，与解析器的事实相反。
- **落地**：新增 `dsl.OrderStepsByDependsOn`——按 `depends_on` 做**稳定拓扑排序**（依赖已满足的步骤保持声明顺序，因此按依赖书写的 workflow 逐字节不变，现有示例零影响），并把两类非法图报出来：引用不存在的 step（**新增 LE104**）、依赖成环（**LE071 重新登记**：2026-10-12 因无产出方被撤回，现在有了产出方）。编译期 `dsl.Validator` 与计划期 `plan.Generator`（`wiring.GeneratePlan` 不经 validator 的那条路）**共用同一份实现**，两端结论不可能分叉。
- **边界如实**：消费的是**依赖顺序**这一半；并行调度与 `{{step.x.output}}` 输出引用仍是 V1 项（`docs/mvp-tasks.md` N-18）。规范 §4.5 / §8.1 V9 / §8.2 同步更新。
- **测试**：dsl 10 例（乱序重排、稳定、无依赖不变、未知引用、环、自依赖、重复边算一条、坏图仍返回全序、validator 报错）；plan 3 例（生成器按依赖排序、未知引用 LE104 拒绝、环 LE071 拒绝）；`internal/errors` 目录计数 25→27 与 `TestEveryCatalogueCodeHasAProducer` 双向守卫自动覆盖；`TestShippedExamplesCompile` 保持绿。
- **验证**：`gofmt -l` 干净；`go test -count=1 ./internal/dsl/ ./internal/errors/ ./internal/plan/` 全绿。

### 测试（`target-ownership.yaml` 的 serve 侧：加载 / 拒启 / 审计非致命 + 接线守卫）

- **缺口**：`internal/inventory` 只测了比较函数 `LabelConflicts`；从文件到进程的那一段**零测试**——`loadTargetOwnershipRegistry` 的坏文件、冲突是否影响启动、以及 `buildServeServices` 是否真把加载值交给 `TargetEnvResolver`，删掉任何一环都不会有测试变红。而这条链决定 authz 判 prod 还是 dev（`TargetEnvResolver` 的 registry-first 规则），属「函数正确≠被调用」的典型缺口。
- **落地（只加测试，产品代码零改动）**：
  - 缺失文件 = 文档化的 labels-only 姿态：不报错、注册表未配置；
  - 坏文件四态（YAML 不合法 / 缺 `environment` / 缺 `hostname` / 同一主机声明两次）一律**拒绝启动**，错误串点名事实源（`target ownership`），而不是静默退回调用方可声明的 labels；
  - 冲突**是审计不是故障**：启动继续，WARN 行必须同时给出主机名、注册表的 `prod` 与行的 `dev`；同一份注册表 + 不一致的 `labels["env"]` 下，按 target id 解析必须得到注册表的 `prod`（安全相关的那一半）；一致清单只打 `consistent`、不许 WARN（审计噪音会训练运维忽略它）；
  - 接线守卫：`cmd_serve.go` 必须 `ownershipReg, oerr := loadTargetOwnershipRegistry(cfg, store)`、`oerr` 必须中止构造、加载值必须以 `ownershipReg` 原样交给 `TargetEnvResolver`，且两者之间不许重新赋值——编译器已经挡住「传 nil 再留个未用变量」，剩下的可乘之机正是中途换成另一个注册表。
- **变异咬合（四条，均先红后还原；`cmd_serve.go` 还原后与 HEAD 逐字节相同）**：① 加载错误改成 `return reg, nil` → 坏文件四条子用例全红；② 丢弃 `LabelConflicts` 的结果 → 冲突用例红而「一致不告警」仍绿（窄射程）；③ 在加载与解析器之间插入 `ownershipReg = &inventory.OwnershipRegistry{}` → 接线用例红；④ `return serveServices{}, oerr` 改成 `nil` → 接线用例红。
- **验证**：新文件 5 个用例全过；`gofmt -l` 干净；`go vet ./cmd/levee` rc=0；`go test ./cmd/levee` 全包 ok（127s）。
- **边界**：日志断言靠 `log.InitLoggerWithWriter` 临时替换进程单例并在 `t.Cleanup` 还原——本包没有 `t.Parallel` 用例，窗口不被共享；接线守卫是定点读调用点（与 `serve_gateway_wiring_test.go` 同一取舍），`buildServeServices` 的长启动序列单测驱动不了。

### 新增（`#244` 的单步验证补齐行为覆盖：mock 网关三态 + browser-check 驱动弹窗）

- **缺口**：`#244`（门禁面板「单步验证」弹窗，接 `POST /gates/verify`）合并时**没有**碰 `web/scripts/mock-gateway.mjs` 与 `web/scripts/browser-check.mjs`，所以那个新交互在 mock 模式下调不通（端点 404），也没有任何行为级覆盖。它最值得钉的语义——**200 + `passed:false` 是判决，400 是请求被拒绝，两者不能混为一谈**（混了等于声称引擎评判了一个它从未执行的检查）——只有跑在真浏览器里才算被验证过。
- **落地**（零产品源码改动，只加测试基建）：
  - `mock-gateway.mjs` 新增 `handleGateVerify`，镜像真实端点的**诚实三态**而非恒绿：未知类型 / cmd 无目标 / 标记 `reject:` → 400；标记 `fail:` → 200 `passed:false`（真判决）；其余 → 200 `passed:true`。cmd 走命令文本标记（`ok:`/`fail:`/`reject:`），因为 cmd 是弹窗的**默认类型**——不碰类型选择器、直接落在操作员真的会看到的那条路径上；probe 仍按 URL 标记走。**一个永远通过的 mock 会让「判决 vs 拒绝」的区分静默退化**。
  - `browser-check.mjs` 新增 `[4d]`：打开 `/monitor/run-1000` → 唤出弹窗 → 填命令 → 立即验证，把**三种结果各驱动一遍**：通过（success alert + 描述里带 `ms` 与 gate 名 = snake_case 字段确实被读到了）、未通过（error alert 但标题恰为「未通过」= 判决）、拒绝（error alert 标题以 mock 的 `gate verify:` 拒绝文案开头且**不是**「未通过」）。每次运行之间关弹窗重开，让每条请求都从无旧判决的状态出发。
- **两个本机踩到并已落注释的坑**：① `el-input` 是 `inheritAttrs:false`，`$attrs`（含 `placeholder`、以及任何 `data-testid`）被铺到**内部原生 `<input>`** 而非包裹 div（element-plus 2.14.7 `input.vue` 源码核实），所以按 `placeholder` 选字段是对的、写 `[data-testid=x] 内层控件` 永远找不到；② `el-dialog` 关闭时是**隐藏而非卸载**（`destroyOnClose` 默认关），“弹窗已关”必须判 `.el-overlay` 的 `display:none`，不能判 `.el-dialog` 是否存在。
- **验证**：mock 端点 curl 六条路径（200 通过 / 200 未通过 / 400 拒绝 / probe fail / 未知类型 / cmd 无目标）全部符合契约；本机 Chrome 用隔离端口（5173→5273、4599→4699，避让他人 dev server）实跑全套 browser-check **ALL PASS**（新增 4 条断言、回归无破坏；含 `[4d]` 三态）。两个引擎的覆盖由 CI 四条腿复核。
- **边界**：mock 把真实端点「引擎未挂载时的 404」塌缩成 400——被测属性是「拒绝 ≠ 判决」，不是拒绝用哪个状态码到达。本笔不触碰 `#244` 的任何产品源码与既有 spec；`internal/web/dist` 无需重建（无产品改动）。
### 变更（前端工具链换代：Node 24 + Vite 8 + vue-tsc 3 + jsdom 30；TypeScript 留在 5.9）

- **触发**：`jsdom@30` 的 `engines` 是 `^22.22.2 || ^24.15.0 || >=26`，`vite@8` 是 `^20.19.0 || >=22.12.0`，而 CI 与镜像都钉在 Node 20（`node-version: '20'` 两处、`ARG NODE_VERSION=20-alpine`）。Node 20 已过维护期，继续留在 20 就只能永远不升这两个包。
- **Node 24（两处 CI + 镜像）**：`ci.yml` 的 frontend 与 browser-check 两个 job 改成 `node-version: '24'`；`Dockerfile` 的 `ARG NODE_VERSION=24-alpine`、`ARG ALPINE_VERSION=3.24`（运行时镜像）。三个镜像 tag 在本机实测可拉起：`node:24-alpine` = v24.22.2、`golang:1.26-alpine` = go1.26.10（与 `go.mod` 一致）、`alpine:3.24` = 3.24.3。
- **Vite 8 的两个必要适配**：rolldown 的 `manualChunks` **只接受函数**（rollup 的对象写法在建置期直接 `TypeError: manualChunks is not a function`），按原意图改成包前缀匹配（框架 + UI kit 图 → `vendor`，其余随懒加载视图）；`__dirname` 在 `configLoader: 'native'` 下会告警，改用 `import.meta.dirname`。
- **vue-tsc 3**：`vue-tsc --noEmit` 零改动通过（38 文件 / 327 用例的单测同批通过）。
- **TypeScript 刻意不升 7**：`typescript@7.0.2`（原生编译器）移除了 `./lib/tsc` 子路径，`vue-tsc@3.3.13` 直接 `ERR_PACKAGE_PATH_NOT_EXPORTED` 起不来——实测复现，不是推断。上到 TS 7 要先等 `@vue/language-core` 换到原生 API，因此本轮把 `typescript` 钉在 `^5.9.3`（同时满足 vitest 与 vue-tsc 的 peer 范围）。
- **dist 的一致性证据**：本机（Windows / node 26）重建与 `node:24-alpine` 容器内 `npm ci && npm run build` 的产物 `diff -r` 为**空**，说明提交的 `internal/web/dist` 对 CI 的 Node 24 也是新鲜的，不是"本机碰巧过"。
- **产物变化**：vendor chunk 1,237,049 → 1,215,229 字节（gzip 394→385 kB）；构建耗时 40s → 5.7s（rolldown）。
- **本地验证**：`vue-tsc --noEmit` rc=0；`vitest` 38 文件 / 327 用例全过；`vite build` 成功；`go build ./...`、`go vet ./...`、`gofmt -l` 干净；`go test ./...` 全量 rc=0；`docgen -check` 两个目标 up to date；`python -m unittest discover -s scripts` 70 tests OK；`docker build --target node` 在 node:24-alpine 上完整跑通。
- **边界**：本机 Docker 容器到 `proxy.golang.org` 被网络策略拒绝，因此完整镜像构建的 `go mod download` 这一步只能在 CI 的 trivy 腿（`docker build -t levee:scan .`）上验证；`web/node_modules` 未入库，`npm ci` 走锁文件。

### 修复（`internal/diagnosis` 的耗时断言不再判时钟分辨率——windows 腿因此假红过一次）

- **现象**：`PR #236`（发布 tag 单父断言）的 `test (windows-latest)` 红在 `TestProbeAllAggregation`，现场是 `health_probe_test.go:858` 的 `assert.Positive(t, report.Duration)` 报 `"0s" is not positive`；同一个探针在日志里有大量 `duration_ms=0` 的子用例。这是本窗口 52 次 windows 腿里第一次出现，与那笔改动（只动 `scripts/` 与两个 `.md`）无关。
- **根因**：`report.Duration` 取 `time.Since(start)`，是单调钟读数；而**时钟分辨率**在虚拟化 runner 上可能比这次探测的跨度还粗，于是"探测跑完了但读数为 0"是合法结果。原断言实际判的是时钟分辨率，不是被测代码——`assert.Positive` 在这种粒度下必然偶发假红。
- **改法**（只改测试，生产代码不动）：换成两条不依赖分辨率的性质——① `report.Duration >= 0`（跨度不倒流）；② `report.Duration <= 被测调用外层观察到的墙钟跨度`（探针跑不出它所在的那段时间）。第二条仍能抓住"耗时报成 `1h`"这类垃圾值（实测变异：`Duration: time.Hour` → 判红）。
- **诚实的边界**：① 与 ② 都拦不住"Duration 未被赋值"——在"合法 0"与"未赋值"不可区分的前提下，没有时钟 seam 就测不出赋值本身。要测它得给 `HealthProber` 注入时钟，那是另一个决定（本笔把它写进注释而不是假装测到了）。
- **变异**：`Duration: time.Hour` → ② 红（`"1h0m0s" is not less than or equal to "514.1383ms"`）；`Duration: -time.Second` → ① 红；文件按 `sha256` 逐字节还原，`go test ./internal/diagnosis -run TestProbeAllAggregation -count=50` 全绿。
### 新增（插件动作三态分类：未声明即默认不可逆，判定进 plan hash 且三处闸门同源）

- **缺口（E-3；取代 D-1b 条目里「仍未做强制档」的边界）**：D-1b 只让**显式声明** `destructive_actions` 的插件动作进不可逆词表。一个 manifest 漏写、或根本没写这张表的插件，其破坏型动作仍会静默通过 V14/LE082——而「没声明」不是「安全」的证据。
- **三态而非两态**：manifest 新增 `reversible_actions`。每个动作落在**破坏性 / 可逆 / 未分类**之一；只有「可逆」能在不写 `allow_irreversible` 的情况下通过，未分类的动作按不可逆处理（fail-closed）。这是本笔唯一的行为变更，也是需要产品拍板的原因：既有 manifest 里没列进两张表的动作会开始要求白名单。`NewPluginModule` 对两张表做同一套子集校验（必须是握手 `actions` 的子集），并**拒绝同时出现在两张表里的动作**——自相矛盾的分类不能由代码随便选一边。
- **单一来源**：新增 `dsl.PluginActionPolicy`（破坏性对 / 可逆对 / 已安装模块名，构造时归一化去重）。三个消费方都从它取判定，而不是各自维护词表：
  - 编译期 `dsl.NewValidatorWithPluginPolicy`（V14/LE082）；
  - 计划期 `plan.NewGeneratorWithPluginPolicy` + `IrreversibleChecker.RegisterPolicy`（LE082 与步骤 `irreversible` 戳记）；
  - 对话草案闸门 `ConversationEngineConfig.PluginActionPolicy`。
  CLI 侧 `declaredPluginActionPolicy()` 从注册表一次读出三者（`plugin.DeclaredActionClassification`），`compile` / `new` / `import` / `converse` / `serve` / `plan --local` 全部改用它。旧入口 `NewValidator(extra...)` / `NewGenerator(extra...)` / `WithInherentIrreversible` / `InherentIrreversible` 保留为「只声明破坏性」的兼容简写，语义不变。
- **判定进 hash**：不可逆结论本来就在 `plan_hash` 的 `irreversible` / `irreversible_reason` 里；三态分类让「同一插件动作在未分类部署上不可逆、在声明可逆的部署上可逆」成为事实，因此**同一 workflow 在不同部署会得到不同 hash 是设计预期**，不是漂移——部署事实本来就该进审批绑定的产物。
- **可观测**：`NewPluginModule` 在发现未分类动作时打一条 warn（列出具体动作），这是运维在第一次 plan 之前修正 manifest 的唯一提示；`serve` 启动日志给出 destructive / reversible / modules 三个计数。
- **测试**：dsl（三态语义、零值不表态、归一化、破坏性压过重叠、validator 三种状态 + 未安装模块对照）、plan（三种状态戳记、未分类无白名单 → LE082、可逆免白名单、未安装模块仍可逆、**同一 policy 两实例同 hash / 分类不同则 hash 不同**、旧构造器只认破坏性）、executor（`RegisterPolicy` 三态、显式声明优先、空 policy 不生效）、plugin（`reversible_actions` 读写往返、上一版表结构自动加列、`DeclaredActionClassification` 三输出、子集/重叠拒绝、适配器未分类视图）、wiring（`WithPluginActionPolicy` 真的进 `GeneratePlan`，含无 policy 对照与旧选项兼容）、conversation（草案三态 + 旧字段兼容）。
- **变异咬合**（三条均先红后还原，备份在 Temp）：①去掉 `dsl.PluginActionPolicy.JudgesIrreversible` 的 fail-closed 分支 → dsl/plan/wiring/conversation 的未分类断言同时红；②`RegisterPolicy` 不登记模块名 → executor/plan/wiring 的戳记与 hash 断言红；③删掉两张表的重叠拒绝 → plugin 的构造与未分类用例红。另有一条对照：三条变异下 executor 的独立 fail-closed 逻辑仍生效，证明计划期判定不是只靠 policy 那一条路径（纵深防御）。
- **验证**：`gofmt -l` 干净；`go vet` 对受影响包 rc=0；`go test ./...` 全量 rc=0。
- **边界**：声明粒度仍是动作级（`<plugin>.<action>`），不做参数级；既有 manifest 未分类的动作会开始要求白名单——这是 E-3 的既定代价，升级说明写在 `docs/blocked-items.md`。

### 新增（插件动作分类透出到 CLI：`plugin info` 读声明、`plugin test` 点名未分类动词）

- **缺口（E-3 的运维半边）**：分类落进注册表与 `plan_hash` 之后，运维侧唯一的提示是 `serve` 启动时的一条 warn。`levee plugin info` 打印的是记录字段，`plugin list` 亦然——也就是说「我这个插件哪些动词会被当成不可逆」在第一次被 LE082 拒掉之前**没有任何命令能回答**。
- **`plugin info` / `plugin list`：只读注册表，不执行插件**。`type: module` 的记录额外带 `destructive_actions` / `reversible_actions`（`--json` 是同名两字段，人类可读输出是同名两行），其余三类型**不输出**这两项：gate/channel/notifier 没有可被步骤引用的动词，打两行空值会把「不适用」渲染成「声明了没有破坏性动作」。这一条有反向用例钉住（`TestPluginInfoOmitsClassificationForNonModule`、`TestPluginRecordToMapModuleOnly`）。
- **`plugin test`：把握手动词与 manifest 两张表对齐**，输出三行 `destructive` / `reversible` / `unclassified (judged irreversible at plan time)`（`--json` 落在 `data.classification`）。未分类那一行**即使为空也打印 `(none)`**——「没有」就是要的答案，省略等于把问题留回给第一次失败的 plan。核对不过**直接报错**，与 `serve` 装配该模块时的拒绝一致：幻影动作（manifest 点了握手不提供的动词）与自相矛盾（同一动词进两张表）都不许被「测试通过」掩盖。
- **单一实现**：新增 `plugin.ClassifyModuleSurface(rec, meta)` + 包内 `classifyActions`，`NewPluginModule` 改用它——CLI 看到的分箱与启动期算的是同一份，不可能分叉；`TestClassifyModuleSurfaceMatchesNewPluginModule` 用真子进程握手把两侧逐字段对齐（actions / destructive / reversible / unclassified）。
- **测试**：internal/plugin 7 例（三态、全分类、幻影动作两路、重叠、meta 不可用三路、nil 记录、与 `NewPluginModule` 等价）；cmd/levee 8 例（install→info JSON/human 走真注册表、非 module 不输出、list JSON 携带、渲染两态、以及一条「读声明不需要执行插件」的性质用例）。
- **变异咬合**（四条均先红后按 sha256 逐字节还原，备份在 Temp）：①去掉 `pluginRecordToMap` 的类型门槛 → 两条「非 module 不输出」断言红；②删掉 fail-closed 说明两行 → info 人类输出用例红；③`unclassified` 改为空时不打印 → 两条渲染用例红；④`classifyActions` 把重叠校验旁路 → 重叠拒绝与 `NewPluginModule` 用例红。②③最初写成含转义的补丁导致编译错（「编不过」不算断言生效），改用行替换后重做并取得真红。
- **未覆盖的诚实边界**：`plugin test` 里「调用分类器」这一小段装配没有端到端用例——cmd 包内无法起会讲 JSON 行协议的假二进制（`internal/plugin` 的子进程人格不跨包可见）；分类逻辑与渲染分别在其 owning 包内被覆盖，调用点是三行直筒代码。
- **文档**：`docs/cli-reference.md` 16.6 / 16.7 两节改写，写明只读语义、非 module 不输出、未分类即不可逆的默认，以及 `plugin test` 会因分类不过而报错。
- **验证**：`gofmt -l` 干净；`go vet ./cmd/... ./internal/...` rc=0；`go test ./...` 全量 rc=0（68 个包，基线 `de620bd` 复跑）。

### 新增（运行期通道的会话跨 run 池化：同目标的下一批 run 复用同一条已拨通的会话）

- **缺口（D-1c 的池化半）**：运行期通道每个 target 每次 run 都起一个子进程、`Close` 一律杀掉——`channelFor` 的通道生命周期就是 run 的生命周期。对内置 `ssh` 这只是重连一次，对通道插件却是**重新付最贵的那一步**：NETCONF hello、堡垒机跳转、厂商握手，会话状态全在那条已建立的连接里。大批量目标还意味着等量子进程被反复创建/销毁。
- **键**：`(插件, host, port, type, 凭据指纹)`，池在 `PluginChannelFactory` 内、按插件一个（`internal/plugin/pool.go`）。凭据取**指纹而不是引用名**：两行 inventory 可以写同一个引用名却解析出不同密钥（或跨租户同名），按名共享等于让一次 run 以别人的身份执行——这正是 `channelConnectParams` 的明文边界要防的事。指纹只在宿主侧参与键计算，永不过管道。
- **三道上界，任一单独都不够**：每插件空闲 ≤8（超出淘汰**最久空闲**的，正在服务的不算空闲）、空闲 2 分钟回收、闲置会话最长 30 分钟（远端状态会累积）。**带租约的会话从不被任何一条回收**——把 run 正在用的会话抽走会把一次池命中变成一次失败 run，所以 age 只对空闲会话生效。
- **并发拨号只留一条**：两个通道同时错过池、各自拨号，`publish` 让先到者入池、后来者的会话当场拆掉并改为持有胜者的租约——不会被交出一条池不知道的会话，也不会让同一个键存活两个子进程。
- **`Close` 的语义由此改变**：只归还租约，不杀进程；池的生命周期挂在 serve 的 ctx 上（ctx 结束 → 空闲会话全部拆掉，shutdown 后新拨号明确失败而不是借用已关闭的池）。对 run 可见的契约不变：Close 后 `IsConnected()==false`、拒绝工作；已判死的会话（请求失败/被终止）在 acquire 时被丢弃，不会交给下一次 run。协议没有心跳，空闲期自行退出的进程要等第一次复用失败才暴露——那次请求报错、会话随即被判死，不会静默成功。
- **测试**：`pool_test.go` 11 例用可注入时钟覆盖复用、引用计数、死会话丢弃、空闲 TTL、age 仅对空闲生效、空闲上界淘汰、publish 竞态、shutdown 语义、指纹隔离；`channel_pool_test.go` 5 例用**真实子进程**证明（子进程回答自己的 PID）：同 target+凭据跨 Close/Connect 是**同一个 PID**，换凭据是不同 PID，会话死后重拨是不同 PID，serve ctx 结束后池清空且新拨号报 `shutting down`，同步 shutdown 后子进程**已被 Wait 回收**（`ProcessState` 非空）而非仅被遗忘。
- **变异咬合**（三条均先红后还原）：①`Close` 改成直接 `teardown` → 复用断言看到两个 PID；②池键去掉凭据指纹 → 换密钥的第二个 target 拿到第一个的 PID；③`shutdown` 只从 map 删除、不拆会话 → 两条停机断言（会话仍 Alive / 未回收）同时变红。
- **验证**：本批新增用例在 `-race` 下绿（CGO_ENABLED=1 + mingw64）；`go test ./cmd/...` 与 `./internal/...` 全量各 rc=0；`gofmt` / `go vet` 干净。
- **边界**：一个插件仍只提供一种传输类型（D-1c 的另一半，仍留在 `docs/blocked-items.md`）；池只存在于常驻进程里，一次性 `levee run` 不跨调用复用。

### 新增（跨浏览器矩阵：console 的行为第一次在第二个引擎上被验证）

- **背景**：账本一直登记“跨浏览器矩阵（只有 Chrome 验证过）”。这块一直没做的原因是“runner 上能不能起 Firefox”无人知道——本会话先用**一次性探针**回答了它：**两个 runner 都自带 Firefox**（ubuntu-latest `/usr/bin/firefox` 157.0；windows-latest `C:\Program Files\Mozilla Firefox\firefox.exe` 158.0.1），puppeteer 都能 headless 起来并渲染页面（探针实际读了 `h1.textContent`），**不需要任何安装步骤** ⇒ 这条轴是免费的。
- **落地**：`browser-check.mjs` 把“只有 Chrome”泛化成 `BROWSER_CHECK_BROWSER=chrome|firefox`——
  - 浏览器候选表按引擎分表（Firefox 的 PATH 候选含 `firefox-esr`）；覆盖变量相应分开（`FIREFOX_PATH` / `CHROME_PATH`），未知值**解析回 chrome**而不是传给 puppeteer 让它在 launch 时报错；
  - **每个引擎一套启动参数，这是本笔真正容易错的地方**：核实 `puppeteer-core` 的 `FirefoxLauncher.js`——它自己计算 `--headless` 并**把用户 args 原样追加**，所以 Chrome 专用的 `--headless=new` / `--disable-dev-shm-usage` 会被原样喂给 Firefox，一个拒绝未知选项的二进制会因此拒启。探针成功时恰好**没传任何自定义 args**，正是这个原因。Firefox 档传空数组；Chrome 档原样保留。
  - CI 的 `browser-check` 加 `browser: [chrome, firefox]` 轴（`fail-fast: false` 保持——一个引擎红不能藏住另一个看到的），`name` 与截图 artifact 名都带上引擎（`browser-shots-<browser>-<os>`，四条腿不再互相覆盖）。
  - **dev server 与探针对齐到字面 `127.0.0.1`**：bare `vite` 绑的是 `localhost`，而 runner 上它解析成 `::1`——探针（`127.0.0.1`）对一个绑在 `::1` 的服务器就是 90 秒“services did not come up”。`npm run dev -- --host 127.0.0.1` 之后 vite 绑字面、curl 探字面、两个引擎都加载字面，DNS 从每条路径上同时消失。
  - **mock 网关补上两个 SSE 端点**（Firefox 第一轮把矩阵挂红的真根因）：`/api/v1/alerts/stream` 与 `/changes/run-1000/events` 原来落到 404 兜底——**Chrome 把 EventSource 失败静默吞掉（网络事件，不冒泡成 page error），Firefox 冒泡成 “Firefox can’t establish a connection”**，于是 Firefox 每个页面都带着两条连接错误、`renders` 全红，而 Chrome 一直绿。这正是第二个引擎存在的意义。后端本就有这两个端点：mock 现在 frame 一次快照后**保持连接**（replay-then-tail 的产品形状），并新增 `[1b]` 断言——badge 的 critical 计数必须从 SSE 帧到达（`1`），使**Chrome 腿**也能抓住流的退化，不必等 Firefox。
- **验证**：`node --check` 干净；`ci.yml` YAML 合法且矩阵解析为 `browser: [chrome, firefox] × os: [ubuntu, windows]`；两个 SSE 路由 curl 实测 **200 + `text/event-stream` + 帧正确 + 连接保持**（不 `end`）；本机 Chrome 路径完整跑一遍 **ALL PASS**（含新增 `[1b]` badge 断言，回归无破坏）；本机无 Firefox 时**干净地 skip-and-tell**（`REQUIRED=1` 下 rc=1，CI 会硬失败而非静默绿）；Firefox 真实路径由 CI 四条腿验证（探针已证可行，且第一轮 CI 已实战暴露了 mock 的 SSE 缺口——Firefox 报的 10 个 FAIL 是真信号，不是引擎怪癖）。
- **刻意不做**：不加第三引擎（webkit/Safari——GitHub runner 没有 Safari，Linux 的 webkit 需要额外依赖）；不改任何断言。若 Firefox 在某条断言上真的与 Chrome 不同，那是**产品兼容性信号**，按各自失败处理，不用“跳过 Firefox”掩盘。

### 新增（插件声明的破坏性动作接进编译期与计划期：破坏型插件步骤不再默认静默通过）

- **缺口（D-1b；PR #234 于 2026-10-28T04:11Z 合入时没有条目，本笔补记）**：内置不可逆词表 `internal/dsl/inherent_irreversible.go` 是**静态**的，插件模块不在内——破坏型插件步骤默认静默通过，只能靠作者自觉写 `irreversible: true`。
- **为什么不是「未知模块一律不可逆」**：不可逆判定**进 plan hash**（`internal/plan/hash.go` 的 `irreversible` / `irreversible_reason`），而「本部署注册了哪些模块」在 CLI 与 serve 上并不相同（serve 注册插件模块，CLI 不加载注册表）——同一份 workflow 会因「谁来 plan」得到不同 hash，而 plan hash 是审批绑定的产物。所以判据改成**插件自声明**：取自已安装产物的持久事实，两侧读到同一份。
- **声明 → 持久化**：`PluginMeta.DestructiveActions`（yaml `destructive_actions`）落在 **manifest** 而不是 meta 握手——判定必须**不执行插件**就拿到，且 manifest 是运维可评审的安装产物。registry 增列 `destructive_actions`，含**旧库迁移**（`PRAGMA table_info` 预检 + `ALTER TABLE`，不吞真错误）；`Install/Get/List` 全链读写。
- **两侧消费同一份词表**：计划期 `plan.NewGenerator(extra ...string)` 把插件对注册进**同一个** `IrreversibleChecker`（V14/LE082 判定同时认内置词表与部署词表），`wiring.WithInherentIrreversible` 由 serve 与 `levee plan` 注入；编译期 `dsl.NewValidator(extraInherent ...string)`，`levee compile` / `new` / `import` 经 `declaredPluginIrreversiblePairsForCmd()` 读注册表后注入，serve 的对话草案闸门与 `levee converse` 走 `ConversationEngineConfig.InherentIrreversible`。`NewPluginModule` 交叉校验声明必须是握手 `actions` 的子集（幻影动作会污染词表）；`DeclaredDestructivePairs` 只取 enabled + module 记录并排序去重；注册表缺失=未配置（空表）、存在但坏=报错（serve 拒绝启动）。
- **测试与变异**：plugin（读写往返 / 旧库迁移 / 声明子集校验 / 去重排序）、plan（未进 `allow_irreversible` → LE082 且点名；进了 → 步骤被标不可逆；**同一插件对在未声明的部署上仍可逆**——承重对照）、dsl（缺省放行=缺口本身 / 声明后 LE082 / 列名后通过 / 畸形条目被忽略）。四条变异均确认承重并已还原：去掉 `Generate` 的部署词表判定、去掉 `validateIrreversibleWhitelist` 的部署词表判定、去掉子集校验、去掉 `ensurePluginRegistryColumns` 调用。
- **明写的边界**：仍未做「插件未声明即默认不可逆」的强制档（更保守的产品决定，已登记为 `docs/blocked-items.md` E-3）；声明粒度是动作级（`<name>.<action>`），不做参数级。
- **验证**（PR #234 内实测）：`gofmt` / `go vet` 干净；`internal/dsl` / `plugin` / `plan` / `wiring` / `conversation` / `cmd/levee` 全绿。

### 修复（`approval.CheckExpiry` 的写回改成 CAS：不覆写已落地的决策，并发清扫不再重复上报同一条）

- **上一笔接上驱动时留下的竞争面**：`CheckExpiry` 先用 `ListPending` 扫描、再逐条 `Update` 写回，两步之间没有任何条件。两个后果都不是理论问题：①另一副本的清扫读到同一条待决记录，两边都写成功、都上报——同一次超时被播报两遍；②调用方在此期间投了票（`StatusApproved`），清扫把那笔决策无条件盖回 `expired`，等于静默撤销一个人已经做出的判定。`Approval.Store` 接口早就声明了 `UpdateIfPending`，state 层的 `UpdateApprovalIfPending` 也早已实现（`WHERE id=? AND status='pending' AND revision=?`），只是清扫器没用它。
- **改动**：写回换成 `s.store.UpdateIfPending(ctx, &settled)`。**写的是扫描记录的一份拷贝**——Store 允许把内部持有的同一指针交出来，原地改 `Status` 会让 CAS 的 `revision` 比对撞上自己那次修改，守卫变成恒失败。CAS 返回 `false` 不是错误：记录已被别的写入方结算，这一条既不上报也不覆写，只落一行 info 说明“settled concurrently”；store 真报错仍照旧中止整批（返回 error），两者不可混为一谈。
- **测试**：新增 `internal/approval/expiry_cas_test.go` 四例。①`decisionFirstStore` 让 CAS 首次调用先写入一个 `approved` 再返回 false —— 断言清扫**不上报**、库里仍是 `approved`（这条直接覆盖“覆盖人工决定”）；②强制 CAS 落败时静默（无 error、无上报、记录仍 pending）；③CAS 报错仍中止整批（丢 CAS 是静默，写失败不是）；④`barrierStore` 让两个清扫器**都完成扫描后**才允许任一方写入，跑两个 `SweepOnce`，断言同一条过期记录恰好上报一次。
- **并发用例的第一版是假绿**：不加屏障时两个 goroutine 常常串行成 scan→write→scan，第二个清扫看到队列已空，于是“恰好一次”在**未修复**的代码上也能通过——它证明的是没有并发，不是修复有效。加上屏障后，旧实现如约把同一 ID 报了两遍。
- **变异咬合**：改回无条件 `Update` → ①②④如约变红（决策被盖成 `expired`、同一 ID 上报两次）；只把拷贝改回原地修改 → 用例在 CAS 上恒失败、清扫一条都不落定，同样变红。
- 顺带：`TestCheckExpiry_StoreUpdateError` 的注入键从 `update` 改为 `update-if-pending`，注释说明写回已是 CAS。
- 验证：`go test ./internal/approval/` 绿、`gofmt -l` 干净。

### 修复（发布 tag 的落地形状第一次有了机器断言——`landing shape` 在 tag 路径上是 skipped）

- **缺口是切 v1.23.0 时实测到的**：`ci.yml` 的 `landing shape` 步条件写在 **step** 上（`github.event_name == 'push' && github.ref == 'refs/heads/master'`）。推 `v*` tag 时 `github.ref` 是 `refs/tags/v1.23.0`，这一步 **skipped**，`release gate policy` job 的结论照样 success——`v1.23.0` 的 tag run 通过时该步就是 skipped。也就是说 §5.4「PR 以 `gh pr merge --rebase` 落地」对**发布 tag** 从来只靠流程自觉，没有任何机器兜底。
- **落地**：`scripts/require_ci_release.py` 新增 `assert_linear_tagged_commit`，在 `main()` 里**先于**等待 tag 的 CI 运行调用。它取 `repos/<owner>/<repo>/commits/<sha>` 的 `parents`，长度不是恰好 1 就立刻失败，并打出每个父提交的短 sha 与补救口径（§5.4 / `gh pr merge --rebase`）。父数读的是 API 渲染的提交对象，不是本地 `git rev-list`——因此不受检出深度影响，与 `ci.yml` 那步读 `git cat-file commit HEAD` 同一个理由。
- **为什么放发布门而不是 `ci.yml`**：① 语义上这是发布策略（"tag 打在哪"），不是每次 push 的策略；② 只在 tag 事件触发，**不会把历史 tag 的 CI 重跑染红**——`v1.22.0` 的 tag 指向合并提交 `5139395f`，那是 §5.4 生效前的事实，用今天的规则去追溯旧版本只会制造噪声；③ 立刻失败比等完 90 分钟的 CI 再拒更省。
- **测试**：`scripts/test_require_ci_release.py` 新增 `TaggedCommitLinearityTests` 五例——单父通过并只打一次 `commits/<sha>`；双父拒绝；**零父拒绝**；`main()` 在双父时返回 1 且**根本不会进入等待**；对照例证明单父时确实走到了等待（否则"没等待"在 `main()` 干脆不调用等待时也会绿）。3 项变异全部被断言抓住：删掉 `main()` 里的调用 → 顺序用例红；父数判断恒假 → 3 例红；把 `!= 1` 放宽成 `> 1` → 零父用例红（证明"0 个父"这一支不是空转）。

### 加固（`run_assignment.result` 的词表从 Go 守卫下沉到数据库：迁移 v10 回填 + 双触发/CHECK，期望值从常量推导）

- **上一笔只锁了一半**：`SetAssignmentResult` 的 `isAssignResult` 守卫管住了走 store 的调用方，DDL 侧没有任何约束——手工修数据、或将来某个绕过 store 的直写方，仍能把 `running` 塞进一个下游当判定读的列。本次把同一词表钉在表上。
- **迁移 v10（两方言各自一步）**：先回填、再上锁。回填按**两种垃圾的语义分叉**，而不是一律折成 `failed`：`'done'` 不是任何人记录的判定，它是旧 `UpdateAssignmentStateIf` 在把分配推进到 done 状态时往空 result 里塞的状态名，意思是“跑完了、没记判定”，折成 `''`（文档化的“尚无结果”）；其余词表外的值（`running` / `interrupted` / `paused` …）是旧透传漏进来的 run 状态，意思是“终态但不干净”，折成 `'failed'`——与 executor default 分支同值。`''` 原样保留。SQLite 追加 `run_assignment_result_domain_insert` / `_update` 两个触发器（`RAISE(ABORT, 'run_assignment.result is not an assignment result')`），PostgreSQL 追加命名约束 `run_assignment_result_domain`，外层用 `pg_constraint` 存在性判断包成 `DO $$`，让这一步可重放。`currentSchemaVersion` / `pgCurrentSchemaVersion` 同步 9→10，`schema.sql` / `pgschema.sql` 已是迁移后的形状（新库不重放步骤，靠这一条）。
- **约束先照出一个真 bug（已修）**：`UpdateAssignmentStateIf` 的 SQL 里写着 `result=CASE WHEN ?='done' THEN COALESCE(NULLIF(result,''), result) …`，即把**状态名** `done` 当占位符写进 result 列，再指望 `MarkDone` 紧随其后的 `SetAssignmentResult` 覆盖它。但 `MarkDone` 发现自己已经是 done 状态时会**幂等提前返回**，所以只要第一次覆盖失败、或进程在两笔写之间死掉，`done` 就永久留在列里，被下游当成一个判定读。触发器和 CHECK 落上后，`internal/dispatch` 的两条用例立刻红在 `constraint failed: run_assignment.result is not an assignment result`——这是全量 `go test ./...` 抓到的，单跑 `internal/state` 不会发现。修法：状态迁移只改 `state`，把判定留给 `SetAssignmentResult`，中间态是 `''`（真实、合法、可修）。
- **为什么两边写法不同**：SQLite 没有 `ALTER TABLE ... ADD CONSTRAINT`，触发器是存量库上唯一可加的列级域约束；PostgreSQL 有真 CHECK，新建库写进 `pgschema.sql` 内联、存量库 `ADD CONSTRAINT`。表达的是同一个域，漂移由测试兜。
- **测试**：新增 `internal/state/assignment_result_db_constraint_test.go` 四例。①**不经 store 的裸 SQL**：`''` 与三个合法判定放行，`running` / `interrupted` / `rolled_back_partial` / `done` / `Completed` 五个坏值在 INSERT 上被拒且错文可读，UPDATE 改回 `running` 另测一条；②SQLite v10 恰好 3 条语句（回填 + 两个触发器），四处 `IN (...)` 列表全部等于 `AssignResultNames() ∪ {""}`，且回填的 `CASE WHEN result = 'done' THEN ''` 被钉住（两种垃圾不许折成同一个值）；③PostgreSQL v10 恰好 2 条语句（回填 + 带重放守卫的 `ADD CONSTRAINT`），约束名、列表与 `CASE` 同样对齐，`pgschema.sql` 里确有同名约束；④状态迁移后 result 仍为空——直接用 `CreateAssignment` + `UpdateAssignmentStateIf(pending→done)` 读回，钉住“状态迁移不记录判定”这半边。期望值全部从 `AssignResultNames()` 推导——词表加减一项，四处会一起红，而不是各自安静地持有旧副本（原始 read/write 错位就出在两个手抄列表上）。
- **变异咬合**：删掉 `schema.sql` 的两个触发器后①如约变红（裸 INSERT `running` 成功）；把 `''` 从任一处 `IN (...)` 里拿掉后②③变红；把 `UpdateAssignmentStateIf` 改回写 `done` 占位符，④如约红在触发器上。
- 验证：`go test ./internal/state/ ./internal/dispatch/ ./internal/takeover/ ./internal/tenant/` 绿、`gofmt -l` 干净。

### 新增（运行期通道的 `upload` 动词——`file.copy` 这类推文件的步骤第一次能在插件通道上跑）

- **协议**：通道会话动作集加入 `upload`（params：`path` + `data`（base64）），与既有的 `connect`/`exec`/`collect`/`disconnect` 并列。**帧上限随之成文**：`maxTransferPayloadBytes = 512 KiB`（裸字节，base64 后 ~683 KiB）——上限不是策略选择，是 JSON 行协议的物理约束（两端行读取器 1 MiB，插件侧配到这个上限是**插件的义务**，示例都配了；一个用默认 64 KiB scanner 的插件会在真传输帧上掐断自己的连接——测试子进程第一版就死在这，才把这条契约写成文字）。宿主在**发送前**拒绝越界，不会撕坏流：会话保持可用，错误点名上限与出路（分块或内建传输）。
- **`PluginChannel.Upload`**：读内容 → 越界检查 → base64 → 一次 upload 请求。配套给 `Download`（collect）加了同帧的**载荷防御检查**——插件回了个超帧载荷时，宿主给的是点名错误而不是深处 scanner 撕流的模糊失败。
- **能力矩阵变化**：`file.copy`/`file.template` 这类推文件的步骤在运行期通道上从"按名拒绝"变为可用（≤512 KiB/笔）。`docs/cli-reference.md` 第 16 章、`docs/channel-plugins.md` 第 7/8 节、README 的边界句同步改写；`examples/plugins/tcp_console` 显式拒绝 upload 并注明原因（行协议没有文件语义——拒绝是诚实，不是缺陷）。
- **测试**：upload→collect 往返（内容完整性跨 base64 帧两个方向）/ 目标侧拒投浮出 / **恰好 512 KiB 边界**（off-by-one 边缘）/ **越界一字节拒发且会话存活**（宿主侧拒绝不碰管道——用随后的一次成功 exec 钉住）。子进程人格加 upload 存储（`/denied` 拒投）与 collect 回读。
- **blocked-items**：D-1c 收窄为「跨 run 会话池 / 一插件多类型」（upload 移出）；D-1b 标注 PR #234 在途。

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

> **发布状态：已切版。** 附注 tag `v1.23.0`（tag 对象 `656282442ee1`，指向折叠提交 `f3c1eda1`——单父、以 `--rebase` 落地）于 2026-10-27T16:29Z 推送；`release.yml` 三段（CI gate / goreleaser / container image，run `40541440308`）全部 success：GitHub Release `draft=false`、`publishedAt=2026-10-27T16:47:45Z`（10-28 00:47:45 +0800），资产 7 个（6 个平台包 + `checksums.txt`）；镜像 `ghcr.io/levango7/levee:v1.23.0` 按外部事实核过——`manifests/v1.23.0` 返回 **200**（同一次探测里 `v1.22.0` → 200、不存在的 `v1.23.99` → 404，证明这个探针有分辨力），`tags/list` = `[v1.18.0, latest, v1.19.0, v1.20.0, v1.21.0, v1.22.0, v1.23.0]`。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.23.0` / 逐字 `v1.23.0`（顺序不能反：规则②要求它逐字出现在 tag 集合里）。 本节共 **38** 个小节，六条主线：① **插件系统**走到两个真执行面（notifier 装配进通知管理器、gate 接进验证链与 plan 期可执行性判定），`plugin test` 给出最小活证据，文档与 CLI 的执行面声明二次分型；② **通知与告警链路**补齐——drift 常驻巡检接进 serve、run 生命周期全量 webhook 镜像、告警每源入口身份与按归属可见、审批超时第一次生效、门禁失败进聊天；③ **授权与安全**——`DecisionGuard` 补上两处绕行、CLI/ChatOps 投票走服务层且匿名被拒、清单 env 归属有事实源、ChatOps 重放防护、被服务而未判定的三个面穷举收口；④ **供应链与工具链**——Go 1.26.10 收口 GO-2026-6809、codeql 锁步、前端生产依赖 high 清零、vitest 3→5；⑤ **词表单一来源与控制台/CLI**——docgen 的生成表从 2 张长到 12 张（两笔增量 + 四套取值 + 告警 severity）、控制台字段收成严格联合、详情页存量计划、密度档位、`--as` 本地可判路径；⑥ **正确性、验证基建与协作口径**——`{target}` 展开、审批与结果列的数据卫生、未链入包守卫、windows 崩溃证据自动留档、浏览器假红自证、§5.4 线性落地守卫与 rebase 落地口径、账本两处订正。

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

> **订正（2026-10-28，账本对账）**：本节此前收有「跨浏览器矩阵」一条，已原样移至 [Unreleased]。理由：该功能**不在 tag `v1.23.0` 里**——PR #208 于 2026-10-28T07:39Z 才合入，而 tag 指向的折叠提交 `f3c1eda1` 的树内 `web/scripts/browser-check.mjs` 尚无 `FIREFOX` 分支（`git merge-base --is-ancestor d723bfb6 f3c1eda1` = 1，`git show f3c1eda1:CHANGELOG.md` 的该节为 38 个小节，与本条移出后的计数一致）。发布说明不应声称 tag 里没有的行为。

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
  - `paused → running` = `resumed`（先于 `run_started` 判，否则恢复会被误报成新启动）、`→ running` = `run_started`、`→ completed` = `run_completed`、其余终态（failed / 三种回滚判定 / cancelled / rejected / interrupted / archived）= `run_failed`（LevelError，**精确状态保留在 metadata**）、`→ paused` = `paused`（LevelWarning）、其余（draft→planned / approved…）= 新触发器 `run_status_changed`。
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
- **终态播报（chatops 半边）**：`bridge.OnRunTransition` 消费漏斗——`→running` 发 run_started、`→completed` 发 run_completed、其余终态（failed / rolled_back / rolled_back_partial / rolled_back_incomplete / interrupted）发 run_failed 且**卡片携带真实状态**（部分回滚不得读作干净失败或干净完成）。**分工是产品决定**：中间跳（paused / pending_approval）不上聊天——一跳一卡是噪音，且可行动的时刻已有自己的卡片。
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

> **订正（2026-11-06，发布实况）**：本节摘要 ① 里的「部分回滚补救（先 dry_run 摊证据）」，
> 在 v1.22.0 发布时**只做到了 UI 那一半**：控制台的补救向导确实先发 `dry_run` 预览再问
> 「现在补吗」，但服务端 `ChangeService.RollbackChange` **从未读过 `req.GetDryRun()`**——
> 所谓预览**真的执行了回滚**（补偿派发到生产主机、run 状态被改写成 `rolled_back`、审计链留下
> 一条 `rollback` 记录），而真正点「现在补」的那一次又因状态已变成 `rolled_back` 被准入检查
> 拒绝。**该契约直到 2026-11-06 才兑现**（见本文件 [Unreleased] 与 `docs/levee-api.md` §13）。
> 发布说明不应声称 tag 里还没有的行为；此处按实况订正，原文不抹。

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
