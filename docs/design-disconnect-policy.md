# 设计提案：断网策略（专线中断时平台怎么办）

状态：**提案（待 owner 拍板 Q1–Q4）**。本文档是策略层提案，不改任何代码；机制全部为既有实现（`docs/design-cluster-failover.md` B1–B5 已实施）+ 2026-11-10 容器靶场三场景实测（#96，证据 `run/cluster/evidence/s{1,2,3}/`，本地产物不入库；PR #337）。

## 0. 事实基线（全部实测，可复跑）

- 断网形态 S2（执行节点进程活、与 PG / 目标机真分区）：租约 TTL（默认 30s，心跳 TTL/3 续约）过期 ⇒ leader 接管 `running→interrupted` + trace `interrupted_by_takeover` + 删 `run_execution` 行。S2 实测 9/9 PASS。
- 断网形态 S1（节点硬杀，等价 OOM/断电）：步级账本在批次闭包结束一次性落盘（`persistClosureResults`）⇒ 在途轮次账本零行，不补写；接管同样收敛 `interrupted`。S1 实测 10/10 PASS。
- 愈合后旧执行者的晚期写入：epoch 三道门（派发前 `Owns`、落库前 `Owns`、终态 CAS）全拦，零落盘、零状态写，trace `evidence_discarded ≥ 1`。S2/S3 实测。
- 恢复是人驱动：REST `POST /changes/:id/retry`（`replan=false` + 显式 `targetHosts`）。CLI `levee retry` 的 `isRetryableStatus` 只认 failed/paused（`cmd/levee/cmd_retry.go`），对 `interrupted` 是错指令。
- 物理 at-least-once：目标机标记文件 mtime 推后（重跑轮次重写副作用）；账本 exactly-once：每步恰一行 success，无鬼影行。
- **恢复时延上界 = 目标机锁 TTL（300s = exec_timeout）**：硬杀后目标机锁的 owner 是引擎临时执行 id（`internal/engine/closure.go` 的 `ClosureRunner.Run` 铸造），接管只结算 run 状态不碰目标锁；TTL 内 retry 必被 `ErrLockHeld` 挡。这是防物理双跑的安全网（锁 TTL 覆盖单步超时 ⇒ 锁过期 ⇒ 旧执行无在途步骤），产品行为合理不改。
- S3（PG 进程级重启 + 命名卷）：控制面自动收敛，短 outage <TTL 不误判、长 outage >TTL 收敛 `interrupted`。S3 实测 8/8 PASS。**S3 是单 PG 进程重启，不是副本提升。**
- 执行结果不确定时的判定已有统一实现：`internal/evidence` 的 `Classify` 六种判定（not_started / in_flight / complete / partial / missing / unrecorded），`interrupted` 永远不是 `complete`。

## 1. 问题与收益

断网时平台要回答四问，今天都只有机制、没有策略：① 新任务进不进；② 在途怎么办（机制已有，策略未成文）；③ 哪些操作断网期仍可用；④ 愈合后怎么补证据、对账。

成文的收益：操作员在断网现场不再靠读代码推理（本批验收前 DEMOS A4 就有三处这样的口径冲突）；四问的答案可验收、可机器断言。

## 2. 四段策略（每段：形态选项 + 代价 + 判据）

### 2.1 停新任务（分区期间新变更准入）

| 案 | 形态 | 代价 |
|----|------|------|
| A | 全拒（fail-closed）：serve 检测到 PG 不可达 / 租约不可续约后，拒绝新 apply 与新审批 | 可用性归零直到愈合；但断网期本来就无法证明执行权 |
| B | 排队：新变更入 pending，愈合自动放行 | 放行时刻与批准时刻的状态可能已漂移（目标清单、审批人、窗口）；要新增 pending 恢复路径与对应的审批语义 |
| C | 分层：只读 + 已批准 run 的续跑放行，新 apply 拒 | 「已批准」集合的边界要定义（审批过期 sweep 是 1m 周期）；续跑仍需执行权登记，分区期登记不可见 ⇒ 名义分层、实为 A |

**判据（前置）**：任何放行形态必须先回答「分区期间执行权登记（`run_execution` / 租约）对谁可见」。答不出就是无围栏执行 = 物理双跑面。
**推荐 A**：与围栏同源——断网 ⇒ 无法续约 ⇒ 无法证明执行权 ⇒ 拒绝。B/C 的可用性收益以放弃执行权权威为代价，不成立。

### 2.2 在途收敛

- 现状即策略：自动收敛，TTL 30s + 接管 interval 10s ⇒ 收敛上界 ≈ 40s（`design-cluster-failover.md` §7.3 已论证），S2/S3 实测均在界内。
- 形态：维持自动收敛（推荐）vs 收敛前人工确认（拖长时延、且值班未必在场；违背「收敛不能等值班」的既定拍板）。

**判据**：收敛必须同时产出可读终态（`interrupted`）+ 接管事件（`/system/takeover-events`）+ 零行账本（如实），三者缺一不可人工补写。

### 2.3 应急白名单（断网期仍允许的操作集）

- 候选集与事实约束：
  - 只读命令（show / list / status / audit）：单节点 SQLite 路径天然可读，集群态读依赖 PG。
  - 本地 apply / CLI retry：**不接围栏**（`engine_cli.go` 无 guard，#96 机制核实）——进白名单 = 给分区期双跑开后门，明确反对。
  - 手动回滚：`RollbackChange` 在围栏外是既定口径（`design-cluster-failover.md` §7.3）——分区期使用 = 明知无围栏仍放行，不进默认白名单，按次人工承担。
- **推荐**：只读白名单，且逐条声明其依赖（PG 可达性）。

**判据**：白名单每条必须逐条回答「分区期间执行它会不会产生远端副作用」。会产生副作用的一律不进。

### 2.4 恢复补证据（愈合后对账流程）

- 流程（人驱动，与实测一致）：① 愈合确认 → ② 查 `locks` 表孤儿锁 `expires_at`（机器判据：`wait_lock_free` 同款查询；TTL 300s 是上界不是估计）→ ③ REST retry（`replan=false` + 显式 `targetHosts`）→ ④ 断言 `completed` + 每步恰一行 success + 标记文件 mtime 推后（如实呈现物理 at-least-once）+ trace 无新增 `evidence_discarded`。
- 形态：人驱动（推荐）vs 自动重驱。自动 = 在状态不明时自动产生远端副作用，正面违反 R1 的三层缓解（`design-cluster-failover.md` §2）。
- 补证据不是补写账本：零行是设计事实（闭包结束一次性落盘），对账用三源——账本（exactly-once 行数）+ trace（`evidence_discarded` / `interrupted_by_takeover`）+ 目标机 mtime（物理面）。`internal/evidence.Classify` 的六种判定是对账的现成判据。

**判据**：四步每步都要有机器判据，不允许「看着像好了」。

## 3. 验收门槛（提案若实施，可机器断言的部分）

1. 分区期新 apply 被拒，响应含明确 reason（非 5xx、非挂起）。
2. 白名单只读命令在分区期可读（单节点路径），返回数据带「PG 不可达」标记（如实而非假装新鲜）。
3. 恢复流程四步的判据脚本化（② 的 locks 查询、④ 的行数 / mtime / trace 断言），与 #96 驱动同构复用。
4. Q1=A 的前提：serve 对「PG 不可达」的检测判据必须有测试覆盖——误判会把本可放行的变更也拒掉，A 的诚实性依赖它。

## 4. 待拍板（评审问题）

- **Q1 停新任务**：A 全拒（推荐）vs B 排队 vs C 分层（实为 A，见 §2.1）。
- **Q2 在途收敛**：自动（推荐，现状）vs 收敛前人工确认。
- **Q3 白名单**：只读（推荐）vs 只读 + 手动回滚（按次人工承担）vs 只读 + 本地 apply（明确反对：绕过围栏）。
- **Q4 恢复**：人驱动（推荐）vs 自动重驱（反对：状态不明时自动产生远端副作用）。

## 5. 成本

纯文档提案：0 代码、0 新依赖。若 Q1 选 B 或 Q3 扩容，实施成本在那时另估（现在估是赌——B 要动审批语义与 pending 恢复路径，Q3 任一扩容都要先解决「分区期执行权对谁可见」）。

## 6. 拍板记录

- **Q1 停新任务 → A 全拒**（2026-11-10）：分区期执行权登记不可见，B/C 都答不出「执行权对谁可见」；拒绝是诚实而非损失。
- **Q2 在途收敛 → 自动（维持现状）**（2026-11-10）：收敛上界 ≈40s，#96 S1/S2/S3 实测均在界内；人工确认拖长时延且违背「收敛不能等值班」。
- **Q3 应急白名单 → 只读**（2026-11-10）：分区期零远端副作用；手动回滚在明知无围栏下放行远端补偿（撞 R1），本地 apply 不接围栏（双跑后门）。
- **Q4 恢复 → 人驱动四步对账**（2026-11-10）：状态不明时自动重驱 = 自动产生远端副作用，正面违反 R1；四步判据见 §3。
