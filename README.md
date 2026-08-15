# LEVEE

> LEVEE = Lifecycle Enforcement & Verification Engine
> 非云原生基础设施的变更流水线引擎，与 ArgoCD/Flux 分层互补

## 定位

把"改一台主机/数据库/网络设备/中间件"从手工命令和线性 playbook，升级为
计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚 -> 审计留痕 的完整闭环，
默认无代理、CLI 优先。

与 ArgoCD/Flux 的边界：ArgoCD/Flux 管 K8s 集群内（云原生层），
LEVEE 管 K8s 集群外（非云原生基础设施层），各管一摊。

## 快速开始

`ash
# 构建
make build

# 运行
./levee --help

# 创建变更
./levee new --file examples/workflows/batch-update.yaml

# 查看变更
./levee list
./levee show <change-id>
`

## 项目结构

`
levee/
├── cmd/levee/              # CLI 入口
├── internal/
│   ├── engine/             # 工作流引擎
│   ├── executor/           # 模块执行器
│   ├── channel/            # 通道抽象层 (SSH/WinRM)
│   ├── approval/           # 审批服务
│   ├── audit/              # 审计哈希链
│   ├── state/              # SQLite 存储层
│   ├── plan/               # plan 生成与哈希锁定
│   ├── rollback/           # 回滚协议
│   ├── verify/             # 验证门禁
│   ├── batch/              # 批次控制
│   ├── lock/               # 互斥锁
│   ├── credential/         # 凭据管理
│   ├── template/           # 模板库
│   ├── compat/             # playbook 兼容层
│   ├── dsl/                # YAML 子集解析
│   ├── notify/             # 通知
│   ├── config/             # 配置管理
│   ├── permission/         # 权限
│   └── pause/              # 全局暂停
├── configs/                # 配置文件示例
├── docs/                   # 设计文档
├── assets/                 # 架构图
├── examples/               # 示例工作流
├── tests/                  # 集成/E2E 测试
└── scripts/                # 脚本
`

## 文档

| 文档 | 说明 |
|---|---|
| [docs/levee-design.md](docs/levee-design.md) | 完整设计文档 |
| [docs/leveelang-spec.md](docs/leveelang-spec.md) | LEVEELang DSL 规范 |
| [docs/levee-api.md](docs/levee-api.md) | CLI 命令与 API 设计 |
| [docs/mvp-tasks.md](docs/mvp-tasks.md) | MVP 开发任务拆分 |

## 技术栈

- Go 1.22+ (静态编译，单二进制)
- SQLite (嵌入式，零依赖)
- SSH: golang.org/x/crypto/ssh
- WinRM: masterzen/winrm
- CLI: cobra

## 开发

`ash
make lint      # 静态检查
make test      # 单元测试
make build     # 构建
make cross-build  # 跨平台编译
`

## 版本

MVP (3 个月) -> V1 (6 个月) -> V2 (12 个月)

详见 [docs/mvp-tasks.md](docs/mvp-tasks.md)。
