# PG 方言可读性预研（P1 D-6，2026-11-21）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 实现前可行性 spike 记录，**不是已落地的审核能力** |
| 基线 | `master@8fc1135`；`github.com/sqlc-dev/oliphant v0.2.0`（MIT + BSD-3 + PostgreSQL 许可，纯 Go、无 cgo/wasm） |
| 证据 | `internal/sqlreview/pg_oliphant_spike_test.go`（`go test -run Oliphant -v`） |
| 状态 | **预研通过，待产品决策后立项**；生产行为不变，测试专用 import 不进发布二进制 |

---

## 1. 为什么要做

D-6 已登记「PG 能跑不能审」：`internal/executor/modules/pg` 已可执行 PostgreSQL，而审核引擎 `internal/sqlreview` 只认 MySQL 语法。此前实测四档结论（`docs/blocked-items.md` D-6）：通用危险 DML 已被 SR-02/SR-03 拦住、PG 兼容的安全 DML 能过审，但**带 PG 方言标记语法的语句**（`$1` / `RETURNING` / `ON CONFLICT` / `::` / `CREATE TABLE … AS SELECT`）会 SR-01 fail。要做的是"让带方言标记的语句读得懂"，不是重写审核引擎。

## 2. 选型结论（一手实测，沿用 2026-11-20 调研）

| 候选 | 结论 |
| --- | --- |
| `pganalyze/pg_query_go/v6` | **判死**：`CGO_REQUIRED`，与 LEVEE 纯静态构建链冲突 |
| `wasilibs/go-pgquery` | 可用但 WASM 运行时 **13.9 MB** |
| **`sqlc-dev/oliphant` v0.2.0** | **推荐**：纯 Go、**8.3 MB**、avg ~5–8 µs（单次解析）、**10/10 PG 方言样本解析成功**；API 与 pg_query_go v6 一一对应（同导出函数、同 protobuf AST、同 JSON、同 fingerprint） |

## 3. 本轮 spike 实测

`pg_oliphant_spike_test.go` 用 10 条样本覆盖：`$1` 占位符、`RETURNING`、`ON CONFLICT DO NOTHING`、`::date` 转换、CTE、JSONB 操作符、`LATERAL`、窗口函数、`CREATE TABLE … AS SELECT`、带 WHERE 的安全 DELETE。

结果：
- **10/10 解析成功**，每条恰好 1 个 statement；`ParseToJSON` 产出合法 JSON
- 累计解析 1.69 ms / 10 条（含 JSON 序列化的测试开销），**avg ~168 µs/条**（进程冷启动口径，不是稳态基准）
- 否定对照：非法 SQL 正确报错；`Fingerprint` 对字面量变化稳定
- 2 条样本的 AST 节点名未以字面量出现在 JSON 中（CTE→`WithClause`、窗口→`WindowDef`），属节点命名差异，**不影响解析成功**，信息性日志

## 4. 若立项：改写面与路由缺口

**需要改动的主体**（仅列面，不动代码）：
- `internal/sqlreview/{review.go,rules.go,impact.go,dialect.go}`：按目标方言选解析器；`findings[].dialect` 已存在，可复用
- `internal/executor/sqlguard/sqlguard.go`：与执行侧方言判定对齐
- **SR-11**（"CREATE TABLE 未声明 ENGINE="）是 MySQL 专属口径，对 PG 恒误报，方言化时一并处置
- `internal/executor/modules/pg` 已能执行 PG → 审核侧接上 oliphant 后即可闭环"能审 + 能跑"

**不建议做**：
- 把 oliphant 设为默认解析器（会改变现有 MySQL 审核语义，需产品决策）
- 借机重写规则引擎（D-6 范围应收敛为"方言可读性"）

**构建影响**：oliphant 为测试专用 import，不链接进发布二进制；若未来生产接入，需重新核对镜像体积（纯 Go 8.3 MB）与 SBOM（`docs/security-scan-2026-11.md`）。

## 5. 立项前的两个待决问题

1. **方言判定来源**：由 `target` 的标签/凭据推断，还是 workflow 显式声明 `dialect`？后者更可控但改 DSL。
2. **不可读时的语义**：SR-01 fail（现状）还是新增独立的"方言不可读"告警级别？这会影响现有测试与对外口径。

## 6. 边界

- 本文件是**预研 + 设计输入**，不代表 LEVEE 已支持 PostgreSQL 方言审核；对外口径仍按 D-6 四档表，**不得说"PG 变更能审"**。
- 依赖 oliphant 的许可证为 MIT/LIBPG_QUERY(BSD-3)/PostgreSQL，与现有 SBOM 流程兼容。
