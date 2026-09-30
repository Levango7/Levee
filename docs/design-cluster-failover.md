# 设计提案：集群在途变更的故障接管（failover takeover）

状态：**已实施（B1–B5，2026-09-08）**。落定口径：新终态 `interrupted`；leader 独占接管；断点续跑进 backlog（本期只做中断收敛）；跨节点调度出局。实施拍板见 §7.5（Q1 interrupted 可 Retry 重驱动 / Q2 断水模拟定案 / Q3 手动 rollback 在围栏外）。验收状态：B1 终态 CAS 化、B2 围栏+哨兵免回滚、B3 接管循环、B4 `interrupted` 全触点、B5 双节点 e2e（收敛/僵尸零污染/无双写）全部落地；CI integration&postgres 腿执行真 PG 验收。README 集群告警已按 §4 验收达成删除。
**前置依赖**：执行引擎接入（design-engine-wiring.md，设计 A）已合入——"在途变更"在生产中真实存在，fencing 接在 A 的 EngineAdapter 执行循环上。

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

## 7. 实施方案（v0.4，前置 A 已合入，待拍板）

A 合入后的代码事实（2026-09-07 复核，比 §0 更精确的四点）：

- **fencing 原语已存在但尚未被消费**：`cluster_locks.fence_token` 是单调序列
  （`cluster_locks_fence_seq`），`DistributedLockManager.Acquire` 每次易主都发新
  token；`clusterNodes` 心跳/摘除、`GetLeader`（收敛式选举）均可直接复用。
- **证据持久化是"闭包完成后一次落库"**（`wiring.executePlan`：`runner.Run` 全程
  进程内执行，`persistClosureResults` 在 Run 返回后一次性写 batch/step 行）。
  因此 §2-R1 缓解②"每一次步骤写入 CAS"在实施中收敛为**结构性写入点**的
  epoch 校验（详见 7.2-B3）。
- **⚠ 闭包的失败语义与"中止"耦合了"回滚"**（本次细化新发现，v0.3 未识别）：
  `closure.Run` 进入批次执行后，**任何**失败路径——步骤错误、批后门禁失败、
  ctx 取消——都置 `triggerRollback=true`，且回滚刻意用 `context.Background()`
  跑到完成（closure.go:394-412 的既有设计）。含义：**围栏失主信号绝不能走
  ctx 取消或普通错误传播**，否则失主僵尸会触发一次不可取消的自动回滚，
  带着 undo 命令扑向生产主机——这正是 R1 要防的远端双写，且以更危险的
  "回滚"形态出现。解法见 7.2-B2 的引擎哨兵。
- **A 遗留终态盲写洞**：`ApplyChange` 错误路径、`retryChange` 终态、
  `rollbackChange` 终态都是 `GetRun`+`UpdateRun` 盲写。接管把 run 置
  `interrupted` 后，僵尸复活的第一件事就是把终态盖回去。fencing 不做
  终态 CAS 化等于没做（7.2-B1）。

### 7.1 分层与依赖方向（硬约束）

- `internal/cluster`：只加**原语**——`run_execution` 表（并入 `clusterSchemaSQL`
  的 advisory-lock 串行建表路径，共享 770_001 锁键，**不进** pgschema.sql：
  非集群 PG 库不需要，少一处镜像就少一处漂移）+ `ExecutionGuard`（登记/
  续约/`Owns` 复验/候选扫描）。epoch 单调性复用 `cluster_locks_fence_seq`
  的 nextval 模式（新序列 `run_execution_epoch_seq`，INSERT…ON CONFLICT
  DO UPDATE 与 acquireLockSQL 同形——已被证明的 SQL 形态）。零新依赖。
- `internal/engine`：**唯一**改动是围栏哨兵 `ErrFencedOut` + 批次失败分支
  识别哨兵后**跳过回滚**直接失败（~10 行 + 文档注释）。闭包的其余执行
  语义一行不动。回滚路径内的失主由 execFn 的 Owns 门禁自然失败化 undo
  步骤（不派发、不重试派发），无需引擎改动。
- `internal/takeover`（新包）：**语义层**——依赖 `state.Store` + `cluster`
  原语。leader 独占 + 每 run 接管锁 + `running→interrupted` 状态 CAS
  三重幂等。不 import wiring/engine。
- `internal/wiring`：定义 `ExecutionGuard`/`ExecutionLease` 接口
  （nil=单节点，行为零变化）+ 接线。依赖方向无环。

### 7.2 提交分解（B1–B5，每个提交独立成立、独立全绿）

| 提交 | 内容 | 关键测试 |
|------|------|---------|
| **B1** | **终态写入 CAS 化**（A 遗留洞，先行独立修复）：`ApplyChange` 成功/错误路径、`retryChange` 终态改 `UpdateRunStatusIf(running→final)`；CAS 失败=已被并发转移，如实返回当前状态、不写不发事件。`rollbackChange` 保持现状（非 running 源状态，属 Q3 范围）。单节点语义不变（期望值恒 running，无人竞争） | gRPC 层：并发 apply/pause 竞争败者不脏写；wiring 层 retry CAS 竞争败者错误明确；全量既有套件零回归 |
| **B2** | **围栏与登记**：`run_execution` DDL+序列；`cluster.ExecutionGuard`：`Begin`（INSERT…ON CONFLICT，易主必发新 epoch，RETURNING epoch）、`Owns`（**续约式复验**——见下）、`Heartbeat`、`End`（仍持有时删行，僵尸 End 为 0 行无副作用）、候选扫描（`lease_expires<NOW() ∪ status=running` 且 `run_execution` 无行且 `updated_at` 老于孤儿宽限 5min 常量）。引擎哨兵 `ErrFencedOut`+免回滚分支。`wiring`：`WithExecutionGuard`、`runChange/retryChange` 头部 Begin（**失败即拒**——集群模式下无围栏的执行就是接管盲区，run 落 failed 可重试）、每步派发前 `Owns` 门禁、心跳 goroutine（TTL/3，独立于步骤进度——慢步骤期间健康节点不掉租约；TTL 语义=死后检测延迟，不是执行时长上限）、`defer End`、落库前 `Owns` 复验 | guard 单测（PG 门控）：epoch 单调、旧主重登记必废、行删即失主、续约式 Owns 延长租约；引擎单测：哨兵失败→PhaseFailed+零回滚；wiring 单测（假 guard）：Begin 失败拒绝执行、Owns=false 跳落库+失主错误、End 幂等 |
| **B3** | **接管循环** `internal/takeover`：leader-only（收敛选举视图）+ 每 run 接管锁 + 事务内 `UpdateRunStatusIf(running→interrupted)` + 防御性 batch/step 标记（按构造 0 行命中——行只以终态落库，语句为未来增量持久化留形并如实注明）+ `CreateTrace("interrupted_by_takeover", actor="cluster-takeover")` + 删执行行；`TakeoverOnce()` 测试钩子；`levee_takeover_events_total{result}` 计数。**ci.yml** 的 integration&postgres 作业包清单加 `./internal/takeover/...`。serve 旗标：`--cluster-takeover-interval`（默认 10s，≤0 禁循环但**不禁**登记/围栏）、`--cluster-exec-lease-ttl`（默认 30s） | takeover 单测（PG 门控）：过期→interrupted+trace+行删+计数；重扫幂等；非 leader 零动作；孤儿宽限内不误伤；活节点但执行租约过期同样接管（执行存活≠节点存活，租约是唯一权威） |
| **B4** | **`interrupted` 全触点**：`isValidTransition`（archived 合法前态+、cancelled 拒绝）、`RetryChange` 准入（见 Q1）、WatchChange `terminalStates`、状态机文档注释、`metrics.changeStatuses` 增标签、前端 label/色表 + vitest 用例 + dist 重建 | gRPC 状态机表测试扩展；前端三连 |
| **B5** | **双节点 e2e + 文档收口**：tests/integration（PG）三用例（见 7.4）；CHANGELOG；本设计稿状态置已实施；README 集群段改写 + 删告警（以 §4 全过为前置） | 7.4 全部 |

### 7.3 关键机制的因果链（细化版，供实现时对照）

1. **健康长执行不被误杀**：心跳 goroutine 以 TTL/3 独立续约，与步骤进度
   解耦——一个跑 10 分钟的慢 SSH 步骤期间租约始终新鲜。TTL 的语义因此
   是"**死后检测延迟上界**"，不是执行时长上限。默认 TTL=30s ⇒ 崩溃后最迟
   ~TTL+interval≈40s 收敛，满足 §4-1 的 ≤2×TTL。
2. **续约式 Owns 关闭落库 TOCTOU**：`Owns` 的 UPDATE 在校验的同时延长租约
   ——Owns 通过 ⇒ 租约在随后 TTL 内不可能过期 ⇒ 接管循环不可能在校验与
   落库之间插入。若 Owns 只读不续约，"校验通过→接管→落库"的毫秒窗口
   就会漏出迟到证据行。这个"为什么必须续约"的因果写进 Owns 的文档注释。
3. **失主僵尸的三道门，全部不走 ctx**：① 每步派发前 Owns（远端门）；
   ② 落库前 Owns（证据门）；③ 终态 CAS（状态门，B1）。失主信号=哨兵
   错误，引擎免回滚分支消化之——**绝不取消 ctx**（7 开头发现的耦合）。
   GC 停顿僵尸醒来后的首个 Owns 必失败，后续零派发、零落库、零状态写。
4. **孤儿扫描的可达性论证**：B2 起集群模式所有 apply/retry 都 Begin 登记
   且 Begin 失败即拒——因此"running 无执行行"只可能来自 B 之前的历史崩溃
   （一次性存量）或 CAS-running 与 Begin 之间的微秒级崩溃窗口；5min 宽限
   常量覆盖两者且不可能误伤健康执行（健康执行必有行）。
5. **手动 RollbackChange 明确在围栏外**（Q3 推荐口径）：不经 running、不
   登记；节点死在手动回滚中途时 run 停在其非 running 源终态，接管循环按
   构造跳过，人工可重发。README 如实注明。

### 7.4 验收映射（§4 → 具体测试，全部挂现有 integration&postgres 作业）

| §4 条款 | 测试 |
|---------|------|
| 1. 崩溃 ≤2×TTL 收敛 + 审计链完整 | `TestTakeover_TwoNodeConvergence`：双 `ClusterManager`（1s 间隔/2s TTL 配置加速）+ 双 wiring 引擎共享真 PG；node-a apply 阻塞于 loopback 步骤（通道 release 门，确定性）；真实健康循环走完 `Eventually(interrupted, 10s)`；断言 trace 存在、哈希链 verify 通过 |
| 2. 僵尸复活零污染 | `TestTakeover_ZombieWritesRejected`：人工 `UPDATE run_execution SET lease_expires=NOW()-1s` 制造过期（无 sleep）→ node-b `TakeoverOnce()` → 放行阻塞通道模拟僵尸醒来 → 断言：后续步骤行零新增（落库门）、run 仍 interrupted（CAS 门）、接管锁期间无双写 |
| 3. 无双写 | 同上 + 接管窗口内并发 `TakeoverOnce()`×2（不同节点）恰好一个产出 trace |
| 4. 单节点零改动 | 全量既有套件 + guard=nil 路径审查（B1 的 CAS 化是唯一公共路径改动，B2-B5 触点全部集群门控） |
| 5. 确定性 | 无 sleep（过期靠注入、死亡靠断水/阻塞门、循环靠 `TakeoverOnce` 直调或 Eventually-on-state）；review 阶段 grep 校验 |
| 6. CI ≤+3min | integration&postgres 作业时长前后对比 |

**Q2 裁定（本次细化定案，不再作为开放问题）**：死亡模拟采用**进程内断水**
（阻塞通道 + 注入过期租约 + 停心跳），不引入 kill -9 子进程腿。论证：PG 与
接管循环可观测的全部状态只有（a）run_execution 行、（b）run 状态、（c）审计
行——kill -9 与断水在这三处产生的 DB 可见状态**完全一致**，差异仅是进程残骸，
而进程残骸不在任何验收断言的观测面内；真子进程腿恰恰是我们刚清剿完的 CI
flaky 温床（进程回收时序）。等价性论证写进测试文件头注释。

### 7.5 拍板记录（v0.4，2026-09-07，实施口径全定）

- **Q1 → 可 Retry**：`interrupted` 加入 `RetryChange` 可重试集，从干净计划
  重驱动（与接管"永不重跑副作用"不矛盾：接管不做远端补偿，重驱动是
  显式新执行，走完整审批/围栏/落库门）。
- **Q2 → 断水模拟定案**（7.4 末段，等价性论证入测试文件头注释）。
- **Q3 → rollback 在接管面外**：rollbackChange 不经 running、不登记；
  节点死在手动回滚中途时 run 停在原终态，人工可重发。README 如实注明。

至此 §7 实施口径全部落定，无开放问题。开工约束：按 B1→B5 顺序提交，
每提交独立全绿；全局核验 + CI 双分支绿后才动 README 告警与设计稿状态。
