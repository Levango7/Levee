# LEVEE CLI 参考文档

## 全局选项

以下选项适用于所有子命令，通过 persistent flags 注册在根命令上。

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--config` | `-c` | `~/.levee/config.yaml` | 配置文件路径 |
| `--json` | `-j` | `false` | JSON 输出格式，机器可读 |
| `--quiet` | `-q` | `false` | 静默模式，仅输出对象 ID |
| `--verbose` | `-v` | `false` | 详细输出，包含调试日志 |
| `--no-color` | | `false` | 禁用彩色输出，用于管道/日志归档 |
| `--profile` | | `default` | 配置 profile，用于多环境切换 |
| `--timeout` | | `30m` | 单命令超时，超时退出码 8 |
| `--api` | | | 后端 API 地址，用于 CLI 直连集群形态 server |
| `--token` | | | API token，CLI 认证用 |

## 退出码

| 退出码 | 含义 |
|--------|------|
| 0 | 成功 |
| 1 | 通用错误 |
| 2 | 参数错误（缺少必填参数、格式错误） |
| 3 | 权限不足 / 审批授权被拒 |
| 4 | 状态冲突（run 当前状态不允许该操作） |
| 5 | 重试次数超限（最多 3 次） |
| 6 | 哈希链验证失败 |
| 7 | 归档部分失败 |
| 8 | 命令超时 |

## 第1章 变更管理

### 1.1 new

从模板实例化一个 workflow。

```text
levee new <template> [--params key=val,key2=val2]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<template>` | 是 | 模板名称 |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--params` | | 参数键值对，格式 `key=val,key2=val2` |

**示例**

```命令示例：从模板创建 workflow
levee new nginx-reload --params target=web01.prod,env=production
```

**输出**

创建的 run 状态为 `draft`，返回 `run_id`、模板名称、实例化内容和参数。

### 1.2 clone

克隆历史 run 为可编辑草稿。

```text
levee clone <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | 要克隆的历史 run ID |

**说明**

- 克隆保留原始 run 的参数、batch 结构和 step 定义
- 克隆后的 run 获得新 ID，状态为 `draft`
- 不保留原始 run 的 trace、审计和审批记录

**示例**

```命令示例：克隆历史 run
levee clone run-abc123
```

### 1.3 show

查看 run 详细信息。

```text
levee show <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**输出**

包含 run 元数据、batch 执行摘要、step 状态和审计 trace 列表。

**示例**

```命令示例：查看 run 详情
levee show run-abc123
```

### 1.4 list

列出变更 run，支持过滤和分页。

```text
levee list [--status STATUS] [--template NAME] [--limit N] [--offset N]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--status` | | 按状态过滤（如 running, completed, draft） |
| `--template` | | 按模板名称过滤 |
| `--limit` | `20` | 最大返回数量 |
| `--offset` | `0` | 跳过数量（分页用） |

**示例**

```命令示例：列出所有运行中的 run
levee list --status running

命令示例：分页查询
levee list --limit 10 --offset 20
```

### 1.5 diff

比较两个 run 的差异。

```text
levee diff <run-a> <run-b>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-a>` | 是 | 第一个 run ID |
| `<run-b>` | 是 | 第二个 run ID |

**输出**

比较参数、状态、审批状态、plan hash 和 batch 执行情况，输出差异列表。无论是否存在差异，退出码均为 0；退出码 1 表示操作错误。

**示例**

```命令示例：比较两个 run
levee diff run-abc123 run-def456
```

## 第2章 审批控制

### 2.1 approve

审批通过一个 run。

```text
levee approve <run-id> [--comment TEXT] [--level LEVEL]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--comment` | | 审批备注 |
| `--level` | | 审批级别：standard / high / emergency |

**说明**

- 当足够多的审批人通过后（由审批级别决定），run 状态转为 `approved`
- 一票否决：任何一次 reject 即可否决整个审批

**示例**

```命令示例：标准审批
levee approve run-abc123

命令示例：高级别审批带备注
levee approve run-abc123 --level high --comment "已确认变更窗口"
```

### 2.2 reject

驳回一个 run。

```text
levee reject <run-id> --reason TEXT
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--reason` | | 驳回原因（必填） |

**说明**

- 单次 reject 立即否决整个审批（一票否决语义）
- `--reason` 为必填参数，省略时返回退出码 2

**示例**

```命令示例：驳回 run
levee reject run-abc123 --reason "变更窗口已关闭"
```

## 第3章 执行控制

### 3.1 apply

触发 run 执行。

```text
levee apply <run-id> [--force]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--force` | `false` | 跳过审批检查，强制执行 |

**说明**

- 不加 `--force` 时，仅 `approved` 状态的 run 可执行
- 加 `--force` 时，`pending` 和 `draft` 状态也可执行
- 执行流程：hash 校验 -> pre-apply 快照 -> 分批执行 -> 验证门禁 -> 失败回滚

**示例**

```命令示例：正常执行
levee apply run-abc123

命令示例：强制执行
levee apply run-abc123 --force
```

### 3.2 pause

暂停单个 run。

```text
levee pause <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**说明**

- 仅 `running` 或 `pending` 状态的 run 可暂停
- 暂停后保留所有状态，可通过 `resume` 恢复

**示例**

```命令示例：暂停 run
levee pause run-abc123
```

### 3.3 resume

恢复暂停的 run。

```text
levee resume <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**说明**

- 仅 `paused` 状态的 run 可恢复
- 恢复后状态转为 `running`，从暂停点继续执行

**示例**

```命令示例：恢复 run
levee resume run-abc123
```

### 3.4 pause-all

全局暂停所有 run。

```text
levee pause-all [--reason TEXT]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--reason` | | 全局暂停原因（记录在审计中） |

**说明**

- 高权限操作，需要 `pause:all` 权限
- CLI 模式下默认拥有该权限（通过 `LEVEE_PERMISSIONS` 环境变量控制）
- 为每个受影响的 run 记录审计条目

**示例**

```命令示例：全局暂停
levee pause-all --reason "紧急维护窗口"
```

### 3.5 resume-all

全局恢复所有暂停的 run。

```text
levee resume-all [--reason TEXT]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--reason` | | 全局恢复原因（记录在审计中） |

**说明**

- 高权限操作，需要 `resume:all` 权限
- CLI 模式下默认拥有该权限

**示例**

```命令示例：全局恢复
levee resume-all --reason "维护窗口结束"
```

### 3.6 cancel

取消未执行的 run。

```text
levee cancel <run-id> [--reason TEXT]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--reason` | | 取消原因（记录在审计中） |

**说明**

- 仅 `pending` 或 `draft` 状态的 run 可取消
- 已运行或已完成的 run 不可取消，请使用 `pause` 或 `rollback`
- 取消操作记录审计条目

**示例**

```命令示例：取消 run
levee cancel run-abc123 --reason "需求变更"
```

### 3.7 retry

重试失败的 run。

```text
levee retry <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**说明**

- 仅 `failed` 或 `paused` 状态的 run 可重试
- 每个 run 最多重试 3 次，超出返回退出码 5
- 重试从失败点重新执行

**示例**

```命令示例：重试 run
levee retry run-abc123
```

### 3.8 retry-host

重试 run 中失败的主机。

```text
levee retry-host <run-id> <host>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |
| `<host>` | 是 | 目标主机名 |

**说明**

- 仅 `failed` 或 `paused` 状态的 run 可执行
- 每个主机最多重试 3 次，超出返回退出码 5

**示例**

```命令示例：重试指定主机
levee retry-host run-abc123 web01.prod
```

### 3.9 rollback

手动触发回滚。

```text
levee rollback <run-id> [--force]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--force` | `false` | 强制回滚，忽略状态检查 |

**说明**

- 不加 `--force` 时，仅 `running`、`completed` 或 `failed` 状态可回滚
- 回滚按执行 batch 的逆序执行 rollback 步骤
- 回滚完成后运行 post-rollback 验证确认目标状态健康
- run 状态转为 `rolling_back`

**示例**

```命令示例：回滚 run
levee rollback run-abc123

命令示例：强制回滚
levee rollback run-abc123 --force
```

## 第4章 观察性

### 4.1 logs

查看 run 日志。

```text
levee logs <run-id> [--target HOST] [-f]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--target` | | | 按主机过滤日志 |
| `--follow` | `-f` | `false` | 实时跟踪日志流（每 2 秒轮询） |

**示例**

```命令示例：查看日志
levee logs run-abc123

命令示例：按主机过滤
levee logs run-abc123 --target web01.prod

命令示例：实时跟踪
levee logs run-abc123 -f
```

### 4.2 trace

查看审计 trace 并验证哈希链完整性。

```text
levee trace <run-id> [--verify]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--verify` | `false` | 验证哈希链完整性 |

**说明**

- 输出完整的审计 trace 记录，包含 prev_hash 和 curr_hash
- 加 `--verify` 时验证哈希链，验证失败返回退出码 6

**示例**

```命令示例：查看审计 trace
levee trace run-abc123

命令示例：验证哈希链
levee trace run-abc123 --verify
```

### 4.3 archive

将 run 的审计 trace 归档到 WORM 存储。

```text
levee archive <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**说明**

- 将 trace 记录追加到 Write-Once-Read-Many 存储
- 每条记录附带内容校验和
- 已归档的记录自动跳过
- 部分记录归档失败时打印警告但继续处理（非阻塞）
- 部分失败时返回退出码 7

**示例**

```命令示例：归档审计 trace
levee archive run-abc123
```

### 4.4 link

关联 run 与事件单。

```text
levee link <run-id> --incident <incident-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--incident` | | 事件单 ID（必填） |

**说明**

- 更新 run 的 `incident_id` 字段，建立变更与事件的追溯关联

**示例**

```命令示例：关联事件单
levee link run-abc123 --incident INC-20260816-001
```

## 第5章 模板管理

### 5.1 template list

列出所有模板。

```text
levee template list [--tag key=value]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--tag` | | 按 tag 过滤（格式 key=value） |

**示例**

```命令示例：列出所有模板
levee template list

命令示例：按 tag 过滤
levee template list --tag category=web
```

### 5.2 template show

查看模板详情。

```text
levee template show <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 模板名称 |

**示例**

```命令示例：查看模板详情
levee template show nginx-reload
```

### 5.3 template create

创建新模板。

```text
levee template create --name NAME --content YAML [--description TEXT] [--params JSON]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 模板名称（必填） |
| `--content` | | 模板 YAML 内容（必填） |
| `--description` | | 模板描述 |
| `--params` | | 模板参数，JSON 数组格式 |

**示例**

```命令示例：创建模板
levee template create --name nginx-reload --content "name: nginx-reload
steps:
  - name: reload
    action: shell
    command: systemctl reload nginx" --params '[{"name":"target","description":"Target host"}]'
```

### 5.4 template delete

删除模板。

```text
levee template delete <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 模板名称 |

**示例**

```命令示例：删除模板
levee template delete nginx-reload
```

## 第6章 目标管理

### 6.1 target list

列出已知目标主机。

```text
levee target list
```

**说明**

- 从历史变更 run 的 step 记录中收集去重后的主机列表

**示例**

```命令示例：列出目标主机
levee target list
```

### 6.2 target import

从文件导入目标主机。

```text
levee target import --file PATH
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--file` | | 输入文件路径（必填，每行一个主机名） |

**说明**

- 文件格式为每行一个主机名
- 以 `#` 开头的行视为注释并跳过
- 空行自动跳过

**示例**

```命令示例：导入主机列表
levee target import --file hosts.txt
```

### 6.3 target check

对目标主机运行预检。

```text
levee target check <host>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<host>` | 是 | 目标主机名 |

**说明**

- 验证目标主机的可达性
- 输出可达性状态、延迟和错误信息

**示例**

```命令示例：预检主机
levee target check web01.prod
```

## 第7章 审计管理

### 7.1 audit verify

验证 run 的哈希链完整性。

```text
levee audit verify <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**说明**

- 验证通过返回退出码 0
- 验证失败返回退出码 6，输出被篡改的记录信息

**示例**

```命令示例：验证哈希链
levee audit verify run-abc123
```

### 7.2 audit export

导出 run 的审计 trace。

```text
levee audit export <run-id> [--format FORMAT]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--format` | `json` | 导出格式：json 或 csv |

**示例**

```命令示例：导出为 JSON
levee audit export run-abc123

命令示例：导出为 CSV
levee audit export run-abc123 --format csv
```

### 7.3 audit list

列出 run 的所有 trace 记录。

```text
levee audit list <run-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<run-id>` | 是 | run ID |

**示例**

```命令示例：列出 trace 记录
levee audit list run-abc123
```

### 7.4 audit show

查看单条 trace 记录详情。

```text
levee audit show <trace-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<trace-id>` | 是 | trace 记录 ID |

**输出**

包含 ID、run_id、event、actor、detail、prev_hash、curr_hash 和 timestamp。

**示例**

```命令示例：查看 trace 详情
levee audit show trace-xyz789
```

## 第8章 凭据管理

### 8.1 secret list

列出所有凭据（仅元数据）。

```text
levee secret list
```

**说明**

- 仅显示凭据元数据（ID、名称、类型、创建时间）
- 永不显示明文值

**环境变量**

| 变量 | 必填 | 说明 |
|------|------|------|
| `LEVEE_MASTER_PASSWORD` | 是 | 凭据加密主密码 |

**示例**

```命令示例：列出凭据
levee secret list
```

### 8.2 secret add

添加新凭据。

```text
levee secret add --name NAME --value VALUE [--type TYPE]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 凭据名称（必填） |
| `--value` | | 明文值（必填，加密存储） |
| `--type` | `ssh_password` | 凭据类型：ssh_key / ssh_password / winrm_password / api_token |

**示例**

```命令示例：添加 SSH 密码凭据
levee secret add --name prod-ssh --value "s3cret" --type ssh_password
```

### 8.3 secret rotate

轮换凭据值。

```text
levee secret rotate --name NAME
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 凭据名称（必填） |

**说明**

- 执行后从 stdin 交互式读取新值
- 替换现有加密值为新值

**示例**

```命令示例：轮换凭据
levee secret rotate --name prod-ssh
```

### 8.4 secret revoke

吊销（删除）凭据。

```text
levee secret revoke --name NAME
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 凭据名称（必填） |

**示例**

```命令示例：吊销凭据
levee secret revoke --name prod-ssh
```

### 8.5 secret show

查看凭据元数据。

```text
levee secret show --name NAME
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 凭据名称（必填） |

**说明**

- 显示 ID、名称、类型、创建时间和轮换时间
- 永不显示明文值

**示例**

```命令示例：查看凭据元数据
levee secret show --name prod-ssh
```

## 第9章 用户与团队管理

### 9.1 user list

列出权限矩阵中的用户。

```text
levee user list
```

**示例**

```命令示例：列出用户
levee user list
```

### 9.2 user add

添加用户到权限矩阵。

```text
levee user add --name NAME --team TEAM --role ROLE
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 用户名（必填） |
| `--team` | | 团队名（必填，需在权限矩阵中存在） |
| `--role` | | 角色（必填）：admin / operator / viewer |

**示例**

```命令示例：添加用户
levee user add --name zhangsan --team infra --role operator
```

### 9.3 team list

列出权限矩阵中的团队。

```text
levee team list
```

**输出**

包含团队名称和每个团队在各环境下的权限操作列表。

**示例**

```命令示例：列出团队
levee team list
```

### 9.4 team add

添加团队并分配环境权限。

```text
levee team add --name NAME --env ENV
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 团队名（必填） |
| `--env` | | 环境名（必填） |

**说明**

- 新团队默认获得 plan、apply、view 三项操作权限
- 如团队已存在，则为其追加新环境的权限

**示例**

```命令示例：添加团队
levee team add --name infra --env production
```

## 第10章 系统管理

### 10.1 system version

打印版本信息。

```text
levee system version
```

**输出**

包含版本号、构建时间、commit hash 和 Go 工具链版本。

**示例**

```命令示例：查看版本
levee system version
```

### 10.2 system status

查看系统状态。

```text
levee system status
```

**输出**

包含版本号、配置文件路径、数据库连接状态和数据库路径。

**示例**

```命令示例：查看系统状态
levee system status
```

### 10.3 system config get

获取配置值。

```text
levee system config get <key>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<key>` | 是 | 配置键路径（点分格式） |

**可查询的配置键**

| 键 | 说明 |
|----|------|
| `server.data_dir` | 数据目录 |
| `server.log_level` | 日志级别 |
| `server.log_format` | 日志格式 |
| `database.driver` | 数据库驱动 |
| `database.path` | 数据库路径 |
| `database.max_open_conns` | 最大打开连接数 |
| `database.max_idle_conns` | 最大空闲连接数 |
| `log.level` | 日志级别 |
| `log.format` | 日志格式 |
| `log.output` | 日志输出 |
| `permission.default_team` | 默认团队 |
| `permission.default_env` | 默认环境 |

**示例**

```命令示例：获取配置值
levee system config get database.path
```

### 10.4 system config set

设置配置值。

```text
levee system config set <key> <value>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<key>` | 是 | 配置键路径 |
| `<value>` | 是 | 配置值 |

**可设置的配置键**

| 键 | 说明 |
|----|------|
| `server.log_level` | 日志级别 |
| `server.log_format` | 日志格式 |
| `log.level` | 日志级别 |
| `log.format` | 日志格式 |
| `log.output` | 日志输出 |

**说明**

- 变更写入当前激活的配置文件
- 未指定配置文件时写入 `~/.levee/config.yaml`

**示例**

```命令示例：设置日志级别
levee system config set log.level debug
```

### 10.5 system doctor

运行诊断检查。

```text
levee system doctor
```

**检查项**

| 检查项 | 说明 |
|--------|------|
| config | 配置文件是否可加载 |
| database | 数据库是否可达 |
| permission_matrix | 权限矩阵是否可加载 |

**输出**

- 整体状态：`healthy` 或 `unhealthy`
- 每项检查状态：`OK` / `FAIL` / `SKIP`

**示例**

```命令示例：运行诊断
levee system doctor
```

## 第11章 version

打印 LEVEE 版本信息（根命令级别）。

```text
levee version
```

**说明**

与 `levee system version` 功能相同，提供便捷的顶层访问。输出包含版本号、构建时间、commit hash 和 Go 工具链版本。加 `--json` 输出结构化 JSON。

**示例**

```命令示例：查看版本
levee version

命令示例：JSON 格式输出
levee version --json
```

## 第12章 compile — 编译 workflow

对 LEVEELang workflow 文件执行编译期类型检查与 IR 生成。

### 12.1 compile

```text
levee compile <file> [--strict|--lenient] [--ir] [--check-only]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<file>` | 是 | LEVEELang YAML workflow 文件路径 |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--strict` | `true` | 严格模式：类型错误致命（默认） |
| `--lenient` | `false` | 宽松模式：类型错误降级为警告，仍生成 IR |
| `--ir` | `false` | 将 IR 以 JSON 文档输出到 stdout |
| `--check-only` | `false` | 仅类型检查，不生成 IR |

**说明**

- 执行流程：解析 YAML → 结构校验 → 类型检查 →（可选）IR 生成
- `--lenient` 优先于 `--strict`：两者同时设置时按宽松模式处理
- 所有错误附带源文件 + 行 + 列信息，多错误合并为单次报告

**示例**

```命令示例：编译并类型检查
levee compile deploy.yml

命令示例：宽松模式并输出 IR
levee compile deploy.yml --lenient --ir

命令示例：仅类型检查
levee compile deploy.yml --check-only
```

## 第13章 calendar — 变更日历管理

管理变更窗口与冻结期，支持 cron 重复规则与冲突检测。

### 13.1 calendar list

列出所有变更窗口与冻结期。

```text
levee calendar list [--limit N]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--limit` | `0` | 最大返回数量（0 表示全部） |

**示例**

```命令示例：列出所有变更窗口
levee calendar list
```

### 13.2 calendar show

查看单个变更窗口详情。

```text
levee calendar show <id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<id>` | 是 | 变更窗口 ID |

**示例**

```命令示例：查看变更窗口详情
levee calendar show win-001
```

### 13.3 calendar create

创建变更窗口或冻结期。

```text
levee calendar create --name <n> --start <t> --end <t> --targets <labels> [--frozen] [--cron <expr>] [--repeat <hint>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 窗口名称（必填） |
| `--start` | | 起始时间，RFC3339 格式（必填，如 `2026-08-16T10:00:00Z`） |
| `--end` | | 结束时间，RFC3339 格式（必填） |
| `--targets` | | 逗号分隔的目标标签列表（必填） |
| `--frozen` | `false` | 标记为冻结期 |
| `--cron` | | 5 字段 cron 重复规则（如 `0 2 * * *`） |
| `--repeat` | | 人类可读的重复提示（如 `weekly`） |

**示例**

```命令示例：创建变更窗口
levee calendar create --name "发版窗口" --start 2026-08-16T10:00:00Z --end 2026-08-16T12:00:00Z --targets web,batch

命令示例：创建冻结期并设置 cron 重复
levee calendar create --name "月末冻结" --start 2026-08-31T00:00:00Z --end 2026-08-31T23:59:59Z --targets prod --frozen --cron "0 0 1 * *"
```

### 13.4 calendar update

更新变更窗口。仅设置的标志生效，未设置标志保留原值。

```text
levee calendar update <id> [--name <n>] [--start <t>] [--end <t>] [--targets <labels>] [--frozen] [--cron <expr>] [--repeat <hint>]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<id>` | 是 | 变更窗口 ID |

**示例**

```命令示例：更新窗口名称
levee calendar update win-001 --name "扩展发版窗口"
```

### 13.5 calendar delete

删除变更窗口或冻结期。

```text
levee calendar delete <id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<id>` | 是 | 变更窗口 ID |

**示例**

```命令示例：删除变更窗口
levee calendar delete win-001
```

### 13.6 calendar check

检查目标集当前是否处于冻结期，并列出覆盖该目标集的活动变更窗口。

```text
levee calendar check --targets <labels>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--targets` | | 逗号分隔的目标标签列表（必填） |

**示例**

```命令示例：检查目标集冻结状态
levee calendar check --targets web,batch
```

## 第14章 kms — KMS 管理

查看与测试外部密钥管理系统（HashiCorp Vault、AWS KMS）集成状态。

### 14.1 kms status

显示所有已注册 KMS Provider 的健康与可达性。

```text
levee kms status
```

**说明**

- 未配置任何 Provider 时，报告本地 CredentialStore（AES-256-GCM）正在使用
- 输出包含每个 Provider 的健康状态、默认 Provider 与本地降级开关

**示例**

```命令示例：查看 KMS Provider 状态
levee kms status
```

### 14.2 kms config

显示 KMS 配置：启用的 Provider 列表、默认 Provider、路由表与本地降级开关。

```text
levee kms config
```

**示例**

```命令示例：查看 KMS 配置
levee kms config
```

### 14.3 kms test

对每个已注册 Provider 执行连通性测试（HealthCheck），可选执行完整 GetSecret 往返。

```text
levee kms test [--name <credential-name>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 凭据名称，提供时对每个健康 Provider 执行完整 GetSecret 往返测试 |

**说明**

- GetSecret 往返成功后立即清零明文，永不打印明文值

**示例**

```命令示例：测试 KMS 连通性
levee kms test

命令示例：测试指定凭据的完整往返
levee kms test --name prod-ssh
```

## 第15章 rbac — RBAC 管理

管理角色继承树、细粒度权限策略与权限检查。

### 15.1 rbac role list

列出所有角色及其父级继承关系。

```text
levee rbac role list
```

**示例**

```命令示例：列出角色
levee rbac role list
```

### 15.2 rbac role add

添加角色，可选指定父级以建立继承。

```text
levee rbac role add --name <role> [--parent <parent-role>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 角色名称（必填） |
| `--parent` | | 父级角色名（可选，用于继承） |

**示例**

```命令示例：添加角色并继承
levee rbac role add --name operator --parent viewer
```

### 15.3 rbac role remove

从继承树中移除角色。

```text
levee rbac role remove --name <role>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 要移除的角色名称（必填） |

**示例**

```命令示例：移除角色
levee rbac role remove --name operator
```

### 15.4 rbac policy list

列出所有权限策略。

```text
levee rbac policy list
```

**示例**

```命令示例：列出策略
levee rbac policy list
```

### 15.5 rbac policy add

添加 `Resource × Action × Condition` 权限策略。

```text
levee rbac policy add --id <id> --resource <pattern> --action <action> [--effect allow|deny] [--condition <label-expr>] [--description <text>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--id` | | 策略 ID（必填） |
| `--effect` | `allow` | 效果：`allow` 或 `deny` |
| `--resource` | | 资源模式（必填） |
| `--action` | | 动作（必填） |
| `--condition` | | 标签条件表达式（可选） |
| `--description` | | 人类可读描述 |

**示例**

```命令示例：添加允许策略
levee rbac policy add --id p001 --resource "change:*" --action apply --effect allow --description "允许应用变更"
```

### 15.6 rbac policy remove

按 ID 移除策略。

```text
levee rbac policy remove --id <id>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--id` | | 要移除的策略 ID（必填） |

**示例**

```命令示例：移除策略
levee rbac policy remove --id p001
```

### 15.7 rbac check

检查用户是否可对资源执行指定动作（支持 ABAC 标签）。

```text
levee rbac check --user <u> --action <a> --resource <r> [--label key=value]... [--verbose]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 主体 / 用户（必填） |
| `--action` | | 动作（必填） |
| `--resource` | | 资源（必填） |
| `--label` | | 资源标签，`key=value` 格式，可重复 |
| `--verbose` | `false` | 显示详细判定解释 |

**示例**

```命令示例：权限检查
levee rbac check --user zhangsan --action apply --resource change:run-001

命令示例：带标签的 ABAC 检查
levee rbac check --user zhangsan --action apply --resource change:run-001 --label env=production --verbose
```

### 15.8 rbac tree

显示角色继承树。

```text
levee rbac tree
```

**示例**

```命令示例：显示角色继承树
levee rbac tree
```

## 第16章 plugin — 插件管理

管理 LEVEE 插件（Channel/Gate/Module/Notifier 四类接口）。

### 16.1 plugin list

列出已注册的所有插件，按名称排序。

```text
levee plugin list
```

**示例**

```命令示例：列出插件
levee plugin list
```

### 16.2 plugin install

从目录或二进制路径安装插件。

```text
levee plugin install <path> [--verify-signature]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<path>` | 是 | 插件目录或二进制路径（读取同目录 `plugin.yaml` 清单） |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--verify-signature` | `false` | 安装与启用时校验二进制 SHA-256 签名 |

**示例**

```命令示例：安装插件
levee plugin install ./plugins/http-probe

命令示例：安装并校验签名
levee plugin install ./plugins/http-probe --verify-signature
```

### 16.3 plugin enable

启用插件：启动子进程并标记为 enabled。

```text
levee plugin enable <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 插件名称 |

**示例**

```命令示例：启用插件
levee plugin enable http-probe
```

### 16.4 plugin disable

禁用插件：停止子进程并标记为 disabled。

```text
levee plugin disable <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 插件名称 |

**示例**

```命令示例：禁用插件
levee plugin disable http-probe
```

### 16.5 plugin remove

从注册表移除插件。若插件处于启用状态则先禁用。

```text
levee plugin remove <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 插件名称 |

**示例**

```命令示例：移除插件
levee plugin remove http-probe
```

### 16.6 plugin info

显示插件在注册表中的完整记录。

```text
levee plugin info <name>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<name>` | 是 | 插件名称 |

**示例**

```命令示例：查看插件详情
levee plugin info http-probe
```

## 第17章 chatops — ChatOps 机器人

飞书 / 钉钉 / Slack 机器人管理，支持交互卡片消息与一键审批。

### 17.1 chatops start

启动指定平台的机器人，开始监听 LEVEE 事件并向 IM 群推送卡片消息。

```text
levee chatops start --platform <p> [--config <c>] [--timeout <d>]
```

**选项**

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--platform` | | `feishu` | 平台：`feishu` / `dingtalk` / `slack` |
| `--config` | `-c` | | 机器人配置文件路径（JSON） |
| `--timeout` | | `0` | 运行时长；`0` 表示持续运行直到收到信号 |

**示例**

```命令示例：启动飞书机器人
levee chatops start --platform feishu --config bot.json

命令示例：运行 60 秒后退出
levee chatops start --platform slack --config bot.json --timeout 60s
```

### 17.2 chatops send

通过指定平台的机器人向目标 channel 发送一条文本消息。

```text
levee chatops send --platform <p> [--config <c>] --channel <c> --message <m>
```

**选项**

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--platform` | | `feishu` | 平台：`feishu` / `dingtalk` / `slack` |
| `--config` | `-c` | | 机器人配置文件路径（JSON） |
| `--channel` | `-C` | | 目标 channel / 群 ID |
| `--message` | `-m` | | 消息内容 |

**示例**

```命令示例：发送消息
levee chatops send --platform feishu --config bot.json --channel oc_123 --message "变更 run-001 已完成"
```

### 17.3 chatops approve

通过 ChatOps 触发审批通过，复用 approval 服务。

```text
levee chatops approve [--platform <p>] [--config <c>] --id <change-id>
```

**选项**

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--platform` | `-p` | `feishu` | 平台：`feishu` / `dingtalk` / `slack` |
| `--config` | `-c` | | 机器人配置文件路径（JSON，可选） |
| `--id` | | | 变更 ID（等价于位置参数） |

**示例**

```命令示例：通过 ChatOps 审批通过
levee chatops approve --id run-abc123
```

### 17.4 chatops reject

通过 ChatOps 触发审批驳回，需提供驳回原因。

```text
levee chatops reject [--platform <p>] [--config <c>] --id <change-id> --reason <r>
```

**选项**

| 选项 | 短选项 | 默认值 | 说明 |
|------|--------|--------|------|
| `--platform` | `-p` | `feishu` | 平台：`feishu` / `dingtalk` / `slack` |
| `--config` | `-c` | | 机器人配置文件路径（JSON，可选） |
| `--id` | | | 变更 ID（等价于位置参数） |
| `--reason` | `-r` | | 驳回原因（必填） |

**示例**

```命令示例：通过 ChatOps 驳回
levee chatops reject --id run-abc123 --reason "变更窗口已关闭"
```

## 第18章 web — Web UI 服务

提供 LEVEE Web UI（Vue 3 SPA）HTTP 服务。

### 18.1 web

```text
levee web [--port <p>] [--addr <a>] [--api <url>] [--dev] [--dev-server <url>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--port` | `8080` | HTTP 监听端口 |
| `--addr` | | HTTP 监听地址（覆盖 `--port`，如 `0.0.0.0:8080`） |
| `--api` | | gRPC-gateway 后端 URL，用于 `/api/*` 代理（空 = 不代理） |
| `--dev` | `false` | 开发模式：代理到 Vite dev server |
| `--dev-server` | `http://localhost:5173` | Vite dev server URL（仅 dev 模式） |

**说明**

- 生产模式直接服务 go:embed 嵌入的静态资源
- 开发模式（`--dev`）将非 API 请求代理到 Vite dev server 以支持热更新
- 使用 `--api` 将 `/api/*` 代理到独立运行的 gRPC-gateway 后端

**示例**

```命令示例：启动 Web UI（默认 8080 端口）
levee web

命令示例：指定端口并代理到后端
levee web --port 8080 --api http://localhost:9090

命令示例：开发模式
levee web --dev
```

## 第19章 serve — gRPC 服务

在当前进程运行 LEVEE gRPC 服务器，暴露全部五个服务（Change / Template / Target / Audit / System）。

### 19.1 serve

```text
levee serve [--addr <a>] [--tls-cert <c>] [--tls-key <k>] [--token <t>]
           [--cors-origin <o>]... [--insecure]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--addr` | `:9090` | 监听地址 |
| `--tls-cert` | | TLS 证书路径（可选，省略则明文） |
| `--tls-key` | | TLS 私钥路径（可选） |
| `--token` | | 要求客户端提供的 Bearer token（必填，见下方安全门禁） |
| `--cors-origin` | 空（拒绝跨域） | REST 网关允许的 CORS 来源，可重复传入多个；传 `*` 表示允许所有来源 |
| `--rate-limit` | `200` | REST 网关全局限流（req/s，令牌桶）；传负数关闭限流 |
| `--rate-burst` | `400` | 令牌桶突发容量 |
| `--insecure` | false | 显式接受无鉴权风险，仅供本地开发 |

**说明**

- 安全门禁：不传 `--token` 且未显式指定 `--insecure` 时，服务拒绝启动，避免生产环境意外暴露无鉴权 API
- 设置 `--token` 后 gRPC 与 REST 网关均要求匹配的 Bearer token
- CORS 默认拒绝所有跨域请求；同源请求不受影响；需要跨域时用 `--cors-origin` 白名单
- 限流触发时返回 HTTP 429 并附带 `Retry-After`
- 每个 REST 响应携带 `X-Request-Id`（可由客户端传入复用）；gRPC 日志含 `request_id` 字段，支持链路关联
- Linux 上插件沙箱经 cgroup v2 强制内存/CPU 限额（需 /sys/fs/cgroup 可写）；不可用时降级为仅墙钟超时并输出 WARN

**示例**

```命令示例：启用 token 鉴权（生产推荐）
levee serve --addr :9090 --tls-cert server.crt --tls-key server.key --token s3cret

命令示例：本地开发（无鉴权，需显式确认）
levee serve --token dev-token
levee serve --insecure
```

## 第20章 agent — 分布式执行 Agent 管理

管理分布式执行 Agent 常驻进程，支持注册到 master 节点、心跳保活、任务执行与结果回传。

### 20.1 agent start

启动 Agent 常驻进程，注册到 master 节点并开始心跳。

```text
levee agent start --addr <addr> --master <master-addr> --caps <caps> [--id <agent-id>] [--heartbeat <duration>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--addr` | | Agent 监听地址（必填，如 `:9091`） |
| `--master` | | master 节点地址（必填，如 `localhost:9090`） |
| `--caps` | | Agent 能力列表，逗号分隔（必填，如 `shell,file,pkg`） |
| `--id` | 自动生成 | Agent ID，省略时自动生成 UUID |
| `--heartbeat` | `15s` | 心跳间隔 |

**说明**

- Agent 启动后向 master 注册自身地址与能力，并按 `--heartbeat` 间隔周期发送心跳
- master 节点根据 Agent 能力（`caps`）调度匹配的任务到该 Agent
- Agent 进程退出时自动向 master 注销

**示例**

```命令示例：启动 Agent 并注册到 master
levee agent start --addr :9091 --master localhost:9090 --caps shell,file

命令示例：指定 Agent ID 与心跳间隔
levee agent start --addr :9092 --master localhost:9090 --caps shell,file,pkg --id agent-web-01 --heartbeat 10s
```

### 20.2 agent status

查看当前 Agent 进程状态。

```text
levee agent status
```

**输出**

包含 Agent ID、注册状态、master 连接状态、能力列表、心跳计数与最近一次心跳时间。

**示例**

```命令示例：查看 Agent 状态
levee agent status
```

### 20.3 agent list

列出所有已注册 Agent（master 端）。

```text
levee agent list [--status STATUS]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--status` | | 按状态过滤：`active` / `inactive` / `lost` |

**示例**

```命令示例：列出所有已注册 Agent
levee agent list

命令示例：仅列出活跃 Agent
levee agent list --status active
```

### 20.4 agent show

查看特定 Agent 详情。

```text
levee agent show <agent-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<agent-id>` | 是 | Agent ID |

**输出**

包含 Agent ID、地址、能力、注册时间、最近心跳、当前执行任务等。

**示例**

```命令示例：查看 Agent 详情
levee agent show agent-web-01
```

### 20.5 agent remove

从 master 移除 Agent 注册记录。

```text
levee agent remove <agent-id> [--force]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<agent-id>` | 是 | Agent ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--force` | `false` | 强制移除，即使 Agent 仍有正在执行的任务 |

**示例**

```命令示例：移除 Agent
levee agent remove agent-web-01

命令示例：强制移除
levee agent remove agent-web-01 --force
```

## 第21章 tenant — 租户管理

管理多租户隔离与资源配额，支持租户创建、暂停/恢复、配额管理与使用量查询。

### 21.1 tenant create

创建新租户。

```text
levee tenant create --name <name> --display <display> [--max-targets <n>] [--max-changes <n>] [--max-storage <bytes>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 租户名称（必填，唯一标识） |
| `--display` | | 显示名称（必填） |
| `--max-targets` | `100` | 最大目标机数 |
| `--max-changes` | `10` | 最大并发变更数 |
| `--max-storage` | `1GB` | 最大存储空间（字节） |

**示例**

```命令示例：创建租户
levee tenant create --name acme --display "ACME Corp" --max-targets 100 --max-changes 10
```

### 21.2 tenant list

列出所有租户。

```text
levee tenant list [--status STATUS]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--status` | | 按状态过滤：`active` / `suspended` |

**示例**

```命令示例：列出所有租户
levee tenant list
```

### 21.3 tenant show

查看租户详情。

```text
levee tenant show <tenant-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**示例**

```命令示例：查看租户详情
levee tenant show t-001
```

### 21.4 tenant suspend

暂停租户，阻止该租户发起任何变更操作。

```text
levee tenant suspend <tenant-id> [--reason TEXT]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--reason` | | 暂停原因（记录在审计中） |

**示例**

```命令示例：暂停租户
levee tenant suspend t-001 --reason "合规审查"
```

### 21.5 tenant resume

恢复暂停的租户。

```text
levee tenant resume <tenant-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**示例**

```命令示例：恢复租户
levee tenant resume t-001
```

### 21.6 tenant delete

删除租户。仅当租户处于 `suspended` 状态且无活跃变更时可删除。

```text
levee tenant delete <tenant-id> [--force]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--force` | `false` | 强制删除，级联清理租户所有数据 |

**示例**

```命令示例：删除租户
levee tenant delete t-001
```

### 21.7 tenant quota

查看或设置租户资源配额。

```text
levee tenant quota <tenant-id> [--max-targets <n>] [--max-changes <n>] [--max-storage <bytes>]
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--max-targets` | | 最大目标机数（不设置则不修改） |
| `--max-changes` | | 最大并发变更数 |
| `--max-storage` | | 最大存储空间（字节） |

**说明**

- 不带任何选项时仅显示当前配额
- 带选项时更新对应配额项，未设置的项保留原值

**示例**

```命令示例：查看租户配额
levee tenant quota t-001

命令示例：调整租户配额
levee tenant quota t-001 --max-targets 200 --max-changes 20
```

### 21.8 tenant usage

查看租户资源使用量。

```text
levee tenant usage <tenant-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<tenant-id>` | 是 | 租户 ID |

**输出**

包含已用目标机数、当前并发变更数、已用存储空间与对应配额上限的对比。

**示例**

```命令示例：查看租户使用量
levee tenant usage t-001
```

## 第22章 drift — 配置漂移检测

检测目标机配置漂移、管理漂移基线、调度定期巡检与查看漂移报告。

### 22.1 drift detect

检测目标机配置漂移。

```text
levee drift detect --host <host> [--baseline <baseline-id>|auto]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--host` | | 目标主机名（必填） |
| `--baseline` | | 基线 ID 或 `auto`（必填，`auto` 表示从最近 apply 自动生成） |

**说明**

- 对比目标机当前实际状态与基线状态，输出漂移项列表
- 漂移项包含文件路径、期望内容哈希、实际内容哈希、差异类型
- 无漂移时退出码 0，存在漂移时退出码 0 并打印漂移列表

**示例**

```命令示例：使用自动基线检测漂移
levee drift detect --host web-01 --baseline auto

命令示例：使用指定基线检测漂移
levee drift detect --host web-01 --baseline base-001
```

### 22.2 drift baseline set

手动设置漂移基线。

```text
levee drift baseline set --host <host> --file <path> [--name <name>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--host` | | 目标主机名（必填） |
| `--file` | | 基线文件路径（必填，JSON 格式） |
| `--name` | 自动生成 | 基线名称 |

**示例**

```命令示例：手动设置基线
levee drift baseline set --host web-01 --file baseline.json --name "v1.0 基线"
```

### 22.3 drift baseline auto

从目标机最近一次 apply 的预期状态自动生成基线。

```text
levee drift baseline auto --host <host> [--name <name>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--host` | | 目标主机名（必填） |
| `--name` | 自动生成 | 基线名称 |

**示例**

```命令示例：自动生成基线
levee drift baseline auto --host web-01
```

### 22.4 drift baseline list

列出目标机的所有基线。

```text
levee drift baseline list --host <host>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--host` | | 目标主机名（必填） |

**示例**

```命令示例：列出基线
levee drift baseline list --host web-01
```

### 22.5 drift schedule add

添加定期巡检调度任务。

```text
levee drift schedule add --name <name> --cron <expr> --hosts <hosts> [--baseline <baseline-id>|auto]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--name` | | 调度任务名称（必填） |
| `--cron` | | 5 字段 cron 表达式（必填，如 `0 2 * * *`） |
| `--hosts` | | 目标主机列表，逗号分隔（必填） |
| `--baseline` | `auto` | 基线 ID 或 `auto` |

**示例**

```命令示例：添加每日凌晨巡检
levee drift schedule add --name daily-check --cron "0 2 * * *" --hosts web-01,web-02
```

### 22.6 drift schedule list

列出所有定期巡检调度任务。

```text
levee drift schedule list
```

**示例**

```命令示例：列出巡检调度
levee drift schedule list
```

### 22.7 drift schedule remove

移除定期巡检调度任务。

```text
levee drift schedule remove <schedule-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<schedule-id>` | 是 | 调度任务 ID |

**示例**

```命令示例：移除巡检调度
levee drift schedule remove sched-001
```

### 22.8 drift schedule run

立即触发一次巡检（不等 cron 时刻）。

```text
levee drift schedule run <schedule-id>
```

**参数**

| 参数 | 必填 | 说明 |
|------|------|------|
| `<schedule-id>` | 是 | 调度任务 ID |

**示例**

```命令示例：立即触发巡检
levee drift schedule run sched-001
```

### 22.9 drift report

查看漂移报告与趋势分析。

```text
levee drift report --host <host> [--days <n>] [--format FORMAT]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--host` | | 目标主机名（必填） |
| `--days` | `30` | 报告时间范围（天） |
| `--format` | `text` | 输出格式：`text` 或 `json` |

**输出**

包含漂移次数趋势、漂移项分布、最近一次漂移详情与建议。

**示例**

```命令示例：查看 30 天漂移报告
levee drift report --host web-01 --days 30

命令示例：JSON 格式报告
levee drift report --host web-01 --days 7 --format json
```

## 第23章 push — 推送通知管理

管理移动设备推送通知（APNs / FCM），支持设备注册、推送发送与配置管理。

### 23.1 push register

注册移动设备用于接收推送通知。

```text
levee push register --user <user> --token <device-token> --platform <platform>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 用户名（必填） |
| `--token` | | 设备推送 token（必填） |
| `--platform` | | 平台：`ios` 或 `android`（必填） |

**示例**

```命令示例：注册 iOS 设备
levee push register --user alice --token <device-token> --platform ios

命令示例：注册 Android 设备
levee push register --user bob --token <device-token> --platform android
```

### 23.2 push unregister

注销移动设备。

```text
levee push unregister --user <user> --token <device-token>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 用户名（必填） |
| `--token` | | 设备推送 token（必填） |

**示例**

```命令示例：注销设备
levee push unregister --user alice --token <device-token>
```

### 23.3 push send

向用户的所有已注册设备发送推送通知。

```text
levee push send --user <user> --title <title> --body <body> [--deep-link <url>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 目标用户名（必填） |
| `--title` | | 通知标题（必填） |
| `--body` | | 通知正文（必填） |
| `--deep-link` | | 深度链接 URL，点击通知后跳转（如 `levee://approval/run-123`） |

**说明**

- iOS 设备通过 APNs（HTTP/2 + ES256 JWT）推送
- Android 设备通过 FCM（HTTP v1 + OAuth2）推送
- 失败的设备会标记并返回部分成功结果

**示例**

```命令示例：发送审批推送
levee push send --user alice --title "审批请求" --body "变更 run-123 待审批" --deep-link "levee://approval/run-123"
```

### 23.4 push devices

列出用户已注册的设备。

```text
levee push devices --user <user>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 用户名（必填） |

**示例**

```命令示例：列出用户设备
levee push devices --user alice
```

### 23.5 push test

向用户发送测试推送通知，验证推送配置是否正常。

```text
levee push test --user <user>
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--user` | | 用户名（必填） |

**示例**

```命令示例：测试推送
levee push test --user alice
```

### 23.6 push config

查看或配置 APNs / FCM 推送凭证。

```text
levee push config [--platform <platform>] [--key <path>] [--key-id <id>] [--team-id <id>] [--project <id>] [--service-account <path>]
```

**选项**

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--platform` | | 平台：`ios` 或 `android`（不设置则显示当前配置） |
| `--key` | | APNs 私钥文件路径（p8 格式） |
| `--key-id` | | APNs Key ID |
| `--team-id` | | APNs Team ID |
| `--project` | | FCM 项目 ID |
| `--service-account` | | FCM 服务账号 JSON 文件路径 |

**说明**

- 不带任何选项时显示当前 APNs / FCM 配置状态
- 设置 `--platform ios` 并提供 `--key` / `--key-id` / `--team-id` 配置 APNs
- 设置 `--platform android` 并提供 `--project` / `--service-account` 配置 FCM

**示例**

```命令示例：查看推送配置
levee push config

命令示例：配置 APNs
levee push config --platform ios --key AuthKey.p8 --key-id ABC123 --team-id TEAM456

命令示例：配置 FCM
levee push config --platform android --project my-project --service-account sa.json
```