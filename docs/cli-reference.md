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