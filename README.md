# LEVEE

> LEVEE = Lifecycle Enforcement & Verification Engine
> 高危变更治理引擎：变更影响生产、不可逆、需审批留痕的变更，都走 LEVEE

## 定位

把"改一台主机/数据库/网络设备/中间件"从手工命令和线性 playbook，升级为
计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚 -> 审计留痕 的完整闭环，
默认无代理、CLI 优先。

### LEVEE 是一套系统，不只是一个 workflow 引擎

核心实体是**变更（Change）**，不是工作流：53 个 gRPC RPC（`proto/levee.proto`
40 个 + `proto/levee_extra.proto` 13 个）里 22 个挂在 `ChangeService` 上，
是九个已注册服务中最大的一个。工作流（LEVEELang 文档）只是变更的**声明式定义**，
计划（Plan）才是被 `plan_hash` 绑定、被审批、被执行的那份制品。

按 `internal/` 的实际构成，与编排直接相关的包（dsl / plan / engine / wiring /
executor / dispatch / batch / cluster）约占三成，其余是支撑一套系统所需的
平台能力：身份与授权（auth / permission ABAC / credential 加密 / tenant）、
生命周期（scheduler / calendar / backup / cluster 故障转移 / takeover）、
运维面（metrics / tracing / audit / notify / push / chatops）、
诊断与建议（diagnosis / recommend / autoplanner 已并入 recommend / drift）、
以及对外集成（itsm / opsmesh）。这些不是 workflow 引擎的组成部分——
workflow 引擎不拥有身份、不拥有数据生命周期、也不做集群故障接管。

术语由此确立：**产品面一律说「变更 / change」**，`workflow` 只在指代
LEVEELang 定义本身时出现。完整定义见
[`docs/leveelang-spec.md` 第 0 章](docs/leveelang-spec.md)（术语与状态词表的
权威来源，由 `internal/runstatus` 与跨语言一致性测试在 CI 中钉住）。

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
- **双协议 API**：gRPC（9 个业务服务 + 标准 `grpc.health.v1`）+ REST 网关（`/api/v1/`），共享同一服务实例
- **AI 辅助（部分能力仅库就绪，见下方"能力可达性"）**：`internal/diagnosis`（日志采集+分析、健康探测）已接入 `serve` 与 `levee diagnose`；`levee converse` / REST 对话闭环已接入，确认的建议会变成 draft 变更进治理链（不执行、不跳审批）。**LLM 与 RAG 尚无生产调用方**：`recommend.NewLLMClient`（OpenAI / Ollama 客户端）只被测试调用，`serve` 与 `converse` 构造推荐引擎时不传 `LLMClient`（`recommend/engine.go:130` 注释即"nil = pure knowledge-base mode"），建议来自内置静态知识库；`internal/config` 也没有 llm/rag 相关配置项，运维无法开启。
- **多通道执行**：SSH / WinRM 无代理通道 + local 沙箱通道（CI/单机自测，程序白名单+参数策略+沙箱根三重门禁，fail-closed 默认），插件注册表开放第三方通道接入（见 `docs/channel-plugins.md`）
- **快照回滚**：`rollback: {strategy: snapshot, snapshot_paths: [...]}` 声明式文件级备份——apply 前经通道采集目标机文件（base64 传输），回滚时原样恢复（与 undo-action 显式互斥）；快照按 change id 键存，手动回滚路径可达；`--engine-snapshot-dir` 一旗标启用
- **数据库动作模块**：`mysql.query`（幂等 DDL/DML，SQL 经 base64 管道防注入）、`mysql.pt_osc`（pt-online-schema-change 在线大表变更，白名单校验 alter 子句）、`mysql.replica_switch`（主从切换编排，强制确认门）；不可逆动作（含 replica_switch/pt_osc）在 plan 生成时自动标记并路由到高危审批（R2/R4 全链路接线）
- **资源沙箱**：Linux 下 cgroup v2 硬限制插件内存/CPU；墙钟超时全平台兜底
- **集群模式**：PostgreSQL 共享存储 + 持久化成员注册（心跳/stale 检测）+ 租约式分布式锁（过期自动可抢占）保证数据一致性；**在途变更故障接管**——执行节点崩溃后其运行中变更由执行租约（epoch 围栏）与 leader 接管循环收敛至 `interrupted` 终态（审计留痕、永不重跑副作用，`RetryChange` 显式重驱动）；**跨节点调度**——leader 将已批准 run 分派给空闲 worker 节点水平执行（run 级分派，loopback 通道已验证收敛到终态 + 审计证据）
- **合规交付**：`levee audit report` 一键生成时间窗合规报告（HTML 自包含：变更清单+审批链+哈希链验证结论+回滚记录），监管/审计离线可读
- **ChatOps 审批桥**：`internal/notify/chatopsbridge` 提供 approval 创建/决策到 BotManager 事件的组合接点，可将请求与 1/2 决策进度投影到钉钉/飞书/Slack；具体 bot 进程与 manager 生命周期仍由部署侧组合
- **安全默认**：auth 启动门禁、CORS 默认拒绝、限流、请求 ID 追踪、TLS 支持

### 能力可达性

上面的特性列表混着两种状态：**已接入 `levee` 二进制**，和**包已实现、包内测试完备、
但没有任何生产调用方**。这两件事对用户完全不同，所以单列。判定口径是可复现的：

```bash
# 二进制真实链入了哪些包（非测试构建）：
go list -deps ./cmd/levee | grep '^github.com/nexus/levee/internal'
# 某个包有没有生产调用方（去掉包自身的测试；结果为 0 即只有包内自测）：
grep -rl 'nexus/levee/internal/compat"' --include='*.go' . \
  | grep -v _test.go | grep -v '^./internal/compat/'
```

当前未链入 `cmd/levee` 的 `internal` 包（除 `docgen` 是 CI 工具外，共 8 个，约 4.7k 行非测试代码）：

| 包 | 它本来要承担什么 | 现状 |
| --- | --- | --- |
| `diagnosis/llm_diag` | LLM 推理定位 | 无生产调用方；`NewLLMClient` 仅测试调用 |
| `diagnosis/topology` | 拓扑诊断 | 无生产调用方 |
| `recommend/rag` | RAG 知识增强 | 无生产调用方 |
| `recommend/feedback` | 效果学习 | 无生产调用方 |
| `compat` | Ansible playbook 兼容层（MVP 交付项 D-08） | 全仓零引用，CLI 无对应命令；且其执行器自述**不强制审批与门禁**（`compat/executor.go:4-7`），动作表还映射了 4 个执行器不存在的动作（`compat.go:48-55`）——接线前必须先解决这两点 |
| `scheduler` | agent 任务派发（`Schedule([]agent.Task)` + 负载均衡），不是变更时间窗触发 | 无生产调用方；跨节点派发现在由已接入的 `internal/dispatch` 承担 |
| `opsmesh` | OpsMesh 平台集成 | 无生产调用方 |
| `notify/chatopsbridge` | ChatOps 审批桥接点 | 无生产调用方（上文那条已注明"由部署侧组合"） |

这些包**没坏**——包内测试覆盖实测在 90%~96%（90.4%~95.4%），问题是"覆盖"的是没人调用的代码。
把它们接进 `serve` 前，请按"库已就绪、产品不可用"对待；`docs/security-audit.md`
"已知限制"一节对多租户用的是同一套写法。

## 快速开始

```bash
# 构建
make build

# 运行
./levee --help

# 模板库初始是空的（`template list` 返回 "No templates found."），
# 所以先建模板；参数要用 --params 以 JSON 显式声明：
./levee template create --name nginx-reload \
  --content 'name: nginx-reload
steps:
  - name: reload-nginx
    action: shell
    command: systemctl reload {{.service}}' \
  --params '[{"name":"service","type":"string","required":true}]'

# 从模板实例化一个变更（产出 draft，等计划与审批）
./levee new nginx-reload --params service=nginx

# 查看变更
./levee list
./levee show <run-id>
```

以上四条在本仓库实测通过：`template create` 与 `new` 均 exit 0，`new` 输出
`params: map[service:nginx]`、`status: draft`，`list` / `show` 能看到并展开那条 run。

**`template create` 不会解析 `--content` 里的 `params:` 块**——参数必须经 `--params`
以 JSON 数组显式传入。实测拿仓库自带的 `examples/templates/patch-rolling.yaml`（它在
content 里声明了 `params: [package, target_group]`）建模板，`template show` 显示
`Parameters: (none)`，随后 `new patch-rolling --params package=nginx` 报
`unknown parameter: package`（实例化按记录里的 `tmpl.Parameters` 校验，
`internal/template/instantiate.go:120-127`）。所以照抄该示例文件建模板时，
参数要另写一遍 `--params '[{"name":"package","type":"string","required":true}]'`。

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
# 关闭时两个入口行为不同，别按同一个心智模型写脚本：
#   · ApplyChange RPC 明确拒绝（FailedPrecondition "no engine wired"），不碰状态机；
#   · CLI `levee apply` 不拒绝——它把 run 置为 running 后 exit 0，并打印
#     "apply is status-only (no batches executed)"、JSON 里带 engine_wired:false。
#     所以 CLI 的 exit 0 只代表"没有报错"，不代表"执行过任何批次"。
# 并发执行上限 --engine-max-parallel-runs（默认 4，超出的 apply 快速失败）；
# slo 验证门禁需 --engine-gate-prometheus 提供 Prometheus 地址，缺省则 slo 门禁 fail-closed。
# 目标凭据解析依赖 LEVEE_MASTER_PASSWORD 环境变量（未设置时通道匿名拨号并输出警告）。
./levee serve --token <secret> --engine-enabled
```

无 token 且未传 `--insecure` 时服务拒绝启动；无 TLS 时输出明文传输警告。

**提交审批需要能识别到人的凭据**：`approve` / `reject` 的投票人取自已认证主体，不接受
请求里自报的名字——共享 `--token` 只证明"部署内部有人"，不足以支撑一条审批记录。需要
审批请配置命名令牌（`server.auth_tokens`，每个令牌绑定一个身份）或 SSO / OIDC；
`--insecure` 开发模式与 CLI 本地模式（进程内即权威）仍按当前操作者记录。

**治理动作会按策略准入**：配了权限矩阵（`permissions.yaml`，用 `levee team add` /
`rbac grant` 维护）的部署，`apply` / `rollback` / `approve` / `reject` 还会再问一次
策略——矩阵决定"这个队能在哪些环境活动"，角色树（`roles.yaml`，`levee user add`
里的 role）决定"在这些环境里能做什么"。没有矩阵的部署不受影响（启动会 WARN 明示
未启用）。排查一次拒绝用 `levee authz explain --subject <你> --env <环境> --action apply`，
它会打印解析到的队/角色、命中的判定轴与拒绝原因。

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
│   ├── scheduler/          # agent 任务派发（未接入二进制，见"能力可达性"）
│   ├── agent/              # agent
│   ├── alert/              # 告警网关 (Zabbix/Nagios 适配)
│   ├── diagnosis/          # 诊断引擎 (拓扑分析/LLM 推理)
│   ├── recommend/          # AI 推荐 (RAG/反馈学习) — 产出 LEVEELang 草稿
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
| [docs/product-positioning.md](docs/product-positioning.md) | 产品定位（它是什么 / 不是什么 / 边界） |
| [docs/ui-blueprint.md](docs/ui-blueprint.md) | UI 蓝图（导航、线框、关键交互） |
| [docs/product-roadmap.md](docs/product-roadmap.md) | 产品路线图（代码 / 可视化 / 交互 / 运营四层） |
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
