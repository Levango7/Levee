# LEVEE MVP+1 路线图

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE MVP+1 路线图 |
| 文档类型 | 产品路线图文档 |
| 版本 | v1.0 |
| 日期 | 2026-08-16 |
| 上游依据 | levee-design.md 第 8 章、mvp-tasks.md MVP 交付清单 |
| 适用范围 | MVP 完成后下一阶段特性规划（Q3 2026 - Q1 2027） |
| 评审状态 | 待评审 |

---

## 第1章 愿景

MVP 阶段已交付"单二进制零依赖的变更闭环引擎"，覆盖从计划到归档的完整生命周期最小可用集。MVP+1 的核心目标是：**从"能用"到"好用"，从"单机工具"到"平台服务"**。

三个方向性目标：

1. **可视化与协作**：CLI 优先不变，但 Web UI 让审批人、管理层、跨团队协作方无需 CLI 即可参与变更治理，降低使用门槛，扩大用户覆盖面。
2. **开放接口与可扩展性**：gRPC API 让 LEVEE 成为可编程的平台服务，插件系统让社区和企业可以自行扩展通道、门禁、模块，不再等待上游。
3. **安全与合规增强**：外部 KMS 集成解决凭据治理合规痛点，RBAC 增强满足企业级权限管控需求，变更日历解决变更窗口管理的真实运营痛点。

MVP+1 不追求功能堆叠，每个特性必须回答：**它解决了 MVP 用户反馈的哪个真实痛点？**

---

## 第2章 特性评估矩阵

### 2.1 评估维度说明

- **用户价值**：解决真实痛点的强度。高 = 核心客户强烈诉求且 MVP 无替代方案；中 = 改善体验但 MVP 有变通方案；低 = 锦上添花。
- **技术复杂度**：实现工作量与架构侵入性。高 = 需引入新框架或重构核心路径；中 = 新增独立模块但依赖现有接口；低 = 增量开发。
- **依赖关系**：该特性是否阻塞其他特性，或被其他特性阻塞。
- **建议优先级**：P0 = Phase 1 必做；P1 = Phase 2 应做；P2 = Phase 3 可做；P3 = 延后观察。

### 2.2 特性评估矩阵

表：MVP+1 特性评估矩阵对照表

| 编号 | 特性 | 用户价值 | 技术复杂度 | 依赖关系 | 建议优先级 |
| --- | --- | --- | --- | --- | --- |
| F01 | Web UI | 高 | 高 | 依赖 F02（gRPC API）提供数据层 | P0 |
| F02 | gRPC API | 高 | 中 | 无前置依赖，被 F01/F03/F12 依赖 | P0 |
| F03 | 插件系统 | 高 | 高 | 依赖 F02（插件注册与发现走 API） | P1 |
| F04 | 分布式执行 | 中 | 高 | 依赖集群形态（PostgreSQL store），与 MVP 单机架构差异大 | P2 |
| F05 | 外部 KMS 集成 | 高 | 中 | 无前置依赖，凭据模块已有接口抽象 | P0 |
| F06 | RBAC 增强 | 高 | 中 | 无前置依赖，权限模块已有 v0 基础 | P1 |
| F07 | 多租户 | 中 | 高 | 依赖 F06（RBAC 增强为基础） | P2 |
| F08 | 变更日历 | 高 | 低 | 无前置依赖，独立于核心闭环 | P0 |
| F09 | ChatOps | 中 | 中 | 依赖 F02（事件推送走 gRPC stream） | P1 |
| F10 | 配置漂移检测 | 中 | 中 | 无前置依赖，drift scan 已有 MVP 基础 | P2 |
| F11 | Ansible/Terraform 深度集成 | 低 | 中 | 兼容层已有 MVP 基础，深度集成收益有限 | P3 |
| F12 | 移动端审批 | 中 | 中 | 依赖 F01（Web UI 响应式）+ F09（推送通道） | P2 |

### 2.3 新增候选特性

在 12 个原始候选基础上，基于 MVP 用户反馈与架构演进需要，新增 3 个特性：

表：新增候选特性对照表

| 编号 | 特性 | 用户价值 | 技术复杂度 | 依赖关系 | 建议优先级 |
| --- | --- | --- | --- | --- | --- |
| F13 | LEVEELang 编译期完整类型检查 | 高 | 中 | MVP DSL 解析器已有基础 | P0 |
| F14 | SLO 门禁三段时序完整版 | 高 | 低 | MVP 门禁框架已有 post_batch | P0 |
| F15 | 集群形态 + PostgreSQL store | 中 | 高 | 无前置依赖，但为 F04 前置 | P1 |

### 2.4 优先级决策逻辑

P0 特性的入选理由：

- **F02 gRPC API**：是平台化的基础设施，Web UI、插件系统、ChatOps 均依赖它。先做 gRPC，后续特性可并行开发。
- **F05 外部 KMS 集成**：金融/政企客户强诉求，MVP 本地加密存储不满足合规审计要求，凭据模块已有 `CredentialStore` 接口抽象，实现成本低。
- **F08 变更日历**：变更窗口管理是运维日常高频痛点，MVP 的 `window` 声明仅做静态校验，无冻结期、无日历视图、无冲突检测。实现独立，不侵入核心闭环。
- **F13 LEVEELang 编译期完整类型检查**：设计文档 R6 红线"语言必须类型安全"，MVP 仅做基础校验，完整类型检查是 V1 承诺项，也是降低运行期错误的核心手段。
- **F14 SLO 门禁三段时序完整版**：设计文档 D4 验证门禁的核心设计，MVP 仅实现 post_batch，pre_apply / grace_period 是高危变更场景的刚需。

P3 延后理由：

- **F11 Ansible/Terraform 深度集成**：兼容层已覆盖"能用"，深度集成（双向同步、状态导入）收益有限且增加维护负担，等插件系统成熟后以插件形式实现更合理。

---

## 第3章 分期规划

### 3.1 Phase 1: 核心增强（Q3 2026，8 周）

定位：补齐 MVP 设计承诺中未完成的关键特性，开放 API 层，解决合规与运营痛点。

#### 3.1.1 特性列表

| 编号 | 特性 | 范围 | 估时(PD) |
| --- | --- | --- | --- |
| F02 | gRPC API | 定义 protobuf 服务（ChangeService / TemplateService / TargetService / AuditService / SystemService），实现服务端，CLI 通过 gRPC client 调用（保留本地直连模式），REST API 网关兼容现有 HTTP 端点 | 15 |
| F05 | 外部 KMS 集成 | 实现 Vault KMS provider（KV v2 + AppRole 认证），实现 AWS KMS provider（Encrypt/Decrypt API），KMS 接口抽象层，凭据获取失败按 R7 结构化失败，本地加密存储降级为 fallback | 8 |
| F08 | 变更日历 | 变更窗口 CRUD + 冻结期声明 + 冲突检测（同目标集同窗口互斥）+ 日历视图 CLI 命令（`levee calendar list/show/create`）+ plan 阶段窗口校验增强 | 6 |
| F13 | LEVEELang 编译期完整类型检查 | 类型系统实现（string / int / float / bool / duration / percent / map / list），编译期类型推导与校验，IR 生成，编译错误定位到源码行号，`levee compile` 命令 | 10 |
| F14 | SLO 门禁三段时序完整版 | pre_apply 门禁（变更前 SLO 基线校验）+ grace_period 门禁（全量批次后等待 + 再次查询）+ 时序配置化，与现有 post_batch 门禁统一框架 | 5 |

#### 3.1.2 关键里程碑

表：Phase 1 里程碑对照表

| 里程碑 | 周次 | 交付物 | 验收标准 |
| --- | --- | --- | --- |
| PM1-1 | W3 | gRPC API 服务端可用 | protobuf 定义完成，5 个 Service 端点可调用，CLI gRPC client 模式跑通 |
| PM1-2 | W5 | KMS 集成 + SLO 门禁完整版 | Vault provider 可获取/归还凭据；pre_apply + grace_period 门禁端到端通过 |
| PM1-3 | W7 | LEVEELang 编译期类型检查 + 变更日历 | 类型错误可定位到行号；变更窗口冲突检测生效；冻结期阻断变更 |
| PM1-4 | W8 | Phase 1 集成验证 | 全部 P0 特性端到端通过；gRPC API 覆盖 CLI 全集命令；10 分钟测试门禁不退化 |

#### 3.1.3 不做清单

- 不做 Web UI（Phase 2，依赖 gRPC API 先稳定）
- 不做插件系统（Phase 2，依赖 gRPC API 注册机制）
- 不做集群形态（Phase 2，单机形态先验证 gRPC API 稳定性）
- 不做移动端（Phase 3）

---

### 3.2 Phase 2: 平台化（Q4 2026，10 周）

定位：Web UI 让非 CLI 用户参与变更治理，插件系统开放扩展能力，RBAC 增强满足企业权限管控，ChatOps 提升协作效率。

#### 3.2.1 特性列表

| 编号 | 特性 | 范围 | 估时(PD) |
| --- | --- | --- | --- |
| F01 | Web UI | 变更看板（列表/筛选/状态卡片）、审批界面（审批/驳回/转授权）、实时执行监控（SSE 日志流 + 批次进度）、模板管理、目标机管理、审计查询、系统状态。技术栈：Vue 3 + Vite + TypeScript，通过 gRPC-Web 调用后端 | 25 |
| F03 | 插件系统 | 插件接口定义（Channel / Gate / Module / Notifier 四类）、插件注册与发现（gRPC 插件协议，基于 hashicorp/go-plugin）、插件沙箱（资源限制 + 超时）、插件 CLI 命令（`levee plugin list/install/enable/disable`）、示例插件（IPMI 通道 + HTTP 探针门禁） | 15 |
| F06 | RBAC 增强 | 角色继承（角色树 + 权限继承）、细粒度权限（资源 × 动作 × 条件三元组）、ABAC 初步（基于标签的访问控制，如"仅允许操作 label:env=prod 的目标"）、权限缓存 + 预计算 | 10 |
| F09 | ChatOps | 飞书/钉钉/企微/Slack 机器人适配层，审批交互（卡片消息 + 一键审批/驳回）、变更通知推送（状态变更 → 群消息）、命令交互（`/levee list` / `/levee approve`），通过 gRPC stream 订阅事件 | 10 |
| F15 | 集群形态 + PostgreSQL store | Store 接口抽象（SQLite / PostgreSQL 双实现）、PostgreSQL schema + 迁移、连接池 + 事务管理、server 无状态化（session 外置）、3 节点高可用验证 | 12 |

#### 3.2.2 关键里程碑

表：Phase 2 里程碑对照表

| 里程碑 | 周次 | 交付物 | 验收标准 |
| --- | --- | --- | --- |
| PM2-1 | W3 | 集群形态 + PostgreSQL store | PostgreSQL store CRUD 通过；3 节点 server 可水平扩；单机 → 集群迁移脚本可用 |
| PM2-2 | W6 | Web UI 核心页面 + RBAC 增强 | 变更看板 + 审批界面 + 实时监控可用；角色继承 + ABAC 权限校验生效 |
| PM2-3 | W8 | 插件系统 + ChatOps | 示例插件（IPMI 通道）可加载执行；飞书/钉钉机器人可审批/推送 |
| PM2-4 | W10 | Phase 2 集成验证 | 全部 P1 特性端到端通过；Web UI 覆盖 CLI 80% 高频操作；插件系统可加载第三方插件；集群形态 3 节点压测通过 |

#### 3.2.3 不做清单

- 不做分布式执行（Phase 3，集群形态先验证稳定性）
- 不做多租户（Phase 3，RBAC 增强先验证权限模型合理性）
- 不做移动端审批（Phase 3）
- 不做配置漂移检测增强（Phase 3）

---

### 3.3 Phase 3: 生态扩展（Q1 2027，12 周）

定位：分布式执行突破单机性能上限，多租户支持团队隔离，漂移检测增强运维可观测性，移动端审批覆盖全场景。

#### 3.3.1 特性列表

| 编号 | 特性 | 范围 | 估时(PD) |
| --- | --- | --- | --- |
| F04 | 分布式执行 | Agent 模式（可选常驻，用于无 SSH 场景）、多节点调度器（任务分片 + 负载均衡）、Agent 心跳 + 注册/注销、Agent 任务执行 + 结果回传、Agent 通道插件（复用插件系统 F03） | 20 |
| F07 | 多租户 | 租户隔离（数据行级隔离 + 命名空间）、资源配额（目标机数 / 并发数 / 存储空间）、租户管理 CLI + API、租户间审计隔离 | 12 |
| F10 | 配置漂移检测增强 | 定期巡检调度（cron 表达式）、漂移基线自动生成（从最近一次 apply 快照提取）、漂移告警（与通知系统联动）、漂移趋势图（Web UI 展示） | 8 |
| F12 | 移动端审批 | Web UI 响应式适配（移动端布局）、推送通知（APNs/FCM）、一键审批/驳回（深度链接）、审批历史查看 | 8 |

#### 3.3.2 关键里程碑

表：Phase 3 里程碑对照表

| 里程碑 | 周次 | 交付物 | 验收标准 |
| --- | --- | --- | --- |
| PM3-1 | W4 | 分布式执行 Agent 模式 | Agent 可注册/注销/心跳；通过 Agent 通道执行命令；多节点调度器分片执行 500+ 目标机 |
| PM3-2 | W7 | 多租户 + 漂移检测增强 | 租户数据隔离验证通过；资源配额生效；定期巡检产出漂移报告；漂移告警通知发出 |
| PM3-3 | W10 | 移动端审批 | 移动端审批界面可用；推送通知送达；一键审批端到端通过 |
| PM3-4 | W12 | Phase 3 集成验证 | 全部 P2 特性端到端通过；分布式执行 500+ 目标机压测通过；多租户隔离无泄漏；移动端审批延迟 < 3s |

#### 3.3.3 不做清单

- 不做自愈（设计文档明确 V2+ 且默认关，MVP+1 不引入）
- 不做事件驱动规则引擎（复杂度高，等插件系统成熟后以插件形式实现）
- 不做气隙形态离线镜像（强隔离场景，等集群形态稳定后再做）
- 不做 Ansible/Terraform 深度集成（F11，以插件形式延后）

---

## 第4章 技术预研

### 4.1 F02 gRPC API

#### 4.1.1 技术方案概要

- **协议定义**：使用 protobuf v3 定义 5 个 Service（ChangeService / TemplateService / TargetService / AuditService / SystemService），每个 Service 对应 CLI 一个资源域。
- **双模式运行**：server 启动时同时监听 gRPC（默认 9090）和 HTTP（默认 8080）端口。HTTP 端点通过 grpc-gateway 转发到 gRPC Service，保持 REST API 兼容。
- **CLI 调用模式**：`--local` 模式直连本地 store（MVP 行为，默认）；`--remote` 模式通过 gRPC client 连接远端 server。两种模式共用同一 Service 接口，local 模式直接调用 Service 实现，remote 模式走 gRPC client。
- **流式接口**：`WatchChange` / `StreamLogs` 使用 gRPC server streaming，支持实时状态推送和日志流。
- **认证**：gRPC 基于 TLS + Bearer token metadata，与 REST API 认证统一。

#### 4.1.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| gRPC-Web 浏览器兼容性 | Web UI 需通过 gRPC-Web 调用，部分代理不支持 HTTP/2 | 使用 Envoy 或 grpcwebproxy 做协议转换；备选 REST API 网关 |
| protobuf 向后兼容 | 后续 API 迭代需保持向后兼容 | 遵循 protobuf 兼容性规则（只加字段不改编号）；API 版本化（v1/v2） |
| CLI 双模式切换复杂度 | local/remote 两种模式增加测试矩阵 | Service 接口统一抽象，local 模式复用 Service 实现而非独立路径 |

### 4.2 F05 外部 KMS 集成

#### 4.2.1 技术方案概要

- **接口抽象**：定义 `KMSProvider` 接口（`GetSecret` / `ReturnSecret` / `HealthCheck`），与现有 `CredentialStore` 解耦。
- **Vault Provider**：使用 `hashicorp/vault/api` Go 客户端，AppRole 认证，KV v2 引擎，租约管理（凭据获取后自动续租，apply 结束后撤销租约）。
- **AWS KMS Provider**：使用 `aws-sdk-go-v2`，Encrypt/Decrypt API 做信封加密，数据密钥由 AWS KMS 生成，LEVEE 仅持有加密后的数据密钥。
- **降级策略**：KMS 不可用时降级到本地 AES-GCM 加密存储（MVP 行为），降级事件告警，不阻断变更。
- **凭据不落盘**：KMS 获取的凭据仅存于内存，apply 结束后立即清零，不进 trace、不进日志。

#### 4.2.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| Vault 连接延迟 | 凭据获取增加网络往返，影响 apply 启动速度 | 连接池 + 预取（plan 阶段验证 KMS 可达，apply 阶段批量获取） |
| Vault 不可用 | 变更阻断 | 降级到本地加密存储 + 告警；凭据获取失败按 R7 结构化失败 |
| AWS KMS 限流 | 大规模并发时 KMS API 限流 | 信封加密减少 KMS 调用（数据密钥缓存 1h）；限流时排队重试 |

### 4.3 F08 变更日历

#### 4.3.1 技术方案概要

- **数据模型**：`calendar` 表存储变更窗口定义（名称/起止时间/目标集标签/冻结期标记/重复规则），`calendar_conflicts` 表存储冲突检测结果。
- **冻结期**：冻结期内同目标集不允许创建新变更（紧急审批除外），已有变更允许执行但不允许新建。
- **冲突检测**：plan 阶段检查目标集与变更窗口的交集，同窗口同目标集互斥告警（非硬阻断，允许人工确认继续）。
- **重复规则**：支持简单 cron 表达式定义周期性变更窗口（如"每月第二个周六 02:00-06:00"）。
- **CLI 命令**：`levee calendar list/show/create/update/delete`，`levee calendar check --target <label>` 检查目标集当前窗口状态。

#### 4.3.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| 时区处理 | 跨时区团队变更窗口计算错误 | 统一使用 UTC 存储，展示层按用户时区转换；与 LEVEELang window 时区声明一致 |
| 冲突检测性能 | 大量目标集 + 大量窗口组合爆炸 | 按目标集标签倒排索引加速查询；plan 阶段增量检查而非全量扫描 |

### 4.4 F13 LEVEELang 编译期完整类型检查

#### 4.4.1 技术方案概要

- **类型系统**：实现 8 种基础类型（string / int / float / bool / duration / percent / map / list）+ 自定义类型别名（`type port = int`）+ 枚举（`enum status { ok, warn, crit }`）。
- **类型推导**：input 参数显式声明类型，workflow 内变量通过赋值推导类型，模板参数编译期校验类型匹配。
- **IR 生成**：LEVEELang 编译为 IR（中间表示），IR 包含类型信息 + 批次结构 + 步骤依赖，编排器直接消费 IR 执行。
- **错误报告**：编译错误定位到源码行号 + 列号，错误信息含期望类型与实际类型，支持多个错误批量报告。
- **`levee compile` 命令**：显式编译 workflow 文件，产出 IR + 类型检查报告，CI/CD 集成用。

#### 4.4.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| 向后兼容 | 新增类型检查可能导致现有 YAML workflow 编译失败 | 提供 `--strict` / `--lenient` 模式切换；lenient 模式仅警告不阻断；MVP YAML 子集保持兼容 |
| IR 格式稳定性 | IR 格式变更影响编排器 | IR 版本化（ir_version 字段）；编排器支持多版本 IR |

### 4.5 F14 SLO 门禁三段时序完整版

#### 4.5.1 技术方案概要

- **pre_apply 门禁**：apply 启动前查询 SLO 指标，确认系统健康。查询失败按策略处理（默认阻断，可配置降级为告警继续）。
- **grace_period 门禁**：全量批次结束后等待可配置时间（默认 5min），再次查询 SLO 指标，确认无延迟暴露的异常。
- **时序配置**：在 LEVEELang gate 声明中增加 `timing: pre_apply | post_batch | post_apply | grace_period` 字段，一个 gate 可声明多个时机。
- **与现有框架集成**：复用 MVP `GateManager` 接口，扩展 `CheckTiming` 枚举，编排器按时机调度门禁执行。

#### 4.5.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| grace_period 延长 apply 周期 | 等待 5min 增加 apply 总耗时 | grace_period 可配置（0-30min），非高危 workflow 可设为 0；等待期间资源释放不占连接 |
| Prometheus 查询超时 | SLO 查询超时导致门禁误判 | 重试 + 超时配置（默认 30s）；超时按策略处理（阻断/告警/跳过） |

### 4.6 F01 Web UI

#### 4.6.1 技术方案概要

- **技术栈**：Vue 3 + Vite + TypeScript + Element Plus，通过 gRPC-Web 调用后端 API。
- **核心页面**：变更看板（列表/筛选/状态卡片/批量操作）、审批界面（审批/驳回/转授权/审批历史）、实时执行监控（SSE 日志流 + 批次进度 + 门禁状态）、模板管理（CRUD + 参数表单）、目标机管理（列表/导入/连通性检查）、审计查询（trace + 哈希链校验）、系统状态（仪表盘）。
- **认证**：session cookie 认证，登录页 + token 管理，与 REST API 认证统一。
- **部署**：Web UI 构建为静态文件，嵌入 Go 二进制（go:embed），server 启动时同时服务静态文件和 API。

#### 4.6.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| gRPC-Web 代理依赖 | 需要额外部署 Envoy/grpcwebproxy | Phase 1 先用 REST API 网关（grpc-gateway），Phase 2 评估 gRPC-Web 性能后决定是否切换 |
| SSE 日志流性能 | 大量并发变更时日志推送压力大 | 日志流按 run 粒度订阅，非全局广播；背压控制 + 客户端限速 |
| 前端构建体积 | 嵌入 Go 二进制增加体积 | Vite tree-shaking + 按需加载；静态资源 CDN 可选 |

### 4.7 F03 插件系统

#### 4.7.1 技术方案概要

- **插件协议**：基于 `hashicorp/go-plugin`，gRPC 插件协议。四类插件接口：`ChannelPlugin`（通道扩展）、`GatePlugin`（门禁扩展）、`ModulePlugin`（动作模块扩展）、`NotifierPlugin`（通知渠道扩展）。
- **插件生命周期**：安装（`levee plugin install`）→ 注册（写入 plugin_registry 表）→ 启用（`levee plugin enable`）→ 运行（按需启动子进程）→ 停用（`levee plugin disable`）。
- **沙箱**：插件运行在独立子进程，资源限制（CPU/内存/超时），崩溃不影响主进程。插件间通过 gRPC 通信，不共享内存。
- **示例插件**：IPMI 通道插件（goipmi 库）+ HTTP 探针门禁插件（HTTP 健康检查）。

#### 4.7.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| 插件稳定性 | 第三方插件崩溃可能影响调度 | 子进程隔离 + 崩溃自动重启（最多 3 次）+ 崩溃告警；关键路径（如审批）不依赖插件 |
| 插件安全 | 恶意插件可能窃取凭据 | 插件无权访问凭据明文；凭据由主进程获取后注入插件环境变量；插件需签名验证 |
| 插件版本兼容 | 插件与主程序版本不兼容 | 插件声明兼容版本范围；主程序启动时校验插件版本；不兼容时拒绝加载并告警 |

### 4.8 F06 RBAC 增强

#### 4.8.1 技术方案概要

- **角色继承**：角色树结构，子角色继承父角色全部权限。如 `db-operator` 继承 `operator` 并增加数据库专属权限。
- **细粒度权限**：资源 × 动作 × 条件三元组。如 `resource=change, action=apply, condition=target.env=prod` 表示"仅允许对生产环境目标执行变更"。
- **ABAC 初步**：基于标签的访问控制，策略声明格式 `allow/deny <action> on <resource> when <label-condition>`。
- **权限缓存**：权限计算结果缓存到内存（TTL 5min），变更权限配置时主动失效。

#### 4.8.2 关键技术风险

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| 权限计算性能 | 细粒度权限 + ABAC 增加每次操作的计算开销 | 权限缓存 + 预计算；热路径权限校验 < 5ms |
| 权限模型复杂度 | 角色继承 + ABAC 组合可能导致权限冲突 | 冲突检测（deny 优先于 allow）；权限预览命令（`levee user check-perms`） |

---

## 第5章 资源估算

### 5.1 各 Phase 人力需求

表：各 Phase 人力需求对照表

| Phase | 周期 | 特性数 | 总估时(PD) | 建议团队规模 | 可用人天 | 缓冲率 |
| --- | --- | --- | --- | --- | --- | --- |
| Phase 1 | 8 周 | 5 | 44 | 3 人 | 120 | +63% |
| Phase 2 | 10 周 | 5 | 72 | 4 人 | 200 | +64% |
| Phase 3 | 12 周 | 4 | 48 | 3 人 | 180 | +72% |

Phase 1 缓冲率高是因为 gRPC API 是新基础设施，需要额外时间做稳定性验证和性能调优。Phase 2 Web UI 工作量大（25 PD），建议增加 1 名前端工程师。Phase 3 分布式执行是架构级变更，需要充裕的集成验证时间。

### 5.2 团队角色建议

表：各 Phase 团队角色对照表

| Phase | 角色 | 人数 | 职责 |
| --- | --- | --- | --- |
| Phase 1 | 后端引擎 | 2 | gRPC API + KMS 集成 + LEVEELang 类型检查 + SLO 门禁 |
| Phase 1 | 运营体验 | 1 | 变更日历 + 集成测试 |
| Phase 2 | 后端引擎 | 2 | 插件系统 + RBAC 增强 + 集群形态 |
| Phase 2 | 前端 | 1 | Web UI 全栈 |
| Phase 2 | 集成体验 | 1 | ChatOps + 集成测试 |
| Phase 3 | 后端引擎 | 2 | 分布式执行 + 多租户 |
| Phase 3 | 前端/全栈 | 1 | 移动端审批 + 漂移检测 Web UI |

### 5.3 关键路径

Phase 1 关键路径：

```
F02 gRPC API 定义 → F02 gRPC 服务端实现 → F02 CLI gRPC client
  → F02 集成测试 → PM1-4
```

Phase 2 关键路径：

```
F15 集群形态 → F01 Web UI 数据层 → F01 Web UI 核心页面
  → F01 集成测试 → PM2-4
```

Phase 3 关键路径：

```
F04 Agent 模式 → F04 多节点调度 → F04 集成压测
  → F07 多租户 → PM3-4
```

---

## 第6章 与 MVP 架构兼容性分析

### 6.1 接口兼容

表：MVP+1 与 MVP 接口兼容性对照表

| 模块 | MVP 接口 | MVP+1 变更 | 兼容策略 |
| --- | --- | --- | --- |
| CredentialStore | `GetCredential(target) → Credential` | 新增 `KMSProvider` 接口，`CredentialStore` 委托给 KMS | KMS 不可用时降级到本地加密，接口不变 |
| GateManager | `CheckGate(gate, timing) → Result` | 扩展 `timing` 枚举增加 `pre_apply` / `grace_period` | 新 timing 值为可选，MVP workflow 无此声明时跳过 |
| Store | SQLite 直接操作 | 新增 PostgreSQL 实现，Store 接口抽象 | SQLite 实现保留，通过配置切换，接口不变 |
| PermissionChecker | `CheckPermission(user, action, resource) → bool` | 扩展为角色继承 + ABAC | v0 权限矩阵作为默认策略，增强功能向后兼容 |
| Channel | `Connect / Exec / Collect / Disconnect` | 无变更，插件系统通过 `ChannelPlugin` 扩展 | 核心接口不变，插件实现走独立子进程 |

### 6.2 数据兼容

- SQLite schema 变更通过迁移脚本增量升级，不破坏现有数据。
- gRPC API 版本化（v1），后续迭代通过 v2 扩展，v1 保持可用。
- LEVEELang IR 版本化，编排器支持多版本 IR。

### 6.3 部署兼容

- 单二进制形态保留：Phase 1/2 特性均可单机运行，不强制集群。
- 集群形态为可选增强：PostgreSQL store 通过配置切换，SQLite 为默认。
- Web UI 嵌入二进制：不增加额外部署组件。

---

## 第7章 风险与缓解

表：MVP+1 风险与缓解对照表

| 编号 | 风险 | 影响 | 概率 | 缓解措施 |
| --- | --- | --- | --- | --- |
| R1 | gRPC API 稳定性 | API 不稳定导致 Web UI / 插件系统阻塞 | 中 | Phase 1 先做 API 并做充分集成测试；API 兼容性测试加入 CI |
| R2 | Web UI 工期超期 | 前端工作量 25 PD，可能超期 | 中 | 分优先级交付：审批界面 > 执行监控 > 管理页面；MVP 功能优先 |
| R3 | 插件安全漏洞 | 恶意插件窃取凭据或破坏系统 | 低 | 子进程隔离 + 凭据不注入插件 + 插件签名验证 |
| R4 | 集群形态复杂度 | PostgreSQL 迁移 + 多节点调试增加工期 | 中 | 先做单机 PostgreSQL 验证，再做多节点；迁移脚本充分测试 |
| R5 | LEVEELang 类型检查向后兼容 | 新类型检查导致现有 workflow 编译失败 | 高 | lenient 模式仅警告不阻断；提供迁移工具自动修复常见类型问题 |
| R6 | ChatOps 多平台适配 | 飞书/钉钉/企微 API 差异大 | 中 | 先做 Slack + 飞书两个平台，其余通过插件系统扩展 |
| R7 | 分布式执行一致性 | Agent 模式下任务分发与结果收集的一致性 | 中 | Agent 任务幂等 + 重试 + 超时；结果回传带校验和 |

---

## 第8章 成功指标

表：MVP+1 成功指标对照表

| Phase | 指标 | 目标值 |
| --- | --- | --- |
| Phase 1 | gRPC API 覆盖率 | 覆盖 CLI 全集 100% 命令 |
| Phase 1 | KMS 集成可用性 | Vault provider 端到端通过；凭据获取延迟 < 200ms |
| Phase 1 | LEVEELang 类型检查 | 编译期类型错误检出率 ≥ 95%（对比运行期错误） |
| Phase 1 | SLO 门禁完整版 | pre_apply + grace_period 端到端通过 |
| Phase 2 | Web UI 覆盖率 | 覆盖 CLI 80% 高频操作 |
| Phase 2 | 插件系统可用性 | 示例插件可加载执行；第三方插件可安装运行 |
| Phase 2 | 集群形态 | 3 节点高可用；单机 → 集群迁移 < 30min |
| Phase 2 | ChatOps 延迟 | 审批通知 → 用户收到 < 5s |
| Phase 3 | 分布式执行规模 | 500+ 目标机并发执行通过 |
| Phase 3 | 多租户隔离 | 租户间数据零泄漏 |
| Phase 3 | 移动端审批延迟 | 推送通知 → 一键审批完成 < 3s |