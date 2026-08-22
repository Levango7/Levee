# LEVEE CLI 命令与 API 设计

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE CLI 命令与 API 设计 |
| 文档类型 | API / 命令设计文档 |
| 版本 | v0.1 |
| 日期 | 2026-08-15 |
| 关联文档 | levee-design.md（LEVEE 设计文档 v1.0） |
| 适用范围 | LEVEE CLI 全部命令、REST API V1 端点、配置与退出码规范 |

---

## 第1章 设计原则

### 1.1 CLI 优先

LEVEE 遵循 R2 红线"变更必须可回滚"与 D2 中"CLI 优先"的体验约束：所有变更生命周期操作、模板管理、目标管理、审计、漂移检测、凭据、权限、系统管理操作均可通过 CLI 完成，不强制 Web UI。Web UI（V1 门户）是 CLI 的可视化层，不引入 CLI 没有的能力，二者共用同一编排后端。

CLI 输出结构化：默认人类可读（表格 + 彩色状态），`--json` 切换机器可读，便于嵌入 CI / CD 与脚本。所有命令均可在批处理 / 流水线中调用，退出码语义稳定（见第11章）。

### 1.2 命令风格

采用子命令式风格，统一前缀 `levee`，层级不超过三层：

```
levee <command> [subcommand] [positional] [options]
```

命令名采用小写连字符（kebab-case），选项采用 `--long` 形式（短选项仅保留高频 `-f` / `-o` / `-h`）。子命令按资源域分组（变更、模板、目标、审计、漂移、凭据、权限、系统），跨域操作挂在顶层（如 `apply` / `rollback` 属变更域）。

### 1.3 输出格式

默认人类可读：列表用表格，单对象用字段对齐块，状态用彩色（绿 = 成功 / 通过，黄 = 进行中 / 警告，红 = 失败 / 驳回，灰 = 跳过 / 归档）。`--json` 输出结构化 JSON（见第12章），`--quiet` 仅输出对象 ID，`--verbose` 输出调试日志。

### 1.4 退出码规范

退出码语义稳定，脚本可按退出码分支处理，详见第11章。

### 1.5 配置文件

全局配置位于 `~/.levee/config.yaml`，支持多 profile 切换（`--profile <name>`），环境变量 `LEVEE_*` 覆盖配置项，详见第14章。

---

## 第2章 变更生命周期命令

变更生命周期对应设计文档 D4 闭环：`plan → 审批 → apply → verify → (回滚 | 归档)`。本章命令覆盖该闭环的全部操作，包括创建、克隆、查看、审批、执行、暂停 / 恢复、取消 / 重试、回滚、日志诊断、归档关联。

### 2.1 创建变更

从 LEVEELang 文件或模板创建新变更（对应 D2 模板实例化）。

命令示例：从 LEVEELang 文件创建变更

```bash
levee new --file ./workflows/db-migrate-orders.leveelang \
  --label "orders-ddl-20260815" \
  --priority normal \
  --params table=orders,batch_pct=10%
```

命令示例：从模板实例化创建变更

```bash
levee new --template db-migrate \
  --params table=orders,batch_pct=10%,grace_period=5m \
  --label "orders-ddl-20260815"
```

表：new 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--file <path>` | path | 与 `--template` 二选一 | LEVEELang workflow 文件路径 |
| `--template <name>` | string | 与 `--file` 二选一 | 已注册模板名，实例化时填参数 |
| `--params key=val,...` | map | 否 | 输入参数，编译期校验类型与完整性 |
| `--dry-run` | bool | 否 | 仅产出执行计划，不进审批不执行 |
| `--label <text>` | string | 否 | 人类可读标签，便于检索 |
| `--priority low|normal|high|urgent` | enum | 否 | 优先级，影响调度顺序，默认 normal |
| `--target-group <expr>` | string | 否 | 覆盖 workflow 内 target 查询，限定目标集 |
| `--window <start>-<end>` | range | 否 | 覆盖 workflow 内变更窗口 |

`--dry-run` 等价于直接调用 `levee plan`，产出目标集、批次划分、影响面、预估耗时、潜在冲突报告，不创建 run。

### 2.2 克隆变更

基于历史 run 生成可编辑副本，保留原参数与批次结构，用于"上次成功再来一次"（对应 D2 变更克隆）。

命令示例：克隆历史变更

```bash
levee clone run-20260812-001 \
  --override table=order_items,batch_pct=5%
```

表：clone 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<change-id>` | string | 是 | 源变更 run-id |
| `--override key=val,...` | map | 否 | 覆盖原参数，未覆盖项沿用原值 |
| `--new-label <text>` | string | 否 | 新变更标签，默认沿用原标签加 `-clone` 后缀 |

克隆产出新的 run-id，状态为 `draft`，需重新走 plan + 审批闭环，不复用原审批。

### 2.3 查看变更

命令示例：查看单条变更详情

```bash
levee show run-20260815-001
```

输出含：基本信息、plan 摘要、plan_hash、审批状态、批次进度、门禁结果、当前阶段、关联工单。

命令示例：列出变更

```bash
levee list --status running,approved --target group=web --limit 20
```

表：list 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--status <s1,s2,...>` | enum list | 否 | 按状态过滤：pending / approved / rejected / running / paused / completed / failed / rolled_back / archived |
| `--target <label=val>` | label | 否 | 按目标标签过滤，如 `group=web`、`az=a` |
| `--template <name>` | string | 否 | 按模板名过滤 |
| `--initiator <user>` | string | 否 | 按发起人过滤 |
| `--from <date>` | date | 否 | 起始时间，ISO8601 |
| `--to <date>` | date | 否 | 截止时间 |
| `--limit <n>` | int | 否 | 返回条数，默认 20 |
| `--offset <n>` | int | 否 | 偏移量，分页 |

### 2.4 审批变更

对应 D4 审批阶段分级（标准 / 高危 / 紧急）。

命令示例：通过审批

```bash
levee approve run-20260815-001 --comment "影响面已确认，SLO 基线正常"
```

命令示例：驳回审批

```bash
levee reject run-20260815-001 --reason "未提供回滚演练记录，驳回重提"
```

命令示例：转授权

```bash
levee delegate run-20260815-001 --to user-b --reason "出差，转 user-b 审批"
```

表：审批命令选项参数说明表

| 命令 | 选项 | 必填 | 说明 |
| --- | --- | --- | --- |
| `approve` | `--comment <text>` | 否 | 审批意见，留痕 |
| `reject` | `--reason <text>` | 是 | 驳回理由，发起人基于理由修改重提 |
| `delegate` | `--to <user>` | 是 | 被转授权人，须有审批权限，不能再转 |

高危审批要求至少 2 人审批且不能是发起人，第二审批人通过同一 `approve` 命令追加审批。紧急审批通道超时自动驳回并升级 oncall。

### 2.5 执行变更

命令示例：生成执行计划（不执行）

```bash
levee plan run-20260815-001
```

`plan` 产出目标预检、影响面分析、冲突检测、动态目标集物化、plan_hash 锁定结果，不进入 apply。

命令示例：执行已审批变更

```bash
levee apply run-20260815-001
```

`apply` 前强制校验 plan_hash 与审批时一致，不一致阻断（防止审批后偷改参数）。apply 阶段依次：快照创建 → 分批执行 → 批次间门禁 → 全量门禁 → 归档。

表：apply 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<change-id>` | string | 是 | 已审批变更 run-id |
| `--batch <n>` | int | 否 | 仅执行到第 n 批后暂停，用于手动分批推进 |
| `--skip-gate <name>` | string list | 否 | 跳过指定门禁，需管理员权限，留痕 |
| `--no-rollback` | bool | 否 | 失败不自动回滚，需管理员权限，留痕 |

### 2.6 暂停 / 恢复

对应 D4 主动暂停与恢复（4.4.4.4）和全局暂停（4.4.7）。

命令示例：暂停单条变更

```bash
levee pause run-20260815-001
```

暂停后已完成批次保留，未开始批次不进。暂停 / 恢复留痕，需有权限操作。

命令示例：恢复单条变更

```bash
levee resume run-20260815-001
```

从暂停点恢复，不重跑已完成批次。

命令示例：全局暂停（紧急止血）

```bash
levee pause-all --reason "全局 SLO 异常，暂停所有变更止血"
```

`pause-all` 暂停所有进行中的 run，需高权限操作，留痕。用于全局故障止血。

命令示例：全局恢复

```bash
levee resume-all
```

恢复所有因 `pause-all` 暂停的 run，单独 `pause` 的 run 不受影响。

### 2.7 取消 / 重试

对应 D4 操作全集（4.4.8）中的 cancel、retry，以及单台重跑（4.4.4.5）。

命令示例：取消变更

```bash
levee cancel run-20260815-001 --reason "需求变更，取消执行"
```

取消未开始批次，已开始批次按策略（等待完成 / 强制中断）处理。需发起人或管理员权限。

命令示例：重试失败批次

```bash
levee retry run-20260815-001
```

从失败步骤继续，重试失败批次。重试次数有上限（默认 3），超限升级人工。

命令示例：单台重跑

```bash
levee retry-host run-20260815-001 web-03.example.com
```

批次内某台失败可单独重跑，不影响批次其他主机状态。对应 D4 单台重跑（4.4.4.5）。

### 2.8 回滚

对应 D4 回滚协议（4.4.6）：白名单 + 快照 + 按批逆序。

命令示例：按批回滚

```bash
levee rollback run-20260815-001 --batch 3
```

回滚指定批次（按批逆序，从第 3 批向前回滚到指定点）。回滚不受原 workflow 变更窗口约束。

命令示例：全量回滚

```bash
levee rollback-all run-20260815-001
```

回滚所有批次，按批逆序逐批回滚，每批回滚后做回滚后验证。回滚完成后不自动归档，需人工确认（对应 4.4.6.6）。

表：rollback 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<change-id>` | string | 是 | 变更 run-id |
| `--batch <n>` | int | 否 | 仅回滚到第 n 批，省略则全量回滚 |
| `--force` | bool | 否 | 跳过回滚后验证，需管理员权限，留痕 |

### 2.9 日志与诊断

对应 D4 实时日志（4.4.4.6）和 D7 审计 trace。

命令示例：查看变更日志

```bash
levee logs run-20260815-001 -f --host web-03.example.com --step migrate
```

表：logs 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<change-id>` | string | 是 | 变更 run-id |
| `-f, --follow` | bool | 否 | 实时跟随日志输出 |
| `--host <host>` | string | 否 | 仅显示指定目标机日志 |
| `--step <step>` | string | 否 | 仅显示指定步骤日志 |
| `--batch <n>` | int | 否 | 仅显示指定批次日志 |
| `--since <time>` | duration | 否 | 仅显示指定时间后日志 |

命令示例：变更前后 diff

```bash
levee diff run-20260815-001
```

对比变更前后目标机状态差异（基于快照）。两个 run-id 对比时：

```bash
levee diff run-20260812-001 run-20260815-001
```

对比两次 run 的参数与执行计划差异，对应 D4 操作全集（4.4.8）中的 diff。

命令示例：查看执行 trace

```bash
levee trace run-20260815-001
```

输出完整执行 trace：每个动作的输入、输出、耗时、目标机上下文、哈希链。对应 D7 审计 trace（4.7.1）。

### 2.10 归档与关联

命令示例：归档变更

```bash
levee archive run-20260815-001
```

归档需人工确认（回滚完成后不自动归档，对应 4.4.6.6）。归档写入 WORM 存储，不可篡改。

命令示例：关联 ITSM / Jira 工单

```bash
levee link run-20260815-001 --itsm CHG-2026-0815 --jira INFRA-1234
```

对应 D4 操作全集（4.4.8）中的 link，关联变更与 incident / 工单，用于审批留痕同步与复盘。

表：link 命令选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<change-id>` | string | 是 | 变更 run-id |
| `--itsm <ticket>` | string | 否 | ITSM 工单号（ServiceNow / 自研） |
| `--jira <issue>` | string | 否 | Jira issue key |
| `--incident <id>` | string | 否 | 事件 id，用于变更-告警关联 |

---

## 第3章 模板管理命令

模板管理对应 D2 模板实例化机制，模板带参数占位，实例化时填入具体值，编译期校验参数完整性。

命令示例：列出模板

```bash
levee template list --tag db --limit 50
```

命令示例：查看模板详情

```bash
levee template show db-migrate
```

输出含：模板参数 schema、默认值、目标集声明、批次策略、审批级别、回滚声明。

命令示例：创建模板

```bash
levee template create --file ./templates/db-migrate.yaml --name db-migrate
```

命令示例：校验模板

```bash
levee template validate db-migrate
```

校验模板参数 schema 完整性、LEVEELang 编译期类型检查、回滚声明完备性。校验失败退出码 5。

表：template 子命令对照表

| 子命令 | 语义 | 权限 |
| --- | --- | --- |
| `list` | 列出已注册模板 | 任一用户 |
| `show <name>` | 查看模板详情 | 任一用户 |
| `create --file <path>` | 注册新模板 | 模板管理员 |
| `validate <name>` | 校验模板合法性 | 任一用户 |
| `update --file <path>` | 更新模板（生成新版本） | 模板管理员 |
| `delete <name>` | 删除模板（软删，保留历史 run 引用） | 模板管理员 |
| `version <name>` | 查看模板版本历史 | 任一用户 |

---

## 第4章 目标管理命令

目标管理对应 D1 执行通道与目标机抽象层（4.1.4）。

命令示例：列出目标机

```bash
levee target list --label group=web --label az=a --status reachable
```

表：target list 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--label key=val` | label | 否 | 按标签过滤，可多次指定 |
| `--status <s>` | enum | 否 | reachable / unreachable / unknown |
| `--channel <type>` | enum | 否 | ssh / winrm / agent / api / interactive |
| `--limit <n>` | int | 否 | 返回条数 |

命令示例：目标机连通性检查

```bash
levee target check web-03.example.com --timeout 10s
```

执行建连 + noop 预检，对应 D1 目标可达性预检（4.1.7）。输出通道类型、握手耗时、认证方式、可达性结论。

命令示例：导入主机清单

```bash
levee target import --file ./hosts.yaml --merge
```

表：target import 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--file <path>` | path | 是 | 主机清单 YAML 文件 |
| `--merge` | bool | 否 | 合并模式，已存在主机更新标签，不删除 |
| `--replace` | bool | 否 | 替换模式，未在清单中的主机标记为 unmanaged |
| `--dry-run` | bool | 否 | 仅展示变更不落库 |

### 4.4 target remove

移除目标机并清理相关状态。对应设计文档 4.3.2 显式动作 remove（移除并清理），需走审批闭环。

命令示例：移除目标机并清理相关状态

```bash
levee target remove <host> [--force]
```

表：target remove 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<host>` | string | 是 | 目标机主机名或地址 |
| `--force` | bool | 否 | 跳过确认提示（不推荐），需管理员权限，留痕 |

说明：
- 需审批权限（高危操作）
- 移除后该目标机不再被管理，相关状态记录清理
- `--force` 跳过确认提示（不推荐）

### 4.5 target unmanage

停止管理目标机但保留状态记录。对应设计文档 4.3.2 显式动作 unmanage（仅停止管理），需走审批闭环。

命令示例：停止管理目标机但保留状态记录

```bash
levee target unmanage <host>
```

表：target unmanage 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `<host>` | string | 是 | 目标机主机名或地址 |

说明：
- 需审批权限
- 停止管理后不再执行变更和漂移检测，但历史审计记录保留
- 可通过 `target import` 重新纳入管理

---

## 第5章 审计与合规命令

对应 D7 可观测与审计（4.7）：每次 apply 产出 trace + 哈希链，写入 WORM 存储。

命令示例：列出审计记录

```bash
levee audit list --from 2026-08-01 --to 2026-08-15 --who user-a --action apply
```

表：audit list 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--from <date>` | date | 否 | 起始日期 |
| `--to <date>` | date | 否 | 截止日期 |
| `--who <user>` | string | 否 | 操作人 |
| `--action <type>` | enum | 否 | plan / approve / apply / rollback / pause / resume / cancel |
| `--change <id>` | string | 否 | 按变更 run-id 过滤 |
| `--limit <n>` | int | 否 | 返回条数 |

命令示例：查看单条变更审计详情

```bash
levee audit show run-20260815-001
```

输出完整审计链：plan、审批、apply 各批次、门禁结果、回滚（如有）、归档，含哈希链。

命令示例：导出审计记录

```bash
levee audit export --format json --from 2026-08-01 --to 2026-08-15 -o ./audit-202608.json
```

支持 json / csv 两种格式，用于合规报送与外部审计平台对接。

命令示例：哈希链校验

```bash
levee audit verify run-20260815-001
```

校验 trace 哈希链完整性，任何中间篡改都会断链并报错。对应 D7 哈希链（4.7.1）与哈希链分片（4.7.2）。

---

## 第6章 漂移检测命令

对应 D3 状态与漂移（简化）：不做主动漂移检测，只读漂移报告。

命令示例：触发漂移扫描

```bash
levee drift scan --target group=web --baseline ./baselines/web-20260801.yaml
```

对指定目标集做一次性比对，产出报告，不自动纠正。报告含：每台目标机的期望状态、实际状态、差异项、严重级别。

命令示例：查看漂移报告

```bash
levee drift show drift-20260815-001
```

表：drift scan 选项参数说明表

| 选项 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `--target <label>` | label | 是 | 目标集标签表达式 |
| `--baseline <file>` | path | 是 | 基线状态文件 |
| `--depth shallow|deep` | enum | 否 | shallow 仅比对关键项，deep 全量比对，默认 shallow |

---

## 第7章 凭据管理命令

对应 D8 安全与集成（4.8.1 凭据代理）。LEVEE 不直接持有目标机凭据，通过凭据代理（Vault / CyberArk / 自研 4A）按需获取，用完即弃。凭据管理命令操作的是凭据代理中的引用，不触碰凭据明文。

> 阶段说明：MVP 阶段采用本地 AES-GCM 加密存储凭据（见 mvp-tasks T047），凭据代理（Vault / CyberArk / 自研 4A）在 V1 引入。本章命令同时覆盖两种模式。

命令示例：列出凭据引用

```bash
levee secret list --type ssh-key --team infra
```

命令示例：注册凭据引用

```bash
levee secret add --name web-ssh-key --type ssh-key --file ~/.ssh/web_id_ed25519 --team infra
```

`--file` 内容上传至凭据代理，本地不留存。LEVEE 仅记录引用句柄，不存明文。

命令示例：轮换凭据

```bash
levee secret rotate --name web-ssh-key
```

触发凭据代理侧轮换，LEVEE 不感知轮换周期，仅在凭据获取失败时按 R7 产出结构化失败，不缓存旧凭据（对应 4.8.5）。

表：secret 子命令对照表

| 子命令 | 语义 | 权限 |
| --- | --- | --- |
| `list` | 列出凭据引用 | 凭据管理员 |
| `add` | 注册凭据引用 | 凭据管理员 |
| `rotate` | 触发轮换 | 凭据管理员 |
| `revoke` | 吊销凭据 | 凭据管理员 |
| `show` | 查看凭据元信息（不含明文） | 凭据管理员 |

---

## 第8章 权限管理命令

命令示例：列出用户

```bash
levee user list --team infra
```

命令示例：添加用户

```bash
levee user add --name user-a --team infra --role operator
```

命令示例：列出团队

```bash
levee team list
```

命令示例：添加团队

```bash
levee team add --name infra --env prod --approver user-b
```

表：角色权限对照表

| 角色 | 可执行操作 | 不可执行操作 |
| --- | --- | --- |
| `viewer` | list / show / logs / trace / audit | new / approve / apply / rollback / pause / cancel |
| `operator` | viewer 全部 + new / clone / apply / pause / resume / retry | approve / rollback-all / pause-all / secret / user |
| `approver` | viewer 全部 + approve / reject / delegate | new / apply / rollback / pause-all |
| `admin` | 全部操作 | 无 |

---

## 第9章 系统管理命令

命令示例：查看版本

```bash
levee version
```

输出 LEVEE 版本、构建时间、Git commit、Go 版本、部署形态（单机 / 集群 / 气隙）。

命令示例：读写配置

```bash
levee config get server.url
levee config set output.color false
```

命令示例：系统状态

```bash
levee status
```

输出系统状态：部署形态、store 类型与连通性、进行中 run 数、近期成功率、凭据代理连通性、集成（Prometheus / ITSM / Vault）连通性。

命令示例：环境检查

```bash
levee doctor
```

检查运行环境：配置文件合法性、store 连通性、凭据代理可达性、通道插件加载、网络隔离区跳板机配置、WORM 存储可写性。用于安装后自检与故障排查。

---

## 第10章 全局选项

全局选项可在任意命令前 / 后使用，优先级：命令行显式参数 > 环境变量 > 配置文件 profile > 配置文件默认。

表：全局选项对照表

| 选项 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `--config <path>` | path | `~/.levee/config.yaml` | 配置文件路径 |
| `--profile <name>` | string | `default` | 配置 profile，用于多环境切换 |
| `--json` | bool | false | 输出 JSON 格式，机器可读 |
| `--quiet` | bool | false | 静默模式，仅输出对象 ID |
| `--verbose` | bool | false | 输出调试日志 |
| `--no-color` | bool | false | 禁用彩色输出，用于管道 / 日志归档 |
| `--timeout <duration>` | duration | `30m` | 单命令超时，超时退出码 8 |
| `--api <url>` | string | 配置文件 | 后端 API 地址，用于 CLI 直连集群形态 server |
| `--token <token>` | string | 配置文件 | API token，CLI 认证用 |

---

## 第11章 退出码规范

退出码语义稳定，脚本可按退出码分支处理。0 表示成功，非 0 表示失败，不同失败原因对应不同退出码。

表：退出码对照表

| 退出码 | 含义 | 典型场景 |
| --- | --- | --- |
| 0 | 成功 | 命令正常完成 |
| 1 | 一般错误 | 未分类错误，详见 stderr |
| 2 | 配置错误 | 配置文件解析失败、profile 不存在、必填项缺失 |
| 3 | 认证错误 | token 无效 / 过期、权限不足 |
| 4 | 审批被拒 | `approve` 被驳回、审批超时、审批人不合规 |
| 5 | 验证失败 | plan 预检失败、模板校验失败、参数类型不匹配 |
| 6 | 回滚失败 | 回滚本身失败、快照损坏、回滚后验证失败 |
| 7 | 连接失败 | 目标机不可达、通道握手失败、凭据代理不可达 |
| 8 | 超时 | 命令超时、门禁超时、审批超时 |

脚本示例：

```bash
levee apply run-20260815-001
case $? in
  0)   echo "变更成功" ;;
  6)   echo "回滚失败，需人工介入" ; notify_oncall ;;
  7)   echo "连接失败，检查目标机可达性" ;;
  8)   echo "超时，检查网络或调大 --timeout" ;;
  *)   echo "失败，退出码 $?" ;;
esac
```

---

## 第12章 输出格式

### 12.1 人类可读格式（默认）

默认输出表格 + 彩色状态，适合终端交互。

命令示例：人类可读输出示例

```bash
$ levee list --status running
```

输出示例：

```
CHANGE ID            TEMPLATE      STATUS    PROGRESS    INITIATOR    UPDATED
run-20260815-001     db-migrate    running   2/3 batches user-a       2026-08-15 02:14
run-20260815-002     os-patch      running   1/5 batches user-b       2026-08-15 02:18
```

状态颜色：running 黄、completed 绿、failed 红、paused 灰、rolled_back 红。

### 12.2 JSON 格式（--json）

`--json` 输出结构化 JSON，便于脚本与 CI / CD 消费。所有命令统一外层结构：`data` 为业务数据，`meta` 为分页 / 耗时等元信息，`error` 为错误信息（成功时为 null）。

命令示例：JSON 输出结构

```bash
$ levee list --status running --json
```

输出示例：

```json
{
  "data": [
    {
      "change_id": "run-20260815-001",
      "template": "db-migrate",
      "status": "running",
      "progress": { "batch_done": 2, "batch_total": 3 },
      "initiator": "user-a",
      "plan_hash": "sha256:9f4b...",
      "created_at": "2026-08-15T02:10:00+08:00",
      "updated_at": "2026-08-15T02:14:32+08:00"
    }
  ],
  "meta": {
    "total": 2,
    "limit": 20,
    "offset": 0,
    "elapsed_ms": 38
  },
  "error": null
}
```

错误时结构：

```json
{
  "data": null,
  "meta": null,
  "error": {
    "code": 7,
    "message": "target unreachable: web-03.example.com",
    "detail": {
      "host": "web-03.example.com",
      "channel": "ssh",
      "reason": "connection refused"
    }
  }
}
```

---

## 第13章 REST API（V1 门户用）

### 13.1 API 风格

RESTful 风格，支持两套路径：

- **RESTful 路径**（前端/Web UI 默认使用）：无前缀，如 `/changes`、`/templates/:name`、`/targets/:id`、`/audit/log`、`/system/version`。
- **gRPC 兼容路径**（向后兼容）：带版本前缀 `/api/v1/`，如 `/api/v1/ChangeService/ListChanges`。

资源命名用复数名词，子资源用路径嵌套（`/changes/:id/approve`）。HTTP 方法语义：GET 查询、POST 创建 / 动作、DELETE 删除。

所有端点返回统一 JSON 结构（同 12.2），HTTP 状态码与退出码对齐：200 成功、400 验证失败、401 认证失败、403 权限不足、404 不存在、409 冲突（如目标机互斥锁占用）、422 审批被拒、500 一般错误、503 连接失败、504 超时。

### 13.2 端点清单

表：REST API 端点对照表（RESTful 路径 / gRPC 兼容路径）

| 方法 | RESTful 路径 | gRPC 兼容路径 | 语义 | 对应 CLI |
| --- | --- | --- | --- | --- |
| POST | `/changes` | `/api/v1/ChangeService/CreateChange` | 创建变更 | `levee new` |
| GET | `/changes` | `/api/v1/ChangeService/ListChanges` | 列出变更 | `levee list` |
| GET | `/changes/:id` | `/api/v1/ChangeService/GetChange` | 查看变更详情 | `levee show` |
| POST | `/changes/:id/plan` | `/api/v1/ChangeService/PlanChange` | 生成执行计划 | `levee plan` |
| POST | `/changes/:id/apply` | `/api/v1/ChangeService/ApplyChange` | 执行变更 | `levee apply` |
| POST | `/changes/:id/approve` | `/api/v1/ChangeService/ApproveChange` | 通过审批 | `levee approve` |
| POST | `/changes/:id/reject` | `/api/v1/ChangeService/RejectChange` | 驳回审批 | `levee reject` |
| POST | `/changes/:id/pause` | `/api/v1/ChangeService/PauseChange` | 暂停 | `levee pause` |
| POST | `/changes/:id/resume` | `/api/v1/ChangeService/ResumeChange` | 恢复 | `levee resume` |
| POST | `/changes/:id/cancel` | `/api/v1/ChangeService/CancelChange` | 取消 | `levee cancel` |
| POST | `/changes/:id/retry` | `/api/v1/ChangeService/RetryChange` | 重试 | `levee retry` |
| POST | `/changes/:id/rollback` | `/api/v1/ChangeService/RollbackChange` | 回滚 | `levee rollback` |
| POST | `/changes/:id/archive` | `/api/v1/ChangeService/ArchiveChange` | 归档 | `levee archive` |
| GET | `/changes/:id/logs` | `/api/v1/ChangeService/GetLogs` | 查看日志 | `levee logs` |
| GET | `/changes/:id/trace` | `/api/v1/ChangeService/GetTrace` | 查看 trace | `levee trace` |
| POST | `/changes/deeplink/approve` | — | 一键审批（移动端） | — |
| POST | `/templates` | `/api/v1/TemplateService/CreateTemplate` | 创建模板 | `levee template create` |
| GET | `/templates` | `/api/v1/TemplateService/ListTemplates` | 列出模板 | `levee template list` |
| GET | `/templates/:name` | `/api/v1/TemplateService/GetTemplate` | 查看模板 | `levee template show` |
| DELETE | `/templates/:name` | `/api/v1/TemplateService/DeleteTemplate` | 删除模板 | — |
| POST | `/templates/instantiate` | `/api/v1/TemplateService/InstantiateTemplate` | 实例化模板 | — |
| POST | `/targets` | `/api/v1/TargetService/AddTarget` | 添加目标机 | `levee target add` |
| GET | `/targets` | `/api/v1/TargetService/ListTargets` | 列出目标机 | `levee target list` |
| GET | `/targets/:id` | `/api/v1/TargetService/GetTarget` | 查看目标机 | — |
| DELETE | `/targets/:id` | `/api/v1/TargetService/RemoveTarget` | 移除目标机 | `levee target remove` |
| POST | `/targets/:id/check` | `/api/v1/TargetService/CheckTarget` | 连通性检查 | `levee target check` |
| GET | `/audit/log` | `/api/v1/AuditService/GetAuditLog` | 列出审计记录 | `levee audit list` |
| GET | `/audit/traces` | `/api/v1/AuditService/ListAuditTraces` | 审计 trace | — |
| GET | `/audit/verify` | `/api/v1/AuditService/VerifyHashChain` | 哈希链校验 | `levee audit verify` |
| GET | `/system/version` | `/api/v1/SystemService/GetVersion` | 系统版本 | — |
| GET | `/system/status` | `/api/v1/SystemService/GetStatus` | 系统状态 | `levee status` |
| GET | `/system/config` | `/api/v1/SystemService/GetConfig` | 系统配置 | — |
| POST | `/system/doctor` | `/api/v1/SystemService/RunDoctor` | 系统诊断 | — |

### 13.3 认证

Token-based 认证，两种模式：

- CLI / API 客户端：Bearer token，通过 `--token` 或配置文件 `auth.token` 传入，请求头 `Authorization: Bearer <token>`。
- 门户（Web UI）：同源嵌入在二进制中，无需额外认证；外部调用需携带 Bearer token。

> **安全提示**：默认情况下（不传 `--token`）鉴权处于关闭状态，所有 API 请求无需认证即可访问。生产环境必须通过 `--token <secret>` 设置 Bearer token。gRPC 和 REST 网关共享同一 token 校验逻辑。

### 13.4 分页与过滤

标准分页参数：

表：分页与过滤参数说明表

| 参数 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `limit` | int | 20 | 返回条数，上限 100 |
| `offset` | int | 0 | 偏移量 |
| `sort` | string | 按 created_at desc | 排序字段，如 `updated_at asc` |
| `fields` | string list | 全部字段 | 投影字段，逗号分隔，减少传输 |

过滤参数按资源域不同，如 `/changes` 支持 `status`、`template`、`initiator`、`from`、`to`，`/audit` 支持 `who`、`action`、`change`。过滤参数均为可选，多参数间为 AND 关系。

---

## 第14章 配置文件

### 14.1 ~/.levee/config.yaml

配置文件支持多 profile，默认 profile 为 `default`，通过 `--profile` 切换。

配置示例：config.yaml

```yaml
# 默认 profile
default:
  server:
    url: https://levee.internal.example.com
    timeout: 30m
  auth:
    token: ${LEVEE_TOKEN}        # 环境变量引用，避免硬编码
    token_file: ~/.levee/token   # 或从文件读取，二选一
  output:
    color: true
    format: table                # table | json
    quiet: false
  defaults:
    priority: normal
    batch_gate: true
    rollback_on_failure: true
    approval_timeout: 24h
  integrations:
    vault:
      addr: https://vault.internal:8200
      role: levee
    prometheus:
      url: https://prom.internal:9090
    itsm:
      type: servicenow
      url: https://snow.internal
    notification:
      channels: [slack, email]
      slack_webhook: ${LEVEE_SLACK_WEBHOOK}

# 生产环境 profile
prod:
  server:
    url: https://levee-prod.internal.example.com
  auth:
    token: ${LEVEE_PROD_TOKEN}
  defaults:
    priority: normal
    approval_timeout: 4h         # 生产环境审批更紧凑

# 气隙环境 profile
airgap:
  server:
    url: https://levee-airgap.local
  integrations:
    vault:
      addr: https://vault-airgap.local:8200
    prometheus:
      url: https://prom-airgap.local:9090
```

表：配置项说明对照表

| 一级 | 二级 | 类型 | 说明 |
| --- | --- | --- | --- |
| `server` | `url` | string | 后端 API 地址 |
| `server` | `timeout` | duration | 单命令超时 |
| `auth` | `token` | string | API token，建议用环境变量引用 |
| `auth` | `token_file` | path | token 文件路径，与 token 二选一 |
| `output` | `color` | bool | 彩色输出 |
| `output` | `format` | enum | table / json |
| `output` | `quiet` | bool | 静默模式 |
| `defaults` | `priority` | enum | 默认优先级 |
| `defaults` | `batch_gate` | bool | 批次间默认插入门禁 |
| `defaults` | `rollback_on_failure` | bool | 失败默认自动回滚 |
| `defaults` | `approval_timeout` | duration | 默认审批超时 |
| `integrations` | `vault` | map | Vault 凭据代理配置 |
| `integrations` | `prometheus` | map | Prometheus SLO 查询配置 |
| `integrations` | `itsm` | map | ITSM 集成配置 |
| `integrations` | `notification` | map | 通知渠道配置 |

### 14.2 环境变量

环境变量以 `LEVEE_` 前缀，覆盖配置文件同名项（大小写不敏感，下划线对应配置层级）。优先级高于配置文件、低于命令行显式参数。

表：环境变量对照表

| 环境变量 | 对应配置项 | 说明 |
| --- | --- | --- |
| `LEVEE_CONFIG` | `--config` | 配置文件路径 |
| `LEVEE_PROFILE` | `--profile` | 配置 profile |
| `LEVEE_SERVER_URL` | `server.url` | 后端 API 地址 |
| `LEVEE_TOKEN` | `auth.token` | API token |
| `LEVEE_TOKEN_FILE` | `auth.token_file` | token 文件路径 |
| `LEVEE_OUTPUT_FORMAT` | `output.format` | 输出格式 |
| `LEVEE_NO_COLOR` | `output.color` | 禁用彩色（设为 1 / true） |
| `LEVEE_TIMEOUT` | `server.timeout` | 单命令超时 |
| `LEVEE_API` | `--api` | 后端 API 地址，同 `LEVEE_SERVER_URL` |
| `LEVEE_SLACK_WEBHOOK` | `integrations.notification.slack_webhook` | Slack webhook 地址 |
| `LEVEE_LOG_LEVEL` | - | 日志级别：debug / info / warn / error |

---

## 附录A 命令速查表

按资源域分组，便于快速检索。

表：变更生命周期命令速查对照表

| 命令 | 语义 | 章节 |
| --- | --- | --- |
| `levee new` | 创建变更 | 2.1 |
| `levee clone` | 克隆变更 | 2.2 |
| `levee show` | 查看变更 | 2.3 |
| `levee list` | 列出变更 | 2.3 |
| `levee approve` | 通过审批 | 2.4 |
| `levee reject` | 驳回审批 | 2.4 |
| `levee delegate` | 转授权 | 2.4 |
| `levee plan` | 生成执行计划 | 2.5 |
| `levee apply` | 执行变更 | 2.5 |
| `levee pause` | 暂停单条 | 2.6 |
| `levee resume` | 恢复单条 | 2.6 |
| `levee pause-all` | 全局暂停 | 2.6 |
| `levee resume-all` | 全局恢复 | 2.6 |
| `levee cancel` | 取消变更 | 2.7 |
| `levee retry` | 重试失败批次 | 2.7 |
| `levee retry-host` | 单台重跑 | 2.7 |
| `levee rollback` | 按批回滚 | 2.8 |
| `levee rollback-all` | 全量回滚 | 2.8 |
| `levee logs` | 查看日志 | 2.9 |
| `levee diff` | 变更 diff | 2.9 |
| `levee trace` | 执行 trace | 2.9 |
| `levee archive` | 归档 | 2.10 |
| `levee link` | 关联工单 | 2.10 |

表：其他域命令速查对照表

| 命令 | 语义 | 章节 |
| --- | --- | --- |
| `levee template list/show/create/validate/update/delete/version` | 模板管理 | 3 |
| `levee target list/check/import/remove/unmanage` | 目标管理 | 4 |
| `levee audit list/show/export/verify` | 审计合规 | 5 |
| `levee drift scan/show` | 漂移检测 | 6 |
| `levee secret list/add/rotate/revoke/show` | 凭据管理 | 7 |
| `levee user list/add` | 用户管理 | 8 |
| `levee team list/add` | 团队管理 | 8 |
| `levee version` | 查看版本 | 9 |
| `levee config get/set` | 读写配置 | 9 |
| `levee status` | 系统状态 | 9 |
| `levee doctor` | 环境检查 | 9 |

---

## 附录B 与设计文档的映射关系

本设计中命令与设计文档（levee-design.md）章节的对应关系，便于追溯设计意图。

表：命令与设计文档映射对照表

| 命令 / 章节 | 设计文档章节 |
| --- | --- |
| 第2章 变更生命周期命令 | 4.4 D4 变更闭环（4.4.1-4.4.9） |
| `levee new --template` | 4.2.3 模板实例化 |
| `levee clone` | 4.2.3 变更克隆 |
| `levee plan` / `--dry-run` | 4.4.2 plan 阶段、4.2.4 dry-run |
| `levee approve` / `reject` / `delegate` | 4.4.3 审批阶段（分级 / 超时 / 驳回 / 转授权） |
| `levee apply` | 4.4.4 apply 阶段（哈希校验 / 快照 / 分批 / 重启处理） |
| `levee pause` / `resume` | 4.4.4.4 主动暂停与恢复 |
| `levee pause-all` / `resume-all` | 4.4.7 全局暂停 |
| `levee retry-host` | 4.4.4.5 单台重跑 |
| `levee logs -f` | 4.4.4.6 实时日志 |
| `levee rollback` / `rollback-all` | 4.4.6 回滚协议 |
| `levee trace` | 4.7.1 变更审计 trace |
| `levee audit verify` | 4.7.1 哈希链、4.7.2 哈希链分片 |
| `levee drift scan` | 4.3 D3 状态与漂移（简化） |
| `levee secret` | 4.8.1 凭据代理、4.8.5 凭据轮换 |
| 第13章 REST API | 7 组件架构 L1 接入层 |
| 第14章 配置文件 | 4.10 D12 部署形态（单机 / 集群 / 气隙 profile） |