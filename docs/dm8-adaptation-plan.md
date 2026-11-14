# DM8 适配方案（不写代码版）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 立项前评估（go/no-go 决策依据；**不含可合并代码**） |
| 基线 | `origin/master` @ `b9215c0`（2026-11-13） |
| 数据 | 2026-11-14 本地 DM8 容器（`levee-dm8`，`dm8_single` 镜像，`127.0.0.1:15236`，SYSDBA）实测；LEVEE 仓库文件级 grep |
| 现状 | **不支持（已实测）**；`docs/platform-support-matrix.md` §2 |
| 维护人 | opencode（文档线）；代码实现归属须另行排队 |

---

## 0. 一句话结论

**DM8 驱动可以连上、可以读写 VARCHAR/INT/CAST——驱动本身没问题；但 LEVEE 的 PG 路径几乎只剩 ON CONFLICT 最要命的位置：集群 fencing 的原子「upsert + RETURNING」在 DM8 上是语法错误。要支持 DM8 不是「换个驱动」，而是重写 LEVEE 最危险的并发构造，同时动 qoder 刚稳定下来的 `internal/cluster` 与 `internal/state`。没有标的点名 DM 时，不建议现在做。**

---

## 1. 实测证据（2026-11-14，本地容器）

环境保持两个容器在用，本结论随时可复测（`docker start levee-dm8`）：

- 驱动来源：达梦官方 adapter 直链 `https://download.dameng.com/eco/adapter/resource/go/dm-go-driver.zip`（`dm-go-driver.zip` 内 `dm` 包，zip 内层带 gorm 方言包），`sha256=5d10cc42bbe8e4c7e6d9031d5306caff57b6da6e918868b5d7f70fc678063599`（**官方自家所发，非 GitHub 第三方拷贝**）；
- Go 1.26.11，`replace dm => <local path>` 本地引入（不上游 fs），探针 `dmprobe` 编译通过；
- 连接串 `dm://SYSDBA:SYSDBA001@127.0.0.1:15236` → `Ping OK`，`V$VERSION` 返回 `DM Database Server 64 V8`；
- 语法探针（全部**在 DM8 上跑过**）：

| SQL | 结果 |
| --- | --- |
| `INSERT ... ON CONFLICT (id) DO UPDATE ...` | **Error -2007**（语法错误；COMPATIBLE_MODE 不改变它） |
| `INSERT ... ON CONFLICT DO NOTHING` | **Error -2007** |
| `INSERT ... RETURNING id` | **Error -2007** |
| `CREATE TABLE ... TEXT PRIMARY KEY` | **Error -3207**（CLOB 列不能主键/建索引） |
| `CREATE TABLE ... VARCHAR(16) PRIMARY KEY` + 普通 INSERT | **OK** |
| `CAST(NOW() AS TIMESTAMP WITH TIME ZONE)` | **OK**（类型可用） |
| `'' IS NULL`（Oracle 语义） | 返回 `'empty=null'`（**`''` ≡ NULL，Oracle 血统**） |

驱动版本：随 zip 发布的 `dm` 包（GO 1.13 起跑），接口兼容 `database/sql`，`dm://user:pwd@host:port?schema=...` DSN。

---

## 2. LEVEE 里 PG 专有构造在哪（文件级）

以下为本仓 fnmatch 到的非测试文件，全部落在 **qoder 认领面**：

### 2.1 `ON CONFLICT`（18 处，非测试）

| 文件（相对） | 用途 |
| --- | --- |
| `internal/cluster/distributed_lock.go`（`acquireLockSQL`，§92-103） | **集群租约（fencing fence_token CAS，含 `nextval` 序列 + 条件 UPDATE + `RETURNING fence_token, acquired_at, lease_expires`）** |
| `internal/cluster/exec_guard.go`（`registerExecSQL`，§57-72） | **run 单飞 CAS（`run_execution`：epoch 分配，RETURNING owner, epoch, lease_expires）** |
| `internal/cluster/pg_registry.go`（108/117/130 及 §217） | watchdog 注册/心跳的并发写 CAS |
| `internal/state/pgstore_support.go`（442） | **pgMigrations 的 `record pg schema version`**（`INSERT ... ON CONFLICT`，`migrate.go:422` 同构）——sqlite/pg 双实现切换点 |
| `internal/state/inventory_pg.go`（24/95）、`agents_pg.go`（26）、`inventory_sqlite.go`（27/77，sqlite 为参照实现） | PG 目录表 upsert |
| `internal/cluster/distributed_lock.go`（5/82/92 注释引用） | 文档注释引用，非可执行 SQL |

### 2.2 `TEXT`（LEVEE 把 id 全部存 TEXT）

`internal/state/migrate.go:78,250`（runs/workflows `TEXT PRIMARY KEY`）、`internal/state/pgstore_support.go:271`、`internal/cluster/pg_registry.go:37,54`（watchdog 表 `TEXT PRIMARY KEY`）。DM8 里 TEXT 是 CLOB，主键/索引失败——**每张表的主键都要 `VARCHAR(N)` 化**。

### 2.3 类型语义差异（A 格式 Oracle 血统）

`internal/state/pgstore.go:10,1684`；`pgstore_support.go:275-374`：`TIMESTAMPTZ` → DM8 类型 `TIMESTAMP(9) WITH TIME ZONE` 存在，但 `pgx` 语义与 LEVEE DTO 期望对不齐；且 A 格式下 `'' IS NULL`（实测 `'empty=null'`），LEVEE 到处都显式插 `''` 而非 NULL——**全部直接撞 NOT NULL（`tenant_id`、`tags` 等）**。

### 2.4 其他

- `nextval('cluster_locks_fence_seq')`：序列 + `nextval` 语法在 DM8 可用（Oracle 血统），**这是唯一「不用改」的 PG 专有构造**；
- `NOW() + ($3::bigint * INTERVAL '1 millisecond')`：DM8 同样支持 `INTERVAL` 式，**不用改**；
- `pgx` 类型映射：DM8 驱动不是 `pgx` 而是 `dm`（`database/sql`）——`sql` 驱动下 sql.Null* 与时间类型扫描需重验。

---

## 3. 要改的东西（方言层，真实清单）

| 改动 | 文件 | 性质 |
| --- | --- | --- |
| `INSERT ... ON CONFLICT ... RETURNING` 重写为 DM8 语法：PG `MERGE`（Oracle `MERGE INTO ... USING ... ON ... WHEN MATCHED/NOT MATCHED`）+ 行号回查拿 `fence_token`/`epoch` | `internal/cluster/distributed_lock.go` | **架构级重写，而不是改参数**。CAS 语义（不回退 fence_token、续约保 epoch、过期 takeover）必须逐字符重验 |
| 同上 | `internal/cluster/exec_guard.go` | 同上 |
| 同上 | `internal/cluster/pg_registry.go` | watchdog 写路径 |
| `pgMigrations` 的 `record schema version` 改写 + 全部 `TEXT PRIMARY KEY` → `VARCHAR(N)` | `internal/state/migrate.go`、`pgstore_support.go`、`pg_registry.go` | 迁移层改动序号 + 全部 schema DDL（含全部 `TEXT` 列） |
| `''` → 哨兵或 `NULL` + NOT NULL 列放宽；`TIMESTAMPTZ` 生命周期验证 | `internal/state/*.go`（DTO/写入路径） | 空串语义差异，系统性改动 |
| 驱动接入：`internal/wiring` 新增 `database.driver: dm` 路径（`openStoreFromConfig`、`config.Validate`、DSN 绑定、`LEVEE_DATABASE_DM_DSN`）+ `NewDMStore` | `cmd/levee/`、`internal/wiring/`、`internal/config` | 接入面，机械部分，工作量中 |
| 测试：本地 DM8 容器集成测试（`build tag: dm8` 或本地专项 job），不进默认 CI | `internal/state`/`internal/cluster`（qoder 线） | 验收用 |
| CI/合规：驱动 `dm-go-driver.zip` 不允许再分发、不入仓——只用 `replace` 本地路径或 Gitee 私有制品 | — | 强制约束 |

**总工作量估算**：一位熟悉 LEVEE 的工程师 **15–25 个工作日**，其中 `internal/cluster` fencing 重写与重验占一半；若开 **opt-out 模式**（`database.driver: dm` 时集群 CAS 全部改本地 sqlite/lockfile，DM 仅单节点），降到 **5–8 个工作日**，但等于**把 DM8 限定为单节点、不支持集群 CAS**。

---

## 4. 为什么不在本阶段动代码

1. **所有改动落在 qoder 认领面**：`internal/state`、`internal/cluster`、`internal/wiring`、`cmd/levee`——这些线 2026-11 刚从大重构中稳定（#343–#367），现在再插一轮 DDL + CAS 重写，等于让两条线同时改同一个 CAS 函数。
2. **没有需求来证明这笔钱该花**：目前金仓 V9 PG 模式全绿（含集群 CAS）；达梦没有客户/合同/标的点名。Meyer-Sommerville 规则：不给钱就不排队。
3. **驱动不可再分发 → CI 门禁不能做**：DM8 集成只能本地跑，质量体系里会出现「绿的不敢合」的豁免区。
4. **`ON CONFLICT ... RETURNING` 在 DM8 上不存在**，没有参数能补、没有 GUC 能切；唯一办法是把 LEVEE 最权重的 CAS 构造重写为 `MERGE INTO + 事务内回查`，而这**改语义、不是改方言**——改完后续约 fence_token 的单调性证明要重新写（这正是 `distributed_lock.go` 存在的全部理由）。

**诚实的范围界定**：这份方案文档（`docs/dm8-adaptation-plan.md`）是 opencode 文档线的产物，**它不代表 opencode 认领了 DM8 的代码实现**。若要做，按 §6 走结对立项。

---

## 5. 两条路（选一）

### Route A：完整 DM8 支持（需求明确点名 DM 时才动）

- 需求判据（满足任一，以合同/标的为准）：
  1. 客户交付文档点名「达梦 DM8」作为数据库组件；
  2. 信创招标评分项要求「国产数据库无缝切换」且指定达梦；
  3. 销售侧签了 DM8 前置条款。
- 工作量：15–25 人日；需 qoder 配合（exchange-CAS 重写）+ opencode 接入面 + 本地 DM8 集成测试；不进公开 CI。
- 验收：金仓一列切换到 DM8 的 `scripts/verify_xinchuang_evidence.py` 式复核脚本（ISO → 容器 → doctor → 一条 change `approved→completed`→回滚→audit/verify）；`dm8` 容器跑通全链路后矩阵才可标「已验证（容器，非生产）」。

### Route B：单节点 DM8（成本低，覆盖「试点」场景）

- 限制：单节点、不启集群协调（`--cluster` 仍走 PG/sqlite 或拒绝 DM），fencing CAS 顺用 `distributed_lock.go` 的**sqlite 实现**或本地文件锁——等于 DM 方言只替 PG 同形态的单节点存储。
- 工作量：5–8 人日；改动面见 §3（去掉 `internal/cluster` 那三个文件）。
- 验收范围：单机 doctor、单条 change 全链路、容器内取证；**对外标语仍必须写「达梦 DM8 × 单节点 × 容器，非生产集群」**。

### Route C（当前状态）：不支持，等明确需求

矩阵 §2 保持「不支持（已实测）」。所有信创话术落在金仓 V9 PG 模式。

---

## 6. 若要立 Route A / Route B，要先谈这三件事

1. **范围**：单节点（Route B）还是集群（Route A）？客户实际部署形态决定；
2. **owner**：代码线由 qoder 还是 opencode 认领？qoder 在 DM8 CAS 重写上有 continuation risk；请勿混动；
3. **环境与门禁**：CI 不引入 DM8（合规）；本地 DM8 容器留着，验收靠 `scripts/verify_dm8_evidence.py`（Route A 立项时建）+ 矩阵新格「已验证（容器，非生产）」。

**不再有「零成本」选项**：DM8 要真正进产品，必是正式工程项；本文档即立项的输入。

---

## 7. 复核索引

- 容器：`docker start levee-dm8`（`dm8_single`，端口 15236，SYSDBA / SYSDBA001）；
- 驱动探针源码：`C:\Users\winge\AppData\Local\Temp\opencode\dmprobe\probe3.go`，二进制 `dmprobe3.exe`；
- 金仓参照（唯一可用信创 DB 位）：`docs/platform-support-matrix.md` §2（`kingbase_v009r001c010b0004`，`DB_MODE=pg`）；
- openGauss 对照：`docs/opengauss-probe-2026-11.md`（同为「已实测不支持」，A 格式 `''=NULL`，B/C 拒 `ON CONFLICT`）；
- 问题清单（leveller 门禁渲染）：`docs/gates.md` S1 策略门禁一节，DM8 无直接相关项。
