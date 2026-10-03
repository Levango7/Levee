# LEVEE 设计文档审核报告

| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE 设计文档审核报告 |
| 文档类型 | 审核报告 |
| 审核日期 | 2026-08-15 |
| 审核对象 | levee-design.md / leveelang-spec.md / levee-api.md / mvp-tasks.md |
| 审核维度 | 内容完整性 / 内部一致性 / 技术正确性 / 可执行性 / 格式规范 |
| 审核结论 | 有条件通过（1 个 P0 必须修，10 个 P1 建议修，10 个 P2 可选修） |

---

## 第1章 审核概要

### 1.1 审核范围

本次审核覆盖 LEVEE 项目四份核心设计文档，共计 4408 行内容。

表：审核对象基本信息对照表

| 文档 | 行数 | 章节数 | 定位 |
| --- | --- | --- | --- |
| levee-design.md | 996 | 10 章 + 2 附录 | LEVEE 总体设计文档（评审稿 v1.0） |
| leveelang-spec.md | 1892 | 10 章 + 3 附录 | LEVEELang DSL 目标规范（v0.1） |
| levee-api.md | 1015 | 14 章 + 2 附录 | CLI 命令与 REST API 设计（v0.1） |
| mvp-tasks.md | 505 | 7 章 + 2 附录 | MVP 开发任务拆分（v1.0） |

### 1.2 总体评价

四份文档整体质量较高，结构清晰、层次分明、内容详实。设计文档从定位、背景、原则、核心设计、DSL、场景、架构、路线图到风险评审形成完整闭环；DSL 规范语法、类型、字段、校验、错误码定义完备；API 文档命令覆盖变更生命周期全流程；MVP 任务拆分到周次和依赖簇，可执行性强。

主要问题集中在跨文档一致性：部分命令名、参数名、动作模块在四份文档间不统一；DSL 规范的 MVP 阶段限制与 MVP 任务存在一处硬矛盾（emergency 审批级别）；DSL 规范完整示例引用了未定义的动作模块；MVP CLI 命令（T080-T085）与 API 文档命令名系统性偏离。

### 1.3 问题统计

表：问题数量统计对照表

| 严重度 | 数量 | 含义 | 处理要求 |
| --- | --- | --- | --- |
| P0 | 1 | 必须修：文档间硬矛盾 | 评审前必须修复 |
| P1 | 10 | 建议修：不完整、术语不统一、命令覆盖不全 | 评审时确认修复方案 |
| P2 | 10 | 可选修：格式小问题、措辞优化 | 视进度安排 |
| 合计 | 21 | - | - |

---

## 第2章 逐文档审核

### 2.1 levee-design.md 审核结果

表：设计文档章节完整性审核结果

| 章节 | 完整性 | 说明 |
| --- | --- | --- |
| 执行摘要 | 完整 | 定位、价值链、落地节奏清晰 |
| 第1章 定位与边界 | 完整 | 1.1-1.5 覆盖定位、分层、互补、客户、不做什么 |
| 第2章 背景与动机 | 完整 | 现状、工具缺失、事故驱动三节齐全 |
| 第3章 设计原则 | 完整 | 继承优点 + 八条红线 |
| 第4章 核心设计 | 基本完整 | D1-D9 + D12，但跳过 D10、D11 未说明（见问题 1） |
| 第5章 工作流 DSL | 完整 | 设计理念 + 代码示例 |
| 第6章 场景覆盖 | 完整 | 8 场景 + 6 类 20 子类资产 |
| 第7章 组件架构 | 完整 | 6 层架构 + 数据流 |
| 第8章 落地路线图 | 完整 | MVP/V1/V2 三阶段 |
| 第9章 风险与待验证项 | 完整 | K1-K10 |
| 第10章 评审议题 | 完整 | C1-C4 |
| 附录A/B | 完整 | 差异说明 + 23 项移除清单 |

设计文档格式规范：H1-H5 标题层级正确，表格格式规范，代码块标注规范（leveelang/bash），无 emoji。

### 2.2 leveelang-spec.md 审核结果

表：DSL 规范章节完整性审核结果

| 章节 | 完整性 | 说明 |
| --- | --- | --- |
| 第1章 概述 | 完整 | 定位、设计目标、与 Ansible 对比 |
| 第2章 语法结构 | 完整 | 顶层结构 + 关键字清单 + 注释规则 |
| 第3章 类型系统 | 完整 | 基本类型 + 标签表达式 + 百分比数组 + 审批枚举 |
| 第4章 字段定义 | 完整 | target/window/batches/approval/step 字段 |
| 第5章 动作声明 | 完整 | action 引用 + 内置模块 + 输入输出 |
| 第6章 验证门禁声明 | 完整 | 四类门禁 + 逻辑组合 + SLO 时序 |
| 第7章 回滚声明 | 完整 | rollback 字段 + 不可逆操作 |
| 第8章 编译期校验 | 完整 | V1-V23 校验规则 + LE001-LE096 错误码 |
| 第9章 完整示例 | 基本完整 | 4 个示例，但引用未定义动作（见问题 9） |
| 第10章 MVP 阶段限制 | 基本完整 | YAML 子集定义，但与 MVP 任务有矛盾（见问题 7） |
| 附录A/B/C | 完整 | 关键字/类型/错误码速查 |

DSL 规范格式规范：H1-H4 标题层级正确，代码块标注规范（leveelang/yaml），无 emoji。

### 2.3 levee-api.md 审核结果

表：API 文档章节完整性审核结果

| 章节 | 完整性 | 说明 |
| --- | --- | --- |
| 第1章 设计原则 | 完整 | CLI 优先 + 命令风格 + 输出格式 + 退出码 + 配置 |
| 第2章 变更生命周期命令 | 完整 | 2.1-2.10 覆盖创建到归档全流程 |
| 第3章 模板管理命令 | 完整 | list/show/create/validate/update/delete/version |
| 第4章 目标管理命令 | 不完整 | 缺 remove/unmanage 命令（见问题 4） |
| 第5章 审计与合规命令 | 完整 | list/show/export/verify |
| 第6章 漂移检测命令 | 完整 | scan/show |
| 第7章 凭据管理命令 | 完整 | list/add/rotate/revoke/show |
| 第8章 权限管理命令 | 完整 | user/team + 角色权限 |
| 第9章 系统管理命令 | 完整 | version/config/status/doctor |
| 第10章 全局选项 | 完整 | 9 个全局选项 |
| 第11章 退出码规范 | 完整 | 0-8 退出码 |
| 第12章 输出格式 | 完整 | 人类可读 + JSON |
| 第13章 REST API | 基本完整 | 端点清单未覆盖全部命令（见问题 14） |
| 第14章 配置文件 | 完整 | config.yaml + 环境变量 |
| 附录A/B | 完整 | 命令速查 + 设计映射 |

API 文档格式规范：H1-H3 标题层级正确，代码块标注规范（bash/json/yaml），无 emoji。

### 2.4 mvp-tasks.md 审核结果

表：MVP 任务文档章节完整性审核结果

| 章节 | 完整性 | 说明 |
| --- | --- | --- |
| 第1章 MVP 范围与目标 | 完整 | 交付清单 D-01~D-20 + 不做清单 N-01~N-20 + 门禁 G-01~G-07 + 技术选型 |
| 第2章 模块划分 | 完整 | 20 个模块，6 层架构映射 |
| 第3章 任务拆分 | 完整 | T001-T103，9 个周次段 |
| 第4章 任务依赖图 | 完整 | 关键路径 + 依赖簇 + 依赖约束 |
| 第5章 里程碑 | 完整 | M1-M4，硬依赖链 |
| 第6章 风险与应对 | 完整 | M-K1~M-K6 |
| 第7章 团队建议 | 完整 | 规模 + 分工 + 协作 |
| 附录A/B | 完整 | 任务统计 + 周次排期 |

MVP 任务格式规范：H1-H3 标题层级正确，表格格式规范，无 emoji。

每个任务（T001-T103）均有验收标准列，可独立执行。依赖关系无循环（关键路径线性）。里程碑 M1-M4 有硬依赖链，可达。

---

## 第3章 跨文档一致性检查结果

### 3.1 设计文档能力 vs DSL 规范语法

表：设计文档能力与 DSL 规范覆盖对照表

| 设计文档能力 | DSL 规范对应 | 一致性 |
| --- | --- | --- |
| D2 LEVEELang 类型化 DSL | 第2-8 章完整定义 | 一致 |
| D4.4.3.1 三级审批（标准/高危/紧急） | 3.4 审批枚举 standard/high/emergency | 一致（中英文对应） |
| D4.4.5.1 四类门禁 | 6.1 四类门禁原语 | 一致 |
| D4.4.5.2 SLO 三段时序 | 6.3 SLO 门禁三段时序 | 一致 |
| D4.4.6 回滚协议 | 7.1 rollback 字段 | 一致 |
| D4.4.6.1 不可逆操作 | 7.2 不可逆操作 | 一致 |
| D5.2 DAG 编排 | 4.5 step 块 + depends_on | 一致 |
| D5.3 原生原语 | 2.2 关键字清单 | 一致 |
| D5.4 互斥锁 TTL | MVP T040 | 一致 |

### 3.2 设计文档命令 vs API 文档覆盖

表：设计文档命令与 API 文档覆盖对照表

| 设计文档命令 | API 文档覆盖 | 一致性 |
| --- | --- | --- |
| levee new / clone / plan / apply | 2.1 / 2.2 / 2.5 | 一致 |
| levee approve / reject / delegate | 2.4 | 一致 |
| levee pause / resume / pause-all / resume-all | 2.6 | 一致 |
| levee cancel / retry | 2.7 | 一致 |
| levee retry --target（单台重跑） | 2.7 retry-host | 不一致（见问题 3） |
| levee rollback | 2.8 | 一致 |
| levee logs / trace / diff / archive / link | 2.9 / 2.10 | 一致 |
| levee drift report | 6.1 drift scan | 一致（report 细化为 scan/show） |
| levee schedule（D6.1 计划触发） | 缺失 | 不一致（见问题 5） |
| remove / unmanage（D3.2） | 缺失 | 不一致（见问题 4） |
| 模板实例化 --set | API 用 --params | 不一致（见问题 2） |

### 3.3 MVP 任务 vs 设计文档 MVP 交付项

表：MVP 任务覆盖设计文档交付项对照表

| 设计文档第8章 MVP 交付物 | MVP 任务覆盖 | 一致性 |
| --- | --- | --- |
| 单二进制零依赖 | D-18 / T095 / G-02 | 一致 |
| SSH / WinRM 通道 | D-02 / T011-T014 | 一致 |
| LEVEELang 基础语法 | D-09 / D-10 / T020-T021 | 一致 |
| plan/apply/verify/rollback 闭环 | D-03~D-05 / T022-T042 | 一致 |
| 命令门禁 + SLO 门禁 | D-04 / T028-T029 | 一致 |
| 审计 trace + 哈希链 | D-07 / T043-T046 | 一致 |
| 兼容层 | D-08 / T055-T057 | 一致 |
| 通知 webhook | D-20 / T051-T053 | 一致 |
| 回滚演练 | D-16 / T087-T088 / G-04 | 一致 |
| 100 台批量变更 | D-19 / T089 / G-05 | 一致 |

### 3.4 术语统一性检查

表：术语统一性检查结果

| 术语 | 设计文档 | DSL 规范 | API 文档 | MVP 任务 | 一致性 |
| --- | --- | --- | --- | --- | --- |
| LEVEELang | LEVEELang | LEVEELang | LEVEELang | LEVEELang | 一致 |
| 审批级别 | 标准/高危/紧急 | standard/high/emergency | 标准/高危/紧急 | 标准/高危/紧急 | 一致（中英文对应） |
| 技术栈 | Go + SQLite | - | - | Go 1.22+ + SQLite + cobra | 一致 |
| JSON 输出选项 | -o json | - | --json | -o json | 不一致（见问题 12） |
| 日志目标参数 | --target | - | --host | --target | 不一致（见问题 13） |
| 凭据命令 | - | - | secret | credential | 不一致（见问题 8） |

### 3.5 审批级别命名统一性

审批级别在四份文档中命名统一：DSL 规范用英文枚举 standard/high/emergency（作为语法关键字），其余文档用中文标准/高危/紧急（作为描述用语），中英文对应关系清晰，无混用。

---

## 第4章 问题汇总（按严重度分级）

### 4.1 P0 必须修（1 个）

表：P0 级别问题清单

| 编号 | 所在文档 | 位置 | 问题描述 | 建议修复 |
| --- | --- | --- | --- | --- |
| P0-01 | leveelang-spec.md / mvp-tasks.md | DSL 规范 10.3 vs MVP 任务 T032 | emergency 审批级别矛盾：DSL 规范 10.3 表明确写"approval.level MVP 支持 standard/high"，MVP 不支持列表含"emergency 审批级别"；但 MVP 任务 T032 验收标准要求"标准/高危/紧急三级，触发条件 + 审批人要求 + 超时配置"。两份文档对 MVP 是否实现 emergency 审批直接矛盾，将导致实现时无法确定范围 | 统一定论：若 MVP 实现三级，则更新 DSL 规范 10.3 将 emergency 移出不支持列表；若 MVP 仅两级，则更新 MVP 任务 T032 改为"标准/高危两级"。建议 MVP 实现三级（emergency 是紧急故障恢复关键能力，MVP 应覆盖） |

### 4.2 P1 建议修（10 个）

表：P1 级别问题清单

| 编号 | 所在文档 | 位置 | 问题描述 | 建议修复 |
| --- | --- | --- | --- | --- |
| P1-01 | levee-design.md | 第4章标题 | 第4章标题"核心设计 D1-D9 + D12"跳过 D10、D11，全文无任何说明为何缺这两个编号。附录B 精简移除清单 23 项也未提及 D10/D11。读者会疑惑编号是否遗漏 | 在第4章标题下或附录补充说明：D10、D11 在原 OpsChain 设计中存在，LEVEE 精简移除或合并到其他 D 项，列出具体去向 |
| P1-02 | levee-design.md / levee-api.md / mvp-tasks.md | 设计文档 4.2.3 vs API 2.1 vs MVP T062 | 模板实例化参数选项不一致：设计文档 4.2.3 用 `--set table=orders --set batch=10%`，API 文档 2.1 用 `--params table=orders,batch_pct=10%`，MVP 任务 T062 用 `--set k=v`。三份文档两种写法 | 统一为 `--params key=val,...`（API 文档的逗号分隔 map 风格更符合 CLI 惯例），同步更新设计文档 4.2.3 和 MVP T062 |
| P1-03 | levee-design.md / levee-api.md | 设计文档 4.4.4.5 / 4.4.8 vs API 2.7 | 单台重跑命令不一致：设计文档 4.4.4.5 和 4.4.8 操作全集用 `levee retry <run-id> --target <host>`，API 文档 2.7 用独立命令 `levee retry-host <run-id> <host>`。参数式 vs 独立命令式 | 统一为 `levee retry-host <run-id> <host>`（独立命令语义更清晰，与 retry 区分明确），同步更新设计文档 4.4.4.5 和 4.4.8 |
| P1-04 | levee-design.md / levee-api.md | 设计文档 4.3.2 vs API 第4章 | remove / unmanage 命令缺失：设计文档 4.3.2 定义了 remove（移除并清理）和 unmanage（仅停止管理）两个显式动作，需走审批闭环；API 文档第4章目标管理命令只有 list/check/import，无 remove/unmanage 对应命令 | 在 API 文档第4章补充 `levee target remove <host>` 和 `levee target unmanage <host>` 命令，注明需审批权限 |
| P1-05 | mvp-tasks.md | 附录A 任务统计 | MVP 估时超过可用人天：总估时 196 人天，3 人团队 12 周（每周 5 工作日）可用 180 人天，缺口 -8%。文档已识别此问题并提出应对（并行调度 + 任务压缩或延长周期），但仍是排期风险 | 建议明确选定一种应对方案：方案 A 按文档建议 2 人 12 周 + 1 人辅助 4 周 = 140 人天，裁剪 56 人天非关键任务；方案 B 延长周期至 4 个月（240 人天可用，余量充足）。在附录A 写定选定方案 |
| P1-06 | mvp-tasks.md / levee-api.md | MVP T080-T085 vs API 第3-9章 | MVP CLI 命令名与 API 文档系统性不一致：T080 template add/remove vs API create/delete；T081 target add/remove/precheck vs API check/import；T082 audit search vs API list/show；T083 credential vs API secret；T084 permission vs API user/team；T085 system init/status/config vs API version/status/config/doctor。6 组命令名偏离 | 以 API 文档为准（API 文档命令名更规范），更新 MVP 任务 T080-T085 验收标准中的命令名：template create/delete、target check/import、audit list/show、secret、user/team、version/status/config/doctor |
| P1-07 | leveelang-spec.md | 第9章 完整示例 | 示例引用未定义的动作模块：9.1 用 `patch.scan`（5.2 内置模块无 patch，V1 扩展也无）；9.3 用 `net.config-backup`/`net.config-commit`/`net.config-restore`（5.2 V1 扩展 net 模块只有 acl-update/route-update/bgp-neighbor）；9.4 用 `svc.reload`（5.2 内置 svc 模块只有 start/stop/restart/enable/disable，无 reload） | 在 5.2 内置模块或 V1 扩展模块表补全这些动作：patch 模块新增 scan 动作；net 模块新增 config-backup/config-commit/config-restore；svc 模块新增 reload。或将示例改为仅引用已定义动作 |
| P1-08 | leveelang-spec.md / mvp-tasks.md | DSL 规范 5.2 vs MVP 任务 | MVP 内置动作模块实现任务缺失：DSL 规范 5.2 定义 MVP 内置 5 个模块（shell/file/pkg/svc/user，共 17 个动作），但 MVP 任务只有 T017 shell 直译模块，file/pkg/svc/user 无专门实现任务（兼容层 T056 部分覆盖 file 的 copy/template，pkg/svc/user 仍缺） | 在 MVP 任务第3.2节（通道与执行）或第3.3节（工作流核心）补充 file/pkg/svc/user 模块实现任务，估时约 4-6 PD，或在 T017 验收标准中明确包含 5 个模块 |
| P1-09 | levee-api.md / mvp-tasks.md | API 第7章 vs MVP N-11/T047 | 凭据管理阶段定位不一致：API 文档第7章描述"通过凭据代理（Vault/CyberArk/自研 4A）按需获取，用完即弃"，但 MVP N-11 明确"凭据代理延后 V1，MVP 本地加密存储"，T047 实现本地 AES-GCM 加密。API 文档 v0.1 若对应 MVP 阶段，则凭据代理描述与 MVP 本地加密矛盾 | 在 API 文档第7章开头注明阶段："MVP 阶段为本地加密存储（见 mvp-tasks T047），凭据代理（Vault/CyberArk）在 V1 引入"，或拆分 MVP/V1 两节描述 |
| P1-10 | levee-design.md | 4.4.8 操作全集 | 操作全集标题与内容不符：4.4.8 标题"操作全集"，但表格只列 6 个操作（取消/重试/克隆/模板/关联/diff），未包含 new/plan/apply/approve/reject/pause/resume/rollback/archive/logs/trace 等核心操作。读者期望"全集"包含全部操作 | 将标题改为"补充操作"或"辅助操作"（这些是前面章节未详述的操作），或补全为真正的操作全集（含全部生命周期操作） |

### 4.3 P2 可选修（10 个）

表：P2 级别问题清单

| 编号 | 所在文档 | 位置 | 问题描述 | 建议修复 |
| --- | --- | --- | --- | --- |
| P2-01 | levee-design.md / levee-api.md | 4.6.1 vs API | schedule 命令在 API 文档缺失：设计文档 4.6.1 定义 `levee schedule` 按 cron 定时触发，API 文档无此命令。MVP N-20 已明确延后 V1，可接受，但 API 文档应至少在附录提及 | 在 API 文档附录A 命令速查表补充 `levee schedule`（标注 V1），或在附录B 映射关系注明 |
| P2-02 | levee-design.md / levee-api.md / mvp-tasks.md | 4.2.5 vs API 1.1 vs MVP T007 | JSON 输出选项不一致：设计文档 4.2.5 和 MVP T007 用 `-o json`，API 文档 1.1 和全局选项表用 `--json`。短选项 vs 长选项 | 统一为 `--json`（API 全局选项表已定义），`-o` 保留为输出文件路径短选项。更新设计文档 4.2.5 和 MVP T007 |
| P2-03 | levee-design.md | 4.4.3.1 审批分级表 | 审批级别未注明英文枚举名：设计文档 4.4.3.1 用中文"标准/高危/紧急"，DSL 规范用英文 standard/high/emergency，设计文档未注明对应关系 | 在 4.4.3.1 表格增加一列"枚举值"标注 standard/high/emergency，或在表后补充对应说明 |
| P2-04 | leveelang-spec.md | 2.2 关键字清单 | 关键字清单未包含逻辑组合块：2.2 块关键字清单只有 slo/cmd/probe/human，未列 6.2.2 引入的 all/any 块和 allow_irreversible 字段（附录A 速查表有补充） | 在 2.2 块关键字表补充 all/any，在字段关键字表补充 allow_irreversible |
| P2-05 | levee-api.md | 13.2 REST API 端点清单 | REST API 端点未覆盖全部 CLI 命令：13.2 端点清单缺 secret/credential、user/team/permission、config 等资源端点（CLI 第7-9章有对应命令） | 在 13.2 端点表补充 `/api/v1/secrets`、`/api/v1/users`、`/api/v1/teams`、`/api/v1/config` 等端点，或在表后注明"仅列核心端点，管理类端点见对应 CLI 章节" |
| P2-06 | levee-design.md / levee-api.md / mvp-tasks.md | 4.4.4.6 vs API 2.9 vs MVP T075 | 日志目标参数名不一致：设计文档 4.4.4.6 和 MVP T075 用 `--target <host>`，API 文档 2.9 用 `--host <host>` | 统一为 `--host`（logs 上下文中 host 更精确，--target 在其他命令表示标签表达式），更新设计文档 4.4.4.6 和 MVP T075 |
| P2-07 | mvp-tasks.md | T058 | `levee run --shell` 命令 API 文档未定义：MVP T058 定义 `levee run --shell "cmd"` 单命令直跑，API 文档无 `levee run` 命令 | 在 API 文档第2章或第9章补充 `levee run --shell <cmd>` 命令，或在 T058 注明为内部调试命令不暴露给用户 |
| P2-08 | leveelang-spec.md | 10.3 MVP YAML 子集 | target.type mysql 与动作模块矛盾：10.3 表说 MVP target.type 支持 host/mysql，但 5.2 内置模块无 mysql（mysql 是 V1 扩展），MVP 阶段 mysql target 无可用动作模块 | 在 10.3 表 target.type 限制列注明"mysql target 在 MVP 阶段无内置动作模块，仅 host 可执行变更；mysql 完整支持在 V1"，或移除 mysql 仅留 host |
| P2-09 | levee-design.md | 5.2 代码示例 | 示例使用 V1+ 动作模块未注明：设计文档 5.2 数据库 schema 变更示例用 `mysql.pt-online-schema-change`（V1 扩展模块），未注明该动作在 MVP 阶段不可用 | 在 5.2 代码示例标题或注释注明"本示例为目标态（V1+），MVP 阶段用 YAML 子集 + 兼容层表达" |
| P2-10 | mvp-tasks.md | T100 | 任务命名易混淆：T100"10 分钟测试找新手"与 G-01"10 分钟测试门禁"都含"10 分钟测试"，但含义不同（G-01 指 `levee test` 10 分钟完成，T100 指新手 10 分钟上手） | 将 T100 改名为"新手上手验证"或"10 分钟上手门禁"，与 G-01"10 分钟测试门禁"区分 |

---

## 第5章 审核结论

### 5.1 结论判定

表：审核结论判定依据

| 判定项 | 结果 | 说明 |
| --- | --- | --- |
| 关键章节完整性 | 通过 | 四份文档章节完整，无关键章节遗漏 |
| 文档间硬矛盾 | 不通过 | 1 个 P0：emergency 审批级别在 DSL 规范与 MVP 任务间直接矛盾 |
| 技术正确性 | 基本通过 | DSL 语法自洽、CLI 闭环完整、任务依赖无环；但 DSL 示例引用未定义动作（P1-07） |
| 可执行性 | 通过 | MVP 任务均有验收标准、可独立执行、依赖无环、里程碑可达 |
| 格式规范 | 通过 | 四份文档标题层级、表格、代码块、命名均规范，无 emoji |
| 术语统一性 | 基本通过 | LEVEELang、审批级别等核心术语统一；部分命令名/参数名不统一（P1-02/03/06） |

### 5.2 最终结论

**有条件通过。**

四份文档整体设计质量高，架构清晰，闭环完整，可执行性强。但存在 1 个 P0 级硬矛盾必须修复后方可进入评审：

1. P0-01：emergency 审批级别在 DSL 规范 MVP 限制与 MVP 任务 T032 间直接矛盾，必须统一定论。

同时建议在评审前修复 10 个 P1 级问题，其中优先级最高的三项：

1. P1-06：MVP CLI 命令名（T080-T085）与 API 文档系统性不一致，会导致 MVP 实现与 API 设计脱节。
2. P1-07：DSL 规范完整示例引用未定义的动作模块，示例代码无法对应规范定义。
3. P1-02：模板实例化参数选项 `--set` vs `--params` 三份文档不统一，影响 CLI 实现一致性。

P2 级问题可在评审后迭代修复，不影响评审通过。

### 5.3 修复优先级建议

表：修复优先级建议

| 优先级 | 问题编号 | 修复时机 | 预计工作量 |
| --- | --- | --- | --- |
| 必须立即修 | P0-01 | 评审前 | 0.5 小时（统一 emergency 定论 + 更新一处文档） |
| 评审前修 | P1-06, P1-07, P1-02 | 评审前 | 2 小时（命令名对齐 + 动作模块补全 + 参数选项统一） |
| 评审时确认 | P1-01, P1-03, P1-04, P1-08, P1-09, P1-10, P1-05 | 评审会议 | 3 小时（补充说明 + 命令补充 + 模块任务补充 + 阶段标注 + 估时方案选定） |
| 评审后迭代 | P2-01 至 P2-10 | 文档迭代 | 2 小时（格式微调 + 措辞优化） |

---

## 第6章 复核追踪（2026-10-03）

2026-09 以来代码与文档经历了大量修复（词表单一来源 `internal/runstatus`、审批绑定 `plan_hash`、错误码目录 `internal/errors`、`internal/docgen` 生成规范表格、变更窗口 LE020/LE021、LE097 等）。本章对本报告第 4 章的 21 个问题逐条复核，判定**已修复/已过时**或**仍有效**，全部给出当前证据。原始判定保留在第 4 章不动；本章是追加的追踪，不是改写。

### 6.1 逐条复核结果

| 编号 | 严重度 | 一句话问题 | 复核判定 | 证据（当前文件:行号） |
| --- | --- | --- | --- | --- |
| P0-01 | P0 | emergency 审批级别：DSL 规范 10.3 与 MVP T032 硬矛盾 | **已修复/已过时** | leveelang-spec.md:1917（approval.level 支持列表含 emergency）、:1932-1942（不支持清单已无 emergency）；代码 internal/dsl/ast.go:70、internal/approval/levels.go:41、internal/dsl/validator.go:8 |
| P1-01 | P1 | 第4章跳过 D10/D11 无说明 | **已修复/已过时** | levee-design.md:192（编号说明：D10 自愈/D11 多集群联邦移除）、:1001-1002（附录B 第 24/25 项） |
| P1-02 | P1 | 模板实例化 `--set` vs `--params` 不一致 | **已修复/已过时** | levee-design.md:277、levee-api.md:58/66、mvp-tasks.md:269/280 统一为 `--params key=val,...`；代码 cmd/levee/cmd_new.go:35 |
| P1-03 | P1 | 单台重跑 `retry --target` vs `retry-host` 不一致 | **已修复/已过时** | levee-design.md:417/506、levee-api.md:236/1087 统一为 `retry-host`；代码 cmd/levee/cmd_retry.go |
| P1-04 | P1 | API 第4章缺 remove/unmanage 命令 | **已修复/已过时**（文档已补；代码侧现状：cmd_target.go 子命令为 list/import/freeze/unfreeze/retire/history/check，无 remove/unmanage，与 T081 "remove 延后 V1" 一致） | levee-api.md:429-449（4.4 target remove）、:451-470（4.5 target unmanage）；cmd/levee/cmd_target.go:56-140 |
| P1-05 | P1 | MVP 估时 196 人天超可用 180 人天 | **已修复/已过时**（已选方案 B：16 周/240 人天；残留：附录B 周次表仍只列 W1-W12 未反映 16 周） | mvp-tasks.md 附录A.1 估时方案对照表及"方案选定：采用方案 B" |
| P1-06 | P1 | MVP T080-T085 命令名与 API 文档系统性偏离（6 组） | **已修复/已过时** | mvp-tasks.md:296-301（全部标注"对齐 API"）；代码 cmd_template.go:52-95、cmd_secret.go:51-108、cmd_user/cmd_team.go、cmd_system.go:49-130、cmd_audit.go、cmd_target.go |
| P1-07 | P1 | DSL 第9章示例引用未定义动作模块 | **已修复/已过时** | leveelang-spec.md:787（svc.reload 入 MVP 表）、:807（patch.scan）、:811-813（net.config-backup/commit/restore 入 V1 扩展表） |
| P1-08 | P1 | MVP 缺 file/pkg/svc/user 模块实现任务 | **已修复/已过时** | mvp-tasks.md:185-188（T017.1-T017.4）；代码 internal/executor/modules/{file,pkg,svc,user} 均存在 |
| P1-09 | P1 | API 第7章凭据代理描述与 MVP 本地加密矛盾 | **已修复/已过时** | levee-api.md:553（阶段说明：MVP 本地 AES-GCM/T047，凭据代理 V1 引入，本章覆盖两模式） |
| P1-10 | P1 | design 4.4.8 标题"操作全集"与内容不符 | **已修复/已过时** | levee-design.md:498（标题已改"补充操作"）、:500-511 |
| P2-01 | P2 | schedule 命令 API 文档缺失 | **仍有效** | levee-api.md 全文无 "schedule"；design:557 仍定义；代码仅 `levee drift schedule`（cmd_drift.go:420），变更级 cron 触发命令不存在（与变更日历的接线待定夺同源） |
| P2-02 | P2 | `-o json` vs `--json` 不一致 | **仍有效（本复核当场修掉）** | 代码以 `--json` 为准（root.go:66，短 `-j`）；levee-design.md 与 mvp-tasks.md 的 `-o json` 已随本次复核改为 `--json` |
| P2-03 | P2 | design 4.4.3.1 审批分级表未注明英文枚举名 | **仍有效（本复核当场修掉）** | 表已补枚举列（standard/high/emergency），并顺带修正"紧急超时 15min"——代码实际是 30min（见 CHANGELOG「规范三张词表与代码不一致」条目的勘误）；新生成表见 internal/docgen 的 docs 门禁 |
| P2-04 | P2 | spec 2.2 关键字清单缺 all/any 块与 allow_irreversible | **仍有效** | leveelang-spec.md:219-227（块关键字仍仅 slo/cmd/probe/human；6.2.2 :981-1005 用 any/all）；:228-252（字段表无 allow_irreversible，但 :1235/:1276/:2021 在用） |
| P2-05 | P2 | 13.2 REST 端点缺 secret/user/team 等资源端点 | **仍有效（部分缓解）** | 已补 /system/config、/audit/*、/targets/*；secrets/users/teams 端点仍缺，表后无"仅列核心端点"注记（levee-api.md:799-843） |
| P2-06 | P2 | 日志目标参数 `--target` vs `--host` 不一致 | **仍有效（本复核当场修掉）** | 代码实际用 `--target`（cmd/levee/cmd_logs.go:38）——API 文档 2.9 的 `--host` 是 stale，已随本次复核改为 `--target`（与代码及 design/mvp 一致） |
| P2-07 | P2 | `levee run --shell` API 文档未定义 | **仍有效** | levee-api.md 无 "levee run"；internal/executor/shell_run.go 实现了 ShellRunner 但命令未注册（NewShellRunner 无生产调用者）——是处置决策不是文档遗漏 |
| P2-08 | P2 | spec 10.3 target.type 含 mysql 但 MVP 无 mysql 模块 | **仍有效（部分缓解）** | internal/executor/modules/mysql 已实现 query/pt_osc/replica_switch；leveelang-spec.md:1909 限制列仍无说明（:769/:803 mysql 仍归 V1 扩展） |
| P2-09 | P2 | design 5.2 示例用 V1+ 动作模块未注明 | **仍有效** | levee-design.md:753/:767 用 mysql.pt-online-schema-change，前后无 V1+/MVP 注记 |
| P2-10 | P2 | T100 与 G-01 均含"10 分钟测试"命名易混淆 | **仍有效（本复核当场修掉）** | mvp-tasks.md T100 已改名"10 分钟上手门禁"，与 G-01"10 分钟测试门禁"区分 |

### 6.2 总体结论

21 项中 **11 项已修复/已过时**（P0-01 与全部 10 个 P1），证据链完整；**10 项仍有效**（全部 P2，无一无法核实），其中 4 项（P2-02/03/06/10）已随本次复核当场修掉，剩余 6 项（P2-01/04/05/07/08/09）为文档迭代或处置决策项。原报告"有条件通过"的 P0 前提已消除。

### 6.3 剩余 6 项的处置建议

| 编号 | 处置 | 归属 |
| --- | --- | --- |
| P2-01（schedule 命令缺失） | 随组织级变更日历的接线一起定案（roadmap「日历侧一半待接」行） | roadmap |
| P2-04（spec 关键字清单缺漏） | 文档迭代：2.2 块关键字表补 all/any、字段表补 allow_irreversible | docs |
| P2-05（REST 端点覆盖） | 表后补"仅列核心端点"注记；secrets/users/teams 端点随对应管理面定案 | docs / roadmap |
| P2-07（levee run --shell 未注册） | 处置决策：注册为内部调试命令或退役 ShellRunner | roadmap |
| P2-08（spec 10.3 mysql 限制无说明） | 文档迭代：限制列注明 mysql 模块现状 | docs |
| P2-09（design 5.2 示例无注记） | 文档迭代：示例注明目标态归属 | docs |

### 6.4 剩余项收口（2026-10-03 同日）

上表 6 项**已全部落地**：

- **P2-01**：`levee-api.md` 附录 A 补 `levee schedule`（cron 定时触发变更）行并标注 **V1 尚未实现**（当前定时能力仅为 `levee drift schedule` 的漂移巡检）；组织级变更日历接线仍由 roadmap「日历侧一半待接」行跟踪。
- **P2-04**：`leveelang-spec.md` §2.2 块关键字表补 `any`（OR 组合）/ `all`（AND 组合，最多 2 层），字段关键字表补 `allow_irreversible`（workflow 级白名单，未列入则 LE082 拒绝）。
- **P2-05**：§13.2 端点表后补「仅列核心端点」注记（管理面资源走第 7–9 章 CLI）。
- **P2-07**：`levee run --shell <cmd>` **注册为显式标注的本地调试命令**（`cmd/levee/cmd_run.go`：平台 shell、超时、结构化输出、子进程退出码经 `[exit=N]` 透传、超时代码 8），帮助文本与 `levee-api.md` 第 9 章都明说「不经 plan/审批/审计链」；`mvp-tasks.md` T058 同步注记。
- **P2-08**：§10.3 的 `target.type` 限制列注明 mysql 动作模块（`mysql.query` / `pt_osc` / `replica_switch`）已实现并通过 CI。
- **P2-09**：`levee-design.md` §5.2 示例加「目标态示例（V1+）」注记。

至此 21 项全部闭环（11 项复核确认已解决 + 10 项 P2 已处置/修复）。

---

（审核报告结束）