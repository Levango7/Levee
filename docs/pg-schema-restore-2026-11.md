# PostgreSQL schema 恢复切片（2026-11-25）

| 元信息项 | 内容 |
| --- | --- |
| 范围 | `pg.schema_snapshot_ddl`、`pg.schema_restore`、rollback snapshotter 接线 |
| 目标 | 用独立、严格校验的版本化格式，恢复自包含表的 DDL，并证明真实 PostgreSQL 可回放 |
| 实测环境 | PostgreSQL 16.15、真实 SSH channel、一次性隔离容器 |
| 边界 | DDL-only、仅目标关系不存在时恢复；不含行数据，不覆盖现存对象，不是通用 undo 生成器 |

## 1. 快照格式

每个受影响表保存一份 JSON，格式为 `levee.pg.schema/v1`：

- `table`：服务端解析后的 `schema.table`，不接受靠 search_path 猜测的恢复目标。
- `ddl`：目标机 `pg_dump --schema-only` 的输出，含表、其拥有的 SERIAL 序列、约束与索引。
- `sha256`：DDL 原文的 SHA-256；任何捕获后漂移都拒绝。

`pg.schema_snapshot` 的 information_schema/pg_catalog 内省报告继续保留作人工证据，**不作为恢复负载**。`pg.schema_snapshot_ddl` 是另一条明确动作，避免破坏既有调用方。

捕获命令使用 `--no-owner --no-privileges --no-tablespaces --no-comments --no-security-labels --quote-all-identifiers`，并设置 `pipefail`，避免 `pg_dump | base64` 把 pg_dump 失败掩盖为空快照。新版 pg_dump 的 `\restrict`/`\unrestrict` 元命令使用固定的非密钥标识 `leveepgschema1`；AST 校验只接受配对且位置合法的这对命令，其他 psql 元命令全部拒绝。目标 pg_dump 不支持 `--restrict-key` 时捕获失败，不降级为未验证文本。

## 2. 恢复前校验

恢复输入先通过 JSON、校验和和 oliphant AST 三层校验，再触达目标：

- JSON 只含格式声明的四个字段，限制 4 MiB，拒绝尾随数据、未知版本和表名不匹配。
- 恰好一条目标表的 `CREATE TABLE`；允许其约束、索引、默认值和 pg_dump 的固定 SET/search_path 保护。
- SERIAL 序列必须由 `ALTER SEQUENCE ... OWNED BY` 证明属于该表；不接受名字前缀猜测。
- 自引用外键可接受；跨表外键、分区/继承/typed 表、IDENTITY、DML、DROP、GRANT、跨表对象和其他语句类型拒绝。
- 目标关系必须不存在。`to_regclass` 返回任何现存关系都停止，绝不 DROP 后重建。

恢复执行后重新运行 pg_dump，要求新定义与快照逐字一致；执行或校验失败都不能记为成功，`Changed=true` 只在真实创建且验证通过时产生。

## 3. snapshotter 生命周期

- `pg.query + strategy: snapshot` 先经 `ReviewPostgres`：DML、DBDDL、影响面不完整或无受影响表均 fail-closed。
- 每个写目标捕获一份 `__pg_schema__/<schema.table>` 负载；失败捕获会删除新建空记录。
- 自动回滚与手动 RollbackChange 都按 change id 选择最新捕获；多份负载按表名确定性处理。
- 历史空记录、旧内省记录或缺少 `__pg_schema__` 负载的记录，恢复时仍失败，不会重新变成 no-op 成功。

## 4. 明确不支持

- 行数据恢复、跨表事务一致性、跨表外键依赖图、IDENTITY、分区/继承、视图和库级 DDL。
- 覆盖或修补仍存在的表。例如 `ALTER TABLE ... DROP COLUMN` 后表仍存在，本动作会拒绝，必须人工或用显式 rollback steps 处理。
- 不支持的形态不是“尽力恢复”，而是捕获/恢复失败并保留人工处理入口。

## 5. 真实回放证据

执行：

```sh
python scripts/replay_pg_schema.py
```

启动器创建一次性 SSH+PostgreSQL 容器，不发布数据库端口，生成临时密钥并使用 strict known_hosts；结束后删除容器与密钥。真实测试完成：

1. 创建含 SERIAL 主键、UNIQUE、自引用外键和二级索引的表。
2. 捕获 2344 字节的版本化快照，DROP TABLE 后恢复成功。
3. 独立目录检查为 `3|3`：主键/自引用外键/唯一约束，以及对应三个索引。
4. 再次恢复因目标已存在而拒绝。
5. 追加重算校验和的 DROP 负载仍在触达服务端前拒绝。

最终日志：`REAL_PG_REPLAY PASS`。这是一项真实单实例功能证据，不是生产集群、HA 接管或行数据一致性证明。
