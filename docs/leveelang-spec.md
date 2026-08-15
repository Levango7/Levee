# LEVEELang DSL 规范

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEELang DSL 规范 |
| 文档类型 | 语言规范 |
| 版本 | v0.1（MVP 阶段用 YAML 子集，本文档定义目标规范） |
| 日期 | 2026-08-15 |
| 定位 | LEVEE 工作流定义语言的目标规范 |
| 适用范围 | LEVEELang 编译器、模板作者、workflow 开发者 |
| 关联文档 | LEVEE 设计文档（levee-design.md）第 4 章 D2、第 5 章 |

---

## 第1章 概述

### 1.1 LEVEELang 是什么

LEVEELang 是 LEVEE 的领域专用语言（DSL），用于定义非云原生基础设施的变更工作流。它把"改一台主机 / 数据库 / 网络设备 / 中间件"这件事从手工命令、线性 playbook、堡垒机点选，升级为带类型、带审批、带分批、带验证门禁、带回滚、带审计的声明式工作流。

LEVEELang 不是通用编程语言，不追求图灵完备，而是面向变更治理领域收敛语法原语。它的核心表达力集中在以下五类对象：

- 目标集（target）：变更作用范围。
- 变更窗口（window）：时间约束。
- 批次（batches）：分批策略与边界。
- 审批（approval）：人工门禁分级。
- 步骤（step）：动作序列、验证门禁、回滚声明。

### 1.2 设计目标

表：LEVEELang 设计目标与对应机制对照表

| 设计目标 | 机制 | 收益 |
| --- | --- | --- |
| 类型化 | 静态类型系统，编译期检查 | 运行期不做隐式类型推断，错误前置 |
| 编译期校验 | LEVEELang 编译为 IR，编译期做类型检查、引用检查、影响面分析 | plan 哈希锁定，审批基于不可变计划 |
| 模板与逻辑分离 | 动作模块封装执行逻辑，workflow 只声明编排 | 模块可复用、可测试、可版本化 |
| 原生原语 | batch / gate / rollback / pause 是一等原语 | 非模板字符串，语法显式、可读性强 |
| 可读性优先 | 语法贴近运维心智，关键字短、结构扁平 | 降低学习成本，IDE 补全友好 |
| 模块化 | workflow 声明 input / output，跨 workflow 调用走显式接口 | 复用有契约，不靠隐式 include |

### 1.3 与 Ansible YAML 的对比

Ansible YAML + Jinja2 在大型变更场景存在结构性缺陷，LEVEELang 针对性解决。

表：LEVEELang 与 Ansible YAML 对照表

| 维度 | Ansible YAML + Jinja2 | LEVEELang |
| --- | --- | --- |
| 类型系统 | 无，变量类型运行期暴露 | 静态类型，编译期检查 |
| 引号处理 | YAML 引号规则复杂，Jinja2 模板字符串嵌套引号地狱 | 显式类型字面量，无引号歧义 |
| 变量优先级 | 多层 inventory / group_vars / host_vars / extra_vars 优先级混乱 | 显式 input 声明，无隐式覆盖 |
| 批次边界 | serial: 1 + when 模拟，非一等公民 | batches 一等原语，显式声明 |
| 验证门禁 | 靠 post_task 手写，无时序约束 | gate 原语 + pre_apply / post_batch / grace_period 三段时序 |
| 回滚 | ignore_errors + 手写 undo，无白名单 | rollback 原语 + 白名单 + 不可逆标记 |
| 审批 | 无原生支持 | approval 原语 + 三级审批 |
| 报错定位 | YAML 行号 + Jinja2 模板行号错位 | 编译期错误码 + 精确行号 + 类型信息 |
| 复用 | include / import 无接口契约 | workflow input / output 显式契约 |
| 编译期分析 | 无 | IR + 影响面分析 + 冲突检测 + plan 哈希 |

典型痛点对照：

- 引号地狱：Ansible 中 `cmd: "echo '{{ var }}' && do_something '{{ other_var }}'"` 三层引号嵌套，编辑易错；LEVEELang 用 `cmd { run: "..."  args: {...} }` 结构化表达，引号层级扁平。
- 变量优先级混乱：Ansible 中同一变量被 inventory、group_vars、host_vars、extra_vars 多层覆盖，最终值难追溯；LEVEELang 变量只来自 input 声明和 step output，来源单一。
- 报错错行：Ansible 报错指向 YAML 行号，但实际错误在 Jinja2 模板内部，定位需二次解读；LEVEELang 编译期报错带类型信息和精确行号，一次定位。

---

## 第2章 语法结构

### 2.1 顶层结构

LEVEELang 的顶层结构是 `workflow` 声明块，一个文件可包含一个或多个 workflow 声明。

workflow 声明块包含以下子块，顺序建议但不强制：

1. `input`：参数声明块（可选）。
2. `target`：目标集声明块（必需）。
3. `window`：变更窗口声明块（可选）。
4. `batches`：批次声明块（可选，缺省单批）。
5. `approval`：审批声明块（可选，缺省 standard）。
6. `step`：步骤声明块（必需，可多个）。
7. `gate`：门禁声明块（可选，可多个）。
8. `rollback`：回滚声明块（必需，对应设计红线 R2）。

代码示例：完整 workflow 顶层结构

```leveelang
workflow <name> {
  input {
    <param>: <type> [= <default>]    # 参数声明
  }

  target {
    type: "<asset-type>"
    query: "<label-expr>"            # 标签表达式选目标
  }

  window {
    start: "HH:MM"
    end:   "HH:MM"
    timezone: "<tz>"
  }

  batches {
    strategy: "<strategy-name>"
    # 批次划分声明
    gate post_batch { ... }          # 批次间门禁
  }

  approval {
    level: <approval_level>
    min_approvers: <int>
    exclude_initiator: <bool>
    timeout: <duration>
  }

  step <step-name> {
    action: "<module.name>"
    args { ... }
    requires_reboot: <bool>
    irreversible: <bool>
  }

  gate <position> { ... }            # pre_apply / post_apply 门禁

  rollback {
    strategy: "<rollback-strategy>"
    on_failure: "auto" | "manual"
    verify_after: <bool>
    step <undo-step> { ... }
  }
}
```

### 2.2 关键字清单

关键字分为声明关键字、块关键字、字段关键字、修饰关键字四类。

表：声明关键字清单

| 关键字 | 语义 | 必需 | 示例 |
| --- | --- | --- | --- |
| workflow | 声明一个工作流，后接 name | 是 | `workflow db-migrate { }` |
| input | 声明 workflow 输入参数 | 否 | `input { table: string }` |
| target | 声明目标集 | 是 | `target { type: "mysql" }` |
| window | 声明变更窗口 | 否 | `window { start: "02:00" }` |
| batches | 声明批次策略 | 否 | `batches { strategy: "percent" }` |
| approval | 声明审批要求 | 否 | `approval { level: high }` |
| step | 声明一个步骤，后接 step-name | 是 | `step migrate { }` |
| gate | 声明验证门禁，后接 position | 否 | `gate post_batch { }` |
| rollback | 声明回滚计划 | 是 | `rollback { strategy: "snapshot" }` |
| action | 引用动作模块 | 是（step 内） | `action: "mysql.pt-osc"` |
| args | 声明动作参数 | 否 | `args { host: "{{target.host}}" }` |
| verify | 声明验证动作 | 否 | `verify { cmd { ... } }` |
| batch | 批次内单批声明 | 否 | `batch { percent: 10% }` |
| name | 命名字段 | 否 | `name: "canary-batch"` |

表：块关键字清单

| 关键字 | 语义 | 出现位置 |
| --- | --- | --- |
| slo | SLO 门禁块 | gate 内 |
| cmd | 命令门禁块 | gate / verify 内 |
| probe | 探针门禁块 | gate 内 |
| human | 人工门禁块 | gate 内 |

表：字段关键字清单

| 关键字 | 类型 | 语义 |
| --- | --- | --- |
| type | string | 资产类型或目标类型 |
| query | label_expr | 标签表达式选目标 |
| start | string | 窗口起始时间 HH:MM |
| end | string | 窗口结束时间 HH:MM |
| timezone | string | 时区，IANA 名称 |
| strategy | string | 批次或回滚策略名 |
| level | approval_level | 审批级别 |
| min_approvers | int | 最少审批人数 |
| exclude_initiator | bool | 是否排除发起人审批 |
| timeout | duration | 超时时长 |
| run | string | 命令门禁要执行的命令 |
| expect_exit | int | 期望退出码 |
| expect_stdout | string | 期望 stdout 匹配（正则） |
| source | string | SLO 指标来源（如 prometheus） |
| wait | duration | 等待时长（grace period） |
| requires_reboot | bool | 该步是否需要目标机重启 |
| irreversible | bool | 该步是否不可逆 |
| on_failure | string | 回滚触发策略：auto / manual |
| verify_after | bool | 回滚后是否验证 |

表：修饰关键字清单

| 关键字 | 语义 |
| --- | --- |
| pre_apply | 门禁位置：变更前 |
| post_batch | 门禁位置：每批后 |
| post_apply | 门禁位置：全部变更后 |
| rest | 批次声明中的剩余全部 |
| auto | 自动触发 |
| manual | 人工触发 |

### 2.3 注释规则

LEVEELang 支持单行注释和多行注释，注释不参与编译，不出现在 IR 中。

单行注释以 `#` 起始，从 `#` 到行尾为注释内容：

```leveelang
workflow example {
  # 这是单行注释，整行被忽略
  target {
    type: "mysql"   # 行尾注释，从 # 到行尾被忽略
    query: "role=primary"
  }
}
```

多行注释以 `/*` 起始、`*/` 结束，可跨行，不可嵌套：

```leveelang
workflow example {
  /*
   * 这是多行注释，
   * 可跨多行，常用于解释复杂批次策略。
   */
  target {
    type: "mysql"
    query: "role=primary"
  }
}
```

多行注释不可嵌套，`/* /* */ */` 中第一个 `*/` 即结束，后续 `*/` 报语法错误。

---

## 第3章 类型系统

### 3.1 基本类型

LEVEELang 静态类型系统在编译期完成类型检查，运行期不做隐式类型推断（对应设计红线 R6）。

表：基本类型清单

| 类型 | 语义 | 字面量示例 | 用途 |
| --- | --- | --- | --- |
| string | 字符串 | `"mysql"` | 名称、命令、路径 |
| int | 整数 | `42` | 数量、退出码、并发数 |
| float | 浮点数 | `0.01` | 阈值、比率 |
| bool | 布尔 | `true` / `false` | 开关、标记 |
| duration | 时长 | `5m` / `4h` / `30s` | 超时、grace period |
| percent | 百分比 | `10%` | 批次百分比 |
| label_expr | 标签表达式 | `role=primary AND az=a` | target query |
| percent_array | 百分比数组 | `[1, 10, 50, 100]` | batches 划分 |
| approval_level | 审批枚举 | `standard` / `high` / `emergency` | approval level |
| action_ref | 动作引用 | `mysql.pt-online-schema-change` | step action |
| target_ref | 目标引用 | `{{target.host}}` | args 中引用目标属性 |
| input_ref | 输入引用 | `{{input.table}}` | args 中引用输入参数 |
| output_ref | 输出引用 | `{{step.migrate.output}}` | 引用前步输出 |

duration 字面量支持单位：`s`（秒）、`m`（分）、`h`（时）、`d`（天），如 `90s`、`5m`、`4h`、`1d`。

percent 字面量以 `%` 后缀标识，如 `10%`，等价于 float 0.10，但类型独立以避免与 float 混用。

### 3.2 标签表达式

标签表达式（label_expr）用于 target query，按标签键值对圈选目标集。语法支持四种原子谓词和逻辑组合。

表：标签表达式原子谓词

| 谓词 | 语义 | 示例 |
| --- | --- | --- |
| `key=value` | 标签 key 等于 value | `role=primary` |
| `key!=value` | 标签 key 不等于 value | `role!=replica` |
| `key in [v1,v2]` | 标签 key 在值列表中 | `az in [a,b]` |
| `key matches <regex>` | 标签 key 匹配正则 | `hostname matches "^db-.*-primary$"` |

原子谓词通过 `AND`、`OR`、`NOT` 组合，`AND` 优先级高于 `OR`，可用括号改变优先级。

代码示例：标签表达式

```leveelang
target {
  # 选所有 az=a 或 az=b 的 MySQL 主库
  query: "role=primary AND az in [a,b] AND cluster=orders-db"
}

target {
  # 选非 canary 的所有 Linux 主机
  query: "os=linux AND NOT tier=canary"
}

target {
  # 按主机名正则圈选
  query: "hostname matches \"^web-\\d+-prod$\" AND env=prod"
}
```

标签表达式编译期做语法检查（括号匹配、正则合法性、键名符合命名规范），运行期由 target resolver 物化为具体目标列表。

### 3.3 百分比数组

百分比数组（percent_array）用于 batches 声明按百分比划分批次。

语法形式：`[p1, p2, ..., pn]`，其中 `pi` 为正整数，表示累计百分比。

语义：`[1, 10, 50, 100]` 表示四批，累计目标机比例分别达到 1%、10%、50%、100%，即：

- 第 1 批：1% 的目标机（金丝雀）。
- 第 2 批：累计 10%，即本批新增 9%。
- 第 3 批：累计 50%，即本批新增 40%。
- 第 4 批：累计 100%，即本批新增剩余 50%。

边界规则：

1. 数组必须递增，`p1 < p2 < ... < pn`，否则编译期报错 LE031。
2. 最后一个元素必须为 100，表示覆盖全部目标机，否则编译期报错 LE032。
3. 每批目标机数量向下取整，但每批至少 1 台（避免小规模时首批 0 台）。
4. 最后一批包含剩余全部目标机（避免取整导致遗漏）。
5. 首批建议不超过 5%（金丝雀原则），超过告警 LE033（warning，不阻断）。

代码示例：百分比数组

```leveelang
batches {
  strategy: "percent"
  steps: [1, 10, 50, 100]    # 1 台金丝雀 → 10% → 50% → 100%
}

batches {
  strategy: "percent"
  steps: [5, 30, 100]        # 5% → 30% → 100%
}
```

也可用 `rest` 关键字显式标记最后一批为剩余全部：

```leveelang
batches {
  strategy: "percent"
  steps: [1, 10, 50, rest]   # 等价于 [1, 10, 50, 100]
}
```

### 3.4 审批枚举

审批枚举（approval_level）对应设计文档 4.4.3.1 的三级审批。

表：审批级别枚举

| 枚举值 | 触发条件 | 审批人要求 | 超时 | 超时处理 |
| --- | --- | --- | --- | --- |
| standard | 默认级别 | 任一有权限审批人 | 24h | 超时驳回 |
| high | 命中高危规则（删库、主从切换、防火墙全量变更等） | 至少 2 人审批，且不能是发起人 | 4h | 超时驳回 |
| emergency | 紧急通道（线上故障恢复） | 1 人审批 + 事后补审 | 15min | 超时自动驳回并升级告警 oncall |

枚举值在编译期校验合法性，非三类之一报错 LE041。

---

## 第4章 字段定义

### 4.1 target 字段

target 字段声明变更的目标集，是 workflow 的必需块。

表：target 字段定义

| 字段 | 类型 | 必需 | 语义 | 约束 |
| --- | --- | --- | --- | --- |
| type | string | 是 | 资产类型，决定通道与动作模块 | 必须在资产类型白名单内 |
| query | label_expr | 是 | 标签表达式圈选目标 | 编译期语法检查 |
| static | bool | 否 | 是否静态目标集，缺省 false | 静态集在 plan 前固定 |
| min_count | int | 否 | 目标集最小数量，少于则阻断 plan | 正整数 |
| max_count | int | 否 | 目标集最大数量，超过则阻断 plan | 正整数 |

资产类型白名单（对应设计文档 6.2 资产覆盖）：

- 计算层：`host`、`vm`、`baremetal`
- 存储层：`san`、`nas`、`localdisk`
- 网络层：`router`、`switch`、`firewall`、`lb`
- 数据库层：`mysql`、`postgresql`、`oracle`、`redis`
- 中间件层：`kafka`、`zookeeper`、`nginx`、`tomcat`
- 安全设备：`ids`、`ips`、`waf`、`bastion`

代码示例：target 字段

```leveelang
# 动态目标集
target {
  type: "mysql"
  query: "role=primary AND cluster=orders-db"
  min_count: 1
  max_count: 10
}

# 静态目标集
target {
  type: "host"
  static: true
  query: "hostname in [web-01, web-02, web-03]"
}
```

语义：

- 动态目标集在 plan 阶段物化为具体列表并锁定，apply 阶段不再重新计算（对应设计文档 4.4.2.4）。
- min_count / max_count 在 plan 阶段校验，不满足则阻断 plan，不进审批。

### 4.2 window 字段

window 字段声明变更时间窗口，是 workflow 的可选块，缺省表示无窗口约束（任何时刻可变更，不推荐）。

表：window 字段定义

| 字段 | 类型 | 必需 | 语义 | 约束 |
| --- | --- | --- | --- | --- |
| start | string | 是 | 窗口起始时间 | HH:MM 格式，24 小时制 |
| end | string | 是 | 窗口结束时间 | HH:MM 格式，必须晚于 start |
| timezone | string | 否 | 时区 | IANA 时区名，缺省 UTC |
| days | string[] | 否 | 允许的星期 | ["Mon","Tue",...]，缺省每天 |

时间格式为 24 小时制 `HH:MM`，如 `"02:00"`、`"23:30"`。

时区处理：

- timezone 用 IANA 时区名，如 `"Asia/Shanghai"`、`"America/New_York"`、`"UTC"`。
- 窗口校验在 plan 阶段做：plan 时刻不在窗口内则阻断，不进审批。
- 跨时区团队建议显式声明 timezone，避免隐式 UTC 导致误判。
- 回滚不受窗口约束（对应设计文档 4.4.6.2），即使窗口已关闭回滚仍可执行。

代码示例：window 字段

```leveelang
window {
  start: "02:00"
  end:   "04:00"
  timezone: "Asia/Shanghai"
  days: ["Sat", "Sun"]    # 仅周末窗口
}
```

### 4.3 batches 字段

batches 字段声明批次划分策略，是 workflow 的可选块，缺省表示单批全量执行（不推荐，告警 LE051 warning）。

表：batches 字段定义

| 字段 | 类型 | 必需 | 语义 |
| --- | --- | --- | --- |
| strategy | string | 是 | 批次策略名 |
| steps | percent_array / int[] / batch[] | 是 | 批次划分 |
| gate post_batch | gate 块 | 否 | 批次间门禁，缺省无门禁（告警 LE052 warning） |

批次策略（strategy）取值：

表：批次策略清单

| 策略 | steps 类型 | 语义 | 示例 |
| --- | --- | --- | --- |
| percent | percent_array | 按累计百分比划分 | `steps: [1, 10, 50, 100]` |
| count | int[] | 按数量划分 | `steps: [3, 10, rest]` |
| one-per-target | 无需 steps | 每批一台目标机，串行 | 用于 DB 主库逐个切换 |
| by-tag | batch[] | 按标签分组划分 | `steps: [{tags:[canary]}, {tags:[primary]}]` |
| by-group | batch[] | 按主机组划分 | `steps: [{group:az-a}, {group:az-b}]` |

边界处理：

1. 批次内目标机并发执行，受全局并发上限约束（默认 100）。
2. 批次间串行，前批未完成不进下批。
3. 批次间默认插入 post_batch 门禁，门禁失败阻断后续批次（对应设计红线 R5）。
4. 单批失败不自动回滚已成功批次，由失败语义决定（见设计文档 4.4.9）。

代码示例：batches 字段

```leveelang
# 按百分比
batches {
  strategy: "percent"
  steps: [1, 10, 50, 100]
  gate post_batch {
    slo {
      query: "rate(mysql_errors_total[5m]) < 0.01"
      source: "prometheus"
    }
  }
}

# 按标签分组
batches {
  strategy: "by-tag"
  steps: [
    { tags: [canary], name: "canary-batch" },
    { tags: [primary], name: "primary-batch" },
    { tags: [edge], name: "edge-batch" }
  ]
}

# 每台串行
batches {
  strategy: "one-per-target"
  gate post_batch {
    cmd {
      run: "mysqladmin ping -h {{target.host}}"
      expect_exit: 0
    }
  }
}
```

### 4.4 approval 字段

approval 字段声明审批要求，是 workflow 的可选块，缺省为 standard 级别。

表：approval 字段定义

| 字段 | 类型 | 必需 | 语义 | 约束 |
| --- | --- | --- | --- | --- |
| level | approval_level | 是 | 审批级别 | standard / high / emergency |
| min_approvers | int | 否 | 最少审批人数 | 缺省按 level 默认值 |
| exclude_initiator | bool | 否 | 是否排除发起人 | high 强制 true |
| timeout | duration | 否 | 审批超时 | 缺省按 level 默认值 |
| auto_approve | bool | 否 | 是否允许自动审批 | high / emergency 强制 false |

各级别默认值与强制约束：

表：审批级别默认值与强制约束

| 级别 | 默认 min_approvers | 默认 timeout | 强制约束 |
| --- | --- | --- | --- |
| standard | 1 | 24h | 无 |
| high | 2 | 4h | exclude_initiator 强制 true，auto_approve 强制 false |
| emergency | 1 | 15min | auto_approve 强制 false，需事后补审 |

代码示例：approval 字段

```leveelang
# 高危审批
approval {
  level: high
  min_approvers: 2
  exclude_initiator: true
  timeout: 4h
}

# 紧急审批
approval {
  level: emergency
  timeout: 15m
}

# 标准审批（可省略，缺省即 standard）
approval {
  level: standard
}
```

语义：

- 高危变更不允许 auto-approve，审批人不能是发起人（对应设计红线 R4）。
- 审批超时默认驳回，紧急审批超时升级告警 oncall（对应设计文档 4.4.3.2）。
- 驳回需填写理由，重提生成新 run-id，不复用旧 run（对应设计文档 4.4.3.3）。

### 4.5 step 块

step 块声明一个变更步骤，是 workflow 的必需块，可声明多个。step 之间按 DAG 编排（对应设计文档 4.5.2），支持并行与依赖声明，不支持循环。

表：step 字段定义

| 字段 | 类型 | 必需 | 语义 |
| --- | --- | --- | --- |
| action | action_ref | 是 | 引用动作模块 |
| args | 块 | 否 | 动作参数 |
| batch | string | 否 | 覆盖批次策略，仅本 step 用 |
| verify | 块 | 否 | 步骤级验证（post_step） |
| requires_reboot | bool | 否 | 是否需要目标机重启，缺省 false |
| irreversible | bool | 否 | 是否不可逆，缺省 false |
| depends_on | string[] | 否 | 显式依赖的前置 step |
| output | 块 | 否 | 输出声明，供后续 step 引用 |

#### 4.5.1 step 名称

step 名称在 step 关键字后声明，需在 workflow 内唯一：

```leveelang
step migrate { ... }
step verify_schema { ... }
step cleanup { ... }
```

命名规则：小写字母、数字、下划线，以字母起始，长度 1-64。

#### 4.5.2 action 引用

action 字段引用动作模块，格式 `module.name`，详见第 5 章。

#### 4.5.3 batch 覆盖

step 可声明 `batch` 字段覆盖 workflow 级批次策略，仅对本 step 生效：

```leveelang
step scan {
  action: "patch.scan"
  batch: "one-per-target"    # 本步逐台扫描，不受 workflow batches 约束
}
```

#### 4.5.4 verify 声明

step 内可声明 verify 块，作为步骤级验证（post_step 时机），与 gate 区别在于 verify 是 step 内联的，gate 是 workflow 级的：

```leveelang
step migrate {
  action: "mysql.pt-online-schema-change"
  args { ... }
  verify {
    cmd {
      run: "mysql -e 'DESCRIBE {{input.table}}' | grep status"
      expect_exit: 0
    }
  }
}
```

#### 4.5.5 rollback 声明

rollback 在 workflow 级声明，引用白名单逆操作，详见第 7 章。

#### 4.5.6 隐式依赖

step 引用前步 output 时自动建立依赖边，无需显式 depends_on：

```leveelang
step scan {
  action: "patch.scan"
  output {
    vulnerable_hosts: string[]    # 输出漏洞主机列表
  }
}

step patch {
  action: "pkg.upgrade"
  args {
    hosts: "{{step.scan.output.vulnerable_hosts}}"    # 引用前步输出，隐式依赖 scan
  }
}
```

引用 `{{step.scan.output.vulnerable_hosts}}` 自动建立 `patch depends_on scan` 的 DAG 边，编译期校验 scan 是否存在、output 是否声明该字段、类型是否匹配。

---

## 第5章 动作声明

### 5.1 action 引用格式

action 字段引用动作模块，格式为 `module.name`，点号分隔模块名与动作名。

- module：动作模块名，对应一类资产或一类操作。
- name：模块内的具体动作名。

命名规则：module 与 name 均为小写字母、数字、连字符，以字母起始。

代码示例：action 引用

```leveelang
action: "patch.scan"                    # patch 模块的 scan 动作
action: "pkg.upgrade"                   # pkg 模块的 upgrade 动作
action: "mysql.pt-online-schema-change" # mysql 模块的 pt-osc 动作
action: "db.schema-migrate"             # db 模块的 schema-migrate 动作
action: "file.copy"                     # file 模块的 copy 动作
action: "svc.restart"                   # svc 模块的 restart 动作
action: "net.acl-update"                # net 模块的 acl-update 动作
```

action 引用编译期校验：

1. 模块是否存在（在已加载模块清单内）。
2. 动作是否存在于该模块内。
3. args 是否满足该动作的参数契约。
4. 该动作是否在 workflow 的白名单内（不可逆动作需显式白名单）。

### 5.2 内置动作模块

MVP 阶段内置 5 个动作模块，覆盖主机运维基础操作。V1 起扩展数据库、网络、中间件模块。

表：MVP 内置动作模块

| 模块 | 动作 | 语义 | 参数 | 不可逆 |
| --- | --- | --- | --- | --- |
| shell | exec | 执行任意命令 | cmd: string, timeout: duration | 视命令而定 |
| file | copy | 复制文件到目标机 | src: string, dest: string, mode: string | 否 |
| file | template | 渲染模板并下发 | src: string, dest: string, vars: map | 否 |
| file | delete | 删除文件 | path: string | 是 |
| file | exists | 检查文件是否存在 | path: string | 否（只读） |
| pkg | install | 安装包 | name: string, version: string | 否 |
| pkg | upgrade | 升级包 | name: string, version: string | 否 |
| pkg | downgrade | 降级包 | name: string, version: string | 否 |
| pkg | remove | 卸载包 | name: string | 是 |
| svc | start | 启动服务 | name: string | 否 |
| svc | stop | 停止服务 | name: string | 否 |
| svc | restart | 重启服务 | name: string | 否 |
| svc | reload | 重载配置不重启进程 | name: string | 否 |
| svc | enable | 设置开机启动 | name: string | 否 |
| svc | disable | 取消开机启动 | name: string | 否 |
| user | create | 创建用户 | name: string, uid: int, groups: string[] | 否 |
| user | modify | 修改用户属性 | name: string, attrs: map | 否 |
| user | delete | 删除用户 | name: string | 是 |
| user | lock | 锁定用户 | name: string | 否 |

不可逆动作（delete、remove）在模块声明 `irreversible: true`，workflow 使用时强制要求白名单并升高审批级别（对应设计文档 4.4.6.1）。

V1 扩展模块（非 MVP）：

表：V1 扩展动作模块

| 模块 | 动作 | 语义 |
| --- | --- | --- |
| mysql | pt-online-schema-change | pt-osc 在线 schema 变更 |
| mysql | logical-backup | 逻辑备份 |
| mysql | switch-primary | 主从切换 |
| pg | schema-migrate | PostgreSQL schema 迁移 |
| patch | scan | 漏洞扫描 |
| net | acl-update | ACL 规则下发 |
| net | route-update | 路由策略变更 |
| net | bgp-neighbor | BGP 邻居配置 |
| net | config-backup | 配置备份 |
| net | config-commit | 配置提交 |
| net | config-restore | 配置恢复 |
| kafka | topic-create | topic 创建 |
| kafka | partition-expand | partition 扩容（不可逆） |
| nginx | config-update | 配置更新 + reload |

### 5.3 动作输入输出

step 间通过 output 传递数据，形成隐式依赖。output 在 step 内声明类型，运行期由动作模块填充。

代码示例：step 间数据传递

```leveelang
step scan {
  action: "patch.scan"
  args {
    host: "{{target.host}}"
  }
  output {
    vulnerable: bool              # 是否发现漏洞
    cve_list: string[]            # CVE 列表
    patch_url: string             # 补丁下载地址
  }
}

step patch {
  action: "pkg.upgrade"
  args {
    host: "{{target.host}}"
    url: "{{step.scan.output.patch_url}}"    # 引用 scan 输出
  }
  # 隐式依赖：patch depends_on scan
}

step verify {
  action: "shell.exec"
  args {
    cmd: "rpm -qa | grep -c {{step.scan.output.cve_list[0]}}"
  }
  # 隐式依赖：verify depends_on scan
}
```

编译期校验：

1. 被引用的 step 是否在当前 workflow 内声明。
2. 被引用的 output 字段是否在该 step 的 output 块内声明。
3. 引用类型是否与 args 期望类型匹配。
4. 引用是否构成循环依赖（DAG 校验，循环报错 LE071）。

---

## 第6章 验证门禁声明

### 6.1 验证类型

LEVEELang 提供四类验证门禁原语，对应设计文档 4.4.5.1。

表：四类验证门禁原语

| 门禁原语 | 检查内容 | 适用时机 | 失败动作 |
| --- | --- | --- | --- |
| cmd | 在目标机执行检查命令，期望 exit_code / stdout 匹配 | pre_apply / post_batch / post_apply / post_step | 阻断后续 |
| slo | 查询外部 SLO 指标，期望在阈值内 | post_batch / post_apply（grace_period） | 阻断后续 + 触发回滚 |
| probe | 调用外部探针端点（健康检查、readiness） | post_batch | 阻断后续 |
| human | 通知人工确认，等待通过 / 驳回 | post_batch（可选） | 超时按策略 |

#### 6.1.1 命令门禁 cmd

```leveelang
gate post_batch {
  cmd {
    run: "mysqladmin ping -h {{target.host}}"
    expect_exit: 0
    expect_stdout: "mysqld is alive"    # 可选，正则匹配
    timeout: 30s                        # 可选，缺省 60s
    retry: 3                            # 可选，缺省 3 次
    interval: 10s                       # 可选，重试间隔，缺省 10s
  }
}
```

#### 6.1.2 SLO 门禁 slo

```leveelang
gate post_batch {
  slo {
    query: "rate(mysql_errors_total[5m]) < 0.01"
    source: "prometheus"
    timeout: 60s
    retry: 3
  }
}
```

SLO 查询表达式语法遵循所选 source 的查询语言（Prometheus 用 PromQL），LEVEELang 不解析查询内容，只做字符串传递与 source 校验。

#### 6.1.3 探针门禁 probe

```leveelang
gate post_batch {
  probe {
    url: "http://{{target.host}}:8080/healthz"
    expect_status: 200
    expect_body: "ok"            # 可选，正则匹配响应体
    timeout: 10s
  }
}
```

#### 6.1.4 人工门禁 human

```leveelang
gate post_batch {
  human {
    message: "批次 {{batch.index}} 完成，请人工确认后继续"
    timeout: 30m
    notify: ["slack:#ops", "email:oncall@corp"]
  }
}
```

#### 6.1.5 内置验证谓词

除上述四类门禁原语，LEVEELang 还提供一组内置验证谓词，可在 cmd / verify 内组合使用：

表：内置验证谓词

| 谓词 | 语义 | 参数 | 示例 |
| --- | --- | --- | --- |
| health.port(n) | 检查端口 n 是否监听 | port: int | `health.port(3306)` |
| process.exists(name) | 检查进程是否存在 | name: string | `process.exists("mysqld")` |
| cmd.run(command) | 执行命令并检查退出码 | command: string | `cmd.run("nginx -t")` |
| metric.qps | 查询 QPS 指标 | source, threshold | `metric.qps < 10000` |
| metric.error_rate | 查询错误率 | source, threshold | `metric.error_rate < 0.01` |
| slo.error_budget | 查询 SLO 错误预算剩余 | source, threshold | `slo.error_budget > 0.1` |

代码示例：内置验证谓词

```leveelang
gate post_batch {
  cmd {
    run: "levee-check health.port(3306) && levee-check process.exists(mysqld)"
    expect_exit: 0
  }
  slo {
    query: "metric.error_rate < 0.01 AND metric.qps > 1000"
    source: "prometheus"
  }
}
```

### 6.2 验证逻辑

#### 6.2.1 多验证 AND 逻辑

同一 gate 块内多个验证原语默认 AND 逻辑，全部通过才判定门禁通过：

```leveelang
gate post_batch {
  cmd { run: "...", expect_exit: 0 }    # 验证 1
  slo { query: "...", source: "..." }   # 验证 2
  probe { url: "...", expect_status: 200 }  # 验证 3
  # 三者全部通过才通过
}
```

#### 6.2.2 可配 OR 逻辑

需 OR 逻辑时用 `any` 块包裹，任一通过即判定通过：

```leveelang
gate post_batch {
  any {
    cmd { run: "check_local", expect_exit: 0 }
    probe { url: "http://{{target.host}}/healthz", expect_status: 200 }
  }
  # 任一通过即通过
}
```

`any` 块内可嵌套 `all` 块表达 AND-OR 组合，但不支持任意深度嵌套（最多 2 层，避免逻辑复杂化）：

```leveelang
gate post_batch {
  any {
    all {
      cmd { run: "check1", expect_exit: 0 }
      slo { query: "...", source: "..." }
    }
    probe { url: "...", expect_status: 200 }
  }
}
```

### 6.3 SLO 门禁

SLO 门禁分三段时序，覆盖变更前后全窗口（对应设计文档 4.4.5.2）。

表：SLO 门禁三段时序

| 时序 | 时机 | 语义 | 失败动作 |
| --- | --- | --- | --- |
| pre_apply | 变更前 | 查询 SLO 基线，确认变更前系统健康 | 不开始变更 |
| post_batch | 每批后 | 立即查询，确认本批未引入异常 | 阻断后续批次 + 触发回滚 |
| grace_period | 全部批次后 | 等待 grace period 后再查询，确认无延迟暴露异常 | 触发回滚 |

代码示例：SLO 门禁三段时序

```leveelang
# pre_apply：变更前基线
gate pre_apply {
  slo {
    query: "rate(mysql_errors_total[5m]) < 0.01"
    source: "prometheus"
  }
}

# post_batch：每批后
batches {
  strategy: "percent"
  steps: [1, 10, 50, 100]
  gate post_batch {
    slo {
      query: "rate(mysql_errors_total[5m]) < 0.01"
      source: "prometheus"
    }
  }
}

# post_apply + grace_period：全部变更后等待再查
gate post_apply {
  slo {
    query: "rate(mysql_errors_total[5m]) < 0.01"
    source: "prometheus"
    wait: 5m                # grace period，等待 5 分钟再查
  }
}
```

grace_period 配置：

- `wait` 字段声明 grace period 时长，缺省 5m。
- grace period 内不查询，仅等待，避免变更刚结束指标抖动误判。
- grace period 结束后查询，失败触发回滚。
- grace period 可被 workflow input 参数化：`wait: "{{input.grace_period}}"`。

重试与超时（对应设计文档 4.4.5.3）：

- 门禁失败可配置重试（默认 3 次，间隔 10s），避免瞬时抖动误判。
- 重试全失败才判定门禁失败。
- 门禁整体超时（默认 5min），超时判定失败。

---

## 第7章 回滚声明

### 7.1 rollback 字段

rollback 字段声明回滚计划，是 workflow 的必需块（对应设计红线 R2：变更必须可回滚）。

表：rollback 字段定义

| 字段 | 类型 | 必需 | 语义 |
| --- | --- | --- | --- |
| strategy | string | 是 | 回滚策略：snapshot / undo-action / config-revert |
| on_failure | string | 是 | 触发策略：auto / manual |
| verify_after | bool | 否 | 回滚后是否验证，缺省 true |
| step | 块 | 否 | 回滚步骤声明（undo-action 策略时必需） |

回滚策略：

表：回滚策略清单

| 策略 | 语义 | 适用场景 |
| --- | --- | --- |
| snapshot | 按 apply 前快照恢复 | 文件配置、设备 running-config |
| undo-action | 执行逆操作动作 | schema 变更（pt-osc reverse）、包降级 |
| config-revert | 回退到上一版本配置 | Nginx 配置、防火墙规则 |

代码示例：snapshot 回滚

```leveelang
rollback {
  strategy: "snapshot"
  on_failure: "auto"
  verify_after: true
}
```

代码示例：undo-action 回滚

```leveelang
rollback {
  strategy: "undo-action"
  on_failure: "auto"
  verify_after: true
  step undo_migrate {
    action: "mysql.pt-online-schema-change"
    args {
      host: "{{target.host}}"
      table: "{{input.table}}"
      alter: "DROP COLUMN status"
    }
  }
}
```

回滚执行策略（对应设计文档 4.4.6.3）：

1. 白名单：只有声明了 rollback 的 workflow 才可自动回滚。
2. 快照：按 apply 前创建的快照恢复。
3. 按批逆序：从最后一批向前逐批回滚，每批回滚后做回滚后验证。
4. 回滚不受窗口约束（对应设计文档 4.4.6.2）。

### 7.2 不可逆操作

部分动作天然不可逆（如 `DROP TABLE`、`DELETE FROM`、Kafka partition 增加），在模块声明 `irreversible: true`。

不可逆操作的处理：

1. workflow 使用不可逆动作时强制声明 `irreversible: true` 显式标记。
2. 强制进入白名单：显式允许该不可逆动作在该 workflow 使用。
3. 审批级别强制升高到 high（即使 workflow 声明 standard，编译期自动升高并告警 LE081）。
4. 不参与自动回滚：回滚协议跳过该动作，仅回滚可逆部分。
5. 强制 snapshot 回滚：不可逆动作所在 workflow 的 rollback strategy 强制为 snapshot，并要求 apply 前创建完整快照。

代码示例：不可逆操作标记

```leveelang
step drop_temp_table {
  action: "mysql.exec"
  args {
    host: "{{target.host}}"
    sql: "DROP TABLE temp_orders_2024"
  }
  irreversible: true    # 显式标记不可逆
}

rollback {
  strategy: "snapshot"    # 不可逆操作强制 snapshot 回滚
  on_failure: "manual"    # 不可逆操作建议 manual 回滚
  verify_after: true
}
```

不可逆操作白名单在 workflow 头部声明（可选，缺省禁止不可逆动作）：

```leveelang
workflow cleanup-temp-tables {
  allow_irreversible: ["mysql.exec"]    # 白名单允许 mysql.exec 不可逆

  target { ... }
  step drop_temp_table {
    action: "mysql.exec"
    args { ... }
    irreversible: true
  }
  rollback { strategy: "snapshot", on_failure: "manual" }
}
```

---

## 第8章 编译期校验

### 8.1 校验规则

LEVEELang 编译为 IR（中间表示）时执行以下编译期校验，全部通过才进入 plan 阶段。校验失败产出错误码与精确位置，不进入运行期。

表：编译期校验规则

| 编号 | 校验项 | 校验内容 | 失败错误码 |
| --- | --- | --- | --- |
| V1 | 类型检查 | 所有字段值类型与声明类型匹配 | LE001 |
| V2 | 必需字段检查 | 必需字段是否声明 | LE002 |
| V3 | 枚举值检查 | 枚举字段取值在允许集合内 | LE003 |
| V4 | 标签表达式语法 | target query 语法合法、括号匹配、正则合法 | LE010 |
| V5 | 标签键名规范 | 标签键符合命名规范（小写字母数字下划线） | LE011 |
| V6 | 批次定义合法性 | 百分比数组递增、末尾 100、首批 ≤5% | LE031/LE032/LE033 |
| V7 | 批次策略一致性 | strategy 与 steps 类型匹配 | LE034 |
| V8 | step 引用前步输出 | 被引用 step 存在、output 字段存在、类型匹配 | LE061 |
| V9 | DAG 无环 | step 依赖不构成循环 | LE071 |
| V10 | action 模块存在 | 引用的 module.name 在已加载清单内 | LE041 |
| V11 | action 参数契约 | args 满足动作声明的参数契约 | LE042 |
| V12 | verify 表达式语法 | cmd / slo / probe 表达式语法合法 | LE051 |
| V13 | rollback action 白名单 | rollback 引用的 action 在白名单内 | LE081 |
| V14 | 不可逆动作白名单 | irreversible: true 的 action 在 allow_irreversible 内 | LE082 |
| V15 | 不可逆动作审批级别 | 含不可逆动作的 workflow approval level ≥ high | LE083 |
| V16 | rollback 必需 | workflow 必须声明 rollback 块 | LE091 |
| V17 | target 必需 | workflow 必须声明 target 块 | LE092 |
| V18 | step 必需 | workflow 至少声明一个 step | LE093 |
| V19 | window 时间合法 | start < end，HH:MM 格式合法 | LE020 |
| V20 | timezone 合法 | timezone 为合法 IANA 时区名 | LE021 |
| V21 | 资产类型白名单 | target.type 在资产类型白名单内 | LE012 |
| V22 | approval 约束 | high 级别 exclude_initiator 强制 true | LE043 |
| V23 | 命名唯一性 | step 名称、input 参数名在 workflow 内唯一 | LE002 |

### 8.2 错误码定义

错误码格式 `LE###`，三位数字，按类别分段。

表：错误码定义

| 错误码 | 类别 | 含义 | 严重度 |
| --- | --- | --- | --- |
| LE001 | 类型 | 类型不匹配 | error |
| LE002 | 结构 | 必需字段缺失或命名重复 | error |
| LE003 | 类型 | 枚举值非法 | error |
| LE010 | 标签 | 标签表达式语法错误 | error |
| LE011 | 标签 | 标签键名不符合命名规范 | error |
| LE012 | 标签 | 资产类型不在白名单 | error |
| LE020 | 窗口 | 时间格式非法或 start ≥ end | error |
| LE021 | 窗口 | timezone 非合法 IANA 名称 | error |
| LE031 | 批次 | 百分比数组非递增 | error |
| LE032 | 批次 | 百分比数组末尾非 100 | error |
| LE033 | 批次 | 首批超过 5%（金丝雀原则） | warning |
| LE034 | 批次 | strategy 与 steps 类型不匹配 | error |
| LE041 | 动作 | action 模块或动作不存在 | error |
| LE042 | 动作 | action 参数不满足契约 | error |
| LE043 | 审批 | high 级别未排除发起人 | error |
| LE044 | 审批 | 审批级别非法（非三类之一） | error |
| LE051 | 门禁 | verify / gate 表达式语法错误 | error |
| LE052 | 门禁 | batches 缺省 post_batch 门禁 | warning |
| LE061 | 引用 | 引用前步输出不存在或类型不匹配 | error |
| LE071 | 依赖 | DAG 存在循环 | error |
| LE081 | 回滚 | rollback action 不在白名单 | error |
| LE082 | 回滚 | 不可逆动作不在 allow_irreversible | error |
| LE083 | 回滚 | 含不可逆动作但审批级别 < high | error |
| LE091 | 结构 | 缺少 rollback 块 | error |
| LE092 | 结构 | 缺少 target 块 | error |
| LE093 | 结构 | 缺少 step 块 | error |
| LE094 | 结构 | 缺少 approval 块（将用缺省 standard，仅 warning） | warning |
| LE095 | 结构 | 缺少 window 块（无窗口约束，仅 warning） | warning |
| LE096 | 结构 | 缺少 batches 块（单批全量，仅 warning） | warning |

严重度语义：

- error：编译失败，不进入 plan 阶段。
- warning：编译通过，但产出告警，建议修正。warning 不阻断流程，但会在 plan 报告中列出供审批人参考。

错误输出格式：

```
LE042: action 参数不满足契约
  file: workflows/db-migrate.leveelang
  line: 18, column: 5
  step: migrate
  action: mysql.pt-online-schema-change
  missing required param: alter
  expected: string, got: (unset)
```

---

## 第9章 完整示例

### 9.1 示例：补丁灰度

场景：对一批 Linux 主机执行 OS 补丁灰度升级，按 AZ 分批，首批 1 台金丝雀，批次间 SLO 与健康门禁，回滚用包降级。

代码示例：补丁灰度 workflow

```leveelang
workflow patch-rolling {
  # 输入参数：补丁包名与版本
  input {
    pkg_name: string              # 补丁包名，如 kernel
    pkg_version: string           # 目标版本，如 5.15.0-91
    grace_period: duration = 5m   # SLO 门禁 grace period
  }

  # 目标集：所有 az=a 的生产 Linux 主机
  target {
    type: "host"
    query: "os=linux AND env=prod AND az=a"
    min_count: 1
    max_count: 500
  }

  # 变更窗口：周末 02:00-06:00 上海时间
  window {
    start: "02:00"
    end:   "06:00"
    timezone: "Asia/Shanghai"
    days: ["Sat", "Sun"]
  }

  # 批次：1 台金丝雀 → 10% → 50% → 100%
  batches {
    strategy: "percent"
    steps: [1, 10, 50, 100]
    gate post_batch {
      # 命令门禁：检查关键服务存活
      cmd {
        run: "systemctl is-active sshd && systemctl is-active crond"
        expect_exit: 0
      }
      # SLO 门禁：错误率与延迟
      slo {
        query: "rate(node_load1[5m]) < 4 AND rate(node_network_drop_total[5m]) < 0.01"
        source: "prometheus"
      }
    }
  }

  # 审批：高危（补丁影响内核）
  approval {
    level: high
    min_approvers: 2
    exclude_initiator: true
    timeout: 4h
  }

  # 步骤 1：扫描漏洞
  step scan {
    action: "patch.scan"
    args {
      host: "{{target.host}}"
      pkg: "{{input.pkg_name}}"
    }
    output {
      vulnerable: bool
      current_version: string
    }
  }

  # 步骤 2：升级补丁（依赖 scan 输出）
  step upgrade {
    action: "pkg.upgrade"
    args {
      host: "{{target.host}}"
      name: "{{input.pkg_name}}"
      version: "{{input.pkg_version}}"
    }
    requires_reboot: true     # 内核补丁需重启
    depends_on: ["scan"]
  }

  # 步骤 3：重启后健康检查
  step health_check {
    action: "shell.exec"
    args {
      host: "{{target.host}}"
      cmd: "uname -r && systemctl is-active sshd"
    }
    verify {
      cmd {
        run: "uname -r | grep -q {{input.pkg_version}}"
        expect_exit: 0
      }
    }
    depends_on: ["upgrade"]
  }

  # 回滚：包降级到原版本
  rollback {
    strategy: "undo-action"
    on_failure: "auto"
    verify_after: true
    step downgrade {
      action: "pkg.downgrade"
      args {
        host: "{{target.host}}"
        name: "{{input.pkg_name}}"
        version: "{{step.scan.output.current_version}}"
      }
    }
  }

  # 变更后 grace period SLO 门禁
  gate post_apply {
    slo {
      query: "rate(node_load1[5m]) < 4"
      source: "prometheus"
      wait: "{{input.grace_period}}"
    }
  }
}
```

逐行注释要点：

- input 声明补丁包名、版本、grace period，类型化且 grace period 有默认值。
- target 用标签表达式圈选 az=a 的生产 Linux 主机，限 1-500 台。
- window 限定周末 02:00-06:00 上海时间窗口。
- batches 按 1/10/50/100 百分比分批，批次间插命令门禁（服务存活）与 SLO 门禁（负载、丢包）。
- approval 高危审批，2 人审批且排除发起人。
- step scan 扫描漏洞并输出当前版本，step upgrade 升级并声明需重启，step health_check 重启后验证内核版本。
- step 间通过 output 传递 current_version，回滚时降级到该版本。
- rollback 用 undo-action 策略，执行 pkg.downgrade 降级。
- post_apply 门禁等待 grace period 后查 SLO。

### 9.2 示例：数据库 schema 变更

场景：对 MySQL 订单库执行 schema 变更，加列 status，按主库逐个切换，pt-osc 在线变更，回滚用 pt-osc reverse。

代码示例：数据库 schema 变更 workflow

```leveelang
workflow db-migrate-orders {
  input {
    table: string                # 待迁移表名
    alter_sql: string            # ALTER 语句
    grace_period: duration = 5m  # SLO 门禁 grace period
  }

  # 目标集：所有 orders-db 集群的 MySQL 主库
  target {
    type: "mysql"
    query: "role=primary AND cluster=orders-db"
    min_count: 1
  }

  # 变更窗口：凌晨 02:00-04:00
  window {
    start: "02:00"
    end:   "04:00"
    timezone: "Asia/Shanghai"
  }

  # 批次：每台主库逐个切换（避免多主库并发 DDL）
  batches {
    strategy: "one-per-target"
    gate post_batch {
      # SLO 门禁：错误率
      slo {
        query: "rate(mysql_errors_total[5m]) < 0.01"
        source: "prometheus"
      }
      # 命令门禁：主库存活
      cmd {
        run: "mysqladmin ping -h {{target.host}}"
        expect_exit: 0
        expect_stdout: "mysqld is alive"
      }
    }
  }

  # 审批：高危（schema 变更）
  approval {
    level: high
    min_approvers: 2
    exclude_initiator: true
    timeout: 4h
  }

  # 步骤：pt-osc 在线 schema 变更
  step migrate {
    action: "mysql.pt-online-schema-change"
    args {
      host: "{{target.host}}"
      table: "{{input.table}}"
      alter: "{{input.alter_sql}}"
    }
    requires_reboot: false
    irreversible: false
  }

  # 回滚：pt-osc reverse（白名单逆操作）
  rollback {
    strategy: "undo-action"
    on_failure: "auto"
    verify_after: true
    step undo_migrate {
      action: "mysql.pt-online-schema-change"
      args {
        host: "{{target.host}}"
        table: "{{input.table}}"
        alter: "DROP COLUMN status"
      }
    }
  }

  # 变更后 grace period SLO 门禁
  gate post_apply {
    slo {
      query: "rate(mysql_errors_total[5m]) < 0.01"
      source: "prometheus"
      wait: "{{input.grace_period}}"
    }
  }
}
```

逐行注释要点：

- input 声明表名、ALTER 语句、grace period，参数化便于复用。
- target 选所有 orders-db 集群的 MySQL 主库，至少 1 台。
- window 限定凌晨 02:00-04:00 低峰窗口。
- batches 用 one-per-target 策略，每台主库逐个切换，避免多主库并发 DDL。
- 批次间 SLO 门禁（错误率）与命令门禁（mysqladmin ping）。
- approval 高危审批，schema 变更属高危。
- step migrate 调用 pt-osc 在线变更，不可逆性 false。
- rollback 用 undo-action，执行 pt-osc reverse 删除新列。
- post_apply 门禁等待 grace period 后查错误率。

### 9.3 示例：网络设备配置变更

场景：对一批防火墙下发 ACL 规则更新，先 canary 分组验证连通性，再全量，回滚用配置回退。

代码示例：网络设备配置变更 workflow

```leveelang
workflow firewall-acl-update {
  input {
    acl_rules: string            # ACL 规则配置文件路径
    canary_group: string = "canary"  # 金丝雀分组标签
  }

  # 目标集：所有生产防火墙
  target {
    type: "firewall"
    query: "env=prod AND vendor=fortinet"
    min_count: 1
  }

  # 变更窗口：工作日 23:00-02:00 维护窗口
  window {
    start: "23:00"
    end:   "02:00"
    timezone: "Asia/Shanghai"
    days: ["Mon", "Tue", "Wed", "Thu", "Fri"]
  }

  # 批次：先 canary 分组，后全量
  batches {
    strategy: "by-tag"
    steps: [
      { tags: [canary], name: "canary-batch" },
      { tags: [primary], name: "primary-batch" }
    ]
    gate post_batch {
      # 探针门禁：业务连通性
      probe {
        url: "http://monitor.corp/api/connectivity?from=app&to=db"
        expect_status: 200
        expect_body: "\"ok\":true"
        timeout: 30s
      }
      # SLO 门禁：丢包率
      slo {
        query: "rate(node_network_drop_total[5m]) < 0.001"
        source: "prometheus"
      }
    }
  }

  # 审批：高危（防火墙全量变更）
  approval {
    level: high
    min_approvers: 2
    exclude_initiator: true
    timeout: 4h
  }

  # 步骤 1：备份当前配置
  step backup {
    action: "net.config-backup"
    args {
      host: "{{target.host}}"
      dest: "/backup/firewall/{{target.host}}-{{run.id}}.cfg"
    }
    output {
      backup_path: string
    }
  }

  # 步骤 2：下发新 ACL
  step update_acl {
    action: "net.acl-update"
    args {
      host: "{{target.host}}"
      rules: "{{input.acl_rules}}"
    }
    depends_on: ["backup"]
  }

  # 步骤 3：提交配置
  step commit {
    action: "net.config-commit"
    args {
      host: "{{target.host}}"
    }
    depends_on: ["update_acl"]
  }

  # 回滚：回退到备份配置
  rollback {
    strategy: "config-revert"
    on_failure: "auto"
    verify_after: true
    step revert {
      action: "net.config-restore"
      args {
        host: "{{target.host}}"
        from: "{{step.backup.output.backup_path}}"
      }
    }
  }

  # 变更后探针门禁
  gate post_apply {
    probe {
      url: "http://monitor.corp/api/connectivity?from=app&to=db"
      expect_status: 200
      timeout: 30s
    }
  }
}
```

逐行注释要点：

- input 声明 ACL 规则文件路径与 canary 分组标签。
- target 选所有生产 Fortinet 防火墙。
- window 限定工作日夜间维护窗口。
- batches 用 by-tag 策略，先 canary 分组后 primary 分组。
- 批次间探针门禁（业务连通性）与 SLO 门禁（丢包率）。
- approval 高危审批，防火墙全量变更属高危。
- step backup 备份当前配置并输出备份路径，step update_acl 下发新 ACL，step commit 提交配置。
- step 间通过 output 传递 backup_path，回滚时从该路径恢复。
- rollback 用 config-revert 策略，执行 net.config-restore 从备份恢复。
- post_apply 探针门禁验证业务连通性。

### 9.4 示例：批量文件分发

场景：向一批 Web 服务器分发新配置文件，按上游分组分批，分发后验证文件校验和，回滚用旧文件恢复。

代码示例：批量文件分发 workflow

```leveelang
workflow distribute-config {
  input {
    src_file: string             # 源文件路径（控制端）
    dest_path: string            # 目标路径
    checksum: string             # 期望校验和（sha256）
    file_mode: string = "0644"   # 文件权限，默认 0644
  }

  # 目标集：所有生产 Web 服务器
  target {
    type: "host"
    query: "role=web AND env=prod"
    min_count: 1
    max_count: 200
  }

  # 变更窗口：任意时刻（低风险配置分发）
  window {
    start: "00:00"
    end:   "23:59"
    timezone: "Asia/Shanghai"
  }

  # 批次：按上游分组分批
  batches {
    strategy: "by-group"
    steps: [
      { group: upstream-a, name: "upstream-a-batch" },
      { group: upstream-b, name: "upstream-b-batch" },
      { group: upstream-c, name: "upstream-c-batch" }
    ]
    gate post_batch {
      # 命令门禁：校验和匹配
      cmd {
        run: "sha256sum {{input.dest_path}} | awk '{print $1}'"
        expect_stdout: "{{input.checksum}}"
        expect_exit: 0
      }
      # 探针门禁：Nginx 配置语法检查
      probe {
        url: "http://{{target.host}}:8080/nginx-status"
        expect_status: 200
        timeout: 10s
      }
    }
  }

  # 审批：标准（配置文件分发，低风险）
  approval {
    level: standard
    min_approvers: 1
    timeout: 24h
  }

  # 步骤 1：备份现有文件
  step backup {
    action: "file.copy"
    args {
      src: "{{input.dest_path}}"
      dest: "{{input.dest_path}}.bak.{{run.id}}"
      host: "{{target.host}}"
    }
    output {
      backup_path: string
    }
  }

  # 步骤 2：分发新文件
  step distribute {
    action: "file.copy"
    args {
      src: "{{input.src_file}}"
      dest: "{{input.dest_path}}"
      mode: "{{input.file_mode}}"
      host: "{{target.host}}"
    }
    depends_on: ["backup"]
  }

  # 步骤 3：reload 服务使配置生效
  step reload {
    action: "svc.reload"
    args {
      name: "nginx"
      host: "{{target.host}}"
    }
    verify {
      cmd {
        run: "nginx -t"
        expect_exit: 0
      }
    }
    depends_on: ["distribute"]
  }

  # 回滚：恢复备份文件
  rollback {
    strategy: "undo-action"
    on_failure: "auto"
    verify_after: true
    step restore {
      action: "file.copy"
      args {
        src: "{{step.backup.output.backup_path}}"
        dest: "{{input.dest_path}}"
        host: "{{target.host}}"
      }
    }
    step reload_after_rollback {
      action: "svc.reload"
      args {
        name: "nginx"
        host: "{{target.host}}"
      }
    }
  }
}
```

逐行注释要点：

- input 声明源文件、目标路径、期望校验和、文件权限，权限有默认值 0644。
- target 选所有生产 Web 服务器，限 1-200 台。
- window 全天窗口（配置分发低风险，但仍声明窗口便于审计）。
- batches 用 by-group 策略，按上游分组分三批。
- 批次间命令门禁（校验和匹配）与探针门禁（Nginx 状态）。
- approval 标准审批，配置文件分发属低风险。
- step backup 备份现有文件并输出备份路径，step distribute 分发新文件，step reload reload Nginx 并内联 verify 检查配置语法。
- rollback 用 undo-action 策略，恢复备份文件后 reload，含两个回滚步骤。
- 回滚后 verify_after: true 强制验证。

---

## 第10章 MVP 阶段限制

### 10.1 MVP 用 YAML 子集

MVP 阶段（3 个月）不实现完整 LEVEELang 语法与编译器，而是用 YAML 子集表达 workflow，由 MVP 解析器做基础校验后执行。完整 LEVEELang 编译期类型检查在 V1（6 个月）引入。

原因：

1. MVP 优先保证"第一天能干活"，单二进制零依赖，不引入编译器复杂度。
2. YAML 子集兼容现有运维心智，降低迁移门槛。
3. 兼容层可导入现有 Ansible playbook，作为过渡。
4. 完整类型系统与 IR 编译器工程量大，放 V1 交付。

### 10.2 V1 引入完整语法

V1 阶段引入完整 LEVEELang：

- 完整语法解析器（本文档定义的全部关键字与类型）。
- 编译期类型检查（第 8 章 V1-V23 全部校验项）。
- IR（中间表示）与 plan 哈希锁定。
- 错误码 LE001-LE099 全部实现。
- IDE 插件（语法高亮、补全、错误提示）。

### 10.3 MVP 支持的 YAML 子集字段

MVP 阶段 YAML 子集支持以下字段，对应本文档的目标语义但语法为 YAML：

表：MVP YAML 子集字段

| YAML 字段 | 对应 LEVEELang 字段 | MVP 支持 | 限制 |
| --- | --- | --- | --- |
| name | workflow name | 是 |  |
| target.type | target.type | 是 | 仅 host / mysql |
| target.query | target.query | 是 | 仅 key=value 简单表达式 |
| target.hosts | target（静态） | 是 | 静态主机列表 |
| window.start | window.start | 是 |  |
| window.end | window.end | 是 |  |
| window.timezone | window.timezone | 是 | 缺省 UTC |
| batches.strategy | batches.strategy | 是 | 仅 percent / one-per-target |
| batches.steps | batches.steps | 是 | 仅百分比数组 |
| approval.level | approval.level | 是 | standard / high / emergency |
| approval.min_approvers | approval.min_approvers | 是 |  |
| approval.timeout | approval.timeout | 是 |  |
| steps[].name | step name | 是 |  |
| steps[].action | step.action | 是 | 仅 MVP 内置模块 |
| steps[].args | step.args | 是 |  |
| steps[].verify | step.verify | 是 | 仅 cmd 门禁 |
| gates[].position | gate position | 是 | 仅 post_batch / post_apply |
| gates[].cmd | gate.cmd | 是 |  |
| gates[].slo | gate.slo | 是 | 仅 PromQL 查询 |
| rollback.strategy | rollback.strategy | 是 | 仅 snapshot |
| rollback.on_failure | rollback.on_failure | 是 |  |

MVP 不支持（V1 引入）：

- 完整标签表达式（key!=value、key in []、key matches regex）。
- by-tag / by-group 批次策略。
- probe / human 门禁。
- 内置验证谓词（health.port、process.exists 等）。
- step output 与隐式依赖。
- 不可逆动作标记与白名单。
- undo-action / config-revert 回滚策略。
- 编译期类型检查与 IR。
- 完整错误码（仅基础错误，无类型错误）。

MVP 阶段 workflow 示例（YAML 子集）：

```yaml
name: patch-rolling-mvp
target:
  type: host
  query: "os=linux AND env=prod"
  min_count: 1
  max_count: 100
window:
  start: "02:00"
  end: "06:00"
  timezone: "Asia/Shanghai"
batches:
  strategy: percent
  steps: [1, 10, 50, 100]
approval:
  level: high
  min_approvers: 2
  timeout: 4h
steps:
  - name: upgrade
    action: pkg.upgrade
    args:
      name: kernel
      version: 5.15.0-91
gates:
  - position: post_batch
    cmd:
      run: "systemctl is-active sshd"
      expect_exit: 0
  - position: post_apply
    slo:
      query: "rate(node_load1[5m]) < 4"
      source: prometheus
rollback:
  strategy: snapshot
  on_failure: auto
```

该 YAML 子集在 MVP 阶段由解析器做基础校验（必需字段、枚举值、批次合法性），不做完整类型检查。V1 阶段同一 workflow 可改写为 LEVEELang 语法获得完整编译期校验。

---

## 附录 A 关键字速查

表：LEVEELang 关键字速查表

| 关键字 | 类别 | 出现位置 | 简述 |
| --- | --- | --- | --- |
| workflow | 声明 | 顶层 | 声明工作流 |
| input | 声明 | workflow 内 | 声明输入参数 |
| target | 声明 | workflow 内 | 声明目标集 |
| window | 声明 | workflow 内 | 声明变更窗口 |
| batches | 声明 | workflow 内 | 声明批次策略 |
| approval | 声明 | workflow 内 | 声明审批要求 |
| step | 声明 | workflow 内 | 声明步骤 |
| gate | 声明 | workflow / batches 内 | 声明验证门禁 |
| rollback | 声明 | workflow 内 | 声明回滚计划 |
| action | 字段 | step 内 | 引用动作模块 |
| args | 声明 | step 内 | 声明动作参数 |
| output | 声明 | step 内 | 声明步骤输出 |
| verify | 声明 | step 内 | 声明步骤级验证 |
| batch | 字段 | step 内 | 覆盖批次策略 |
| depends_on | 字段 | step 内 | 显式依赖声明 |
| slo | 块 | gate 内 | SLO 门禁 |
| cmd | 块 | gate / verify 内 | 命令门禁 |
| probe | 块 | gate 内 | 探针门禁 |
| human | 块 | gate 内 | 人工门禁 |
| all | 块 | gate 内 | AND 逻辑组合 |
| any | 块 | gate 内 | OR 逻辑组合 |
| pre_apply | 修饰 | gate 位置 | 变更前 |
| post_batch | 修饰 | gate 位置 | 每批后 |
| post_apply | 修饰 | gate 位置 | 全部变更后 |
| rest | 修饰 | 批次声明 | 剩余全部 |
| auto | 修饰 | on_failure | 自动触发 |
| manual | 修饰 | on_failure | 人工触发 |
| allow_irreversible | 字段 | workflow 内 | 不可逆动作白名单 |

---

## 附录 B 类型速查

表：LEVEELang 类型速查表

| 类型 | 字面量示例 | 用途 |
| --- | --- | --- |
| string | `"mysql"` | 名称、命令、路径 |
| int | `42` | 数量、退出码 |
| float | `0.01` | 阈值、比率 |
| bool | `true` / `false` | 开关、标记 |
| duration | `5m` / `4h` / `30s` | 超时、grace period |
| percent | `10%` | 批次百分比 |
| label_expr | `role=primary AND az=a` | target query |
| percent_array | `[1, 10, 50, 100]` | batches 划分 |
| approval_level | `standard` / `high` / `emergency` | approval level |
| action_ref | `mysql.pt-online-schema-change` | step action |
| target_ref | `{{target.host}}` | 引用目标属性 |
| input_ref | `{{input.table}}` | 引用输入参数 |
| output_ref | `{{step.migrate.output}}` | 引用前步输出 |

---

## 附录 C 错误码速查

表：错误码速查表

| 错误码 | 严重度 | 含义 |
| --- | --- | --- |
| LE001 | error | 类型不匹配 |
| LE002 | error | 必需字段缺失或命名重复 |
| LE003 | error | 枚举值非法 |
| LE010 | error | 标签表达式语法错误 |
| LE011 | error | 标签键名不符合命名规范 |
| LE012 | error | 资产类型不在白名单 |
| LE020 | error | 时间格式非法或 start ≥ end |
| LE021 | error | timezone 非合法 IANA 名称 |
| LE031 | error | 百分比数组非递增 |
| LE032 | error | 百分比数组末尾非 100 |
| LE033 | warning | 首批超过 5% |
| LE034 | error | strategy 与 steps 类型不匹配 |
| LE041 | error | action 模块或动作不存在 |
| LE042 | error | action 参数不满足契约 |
| LE043 | error | high 级别未排除发起人 |
| LE044 | error | 审批级别非法 |
| LE051 | error | verify / gate 表达式语法错误 |
| LE052 | warning | batches 缺省 post_batch 门禁 |
| LE061 | error | 引用前步输出不存在或类型不匹配 |
| LE071 | error | DAG 存在循环 |
| LE081 | error | rollback action 不在白名单 |
| LE082 | error | 不可逆动作不在 allow_irreversible |
| LE083 | error | 含不可逆动作但审批级别 < high |
| LE091 | error | 缺少 rollback 块 |
| LE092 | error | 缺少 target 块 |
| LE093 | error | 缺少 step 块 |
| LE094 | warning | 缺少 approval 块 |
| LE095 | warning | 缺少 window 块 |
| LE096 | warning | 缺少 batches 块 |