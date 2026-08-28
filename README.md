# LEVEE

> LEVEE = Lifecycle Enforcement & Verification Engine
> 非云原生基础设施的变更流水线引擎，与 ArgoCD/Flux 分层互补

## 定位

把"改一台主机/数据库/网络设备/中间件"从手工命令和线性 playbook，升级为
计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚 -> 审计留痕 的完整闭环，
默认无代理、CLI 优先。

与 ArgoCD/Flux 的边界：ArgoCD/Flux 管 K8s 集群内（云原生层），
LEVEE 管 K8s 集群外（非云原生基础设施层），各管一摊。

## 核心特性

- **变更流水线**：计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚，全程审计哈希链留痕（WORM）
- **双协议 API**：gRPC（5+ 服务）+ REST 网关（`/api/v1/`），共享同一服务实例
- **AI 辅助运维**：告警接入 → 拓扑诊断 → LLM 对话式定位 → RAG 知识增强推荐 → 自动执行 → 效果学习
- **多通道执行**：SSH / WinRM 无代理通道，插件系统可扩展
- **资源沙箱**：Linux 下 cgroup v2 硬限制插件内存/CPU；墙钟超时全平台兜底
- **集群模式**：PostgreSQL 共享存储 + 持久化成员注册（心跳/stale 检测）+ 租约式分布式锁（过期自动可抢占）保证数据一致性（在途变更自动故障转移/跨节点调度尚在开发中，见 `levee serve --cluster` 启动告警）
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

- Go 1.25+（静态编译，单二进制）
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

当前版本 v1.12.0。MVP (3 个月) -> V1 (6 个月) -> V2 (12 个月)

详见 [docs/mvp-tasks.md](docs/mvp-tasks.md) 与 [CHANGELOG.md](CHANGELOG.md)。
