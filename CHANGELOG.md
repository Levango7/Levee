# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

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

## [Unreleased]

### Security

- [SA-004] SecureZero 添加 runtime.KeepAlive 防止编译器优化
- [SA-005] argon2id memory cost 提升至 194MiB（OWASP 2024 推荐）
- [SA-006] 权限矩阵添加 sync.RWMutex 保证线程安全
- [SA-007] 权限校验拒绝时自动记录审计 trace
- [SA-008] 哈希链排序添加二级排序键确保确定性