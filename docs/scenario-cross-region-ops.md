# 场景方案：中大型集团跨区域数据同步与运维的变更治理

| 元信息项 | 内容 |
| --- | --- |
| 适用场景 | 1 个核心物理机房 + 4~5 个区域数据中心；跨区域 MySQL 主从链路与批量主机/中间件运维 |
| 交付形态 | 裸金属二进制 / systemd（主线）或 Helm（客户有 K8s 时），见 [deploy/](../deploy/README.md) |
| 配套示例 | [examples/workflows/dr-switch-orders-db.yaml](../examples/workflows/dr-switch-orders-db.yaml)（CI 强制编译） |
| 竞争定位 | 不与数据同步产品（DTS/OGG/自研脚本）正面竞争，卖**跨区域变更的治理与可审计层** |

## 1. 场景画像

- **拓扑**：集团 IT 集中管控 + 区域运维执行；核心机房跑主控与数据库主库，区域机房有副本、批量主机、中间件。
- **高频变更**：DB 主从切换（故障/演练/维护窗口）、批量基线加固、中间件重启、跨区域发布与回滚。
- **合规压力**：等保/内审/监管要求「谁、在什么窗口、对哪些目标、做了什么、结果如何」全链路可举证。
- **现状**：切换靠操作手册 + 人盯屏幕；同步/加固是散落脚本；审批在群里口头确认；出事翻聊天记录。

## 2. 痛点 → LEVEE 能力映射（全部为已接线能力，非路线图）

| 痛点 | LEVEE 能力 |
| --- | --- |
| 主库切换不可逆，一步错就是事故 | `mysql.replica_switch` 运行时**强制 `confirm=yes` 确认门**（模块内硬校验，拒绝无确认的切换）；计划侧对 `irreversible: true` 步骤自动标红并路由 high/emergency 审批 |
| 多副本逐个切换，怕并发错峰 | `batches.strategy: one-per-target`：严格每批一台、串行推进（规范为「DB 主库逐个切换」而写） |
| 各区域维护窗口不同 | `window` 按 `HH:MM` + **IANA 时区**判定，跨零点支持（如 23:00→02:00）；窗外 **plan 阶段直接阻断**（LE020/LE021），不依赖执行期自觉 |
| 同步/加固脚本一次性、无门禁 | 步骤化（`shell.exec` / `mysql.query` / `mysql.pt_osc` / `file.copy`…）+ 四类门禁（cmd / slo / probe / human）：切换后核对复制状态、SLO 门禁可查 Prometheus 的复制延迟 |
| 审批靠口头 | 审批分级（standard/high/emergency）+ 最少人数 + 排除发起人 + 按级别配额；投票**绑定认证主体**；IM/deeplink 一键审批 |
| 回滚不干净却显示成功 | 执行证据账本只补偿**确实改过**的步骤；`rolled_back_partial` / `rollback_incomplete` 不冒充干净回滚；snapshot 与 run 级基线可还原文件 |
| 内审要链路证据 | `audit` 表 WORM 触发器 + 全局哈希链（写入即封链）+ trace 按 run 封链；`levee audit report` 输出**自包含 HTML**，离线可读可归档 |
| 执行机崩溃后变更卡死 | 集群执行租约 + epoch 围栏 + 接管循环：崩溃节点的运行中变更收敛到 `interrupted` 终态（审计留痕、**不重跑副作用**），`RetryChange` 显式重驱动 |
| 跨区域集中执行的带宽/延迟 | 集群 worker **就近执行**：各区域一台 worker，leader 统一派发（容量/回收/claim 超时均已接线） |
| 谁在暗改配置 | `levee drift` 基线 + 巡检；审批与执行全程入审计链 |

## 3. 参考部署形态

```
        核心机房                          区域 DC ×4~5
  ┌───────────────────┐         ┌──────────────────────────┐
  │ levee master      │◄─ PG ──►│ levee worker（每区一台）   │
  │ (API/UI/调度/接管) │         │  就近执行、本区 SSH 到目标机 │
  │ PostgreSQL        │         └──────────────────────────┘
  │ (客户既有 / RDS)   │               │
  └───────────────────┘               ▼
     ▲ CLI / 浏览器               目标机（SSH，无需装 agent）
```

- 装法：核心机房与区域各机 `deploy/baremetal/install.sh` + systemd（主线）；有 K8s 的客户用 `deploy/helm/levee`。
- 网络面：CLI/Web → master `:8080`（gRPC `:9090` 不对外，见 [deploy/README.md](../deploy/README.md) 安全约束）；worker → master 只需可达 PostgreSQL。
- 认证：**命名令牌或 OIDC**（单令牌模式无法在审计里证明「谁做的」）。

## 4. 一条真实变更长什么样

[examples/workflows/dr-switch-orders-db.yaml](../examples/workflows/dr-switch-orders-db.yaml)（CI 强制编译的现行方言示例）节选：

```yaml
target:   { type: mysql, query: "role=replica AND cluster=orders-db", min_count: 1, max_count: 8 }
window:   { start: "01:00", end: "05:00", timezone: Asia/Shanghai }
batches:  { strategy: one-per-target }     # 严格逐台串行
approval: { level: high }                  # 两人、排除发起人
steps:
  - name: precheck-replication             # 切换前：IO/SQL 线程与延迟
    action: shell.exec
    args: { cmd: "mysql -e \"SHOW SLAVE STATUS\\G\" | grep ..." }
  - name: switch-primary
    action: mysql.replica_switch
    args: { new_primary: "{{.new_primary}}", confirm: "yes" }   # 强制确认门
    irreversible: true
    verify:                               # 切换后核对复制就绪才进下一台
      cmd: { run: "...Slave_IO_Running: Yes...", expect_exit: 0 }
rollback: { on_failure: manual, verify_after: true }   # 不可逆：失败不自动回滚，按手册决策
```

从 `levee new` 到审计报告的完整链路：计划（哈希锁定审批与门禁）→ 审批（跨团队）→ 逐台串行执行（窗内）→ 门禁 → 失败按策略收敛 → `levee audit report` 出证。

## 5. 试点与验收（对接 docs/deployment.md 交付检查单）

建议的试点范围（1~2 周）：一个区域、3~5 台测试机 + 一套测试 MySQL 主从。

| 演练 | 验收点 |
| --- | --- |
| 1. 一次 `replica_switch` 演练 | 无 `confirm=yes` 被拒；审批流完整；切换后 verify 通过；审计链 `levee audit verify` 通过 |
| 2. 一次批量重启（故意让一台失败） | 失败步骤触发策略（auto 回滚或 manual 挂起）；回滚账本如实区分「未执行」与「补偿缺口」 |
| 3. 一次窗外 plan | 被 LE020/LE021 拒绝（规范承诺行为） |
| 4. 杀掉 worker 进程 | 接管循环收敛到 `interrupted`；`RetryChange` 重驱动且不重跑已完成步骤 |
| 5. 导出审计报告给内审同事 | 自包含 HTML 离线可读，含审批链与哈希链校验结论 |

## 6. 错位竞争

- **卖治理层，不卖搬运工**：rsync / mysqldump / 原生复制 / 现有同步平台继续干活；LEVEE 把它们的执行纳入 审批 × 窗口 × 逐台串行 × 回滚 × 审计链。
- 对照：Ansible AWX/Tower 偏执行编排，**没有**审批门禁、哈希链审计、回滚补偿账本；大厂变更平台绑定自家云栈不外售；互联网内部平台不适配「无 K8s、要离线举证」的政企现场。
- ROI 计算框架（按客户实际填）：一次误切/误改的停机分钟数 × 每分钟业务损失 × 年发生概率，对比平台一次性实施 + 年度支持费用。合规审计的人工取证成本（每次数人日）是第二项可量化收益。

## 7. 诚实边界

- **数据同步的搬运本身不是 LEVEE 的能力**：它编排与治理执行（含逐台串行与门禁），同步工具与复制拓扑仍是客户既有资产。
- **`allow_irreversible` 白名单已接线（2026-10-04，v1.19.0）**：不可逆保护现为三层——编译期白名单门禁（V14/LE082：判定为不可逆的步骤未列名即拒绝编译，缺省拒绝）+ 模块内 `confirm=yes` 硬门（运行时拒执行）+ 计划侧高危审批路由。场景示例 `examples/workflows/dr-switch-orders-db.yaml` 已带白名单。
- 断网区域需网络可达（跳板/VPN）；无 agent 模式意味着目标机必须开放 SSH/WinRM。
- 首次生产接入前必须完成第 5 节的试点演练——本仓库的测试覆盖控制流与存储一致性，不能替代客户现场的网络与工具链现实。
