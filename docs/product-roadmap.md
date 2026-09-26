# LEVEE 产品路线图（2026-09-25 起）
| 元信息项 | 内容 |
| --- | --- |
| 文档标题 | LEVEE 产品路线图 |
| 文档类型 | 产品路线图 |
| 版本 | v1.0 |
| 日期 | 2026-09-25 |
| 上游文档 | [`product-positioning.md`](product-positioning.md)、[`ui-blueprint.md`](ui-blueprint.md) |
| 范围 | 现在起，按"系统而非 workflow"的定位推进的产品工作清单 |

---

## 怎么读这份文档

四层分法：

- **代码层**：引擎/治理链自身的正确性和能力。这是所有 UI 的地基，除非说明，否则先修这里。
- **可视化层**：把已有能力用 UI 暴露给不同角色。任何一层都必须先被**代码层**证实可用，`ui-blueprint.md` 中的界面不能编造 API。
- **交互层**：操作路径的闭环，包括向导、空状态、错误恢复。
- **运营层**：面向未开发者（controller、值班），不是写给开源用户。

## 代码层（平台正确性）

| 优先级 | 项 | 理由 | 依赖 |
| --- | --- | --- | --- |
| P0 | ~~`workflow.Rollback`（工作流级回滚声明）是否落 `PlanStep`~~ **已定案（2026-09-26）** | **不落 `PlanStep`**：补偿账本按 `(host, 前向步骤)` 归属，工作流级补偿无法归因（全局 undo 复制到每步会重复撤销或整份漏掉；全局 snapshot 逐 step 采集会拿到中间态）。改为分层治理：workflow 级只承载运行态策略（`on_failure` 已接入执行器 / `verify_after` 待装配），补偿内容由 **LE097** 在 `dsl.Validator` 与 plan 生成器两处 fail-closed 拒绝 | 无（已实现） |
| P1 | 回滚后验证的生产装配：把 `rollback.PostRollbackVerifier` 注入 `wiring` 的闭包执行器（`NewClosureRunner(..., nil)` 目前恒为 nil） | 验证器与 `Grader`（T037）都已实现，只差装配——否则 `verify_after` 仍是"声明了但没人做" | 无 |
| P2 | run 级快照原语（显式 `scope: run` 或独立顶层 `snapshot {}`）：首批发前**一次性**采集、回滚时恢复 | 方案 B 明确拒绝把 workflow 级 `snapshot_paths` 投影到每个 step（语义错误）；若确实需要"整次运行基线"，应作为独立原语设计，而不是伪装成 step 补偿 | 无 |
| P0 | ~~完成 D-3 遗漏的链接：CLI 终态矩阵 / proto 注释 / metrics 标签引用 `runstatus`~~ **已定案并落地（2026-09-26）** | 三处副本全部换成引用：CLI 的 `isRollbackableStatus` / `isTerminalStatus` 改调 `runstatus`（前者此前比服务端门禁多放行 `running`，后者漏掉三个终态），帮助文本与报错由 `JoinRollbackAdmitted()` 拼装；`metrics` 标签常量改为别名（只保留非 run 状态的 `created` / `succeeded`）；proto 注释改指 `runstatus` 并由 `TestProtoStatusCommentMatchesGo` 钉定。守门测试去掉 `metrics` 豁免、扫描范围扩到 `cmd/` | 无（已实现） |
| P0 | ~~`recommend` → `Change` 的正式桥~~ **已定案并落地（2026-09-26）** | `internal/conversation/change_bridge.go` 在确认后把 `WorkflowDraft` 交给既有 `ChangeService.CreateChange`（`levee serve` 装配），产出 `draft` 变更进入标准治理链；草案先过与 `levee compile` 严格模式相同的解析 + 校验两道门（含 LE097），fail-closed：不通过则一条变更记录都不建，会话留在 `reviewing`。**桥不执行工作流、不跳过审批**；裸 `levee converse` 未装配桥，保持"尚未提交"的诚实文案 | 无（已实现） |
| P1 | `idempotent: true` 的存量 Lint：检出未声明幂等的 undo 步骤并引导作者补齐 | 方案 C 留下的最后一步：从被动门禁变主动信息 | D-3 |
| P2 | `internal/docgen` 的多语言实现（把 `web/src/types/levee.ts` 那张表也生成为 TS） | 现在 `TestWebStatusMirrorMatchesGo` 还是手工比对——它本身也不过是"另一份手写词表"在 Go 里的验证镜 | D-3 |

## 可视化层（UI / Web / ChatOps）

| 优先级 | 项 | 产出 | 依赖 |
| --- | --- | --- | --- |
| P0 | **变更详情页**（`/changes/:id`）：生命周期时间线 + 批次/目标矩阵 + 回滚证据 + 一键补救 | 对一个变更的所有关键指纹做卡片化拼装：计划哈希+审批+门禁+证据，且把"补救"做成显式按钮 | 代码层 P0（Rollback 补偿幂等、D-3） |
| P0 | **审批中心卡片化**：把准入门控的中间态（批准人、是否过 threshold、历史 plan 版本）清楚地并排显示 | 避免"看上去能批，实际被拒"的 entrap | 代码层状态机一致性（`change_service.go`） |
| P0 | **/monitor 的三视图联动**：批次进度条 + 目标地图 + 实时日志流（已有 `WatchChange` / `StreamLogs`） | 现有接口已经能给出，需要补 UI | 无 |
| P1 | **审计视图**：时间线 + hash 链校验按钮 + 证据片段卡 | 合规性证据的现场可视化 | P0 详情页（复用时间线组件） |
| P1 | **集群拓扑** + 接管事件时间线 | cluster 现有接口已经足够（`GetStatus` 等） | 无 |
| P1 | **ChatOps 卡片**：同 web 审批卡片的迷你版（文字 + 两个按钮） | 审批人不必打开页面 | 审批卡片已经存在 |
| P2 | **深色模式**、密度档位、可配置列 | 长期值守场景 | 视觉规范先定稿 |

## 交互层（操作路径闭环）

| 优先级 | 项 | 定义 | 依赖 |
| --- | --- | --- | --- |
| P0 | **模板实例化向导**（`/templates` → 参数表单 → 预览 plan → 提交审批） | 从"select template + textbox"走向操作者不会犯错的多步向导 | 可视化层 P0（详情页） |
| P0 | **部分回滚补救向导**：`rolled_back_partial` / `rollback_incomplete` 后的一键向导 | 里侧展示证据链，外侧只问"现在补吗" | 代码层已具备（RollbackChange 幂等） |
| P1 | **空状态及错误状态统一**：所有列表/详情/日志区必须有明确的空 / 失败 / 权限不足 三档 | 不再出现 pending_approval 那种"什么都没有也不报错" | 视觉规范（状态色板） |
| P1 | **键盘导航 + 全键盘路径** | 审批 + 详情页的操作链应能在不开鼠标的情况下完成 | 视觉规范 |
| P2 | **移动审批卡的深链验证** | approve 深链回落时带上已有证据，能告诉用户"这个已批准以免重复点击" | 移动/深链页面 |

## 运营层（产品运营使用）

| 项 | 目标 |
| --- | --- |
| 产品 demo 库：MySQL 主从切换、nginx 灰度、web 配置回滚、集群接管录像 | 现场讲不等于文档 |
| 角色视频：审批人 90 秒、发起人 120 秒、补救 90 秒 | 把入口变成情境演练 |
| 品牌资产：堤坝符号 + 状态色板 + 关键交互 GIF | 不只是一张 README 截图 |

## 明确的**不做**

- 不做通用 workflow 编辑器（拖拽画 DAG）——超出定位。
- 不引入 RBAC 编辑器 UI——`permission` 已供 ABAC 接口，让 API 满足需求即可，不在 UI 开编辑器。
- 不做自由查询编辑器（"搜索任何状态的任何字段"）——REST 列表已经为状态过滤服务。
