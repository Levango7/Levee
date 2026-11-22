# MySQL schema 恢复切片（2026-11-22）

| 元信息项 | 内容 |
| --- | --- |
| 范围 | `mysql.schema_restore` + 既有 rollback snapshotter 接线 |
| 目标 | 用一条真实、非破坏的恢复路径证明「变更前快照可用于恢复」，而不是只用于展示 |
| 边界 | MySQL、DDL-only、仅目标表不存在时重建；**不含行数据恢复，不含 PG 内省恢复** |

## 1. 恢复契约

一次恢复必须依次满足：

1. 快照是**一条完整的 CREATE TABLE**，且表名与恢复目标完全一致；多语句、DROP、ALTER、不可读前缀全部拒绝。
2. 目标表在恢复时**不存在**。只要表仍存在，就拒绝继续，绝不 DROP 后重建——现存表的数据和失败后的定义必须留给操作员人工判断。
3. 执行 CREATE 后，再次 `SHOW CREATE TABLE`，要求服务端返回的定义与快照逐字一致（忽略首尾空白）。执行失败或校验失败都不能记为恢复成功。

这是一项 **schema 恢复**，不是行数据恢复，也不是通用 undo 生成器。`Changed=true` 只在真实创建并通过验证时产生。

## 2. 与既有快照生命周期的关系

- apply 前：`remoteSnapshotter.captureMySQLSchema` 复用 `sqlreview` 的写目标，逐表调用 `mysql.schema_snapshot` 获取服务端 canonical DDL，并按 `__mysql_schema__/<table>` 写入既有 snapshot store。
- rollback：`RestoreForStep` 对相同快照键调用 `mysql.schema_restore`，沿用 change id 而非 execution run id，所以自动回滚和手动 RollbackChange 都找到同一份快照。
- 文件快照与 schema 快照继续共用原有存储和 change key；文件恢复路径不变。

## 3. 明确拒绝的情况

- 任意 DML（INSERT/UPDATE/DELETE），包括 DDL 与 DML 混合：schema 快照恢复不了行变化，不能声称完整回滚。
- DBDDL（CREATE/DROP DATABASE）：本切片不覆盖库级恢复。
- 不完整 SQL 影响面：无法证明覆盖了所有受影响表。
- 目标表已存在：禁止覆盖。
- 快照表名不匹配、多语句、非 CREATE、校验不一致：禁止宣称恢复。

## 4. 验证证据

- `internal/executor/modules/mysql/restore_test.go`：成功重建并校验、现存表拒绝、表名不匹配、多语句、CREATE 失败、校验不一致、限定 schema。
- `internal/wiring/snapshotter_schema_test.go`：真实 snapshotter 链路「捕获→模拟 DROP→恢复→SHOW CREATE 验证」；现存表不覆盖；混合 DML 在捕获前拒绝。
- 既有 `internal/wiring` 全套回归通过，文件快照恢复不变。
- 全量 `go test ./...` 77 包零失败、vet/gofmt 干净。

## 5. 后续边界

- PostgreSQL 的 `pg.schema_snapshot` 是 information_schema/pg_catalog 内省结果，**不是可恢复 DDL**，不能直接复用本路径。PG 恢复需要先定义可还原 DDL 生成与验证规则。
- 行数据恢复需要独立的数据备份格式、冲突处理、部分恢复与一致性验证，不在本切片。
- 真正的 MySQL 服务器/集群演练仍需环境执行；本切片通过真实生产代码路径与可记录目标状态的传输测试证明行为，不把 mock 当成真机验收。
