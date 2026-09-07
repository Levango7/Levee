# 设计提案：A —— 执行引擎接入 serve（前置步骤）

状态：**已过审并实施（A1–A5，2026-09-07）**。落定口径：Q1 `--engine-enabled` 默认关闭；Q2 无持久化计划的 run 直接拒绝 apply（引导重新 plan）；Q3 CLI apply 与 serve 共用同一进程内 ChangeService 路径（并新增 `levee plan` 命令补足 CLI 侧唯一的计划持久化入口）。本设计是已批准的故障接管设计（design-cluster-failover.md，下称 B）的**前置**：没有 A，B 在生产上保护的对象（"在途变更"）根本不存在。

## 0. 事实基线（2026-09-07 代码考古）

- **没有任何生产入口驱动 `engine.ClosureRunner`**。`NewClosureRunner` 的全部调用方是单元测试与 `tests/e2e/rollback_drill_test.go`；生产侧 serve 的 `buildServeServices` 明确传 nil engine（apply → `FailedPrecondition`），CLI apply 是 status-only。
- 引擎本体质量良好：完整生命周期编排（锁→预门禁→批次执行→验证→回滚→后验证→锁释放），单测+e2e 演练覆盖；"生产级组装配方"在 e2e 演练里现成可见（lock/gate/rollback/batch 四件套 + per-run runner）。
- 执行器模块齐备：`executor.Executor` 分发器 + file/pkg/shell/svc/user 模块，全部经 `channel.Channel` 触达目标（含 local 通道）。**缺的只是 `plan.PlanStep → 模块执行` 的 execFn 生产适配器**（目前只有测试里的假适配器）。
- **计划不持久化**：`PlanChange` 现算现返；`Run.PlanHash` 列存在但无人写入。apply 执行的必须是"被批准的那份计划"——否则审批语义形同虚设。
- 引擎并发约束：`ClosureRunner` 单实例不支持并发计划 → 按 run 构造（组装廉价）。
- serve 已有凭据面：`LEVEE_MASTER_PASSWORD` → CredentialStore（目标探测在用），执行侧可复用。

## 1. 收益

1. **产品第一次真正"能跑"**：serve 模式从"status-only 演示"变成真的执行变更；这是所有后续价值（B、试点、GA）的总闸。
2. **审计/审批闭环成立**：计划持久化 + plan_hash 绑定后，"批的就是跑的、跑的就是留痕的"才成立。
3. **解锁 B 的端到端验收**：接管测试可用真实引擎跑真实 run，而不是靠假引擎喂状态。
4. 引擎与执行器模块此前已有测试覆盖，A 的净新增是"组装+适配"而非新执行语义——**风险集中在装配层，可整体关停**。

## 2. 风险与缓解

| # | 风险 | 固有 | 缓解 | 残余 |
|---|------|------|------|------|
| R1 | **生产首次真触达远端主机**：从"不会执行"变为"会执行"，本身就是最大行为变化 | 高 | 总开关 `--engine-enabled`（默认 **false**，维持现状 FailedPrecondition）；试点按需显式开启。灰度权在部署者手里 | 低 |
| R2 | 执行到的计划≠批准的计划（计划现算现执行的既有语义） | 高 | **计划持久化**：PlanChange 序列化计划入 run（新列 `plan_json`，SQLite/PG 双轨），审批只允许带已存计划的 run；apply 读存量计划并校验 `plan_hash`，参数漂移即拒（FailedPrecondition） | 低 |
| R3 | 凭据/连接在请求路径上的耗时与泄漏（KMS 教训：argon2id 单次可达秒级） | 中 | 连接与凭据解析按 run 作用域缓存、run 结束统一关闭；执行超时沿用步骤级超时；凭据不落步骤 stdout（审计脱敏已有） | 低 |
| R4 | serve 并发执行失控 | 中 | 每 run 独立 runner（绕开单实例约束）+ 服务端并发闸 `--engine-max-parallel-runs`（默认 4），超出即 FailedPrecondition 快速失败；目标级互斥仍由引擎锁保证 | 低 |
| R5 | 参数化门禁（command/probe remote/slo）在 serve 缺运行时依赖 | 中 | 门禁运行时按需组装：command/probe 复用执行器同一拨号器；slo 需配置 Prometheus 地址，缺配置时门禁**fail-closed**（既有语义），部署者可选择性开启 | 低 |
| R6 | 快照落盘位置失控 | 低 | 快照根目录走 serve 配置（数据目录之下），沿用快照存储既有的目录对账与拒绝穿越逻辑 | 低 |
| R7 | 范围蠕变（顺手重写执行器/通道层） | 中 | 硬边界：A 只做 持久化 + 组装 + 适配 + 开关 + 并发闸；通道/模块/引擎内核零改动 | 低 |

**总判断**：A 的本质是把已测试的部件接起来，行为变化被"默认关"完全兜住；真正的语义新增只有一条（计划持久化+hash 绑定），且有独立验收项。**过关。**

## 3. 设计骨架（约 5-7 提交，串行可做）

1. **A1 计划持久化**：`run.plan_json`（迁移含 advisory-lock 路径）；PlanChange 写入 + 置 `plan_hash`；审批/apply 消费存量计划；hash 不符拒执行；旧 run（无存量计划）apply 时明确报错引导重新 plan。
2. **A2 组装层**：serve 启动时按 `--engine-enabled` 构造 lockMgr/gateMgr(+runtime)/rollbackMgr(快照目录)/batchCtrl 工厂（每 run 独立实例）；关闭时维持现状一行不变。
3. **A3 execFn 适配器**：`plan.PlanStep` → 目标解析（inventory/target）→ 凭据解析（master password）→ 通道拨号（run 级连接缓存，统一 Close）→ `executor.Executor` 分发 → 步骤输出映射进引擎回调。
4. **A4 EngineAdapter.Run 闭包**：读 run+plan → 并发闸 → NewClosureRunner → Run(execFn) → phase/success 映射（既有映射表）；RetryChange/Rollback 同步接线（Rollback 复用 rollbackMgr）。
5. **A5 配置面**：`--engine-enabled` / `--engine-max-parallel-runs` / 快照目录 / Prometheus 地址（slo 门禁）；文档与 README 状态如实更新（**告警删除留到 B 验收**）。

## 4. 验收门槛

1. `--engine-enabled=false`（默认）：全量既有行为与测试零变化。
2. 开启后 gRPC e2e：plan→approve→apply 走本地通道打到本机的真实步骤 → run `completed`、步骤行落库、审计链完整。
3. 计划漂移（改参后旧计划 apply）被 plan_hash 拒。
4. 并发闸生效：第 N+1 个并发 apply 快速 FailedPrecondition。
5. 回滚路径经 serve 触发可达 `rolled_back`（复用 rollback drill 场景上移到 serve 层）。
6. 双平台/全量测试 + lint/gosec 全绿；无新增 flaky。

## 5. 待拍板

- **Q1 开关默认值**：默认关（推荐，风险优先）vs 默认开（激进，不推荐）。
- **Q2 旧计划语义**：无存量计划的 run apply 直接拒（推荐）vs 现场重算（保留旧行为，审批语义破口）。
- **Q3 CLI apply**：本期顺手接真引擎（CLI 与 serve 共用组装工厂，边际成本低，推荐）vs 只接 serve。

## 6. 与 B 的关系

A 合入并全绿后回到 B（设计已过审，四问已拍板）：B 的 fencing 恰好补在 A4 的执行循环上（apply 登记 epoch → 引擎循环续约 → 写库校验）。两期合起来 README 集群告警才有依据删除。
