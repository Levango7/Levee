# 设计提案（v0.1）：跨节点调度（cluster dispatch）

状态：**草案，待评审**。前置依赖均已合入（设计 A 执行引擎接线、设计 B 集群故障接管，v1.13.0）。本设计的定位：把已计划/已批准的 run 分派给 worker 节点执行——与 B 的接管（"节点死了怎么办"）互补，合起来构成完整的集群执行。

## 0. 事实基线

- 执行模型：apply 经 CAS 把 run 置 `running` 后，引擎在 gRPC 进程内同步执行。`run` 行无 owner 节点字段——执行在数据库层面是"无主"的。
- 集群原语就绪（B）：节点注册/心跳（`internal/cluster/pg_registry.go`）、leader 选举（`GetLeader`）、分布式租约锁（`DistributedLockManager`，UNIQUE→`ErrLockHeld` 归一）、执行租约（`run_execution` 表 + epoch 围栏）、接管循环（`internal/takeover`）。
- 调度器目前不存在：所有 run 都在 leader 进程内执行，worker 空转。

## 1. 收益（明确、可验收）

1. **水平扩展**：N worker 并发执行 N 个 run，吞吐 ∝ 节点数。v1 单节点瓶颈消除。
2. **HA 完整**：调度 + 接管（B）= 节点挂了其 run 被标记 interrupted 并重新分派，不丢工作。
3. **GA 资格**：集群模式从"单节点演示 + 故障接管"升级为真正的多节点分布式执行。
4. **与 B 正交复用**：调度只读 B 的注册/心跳/选举原语，不修改它们；接管循环原样处理调度产生的 run。

## 2. 范围（硬边界，防蠕变）

**本期只做（run-level dispatch）**：
- leader 把 `approved` run 分派给空闲 worker（不拆 batch、不拆 step）
- worker 拉取分派给自己的 run 并执行（走**现有 apply 路径不变**）
- worker 死亡 → 接管循环（B）标记 `interrupted` → leader 重新分派
- 调度器 leader 死亡 → 新 leader 重新扫描未完成的 assignment

**本期不做（明确出局）**：
- ❌ 单 run 的 batch/step 跨节点分派（需要跨节点回滚协调 + 快照共享，独立项目）
- ❌ 执行器/closure/引擎接口任何改动
- ❌ 智能负载均衡（v1 最少连接数即可，复杂度不匹配收益）
- ❌ run 表加 owner 字段（避免 ALTER 破坏单节点路径）

## 3. 数据模型

新表 `run_assignment`（独立表零 ALTER，干净 slate，无存量数据债）：

```sql
CREATE TABLE IF NOT EXISTS run_assignment (
    run_id      TEXT PRIMARY KEY,        -- 1:1 with run；不 ALTER run 表
    owner_node  TEXT NOT NULL,           -- 被分派的 worker node id
    epoch       BIGINT NOT NULL,         -- 单调递增；易主/重调度时 +1
    state       TEXT NOT NULL,           -- pending | executing | done | interrupted
    result      TEXT NOT NULL DEFAULT '',-- 终态：completed | failed | rolled_back | ''（词表冻结，见下）
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(run_id, epoch)                -- 允许同一 run 的历史 epoch 留存（审计）
);

CREATE INDEX IF NOT EXISTS idx_assignment_owner_state ON run_assignment (owner_node, state);
CREATE INDEX IF NOT EXISTS idx_assignment_state ON run_assignment (state);
```

**设计决策（向前兼容的伏笔）**：
- `run_id` 做 PK 而非 `(run_id, epoch)`：当前 run-level 语义下每个 run 只有一个活跃 assignment。未来若进 batch-level，PK 改为 `(run_id, batch_no)` 即可——`epoch` 列已预留，`UNIQUE(run_id, epoch)` 已支持多 epoch 历史。
- `state` 用 free-text（与 `run.status`、`run_execution.status` 同口径，无 CHECK）：vocab 由代码层约束。
- `result` 列冗余终态：避免调度循环频繁 JOIN run 表；`state=done` 时 `result` 必填。
- `result` 词表**刻意不随 run 状态扩展**（D-2 v2 设计裁定）：run 增加了 `rolled_back_partial` / `rollback_incomplete` 两个回滚判定，但 assignment 侧仍只认 `completed | failed | rolled_back`——映射点在 `cmd/levee/grpc_change_executor.go`：`retry()` 显式把两个新判定归为 `failed`，`apply()` 也经 default 分支落到 `failed`；run 行保留精确状态，需要区分时查 run 表。**扩这个词表会动到 assignment 终态识别与「是否已分派/已终结」的判定，改之前先读这条。**

**不用旧表 `run_execution` 的原因**：该表语义是"执行租约/围栏"（owner + epoch + lease_expires），混入调度分派语义会让 lease 续约逻辑与调度重调度逻辑耦合，债越积越多。新表隔离关注点。

## 4. 调度循环（leader 独占）

新包 `internal/dispatch`，`internal/takeover` 同形：

```go
type Loop struct {
    mgr      *cluster.ClusterManager
    store    state.Store
    nodeID   string
    interval time.Duration
    // worker 容量 = 单节点并行 run 数（默认复用 wiring.DefaultMaxParallelRuns）
    workerCapacity int
}
```

```
DispatchOnce():
    if not leader: return
    // 1. 候选：status=approved 且无活跃 assignment（state pending/executing）的 run
    candidates = approved_runs_without_active_assignment()
    // 2. 空闲 worker：心跳 active 且 active_executions < capacity，按负载升序
    workers = active_workers_by_load()
    // 3. 配对（最少连接数）
    for run, worker in zip(candidates, workers):
        try_claim(run, worker)   // INSERT ... ON CONFLICT (run_id) WHERE state NOT IN ('executing','done')
```

`try_claim` 的原子性：`INSERT INTO run_assignment ... ON CONFLICT (run_id) DO UPDATE SET ... WHERE state NOT IN ('executing','done')`。`RowsAffected()==0` 表示被并发调度器或接管循环抢走，跳过。

## 5. 执行路径（worker 侧，不改引擎）

`internal/wiring` 加 `WithDispatchNode(nodeID)` 选项。worker 启动后：

```
worker 注册自己（复用 Join/Heartbeat）
Loop:
    assignment = get_pending_assignment_for(nodeID)
    if assignment:
        transition(assignment, executing)
        execute_via_existing_apply_path(assignment.run_id)  // 不变
        // apply 路径落终态后：
        transition(assignment, done, result=run.Status)
```

关键：执行**完全走现有 apply 路径**（CAS running → engine.Run → 终态 CAS）。`run_assignment` 表只记录"谁被分配了"，不介入执行引擎。

## 6. 失败处理（三层，均复用 B）

| 场景 | 处理 | 来源 |
|------|------|------|
| worker 心跳超时 | 接管循环→ run `interrupted` + assignment `interrupted` | B 已有 |
| 调度器 leader 死亡 | 新 leader 收敛选举后重新 `DispatchOnce()`，跳过已有 `executing` 的 assignment | 收敛选举 |
| 执行 CAS 竞争败 | 快速失败，assignment 留给胜出者 | 已有 CAS |
| 同一 run 被双调度 | `ON CONFLICT ... WHERE` 串行化，输家跳过 | 新表约束 |
| **worker 分派后、领取前死亡** | leader 扫描回收：claim timeout 超时 → 重指到活跃节点并 `epoch+1`（旧 owner 迟到领取被 epoch fencing 拒绝） | 新 `ReclaimAssignment`（D-4 v2） |

### 6.1 调度回收与容量（D-4 v2）

`pending` assignment 在 worker 领取前就是「已承诺的容量」，同时也可能永久卡住：owner 在分派之后、领取之前死亡（或停止轮询）时，行停在 `pending`，而 `candidates` 把它当「有活跃 assignment」永远跳过该 run —— 既执行不了，也不会被重派。

修法（零 schema 变更）：

- **claim timeout（默认 10m，`--cluster-dispatch-claim-timeout` 可配）**：leader 每次扫描先回收超时仍未领取的 `pending` assignment，重指到当前负载最低的活跃节点。10m 远大于 CAS→Begin 临界区与 worker 轮询间隔（2s），健康集群永不触发。
- **epoch fencing**：回收走 `ReclaimAssignment`，CAS 条件为 `(run_id, epoch, state='pending')`，成功后 `epoch+1`——迟到 worker 的领取 CAS 仍比较旧 epoch 因而失败，同一 run 不会被两个节点执行。与既有 `Reassign` 的关键差别正在此：`Reassign` 只比 epoch、不锁状态（用于终态行重派），拿它做回收会把已被领取的 `executing` 行拖回 `pending`。
- **孤儿行终止**：若该 run 已不在 `approved`（暂停、重 plan 回 draft、已终态），回收不重派，而把 assignment 置 `interrupted`——没有可执行的工作，该行必须停止计入负载；后续重新批准经 `Reassign` 以 `epoch+1` 重新开始，旧 epoch 不会复活。
- **容量计入 pending + executing**：`workerLoads` 只排除终态（`done`/`interrupted`）。pending 是已承诺的工作，只算 `executing` 会让 leader 在节点队列未清空时继续超发（P1-6）。

回收留痕：metric `levee_dispatch_attempts_total{result=reclaimed}` + 日志 `stale pending assignment reclaimed`（含 `stale_owner` / `new_owner` / `epoch` / `age`）。

## 7. 与单节点路径的兼容性

- **单节点部署（SQLite，无 `--cluster`）**：调度循环不启动（`DispatchLoop.Start` 仅在 clusterMgr != nil 时调用），行为与今天完全一致。
- **集群模式但无 worker**：调度循环照常运行，只是 candidates 找不到 worker → 不分派，run 留在 `approved` 等人工处理或 worker 加入。
- **混合部署（leader 也当 worker）**：leader 节点也在 workers 列表中，可执行分派给自己的 run——与今天 leader 执行所有 run 的行为兼容。

## 8. 观测

- `levee_dispatched_runs_total{state=pending|executing|done|interrupted}` 计数
- `levee_worker_active_runs{node_id=...}` gauge
- `levee_dispatch_attempts_total{result=claimed|skipped_busy|reclaimed}` 计数
- assignment 状态变迁走 trace（actor="cluster-dispatch"）

## 9. 风险与缓解

| 风险 | 固有危害 | 缓解 | 缓解后残余 |
|------|---------|------|-----------|
| 双分派（两 leader 同时调度同一 run） | 中 | `run_assignment` 唯一 PK + `ON CONFLICT ... WHERE state NOT IN` 串行化 | 低 |
| 调度循环与接管循环竞争同一 assignment | 中 | 接管只改 state→interrupted；调度只 INSERT/UPDATE 自己拥有的 epoch；`WHERE` 子句隔离 | 低 |
| worker 拿到 assignment 后崩溃，assignment 卡在 `executing` | 中 | 接管循环（B）检测心跳超时→置 `interrupted`→调度循环重新分派 | 低 |
| 范围蠕变（做到一半想拆 batch 分派） | 中 | 硬边界写进本文档 §2；batch 级独立项目 | 低 |
| 新表与 `run_execution` 语义重叠 | 低 | 明确分工：`run_execution`=执行租约/围栏，`run_assignment`=调度分派；不混用 | 低 |

## 10. 成本估算

- 新增包 `internal/dispatch`（调度循环）+ `internal/state` 加 `run_assignment` CRUD + `internal/wiring` 加 `WithDispatchNode` 选项
- 实现 + 测试：~10-14 提交
- e2e：双 worker + leader，分派→执行→worker 死亡→接管→重调度（真 PG）

## 11. 验收门槛（全过才合入）

1. 双节点 + PG e2e：worker-a 执行 run-1，worker-b 执行 run-2，均落终态 + assignment `done`。
2. worker 死亡：执行中 worker 被 kill，≤2×租约周期 run 落 `interrupted`，重调度后另一 worker 完成它。
3. 无双写：并发调度场景下 assignment 行不产生重复 `executing`。
4. 单节点全量既有测试零改动全绿。
5. 新测试全部确定性（无时钟赌博、无 sleep 赌调度）。
6. CI 时长增幅 ≤ ~3 分钟。
