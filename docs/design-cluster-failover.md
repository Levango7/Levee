# 设计提案（v0.2 已评审）：集群在途变更的故障接管（failover takeover）

状态：**评审通过（2026-09-07：Q1 新终态 interrupted / Q2 leader 独占 / Q3 断点续跑进 backlog / Q4 跨节点调度出局）**。
**前置依赖（评审后新增）**：生产考古发现 serve 未接线执行引擎（详见 design-engine-wiring.md）——"在途变更"在生产中尚不存在，故本设计的实现排在其前置 A（引擎接入）之后；fencing 恰好接在 A 的 EngineAdapter 执行循环上。A 过审合入前，本设计冻结不动工。

## 0. 今天的事实基线（代码考古结论）

- 执行模型：`ApplyChange` 经 CAS 把 run 置 `running` 后，**引擎在 gRPC 进程内同步执行**（`EngineAdapter.Run`）。`Run` 模型没有 owner 节点、epoch、租约任何字段——执行在数据库层面是"无主"的。
- 集群原语已就绪且 CI 全绿：节点注册/心跳（`pg_registry` + `healthCheckLoop`）、leader 选举（`GetLeader`）、分布式租约锁（`DistributedLockManager`，UNIQUE→`ErrLockHeld` 归一）、多节点并发 DDL 已用 advisory lock 串行化。
- 断点：节点在 apply 中途死亡时，run 状态**永远停在 `running`**——没有回收者、没有接管者、没有告警。这是状态机级死区，比"不能接管"更糟：连一个诚实的终态都给不出。
- 步骤级持久化已存在：`Batch`/`Step` 逐主机逐步骤落库（status/started/completed），这给"跳过已完成步骤"的收敛语义提供了数据基础。
- 约束：回滚快照落**执行节点本地盘**（`internal/rollback/snapshot.go`），跨节点不可见。

## 1. 收益（明确、可验收）

1. **消除最大的宣称差距**。集群模式是 2026-09 评估认定的头号短板，README 至今挂告警。达标即有依据删警告，直接改善 GA 定性。
2. **把一类自身故障从"事故"降为"有界延迟"**：现状下节点崩溃 → run 永久卡 `running`，只能人工改库；接管后同一场景在 ≤2×租约周期内收敛到终态 + 告警 + 完整审计链。
3. **边际成本低**（相对收益）：CAS 状态机、租约锁、步骤检查点、确定性测试范式全部已存在；净新增是一个"接管循环 + 执行登记/围栏"，不是调度器重写。
4. **试点前置资格**：真实场景试点必然问"你节点挂了在跑的变更怎么办"，届时答案不能是"人工救库"。

## 2. 风险与缓解（按危害排序）

| # | 风险 | 固有危害 | 缓解 | 缓解后残余 |
|---|------|---------|------|-----------|
| R1 | **双执行**：旧节点没死透（网络分区/GC 停顿），新接管者与它并发推进同一 run，重复副作用打到生产主机 | 高 | 三层：① 接管须同时满足注册租约过期 **且** 抢到接管锁；② 执行登记带单调 `epoch`，执行者**每一次**步骤/状态写入都 CAS 校验 epoch，不符即自杀（fencing token）；③ 接管者**永不重跑**非终态步骤——`completed` 跳过、`running/pending` 一律记 `unknown` 后整 run 收敛为中断终态 | 旧执行者在租约过期前的窗口内仍可写（它认为自己健康）→ 用 epoch 单调性把它的写入变成硬失败；最坏情形是"旧节点多做了一步远端副作用"，这与单节点崩溃时的既有暴露相同，无**新增**面 |
| R2 | 接管后无法回滚（快照在死者本地盘） | 中 | 本设计**不尝试跨节点回滚**：接管只做状态收敛+告警，回滚留给人工（`RetryChange`/`RollbackChange` 既有语义走干净状态）。快照落 PG 列入后续独立议题 | 低 |
| R3 | 状态语义漂移：新终态侵入状态机/前端/审计 | 中 | 新终态只在集群模式产生；单节点（SQLite）路径一行代码不变（接管循环不启动）；前端 label/色表与状态机表同步扩展有既有惯例可循 | 低 |
| R4 | **测试不确定性**——刚花一天清剿 flaky，不能再引入新的 | 中 | 只用确定性范式：通道化启动同步、进程级 kill 模拟（exec 子进程而非线程）、`Eventually` 只等视图收敛、赌表用 PG 真实并发；接管循环暴露一次性 `TakeoverOnce()` 钩子供测试驱动，不靠真实时钟 | 低 |
| R5 | 范围蠕变：滑向分布式调度器/快照共享/断点续跑大重写 | 中 | 硬边界写进本文档：本期**只**做"租约登记 + fencing + 中断收敛 + 告警"。跨节点调度、快照入 PG、可恢复续跑明确进 backlog 不做 | 低 |
| R6 | schema 迁移风险 | 低 | 新表 `run_execution`（独立表零 ALTER）+ 既有 advisory-lock 迁移路径 + 空表回归；回滚兼容（旧代码无视新表） | 低 |

**总判断**：核心风险 R1 的本质是"死没死透"，fencing token 是分布式系统的标准答案且有 PG 单调性背书；本设计通过"接管者永不重跑副作用"把最坏情形压回**现状同等暴露**——即风险不是被赌掉，而是被压平。收益侧则全是净增。**过关。**

## 3. 设计骨架（最小接管语义，~4-6 提交）

1. **新表 `run_execution`**：`run_id PK, owner_node, epoch BIGINT, lease_expires_at, heartbeat_at`。SQLite 不建（单节点不启用）。
2. **执行登记**：apply 的 CAS 成功后同事务登记 epoch（`INSERT ... ON CONFLICT` + 单调取新）；执行循环每 TTL/3 续约；每个步骤落库都是 `UPDATE ... WHERE run_id=? AND owner_epoch=?` 的 CAS——`0 行受影响`即失主，执行者立即中止剩余步骤（fail-closed）。
3. **接管循环**：leader 独占，周期扫描 `lease_expires_at < now()` 且 `run.status IN (running)` → 事务内：run `running → interrupted`（新终态），未完成步骤标 `unknown`，写 trace/审计行，删 `run_execution` 行。全程持接管用分布式锁（复用 `DistributedLockManager`）。
4. **中断语义**：`interrupted` 不可自动越过；人工经 `RetryChange` 从干净状态重驱动（既有语义，零新面）。旧节点复活后 epoch 已废，所有写入被 fencing 拒绝，零污染。
5. **观测**：`levee_takeover_events_total` 计数 + 中断 run 走既有告警链。
6. **配置**：`--cluster-takeover-interval`（默认 10s）、租约 TTL（默认 30s，注册心跳复用同一常量）；仅集群模式生效。

## 4. 验收门槛（全过才删 README 集群告警）

1. 双节点 + PG e2e：executor `kill -9` 后 ≤2×TTL run 收敛 `interrupted`，审计哈希链完整。
2. 僵尸复活：分区恢复的旧执行者后续所有写入断言为 0 行生效。
3. 无双写：接管窗口内同一步骤行不产生重复状态转移。
4. 单节点全量既有测试零改动全绿。
5. 新测试全部确定性（无时钟赌博、无 sleep 赌调度）。
6. CI 时长增幅 ≤ ~3 分钟（挂现有 integration&postgres job，不新增 OS 腿）。

## 5. 待拍板（评审问题）

- **Q1 终态命名**：新增 `interrupted` 终态（推荐，审计语义清晰、与"执行失败"区分）vs 直接复用 `failed`+reason 字段？
- **Q2 接管者**：leader 独占（推荐，单写入者无竞争）vs 任意抢到接管锁的节点？
- **Q3 断点续跑**（跳过 completed、自动重跑 pending 的 resumable 语义）：本期**不做**（推荐——需要 executor 逐类型幂等声明，是独立项目）；若要进本期，工期约翻倍。
- **Q4 范围确认**：跨节点调度（dispatcher）确认不在本期——接管≠调度。

## 6. 成本

实现+测试+核验约 4-6 个提交；主要工时在确定性双节点测试的打磨（参考：本轮 KMS/沙箱教训，测试质量是生命线）。工具链零新增。
