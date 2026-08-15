# LEVEE MVP 开发任务拆分

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE MVP 开发任务拆分 |
| 文档类型 | 开发任务拆分文档 |
| 版本 | v1.0 |
| 日期 | 2026-08-15 |
| 周期 | 3 个月（12 周） |
| 技术栈 | Go + SQLite |
| 上游依据 | levee-design.md 第 8 章 MVP 路线图 |
| 评审状态 | 待评审 |

---

## 第1章 MVP 范围与目标

### 1.1 交付清单

MVP 周期 3 个月，单二进制零依赖部署，覆盖从计划到归档的完整变更闭环最小可用集。交付项源自设计文档第 8 章 MVP 路线图。

表：MVP 交付清单对照表

| 编号 | 交付项 | 来源设计 | 说明 |
| --- | --- | --- | --- |
| D-01 | CLI 优先 | D2.2.5 | 所有操作可通过 CLI 完成，不强制 Web UI |
| D-02 | 连接池执行（SSH + WinRM 最小） | D1.1 | 通道抽象层 + SSH 连接池 + WinRM 最小子集 |
| D-03 | 批次执行 | D5.1 | 批次一等公民，按百分比 / 数量 / 标签分批 |
| D-04 | 验证门禁 | D4.4.5 | 命令门禁 + SLO 门禁（post_batch） |
| D-05 | 回滚协议 | D4.4.6 | 白名单 + 快照 + 按批逆序 + 回滚后验证 |
| D-06 | dry-run 预览 | D2.2.4 | 产出执行计划不真正执行 |
| D-07 | 审计（哈希链） | D7.1 | trace + 哈希链 + WORM 存储 |
| D-08 | playbook 兼容层最小子集 | D8.2 | 导入并执行现有 Ansible playbook |
| D-09 | YAML 子集表达工作流 | D2.2.2 | LEVEELang 基础语法（batch / gate / approval / rollback） |
| D-10 | LEVEELang 基础 | D2.2.2 | 类型化 input / target / window / batches / step / rollback |
| D-11 | 变更克隆 / 模板实例化 | D2.2.3 | clone 历史变更 + 模板参数填充 |
| D-12 | 目标预检 | D4.1.7 | apply 前可达性预检，剔除失败目标 |
| D-13 | 不可逆操作标记 | D4.4.6.1 | irreversible: true + 白名单 + 强制高危审批 |
| D-14 | 全局暂停 | D4.4.7 | pause-all / resume-all 紧急止血 |
| D-15 | 互斥锁 + TTL | D5.4 | 同目标机单 workflow + TTL 防死锁 |
| D-16 | 回滚演练 | D4.4.6 | 隔离环境强制失败验证回滚 |
| D-17 | 权限 v0（团队 × 环境） | D8 | 二维权限矩阵最小子集 |
| D-18 | 单机零依赖部署 | D12.1 | 单二进制 + 内嵌 SQLite，空机器一键跑通 |
| D-19 | 跑通批量变更 100 台 | 第 8 章门禁 | 端到端集成验证 |
| D-20 | 通知（webhook） | D9 | webhook 通知渠道 |

### 1.2 不做清单

MVP 明确不做以下项，避免范围蔓延。不做项源自设计文档第 8 章 MVP 路线图"不做"列。

表：MVP 不做清单对照表

| 编号 | 不做项 | 延后到 | 原因 |
| --- | --- | --- | --- |
| N-01 | 集群形态 | V1 | 单机形态先跑通，集群需 PostgreSQL store 高可用 |
| N-02 | Agent 通道 | V2 | 默认无代理（R1），Agent 模式为可选增强 |
| N-03 | LEVEELang 编译期完整类型检查 | V1 | MVP 仅做基础校验，完整类型系统在 V1 |
| N-04 | 自愈 | V2 | 默认关，避免误触发扩大变更半径 |
| N-05 | Web UI | V1 | MVP 仅 CLI，Web UI 是 CLI 可视化层 |
| N-06 | 事件驱动规则引擎 | V2 | 复杂度高，MVP 不引入 |
| N-07 | 气隙形态离线镜像 | V2 | 强隔离场景，MVP 不覆盖 |
| N-08 | 厂商 API 通道（F5 / 华为 eNSP 等） | V1 | MVP 仅 SSH + WinRM |
| N-09 | 交互式 SSH（mysql / vty 会话） | V1 | 会话级独占，MVP 不实现 |
| N-10 | ITSM 集成（ServiceNow / Jira） | V1 | MVP 审批闭环内置，不接外部 ITSM |
| N-11 | 凭据代理（Vault / CyberArk） | V1 | MVP 本地加密存储，V1 接外部凭据代理 |
| N-12 | 变更-告警关联 | V2 | 依赖外部告警系统接入 |
| N-13 | 效能数据导出 | V2 | 依赖 SRE 平台消费侧 |
| N-14 | 主动漂移检测 | 不做 | 设计已明确不做，仅只读漂移报告 |
| N-15 | SLO 门禁 grace_period 三段时序完整版 | V1 | MVP 实现 post_batch，pre_apply / grace_period 在 V1 |
| N-16 | 人工门禁 | V1 | MVP 聚焦自动化门禁 |
| N-17 | 探针门禁 | V1 | MVP 聚焦命令 + SLO 两类 |
| N-18 | DAG 完整编排 | V1 | MVP 步骤间串行 + 批次并行，完整 DAG 在 V1 |
| N-19 | 转授权 | V1 | MVP 审批人固定，转授权在 V1 |
| N-20 | webhook 触发 + schedule 触发 | V1 | MVP 仅 CLI 手动触发 |

### 1.3 发布门禁

MVP 发布前必须通过以下门禁，对应设计文档第 8 章门禁列。

表：MVP 发布门禁对照表

| 编号 | 门禁项 | 验收标准 | 来源 |
| --- | --- | --- | --- |
| G-01 | 10 分钟测试门禁 | `levee test` 10 分钟内完成：单元测试 + 集成测试（mock target）+ Lint | D2.2.6 |
| G-02 | 单二进制一键跑通 | 空机器下载二进制 + 配置文件即可启动，无外部依赖 | D12.1 |
| G-03 | 兼容层可导入执行 | 现有 Ansible playbook 可导入并执行，包审批 / 门禁 / 审计 | D8.2 |
| G-04 | 回滚演练通过率 | 隔离环境强制失败，回滚成功率 ≥ 95% | D4.4.6 |
| G-05 | 批量变更 100 台跑通 | 端到端 100 台目标机批量变更全流程通过 | 第 8 章 |
| G-06 | 对照数据（2h → 10min） | 与 Ansible 同场景对比，LEVEE 端到端耗时 ≤ 10min（Ansible 基线 2h） | 第 8 章 |
| G-07 | 审计哈希链可校验 | 任意 run 的 trace 哈希链可独立校验通过，篡改可检出 | D7.1 |

### 1.4 技术选型

MVP 技术栈遵循"静态编译、单二进制、零依赖"原则。

表：MVP 技术选型对照表

| 类别 | 选型 | 版本 | 用途 | 选型理由 |
| --- | --- | --- | --- | --- |
| 语言 | Go | 1.22+ | 全栈实现 | 静态编译单二进制，并发原语成熟，跨平台 |
| 存储 | SQLite | 内嵌 | 状态 / 审计 / 凭据 | 嵌入式零依赖，单文件，WAL 模式够用 |
| SSH | golang.org/x/crypto/ssh | latest | SSH 通道 | 官方扩展库，纯 Go 实现 |
| WinRM | masterzen/winrm | latest | WinRM 通道 | 社区主流，纯 Go 实现 |
| CLI | cobra | v1.x | CLI 框架 | Go 生态事实标准，子命令 + 补全 |
| 配置 | viper | v1.x | 配置管理 | 多格式 + 环境变量覆盖 |
| 日志 | slog | 标准库 | 结构化日志 | Go 1.21+ 标准库，无外部依赖 |
| YAML | gopkg.in/yaml.v3 | v3 | YAML 子集解析 | 稳定成熟 |
| 哈希 | crypto/sha256 | 标准库 | 哈希链 | 标准库 |
| 加密 | crypto/aes + argon2 | 标准库 + golang.org/x/crypto/argon2 | 凭据本地加密 | 标准库 + 官方扩展 |
| 测试 | testing + testify | 标准库 + v1.x | 单元 + 集成测试 | 标准库 + 断言辅助 |
| Lint | golangci-lint | latest | 静态检查 | Go 生态主流 |
| 构建 | Make + goreleaser | latest | 跨平台打包 | goreleaser 跨平台交叉编译 |

---

## 第2章 模块划分

MVP 模块划分遵循设计文档第 7 章 6 层架构，按 Go 包组织，模块间通过接口解耦。

表：MVP 模块划分对照表

| 模块路径 | 层 | 职责 | 关键接口 | 依赖 |
| --- | --- | --- | --- | --- |
| cmd/levee/ | L1 接入层 | CLI 入口，cobra 子命令注册 | main | internal/* |
| internal/engine/ | L2 编排层 | 工作流引擎，plan / apply / verify / rollback 编排 | Engine, Run | plan, batch, executor, verify, rollback, approval, audit, state |
| internal/executor/ | L2 编排层 | 模块执行器，shell 直译 + 模块分发 | Executor, Module | channel, state |
| internal/channel/ | L4 通道层 | 通道抽象层 + SSH / WinRM 实现 | Channel, Target | 无 |
| internal/approval/ | L3 闭环层 | 审批服务，分级审批 + 模板库 | ApprovalService | state, notify |
| internal/audit/ | L6 集成层 | 审计哈希链，trace + WORM + 校验 | AuditChain | state |
| internal/state/ | L5 状态层 | SQLite 存储层，schema + 迁移 + CRUD | Store | 无 |
| internal/plan/ | L2 编排层 | plan 生成 + 哈希锁定 + 影响面 | Planner | dsl, state |
| internal/rollback/ | L3 闭环层 | 回滚协议，白名单 + 按批逆序 + 快照 | RollbackManager | executor, verify, state |
| internal/verify/ | L3 闭环层 | 验证门禁，命令 + SLO 两类 | GateManager | channel |
| internal/batch/ | L2 编排层 | 批次控制，分批 + 批间等待 + 并发限速 | BatchExecutor | executor, verify |
| internal/lock/ | L5 状态层 | 互斥锁 + TTL，目标机级 | LockManager | state |
| internal/credential/ | L6 集成层 | 凭据管理，本地加密存储 | CredentialStore | state |
| internal/template/ | L2 编排层 | 模板库，参数实例化 + 变更克隆 | TemplateEngine | dsl, state |
| internal/compat/ | L6 集成层 | playbook 兼容层，最小子集导入 | CompatLayer | executor, plan |
| internal/dsl/ | L2 编排层 | YAML 子集解析，LEVEELang 基础 | Parser, AST | yaml.v3 |
| internal/notify/ | L6 集成层 | 通知，webhook 渠道 | Notifier | 无 |
| internal/config/ | L1 接入层 | 配置管理，config.yaml + 环境变量 | Config | viper |
| internal/permission/ | L3 闭环层 | 权限 v0，团队 × 环境二维 | PermissionChecker | state |
| internal/pause/ | L3 闭环层 | 全局暂停 / 恢复 | PauseManager | state, engine |

---

## 第3章 任务拆分（按周排期）

MVP 拆分为 12 周开发任务，任务编号 T001-T0NN，按周次排期。估时单位为人天（PD），按 2-3 人团队折算。

### 3.1 第 1-2 周：基础框架

搭建项目骨架与基础设施，为后续模块提供运行底座。

表：第 1-2 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T001 | Go 项目脚手架 | 根目录 | 1 | 无 | go.mod / Makefile / .golangci.yml / README 就绪，`make build` 产出二进制 | W1 |
| T002 | CI 流水线 | .github/workflows | 1 | T001 | GitHub Actions 跑 lint + test + build，PR 强制通过 | W1 |
| T003 | SQLite 存储层 schema | internal/state | 3 | T001 | schema.sql 定义 runs / batches / steps / trace / approvals / locks / credentials / audit 表，迁移机制就绪 | W1 |
| T004 | SQLite CRUD 封装 | internal/state | 3 | T003 | Store 接口定义 + SQLite 实现，单元测试覆盖 CRUD，WAL 模式开启 | W1-W2 |
| T005 | 配置管理 | internal/config | 2 | T001 | config.yaml 加载 + 环境变量覆盖 + 校验，viper 集成 | W1 |
| T006 | 日志系统 | internal/ | 1 | T001 | slog 结构化日志，级别可配，JSON / text 双格式 | W1 |
| T007 | CLI 框架基础 | cmd/levee | 2 | T001 | cobra 根命令 + version / help，子命令注册框架，`-o json` 输出切换 | W2 |
| T008 | 错误码体系 | internal/ | 1 | T001 | 错误码定义 + 结构化错误包装，对应失败语义五档 | W2 |
| T009 | 单元测试基础设施 | internal/ | 1 | T001 | testify 集成 + mock target 框架 + 测试夹具 | W2 |

### 3.2 第 2-4 周：通道与执行

实现 SSH + WinRM 通道与执行器，打通"能连上目标机执行命令"。

表：第 2-4 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T010 | 通道抽象层接口 | internal/channel | 2 | T001 | Channel / Target 接口定义，生命周期建连 / 执行 / 收证 / 断开 | W2 |
| T011 | SSH 通道实现 | internal/channel/ssh | 4 | T010 | golang.org/x/crypto/ssh 集成，密码 / 密钥认证，exec + 文件传输（scp） | W2-W3 |
| T012 | SSH 连接池 | internal/channel/ssh | 3 | T011 | 连接池 + 多路复用（ControlMaster 等价），单目标机并发上限可配 | W3 |
| T013 | WinRM 通道最小子集 | internal/channel/winrm | 3 | T010 | masterzen/winrm 集成，negotiate + exec，单连接单命令 | W3 |
| T014 | WinRM 连接池 | internal/channel/winrm | 2 | T013 | 连接池 + 单连接单命令策略，并发上限可配 | W3 |
| T015 | 通道限速与背压 | internal/channel | 2 | T012, T014 | 全局并发上限 + 单通道 + 单目标机三级限速，背压排队 + 超时 | W3 |
| T016 | 模块执行器框架 | internal/executor | 2 | T010 | Executor 接口 + Module 注册机制，输入 / 输出 / 耗时 / exit_code 结构化 | W3 |
| T017 | shell 直译模块 | internal/executor/modules/shell | 2 | T016 | shell / command 模块，exec 命令并收证，幂等契约声明字段 | W4 |
| T017.1 | file 模块实现 | internal/executor/modules/file | 2 | T016 | file 模块 copy / template 动作，文件分发 + 模板渲染 + 校验，幂等契约声明字段（DSL 5.2 内置模块） | W4 |
| T017.2 | pkg 模块实现 | internal/executor/modules/pkg | 2 | T016 | pkg 模块 install / remove / upgrade 动作，包管理器抽象（apt/yum/dnf），幂等契约声明字段（DSL 5.2 内置模块） | W4 |
| T017.3 | svc 模块实现 | internal/executor/modules/svc | 2 | T016 | svc 模块 start / stop / restart / enable / disable / reload 动作，服务管理器抽象（systemd/sysvinit），幂等契约声明字段（DSL 5.2 内置模块） | W4 |
| T017.4 | user 模块实现 | internal/executor/modules/user | 1 | T016 | user 模块 add / remove / modify 动作，用户/组管理 + SSH 公钥分发，幂等契约声明字段（DSL 5.2 内置模块） | W4 |
| T018 | 目标可达性预检 | internal/channel | 2 | T011, T013 | apply 前对每台目标机 noop 探测，失败剔除并产出预检报告 | W4 |
| T019 | 通道集成测试 | internal/channel | 2 | T012, T014 | 对 mock SSH / WinRM server 端到端测试，连接复用 + 限速验证 | W4 |

### 3.3 第 3-5 周：工作流核心

实现 YAML 子集解析、plan 生成、批次控制、验证门禁，打通"能按计划分批执行"。

表：第 3-5 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T020 | YAML 子集解析器 | internal/dsl | 3 | T005 | 解析 input / target / window / batches / step / rollback / approval，产出 AST | W3 |
| T021 | LEVEELang 基础校验 | internal/dsl | 2 | T020 | 必填字段校验 + 类型基础校验 + 批次声明合法性，非法 DSL 报错阻断 | W4 |
| T022 | plan 生成器 | internal/plan | 3 | T020, T018 | 目标解析 + 批次划分 + 步骤编排，产出执行计划结构体 | W4 |
| T023 | 影响面分析 | internal/plan | 2 | T022 | 直接受影响目标集 + 间接影响标注，产出影响面报告 | W4 |
| T024 | plan 哈希锁定 | internal/plan | 2 | T022 | canonical 化 + sha256 计算，plan_hash = hash(workflow + 目标集 + 参数 + 批次 + 影响面) | W4 |
| T025 | 批次控制器 | internal/batch | 3 | T016, T022 | 分批执行 + 批间串行 + 批内并发，批次边界显式，批间等待可配 | W5 |
| T026 | 批次并发限速 | internal/batch | 1 | T025 | 批内并发受 T015 限速约束，超限排队 | W5 |
| T027 | 验证门禁框架 | internal/verify | 2 | T010 | GateManager 接口 + 门禁注册，pre_apply / post_batch / post_apply 三时机 | W5 |
| T028 | 命令门禁 | internal/verify | 2 | T027 | 在目标机执行检查命令，期望 exit_code / stdout 匹配，重试 + 超时 | W5 |
| T029 | SLO 门禁 post_batch | internal/verify | 2 | T027 | 查询 Prometheus 指标，阈值比对，post_batch 时机，重试 + 超时 | W5 |
| T030 | plan 集成测试 | internal/plan | 1 | T024, T025 | 端到端 plan → batch 划分 → 哈希校验，mock 目标集 | W5 |

### 3.4 第 4-6 周：变更闭环

实现审批、回滚、互斥锁、全局暂停，打通"变更闭环可回滚"。

表：第 4-6 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T031 | 审批服务框架 | internal/approval | 2 | T004 | ApprovalService 接口 + 状态机（待审批 / 通过 / 驳回 / 超时） | W4 |
| T032 | 审批分级 | internal/approval | 2 | T031 | 标准 / 高危 / 紧急三级，触发条件 + 审批人要求 + 超时配置 | W5 |
| T033 | 审批模板库 | internal/approval | 1 | T031 | 高危规则模板（删库 / 主从切换 / 防火墙全量），可配置 | W5 |
| T034 | 不可逆操作标记 | internal/executor | 2 | T016 | irreversible: true 声明 + 白名单校验 + 强制升高审批级别 | W5 |
| T035 | 回滚协议框架 | internal/rollback | 2 | T016 | RollbackManager 接口 + 白名单校验 + 按批逆序调度 | W5 |
| T036 | 快照管理 | internal/rollback | 3 | T035 | apply 前快照创建（文件 / 配置备份）+ 快照存储 + 快照恢复 | W6 |
| T037 | 回滚后验证 | internal/rollback | 2 | T035, T027 | 回滚完成后强制跑 verify，失败按回滚失败处理 | W6 |
| T038 | 回滚失败分级 | internal/rollback | 1 | T035 | 成功 / 部分回滚 / 回滚失败三档，对应通知 + 升级动作 | W6 |
| T039 | 失败语义五档 | internal/engine | 2 | T008 | retryable / manual_retry / rollback / escalate / fatal 五档处理逻辑 | W6 |
| T040 | 互斥锁 + TTL | internal/lock | 3 | T004 | 目标机级锁 + TTL 默认 1h + 锁过期抢占 + 抢占前状态检查 | W6 |
| T041 | 全局暂停 / 恢复 | internal/pause | 2 | T040 | pause-all / resume-all + 单 run pause / resume，留痕 + 权限校验 | W6 |
| T042 | 闭环集成测试 | internal/engine | 2 | T037, T040 | plan → apply → verify → rollback 端到端，mock 目标机 | W6 |

### 3.5 第 5-7 周：审计与安全

实现审计哈希链、凭据管理、权限 v0、通知，打通"变更可审计 + 凭据安全"。

表：第 5-7 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T043 | 审计 trace 记录 | internal/audit | 2 | T004 | 每个动作记录输入 / 输出 / 耗时 / 目标机上下文，写入 store | W5 |
| T044 | 哈希链构建 | internal/audit | 3 | T043 | 每个动作 hash 包含前一动作 hash，链式结构，分批分片 | W6 |
| T045 | WORM 存储模拟 | internal/audit | 2 | T044 | SQLite 模拟 WORM（追加只写 + 校验和），不可篡改 | W6 |
| T046 | 哈希链校验 | internal/audit | 2 | T044 | 任意 run 的 trace 可独立校验，篡改可检出并报错 | W6 |
| T047 | 凭据本地加密存储 | internal/credential | 3 | T004 | AES-GCM 加密 + argon2 密钥派生，凭据不落盘明文 / 不进 trace / 不进日志 | W6 |
| T048 | 凭据按需获取 | internal/credential | 1 | T047 | apply 时按目标机获取凭据，用完即弃，失败按 R7 结构化失败 | W7 |
| T049 | 权限 v0 框架 | internal/permission | 2 | T004 | 团队 × 环境二维权限矩阵，配置文件定义 | W7 |
| T050 | 权限校验集成 | internal/permission | 2 | T049 | plan / apply / approval / rollback 操作前权限校验，无权限阻断 | W7 |
| T051 | 通知框架 | internal/notify | 2 | T006 | Notifier 接口 + 触发点注册，对象分级（发起人 / 审批人 / oncall / 订阅人） | W7 |
| T052 | webhook 通知渠道 | internal/notify | 2 | T051 | webhook 发送 + 签名校验 + 重试，触发点贯穿闭环 | W7 |
| T053 | 回滚通知独立 | internal/notify | 1 | T052 | 回滚触发 / 结果独立通知发起人 + 审批人 + oncall，不与 apply 合并 | W7 |
| T054 | 审计集成测试 | internal/audit | 1 | T046 | 端到端 apply → trace → 哈希链 → 校验，篡改检出 | W7 |

### 3.6 第 6-8 周：兼容与体验

实现 playbook 兼容层、dry-run、变更克隆 / 模板实例化，打通"第一天能干活"。

表：第 6-8 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T055 | playbook 兼容层框架 | internal/compat | 2 | T016 | CompatLayer 接口 + playbook 导入解析，独立模块不引入核心依赖（R8） | W6 |
| T056 | playbook 最小子集执行 | internal/compat | 3 | T055, T017, T017.1 | 支持 shell / command / file / copy / template 模块，包审批 / 门禁 / 审计 | W7 |
| T057 | 兼容层风险评估 | internal/compat | 2 | T055 | 静态分析 shell / command 非幂等 + ignore_errors + 无 rollback，命中标记高危 | W7 |
| T058 | 裸 shell 直跑 | internal/executor | 1 | T017 | `levee run --shell "cmd"` 单命令直跑，不走 workflow，最小可用 | W7 |
| T059 | dry-run 预览 | internal/plan | 2 | T022, T023 | `levee plan --dry-run` 产出目标集 / 批次 / 影响面 / 预估耗时 / 潜在冲突，不执行 | W7 |
| T060 | 变更克隆 | internal/template | 2 | T004 | `levee clone <run-id>` 生成可编辑副本，保留原参数与批次结构 | W8 |
| T061 | 模板库管理 | internal/template | 2 | T060 | 模板存储 + 列表 + show，模板带参数占位 | W8 |
| T062 | 模板实例化 | internal/template | 2 | T061 | `levee new <template> --params key=val,...` 参数填充 + 完整性校验，编译期校验参数 | W8 |
| T063 | 兼容层集成测试 | internal/compat | 2 | T056, T057 | 现有 Ansible playbook 导入执行，审批 / 门禁 / 审计包裹验证 | W8 |

### 3.7 第 8-10 周：CLI 命令全集

实现变更生命周期全套 CLI 命令，对应设计文档 D4.4.8 操作全集。

表：第 8-10 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T064 | new 命令 | cmd/levee | 1 | T062 | `levee new <template> --params key=val,...` 实例化 workflow | W8 |
| T065 | clone 命令 | cmd/levee | 1 | T060 | `levee clone <run-id>` 克隆历史变更 | W8 |
| T066 | show 命令 | cmd/levee | 1 | T004 | `levee show <run-id>` 展示 run 详情（plan / 批次 / 状态 / trace） | W8 |
| T067 | list 命令 | cmd/levee | 1 | T004 | `levee list [--status] [--template]` 列出 run，过滤 + 分页 | W8 |
| T068 | approve / reject 命令 | cmd/levee | 2 | T032 | `levee approve <run-id>` / `levee reject <run-id> --reason`，权限校验 | W8 |
| T069 | apply 命令 | cmd/levee | 2 | T042 | `levee apply <run-id>` 触发 apply，哈希校验 + 快照 + 分批 + 门禁 + 回滚 | W9 |
| T070 | pause / resume 命令 | cmd/levee | 1 | T041 | `levee pause <run-id>` / `levee resume <run-id>` 单 run 暂停恢复 | W9 |
| T071 | pause-all / resume-all 命令 | cmd/levee | 1 | T041 | 全局暂停恢复，高权限 + 留痕 | W9 |
| T072 | cancel 命令 | cmd/levee | 1 | T041 | `levee cancel <run-id>` 取消未开始批次，已开始按策略 | W9 |
| T073 | retry / retry-host 命令 | cmd/levee | 2 | T025 | `levee retry <run-id>` / `levee retry-host <run-id> <host>`，重试上限 3 | W9 |
| T074 | rollback 命令 | cmd/levee | 2 | T037 | `levee rollback <run-id>` 手动触发回滚，按批逆序 + 回滚后验证 | W9 |
| T075 | logs 命令 | cmd/levee | 2 | T043 | `levee logs <run-id> [--target] [-f]` 实时日志跟随 + 历史日志查询 | W9 |
| T076 | diff 命令 | cmd/levee | 1 | T004 | `levee diff <run-a> <run-b>` 对比两次 run 参数与执行计划 | W9 |
| T077 | trace 命令 | cmd/levee | 1 | T046 | `levee trace <run-id>` 展示完整 trace + 哈希链校验结果 | W10 |
| T078 | archive 命令 | cmd/levee | 1 | T046 | `levee archive <run-id>` 归档到 WORM，归档失败告警不阻断 | W10 |
| T079 | link 命令 | cmd/levee | 1 | T004 | `levee link <run-id> --incident <id>` 关联变更与 incident | W10 |
| T080 | 模板管理命令 | cmd/levee | 2 | T061 | `levee template list / show / create / delete` 模板库管理（对齐 API create/delete） | W10 |
| T081 | 目标管理命令 | cmd/levee | 2 | T004 | `levee target list / import / check` 目标机管理 + 预检（对齐 API import/check，remove 延后至 V1） | W10 |
| T082 | 审计命令 | cmd/levee | 2 | T046 | `levee audit verify / export / list / show` 审计校验 + 导出 + 查询（对齐 API list/show） | W10 |
| T083 | 凭据命令 | cmd/levee | 2 | T047 | `levee secret list / add / rotate / revoke / show` 凭据管理，不回显明文（对齐 API secret 命名） | W10 |
| T084 | 权限命令 | cmd/levee | 2 | T049 | `levee user list / add` + `levee team list / add` 权限矩阵管理（对齐 API user/team 维度） | W10 |
| T085 | 系统命令 | cmd/levee | 2 | T005 | `levee version / status / config get / config set / doctor` 系统初始化 + 状态 + 配置（对齐 API 独立命令） | W10 |
| T086 | CLI 集成测试 | cmd/levee | 2 | T064-T085 | 全套命令端到端测试，`-o json` 输出校验，补全提示 | W10 |

### 3.8 第 10-11 周：集成与演练

回滚演练 + 端到端集成测试 + 对照数据采集，验证发布门禁。

表：第 10-11 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T087 | 回滚演练环境搭建 | tests/e2e | 2 | T042 | 隔离环境（docker-compose mock 目标机）+ 强制失败注入 | W10 |
| T088 | 回滚演练用例 | tests/e2e | 3 | T087 | 覆盖 8 个核心场景的回滚演练，回滚成功率 ≥ 95% | W10-W11 |
| T089 | 批量变更 100 台集成 | tests/e2e | 3 | T086 | 100 台 mock 目标机端到端 plan → apply → verify → rollback | W11 |
| T090 | 批量变更性能调优 | internal/batch | 2 | T089 | 100 台端到端耗时 ≤ 10min，连接池 + 并发参数调优 | W11 |
| T091 | 对照数据采集脚本 | tests/benchmark | 2 | T089 | Ansible 同场景基线 + LEVEE 计时对比脚本，产出对照报告 | W11 |
| T092 | 对照数据验证 | tests/benchmark | 1 | T091 | LEVEE 端到端 ≤ 10min，Ansible 基线 2h，对照数据达成 | W11 |
| T093 | 10 分钟测试门禁 | tests/ | 2 | T086 | `levee test` 10 分钟内完成单元 + 集成 + Lint，超时失败 | W11 |
| T094 | 哈希链篡改检出测试 | tests/e2e | 1 | T046 | 篡改 trace / 审计记录，校验可检出并报错 | W11 |

### 3.9 第 11-12 周：发布准备

单二进制打包 + 文档 + 发布门禁检查，准备可交付物。

表：第 11-12 周任务对照表

| 任务ID | 任务名称 | 模块 | 估时(PD) | 依赖 | 验收标准 | 周次 |
| --- | --- | --- | --- | --- | --- | --- |
| T095 | goreleaser 跨平台打包 | build/ | 2 | T086 | linux amd64 / arm64 + windows amd64 单二进制，含 checksum + changelog | W11 |
| T096 | 安装脚本 | scripts/ | 1 | T095 | install.sh 一键下载 + 安装 + 初始化，空机器跑通 | W11 |
| T097 | 快速开始文档 | docs/ | 2 | T086 | 5 分钟快速开始：安装 + 配置 + 第一个 workflow + apply | W12 |
| T098 | CLI 参考文档 | docs/ | 2 | T086 | 全套命令参考 + 参数说明 + 示例，自动生成 + 人工补充 | W12 |
| T099 | LEVEELang 语法文档 | docs/ | 2 | T020 | YAML 子集语法 + 字段说明 + 示例 workflow | W12 |
| T100 | 10 分钟测试找新手 | tests/ | 2 | T093 | 找 2-3 名新手盲写 workflow + apply，10 分钟内完成 | W12 |
| T101 | 发布门禁检查 | scripts/ | 1 | T088-T094, T100 | G-01 至 G-07 全部门禁通过，产出门禁报告 | W12 |
| T102 | 发布候选构建 | build/ | 1 | T095, T101 | RC 构建 + 签名 + 发布到 artifact 存储 | W12 |
| T103 | 发布说明 | docs/ | 1 | T102 | RELEASE.md 含交付清单 + 已知限制 + 升级路径 | W12 |

---

## 第4章 任务依赖图

任务间存在显式依赖，关键路径决定整体周期。依赖关系按"必须先完成"语义声明。

### 4.1 关键路径

关键路径为最长依赖链，决定 MVP 最早完成时间：

```
T001 脚手架 → T003 SQLite schema → T004 SQLite CRUD
  → T010 通道接口 → T011 SSH → T012 SSH 连接池
  → T016 执行器 → T020 YAML 解析 → T022 plan 生成 → T024 plan 哈希
  → T025 批次控制 → T027 门禁框架 → T028 命令门禁
  → T035 回滚框架 → T036 快照 → T037 回滚后验证
  → T042 闭环集成 → T069 apply 命令 → T086 CLI 集成
  → T089 批量 100 台 → T090 性能调优 → T092 对照数据
  → T101 发布门禁 → T102 RC 构建
```

### 4.2 依赖簇

按模块归并依赖簇，便于并行调度：

表：任务依赖簇对照表

| 依赖簇 | 入口任务 | 前置依赖 | 可并行任务 |
| --- | --- | --- | --- |
| 基础设施 | T001 | 无 | T002, T005, T006, T008, T009 |
| 存储层 | T003, T004 | T001 | T047, T049 并行于 T010 |
| 通道层 | T010 | T001 | T011 / T013 并行，T012 / T014 并行 |
| 执行层 | T016 | T010 | T017, T017.1, T017.2, T017.3, T017.4, T034 |
| 解析层 | T020 | T005 | T021 |
| plan 层 | T022 | T020, T018 | T023, T024 并行 |
| 批次层 | T025 | T016, T022 | T026 |
| 门禁层 | T027 | T010 | T028, T029 并行 |
| 审批层 | T031 | T004 | T032, T033 并行 |
| 回滚层 | T035 | T016 | T036, T037, T038 串行 |
| 锁层 | T040 | T004 | T041 |
| 审计层 | T043 | T004 | T044 → T045 → T046 串行 |
| 凭据层 | T047 | T004 | T048 |
| 权限层 | T049 | T004 | T050 |
| 通知层 | T051 | T006 | T052, T053 |
| 兼容层 | T055 | T016 | T056, T057 并行 |
| 模板层 | T060 | T004 | T061, T062 串行 |
| CLI 层 | T064-T085 | 各对应内部模块 | 按命令分组并行 |
| 集成层 | T087 | T042 | T088, T089, T091 并行 |
| 发布层 | T095 | T086 | T096, T097, T098, T099 并行 |

### 4.3 依赖约束

- T022 plan 生成强依赖 T018 目标预检，预检失败的目标在 plan 阶段剔除。
- T024 plan 哈希强依赖 T022，哈希锁定后 apply 前必须校验一致。
- T035 回滚强依赖 T036 快照，无快照则无回滚依据。
- T040 互斥锁强依赖 T004，锁状态持久化到 SQLite。
- T056 兼容层执行强依赖 T057 风险评估，风险命中先升高审批级别。
- T088 回滚演练强依赖 T042 闭环集成，闭环不通则演练无意义。
- T101 发布门禁强依赖 T088-T094 全部集成验证通过。

---

## 第5章 里程碑

MVP 设置 4 个里程碑，每个里程碑有明确交付物与验收标准，对应关键路径关键节点。

表：MVP 里程碑对照表

| 里程碑 | 周次 | 名称 | 交付物 | 验收标准 | 关键任务 |
| --- | --- | --- | --- | --- | --- |
| M1 | W4 | 通道 + 执行可跑通 | SSH + WinRM 通道 + shell 执行器 + 目标预检 | 单台 SSH 命令执行成功，exit_code / stdout / stderr 正确回收；连接池复用率 ≥ 80%；目标预检报告产出 | T011-T019 |
| M2 | W7 | 变更闭环可跑通 | plan + apply + verify + rollback + 审计 + 凭据 + 权限 + 通知 | 端到端 plan → apply → verify → rollback 通过；哈希链可校验；凭据不落盘明文；权限校验生效；webhook 通知发出 | T020-T054 |
| M3 | W10 | CLI 全集 + 兼容层可用 | 全套 CLI 命令 + playbook 兼容层 + dry-run + 模板 | 全套命令端到端可用；现有 Ansible playbook 可导入执行；dry-run 产出执行计划；模板实例化参数校验 | T055-T086 |
| M4 | W12 | 发布门禁通过 | 单二进制 + 文档 + 对照数据 + 回滚演练 | G-01 至 G-07 全部通过；100 台批量变更 ≤ 10min；回滚演练 ≥ 95%；对照数据 2h → 10min 达成 | T087-T103 |

里程碑间存在硬依赖：M1 不通过不进 M2，M2 不通过不进 M3，M3 不通过不进 M4。每个里程碑设 1 天缓冲，用于修复验收发现问题。

---

## 第6章 风险与应对

MVP 阶段识别 6 项主要风险，每项给出影响与缓解措施。风险编号延续设计文档第 9 章 K 系列。

表：MVP 风险与应对对照表

| 编号 | 风险 | 影响 | 概率 | 缓解措施 | 负责模块 |
| --- | --- | --- | --- | --- | --- |
| M-K1 | SSH 库选型风险 | golang.org/x/crypto/ssh 在大规模并发或跳板机多跳场景可能不稳定 | 中 | T011 先做连接池压测（100 并发持续 10min）；若不稳定备选 gliderlabs/ssh；跳板机 ProxyJump 在 MVP 仅支持单跳 | internal/channel/ssh |
| M-K2 | WinRM 兼容性风险 | masterzen/winrm 对 Windows Server 版本 / 认证方式兼容性差异 | 中 | T013 仅验证 Windows Server 2016+ + Negotiate 认证；Kerberos / NTLM 在 V1；备选 github.com/bhoriuchi/go-winrm | internal/channel/winrm |
| M-K3 | SQLite 并发性能 | SQLite WAL 模式下写并发上限，批量 100 台 trace 写入可能瓶颈 | 中 | T043 trace 写批量提交 + 单写锁；T089 压测验证；若瓶颈备选 PRAGMA journal_mode + synchronous 调优；万级目标机明确不在 MVP | internal/state, internal/audit |
| M-K4 | 回滚演练环境搭建 | mock 目标机回滚语义模拟难度，部分动作回滚难复现 | 高 | T087 用 docker-compose + 文件系统快照模拟；不可逆动作（DROP TABLE）用白名单跳过 + 人工确认；演练用例覆盖 8 场景但允许部分标记"仅校验不回滚" | tests/e2e |
| M-K5 | 10 分钟测试找人 | 新手盲写 workflow 可能在 10 分钟内无法完成，门禁无法通过 | 高 | T100 提前准备 2-3 名新手 + 快速开始文档；若失败则迭代文档而非放宽门禁；文档先行（T097 在 T100 前完成） | docs/, tests/ |
| M-K6 | 对照数据基线采集 | Ansible 同场景基线 2h 依赖真实环境，mock 环境可能不真实 | 中 | T091 在真实或半真实环境采集；若无法获取真实基线，用 Ansible 官方 benchmark 数据 + 合理外推；对照报告标注数据来源 | tests/benchmark |

风险监控：每周例会回顾风险状态，概率 / 影响变化及时更新缓解措施。M-K4 / M-K5 为高概率风险，需在 W10 前启动缓解。

---

## 第7章 团队建议

### 7.1 团队规模

MVP 周期 3 个月，建议团队规模 2-3 人。低于 2 人周期可能延长到 4-5 个月，高于 3 人沟通成本上升收益递减。

表：团队规模与周期对照表

| 规模 | 配置 | 预计周期 | 适用场景 |
| --- | --- | --- | --- |
| 1 人全栈 | 单人覆盖全部模块 | 4-5 个月 | 资源受限 / 原型验证 |
| 2 人分工 | 1 引擎 + 1 体验 | 3 个月 | 推荐，MVP 标配 |
| 3 人分工 | 1 引擎 + 1 体验 + 1 通道 | 3 个月 | 推荐，并行度更高 |
| 4+ 人 | 增加测试 / 文档专职 | 3 个月 | 沟通成本上升，边际收益递减 |

### 7.2 角色分工

3 人团队推荐分工：

表：3 人团队角色分工对照表

| 角色 | 负责模块 | 负责任务 | 关键里程碑 |
| --- | --- | --- | --- |
| 核心引擎 | internal/engine, plan, batch, verify, rollback, lock, state, audit | T003-T004, T020-T030, T035-T042, T043-T046 | M1, M2 |
| CLI + 体验 | cmd/levee, internal/config, template, compat, notify, dsl, permission, credential, pause, approval | T005-T009, T031-T034, T047-T054, T055-T063, T064-T086 | M3 |
| 通道 + 兼容 | internal/channel, executor, internal/compat | T010-T019, T016-T017, T055-T057, T063 | M1, M3 |

2 人团队合并"通道 + 兼容"到"核心引擎"角色，通道任务串行进入引擎角色排期，周期仍可控但并行度降低。

### 7.3 协作约定

- 每日站会：15 分钟，同步进展 + 阻塞。
- 每周例会：1 小时，回顾 + 风险 + 下周排期。
- 里程碑评审：每个里程碑结束 1 小时，验收 + 缓冲修复。
- 代码评审：所有 PR 至少 1 人评审，核心引擎模块（engine / plan / rollback / audit）至少 2 人评审。
- 文档同步：每个任务完成同步更新对应文档，避免文档滞后。

---

## 附录A 任务统计

表：MVP 任务统计对照表

| 统计项 | 数值 |
| --- | --- |
| 任务总数 | 107 |
| 总估时 | 203 人天 |
| 周次覆盖 | 12 周 |
| 模块覆盖 | 20 个 |
| 里程碑数 | 4 个 |
| 风险项数 | 6 个 |
| 发布门禁数 | 7 个 |

按 3 人团队 12 周（每周 5 工作日）计算，可用 180 人天，估时 203 人天，缓冲约 -13%。需通过并行调度 + 部分任务压缩吸收，或按 2 人团队 12 周（120 人天）+ 1 人辅助 4 周（20 人天）= 140 人天，缺口 63 人天需延长周期至 4 个月或裁剪非关键任务。

### A.1 估时方案

针对上述估时缺口，识别两个候选方案：

表：MVP 估时方案对照表

| 方案 | 内容 | 可用人天 | 缺口 | 风险 |
| --- | --- | --- | --- | --- |
| 方案 A | 维持 3 人 × 12 周，并行调度 + 非关键任务压缩吸收 -13% 缺口 | 180 | -23 PD | 关键路径无缓冲，任一任务延期即延期发布；非关键任务压缩可能牺牲质量 |
| 方案 B | 延长周期至 16 周（3 人 × 16 周 × 5 工作日 = 240 人天） | 240 | +37 PD（15%） | 周期延长 4 周，M1-M4 里程碑相应后延，但关键路径不变，余量用于缓冲和探索性工作 |

方案选定：采用方案 B——延长周期至 16 周（3 人 × 16 周 × 5 工作日 = 240 人天可用），余量 37 人天（15%）用于缓冲和探索性工作。M1-M4 里程碑相应后延 4 周，但关键路径不变。

## 附录B 周次排期总览

表：MVP 周次排期总览对照表

| 周次 | 阶段 | 关键任务 | 里程碑 |
| --- | --- | --- | --- |
| W1 | 基础框架 | T001-T006 | - |
| W2 | 基础框架 + 通道 | T007-T011 | - |
| W3 | 通道 + 工作流核心 | T012-T016, T020 | - |
| W4 | 通道 + 工作流核心 + 闭环 | T017-T017.4, T018-T019, T021-T024, T031 | M1 |
| W5 | 工作流核心 + 闭环 | T025-T030, T032-T035 | - |
| W6 | 闭环 + 审计 + 兼容 | T036-T042, T043-T045, T055 | - |
| W7 | 审计 + 兼容 + 体验 | T046-T054, T056-T059 | M2 |
| W8 | 兼容 + 体验 + CLI | T060-T063, T064-T068 | - |
| W9 | CLI | T069-T076 | - |
| W10 | CLI + 集成 | T077-T086, T087 | M3 |
| W11 | 集成 + 发布准备 | T088-T095 | - |
| W12 | 发布准备 | T096-T103 | M4 |
