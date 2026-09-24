# LEVEE

> LEVEE = Lifecycle Enforcement & Verification Engine
> 高危变更治理引擎：变更影响生产、不可逆、需审批留痕的变更，都走 LEVEE

## 定位

把"改一台主机/数据库/网络设备/中间件"从手工命令和线性 playbook，升级为
计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚 -> 审计留痕 的完整闭环，
默认无代理、CLI 优先。

LEVEE 治理的边界是**变更的危险度**，不是资产的位置：

- 变更影响生产、动作不可逆、合规要求审批留痕 -> LEVEE
- 日常巡检/作业流/告警 -> 其他平台（如 OpsMesh）

- **云上云下、K8s 集群内外**的"高危变更"都归 LEVEE 管：物理机/虚拟机/
  数据库（含云上 RDS）/网络设备/中间件，经 SSH / WinRM 直推，默认无代理
- 与 ArgoCD/Flux 分层互补：集群内声明式交付（manifest 级、控制器调谐）
  归它们；集群外命令式高危变更（动作级、人工审批、显式回滚）归 LEVEE
- 与 OpsMesh 的边界（运维同宅、职责分层）：OpsMesh 是综合全能的私有化
  运维平台（agent 常驻纳管、CMDB、日志采集、作业流 DAG、告警引擎，日常
  操作都由它干）；LEVEE（堤坝）专管**高危变更**——无 agent、SSH/WinRM
  直推、强制走 计划→审批→分批→验证→回滚 门禁链。
  日常自动化走 OpsMesh，动生产数据的变更走 LEVEE。

## 核心特性

- **变更流水线**：计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚，全程审计哈希链留痕（WORM）；真实执行由执行引擎承担，显式 `--engine-enabled` 开启（默认关闭；计划持久化 + plan_hash 绑定，apply 执行的必是被批准的那份计划）
- **危险度评分与审批分级路由**：plan 生成时自动评分（不可逆步骤/破坏性动作/影响面/回滚覆盖/批次扇出五因子，明细可解释），分数+红线推导审批下限（任一不可逆步骤 ⇒ 至少 high），**tier = max(workflow 声明, 计划下限) 只升不降**；PlanChange 自动启动审批链（补齐"声明了但没人创建 pending 记录"断链），评分与下限随 plan_json 落盘
- **单步验证门 API**：`POST /gates/verify` 按需执行一条 cmd/probe/slo 验证（与引擎门禁同源构造器，零语义漂移）——操作员预检、流水线 pre-check、ChatOps 即席健康检查无需规划整个变更；human 检查点显式排除（归审批链）；每次执行留审计
- **ITSM Jira 审批镜像**：`notify.jira.*` 配置启用后审批链开始建 Jira issue、决策自动评论（纯出站镜像，LEVEE store 仍是唯一事实来源；禁用时零外发零装配）
- **双协议 API**：gRPC（5+ 服务）+ REST 网关（`/api/v1/`），共享同一服务实例
- **AI 辅助运维**：告警接入 → 拓扑诊断 → LLM 对话式定位 → RAG 知识增强推荐 → 自动执行 → 效果学习
- **多通道执行**：SSH / WinRM 无代理通道 + local 沙箱通道（CI/单机自测，程序白名单+参数策略+沙箱根三重门禁，fail-closed 默认），插件注册表开放第三方通道接入（见 `docs/channel-plugins.md`）
- **快照回滚**：`rollback: {strategy: snapshot, snapshot_paths: [...]}` 声明式文件级备份——apply 前经通道采集目标机文件（base64 传输），回滚时原样恢复（与 undo-action 显式互斥）；快照按 change id 键存，手动回滚路径可达；`--engine-snapshot-dir` 一旗标启用
- **数据库动作模块**：`mysql.query`（幂等 DDL/DML，SQL 经 base64 管道防注入）、`mysql.pt_osc`（pt-online-schema-change 在线大表变更，白名单校验 alter 子句）、`mysql.replica_switch`（主从切换编排，强制确认门）；不可逆动作（含 replica_switch/pt_osc）在 plan 生成时自动标记并路由到高危审批（R2/R4 全链路接线）
- **资源沙箱**：Linux 下 cgroup v2 硬限制插件内存/CPU；墙钟超时全平台兜底
- **集群模式**：PostgreSQL 共享存储 + 持久化成员注册（心跳/stale 检测）+ 租约式分布式锁（过期自动可抢占）保证数据一致性；**在途变更故障接管**——执行节点崩溃后其运行中变更由执行租约（epoch 围栏）与 leader 接管循环收敛至 `interrupted` 终态（审计留痕、永不重跑副作用，`RetryChange` 显式重驱动）；**跨节点调度**——leader 将已批准 run 分派给空闲 worker 节点水平执行（run 级分派，loopback 通道已验证收敛到终态 + 审计证据）
- **合规交付**：`levee audit report` 一键生成时间窗合规报告（HTML 自包含：变更清单+审批链+哈希链验证结论+回滚记录），监管/审计离线可读
- **ChatOps 审批桥**：`internal/notify/chatopsbridge` 提供 approval 创建/决策到 BotManager 事件的组合接点，可将请求与 1/2 决策进度投影到钉钉/飞书/Slack；具体 bot 进程与 manager 生命周期仍由部署侧组合
- **安全默认**：auth 启动门禁、CORS 默认拒绝、限流、请求 ID 追踪、TLS 支持

## 快速开始

```bash
# 构建
make build

# 运行
./levee --help

# 创建变更（从模板实例化；可用模板用 ./levee template list 查看）
./levee new nginx-reload --params target=web01.prod

# 查看变更
./levee list
./levee show <run-id>
```

### 启动 API 服务

```bash
# 生产模式：必须提供 token（或设置 LEVEE_TOKEN 环境变量）
./levee serve --token <your-secret>

# 开发模式：跳过认证门禁（仅本地调试）
./levee serve --insecure

# 可选：CORS 白名单、限流、网关监听地址
./levee serve --token <secret> --cors-origin https://ops.example.com --rate-limit 200 --rate-burst 400 --http-addr :8080

# 执行引擎（默认关闭）：开启后 PlanChange 生成并持久化真实计划、
# ApplyChange 经 SSH/WinRM 通道真正执行被批准的计划（失败自动回滚）。
# 关闭时 apply 明确拒绝（FailedPrecondition），不会假装执行。
# 并发执行上限 --engine-max-parallel-runs（默认 4，超出的 apply 快速失败）；
# slo 验证门禁需 --engine-gate-prometheus 提供 Prometheus 地址，缺省则 slo 门禁 fail-closed。
# 目标凭据解析依赖 LEVEE_MASTER_PASSWORD 环境变量（未设置时通道匿名拨号并输出警告）。
./levee serve --token <secret> --engine-enabled
```

无 token 且未传 `--insecure` 时服务拒绝启动；无 TLS 时输出明文传输警告。
健康探针：标准 gRPC health service（`grpc.health.v1`）；REST 网关（默认监听
`:8080`，可用 `--http-addr` 调整）提供 `/healthz`——在服务注册完成前返回
503 `{"status":"unavailable"}`，`serve` 启动流程会自动注册服务，正常运行时为 200。

## 项目结构

```text
levee/
├── cmd/levee/              # CLI 入口
├── internal/
│   ├── engine/             # 工作流引擎
│   ├── executor/           # 模块执行器 (file/pkg/shell/svc/user)
│   ├── channel/            # 通道抽象层 (SSH/WinRM)
│   ├── plugin/             # 插件系统 + cgroup 沙箱
│   ├── approval/           # 审批服务
│   ├── audit/              # 审计哈希链 (WORM)
│   ├── state/              # SQLite / PostgreSQL 存储
│   ├── plan/               # plan 生成与哈希锁定
│   ├── rollback/           # 回滚协议
│   ├── verify/             # 验证门禁
│   ├── batch/              # 批次控制
│   ├── lock/               # 互斥锁
│   ├── credential/         # 凭据管理
│   ├── template/           # 模板库
│   ├── compat/             # playbook 兼容层
│   ├── dsl/                # YAML 子集解析 (LEVEELang)
│   ├── notify/             # 通知
│   ├── config/             # 配置管理
│   ├── permission/         # RBAC 权限
│   ├── pause/              # 全局暂停
│   ├── grpc/               # gRPC 服务 + REST 网关
│   ├── web/                # Web UI
│   ├── cluster/            # 集群模式
│   ├── tenant/             # 多租户隔离
│   ├── drift/              # 漂移检测
│   ├── calendar/           # 变更日历
│   ├── chatops/            # ChatOps 集成
│   ├── scheduler/          # 调度
│   ├── agent/              # agent
│   ├── alert/              # 告警网关 (Zabbix/Nagios 适配)
│   ├── diagnosis/          # 诊断引擎 (拓扑分析/LLM 推理)
│   ├── recommend/          # AI 推荐 (RAG/反馈学习)
│   ├── autoplanner/        # 自动规划与执行
│   ├── opsmesh/            # OpsMesh 平台集成
│   └── conversation/       # 对话引擎
├── configs/                # 配置文件示例
├── docs/                   # 设计文档 + 发布说明
├── examples/               # 示例工作流/模板/插件
├── tests/                  # 集成/E2E 测试
└── scripts/                # 脚本
```

## 文档

| 文档 | 说明 |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | 快速上手指南 |
| [docs/levee-design.md](docs/levee-design.md) | 完整设计文档 |
| [docs/leveelang-spec.md](docs/leveelang-spec.md) | LEVEELang DSL 规范 |
| [docs/levee-api.md](docs/levee-api.md) | CLI 命令与 API 设计 |
| [docs/cli-reference.md](docs/cli-reference.md) | CLI 参考手册 |
| [docs/deployment.md](docs/deployment.md) | 生产部署与升级手册 |
| [docs/security-audit.md](docs/security-audit.md) | 安全审计与部署安全声明 |
| [docs/opsmesh-integration-design.md](docs/opsmesh-integration-design.md) | OpsMesh 集成设计 |
| [docs/release-notes/](docs/release-notes/) | 各版本发布说明 |
| [CHANGELOG.md](CHANGELOG.md) | 变更日志 |

## 技术栈

- Go 1.26+（静态编译，单二进制）
- SQLite（嵌入式，零依赖）/ PostgreSQL（集群模式可选）
- SSH: golang.org/x/crypto/ssh
- WinRM: masterzen/winrm
- gRPC + protobuf（REST 网关同进程反代）
- CLI: cobra

## 安全说明

- `serve` 默认要求认证 token，`--insecure` 仅限开发环境
- CORS 默认拒绝所有跨域，需显式白名单
- 内置限流（默认 200 rps）与请求 ID 全链路追踪
- 密码等敏感参数经临时文件传输，不出现在进程 argv
- Linux 插件受 cgroup v2 内存/CPU 硬限制约束
- CI 集成 gosec（静态扫描）+ trivy（镜像 CVE 扫描）

详见 [docs/security-audit.md](docs/security-audit.md)。

## 开发

```bash
make lint      # 静态检查
make test      # 单元测试
make build     # 构建
make cross-build  # 跨平台编译
```

## 版本

当前版本 v1.13.0。MVP (3 个月) -> V1 (6 个月) -> V2 (12 个月)

详见 [docs/mvp-tasks.md](docs/mvp-tasks.md) 与 [CHANGELOG.md](CHANGELOG.md)。
