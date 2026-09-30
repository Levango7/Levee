# LEVEE 产品定位
| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE 产品定位 |
| 文档类型 | 产品文档 |
| 版本 | v1.1（v1.0 定为 workflow 引擎；v1.1 在 2026-09-25 重写为系统） |
| 适用范围 | README、所有面向用户的文档、UI 文案的统一参考 |
| 关联文档 | [`docs/ui-blueprint.md`](ui-blueprint.md)、[`docs/product-roadmap.md`](product-roadmap.md)、`docs/leveelang-spec.md` 第 0 章 |
| 上游事件 | 2026-09-25 体量测量与产品结构检讨（见 `notes/levee-review-2026-09-17`，`2c97899`） |

---

## 1. 一句话定位

**LEVEE 是一套高危变更治理系统**：把一个「改一台主机/数据库/网络设备/中间件」从手工命令升级为
**计划 → 审批 → 分批执行 → 验证门禁 → 自动回滚 → 审计留痕** 的受治理闭环，默认无代理、CLI 优先。

## 2. 它是什么 / 不是什么

| | |
| --- | --- |
| **是** | 面向生产变更的**治理控制平面**：状态机 + 版本绑定的审批 + 执行证据账本 + 补偿幂等性 + 不可变审计链 + 集群故障接管 |
| **不是** | generic workflow / DAG 调度器。workflow 编排只是实现里的一个子系统（`internal/` 里与编排直接相关的包约三成） |
| **不是** | CMDB / 日志平台 / 告警引擎 / Agent 常驻纳管平台。这些归 OpsMesh；LEVEE 只在 OpsMesh 之上承担**高危变更**这一件事 |
| **不是** | ArgoCD / Flux 的替代。集群内声明式交付归它们；集群外命令式高危变更归 LEVEE |

## 3. 为什么它是「系统」而不是「workflow 引擎」

| 能力 | 属于哪一层 | 为什么 workflow 引擎不拥有它 |
| --- | --- | --- |
| 身份/授权（auth / permission ABAC / credential 加密 / tenant） | 平台 | 审批门禁本身就是治理目标，必须先定义"谁能批" |
| 生命周期与持久化（state / backup / calendar / scheduler / cluster） | 系统 | 状态机跨节点迁移、WORM 审计链、故障接管 |
| 运维面（metrics / tracing / audit / notify / push / chatops） | 系统 | 高危变更的第一步是"让谁看见" |
| 智能层（diagnosis / recommend / drift / conversation） | 产品 | 诊断→建议→生成草稿→门禁执行 是真正的人类闭环 |
| 引擎（dsl / plan / engine / wiring / executor / rollback / dispatch / batch） | 子系统 | 只是在治理链里扮演"执行"那一段 |

结论：**workflow 是 LEVEE 的内部实现，Change 才是它的公开主语**。
proto 侧已为这个语义提供了依据——40 个 RPC 里 23 个挂在 `ChangeService`。

## 4. 角色与核心场景

| 角色 | 进入界面 | 核心场景 |
| --- | --- | --- |
| **变更发起人** | CLI、`/changes`、`/conversation` | 从模板/工作流发起变更，跟踪到归档 |
| **审批人** | `/approval`、ChatOps、移动深链 | 只看不编辑；决定批准或否决 |
| **值班运维** | `/monitor`、`/changes/:id` | 接管 partial rollback / rollback_incomplete 的补救 |
| **平台管理员** | `/targets`、`/system` | 目标机维护、权限、集群健康 |
| **审计/合规** | `/audit` | 证据链校验、报告导出、不可抵赖性出示 |
| **AI 用户** | `/conversation`（或 ChatOps） | 告警→诊断→建议→草稿→审批→执行的闭环 |

## 5. 地盘边界

| 范围 | 归属 |
| --- | --- |
| 高危变更（影响生产、不可逆、要审批留痕） | LEVEE |
| 日常巡检/作业流/告警规则/CMDB/日志采集 | OpsMesh |
| K8s 集群内的声明式 manifest 交付 | ArgoCD / Flux |
| 集群外命令式动作（动作级、人工审批、显式回滚） | LEVEE |

## 6. 本文档与 UI/代码的关系

- **UI 蓝图**见 [`ui-blueprint.md`](ui-blueprint.md)——导航、视图线框、关键交互。
- **路线图**见 [`product-roadmap.md`](product-roadmap.md)——代码层/可视化层/交互层的下一步清单与依赖。
- **术语**见 `leveelang-spec.md` 第 0 章（权威），并由 `internal/runstatus` + `internal/docgen` 在 CI 中钉住。
- **状态词表**归 `internal/runstatus`；UI 与 REST 过滤一律以它为准。
