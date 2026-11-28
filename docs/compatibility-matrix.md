# 动作兼容矩阵与目标自诊断

本文是 `docs/compatibility-matrix.json`（`levee.compatibility/v1`）的导读。**JSON 是机器判据，本文不是第二份动作清单**；`scripts/test_compatibility_matrix.py` 会逐动作核对 Go `Actions()`、环境等级和真实回放证据。

## 1. 三种不同的「支持」

| 层级 | 含义 | 可以用于生产承诺吗 |
| --- | --- | --- |
| `unit` | 单元/契约测试通过 | 不能直接承诺真实目标兼容 |
| `lab` | 容器/CI 集成或故障场景通过 | 可以说明实验室证据及边界 |
| `real-single` | 真实目标、真实通道、真实单实例回放通过 | 可以说明真实单实例证据，不等于 HA/生产验收 |
| `unverified` | 有实现，但证据不足 | 不可承诺 |
| `unsupported` | 当前实现明确没有该路径 | 必须拒绝，不得写成支持 |

另有两个不同口径：

- `levee capabilities` 的 **live-probe** 是目标当前状态，不是历史验收证书。
- `unknown`（例如 `mysql.replica_switch` 缺真实拓扑）**不计入 supported**。

## 2. 目标自诊断

```sh
levee capabilities --target db01.prod
levee capabilities db01.prod --json
```

命令复用执行链的 inventory、credential resolver 和 `Engine.Dial`，只执行 `which`、`--version`、`--help`、`uname`、`cat /etc/os-release` 和 `test` 等只读命令。输出 `levee.capabilities/v1`：

- `package_manager`：apt/dnf/yum/apk 的实际探测结果；
- `init_system`：systemd/sysvinit 或未探测到；
- `tools`：psql、pg_dump、mysql、pt-online-schema-change 版本；
- `actions`：29 个动作的 `supported/unsupported/unknown + reason`；
- `summary`：三类计数，unknown 不会被算作支持。

命令不支持 `--remote`：它刻意探测本地 inventory 对应的真实通道，避免把远端配置和本机执行环境混为一谈。

## 3. 当前实证入口

- PG schema：`python scripts/replay_pg_schema.py`（PostgreSQL 16.15，REAL_PG_REPLAY + REAL_PG_SET_REPLAY，含 plain IDENTITY 与循环跨表 FK）。
- MySQL schema：`python scripts/replay_mysql_schema.py`（MySQL 8.0.46，捕获→DROP→恢复→定义一致，MYSQL_SCHEMA_REPLAY）。
- apk：`python scripts/replay_pg_schema.py --suite pkg-alpine`（tree 安装、重装、升级、删除、重复删除，PKG_APK_REPLAY）。
- 自诊断：`python scripts/replay_pg_schema.py --suite capabilities-alpine`（CAPABILITY_REPLAY）。

这些入口共用一次性 SSH 容器、临时密钥和 strict known_hosts，结束后自动清理。它们证明真实单实例，不证明生产集群。

## 4. 当前必须拒绝的误读

- WinRM 通道存在，不代表 `file/pkg/svc/user/shell` 自动支持 Windows；JSON 中这些动作的 Windows 栏均为 `unsupported`。
- apk 已有真实实现，不再只是注释；探测顺序仍为 apt→dnf→yum→apk。
- PG DDL 恢复已有真实单实例证据；plain IDENTITY 与表组跨表 FK 已覆盖，行数据、自定义 IDENTITY 序列选项、分区和生产 HA 仍不在范围。
- MySQL `replica_switch` 需要真实主从拓扑，不能用客户端存在性判 supported。
