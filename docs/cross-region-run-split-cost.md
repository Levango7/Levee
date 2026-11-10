# 跨机房 run 拆分：其余形态成本表（A-narrow / A-full）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 决策支撑成本表（代码核实产物，非设计提案、非实现） |
| 登记任务 | #98（第 3 条其余形态成本表，等 owner 重裁） |
| 核实基准 | origin/master `a148c4c`（2026-11-10T11:27:39Z），本表所有 file:line 均在该树可读复算 |
| 状态 | 待 owner 重裁；本笔不动代码 |

## 0. 这张表回答什么

派生三件套的第 3 条「跨机房 run 拆分」：一条 run 的目标跨两个机房时，是整条交给一个执行节点（现状），还是把执行权按批拆给多个节点？

- 形态①（维持 fail-closed 拒绝 + 可操作诊断）：owner 重裁后已由 PR #327 交付（见 §3），是本表的对照基线。
- 形态 A（run 内按机房分批）：owner 曾裁 A，批准时引用的成本（「租约/epoch per-run→per-batch + takeover 复核」）经派发/租约代码核实被证伪三处（§6），实现已停。
- 本表把 A 的两档实现（A-full / A-narrow）的真实成本逐条列清，供 owner 基于当前代码重裁。

**术语恢复（如实登记）**：`A-narrow` / `A-full` 是登记 #98 时为本表造的档位名，仓内文档无此二词；其指代＝形态 A 的两档实现范围（§4/§5 定义）。本表不含任何「评估报告§二.2」的引用——该词在仓内与板内零命中，属登记时内部术语。

## 1. 结论（分级）

1. **A-full（完整 per-batch 放置）= 周级工程 + 逆转已 documented 的硬边界 + 双方言 schema 迁移 + 状态机词表扩约**。推荐：不做，除非出现真实客户诉求。
2. **A-narrow（只放天然同机房批）≠ A-full 的便宜子集**。它只省「批次同质化改造」，不省批间执行权移交的四个硬面（闭包可恢复、per-batch 租约、批间状态、takeover 识别）。名义窄、成本不窄。
3. **形态①（已交付）成本为 0 且已在 master**。在出现真实跨机房单 run 诉求前，①是唯一与「假成功零容忍」相容的选项：它拒绝得诚实（四种拒绝词 + 诊断点名），不假装能拆。

## 2. 现状基线（①的地基）

- 派发只把 run 交给**声明覆盖该 run 全部所需机房**的节点；跨两机房而无节点同时声明两者时报 `no_single_node_spans_regions`，部分目标带标签报 `region_unresolved`，无合格节点报 `no_eligible_node`，四种拒绝都 fail-closed 不静默降级（`internal/dispatch/placement.go:49-61`、`docs/scenario-cross-region-ops.md:98`）。
- #327（已合 master `c5ba99d`）把拒绝升级为可操作诊断：Diagnosis 点名 offending batch 混了哪些机房 + 按原因给补救文案（`internal/dispatch/placement.go:81-100`），批次足迹只进诊断不进判定（`TestDispatchOnce_BatchFactsDoNotChangePlacement` 守）。

## 3. 对照基线：形态①已交付，零新增成本

见 §2。①是「不拆」的诚实实现：操作员拿到诊断后，把 run 拆成两条本区 run，或给一个「桥」节点声明两个机房（`docs/scenario-cross-region-ops.md:98` 的既有指引）。

## 4. 形态 A-full 成本表（完整 per-batch 放置）

批准时引用的成本只有一条（租约/epoch per-run→per-batch + takeover 复核）。代码核实后实际成本面：

| # | 成本面 | 证据（当前树 file:line） |
| --- | --- | --- |
| 1 | **计划分批不按机房**。分批策略是 canary/percent/serial/one-per-target，一批可跨机房；「per-batch 放置」要求派发单元同质。要么改 plan 生成器按机房重分批（动 plan 哈希承诺：审批绑定 plan_hash，`apply` 前强制校验一致，`docs/levee-api.md:177`、`internal/approval/gate_approver.go:13-16`），要么派发时拆批（动 batches 表审计粒度 + post_batch 门禁语义——声明的批次边界门会对子批再触发，`internal/engine/closure.go:694-702`） | `internal/plan/generator.go:3-4,365,372,384,390-391` |
| 2 | **闭包是单进程单元**。`ClosureRunner.Run`：全目标 pre-apply 门 → 全目标锁 → 全批次 → post-apply 门 → 整 run 回滚。拆到两节点 = 闭包要在批边界可恢复续跑——failover 文档 R5 明列「可恢复续跑」进 backlog 不做，这是逆转 documented 硬边界，不是实现细节；且失败批的回滚要跨节点编排（补偿账本按 (host, forward step) 持久化，数据在，但回滚驱动今天住在持账本的那个进程里） | `internal/engine/closure.go:535,588,640-644,679,726,737`；`docs/design-cluster-failover.md:29`（R5） |
| 3 | **围栏/租约是 per-run 单 epoch**。`run_assignment`/`run_execution` 均 run_id PK；per-batch 租约要新 schema（复合 PK 或新表 + SQLite/PG 双迁移，当前 `currentSchemaVersion = 10`，即 v11+；WORM/触发器面同步评估） | `internal/state/store.go:692`；`internal/state/migrate.go:29` |
| 4 | **run 状态词表无「批间待派发」态**。词表是契约：`All` 列表 + vocabulary_guard_test 钉死 + docgen 同步 web vocabulary.ts；新增中间态会撞 settleRun 映射、CLI 标签、`RetryChange` 准入、web 标签一整条契约链 | `internal/runstatus/runstatus.go:37-41,89`、`internal/runstatus/vocabulary_guard_test.go` |
| 5 | **takeover 语义要重设计**。批间态不能看起来像「崩了的执行者」：无租约可判过期、无非终态步骤可标 unknown；今天 takeover 的候选集是「过期租约执行 + 孤儿 running」，收敛动作是 running→interrupted CAS | `internal/takeover/takeover.go:4,17,26,211,231` |
| 6 | **文档改写集**：`scenario-cross-region-ops.md:98`（「本版不做」翻转为在做）、`design-cluster-failover.md` R5 边界改写、`design-cluster-dispatch.md`、`cli-reference.md` 跨机房放置段（:2304） | 上述四处 |

## 5. 形态 A-narrow 成本表（只放天然同机房批）

定义：只有计划批次天然同机房的 run（one-per-target 工作流全量覆盖，含 DR 场景旗舰）享受 per-batch 放置；混合批维持现状 fail-closed 拒绝，诊断文案不变。

| # | 成本面 | 与 A-full 的关系 |
| --- | --- | --- |
| 1 | 批次同质化改造 | **省**（§4-1 的前半：不用改 plan 生成器、不用派发时拆批） |
| 2 | 批间执行权移交：闭包在批边界可恢复 | **不省**（§4-2 原样成立——下一批在另一个节点续跑同一份闭包状态） |
| 3 | per-batch assignment 行 + 租约/epoch | **不省**（§4-3 原样成立——A-narrow 仍需按批登记的租约，否则两批之间无围栏可判） |
| 4 | 「批间待派发」run 态 | **不省**（§4-4 原样成立——第二批派发前 run 停在哪个词表态里？） |
| 5 | takeover 对批间态的识别 | **不省**（§4-5 原样成立） |
| 6 | 文档改写集 | **不省**（§4-6 原样成立） |

结论：A-narrow 省的是「让批次同质」这一处，留下的六个面里五个与 A-full 同价。它把「哪些 run 受影响」收窄了，没有把「每个受影响 run 要动的面」收窄。**名义窄、成本不窄**。

## 6. 批准时引用的成本为何被证伪（复算指引）

owner 裁 A 时引用的成本只有「租约/epoch per-run→per-batch + takeover 复核」一条。核实现场（派发/租约代码逐行读）后确认它不完整，证伪三处对应 §4-1/§4-2/§4-3（分批不按机房、闭包单进程、per-run 单 epoch），另加 §4-4 的状态机契约撞车（批间无待派发态）。原始核实记录见板行 `qoder-a27.md` notes 与 #94 任务（「读派发/租约代码定 per-batch 放置设计」，已完成）。

复算命令（在 `a148c4c` 树）：

```bash
grep -n "canary\|percent\|serial\|one-per-target" internal/plan/generator.go   # §4-1
sed -n '535,760p' internal/engine/closure.go                                    # §4-2
grep -n "run_id PK" internal/state/store.go; grep -n currentSchemaVersion internal/state/migrate.go  # §4-3
sed -n '37,95p' internal/runstatus/runstatus.go                                 # §4-4
grep -n "candidate\|unknown\|interrupted" internal/takeover/takeover.go         # §4-5
```

## 7. 给 owner 的选项（含代价与爆炸半径）

| 选项 | 代价 | 爆炸半径 | 前置判据 |
| --- | --- | --- | --- |
| 维持①（推荐） | 0（已在 master） | 0 | 无跨机房单 run 的真实客户诉求；操作员按诊断拆 run 或用桥节点 |
| A-narrow | §5 五个不省面，周级 | 派发/租约/状态机词表/回滚编排 + 双方言迁移 + 文档四面改写 | 有客户诉求且只跑 one-per-target 类工作流；且接受「混合批仍被拒」 |
| A-full | §4 六面，周级+ | 同上，另加 plan 生成器或拆批审计粒度 + plan_hash 承诺重估 | 有客户诉求且混合批也必须拆；且接受 plan_hash 语义变化 |

## 8. 诚实边界

- 本表是**代码核实产物**：每条成本给 file:line，可在 `a148c4c` 树复算；**无实跑**（没有为这张表动过代码，也没有靶场数据）。
- 本表只覆盖成本侧；收益侧（跨机房单 run 的真实诉求频率、客户现场网络拓扑）无数据来源，未量化。
- 批准 A 时引用的成本声明被证伪一事已如实记录（§6）；本表不替 owner 重裁，只把重裁所需的真实成本摆齐。
