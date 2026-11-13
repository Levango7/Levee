# openGauss 探针结论（2026-11-13）

| 元信息项 | 内容 |
| --- | --- |
| 状态 | **不支持（已实测）** |
| 环境 | 麒麟 V11（Swan25）guest，SSH `127.0.0.1:22223`，openGauss-lite **7.0.0**（2397a86f），openEuler 24.03 x86_64 构建 |
| 结论 | LEVEE 现有 PG 路径在 openGauss 7.0 全部三种兼容格式上都无法跑通迁移或关键写入；需要方言适配（与达梦同级工程量），或上游修复 |

## 1. 装得起来吗？可以，但要过三关

openGauss-lite 7.0.0（`openGauss-Lite-7.0.0-openEuler24.03-x86_64.tar.gz`，47 MB，官方 OBS 直链 `opengauss.obs.cn-south-1.myhuaweicloud.com/7.0.0/...`）在麒麟 V11 guest 里可以跑通。过程中实测到的坑，照登：

1. **安装器拒绝 root**（`install.sh`：`can not install gauss with root`），必须另建非 root 用户（本实验用 `omm`）；
2. 依赖要补两个包：`dnf install openblas libatomic`（Kylin V11 仓库里有：openblas-0.3.25-5.ky11、libatomic-12.3.1-62.p05.ky11）；
3. 安装后 gs_guc/gs_ctl 也必须以 omm 身份跑（root 会被拒）。

安装与首启：`gs_initdb -D /home/omm/og/data --nodename=opengauss` → `gs_ctl start/stop/restart`；HA/db_state 显示 Normal。默认密码加密 `password_encryption_type=2`（SCRAM），**pgx 无法握手**（"AuthenticationSASL body is invalid unterminated string"）；改 `password_encryption_type=0`（MD5）后 `ALTER USER omm PASSWORD ...` 重加密才连上。

## 2. 三个兼容格式逐个实测（结论都是「不行」，但原因不同）

| 格式 | 迁移（`pgMigrations`） | 写入 | 结论 |
| --- | --- | --- | --- |
| A（默认 Oracle 式） | ✅ 通过（`levee system doctor` 期间迁移全跑）| ❌ `create credential` 失败：`null value in column "tags" violates not-null constraint（SQLSTATE 23502）` | 不支持 |
| C（标准式） | ❌ `INSERT ON CONFLICT is not supported in C_FORMAT（SQLSTATE 0A000）` | — | 不支持 |
| B（MySQL 式） | ❌ `INSERT ON CONFLICT is not supported in B_FORMAT（SQLSTATE 0A000）` | — | 不支持 |

根因要分开说：

- **A 格式（Oracle 语义）：空字符串 `''` 就是 NULL**（Oracle 遗产）。LEVEE PG 路径里到处都是 `tags TEXT NOT NULL DEFAULT ''` + 显式插 `''` 的地方——`''` 落库即 NULL → 违反 NOT NULL（凭据 `tags` 列实测撞车）。无 GUC 可切换（这是语义层，不是配置）。DDL 迁移本身能跑，所以 doctor 一度 healthy，然后在凭据写入时塌。
- **B/C 格式**：`INSERT ... ON CONFLICT` 语法不被接受（0A000）。LEVEE 的 `pgMigrations` 里必有该语法（`state: record pg schema version 10` 一步即死）——不进场谈。
- A 格式不支持 base pgx 握手以外的"好消息"是真的：迁移、租约 CAS、`RETURNING`、布尔、`TIMESTAMPTZ` 类型本身在 A 格式都能用——只差 Oracle 的 `''=NULL` 语义。

## 3. 如果一定要支持 openGauss，要走什么路

等同 DM8 的一条路子，二选一，都不小：

1. **LEVEE 侧方言层**：禁止一切空串字面量（LEVEE 无入参空串 → 改用 `NULL` 或哨兵 `'-'`）、ON CONFLICT 改写为 `MERGE INTO` / 先查后写；动 `internal/state`（qoder 线）+ `internal/cluster` 租约 CAS；相当于再做一遍 DM8 项目（2–4 周量级）；
2. **上游 openGauss 支持**：跟进 openGauss 7.x/8.x 是否放宽 C_FORMAT 的 ON CONFLICT 或提供 PG 兼容 GUC；或等 `DBCOMPATIBILITY='PG'`（目前不存在）。

**在「顾问顺序」之下，结论是：openGauss 与 DM8 同档——均为已实测不支持，信创 DB 覆盖靠金仓 V9 PG 模式（单实例）。** 有标的点名 openGauss 再立项，别现在动 internal/state。

## 4. 复现索引

- guest：麒麟 V11 `127.0.0.1:22223`（openGauss-lite 装在 `/home/omm/og/`，数据目录 `/home/omm/og/data`）；**gaussdb 已停（`gs_ctl stop`），660 MB 磁盘占用，零 RAM 成本**；
- **保留清单**：`omm` 用户、`T_C` / `T_B`（原样保留供复测）、`postgres` / `template*`；一次性探针残留（`t_c`/`t_b` 小写孤儿库、`probe_*` 表）已清理；
- 配置：guest 内 `/root/vm/config-og.yaml`（DSN 指向 `postgres` 库，MD5 口令 `LeveeOg@2026x`）；
- 探针脚本：guest 内 `/root/omm-*.sh`（安装、gsql、md5、compat、drill-og）；
- 实测表：`T_C`、`T_B`（强制大写名以绕开 gsql 大小写折叠）、以及遗留的 `t_c`/`t_b`（小写，未清理）；`probe_conf`、`probe_bool`、`probe_ret`、`probe_tstz`、`credentials`；
- 板面：见 `opencode-xinchuang-vm-os-drill.md`（本任务记录）。
