# audit 链封链的读放大：量化、方案与定档

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | audit 链封链的读放大——量化、方案与定档 |
| 文档类型 | 设计评估（含实测数据） |
| 日期 | 2026-11-02 |
| 触发 | `internal/audit/auditchain.go` 的 `walkChain` 注释长期登记"sealing still walks every visible row, and Record calls it per write … Making that incremental … is a change to what a broken chain means, so it is not folded in here"——**这句话从未被量化，也没有任何测试或基准钉住它** |
| 结论 | **维持现状**；本笔补基准测试与本文档，把 C/D 两个方案与重新立项的触发条件写死，交给维护者决定，不由一次性能 PR 顺手改掉安全姿态 |

---

## 1. 问题：一个"越用越慢"的写路径

`audit.Record`（`internal/audit/record.go`）= `store.CreateAudit` + `AuditChainBuilder.Seal`。而 `Seal` 走 `walkChain`，后者**分页读完整条审计链**再逐行判定。于是每写一条审计，就要读并哈希此前每一条审计。

这不是冷路径：审批、apply、pause/resume、登录、凭据轮换、目标机变更都写审计。一次中等变更产生几十到几百条；一个跑了几个月的部署，审计表轻易过万行。

## 2. 实测（本机 SQLite，2026-11-02）

`go test -run XXX -bench 'BenchmarkAuditRecordSealsWholeChain|BenchmarkAuditChainSealSteadyState' -benchtime 20x ./internal/audit/`

链已封到稳态（每条 seeded 行都已封，这也是 `Record` 里第二次 `Seal` 的真实前置——没有新行可写，只有旧行要读）：

| 已封行数 | `audit.Record`（插入+封链） | `Seal`（仅封链） | Seal 占 Record |
| --- | --- | --- | --- |
| 100 | 0.53 ms | 0.27 ms | 51% |
| 1 000 | 3.41 ms | 2.39 ms | 70% |
| 5 000 | 18.34 ms | 17.45 ms | **95%** |

线性，约 **3.5 µs/行**（17.45 ms ÷ 5000），分配约 56 B / 24 allocs 每行。据此外推：

| 已封行数 | 每次审计写的封链耗时 |
| --- | --- |
| 10 000 | ~35 ms |
| 100 000 | ~350 ms |
| 1 000 000 | ~3.5 s |

**第一行结论：瓶颈是封链而不是插入**（5 000 行时 95% 的时间花在 Seal 上）。任何只优化插入的方案都动不了真问题。

## 3. 为什么不能"顺手"改成只封尾部

这是本文档的核心。当前 `Seal` 的三分支判定里，与安全性直接相关的那一支是：

```go
case ComputeAuditHash(a, a.PrevHash) == a.CurrHash:
	// Relocated, not altered: relink it here.
default:
	return fmt.Errorf("… record content no longer matches the hashes it was sealed with: %w", ErrChainBroken)
```

注意它**只用这一行自己存的 `PrevHash`**，不依赖顺序累积的 `prev`。也就是说，**"这行的内容是否被改过"是一个单点可判定的性质**——不需要走到它之前的所有行。

推论，也是全部风险所在：

> **跳过已封前缀 = 跳过对前缀内部每一行的"内容 vs 自身哈希"写时校验。**

现状下，任何审计写都会顺带把整条链重判一遍，因此"前缀里某行被改了内容"会在下一次审计写时立刻变成 `ErrChainBroken`（`Record` 记 WARN，`Verify` 报 `FailureEmptyHash`/断链）。改成只封尾部后，这类改动**只在有人运行 `Verify` 时才被发现**。

这里必须把两道防线分清楚：

1. **WORM 触发器**（`internal/audit/worm.go`）：`BEFORE UPDATE`/`BEFORE DELETE` 直接 `RAISE(ABORT)`，在数据库层挡下对已存行内容列的改写。这是**第一道**，也是主力。
2. **Seal 的逐行判定**：纵深防御，覆盖"触发器被 drop"或"绕过应用直连库"的场景。

把 (2) 从"每次写"降为"只在 Verify"，是把纵深防御降级。这个取舍**不由性能动机决定**，所以本笔不做——这正是 `auditchain.go` 原注释那句 "a change to what a broken chain means" 的确切含义。

### 顺带排除一个看似更安全的选项：窄投影读取

"稳态行只需要 `(timestamp, id, prev_hash, curr_hash)`，少读几个内容列总能省 IO"——**不成立**。稳态判定的第一支是

```go
case a.PrevHash == prev && a.CurrHash == want:   // want = ComputeAuditHash(a, prev)
```

`want` 是对**内容字段**算出来的哈希。不算它就无法知道这行是否稳态、也无法算出下一行的 `prev`。所以内容列一行都省不掉，这个方案直接出局。

## 4. 方案对比

| 方案 | 写延迟 | 篡改检测面 | 成本 / 风险 |
| --- | --- | --- | --- |
| **A. 维持现状** | O(N)，~3.5 µs/行 | 每次审计写都全链判定（最大） | 零代码风险；随日志线性变慢 |
| B. 新增 `SealTail`，`Record` 仍调 `Seal` | O(N) 不变 | 不变 | 无收益——`Record` 的延迟一点不降 |
| C. `Record` 改用尾部增量（opt-in 配置，**默认关**） | O(未封尾部) | 前缀内篡改只在 `Verify` 时被发现 | 需部分索引 + 配置面 + 文档明说削弱的检测面；并需重新论证多进程并存两个 builder 时的末端定位（现状靠 cursor 分页天然容忍并发追加） |
| D. C + 后台低频全链校验 | O(1) 写 | 最终一致 | 在 C 的全部前提上再加一个可取消、可观测的后台校验器；复杂度最高 |
| E. 窄投影读 | 常数略降 | 不变 | **不可行**，见 §3 末段 |

## 5. 定档与交付

**选 A。** 写延迟的增长是**可预测、已量化、已记账**的；C/D 换掉的是"每次写都做一次全链完整性校验"这一安全姿态，属产品/维护者决定。本笔交付三件可机器验证的东西，把这次调查钉住，让下一个人不必从零开始：

1. **`internal/audit/benchmark_audit_seal_test.go`**：把 §2 的两个基准固化为可复现输出。它不是 CI 门禁（CI runner 测的是 runner），但其**形状**（随行数线性）是结论的一部分——哪天它不再线性，"sealing still walks every visible row"那句话就过期了，本文档的定档也需要重议。
2. **本文档**：量化、方案、定档理由、C/D 的重新立项条件。
3. **`internal/audit/auditchain.go` 的 `walkChain` 注释**：把原来自说自话的长段改为指向本文档，保留一句话事实，避免两处各自过期。

## 6. 重新立项 C/D 的触发条件（按顺序全部满足才动手）

1. 用本笔基准（或该部署的真实数据）测出审计行数与 `Record` 的 p99。
2. p99 超过运维可接受阈值——建议以 **100 ms** 为讨论起点（§2 表：约 3 万行触达）。
3. 维护者明确接受"写时检测降级为读时检测"（C）或接受引入后台全链校验（D）。**这一步不可跳过**：它是安全姿态变更，不是实现细节。
4. C 的前置工程：`curr_hash` 的**部分索引**（PostgreSQL 与 SQLite 都支持 `CREATE INDEX … WHERE curr_hash = ''`），并把"最后一个已封行"的定位做成能走该索引的查询；否则增量路径退化为一次全表扫，白改。
5. D 的前置工程：serve 侧一个后台校验器——可取消、失败可观测（把 `ErrChainBroken` 点亮而不是吞掉）、不与写路径争锁。

## 7. 已知边界（如实）

- 基准只在 **SQLite** 上跑。PostgreSQL 的执行计划不同，绝对数字会变；"随行数线性增长"这个形状判断不变，但换引擎应重测。
- 基准用 `b.TempDir()`，测的是单连接本地库，**不含网络往返**。部署到远端 PG 时，每页读都要算 RTT，实际代价只会更高。
- §2 的"95% 是 Seal"是稳态测量；首行插入后的第一次 Seal 还要写那一行，略高。
- 本文档**没有**改变任何运行时行为。`Seal`、`Record`、`Verify`、`Rebuild` 的语义逐字未动。
