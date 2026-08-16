# LEVEE × OpsMesh 智能运维闭环引擎 — 设计方案

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE × OpsMesh 智能运维闭环引擎设计 |
| 文档类型 | 方案设计（评审稿） |
| 版本 | v0.1 |
| 日期 | 2026-08-16 |
| 定位 | LEVEE 从变更流水线引擎进化为智能运维闭环引擎 |
| 与 OpsMesh 关系 | LEVEE 是 OpsMesh 的执行引擎，OpsMesh 做监控+告警，LEVEE 做诊断+修复+审计 |

---

## 第1章 愿景

### 1.1 一句话定位

**LEVEE 接收 OpsMesh 推送的告警，自动诊断根因，AI 生成修复建议，用户通过聊天审核，LEVEE 自动拆分任务并执行修复，事后审计留痕。**

### 1.2 核心场景流

```
1. OpsMesh 检测到异常 → 推送告警到 LEVEE
2. LEVEE 告警网关接收 → 归一化 → 去重/聚合/抑制
3. LEVEE 诊断引擎启动：
   a. 日志采集：从告警关联目标机自动拉取相关时间窗口日志
   b. 日志分析：异常模式提取、错误聚类、根因定位
   c. 健康检测：网络连通性 / 节点资源 / 服务状态 / 数据一致性
   d. 拓扑分析：从 SkyWalking/Pinpoint 获取调用链，分析影响半径
4. LEVEE AI 建议引擎：
   a. 知识库匹配：历史故障库 + runbook 匹配
   b. LLM 对话诊断：大模型综合诊断结果，生成修复建议
   c. 修复方案生成：输出 LEVEELang workflow 草稿
5. LEVEE 对话引擎：
   a. 推送诊断摘要 + 修复建议到 IM 机器人 / Web UI
   b. 用户自然语言交互："这个修复安全吗？" "先只重启一个节点试试"
   c. 用户审核确认（全自动模式下，低危自动执行，高危需确认）
6. LEVEE 自动任务拆分：
   a. 修复方案 → LEVEELang workflow 正式化
   b. 影响面分析 + 批次划分
   c. 风险评估 + 审批级别判定
7. LEVEE 现有闭环执行：
   a. 计划 → 审批 → 分批执行 → 验证门禁 → 回滚 → 审计
   b. 全自动模式：低危直接执行，高危聊天确认后执行
8. 结果回传 OpsMesh + 事后审计报告
9. 用户聊天复盘："这次修复做了什么？" "下次能更快吗？"
```

### 1.3 与现有 LEVEE 的关系

| 现有能力 | 在新流程中的角色 |
|---------|---------------|
| LEVEELang DSL | 修复方案的表达格式 |
| Plan Generator | 自动拆分任务的基础 |
| Batch Controller | 修复执行的分批控制 |
| Execute Modules | 修复动作的执行器（shell/file/pkg/svc/user） |
| Verify Gates | 修复后的验证门禁 |
| Rollback | 修复失败的回滚 |
| Audit (WORM + HashChain) | 事后审计的信任基础 |
| Approval (三级审批) | 聊天审核的审批流 |
| ChatOps (F09) | IM 机器人交互基础 |
| Web UI (F01) | Web 对话框基础 |
| Agent (F04) | 远程日志采集和健康探针 |
| Drift Detection (F10) | 配置漂移作为诊断信号 |
| gRPC API (F02) | OpsMesh 集成接口 |

---

## 第2章 新增模块设计

### 2.1 告警网关 (internal/alert/)

**职责**：接收多源告警，归一化为统一格式，去重/聚合/抑制。

**告警适配器**：

| 适配器 | 协议 | 优先级 |
|--------|------|--------|
| Prometheus Alertmanager | HTTP webhook (JSON) | P0 |
| 自研监控平台 | 自定义 webhook | P0 |
| SkyWalking | HTTP webhook + gRPC | P1 |
| Zabbix | Webhook (XML/JSON) | P1 |
| Nagios | NSCA / HTTP | P2 |

**核心结构**：

```go
// internal/alert/alert.go
package alert

// Alert 统一告警结构体
type Alert struct {
    ID          string            // 告警唯一 ID
    Source      string            // 来源 (prometheus/skywalking/zabbix/...)
    Severity    Severity          // critical/warning/info
    Title       string            // 告警标题
    Description string            // 告警描述
    Labels      map[string]string // 标签 (service/host/env/...)
    Fingerprint string            // 指纹（用于去重）
    StartsAt    time.Time         // 告警开始时间
    EndsAt      *time.Time        // 告警结束时间
    Status      AlertStatus       // firing/resolved
    RawPayload  json.RawMessage   // 原始 payload
}

// AlertGateway 告警网关
type AlertGateway struct {
    adapters  map[string]Adapter  // 告警适配器
    deduper   *Deduper             // 去重器
    aggregator *Aggregator         // 聚合器
    silencer  *Silencer            // 抑制器
    handler   AlertHandler         // 告警处理器（→ 诊断引擎）
}

// Adapter 告警适配器接口
type Adapter interface {
    Name() string
    Parse(raw []byte) ([]*Alert, error)
    Validate(raw []byte) error
}
```

**告警处理流水线**：
```
接收 → 解析 → 归一化 → 去重 → 聚合 → 抑制 → 路由到诊断引擎
```

**CLI 命令**：
```bash
levee alert serve --port 9090          # 启动告警接收服务
levee alert list --status firing       # 查看活跃告警
levee alert show <alert-id>            # 查看告警详情
levee alert silence --match "host=web-01" --duration 30m  # 抑制告警
levee alert history --days 7           # 告警历史
```

---

### 2.2 诊断引擎 (internal/diagnosis/)

**职责**：基于告警自动采集证据，分析根因，输出诊断报告。

**诊断子模块**：

#### 2.2.1 日志采集器 (log_collector.go)
```go
// 从告警关联的目标机自动拉取时间窗口内的日志
// 复用 internal/channel/ssh + internal/agent

type LogCollector struct {
    channel channel.Channel   // SSH/WinRM 通道
    agent   *agent.Agent      // 或通过 Agent 远程采集
}

// Collect 从目标机采集日志
// 时间窗口：告警前 30min → 告警后 5min
// 日志源：/var/log/syslog, /var/log/app/*.log, journald, Windows Event Log
func (c *LogCollector) Collect(ctx context.Context, target string, timeWindow TimeWindow) (*LogBatch, error)
```

#### 2.2.2 日志分析器 (log_analyzer.go)
```go
// 异常模式提取 + 错误聚类 + 根因定位

type LogAnalyzer struct {
    patterns  []ErrorPattern    // 已知错误模式库
    llmClient LLMClient         // LLM 辅助分析（可选）
}

type DiagnosisResult struct {
    RootCause     string         // 根因描述
    Confidence    float64        // 置信度 0-1
    Evidence      []Evidence     // 证据链
    AffectedComps []string       // 受影响组件
    Timeline      []TimelineItem // 事件时间线
}

func (a *LogAnalyzer) Analyze(logs *LogBatch) (*DiagnosisResult, error)
```

#### 2.2.3 健康探针 (health_probe.go)
```go
// 网络/节点/服务/数据健康检测

type HealthProber struct {
    agent *agent.Agent
}

// ProbeNetwork 网络连通性检测（ping/tcp_traceroute/dns_resolve）
// ProbeNode 节点资源检测（CPU/内存/磁盘/负载）
// ProbeService 服务状态检测（端口/进程/HTTP健康端点）
// ProbeData 数据一致性检测（DB连接/主从同步/数据校验）
func (p *HealthProber) ProbeAll(ctx context.Context, target string) (*HealthReport, error)
```

#### 2.2.4 拓扑分析器 (topology.go)
```go
// 从 SkyWalking/Pinpoint 获取服务调用链，分析影响半径

type TopologyAnalyzer struct {
    apmClient APMClient   // SkyWalking/Pinpoint API 客户端
}

// AnalyzeImpact 分析告警服务的影响半径
// 返回：上游依赖（谁调用了我）+ 下游依赖（我调用了谁）+ 影响路径
func (t *TopologyAnalyzer) AnalyzeImpact(service string, alert *alert.Alert) (*ImpactAnalysis, error)
```

**诊断引擎入口**：
```go
// internal/diagnosis/engine.go

type DiagnosisEngine struct {
    logCollector  *LogCollector
    logAnalyzer   *LogAnalyzer
    healthProber  *HealthProber
    topology      *TopologyAnalyzer
}

// Diagnose 接收告警，执行完整诊断流程，输出诊断报告
func (e *DiagnosisEngine) Diagnose(ctx context.Context, alert *alert.Alert) (*DiagnosisReport, error)
```

---

### 2.3 AI 建议引擎 (internal/recommend/)

**职责**：基于诊断结果，匹配知识库，LLM 生成修复建议，输出 LEVEELang workflow 草稿。

```go
// internal/recommend/engine.go

type RecommendEngine struct {
    knowledgeBase *KnowledgeBase    // 历史故障库 + runbook
    llmClient     LLMClient         // 大模型客户端
    workflowGen   *WorkflowGenerator // LEVEELang 生成器
}

// Recommend 输入诊断报告，输出修复建议
type Recommendation struct {
    ID            string
    DiagnosisID   string
    Summary       string              // 修复摘要
    Approach      string              // 修复方案描述
    WorkflowDraft *LEVEELangDraft     // LEVEELang workflow 草稿
    RiskLevel     RiskLevel           // 风险等级
    Confidence    float64             // 置信度
    Alternatives  []Alternative       // 备选方案
    PreConditions []string            // 前置条件
    RollbackPlan  string              // 回滚计划
}

func (e *RecommendEngine) Recommend(ctx context.Context, report *DiagnosisReport) (*Recommendation, error)
```

**知识库**：
```go
// internal/recommend/knowledge_base.go

type KnowledgeBase struct {
    incidents  []HistoricalIncident  // 历史故障库
    runbooks   []Runbook             // 运维手册
    patterns   []FixPattern          // 修复模式库
}

// Match 匹配历史故障和 runbook
func (kb *KnowledgeBase) Match(diagnosis *DiagnosisReport) ([]*Match, error)
```

**LLM 集成**：
```go
// internal/recommend/llm.go

type LLMClient interface {
    // Diagnose 对话式诊断
    Diagnose(ctx context.Context, prompt string, history []Message) (string, error)
    // GenerateFix 生成修复方案
    GenerateFix(ctx context.Context, diagnosis *DiagnosisReport, matches []*Match) (*FixProposal, error)
    // GenerateWorkflow 生成 LEVEELang workflow
    GenerateWorkflow(ctx context.Context, proposal *FixProposal) (*LEVEELangDraft, error)
}

// 支持：OpenAI GPT-4 / Claude / 本地模型 (Ollama)
```

---

### 2.4 对话引擎 (internal/conversation/)

**职责**：管理多轮对话上下文，IM + Web UI 双入口统一状态，审核流。

```go
// internal/conversation/engine.go

type ConversationEngine struct {
    sessions   map[string]*Session    // 对话会话
    chatops    *chatops.Bot           // 复用 F09 ChatOps
    webHub     *WebHub                // Web UI WebSocket
    recommend  *recommend.RecommendEngine
    autoplanner *autoplanner.AutoPlanner
    engine     *engine.Engine         // LEVEE 执行引擎
}

// Session 对话会话
type Session struct {
    ID          string
    UserID      string
    AlertID     string                 // 关联告警
    DiagnosisID string                 // 关联诊断
    Recommendation *recommend.Recommendation  // 当前建议
    Messages    []Message              // 对话历史
    State       SessionState           // diagnosing/recommending/reviewing/executing/done
    WorkflowID  string                 // 执行中的 workflow ID
}

// HandleMessage 处理用户消息（自然语言）
func (e *ConversationEngine) HandleMessage(ctx context.Context, sessionID, userID, text string) (*Reply, error)

// Reply 回复（包含文本 + 交互卡片）
type Reply struct {
    Text   string         // 文本回复
    Card   *chatops.Card  // 交互卡片（审批按钮等）
    Action Action         // 关联动作（approve/reject/modify/execute）
}
```

**对话流示例**：
```
[系统] 🔴 告警：web-01 CPU 使用率 > 95%，持续 5 分钟
[系统] 📋 诊断完成：
       根因：Java 进程内存泄漏导致频繁 GC
       影响：web-01 上 3 个服务受影响，下游 api-gateway 延迟升高
       置信度：87%
[系统] 💡 修复建议：
       方案 A（推荐）：重启 Java 服务，释放内存
       方案 B：扩容到 2 个实例，分流负载
       风险等级：中（方案 A）/ 低（方案 B）
[用户] 方案 A 会不会丢请求？
[系统] 根据日志分析，当前 QPS 约 200/s，重启期间预计丢失 ~400 请求。
       建议先摘流再重启。我可以自动执行：
       1. 从负载均衡摘除 web-01
       2. 等待 30s 排空在途请求
       3. 重启 Java 服务
       4. 健康检查通过后重新加回负载均衡
       要执行吗？
[用户] 执行吧
[系统] ✅ 已自动拆分为 4 步 workflow，开始执行...
[系统] ✅ 步骤 1/4：摘流完成
[系统] ✅ 步骤 2/4：排空完成（实际等待 28s）
[系统] ✅ 步骤 3/4：Java 服务已重启
[系统] ✅ 步骤 4/4：健康检查通过，已加回负载均衡
[系统] 📊 修复完成，耗时 1m32s。事后审计报告已生成。
       CPU 使用率降至 23%，api-gateway 延迟恢复正常。
```

---

### 2.5 自动任务拆分 (internal/autoplanner/)

**职责**：将修复方案自动转换为 LEVEELang workflow，影响面分析，批次划分，风险评估。

```go
// internal/autoplanner/planner.go

type AutoPlanner struct {
    planGen    *plan.Generator        // 复用现有计划生成器
    impactAna  *plan.ImpactAnalyzer   // 复用现有影响面分析
    riskAssess *RiskAssessor          // 风险评估器
}

// Plan 输入修复建议，输出可执行的 LEVEE workflow
func (p *AutoPlanner) Plan(ctx context.Context, rec *recommend.Recommendation) (*Workflow, error)

// Workflow 输出的 workflow（LEVEELang 格式）
type Workflow struct {
    ID       string
    Name     string
    YAML     string              // LEVEELang YAML
    Batches  []Batch             // 批次划分
    RiskLevel RiskLevel          // 风险等级
    ApprovalLevel approval.Level // 审批级别
    EstimatedTime time.Duration  // 预估耗时
}
```

---

### 2.6 OpsMesh 集成接口 (internal/opsmesh/)

**职责**：与 OpsMesh 的双向集成 API。

```go
// internal/opsmesh/client.go

type OpsMeshClient struct {
    baseURL    string
    apiKey     string
    httpClient *http.Client
}

// ReportResult 回传修复结果给 OpsMesh
func (c *OpsMeshClient) ReportResult(ctx context.Context, alertID string, result *FixResult) error

// GetTopology 从 OpsMesh 获取服务拓扑
func (c *OpsMeshClient) GetTopology(ctx context.Context, service string) (*Topology, error)

// GetMetrics 从 OpsMesh 获取监控指标
func (c *OpsMeshClient) GetMetrics(ctx context.Context, query string, timeRange TimeRange) (*Metrics, error)
```

**gRPC 扩展**：在现有 proto/levee.proto 中新增 AlertService：

```protobuf
service AlertService {
  // 接收告警
  rpc ReceiveAlert(Alert) returns (AlertResponse);
  // 查询告警状态
  rpc GetAlertStatus(GetAlertStatusRequest) returns (AlertStatus);
  // 订阅告警流
  rpc SubscribeAlerts(SubscribeRequest) returns (stream Alert);
}

service DiagnosisService {
  // 触发诊断
  rpc Diagnose(DiagnoseRequest) returns (DiagnosisReport);
  // 获取诊断结果
  rpc GetDiagnosis(GetDiagnosisRequest) returns (DiagnosisReport);
}

service ConversationService {
  // 发送消息
  rpc SendMessage(SendMessageRequest) returns (Reply);
  // 订阅对话流
  rpc SubscribeConversation(SubscribeRequest) returns (stream Reply);
}
```

---

## 第3章 模块依赖关系

```
告警 ──→ 诊断 ──→ 建议 ──→ 对话 ──→ 自动拆分 ──→ 闭环执行 ──→ 结果回传
 │         │         │         │          │           │          │
 │         │         │         │          │           │          ▼
 │         │         │         │          │           │      OpsMesh
 │         │         │         │          │           │      Client
 │         │         │         │          │           ▼
 │         │         │         │          │      LEVEE Engine
 │         │         │         │          │     (现有闭环)
 │         │         │         │          ▼
 │         │         │         │      AutoPlanner
 │         │         │         │     (→ LEVEELang)
 │         │         │         ▼
 │         │         │    Conversation
 │         │         │    Engine
 │         │         ▼
 │         │    Recommend
 │         │    Engine
 │         │   (知识库+LLM)
 │         ▼
 │      Diagnosis
 │      Engine
 │     (日志/拓扑/健康)
 ▼
 Alert
 Gateway
(多源适配)
```

---

## 第4章 文件清单

### 4.1 新增包

| 包路径 | 职责 | 预估文件数 |
|--------|------|-----------|
| `internal/alert/` | 告警网关 + 多源适配器 | 8 |
| `internal/diagnosis/` | 诊断引擎（日志/健康/拓扑） | 10 |
| `internal/recommend/` | AI 建议引擎 + 知识库 + LLM | 8 |
| `internal/conversation/` | 对话引擎 + 会话管理 | 6 |
| `internal/autoplanner/` | 自动任务拆分 | 4 |
| `internal/opsmesh/` | OpsMesh 集成客户端 | 4 |
| `cmd/levee/cmd_alert.go` | 告警 CLI | 1 |
| `cmd/levee/cmd_diagnose.go` | 诊断 CLI | 1 |
| `cmd/levee/cmd_converse.go` | 对话 CLI | 1 |

### 4.2 修改文件

| 文件 | 修改内容 |
|------|---------|
| `proto/levee.proto` | 新增 AlertService / DiagnosisService / ConversationService |
| `cmd/levee/cmd_serve.go` | 启动告警网关 + 对话引擎 |
| `cmd/levee/root.go` | 注册新命令 |
| `internal/chatops/bot.go` | 扩展为对话引擎的 IM 入口 |

### 4.3 新增依赖

| 依赖 | 用途 |
|------|------|
| `github.com/sashabaranov/go-openai` | OpenAI GPT-4 LLM 客户端 |
| `github.com/tmc/langchaingo` | LangChain Go 版（可选，用于 RAG） |

---

## 第5章 实现优先级

### Phase A (P0) — 告警接入 + 基础诊断

| 特性 | 内容 |
|------|------|
| A1 | 告警网关 + Prometheus/自研 webhook 适配器 |
| A2 | 告警去重/聚合/抑制 |
| A3 | 日志采集器（SSH/Agent 远程拉取） |
| A4 | 基础日志分析（正则模式匹配 + 错误聚类） |
| A5 | 健康探针（网络/节点/服务/数据） |
| A6 | 诊断报告生成 |

### Phase B (P0) — AI 建议 + 对话

| 特性 | 内容 |
|------|------|
| B1 | 知识库框架 + 历史故障匹配 |
| B2 | LLM 集成（OpenAI/本地模型） |
| B3 | 修复方案生成（→ LEVEELang 草稿） |
| B4 | 对话引擎 + 会话管理 |
| B5 | IM 机器人对话扩展（复用 ChatOps） |
| B6 | Web UI 对话框 |

### Phase C (P1) — 自动执行 + OpsMesh 集成

| 特性 | 内容 |
|------|------|
| C1 | 自动任务拆分（→ LEVEELang 正式化） |
| C2 | 全自动执行模式（低危自动/高危确认） |
| C3 | 事后审计报告生成 |
| C4 | OpsMesh 集成客户端（结果回传 + 拓扑获取） |
| C5 | gRPC AlertService / DiagnosisService / ConversationService |

### Phase D (P1) — 高级诊断

| 特性 | 内容 |
|------|------|
| D1 | SkyWalking/Pinpoint 拓扑分析 |
| D2 | Zabbix/Nagios 告警适配器 |
| D3 | LLM 对话式诊断（多轮推理） |
| D4 | RAG 知识库增强（向量检索 + LLM） |
| D5 | 修复效果学习（反馈循环，优化知识库） |

---

## 第6章 与 OpsMesh 集成协议

### 6.1 OpsMesh → LEVEE（告警推送）

```http
POST /api/v1/alerts
Content-Type: application/json
Authorization: Bearer <token>

{
  "source": "opsmesh",
  "severity": "critical",
  "title": "web-01 CPU > 95%",
  "description": "...",
  "labels": { "host": "web-01", "service": "order-service", "env": "prod" },
  "fingerprint": "sha256(...)",
  "starts_at": "2026-08-16T12:00:00Z"
}
```

### 6.2 LEVEE → OpsMesh（结果回传）

```http
POST /opsmesh/api/v1/alerts/{alert_id}/resolution
Content-Type: application/json
Authorization: Bearer <token>

{
  "status": "resolved",
  "resolution": "auto-fixed",
  "workflow_id": "run-abc123",
  "summary": "重启 Java 服务，CPU 降至 23%",
  "audit_report_url": "https://levee/audit/run-abc123",
  "duration_seconds": 92
}
```

### 6.3 LEVEE → OpsMesh（拓扑查询）

```http
GET /opsmesh/api/v1/topology?service=order-service
Authorization: Bearer <token>
```

---

## 第7章 安全与合规

1. **告警注入防护**：所有告警 payload 严格校验 + 大小限制
2. **LLM 安全**：敏感信息（IP/密码/密钥）在发送给 LLM 前脱敏
3. **自动执行边界**：全自动模式仅限低危修复，高危必须人工确认
4. **审计留痕**：所有 AI 决策 + 用户交互 + 自动执行全部写入 WORM 审计
5. **回滚保障**：自动修复失败自动回滚，回滚也失败则升级告警
6. **权限控制**：对话引擎集成 RBAC，只有授权用户可以审核和执行修复

---

## 第8章 验收标准

1. OpsMesh 推送告警 → LEVEE 5 秒内开始诊断
2. 诊断报告在 30 秒内生成（含日志分析 + 健康检测）
3. AI 修复建议在 10 秒内生成
4. 用户通过 IM 聊天确认 → 修复 workflow 在 5 秒内开始执行
5. 全自动模式下，低危修复从告警到修复完成 < 2 分钟
6. 所有修复操作有完整审计链
7. 修复结果自动回传 OpsMesh