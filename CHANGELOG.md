# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 修复（audit 表 action/result 词表：注释里 10 个值零写入方，真实写入的 16 个不在其中；两对孪生拼写，其中一处让重试上限可被绕过）

- **现象（扫出来的，不是照抄注释）**：`audit.action` 的 schema 注释写着 `plan|apply|verify|rollback|approval|lock|credential|archive|login|config`——**这些值大多没有写入方**，而真正被写的 16 个值一个都不在注释里；`audit.result` 注释写 `success|failure|denied|error`，其中 `failure`/`error` 全仓零写入，真实落进去的却是"迁移到的运行状态"以及两处散文。按注释写查询的人会一条都查不到，而且不会报错。
- **两对孪生拼写**（同族缺陷）：① `retry_host`（CLI）vs `retry-host`（gRPC）；② `pause_all`（pause 包）vs `pause-all`（gRPC 的 bulkTransition）。两侧各自"自洽"，正是批次状态那次的形状。
- **其中 ① 是真缺陷，不只是难看**：按主机重试的上限（`docs/levee-api.md`："重试次数有上限（默认 3）"）**靠数审计行**实现——`cmd/levee` 的 `countHostRetries` 按 `Action == "retry_host"` 且 `Target == "<run>/<host>"` 计数，而 gRPC 路径写的是另一种动作名 + 逗号拼接的主机列表 ⇒ **经 API/控制台触发的单机重试对 CLI 的预算完全不可见，上限可被交替入口绕过**，且没有任何测试会红。CLI 那条路径自己写自己数，所以自测全绿。
- **修法**：`internal/state` 建 `AuditAction*` / `AuditResult*` owning 常量（`audit` 是该包的表，与既有 `BatchState*`/`StepStatus*` 同规矩），`Target` 的形状收敛成一个函数 `state.AuditTargetHost(runID, host)`；写入端全部收敛——`change_service.go`、`template_service.go`、`rest_gate.go`、`lock.go`、`cmd_retry.go`、`cmd_cancel.go`、`cmd_rollback.go`、`cmd_pause.go`，`pause` / `template` / `audit` 三包原有的私有常量改成**别名**（别名即 owner 的值，无法漂移）。`result` 里装运行状态的改用 `runstatus.Status*`——那是运行状态的 owner，不另造名字。
- **gRPC 的 RetryHost 改为逐台一行审计**（动作名与 Target 形状都与 CLI 同源），因此 API 触发的重试从此计入 CLI 的预算；这也更贴近它的语义：多主机重试本就是 N 次独立重试。
- **两份 schema 注释（sqlite + pg）改成真词表**，并新增守卫 `internal/state/audit_vocabulary_test.go`：`action` 与常量集**双向相等**（多一个不存在的值、漏一个已声明的值，都判红）；`result` 因为它合法地装**两族**值（结局词 + 迁移到的运行状态），守卫要求两族都被列全——运行状态那族**从 `internal/runstatus` 解析**而不是复述，那里加一个状态就会让注释变错并判红。
- **跨路径守卫** `cmd/levee/retry_budget_crosspath_test.go`：用 gRPC 的写形状种审计行，再用 CLI 的 `countHostRetries` 数——断言的是**入口之间的契约**，不是各自的自洽。变异实测：把 gRPC 侧改回旧形状，`TestRetryHost` 以 `one row per host, each matching the budget's Target shape` 判红。
- **顺手发现同族第三处**：`internal/pause` 自带一份 run 状态词表（6 个值，逐字等于 `runstatus` 的），而它写进的正是 `run.status` 列——那里的注释说"为免 import 环"，但 `runstatus` 是只依赖 `strings` 的叶子包（`go list -deps` 实测只有它自己），环不存在。已改为别名。
- **登记不修（各需一次独立决定）**：① `approval_kickoff` 把审批档位 `tier` 写进 `result`，而 `result` 是枚举列、没有 detail 列可放——要么给它一列，要么别记；② 未结算的 quorum 把散文 `"recorded; quorum pending"` 写进同一列，已给它常量（`AuditResultQuorumPending`）以免写入端与 REST 回显漂移，但"散文进枚举列"这件事本身还在。**【已收口，见下一个 `[Unreleased]` 小节】**决定不加列、三处散文全部移出该列，写入端还会在越界时告警；③ **API 侧仍不自行拒绝超限的重试**——本次只让它记的行被 CLI 的预算数到，控制台/API 路径自己要不要在超限时拒绝是产品决定（改了就是行为变更：原本成功的一次重试会开始报错）。**【已收口，见下一个 `[Unreleased]` 小节】**用户拍板后，两个 RPC 现在都在执行前按同一份预算拒绝。
- 验证：`go test ./...` **65 包全绿（EXIT=0）**；`gofmt` 干净；新增 2 个测试文件（词汇守卫 + 跨路径预算），改 3 处既有测试断言（它们此前钉的正是旧拼写与旧 Target 形状——反过来说明这批碰到了真实契约）。

### 修复（audit.result 只剩 token：三处散文收口；写入口从此会"喊"）

- **决定：不加 `detail` 列，改为不记散文。** 审计链把固定字段集拼起来做哈希（`auditContentFields`），加第 9 个字段进哈希会让**全部历史行**验签失败——审计链没有 trace 那套 V1/V2 回退（它随 v7 schema 一次引入）；而不进哈希，就等于给 WORM 表开一个"改了也不破链"的列。为三个衍生值付这个价不值当。散文移除后信息各有归宿（写在每个写入点的注释里）。
- **三处写入端收口**（只修两处，这列的契约仍是假的）：① `approval_kickoff` 不再把审批档位 `tier` 写进 `result`，改写 `success`——档位没丢：这行的 `Target` 就是 approval id，它的行上有 `level`，run 上有 `ApprovalLevel`；② 未结算 quorum 的 `AuditResultQuorumPending` 从散文 `"recorded; quorum pending"` 改为 token `quorum_pending`（REST 给移动端的那句人话是**另一件事**：`internal/grpc/rest.go` 的 UI 文案，二者 free to differ，已在常量的注释里点明）；③ `rest_gate` 原先把 `outcome + ": " + 截断消息` 拼进该列，现只写 outcome，消息回到响应（本就在）与一行 INFO 日志——顺带一个安全理由：门禁消息里可能有通道输出，不该冻进 WORM 行。
- **写入口第一次会"喊"**：`audit.Record` 现在调用 `state.AuditResultKnown`（两族 = `AuditResult*` ∪ `runstatus.All`，后者**直接读 owner 的 `All`** 而不是复制），越界则 **warn 但照写**——写歪的行仍是证据，丢行不是。
- **守卫与收紧**：`internal/state/audit_vocabulary_test.go` 新增 `TestAuditResultVocabularyIsTheTwoDocumentedFamilies`（谓词与源码解析出的常量集**双向相等**，另拒绝 `L2` / `recorded; quorum pending` / `passed: disk ok` 三例散文）；`internal/audit/record_vocabulary_test.go` 钉住"越界要 warn 且行仍落库"与"两族静默"；既有三处断言从 `Contains` 收紧成 `Equal`——它们此前钉的正是旧形状（`Contains(a.Result, "passed")` 分不出 `passed` 与 `passed: …`，`rest_gate_test.go` 的这条正是漏过一次的形状）。
- **变异七条逐一变红**：词表删 `quorum_pending`；谓词放行散文；schema 注释漏 token；`Record` 不 warn；门禁把消息拼回 `Result`；kickoff 写回 tier；quorum 写回句子。
- 验证：`go test ./...` 全绿（EXIT=0）、`gofmt` 干净、`scripts` 48 例 OK（两份 schema 注释都改了）。

### 修复（冷导入超时：一份常量三处手抄，第四个重导入的 spec 根本没有）

- **形状**：`vi.resetModules()` + `await import()` 的 spec，第一例按 vitest 默认 5000ms 计时，而冷转换空闲时约 1s、负载下实测 18090ms ⇒ 忙的时候随机红。上一批的修法是把 `COLD_IMPORT_TIMEOUT = 30_000` **手抄进三个 spec**（conversation / cluster / client）；`composables/useTheme.spec.ts` 同样重导入模块图，**从头到尾没有超时**——flake 不是消失了，是还没红到。
- **收口**：常量搬到单一 owner `web/src/test/coldImport.ts`（"18090ms under load" 的实测依据只留一份），四个 spec 改为从 owner import；`useTheme` 的套件接上超时。
- **守卫** `web/src/test/coldImport.spec.ts`：用 typescript 自己的 parser 扫 `src/**/*.spec.ts`（**注释里的提及不算调用点**——守卫自己的头部就提及了这个调用，第一版用字符串匹配时它把自己扫成了违规），要求「凡真实调用 `vi.resetModules()` 的 spec 必须 import 并应用该常量」「常量只允许在 owner 里声明」「owner 的值不得回落」；找不到 `web/src` 或扫到 0 个 spec 一律硬失败（静默扫空比不扫更糟）。
- **变异五条逐一变红**：useTheme 去掉超时（原缺陷）/ conversation 重新手抄一份 / 值改成 5_000 / 新增第五个重导入 spec / owner 改名（硬失败，而不是空集通过）。
- 验证：`npm run test` **119 例全绿（13 文件）**、`vue-tsc` 干净、`vite build` 产物与 `internal/web/dist` **逐字节一致**（本批只动测试，不动产物）。

### 门禁（重试上限终于由服务端强制；顺带修好每主机计数器的截断）

- **重试上限此前只在 CLI 里存在**。`docs/levee-api.md` 写着"重试次数有上限（默认 3），超限升级人工"，但那个上限是 `cmd/levee` 的私有逻辑：`RetryChange` / `RetryHost` 两个 RPC **完全不检查**，所以经控制台/API 可以无限重试——文档承诺的封顶在服务端不存在。（上一批修的是"API 写的行 CLI 数不到"，这一批修的是"API 根本不数"。）
- **预算逻辑提取成共用 owner `internal/retrybudget`**：`MaxAttempts`、`RunUsage`、`HostUsage`、`Exhausted` 四样，CLI 与 RPC 都调它——上限只定义一次，计数只有一份实现。`cmd/levee` 的 `maxRetryAttempts` 与两个计数器改成别名/委托（值不变，`[exit=5]` 文案与 JSON 输出不变）。
- **顺带修掉一个此前查不到的计数 bug**：`HostUsage` 原来是"先 `Limit=4` 拉最近 4 行、再在 Go 里按 Target 过滤"——多主机交错重试时，目标主机的行会被挤出窗口。变异实测：3 次 `web-01` 加 4 次其他主机（时间更晚）之后，`web-01` 的计数读到 **0** 而不是 3，即**预算在主机被分散重试时完全失明**（而那正是最需要它的时候）。修法是给 `state.AuditFilter` 加 `Target` 字段、在 SQL 层过滤（sqlite 与 pg 同步，tenant 装饰器透传），`LIMIT` 于是作用在已收窄的集合上。
- **两个入口的语义**：`RetryChange` 按整轮预算（`retry` 行）计，**检查在 replan 分支之前**——replan 会重新生成计划并重新执行，豁免它等于把 replan 变成新的绕过路径；`RetryHost` 按每主机预算（`retry_host` 行、Target 精确匹配）计，**任一主机超限就整批拒绝**（部分执行会是静默超额），并点名是哪几台。拒绝码 `ResourceExhausted`（REST 429），消息指向可达的升级路径：重新规划并审批应用、或克隆一条新变更。
- **拒绝发生在执行之前**：两个 RPC 的预算检查都在调引擎之前，测试的断言不是"返回了什么错"而是**引擎零调用**（`recordingEngine.retryCalled == 0`）——被拒的重试不能执行。
- **顺带：`RetryHost` 的空主机列表**此前会被交给引擎（引擎报一句 `Internal` 风格的手工错误）；现在显式返回 `InvalidArgument` 并说明"要重试全部失败主机请用 RetryChange"——空列表不是"全部"，接受它会让审计行声称做过从未发生的事。
- **登记不修（需要一次产品决定）**：查这条时发现 `RetryChange`/`RetryHost`/`CancelChange`/`ArchiveChange`/`CloneChange`/`CreateChange` 六个写 RPC **没有任何 `authorize` 调用**——`plan`/`apply`/`rollback`/`approve`/`reject` 都按权限矩阵判定，而"谁能重试/取消/归档/克隆"目前没判（`permission.ActionCancel` 早在词表里、`policyResourceFor` 也把它映射到 `change:*`，但没有 RPC 消费它；设计文档 §4.4.8 写的"发起人或管理员"也没有对应机制——矩阵是 team×env×action，表达不了"发起人"）。**不在本批授权范围内**：接上它会改变现有部署里谁能操作，且"重试该映射到哪个 action（`apply`？新的 `retry`？）"是产品决定。**当前姿态如实记录**：无矩阵时一切照旧（无人受影响）；配了矩阵的部署里，这六个 RPC 仍可被任何已认证调用方使用。
- 验证：`go test ./...` **全绿**；新增 `internal/retrybudget/retrybudget_test.go`（3 例，含上面那条截断回归）与 `internal/grpc/change_service_retrybudget_test.go`（9 例子测试）；变异四组各自按预期红——摘掉两个 RPC 的强制（3 条子测试红）、把 Target 过滤改回 Go 侧（截断回归红、计数 3→0）、上一批的 gRPC 旧行形状（`TestRetryHost` 红）。

## [v1.21.0] - 2026-10-09

> **发布状态：已切版。** 附注 tag `v1.21.0`（tag 对象 `b28bbd0f`，指向合并提交 `fcac5dd3`）于 2026-10-09 推送，`release.yml` 三段（CI gate / goreleaser / container image）全部 success：GitHub Release `draft=false`、`publishedAt=2026-10-09T03:50:51Z`，资产 7 个（6 个平台包 + `checksums.txt`）；镜像 `ghcr.io/levango7/levee:v1.21.0` 按外部事实核过——`manifests/v1.21.0` 返回 **200**，`tags/list` = `[v1.18.0, latest, v1.19.0, v1.20.0, v1.21.0]`。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.21.0` / 逐字 `v1.21.0`（顺序不能反：规则②要求它逐字出现在 tag 集合里）。 本节共 **12** 个小节，三条主线：七批词表/判据收口（批次、分配、会话、系统页的中文标签与 owning 常量，每处都补了从常量源解析的守卫）、控制台重做与"玻璃只上 chrome"+ 命令面板、以及三处**门禁级**缺陷（CHANGELOG 结构守卫抓到自己登记的那类损坏；`release-gate` 的 `unittest | tee` 在 `bash -e` 下吞掉一切断言失败；`govulncheck` 被 `go` 指令挡住），另收口 SA-007/SA-011 与看板变更名。**切版顺序（按既有约束执行）**：先折叠 CHANGELOG 与 release notes、chart 不动（`check_release_versions.py` 规则②要求 `image.tag` **逐字**出现在 tag 集合里，tag 存在之前不能升），推 tag 并确认 `release.yml` 三段 success 之后，再随下一笔把 chart 两处升到 `1.21.0` / 逐字 `v1.21.0`。release notes 见 [`docs/release-notes/v1.21.0.md`](docs/release-notes/v1.21.0.md)。

### 修复（批次状态在中文页面上渲染的是线上原值；/cluster 与 /monitor 各自一套词表）

- **`/monitor` 与 `/cluster` 的批次标签把线上字符串直接当文案渲染**（`{{ b.status }}`）：页面其余部分都是中文，批次这一列却是 `completed` / `rolled_back`。#88 修的是判色键词表错，判色已经对了但文字仍是英文——同一条批次的两个呈现面。移动端审批页 `状态` 字段同类（`web/src/views/MobileApprovalView.vue` 渲染 `change.status` 原值，而 `STATUS_LABEL` 早已存在且 `StatusTag.vue` 一直在用），一并按标签表取文案。
- **`/cluster` 的批次行是第二份手写词表**，与 #88 刚统一到 `web/src/utils/batch.ts` 的那份并行存在：内联三元把成功键在**没有任何写入方产生**的 `done` 上（写入方是 `completed`），所以真实的完成批次在 `/cluster` 上是灰的；`interrupted` 被涂成 danger，而 `internal/state` 与 `/monitor` 都拒绝把"跑到一半停了"说成"失败"；进度另有一个本地 `batchProgressPct`，缺 `succeeded+failed > total_hosts` 的钳制。这就是"孪生副本各自落门禁"的形状：`/monitor` 修好了，`/cluster` 原样留着。
- **修法**：`web/src/utils/batch.ts` 改为单一声明 `BATCH_STATES`（七个拼写）+ 三张 `Record<BatchState, …>`（label / tag / bar）。用查表而不是 `switch` 是为了让"新增一个状态忘了配中文/配色"变成 `vue-tsc` 编译错误而不是运行时悄悄落到默认分支；未识别的线上值仍原样显示、判灰，**不猜翻译**（猜出来的"已完成"是在替引擎宣布它没宣布过的结论）。两个视图删掉本地实现，统一 `batchLabel` / `batchTagType` / `batchBarStatus` / `batchProgress`。
- **验证**：前端 `web/src/utils/batch.spec.ts` 16 例（含结构性一条：`BATCH_STATES` 里每个拼写都必须有非空且不同于拼写本身的标签——按状态列出来跑，不是抽查）；`npm run test` 全绿（本小节收尾时 69 例 = 原有 50 + 批次标签 16 + 面板形状 3）、`vue-tsc` 干净、`vite build` 后按 CI 同法刷新并核对 `internal/web/dist`（`batch-*.js` chunk 里七个标签齐全，`ClusterView`/`MonitorView` 两个 chunk 都 import 它）。跨语言两条落在 owning 包：`internal/state/batch_status_vocabulary_test.go` 从 `store.go` 解析 `BatchState* = "…"` 常量（不是复述，复述会让新常量无人发现）与 TS 的 `BATCH_STATES` 双向对齐，另要求每个 Go 拼写都在 `LABEL_BY_STATE` 里有键。变异五条逐一变红：TS 删 `rolled_back`、TS 加不存在的 `success`、`store.go` 新增 `BatchStateAborted`（两条守卫同时红）、删 label 键（只 label 守卫红，证明二者不是同一条断言写两遍）、把 `BATCH_STATES` 改名（锚点缺失必须自曝而不是解析成空集通过）。`go test ./internal/state ./internal/wiring ./internal/grpc` 全绿。
- **真机渲染核对**（无后端，`internal/web/dist` 由一个只读桩服务器供 `/api/v1` 七种批次状态各一行）：`/monitor/run-1` 七行标签依次为 已完成/已完成/失败/已回滚/已中断/执行中/待执行，条形类 `is-success`/`is-exception`/`is-warning`/中性，`leaked=[]`；`/cluster` 填 run_id 查询后同一套七个标签与配色，与 `/monitor` 逐行一致；`/m/approve/run-1` 状态列显示"执行中"。
- **`/cluster` 的批次面板在默认形态下根本渲染不出来（本批一并修）**：`web/src/views/ClusterView.vue:73-81` 是 `v-if="backend === 'sqlite'"` / `v-else-if="backend === 'postgres' && nodes.length === 0"` / `v-else` 三段，面板原先落在最后的 `v-else` 里 ⇒ **SQLite 部署（默认形态）下整块看不到**，与有没有 worker 节点无关；同一批数据在 `/monitor` 可见，所以这不是"没有数据"而是入口被分支挡掉。它读的是 `GET /system/batch-status`，per-run 数据，与集群协调无关 ⇒ 移出该分支。实测（桩服务器给 `backend=sqlite, nodes=[]` 这个曾经看不见的形态）：改前页面上找不到"Run 批次进度"标题；改后标题、输入框、查询按钮都在，填 run_id 后真点"查询"，七行批次照常渲染且标签配色与 `/monitor` 逐行一致。该形状由新增的 `web/src/views/ClusterView.spec.ts` 钉住（3 例：分支闭合点必须早于面板标题、二者之间不得再有条件渲染、面板不得依赖 cluster-status 载荷）；两条变异各自变红（把面板挪回分支内 / 给卡片重新加 `v-if="backend === 'postgres'"`）。用结构断言而不是渲染断言的原因：本仓没有 `@vue/test-utils`，结构断言至少挡住"分支条件把它包回去"这条复发路径。
- **同页"分配状态分布"渲染的是 assignment 词表**（`pending`/`executing`/`done`/`interrupted`）的原值，与批次词表**同名不同物**（`done`/`interrupted` 两边都有但含义不同），需要单独一张标签表，故不在本批——同一个文件里叠两批改动只会让归因变难。

### 修复（批次状态还有第三份抄件：schema 的行内注释；clone 路径的状态写在自家常量上）

- **`batches.status` 的注释是一份假文档**：两个引擎（`internal/state/schema.sql:53`、`internal/state/pgschema.sql:50`）都写着
  `pending|running|completed|failed|skipped`。`skipped` 没有任何 `BatchState*` 常量、也没有任何批次行的写入点
  （`internal/wiring/persist.go:222/429` 的 `"skipped"` 是**步骤**状态，写进 `steps.status`），而真正写入的
  `rolled_back`、以及 `batchDoneStates` 认账的 `done` / `interrupted` 反倒不在名单上。列定义是 `status TEXT NOT NULL`，
  **两库都没有 DEFAULT**（`grep "DEFAULT 'done'" --include=*.go --include=*.sql` 零命中），所以库自己也不会产出 `done`。
  后果不是难看而是可执行：读注释的人会以为库里可能出现 `skipped` 并据此写查询或 UI —— 与 #88 同源（两套词表各自都绿）。
  注释改为按 `internal/state` 的七个常量列全，并新增守卫 `internal/state/batch_status_schema_comment_test.go`：
  名单**从 `store.go` 解析**（复述一份清单会让新常量无人发现），要求与常量集**双向相等**，且**两个文件各自独立校验**。
  另加一条 `TestBatchesStatusCommentIsNotTheStepsComment`：解析必须落在 batches 表块内——`steps` 表也有一行同形的
  `status TEXT NOT NULL, -- …` 注释，按"文件里第一条 status 行"解析会读到另一套词表。
  变异四条各自变红且点名正确：注释删 `done` ⇒ `omits "done"`；注释加 `skipped` ⇒ `no BatchState constant declares`；
  注释整体换成 steps 词表 ⇒ 两条守卫同时红；**只把一个引擎的注释删掉 `rolled_back`** ⇒ 失败信息点名那一个文件
  （先试的是"只改 SQLite 侧"，结果被另一侧代答，说明这条必须单独验）。
- **`internal/template/clone.go` 用自己的一套常量写 state 的状态列**：`StatusDraft = "draft"` / `StatusPending = "pending"`
  分别写进 `runs.status`（:175）、`batches.status`（:203）、`steps.status`（:241），而原来那段注释给的理由是
  "避免 import engine 造成环"——**理由不成立**：这个包本来就 `import` 了 `internal/state`，而
  `go list -deps ./internal/runstatus` 显示 runstatus 除自身外不依赖任何本仓包（只 import "strings"），
  `internal/state` 也不 import `template`，接上 owning 常量不会成环。
  改法：run 写 `runstatus.StatusDraft`、batch 写 `state.BatchStatePending`，模板包的常量只留 step 用并改名
  `StepStatusPending`（"pending" 一词同时充当 run/batch/step 三套词表的默认值，正是靠拼写巧合对齐）。
  对齐之后 `template` 包不再有 run/batch 的平行词表；跨包改名前按旧标识符精确查过：
  `template.StatusDraft` / `template.StatusPending` 全仓零命中，包内只有 clone.go 三处 + clone_test.go 三处断言。
- **测试断言改用字面量，而不是换成 owning 常量**：`assert.Equal(t, state.BatchStatePending, cb.Status)` 在写入端
  也用同一常量时就是 `x == x`，值漂移照样绿（这条教训来自"门禁全绿地校验自己合成的命题"）。字面量才是独立真值：
  它是落进列里的值、是 `batchDoneStates` 读的值、是 web UI 映射的键。实测：把 `BatchStatePending` 改成 `"queued"`，
  `TestClone_Success_PreservesBatches` 立刻以 `expected: "pending"` 变红；还原即 `ok`。
- **两条词表缺口如实登记，本批不动**：① `steps.status` 全仓没有 owning 常量，写入端是散在
  `internal/wiring/persist.go:131/222/224/429` 的裸字面量，注释那份 `pending|running|success|failed|skipped` 因此
  没有可对齐的真值；② `runs.approval_status` 同样是裸字面量（`internal/wiring/run.go:522`、
  `internal/grpc/change_service.go:601` 写 `"pending"`，:990 比较 `"approved"`）。给它们建 owning 包是一次真正的
  收敛改动（要一并改写入端并配守卫），不在这一批里顺手做。

### 修复（分配状态分布也在渲染线上原值；两套词表拼写重叠、语义不同）

- **`/cluster` 的"分配状态分布"把 assignments 的状态原值当文案渲染**（`ClusterView.vue` 里 `{{ state }}`）：
  与 #90 那批同一类缺陷，但读的是**另一套词表**——`internal/state/store.go:738-741` 的
  `AssignStatePending` / `AssignmentStateExecuting` / `AssignmentStateDone` / `AssignmentStateInterrupted`
  （四个状态；`AssignResult*` 是第二组值域 completed/failed/rolled_back，这一栏不渲染，别混）。
  现在按 `web/src/utils/assignment.ts` 取文案：待领取 / 执行中 / 已完成 / 已中断。
- **为什么不复用 #90 那张批次表**：两套词表在拼写上重叠（`pending` / `done` / `interrupted` 三词双跨）而语义不重叠——
  批次的 `pending` 是"这一批还没开始跑"，分配的 `pending` 是"还没被 worker 领取"；批次的 `done` 是历史拼写
  （没有任何写入方，见 #90），分配的 `done` 是活跃终态（`internal/dispatch/dispatch.go` 就在写它）。
  并表会让操作者把"没派出去"读成"没跑起来"。这条语义差异由 `assignmentLabel('pending') !== batchLabel('pending')`
  一条断言钉住，变异实测：把 `待领取` 改成 `待执行`（即抄批次表）该用例立刻变红。
- **守卫**：`web/src/utils/assignment.spec.ts` 6 例（含结构性一条：每个声明状态都必须有非空且不同于拼写本身的标签）；
  `internal/state/assignment_status_vocabulary_test.go` 两条跨语言守卫从 `store.go` **解析状态组**
  （正则要求标识符含 `State`，所以结果组 `AssignResultCompleted` 不会被误收；另有断言显式拒绝 Result 混入）
  与 TS 的 `ASSIGNMENT_STATES` 双向比对，并要求每个 Go 拼写都在 `LABEL_BY_STATE` 里有键。
  本包的批次守卫已有同名 helper，这里刻意自带一份本地 helper 并另起名——不是重复劳动，是为了让本批
  能独立通过 CI（堆叠在未合并分支上的 PR 拿不到 CI）。变异五条逐一变红：`store.go` 新增 `AssignmentStatePaused`
  （两条同时红）／TS 删 `executing`／TS 加只有结果组才有的 `completed`（方向二抓住借词表）／改 TS 声明名（锚点自曝）／
  抄批次措辞（跨词表那条红）。
- **验证**：前端 `npm run test` 75 例、`vue-tsc` 干净；`vite build` 后按 CI 同法刷新 `internal/web/dist`，
  `diff -r web/dist internal/web/dist` 无差异（标签被内联进 `ClusterView-*.js` chunk，`待领取` 在其中可 grep 到）；
  `go test ./internal/state/` 全绿。真机核对（桩服务器给 `backend=postgres` + 一个节点 + 四类分配计数）：
  分布四行依次是 待领取 / 执行中 / 已完成 / 已中断，`leaked=[]`；同一页四个小节标题（节点、分配状态分布、
  Worker 负载、Run 批次进度）都在，#90 的面板可达性没有回退。

### 修复（系统页与会话页也在把线上原值当文案；doctor 判定其实有四个值）

- **两处渲染原值**：`SystemView.vue` 三处（`status.status` 健康状态、`doctor.status` 诊断总评、`row.status` 每条检查）、
  `ConversationView.vue` 一处（`s.state` 会话状态）。与 #90/#92 同一类缺陷，但这是**第三、第四、第五套词表**，
  所以各自一张表，不并入已有表。
- **`skip` 是守卫自己抓出来的，不是我数出来的**：我最初按"pass/warn/fail 三个判定"写表，
  `internal/grpc/system_verdict_vocabulary_test.go`（解析 `system_service.go` 里 `c.Status = "…"` /
  `overall := "…"` 的字面量赋值，而不是复述清单）当场变红，指出端点还会产出第四个值 `skip`
  （`system_service.go:346-349`：数据库检查在未加载配置时主动跳过）。若按我原来的三分表实现，
  诊断表里那一行会直接显示英文 `skip`。补上后标签是 **已跳过**，且 `verdictLabel('skip') !== verdictLabel('pass')`
  一条断言钉住"没检查过"不等于"通过"。
- **会话侧的源头不是常量组，是 `SessionState.String()` 的 return 值**：`SessionState` 是 int 枚举
  （`internal/conversation/session.go:34-50`），线上传的是 `String()` 的结果，因此
  `session_state_vocabulary_test.go` 解析那个方法体内的 `return "…"`（新增枚举成员没命名、或命名了 UI 没配标签，
  都会在这里变红）；顺带把 `default: return "unknown"` 也纳入表内，避免"未知"被当成一个真实状态机节点。
- **`ConversationView` 的判色原来是内联三元**（只认识 `failed`/`done`，其余六个态一律灰），
  其中 `reviewing` 恰恰是"引擎在等人确认"的那个态——灰掉等于把最需要人动手的一步藏起来。
  改成 `TONE_BY_STATE` 查表（漏配是 `vue-tsc` 编译错误），`reviewing` 现在是 warning。
  这与 #92 里 `/cluster` 自带第二份词表是同一个形状：视图里的手写副本与旁边的可测表各说各话。
- **健康侧与判定侧必须分开**：健康有 `unknown`（探针说不清），判定根本没有这个值；
  合并两张表就会把"说不清"和"没通过"涂成同一句话。两条守卫分别钉在
  `internal/diagnosis/health_status_vocabulary_test.go`（解析 `HealthStatus` 常量组）与
  `internal/grpc/system_verdict_vocabulary_test.go`（解析裸字面量），后者还在断言里明确拒绝
  `healthy` 混进判定集。
- **验证**：前端 `npm run test` 91 例（本批新增 `diagnosis.spec.ts` 8 例 + `session.spec.ts` 8 例，均为毫秒级、无网络），
  `vue-tsc` 干净；三条 Go 跨语言守卫全绿；**变异七条逐一变红**（健康表删 `unknown`／健康表凭空加
  `vibrating`／`HEALTH_LABEL` 丢键／判定表丢 `skip`／会话表加 `String()` 产不出的 `paused`／
  把 `SESSION_STATES` 改名以验锚点诚实／把 `reviewing` 的 warning 退回 info 复现旧行为）；七条跑完后
  三条守卫与全量 vitest 均还原为绿（脚本 `finally` 里逐文件回写并再跑一遍，不是口头声明）。
  另外登记一条与本批无关的既有缺陷（不在本批修）：`src/api/conversation.spec.ts` 里 `vi.resetModules()` + 动态
  `import('./index')` 的用例平时 ~1.1s，偶尔整段挂住并以 `Test timed out in 5000ms` 变红（实测一轮里该用例累计
  18090ms；本仓 vitest 未配 `testTimeout`，默认 5s）。同一棵树的 6 轮单跑翻 1 轮，而无本批改动的另一棵树单跑
  4 轮全绿 ⇒ 并发负载敏感，会随机咬任何 PR 的 `frontend` job。本批两个新 spec 都是毫秒级、不做动态重导入，
  但它们让套件多了两个文件、提高了触发概率，所以如实写在这里而不是"全绿"。**这条自测本身也翻过一次车**：第一版变异脚本在 Windows 上用
  `shell=True` 传 `-run 'TestX'`，cmd.exe 不剥单引号 ⇒ Go 匹配到零个用例仍报 `ok`，于是把"守卫没红"
  误读成守卫失效；手工复现（M1 给出 `health state "unknown" has no entry…`、rc=1）证明守卫一直是好的，
  坏的是脚本。第二版脚本改为不引号、不用 `|` 拼测试名，并对 `no tests to run` 主动判红。
- **真机核对**（`internal/web/dist` 由只读桩供 `/api/v1`）：`/system` 健康行显示 **降级**（桩故意不给
  `healthy`，避免"绿着的 fixture 证明不了映射"）、诊断总评 **告警**、四行依次 **通过 / 告警 / 未通过 / 已跳过**；
  `/conversation` 八个会话（每态一行）依次 等待输入 / 诊断中 / 生成建议中 / 等待确认 / 执行中 / 已完成 / 已失败 / 未知，
  tone 各自区分，两页 `leaked` 均为空。
- **如实登记、本批不动**：`system_service.go:107` 与 `:152` 用裸字面量 `"healthy"` / `"degraded"` 写
  `SystemStatus.status`，而 owning 常量组就在 `internal/diagnosis/health_probe.go:39-49`（同仓已有 import 先例，
  不构成环）——与 #91 里 `template/clone.go` 的平行常量同一族，属产品码改动，留给单独一批；
  另外 `web/src/types/levee.ts:117` 把该字段类型写死成三个值（无 `unknown`），与 `HealthStatus` 四值不同源，
  本批的查表入口是 `string`，不受该类型限制。

### 修复（CHANGELOG 自己也被静默改坏过：master 上出现两个 `## [Unreleased]`）

- **现象**：修完 #94 之后 `CHANGELOG.md` 有两个 `## [Unreleased]`（第 5 行与第 78 行）。第一份下面已有 #90/#91/#92 三节，
  第二份下面是 #94 那节 ⇒ 按目录读文件的人只看得到最后一节，前三节在结构上"消失"，而**内容还在**。
- **成因是一次锚点选择**：#94 那节的落笔锚在 `## [v1.20.0] - 2026-10-07` 上、连带写了一个 `## [Unreleased]`；
  三方合并时两侧改的不是同一行，**git 不冲突、门禁不报错**，于是静默进 master。同一个错我在 #92 合并时手工发现并改掉了
  （那次是把它拼成单一标题），但没有任何机器检查防它复发。
- **修法**：只删重复的 3 行（空行 + 标题 + 空行），四节重新挂在唯一的 `## [Unreleased]` 下；
  核对方式是数出来而不是看着说：`git diff --numstat` 为 `0 插入 / 3 删除`，`grep -c "^- "` 前后都是 **504** 行条目，
  即零内容丢失、零内容新增。
- **补上的守卫** `scripts/test_changelog_structure.py`（4 条真文件断言 + 3 条检测器自检）：唯一的 `## [Unreleased]`
  且必须在第 5 行、它必须是第一个 `## ` 标题、它与第一个版本标题之间的内容必须归进 `### ` 小节、
  版本号标题不得重复。**检测器自检不是装饰**：一条只会检查"当前恰好干净"的文件的门禁，无法证明它看得见损坏，
  所以对合成的好/坏样本跑同一表达式，含一条"重复标题出现在文件深处（第 200+ 行）也要抓到"——
  真实事故正是标题隔了 73 行，只看文件头的写法会漏。
- **双向验过**：修复后的文件 7 例 OK；`git checkout HEAD -- CHANGELOG.md` 换回 master 上那份损坏版即
  `AssertionError: Lists differ: [5] != [5, 78]`；再换回修复版重新 OK。登记进 `release-gate` 的逐套件点名表
  （`ci.yml` 里那条 `for suite in …`），因为 discovery 空收集也会以 `Ran 0 tests / OK` 蒙混过去。
- 本批同时把 #27 那条 `frontend` 随机红的根因记录在案（见下一条 PR/另一分支），它属于同一族：
  **能静默通过的检查，不等于在检查什么**。

### 修复（`/system/status` 的健康值以前是手抄的）

- **`GetStatus` 用裸字面量写自己声明的词表**：`internal/grpc/system_service.go:107` 写 `Status: "healthy"`、
  `:152` 写 `resp.Status = "degraded"`，而这组值的 owning 常量就在同仓的
  `internal/diagnosis/health_probe.go:39-49`（`StatusHealthy` / `StatusDegraded`），`internal/grpc`
  也已经 import 了该包（`diagnosis_service.go:23`）⇒ 不存在"为了避环才手抄"的理由。
  这正是 #88 的成因形状（state 按 `"done"` 判完成、引擎写 `"completed"`，两边各自都有绿测试）与
  #91 里 `template/clone.go` 用本包常量写 state 列的同一族：**手抄只在拼写巧合成立时才正确**。
- **两处改用常量**（值不变，`pb.SystemStatus.status` 仍是 string），并在函数注释里写明词表归属。
- **新增守卫** `internal/grpc/system_health_vocabulary_test.go` 两条：① 函数体里 `Status:` / `resp.Status =`
  出现裸字符串即判红，并显式要求两个 `diagnosis.Status*` 引用在场（作用域只圈响应状态，
  同函数里 `state.RunFilter{Status: "running"}` 是**运行**词表，不该被抓——这正是 #94 里
  "同名不同物"的教训）；② 前端类型 `web/src/types/levee.ts` 的 `SystemStatus.status` union 与
  常量组**双向**核对（端点能产出的值必须在 union 里；union 也不许出现常量组之外的自造词），
  常量侧从 `health_probe.go` 源码解析而非复述清单。
- **两处实现细节值得留档，因为它们各藏过一次假结论**：
  ① 我第一版用一条 `(?ms)` 正则去截函数体，静默匹配为零，测试报的是"锚点找不到"而不是"断言失败"——
  改成按行扫描（找到签名列到第一个第 0 列的 `}`）后问题立刻现形；TS 那条也一样，我先假设接口成员
  以 `;` 结尾，而该文件不写分号。判据：**解析不到必须是响的失败，而不是空集合通过**。
  ② 第一次变异我把两处常量一起换回字面量，结果是**编译失败**（`diagnosis` import 变成未使用）——
  那是"语法红"不是"逻辑红"。重做为只改一处、`go build ./internal/grpc/` 先过，
  两条断言才各自报出自己该报的话（`should assign diagnosis.StatusHealthy` + `bare literal assigned to the health status`）。
- **已知且刻意保留的不一致**：类型里还写着 `unhealthy`、常量组里还有 `unknown`，而本端点今天只可能产出
  `healthy` / `degraded`（warnings 非空即降级）。所以 ② 的断言方向是"产出 ⊆ 类型"而非相等，
  并把这条差异写在测试注释里——不用断言把差异抹平，也不用断言制造一个假的完备感。
- 验证：`go build ./...`、`internal/grpc` + `internal/diagnosis` 用例全绿、`golangci-lint --timeout 5m ./internal/grpc/...`、
  `python scripts/check_release_versions.py` PASSED。本批不碰 web 与构建产物。


### 修复（上批登记的两条词表缺口收口；audit 链的封链不再重写内容被改的行）

- **`steps.status` 与 `runs.approval_status` 建 owning 常量，写入端全部收敛**：上一批把两条缺口如实登记为"本批不动"——`steps.status` 全仓没有 owning 常量、写入端是 `internal/wiring/persist.go` 的裸字面量；`runs.approval_status` 同样是裸字面量。现补 `state.StepStatus*`（五个）与 `state.ApprovalStatus*`（三个），`persist.go` 的五个写入点 + 一处比较、`internal/lock` 的两处查询、`grpc/change_service.go`、`grpc/template_service.go`、`template/clone.go`、`wiring/run.go`、`cmd/levee/cmd_new.go` 全部改走常量，`clone.go` 的本地 `StepStatusPending` 改为别名（别名即 owner 的值，无法漂移）。
  **取值集合按写入端扫描确定，不照抄 schema 注释**：`runs.approval_status` 的行内注释写着 `pending|approved|rejected|timeout|skipped`，但后两个值属于**隔壁 approvals 表的 status 列**，run 行上没有任何写入方产生它们。注释与常量都已改正，并新增 `internal/state/status_column_comment_guard_test.go`：解析 store.go 的常量块（不是复述，新常量不会漏检）与两个 schema 文件的对应列注释，双向对齐，两侧缺一即红。
- **守卫的 CRLF 陷阱顺带修掉**：既有的 `batch_status_schema_comment_test.go` 用 `(?m)$` 匹配常量行，在 LF 检出下正常、在 Windows 的 CRLF 工作副本下静默解析成空集后仅以 `require.NotEmpty` 报"块形状变了"——守卫本身在该机器上等于不存在。三处正则补 `
?`，注释写明原因。
- **audit 链的 `Seal` 不再重写"内容与自己的哈希不符"的行**（关闭 SA-002 在 audit 侧的残余）：原实现重写任何哈希不符的行，于是"改一行内容，然后等任意一次审计写入"就会让下一次 Seal 重新链接它、Verify 通过——篡改被自己抹掉。新判据区分两类不符：**行自洽**（`H(内容, 行自己存的 prev) == 行自己存的 curr`，内容完好、只是被后来的行挤走了前驱，如同时间戳下并发写入的乱序落库）→ 正文重链；**行不自洽**（内容已改）→ 不写、返回 `ErrChainBroken`，证据保留。
  **这一版的判据是第二稿**：第一稿照搬 trace 链的"只延展、绝不重写"，被两条既有测试直接证伪——良性重排会被当成永久断链，而 `BuildForce`/`Rebuild` 全仓无生产调用点、CLI 也没有修复子命令，"报断等运维修"这条路对任何人都不可达。一条会对合法写入报警且无法清除的规则，比它要堵的洞更糟。
  **如实登记残留**：**删除**（在 WORM 触发器被摘掉的前提下）仍会被下一次 Seal 重链接覆盖——后继行自洽，判据看不出"前驱被删"与"前驱被挤位"的区别。触发器是删除的防线，Verify 在下一次写入封链前一直报出该断点。
- **新增 `AuditChainBuilder.Rebuild`**：审计侧与 trace 侧 `BuildForce` 对应的显式管理口子，丢弃已存哈希重算全链。它是"已调查并决定重链"的授权动作，故是另一个名字的方法而非 `Seal` 的开关。**与 `BuildForce` 处境相同：无生产调用点、无 CLI**——这是刻意留白的运维决策（什么时候允许重链、谁批准、是否要留审计），不是遗漏。
- 验证：新增 `internal/audit/auditchain_seal_semantics_test.go`（合法重排重链 + 内容篡改报断且不重写 + 重排后幂等）、`internal/state/step_approval_status_vocabulary_test.go`（常量对写入端字面量、过滤器能找到写入行）、`internal/state/status_column_comment_guard_test.go`；`pgstore_auditchain_test.go` 的"重封链修复 curr_hash"断言按新契约改为"Seal 报 `ErrChainBroken`、`Rebuild` 才修复"，并在**活 PostgreSQL 16** 上跑通（`TestPGStore_AuditChainSealAndWORM` 1.14s）。`go build ./...` / `go test ./...`（65 包）/ `golangci-lint run ./...`（0 issues）/ PG 侧五包（state·cluster·takeover·backup·audit）全绿。

### 新增（控制台重做：设计系统、双主题、全部页面的视觉重构）

上一版的控制台只有 59 行全局 CSS、5 个变量和 Element Plus 的默认皮肤——没有设计系统，也就没有可复用的视觉语言。本次把它建成一套控制台界面：**精密、克制、机器数据用等宽字体**，并补齐深色模式。

**设计系统（`web/src/styles/`）**：`tokens.css`（品牌青绿阶、冷灰中性色、语义色、字阶、间距、圆角、阴影、动效曲线，以及把 `--el-*` 逐一映射到语义别名的两个主题块）、`fonts.css`（两个拉丁子集自托管：IBM Plex Sans 承载界面拉丁文与数字，JetBrains Mono 承载**所有机器串**——ID、哈希、时间戳、日志、计数；中文回落系统字体，不打包 CJK 字体）、`base.css`（重置、控制台工具类、Element Plus 组件精修）。**配色只由别名驱动**：组件引用 `--lv-surface` / `--lv-text-2` 这类语义名，深色主题整块重指别名即可，组件无需任何 dark 分支。

**主题**：`useTheme` 组合式（light/dark/system 三态、localStorage 持久化、跟随系统并监听其变化），`index.html` 内联引导脚本在首次绘制前落 class 以免闪白。**它修掉的一个真实缺陷**：应用只记住存储的主题、从未在启动时应用它——一旦内联脚本因 CSP 被剥离或命中缓存的旧壳，控制台会渲染成存储主题的反面且永不自我纠正；模块现在在加载时断言文档状态（测试覆盖「内联引导失败时自我纠正」）。

**外壳（App.vue）**：深色导航轨（两种主题下都是深色，作为控制台的身份与框架）+ 分组导航（变更运营 / 资产与模板 / 平台）+ 活跃项指示条；顶栏含面包屑、本地时钟（运维要把屏幕上的时间与日志/审计行对齐）、主题切换、会话菜单；页面切换过渡。品牌标识改为「堤坝」三重叠石符号，favicon 同步。

**共享组件**：`PageHeader`（统一的标题/说明/动作区）、`MetricCard`（KPI 单元，**固定三行网格**——脚注行始终占位，否则缺脚注的卡片数字会与其邻居差 10px）、`StatusTag` 重写（**点 + 标签**取代彩色药丸；15 个状态映射到 5 种语义色调，每行注释写明运维含义——"回滚"与"执行中"不是一回事，"执行器死了"又是另一回事）、`HashCell`（哈希**中段**省略：首字节用于跨屏比对、尾字节用于区分相邻行，省略尾部会隐去恰好用于区分的那部分）。

**页面**：变更看板（4 个 KPI 卡取自服务端 `totalSize` 而非当页计数，点击即过滤；状态构成条与图例；表格改双行单元格 + 机器串等宽）、实时监控（概览键值化 + 批次**时间线** + 日志改为终端面板，两种主题下都是暗底）、审计（哈希链单元格 `prev → curr` + 校验结果改成裁决面板）、集群（节点卡片 + 负载条）、登录（分屏品牌页，左栏列出流水线的五个真实阶段）、系统（守护进程/构建/诊断清单/配置）、审批/模板/目标机/AI 对话/移动审批/404 全部重构。

**验证**：vitest **101 通过**（新增 10：主题状态机 9 + 上述自我纠正 1）、`vue-tsc` 干净、`vite build` 通过、`internal/web/dist` 同步刷新（CI 的字节新鲜度门禁）、`go test ./...` 65 包全绿（含 4 个解析 `web/src` 的词表守卫——它们钉住 Go 与 TS 的双向对齐，本次未触碰其契约）。另外用**无后端桩服务器 + 无头 Chrome 截图**做了两轮视觉验收：第一轮（8 张，双主题）抓出 3 个真实缺陷——`useTheme` 未应用主题（上述）、全局 `.lv-panel + .lv-panel` 外边距**在栅格里同样命中**导致节点卡片与系统面板错位 16px、KPI 脚注行塌陷；第二轮用像素测量复核，四项几何断言（KPI 基线、节点卡顶边、系统面板顶边、监控两栏底边）散布均已为 0。

### 新增（玻璃材质只上 chrome；命令面板 Ctrl+K）

在控制台重做之后，按"液态玻璃 / Cursor / ZCode"的方向又推进一轮。**判断依据来自实测而非印象**：本机同时装着 Cursor 与 ZCode，我直接读了它们的样式文件——Cursor 默认暗色是**纯中性石墨**（`#181818` 轨道 / `#1F1F1F` 主面 / `#2B2B2B` 发丝线，R=G=B，相邻表面仅差约 3% 明度）；ZCode 的 `--color-background-alt:#26262699` **带 alpha**，且全仓 54 处 `backdrop-filter` 的半径克制在 **blur 5–10px**。（Codex 本机未装、产品页 403，未实测，故不引用。）

**核心取舍：玻璃只给 chrome 层，内容永远实底。** 顶栏、菜单、对话框、命令面板是浮在内容之上的表层，模糊有意义；而面板、表格承载 12px 等宽哈希与状态语义，`backdrop-filter` 会压低文字对比、每元素一次合成开销、滚动时抖动——这也正是 Apple 自己对材质的规则（材质用于导航与控件层）。因此：

- **顶栏改为粘性玻璃**（并且是**滚动容器内**的 sticky，否则内容不会从它下面经过，模糊与不透明看起来完全一样）：半透明 + `blur(14px) saturate(1.6)` + 发丝线 + 1px 镜面内高光；工作区加了几个百分点的**环境色**，玻璃才有东西可拾取。三级回退：`@supports` 无 backdrop-filter → 实底；`prefers-reduced-transparency: reduce` → 实底（这是材质的一部分，Apple 也提供该开关）。
- **镜面内高光 + 同心圆角**：暗色面上的 1px 内侧高光是"玻璃"与"灰色矩形"的分界；圆角改为同心阶（外 12 → 内 8 → 芯片 4，内层 = 外层 − 间距），两层圆角同轴才会读成一体。
- **命令面板（Ctrl/Cmd+K）**：这是 Cursor 真正值得抄的部分——它是**交互**不是配色。粘贴一个变更/run ID 即可直达（本系统两者同键，故给两个视图）；支持页面跳转、主题切换、运行诊断、校验哈希链。判定逻辑全部放在 `utils/commands.ts` 纯函数里（**14 个单测**：排序精确>前缀>包含>子序列、ID 形态识别、自由文本转看板搜索）；面板只负责渲染与键盘。为了让"运行诊断/校验链"是命令而不是二跳，`/system?doctor=1`、`/audit?verify=1`、`/changes?q=` 三条深链同时接通。
- **动效克制**：面板 140ms 缩放+淡入，键盘选中行只做背景色过渡、不做位移——事故期间盯着的控制台不该有东西在动。

**顺带修掉一个系统性对比度问题**：`--lv-text-3`（次级标签/占位符）原为 `#6b7684`，在纯白上只有 4.6:1——刚好压在 4.5:1 线上，于是**任何有色、混合或玻璃表面都会把它压到线下**：实测命令面板的占位符在玻璃底上是 4.16:1。改为 `#5f6b78`（白底 5.4:1），与 text-2（8.5:1）的层级仍然分明。同时把**承载文字的浮层**（菜单/对话框/面板）的不透明度从 0.86 提到 0.93——面板在给自己承载的文字减对比，加不透明度比把标签调更深更对症（标签在不透明底上本来就够）。

**验证**（两主题，无头 Chrome + 桩服务器）：① **差分透视测试**——同一页面滚动与不滚动的顶栏像素：不透明则完全一致，实测亮色 Δ1.8 / 暗色 Δ4.5，证明模糊确实在拾取下方内容；② 顶栏文字对比 亮 17.85:1 / 暗 15.17:1；③ 命令面板**最差文字行**对比 亮 4.90:1 / 暗 5.05:1（均在 4.5:1 线上）。vitest **115 通过**（新增 14）、`vue-tsc` 干净、`vite build`、`internal/web/dist` 已同步、Go 侧 64 包全绿。

### 修复（本批把 CHANGELOG 结构改坏；release-gate 的 unittest 失败一直被管道吞掉）

- **现象**：`12c5b25` 登记"玻璃只上 chrome"那一节时，落笔锚选在了一条编号条目的**句子中间**（`## [v1.20.0] - 2026-10-07` 这串字），于是上一条"成因"被劈成上下两半、玻璃一节整节没有标题，顶上还多出一行形如 `## [v1.20.0] - 2026-10-07` 上、…` 的**假标题**——文件里出现两个 `v1.20.0`。**与 #94 同一个坑，第二次**：都是锚点选择本身造成的结构损坏，而不是内容写错。
- **这个损坏本机是红的、CI 是绿的**：`python scripts/test_changelog_structure.py` 由 `765d2c9` 补的那条守卫抓住（`AssertionError: Lists differ: [] != ['v1.20.0 at lines 159 and 217']`），而 release-gate 在 PR #101 上是绿的。根因不在守卫，在那个步骤的 shell 语义：`python -m unittest discover … | tee /tmp/py.log` 之后跟一条"点名单 grep"，而 runner 的默认 shell 是 `bash -e`（**没有** pipefail）⇒ **管道的退出码是 `tee` 的**。实测（2026-10-09）：`bash -e -c 'python -m unittest … | tee'` 在 `FAILED (failures=1)` 之下 **STEP EXIT=0**，`PIPESTATUS` 为 `unittest=1 tee=0`。点名单堵的是"discovery 空收集"，**堵不住"断言失败"**——正是本仓反复登记的那一族：能静默通过的检查，不等于在检查什么。
- **修法**：① 结构复原——"成因"那条的尾半句回到它自己那行、玻璃一节恢复 `###` 标题、删掉假标题；② 顺带把两节按**日期序**摆回位置（`dadcc68` 控制台重做 10-09 02:33、`12c5b25` 玻璃 10-09 07:01，都晚于 `856ca97` 审计封链登记那一节 10-08 21:48，却排在它上面）；③ `ci.yml` 该步骤加 `set -o pipefail`，并把两个判据**各自记录、一起判死**；④ 新增 `scripts/test_release_gate_step.py` 钉住"这个步骤不得再把失败吞掉"，并登记进点名单。
- **守卫不是复述**：新套件对**合成的坏样本**（同形状但不带 pipefail、以及带 `|| true` 的）必须报出问题，对好样本必须干净——只会给当前恰好干净的仓库打勾的检查，证明不了它看得见损坏（同 `test_changelog_structure.py` 的 `DetectorIsNotVacuousTest`）。检测器读文件时先归一 CRLF：`765d2c9` 之后的守卫踩过这个坑（CRLF 工作副本下 `^…$` 静默解析成空集），本套件不重蹈。
- **验证**：结构复原后 `python -m unittest discover -s scripts -p 'test_*.py'` 由 `FAILED (failures=1)` 变为 **全绿**（42 → 48 例，新增 6 例）；同一命令在 `bash -e` 下复现旧行为 exit 0、加 `set -o pipefail` 后 exit 1——用**把损坏原样放回去**做的变异：旧步骤 exit 0 且日志里就写着 `FAILED (failures=1)`，修复后的步骤 exit 1 并打出 `::error::release-gate self-tests failed: unittest`；`git diff --numstat CHANGELOG.md` 逐行复核内容零丢失（只重排 + 改那两行 + 本批新增小节），复原后 `md5sum` 与备份逐字节相同。
- **如实登记**：这道洞不是本批带进来的——`tee` 管道与点名单由 `14c4a7a`（10-06 01:45）引入，`765d2c9` 只是往点名单里加了一条（新套件被收集到，但断言失败仍被吞）。也就是说它开着的时间以天计：期间任何"断言红、但套件名仍在日志里"的脚本失败都不会让 release-gate 变红——历史 run 无法回查哪些红被吞过，只能确认本批这一次确实被吞（修复前的步骤在本机实测复现的正是它）。

### 门禁（govulncheck 再被 `go` 指令挡住：`go 1.26.6` → `go 1.26.9`；x/net v0.58.0 → v0.60.0）

- **现象**：PR #101 的 `govulncheck` 在 master 上一次绿 run 之后变红。新公告 `GO-2026-6608`～`GO-2026-6617` 落在 1.26.6 的标准库（`net/http`、`net/textproto`）与 `golang.org/x/net v0.58.0` 上，后者经 `grpc.Server` → `http2.Framer` **可达**。与本次前端改动无关，是"公告先于补丁工具链进仓"的时序——与 v1.14.0 那次同一形状。
- **必须动 `go` 指令而不是只升依赖**：`govulncheck` 作业刻意用 `go-version-file: go.mod` 扫描（其余作业全部浮动 `'1.26'`，setup-go 自动取最新补丁 ⇒ 早已跑在 1.26.9 上），所以扫描用的工具链**就是** go.mod 声明的那一个；不升指令，标准库那半永远停在 1.26.6 上。修复版 1.26.9 已发布，指令随之上移。这正是 `1.26.0 → 1.26.6` 那次留下的教训原样复现。
- **依赖侧**：`golang.org/x/net` v0.58.0 → **v0.60.0**（indirect，经 grpc 引入），收口经 http2 可达的那几条。
- **本机验证**（工具链 1.26.9）：`go build ./...`、`go test ./...` **EXIT=0（65 包全绿）**、`govulncheck ./...` = **No vulnerabilities found**；`-show verbose` 下模块级提示仍是既有的 `GO-2026-5932`（`golang.org/x/crypto@v0.57.0`，`Fixed in: N/A`，v1.14.0 已登记、代码不调用相关符号、不触发退出码 3）——**未新增模块级条目**，不重复登记。
- **升指令当场撞上第二层：`'1.26'` 从来没在按"最新补丁"浮动**。推送后除 `govulncheck` 外**所有** Go 作业都在**第一条 Go 命令**上红：CI 里 10 个（vet、三条 test 腿、lint、gosec、integration、docs、smoke、proto build sanity）+ CodeQL 的 analyze，`build` 因 `needs: [vet, lint, test]` 被跳过。CI 日志原文：`go: go.mod requires go >= 1.26.9 (running go 1.26.8; GOTOOLCHAIN=local)`。两层原因都在 `actions/setup-go` 一侧：① 它**自己写 `GOTOOLCHAIN=local`**（禁止任何工具链切换）；② `go-version: '1.26'` 解析到的是 **1.26.8**。第一次修法（11 处加 `check-latest: true`）**被下一次 run 直接证伪**：日志里输入回显是 `check-latest: true`，紧跟着仍是 `Setup go version spec 1.26` / `Resolved as '1.26.8'`——别名路径的解析源本就是滞后的，"浮动"只在缓存里恰好没有更新版本时才成立。第二次修法（本批最终形态）：11 处全部改 `go-version-file: go.mod`，与 `govulncheck` 作业同源——它的日志给出反证链的最后一环：`Setup go version spec 1.26.9`，扫描全绿。**代价与收益如实记账**：v1.14.0 那条"其余作业浮动 `'1.26'`，自动取最新补丁"的设计由此**退役**（它当时是对的，因为最新补丁恰好等于缓存里那个），换来的是"CI 用的编译器 = 模块声明的最低工具链"这条由机器保证的不变量——工具链前进改由 `go.mod` 一笔驱动，而逼它前进的正是 `govulncheck` 这道门（本轮就是它先红的）。gosec 那段"工具链随 `go-version` 浮动"的注释同步改写，不留与代码矛盾的说明。
- **附带影响（给在无校验库环境里构建的人）**：`go 1.26.9` 是**最低**工具链要求，本机若 `GOSUMDB=off`，go 会拒绝验证要下载的工具链并硬失败（`verifying module: checksum database disabled by GOSUMDB=off`）；把 `GOSUMDB` 交回 `sum.golang.org` 即可（`goproxy.cn` 已代理校验库，本批实测工具链经代理 + sumdb 校验后正常下载并运行）。Docker 侧不受影响：`Dockerfile` 的 `GO_VERSION=1.26` 是滚动 tag。

### 修复（SA-007 收口：服务层的权限拒绝落审计链；SA-011 调用点收口；看板不再把工作流源当变更名）

- **SA-007 的判据从"机制存在"改成"生产接线存在"**。`internal/permission.PermissionChecker` 带的那套可选记录器**全仓零生产调用点**（`grep NewPermissionChecker|WithRecorder` 非测试命中为 0），真正判权限的是 `internal/authz`（矩阵 + 角色树 + 条件策略），而它拒绝时**不记任何审计**——于是"我们说过不"这件事只活在调用方收到的那个 gRPC 错误里，调用方丢掉就什么都不剩。现在服务层两个漏斗（`internal/grpc/resource_authz.go` 的 `authorizeResource` / `authorizeResourceRead`）在**返回拒绝之前**调用 `Authorizer.RecordDenial`，落进**审计链**而不是 trace 表：trace.run_id 有 `REFERENCES runs(id)` 外键，无 run 的拒绝写不进去；audit.run_id 无外键、`NOT NULL DEFAULT ''`，`state.Audit` 的注释本来就写着"无 run 的行恰是安全相关的那类"。行形状 `Action=permission.denied`、`Result=denied`、`Actor=<被拒主体>`、`Target=<action>@<env>`、`RunID=''`，词表与行构造单点放在 `internal/audit/denial.go`，`internal/pause` 既有的记录器改为委托它（两条路径不可能写出两种行）。
- **逐行过滤刻意不记**：`resourceVisibility` 的谓词按行调用，回答的是"这个人能看这个环境吗"而不是"这个请求被拒了"——在那里记账会让一页 200 行的列表写出 200 条拒绝。边界写在函数注释里，并有测试钉住（3 次谓词调用不增行）。
- **接线由结构守卫钉住**：`cmd/levee/serve_authz_recorder_test.go` 要求 `cmd_serve.go` 的 `authz.Load(` 与 `WithAuthorizer(` 之间必须装着 recorder。这一行是**只能靠结构断言守住**的那种：摘掉它编译照样过、所有行为测试照样绿（拒绝本身一模一样），只有守卫会红。变异实测：把 recorder 换成空实现，守卫按预期文案判红。
- **SA-011 收口的是"义务仍写在注释里"那半条**。`RetrieveInto` 早就存在（正常返回 / 错误返回 / panic 三条路径都有测试），本次把生产调用点里最后一处 `Retrieve` + 手写 `defer` 改成它，并新增调用点守卫 `internal/credential/retrieve_callers_guard_test.go`：全仓每处 `.Retrieve(` 必须是 `RetrieveInto` 自己体内、或前 5 行内带 `NOTE(SA-011)` 的所有权转移说明（现存两处：`provider.Resolve` 与 KMS 本地回退，都把明文交给文档上拥有它的上层），否则点名 `文件:行` 判红；扫到的调用点数必须 > 0——**锚点失效要响，不许空集通过**。变异实测：摘掉 `provider.go` 的标记，守卫点名 `provider.go:166`。**边界如实保留**：`channel.CredentialRef.Password` 是 `string`，Go 字符串不可清零，要抹掉得把 `CredentialRef` 跨 ssh/winrm/grpc 改成 `[]byte`（并动凭据 blob 的 JSON 形态），仍按设计搁置。
- **看板不再把工作流源当变更名**：`runToPB` 原样把 `r.WorkflowName` 写进 `pb.Change.Label`，而该字段的契约是**工作流源**（模板实例化的 run 里那是渲染后的整份 YAML），控制台 `ChangesView.vue` 又直接渲染 `{{ row.label }}` ⇒ 那一列显示一段 YAML（roadmap 里 2026-10-05 登记为"gRPC 侧改它属于界面行为变更，不在本批修"）。现在两边共用 `dsl.DisplayName`（CLI 的 `workflowDisplay` 此前是它的唯一实现，改为委托同一函数）：单行输入原样返回（路径/名字），可解析文档取声明的 `name:`，**不可解析则原样返回**——运维要认出那份坏文档，不许猜；`WorkflowFile` 仍逐字携带源。证据：`internal/dsl/display_test.go` 两条方向相反的断言（取到名字时不得带换行；回退时保留换行）、`internal/grpc/change_label_display_test.go`、CLI 既有测试原样通过。
- **台账校正与登记**：`docs/product-roadmap.md` 两处被实测证伪——human 门禁的 quorum/`exclude_initiator` **已在 v1.20.0 落地**（`internal/verify/human_gate.go:75-79`，CHANGELOG 有专节），`Window.MaxConcurrency` **已不存在**（`internal/` 全仓只剩 `plan.Batch` 上的同名并发旋钮）；`docs/security-audit.md` 的 SA-007 改判 `[已修复 v1.21.0]`、SA-011 补追记划清收口范围。**顺手查出、未修、登记**：`audit` 表 `action`/`result` 两列的 schema 注释与实际写入值早已不符（注释写 `success|failure|denied|error`，实际还写 `draft`/`rolled_back`/`archived`/`recorded; quorum pending` 等，`rest_gate` 甚至把截断消息拼进 `result`）——那是独立的一批审计词表收口（单一 owner + 列注释守卫），本批不碰。
- 验证：`go test ./...` **65 包全绿（EXIT=0）**；新增 7 个测试文件共 14 例（audit 3、authz 4、dsl 2、credential 守卫 1、grpc 3、cmd 守卫 1）；两处变异各自按预期红、复原后绿；`scripts/` 的 Python 套件与 CHANGELOG 结构守卫保持绿。

## [v1.20.0] - 2026-10-07

> **发布状态：已切版。** 附注 tag `v1.20.0`（tag 对象 `fe4b162`，剥壳指向 `738c477e`）于 2026-10-07 推送，`release.yml` 据此发布：GitHub Release `draft=false`、`publishedAt=2026-10-07T14:35:31Z`、资产为 6 个平台包 + `checksums.txt`；镜像 `ghcr.io/levango7/levee:v1.20.0` 按外部事实核过——`manifests/v1.20.0` 返回 200，`tags/list` = `[v1.18.0, latest, v1.19.0, v1.20.0]`。本小节共 **15** 个小节。chart 的 `appVersion` 与 `values.image.tag` 随这一笔升到 `1.20.0` / 逐字 `v1.20.0`——顺序不能反：`scripts/check_release_versions.py` 规则②要求 `image.tag` **逐字**出现在 tag 集合里，所以在 tag 存在之前它们必须继续指向上一个真实发布（此前指 `1.19.0` / `v1.19.0`）。
>
> **为什么这一版叫 v1.20.0 而不是 v1.19.1**：本小节最初以 `## [v1.19.1] - 未切版` 登记（#77），当时 master 上只有那五批修复与门禁工作。此后 master 又落了 8 批新增能力——human 门禁的 quorum（#84）、human 门禁的审批传输（#83）、审批决定携带可验签审批人（#82）、条件式 ABAC 接入服务层（#81）、清单/模板/审计/系统面的权限矩阵接入（#51）、master 端 Agent 注册表 RPC（#85）、规范文本纠正（#79）与 CI 聚合门禁的 UNVERIFIED/FAILED 分立（#78）——按 semver 属于 minor 而非 patch。`v1.19.1` 从未切版，registry 里也没有那个制品，所以这次改名不收回任何对外承诺。折叠之后、切版之前又并入一批：**#88**（批次状态两套词表 ⇒ `done_batches` 恒为 0；`batch-status` 线上传输是 PascalCase 而视图读 snake_case ⇒ `/cluster` 批次面板从落地起就是死的；`/monitor` 三视图的第一步）。

### 新增（验证门禁按目标执行：cmd 门禁终于能跑它声明的那条检查）

- **`GateInput.Channel` 在 run 路径无人供给，命令门禁一律失败关闭**：引擎构造 `GateInput` 时只填 RunID / BatchID / TargetIDs，全仓唯一的非测试 `Channel:` 赋值在 `internal/wiring/exec.go`，而那是给步骤执行用的。后果不是"少个功能"，而是**任何声明了 cmd 门禁的工作流永远过不去自己这一关**——每次都走 "missing channel" 分支：安全地失败，但从未执行过被声明的那条检查。
- **接缝只开一处**：新增 `verify.GateChannelProvider` 作为 `GateInput` 的可选字段，`engine.GateRuntime` 增加 `Channels` 与 `WithChannels`（返回副本）；wiring 在每次 run 用 `runExec.gateChannelProvider()` 供通道——**复用步骤已有的那条缓存会话**，所以门禁与它守护的执行共用同一份租约与凭据，不可能出现"检查走 A 身份、执行走 B 身份"。plan 期拒绝与 phase 期注册仍读同一个 `gateRuntime()`，两处判定不会分叉。
- **判定规则（全部有测试）**：按 `TargetIDs` 逐台执行，**每台都要过**——web-1 过了不等于这批过了；连不上的目标算**失败**而非跳过（没被证明健康的机器不能算通过）；失败结论列出全部 offender 与 `failed_targets`；空目标集失败关闭（`no_targets`），绝不因为"没有东西要查"就判通过。SLO / human 门禁不消费 provider，保持 run 级一次，不会随目标数被跑 N 次。
- **行为变更**：升级后声明了 cmd 门禁的工作流从"必然回滚"变成"按检查真结果放行或阻断"——这是门禁第一次真正生效。若某条命令在其目标上本就通不过，行为不变（仍回滚），只是原因从 `missing channel` 换成真实退出码。
- **遗留（同日登记，不含在本批）**：`probe` 的 `mode: remote` 仍只认单一 `GateInput.Channel`、未消费 provider，在执行路径下仍以 missing channel 失败关闭；`direct` 模式不受影响。
### 修复（手动回滚的结局不再被当成服务端故障）

- **`RollbackChange` 把"跑了但没补全"当成 `codes.Internal` 抛出，并且这条路径根本不推进 run 状态**：`internal/grpc/change_service.go` 对 `engine.Rollback` 的任何 error 一律 `codes.Internal`，于是补偿已经派发到目标机之后，记录仍停在尝试之前的 `rolled_back_partial` —— 操作者既看不出"我试过且没成功"，也无法据此决定重试还是收工。实测两条路径都中招：从 `rolled_back_partial` 再手动回滚、以及回滚一个步骤没有补偿声明的 `completed` 变更。
- **修法**：新增领域错误类 `internal/rollback.ErrIncomplete`（wiring 在 `!res.Success` 时以 `%w: %w` 双包裹，既保留"哪条 undo 失败"的原文又让 `errors.Is` 可用）；gRPC 侧据此分流——领域结局走 `Success=false` + `rollback_incomplete` + 同状态的审计行与事件，真故障仍是 `codes.Internal` 且**不改写历史**。`rollback_incomplete` 本就在 `runstatus.RollbackAdmitted` 内，因此未补全的回滚仍可再来一次，也不会被误封成终态。
- **验证**：`internal/grpc/change_service_rollback_outcome_test.go` 两条新用例（结局用例自建 `EngineAdapter`，因为共享 fake 在返回 error 时会丢掉 rollback id 与 hosts——那不是生产形状）；`tests/integration/gate_block_rollback_e2e_test.go` 把原先"容忍 Internal"的断言翻成新契约，并如实记录第二次尝试未派发任何新补偿（D-2 台账判定已补过的不重复补）。变异四条各自变红：wiring 不再分类 / grpc 一律 Internal / 状态不推进 / 一律当结局处理（吞掉真故障）。

### 门禁（CI 冒烟脚本：先报端口归属，再谈健康检查）

- **`scripts/smoke_serve.sh` 会在别人的端口上冒充"本仓服务起不来"**：本机 127.0.0.1:9091/9092 被容器栈占用时，它报 `::error::smoke failed: /healthz never became reachable on :9092`，把读者送去查一个并不存在的 bug。根因按实测定位——`--http-addr ":9092"` 绑的是**通配地址**，与已存在的 **127.0.0.1** 专用监听并不冲突，于是进程活着、日志照常打 `REST gateway listening addr=:9092`，而 curl 打 127.0.0.1 时被更具体的那个 socket 接走（实测 prometheus 对 /healthz 回 404、对 / 回 302）。现在在启动任何东西之前逐端口探测并点名归属：`something already listens on :9092 (HTTP, it answered HTTP 200) — free it or set SMOKE_HTTP_PORT`；非 HTTP 的占用（裸 gRPC 监听）只报端口与标签，**不编造 HTTP 状态码**。
- **踩到的 shell 坑写进注释**：探测必须在子 shell 里做。若在父 shell 用 `exec 3<&-` 关闭一个它从未打开的 fd，那是**失败的重定向**，而非交互式 shell 中失败的重定向会**静默终止脚本**——实测退出码 0、停在函数体中途、既不报错也不跑后续用例。改动前那版就是这样"通过"的。
- **每次成功运行都泄漏一个临时目录**：trap 原本 `kill` 完立刻 `rm -rf`，Windows 上"打开即锁定"，levee 还没退出就删 ⇒ stderr 出现 `rm: cannot remove .../levee.db: Device or resource busy`，而脚本自称成功。改为 kill → wait → rm，删不掉就点名路径。
- **验证**（受控监听器，不靠环境巧合）：9092 放一个 HTTP 应答、9091 放一个非 HTTP 裸监听 ⇒ A 默认端口报出 `(HTTP, it answered HTTP 200)`；B 只空出 HTTP 时报 `:9091 (GRPC)` 且无状态码；C 两端口皆空 ⇒ exit 0、**stderr 0 字节**、临时目录计数不增。反向对照：同一二进制跑改动前的脚本，报回的仍是 `healthz never became reachable`——这条预检确实承重。CI 侧无需改动（`smoke` job 跑在 ubuntu-latest，端口本就空着；这道检查在 CI 上是 no-op，只在开发机上把误报变成可读的错）。

### 变更（v1.19.0 切版后的口径对齐）

- **撤掉"未切版"标注并把 Helm 口径升到 1.19.0**：tag `v1.19.0`（附注 tag，指向 `ddad079`，切版时间 2026-10-06 00:19 +0800）已推送，Release workflow 已据此发布镜像与 GitHub Release（实测：`draft=false`、`publishedAt=2026-10-05T16:43:33Z`、assets 为 5 个平台包 + `checksums.txt`）。`Chart.yaml` 的 appVersion 升到 **1.19.0**（此前对齐到已发布的 1.18.0），CHANGELOG 的 v1.19.0 小节移除"未切版"说明并把日期改为真实切版日 2026-10-06。
  > 本条目最初写作"release.yml 正在据此构建 `ghcr.io/levango7/levee:1.19.0`"——那个 tag 拼写是错的，见下一条；措辞按实测收回。
- **交付缺陷（本批修掉；tag `v1.19.0` 快照即带此问题，拼写错误自 v1.18.0 起存在）：chart 默认值渲染出一个从未构建过的镜像 tag。** 在 tag 本身上量（`git archive v1.19.0 | tar -x` 后 `helm template`）：渲染出 `ghcr.io/levango7/levee:1.18.0`——那时 `values.image.tag`/appVersion 对齐到"最后一个已发布的 tag"，而 v1.19.0 的切版发生在其后。本批上一笔把它升到 `1.19.0`，**拼写仍然不存在**。于是两重错同时成立：chart 对外自称的版本与本批二进制不是同一个；那个拼写在 registry 里根本没有——`release.yml` 推的镜像 tag 是 github.ref_name，即 git tag 名本身，带 v。异源证据：① `helm template` 真实解析器（不是 grep）；② registry 对 `manifests/1.18.0`、`manifests/1.19.0` 均返回 **404**，对 `manifests/v1.19.0` 返回 **200**，`tags/list` 只有 [v1.18.0, latest, v1.19.0]。⇒ 按 chart 默认值执行 `helm install` 的客户拿不到镜像。**tag 不可变，v1.19.0 快照维持原样**：`docs/release-notes/v1.19.0.md` 记下该已知问题与绕过方式（`--set image.tag=v1.19.0`），修好的默认值随下一个版本切出。本批把 `image.tag` 改为逐字的 `v1.19.0`。
- **门禁为什么放过了它，以及补上的规则**：`check_release_versions.py` 规则②原先把引用版本**合成** `v<version>` 后去查 git tag，于是 `1.19.0` 与 `v1.19.0` 都能通过——它验的是"git tag 存在"，不是"渲染出的镜像引用存在"。现拆成两条：规则②要求 `values.image.tag` **逐字**出现在 tag 集合里（"镜像以 git tag 名发布"这一事实写进脚注释），appVersion 另按 `v` 前缀可有可无地校验；规则①改按版本号比较（两处拼写有意不同，各自有约定）。变异实测三条：改回 `1.19.0` → 规则②以新文案失败；改成 `v1.18.0` 与 appVersion 分叉 → 规则①失败；恢复 `v1.19.0` → `RELEASE VERSION CHECK PASSED`（tag 集合取自 origin，10 个，`newest_tag=v1.19.0`）。规则③（比最新 tag 更晚的 CHANGELOG 小节须标注）同时满足。
- **给门禁本身补上自测**：`check_release_versions.py` 此前没有测试，规则被重构掉不会有任何东西发现。新增 `scripts/test_check_release_versions.py`（10 例：一致通过、v 前缀差异不算失败、规则①②③各自的失败形态、空 tag 集合必须拒绝、历史无 tag 小节只点名、sidecar 的 `postgres.image.tag` 不被误读）；其中 `test_image_tag_without_v_prefix_fails` 就是本条缺陷的回归用例。反向验过：把规则②改回"合成 `v` 前缀"的旧行为，该用例立刻变红（`AssertionError: 0 != 1`）。`release-gate` job 的 python 步骤改为 `unittest discover -s scripts`，并逐套件点名两个套件是否被收集——空收集会以 `Ran 0 tests / OK` 静默通过。实测本地 21 例全绿。

### 新增（remote 探针与 script 门禁按目标逐台执行，#74 剩下的那一半）

- **通道提供者接到了 `probe` 上**：#74 给 `GateInput` 开了 `ChannelFor` 接缝并把 cmd 门禁接了上去，`probe` 的 `mode: remote` 与 `kind: script` 当时仍只认单一 `Channel`，在执行路径下照旧以 "missing channel" 失败关闭——也就是说"声明了远程探针的变更永远过不了自己那一关"这一半缺陷还在。现在 `verify/probe_gate.go` 的 `checkAcrossTargets` 消费同一个 provider，判定与 cmd 门禁刻意一致：**每台都要过**（web-1 通不等于这批通）、**拨不上算失败**而不是跳过（没连上的机器没被证明可达）、**空目标失败关闭**、结论列出 `failed_targets` 与逐台证据；`direct` 形态**不**接 provider，因为它从控制面测量，接上只会白白拨 N 次号。
- **`script` 一类以前没有归属约束**：脚本探针现在也是逐台上传+执行，任何一台退出码不符就整体判失败并点名。
- **结构守卫补上（这是当初真正断掉的那一层）**：新增 `internal/engine/gate_input_channel_provider_test.go`，要求引擎每一处构造 `verify.GateInput` 都填 `ChannelFor`，并且**不许**直接填单通道 `Channel`（单通道形态等于把"随便挑一台来证明全部"写进结构）。扫描命中数低于 4 处即判红——"标记失效导致 0 命中"不能伪装成通过。
- **文档如实写明新的不对称**：逐台收窄后，remote **tcp** 的 `{target}` / `port_from_target` 天然按本机地址判定，而 remote **http** 的 `url` 不做 `{target}` 展开（direct 模式才会）；`docs/gates.md` 写了这一差异，`docs/product-roadmap.md` 把"要不要让 remote http 也展开"留作界面一致性决策。
- **验证**：`internal/verify/probe_gate_targets_test.go` 10 条用例（全过、一台失败挡整批并点名、拨不上算失败、空目标失败关闭、tcp 每台只见自己的地址、script 逐台、direct 绝不调用 provider、旧的单通道形态行为不变、无 provider 仍失败关闭、取消后不再声明下一台的证据）；`-race` 下 verify 与 engine 包全绿。变异 6 条全部被对应用例抓红（含把"拨不上"改成"跳过"、去掉逐台收窄、让 direct 也扇出、去掉循环内的取消检查），结构守卫另做 3 条变异（抽掉一处 `ChannelFor`、加一处 `Channel:`、把标记改成不存在的串）均判红且点名位置。

### 修复（发布版本门禁的规则③——本批折叠 CHANGELOG 时当场暴露）

- **`未切版` 标注原先是"正文里出现即可"**：规则③用 `PENDING_MARKER in body` 判断一个小节有没有声明"还没切版"。把 v1.19.0 那一节的留痕（"撤掉**未切版**标注并把 Helm 口径升到 1.19.0"）折进新版本小节之后，正文里天然就有这三个字，于是**任何未标注的新小节都能借别人的话通过**——本次折叠 v1.19.1 时实测到：删掉真正的声明行、只留无关散文，门禁仍然 `PASSED`。
- **改法**：只认**行首的声明行**，两种形式（`> 未切版`、`> **发布状态：未切版。**`），并要求标记后面紧跟分隔符（`未切版本` 这种更长词不算）。报错文案给出可照抄的写法。**回归用例**：`test_prose_mention_does_not_count_as_pending_declaration`、`test_marker_inside_a_larger_word_does_not_count`（先跑旧实现会全绿，是真正的盲区）；另在真实文件上做过一次反向验：删掉声明行、保留"撤掉未切版标注"那句 → `FAILED` 且点名缺失声明。
- 台账 `docs/mvp-tasks.md` 的 D-04 行随 #76 更新为"cmd 与 remote/script 探针都按目标逐台执行"，三段留痕保留（2026-08-15"已交付"→ 2026-10-05"部分可达"→ 2026-10-06 现口径），并列出两条未闭合项。



### 修复（批次状态两套词表：`done_batches` 对任何真实 run 恒为 0）

- **缺陷形态**：写批次的那一端（`internal/wiring/persist.go`）落的是 `completed` / `failed`，回滚路径落 `rolled_back`；而判"已彻底完成"的 `state.batchDoneStates` 里只有 `BatchStateDone = "done"`——**这个值全仓没有任何非测试写入点**（实测：`grep` 排除测试后，state 包外的 `BatchState*` 引用数为 0）。后果落在操作者看得见的两个数字上：`BatchSummary.DoneBatches` 对一趟全部成功的 run 恒为 0，`CurrentBatchNo` 又停在第 1 批（"第 1 批还等着续跑"）。
- **为什么整套测试都没发现**：`internal/state` 的用例自己按 `BatchStateDone` 种数据，读的就是它写的那个词；`internal/wiring` 的用例根本不读 `BatchSummary`。两边各自都绿——这是"两套词表各自成立"的形态，不是缺覆盖率。同一条裂缝也映在 UI 上：`/monitor` 先按 `success` 映射（没人写），我修的时候又按 `done` 映射（也没人写），只有按写入端真实值 `completed`/`rolled_back` 才对。
- **改法**：为写入端真正使用的值补常量 `BatchStateCompleted` / `BatchStateRolledBack` 并让 persist.go 改用它（读写共用一组常量，字面量不再各走各的）；`batchDoneStates` 加入 `completed`（保留 `done` 兼容既有行）。`failed` / `rolled_back` / `interrupted` 仍**排除**在外，与该集合原注释的语义一致——那些是"要重跑/已回滚"，不是"前向完成"。
- **前端**：`web/src/utils/batch.ts` 按写入端词表着色（`completed`/遗留 `done` → 绿；`failed` → 红；`rolled_back`/`interrupted` → 警示；未知值一律中性，永不绿），逻辑从 SFC 抽出以在无组件测试依赖（本仓 `@vue/test-utils` 未装）的前提下可测。
- **顺带查出的第二层（同一条裂缝的线上传输面）**：这条路由 `writeJSON(w, summary)` 直接序列化 Go 结构体，而 `state.BatchSummary` / `BatchProgress` **没有 json tag**，于是线上字段名是 PascalCase（`DoneBatches`），而两个视图与 `BatchSummaryDTO` 读的是 snake_case（`done_batches`）。没有任何东西报错——`/cluster` 的批次面板因此一直是死的，`/monitor` 接上去也会一样。修法是把 tag 补上让线上传输与已声明的 DTO 对齐（消费面实测只有这两个视图 + 该路由，CLI 与文档都不读它）。**这条形状此前无测试**：新增 `internal/grpc/rest_batch_status_contract_test.go` 钉住四个顶层键与批次行的五个键，并显式禁止 PascalCase 回流；变异实测：摘掉 `done_batches` 的 tag ⇒ 同一用例带着线上原文判红（`PascalCase key "DoneBatches" leaked back into the payload`）。
- **验证**：新增 `internal/state/batch_status_vocabulary_test.go`（按写入端字面量种数据 + 把常量↔字符串、done 集合成员关系钉成断言）与 `internal/wiring/batch_summary_seam_test.go`（真实走 persist 再读 BatchSummary 的跨缝用例）。**撤掉修复**（done 集合退回只认 `done`）⇒ 跨缝用例两条具名断言同时判红，复原后 state+wiring 全绿。前端 `vitest` 10 例通过、`vue-tsc + vite build` 通过、`internal/web/dist` 与构建产物逐文件一致。**真实渲染已验**：用改动后的二进制起真 serve（`--insecure`）+ 真 SQLite + 真 `levee web`，直接向 `batches` 表写入引擎真正使用的 `completed` 状态，浏览器读到 `批次进度 | 共 2 批 · 已完成 2 批 | 批次 #1 completed 2 台 · 成功 2 · 失败 0 …`，门禁卡显示的是那句诚实说明而非假 `pending`，控制台 0 错误；同一路由的 HTTP 响应在补 tag 前是 `"DoneBatches":2`、补后是 `"done_batches":2`。

### 门禁（CI 聚合步骤把"没有判定结果"和"失败"分开说）
- **一条红并不等于一次回归**：2026-10-06 实测，runner 池饥饿时 `docs`/`gosec`/`build (ubuntu-arm64)`/`check (all jobs passed)` 四个 job **各自在排队 901 秒后被杀**，`steps=0`——一次都没开始跑。聚合步骤原先对所有非 `success` 都写 `required job 'x' did not pass`，读起来像"这次改动没通过检查"，而真相是"这次改动从未被检查过"。这两种情况的下一步动作完全相反（前者查代码，后者重跑），却被同一句话混在一起。
- **改法**：聚合按结论分桶——`failure` 仍是 `FAILED`；`cancelled`/`skipped`/`neutral`/`timed_out`/`stale` 归为 `UNVERIFIED, not failed`，摘要行点名是哪几个 job 没有判定结果，并写出"先重跑，别当回归读"。**两种都仍然 exit 1**：没有证据不许变绿，这条不许松。
- **给聚合步骤本身补上 CI 内的自测**：新增 `scripts/test_ci_aggregate.py`（5 例：全绿、真失败、排队超时、`neutral`/`stale` 归桶、聚合清单与 `needs()` 漂移）。它不另抄一段逻辑，而是**从 `.github/workflows/ci.yml` 原样抽出那 35 行 bash**，只替换两处环境差异（Actions 的 `${{ }}` 插值 → 夹具；`jq` 那条管道 → 同形状 echo），所以测的就是入库文本本身；改坏那段脚本会让这些用例变红。
- `release-gate` job 的逐套件点名清单加上 `test_ci_aggregate`（重命名或删除该套件会直接判红）；`.gitignore` 加 `.agg_case_*.sh`——测试的中途打断不该在 `git status` 里留脏文件。
- 实测：`python -m unittest discover -s scripts` 29 例 OK；把套件文件改名后守卫判 `GUARD RED: test_ci_aggregate not collected`，恢复后转绿；`ci.yml` YAML 解析通过（15 job）。

### 新增（master 端 Agent 注册表 RPC：注册表第一次跨进程可见）
- **注册表原先只是"构造它的那个进程"里的一张 map**：`internal/agent/registry.go` 的 `AgentRegistry` 由 `cmd/levee/cmd_agent_support.go` 提成进程内单例，既没有落盘入口，`proto/` 里也没有任何 agent 服务定义（2026-10-04 实测：`internal/grpc`、`internal/wiring`、`internal/cluster` 三个装配面对它的引用计数均为 0）。后果是 `levee agent list` 跨进程恒空表、`show`/`remove` 恒 not-found——**不是"没有 agent 注册"，而是答案来自错误的进程**。CLI 参考第 20 章当时据此写下边界说明并把示例改成不承诺跨进程可见。
- **交付**：`proto/levee_extra.proto` 新增 `AgentService` 六个 RPC（RegisterAgent / AgentHeartbeat / DeregisterAgent / ListAgents / GetAgent / RemoveAgent）；pb 产物用 CI 钉住的 protoc 27.0 + protoc-gen-go v1.36.12 工具链生成（该 job 以 `git diff --exit-code -- internal/grpc/pb/` 校验产物，本机 protoc 36.2 产出对不上——这正是 roadmap 那条前置条件记录的障碍）。注册表落盘为 `agents` 表：schema **v8** 前向迁移，SQLite 与 PostgreSQL 双引擎同列序，fresh-vs-upgraded 形状与列集分别钉桩。`capabilities` 存 JSON 数组而非分隔符拼接（含逗号的能力名要能原样回来），`last_heartbeat` 可空，用以区分"从未心跳"与"在某个时刻心跳过"。
- **列归属是设计而不是巧合**：`registered_at` 与 `last_heartbeat` 被刻意挡在 `ON CONFLICT DO UPDATE` 的 SET 列表之外——re-register 不许让 agent 变年轻，也不许覆盖心跳已经写下的活跃度；心跳只写活跃度列，`address`/`capabilities`/`max_concurrent` 归 RegisterAgent 所有。**一列两个写主**正是"谁赢了"这类竞态的来源；改了自身并发的 agent 重新注册（那调用幂等）。
- **状态词表被校验而非透传**：注册表只能产出 `registered | idle | busy | offline`（`agent.AgentStatusValues`）。心跳的状态缺省由负载**派生**，复用 `agent.DeriveStatus`——避免出现第二份手写判据，让心跳与调度器对"busy"各有其解；显式状态仍过同一词表。收下任意字符串等于允许一个拼写错误把 agent 永久停在"看起来健康"。
- **`Unimplemented` 与"空注册表"必须可区分**：store 供不出 `state.AgentStore` 时，serve 打 WARN 并交给生成的 stub（`cmd/levee/cmd_serve_agent.go`）；服务自身的 nil-store 分支同样逐方法回 `codes.Unimplemented`。拿一张没有 store 的服务去回答空表，就是本功能要消灭的那个谎言。
- **`RemoveAgent` 的在途拒绝是结局而非错误**：`removed=false` + 可读 refusal（`... pass force to remove the record anyway`），目标不存在（含重复删除）走同一条可读拒绝；CLI 把它翻成非零退出并打印原文，脚本因此看得见失败。roadmap P2「在途任务守卫」的原话——"`--force` 已从文档删除，因为没有守卫可绕过"——至此闭合，`--force` 重新出现在语法行。
- **权限矩阵同日接入**（本批在 rebase 到 master 时发现 #51 刚落地的姿态并要求跟上）：注册表属 fleet 面，按 `internal/grpc/resource_authz.go` 的既有姿态判定——四个写 RPC 要 `admin`（与 `AddTarget` 同一条），两个读 RPC 要 `view`；agent 记录不声明环境，故与 group / template / config 一样在 `permission.default_env` 判定，矩阵存在但无 default 时**拒绝而非猜**。服务构造并入 `buildServeServices`，因而被 `serve_policy_wiring_test.go` 的"每个受策略服务都拒绝未知主体"守卫覆盖；`levee authz status` 的覆盖面文案同步点名 agent registry。**运维推论（新部署要求）**：装了矩阵的部署里，用共享令牌（无可归因主体）的 agent 会被拒写。
- **`agent start` 仍是进程内**（刻意不做）：proto 没有 master→agent 的任务流 RPC，把生命周期切到 gRPC 只会得到一个"注册成功、然后永远等不到派发"的 agent。本批交付 master 侧契约 + 持久化 + 运维可见性这三件，任务通道另行排期。
- **一处入口回归（本批自查修掉）**：改写 list/show/remove 时把 `cmd.Context()` 引入三个 `run*` 入口，而本包既有测试直接以 **nil 命令**驱动 `run*`（全仓 27 处这种调用）——cobra 的 `(*Command).Context` 解引用接收者，`TestRunAgentShowMissing` 当场 panic（`0xc0000005`）。更值得记下的是这条 panic **终止了整个测试进程**，同包后续用例一次都没跑；只按 `-run` 过滤跑单包的验证看不见它。改法取包内既有姿态并补上外层判断（nil 命令与 nil context 都退回 `context.Background()`）。
- **验证**：`internal/grpc/agent_service_test.go` 20 例（列归属 fake + 固定时钟 + 鉴权豁免反查 + nil-store 分流 + 共享令牌写落 `Unauthenticated` 的策略形态）；`internal/state/agents_test.go` 7 例 + `agents_schema_test.go` 6 例（CRUD、能力编码 9 形、v7→v8 真实升级与形状比对）；`cmd/levee/cmd_agent_remote_test.go` 1 例用与 serve 相同的装配跑通 list / 过滤 / show / NotFound / 在途拒绝 / force 后删除；**跨进程可见性由 `internal/grpc/agent_service_e2e_test.go` 直接证明**——测试二进制以子进程重执行自身、独立打开同一 SQLite 文件读注册表，注册→心跳→daemon 重启→删除四腿逐项断言。变异 11 条全部被具名断言抓红（服务层 4：心跳恒 idle / remove 忽略在途 / nil store 竟答空表 / agent 被加进鉴权豁免；存储层 3：re-register 复位年龄 / 心跳对未知 id 报成功 / upsert 覆盖 last_heartbeat；E2E 1：子进程读错文件；权限侧 3：摘写 guard / 摘读 guard / wiring 传 nil 主体），源码逐条复原后按字节核对一致。

### 功能（human 门禁支持 quorum 与 exclude_initiator）
- human 门禁从"单人同意"扩到工作流可声明的**同意形状**：两个新参数
  - `min_approvers: <int>`（>0，缺省 1）：需要**几个不同的人**同意才放行；
  - `exclude_initiator: <bool>`：**禁止 run 的发起人**投这一票（四眼原则）。
- 落地分层：`verify` 的 `HumanRequest` 现在承载同意形状（`MinApprovers` / `ExcludeInitiator` / `Initiator`），而不是只传 run/gate/reason——**形状是问题的一部分，不是传输的实现细节**：表达不了 quorum 的传输必须拒绝，而不是用一个投票作答。`GateInput` 新增 `Initiator`，`engine` 从 `run.Creator` 取（读不到就是空，见下）。
- **审批层不用改**：`approval.Service.decide` 早已强制 `MinApprovers` 配额与 `ExcludeInitiator`（本仓既有实现）；本次只是把工作流的声明透传进 `CreateRequest`。
- **两个 fail-closed 点**：
  - 声明了 `exclude_initiator` 但 run **没有记录的发起人**时，门禁**直接拒绝**（`unverifiable_independence`），不把"无人可排除"悄悄降级成"谁都能投"——正是该声明要防的事；
  - `min_approvers <= 0` 是**参数错误**（fail-closed 拒绝，不静默当 1）。
- 审批人记录：quorum 下 `HumanDecision.Approver` **列出全部同意者**（逗号连接），不是只报最后一票——`min_approvers > 1` 的语义是"这些人同意"，只报一个是少报。
- 迁移说明：**纯新增**——此前 `min_approvers` / `exclude_initiator` 是**未知参数**（strict 校验会 fail-closed 拒绝），所以没有存量工作流使用它们，行为不变。
- 验证：`verify` 3 条（不可执行的排除拒绝且**根本不问传输**、同意形状抵达传输、非正配额拒绝）+ `approval` 2 条（2-of-N：一票**不**放行、两票放行且列出两人；发起人那票被拒后独立投票放行）。**2 项变异被抓**：quorum 不落进记录 → 2-of-N 被一票放行；去掉不可执行排除的拒绝 → 该用例转红。
### 功能（human 门禁终于有传输：复用审批链，`levee gate approve|reject` 是决定面）
- **此前 human 门禁在 serve 下不可用**：`WithGateApprover` 生产 0 调用点，计划期直接拒绝（`ErrGateNotExecutable`），任何声明 `human` 的 workflow **根本 plan 不出来**。上一批只补了"契约能携带审批人身份"（`HumanDecision`），本批补上传输。
- 传输 = **复用审批链**（`approval.GateApprover`，实现 `verify.HumanApprover`）：门禁打开时按确定性 ID `gate-<run>-<gate>` 建一条审批记录，轮询直到有人决定，把决定与**审批人身份**回给门禁。
  - **与变更审批隔离**（关键）：门禁记录绑一个 run 永远不可能有的 PlanHash（`human-gate:<gate>` 前缀）。变更结算/apply/重规划三处都用 `state.Approval.MatchesPlan(run.PlanHash)` 选行，而它只在 PlanHash **非空且相等**时匹配——所以批准变更不会顺带放行门禁、反之亦然。若忘了这层，两类同意就被混成一个（测试 `TestGateApproverOpensAnIsolatedRecord` 钉住）。
  - **重入安全**：确定性 ID 让重启/续跑的门禁**重挂到同一条记录**，而不是叠第二条（`TestGateApproverReattachesToAnExistingRecord`）。
  - 决定面是新 CLI **`levee gate approve|reject <run-id> <gate-name>`**（审批人＝本地 actor，与 `levee approve` 同一信任边界）。**它刻意不结算 run**——run 已经在跑，门禁只是其中一步；这与 `levee approve` 只动门禁、不动 run 状态的区别写进了命令帮助。
  - 未做的（明确留此）：门禁的 quorum / `min_approvers` 与独立性（`exclude_initiator`）——门禁 MVP 是**单人**（与 `verify.HumanApprover` 同口径），多人同意属后续。
- **迁移说明（行为变更）**：装了 `--engine-enabled` 的 serve 从此**会安装门禁传输**——声明 `human` 的 workflow 由"计划期被拒"变为"可计划、运行到门禁时等待人在环"。等不到人时按门禁自身的 `timeout_seconds`（缺省 1800s）超时失败。需要人工介入的部署请确保有人值守 `levee gate approve`。
- `approval.CreateRequest` 新增可选 `ID`（调用方要等待自己创建的记录，没有自选 ID 就没有可轮询的目标）。
- 验证：`internal/approval` 5 条传输测试（批准/驳回各带身份、隔离标记、ctx 取消、重入）+ `cmd/levee` 1 条**走真实 serve 选项构造器与真实 plan 漏斗**的接线测试（带控制组：无审批服务时同一 workflow 仍被 `ErrGateNotExecutable` 拒——证明"能计划"来自接线而非 workflow 本身）。2 项变异被抓（摘掉接线选项 → 接线测试红；去掉隔离 PlanHash → 隔离测试红）。
### 变更（human 门禁的审批决定现在携带可验签审批人身份）
- **此前 `verify.HumanApprover` 连"谁批的"都表达不了**：`RequestAndWait` 只返回一个 `bool`。于是即使将来装了传输，门禁结果里也不会有人名——审计只能记"approved"，记不了"alice 批的"。
- `RequestAndWait` 现在返回 `HumanDecision{Approved bool, Approver string}`；`HumanGate.Check` 在传输能认证主体时把 `Approver` 记进结果的 `details["approver"]`，**认证不了就留空**（不伪造、不渲染空名字），未认证的批准因此在轨迹里可见而非被匿名归因。
- 语义边界：门禁本身不做鉴权判定（那是传输层的边界），它只忠实记录传输给出的身份——与 `#50` 的「归属保真」同一口径：回显即承诺、身份必须可跨进程解析。
- **仍未做（明确记此，不留给沉默）**：生产端**没有**安装任何传输，`WithGateApprover`（`wiring.go:212`）调用点数仍为 0，故 human 门禁在 serve 下仍 **fail-closed**（计划期拒绝 / 运行期 Passed=false，不自动放行）。本次只补上"契约能携带身份"这一半——这是**任何**传输的前置。另一半（门禁级的人在环输入路径）需要**新增一个 RPC**（现有 approve/deeplink 路径都是 change 级、按 run 结算，没有"按门禁"的目标定位），而 `internal/grpc/pb/` 被 CI 的 protoc 版本钉死、本轮不动；或走投递集成（chatops/mobile deeplink 已有部分零件）。这是 roadmap 上 human 门禁审批人传输的开放产品决策，方向已定（复用 `internal/approval`），待独立排期。
- 影响面：接口变更只动了 3 个测试替身（本仓无生产实现），`go build ./...` / `go test ./internal/verify/ ./internal/engine/ ./internal/wiring/` 全绿。3 条新测试 + 1 项变异（去掉记录 → 身份两例转红）。
### 功能（条件式 ABAC 接入服务层：`policies.yaml` 成为第三条收窄约束）
- **此前 `policies.yaml` 只有 `levee rbac` 在算，服务层 `authz.Decide` 从不读它**——同一份策略文件，CLI 判它、serve 不判。现在 `authz.Load` 会读 `<dataDir>/policies.yaml`（缺失=正常；**解析失败=启动失败**，与 permissions.yaml 同姿态），并在矩阵与角色两轴都放行之后，再跑一道**只收窄、绝不能放宽**的策略层。
- 语义（`internal/authz/authz.go` `applyPolicyLayer`）：
  - **只在 allow 路径上咨询**——矩阵/角色拒绝了就不再看策略，所以策略永远不能把被拒的请求救回来（`TestConditionalPolicyCannotWiden` 钉住）。
  - **三态靠 `Evaluate` 的 error 区分**：显式匹配的 deny→拒绝；显式匹配的 allow→放行（`Via=policy`）；**无任何策略匹配（`ErrNoMatch`）→ 维持原判定**。把 `ErrNoMatch` 当成 deny 是这一层最容易犯的错——它就是"没有任何策略谈到这个请求"，与"策略拒绝"是两回事（`TestConditionalPolicyLeavesNonMatchingRequestAlone` 钉住；变异把 no-match 改成 deny 即转红）。
  - **条件求值出错 fail-closed**（拒绝），不静默放过。
  - 条件里能引用的 label：请求环境，以 `env` 与 `target.env` 两种拼写暴露（策略示例用的正是后者）；服务层按 `(env, action)` 判定，没有别的资源属性，故引用未知 key 的条件求值为"缺键"→不匹配→维持原判定（收窄方向）。
  - **资源族映射** `policyResourceFor`：变更类动作（plan/apply/approve/rollback/pause/resume/pause_all/resume_all/cancel）→ `change:*`；其余（view/admin 等跨族动作）→ 裸通配 `*`。即**具体非变更资源模式（如 `target:*`）在服务层暂不匹配**，它们对 `levee rbac` 仍然有效——这是本层的已知边界，写在此处而非留作沉默的空转。
- 与既有语义的关系：不改矩阵×角色两轴口径（继续管"哪些环境能去"与"在那儿能做什么"），策略层只加"且该资源条件成立"。`Decision.Via` 新增取值 `policy`，`levee authz explain` 会显示。
- 迁移说明（行为变更）：**给部署放一份 `policies.yaml` 的运维请注意**——它现在会立刻作用于 serve 的 plan/apply/approve/rollback/暂停/取消等变更动作判定；此前只影响 `levee rbac` 的检查输出。条件写错会拒绝本来放行的动作（收窄方向），需按 `levee authz explain` 的文案排查。
### 文档修正（前瞻步 `idempotent` 被规范误标为治理字段）
- **规范承诺了一个代码从不提供、且设计刻意不提供保证的不可篡改性**。`docs/leveelang-spec.md` §4.5 字段表曾写：step 上的 `idempotent`「**该声明进入 plan 哈希**（v2 治理字段），批准后不可改写；回滚补偿……据此决定重跑还是拒绝」。实测三处都不成立：
  - `plan.PlanStep`（`internal/plan/generator.go`）**没有** `Idempotent` 字段——前瞻步的声明在 parser→IR 之后、进入 plan 层时即被丢弃；`canonicalStepV2`（`internal/plan/hash.go`）同样无此字段，故前瞻步 `idempotent` **从不出现在 plan 哈希里**。
  - 真正进哈希、并被回滚补偿门禁 `compensationRepeatable`（`internal/rollback/manager.go:675`）读取的 `idempotent`，是 **`rollback {}` 内 undo step 上的同名声明**（`canonicalRollbackStep.Idempotent`，`hash.go:195`）——规范把两个字段混为一谈了。
  - 断点续跑「跳过哪一批」的判据是 **executor 模块类型自身**的幂等声明（`completedIdempotentBatches` 调 `executor.IsIdempotent(ps.Module)`，`internal/wiring/persist.go:334`），**从不**读作者的逐步 YAML 声明。这是 `docs/design-cluster-failover.md` §7.5 Q3 的明文取舍（"需要 executor 逐类型幂等声明……是独立项目"）——让调用方自证"可安全重跑"等于把安全属性交给调用方断言，与已修的「客户端控制 autoApprove」同族，故刻意不做。
  - 出厂 `examples/gate-templates/{nginx,mysql}.yaml`、`deploy/lab/workflows/lab-batch-restart.yaml` 里 5 处 `idempotent: true` 都是标在 **shell.exec 前瞻步**上（作者想说"这条重跑安全"），但模块级判定对 shell.exec 恒为 false，因此这些声明按上述设计本就不驱动任何跳过——规范把它们说成哈希绑定的治理字段是错的。
  处置（**改文档以匹配代码与设计，不改代码行为**，兼容零影响）：§2.2 关键字清单、§4.5 字段表、§2.1 语法草图三处统一改口径——前瞻步 `idempotent` 是 advisory（进 IR、不进哈希、非门禁），undo step 的 `idempotent` 才是 v2 治理字段；并在 §7.1「step 级 rollback 字段定义（补偿契约）」表补上此前**完全没登记**的 `idempotent（undo step 上）` 行，说明其哈希绑定 + `compensationRepeatable` 消费 + "一条未声明即整份补偿不可重跑"的语义。
  守门：新增 `internal/plan/idempotent_advisory_test.go` 两条特征测试把这个不对称钉成可执行契约——`TestForwardStepIdempotentIsNotHashBound`（仅改前瞻步 `idempotent` → plan 哈希必须不变）、`TestRollbackStepIdempotentIsHashBound`（仅改 undo step `idempotent` → 哈希必须变）。**变异自证**：把 `Idempotent` 从 `canonicalRollbackStep` 摘掉 → 第二条转红；把前瞻步 `idempotent` 一路接进 `PlanStep`+`canonicalStepV2` → 第一条转红；两条都能编译，红来自断言而非构建失败。若日后有人欲将前瞻步声明升格为治理字段，必须先同时改设计文档 §7.5 Q3 的取舍并翻这两条测试，不能悄悄漂移。
### 安全修复（清单/模板/审计/系统面接入权限矩阵，另 23 个 RPC 从此有判定）
覆盖面按生成的 `*ServiceServer` 接口逐个点数：target 5 + inventory 6 + template 5 + audit 4 + system 4 = **24 个 RPC**，其中 22 个走准入判定、`GetStatus` 走可见性过滤（计数也是一种读），只有 `GetVersion` 刻意保持不门禁。
- **写了 `permissions.yaml` 的部署，只把一半的 API 交给策略管**：`internal/grpc` 中五个服务对 authz 的引用计数为 0——`grep -c authz` 打在 `target_service.go` / `inventory_service.go` / `template_service.go` / `audit_service.go` / `system_service.go` 上全是 0（本批开始前实测）。矩阵只在变更路径生效，于是同一个只被允许看 dev 的调用方仍然可以：**枚举全部 prod 主机及其 `credential_ref`**（`ListTargets`/`GetTarget`/`CheckTarget`）、**冻结或退役一台 prod 主机**（`SetTargetStatus`，等于让别人跑不起来的刹车）、批量导入清单、删掉别队赖以创建变更的模板、读整条审计链与任意 run 的报告（`GetAuditLog`/`ListAuditTraces`/`VerifyHashChain`/`GetRunReport`，其中 `TargetHistory` 还会交出每个触及该主机的 run 的**工作流全文与创建者**）、导出部署配置（`GetConfig`，脱敏后仍含数据目录、监听地址与凭据引用形态）。
  落地姿态（一份 `internal/grpc/resource_authz.go`，五个服务不许各自漂移）：
  - **作用域取资源自己声明的环境**。主机的环境是 `labels["env"]`；变更的是 run 的环境列。分组、模板、配置什么都不声明，它们本来就是部署级的，按 `permission.default_env` 判定——**不去解析分组名 `"prod/db"` 的第一段**，那把一个安全判定挂在命名习惯上；矩阵生效又没有默认环境时是**拒绝**而不是猜（`authz.Decide` 既有语义）。
  - **清单级写 = `admin`**：能在机群里面做什么，不等于能决定机群里面有什么。
  - **读 = `view`，且列表收窄而不是整页拒绝**（拒绝会把本来允许看的行也藏掉）：`ListTargets`、`TargetHistory`、`GetAuditLog`、`ListAuditTraces`、不带 run 参数的 `VerifyHashChain`、以及 `GetStatus` 的 run 计数（**计数也是一种读**："你的 prod 此刻在跑 2 个变更"是关于别人变更的信息）。`TotalSize` 跟着收窄后的集合走，否则分页会承诺下一页拿不到的行。
  - **不可归因主体照旧放行读、拒绝写**——继承既有姿态而非新造（共享令牌下过滤所有人不产生任何隔离，只会把控制台弄黑）。
  - 两处刻意不同：`InstantiateTemplate` 取**请求声明环境里的 `plan`**（它创建的是一条变更）；`ImportTargets` 对文件里**每一个**声明环境都要 `admin`，因为一份清单文件就是一批 `AddTarget`，不能比其中单条更可授权——判定在任何写入之前，含一台越界主机的文件**一台都不导**。`GetVersion` 保持不门禁：需要队授权的探针等于让监控在没人配置时集体失效。
  **接线由测试证明，不靠假设**：`InventoryService` 原本在注册点构造（`cmd_serve.go:792`），游离于所有测试之外；现在它和其余服务一样出自 `buildServeServices`，`cmd/levee/serve_policy_wiring_test.go` 用真实构造函数断言①六个服务对未登记主体都拒、②sre 成员的变更授权齐全仍被拒清单写、③dave 的 `admin` 让同一批调用通过（证明拒的是矩阵而不是硬编码）、④空数据目录一切照旧。**此前 `cmd/levee` 没有任何测试提到过 authorizer**——门在结构体里不等于门在进程里。
  **迁移说明（行为变更）**：启用矩阵的部署会立刻看到三类变化——只读型调用方拿不到别的环境的主机/审计行（列表变短，不是变空）；清单与模板的写、`GetConfig` 需要 `admin`；**未配 `permission.default_env` 时分组/模板/配置一律拒绝**（那是"没有环境可判"的正确回答，补救是配默认环境或给矩阵补一个 `*` 环境）。`levee serve` 启动日志与 `levee authz status` 的文案同步改成如实描述覆盖面。
验证：新增 15 个测试函数（`internal/grpc/service_policy_test.go` 12 + `cmd/levee/serve_policy_wiring_test.go` 3）。**先探针后修复**：12 条服务级用例在未接线树上跑，11 条按断言失败（只有"没配矩阵就不变"那条通过）——这就是"今天是敞开的"的实测证据。**18 项变异全部被抓且均能编译**：判定动作用错（写按 `view` 判）、读判定退回默认环境、过滤整体关闭、过滤恒放行、`envOfTarget` 恒空、`AddTarget`/`RemoveTarget`/`SetTargetStatus`/`ImportTargets`/`InstantiateTemplate` 五处作用域各自退回默认环境、列表与历史与审计行与 verify 全扫描四处去掉过滤、`GetRunReport` 不判环境、`GetStatus` 退回总数、serve 的两处 `WithAuthorizer` 分别删掉。`ChangeService` 的 `authorize`/`authorizeRead`/可见性谓词改为调用同三个助手，拒绝文案与"是否启用过滤"的规则从此只有一份定义。`go build ./...` / `go vet ./...` / `go test ./...`（65 包）与 `internal/grpc/pb/` 未改见提交说明。

## [v1.19.0] - 2026-10-06 — 首个可交付快照：`allow_irreversible` 编译期白名单，外加 post_batch 门禁路由、变更归属绑定、webhook 通知装配与交付物版本门禁

规范登记的最后一个"零产生点"词表被接线：`allow_irreversible` 从"规范目标，尚未接线"变为真实的编译期门禁——判定为不可逆的步骤（显式 `irreversible: true` 或引擎固有破坏性词表命中）必须列在 workflow 级白名单里，否则 LE082 拒绝编译。**这是行为变更**（缺省拒绝），迁移说明见下。

### 变更（含迁移说明）

- **缺省拒绝（breaking）**：升级到本版后，凡步骤判定为不可逆而未列名 `allow_irreversible` 的工作流，`levee compile`、`levee import` 与 serve 的 plan 路径都会以 LE082 拒绝。受影响动作 = 显式声明 `irreversible: true` 的任意 action，以及引擎固有破坏性词表：`pkg.remove`、`file.delete`、`user.remove`、`mysql.replica_switch`、`mysql.pt_osc`。**迁移**：在 workflow 顶层加一段白名单，逐个列名确要执行的不可逆动作——

  ```yaml
  allow_irreversible:
    - mysql.replica_switch
  ```

  白名单条目同样要求 `module.action` 形式（LE101）。无法列名即不应执行——这正是这条门禁的语义。仓库内 2 个示例文档（`dr-switch-orders-db.yaml`、`gate-templates/redis.yaml`）已同步迁移；既有库里**已审批过的 plan 不受影响**（门禁作用在编译/生成期，不追溯已批准的产物）。

### 新增

- **解析器与 AST**：workflow 级 `allow_irreversible` 字段（`yamlWorkflowRaw` → `Workflow.AllowIrreversible`）；IR 产物（`levee compile --ir`）携带 `workflow.allow_irreversible`，作为"本次编译授权了什么"的凭证；发射器按排序输出保证字节稳定，并可回环解析。
- **validator V14 规则（`validateIrreversibleWhitelist`）**：判定优先级与 `executor.IrreversibleChecker.Check` 一致（显式声明 > 固有词表）；白名单条目格式按 LE101 校验；多步骤混合只报未列名者，不误伤已列名步骤。
- **固有词表单一来源**：`dsl.DefaultIrreversibleActions()` / `dsl.IsInherentIrreversible()` 成为"天生不可逆"的唯一词表——plan 生成器的 checker 注册与 dsl 编译门禁消费同一份，两个层面不可能对判定分叉（原内联在 `plan.NewGenerator` 的清单迁入）。
- **plan 期纵深防御**：`plan.Generator.Generate` 对服务器路径（wiring.GeneratePlan 直连、不经 validator）执行同一 V14 判定，LE082 拒绝——与 LE097 的 plan 期门禁同型。
- **测试**：新增 `internal/dsl/allow_irreversible_test.go`（解析/门禁/条目格式/混合步骤/发射回环/IR 携带）与 `internal/dsl/inherent_irreversible_test.go`（词表钉住）；plan 生成器新增未授权拒绝用例；e2e 回滚演练夹具补授权；两个示例进 `TestShippedExamplesCompile` 守护。实测：`levee compile` 对带白名单示例 ok、对未列名 `pkg.remove` 以 LE082 退出 1。
- **文档一致性**：spec 字段表与 V14 行改为已接线口径；roadmap P1 项闭环；`docs/scenario-cross-region-ops.md` §7 边界更新为三层保护（编译期白名单 + `confirm=yes` 硬门 + 高危审批路由）；Chart appVersion → 1.19.0。


### 新增（D-20 webhook 通知渠道装配）

- **`notify.webhook.enabled` 此前是一个背后没有接线的开关**：渠道实现（`internal/notify/webhook.go`）、配置键与启动校验（`internal/config/config.go:724-732`）都在，但全仓 `NewWebhookNotifier` 的非测试调用点为 0，`cmd_serve.go` 从不构造 `NotificationManager`，于是引擎的 `newRunNotifySink` 恒为 nil、回滚分级只落日志（MVP 清单 D-20 原先按"已交付"记录，2026-10-05 上午按实测改标"未装配"）。本批接线：`cmd/levee/serve_notify.go` 的 `buildServeNotifyManager` 在 enabled 时注册名为 `webhook` 的渠道，并作为 `wiring.WithNotificationManager` 装进执行引擎；启用而 URL 缺失**拒绝启动**（与 `notify.chatops` / `notify.jira` 同规则——静默跳过会留下一个看起来配好、实则永不投递的服务）；签名密钥取环境变量 `LEVEE_WEBHOOK_SECRET`，未设置时明文投递并在启动日志与 `config.example.yaml` 里言明"接收端无法验真"；`--engine-enabled=false` 且 webhook 启用时启动告警：回滚分级是当前唯一投递源，引擎不在就一条也不会发。
- **投递范围如实限定**：只有回滚分级（partial / failure）接入该 manager；apply / approval 事件尚无生产者，未在本批假装接通。
- **验证**：`cmd/levee/serve_notify_test.go` 把一条真实回滚通知打到 `httptest` 接收端，断言事件头、`metadata`（`run_id` / `initiator` / `approver` / `scope`）与 `X-Levee-Signature` 可被密钥验证，另有"未设密钥不得出现签名头"的对照与"禁用时不构造管理器"的对照；`internal/wiring/rollback_notify_transport_test.go` 证明 `newRunNotifySink` 在"有传输 + 有 initiator"时真投递、在无传输或无 initiator 时保持 nil（宁可不寄也不寄给编造的收件人）；`TestNotifyWebhookSwitchHasAssemblyPoint` 是结构守卫——`cmd_serve.go` 必须同时存在开关消费点与 `WithNotificationManager` 装配点，防止"配置在骗人"这一族缺陷复发。

### 安全修复（变更创建路径的归属与环境保真）

- **模板实例化出来的变更"不属于任何人、位于默认环境"，三个独立后果**：`InstantiateTemplate` 是三条创建变更路径中的一条（另两条为 `CreateChange` 与 `CloneChange`），但没有跟上把 `run.Creator` 绑到**已验签主体**的那一轮修正——它写入 `Creator: "grpc"`，并且**根本不写 `IncidentID`**，而 `envOf()` 读的就是这一列。
  ① **环境被洗成默认环境**：请求带着 `environment: prod` 来，落库后环境为空，此后每一个治理判定（plan / apply / rollback / approve、以及读可见性过滤）都退回 `permission.default_env`。本机 SQLite 实测（矩阵：dev 可 act、prod 仅 view，`default_env=dev`，alice 的角色只有 view）：**一个只被允许看 prod 的调用方成功 apply 了声明为 prod 的变更**——`ApplyChange` 里策略门在任何其它判定之前，所以它是被无关的批准版本检查拦下的，而不是被策略拦下的。响应体又把 `environment: "prod"` 原样回显，客户端拿不到任何信号（`TestInstantiateTemplate_ResponseAgreesWithTheStoredRow` 现在钉住"回显即承诺"）。
  ② **`exclude_initiator` 对模板创建的变更失效**：`run.Creator` 是审批链 `Initiator` 的唯一来源，而 `"grpc"` 不等于任何真实主体名，于是规范 §8.1 对 high 档强制的独立审查排除的是一个人格化的通道名，作者自己就能投出关键一票（`TestInstantiateTemplate_AuthorCannotSelfApproveWhenIndependenceRequired` 走真实审批服务复现并封口）。
  ③ **创建动作不留审计行**：`CreateChange` 记 `action=create`，模板路径一条都不写，这些变更在 `levee audit log` 与哈希链证据里是凭空出现的。现在补上同款 create 行（写入失败只 WARN，与 `recordAudit` 一致的姿态）。
  落地：三条创建路径共用一个归属函数 `creatorFromCtx`（有可验签主体取主体，否则保留审计标签——就是 `CreateChange` 原本写了一遍的规则，现在只有一份），`InstantiateTemplate` 持久化声明的环境并补审计行。**迁移说明（行为变更）**：存量由模板创建、环境为空的 run 仍按默认环境判定（本批不回填历史行——伪造历史行的环境等于替操作者编造事实）；但从本批起，声明了操作者无权操作的环境的实例化请求会在 apply 处被拒，此前它会成功。
- **`CloneChange` 的归属仍取客户端自报的名字**：那一轮只改了 `CreateChange`，克隆路径依旧是 `Creator: actorFromCtx(ctx)`——`x-actor` 头里写谁就是谁。后果与上面 ② 同源：调用方可以把发起人登记成别人的名字，于是独立审查排除的是那个人，而真正的作者投票无碍。现在三条路径同源，且"无可验签主体时保留标签"这一半由 `TestCloneChange_KeepsTheAssertedLabelWithoutASubject` 单独钉住（本地模式与共享令牌下那是唯一可得的记录，改成常量等于把"谁敲的命令"擦掉）。

验证：新增 8 个测试函数（`internal/grpc/change_attribution_fidelity_test.go`：环境落库、prod 声明不被 dev 授予 apply（含"同一调用者在 dev 确实能 apply"的反证一半，否则测不出"-marker 被尊重"与"策略一律拒绝"的区别）、创建者=已验签主体、响应与行一致、审计行存在、模板变更的作者不能自批、克隆取主体不取 header、无主体时保留 header 标签）。**8 项变异全部被抓且均能编译**（环境写回空、Creator 退回 `"grpc"`、克隆退回 `actorFromCtx`、`creatorFromCtx` 忽略主体、审计 action 改名、审计行的 run_id 变成孤儿、响应 owner 退回常量、响应环境退回空串）。`go build ./...` / `go vet ./...` / `go test ./...`（65 包）见提交说明。

### 变更（门禁位置路由，含迁移说明）

- **`position` 现在真的决定门禁什么时候跑，三处声明点全部被执行**：规范 §2.2 的位置词表（`pre_apply` / `post_batch` / `post_apply`）此前是**装饰**——`convertGate` 无条件把每条声明塞进 `GateSpec.Post`（物化成 `post_apply`），而只有 `GateSpec.Batch` 绑定 `verify.PhasePostBatch`，全仓没有任何代码填充 `.Batch`。同时 `walkPlanGates`（唯一把声明变成可运行门禁的遍历）只走 `Batches[].Steps[].Gate`，于是：
  - **`batches: gate:` 与 workflow 级 `gates:` 进了 plan、进了 plan_hash（即被批准的产物承诺了它），却一次都不执行**。`examples/gate-templates/redis.yaml` 头部注释"after EVERY batch completes and blocks the next batch on failure"挡住的东西为零。
  - **workflow 级的 `human` 门禁连 plan 期拒绝都收不到**（判定与物化共用同一个遍历，遍历没看到它）——声明了、批准了、静默消失，正是 `docs/gates.md` 承诺不可能发生的"声明的门禁假装通过"。
  - **`slo` 是任何 YAML 形状都表达不出来的门禁类型**（它只能在 post_batch 执行），报错文案当时只能如实写"目前没有路径能到达那个槽位"。
  落地：位置词表与槽位映射收成一份 `internal/dsl/gate_position.go`（未知位置 LE051 拒绝，不默认塞入），三个声明点（workflow 级 `gates:`、step `verify:`、`batches.gate:`）共用；`walkPlanGates` 遍历三处并按 `workflow:<槽>:<序>` / `batches:<槽>:<序>` / `step:<名>:<槽>:<序>` 命名，**执行与 plan 期拒绝判定同一个漏斗**；slo 的补救文案换成可执行的真实建议并补正例（同一检查换个位置即可出计划）。
  **迁移说明（行为变更）**：从未运行过的 `batches.gate` / workflow 级 `gates` 现在会真的执行——原本能连续通过的批次可能被挡下并触发回滚。这不是新增压制，而是把已批准产物里那条一直存在的承诺兑现；若某条存量工作流的批级门禁本就不该拦批，请显式改掉那条声明而不是期待它继续沉默。缺省位置：step `verify:` 仍是 `post_apply`（与旧行为一致），`batches.gate:` 缺省即 `post_batch`（§4.3 语义）。
  **LE052 随之路由完成后可发**：`levee compile` / `new` / `import` / 会话桥会提示"声明了 batches 却没有任何 post_batch 检查"（三处任一有即满足；无 batches 块不发，那条由 LE096 负责）。
  测试：`internal/engine/gate_position_wiring_test.go`（从文档一路到注册门禁：槽位路由、三处都注册、workflow 级 human 门禁被 plan 期拒绝、slo 换个位置就能计划）、`internal/dsl/compile_warnings_test.go`（LE052 发出与三处豁免）、原钉住 bug 的 `TestBatchLevelGateIsHashBoundButNeverMaterialised` 翻为 `TestBatchLevelGateIsHashBoundAndMaterialised`、`parser_test.go` 的 `gates:` 断言按槽位重写。

### 安全修复

- **`plan` 从未被授权，于是任何人都能撤销别人的批准（服务端授权覆盖面 6→11 个 RPC）**：`internal/authz` 接线时只覆盖了 apply / rollback / approve / reject 四个治理动作，`PlanChange` 漏在外面。而 plan **是写操作**：它把规范化计划持久化到 run、发起审批链，并且**当计划哈希变化时把 `approved` 的 run 重置回 `draft`**（`change_service.go` 的 re-plan 分支）。后果是任何认证调用方——包括只该看列表的人——都能对一个已批准的变更再 plan 一次，让批准作废。现在 `plan` 与 apply 走同一姿态：矩阵生效且主体不可归因 → `Unauthenticated`；主体可归因但无 `plan` 授予 → `PermissionDenied`，并在**任何状态写入之前**拒绝（断言 `run.PlanJSON` 为空，被拒的 plan 不留产物）。
- **批量暂停/恢复的授权名单原本按"客户端自报的名字"判定**：`bulkTransition` 用的是 `actorFromCtx(ctx)`，而这个函数自己的注释写着 *"suitable for audit-trail attribution and UX only, never for authorization decisions"*——共享单令牌模式下 `x-actor` 随便填，`bulk_grants` 里写了 `sre-oncall` 就等于把钥匙挂在门上。现在判定改用**已验签主体**（命名令牌 / SSO / OIDC），无主体 → `Unauthenticated` 并在消息里给出补救办法（`--auth-token name=secret`）；仍然先问授权检查器再返回，所以被拒事件照旧落到 `permission.denied` 审计行。CLI 本地模式不受影响（`ContextWithActor` 同时设置主体）。
- **`GetConfig` 在 gRPC 上默认返回未脱敏配置，而注释写着"脱敏是默认"**：判定是 `if req == nil || req.GetRedactSecrets() { 脱敏 }`，但 `redact_secrets` 是 proto3 **标量 bool**，"未设置"与"显式 false"在服务器端不可区分——于是裸调 `GetConfig(&pb.GetConfigRequest{})` 拿到的是含目标机凭据的明文（REST 侧因为强制传 true 而把问题藏住了）。修法是把决定权交还给部署：新增 `security.expose_raw_config`（默认 false）；关闭时任何请求都只拿脱敏内容（含 nil 请求与 `?redactSecrets=false`），打开时才允许明文且每次响应记 WARN。**没有新增 wire 字段**：本地 protoc 与 CI 钉住的 27.0 不同版、无法字节复现生成物，加字段会让 CI 的 proto 漂移门变红，而"部署开关"这个形状也不需要字段存在性就能闭合漏洞。
- **变更读路径（25 个 RPC 中的 7 个）接入 `view` 判定，并明确"不可归因主体不过滤"这一取舍**：`GetChange` / `GetLogs` / `GetTrace` / `GetDiff` / `WatchChange` / `StreamLogs` 现在按 `action=view` 判定；`ListChanges` 不是拒绝整页而是**按可见环境收窄**（拒绝会把允许看的行也藏掉），且因为 `RunFilter` 只能带一个 `IncidentID`，可归因主体会走带扫描上限的过滤路径、其余保持原有 store 分页不变。**刻意与写不同的一半**：无可验签主体的调用者（共享令牌）读路径放行——同一令牌的所有人在服务端是同一个主体，拒他们不带来任何隔离、只会让看板变黑；这一半和那一半被同一组测试钉住，写路径的豁免则不存在。姿态同时写进 `config.example.yaml` 与 `docs/security-audit.md` 已知限制。

验证：新增 13 例（`internal/grpc/change_service_readauthz_test.go`）——plan 的两种拒绝与 dev/prod 分界、6 个读 RPC 各自被拒且 `view` 出现在文案里、不可归因主体读放行而写仍拒、列表按环境收窄（含与多状态过滤组合的那条路径）、批量动作在"只有自报名字"时被拒且留下审计行、`GetConfig` 的裸调用/显式 false/nil 请求三种形态都拿不到凭据、开关打开后的明文路径。**变异验证 7 条全部被抓**（去掉 plan 授权、读门恒放行、把不可归因主体改成拒绝、列表过滤永不启用、批量判定退回 `actorFromCtx`、`GetConfig` 退回按请求决定、忽略部署开关）。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / docgen 与 proto 漂移门（pb 未改）见下条提交说明。

- **CLI 与 serve 可以指向两个数据库，而每条命令都报告成功（配置层没有 PostgreSQL 的入口）**：#46 把冻结期接成执行门禁时，日历那一半留了一句"CLI 只能连本机 SQLite，接共享库要先给配置 `database.dsn`"。根因比缺字段更深：`DatabaseConfig` 只有 `driver`(恒 sqlite)/`path`，`config.Validate` 写着 `must be sqlite (MVP)`，而 serve 有 `--pg-dsn`——于是 `--cluster --pg-dsn` 的部署里，`levee calendar freeze` 写在操作者本机文件、服务端读自己的库，**两头各自都成功**，且 plan 照常通过（它读的那张表恰好是空的）。现在 `database.driver` 取 `sqlite` \| `postgres`（postgres 必须配 `database.dsn`；sqlite 多配 dsn、postgres 缺 dsn、未知驱动都是配置错误而不是"退回本机文件"），取库决策收成一处 `cmd/levee/helpers.go:openStoreFromConfig`，由所有 CLI 命令、未加 `--cluster` 的 `serve`、`system status`/`doctor` 的探活、`levee calendar`、`levee backup` 共用。三条启动期硬约束：`serve` 给 `--pg-dsn` 而不给 `--cluster` → 启动失败（未启协调、无租约无 fencing 的进程连集群共享库，比退回本机文件更危险；真要单节点 PG 用配置）；`--cluster` 下 `--pg-dsn` 与 `database.dsn` 不等 → 启动失败并打印两者脱敏端点；`calendar create --frozen` 的 stderr 通知改为如实报"驱动 + 位置"，sqlite 时明说本地文件服务端读不到。
- **`tenant.enabled: true` 会让 #46 的冻结门禁整套失效**：多租户下 serve 交给各服务的 store 是 `tenant.TenantStore` 装饰器，而 `calendarFor` 用类型开关选 SQL 方言——匹配到装饰器就落进 default 分支返回 `unknown state store type`，serve 于是打一条 WARN 后**不带任何冻结检查地继续 plan**。这不是理论问题：治理门禁恰好在最需要它（多租户共享一套服务端）的形态下自己关掉，而日志只说"calendar unavailable"。现在方言按**底层真实库**解析（`state.Underlying` + `Unwrapper` seam，`TenantStore.Underlying()` 委托既有的 `Base()`），并由 `TestCalendarForSeesThroughTheTenantDecorator` 断言"经装饰器写入的冻结期确实能被同一条链拒绝"（不是只断言"能打开"）。
- **`system status` / `doctor` 探测的从来不是部署在用的那个库，`GetSystemStatus` 还对外自称 sqlite**：`system status` 与 `doctor` 的 database 检查直接 `state.NewSQLiteStore(cfg.Database.Path)`——PG 部署下它探的是一个没有数据的本机文件（顺手把文件建出来）却回答 `db_status: ok`，JSON 里还回 `db_path`；gRPC/REST 的 `store_type` 取 `cfg.Database.Driver`，于是 `--cluster --pg-dsn` 的服务端向健康检查消费者报告 sqlite（同函数里还另有一行硬编码 `"sqlite"` 兜底）。现在四处共用一处决策，`db_status` 探的是配置点名的库，输出改为 `db_driver` + `db_location`（PG 下**不再**输出 `db_path`：一个名叫 path 的键偶尔装着连接串，比少一个键更糟）；`store_type` 由**打开的 store** 自报（`state.StoreDriver`，与 REST 早已存在的 `backendLabel` 合并成一份实现，后者原本自己写了一遍类型开关），认不出来的 store 报 `unknown` 而不是猜。
- **`levee backup` 把 PostgreSQL 口令打到 stdout**：结果文档的 `source` 字段用 `mgr.Source()`，而 PG 形态下 `Source()` 就是原始 DSN——`--json` 消费者、终端回显与任何日志采集都会拿到 `user:password@host`。新增 `Manager.SafeSource()`（SQLite 分支原样返回文件路径，PG 分支返回 `postgres@host:port/db`），payload 的 `source`/`target` 都改用它，并把 restore 的 `target` 直接从 payload 复制，避免同一个值留两处调用点；脱敏实现收在 `internal/state.RedactDSN` 一处，`system config get database.dsn`、启动日志、错误串同此口径，非 URL 形态（`host=… password=…`）整体 withheld 而不回显。

验证：新增 28 个测试函数（`internal/config` 5：驱动/DSN 配对校验矩阵、`StoreLocation`、`database.dsn` 从 YAML 读通；`internal/config/sample_config_test.go` 3：出厂两份配置样例必须能加载并通过校验、样例注释里 advertised 的驱动逐个问 Validate（另用一个未实现的 `mysql` 作反例，证明该循环不是恒真）、集群场景那段必须写 `driver: postgres` 且展示 `dsn`；`cmd/levee/store_selection_test.go` 11；`cmd/levee/store_selection_pg_test.go` 1（**活 PostgreSQL** 全链路）；`internal/state/driver_test.go` 5（另把 `TestRedactDSN` 从 config 包搬过来，其中 `TestStoreDriverNamesPostgres` 打活库）；`internal/grpc` 1（`store_type` 由 store 而非配置决定；`TestGetStatus_NoStore` 补断言"没有 store 就报 unknown"）；`internal/backup` 1；`cmd/levee/cmd_backup_test.go` 1）。另有 4 条既有用例改造：`TestValidate_InvalidDBDriver` 与 `TestValidate_MultipleProblemsReported` 原先拿 `postgres` 当"非法驱动"的例子——postgres 已经是合法后端，改用真正未实现的 `mysql`，否则这两条会安静地测一个不存在的情形；`TestPrintSystemStatusHuman` 改为断言新的 `db_driver`/`db_location` 且输出不含 `%!` 格式化残迹。CLI 侧的判定一律取**可观察后果**：配置写 postgres 时必须没有任何 SQLite 文件被建出来、`system status --json` 的 `db_status` 必须是 `unreachable`、DSN 口令不得出现在输出里。

活库证据（本机 `postgres:16-alpine` 容器，私有 schema + 固定 `search_path`，用完即删）：`TestPostgresConfigRoundTripsFreezeThroughTheCLIStore` 实测"配置写 postgres → `openStore` 交出 PGStore 且没建出本机文件 → 日历表在真服务器上建起来 → 冻结期被同一句柄拒绝（`errors.Is(err, calendar.ErrFrozen)`）、emergency 仍可越过 → **另开一次连接**仍读到同一条冻结 → 全程无 SQLite 文件"。`internal/state` 的 `TestStoreDriverNamesPostgres` 与 `internal/calendar` 的两条活库用例同批跑绿（本机未跑 `-race`：Windows 的 `-race` 需容器，由 CI 承担）。

顺带查出并补上 #46 的一处门禁漏洞：**`internal/calendar` 的活 PG 用例在 CI 里没有任何步骤会跑**——postgres job 逐个列举 state/cluster/takeover/backup 四个包，日历包不在其中，所以那批"活库实测"只在本地执行过一次，此后既不会被复跑也不会因漂移而红。本批为该包与新的 CLI 活库用例各加一条 isolated step（各带私有 schema 说明，不与共享 `levee_test` 上其他包的行互踩）。

**21 项变异全部由断言抓住且均能编译**：未知驱动退回 SQLite、postgres 分支改开 SQLite、`calendarFor` 去掉 `Underlying`、`StoreDriver` 的 default 改成返回 sqlite、`Underlying` 不再解包、`GetStatus` 改回读配置、`backendLabel` 忽略 store 类型、`system status` 退回直连 SQLite、删掉 serve 的两条启动拒绝（各一条）、`resolveBackupManager` 忽略配置后端、`backupPayload` 换回 `Source()`、`RedactDSN` 失败开启、`config.Validate` 三项各拆一条（PG 缺 DSN、未知驱动放行、sqlite 多 DSN 忽略）、冻结通知删掉本地文件警告，另有配置样例三条（集群段改回 sqlite、样例多 advertise 一个 `mysql`、样例去掉驱动行注释）。结构测试 `TestCLIHasOneSQLiteOpener` 另实测一次：往 `cmd/levee` 放一条自己开 SQLite 的命令即转红。文档面：`configs/config.yaml` 此前**根本没有 `database` 段**，`config.example.yaml` 还写着"PostgreSQL 支持为后续版本规划"（而 `serve --cluster --pg-dsn` 早已跑在 PG 上），两处都改成落地口径并纳入上述门禁；`deployment.md` 的"集群形态边界"段、`cli-reference.md` 第 10.2/10.3/13.3 节与 serve 旗标表、`leveelang-spec.md` §4.2b 的存储与边界条目一并更新。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / gosec / docgen 见提交说明。

### 修复

- **`cluster_nodes` 的现网欠账补上迁移（PostgreSQL schema v7）**：#21 把 DDL 收成 `internal/dbschema` 一份，并按集群路径实际生效的形状去掉了 `UNIQUE (address)`——但 `CREATE TABLE IF NOT EXISTS` 不改已存在的表，所以由 state 侧先建库的部署**仍带着这条约束**。它不是闲置：节点身份是 `id`，同一监听地址以新 `id` 重新注册正是 failover/takeover 的动作，实测含该约束时 `internal/takeover` 有 10 个用例报 `SQLSTATE 23505 (cluster_nodes_address_key)`。新增 v7 步骤做两件事：`DROP CONSTRAINT IF EXISTS cluster_nodes_address_key`（再跟一条 `DROP INDEX IF EXISTS` 覆盖"以同名唯一索引形式存在"的形态）与 `ADD COLUMN IF NOT EXISTS capabilities`（cluster 侧先建库的历史形状没有这一列）。两条都幂等，两条历史形状迁移后与唯一定义**列集合相等**。如实分档：**约束那一半是真缺陷**（会让接管失败），**补列那一半只是形状收敛**——今天没有任何生产代码读写 `capabilities`（全仓检索只命中注释）。活库实测：`-p 1` 串行跑 `internal/state`、`internal/cluster`、`internal/takeover`、`internal/backup`、`internal/dbschema` 全绿；新增 5 例迁移测试，含"迁移前重复地址插入必须失败、迁移后必须成功"的双向断言；6 项变异全部被抓（删约束语句、约束名拼错、删补列语句、补列写错列名、`pgCurrentSchemaVersion` 不跟着涨、步骤版本号错位）。迁移测试用私有 schema + 固定 `search_path` 的单连接并在用完立即归还——共享测试库上的 `cluster_nodes` 由并发的 cluster 包测试所有，动它会让两边都红；连接若按 helper 逐个持有会耗尽 `MaxOpenConns=5` 把整包挂死到超时（实踩过一次）。
- **Helm chart 向 `levee serve` 传了一个不存在的旗标**：`deploy/helm/levee/templates/deployment.yaml` 传 `--cluster-dispatch-worker-capacity`，二进制注册的是 `--cluster-dispatch-capacity`（`cmd/levee/cmd_serve.go:194`）。后果是**每次 `helm install --set mode=cluster` 起来的 Pod 都在启动时以 "unknown flag" 退出**（CrashLoopBackOff），而 `docs/deployment.md` 也照抄了同一个错名。v1.18.0 发布说明里写的"`helm lint`/双形态 `template` 校验通过"确实做过，但是**手工做的、且手工校验只到 YAML 能解析为止**——没有任何环节把渲染出的 args 与真实命令树比过。两处名字已订正，并补了下述两道持久门禁。
- **租户 ID 每次调用都被重新铸造，所有按 ID 的租户子命令跨进程必失败**：注册表加载路径用 `tm.Create()` 重建每条记录（`Create` 会铸造新 ID），再调 `patchTenant()` 只改结构体字段——map 的键仍是新铸的 ID，磁盘上写的还是旧 ID。实测（`levee tenant create --name acme` 打印 `tenant-a2e96355ea3e35bf`，`tenants.yaml` 里也是这个 ID，随后 `levee tenant show tenant-a2e96355ea3e35bf` 回答 `tenant: not found: tenant-a2e96355ea3e35bf`，且在 show 过程中又铸出 `tenant-330522e0cf7879e6`）。受影响面是 `show/suspend/resume/delete/quota/usage` 全部按 ID 寻址的命令。修法：`internal/tenant` 新增 `TenantManager.Restore(t, quota)`（按记录自带 ID 入表、同名冲突仅在对方未软删除时拒绝、配额按同一 ID 注册——配额表同样以 ID 为键，改错键会让 `tenant show` 静默读回"无限制"），`registryToManager` 改用它并删除 `patchTenant`。重复 ID/活名冲突的记录现在**跳过并 WARN**而不是静默丢弃（保留原容错契约：一个坏行不该让全部租户命令不可用）。
- **文档承诺了实现里不存在的旗标与前置条件（10 项实测，9 项为真）**：`levee push send --deep-link`、`agent list --status`、`agent remove --force`、`tenant list --status`、`tenant suspend --reason`、`tenant delete --force`、`drift baseline set/auto --name`、`drift baseline list --host` 逐条拿构建出的二进制对 `--server http://127.0.0.1:9` 实测，cobra 全部回答 `unknown flag`；`compile --lenient` 是唯一假阳性（旗标真实存在，是扫描器的 `|--lenient]` 分支没剥掉前导 `--`，已修扫描器并补 M1 变异证明该修复是承重的）。处置按"能不能诚实接线"分两类：**接线**——`push send --deep-link`（payload 键用 `deeplink`，与 `internal/approval/mobile.go:110` 既有移动端契约一致）、`tenant list --status`、`agent list --status`、`drift baseline list --host`（改为可选过滤）、`tenant suspend --reason`（`Tenant.SuspendReason` 落盘并在 `tenant show` 可见，`resume` 清除）；**删文档**——`agent remove --force`（`Deregister` 根本不检查 `ActiveTasks`，没有守卫可"强制"绕过）、`tenant delete --force`（`TenantManager` 不持有租户名下资源的反查，级联清理无从谈起）、`drift baseline --name`（基线以 host 为主键、一台至多一份，命名多版本要动存储主键）；`tenant delete` 那句"仅当 suspended 且无活跃变更时可删除"也一并订正——实现里没有这个前置检查。
- **状态词表从三份副本收敛为一份**：`TenantStatus` 的字符串形态原本在 `String()` 与 `ParseTenantStatus()` 两个 switch 里各写一遍，`agent` 侧 `--status` 若照文档写会引入第三份。现 `tenant` 用 `tenantStatusNames` 表驱动（`TenantStatusValues()` 供报错文案与 CLI 帮助渲染），`agent` 用 `AgentStatusValues`/`AgentStatusNames()`/`ParseAgentStatus()`。CLI 过滤器遇到未知取值**报错并列出全部接受值**而不是返回空列表——`levee agent list --status active` 过去那种"看起来集群没有 agent"的静默失败正是运维最难发现的一类。顺带订正 `cli-reference.md` 里凭空捏造的 agent 状态词表（`active / inactive / lost` 三个值本包永远产不出来，真实是 `registered / idle / busy / offline`）。

### 新增

- **交付物旗标门禁 `TestDeliveryArtifactsPassRegisteredServeFlags`**：把 Helm 模板（`deploy/helm/levee/templates/**/*.yaml` 的 args 序列项）与 systemd 单元（只扫 `ExecStart=` 续行，避开注释里的 `$LEVEE_SERVE_EXTRA` 示例）中出现的每一个旗标，对着**活的 cobra 树**解析（含继承的持久旗标与单字短旗标 `ShorthandLookup`），未注册即红；`checked >= 15` 的下限断言防止正则失配把门禁变成空跑。
- **CI `delivery` job（`scripts/validate_delivery.sh`）**：`helm lint --strict` + 6 个取值形态渲染（defaults / cluster / cluster 去掉 snapshotDir / ingress / 内嵌 PG / 3 worker）+ `helm package`。它覆盖文本扫描看不到的那一类：模板语法错误、`.Values` 路径失效、以及 helm 把无法解析的键渲染成字面量 `<no value>` 塞进容器 args。每个形态还断言"渲染出非零个 serve 旗标"（实测 defaults 7 / cluster 17），否则分支没被渲染到就等于没测。helm 按 SHA256 钉版（v3.18.4），本机与容器内均实测脚本全绿后才入库；`delivery` 同时进 `needs` 与聚合清单（后者有防漂移断言，漏一个就会红）。
- **文档旗标守卫的可选值分支修复 + 行号定位**：`[--strict|--lenient]` 形式的每个备选都要剥前导 `--`；finding 现在带 `文件:行号`，一条文档写错不用在 2600 行里人肉找。当前交叉核对 **457** 个文档旗标引用（`cli-reference.md` + `quickstart.md`，含每处调用的选项与继承的持久旗标）。

**两条门禁共做 13 项变异，逐条确认"红"来自断言失败而非编译失败**（先 `go build ./...` 通过才认定是语义变异）：chart 与 systemd 各改一个旗标名、`--status` 过滤被忽略、drift host 过滤反转、push payload 退回 nil、装载路径重新铸造 ID、`suspend_reason` 未写进注册表记录、resume 未清除原因、tenant/agent 两份词表各删一项、重复 suspend 不再更新原因、扫描器不剥备选值前导 `--`（这条复现的正是 `--lenient` 假阳性本身）。

- **编译期提示通道落地：`Validator.Advise` 让目录表里的 `CompileWarning` 一档第一次有了产生点**：LE033 / LE094 / LE095 / LE096 四个警告码自登记起就没有任何代码发出，规范三处（§4.3 "超过告警 LE033（warning，不阻断）"、字段表"缺省 …（告警 LE0xx warning）"、错误码表"仅 warning"）承诺的是一个不存在的通道。原计划"给 `ValidationError` 加 severity、消费者按 severity 过滤"没有采纳：`Validate` 的四个消费点（`cmd_compile.go` 严格模式、`cmd_new.go` 的渲染门、`cmd_import.go` 的产出门、`conversation/change_bridge.go` 的 fail-closed 桥）都把非空结果当致命，把"只警告"的规则放进 `Validate` 就等于让所有未声明 window/approval/batches 的存量工作流在编译期与会话桥同时被拒——那是行为破坏而不是补门禁。改为**并列入口** `Advise(wf)`：只返回目录判为 warning 的码，`Validate` 的产出一个字不动，"提示永不阻断"因此是结构性质而不是四处要各自记住的过滤纪律；severity 由 `errors.Lookup` 现读，本包不抄码表。**发出内容**：缺 `window`（LE095，未声明合法但意味着可以在周日 03:00 或事故中执行）、缺 `approval`（LE094，档位只剩 plan 的 risk 下限在决定）、缺 `batches`（LE096，单批全量恰与金丝雀相反）、percent 首批 >5%（LE033；仅 percent 策略——fixed 的批次是台数，机群规模要等 plan 解析 target 才可知）。**四个消费点各自的去向**：`compile` 逐条打 stderr 且摘要/`--json` 带 `advisories=N`；`new` 以 `advisory: template <名>: …` 打 stderr、run 照建、退出码不变；`import` 只写 stderr（stdout 那份 YAML 是交付物，提示进不去）；会话桥把提示拼进**创建成功**的回复，让审批人看到自己批的草案跑在哪些缺省上。**LE052 刻意不发出**：它的补救指向 post_batch 门禁路由缺陷（`convertGate` 只填 `GateSpec.Post`，`Batch` 槽无人填、`PhasePostBatch` 永不物化），叫操作者加一个要么写不进要么不会跑的检查与"错误文案承诺的自救配置不生效"是同一种错，先修路由。**顺带修掉三处测试夹具的表达能力**：`captureStdout` 把 cobra 的 stderr 并进同一个管道，"stdout 只有文档"这条断言在它里面根本写不出来——新增 `captureStreams`/`runSplit` 双流捕获，`compile --ir`、`new --json`、`import` 三条用例改用它，其中 `TestCompileEmitIR` 现在正面断言 IR 文档不含提示文字。

验证：新增 16 个测试函数（`internal/dsl/compile_warnings_test.go` 9 + `cmd/levee/cli_advisory_wiring_test.go` 5 + `internal/conversation/change_bridge_test.go` 2），另有 `cmd_compile_test.go` 2 条改造为双流捕获。**12 项变异全部由断言抓住**：compile 删掉 emit 调用、把提示打到 stdout、严格模式把提示当致命、摘要计数与 stderr 行数脱钩；`new` / `import` 各删自己的 emit 调用；会话桥恒不提示、恒提示（后者由"全声明的草案不许出现提示段落"抓住）；金丝雀规则恒不触发、金丝雀规则不再只作用于 percent；LE095 一条被抑制；`IsCompileWarning` 退回手抄四条码表。**最后一条第一轮是漏判的**——本包四个码常量与手抄清单恰好同集合，"由目录表拼装"的断言必须用一个目录里是 warning 而本包不发的码才测得出来，故补 `IsCompileWarning("LE052") == true`（`TestLE052IsDeliberatelyNotProduced` 只断言不发出，两者不冲突），复跑转红。结构不变量另有两条：`TestValidateAndAdviseNeverAgreeOnSeverity`（同一文档不得两侧报同一 `(码, 字段)`，且致命侧不得出现 warning 码）、`TestAdviseNeverBlocksWhatValidateAccepts`（同一文档 `Validate` 必须为空）。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / docgen 见提交说明。
- **组织级冻结期第一次真的会拦住变更：日历门禁接到 plan 与 apply，并且建得进 PostgreSQL**：`internal/calendar` 自建成起就是一套"库齐全、产品不可用"的子系统——`CalendarService` 全仓只有 `cmd/levee/cmd_calendar.go` 一个构造点，`IsFrozen` / `AssertNotFrozen` / `CheckWindowForPlan` 的**生产调用点计数为 0**（其余命中全在包内与测试），也就是说 `levee calendar freeze` 写下的行没有任何执行路径会去读。落地内容：**两个执法点**都在动手之前——plan 走那条"所有计划必经的漏斗"（gRPC `PlanChange`、`levee plan --local`、apply 内的 re-plan 同源），被拒不留 `plan_json`/`plan_hash`；apply 挂在既有 host guard 上（与清单主机 frozen 同一处），专门接住"批准之后才新起的冻结"。`emergency` 覆盖**只认作者在 workflow 里声明的 `approval.level: emergency`**，由 risk 分数抬出来的派生档位不构成授权（否则高分变更自己解锁绕过冻结）；**回滚豁免**，与窗口同口径，由 `TestCalendarGateIsForwardOnly` 扫调用点断言"plan.go 1 处 + run.go 1 处、rollbackChange 体内 0 处"。命中词汇表定成一句话说清：主机名 ∪ 清单分组名 ∪ 标签 `key=value` ∪ 标签裸 `value`，**裸标签名刻意排除**（每台主机都有 `env`，接受 `--targets env` 等于一条命令冻结整个机群）。**方言**：原先只有一份 SQLite SQL 却接受任意 `*sql.DB`，所以集群形态下日历根本建不出来（`DATETIME` 在 PG 报 `type "datetime" does not exist`）；改为 `calendar.Dialect` 显式参数且**零值不可用**（猜错比拒接安静），PG 侧用 `TIMESTAMPTZ` + `$n` 占位符，两条 DDL 的列集合与索引由 `TestBothDialectsDefineTheSameTable` 钉住不再漂移（cluster_nodes 已经因两份 DDL 漂移吃过一次），并在**活 PostgreSQL** 上跑通全量 CRUD——占位符编号写错会把值绑进另一列且照样"成功"，只有真库测得出来。三处与实现不符的说法一并修掉：`AssertNotFrozen` 注释承诺"列出违规窗口"而实现只列 target（现在列 id/名字/解除时间，并新增 `AssertNotFrozenAt` 使这条可测）、`CheckWindowForPlan` 自称"plan 阶段提示冲突"（现在真作 advisory 消费，读失败只 WARN）、`wiring/plan.go` 头注释里"frozen hosts 由上游拒绝"其实指**清单主机 status=frozen**（另一套确有强制的机制，同名不同物），已在 spec 新增 §4.2b 用表格分清三种"窗口/frozen"。**仍未收口的一半**：CLI 只能连本机 SQLite（`DatabaseConfig` 只有 `driver`(仅 sqlite)/`path`、`openStore` 恒 SQLite、`--pg-dsn` 是 serve 旗标），所以 `--cluster --pg-dsn` 形态下 CLI 写的冻结期服务端读不到；前置是给配置加 `database.dsn` 并放开 `config.Validate` 的"must be sqlite (MVP)"，那一刀影响所有 CLI 命令的取库路径，不混进本批。（**同日已收口**：`database.dsn` 落地、`openStoreFromConfig` 成为唯一取库点，两头出声的两条也改成如实描述——见本页上方"CLI 与 serve 可以指向两个数据库"一条。同批还查出并修掉本条当时没测到的另一处失效：**开启 `tenant.enabled` 会让这里的冻结门禁整套不生效**，因为 `calendarFor` 的类型开关匹配到的是多租户装饰器而不是底层库。）

验证：新增 20 个测试函数（`internal/calendar` 8 = `dialect_test.go` 6 + 打**活 PostgreSQL** 的 `pg_dialect_test.go` 2；`internal/wiring` 12，其中含 gRPC 码断言 1 条）。**11 项变异全部由断言抓住且均能编译**：删任一侧门禁调用、把覆盖授予任意 high 档、去掉裸 value 匹配、加上裸 key 匹配、PG 延用 `?` 占位符、接受未声明方言、从 PG DDL 删一列、拒绝信息不再列窗口名、把重叠告警改成拒绝、丢掉 sentinel 包装。**第 12 条变异是漏判的，随后把被变异的那段代码删了**：给"冲突查询读失败"写了个 `return err` 当作失败关闭，但它上面那次冻结读同一个库、已经先拒了，这条分支不可达——不可达分支伪装成门禁比没有门禁更误导读者，改为 WARN 并在注释里写明为何不拒绝。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / docgen 见提交说明。


### 已知限制（本次实测确认，登记不修）

- **`levee agent list/show/remove` 读的是进程内注册表**：`AgentRegistry` 既不落盘也没有对应的 master 端 RPC（`proto/` 内无任何 agent 服务定义），因此跨进程执行 `agent list` 恒返回空表、`show`/`remove` 恒 not-found。`cli-reference.md` 第 20 章已加醒目边界说明。接 master 端 Agent 服务需要改 proto，而 CI 用 protoc 27.0 钉住 `internal/grpc/pb/` 并 `git diff --exit-code` 校验，本机 protoc 是 36.2，无法在本地生成一致的产物。
- **`--deep-link` 的移动端消费未验**：CLI 侧 payload 键与 `internal/approval/mobile.go` 一致，但没有真实移动客户端可验证点击行为，验证面到"发出的 APNs/FCM 报文含该键"为止。

### 修复

- **`human` 门禁从"编译通过、运行期中止"改为 plan 期拒绝，并顺手查出批级门禁从不执行**：`verify.HumanApprover` 抽象、`HumanGate`（超时、fail-closed、参数校验）都齐备，但**全仓实现数为 0**，`internal/wiring` 也从不安装它——于是任何声明了 `human` 检查的 workflow 都能编译、能批准，然后在 `RunPhase` 物化门禁那一刻失败；由于声明常在 post-apply/post-batch，**前面几批已经改完目标机**。现在 `GeneratePlan`（唯一 plan 漏斗）用 `engine.PlanGateBlockers(p, e.gateRuntime())` 在任何改动发生前拒绝，哨兵 `engine.ErrGateNotExecutable` 由 gRPC 层映射为 `FailedPrecondition`（不再是 `Internal`）；新增 `wiring.WithGateApprover` 作为唯一接入口，接上传输后同一个 workflow 就能出计划——**所以这不是禁用 `human`，是对部署能力的陈述**。物化侧与 plan 侧共用同一个判定函数（`executabilityProblem`），两处不可能再各说一套。
  顺着这条线查出两条更深的缺陷，本批**如实记录并留成特征测试，不擅自打开**：① **`batches: gate:` 从来没被物化**——`walkPlanGates` 只遍历 `Batches[].Steps[].Gate`，而生成器把批级门禁放进 `plan.Batch.Gate`，该字段除 plan 哈希外**零读取点**，即"进了被批准的产物、一次都不执行"，正是 `docs/gates.md` 承诺不可能发生的"声明了的门禁假装通过"；仓库自带 `examples/gate-templates/redis.yaml` 注释宣称该批间门禁"blocks the next batch on failure"，实际挡住的东西为零。② **`slo` 门禁今天无法声明出来**——`convertGate` 把所有形状（step `verify:`、`batches.gate:`、`gates[].position: post_batch`）一律塞进 `GateSpec.Post`，而只有 `GateSpec.Batch` 绑定 `PhasePostBatch`，`sloGateFromCheck` 又只认那一相，因此每条 `slo` 声明都注定失败。本批把它的报错文案改成实话（说明"没有哪种 YAML 形状能落到那个槽位"），而不是给一条设了也不生效的自救建议。两条修复都会**改变现有工作流的门禁时机或让从未跑过的门禁开始拦批次**，属需要迁移说明的行为变更，已登记 roadmap。
  验证：新增 16 例（`internal/engine` 7 + `internal/wiring` 9），含"拒／接"成对（无 approver 拒、装了 approver 同一 workflow 通过）、拒绝不落 plan 产物（`run.PlanJSON` 仍空，批准不会绑到一个永远执行不了的计划）、RPC 码映射成对、结构测试两条（`PlanGateBlockers` 生产调用点恰好 1 处且 `rollbackChange` 不经过它——回滚必须始终可执行；`engine.GateRuntime{}` 字面量全仓恰好 1 处，防判定与执行再分叉）、以及上面那条三段式特征测试。**14 项变异全部被抓**（删掉 plan 期拒绝、拒绝却仍交出产物、wiring 忘记带 approver、判定改成"human 恒拒"、丢掉 slo 的相位规则、丢掉 slo 的端点规则、遍历丢掉 batch 槽、丢掉 post 槽、pre 绑错相位、门禁改名、物化侧不再咨询判定、gRPC 漏映射新哨兵、冒出第二个 `GateRuntime` 字面量、回滚开始咨询该拒绝）。每项都先确认 `go build ./...` 通过才认定是语义变异。

### 修复（模板链路）

- **`levee new <模板>` 产出的 run 从来无法 plan：渲染后的工作流没有落库**：`cmd/levee/cmd_new.go` 把 `result.TemplateName` 写进 `run.WorkflowName`，而这个字段的契约是**工作流源**——`wiring.resolveWorkflow`（`internal/wiring/plan.go:148`）按内联 YAML 解析它、解析不动才当文件路径读，`internal/grpc/template_service.go:334` 与 `change create` 都按这个契约写。后果是模板路径产出的每条 run 在 plan 期报 `LE001: cannot unmarshal !!str 'probe' into dsl.yamlWorkflowRaw`——拿模板名当工作流解析。实测复现（临时数据目录 + 构建出的二进制，`template create` → `new` → `plan`）与修复后同序列均已跑通：`plan` 现在输出 `Plan for run …: 1 batch(es)` 并落 `plan_json`/`plan_hash`。
  同时补上**落库前的两道门**（与 `internal/conversation/change_bridge.go` 的 fail-closed 同一标准，即 `levee compile --strict` 的解析 + 结构校验），并额外拒绝残留的 `{{.参数}}` 占位符——`internal/template/instantiate.go` 自己写明"可选且无默认值的参数，占位符原样留在 content 里"，而这类文本能解析、能过校验，最终以字面模板语法进入下发给目标机的命令。三条门任一不过就 `exit 2` 且**一条 run 都不建**（断言 store 里 `ListRuns` 为空），因为一条"看着是 draft、plan 不动"的记录比一条报错更难发现。新增 `template.UnsubstitutedPlaceholders`：按本包真实替换的语法匹配（要求前导 `.`，因此 `awk '{{print $1}}'` 这类 shell 花括号不算参数），并且**跳过整行注释**——第一版没跳，被自己的示例文件绊住（模板头注释里写了占位符用法），一条会误报的门禁比没有门禁更糟，已补两条用例把"注释不算漏替换"与"`#` 在引号里是数据、不能拿它当截断点"钉住。
- **CLI 人类输出把整份 YAML 印进一行表格**：`run.WorkflowName` 存的是源，而 `list` / `show` / `audit report` / `target history` 直接把它当"工作流名"打印——多行文本会打散 tabwriter 的每一列（`target history` 那列还写死 `%-20s`）。新增 `workflowDisplay`：内联文档显示其**声明的 `name:`**，单行源（路径、裸名）与解析不了的源原样显示，不做猜测。实测修复后 `list` 该行显示 `patch-rolling` 而不是整份文档；`show --json` 仍返回完整 run（含源），信息没有丢。
- **仓库自带的模板示例没有任何代码路径能读**：`examples/templates/patch-rolling.yaml` 是 `workflow:` 信封 + `params:` 块的旧方言，而 `TemplateLibrary` 只认 `<name>.json` 并用 `json.Unmarshal` 解码（`internal/template/library.go:367-386`），`template create` 也从不解析 `--content` 里的 `params:`——即那份文件既不是可加载的记录、也不是可直接使用的工作流正文。已按现行方言重写为纯工作流正文（含 `{{.package}}` / `{{.target_group}}` 与一份 `target import` 可配的清单说明），它因此**自动进入 `TestShippedExamplesCompile` 的覆盖面**（此前被"模板信封"分类规则跳过；现该跳过只剩 `plugins/` 两项，覆盖数 5→6），示例与文档一起实测通过。
- **`docs/quickstart.md` 否认 `levee plan` 存在**：原文写"LEVEE 没有 `plan` 子命令，也没有独立的 dry-run 步骤，生命周期为 new → approve → apply"。实际 `plan` 已注册（`levee --help` 第 38 行）且是链路的必经一步：实测未 plan 的 run，`approve` 报 `no pending approval found`、`apply` 报 `is in "draft" state, cannot apply`，而 `plan` 会发起审批链并落哈希绑定的产物。已改为 `new → plan → approve → apply` 并给出 `plan --targets` 示例；同页那份旧方言 `template create` 示例（`action: shell` + `command:`，无 `target:`）一并换成现行形状——旧写法今天会被上面那道门拒绝。README 快速开始同批订正（它的示例也用了 `action: shell`）。

验证：新增 8 个测试函数（`cmd/levee/cli_template_new_test.go` 6 + `internal/template/placeholder_gate_test.go` 2，后者表驱动 8 形），全部走真实 CLI 与真实 SQLite/模板库目录，不经替身。**8 项变异全部被抓且均为断言失败**（M1 退回写模板名、M2 关掉占位符门、M3 吞掉解析失败、M4 关掉校验门、M5 整道门不调用、M6 不再跳过注释、M7 去掉前导 `.` 要求、M8 `workflowDisplay` 退回原样打印）。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / docgen 见提交说明。


### 收敛（审批档位词表）

- **同一份"三个审批档位"在仓库里有八处表述，服务侧占七处**：`dsl` 侧三份（parser / validator / typechecker）早已收成 `dsl.ApprovalLevels`，剩下的是 `internal/approval/service.go:219` 的 `validLevel` switch（用的是字面量，连本包自己的 `LevelStandard/High/Emergency` 常量都没用）、四条 `(allowed: standard, high, emergency)` 报错文案（`service.go:292`、`templates.go:221`、`levels.go:216`、`levels.go:270`）、`levels.go:226` `All()` 里逐名列举的三行，以及 `internal/executor/irreversible.go:188` 又自己定义的 `const ApprovalLevelHigh = "high"`。今天值都一样所以没出事——这与 `runstatus`、`batches.strategy` 两轮之前的形状完全同型：一次档位改名会在其中一处留下不一致的判定或文案。现在 `internal/approval` 的档位常量是 `dsl` 常量的别名、`levelOrder` 由 `dsl.ApprovalLevels` 生成、`IsLevel` 委托 `dsl.IsApprovalLevel`（并删除 `validLevel`）、四处报错由 `LevelNamesJoined()` 拼装、`All()` 与 executor 常量都跟着表走。
  **判定面必须跟着改，否则"收表"只是换个写法**：等值断言（"文案 == 表渲染出来的样子"）抓不住"手抄一份恰好相同的字符串"这种实现——所以 `TestLevelTableIsTheOnlyDefinition` 在运行时往表里加一个 `beta-tier`，要求 `Service.Create`、`TemplateLibrary.RegisterTemplate`、`LevelManager.Get`、`LevelManager.SetConfig` 的拒绝文案与 `All()` 的长度**全都跟着动**。`TestLevelVocabularyHasOneSource` 另钉两件事：`LevelNames()` 等于 `dsl.ApprovalLevels` 且交出的是副本（调用方改它改不到词表本身），以及"每个声明的档位都必须有默认配置"——在 `dsl` 加第四档而 `NewLevelManager` 不补，今天会以 `All()` 里的**零值 `LevelConfig`** 静默出现（0 审批人、0 超时），而 `Get()` 又对它报 `ErrInvalidLevel`。executor 那条是边界断言而非同义反复：建议的档位必须是审批服务接受的档位，且必须是 high——换成 emergency 会把"两人签"变成"一人 30 分钟"。
- **同批查出并如实订正：`SuggestLevel` 零生产读取点**。`irreversible.go` 头部原写"不可逆步骤必须在 high 档审批……checker 交出 SuggestLevel 供 planner 升档"与"irreversible 步骤被排除在自动回滚之外"。实测：plan 生成器只读 `Irreversible` 与 `IrreversibleReason`（`generator.go:430`），升档实际来自 risk 分数段的 `ApprovalFloor`（`wiring/plan.go:119` 把每条步骤计入 `pointsIrreversibleStep` → `risk.go:278-283` 推 floor，再取 `max(声明, floor)`）；回滚侧 plan 携带的就是作者声明的步骤级 `rollback`（`generator.go:427`），没有"按 irreversible 排除"这条判定。`SuggestLevel` 全仓唯一比较它的地方是它自己的测试。两处说法已按实测改写，字段保留（边界测试是接它的前提），并登记 roadmap。

验证：**12 项变异全部被抓且均为断言失败**（`IsLevel` 恒真、表里删一档、`LevelNames` 交出共享切片、`LevelNamesJoined` 手抄、`All()` 逐名列举、四处报错文案各自手抄、`Create`/`RegisterTemplate` 两处判定被跳过、executor 常量指向错误档位）。其中两条第一版**编不过**（`strings` / `dsl` 变成未使用导入）——改成同形可编译的变异后才认定，未编译的变异不作证据。另有一处自伤如实记录：第一个提交漏了 `benchmark_test.go:156` 的 `validLevel` 调用点，那条提交单独 checkout 时 `go vet ./...` 会红，由后续提交补齐（未重写已提交历史）。`go build ./...` / `go vet ./...` / `go test ./...` / golangci-lint / docgen / proto 漂移门见提交说明。

### 变更（审计链随写封链与分批遍历）

- **审计链改为按游标分批遍历（`audit: walkChain`）**：`AuditChainBuilder.Seal` / `Verify` 不再"一次读全表 + Go 内排序"，改为经新增的 `state.Store.ListAuditChainPage` 按 **keyset 游标**逐页推进，内存占用从 O(审计行数) 降为 O(页大小)（默认 1000 行），遍历期只保留滚动的前驱哈希。

  为什么不是 OFFSET 分页：`ListAudits` 是**降序**（REST 日志接口依赖），而链必须**升序**推进（每行哈希覆盖前一行）；且 `Record` 每次审计写入都会封链，分页期间必然有并发写入——OFFSET 分页下新行插在队首会把后续 offset 整体前移，导致已处理的行被重复读取、最旧的行永远读不到，而对哈希链而言"重复读到一行"等于**拿一个已不再是其前驱的哈希去封它**，直接写坏链。游标分页只会向前推进：新增行落进下一页，落后于游标的回填行留待下一次封链补上，两者都不会损坏链（后者表现为 `Verify` 报 `Unsealed`，是可见的缺口而非静默通过）。

  两个后端 + 租户层同步实现：`SQLiteStore` / `PGStore` 的链页查询用 `ORDER BY timestamp ASC, id ASC` 把 tie-breaker 钉进 SQL（两个后端原本都只有 `ORDER BY timestamp DESC`，同时间戳行顺序不稳定，而同时间戳恰是链序最不能含糊的地方）；`TenantStore` 强制以上下文解析出的租户限定页面并**忽略调用方传入的 tenantID**，无租户上下文时 fail-closed。

  **如实标注的边界**：分批解决的是内存，不是扫描量——封链仍需遍历全部可见行，而 `Record` 每次写入都调用一次，因此超长审计库每次审计写入仍付一次全表扫描。改为增量封链（只封已封尾巴之后的行）需要先能区分"链中空洞"与"新增尾巴"（例如对未封行建部分索引），这会改变"断链"的既有语义，故不在本版夹带。

  测试：新增 `internal/audit/auditchain_paging_test.go`（页大小 2 强制多次跨界：跨页封链后校验、跨页篡改只报一行不级联、同时间戳跨页、**遍历途中并发插入新行链仍自洽**、回填时间戳的行延后而非损坏）；`internal/state/audit_chain_page_test.go` 与 `pgstore_auditchain_test.go` 新增对等的后端用例（PG 侧实测：微秒精度保留、同秒 tie 按 id）；`internal/tenant/audit_chain_page_test.go` 新增租户隔离用例（含调用方传 tenantID 不能放宽作用域、无租户上下文报错）。

- **trace 哈希链改为「只延展、不重写」的随写封链**：v1.14.0 起 trace 链只在**终态收口点**封一次链（`audit.SealRunTraceChain` → `Build`）。这有两个可量化的后果，本版一并消除。① **执行中的 run 无法验证**：`VerifyHashChain` 把 `empty_hash` 计为失败，而 run 结链前每一行 trace 的 `CurrHash` 都是空，于是任何进行中的 run 都被报成 `valid=false` + `brokenEntryId`——响应里与真实篡改无法区分。② **结链后到达的 trace 永久报假警**：`Build` 对已存在的链一律拒绝（`ErrChainAlreadyBuilt`），一条晚到的 trace（终态收口与最后一条 trace 之间的竞态、或异步收尾写入）此后永远打不开链，`/audit/verify` 恒报篡改。

  新增 `HashChainBuilder.Seal`：按存储顺序遍历、只重算不重写——**`CurrHash` 为空**则写入并延展（新增行、或上次封链失败留下的空洞）；**已封但不匹配**则**不写**并返回 `ErrChainBroken`。`audit.TraceRecorder.Record` 在每次追加后调用它，封链失败只 `log.Warn` 不影响 trace 写入（与 `audit.Record` 同款处理），未封行在下次验证时以 `empty_hash` 可见、下次封链补上。于是：进行中的 run 随写随封可验证、晚到的 trace 延展链而非弄坏它、并发封链安全（哈希由存储内容派生，重叠遍历对共享行算出同值）。

  **SA-002 在 trace 侧由此收敛**：trace 链的封链路径永不重写已封行，"先篡改再封链"销毁证据的场景在 trace 上不成立——封链返回 `ErrChainBroken`、验证继续报出被改的那一行；管理侧显式恢复入口仍是 `BuildForce`（`TestSeal_DoesNotRepairATamperedChain`）。

  **兼容性**：`Build` / `BuildBatch` / `BuildForce` 的签名与语义一律不动（仍是对"无链存量行"的一次性建链入口），`VerifyResult` 与 proto/REST 响应无变化。代价是每次追加遍历该 run 的全部 trace，run 内 O(n²)——MVP 量级下 n 是"每 step/gate/审批一行"，已在代码注释中记录。

- **新增 `state.Store.UpdateTraceChain`（只写链列）**：trace 侧此前用整行 `UpdateTrace` 写链（WORM 触发器按列值比较，整行写能过），使封链成为全代码库唯一能同时触碰每个内容列的操作，其安全性取决于值恰好相同。现在有专门的链列原语，与 `UpdateAuditChain` 对称，写入面在 SQL 层面可证。PG 侧由 `TestPG_UpdateTraceChain_ChainColumnsOnly` 对真实 plpgsql 触发器实跑：链列可写（含重复盖章）、内容列仍被拒、未知 id 报错而非静默成功。

### 新增

- **`ai.llm` 配置键接入默认值 / 环境变量 / 校验（`internal/config`）**：`LLMConfig` 结构体与 `config.example.yaml` 的文档早已存在，`cmd_converse.go` 也确实消费它，但配套三件套全缺——8 个键不在 `allKeys()` 里（`bindEnv` 不绑定 → `LEVEE_AI_LLM_*` 环境变量对 `viper.Unmarshal` 完全不可见，只有 YAML 文件能配，而容器化部署正是只给环境变量的）、无 `SetDefault`、`Validate` 不校验。现补齐：`provider` 默认 `openai`（**这一条是有语义的**：`recommend.NewLLMClient` 把空 provider 当作 `mock` 并返回罐头客户端，运维只开 `enabled` 不配 provider 就会静默拿到**编造的推荐结果**且无任何告警）、`timeout` 默认 30s、`max_tokens`/`temperature` 保持 0（交由客户端内置默认，与文档口径一致）；`enabled` 时校验 provider ∈ {openai, ollama}（**拒绝 `mock`**：那是测试专用值，误配会让服务一边返回假数据一边报告 LLM 健康）、`timeout > 0`、`temperature ∈ [0,1]`；关闭状态下零值不参与校验（保持"默认关闭、既有部署逐字节不变"）。测试见 `internal/config/llm_config_test.go`（默认值 / 环境变量覆盖 / 文件配置 / provider 拒绝 / 关闭态放行 / 数值边界 / `allKeys` 防漂移）。

### 修复

- **同时间戳的 trace 会被随机 id 排到错误位置，把在跑的 run 报成篡改**：trace 链按 `(timestamp, id)` 升序遍历，而 `Record` 每追加一行就封一次链——新行若因时间戳相同而按 id 排在已封行之前，就把那行挤到了它哈希所依据的前驱之外，下一次封链因此返回 `ErrChainBroken`，`/audit/verify` 从此对该 run 恒报篡改（真实成因不是攻击，是时钟粒度）。本机 SQLite 实测 2026-10-05：连续 84 个「三条记录」循环中 5 个的链被 `Verify` 判坏。修法是把 id 变成可排序的：`audit.newID` 现在用 8 字节大端 unix 纳秒前缀 + 8 字节随机后缀，前缀额外强制严格递增（时钟没走时也要大于上一个），于是同时间戳行的 tie-breaker 就是写入顺序，追加只能延展链尾。`Seal` 的「已封不匹配则不重写」规则一字不动——把它放宽成重写会给「改内容后重新接链」留出一条洗白路径。残留面按代码注释登记：墙钟回拨（行时间戳倒退但 id 不倒退）、以及旧二进制留下的随机 id 存量行，两者仍能让晚到的追加换位，处置入口是 `BuildForce`。测试：`internal/audit/trace_chain_order_test.go` 三例——`TestNewIDSortsInCreationOrder`（2000 个 id 严格递增且不重）、`TestSeal_AppendNeverRepositionsSealedRow`（钉死同一时间戳，逐行插入+封链，任何换位都会让 `Seal` 报错；这一例不依赖真实时钟，因此在 ns 粒度时钟的 CI 上同样真跑到 tie-breaker）、`TestSeal_ProductionPathStaysVerifiableAcrossManyAppends`（走 `Record` 生产路径连追 60 行）。变异验证：把 `newID` 还原为纯随机后三例全红（确定性那例停在 `seal 2 must extend the chain`），修复前 `TestVerifyHashChain_RecordedRunVerifies` 在本机 20 次里间歇性红、修复后 40/40 绿。

- **PG 测试在复用数据库上不再假失败**：`newPGTestStore` 刻意不清理 `run_assignment` 与 `cluster_nodes`（这两张表归 dispatch / cluster 包并发持有，CI 亦按包分步串行执行以共用同一个库），但三个用例自身的假设没跟上——`TestClusterNodes_PGListsNodes` 用裸 INSERT 写固定 id（重跑撞唯一键）、`TestAssignmentSummary_PGWithRows` 对全表聚合断言精确计数（他包残留行即失配）、`TestPGStore_ReclaimAssignmentCAS` 用 `CreateAssignment` 写固定 run_id（残留行撞主键）。CI 每次是全新数据库所以从未暴露，本地对着常驻容器复跑必红。现按该文件既有的"只断言存在性 / 只碰自己的键"模式补齐：`cluster_nodes` 改为照抄 cluster 包 `RegisterNode` 的 `ON CONFLICT (id) DO UPDATE`、聚合断言改为相对封链前基线的增量、两个用例在播种前删除自己那把键。实测：常驻共享库连跑三次全绿；新库上 `./internal/state/... ./internal/cluster/... ./internal/takeover/... ./internal/backup/...` 全部通过（与 CI 序列一致）。

### 修复（交付物版本口径与一致性门禁）

- **chart 引用的版本从未发布过**：`CHANGELOG.md` 写有 `## [v1.19.0] - 2026-10-04` 小节、`docs/release-notes/v1.19.0.md` 已存在、`Chart.yaml` 的 appVersion 也升到了 1.19.0，但远端最新 tag 是 `v1.18.0`，`v1.19.0` 从未打过；镜像只在 `release.yml` 的 `push: tags: v*` 时构建，因此 `ghcr.io/levango7/levee:1.19.0` 这个制品并不存在。同一个 chart 里还自相矛盾——`appVersion: "1.19.0"` 而 `values.yaml image.tag: "1.18.0"`，即"声明 1.19.0、部署 1.18.0"。本批把 appVersion 对齐到**已发布**的 1.18.0，并在下方 v1.19.0 小节显式标注"未切版"（内容已在 master，制品不存在）。
- **新增硬门禁 `scripts/check_release_versions.py`**（由 `validate_delivery.sh` 在 helm 检查之前调用；delivery 作业的 checkout 改为 `fetch-depth: 0` + `fetch-tags: true` 才能拿到 tag 集合）：① `appVersion` 必须等于 `image.tag`；② chart 引用的版本必须已有 `v<version>` tag，否则等于把部署指向一个从未构建的镜像；③ CHANGELOG 里比最新 tag 更晚的 `## [vX.Y.Z]` 小节要么已打 tag、要么写明"未切版"（比最新 tag 更早的无 tag 历史小节不阻断，但会被逐条点名，不静默）；④ 一个 `v*` tag 都拿不到时**直接失败**——静默的空 tag 列表会让前三条全部假通过。
- **判据取远端而不是本地 tag**（第一版就被自己的环境教育了）：本地克隆残留了 `v1.0.0`–`v1.9.0`，而 origin 上只有 `v1.10.0`+，于是本地跑"通过"、CI 跑"失败"。脚本现在优先 `git ls-remote --tags origin`，取不到远端才退回本地集合并打印来源警告；顺带查出并如实登记：CHANGELOG 的 v1.7.0 小节在远端无对应 tag（历史追溯，不阻断交付）。
- **验证**：基线通过（20 个 tag，chart 与 image.tag 均为 1.18.0）；变异四条分别变红——appVersion 改回 1.19.0、`image.tag` 改成未发布的 9.9.9、删掉 v1.19.0 的"未切版"标注、在无 tag 仓库里运行（触发规则④）。`bash -n` 通过；用 `HELM_BIN=/nonexistent-helm` 跑 `validate_delivery.sh` 证明版本检查确实在链路上、且先于 helm 检查执行（版本被改坏时它在 helm 之前就失败退出）。

## [v1.18.0] - 2026-10-03 — 交付物齐备：Helm chart + 裸金属一键安装 + 跨区域场景方案 + 镜像发布

本版把"能上线、能交付"所需的**交付物**补齐：K8s 客户有了一配置即部署的 Helm chart（单机/集群两形态，`helm lint`/双形态 `template` 全文档 YAML 校验通过）；裸金属客户有了幂等一键安装脚本与自带 systemd 单元（容器实测）；每个 `v*` tag 现在还发布多架构容器镜像到 GHCR（独立 job，镜像失败不阻塞二进制发布）；跨区域数据同步与运维的**场景方案文档**及其配套示例 workflow 落库（示例进 `TestShippedExamplesCompile`，实测编译通过）；部署手册新增 Helm 与交付检查单两节。同时做了一致性订正：spec 的 `allow_irreversible` 字段如实标注"规范目标未接线"（LE082 零产生点登记 roadmap），security-audit 的三处历史边界留痕订正（trace 链接线关闭、PG 触发器 CI 实跑）。

### 新增

- **Helm chart（`deploy/helm/levee`）**：`mode=single`（SQLite + PVC）与 `mode=cluster`（master 1 副本 + worker N 副本共享 PG；`--node-id/--node-addr/--node-role` 经 Downward API 注入，worker 快照目录可指共享 RWX 卷）两种形态；认证/主密码/PG DSN 全部走 Secret（`auth.existingSecret` 可完全接管）；探针用网关 `/healthz`（就绪——服务未注册返回 503）与 TCP（存活，避免配置错误触发重启风暴）；内嵌演示 PG 明确标注"非生产"；可选 ingress 与 `helm test`。**如实写进模板注释的边界**：Service 只指向 master 副本（`WatchChange` 事件总线是进程内的，多副本前置会让订阅者漏事件）；GHCR 镜像默认私有，需要 imagePullSecrets 或同步到客户镜像库。验证：`helm lint` 0 failed；集群全开（cluster+内嵌 PG+ingress+命名令牌+反亲和）渲染 12 份文档、单机 6 份，全部经 YAML 解析校验。
- **裸金属一键安装（`deploy/baremetal/install.sh` + `deploy/systemd/levee.service`）**：安装脚本幂等（容器实测：两次执行均成功），建系统用户、装二进制、按 `--prefix/--data-dir` sed 渲染单元路径、生成 0600 root:levee 的凭据模板（不覆盖既有文件）；systemd 单元含最小权限加固（`ProtectSystem=strict`、`ProtectHome`、`ReadWritePaths` 限定数据目录、`$LEVEE_SERVE_EXTRA` 按空格展开集群参数，未设置时展开为空不报错）。
- **容器镜像发布（release.yml 新增独立 `image` job）**：每个 `v*` tag 用 buildx（QEMU）构建 linux/amd64+arm64 并推送 `ghcr.io/levango7/levee:<版本>` 与 `:latest`；全部动作按 commit SHA 钉死；**与 goreleaser 解耦**——registry 故障或 buildx 失败不会阻塞二进制/归档发布（run 变红保持可见性）。镜像基于仓库 Dockerfile（node→go→scratch 资产→alpine 运行时、非 root、`/healthz` HEALTHCHECK）。
- **场景方案与配套示例**：`docs/scenario-cross-region-ops.md`（中大型集团 1 核心机房 + 4~5 区域数据中心：痛点→已接线能力映射、参考部署形态、试点演练表、错位竞争定位、诚实边界——含数据搬运不是 LEVEE 的能力、`allow_irreversible` 未接线两点）；`examples/workflows/dr-switch-orders-db.yaml`（跨区域 MySQL 主从切换：`one-per-target` 严格逐台串行、区域时区窗口、高危审批、`confirm=yes` 强制确认门、切换后 verify 门禁；实测 `levee compile` 通过并纳入 `TestShippedExamplesCompile` 永久守护）。
- **部署手册扩充（`docs/deployment.md`）**：§3 补发行版与镜像说明；§6 指向自带单元与安装脚本；新增 **§14 Kubernetes（Helm）部署**（两形态安装命令、Secret 边界、模板注释里的四条边界）与 **§15 交付检查单**（8 项：命名令牌/OIDC、gRPC 不进公网、主密码 secret 注入、doctor 全绿、试点演练、备份恢复演练、升级路径、审计链校验——前四条绑定 `security-audit.md` 的生产准入约束）。
- **`deploy/README.md`**：三条交付路径索引（裸金属 / Helm / 容器）、镜像来源与离线同步、模板与真实旗标的一致性核对方法、部署前必读安全约束。

### 修复

- **一致性订正三处（文档 vs 实现）**：① `leveelang-spec.md` 的 `allow_irreversible` 字段行改为如实口径——解析器无此字段、LE082 全仓零产生点（仅错误目录与故障分类器引用）；今天的不可逆保护是两层真实机制（`mysql.replica_switch` 内 `confirm=yes` 运行时硬门 + 计划侧 `irreversible: true` 高危审批路由），接线项（编译期门禁会改变存量行为，需带迁移说明）登记为 roadmap P1；② `security-audit.md` 两处历史边界留痕订正——「trace 链生产零接线」于 v1.14.0（PR #29）关闭、「PG 触发器未执行验证」已由 `pgstore_auditchain_test.go` 在 CI postgres 腿实跑，原记录保留、更新以日期标注；③ 风险评级行补 2026-10-03 更新（并指向交付检查单）。

## [v1.17.0] - 2026-10-03 — OpsMesh 拉取方向接线（按平台源码核实契约）

v1.16.0 留下的最后一个跨系统问题——「OpsMesh 平台侧的服务寻址语义待定案」——被**直接读平台源码解决**（F:\Nexus\OpsMesh）：设计文档 §6.3 的 service 寻址只是草案，真实端点是目录图 `/api/v1/catalog/topology?tenantID=` 与 PromQL 代理 `/api/v1/prometheus/query`；host→服务的匹配在客户端经 name/metadata 值包含完成，无需平台侧改动。客户端三处与真实契约不符的方法（GetTopology/GetMetrics/Ping）全部修正——旧实现会对着真实部署 404。拉取方向完整接线：诊断影响半径阶段 `provider: opsmesh` + `levee opsmesh topology|metrics` CLI；结果回传的 `resolution` 端点平台尚未实现（只有 ack/silence），404-降级与「不用 ack 顶替」的理由如实写进代码注释与配置说明。

### 修复

- **OpsMesh 集成收口：拉取方向接成真线 + 三处与平台真实契约不符的旧实现修正**。
  **① 契约核实（读平台源码，不再靠设计草案猜）**：`GET /api/v1/topology?service=` 在平台上不存在；真实端点是 `GET /api/v1/catalog/topology?tenantID=`（目录图：节点带 type/status/metadata/children，边带 relationType）、`POST /api/v1/prometheus/query {"query"}`（PromQL 透传、即时求值）、健康探针 `/healthz`。客户端三种拉取全部改到真实契约（types.go 重写为 CatalogGraph/PrometheusResponse 形状）。
  **② host→服务寻址**：目录图返回整租户的图，匹配在客户端完成——`topology.FindNode` 按 ID/Name/Endpoint/metadata 值包含匹配，主机名或 IP 都能命中；适配器刻意不猜 metadata 键名（值包含天然覆盖）。
  **③ 接线面**：诊断影响半径阶段新增 `provider: opsmesh`（`diagnosis.topology.tenant_id`；与 `opsmesh.*` 连接设置共用）；新增只读命令 `levee opsmesh topology [--tenant] [--service]`（含影响半径分析）与 `levee opsmesh metrics --query`（human + --json）。
  **④ 平台缺口的诚实记录**：结果回传的 `/resolution` 端点平台未实现（只有 `{id}/ack`/`{id}/silence`）——上报 404 记 warn、非致命；改用 ack 顶替被**否决**（不带结果载荷、压制平台升级语义，属平台侧策略），理由写进 client 注释与 config 说明。
  **⑤ status 进 finding**：OpsMesh 目录图没有流量指标，节点 status 是它唯一的健康信号——以 `name[status]` 渲染进影响半径 finding（上游/下游列表与 pivot 都带）。
  测试：客户端真实契约（三端点路径/方法/查询参数与形状）、目录图→统一拓扑转换与跨键匹配、影响半径端到端（IP 值匹配）、CLI 两子命令渲染与配置缺失指名、不健康边对无指标图正确为空。

## [v1.16.0] - 2026-10-03 — 未链入包清零 + OpsMesh 反馈闭环

本版把最后三个库就绪、产品不可用的包一次收口，未链入包 8→0：**APM 拓扑分析**接入诊断管线的影响半径阶段（SkyWalking/Pinpoint，`diagnosis.topology.*`，默认关）；**Ansible 导入**成为 CLI 命令 `levee import ansible`（纯翻译，产物先过 parse+validate 两道门，输出保证可编译）；**OpsMesh 结果回传**接线（`opsmesh.*`，告警驱动修复终态上报，拓扑/指标拉取待平台侧寻址语义定案）。逐条明细见下方「新增」。

### 新增

- **最后三个未链入包清零：APM 拓扑分析接线、Ansible 导入成为 CLI 命令（带可编译保证）、OpsMesh 结果回传接线**。逐项：
  **① `diagnosis/topology`（OpsMesh 集成设计的流程 3d）**：SkyWalking / Pinpoint 采集器接到诊断管线的**影响半径阶段**——`DiagEngineConfig.Topology`（可选 `TopologySource` 接口）作为第三个并行证据源，`ImpactRadius` 分析上下游依赖与不健康边（默认阈值 10%，≥50% 转 critical），以 `service` 类 Findings 参与严重度排序；**目标不在图谱中静默跳过**（不是每台主机都是 APM 纳管服务）、采集失败记 `report.Errors` 不致命；`diagnosis.topology.*` 配置（provider/endpoint/timeout），未知 provider 或缺 endpoint 降级 warn。
  **② `compat` → `levee import ansible <playbook>`（`--out`/`--name`）**：纯翻译、不执行、不建变更。play `name:` 映射进 `Meta.Name`——否则产物过不了解析器的 LE002，**导入器不能产出自家编译器拒绝的东西**；新增 `dsl.MarshalWorkflow`（AST → LEVEELang YAML）且**fail-closed**：发不出的非空字段（inputs/审批/门禁/回滚/快照）一律报错指名，绝不静默丢弃；args 键排序保证字节稳定可 diff；产物**先过 parse + validate 两道门**（与 `levee compile` 严格模式相同）再落盘。往返测试钉住导入→发出→重解析→校验全链。
  **③ `opsmesh` 结果回传接线**：`opsmesh.*` 配置（enabled/base_url/api_key/timeout）启用后，**告警驱动**的修复（run params 的 `alert_id`，桥接层从告警会话写入）在终态把结果 POST 到 `/api/v1/alerts/{id}/resolution`（alert_id/success/workflow_id/steps 计数/rollback_used）；**无 alert_id 不报**（平台没有可关的单，编一个会污染记录）、上报失败只记 warn。**顺带修正一处标识误用**：`recordFixOutcome` 曾把 recommendation_id 塞进 `FixOutcome.AlertID`（两个不同的标识），改为读 `alert_id`。拓扑/指标**拉取**方向待 OpsMesh 平台侧的服务寻址语义定案（平台 API 收服务名、诊断持有 host 目标），已写入配置注释与 README，不在本仓单方面猜测。
  **未链入包 8→0**；README 能力表与 roadmap 处置行同步。

## [v1.15.0] - 2026-10-03 — 剩余处置项收口 + AI 闭环闭合 + 兼容层重定义

本版把 v1.14.0 之后登记的剩余处置清单一次清零（PR #33）：**ChatOps 审批镜像接线**（`notify.chatops.*`，slack/dingtalk/feishu webhook bot，与 Jira 镜像扇出并存，默认关）；**llm_diag 接线并反转收敛判据**（跨两轮同假设确认才收敛、`ConfidenceSource=model_self_report`、死状态撤除、`ai.llm` 开启时富化诊断报告）；**feedback 闭环闭合**（PatternID 缺陷修复 + JSON 快照持久化 + 与推荐引擎共享同一 KB + Apply 结论喂入）；**compat D-08 映射重定义 + 外部 oracle 守卫**（状态感知、无忠实动作 fail-closed、幻影动作清零）；**`levee run --shell` 注册为显式标注的本地调试命令**（不经治理链）；**review-report 21 项全部闭环**（6 项 P2 收口）。未链入包 8→3。逐条明细见下方「新增」，策展版见 [docs/release-notes/v1.15.0.md](docs/release-notes/v1.15.0.md)。

### 新增


- **剩余处置项一次收口：ChatOps 审批镜像接线、llm_diag 判据重定义并接线、feedback 闭环闭合、compat 映射重定义 + oracle 守卫、`levee run` 注册、全部 P2 文档项**。逐项：
  **① ChatOps 审批镜像（`notify.chatops.*`）**：slack / dingtalk / feishu webhook bot 在 serve 装配（StartAll 启动订阅、退出 StopAll），bridge 装入**扇出**观察者槽位与 Jira 镜像并存；默认关（零装配），bot 缺 name/webhook_url 或平台未知时**拒绝启动并指名 bot**（静默跳过会像"能送达的镜像永远不送达"）。config.example.yaml 补 notify 的 jira/chatops 两段（jira 段此前在示例里也缺失）。
  **② `diagnosis/llm_diag` 接线并反转收敛判据**：收敛必须**跨两轮同假设确认**（稳定性门禁；单轮自报与振荡都不再收敛，`TestDiagnose_Converge` 从 Turns==1 改写为 2 并留痕、新增拒绝/振荡/稳定但无自报三个负例）；`ReasoningResult.ConfidenceSource` 显式标注 `model_self_report`（映射审批档位时只能当启示，不当作已验证事实）；死状态 `StatusInconclusive`（声明后从未赋值）撤除。`ai.llm` 开启时 DiagnosisService 以**收敛**结果富化规则报告（失败/未收敛保持规则结果并记日志——LLM 故障不拖垮 Diagnose RPC）。
  **③ `recommend/feedback` 闭环闭合**：PatternID 存储缺陷修复（`Record` 存值副本却返回局部指针，`Learn` 的落戳写进无人能读回的记录——改为存储指针切片）；`PersistPath` JSON 快照（原子写；损坏文件降级为空启，学习是辅助状态不是事实来源）+ 重启恢复 records/stats/patterns/incidents 并**回填共享 KB**；serve 与推荐引擎**共享同一 `KnowledgeBase`**（此前 Learn 写进的 KB 引擎根本看不到——学习是断的）；Apply 终态把结论喂入闭环（`change_service` 记录 `FixOutcome`，从 params 取 target / recommendation_id）；CLI 经 `feedback.LoadInto` 只读受益。
  **④ `compat` D-08 映射重定义 + oracle 守卫**：flat 表（4 个幻影动作 + 状态盲：`apt state=absent → pkg.install`）换成状态感知解析器——apt/yum 按 present/absent/latest 映射 install/remove/upgrade、service 按 started/stopped/restarted/reloaded 或 enabled 映射六动作、user 按 present/absent 映射 add/remove；**无忠实动作的构造**（`file` 模块、`group` 模块、未知 state、state 与 enabled 同用）fail-closed 拒绝并在报错里给出替代。oracle 测试（外部测试包，空白导入真实模块注册表）遍历解析器全部输出、逐个对照 `executor.DefaultExecutor()`——"幻影动作"这一类从此进不了门。
  **⑤ `levee run --shell` 注册为显式标注的本地调试命令**：平台 shell、超时、结构化输出（`--json`）、子进程退出码经 `[exit=N]` 透传（超时 8）；帮助文本与 `levee-api.md` 第 9 章明说"不经 plan/审批/审计链"，是调试逃生口而非变更执行路径。
  **⑥ 文档 P2 收口**：spec §2.2 补 `any`/`all` 与 `allow_irreversible`、§10.3 注明 mysql 模块现状、api §13.2 补"仅列核心端点"注记、附录 A 补 `levee run` 与 `levee schedule`(V1)、design §5.2 加目标态注记、mvp-tasks T058 同步；`docs/review-report.md` 第 6 章记录 21 项全部闭环。
  未链入包 8→3（剩 `diagnosis/topology`、`compat` 导入层、`opsmesh`，覆盖率 88%~93% 实测），README 能力可达性表与 roadmap 处置行同步。

## [v1.14.0] - 2026-10-03 — 治理闭环收口 + 安全/新鲜度门禁 + AI 配置面

本版把 v1.13.0 之后的 15 个 PR 一次收口：**治理闭环端到端为真**（审批绑定认证主体与配额、变更窗口真正强制、多租户从已验签凭据接线、两张审计表都带上生产路径真的会封的防篡改链）；**CI 补齐安全与新鲜度门禁**（action 按 SHA 固定、govulncheck 必过、gosec 改固定版本直跑、121 个 benchmark 冒烟、前端产物要求字节一致）；**"建了没人用"的面要么接上配置、要么按定位退役**（`ai.llm` 配置面默认关、rag/scheduler 退役、compat 去莠、观察者槽位扇出）；REST 网关双 URL 形状路由修复（前端带前缀的读不再全部 400）。无已知未闭缺陷；逐条明细见下方各节，策展版见 [docs/release-notes/v1.14.0.md](docs/release-notes/v1.14.0.md)。

### 新增

- **`ai.llm` 配置面：推荐引擎的混合 LLM 模式第一次可由运维开启（README「运维无法开启」的缺口关闭）**。`recommend.NewLLMClient` 的 22 个调用点全在测试里，serve 与 converse 构造推荐引擎时都不传 `LLMClient`，`internal/config` 没有任何 llm 配置键——库完整且可达，产品不可用。新增 `internal/config` 的 `AI/LLM` 节（enabled/provider/api_key/model/base_url/max_tokens/temperature/timeout），`newServeConvEngine` 与 CLI 工厂都读它。**默认关且该默认承重**：不构造客户端，引擎走纯知识库路径，与引入本节之前逐字节一致；开启是显式动作，客户端构造失败降级回知识库模式并打 warn（与 `setupServeTracing` 同一契约），不挡守护进程启动；config 加载失败按普通错误返回（converse 拒绝启动而不是半配置运行）。CLI 工厂签名不变（测试的 var 替换点零改动），配置加载在工厂内、与其他命令同一 `loadConfigForCmd` 入口。`config.example.yaml` 同步示例段。
  同批按 roadmap 已登记的定位处置未链入包（8→6 个）：**退役删除** `internal/recommend/rag`（FNV-1a 伪向量 embedding，接上等于把检索退化成噪声）与 `internal/scheduler`（agent 任务派发，与已接入的 `internal/dispatch` 职责重叠，零生产调用）；**去莠** `internal/compat`——自述「不强制审批与门禁」的模拟执行器（直接建 running run 行、逐目标模拟"执行成功"、完全绕开审批/plan 门禁/ClosureRunner 的影子路径）与重复的风险评估器删除，导入层与 D-08 映射保留待产品定夺（映射仍含 4 个幻影动作，oracle 守卫测试随 D-08 重定义一起加）；`chatopsbridge` 的两个观察者槽位（`approval.Service.WithDecisionObserver` / `ChangeService.WithApprovalCreateObserver`）从单槽改**扇出**——单槽是 last-write-wins 而 serve 已把两槽都给了 Jira 镜像，直接接线会静默挤掉 Jira；现在每次安装都保留、nil 清空、逐调用 panic 恢复（approval 与 grpc 两包守卫测试钉住），桥的接线动作随 bot 渠道配置面一起做。README 能力可达性表与 roadmap 处置行同步（未链入 6 包约 2.5k 行，覆盖率 88%~95% 实测）。

- **trace 哈希链第一次在生产中封链：全部终态收口点接线（「机制存在但生产零接线」就此关闭）**。`HashChainBuilder` 的 Build/BuildBatch/BuildForce 此前全部调用点都在测试里——`/audit/verify` 的 trace 部分恒为「无链可验」，takeover 路径「lands in the run's hash chain」的注释描述的是一个根本不会运行的机制。新增 `audit.SealRunTraceChain`：无 trace 的 run（rejected / 未 apply 就 cancel）→ nil，不是要每个调用方去重的错误；已封链 → 幂等 no-op（并发的封链竞态以 Build 的拒绝语义收场）；`ErrChainBroken` **原样返回、绝不吞**——存在但不验证的链是证据，必须进操作员的日志。五个封链点全部接线：`ApplyChange` 成功分支、`ApplyChange` 引擎错误分支（failed 同样是终态）、Pause/Resume/Cancel 共享的直迁块（`runstatus.IsTerminal` 守卫——中途迁移封链会让之后的每条 trace 都落到链外、以 empty_hash 暴露）、wiring 的 retry 收尾（`retry_finished` 是该 run 最后一条 trace）、takeover 接管点。封链失败 best-effort 不致命（与 `audit.Record` 同一决策：run 的结论已持久化，一个拒绝封链的存储不该改写它），但 warn 必须响。
  如实登记：`Seal` 全量读取的成本经分析为**重算式封链设计的内在成本，不是欠账**——链序必须是 (timestamp, id) 的全序，SQLite 的 DATETIME 存储在 SQL 层不可靠（`ListAudits` 自己都在 Go 侧重排保证可复现），键集分页不安全；乱序写入（跨节点时钟偏斜）会在链中间拼接，只有从链头重算才能修正，所以分页/增量封链反而会把断链固化为静默。实测量级：几十条审计行/天 × 一年 ≈ 2 万行，每次封链的全量读为毫秒级。
  测试：`internal/grpc/trace_chain_seal_test.go` 4 例（成功 / 引擎错误 / cancel-with-trace 三条封链路径 + cancel-before-any-trace 的 no-op 面）；takeover 新增 `TestTakeover_SettledRunTraceChainIsSealed`（真实 PG 实测）；wiring retry 测试补上链验证断言。移除任一封链点即红——这就是接线的变异防线。
- **把 rollback 三处「旋钮在、线没接」接通**：① notify 传输层真的接上了（收件人从 `runs.creator` 解析，而不是配置项）；② `WithVerifyTimeout` 变成可配置的 `WithPostVerifyTimeout`；③ 手动回滚路径恢复 run 级基线——**因此撤回 LE102**。三条都是下面那条 CHANGELOG 里如实记下的遗留，本次逐条兑现。
  **① notify 传输层**。之前的判断是「缺一个事实而非缺代码」：state 不记录变更发起人。复核后发现**事实一直都在**——`runs.creator` 就是发起人，上一轮只 grep 了 `Initiator/RequestedBy/CreatedBy` 因而漏掉它。现在每次 run 用 `resolveRollbackActors` 读一次 run 记录得到收件人，构造 per-run 的 `notifyRollbackSink`（适配器上一轮就已写好并测过，只是没有调用者）。**收件人在构造期绑定而不是在回调里改**：sink 是 per-run 的，回调里改字段是并发回滚下的数据竞争。
  **缺任一条件就退化为记日志、不发送**：没有 `NotificationManager`（`WithNotificationManager` 未配置），或 `creator` 为空。理由与上一轮相同并且更强——**一条看起来已送达、实际无人收到的通知，比一条被记录的分级更坏**，因为操作员会停止追查。run 记录读不出来时降级为 warn 而非报错：分级跑在回滚路径上，此时 store 读不出来恰恰是最需要有人被告知的时刻。
  **② 上界可配**。`WithPostVerifyTimeout(d)` 接到 `rollback.WithVerifyTimeout`；未设置时保持 0，由 verifier 解析为 `DefaultVerifyTimeout`（2 分钟）。它存在的理由在上一轮已经写明：验证刻意脱离调用方取消，没有上界就是无界的门禁扇出。
  **③ 手动回滚恢复基线，并撤回 LE102**。`rollbackChange`（`RollbackChange` / `levee rollback`）此前只接了 step 级 `RestoreForStep`，不碰 run 基线——这正是当时用 LE102 拒绝 `snapshot` + `on_failure: manual` 的唯一理由（采了没人用的基线比没有基线更糟）。现在它接上同一个 `runRemoteSnapshotter`，在补偿走完之后恢复一次。**LE102 随之从常量块、错误目录、规范两张码表与校验测试中一并撤除**——一个有常量、有目录条目、却没有任何产生点的码，比没有这个码更坏：它在目录里显示为编译器可能产出的东西，而它不能。撤回而不是保留，是因为能采到也能还原的文档是正常情况，继续拒绝只会把操作员推向 LE097 一直反对的 step 级绕法。
  **恢复失败用 `errors.Join` 合并进回滚结论**：补偿可能已经成功，只报"基线恢复失败"会吞掉这个事实，只报成功又会吞掉基线，操作员两件都需要，按这个顺序。
  测试：`TestRollbackChangeRestoresRunBaseline` 端到端断言基线**真的带着正确内容落到目标机**（按上传字节数核对，而不是"尝试过"）；`TestRollbackChangeManualPolicyWithBaselineCompiles` 钉住 LE102 撤回的编译侧；`TestManualRollbackWithoutSnapshotStoreDoesNotClaimARestore` 钉住未接线时既不尝试也不谎称；`TestNewRunNotifySinkRequiresTransportAndInitiator` 三组子用例钉住两个降级条件；`TestResolveRollbackActorsReadsCreator` 钉住收件人来自 run 记录、未知 run 降级不报错。测试替身 `loopChannel.Upload` 原本无条件报 `Upload not supported`（无任何测试依赖该失败），改为记录上传并核对内容——否则只能表达"试过了"，无法表达"基线真的回去了"。

- **run 级快照基线：一个独立原语，不是 workflow 级 `snapshot_paths` 的放宽**。§7.1 拒绝 workflow 级 `rollback.snapshot_paths`（LE097），理由是它没有补偿基线可挂——但"整次运行的前状态"是真实需求（多个 step 分别改了不同文件，运维要的是 run 开始前那一份），而把它投影到每个 step 恰是 roadmap 方案 B 明确拒绝的语义错误。所以新增顶层 `snapshot { scope, paths, type }` 块与 `engine.RunSnapshotter` 接口，规范新增 §7.2。
  采集在**所有锁已持有、任何目标尚未被改动**时执行**一次**（覆盖全部 target，与 batch 数、step 数无关），恢复在**补偿全部完成之后**执行**一次**——run 的前状态对它覆盖的路径有最终发言权，否则 step 级恢复会盖掉 run 级基线。校验 fail-closed（LE098 scope 必须是 `run`；LE099 无 paths；LE100 type 非法且报错文案列出全部接受值；LE101 必须绝对路径，相对路径在采集与恢复时解析到的工作目录不同；LE102 曾拒绝与 `on_failure: manual` 同用，已随手动路径接上基线恢复而撤回）。
  **与 step 级刻意相反的一点：缺采集器时拒绝这次 run。** step 级"没装就是 no-op"是未接线行为，可以接受；而 run 级基线是**声明**——运维读到 `scope: run` 的语义就是"回滚会把这些路径还原"，若声明了却无处记录还照常执行，就会改掉一批从未留下前镜像的目标机，同时让运维相信回滚能还原。宁可拒绝 run。
  **进 plan_hash**（`canonicalRunSnapshotV2`）：基线决定"回滚会把什么还原"，两份只在基线上不同的计划**不是同一份计划**——执行动作完全相同，但一份承诺还原 `/etc/app.conf`、另一份什么都不还原。路径排序后再入哈希（声明是集合，采集顺序不是被批准的内容）；`type` 省略与显式写 `file` 归一化后哈希相同。
  `ClosureResult` 新增 `RunSnapshotRestoreError` **独立字段**而非往 `Error` 追加：补偿账本拥有 run 的结论，一次已完成的回滚不该因为基线恢复失败被改写成失败——那是两件不同的事实，混进一个被字符串匹配的字段里会一起丢失。
  测试：`internal/dsl/run_snapshot_test.go`（解析 + 10 组校验表，含「scope 省略是拒绝不是取默认」与「step/batch scope 是 LE097 的同一个洞下一层重现」）；`internal/engine/run_snapshot_hook_test.go` 五组，其中顺序断言不是"回调被调用过"而是**在回调里读执行器当时的调用计数**：采集时必须为 0，恢复时必须已含前向与补偿两类调用；`internal/plan/run_snapshot_hash_test.go` 四组。`internal/errors` 的目录计数 30→35（该测试正是防止新增码只进常量块不进目录表）。
  已知限制：`on_failure: manual` 曾因手动回滚路径不恢复基线而被 LE102 拒绝，该码已随手动路径接上基线恢复一并撤回（见上一条）。基线按 change id 存储，因此操作员触发的回滚同样找得到它。

### 修复

- **121 个 benchmark 第一次进 CI（冒烟门禁）**。benchmark 报告停在 v1.3.0、3 组回归悬空 10 个版本——一个烂掉的 benchmark（编译漂移、回归成 panic）什么都不报。test 作业（ubuntu 腿）新增 `go test -bench=. -benchtime=1x -run '^$' ./...`：每个 benchmark 执行一次，`-run ^$` 跳过测试、`-benchtime=1x` 把它钉在**活性门禁**而不是测量（数字是本地作业的事）；本机实测 121 个全过零 panic。


 d8439b2 (docs: 记录看板读 400 的实测证据与"测试复刻生产接线"这一成因)

- **review-report 的 21 项文档问题逐条复核完毕（2026-10-03），4 项当场修掉**。2026-08-15 的审核报告登记 1 P0 + 10 P1 + 10 P2，逐条复核判定：**11 项已修复/已过时**（P0-01 与全部 10 个 P1——emergency 三级已进 spec 支持列表、命令名 `--params`/`retry-host`/T080-T085 对齐、模块任务 T017.1-T017.4 补齐、凭据阶段说明、估时方案 B、4.4.8 改名，证据链完整）；**10 项仍有效**（全部 P2，无一无法核实），其中四项当场修掉：P2-02（`-o json`→`--json`，代码 root.go 为准）、P2-03（分级表补枚举列并修正「紧急 15min」——代码实为 30min，docgen 生成表为权威）、P2-06（API 文档 logs 的 `--host` 是 stale，代码实为 `--target`）、P2-10（T100 改名「10 分钟上手门禁」区分 G-01）；剩余 6 项（P2-01/04/05/07/08/09）在新增第 6 章逐条登记处置建议（文档迭代或处置决策）。复核表见 `docs/review-report.md` 第 6 章，原始判定保留在第 4 章不动；「有条件通过」的 P0 前提已消除。
 f79b7d7 (docs: recheck the 21 review-report findings; fix four doc drifts on the spot)
 ff2591f (docs: recheck the 21 review-report findings; fix four doc drifts on the spot)
- **前端嵌入产物漂移：CI 第一次要求已提交的 `internal/web/dist` 与源码字节一致，并刷新了已经陈旧的产物**。Go 二进制内嵌的是**已提交**的 dist——它停在最后一次手工 `make web` 时的样子，实测 33 个产物中 19 个（18 个哈希名 + index.html）与同一份源码的全新构建对不上，生产二进制里的 UI 落后于仓库里的源码；而 CI 的 frontend 作业此前只构建不比对，漂移静默积累。修法两层：frontend 作业在构建后把 `web/dist` 拷入 `internal/web/dist` 并要求工作树零 diff、零未跟踪新文件（照抄 proto/docs regenerate check 的门禁形状）；同时随本 PR 提交刷新后的产物（node:20 构建——与 CI 的 node 版本一致，且实测 vite 对 node 20/26 输出逐字节相同，产物不随构建机漂移）。
  **门禁第一次运行就抓到第二层问题：同一份源码，Windows 工作副本的构建产物与 CI 不同**——本机 `core.autocrlf=true` 且 `.gitattributes` 没覆盖 `.svg`（`web/public/assets/favicon.svg` 被原样拷进 bundle），检出早于规则落地的源文件还带着 CRLF，view chunk 内容随行尾变化、哈希名全变；vendor chunk 来自 node_modules、不受检出影响，名字保持一致——这正好解释了漂移只发生在 view 层。处置：`.gitattributes` 补上 `*.svg text eol=lf`（git 的行尾转换——含 `git archive` 导出——从此对构建输入的全部类型都是 LF），产物用 `git archive` 导出后在 node:20 容器内构建，与 CI 的检出形态字节对齐。门禁的两轮红灯各有实证成因：第一轮抓到 view chunk 批量漂移，第二轮抓到 favicon.svg 的行尾残差——不是假阳性。
- **CI 供应链加固：全部 action 按 commit SHA 固定 + 顶层最小权限 + 每个 job 显式超时**。此前 workflow 引用的是可移动的 major tag（`actions/checkout@v7`、`codecov/codecov-action@v7` 等），而 tag 可以被上游重指——一次上游账号失陷即可把任意代码塞进我们的 CI 并拿到仓库默认令牌。现在每个 action 都钉到 40 位 commit SHA 并保留可读的版本注释（`@3d3c42e5…  # v7`），使「升级」变成一次显式提交而非静默滑动。
  顶层加 `permissions: contents: read`：不写这一段时 workflow 继承仓库默认权限（通常是全 scope 读写），于是任何一个被投毒的第三方 action 步骤能拿到远超所需的权限；需要更多权限的作业（trivy 上传 SARIF）在 **job 级**单独放宽，权限面因此逐 job 可审。
  同时给每个 job 补 `timeout-minutes`（此前缺省为 GitHub 的 6 小时上限，一次挂死的作业会长时间占住 runner 并掩盖真实失败）。**未改任何门禁语义**——本次只收紧信任边界与资源边界。
- **govulncheck 门禁被 go.mod 的 `go` 指令挡住：`go 1.26.0` → `go 1.26.6`**。govulncheck 作业刻意用 `go-version-file` 按 go.mod 声明的工具链扫描（其余作业全部用浮动的 `'1.26'`，自动取最新补丁），于是其余作业已经跑在 1.26.6 上时，扫描用的仍是 1.26.0——25 条标准库符号级漏洞（修复散布在 go1.26.1～1.26.6，`crypto/tls` / `net/http` / `html/template` / `crypto/x509` 等）全部来自这 6 个补丁的差距，而真实编译产物的工具链并没有这些洞。升 `go` 指令后扫描与运行时一致，本地 `govulncheck ./...` 实测 0 漏洞。
  如实登记：`golang.org/x/crypto@v0.57.0` 的 `GO-2026-5932`（openpgp 包无人维护）是 **Module 级提示**——`Fixed in: N/A`、无修复版本、代码未调用任何相关符号，也不触发退出码 3，维持现状并留此记录；v0.57.0 已是该模块最新版，「升级解决」不成立。
- **gosec 作业从 docker action 改为 runner 上的固定版本 Go 程序（上一条升 `go` 指令的直接后果）**。securego/gosec 的 docker 镜像内置的是**构建当日**的 Go 工具链（Dockerfile `golang:${GO_VERSION}-alpine`），go.mod 升到 1.26.6 后容器内 go/packages 的 `go list` 触发工具链切换，扫描在 import 阶段后被**静默杀死**——连续两次、无错误输出、无退出码，而同一扫描器版本（v2.28.0）`go run` 对同一棵树 0 issues 全程可完成。改法：扫描器版本仍钉死在 v2.28.0（`go install`），工具链随 `go-version: '1.26'` 浮动——与 govulncheck 作业同款形态，今后 `go` 指令再升无需再动这个作业；排除规则与报告产物原样保留。
- **PG 侧 audit WORM 第一次有了回归防线（audit 条目里「本机无实例未执行验证」的缺口就此关闭）**。audit 的 plpgsql 触发器（schema v6）与全局哈希链此前只在 SQLite 上被测过（`internal/audit/auditchain_test.go`）；CI postgres 腿的 `TestPGStore_TraceHashChainAndWORM` 测的是 **trace**——PG 触发器被改坏或列清单漂移时 CI 不会发现。新增 `internal/state/pgstore_auditchain_test.go`（`TestPGStore_AuditChainSealAndWORM`，真实 postgres:16 实测通过）：封链走触发器的放行路径、重封幂等、七个内容列（action/actor/result/target/run_id/tenant_id/timestamp）与 DELETE 全部拒绝且行内容原样、链列可写且 Verify 立即发现重写、Seal 复原链。放进 `internal/state` 的外部测试包 `state_test` 有两个原因：CI 的 postgres 作业只跑 `./internal/state/...` 等四个包，写在 `internal/audit` 永远不会被 CI 执行；外部包同时避开 state↔audit 的导入环。本机无 PG 的开发环境照旧 skip，`go test ./...` 不受影响。
- **多租户从「store 层有契约测试」变成「daemon 真正接线」，并把隔离原语从 IncidentID 编码换成凭据驱动的上下文**。此前的状态被 CHANGELOG 自己如实登记过（旧条目：「多租户隔离在 store 层有契约测试，但 daemon 主路径未接线（MVP 范围决策，V2 再评估）」）——即又一处「机制存在但生产零接线」。本次把它接通，并顺手换掉了承载它的原语。
  **替换而非叠加**：删除 `internal/tenant/isolation.go`（530 行）与配套两份测试（318 + 537 行），改用 `context.go` + `store.go` + `verify.go`。旧原语把租户编码进 `IncidentID`，租户是**行内容的派生属性**；新原语把租户作为**调用的属性**（`Resolver` 从上下文解析），因此 `TenantStore` 能成为 `state.Store` 的真·drop-in（`var _ state.Store` 编译期断言，84 个方法），而不需要按租户构造实例。`context.go` 的注释把这条理由钉住：`state.Store` 的每个方法首参都是 `ctx`，所以无需改 ~80 个签名。
  **租户只能来自已验签凭据，不能来自请求头**：命名令牌 `--auth-token name=secret,tenant`、OIDC `auth.oidc.tenant_claim`（从**验签后的** claims 读取，与 subject 同等可信）、SSO 会话把租户**铸进签名的 session token**（`SessionManager.Issue(subject, roles, login, tenant)`），因此浏览器拿到 token 后**无法改租户**。客户端的 `X-Tenant-Id` 自断言在设计上就没有入口——这正是它与 `X-Acting-As` 的区别。
  **fail-closed 的不对称是全部安全论证**：多租户**关闭**（默认）时一切归 `DefaultTenantID`，行为与引入该功能前一致；**开启**时上下文缺租户返回 `ErrTenantNotFound`，**绝不回落默认租户**。理由写在 `Resolver` 的注释里：若回落，则任何一条忘记传播上下文的代码路径（新 RPC、后台 goroutine、将来的重构）都会静默读写默认租户的数据——「以为隔离了但漏了一处」正是这个设计拒绝允许的失效形态。
  **谓词进 SQL，且是覆盖而非 AND**：`ListRuns` 等列表方法把 `filter.TenantID` **覆写**为解析结果（注释：「a caller-supplied TenantID must not be able to widen or narrow the scope the resolver decided」），由 base store 翻成 SQL 谓词，因此分页/计数语义正确；单对象读写走 `ownRunByID` 系列先验归属再动作，`Create*` 一律 `x.TenantID = tid` 打戳。10 张租户表（`approvals` / `audit` / `batches` / `credentials` / `inventory_groups` / `lock` / `run_assignment` / `runs` / `steps` / `targets` / `trace` 中的 10 张，两套 schema 各 10）。
  **测试**：`internal/tenant/store_test.go` 覆盖谓词注入与跨租户拒绝；`verify.go` 提供 `VerifyIsolation(ctx, store, tenantA, tenantB)` 作为可复用的隔离自证原语。
  **如实登记的四条边界**：① **GitHub SSO 拿不到租户**——OAuth 只证明身份不证明归属，多租户部署必须改用命名令牌或 OIDC；② 后台接管/派发循环（takeover / dispatch）**刻意使用未包裹的 store**，因为它们没有请求上下文，其写操作目前全是**不写 `tenant_id`** 的窄状态更新——将来若在这些循环里新增 `Create*`，会写出空租户行，需要配套守护测试；③ `ListCredentials` 与 `ListInventoryGroups` 的基础方法签名不带 filter，无法在 SQL 层加谓词，由 `TenantStore` 在**内存中**按租户过滤（凭据密文为 AES-GCM，过滤发生在任何调用方拿到指针之前）；④ 开启前必须先确认 `users.yaml` / 令牌配置覆盖所有会调用治理 RPC 的主体，否则 serve 拒绝启动。以上四条已同步写入 `docs/security-audit.md` 的「已知限制」。
  **本地验证**：`go build ./...` ✅ · `go vet ./...` ✅ · `golangci-lint run` 0 issues ✅ · `go test ./internal/tenant/ -count=1` ✅ · `go test -tags integration ./tests/integration/` ✅。**PG 侧本机无实例，未执行**（CI 的 integration job 覆盖）。
- **`audit` 动作日志补上 WORM 触发器与哈希链，并且**真的**在写入时封链**。`trace` 表自 v1.0.0 起受 WORM 触发器 + 按 run 哈希链保护，但 `audit` 表——`GET /audit/log` 实际服务的"谁对哪个目标做了什么"——两者皆无；它此前只是**因为 `Store` 接口上恰好没有 update 方法**才表现为 append-only，而 DBA 没有义务尊重一个 Go 接口。
  schema v7（PG v6）加 `prev_hash` / `curr_hash` + `idx_audit_chain (timestamp, id)` + UPDATE/DELETE 触发器。**`tenant_id` 必须进不可变列**：否则一次 UPDATE 就能把审计记录搬进别的租户而链毫无反应。
  **链是全局而非按 run**：`run_id` 为空的 audit 行恰是 login / config / credential 这类安全相关性最高的记录，按 run 建链结构上覆盖不到。多租户开启时链的范围即 `Store` 暴露的范围（`TenantStore` 注入租户谓词），表现为每租户一条链——范围收窄，不是漏洞。
  **封链按存储顺序重算，不向链尾追加**。原因是 audit id 为 8 字节随机 hex（`internal/grpc/change_service.go:169`），同毫秒写入的两行按 id 排序本质随机；追加式封链会把后插入的行挂到先插入的行上，直到验证时才以"顺序相反"暴露——而那种暴露无法与真正的篡改区分。重算式封链顺带买到一个性质：**幂等**，两个并发 `Seal` 对重叠行必然导出相同哈希，因此不会分叉（`TestAuditChain_ConcurrentSealIsSafe` 用 8 个 goroutine 抢封 25 行钉住）。
  **最容易漏掉的一步是让链真的被封**。只加列和触发器而不封链，验证会永远报 `empty_hash`；更糟的是把它改成"忽略空哈希"——那恰恰是留给 DBA 的盲区。所以新增 `audit.Record`（`CreateAudit` 后立即 `Seal`）并把 **10 处生产调用点全部改走它**（`change_service` / `rest_gate` / `lock` / `pause`×2 / `template` / `cmd`×4）；非测试代码中的裸 `store.CreateAudit` 已清零（`git grep` 可验）。封链失败只记 warn 不返回错误：行已经落库了，而 LEVEE 一贯把审计写入视为不致命；但未封链的行在下次验证时以 `empty_hash` 暴露，不是静默通过。
  验证入口是 `GET /audit/verify`，响应新增 `auditChain` 成员并折叠进顶层 `valid`。**未改 proto**：`levee.proto` 的 `VerifyHashChainResponse` 字段全是 run 维度的，硬塞进 `RunVerification` 会误报其覆盖范围；且本机无 `protoc`（只有 Go 插件），无法重新生成 pb。REST 侧先把 protojson 结果 marshal 成 `map[string]json.RawMessage` 再 splice，避免任何既有字段被重新编码（int64 计数过 float64 会掉精度）。
  **被这次改动推翻的一个前提，是全量测试自己报出来的**：`cmd_backup_test.go` 的 `TestBackupRestoreEndToEnd` 原本直接 `UPDATE audit SET action='TAMPERED'` 来证明"恢复能把行还原"，而 WORM 触发器正确地拒绝了它——**是控制生效，不是代码坏了**。改为先 `DROP TRIGGER` 再篡改（正好建模"守卫被先摘掉"），并补一条新断言：恢复后触发器必须回来（恢复是整文件覆盖，所以快照里被摘掉的守卫会被重新装回）。同时把 `internal/backup` 的 `verifyWormTriggers` 从"只查 trace"改为按表校验（`trace` 与 `audit` 各自两个），否则一次 PG 破坏性恢复可能只把新控制降级回无控制。
  测试：`internal/audit/auditchain_test.go` 17 个测试函数（`TestWORM_BlocksContentUpdate` 另含 7 个子测试逐列验证不可变性）；`internal/grpc/rest_auditchain_test.go` 4 例。**变异验证**（改坏 → 转红 → 改回）：从 canonical 字段集删掉 `TenantID` → `DistinguishesEveryField/TenantID` 转红；`Verify` 游标改用重算值 → `TamperDoesNotCascade` 转红；触发器去掉 `tenant_id` 条件 → `WORM_BlocksContentUpdate/tenant_id` 转红。
  **如实登记的两条残留**：① **trace 链在生产中仍然零接线**——`HashChainBuilder` 的全部调用点都在 `tests/integration/*` 与 `*_test.go`，生产代码从未调用过 `Build`/`BuildBatch`/`BuildForce`，即按 run 的 trace 链至今没封过一条，与 SA-007 同属"机制存在但生产零接线"，本轮**未修**（会触及 trace 写入路径），已写入 `docs/security-audit.md` 已知限制而非宣称闭环；② `Seal` 每次全量读取可见 audit 行（无 LIMIT），稳态只写新增行但读成本随日志增长，超大审计库需改分批。**PG 侧（plpgsql 触发器、占位符编号、v6 迁移步）本机无实例，未执行验证**，仅经编译期检查与逐行比对。
  本地：`go build ./...` ✅ · `go vet ./...` ✅ · `go test ./... -count=1` 69 包（67 ok / 2 无测试文件）0 FAIL ✅

- **Web 看板的读全部返回 400（网关两条挂载把前端的路径形状挡在门外）**：`internal/grpc/rest.go` 把 `/` 挂在 RESTful 路由（`/changes`、`/templates/:id`、`/system/config`…），把 `/api/v1/` 挂在另一条只接受 `/api/v1/<Service>/<Method>` 的路由上。而前端 `web/src/api/client.ts:9` 的 axios `baseURL` 就是 `/api/v1`，包装层写的都是资源路径（`get('/changes')`、``get(`/changes/${id}/logs`)``、`get('/system/config')`），拼出来正是 `/api/v1/changes` —— Go 的 mux 按更长的 `/api/v1/` 前缀匹配，于是**每一个看板请求都落进"必须是 <Service>/<Method>"那条路由**，回 400（`invalid path: …` 或 `unknown service: changes`）。rest.go 里"RESTful routes take priority over /api/v1/ so the frontend can call /changes"的注释描述的正是这个不成立的假设。
  第二半更隐蔽：`authMiddleware` 判定公开路径用的是**裸路径等值比较**（`/system/auth-info`、`/auth/github`、`/changes/deeplink/approve|reject`），带前缀的请求匹配不上，于是**登录页在还没拿到凭据之前就先被要求提供凭据**——SSO/OIDC 的登录入口拿不到 `auth-info`，无法列出可用的登录方式。
  实测（修复前，本机起 `serve` + `web` 两层）：`GET :19789/api/v1/changes` → 400 `invalid path`；经 `levee web` 代理 `GET :19790/api/v1/changes` → 同样 400；`/api/v1/system/status`、`/api/v1/audit/verify` → 400。修复后同一组请求：直连网关 `/changes`、`/api/v1/changes`、`/api/v1/ChangeService/ListChanges`、`/api/v1/changes/run-1/logs`、`/changes/run-1/logs` **全部 200**；经 SPA 代理带 token → 200；不带 token → 401（路由已通、只差凭据）；`/api/v1/system/auth-info` 无凭据 → 200，登录引导恢复。
  修法：`apiCompatPrefix` 在**中间件链最外层**剥掉可选的 `/api/v1` 前缀（只在路径边界剥，`/api/v1x/…` 不动，与 `apiRelativePath` 同一条规则），然后**一条路由器**按首段判断形状：注册过的服务名走 `<Service>/<Method>`，其余走 RESTful 资源表；未注册但形如 `XxxService` 的仍回 400 `unknown service`（集成者写错服务名不该被报告成"资源不存在"）。服务表与资源表改成 `grpcServiceHandlers()` / `restResourceHandlers()` 两张方法表，路由器与守卫共用同一份数据——`route()` 原先的 switch 与另一份服务名清单并存，正是"路由器与它自己的测试各执一词"的成因。
  **为什么测试此前一直是绿的**：`startTestGateway` 自己**复刻**了一遍两条挂载，而不是挂载生产的管线，所以测试从没走过真实那条链。现在 harness 与 `serveOn` 都调用同一个 `gw.dataHandler()`，"测试镜像生产接线"这类偏差无处藏身。
  防再犯：`TestFrontendApiPathsAreAllRoutable` 从 `web/src/api/*.ts` 抽出前端真正请求的路径，逐个用**网关自己的表**判定可路由（而不是在测试里再抄一份清单），并要求抽取数量 ≥30（实测 44 个调用点；包装函数是泛型 `get<T>('/x')`，只匹配 `get('/x')` 会静默漏掉绝大多数——这条也是写测试时实测出来的）。另有 `TestBothURLShapesResolveToTheSameHandler`、`TestUnroutableShapesKeepGivingActionableErrors`、`TestPublicPathsArePublicUnderBothShapes`、`TestGatewayRouterTablesCoverTheDocumentedServices`。变异验证 5 条全部被抓：服务风格分支永不命中、前缀剥离永不生效、边界规则被去掉、资源表改名、注册服务表少一项，各自使对应用例转红。

 aa16241 (docs: 记录看板读 400 的实测证据与"测试复刻生产接线"这一成因)
- **回滚后验证的两条「已知限制」清零：分级真正落地，且不再被调用方取消污染**。上一条把两件事写成了限制而不是绕过，这里逐条兑现，并且**其中一条是推翻自己上一次的定案**。
  **① `Grade` 恒空 → 真的分级**。上一条的判断（「挂 grader 牵涉通知与升级链路，是另一件事」）只对了一半：`rollback.Grader`（T038）与 `PostRollbackVerifier` 的 `WithGrader` 选项**早就都写好了**，缺的只是装配，而真正的堵点在 engine 调的是 `Verify` 而不是 `VerifyAndGrade`——即使把 grader 装上，分级算完也会被直接丢掉。现改 engine 调 `VerifyAndGrade`，并新增 `internal/wiring/rollback_grade.go` 构造 grader 注入每次 run 的 verifier。engine 侧派发失败只记 warn 不致命：run 的结局早已由回滚本身决定，一个拒绝投递的 webhook 不该把一次已完成的回滚改写成失败的 run（`PostVerifyResult` 不进任何 run 状态映射）。
  **传输层的真实状态，不含糊**：分级机制现已可达，但三个动作今天的落地形态不同，且都不是「假装已接通」——`audit` 写结构化日志（`internal/audit` 有哈希链 `TraceRecorder`，但 engine 路径今天不记任何 trace、state 里也没有回滚 trace 所需的身份，谎称已落审计是编造）；`escalate` 是 error 级日志（文案按凌晨三点被叫醒的人写，明确指出目标可能处于未知状态）；`notify` 走一个窄接口 `rollbackNotifySink`，**当前传 nil**，原因是缺一个事实而非缺代码——state 不记录变更发起人，而同样零调用者的 `notify.RollbackNotifier` 没有 initiator 会拒发，编一个收件人比不发更糟。`notifyRollbackSink` 适配器已备好，谁接上 `NotificationManager` 都是一行的事。**没有往 `Engine` 上加永远没人设置的配置项**：那正是本会话一直在清理的「建了没人用」。
  **② 取消不再伪造结论（推翻上一条的定案）**。上一条写「刻意不脱离 ctx——调用方已经放弃之后不应继续向目标机派发命令」，理由是「没有超时旋钮来约束这项工作」。复核后这个理由不成立：**旋钮根本不存在**（上一条误以为 `WithVerifyTimeout` 已有，实为不存在），所以「有旋钮才安全」的推理当时无据；而它要避免的后果是真的——取消是触发回滚的三个原因之一（Ctrl-C、fencing、上游 deadline），而等到验证跑的时候目标**已经被改回去了**，唯一没被回答的问题正是「改回去了吗」，旧行为保证这个问题的答案永远是 `context canceled`：一个关于请求的结论被当成了关于系统的结论。故改为 `context.WithoutCancel(ctx)` + `WithTimeout(上界)`，并新增 `WithVerifyTimeout` / `DefaultVerifyTimeout`（2 分钟，刻意大于单个门禁自身的 30s，因为上界覆盖整个 phase）与 `VerifyTimeout()`。调用方自带的更早 deadline 仍然生效（`WithoutCancel` 只丢 `Done`、保留 `Deadline`）。回滚派发本身仍是无上界的脱离 ctx（中断会留下未撤销的已应用批次），两者理由不同、边界不同。
  **旧测试被改写而非删除**：`TestVerifyCancelledContext` 钉的正是「取消即失败」这条旧契约，失败后把它改写成 `TestVerifyCancelledContextUsedToFail` 并注明这是刻意反转——契约反转必须留痕，否则下一个读到它的人会以为实现退化了。
  **`RollbackResult.RunID` 新增**：分级之后的一切（验证、notify、escalate、audit）只拿到一个 `*RollbackResult`，别处无处说明这是哪一次运行，没有它，「你的回滚失败了」这条消息无法与任何东西对应。落戳点在 `RollbackWithLedger` 入口，因此包括「连计划都拿不到」那种失败也会带上 run id。
  测试：① `internal/rollback/post_verify_context_test.go` 四组——调用方已取消时门禁仍执行且拿到的不是死 ctx（旧实现在这里会得到 `sawCancelled=true`）、脱离但有界（40ms 上界 + 阻塞门禁，断言被 deadline 掐断而非被调用方掐断）、上界默认与非正值回落（`WithVerifyTimeout(0)` 不能悄悄变成无界）、分级派发同样不受调用方取消影响；② `internal/engine/rollback_verify_after_test.go` 新增 `TestClosureRunner_PostVerifyIsGraded` 钉住两件事：门禁失败**压过**回滚成功（系统不健康就是失败，不管 undo 多干净），以及分级结果**绝不改写 run 的 phase**；③ `internal/wiring/rollback_grade_test.go` 钉住动作只在正确 grade 上派发（success 一个都不发）、无传输层降级为记日志而不是报错、投递失败要能冒出来给 engine 决定、以及 `Grader.Grade(nil)` 这种合法输入不会让通知路径空指针（否则「回滚失败」会变成「回滚失败且进程挂了」）。本地：`go build ./...` ✅ · `go test ./... -count=1` 全量 ✅ · `golangci-lint` ✅ · `docgen -check` ✅。


- **回滚后验证 `verify_after` 装配完成，并把规范里那句无法兑现的「缺省 true」订正为 opt-in**：`rollback.PostRollbackVerifier`（T037）与 `Grader` 早已实现且单测齐全，但 `internal/wiring/run.go` 给 `engine.NewClosureRunner` 的第 6 个参数**恒传 `nil`**——于是 `verify_after` 与 `on_failure` 的命运不同：后者在本次会话早些时候已真正接入执行器，前者连读的人都没有（`git grep VerifyAfter internal/engine` 零命中），正是 roadmap 上明写的「声明了但没人做」。修法分两半：① `wiring.newRunRunner` 为每次 run 用**同一个** `gateMgr` 构造 verifier 并注入，phase 模式（门禁名传 nil）重跑的就是本 plan 声明的 post-apply 门禁，不会凭空引入第二套检查；② engine 侧新增 `postRollbackVerifyRequested`，把「要不要验证」的判定权交回 plan——否则该参数一旦非 nil，回滚后验证会对**每一个**进入回滚的 run 无条件执行，那是比「没人做」更糟的另一个方向。
  **定案：`verify_after` 为 opt-in，规范 §7.1 的「缺省 true」订正为缺省 false。** 这不是「把文档改成实现的样子」，而是一次真实的语义选择：解析器把 `verify_after` 存成普通 `bool`（`yaml:"verify_after"`），未声明与显式 `false` 在 `RollbackSpec`/IR/`plan_hash` 里无法区分，要兑现「缺省 true」必须改成 `*bool` 并让 nil→true；而那样做的直接后果是**所有在该能力诞生之前写好的存量计划**，升级后下一次回滚就开始向目标机派发门禁命令。回滚路径上新增副作用不是修 bug，且与本仓对 `on_failure` 遗留值（如门禁上线前落库的 `abort`）一律保持历史行为的取舍正面冲突，故选择「显式声明才启用」，代价是 `plan_hash` 一个字节都不用动。
  **上一条那两句限制的现状更新（如实记录，不藏）**：① `notify` 的**传输层**尚未接线——分级机制与分级动作的派发已完全可达，但 state 不记录变更发起人，而 `notify.RollbackNotifier` 没有 initiator 会拒发，编一个收件人比不发更糟，故 wiring 传入 nil sink（记 warn 日志）；适配器 `notifyRollbackSink` 已写好并有测试覆盖。`escalate` 与 `audit` 今天分别是 error 级与 info 级结构化日志：`audit` 包虽有哈希链 `TraceRecorder`，但 engine 路径不记任何 trace、state 里也没有回滚 trace 所需的身份，谎称已落审计是编造。② `WithVerifyTimeout` 默认 2 分钟，在本仓**不可调**——它是 verifier 构造选项而 wiring 目前用默认值构造；旋钮已就位，按需配置是接线的一行。
  测试：`internal/engine/rollback_verify_after_test.go`——engine 里**第一次**真正注入 postVerifier（既有 `newTestClosureRunner` 恒传 nil，意味着这条链路在此之前从未被任何测试执行过）。`TestPostRollbackVerifyRequested` 钉住 nil plan / 无 rollback 块 / 未声明 / 显式 false / 显式 true 五种取值；`TestClosureRunner_PostRollbackVerifyIsOptIn` 用故意失败的 post-apply 门禁保证回滚必然发生，再只变 `verify_after` 一个变量：未 opt-in 的三种情形都必须 `PostVerifyResult == nil`（与「没装 verifier」不可区分，这是 nil→注入 改动的唯一回归防线），`true` 才产生结果且恰好跑 1 个门禁，同时断言补偿次数与锁释放不受影响。

- **`batches.strategy` 的第四份词表副本：`one-per-target` 能生成计划却永远编不过（门禁口径失真）**：`2c97899` 把批次策略收成"单一来源" `dsl.BatchStrategies`（含 `one-per-target`）并给生成器补了 `splitOnePerTarget`，但**同一份词表在包内还留着两处副本没同步**——`internal/dsl/validator.go` 的 `allowedBatchStrategies` 与 `internal/dsl/typechecker.go` 的 `batchStrategyEnum`，都只有 `percent/fixed/serial`。后果是同一份文档在不同入口得到相反判定：`levee compile` 与 AI 会话桥（两道都跑 validator）报 `LE034 invalid batch strategy "one-per-target" (allowed: percent, fixed, serial)`，而 serve 的 `PlanChange` 不跑 validator，计划照生成。实测：仓库自带 `examples/gate-templates/mysql.yaml` 编不过；生成器里那个 `splitOnePerTarget` 分支**没有任何能被编译接受的文档可以到达**（"死代码"的另一种成因：被门禁挡住，而不是没人调用）。守门测试 `TestBatchStrategyVocabulariesAgree` 当时是绿的——它只比 parser↔generator 两条腿，恰好是已经一致的那两条。修法：两份副本删掉，validator 改调 `IsBatchStrategy`、类型检查器由 `BatchStrategies` 派生枚举，**报错文案由词表 `strings.Join` 拼装**（帮助文本不可能再与门禁矛盾）；顺带把 approval.level 的三处同族副本收成 `dsl.ApprovalLevels`（今天三份一致，是运气不是机制）。守门测试补成四条腿：parser ↔ generator ↔ validator ↔ typechecker，外加"拒绝文案必须列出全部被接受值"。
  同时补上缺的那道门：新增 `TestShippedExamplesCompile`，把 `examples/` 下的工作流例子全部过一遍 parse + validate + 严格类型检查（插件清单与 `params:`+`workflow:` 模板信封按文档种类排除，并断言"至少 4 份被真正编译"，防止排除规则把整棵树跳掉变成员工绿灯）。三个 gate 模板的 `action: shell` 改成现行词汇 `shell.exec`；`examples/workflows/batch-update.yaml` 是整套旧方言（`workflow:` 顶层键、`target.labels`、`window: "02:00-04:00"` 字符串、`batches: [1,10,50,100]` 序列、`approval: high` 字符串、`verify:` 列表），已按规范现形重写并保留原意图（1% 金丝雀分批、02:00-04:00 UTC 窗口、high 审批、step 级 verify 与补偿）。**注意**：`examples/templates/patch-rolling.yaml` 同样是旧方言，且模板→run 路径另有缺陷（`levee new` 只把模板名写进 run、不写渲染后的内容，plan 阶段于是解析模板名），本条不一并修，已记入 roadmap。
  验证：守门测试从 2 条子测试扩到 4 条 + 1 条例子编译测试（覆盖 4 份例子、按种类排除 3 份非工作流文档）；变异验证——把 validator 的判断换回硬编码三值 → `validator_accepts` 与 `refusal_names_the_real_vocabulary` 转红；从 `batchStrategyEnum` 删掉 `one-per-target` → `typechecker_accepts` 转红；改坏一份例子 → `TestShippedExamplesCompile` 转红。**CLI 实测**：修复前 `levee compile examples/gate-templates/mysql.yaml` 报 LE034，修复后 `ok (val_errs=0, type_errs=0)`。

- **`cluster_nodes` 有两份 `CREATE TABLE IF NOT EXISTS`，谁先建谁说了算——其中一份还带一条与用法冲突的约束**：`internal/state/pgschema.sql` 与 `internal/cluster/pg_registry.go` 各自定义同一张表，且两份**已经漂移**——state 版多一个 `capabilities` 列与 `UNIQUE (address)`、`last_heartbeat` 是 `NOT NULL` 无默认；cluster 版三个 `NOT NULL` 都带默认值却没有 `capabilities`、也没有唯一约束。`IF NOT EXISTS` 让后跑的那份成为静默 no-op，于是数据库真实形状取决于哪个包先初始化。修法：新增 leaf 包 `internal/dbschema` 持有**唯一一份** DDL——列取两份并集、所有 `NOT NULL` 列一律带默认值（含该表的两个索引），但**去掉 `UNIQUE (address)`**；`state` 在 apply 时把嵌入文件与该片段拼接（`pgSchemaFull`），`cluster` 的 `clusterSchemaSQL` 以它开头；两个包互不依赖，`dbschema` 无依赖。
  去掉那条约束不是"清理没人用的东西"，是**活库实测的结果**：节点身份是 `id`，同一个监听地址以新 `id` 重新注册是合法动作（failover/takeover 就是这个形状）。把并集版（含 `UNIQUE`）建到全新 PostgreSQL 上跑，`internal/takeover` 有 **10 个用例**报 `SQLSTATE 23505 (cluster_nodes_address_key)`；去掉约束后同一套 PG 用例（`state`/`cluster`/`takeover`/`backup`，`-p 1` 串行）全绿。原先没暴露是因为集群路径总是 cluster 那份 DDL 先建表——**那条约束只在"state 先初始化"的环境里生效**，而那些环境今天仍然有此隐患：现网修法是 `ALTER TABLE cluster_nodes DROP CONSTRAINT IF EXISTS cluster_nodes_address_key` 的一次迁移，属于对已上线库的 schema 变更，**留给用户定夺**（已记 roadmap）。本 PR 不加任何迁移语句：`IF NOT EXISTS` 本就不动已存在的表，而新增约束/索引反而可能让历史数据不满足的旧库启动失败。
  守卫：`TestClusterNodesSchemaIsSingleSourced` 扫描 `internal/`+`cmd/` 生产源，断言全仓只有一份 `CREATE TABLE IF NOT EXISTS cluster_nodes`；`TestClusterNodesDDLCoversEveryReaderAndWriter` 断言列集合精确等于合并结果、覆盖读点与写点显式命名的每一列，并**显式断言 DDL 里没有 `UNIQUE (address)`**（把上面那条实测教训钉住，重新加回会让 failover 重新踩坑）。写守卫时它先抓到的是我自己：`columnNames` 用"第一个右括号"截列，在 `DEFAULT NOW()` 处截断，把 `capabilities`/`joined_at` 漏了。

- **变更窗口声明全链路不生效（治理门禁缺失，实测）**：`window` 块被解析、被规范承诺（`docs/leveelang-spec.md` §4.2 "声明变更只允许在该时间窗内执行"），但**没有任何代码读取它**——`ChangeWindow` 在全仓只有两个字段声明与一个赋值点（`internal/dsl/parser.go:313`），零读取点；`plan.Plan` 不携带窗口，所以 apply 阶段在结构上就无法判定；另一条路 `calendar.CheckWindowForPlan`（注释写着"validates a planned change against the calendar at plan"）**生产零调用**，`CalendarService` 只在 `cmd_calendar.go` 被 CLI 自己构造。实测两条都成立：`start: "25:99"`、`timezone: "Mars/Olympus_Mons"`、`days: ["funday"]` 的工作流 `levee compile` **通过**（val_errs=0）；声明"周日 03:00-04:00"的变更在周二 22:05 走完 plan→approve→apply 全程无提示。错误码 LE020/LE021 在 `internal/errors` 目录表里早已登记，**没有产生点**。
  落地口径按规范来，不是自创语义：新增 `internal/dsl/window.go` 给出窗口文法与判定（HH:MM 严格两位、时区缺省 UTC、days 用 §4.2 的 Mon..Sun 且大小写不敏感、区间 `[start, end)` 含头不含尾、按声明时区的**墙上时钟**判定——那才是运维写窗口时脑子里的意思）；`internal/dsl/validator.go` 补 V19 校验，坏声明在编译期就带上 LE020/LE021/LE003 与出错字段名；`internal/wiring/plan.go` 在解析工作流之后、生成计划之前阻断窗外 plan（规范原话："plan 时刻不在窗口内则阻断，不进审批"）；哨兵 `dsl.ErrWindowClosed` 让 `PlanChange` 报 **FailedPrecondition** 而不是 `Internal`，消息里带上声明的窗口与当前时刻，运维可据此行动。
  **挂载点选 `GeneratePlan` 的理由是它是唯一的 plan 漏斗**：gRPC `PlanChange`、`levee plan --local`、以及 apply 内的 re-plan（`internal/wiring/run.go:311`）都经过它，因此不存在"某个入口能绕过窗口产出计划"；回滚走 `loadStoredPlan` 不经过它，规范"回滚不受窗口约束（即使窗口已关闭回滚仍可执行）"因此天然成立——由 `TestWindowGateHasExactlyOneEnforcementPoint` 钉住：把门禁挪进 `rollbackChange`、或新增第二个 `OpenAt` 调用点，该测试都会变红。
  **规范自相矛盾之处按语义修正并留在文档里**：§4.2 约束表与 V19 都要求 `start < end`，而规范自己的示例用的是"工作日 23:00-02:00 夜间维护窗口"（`docs/leveelang-spec.md:1566`）——跨零点恰是维护窗口最常见的形态，按原约束它根本写不出来。实现改为允许 `start > end`（`days` 标注**开窗的那一天**：周五列在 days 内，则周五 23:00→周六 02:00 属于该窗口，而周日 23:30→周一 01:30 不属于）；`start == end` 才判非法（零长度与全天在声明上无法区分，全天按规范示例写 `00:00`-`23:59`）。LE020 的目录描述同步由"start >= end"改为"window empty (start == end)"，§4.2 与 V19 一并更新。
  **无法判定 = 关闭**：plan 路径不跑 validator，坏声明可以直达门禁。`OpenAt` 对不可判定的声明返回错误并判定为"关"——把"判不了"默认放行，等于让一个写坏的窗口重新变成没有窗口。
  **内嵌 `time/tzdata`**：仓库的 `dist` 阶段是 `FROM scratch`，镜像里没有 `/usr/share/zoneinfo`，而门禁是失败关闭的；不内嵌的话，在 scratch/distroless 类宿主上运行二进制的部署，所有声明非 UTC 窗口的变更都会被无理由拒绝。
  **兼容性与迁移**：① 声明了窗口的既有工作流，第一次在窗外 plan 会开始被拒——这是规范承诺的行为而非回归，需要立即执行的改动要么改窗口、要么在窗口内操作；re-plan（retry）同样受门禁约束，但**回滚不受影响**，失败的变更永远可恢复。② 仓库内两份测试夹具用 RFC3339 绝对时刻写窗口（`internal/dsl/benchmark_test.go`、`internal/plan/integration_test.go`），与 §4.2 的 HH:MM 不符：`plan/integration_test` 的三个用例改前确实因新校验转红，改后绿——**修的是夹具的文法违规，断言一条未动**。
  验证：新增 20 例（`internal/dsl/window_test.go` 12 个用例、其中文法表 11 个子例，覆盖 HH:MM 严格性/边界含头不含尾/跨零点归因到开窗那天/时区真正参与时钟与日期判定/不可判定失败关闭，外加 `TestValidatorActuallyCallsWindowCheck`——守"函数正确≠被调用"这一半；`internal/wiring/window_gate_test.go` 8 例覆盖窗外拒绝且不留下计划产物、窗内正常、无窗口行为不变、坏声明失败关闭、门禁先于库存校验、gRPC 状态码、结构不变量）。**变异验证 12 条全部被抓**（去掉 validator 的窗口校验调用、门禁永不触发、把不可判定当放行、忽略时区改用 UTC 判定、跨零点归因到当天、忽略 days 列表、`end` 改成含入、去掉 FailedPrecondition 映射、放行 `start == end`、去掉时区校验、把门禁挪到库存校验之后、新增第二个 `OpenAt` 调用点／让 `rollbackChange` 重生成计划），每条都使对应用例转红；`go build ./...` / `go vet ./...` / `go test ./...`（67 个包）/ `-tags integration`（9 过 3 跳）/ `-tags e2e` / golangci-lint（0 issues）/ docgen 漂移检查全绿；CLI 实测三种窗口（乱值、RFC3339、规范夜间窗口）判定与文案符合预期。

- **单节点 `serve --engine-enabled` 在首个 dispatch tick 崩溃（P0，README 教的正是这条命令）**：`startDispatchAndWorkerLoops` 的注释声明"两个环都需要 cluster 模式和执行引擎"，但函数里只判了 `engineEnabled`。takeover 的调用点有 `if clusterMgr != nil` 包裹，dispatch 的调用点（`cmd_serve.go:511`）是裸调，于是单节点模式下把 nil 的 `*cluster.ClusterManager` 交给了 leader-only 的 sweep：`DefaultInterval`（10 秒）首次 tick 在 `internal/dispatch/dispatch.go:156` 的 `mgr.GetLeader()` 解引用崩溃（`internal/cluster/cluster.go:213` 对 nil 接收者无守卫），整个 serve 进程随之退出。**影响面是文档推荐路径**：README 第 99 行教运维开启执行引擎的命令正是 `./levee serve --token <secret> --engine-enabled`（不带 `--cluster`），即"照文档开启真实执行 → 约 10 秒后守护进程死亡"；`openServeStore` 在非 cluster 模式明确返回 `(sqliteStore, nil, nil)`，两者组合 100% 复现。**CI 抓不到**：`dispatch` 的用例都配了真 cluster manager，没有任何用例覆盖"单节点 + 引擎开启"这个入口组合，该 helper 此前零测试引用（master 的 `cmd_serve_test.go` 只有 5 例，全在 flag/token 上）。修法：护栏放在函数内而非调用点——契约写在自己的注释里就该自己守住；`clusterMgr == nil` 时不启动两个环，并打印一行 Info 说明原因。新增 `TestStartDispatchLoopsRefusedInSingleNode`：断言 out-param 保持 nil **而不是等 tick**，所以是确定性的（护栏一旦被删，测试在变异检验下直接复现同一 panic 栈）。判据：修复前以 1 秒 dispatch 间隔实跑同一命令 → 12 秒内 100% panic（栈与本节描述逐帧一致）；修复后同一命令连跑 2 轮 × 12 秒均存活，日志为"cross-node dispatch loops not started: single-node mode (run with --cluster to enable)"；`go test -run TestStartDispatchLoopsRefusedInSingleNode ./cmd/levee/` PASS。

- **SSH 取消路径与库的 stdout 拷贝协程争用同一缓冲区（master CI 红：4 × DATA RACE，P0）**：master 的 `test (ubuntu-latest)` 在 step `go test (race)` 失败，日志 4 处 `WARNING: DATA RACE`，两侧栈是写侧 `x/crypto/ssh.(*Session).stdout.func1` → `io.Copy` → `bytes.(*Buffer).grow`，读侧 `SSHChannel.Exec` 的 `stdout.String()`。根因：`x/crypto/ssh` 把 `Session.Stdout/Stderr` 包成 `io.Copy` 协程，而这些协程**只在 `Session.Wait()` 里 join**——正常返回走 `Run→Wait`，所以安全；`Exec` 的 ctx 取消分支从不调 `Wait`，它 `Close()` 之后立刻读捕获缓冲，于是与仍在写入的拷贝协程并发访问文档明确不可并发的 `bytes.Buffer`。影响面正是运维会踩的那条路：取消或超时一个正在执行的变更时，落进审计的输出是未定义值（最坏 panic）。修法：捕获缓冲由 `bytes.Buffer` 换成 `syncBuffer`（`Write`/`String` 各持一把互斥锁）；`x/crypto` 只把它当 `io.Writer` 用，改类型不改变库语义，正常路径一并受益；注释如实写明互斥只让访问良定义、取消后拿到的仍是"当时已到达的部分"，不假装输出变完整。既有 `TestSSHChannelExecCancelled` 抓不住它——它用**预先取消**的 context、注释还写着"成功或错误都可接受"，可能根本不进入那个分支，输出也只有 `echo hi`。新增确定性守卫：mock 服务器加 `mock-stream`（约 1 秒流式输出），新用例在 120ms 取消、确定落在流中间，断言 `context.Canceled`、耗时 <900ms 且已到达的输出被返回；另加 `TestSyncBufferConcurrentAccess` 并发压 `Write`/`String` 并校验内容完整。判据：`go test -race ./internal/channel/ssh/ -count=5` → exit 0、0 DATA RACE；`./internal/channel/... -count=1` 四包全绿；**变异检验**（摘掉两处加锁）→ exit 1、DATA RACE 计数 33，且两条新用例同时红。同形状的 `internal/channel/local` 已排查**无此问题**（先 `proc.Run()`，`os/exec` 的 `Wait` 会 join 拷贝协程，取消也走这条路），故该缺陷是 ssh 独有的。**本机限制**：Windows 工作副本无 gcc、CI 亦刻意跳过 Windows 腿的 race，检测器信号只能在 Linux/macOS 腿取得（本批的 race 判据在 Linux 容器内取得）。

- **执行租约续期测试把 10ms 当契约，实际是在跟时钟赛跑（CI 门禁红，与本次改动无关）**：`TestExecutionGuard_OwnsRenewsLease` 先把 `lease_expires` 压到 `NOW() + 10 毫秒`，再要求 `Owns` 成功。但 `Owns` 的 UPDATE 条件是 `lease_expires >= NOW()`——**已过期的租约被 fence 掉是设计意图**（否则超期的节点可以在接管已经合法之后复活自己的行继续写，正是这个原语要防的双执行）。于是只要一次 postgres 往返超过 10ms（在 CI runner 上很常见），`Owns` 就会正确地返回 `ErrFencedOut`，测试随即失败——**测试失败时功能其实是对的**。这类"只在 CI 出现、本地永远绿"的测试尤其危险：本地没有 `LEVEE_PG_TEST_DSN`，整个套件被 skip，`go test ./...` 全绿提供不了任何信号。修法：把窗口放宽到 2 秒（远大于任何往返、远小于 30s ttl，续期依然可观测），并把断言改成**读回 `lease_expires` 比较续期前后的差值**（≥25s）——这才是"Owns 会续期"的真正含义，且与两台机器的时钟偏斜无关。同时补上该测试一直在无意违反的另一半契约：新用例 `TestExecutionGuard_OwnsFencesExpiredLease` 断言已过期租约**必须**被 `ErrFencedOut` 拒绝、且行不能被那次被拒的续期悄悄延长。验证：本机起 postgres:16-alpine 实跑该套件 3 轮 × 9 例全绿（修复前这些用例本地根本不会执行）。

- **run 状态词表的最后几份手工副本（D-3 收尾：CLI / metrics / proto 一律改为引用 `runstatus`）**：D-3 把词表收进 `internal/runstatus` 时，`engine` 与 `metrics` 以"各有独立词表"为由豁免，CLI 与 proto 注释则从未进入扫描范围——而这几处恰恰是会真正误导人的副本：① `isRollbackableStatus`（CLI）比服务端 `RollbackChange` 守卫**多允许 `running`**，于是对运行中的 run 执行回滚不会本地快速失败，而是一路打到服务端再拿回 `FailedPrecondition`；`levee rollback` 的帮助文本与报错文案同样手抄了这份清单。② `isTerminalStatus`（`levee logs -f` 的退出条件）漏掉 `rejected` / `archived` / `interrupted` 三个终态，follow 模式会一直等一个永远不会变化的 run。③ `metrics` 的标签常量自己拼了一份 `rolled_back_partial` / `rollback_incomplete`（它的豁免理由"标签是独立词表"只对 `created` / `succeeded` 成立）。④ `proto/levee.proto` 的 `Change.status` 注释把 `internal/grpc/change_service.go` 写成权威来源，而词表早已搬到 `runstatus`——集成者按注释追过去看到的是状态机而不是词表。修法：CLI 两处判定改调 `runstatus.InRollbackAdmitted` / `IsTerminal`，帮助文本与报错由 `runstatus.JoinRollbackAdmitted()` 拼装（**帮助文本不可能再与门禁矛盾**），`cmd_audit_report` 的统计分支与 `grpc_change_executor` 的结果映射改用 `runstatus` 常量（顺带区分清楚了 case 标签是 run 状态、返回值是 assignment 结果词表），`metrics` 标签常量改为别名 `runstatus`，`takeover` 的裸字面量改用常量。守卫同步加码：`TestNoBareRunStatusLiteralsOutsideRunstatus` 去掉 `metrics` 豁免、把扫描范围从 `internal/` 扩到 `cmd/`（CLI 三个文件因此进网），新增 `TestProtoStatusCommentMatchesGo` 把 proto 注释里的状态集合与 `runstatus.All` 逐值比对（少一个 = 契约漏项，多一个 = 退役值回流）。测试：runstatus 守门 10 例全绿；`metrics` 标签断言不变（别名同值）。**注意**：proto 注释是 `.proto` 与 `.pb.go` 的配对手改（本地无 `protoc`），逐行镜像，由 CI `proto regenerate check` 仲裁。

- **AI 对话确认后停在文字状态，没有变成变更（P0，AI 闭环：recommend → 草稿 Change）**：审核阶段回复「执行」此前只记录一次批准并如实回复「建议已确认，尚未启动执行」——这是 D-5 v2 移除"假执行"后的诚实退化，但对话与变更之间没有任何通路，闭环缺最后一跳，运维仍要照着建议手工 `levee change`。修法：新增 `internal/conversation/change_bridge.go`——确认后把 `Recommendation.WorkflowDraft` 交给**既有的 `ChangeService.CreateChange`**（适配器 `grpc.NewConversationChangeCreator`，`levee serve` 装配真实 change 服务），产出 `draft` 状态的变更，从此进入标准治理链（计划 → 审批 → 应用）；**桥自身不执行任何工作流、不跳过审批**。三条约束：① **fail-closed**——草案先过与 `levee compile` 严格模式相同的两道门（`dsl.NewParser().ParseBytes` + `dsl.NewValidator().Validate`，其中含 LE097 的回滚归属规则），不通过则**一条变更记录都不建**，把具体错误回给操作员并把会话留在 `reviewing`（模型生成的文本一旦建出变更行，那行看起来可操作却永远无法 plan）；② 变更携带完整溯源（`recommendation_id` / `target` / `risk_level` / `requested_by` 写入 run params，审计不依赖 label）；③ 交接后置 `done`，后续由 CLI/REST 命令推进，不自动发起审批或应用。未装配桥的部署保持原诚实文案，不会谎称已提交；`levee converse` 也接上了桥（懒打开 store，见下）。测试：新增 `change_bridge_test.go` 6 例（成功路径断言产出的**确实是 draft**；空草案 / YAML 语法错误 / workflow 级回滚声明三种拒绝面均断言**零次** `CreateChangeDraft` 调用且会话留在 reviewing；创建失败透出真实原因且可重试；`执行` 分支确实走桥；未装配桥时不谎称已创建），以及**端到端**的 `internal/wiring/inline_change_plan_test.go`：真实 SQLite + 真实 `ChangeService` + 真实适配器跑完「确认 → 落库 → 计划期解析器 → 生成计划」，并断言被拒绝的草案在 store 里**一条记录都不留**（这条路径正是单测结构上看不见的：对话产出的工作流以**内联 YAML** 落在 run 行里，而计划期 `wiring.resolveWorkflow` 既接受内联也接受文件路径，两端不一致时故障表现为"变更看起来正常、直到计划时才炸"）。适配器因此从 `cmd/levee` 移到 `internal/grpc`——它本就该贴着被包装的服务，且 `internal/grpc` 已依赖 `internal/conversation`，端到端测试得以用真实适配器而不是再造一个测试替身。**CLI 侧补齐同一闭环**（`cmd/levee/conversation_change_bridge.go`）：此前只有 serve 路径接桥，同一个「执行」回复在两个入口含义不同。CLI 的约束是生命周期——命令只在运行期间拥有 store，且 `--list` / `--history` / 单纯提问**根本不需要**数据库，所以 store **懒打开**（第一次确认建议时才开），并通过新增的 `ConversationEngine.AddCloser` 把所有权挂到引擎上（`runConverse` 本来就有 `defer engine.Close()`）：只读命令一个文件都不碰，命令结束不留 SQLite 句柄，测试无需真实数据库。store 打不开时错误按普通创建失败返回，对话如实告知、环境修好后可重试；且**失败被记住不重试**，避免一次环境问题被放大成每次确认都重复报错。测试：懒加载 3 例（构造时零次打开、首次确认后只开一次、失败被记忆）+ 引擎资源接管 1 例（逆序关闭、失败不被吞、二次 Close 幂等）。

- **工作流级 `rollback` 是"声明了但什么都不做"（P0，方案 B：分层治理 + LE097 门禁）**：LEVEELang 允许在 workflow 顶层写 `rollback { strategy / step / steps / snapshot_paths }`，它被解析、写入 plan、计入 `plan_hash`，却**没有任何执行路径读它**——补偿只按 `PlanStep.Rollback` 归因（`rollback.Manager` 的账本键是 `(host, 前向步骤)`，`snapshotter` 采集与快照能力门禁 `planNeedsSnapshot` 同样只看 step 级）。后果是双重的：顶层声明了快照的 workflow **不会**在 apply 前采集、也**不会**因缺少 `--engine-snapshot-dir` 被 fail-closed 拒绝（D-2 v2 第 5 项设计在这里有缺口），而顶层声明了 undo 清单的 workflow 回滚时每个已执行步骤都变成"补偿缺口"——即使作者按当时规范认真写了回滚计划。**决策：补偿契约不落 `PlanStep`，而是把归属规则变硬**：① workflow 级 `rollback` 只允许运行态策略 `on_failure` / `verify_after`，声明 `strategy` / `step` / `steps` / `snapshot_paths` 一律编译期拒绝（新错误码 **LE097**，规则单一实现在 `internal/dsl/rollback_scope.go`，validator 与 plan 生成器共用）；② 两处门禁都守这条线——`dsl.Validator`（`levee compile` 路径）与 `plan.Generator.Generate`（**服务端 plan 路径**：`wiring.GeneratePlan` 解析后直接调用生成器，从不经过 validator，这道门才是真正保护执行的）；③ `on_failure` 第一次真正生效：`manual` 抑制自动回滚，失败的 run 保留已应用批次并标记 `ClosureResult.ManualRollbackRequired`，等待操作员 `RollbackChange`；缺省 / `auto` / 门禁上线前的遗留值（如 `abort`）一律保持历史行为（自动回滚），避免存量 plan 静默失去回滚；④ dry-run 预览的"无回滚计划"告警改按 step 级声明判定，不再被顶层策略块安抚。**为什么不做投影**：全局 undo 清单复制到每个 step 会让同一效果被撤销 N 次或整份漏掉；`snapshot_paths` 复制到每个 step 会在后续步骤改写文件后再次采集，恢复出的不是 apply 前的基线；合成为末尾"伪 step"则污染批次/主机拓扑与因果归属。测试：`internal/dsl/rollback_scope_test.go`（接受面/拒绝面 + 与 `internal/errors.LE097` 的字面量一致性）、`internal/plan/rollback_scope_test.go`（生成器门禁与运行态策略透传）、`internal/engine/rollback_policy_test.go`（manual 抑制且零补偿指令、auto 与遗留值仍回滚、两种情形下锁都被释放）、dryrun 新用例（顶层策略块不再等同于回滚计划）。规范同步：`leveelang-spec.md` §2.1 / §7.1 / §7.2 / §8.1（新增 V24 行并订正 V16）/ §8.2 / 附录 C 全部改为两层归属，正文 4 个示例、附录 4 个示例与 `levee-design.md` 的示例把补偿移入被补偿的 step。

- **规范里的三张词表与代码不一致，其中一张会让运维等一个永远不会来的驳回（生成式文档，`internal/docgen` + CI 门禁）**：D-3 把词表收进 Go 包之后，文档仍需人工同步——实测发现 `docs/leveelang-spec.md` 的**审批级别表有三列与代码不符**：`emergency` 超时写作 `15min`（代码是 `30min`）、`standard` 与 `high` 的超时处理都写作「超时驳回」（代码里 `standard` 是 **notify 并保持 pending**、`high` 是**升级到 emergency** 重新计时）。`SetConfig` 无任何生产调用者，故默认值即实际行为——**照表操作的运维会一直等那个不会来的驳回**。批次策略表也仍列着三个 `plan` 生成器从未实现的策略。修法：新增 `internal/docgen`，从代码生成三张表（run 状态 / 批次策略 / 审批级别）到 `BEGIN/END GENERATED` 标记之间，标记外的手写散文一律不动；`go run ./internal/docgen` 重写，`-check` 只校验，CI 新增 `docs regenerate check` job（照抄既有 `proto regenerate check` 的形状：`go run` → `git diff --exit-code` → `-check` 复验）并接入聚合门禁 `check (all jobs passed)`。生成表与手写表的关键差异是**多出两列由准入集合派生的判定**：「可 RetryChange 再驱动」「可 RollbackChange 撤销」直接读 `runstatus.RetryAdmitted` / `RollbackAdmitted`，文档不可能再声称某个状态可重试而准入门禁实际拒绝。测试 7 例覆盖幂等性、缺标记必须报错（静默失效的生成器与它要防的漂移同构）、每个状态/策略都必须有释义（否则渲染出占位符而被 review 漏掉）、双重撤销规则、以及生成器自身的两个 bug——`humanDuration` 曾把 24h 除以 24h 却打印 `h` 后缀，**把标准档的超时窗口在文档里砍半**；还有一次是我把 `Duration.String()` 的输出断言写错（`90s` 实为 `1m30s`），由测试直接判失败。

- **run 状态词表有五份手工副本，其中三份已经不一致（D-3 词表单一来源，run 状态）**：词表原本只以字符串字面量散落在状态机、gRPC 准入守卫、`WatchChange` 终态集合、`engine.ClosurePhase`、metrics 标签、proto 注释、CLI 终态矩阵与 Web UI 联合类型里——**没有任何一处是权威来源**，于是每新增一个状态就要改八处，漏改不报错、只是功能静默失效（`pending_approval` 就是这么让待审批页签和移动端批准按钮长期查空的）。修法：新增 `internal/runstatus` 作为唯一权威（15 个状态常量 + `Terminal` / `RetryAdmitted` / `RollbackAdmitted` / `RollbackVerdicts` 四个派生集合 + `IsValid` / `IsRollbackVerdict` 等判定），`change_service.go` 的 retry/rollback 准入守卫、终态判定、归档迁移、`terminalRunStatuses` 与 `WatchChange` 终态集合全部改为引用它，**连面向运维的错误文案也由词表拼装**（原来是第四份手抄）。**澄清一处差点合并的概念**：`grpc.terminalRunStatuses` 回答的是"迟到的审批能否覆盖这一行"，因此**含 `approved` 与 `running`**，与生命周期终态集不是一回事——两者过去都叫 terminal，`runstatus` 把它们显式分开并各有测试。跨语言守卫（`internal/runstatus/vocabulary_guard_test.go`，随 CI 常规 test job 运行）：`TestWebStatusMirrorMatchesGo` 逐值比对 Go 词表与 `web/src/types/levee.ts` 的 TS 联合类型（少一个 = UI 无标签，多一个 = 提供一个永远匹配不到的筛选项）、`TestSpecStatusTableMatchesGo` 校验 `leveelang-spec.md` 0.3 节的状态表、`TestNoBareRunStatusLiteralsOutsideRunstatus` 扫描生产代码里残留的裸状态字面量（`engine` / `metrics` 各有独立词表，按设计豁免并由对应性测试覆盖）。`engine.ClosurePhase` 与 run 状态的字符串对应关系由 `TestClosurePhasesMapToRunStatuses` 钉住——它会被 `wiring` 直接写成 `run.status`，漂移将产生一个 UI 无标签、准入门禁拒绝、只能靠手工抢救的状态。词表完整性另有 5 例：无重复、已废弃值被拒（`pending_approval` / `count` / `by-tag` / `by-group` / `all` / 空串）、双重撤销防护、干净回滚与部分回滚的区分、派生文案。**实施中的两次自我纠正**：① 一度按注释推断 `RollbackAdmitted` 不含 `completed`，查实际守卫后发现相反（成功的变更**可以**被回滚，只有已恢复的 `rolled_back` 不行）——注释不可当真相；② 一度断言 `rolled_back` 不可 retry，测试直接判失败，实际它是合法再驱动入口。

- **`batches.strategy` 在两层各有一份互不相容的词表（P0，D-3 词表单一来源第一项）**：`dsl` 解析器接受 `percent / one-per-target / count / by-tag / by-group`，`plan` 生成器实现 `percent / fixed / serial`——**交集只有 `percent`**。后果有两层：① `fixed`（生成器唯一支持、validator 头注释声明为权威的策略之一）过不了 parser，`plan/integration_test.go:447` 那条测试只能**手搓 AST 绕开 parser** 来覆盖它，并在注释里把漂移记录成既成事实；② `one-per-target` 过得了 parser 却在生成器里没有 case，规划这类 workflow 直接 **Fatal LE034**——而它不是废弃值：`examples/gate-templates/mysql.yaml` 真在用，`leveelang-spec` 专门写了它的语义（「每批一台目标机，串行」，DB 主库逐个切换），并发批次意味着同一集群两个 DDL 窗口。差点把它当漂移删掉造成真实回归。修法：词表收进 `dsl.BatchStrategies` / `IsBatchStrategy` 单一来源（`percent` / `fixed` / `serial` / `one-per-target`），解析器与生成器都引用它，错误信息也由该列表拼装（原来两处各抄一份字面量）；**补齐 `plan.splitOnePerTarget` 实现**（每批一台，严格串行，`steps` 按规范不使用），使规范与示例模板从"能解析但规划即 Fatal"变成真正可跑。新增 `TestBatchStrategyVocabulariesAgree` 逐个策略断言「能解析 ⇒ 一定能生成」，外加 `TestOnePerTargetSplitsStrictlySerially` 钉住串行语义——今后任何一侧单边改动都由 CI 拦下，而不是等到生产。

- **AI 修复建议产出的是第三种方言，且被一个绕过治理链的影子包遮住（P0，删除 `internal/autoplanner`）**：`recommend.WorkflowGenerator` 的文件头**声称**「生成的 YAML 遵循 LEVEELang schema」，实际发出的是 `name/description/target.hosts/window{batches,approval}/batches[命名批次]/顶层 rollback 列表`——既不是规范里的形状，也不是 parser 接受的形状：实测 `dsl.NewParser().ParseBytes` 直接报 `cannot unmarshal !!seq into yamlBatchesRaw` / `yamlRollbackRaw`。**这份草稿永远无法进入治理链**（`PlanChange → plan.NewGenerator → plan_hash → 审批` 是唯一入口），而 `recommend.WorkflowDraft` 是 `conversation` 会话批准后唯一可交付物——即"AI 建议已确认"到"可执行变更"之间**根本没有桥**。遮住这个事实的正是 `internal/autoplanner`（2,766 LOC，生产代码零引用）：它自带一套与 `plan` 重复的影子领域模型（自己的 `Workflow`/`Step`/`Batch`）、手写行解析器去读那份坏 YAML、以及一个 `AutoExecutor`——**后者是一条平行执行路径，`ModeForce` 的注释声称「caller must hold the elevated permission」而包内零权限检查**，既不过 plan_hash 也不过审批与执行账本。把它"接线"等于装上与 `58ff991` 同款的 P0 后门，故**整包删除**而非接线。真正要补的桥是生产者侧：`WorkflowGenerator` 改为产出**真正可解析的 LEVEELang**（顶层 `steps:`、`batches` 为 map、rollback **step 级内嵌**——因为补偿账本正是按 step 归属归因、`target` 用 `type`+`hosts`；`target.query` 虽在校验器里查非空但**无解析器**，真正执行的目标集来自 `PlanChangeRequest.target_hosts`，不来自文档）。同时修掉一处**假装补偿**：`reverseAction` 对未知动作返回 `"noop"`，于是该 step 的 `rollback != nil`、补偿缺口检查通过，而实际什么也没撤销——正是 D-2 要消除的"状态没恢复却报干净"；改为返回 `ok=false`、**不生成 rollback 块**，让该 step 成为诚实的补偿缺口由判定报出。旧测试曾把 `"unknown" → "noop"` 钉为期望行为，一并改写。测试：草稿可解析性 4 个子用例、rollback 归属与"绝不 noop"、不发明逆动作；`dsl`/`plan`/`recommend` 三包全绿。

- **回滚补偿的"能不能重复跑"从未被判定过，且该声明根本没进哈希（方案 C，收口 D-2 v2 补偿幂等的剩余面）**：上一批把"第二次补偿"按**完成时间定序**挡住了，但刻意留了一个安全方向的兜底——证据无法定序（前向行缺完成时间戳）时**再补偿一次**。这个兜底此前是无条件的，等于默认假设所有 undo 步骤都可重复执行，而 DSL 里其实**一直有**作者写下的 `idempotent: true` 声明（`parser.go` 早就解析、IR 也带），只是**全仓没有任何代码读它**。更要紧的是：即便现在拿它当门禁也不成立——`canonicalRollbackStep` 只覆盖 `Name/Module/Action/Args`，**undo 步骤的 `idempotent` 根本不在 v2 治理哈希里**，于是"批准后把非幂等声明改成幂等"可以零成本绕过（这正是 P1-1 建立 v2 要堵的那类漂移）。修法两步：① 把 `Idempotent` 纳入 `canonicalRollbackStep`（`omitempty`，既有 plan 哈希字节不变，D-6 旧树 golden 仍通过），声明从此受哈希绑定；② 账本新增第四态 `Uncertain`（`CompensationUncertain`）——**只**在"确有旧补偿成功记录、但无法与前向执行定序"时置位，并在 `rollbackTarget` 里于 `NotExecuted`（未跑过=良性）之后判定：未声明幂等的 undo **拒绝重跑**并记为**缺口**（`NotExecuted=false`——与"良性跳过"相反：我们确实不知道状态是否已恢复，判定不得读成干净），声明了 `idempotent: true` 的才允许重跑；快照策略按构造可重复（写回同一份捕获）豁免，一个未声明的 undo 步骤即让整条补偿不可重复（**部分重复比不重复更糟**）。门禁顺序另有专门用例钉住：未执行步骤即使被误标 uncertain 也必须报 `NotExecuted` 而非缺口。存量影响为零——正常写入路径的前向行都带完成时间戳，定序成立即不查该字段，只有手工/历史/残缺证据才会走到这个角。对存量 workflow 的建议（**非破坏性**）：确实可重复的 undo 步骤补一行 `idempotent: true`，从此在证据残缺时也不必被拒。测试：门禁 5 例（拒绝/放行/无不确定时照常/顺序钉住/可重复性判定表）、推导层 2 例（定序失败→置 `Uncertain`、无旧补偿→不置）、哈希治理覆盖新增 1 个变异用例。

- **手工事后回滚重复执行已补偿的 undo（D-2 v2 挂账项收口，补偿幂等）**：`RollbackChange` 准入 `rolled_back_partial` / `rollback_incomplete`（"自动补偿留了缺口，把剩下的补完"），但手工回滚的执行账本只从前向 `success`/`failed` 行推导 Ran/Unknown，**从不过问该步骤是否已被补偿过**——于是补救路径会把第一次已经补偿成功的步骤**再补偿一次**。对幂等 undo（`systemctl stop`、设副本数、从备份恢复）无害，对非幂等 undo（追加一行、计数器 +1、建工单、置 metric）就是**二次副作用**，且证据表里只会多出一条 undo 行，看不出它是重复的。根因不是"同名 undo 步骤误判"（那只是次要歧义），而是 `(host, 前向步骤) → 已补偿` 这条关系**在持久化时被丢掉**：`StepResult.OrigStepName` 的注释明写它是 undo→forward 的审计线索，但 `persistRollbackResults` 写库时只取 `RollbackStepName`，`state.Step` 也没有承载它的列。修法（零 schema 变更、零迁移）：`ExecutionLedger` 增加第三态 `Compensated`（`MarkRan`/`MarkUnknown` 会清掉它，所以重试重执行后的新副作用仍需补偿）；手工账本推导改为「plan（hash 绑定、不可变）给出 undo 步骤名 + 已有 undo 行给出成功记录」联合判定，且**按完成时间定序**——只有完成时间晚于该 host 上最近一次前向执行的补偿才算数（重试会追加新的前向行，旧 undo 行不得被读成已恢复更新的副作用）；缺时间戳无法定序时取安全方向（再补偿一次，因为重复补偿是可见的，错误跳过会静默掩盖缺口）。快照策略的补偿以合成名 `snapshot:<step>` 落库，因此同样可识别，不会重复 restore 同一份捕获。`rollback` 侧新增 `StepRollbackResult.AlreadyCompensated`：这类跳过对判定是良性的（目标状态已恢复），但**单独成标志**而非并入 `NotExecuted`——两者对运维含义相反（"这里从未改过" vs "这里已经撤销过"），其 `SkipReason` 会写进持久化证据行。顺带把同名 undo 步骤从"静默歧义"改为 **fail-closed 拒绝**手工回滚并提示改名或声明幂等。测试：账本三态与 `MarkRan` 清标志 2 例；推导层 4 例（已补偿被标记 / 补偿失败仍重试 / 快照 restore 被识别 / 同名 undo 被拒绝）；端到端 3 例（二次回滚零派发且留证据、失败补偿仍重试、重执行后需再次补偿）。

- **Web UI 状态词表与后端状态机脱节，导致两处功能静默失效（P0，UI 侧收口）**：前端手工镜像的状态词表两次漂移，且都不是外观问题。① `pending_approval` 作为第二个「待审批」键与 `pending` 并存，但后端从不产生该值——「待审批」的 run 状态是 `pending`（`isValidTransition`，`TestInstantiateTemplate_NormalRunPending` 记录了这次替换）。REST 列表按 `run.status` **精确匹配**过滤（`rest.go` → `ListChangesRequest.Statuses`），于是两条活跃路径恒为空：`ApprovalView` 的待审批页签查 `status=['pending_approval']` → 永远列不出任何可审批项；`MobileApprovalView` 的批准/拒绝按钮 `v-if status === 'pending_approval'` → 永远不渲染。`ChangesView` 还把该死值作为筛选项（重复的「待审批」入口，选中即空表）。② D-2 v2 的两个回滚判定在 UI 全线缺失：`rolled_back_partial` / `rollback_incomplete` 没有标签、没有配色（`StatusTag` 退回显示原始状态串与 `info`）、没有筛选项、且不可重试——而 `RetryChange` 恰恰准入这两个状态，UI 把它们唯一的再驱动入口藏了起来。修法：`ChangeStatus` 联合类型改为与 `internal/grpc/change_service.go`（`isValidTransition` / `terminalRunStatuses`）一致（去掉 `pending_approval`，补 `rejected` 与两个回滚判定）；`STATUS_LABEL` / `STATUS_COLOR` 补齐（部分回滚=warning、回滚未完成=danger——**状态没恢复就不能显示成「已回滚」**，那正是 D-2 要消除的误报）；`RETRYABLE_STATUSES` 对齐 `RetryChange` 准入集；两表均为 `Record<ChangeStatus, …>`，联合类型加值而此处漏改会被 `vue-tsc` 直接拦下；三处死值用法改为 `pending`。测试 50 例，新增用例钉住「两个回滚判定必须与已回滚显示不同」「表中不得再出现 `pending_approval`」「标签表与配色表键集一致」。`internal/web/dist` 已重建并随提交入库（Go 二进制内嵌）：产物中已含「部分回滚」且不再含 `pending_approval`。

- **重试 re-plan 绕过审批直接执行（P0，收口 D-1 v2 遗留项）**：`RetryChange` 带 `replan=true` 时在 `internal/wiring/retryChange` 里重新生成计划、直接覆写 `run.PlanJSON/run.PlanHash` 然后进入执行，**全程不校验任何审批**——这正是 D-1 建立的 Apply 门禁要挡的「批准 v1、执行 v2」漂移（P0-1），而且可经重试路径绕过；D-1 实施记录当时就点名了这条路径（`internal/wiring/run.go:322-342`「Retry 的 replan 直接进执行，不经审批」），D-2 只扩了 retry 的状态准入、没有收口。修法：把「审批是否覆盖当前计划版本」的判定上提为 `state.Approval.MatchesPlan`（空 `plan_hash` = legacy 行仍授权任意计划；绑定行只匹配自身版本，且绝不匹配空 run hash），**结算、Apply 门禁、本门禁三处共用同一实现**（原 `approvalPlanMatch` 改为一行委托），杜绝三门禁日后各自漂移；`retryChange` 在派发前查是否存在**已 approved** 的行指向新生成的 hash（pending / rejected / superseded 一律不授权）：命中（精确版本，或 legacy 未绑定行）则照常执行——该 API 不沦为死路；未命中则新计划**持久化但不执行**，run 退回 `draft/pending`、旧版本残留的 pending 审批行标记 `expired`（与 PlanChange 重规划同一处理），并返回 `grpc.ErrReplanNeedsApproval`，由 `RetryChange` 映射为 `FailedPrecondition`（REST 412）而非 Internal，提示操作者去批准新计划。测试：`MatchesPlan` 6 例表驱动；wiring 回归「审批绑定旧版本 + 扩大 host 集合 → 铸出新版本 → 零派发、run 退回 draft/pending、pending 行过期」，外加「审批绑定新版本」与「legacy 未绑定行」两个正例；gRPC 用例钉住 412 映射。同批补 D-2 记录的 proto 注释欠账（`levee.proto:18/624`）：两处状态词表仍列着早已废弃的 `pending_approval`（状态机实际用 `pending`，`TestInstantiateTemplate_NormalRunPending` 记录了这次替换），且漏掉 D-2 v2 的 `rolled_back_partial` / `rollback_incomplete`；现改为与 `internal/grpc/change_service.go` 权威词表一致并注明权威文件。用 pinned 工具链（protoc 27.0 / protoc-gen-go v1.36.12 / protoc-gen-go-grpc v1.6.2）重生成，`internal/grpc/pb/levee.pb.go` 仅有该两处注释变化（+8/−2），二次生成字节一致，CI drift check 保持绿。

- **计划哈希未覆盖审批、回滚、门禁与风险语义（P0，P1-1：v2 治理感知哈希 + v1 兼容）**：旧 canonical hash 只覆盖 workflow 名、目标、批次、step 参数和影响面，明确排除了 step 的 Rollback/Approval/Gate；Plan 顶层的 RiskScore/RiskFactors/ApprovalFloor 虽已持久化却没有 hash 字段。因此审批人/人数、回滚命令、快照路径、门禁命令、不可逆判定或风险结论在批准后被改写，`plan_hash` 仍可能通过。修法：新计划 hash 升级为显式版本化的 `v2:<sha256>`，覆盖 workflow/step/batch 三层治理声明、不可逆判定、风险分/因子/审批下限；审批人、快照路径、风险因子按集合排序，执行顺序保持不变。Generator 同步把 workflow Approval/Rollback/Gate 与 batch gate 写入 artifact，kickoff 直接消费 hash 绑定的 Approval，path-shaped workflow 不再旁路解析失败。`VerifyHash` 保留裸 64 位 v1 验证路径，gRPC/wiring 存量计划继续可执行；用 D-6 旧树计算固定 golden hash 防 canonical 漂移。未知版本/畸形 hash fail-closed。旧计划要获得 v2 覆盖必须 re-plan/re-approve，文档明确该升级边界。
- **发布可在无 CI 结论时直接开始（P2，D-6 v2：tag CI + 精确 check-run 门禁）**：`v*` tag 过去只触发 `release.yml`，CI workflow 仅监听分支/PR，因而 GoReleaser 可以在测试、lint、build、安全扫描尚未运行或已失败时发布。本次 `ci.yml` 增加 tag 触发并新增 `release-gate` 策略单测；`release.yml` 在打包前运行 `scripts/require_ci_release.py`，按 **tag 名 + 完整 commit SHA** 定位本次 CI workflow run，再校验该 run 的 GitHub Actions 汇总 check `check (all jobs passed)` 为 `completed/success`。旧分支构建的同 commit 成功检查、错误 app 的同名 check、重复 check 中较旧的 success 均不能冒充本次门禁。CI/check 缺失、仍在运行、失败、跳过、超时，以及 GitHub API/权限/JSON 错误全部 fail-closed；`goreleaser` 仅在 gate 成功后启动。标准库单测覆盖成功、pending→success、CI/check 失败、缺失超时、API 错误、旧 run/check、重复 check 与错误 app 共 11 个场景。
- **对话前端/后端契约错位 + 批准假执行（P2，D-5 v2：前端单边对齐 + 会话所有权 + 确认不执行）**：① 契约错位——REST 会话端点返回信封（`{session}` / `{sessions}` / `{reply}`），但前端 `getSession`/`sendMessage` 按裸 DTO 解析且 reply 还按扁平 `action_type` 读，实际拿到 `undefined`——会话详情刷新与消息回复全部静默坏掉，mock 又与前端同错所以测试全绿；修法为前端单边适配（后端信封是既定 API）：`conversationApi` 统一拆封 `{session}`/`{reply}` 并把引擎线形 `{text, action:{type,payload}}` 摊平为视图消费的扁平 DTO，契约测试（axios adapter mock）改为后端真实线形。② 会话无所有权——会话端点直接信客户端断言的 `user_id`，任何人可读/改/关他人会话；修法：命名令牌 / OIDC / SSO 的认证主体（`actorKey`）一律覆盖请求中的 `user_id`（gRPC `SendMessage` 同规则），详情/发消息/关闭校验归属、跨主体 403（引擎侧新增哨兵错误 `ErrNotOwner` → gRPC `PermissionDenied`）；无认证开发模式与旧式静态令牌回退请求 `user_id` 保持可用。③ 批准假执行——审核阶段回复「执行」直接置 `executing` 并答复「开始执行」，但没有任何执行器接管，会话烂在 `executing` 态；修法：确认后保持 `reviewing`，文案改为「建议已确认，尚未启动执行」，执行链接通前 `executing` 不可达（状态与处理器保留作防御路径）。测试：引擎新增「确认保持 reviewing 且可再拒绝」用例并更新 10 处旧断言；gRPC 新增主体覆盖与跨用户 PermissionDenied 用例；REST 新增所有权测试文件（主体覆盖断言 user_id、跨用户读/写/关 403、insecure 回退）；前端契约测试 5 用例全量对齐真实信封；`docs/levee-api.md` 新增 §13.7 对话会话端点契约。
- **集群 assignment 不回收：worker 分派后死亡则 run 永久卡死（P1，D-4 v2：claim timeout + epoch fencing + 容量口径）**：dispatch 把 run 交给某 worker 后写入 `pending` assignment，若该 worker 在**领取之前**死亡（或停止轮询），这一行永远停在 `pending`：`candidates` 视其为「有活跃 assignment」而永久跳过该 run，takeover 只处理已过期执行租约（pending 没有租约）也看不见它——run 既执行不了、也不会被重派，只能人工改库；同一缺陷的另一面是 `workerLoads` **只统计 executing**，pending 这类「已承诺工作」不计入容量，leader 会在节点队列未清空时继续超发（P1-6）。修法（零 schema 变更）：
  - **claim timeout**（默认 10m，`levee serve --cluster-dispatch-claim-timeout` 可配）：leader 每轮扫描**先回收**超过该时长仍未领取的 pending assignment——10m 远大于 CAS→Begin 临界区与 worker 轮询间隔，健康集群永不触发，只把「worker 半路死亡」的暴露时间从「无限」压到「一个超时窗口」。
  - **回收即重派 + epoch fencing**：新 store 原语 `ReclaimAssignment` 在 `(run_id, epoch, state='pending')` 上 CAS，成功后 `epoch+1` 并把 owner 指向当前负载最低的活跃节点；迟到 worker 的领取 CAS 仍比较旧 epoch 因而失败——**不双执行**（与既有 `Reassign` 的区别：后者只比 epoch、不锁状态，用于终态行重派，拿它回收会把已领取的 executing 行拖回 pending）。
  - **孤儿行终止**：run 已不在 `approved`（暂停 / 重 plan 回 draft / 终态）时，回收不重派而把 assignment 置 `interrupted`（终态），该行随即停止计入负载；后续重新批准经 `Reassign` 以 `epoch+1` 重新开始，旧 epoch 不会复活。
  - **容量口径修正**：`workerLoads` 改为「只排除终态（done/interrupted）」，pending 计入负载；`sortByLoad` 单点化并被回收与配对共用，保证重派始终落到负载最低节点。
  - 留痕：metric `levee_dispatch_attempts_total{result=reclaimed}` + `stale pending assignment reclaimed` 日志（含 stale_owner/new_owner/epoch/age）。
  - 测试：dispatch 新增 5 用例（超时收敛到活跃节点、迟到领取被 epoch 拒绝且当前 owner 仍可领取、未超时不动、非 approved run 转 interrupted、负载计入 pending + 容量不再超发）；state 新增 `ReclaimAssignment` SQLite CAS 用例 2 个与 PG 用例 1 个（PG 套件内跑）；`docs/design-cluster-dispatch.md` 补 6.1 节与失败处理表行。
- **PG 备份能生成不能恢复（P1，D-3 v2：一致性快照 + 可恢复回放）**：PostgreSQL dump 此前在连接池上逐语句执行——每张表可能读到不同提交点的快照，外键引用链可在 dump 中间断裂；且按字母序写 `DELETE/INSERT`（子表先于父表，恢复到带 FK 的正式 schema 必然违约）；`DELETE FROM runs` 的级联又会触发 trace 的 WORM 删除守卫，使恢复到已有库必然失败。净效果："能备份、不能恢复"。修法：
  - **一致性快照**：dump 固定在单一连接的 `REPEATABLE READ READ ONLY` 事务内执行全部目录与数据读取，并持有与 `state.pgMigrate` 相同的 schema advisory lock（770_001）——备份内部一致且绝不跨越迁移中点。
  - **FK 拓扑序**：dump 从 information_schema 读取外键依赖并拓扑排序（父表在前，字典序 tie-break 保证确定性，头注释标注顺序）；依赖环（含自引用 FK）检测后明确报错，绝不静默产出错误顺序。
  - **恢复主路径 = 全新 schema**：恢复前先把目标库迁移到当前版本（空库从零重放 pgschema.sql + 迁移步骤，已有库补挂起步骤），再按**目标库实时外键图重排**的语句组回放——v1.13 及更早的字母序旧备份同样可恢复；`schema_version` 内容永不回放（版本行属迁移器，回放旧行会让非幂等 ALTER 在下次启动重跑）；dump 里已被新 schema 删除的表保留为末尾的存根表组，数据不丢。
  - **已有库恢复默认拒绝 + 显式 opt-in**：目标库非空（schema_version 除外）时主路径拒绝并列出持数表；`levee restore --allow-destructive-restore`（API `backup.RestoreOptions.AllowDestructive`）才走灾难恢复路径——单事务内 `DISABLE TRIGGER USER`（含 WORM 守卫，仅需表属主权限，外键约束保持生效）→ 回放 → `ENABLE TRIGGER USER` → 校验两个 WORM 触发器在位后才提交，任何失败整体回滚。
  - **CI 恢复演练门禁**：integration job 新增独立步骤运行 `./internal/backup` PG 套件；`TestPostgresBackupRestoreDrill` 在真实 PG 上完成「建库+造数 → dump → 全新库主路径恢复 → 逐表行数+内容指纹对比 → 非空拒绝 → opt-in 破坏恢复 → WORM 触发器行为验证」全链路（演练自建自删 scratch 库，不碰共享 CI 库）。
  - 测试与文档：backup 包新增拓扑排序/语句重排/环检测/非空检测单测 12 个；cli-reference 第 28 章与 deployment 第 10 节同步 PG 恢复语义。
- **回滚范围越界：从未执行的工作也被"补偿"（P0，D-2 v2：执行证据账本）**：自动回滚拿到的是**原计划结构**（按计划的批次/目标/步骤走），批次中途失败时从未 dispatch 的目标/步骤照样被补偿——把撤销打到了从未改变的状态上；手动 `RollbackChange` 同病（整计划盲撤）。修法：新增 `rollback.ExecutionLedger`（每 (target,step) 两问：是否 dispatch、失败与否），闭包路径从已有的 `BatchResults` 派生（batch controller 只为真正 dispatch 的步骤记 `StepResult`——证据天然存在，零 schema 变更，设计第 1 项）；`RollbackWithLedger(ctx,p,execFn,ledger)`（nil=旧行为，直接/无证据调用方兼容），未执行步骤记 `NotExecuted` 良性 skip 且不计入判定；手动路径 `ledgerFromStoredSteps` 从 steps 表前向证据派生（success→ran、failed→unknown、skipped=恢复标记忽略；undo 行按步骤名自然排除），无证据=空账本=不补偿（never-applied 手动回滚变干净 no-op），证据读取失败 fail-closed。钉定测试：mid-batch 只补已 dispatch（P0-3）、手工构造部分执行只补有证据主机、nil 账本兼容旧行为。
- **回滚结果语义：判定基于"状态恢复"而非"流程走完"（P0，D-2 v2 设计第 3/4 项）**：`RollbackResult` 新增 `RequiredCompensations/CompletedCompensations/UnknownSideEffects`；skip 拆两类——`NotExecuted`（前向未执行，良性）与"已执行但缺补偿"（无 rollback 声明 / 不在白名单 / snapshot 接线缺失——计入缺口）；**`Success = Error == nil && Required == Completed`**：缺口即使零命令错误也判非成功。`UnknownSideEffects`（dispatch 过但前向失败的步骤数）按设计«可持续»条目为**信息性扩展点**，不翻转判定。闭包三相判定→阶段/状态映射（增量，不删旧值）：全部必要补偿完成→`rolled_back`；部分完成→`rolled_back_partial`（新）；零完成→`rollback_incomplete`（新）。两新状态贯穿全部门禁：结算终态保护、WatchChange 终态、archivable、RetryChange 准入（失败家族再驱动）、RollbackChange 准入（=补救入口，干净 rolled_back 仍拒绝防双重撤销）、CLI 终态/可回滚矩阵、metrics 标签预注册；assignment 结果词表不扩展（新状态在 dispatch 层按非干净结局记 `failed`，run 行保留精确判定）。**有意语义变化**：失败步骤无补偿声明的强制失败演练从 `rolled_back` 改判 `rolled_back_partial`——状态未恢复不得宣称干净回滚（P0-4 的核心）。
- **snapshot 回滚计划无快照能力时静默降级（P1，D-2 v2 设计第 5 项）**：声明 `strategy: snapshot` 的计划在未配置 `--engine-snapshot-dir`（或 store/manager 不可构建）时照常执行，回滚时才发现恢复基准不存在、以 "not wired" skip 带过——正向状态已改变却无从恢复。修法：`executePlan` 派发前 `checkSnapshotCapability` **fail-closed 拒绝**（dir + store + manager 构建三关全过才放行），把缺口从"回滚中发现"提前到"apply 前拒绝"。
- **手动回滚证据读取失败 fail-closed（D-2 v2 加固）**：`ledgerFromStoredSteps` 读 steps 表失败时报错中止，绝不回退到"整计划盲撤"（那正是本批修掉的越界行为）。
- **升级/回归测试**：rollback 新增 6 用例（证据派生、未启动不补偿、unknown 信息性钉设计公式、缺口、部分失败、nil 兼容）；closure 重写 mid-batch/全失败两用例并新增部分补偿用例；wiring 新增手工构造部分执行、never-applied 安全、snapshot 拒绝、`planNeedsSnapshot` 单测；gRPC 新增相位→状态持久化与 retry/rollback/archive 准入钉定；CLI 终态与可回滚矩阵补两新状态。

- **并发多人审批丢票（P0，D-1 v2：pending-only CAS 丢票）**：多人审批未达 quorum 时状态仍为 pending，`decide()` 读完整投票列表→追加→`UpdateIfPending`，但底层 UPDATE 条件仅有 `WHERE id=? AND status='pending'`，没有投票内容版本比较——并发两票可同时“成功”而后写覆盖前写。修法：`approvals` 表新增 `revision` 列（SQLite v5 / PG v4 迁移），`UpdateApprovalIfPending` 的 WHERE 追加 `AND revision=?`、SET 自增；`decide()` CAS 失败重读重试。真实 SQLite + 生产适配器上，20 轮并发 2/2 审批每一票都持久化、quorum 正确（`TestApproveConcurrent_Quorum2NoLostVotes`）。
- **计划与审批版本未绑定，批准可漂移（P0，D-1 v2）**：PlanChange 保存新计划不重置 run 审批状态、重 plan 只废止旧 pending 审批、结算读全部审批行、Apply 不校验“当前版本计划获得过批准”。修法：`approvals` 新增 `plan_hash` 列；Approval/CreateRequest/SettleApproval/kickoff 全链路携带并过滤 plan_hash（空 = legacy 仍参与，兼容存量）；Apply 增加“审批匹配当前计划版本”门禁（无匹配即拒绝）；PlanChange 改 plan 成功后把 approved run CAS 回 draft/pending，且持久化失败改为显式报错（不再告警后继续）。
- **审批结算 run 状态写入无 CAS（P2）**：`transitionRunForApproval` 读取后整行 `UpdateRun`，可被并发状态迁移覆盖。改为一并更新 status + approval_status 的单次 CAS（新 store 原语 `UpdateRunApprovalStatusIf`）。
- **PlanChange 全行 UpdateRun 会覆盖并发状态**：计划持久化改为专用写 `UpdateRunPlan`（只写 plan_json/plan_hash/updated_at），不再触碰 status/approval_status。
- **SQLite 惰性连接丢失 pragma（并发写 BUSY）**：文件库连接池多连接时 `PRAGMA busy_timeout/WAL` 只在一个连接上生效，新连接不继承，并发审批写会 `SQLITE_BUSY`。改为在 DSN 上以 `_pragma=` 声明，使每条连接都继承（busy_timeout=5000/WAL/foreign_keys/synchronous/recursive_triggers）。
- **审批元数据解析失败静默降级（P1）**：`stateToApproval` 解析 Comment JSON 失败时改为返回错误（fail-closed），不再返回零值记录令 MinApprovers≈0；`decide()` 对 MinApprovers≤0 兜底为 1。
- **审批-计划匹配规则单点化 + 重试加固（D-1 v2 加固）**：`approvalPlanMatch` 成为结算与 Apply 门禁共用的唯一判定（legacy 空 plan_hash 规则只此一处），避免两个门禁日后漂移；该判定明确“无计划版本的 run 不被版本化审批授权”。并发票 CAS 重试预算 2→4 并加入小幅退避（`casRetryBackoff`），使多审批人同时决策时收敛而非抛出伪冲突（ErrConflict 仍是穷尽后的诚实结果）。
- **升级数据安全测试**：新增 `TestMigrate_LegacyApprovalRowSurvivesUpgrade`——升级前的 in-flight pending 审批行在 v2→v5 迁移后原样保留（plan_hash 空、revision 0）且仍可被 CAS 决策；3 审批人并发 3/3 quorum 回归（`TestApproveConcurrent_Quorum3NoLostVotes`）；autoApprove 显式绕过审批门禁的回归（`TestApplyChange_AutoApproveBypassesPlanApprovalGate`）。
- **unparam 清理（D-1 v2 加固）**：`settleApprovalFromCLI` 去掉未使用的 `actor` 参数，并把 `action` 用于部分票进度提示（原先两处告警）。`itsm/jira.Client.do` 的同类清理留在工作区，随该新包自身的提交一并入库。

- **审批多人门在 run 层被短路（P0：`min_approvers ≥ 2` 时第一票即放行）**：`ApproveChange` 此前调完 `approval.Service.Approve` 后**不看法票结果**，无条件把 run 置 approved——approval 层 `decide()` 的多人票语义（1/N 票保持 pending）在 run 层被完全绕过：`min_approvers: 2` 的变更，第一个审批人点同意即生效。修法：新增 **`ChangeService.SettleApproval` 单一结算点**——决策记录后读取该 run 全部 approval 行的**落库后状态**（approved/rejected/pending/expired）镜像到 run（票满→approved、一票否决→rejected、1/N→run 保持原状），终态保护（approved/rejected/running/completed/… 永不被迟到的结算降级或复活），幂等（重复结算 no-op）。
- **三个决策面记录决策后 run 永不翻转（P1：approve 了但 apply 永远拒绝）**：CLI `levee approve/reject`、ChatOps `approve/reject`、移动端 deeplink 三面都**直调** `approval.Service`（只写 approval 行）从不回写 run——run 永远停在 draft，后续 `apply` 因状态门拒绝，而操作员以为已批准。修法：三面全部收敛到 `SettleApproval`（CLI/ChatOps 在决策落库后显式调用并打印进度——"quorum still pending" 提示还需 N 票；deeplink 走 REST 层：`ApproveViaDeepLink/RejectViaDeepLink` 改返回 runID，网关结算，部分票响应 `recorded; quorum pending` 而非谎报 approved）。
- **CLI `levee plan` 不启动审批链（P1：kickoff 断链的 CLI 残留）**：`newCLIChangeService` 此前 approval service 恒为 nil——CLI 规划的变更同样不产生 pending approval，`levee approve` 找不到记录。修法：与 serve 同口径接上真实 approval adapter。
- **多人票测试基建缺陷**：kickoff 测试 adapter 的 Get 往返丢失 Decisions（Comment JSON 只编 Approvers/MinApprovers），`decide()` 的读-改-写会**跨决策丢票**（第二票时第一票消失、永远 1/2）——补全字段往返后多人票门才真正可测。

- **单节点 `serve --engine-enabled` 启动约 10 秒后 nil-panic 崩溃**：`startDispatchAndWorkerLoops` 的函数注释声明"Both require cluster mode and the execution engine"，函数体却只判了 `engineEnabled`。同文件里 takeover 的调用点有 `if clusterMgr != nil` 包裹（`cmd_serve.go:489`），dispatch 的调用点（`:513`）是裸调，于是单节点 + 引擎开启时把 nil 的 `*ClusterManager` 交给了 leader-only 的 sweep；`dispatch.NewLoop`（`dispatch.go:78`）不校验参数，首个 `DefaultInterval`（10 秒，`dispatch.go:28`）tick 在 `dispatch.go:156` 调 `GetLeader` → `cluster.go:213` 解引用 `m.registry`，goroutine 里没有 recover，进程以退出码 2 崩。**README 教运维开启真实执行的正是这条命令**（`./levee serve --token <secret> --engine-enabled`）。实跑复现 2/2，对照组（同命令不加该旗标）25 秒存活。修法：把护栏放在函数内而非调用点——契约写在自己的注释里就该由自己守住，只在调用点补等于给下一个调用者留同一个坑，同时打一条 INFO 说明单节点下这两个环未启动。新增 `TestStartDispatchLoopsRefusedInSingleNode`，断言两个 out-param 保持 nil（不靠"等 tick 看会不会崩"，因此是确定性的：护栏一旦被删，循环被建出来即刻变红）。变异检验：把条件改成恒不触发，测试进程内直接复现同一条 panic 栈（`cluster.go:213 ← dispatch.go:156 ← dispatch.go:144`）。

### 新增

- **`internal/identity`：把用户注册表从 `package main` 提到可被服务端导入的位置（P0-1 前置的第 0 阶段，零行为变更）**：`users.yaml` 的 `userEntry{Name, Team, Role}` 一直就是"调用者 → 团队"的映射（`levee user add` 在写），但类型定义在 `cmd/levee/cmd_user.go` 里，`internal/grpc` 导不到，所以服务端始终无法解析"这个已认证主体属于哪个队"——这也是 ABAC 接不进服务层的第一个原因。本次把注册表（`User` / `Registry` / `Load` / `Save` / `Lookup` / `Names` / `FilePath`）移入新包，CLI 侧保留 `userRegistry` / `userEntry` 类型别名与三个同签名薄封装，**所有调用点、错误文案与既有测试一字未改即通过**。包注释里钉住三件事，避免后来者用错：① join 键是**已认证主体**，不是客户端可自报的 `x-actor` / `X-Acting-As`（否则等于让调用者自己挑团队）；② 比对区分大小写（折叠会让 `Alice`/`alice` 变成一个主体，而 IdP 可能把它们当两个账号）；③ 本包只是**载入时的快照**，没有 watch/重载，`serve` 运行中改注册表不会生效，且它不做任何允许/拒绝判定。新增 `internal/identity/registry_test.go` 8 例：其中 `TestLoadReadsPreMoveFile` 直接读**搬家前 CLI 写出的原始字节**、`TestSaveLoadRoundTripAlsoMatchesLegacyReader` 反向断言新包输出仍是旧 schema（防"搬家顺手改格式"静默丢掉运维已配的归属），两者都做过变异验证（把 `Team` 的 yaml 标签改成 `team_name` → 两例同时转红）。权限位断言在 Windows 上按仓库既有惯例 `t.Skip`（POSIX 模式位不存在，`os.Stat` 对任何普通文件都报 0666，既不会真失败也不会真通过）。剩余阶段（`serve` 加载四份 RBAC 资产、`Authorizer`、逐 RPC 启用）仍待产品定 team/role 两套授予的合并规则，见 `docs/product-roadmap.md` 同一行。

- **危险度评分与审批分级自动路由（R4 红线落地，`internal/risk`）**：变更加"有多危险"从此可量化、可解释、可路由——
  - **评分因子**（5 类，均带分值上限防单因子淹没）：不可逆步骤数（×20，沿用 batch 1 的 `PlanStep.Irreversible` 判定，单一来源）；破坏性动作名词典（remove/delete/drop/truncate/destroy/purge/wipe，白名单滞后模块时的纵深防御）；影响面分档（复用 `plan.ImpactAnalyzer` 的低/中/高档）；无回滚声明步骤数；批次扇出。
  - **审批下限推导**：分数带（0-39 standard / 40-69 high / 70+ emergency）之上叠加硬红线——**任一不可逆步骤 ⇒ 至少 high**（分数再低也不豁免）；不可逆 + 高档影响面 ⇒ emergency。评分与因子明细落盘进 plan_json 工件（`Plan.RiskScore/RiskFactors/ApprovalFloor`），批准的分数即执行的分数（plan_hash 绑定）。
  - **审批链启动补断链（kickoffApproval）**：workflow 的 `approval:` 声明此前从未产生 pending approval 记录——ApproveChange 只能找到手工 seed 的行，审批链实际从未被系统启动过。现在 `PlanChange` 成功持久化工件后自动创建 pending approval：**tier = max(workflow 声明, 计划下限)**（下限只升不降，声明 emergency 仍赢过 floor high）；审批人集合取 workflow 声明，无声明时创建者兜底 + 24h 过期（fail-safe：无审批声明的变更也留真实记录而非无记录）；重 plan 覆盖旧 pending（旧行标 expired，审计留双份）。失败仅告警不阻断——apply 门的审批检查仍是最终防线。serve 侧 ChangeService 此前 approval service 恒为 nil，已接上真实 adapter。
  - **风险包零依赖设计**：`risk` 不 import `plan`（`plan` 反向持有风险字段），调用方组装扁平 `Input`——依赖图无环，字段面只有一处真身。
- **单步验证门 API（`POST /gates/verify`，`internal/grpc/rest_gate.go`）**：操作员/流水线预检/ChatOps"现在跑一条健康检查"的按需单步验证——无需为一次检查规划整个变更：
  - 复用引擎同源 `verify.NewCommandGate/NewProbeGate/NewSLOGate` 构造器（零重实现、零语义漂移）；cmd 检查经注入的 ChannelDialer 对 inventory 目标拨号执行（未知/retired 目标前置拒绝），probe 自描述，slo 必须配置 Prometheus URL 否则 fail-closed。
  - **human 显式排除**：阻塞审批检查点不属于单步验证语义，指向审批链（ApproveChange/mobile）。
  - 每次执行落审计（action `gate_verify`，带可选 run_id 关联）——按需检查不留审计盲区。REST 400 语义：调用方描述了系统诚实无法执行的检查（未知类型/缺参/缺运行时依赖），是请求错误而非服务器错误。
  - serve 接线：`--engine-enabled` 时经 `wiring.Engine.Dial`（inventory 查询 + 凭据展开 + registry 拨号与 apply 路径同口径）注入 dialer；无引擎时端点不挂载（404）。
- **ITSM Jira 出站审批桥（`internal/itsm/jira`）**：审批留痕对外可追溯——审批链开始建 Jira issue（携带 `approval:<id>` 标签做链接），审批决策经 JQL 按标签定位镜像 issue 并评论（"approved by alice (2/2)"）：
  - **纯出站镜像**：LEVEE 自身 store 是唯一事实来源，apply 路径不读 Jira——Jira 故障只延迟镜像、永不阻断或伪造审批。
  - **config 驱动 no-op**：`notify.jira.enabled=false`（默认）什么都不装什么都不拨；启用必须齐 url/project_key/api_token（env `LEVEE_NOTIFY_JIRA_API_TOKEN` 可覆盖），缺了 serve 启动失败（拒绝半桥）。Jira Cloud 走 Basic（email:token），Server/DC 走 Bearer。
  - **错误遏制**：所有外发 best-effort，错误日志后丢弃——镜像失败绝不影响审批链本身。kickoff 侧经 ChangeService 新增 `WithApprovalCreateObserver` 钩子（与 approval.Service 的 DecisionObserver 同契约语义：副作用失败不连坐主流程）。
- **审批创建观察者（`ChangeService.WithApprovalCreateObserver`）**：kickoff 后落库即可通知外部镜像（ChatOps 卡/Jira issue）——chatopsbridge 的 `OnApprovalCreated` 此前同样从未被生产调用，观察者面补齐后两个桥均可挂上。

- **回滚快照全链路接线（修复"声明了但零执行"缺陷）**：`rollback: {strategy: snapshot}` 此前 DSL 可声明、plan 携带，但创建与恢复两侧都无消费者（v1.13.0 Known Limitations 原文承认 "built-but-unwired"）。六层全部接通：
  - **DSL 声明面**：`RollbackSpec.SnapshotPaths`（步骤级 `snapshot_paths:` 列表，声明要备份的目标机路径）+ parser 透传。
  - **采集（创建）侧**：closure 增加采集阶段（锁获取后、首个批次前，设计 4.4.4.2"快照创建失败则该目标机不进 apply"）；`engine.Snapshotter` 接口 + `WithSnapshotter/SetSnapshotter` 钩子；只对 `strategy: snapshot` 步骤采集。
  - **通道感知采集器**（`internal/wiring/snapshotter.go`）：经既有 channel 缓存对目标机执行 `base64 '<path>'` 拉取文件内容（修复 SnapshotManager 只认本地 FS 的缺陷——naive 接线会快照 master 自己的文件系统）；`base64` 编码传输杜绝引号/注入面；payload 落 `files/<flattened>` + `paths.json`，与本地 FS 采集**字节兼容**（`rollback.WriteSnapshotPayloads/ReadSnapshotPayloads`）。
  - **恢复（回滚）侧**：`rollback.Manager` 新增 `strategy: snapshot` 分支——恢复快照**而非**执行 undo 步骤（混合执行等于双重撤销，被显式禁止）；`WithSnapshotRestore` + `WithRunID/SetRunID` 注入恢复回调与闭包 run id；fail-closed 三路：无回调→skip "not wired"（绝不静默降级成 undo）、无 run id→skip、恢复错误→step 失败（操作员可见）。
  - **键设计**：快照按 **change id** 键存（非闭包 run id）——手动 `RollbackChange` 路径只知 change id 也能找到快照；重跑覆盖旧采集，恢复总是回到最近一次执行前状态。
  - **serve 旗标**：`--engine-snapshot-dir`（空=禁用，快照步骤恢复为 "not wired" skip 而非失败）；闭包自动回滚与手动回滚两条路径都装配恢复回调。
- **local 通道（`internal/channel/local`）**——通道插件化开放的第一个证明实现：
  - CI/沙箱/单机自测场景的零网络执行通道（在 LEVEE 进程本地 OS 执行，实现完整 `channel.Channel` 契约）。
  - **三重安全门禁**（防"workflow 任意命令上 master 执行"的 R2 红线）：程序白名单（精确名匹配，`map[string]ArgPolicy`）+ 参数策略（`ArgNone`/`ArgPrefix`(仅 --flag)/`ArgShell`(信任沙箱)）+ 沙箱根目录（Upload/Download 路径穿越拒绝，`../` 与根外绝对路径均 `ErrPolicyDenied`）。
  - **fail-closed 默认**：零值策略拒绝一切；未 enable 时 Connect/Exec/Upload/Download 全部 `ErrDisabled`；Windows 拒绝 Exec（os/exec 解析 cmd 内建/PATHEXT 无法白名单化，平台门在 Exec 入口，CheckPolicy 保持纯逻辑便于跨平台测试）。
  - 工厂经 `init()` 注册进 `DefaultRegistry()`（"local"），`local.Target` 实现 `channel.Target`。
- **通道插件开发指南（`docs/channel-plugins.md`）**：第三方通道注册的完整文档——Channel/ChannelFactory/Target 契约表、安全底线（凭据零泄露/fail-closed/命令注入收敛）、参考实现对照（local→winrm→ssh 按复杂度）、注册后的引擎行为链、测试要求、已知限制。
- **MySQL 动作模块（`internal/executor/modules/mysql`）**：设计文档 8 大场景中最重的"数据库 schema 变更 / 主从切换"场景首次落地代码实现——
  - `mysql.query`：幂等 DDL/DML；SQL 全程 base64 编码经管道传输（`base64 -d | mysql`），SQL 文本永不上命令行，杜绝引号/反引号/`$(...)` 注入面；破坏性语句（DROP TABLE/DATABASE/INDEX/USER、TRUNCATE、无 WHERE 的 DELETE）在 query 动作上直接拒绝，引导走显式标记不可逆的受审步骤（R2 红线）。
  - `mysql.pt_osc`：封装 pt-online-schema-change 在线大表变更；alter 子句白名单正则（ADD/DROP/MODIFY/CHANGE/RENAME COLUMN、INDEX/KEY/CONSTRAINT、ALTER COLUMN SET/DROP DEFAULT、CONVERT TO CHARSET、ENGINE=）双端锚定 + 单引号 shell 转义，白名单外子句（PARTITION BY、LOAD DATA、链式命令）拒绝执行。
  - `mysql.replica_switch`：主从切换编排（SHOW SLAVE STATUS → STOP SLAVE → CHANGE MASTER TO MASTER_AUTO_POSITION → START SLAVE），逐步失败即中止且后续步骤不再执行；强制 `confirm=yes` 确认门——不可逆拓扑变更必须显式声明。
  - 主机名/标识符双校验规则（hostname 允许 `.-:[]`，库/表/用户名仅 `[A-Za-z0-9_$]`），`port`/`user`/`database` 全部标识符校验后才可拼接。
- **不可逆动作检查器生产接线（修复"有框架零接线"缺陷）**：`IrreversibleChecker` 此前仅测试注册、生产代码从未接线。现在 `plan.Generator` 在生成时对每个步骤执行检查（显式声明优先，其次默认白名单 pkg.remove/file.delete/user.remove/mysql.replica_switch/mysql.pt_osc），判定结果连同 reason 落盘到 `PlanStep.Irreversible/IrreversibleReason`（进 plan_json 工件）——审批分级（R4）与回滚门禁（R2）自此有单一事实来源，下游无需重新推导。
- **mysql 动作登记 DSL 类型签名表**（`internal/dsl/typechecker.go`）：`mysql.query/pt_osc/replica_switch` 的 args 类型完整登记，编译期类型检查覆盖新模块。
- **一键合规报告（`levee audit report`）**：时间窗（--since/--until，日期或 RFC3339）内全部变更 run 的审批链、哈希链验证结论（复用 ChainVerifier）、回滚记录聚合为自包含 HTML（无外部资源、可归档可邮件），--output 落盘或 stdout；监管/审计人员离线可读，"给监管看的一键报告"。
- **ChatOps 审批桥（`internal/notify/chatopsbridge`）**：approval 服务提供可选 `DecisionObserver` 钩子（决策落库后触发，错误不影响决策本身）；桥接包把审批创建/决策转换为 chatops 事件（approval_requested 卡 + approval_decision 进度卡"1/2 approved"/一票否决）广播到调用方提供的 BotManager。包内单测钉住 observer 组合与事件语义；常驻 bot 进程及 BotManager 生命周期属于部署侧组合，不在默认 serve 启动路径中。
- **README 定位升级**：从"非云原生基础设施"（资产位置边界）升级为"高危变更治理"（变更危险度边界）——云上云下、集群内外的高危变更统一归口；ArgoCD/Flux（集群内声明式交付）与 OpsMesh（日常自动化）的分层叙事保留。

- **CI 新增 `smoke` 作业：从二进制角度守入口装配**（`scripts/smoke_serve.sh` + `ci.yml` 的 `smoke` job）。上面的崩溃不是覆盖率不够，而是**覆盖率的口径根本没包住"可执行文件"**：4,905 个测试函数里没有一个 exec 过构建产物——`tests/e2e` 全程 in-process（`:memory:` store + `MockCluster`），`tests/e2e/docker-compose.yml` 零引用。新作业按发布形态（`CGO_ENABLED=0 -trimpath`）编出二进制，用运维实际敲的命令起 `serve`，再要求：① `/healthz` 可达（挡住 bind / store 类启动失败）；② 存活 25 秒跨过 `dispatch.DefaultInterval` 的 tick（崩溃发生在首次 tick，5 秒探针会漏过）；③ 进程仍在、日志无 panic、`/healthz` 仍应答。脚本本身做过双向变异验证：打在修复前的二进制上 exit 1（日志先出现 `grpc server listening` 再 panic——正是此前 CI 完全看不见的"先起来后崩"形态），打在修复后的二进制上 exit 0。Windows/MSYS 下 `mktemp` 给出的 `/tmp` 路径原生二进制读不懂，脚本按 `cygpath` 转换，顺带成为"配置文件加载"的一次真实检验。
- **`check` 聚合门禁补回 `docs`，并加 needs()/清单漂移守卫**：`needs` 里列着 `docs`，而聚合脚本手抄的 `status` 串里没有它，`check` 又带 `if: always()`，所以 `docs` 重新生成只要变红也拖不红聚合——这个洞已经存在一个多月（期间 docs 一直是绿的，故没有造成现行假绿）。手抄清单本身是 Actions 插值语义逼出来的（`${{ needs[job].result }}` 在 bash 看到之前就被引擎展开成空串），每加一个 job 都得记得改两处，忘记就静默失效。现在除了补回 `docs` 与新增的 `smoke`，聚合脚本还会比较 `toJSON(needs)` 的键与清单里的键，不一致就 `::error` 并 exit 1。守卫逻辑从 ci.yml 抽出原文渲染成三段脚本实跑：全列出且全绿 → exit 0；`docs=failure` → exit 1 并打印 `required job 'docs' did not pass`；清单漏掉 `docs`+`smoke` → exit 1 并点名缺哪两个。两侧各加 `tr -d '\r'`：验证时发现行尾噪声会让两个肉眼相同的字符串判不相等，守卫不该对 CRLF 敏感（本仓有过 CRLF 引发 lint 假阳性的前科）。

### 安全修复

- **权限矩阵第一次有了判定消费者：治理型 RPC 开始按策略准入（P0-1 接线）**：`permission` 的矩阵、`roles.yaml` 的角色树、`users.yaml` 的成员表此前只有 CLI 读写，`serve` 从不加载它们中的任何一份——所以运维用 `levee team add` / `levee rbac grant` / `levee user add` 精心配好的策略，对 53 个 RPC 的准入没有任何影响（这是审查报告 P0-1 的实质）。现在 `internal/authz` 把三者组合成一个判定，`apply` / `rollback` / `approve` / `reject` 四个治理入口在动任何状态之前先问它。**组合规则是两轴正交而非二选一**：矩阵说"这个队能在哪些环境活动"（环境可达性），角色树说"它的成员在这些环境里能做什么"（环境内动作能力）——单用矩阵则授权扁平（每个 队×环境×动作 都要写全），单用角色则没有环境概念（`operator` 在 dev 与 prod 一样，环境隔离失效）。判定 = `matrix.Allow(team, env, action)` || （角色授予该动作 && 该队在该环境至少已有一个动作 && 该动作未被显式撤销）；最后一条是刻意的，矩阵文档写明"显式撤销优先于一切"，组合层若忽略它，角色授予会悄悄推翻运维写下的明确拒绝（为此给矩阵补了最小 API `Revoked`）。**姿态**：没有矩阵 = 没声明策略 → 不拦、`serve` 启动 WARN（与 `bulk_grants` 同一原则）；矩阵存在但解析失败 → **启动即失败**（把坏策略静默降级成"不拦"，等于让运维以为策略生效）；矩阵存在时未登记主体一律拒绝。启动还会做一次对账：命名令牌的 subject 不在 `users.yaml` 里就逐条 WARN——这类调用者在策略生效后必然被拒，等到第一封拒绝工单才发现太晚。新增 `levee authz status` / `levee authz explain --subject X --env E --action A`（把一次判定摊开：命中哪条轴、解析到哪个队/角色、为什么拒），没有可自证的诊断，RBAC 迟早又退回"配了不生效"。**迁移前置**：启用前先跑 `levee authz status` 确认 `users.yaml` 覆盖了所有会调用治理 RPC 的主体。验证：`internal/authz` 9 例 + RPC 级 4 例（含"无授权器时行为与之前完全一致"的兼容性守卫）、5 次变异（去掉可达性要求 / 去掉撤销优先 / 未登记放行 / ApplyChange 不判定 / ApproveChange 不判定）全部使对应测试转红；`levee authz explain` 用真实策略文件实跑验证（角色轴在可达环境内补 apply、未登记拒绝、`default_env` 回落并标注来源）。

- **批量暂停/恢复的 API 路径绕过授权名单（P0-1 的第一处闭合）**：`levee pause all` 一直经 `pause.PauseManager.PauseAll(ctx, actor, perm)` 检查 `pause:all`，而被治理系统真正暴露出去的入口——gRPC/REST 的 `PauseAll`/`ResumeAll`——走的是 `ChangeService.bulkTransition`，它直接扫 store 改状态，**完全绕过 `PauseManager`，因此不看任何权限**。后果是运维配好的权限集在 API 上不作数，任何持有效令牌的调用者都能停掉或放行整个车队，而 CLI 里同一个操作会被拒。修法：`ChangeService` 增加 `pause.PermissionChecker` 依赖（`WithBulkPauseAuthorizer`），`bulkTransition` 在读改任何 run **之前**判定，权限名与 CLI 同一套（`pause:all` / `resume:all`），拒绝沿用 `PermissionDenied`；`cmd_serve` 从新增配置 `permission.bulk_grants`（调用者 → 权限名）构造 checker，并挂上 CLI 已在用的 `NewDenialAuditRecorder`，使被拒的批量动作进审计（`action=permission.denied`）。**缺省策略选"不拦 + 启动 WARN"而不是默认拒绝**：批量暂停是事故处置手段，把它绑在一个从来没人写过的配置键上，等于在最需要刹车的时候没有刹车；作为交换，未配置时 `serve` 启动即以 WARN 明示"authorization is NOT configured"，把静默缺口变成可见状态。留空的 `SimplePermissionChecker`（显式空名单）仍然是全拒——"没配"与"配了但谁都不给"是两件事。**为什么不是完整方案**：`internal/permission` 的 ABAC 矩阵按 `team × env × action` 判定，但全仓不存在"调用者属于哪个 team"的解析（`TeamRule` 无成员字段、`state` 无用户/团队实体，而 `Check` 刻意拒绝空 team），所以服务层准入目前只能用到有数据的那半个模型；缺的前置原语已记入 `docs/product-roadmap.md` 代码层 P0。验证：新增 `internal/grpc/change_service_bulkauth_test.go` 4 例（未配置放行、名单生效、pause 授权不等于 resume 授权、被拒动作留审计两条、经真实拦截器链的命名令牌下 `x-actor` 伪造不改变判定身份）；两次变异（判定短路 / resume 复用 pause 权限名）均转红。

- **审批链可被"发起人 1 票自批"与"拼名字凑齐双人签名"满足（P0，审计驱动）**：三处叠加导致 `high` 级变更的审批门禁形同虚设，且审计会把它记成合规。① `kickoffApproval` 在 workflow 未声明 `approval.approvers` 时把审批人设为**变更发起人自己、配额写死 1 票**，完全不看 tier——而 tier 恰恰是它自己上一行算出来的（`risk.MaxLevel(declared, p.ApprovalFloor)`，任何不可逆步骤都会抬到 high）；`LevelManager` 里 `LevelHigh.MinApprovers = 2`（`internal/approval/levels.go`）在这条路径上从未被引用。② `ApproveChange` 的投票人取自 `req.GetApprover()`（REST 侧取 JSON 的 `approver`，`internal/grpc/rest.go`），**从不与已认证主体比对**，`isAuthorized` 只查"这个名字在名单里"，`hasDecided` 也只比名字——于是任何持有效凭据的调用者把两个已知审批人的名字各打一遍即可凑齐 2-of-2，而 `run.Creator` 来自 `x-actor` 请求头（Legacy 单 token 下 `auth.go` 自己注明"actor remains whatever the client asserts"），连发起人身份都是自报的。③ 规范要求的 `high 强制 exclude_initiator`（`docs/leveelang-spec.md` 审批级别表）**零实现**：该字段只被解析并进 plan_hash（`internal/plan/hash.go`），`internal/approval` 无任何消费者，`approval.CreateRequest` 连字段都没有。修法：新增 `internal/grpc/subject.go` 区分"审计标签"与"可验证主体"（`SubjectFromContext` / `ContextWithSubject` / `requireSubject`），主体只由 Named token / SSO 会话 / OIDC 产出，`x-actor` 降级为纯审计字段；`ApproveChange` / `RejectChange` 的投票人改为认证主体，客户端声明的名字与主体不符直接 `PermissionDenied`（不是静默忽略）；`approval` 包补 `Initiator` / `ExcludeInitiator` 并在 `decide()` 落新哨兵 `ErrInitiatorExcluded`（生产适配器 `approvalExtra` 随之持久化）；`kickoffApproval` 的配额改由 `approval.MinApproversFor(tier)` 决定（workflow 可收紧不可放宽），high 强制 `approval.ForcesInitiatorExclusion`，当"剔除发起人后的可投票人数 < 配额"时 **PlanChange 直接返回 `ErrApprovalUnderprovisioned` / FailedPrecondition**，不再下发一条自证合规的审批记录。**破坏性变更**：审批/驳回 RPC 现在要求可验证身份——只用 `--token <secret>` 共享 token 的部署不能再提交审批（该凭据只证明"部署内部有人"，恰是审批不能建立在其上的声明），需改用 named token（`server.auth_tokens`）/ SSO / OIDC；`--insecure` 开发模式与 CLI 本地模式（进程内即权威）不受影响。验证：新增 `internal/grpc/change_service_governance_test.go` 六例（含经真实拦截器链的 legacy-token 拒绝 + 命名凭据放行对照、REST 侧 body 名字与 bearer 主体不符返回 403）；四条修复逐个做**变异验证**（还原配额强制 / 去掉发起人回避 / 改回采信 `req.GetApprover()` / 取消 plan 期拒绝，对应测试全部转红）。另修测试替身 `kickoffStoreAdapter`：`Update` / `UpdateIfPending` 回写时丢 `PlanHash`、且不带新字段，会让依赖它的测试得到"legacy 空 plan_hash"假警告（生产适配器一直是对的，之前据此得到的结论均不可信）。

- **有引擎但无审批服务的部署跑完 plan 也不记录审批等级，导致不可逆变更可被自动批准（P0 伴生缺陷）**：`kickoffApproval` 是 `run.ApprovalLevel` 唯一的 tier 写入点，而它整段在 `s.approval == nil` 时提前返回。`run.ApprovalLevel` 这一列被 priority 与 approval tier **共用**（`CreateChange` 把客户端的 `low/normal/high/urgent` 写进去，`runToPB` 又按 priority 镜像回去），于是这种部署里该列永远留着客户端自报的 priority：plan 的 `ApprovalFloor=high`（不可逆）读出来是 `normal` → `AutoApproveForbidden` 不触发 → **自动批准直接执行**；反向，一个 `Priority: "high"` 的普通变更被当成 high 级拒绝自动批准。修法：`PlanChange` 落盘 plan artifact 后，若无审批链会去写 tier，则由 `derivedApprovalTier(run)`（与 kickoff 共用同一份推导规则，消除两处各写一遍的规则漂移）把 tier 记进 run；记录失败即 plan 失败——记不下等级的变更无法被正确门禁。顺带把 `AutoApproveForbidden` 门禁移到 plan 存在性与 plan_hash 完整性校验**之后**：未 plan 的 run 本就没有等级，此前会先撞上"medium-risk 不允许自动批准"而把可执行答案"请先 PlanChange"埋掉。验证：`TestPlanChangeRecordsTierWithoutApprovalService` 断言不可逆 plan 在无审批服务时被记为 high 且 apply 被拒；变异（取消该记录）后此例转红；`tests/integration` 的未 plan / 篡改 plan 两用例把顺序契约钉住。

- **CVE-2026-84445（CRITICAL/HIGH）修复**：builder 阶段基础镜像 alpine 包升级。runtime 阶段已有 `apk upgrade --no-cache`（上次 CVE-2026-14456 openssl 的同模式修复），但 builder 阶段（`golang:1.26-alpine`）缺失该步骤，导致该基础镜像新发布的 CVE 在 trivy 门禁被检出阻断。builder 阶段补上 `apk upgrade --no-cache`，与 runtime 阶段同模式，消除 builder 层已修复基础 CVE。

### 文档

- **`docs/leveelang-spec.md` 第 7 章重写为"回滚声明的两层归属"**：原文把 `rollback` 写成 workflow 的**必需**块、把补偿（strategy / step / snapshot_paths）与运行态策略（on_failure / verify_after）混进同一张字段表，正文与附录 8 个示例全部把 undo 清单写在顶层——照抄即被 LE097 拒绝。现按实现拆成两张表（step 级=补偿契约、workflow 级=运行态策略）并逐示例迁移；`verify_after` 的现状（回滚后验证器已实现、生产装配尚未注入，故暂不改变行为）如实标注并指向 roadmap，避免又一处"文档说有、执行没做"。`docs/levee-design.md` 的完整示例同步迁移，`docs/product-roadmap.md` 把该决策记为已定案并新增两项（回滚后验证装配、run 级快照原语）。

- **`docs/levee-api.md` 第 12 章输出格式与实现全面不符（按真实运行逐项订正）**：原文描述的 `levee list` 输出基本是想象的——`PrintHuman` 走 `text/tabwriter`，**完全不含 ANSI 颜色**（着色只存在于 `levee audit` 导出的 HTML 报告 `.status-*`），列集合是 `ID / WORKFLOW_NAME / TEMPLATE_NAME / STATUS / APPROVAL_STATUS / CREATOR / CREATED_AT`，**没有**原文写的 `PROGRESS` 列（也没有 `INITIATOR` / `UPDATED`，实际叫 `CREATOR` / `CREATED_AT`）。JSON 示例同样失真：真实行字段是 `id` / `workflow_name` / `template_name` / `status` / `approval_status` / `creator` / `created_at`（原文的 `change_id` / `template` / `progress` / `initiator` / `plan_hash` / `updated_at` 均不存在），`meta` 是 `count` / `limit` / `offset`（原文的 `total` / `elapsed_ms` 不存在），`created_at` 是 `2006-01-02 15:04:05` 字符串而非 RFC3339，错误信封实测为 `{code, message}`（原文的 `detail` 对象未见于实际输出）。本次改动用编译出的 `levee` 二进制 + 临时 SQLite 库实跑 `template create` → `new` → `list` / `list --json` / `show <不存在 id> --json`，把三处示例换成真实输出并标注取自实跑；`--status` 过滤项补齐为权威词表（原文漏掉 `draft` / `planned` / `cancelled` / `interrupted` 与 D-2 v2 的两个回滚判定），同时写明「精确匹配 + CLI 不做白名单校验」。示例命令同步改为可复现的 `levee list`（原文写 `--status running` 却展示非 running 行）。
- **`docs/cli-reference.md` apply 终态词表补齐**：引擎路径回写的终态补上 D-2 v2 的 `rolled_back_partial` / `rollback_incomplete`，并注明「回滚未完全收敛不得报告为干净的 `rolled_back`」。
- **`docs/design-cluster-dispatch.md` 标注 assignment `result` 词表是刻意冻结的**：run 状态新增两个回滚判定后，`run_assignment.result` 仍只认 `completed | failed | rolled_back`；映射点在 `cmd/levee/grpc_change_executor.go`（`retry()` 显式归为 `failed`，`apply()` 经 default 分支落到 `failed`），run 行保留精确状态。补这条是为了防止后来者把「看起来漏了」的词表当 bug「修掉」，反而动到 assignment 终态识别与「是否已分派/已终结」的判定。

- **`docs/levee-api.md` 13.5 分页与过滤参数表同样失真（按 handler 逐个核对订正）**：原文声明的 `limit`（默认 20、上限 100）/ `offset` / `sort` / `fields` 以及「`/changes` 支持 `template`/`initiator`/`from`/`to`、`/audit` 支持 `who`/`action`/`change`」几乎全不存在——网关不校验未知参数，传了会被静默忽略，而 `rest.go` 正是为杜绝「假装支持」才刻意不解析它们。现改为按 `internal/grpc/rest.go` 逐 handler 列出的实际参数表（`/changes`：`status`/`labelContains`/`pageSize`/`pageToken`；`/audit/log`：`actor` 而非 `who`、`changeId` 而非 `change`；targets/templates/system-config 各自实际解析项），并单列「刻意不支持」清单。分页事实一并订正：默认 `pageSize` 是 **50**（原文写 20）；`maxPageSize = 1000` 的上限**只在审计类服务生效**，`ListChanges` 侧没有上限钳制（原文的「上限 100」两头不靠）；`pageToken` 是「下一页起始偏移」的十进制字符串（`helpers.go` 的 `parsePageToken`/`buildPageToken`），畸形或负值返回 `InvalidArgument` 而非静默回到第 0 页；**没有**字段投影。

- **README 四处与实现不符，按实测逐项订正**（同时更正 `docs/leveelang-spec.md` 里的一份复制件）：① **RPC 计数**——原写"40 个 gRPC RPC 里 23 个挂在 ChangeService"，两个数都不对：40 只是 `proto/levee.proto` 单文件的 rpc 行数，漏了 `proto/levee_extra.proto` 的 13 个（合计 53），`ChangeService` 实际 22 个方法（service 块与生成的 `_ServiceDesc` 双向核对）；serve 路径注册的是 9 个业务服务 + 标准 health，"5+ 服务"是早期只有五个核心服务时的行文。现在把来源文件名写进句子，读者可用 `grep -cE '^\s*rpc\s+' proto/levee.proto proto/levee_extra.proto` 自行复核，不必再信一个裸数字。② **apply 语义**——原写"关闭时 apply 明确拒绝（FailedPrecondition），不会假装执行"，这只对 gRPC 入口成立（`change_service.go:806-809` 拒绝且刻意不推进状态机）；CLI 入口相反，`cmd_apply.go:97` 把 run CAS 到 `running`、`:132-134` 打一行 status-only 警告后 exit 0——对运维而言"置为 running 但一个批次都没派发"正是那句话否认的形态。README 现按入口分列，并写明"CLI 的 exit 0 只代表没报错，不代表执行过批次"。顺带纠正 `ApplyChange` 的函数注释（它写着"nil 引擎时做最小状态迁移，与 CLI 同"，与自己函数体里的拒绝分支直接矛盾——注释是引擎接入前的旧行为）。③ **新增"能力可达性"一节**——特性列表原本混着"已接入二进制"和"包已实现、测试完备、但零生产调用方"两种状态，判定口径可复现（`go list -deps ./cmd/levee`）：`compat`（MVP 交付项 D-08）/ `diagnosis/llm_diag` / `diagnosis/topology` / `notify/chatopsbridge` / `opsmesh` / `recommend/feedback` / `recommend/rag` / `scheduler` 共 8 个未链入，非测试代码 4,691 行，包内测试覆盖实测 90.4%~95.4%——覆盖的是没人调用的代码。据此把"AI 辅助运维：告警接入→拓扑诊断→LLM 对话式定位→RAG 知识增强推荐→自动执行→效果学习"改写为真实状态：`NewLLMClient`（真的实现了 OpenAI / Ollama 客户端）的 22 个调用点**全部**在 `llm_test.go`，`cmd_serve.go:301-303` 与 `cmd_converse.go:159` 构造推荐引擎时都不传 `LLMClient`（`recommend/engine.go:130` 注释即"nil = pure knowledge-base mode"），`internal/config` 没有任何 llm/rag 配置键、`config.example.yaml` 无对应段——运维也开不了；建议来自内置静态知识库。措辞沿用 `docs/security-audit.md:510` 对多租户的写法。④ **快速开始**——原来直接 `./levee new nginx-reload --params target=web01.prod` 并注明"可用模板用 template list 查看"，但全新数据目录实测 `template list` 返回 `No templates found.`（二进制不预置模板），第一条命令就报 `template not found`；`docs/quickstart.md:80` 其实有前置的 `template create` 步骤，README 抄结果时漏了。补上后四步逐字实测通过（`create` 带 `--params` JSON → `new` 输出 `params: map[service:nginx]`、`status: draft` → `list` / `show` 可见）。另记录一处真实陷阱：`template create` 不解析 `--content` 里的 `params:` 块，拿仓库自带的 `examples/templates/patch-rolling.yaml` 实测，建完 `template show` 是 `Parameters: (none)`，随后 `new patch-rolling --params package=nginx` 报 `unknown parameter: package`（实例化按记录里的 `tmpl.Parameters` 校验，`instantiate.go:120-123`），参数必须另用 `--params` 传。改完确认 `go run ./internal/docgen -check -root .` 仍 `spec is up to date`（订正落在手写正文，不在三个 GENERATED 块内）。

## [v1.13.0] - 2026-09-08 — 执行引擎接线 + 集群故障接管

本版落地两大设计（`docs/design-engine-wiring.md` / `docs/design-cluster-failover.md`）：**A —— 执行引擎接入 serve**，计划→审批→执行自此真实闭环（计划持久化 + plan_hash 绑定、显式 `--engine-enabled`、CLI plan/apply 引擎路径、批次/步骤证据落库）；**B —— 集群在途变更故障接管**，执行节点崩溃后其运行中变更由租约围栏与 leader 接管循环收敛至新终态 `interrupted`（审计留痕、永不重跑副作用、`RetryChange` 显式重驱动）。另含双 SSO（OIDC/GitHub）、多令牌认证、REST 方法校验等安全加固与全链测试战役。详细说明见 [docs/release-notes/v1.13.0.md](docs/release-notes/v1.13.0.md)。以下为逐条明细。

### 新增

- **B 系列收尾四项（C1–C4，`docs/design-cluster-failover.md` 承诺对齐）**：
  - **孤儿 run 扫描（C1，补齐设计 §7.2-B2 承诺的候选扫描另一半）**：`ExecutionGuard.OrphanedExecutions`（`running` 且无 `run_execution` 行且 `updated_at` 老于宽限——默认 5 分钟）并入接管循环候选（与过期租约扫描去重合并）。此前 `ExpiredExecutions` 只扫有租约的行，CAS→Begin 微秒级窗口崩溃的 run 将永远卡在 `running`——恰好是接管要消灭的死区；宽限期保证不误伤健康执行（健康执行必有行）。四个 PG 门控用例：孤儿收敛 interrupted+trace / 宽限期内新 running 不动 / 有活租约不算孤儿 / 终态永不触碰。
  - **接管结果计数器（C2，补齐设计 §3-5 承诺）**：`levee_takeover_events_total{result}`（settled/skipped 预注册零值序列），`TakeoverOnce` 对每个候选按结果打点——接管行为在 `/metrics` 可观测，不再只能翻日志；与既有 `levee_changes_total{interrupted}`（run 状态转移维度）正交互补。
  - **前端重试入口（C3）**：`isRetryableStatus` 纯函数（镜像后端 RetryChange 准入集 failed/rolled_back/interrupted，spec 用例对词表补集防漂移）+ ChangesView 操作列"重试"按钮（confirm 弹窗防误触，调既有 retry API）；vitest 40 用例全绿 + dist 重建。此前前端 retry API client 存在但**零调用方**——interrupted 的机器入口在 UI 不可达（所有可重试状态均然）。
  - **SQLite splitter 防御钉（C4）**：`splitSQLStatements` 按"行尾分号"切分，行尾 inline 注释含分号（plan_json 列即此形态）依赖"注释行不以分号结尾"这一隐性约定才不被切断——PG 侧同类形态已造成过生产建表事故。schema.sql 加 SPLITTER HAZARD 警示注释 + 两个用例钉住安全形态与切分规则。

- **收敛选举无条件重选（CI 真 PG 腿揭出的生产缺陷）**：`NodeRegistry.Register` 自动把本地注册表里第一个 master 设为 leader，而后 `SyncFromPeers` 折叠共享表视图时只在"旧 leader 掉线"才重选——节点在只含自己的本地视图上加入时会**自封 leader 且永不纠正**（自己永远 active）。生产后果：新加入节点可长期自认为是 leader（B 的 takeover 循环是 leader-only，错误的自认会让非 leader 节点试图接管）。修复：`SyncFromPeers` 折叠后**无条件重跑**确定性选举策略（最小 active master ID）——对同一共享视图幂等、全节点必然收敛一致。CI 表现：`TestTakeover_NonLeaderDoesNotAct` 在真 PG 上翻车（follower 自认 leader 竟然接管成功），本地无 DSN 跳过故此前未拦住。
- **takeover 测试夹具收敛时序**：leader 收敛改为 `Eventually` 双节点断言（驱动 sync 轮直到两视图同 leader，不再赌单次同步）；测试自身行清库用 `TRUNCATE ... CASCADE`（`DELETE trace` 被 WORM 触发器拦；state 包测试同套路）。本地以 docker `postgres:16-alpine` 起真 PG 实测：takeover 6 用例 + integration 3 e2e + serve 层 3 验收 + cluster/state 全包全绿。

- **集群在途变更故障接管（设计 B 全部落地，`docs/design-cluster-failover.md`）**：集群模式下节点在 apply 中途崩溃，run 从此不再永久卡死在 `running` 等人工救库——leader 的接管循环在租约过期后把它收敛到新终态 **`interrupted`**，全程审计留痕；接管**永不重跑副作用**，重驱动是人工经 `RetryChange` 的显式新执行（§7.5-Q1）——
  - **执行围栏（fencing）**：新表 `run_execution`（run_id/owner/epoch 单调序列/lease_expires，并入 clusterSchemaSQL 的 advisory-lock 串行建表路径）。集群模式所有 apply/retry 执行前必须**登记租约**（活租约被他人持有时拒绝执行=集群单飞），心跳 TTL/3 独立于步骤进度续约（慢 SSH 不掉租约；TTL 语义=死后检测延迟上界而非执行时长上限）；`Owns` **续约式复验**（校验同时延长租约，构造上关闭"校验→接管→落库"TOCTOU）；步骤派发前、证据落库前双重门禁；终态写入全部 CAS 化（B1：`casRunStatus` choke point + `retryChange` 终态 CAS——迟到的僵尸不再能覆盖接管赢家的状态，事件发布纪律=赢家独占）。
  - **失主免回滚哨兵**：闭包进入批次执行后任何失败都触发刻意不可取消的自动回滚（既有设计），失主信号若走 ctx 取消会让僵尸带着 undo 扑向生产主机——新增 `engine.ErrFencedOut` 哨兵，`closure.Run` 识别后**跳过回滚直接失败**（引擎净改动 ~15 行）；wiring 的 `SentinelAdapter`（`WithExecutionGuard` 内置强制适配）把 cluster 层哨兵错误链映射进引擎哨兵，组合根忘包适配器再无静默降级面。
  - **接管循环（`internal/takeover`）**：leader 独占（收敛选举视图；per-run 接管锁 + `running→interrupted` 状态 CAS 双重幂等兜底）→ 防御性非终态步骤标记（`state.Store.MarkNonTerminalSteps`，SQLite/PG 双实现；当前"闭包完成后一次落库"形态下按构造 0 行命中，为未来增量持久化留形）→ `interrupted_by_takeover` 审计 trace → 删执行行（续行 executor 重新登记，旧主被缺行本身围栏）→ 指标计数。`TakeoverOnce()` 测试钩子全程无时钟依赖。
  - **`interrupted` 全触点**：状态机（可 archived、拒 cancelled——接管判定已被审计链记录）、`RetryChange` 准入（failed/rolled_back/**interrupted**）、WatchChange 终态集、`levee_changes_total` 预注册标签、前端色表（已中断/danger）+ dist 重建。
  - **serve 旗标**：`--cluster-takeover-interval`（默认 10s，≤0 只禁循环、围栏永在）、`--cluster-exec-lease-ttl`（默认 30s）；组合根类型桥保持 wiring↛cluster 依赖方向。
  - **验收（tests/integration，真 PG）**：崩溃执行者 ≤2×TTL 收敛 interrupted + 审计哈希链验证通过；僵尸复活零污染（零步骤行/零 undo 派发/终态不被盖写）；双节点并发接管恰一个赢家。死亡模拟=进程内断水+定点租约过期 UPDATE，与 kill -9 在全部断言观测面上等价（等价性论证入测试文件头）。
  - 如实注明（§7.5-Q3）：手动 `RollbackChange` 在围栏外（不经 running、不登记），节点死在手动回滚中途时 run 停在原终态、人工可重发；跨节点调度、快照入 PG、断点续跑仍在 backlog（R5 硬边界）。
- **执行引擎接线（设计 A 全部落地，`docs/design-engine-wiring.md`）**："计划 → 审批 → 执行"自此闭环，serve 模式从 status-only 演示变为真实执行变更——
  - **计划持久化 + plan_hash 绑定**：`PlanChange` 把生成的计划工件持久化到 run 行（`plan_json` 列，SQLite/PG 双实现）并写入 `plan_hash`；引擎路径的 apply/retry/rollback 先加载存储计划并做哈希门校验，工件缺失或被篡改 → `FailedPrecondition` 引导重新 plan——apply 执行的必是被批准的那份计划，审批语义自此成立。从未 plan 过的 run 拒绝 apply（明确报错，不再静默 status-only 假成功）。
  - **组装工厂 + 显式开关**：新增 `internal/wiring` 组装工厂（`NewEngine(store, WithCredentialResolver/WithMaxParallelRuns/WithGatePrometheusURL/WithChannelRegistry…)` → `EngineAdapter`），"生产级组装配方"从 e2e 演练测试提升为生产代码。`serve --engine-enabled`（默认 **false**，关闭时行为与此前完全一致；开启时装配执行引擎、通道注册表与凭据解析，`LEVEE_MASTER_PASSWORD` 未设置则匿名拨号并输出警告，与目标探测同口径）。
  - **执行闭包 + 证据持久化**：apply 经 `ClosureRunner` 按已存计划同步执行（SSH/Local 通道、inventory 冻结守卫、cmd/human/slo 门禁、`rollback.Manager` 自动回滚、`batch.Controller` 中断策略）；批次行按 `(run_id, batch_no)` 复用更新、步骤行只追加（action/exit_code/stdout/stderr/耗时全量落库），自动回滚的 undo 证据与前向证据同轮持久化。`RetryChange` 支持主机子集重规划（复用批次行、CAS 抢占 running、trace 记 `retry_finished`）；`RollbackChange` 支持手动回滚（→ `rolled_back`）。并发闸为 `max-parallel-runs` 信号量：第 N+1 个 apply 快速失败并提示排队。
  - **CLI 引擎路径**：新增 `levee plan <run-id> --targets h1,h2 [--dry-run]`（CLI 侧唯一的计划持久化入口；生成阶段不触达任何主机故无需引擎开关，`--dry-run` 只预览不持久化）。`levee apply --engine-enabled` 走与 serve 完全相同的进程内 `ChangeService` 路径（审批/冻结/计划门全部共享而非重写；`--force` 映射自动批准、`--max-concurrency` 统一覆盖批次并行度；未 plan 或门拒绝 → exit=4，执行失败 → exit=1 且 JSON 输出先行）。默认路径保持 status-only 且帮助文本如实标注。
  - **运维配置面**：`serve --engine-max-parallel-runs`（默认 4）、`--engine-gate-prometheus`（未配置时 slo 门禁 fail-closed 拒绝执行，与未接引擎同语义）。导出 `grpc.ContextWithActor` 供进程内 CLI 调用注入审计主体。
  - 如实注明：快照子系统（`rollback.SnapshotManager`）当前仍无非测试消费者，故**未**提供快照目录配置旗标——接一个"看似可配、实为死配置"的旗标违反设计 R7 边界（不新增执行语义），待其真正被消费时再接线。

### 修复

- **PG 新库建表全线失败（CI postgres 腿红灯，plan_json 上线连带引入）**：`pgExecMultiStatement` 只剥**整行** `--` 注释，而 pgschema.sql 的 runs 表 DDL 中 plan_json 列的 inline 注释恰含分号（`('' = not planned; v3…)`），`pgSplitSQLStatements` 按 `;` 在注释内部截断 `CREATE TABLE` 语句——PG 收到括号未闭合的片段（SQLSTATE 42601 "syntax error at end of input"），所有打开全新 PG store 的测试（25 个）全灭；存量库迁移路径（独立 ALTER 语句）不受影响，SQLite 侧 splitter 按"行尾分号"切分天然免疫，故仅 CI 的 PG 腿暴露。`pgSplitSQLStatements` 升级为完整状态机：单引号字符串跟踪（`''` 为 PG 转义引号）+ inline `--` 注释跳过，`;` 仅在字符串/注释/dollar-quote/BEGIN…END 之外才是分隔符；新增 5 个回归用例，其中一个直接对**内嵌真实 schema** 做结构断言（语句计数、逐语句括号平衡、注释文本零残留）——钉住工件本身而不只是扫描器。
- **ApplyChange 状态机竞态（P1）**：`ApplyChange` 读-判-写状态流转此前非原子——两个并发请求可同时通过状态检查并互相覆盖终态（双跑/状态翻转/审计污染）。新增 `state.Store.UpdateRunStatusIf`（`WHERE id=? AND status=?` 的 compare-and-set，SQLite/PG 双实现），gRPC `ApplyChange` 与 CLI `apply` 均改用 CAS 抢占 `approved/pending/draft → running`；失败方返回 `FailedPrecondition` 并携带最新状态（SQLite/PG 各新增 CAS 测试 + gRPC 并发双跑互斥测试 `TestApplyChange_ConcurrentDoubleApplyIsSerialised`）。
- **回滚状态语义丢失（P2）**：`EngineAdapter.Run` 此前只返回 success bool，引擎 `PhaseRolledBack` 在 `ApplyChange` 中被压平为 `failed`，回滚成功与彻底失败不可区分（而 `RetryChange` 依赖 `rolled_back` 状态却永远等不到）。`EngineAdapter.Run` 新增 `phase` 返回值，`phase=="rolled_back"` 时 run 落库 `rolled_back`（新增 `TestApplyChange_EngineRolledBackPersistsStatus`）。
- **无引擎 apply 假成功（P0 级体验缺陷）**：serve/网关未接线执行引擎时 `ApplyChange` 此前返回 `Success:true` 并把 run 置 `running` 后永久卡死。现改为 `FailedPrecondition`（"no engine wired ... status-only mode"）且不做状态流转；`serve` 启动时输出引擎未接线的 WARN 日志；CLI `apply` 保留状态流转但帮助文本如实描述为 status-only、非 JSON 输出追加 WARNING 行、JSON 输出新增 `engine_wired:false`。
- **`drift schedule add` 恒 panic**：`DriftScheduler.AddJob` 按值接收 job，调度器生成的 ID/NextRun 只存在内部副本，调用方持有的 `job.ID` 始终为空——CLI 以空 ID 回查 `GetJob("")` 得到 nil 且错误被丢弃，随后 `jobToMap(nil)` 空指针崩溃（该命令自诞生起从未成功过）。`AddJob` 改为返回存储副本 `(*DriftJob, error)`，CLI 与磁盘装载路径改用返回值；`internal/drift` 无外部调用方，API 变更完全收敛。
- **`drift baseline delete` 静默无效**：删除后保存只写不删，磁盘上的孤儿 JSON 文件被后续任意 drift 命令重新装载，被删基线"复活"。新增 `syncJSONDir` 目录全量对账（写入期望集 + 删除不在期望集中的 `*.json`），`baseline delete` 与 `schedule remove`（同类问题）同时修复；对账顺带拒绝含路径分隔符的 host/ID 文件名，封死以主机名/作业 ID 逃逸数据目录的路径穿越面。
- **`drift report` 缺 `--host` 打印空主机趋势**：不再输出空 host 的伪趋势表，改为 `no host specified (use --host)` 并以 exit=2 退出。
- **`system config set` Windows 路径 panic**：定位配置目录时按 `'/'` 做 `LastIndex`，Windows 反斜杠路径返回 -1 导致切片越界 panic；改用 `filepath.Dir`。
- **gRPC 服务器发布竞态（CI `-race` 捕获，生产代码）**：`Server.Start` 在 `mu` 临界区外写 `s.listener`，与 `Stop`/`GracefulStop`/`Addr` 的锁内读构成数据竞态——`Addr()` 可能读到半发布状态或读到与关闭操作不一致的 listener。`Start` 的 listener 发布移入锁内，`Stop`/`GracefulStop` 锁内快照、锁外关闭，`Addr` 补锁读取。
- **ShellRunner 输出缓冲竞态（CI `-race` 捕获，生产代码）**：命令超时路径上主 goroutine 读取 stdout/stderr 汇总时，`exec` 的 io 拷贝 goroutine 仍在向同一 `bytes.Buffer` 写入。输出缓冲改为带互斥锁的 `syncBuffer`（`io.Writer` 接口不变），超时截断与正常完成两条路径统一消竞态。
- **变更事件总线发布竞态（CI `-race` 捕获，生产代码）**：`publishEvent` 先在 `bus.mu` 锁内取出订阅者 map 引用、解锁后再遍历，而 `WatchChange` 退出时的 `unsubscribe` 并发向同一 map 删除键——race 检测器报 `WARNING: DATA RACE`，且 Go runtime 对"边遍历边写 map"可直接 fatal 崩溃。改为 fan-out 全程持锁：投递本身是非阻塞 `select/default`，锁持有时间有界。
- **插件沙箱 `Stop` 竞态 + 必然空等宽限期（CI `-race` 捕获，生产代码）**：`Stop` 在 `s.mu` 临界区内等待 `doneCh`，而 monitor 必须先拿到 `s.mu` 记录 waitErr 才能关闭 `doneCh`——等待永远失败、宽限期每次都耗满，然后读 `cmd.ProcessState` 判断进程是否已被回收；该字段由 monitor 协程里的 `cmd.Wait()` 写入，无任何同步边，构成真竞态（`TestManager*`/`TestSandbox*` 六测试在 linux/macOS race 腿全红）。重构为：锁内快照 cmd/doneCh、锁外等宽限；升级 kill 不再读 `ProcessState`（kill 到已退出的进程本就无害，以 `doneCh` 关闭作为进程消亡的唯一证据）。附带效果：`internal/plugin` 包测试 18.9s → 1.3s，不再每测固定空等宽限期。
- **gRPC `GracefulStop` 超时回退死锁（CI 10 分钟超时元凶，生产代码）**：宽限期耗尽后回退调用 grpc 的 `Stop()`，与仍在进行的 `GracefulStop()` 并发——grpc 文档明示二者互斥；v1.83.1 实现中 graceful 路径持 server 互斥等待 `handlersWG`，并发 `Stop()` 与之死锁，`Serve` 的退出等待（`<-s.done`）连带挂起整个关停序列（ubuntu race 腿 `TestGracefulStopTimeoutFallsBackToHardStop` 挂 9m22s 拖死整包测试二进制）。新增连接追踪监听器：graceful 排空进行中触发硬停时不再触碰 grpc `Stop()`，改为关闭监听器与全部已跟踪连接——HTTP/2 传输层被强断、客户端收到连接错误、grpc 在传输层关闭时取消所有活动流的 context，尊重 ctx 的 handler 得以退出（忽略 ctx 的 handler 与 grpc 自身 `Stop()` 行为一致，只能等其自然返回）。`gateStore` 测试网关同步改为感知 ctx。
- **`unflattenPath` Unix 下返回双斜杠**：Unix 的 flatten 把前导 `/` 编码为第一个 `_`，unflatten 剥掉 `root` 标记后替换出的路径已自带前导分隔符，再手动前插一个分隔符得到 `//etc/nginx/nginx.conf`（Linux/macOS `TestUnflattenPathAbsolute` 红）；现仅当重建结果不以分隔符开头（Windows 盘符形态）才前插。
- **Dockerfile 构建基底标签不存在（trivy 作业红灯）**：`golang:1.25-alpine3.20` 从未发布（Go 1.25 镜像从未跟踪 alpine 3.20），`docker build` 直接拉取失败。builder 改用官方为每个受支持 Go 系列必发的无后缀别名 `golang:1.25-alpine`；运行时基底升 `alpine:3.22`（`ALPINE_VERSION` 自此只约束运行时阶段）。
- **Linux 构建上下文安全/风格残留（本地工具盲区补漏）**：Windows 本地 gosec/golangci-lint 看不见 `_linux.go` 文件，ubuntu 腿暴露 `sandbox_linux.go` 三处——cgroup 目录 `0o755`→`0o750`（G301）、控制文件打开模式 `0o644`→`0o600` 并补 `Close` 错误处理（G302/errcheck）、`formatCpuMax`→`formatCPUMax`（revive）。本地复验流程同步固化 `set GOOS=linux` gosec/lint/vet 三件套。
- **分布式锁并发冲突错误误分类（windows 腿红灯暴露的生产缺陷）**：`internal/lock` 的 `Acquire` 在"查重后插入"窗口被并发对手抢占时，底层 `CreateLock` 的数据库 UNIQUE 约束错误被当作 `lock: create` 通用错误抛出——调用方无法把"锁已被并发持有"与"存储故障"区分开，`ForceAcquire` 抢占竞态同样裸抛。新增 `isUniqueViolation`（按 SQLite `UNIQUE constraint failed` / PostgreSQL `duplicate key value violates unique constraint` 文案匹配，与 registry、inventory 既有惯例口径一致）：`Acquire` 输家归一为 `ErrLockHeld` 哨兵，`ForceAcquire` 输家自动重试。`TestManager_ConcurrentAcquire_SingleWinner` 断言同步收紧为恰好 `1` 个成功、其余全部 `ErrLockHeld`（旧界限恰好容忍了该缺陷，测试此前常绿、本次调度才暴露）。
- **PostgreSQL 并发建表目录竞态（integration&postgres 腿红灯，生产多节点同样暴露）**：`CREATE TABLE IF NOT EXISTS` 的存在性检查与系统目录插入非原子——多个连接并发创建同名表会在 `pg_type_typname_nsp_index` 唯一索引上相撞，输家事务整体以 `duplicate key value` 中止。CI 里 state 与 cluster 两个测试二进制并发创建 `cluster_nodes` 触发（state 的 pgschema.sql 与 cluster 的自建 DDL 都含该表）；生产上多节点同时对同一库首次启动同样会失败。`pgMigrate`（internal/state）与 `ensureClusterSchema`（internal/cluster）改在**专用连接**（`db.Conn`）上以共享 key 的 `pg_advisory_lock` 串行化 DDL——DDL 全程持有同一连接，完成经 `context.WithoutCancel` 解锁、`conn.Close()` 作为兜底释放（即使 DSN 配 `MaxOpenConns=1` 也不会自锁死）；`dbExecutor` 接口加 `QueryRowContext` 以同时接受 `*sql.DB`/`*sql.Tx`/`*sql.Conn`。新增回归 `TestClusterPGConcurrentEnsureSchemaSerialisesDDL`（6 goroutine 并发建 schema）。
- **运行镜像基底快照自带 HIGH CVE（trivy 门禁拦截）**：`alpine:3.22`（3.22.5）基底快照携带 openssl `libcrypto3`/`libssl3` 3.5.7-r0，CVE-2026-14456（HIGH，QUIC 服务器无界内存增长 DoS）在 3.5.8-r0 已修复——基底镜像发布后不再滚动更新补丁，`--ignore-unfixed` 下照样命中门禁。运行时阶段构建时 `apk upgrade --no-cache` 全量升到仓库当前发布版（本地实测 3.5.7-r0→3.5.8-r0），此后任何基底包已修复 CVE 不再需要改 Dockerfile。

### 测试

- **CLI 命令族 e2e（E-2）**：新增 12 个测试文件覆盖 drift / calendar / target / secret / pause / retry / audit / system / push / plugin / chatops / 共享 helper——每条命令走真实 `rootCmd.Execute()`，SQLite 临时库 + `--config` 隔离（push 族此前会写入真实用户目录，一并修复为隔离夹具）。harness 以"复位全部选项变量 + 遍历命令树清 pflag `Changed`"模拟 fresh process，消除 cobra 进程内全局状态跨用例泄漏（曾致 no-flag `drift detect` 误检、calendar 局部更新误报必填）。`cmd/levee`（剔除 serve）行覆盖率 45.9% → **66.8%**（质量方案目标 ≥60% 达成；最差的 `cmd_drift.go` 33.7% → 78.9%）。audit 族含 WORM 端到端篡改检测（临时库 DROP 触发器后裸 UPDATE 篡改 detail，`verify` 报 tampered / exit=6）。
- **前端单元测试基线（E-3）**：`web/` 引入 vitest + jsdom（38 用例全绿）——`api/client`（token 三态存取、Bearer 请求拦截、`AxiosError → ApiError` 归一化的 401/5xx/网络/预请求四类路径、401 清 token 与统一文案、403 不清 token，传输层在 axios adapter 处替换，拦截器与 baseURL 走真实逻辑）；`api/sso`（OIDC PKCE 与 GitHub 两条登录流的 state/verifier 持久化、CSRF state 校验、令牌交换请求体、JWT access_token 优先 / id_token 回退的选牌逻辑、登录后一次性凭据清理、开放重定向防护）；`utils/format`（时间戳/时长/运行时长格式化与边界、状态/优先级标签色表一致性）。jsdom 的 `window.location` 不可伪造（unforgeable），401 重定向与 SSO 跳转以可观测副作用（token 清除、storage 簿记）断言并在配置中定点 origin、过滤 jsdom 导航噪音；Node ≥25 原生 webstorage 全局遮蔽 jsdom Storage，`vitest.setup.ts` 以内存实现顶替。CI `frontend` 作业纳入 `npm run test`（vitest → vue-tsc → vite build 三连）。

- **集成套件语义对账（tests/integration）**：b12eafc 改变 apply 语义（有引擎同步完成至 `completed`、无引擎 `FailedPrecondition` 拒绝）后，lifecycle 套件的旧断言仍停留在"apply 后 running、可 pause"时代。重写：生命周期主用例改为 创建→计划→批准→apply→`completed`（run_id 非空、审计哈希链有效），终态 `CancelChange` 断言拒绝（`FailedPrecondition`）且状态不变；新增回归 `TestApplyChange_NoEngineRefused`（无引擎 apply 拒绝且状态停留 `approved`）；pause/resume 自终态非法、跨服务审计计数、哈希链完整性用例同步对账。
- **diagnosis 窗口用例 Linux 抖动修复**：`TestCollect_InvalidWindow` 原以秒级截断构造 `Start > End`，Linux 纳秒精度单调钟下不总成立（CI 偶发假阴/假阳）；改为同一时间戳显式构造非法窗口。
- **权限矩阵并发一致性用例改确定性启动同步（windows 腿红灯修复）**：`TestConcurrent_GrantThenReadConsistency` 断言"读者在写者并发期间至少观测到 1 次授权"，但该重叠纯靠调度碰运气——windows runner 上读者 1 万次扫描可全部跑完而写者一次 Grant 都未开始（`observed==0` 假红）。改为启动同步：写者首笔 Grant 后关闭信号通道、读者等通道再扫描；通道关闭顺带建立 happens-before，断言从概率性变确定性（其余 49+1 遍扫描仍与写者真并发）。
- **file 模块绝对路径拒绝用例按平台拆分（linux/macOS 腿红灯修复）**：`TestResolveLocalSrcAbsoluteRejected` 用 `C:\Windows\...` 作第二个绝对路径样本，但 Unix 上盘符路径是普通相对文件名（策略上合法），断言必然假红。改为按 `runtime.GOOS` 分发：Windows 验盘符绝对路径，Unix 验经 `filepath.Clean` 的 `..` 存活的根路径形态（两平台都保有双重样本）。
- **集群 leader 可见性断言改确定性收敛（windows/PG 腿红灯修复）**：`TestClusterPGTwoNodeVisibility` 在"发现对端 active"的 Eventually 之后立即裸读 `GetLeader`，但各 manager 的内存视图要等下一轮心跳才刷新——node-b 侧 leader 可能尚未收敛，断言纯赌时序。改为 `require.Eventually` 等双侧都产出 leader 再断言 ID 一致。
- **trivy 作业可见性重构（CI）**：门禁步 `format: sarif` + `exit-code: 1` 失败时 stdout 零输出，sarif 只被其后的上传步消费——而上传步没有 `if: always()`，门禁一红即被跳过，红灯完全盲视（上一轮 trivy 失败只能看到 exit 1）。门禁前新增"scan (print findings)"步（`format: table`、CRITICAL/HIGH、`exit-code: 0`）先打印明细，两个上传步改 `if: always() && hashFiles('trivy-results.sarif') != ''`。
- **trivy 门禁语义修正 + Code Scanning 权限补齐（CI，可见性重构揭出的两层潜伏问题）**：① trivy-action 在 `format: sarif` 下强制按全等级扫描（其日志自述 "Building SARIF report with all severities"，`severity` 输入不参与退出码判定），门禁步 `exit-code: 1` 实际会把可修复的 MEDIUM/LOW 一并拦红（x/crypto v0.55.0 的两条中低危即触发）——与门禁声明的"仅拦可修复 CRITICAL/HIGH"语义不符。sarif 步改 `exit-code: 0` + `limit-severities-for-sarif: true`（报告仍按 CRITICAL/HIGH 过滤），门禁移到两个上传步之后：显式 `jq` 统计过滤后 sarif 的 `runs[].results` 计数并打印 CVE 清单，红灯运行不再牺牲报告与 Code Scanning 告警。② `upload-sarif` 步历史上从未真正执行过（门禁先红即被跳过），`if: always()` 补上后立刻暴露 trivy 作业缺 `security-events: write`（`Resource not accessible by integration`），作业级 `permissions` 补齐。
- **check 聚合作业恒红修复（CI，首次全绿运行暴露）**：聚合脚本循环体里的 `${{ needs[job].result }}` 从未生效——Actions 表达式引擎在 bash 启动前渲染 `${{ }}`，引擎只见到字面 `job`（bash 循环变量对其不可见），查无此 needs 条目渲染为空串，`[[ "" != "success" ]]` 首个迭代必炸；此前从未暴露是因为运行总在叶子作业就红了，第 6 轮 16 个叶子全绿才把聚合作业自身打到红灯。改为把各 needs 结果以字面 `name:value` 对传入 bash 逐项检查，顺带把"遇首个失败即 exit"改为汇总报告所有未通过作业（错误信息携带具体 result）。首版修复还踩中次生坑：`run:` 块标量里的 YAML 注释是字符串内容而非注释，注释文字里的字面 `${{ }}` 被当作（非法）表达式解析，workflow 编译失败、零作业即红——注释改写为不含 `${{` 字面序列后消除。
- **探测门禁时延断言按 runner 时钟粒度加守卫（windows 腿偶发红灯修复）**：`TestProbeGateHTTPDirectPass` 对环回 HTTP 往返的 `assert.Positive(res.Latency)` 偶发假红——go1.26 升代首跑中同 sha 三连（race 腿绿 / master 腿绿 / main 腿红），失败信息 `"0s" is not positive` 意味着该 Windows runner 的单调钟把一整次环回往返读成零流逝（虚拟化 TSC/QPC 瞬时不推进），本地同平台 300 连跑零复现。断言前加有界探测：睡 1ms 看时钟能否观察到流逝，观察不到即 `t.Skip`（该断言在此 runner 上本就空洞），能观察则照常断言，回归防护不降级。同包 `TestRunPhaseLatencyPopulated` 以 20ms 人为延迟测时，天然免疫亚毫秒抖动，不改。
- **沙箱 Start/Stop 用例改存活脚本（ubuntu race 腿偶发红灯修复）**：`TestSandboxStartAndStop` 用 `writeExitScript(t, 0)`（**立即退出**的脚本）却在 `Start` 后立刻断言 `IsRunning()==true`——monitor 协程可能先一步收割进程，断言纯赌调度时序，满载 race runner 上偶发 "Should be true" 假红（0.00s 即败露是其特征）。该用例语义本就只是 Start→running→Stop→stopped（干净退出/崩溃重启语义由 `TestSandboxCrash*` 系列专职覆盖），改用 `sleep 5` 存活脚本消除竞态；Stop 自带 `stopped` 标志抑制重启，`MaxRestarts=0` 保留为意图声明。
- **KMS 回退测试夹具吞错超时修复（macOS race 腿红灯定位）**：`storeInFallbackForTest` 向本地回退库写种子凭据时以 5s 超时 ctx 调用 `Store` 且错误被 `_, _ =` 丢弃——满载共享 runner 的 `-race` 腿下 argon2id KDF 主导的 `Store` 实测可达 ~9s（同 run 日志相邻两条 `credential stored` 间隔即 8-9s），超时静默过期后测试以 `credential: not found` 假象收场（子测试 5.54s ≈ 5s 超时 + 探测开销），纯时序赌博、此前绿全靠运气。夹具改为 60s 超时且 `require.NoError` 大声失败（存储失败直接指向 Store 而非误导性的 GetCredential not found），并入唯一调用方、删除中间层；同子测试里一段从未生效的死脚手架（5 字节 `pt` 与被 `_ = err` 吞掉的首次调用）一并清除。

### 前端

- **401 统一处理（UX）**：`web/src/api/client.ts` 响应拦截器对 401 统一清除本地 token 并跳转 `/login`（登录/回调页豁免防死循环；并发 401 只触发一次重定向），错误文案统一为「登录已过期，请重新登录」。`internal/web/dist` 同步重新构建。

### 文档

- 修正 `internal/grpc/rest.go` 中 `SetExtraRoute` 鉴权语义的矛盾注释（实际行为：配置 token 时挂载端点默认要求 Bearer 鉴权，CORS/限流不适用）。
- 补注 `closure.go` 回滚路径刻意使用 `context.Background()` 的意图（回滚触发即不可取消，依赖逐步执行超时控制运行时长）。
- 修正 `.golangci.yml` 中 `misspell` 启用的过时注释（原文误标 disabled）。

### 安全修复

- **x/crypto 升级 v0.56.0（连带 go1.26 工具链升代）**：收口 trivy 台账上最后两条依赖类遗留（CVE-2026-56855 MEDIUM、CVE-2026-78662 LOW）。v0.56.0 声明 go 1.26.0，故模块 go 指令 1.25.0→1.26.0，`ci.yml`（7 处 setup-go）、`release.yml`、`Dockerfile` `GO_VERSION`、README/quickstart 同步升代。go1.26 弃用 `httputil.ReverseProxy.Director`（staticcheck SA1019 即拦），`internal/web` API 代理迁移到等价的 `Rewrite` 形态（`SetURL`+`SetXForwarded`+固定出站 Host），`TestServer_ApiProxy` 断言收紧为锁定"完整路径透传 + Host 钉到上游"两条行为契约。
- **REST 方法校验（P1）**：`/changes/{id}/{plan,apply,approve,reject,pause,resume,cancel,retry,rollback,archive}` 现强制 `POST`、`/changes/{id}/{logs,trace}` 强制 `GET`；此前任意 HTTP 方法（含 GET）即可触发状态变更，爬虫/预取可误暂停变更。`pause/resume/archive` 不再吞掉请求体解析错误：空体合法、畸形 JSON 返回 400。
- **SSH BecomeUser 注入（P2）**：`buildExecCommand` 现对 `become_user` 也做 POSIX shell 引用（此前仅引用命令本体），阻断来自配置值的 `sudo -u` 注入面。
- **/metrics 默认鉴权（P2）**：网关 `SetExtraRoute` 挂载的运维端点（`/metrics`）在配置了任一 token 时默认要求 Bearer 鉴权；新增 `--metrics-public` / `ServeGatewayConfig.MetricsPublic` 显式放开（供无法携带凭据的采集器）。
- **凭据 blob 版本化（SA-005/SA-015）**：加密 blob 添加版本前缀 + 自描述 KDF 参数——存量密文按写入时的参数解密、新写入用当前参数。收口 v1.11.0 argon2id 提参至 194MiB 造成的存量密文断代（v1.11/12 为 194MiB 代、v1.10 前为 64MiB 代），并为未来算法/参数迁移留出演进路径。
- **随机 ID 生成失败统一硬失败（SA-012）**：身份类标识（凭据 ID、deeplink 一次性授权 token、审批/运行/锁/快照/租户/通知/计划/盘点导入等）在 `crypto/rand` 失败时一律返回错误并逐点传播，deeplink 不再降级为可猜测的 `fallback-<nano>` 时间戳；纯观测类 ID（请求关联标签、临时文件名、展示标签）保留降级并以注释标注分类；`change_service` 的 panic+recovery 策略经评审豁免。
- **审计脱敏增强（SA-009/SA-010）**：内置敏感词表 8→16（新增 passphrase/auth_code/refresh_token/access_token/ssh_key/cert/certificate/connection_string）；匹配升级为 `_`/`-` 词边界后缀命中（`db_password` 命中、`sort_key` 不误伤、裸 `key` 仅全等匹配）；新增 `security.sensitive_fields` 自定义词表（进程级注册、热路径无锁）；脱敏支持结构体/嵌入/切片/指针的 reflect 遍历（可见性口径 = `json.Marshal` 可见字段，无命中子树原值透传零漂移）。已文档化边界：值内嵌明文（如 error 文本中的 `secret=…`）无键上下文，键名规则不可覆盖。
- **权限矩阵加固（SA-013/SA-016）**：`Grant`/`Revoke` 未知 action 默认 WARN 后仍记录（兼容存量拼写），新增 `StrictActions` 严格模式整批拒绝且装载原子生效；`admin` + 环境通配 `*` 授予组合在装载完成后汇总 WARN 一次并列出受影响团队。
- **权限拒绝审计接线（SA-007，部分修复）**：CLI 全局暂停/恢复的权限拒绝现落审计表一行（`permission.denied`，含 Actor 与权限串；选择 audit 表因 CLI 拒绝发生在选定任何 run 之前）。剩余边界已文档化：serve 路径不构造 PermissionMatrix（`LoadFrom*` 仅 cmd_user/cmd_rbac 调用），gRPC 侧权限检查接线待生产装配。
- **WORM 重复写入错误语义收口（SA-014，台账降档 LOW）**：`state.CreateTrace` 将驱动 UNIQUE 约束错误映射为 `ErrTraceExists` 哨兵（SQLite 按约束错误码集合判定、不误分类 FK 违反；PG 按 SQLSTATE 23505），`WORMStore.Append` 转译为 `ErrAlreadyExists`——TOCTOU 竞态输家不再收到不透明 SQL 错误；双后端并发双写约束测试固化"恰好一方成功"。
- **凭据 Tags 持久化（SA-018）**：新增 `credentials.tags` 列（schema v2 迁移步，SQLite/PG 形状一致，v1→v2 升级以手工构建的旧库文件实测），以 JSON map 存储；无 tag 行以 `''` 存储与历史行不可区分；`Rotate`/`RotateMasterPassword` 保留 tags。
- **SQLite 持久级别可配置（SA-019）**：新增 `state.sqlite_synchronous = normal|full`（默认 `normal`；`full` 每提交 fsync，供审计强持久场景，写放大约 1-2%；非法值拒绝启动），环境变量 `LEVEE_STATE_SQLITE_SYNCHRONOUS` 同步可用。
- **RetrieveInto 回调式凭据读取（SA-011，部分修复）**：`CredentialStore.RetrieveInto` 在回调返回（含 panic 展开）后必然 `SecureZero` 明文；`Retrieve` 文档清零义务升为 MUST 并推荐新代码走 RetrieveInto。已知残留（代码注释标注、非本轮修复）：serve 凭据解析器裸密码路径经 `string` 返回（`CredentialRef.Password` 为 string 类型不可清零，类型改造波及 ssh/winrm/grpc 通道）。

### 新增

- **计划持久化与执行门（A1 / 引擎接线第一阶段）**：`PlanChange` 生成的执行计划自此落库——`runs` 表新增 `plan_json` 列（SQLite/PG 双轨 schema v3 迁移，旧库原地升级），与既有 `plan_hash` 一同构成"被批准的就是被执行的那个计划"的锚点。`EngineAdapter.Plan` 接缝升为同时返回面向客户端的 `pb.Plan` 与可持久化的 `StoredPlan{JSON,Hash}`（`PlanChange` 在非 dry-run 时写入 run；dry-run 仍为纯预览不落库）。`ApplyChange` 在引擎已接线的部署中于状态 CAS **之前**执行两道诚实校验并全部以 `FailedPrecondition` 拒绝、不消耗 run：无存量计划（引擎接线前创建的旧 run——明确引导重新 plan）与 plan_json 重算哈希 ≠ plan_hash（计划被替换/损坏，即参数漂移）。`ApproveChange` 同步收紧：引擎已接线时，批准必须对着已持久化的计划。无引擎部署（当前 serve 默认形态）行为零变化。新增 7 项门语义测试（持久化/预览不落库/缺计划拒/漂移拒/损坏拒/批准门/旧形态不变）。
- **GitHub OAuth 单点登录（可选）**：GitHub 不是 OIDC 提供方（access token 不透明、token 端点无浏览器 CORS），故 code 交换在服务端完成——浏览器 POST `{"code","state"}` 到公开端点 `/auth/github`，网关持 client_secret 与 GitHub 换取 token、调 `/user`+`/user/teams` 完成身份/成员校验后**丢弃 GitHub token**，签发 LEVEE 本地会话令牌（HS256 HMAC、12h TTL、`internal/auth/session.go`）返回浏览器；后续请求走本地会话令牌验证，**不逐请求访问 api.github.com**（无限流/可用性耦合），client_secret 全程不出进程。凭据解析升为三源：静态（constant-time）→ LEVEE 会话令牌 → OIDC JWT，gRPC 与 REST 一致；审计主体 = GitHub login（注入 actorKey，覆盖 X-Acting-As），`team_role_map`（`"org/team-slug"` 键）映射团队为角色，`org` 限制仅组织成员可登录（内部部署必须设置）。`session_secret` 为空时进程内随机密钥（重启失效、多节点不共享，启动时告警；多节点须配 ≥32 字符共享密钥）。配置 `auth.github` 段（`LEVEE_AUTH_GITHUB_*` 同步可用；`team_role_map` 仅文件）。安全门同步扩展：无任何凭据源（含 GitHub）仍拒绝启动。前端：登录页"通过 GitHub 登录"按钮（auth-info 探测），`/login/callback` 按提供方分发（GitHub → POST 网关交换；OIDC → 浏览器直连 IdP），state 防 CSRF 两种流程共用。
- **OIDC 单点登录（可选）**：`serve` 支持 OpenID Connect JWT 作为静态 Bearer 令牌之外的附加凭据源，与 IdP 无关（Keycloak / Zitadel / Entra ID / Okta 等标准提供方均可）。新增 `internal/auth` 包（`coreos/go-oidc/v3`）：启动时对 `auth.oidc.issuer_url` 做发现（10s 超时，失败拒绝启动——fail-fast，防止误配置导致令牌永远验不过却无告警），验证签名（JWKS）、issuer、audience、expiry；主体取 `username_claim`（默认 `preferred_username`，回退 `email`、`sub`），`role_claim` 声明经 `role_map` 映射后注入上下文（v1 仅审计/预留，无授权消费）。凭据解析顺序：静态令牌（constant-time）→ JWT 形态令牌走 OIDC 验证；启用 OIDC 不影响既有静态令牌，gRPC 拦截器与 REST 中间件行为一致。配置经 `auth.oidc` 段（`LEVEE_AUTH_OIDC_*` 环境变量同步可用，`role_map` 仅文件可配）。安全门同步扩展：无 `--token`、无 `--auth-token` 且未启用 OIDC 时仍拒绝启动。前端：登录页经公开描述符 `GET /system/auth-info`（免 Bearer）探测 SSO，展示“通过 SSO 登录”按钮，走 authorization code + PKCE（公共客户端，Web Crypto 生成 verifier/challenge，state 防 CSRF）；新增 `/login/callback` 在浏览器直接与 IdP token 端点交换令牌（要求 IdP 允许本站点 CORS），存 access_token（JWT 形态）或回退 id_token；静态令牌登录路径完全保留。
- **多令牌身份认证**：`serve` 新增可重复 `--auth-token name=secret`，每个命名令牌映射到一个主体（subject）；gRPC 拦截器与 REST 中间件均支持 `AuthTokens`（`Legacy` + `Named`）。命名令牌认证后，其主体注入请求上下文并**优先于**客户端自报的 `X-Acting-As`，使审计归属为“被证明的身份”而非“断言”。单令牌（`--token`/`LEVEE_TOKEN`）行为完全向后兼容。
- **serve 装配 AI 引擎**：`levee serve` 现装配真实的诊断引擎（日志管线 + 健康探针，本地执行器）与对话引擎（内置推荐引擎），`Diagnose`/`SendMessage` RPC 不再返回 `Unimplemented`；告警服务保持独立环形存储（完整网关仍用 `levee alert serve`）。
- **前端 CI 门禁**：新增 `frontend` 作业（`vue-tsc --noEmit` + `vite build`），并纳入 `check` 聚合门禁。
- **发布工作流**：新增 `.github/workflows/release.yml`——推送 `v*` tag 时自动触发 goreleaser，按既有 `.goreleaser.yml` 构建 linux/darwin/windows × amd64/arm64 产物并发布 Release；此前仅有 goreleaser 配置、无触发工作流，发布依赖手工构建。
- **集群成员持久化与租约锁**：集群模式（`serve --cluster`）新增两项协调能力。其一，持久化节点注册：节点写入 `cluster_nodes` 表并周期心跳（健康循环每 10s 刷新、30s 未心跳被标记 offline），各节点本地注册表从共享表收敛，leader 按确定性策略（active master 最小 ID，缺则 active worker 最小 ID）独立收敛——此前节点注册仅为进程内，节点之间互不可见。其二，租约式分布式锁（`cluster_locks` 表）取代原 PG advisory lock：锁带租约过期时间，持有者周期续租，持有者崩溃/失联后租约到期即可以被其他节点自动抢占（advisory lock 永不失效，挂死但连接存活的持有者会永久阻塞锁键）；每次获取/抢占携带单调递增的 fence token 供接管隔离。两张表由 cluster 包首次使用时自建（幂等 DDL），不依赖 state 包 schema 版本。在途变更的自动故障转移与跨节点调度仍未实现，启动告警文案同步更新。
- **protobuf 契约补源与双轨再生**：新增 `proto/levee_extra.proto`，为手写的 AlertService / DiagnosisService / ConversationService / InventoryService 四个服务补齐 .proto 定义（共 24 个消息，字段编号与既有手写 pb 的 struct tag 逐一比对对齐）；原内嵌于 `proto/levee.proto` 尾部的 extra 定义拆分至该文件（主文件仅保留核心域消息，尾部留有指引注释）。新增 `scripts/gen-proto.sh`（锁定 protoc v27.0 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2，与已检入生成代码的版本头一致）。手写文件（`levee_extra*.pb.go`、`inventory_extra*.pb.go`）标注 `//go:build !proto_regenerate`，默认构建行为不变；protoc 再生输出以 `levee_extra*.regen.pb.go` 落盘并标注 `//go:build proto_regenerate`，作为可再生的验证轨道（两轨字段/服务契约已逐字段比对一致，双轨全量测试均通过）。新增 CI `proto` 作业：用锁定版本工具链再生后与检入文件做字节级漂移检查（git diff），并在 proto_regenerate 轨道执行 build + 全量测试；已纳入 `check` 聚合门禁。

### 变更

- **CI 工具链与口径对齐**：`.gitattributes` 统一行尾；CI golangci-lint 升至 v2.13 与本地同配置（本轮清零存量 34 处告警）；CI 覆盖率统计剔除生成代码。E 轮测试补齐后 **CI 覆盖率地板 60% → 70%** 落地（state sqlite+PG 联合口径 ≥75%、CLI 剔 serve ≥60%、web vitest 纳入 CI）。
- **CI 行动项修复（master 转绿）**：地板抬升触发的那次 CI 运行暴露 8 个红作业——其中 lint、trivy、gosec 等为存量债务（master 自 2026-08-28 前即持续红色，与本轮提交无关），逐项修复：`golangci-lint-action` v6→v7（v6 无法解析 v2.x 工具版本串）；`trivy-action` v0.30.0→v0.36.0（v0.30.0 的组合实现按已被上游删除的 `setup-trivy@v0.2.2` tag 引用，v0.36.0 已改为 commit SHA 钉定）；Windows 测试步骤给 `-coverprofile=coverage.out` 加引号（pwsh 默认 shell 会把未加引号的路径拆成 `-coverprofile=coverage` + 裸包名 `.out` 导致 setup 失败）；其余为上文两个 `-race` 生产竞态、diagnosis 窗口用例与集成套件语义对账。
- **gosec 全量分诊（60 → 0）**：默认流水线口径（剔 pb）下 60 条发现逐条处置——28 处权限收紧（目录 0o755→0o750、文件 0o644→0o600，快照/备份/盘点库/模板库/插件沙箱等落盘路径）；32 处附逐条理由的 `#nosec` 标注（每条先读代码核实：如快照恢复的 Walk 遍历的是操作员自有存储目录、calendar 动态 WHERE 全部为 `?` 占位参数拼接、SSH 弱主机密钥校验是配置显式 opt-in 且严格模式永不降级、`system config set` 写的是操作员 `--config` 自指路径且无服务端可达路径）；G304（40 条，"按变量路径加载配置/模板/插件/基线"即产品形态）与 G115（17 条，protobuf 分页/计数的有界 int↔int32 转换）两类经全量人工审读后在 CI 参数整体排除并注释理由。此后 gosec 新增任何一条发现都是净新信号。
- **Trivy 阻断合并**：`trivy` 作业对可修复的 CRITICAL/HIGH CVE 设 `exit-code: 1`，由“仅上报”改为“阻断”。
- **集群模式如实标注**：`serve --cluster` 启动时输出告警，说明当前集群协同仅限共享 PostgreSQL 存储（数据一致性 + 咨询锁），节点注册为进程内、尚无自动故障转移/跨节点调度；README 特性描述同步收敛。
- **前端产物清理**：`internal/web/dist` 重新构建，移除历史遗留的多代哈希资产，仅保留当前一代。
- **手写服务 pb 全量切换为生成代码，REST 序列化统一 protojson**：AlertService / DiagnosisService / ConversationService / InventoryService 的 pb 绑定现为 protoc 生成代码——手写文件与 `proto_regenerate` build tag 双轨脚手架移除（上一条目中的 `levee_extra*.regen.pb.go` 提升为正式文件），`scripts/gen-proto.sh` 直接产出全部四个绑定文件，CI `proto` 作业相应简化为工具链再生 + 字节级漂移检查 + 构建。`/api/v1/{AlertService,DiagnosisService,ConversationService}` 五个非流式端点由手写反射镜像切换为 protojson（`rest.go` 移除 `readProtoJSON`/`writeProtoJSON` 及约 270 行 protoLike* 镜像机制），与其他全部端点统一遵循 13.5 约定。**客户端可见变化**：这五个端点的响应中，proto3 标量零值不再显式输出（此前 `0`/`""`/`false` 会出现，现按规范省略），int64 由 JSON 数字改为 JSON 字符串；请求体仍同时接受 camelCase 与 snake_case 拼写。仓库前端未消费这五个端点（已全量检索确认），新增契约测试 `TestExtraServicesProtoJSONContract` 固化零值省略 / int64 字符串 / 双拼写兼容三项行为。

### 文档

- README 版本号由过时的 v1.10.0 更正为 v1.12.0。
- 补充 v1.5.0 跳号说明（该号未发布）。
- 修正 `.gitignore` 中 `.env"coverfunc.txt"` 的粘连/引号错误。
- 新增生产部署与升级手册（docs/deployment.md）。
- 补全 CLI 参考缺失命令（alert/backup/restore/group/converse/diagnose 等）。
- `docs/levee-api.md` 新增 13.5 节，声明 REST JSON 字段约定：protojson 序列化采用 lowerCamelCase 键名、**省略 proto3 零值字段**（客户端须将缺失字段视为零值）、int64 输出为 JSON 字符串；并如实标注 AlertService/DiagnosisService/ConversationService 五个端点的手写镜像序列化差异（标量零值显式输出、int64 为数字）。同步修正 13.1 与实现不符的描述：REST 成功响应为 proto 消息直接序列化（无 `data`/`meta`/`error` 包装），HTTP 状态码映射改按 `writeGRPCError` 实际实现（移除不存在的 422，补 412/429/501）。

### 修复

- **Target.status 序列化丢失**：`pb.Target` 的 `Status` 字段（v1.11.0 引入，af62981）当时以手工编辑方式加入生成文件 `internal/grpc/pb/levee.pb.go` 的 Go 结构体，未同步更新文件内嵌的 protobuf 描述符（rawDesc）。protobuf-go 的序列化完全由描述符驱动，该字段对 protojson 与二进制 wire 编码均不可见——“REST/gRPC 暴露库存生命周期状态”实际静默失效：字段在进程内可读写，但 REST 响应与 gRPC 传输永远丢弃它（实测 wire 字节与 protojson 输出均无 field 8）。本次以锁定工具链从当前 `proto/levee.proto` 再生该文件（与手工版本的差异仅为 field 8 的描述符字节与一行注释），`status` 现按 proto3 语义正常序列化（非空时输出，空值省略）。
- **移动一键审批 401**：`/changes/deeplink/approve` 与 `/changes/deeplink/reject` 现接受请求体内的一次性 token 作为认证凭据，不再要求 Bearer 头——此前启用鉴权后，无法携带 Bearer 的移动设备点击审批深链必然 401。一次性 token 为 32 字节随机数、30 分钟 TTL、单次消费并绑定 (run-id, 用户, 动作)；无效/过期 token 现返回 401（此前被不透明地映射为 500）。豁免仅覆盖这两个端点，其余端点的 Bearer 校验不受影响。
- **告警订阅流测试竞态**：`SubscribeAlerts` 广播为尽力而为（best-effort），订阅注册完成前发布的告警会被丢弃；相关流式测试此前以固定 `time.Sleep` 等待注册，在高负载下（如完整 `go test ./...`）可能竞态导致 `RecvMsg` 永久阻塞（触发 10 分钟单测超时）。新增 `AlertService.SubscriberCount()` 观测方法，全部 5 个订阅流式测试改为 `require.Eventually` 等待注册完成后再发布告警，消除挂起。

## v1.12.0 - 2026-08-27

### Added

- **Self-metrics endpoint** (`internal/metrics/`): lightweight atomic-counter collector exposing 10 metric families (change lifecycle, batch duration, gates, approvals, channel acquisition, locks, rollbacks, backups, alerts) in Prometheus text 0.0.4 format; mounted at `/metrics` on the REST gateway; coverage 98.1%, 19 tests
- **Data backup/restore** (`internal/backup/` + `levee backup` / `levee restore` CLI): SQLite backups via `VACUUM INTO` + `integrity_check` + `.sha256` checksum; pure-Go PostgreSQL SQL dump via pgx (no `pg_dump` dependency); restore performs an automatic `.pre-restore` safety backup, atomic replacement, and stale-WAL cleanup; flags: `backup [--output] [--verify-only] [--pg-dsn]`, `restore --input <path> [--yes]`; coverage 86.8%, 60 tests
- **OpenTelemetry tracing** (`internal/tracing/`): Tracer interface + stdouttrace exporter + W3C `traceparent` parse/format utilities; initialized on `serve` startup with graceful noop fallback on failure; new `tracing` config section (`enabled` / `exporter` / `endpoint`, disabled by default); OTel v1.44.0; coverage 95.3%, 17 tests
- **Dependabot** (`.github/dependabot.yml`): scheduled dependency updates for gomod / github-actions / docker ecosystems
- **CodeQL** (`.github/workflows/codeql.yml`): security analysis on push / pull request / weekly schedule
- **`config.example.yaml`**: complete example of all 52 config keys, cross-checked one-by-one against `internal/config/config.go` (zero fabricated keys), with default values and scenario examples
- **`CODE_OF_CONDUCT.md`**: Contributor Covenant 2.1 full text

### Changed

- `internal/grpc/rest.go`: added `SetExtraRoute` generic extension point (~30 lines) so external handlers can be mounted on the gateway mux (used for `/metrics`); no impact on existing behavior

详细说明见 [docs/release-notes/v1.12.0.md](docs/release-notes/v1.12.0.md)。

## v1.11.0 - 2026-08-27

### 安全加固

- **P0 网关接线修复**：`levee serve` 此前会启动未挂载任何服务的 REST 网关；现改为启动配置的服务实例。`/healthz` 在服务注册完成前返回 503 `{"status":"unavailable"}`（服务随 serve 自动注册，正常运行时为 200）
- user 模块命令行参数统一引号转义，防止参数注入
- WinRM 通道 PowerShell 路径转义修复
- SSH 主机密钥校验默认开启（strict-by-default，known_hosts 路径与豁免开关可配）
- 审批 / 归档状态机守卫：拒绝非法状态迁移并留痕
- WORM 级联删除防护：SQLite 开启 `PRAGMA recursive_triggers=ON`，外键 `ON DELETE CASCADE` 触发的删除同样命中审计保护触发器
- [SA-004] SecureZero 添加 runtime.KeepAlive 防止编译器优化
- [SA-005] argon2id memory cost 提升至 194MiB（OWASP 2024 推荐）
- [SA-006] 权限矩阵添加 sync.RWMutex 保证线程安全
- [SA-007] 权限校验拒绝时自动记录审计 trace
- [SA-008] 哈希链排序添加二级排序键确保确定性

> **勘误（2026-09-06 追加，不改写上文历史行文）**：上文 SA-007 条目与当时实际落地不符——v1.11.0 交付的是拒绝审计**机制**（recorder 注入点），生产路径（CLI/serve）并未接线，权限拒绝不会自动落审计记录，"自动记录审计 trace"属过度声明（核查证据见 `docs/security-audit.md` "2026-09-06 核查记录"）。实际接线（CLI 全局暂停/恢复拒绝落审计表）于 Unreleased 落地。

### Bug 修复

- macOS/BSD 插件沙箱构建修复（sandbox unix 构建约束整理）
- 列表分页 `offset` 参数生效（SQLite / PostgreSQL 存储层）

### Web UI

- 前后端 API 契约对齐；新增登录页

### 文档

- CLI 文档与实现对齐：`new <template> --params k=v`（移除虚构的 `--file/--template/--dry-run/--label/--priority`）、删除不存在的 `levee plan --dry-run` 步骤、push config / tenant create（配额默认 0 = 不限，存储单位 MB，补 `--max-api-rate`）/ drift schedule add（去 `--baseline`，补 `--alert/--enabled`）/ drift report（无 `--format`）/ agent start（默认值修正，补 `--max-concurrent`）/ serve 补 `--http-addr`

### 工程修复

- vet/gofmt 清理：`cmd_serve.go` IPv6 安全的 host:port 拼接（`net.JoinHostPort`），3 个文件格式对齐
- 添加 Apache-2.0 LICENSE 文件

## v1.10.0 - 2026-08-23

### 安全加固（审计修复）

#### 认证与访问控制
- **auth 启动门禁**：`levee serve` 无 token 时拒绝启动，除非显式传 `--insecure`；token 可经 `--token` 或 `LEVEE_TOKEN` 环境变量提供
- **CORS 默认拒绝**：origin 列表为空不再隐含通配，需显式配置 `"*"`
- Bearer token 校验改用 `crypto/subtle.ConstantTimeCompare`
- 无 TLS 启动时输出明文传输警告日志

#### 凭据处理
- user 模块密码不再出现在命令行参数中：凭据以临时文件上传后 `chpasswd < file` 消费，避免泄露到 argv / SSH 日志

#### Linux 插件沙箱
- 基于 cgroup v2 的硬性资源限制：`memory.max` + `cpu.max`
- 插件进程挂入独立 `levee-plugin-{pid}` cgroup
- cgroup 不可用时优雅降级（保留墙钟超时兜底）
- 新增 `sandbox_linux_test.go` 验证测试（无 cgroup 写权限时跳过）

### REST 网关

- RESTful 路由完善：`/api/v1/changes/{id}` 等 `/:id` 路径正确解析（修复前导斜杠导致的 400）
- HTTP 端点 token 认证中间件
- 全局令牌桶限流：`--rate-limit` / `--rate-burst`，429 + Retry-After
- 请求 ID 追踪：`X-Request-Id` 响应头 + gRPC metadata 透传
- 移动审批 deeplink 端点
- 注册标准 `grpc.health.v1` 健康服务（Start/Stop 联动 SERVING/NOT_SERVING）

### Bug 修复

- GetLogs 现在生效 `levels` 过滤（stdout→INFO，stderr→ERROR）
- GetTrace 显式 `run_id` 优先于 change 级默认值
- ArchiveChange purge 对 WORM trace 的保留行为改为显式设计并文档化
- REST 网关限流与请求 ID 中间件

### CI/CD

- 新增 gosec 静态安全扫描 job（JSON 报告上传为 workflow artifact `gosec-report`）
- 新增 trivy 容器镜像扫描（SARIF 上传 GitHub Code Scanning；CRITICAL/HIGH 阻断）
- `check` 聚合门禁 job 覆盖 vet/lint/test/build/gosec/trivy

### 测试覆盖率

| 包 | 之前 | 之后 |
|---|---|---|
| internal/grpc/ | ~39% | 80.5% |
| internal/state/ | 34.9% | 55.5% |
| internal/tenant/ | — | 90.5% |

### 已知限制

- 多租户隔离在 store 层有契约测试，但 daemon 主路径未接线（MVP 范围决策，V2 再评估）
- cgroup 沙箱要求 `/sys/fs/cgroup` 可写（root 或授权容器）

详细说明见 [docs/release-notes/v1.10.0.md](docs/release-notes/v1.10.0.md)。

## v1.9.0 - 2026-08-18

### Phase D: 高级诊断

#### D1: SkyWalking/Pinpoint 拓扑分析
- 新增 `internal/diagnosis/topology/` 包
- 统一 `Collector` 接口 + `Topology`/`Node`/`Edge` 类型
- SkyWalking GraphQL API 客户端
- Pinpoint REST API 客户端
- 覆盖率 93.0%

#### D2: Zabbix/Nagios 告警适配器
- 新增 `internal/alert/adapter_zabbix.go` + `adapter_nagios.go`
- Zabbix webhook JSON payload 解析（支持单对象和数组）
- Nagios HTTP webhook JSON payload 解析（支持单对象和数组）
- 哨兵错误 `ErrInvalidPayload` / `ErrMissingField`
- 覆盖率 93.2%

#### D3: LLM 对话式诊断
- 新增 `internal/diagnosis/llm_diag/` 包
- 多轮推理引擎 `ReasoningEngine`
- 推理上下文 `ReasoningContext` + `Turn` + `ReasoningStatus`
- 收敛检测 + 最大轮次控制
- 覆盖率 95.0%

#### D4: RAG 知识库增强
- 新增 `internal/recommend/rag/` 包
- `EmbeddingProvider` 接口 + `MockEmbeddingProvider`（FNV-1a 哈希确定性嵌入）
- `VectorStore` 接口 + `InMemoryVectorStore`（余弦相似度，线程安全）
- `Retriever` + `AugmentPrompt` RAG pipeline
- 覆盖率 94.4%

#### D5: 修复效果学习
- 新增 `internal/recommend/feedback/` 包
- `FeedbackLearner`：Record / Learn / RecordAndLearn / GetStats
- 反馈循环：成功→创建新 FixPattern + HistoricalIncident 添加到 KB
- 线程安全（sync.RWMutex）
- 覆盖率 93.5%

## [1.8.0] - 2026-08-18

### Added — Phase C: 自动执行 + OpsMesh 集成

- **C1 AutoPlanner** (`internal/autoplanner/`): 自动任务拆分引擎，将 AI 推荐（Recommendation）转换为可执行的 LEVEELang workflow，包含风险评估和批次划分
  - `AutoPlanner.Plan()` — Recommendation → Workflow 转换
  - `RiskAssessor.Assess()` — 风险→审批级别映射（低危→标准/中高危→高危/紧急→紧急）
  
- **C2 AutoExecutor** (`internal/autoplanner/auto_executor.go`): 全自动执行模式
  - 低危（RiskLow）→ 自动执行（LevelStandard）
  - 中高危（RiskMedium/High）→ 需人工确认（LevelHigh）
  - 紧急（RiskCritical）→ 紧急审批（LevelEmergency）
  - 失败自动回滚，回滚也失败则升级告警
  - 三种执行模式：ModeDryRun / ModeAuto / ModeForce
  
- **C3 PostReport** (`internal/autoplanner/post_report.go`): 事后审计报告生成
  - 修复摘要 + 指标对比（MetricsBefore/After/Delta）+ 审计链验证
  - `ToText()` 纯文本格式 + `ToJSON()` JSON 格式
  
- **C4 OpsMesh Client** (`internal/opsmesh/`): OpsMesh 平台集成客户端
  - `ReportResult()` — 回传修复结果给 OpsMesh
  - `GetTopology()` — 获取服务拓扑
  - `GetMetrics()` — 获取监控指标
  - `Ping()` — 健康检查
  - HTTP Bearer 认证 + 重试 + 限速
  
- **C5 gRPC Services** (`internal/grpc/`): 3 个新 gRPC 服务
  - `AlertService`: ReceiveAlert / GetAlertStatus / SubscribeAlerts（流式）
  - `DiagnosisService`: Diagnose / GetDiagnosis
  - `ConversationService`: SendMessage / SubscribeConversation（流式）
  - 手动编写 pb 代码（protoc 不可用）

### Changed

- `proto/levee.proto`: 追加 AlertService / DiagnosisService / ConversationService 定义
- `internal/grpc/pb/`: 新增 levee_extra.pb.go + levee_extra_grpc.pb.go（手动编写）

### Test Coverage

| 包 | 覆盖率 |
|----|--------|
| internal/autoplanner (C1) | 93.2% |
| internal/autoplanner (C2) | 96.77% |
| internal/autoplanner (C3) | 100% |
| internal/opsmesh (C4) | 90.3% |
| internal/grpc (C5) | 28 tests pass |

## [v1.7.0] - 2026-08-16

### Phase B — AI 建议 + 对话引擎

#### 新增特性
- **B1 知识库框架**：历史故障/Runbook/FixPattern 匹配引擎，Jaccard + 症状 + 根因三维评分
- **B2 LLM 集成**：OpenAI/Ollama adapter + Mock client，8 条内置脱敏规则（IP/密码/API key/DB连接/JWT/AWS key/邮箱/手机号）
- **B3 修复方案生成**：RecommendEngine 集成知识库+LLM+脱敏+优雅降级，WorkflowGenerator 生成 LEVEELang YAML 草稿
- **B4 对话引擎**：ConversationEngine 多轮对话状态机（Idle→Diagnosing→Recommending→Reviewing→Executing→Done/Failed）
- **B5 IM 对话扩展**：IMAdapter 桥接飞书/钉钉/Slack → ConversationEngine，审批卡片交互
- **B6 Web UI 对话框**：WebSocket Hub 实时对话通道，WSRequest/WSResponse JSON 协议
- **B7 CLI 对话命令**：`levee converse` 单次+交互模式，支持 /help /state /history /sessions /new

#### 新增文件
- `internal/recommend/` — AI 建议引擎（11 files: types, knowledge_base, defaults, llm, sanitizer, engine, workflow_gen + tests）
- `internal/conversation/` — 对话引擎（7 files: session, engine, im_adapter, web_hub + tests）
- `cmd/levee/cmd_converse.go` — CLI 对话命令

#### 测试覆盖率
- `internal/recommend/`: 91.8%
- `internal/conversation/`: 94.9%
- `cmd/levee/cmd_converse.go`: 98.88%

#### 依赖
- 无新增外部依赖

## v1.6.0 - 2026-08-16

### Phase A: 智能运维闭环引擎 - 告警接入 + 基础诊断

#### 新增功能
- **告警网关** (internal/alert/): 统一告警模型 + HTTP 网关 + Prometheus Alertmanager 适配器 + 自研平台适配器 + 去重/聚合/抑制
- **日志采集器** (internal/diagnosis/log_collector.go): SSH/Agent 远程日志拉取 + 多源并发采集 + syslog/journald/eventlog/app 四类源
- **日志分析器** (internal/diagnosis/log_analyzer.go): 8 种内置错误模式匹配 + 错误聚类 + 根因定位 + 置信度评分
- **健康探针** (internal/diagnosis/health_probe.go): 网络(ping/DNS/TCP) + 节点(CPU/内存/磁盘/负载) + 服务(进程/端口/HTTP) + 数据(DB/复制延迟) 四类探针
- **诊断引擎** (internal/diagnosis/engine.go): 并发执行日志分析+健康探针 + 综合诊断报告 + 告警触发诊断 + 多目标诊断
- **CLI 命令**: `levee alert serve/list/show/silence` + `levee diagnose <target>`

#### 技术指标
- 新增代码: ~5,500 行 Go 代码 + ~2,000 行测试代码
- 测试覆盖率: alert 90.8%, diagnosis 95.4%
- 新增包: internal/alert, internal/diagnosis (扩展)
- 新增 CLI 命令组: alert, diagnose

> **说明**：不存在 v1.5.0 —— 版本号从 v1.4.0 直接跳到 v1.6.0（该号被跳过、未发布）。

## [1.4.0] - 2026-08-16 — Phase 3: 生态扩展

### Added

- **F04 分布式执行**: Agent 常驻进程 + 心跳 + 注册/注销 + 任务执行 + 结果回传 + 多节点调度器（任务分片 + 负载均衡）+ `levee agent` CLI
- **F07 多租户**: 租户隔离（行级数据隔离 + 命名空间）+ 资源配额（目标机数/并发数/存储空间）+ 租户管理 CLI + 审计隔离
- **F10 配置漂移检测增强**: 定期巡检调度（cron）+ 漂移基线自动生成 + 漂移告警 + 趋势报告 + `levee drift` CLI
- **F12 移动端审批**: APNs/FCM 推送通知 + 深度链接 + 一键审批/驳回 + 响应式 Web UI 适配 + `levee push` CLI

## [1.3.0] - 2026-08-16 — Phase 2: 平台化

### Added

- **F01 Web UI**: Vue 3 + Vite + TypeScript 前端，7 个核心页面（变更看板/审批/监控/模板/目标/审计/系统），gRPC-Web API 客户端，go:embed 嵌入静态文件，`levee web` CLI 命令
- **F03 插件系统**: 四类插件接口（Channel/Gate/Module/Notifier），子进程沙箱（资源限制+崩溃恢复），SQLite 注册表，`levee plugin` CLI 命令，HTTP 探针示例插件
- **F06 RBAC 增强**: 角色继承树，细粒度权限策略（Resource×Action×Condition），ABAC 基于标签的访问控制，权限缓存（TTL 5min），`levee rbac` CLI 命令
- **F09 ChatOps**: 飞书/钉钉/Slack 机器人适配层，交互卡片消息，一键审批/驳回，变更通知推送，命令路由，`levee chatops` CLI 命令

## [1.2.0] - 2026-08-16 — Phase 1: 核心增强

### Added

- **F05 外部 KMS 集成**: HashiCorp Vault Provider（AppRole+KV v2+租约管理）+ AWS KMS Provider（信封加密）+ 降级策略 + `levee kms` CLI
- **F08 变更日历**: 冻结期 + 冲突检测（倒排索引）+ cron 重复规则（5 字段 POSIX）+ `levee calendar` CLI（6 个子命令）
- **F13 LEVEELang 类型检查**: 8 种/种基础类型 + 别名 + 枚举 + 类型注册表 + IR 生成 + `levee compile` 命令
- **F14 SLO 门禁三阶段时序**: PreApplySLOGate（基线检查）+ GracePeriodGate（延迟回归检测）+ PhaseGracePeriod

## [1.0.0] - 2026-08-16

### Added

#### 通道层

- SSH 通道实现（golang.org/x/crypto/ssh），支持密码/密钥认证 + 文件传输（scp）
- SSH 连接池 + 多路复用（ControlMaster 等价），单目标机并发上限可配
- WinRM 通道最小子集（masterzen/winrm），Negotiate 认证 + 命令执行
- WinRM 连接池，单连接单命令策略，并发上限可配
- 通道限速与背压：全局并发上限 + 单通道 + 单目标机三级限速，背压排队 + 超时
- 目标可达性预检：apply 前对每台目标机 noop 探测，失败剔除并产出预检报告

#### 工作流核心

- LEVEELang YAML 子集解析器：解析 input / target / window / batches / step / rollback / approval，产出 AST
- LEVEELang 基础校验：必填字段校验 + 类型基础校验 + 批次声明合法性
- Plan 生成器：目标解析 + 批次划分 + 步骤编排，产出执行计划结构体
- 影响面分析：直接受影响目标集 + 间接影响标注，产出影响面报告
- Plan 哈希锁定：canonical 化 + sha256 计算，plan_hash = hash(workflow + 目标集 + 参数 + 批次 + 影响面)
- 批次控制器：分批执行 + 批间串行 + 批内并发，批次边界显式，批间等待可配
- 批次并发限速：批内并发受通道限速约束，超限排队
- 验证门禁框架：GateManager 接口 + 门禁注册，pre_apply / post_batch / post_apply 三时机
- 命令门禁：在目标机执行检查命令，期望 exit_code / stdout 匹配，重试 + 超时
- SLO 门禁 post_batch：查询 Prometheus 指标，阈值比对，重试 + 超时

#### 变更闭环

- 审批服务框架：ApprovalService 接口 + 状态机（待审批 / 通过 / 驳回 / 超时）
- 审批三级分级：标准 / 高危 / 紧急，触发条件 + 审批人要求 + 超时配置
- 审批模板库：高危规则模板（删库 / 主从切换 / 防火墙全量），可配置
- 不可逆操作标记：irreversible: true 声明 + 白名单校验 + 强制升高审批级别
- 回滚协议框架：RollbackManager 接口 + 白名单校验 + 按批逆序调度
- 快照管理：apply 前快照创建（文件 / 配置备份）+ 快照存储 + 快照恢复
- 回滚后验证：回滚完成后强制跑 verify，失败按回滚失败处理
- 回滚失败分级：成功 / 部分回滚 / 回滚失败三档，对应通知 + 升级动作
- 失败语义五档模型：retryable / manual_retry / rollback / escalate / fatal
- 互斥锁 + TTL：目标机级锁 + TTL 默认 1h + 锁过期抢占 + 抢占前状态检查
- 全局暂停 / 恢复：pause-all / resume-all + 单 run pause / resume，留痕 + 权限校验

#### 审计安全

- 审计 trace 记录：每个动作记录输入 / 输出 / 耗时 / 目标机上下文
- 哈希链构建：每个动作 hash 包含前一动作 hash，链式结构，分批分片
- WORM 存储模拟：SQLite 模拟 WORM（追加只写 + 校验和），不可篡改
- 哈希链校验：任意 run 的 trace 可独立校验，篡改可检出并报错
- 凭据本地 AES-GCM 加密存储：argon2 密钥派生，凭据不落盘明文 / 不进 trace / 不进日志
- 凭据按需获取：apply 时按目标机获取凭据，用完即弃
- 权限 v0 框架：团队 x 环境二维权限矩阵，配置文件定义
- 权限校验集成：plan / apply / approval / rollback 操作前权限校验
- 通知框架：Notifier 接口 + 触发点注册，对象分级（发起人 / 审批人 / oncall / 订阅人）
- Webhook 通知渠道：webhook 发送 + 签名校验 + 重试
- 回滚通知独立：回滚触发 / 结果独立通知，不与 apply 合并

#### 兼容体验

- Playbook 兼容层框架：CompatLayer 接口 + playbook 导入解析，独立模块不引入核心依赖（R8）
- Playbook 最小子集执行：支持 shell / command / file / copy / template 模块，包审批 / 门禁 / 审计
- 兼容层风险评估：静态分析 shell / command 非幂等 + ignore_errors + 无 rollback，命中标记高危
- 裸 shell 直跑：`levee run --shell "cmd"` 单命令直跑，不走 workflow
- Dry-run 预览：`levee plan --dry-run` 产出目标集 / 批次 / 影响面 / 预估耗时 / 潜在冲突
- 变更克隆：`levee clone <run-id>` 生成可编辑副本，保留原参数与批次结构
- 模板库管理：模板存储 + 列表 + show，模板带参数占位
- 模板实例化：`levee new <template> --params key=val,...` 参数填充 + 完整性校验

#### CLI 命令全集

- 变更管理：new / clone / show / list / diff
- 审批控制：approve / reject
- 执行控制：apply / pause / resume / pause-all / resume-all / cancel / retry / retry-host / rollback
- 观察性：logs / trace / archive / link
- 模板管理：template list / show / create / delete
- 目标管理：target list / import / check
- 审计管理：audit verify / export / list / show
- 凭据管理：secret list / add / rotate / revoke / show
- 用户与团队：user list / add / team list / add
- 系统管理：system version / status / config get / config set / doctor / version

#### 发布工具

- GoReleaser 跨平台打包：linux amd64/arm64 + windows amd64，含 checksum
- 10 分钟测试门禁：单元测试 + 集成测试 + Lint，10 分钟内完成
- 发布门禁检查：G-01 至 G-07 全部通过

#### 内置模块

- shell 模块：shell / command 动作，幂等契约声明
- file 模块：copy / template 动作，文件分发 + 模板渲染 + 校验
- pkg 模块：install / remove / upgrade 动作，包管理器抽象（apt/yum/dnf）
- svc 模块：start / stop / restart / enable / disable / reload 动作，服务管理器抽象（systemd/sysvinit）
- user 模块：add / remove / modify 动作，用户/组管理 + SSH 公钥分发

### Security

- 凭据 AES-GCM 加密存储，argon2 密钥派生，凭据不落盘明文 / 不进 trace / 不进日志
- WORM 审计存储，追加只写 + 校验和，不可篡改
- 哈希链校验，任意 run 的 trace 可独立校验，篡改可检出
- 三级审批模型，高危变更强制人工审批，审批人不能是发起人
- 不可逆操作白名单 + 强制升高审批级别
- 团队 x 环境二维权限矩阵，操作前权限校验
- [SA-001] WORM 存储硬编码 SQLite 触发器，阻止 trace 表内容字段 UPDATE/DELETE，创建 WORMStore 接口限制审计层只读访问
- [SA-002] 哈希链 Build 前先 Verify 链完整性，拒绝重建已存在链（ErrChainAlreadyBuilt/ErrChainBroken），新增 BuildForce 用于管理恢复
- [SA-003] 实现 RotateMasterPassword 三阶段原子轮换（旧密码解密→新密码加密→更新内存），所有错误路径 SecureZero 清理
