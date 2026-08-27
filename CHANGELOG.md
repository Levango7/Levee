# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 安全修复

- **REST 方法校验（P1）**：`/changes/{id}/{plan,apply,approve,reject,pause,resume,cancel,retry,rollback,archive}` 现强制 `POST`、`/changes/{id}/{logs,trace}` 强制 `GET`；此前任意 HTTP 方法（含 GET）即可触发状态变更，爬虫/预取可误暂停变更。`pause/resume/archive` 不再吞掉请求体解析错误：空体合法、畸形 JSON 返回 400。
- **SSH BecomeUser 注入（P2）**：`buildExecCommand` 现对 `become_user` 也做 POSIX shell 引用（此前仅引用命令本体），阻断来自配置值的 `sudo -u` 注入面。
- **/metrics 默认鉴权（P2）**：网关 `SetExtraRoute` 挂载的运维端点（`/metrics`）在配置了任一 token 时默认要求 Bearer 鉴权；新增 `--metrics-public` / `ServeGatewayConfig.MetricsPublic` 显式放开（供无法携带凭据的采集器）。

### 新增

- **多令牌身份认证**：`serve` 新增可重复 `--auth-token name=secret`，每个命名令牌映射到一个主体（subject）；gRPC 拦截器与 REST 中间件均支持 `AuthTokens`（`Legacy` + `Named`）。命名令牌认证后，其主体注入请求上下文并**优先于**客户端自报的 `X-Acting-As`，使审计归属为“被证明的身份”而非“断言”。单令牌（`--token`/`LEVEE_TOKEN`）行为完全向后兼容。
- **serve 装配 AI 引擎**：`levee serve` 现装配真实的诊断引擎（日志管线 + 健康探针，本地执行器）与对话引擎（内置推荐引擎），`Diagnose`/`SendMessage` RPC 不再返回 `Unimplemented`；告警服务保持独立环形存储（完整网关仍用 `levee alert serve`）。
- **前端 CI 门禁**：新增 `frontend` 作业（`vue-tsc --noEmit` + `vite build`），并纳入 `check` 聚合门禁。

### 变更

- **Trivy 阻断合并**：`trivy` 作业对可修复的 CRITICAL/HIGH CVE 设 `exit-code: 1`，由“仅上报”改为“阻断”。
- **集群模式如实标注**：`serve --cluster` 启动时输出告警，说明当前集群协同仅限共享 PostgreSQL 存储（数据一致性 + 咨询锁），节点注册为进程内、尚无自动故障转移/跨节点调度；README 特性描述同步收敛。
- **前端产物清理**：`internal/web/dist` 重新构建，移除历史遗留的多代哈希资产，仅保留当前一代。

### 文档

- README 版本号由过时的 v1.10.0 更正为 v1.12.0。
- 补充 v1.5.0 跳号说明（该号未发布）。
- 修正 `.gitignore` 中 `.env"coverfunc.txt"` 的粘连/引号错误。
- 新增生产部署与升级手册（docs/deployment.md）。
- 补全 CLI 参考缺失命令（alert/backup/restore/group/converse/diagnose 等）。

### 修复

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