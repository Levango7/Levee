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
| P0（阶段 0–2 已完成，阶段 3+ 待做） | **ABAC 服务层准入：把注册表接进 `serve`** | `permission.Checker` 按 `team × env × action` 判定且**拒绝空 team**。数据其实已经有了——`<dataDir>/users.yaml` 的 `userEntry{name, team, role}` 就是 actor→团队映射，`levee user add` 一直在写——问题是四份 RBAC 资产（`permissions.yaml` 矩阵 / `users.yaml` 注册表 / `roles.yaml` 角色树 / `policies.yaml` 策略）**只有 CLI 读写，`serve` 一次都不加载**；且 `users.role` 至今无消费者，而 `RoleTree.EffectivePermissions(role)` 恰好提供了本该接它的 API（`users.role → 角色树` 与 `users.team → 矩阵` 是两套并行、互不知情的授权模型，合并规则待产品定）。**阶段 0–2 已落地（2026-09-28）**：注册表提为 `internal/identity`；`roles.yaml` 读写归位到 `permission`；`internal/authz` 组合矩阵（环境可达性）与角色树（环境内动作能力），`apply`/`rollback`/`approve`/`reject` 已按策略准入，并配 `levee authz status|explain` 诊断。**阶段 3 已落地（2026-10-04）**：`plan` 也按策略准入（此前漏在外面，而 re-plan 会把 `approved` 重置回 `draft`——任何认证调用方都能撤销别人的批准）；变更读路径（`GetChange`/`GetLogs`/`GetTrace`/`GetDiff`/`WatchChange`/`StreamLogs`）按 `view` 准入，`ListChanges` 按可见环境**收窄**而非整页拒绝；`pause*` 的批量动作改按**已验签主体**判定（原先用客户端自报的 actor，等于把 `bulk_grants` 变成可宣称的名字）。**缺省姿态（刻意与写不同）**：无可归因主体的调用者读路径放行并只在启动 WARN——共享令牌下所有人是同一主体，拒他们不产生隔离、只让看板变黑；要按队隔离读可见性就启用命名令牌/SSO/OIDC。**待做**：`policies.yaml` 条件式 ABAC（仍只有 `levee rbac` 在算）、其余服务的读面（template/target/audit/system——`SystemService.GetConfig` 已改由 `security.expose_raw_config` 决定脱敏，但 `GetConfig`/`GetStatus` 本身仍无授权判定）。设计提案见审查报告附档（含分阶段与"先对账再启用"的迁移要求） | 无 |
| P1 | ~~回滚后验证的生产装配：把 `rollback.PostRollbackVerifier` 注入 `wiring` 的闭包执行器（`NewClosureRunner(..., nil)` 目前恒为 nil）~~ **已完成（2026-10-01）** | 每次 run 注入 verifier（与门禁共用同一个 `GateManager`，phase 模式重跑本 plan 的 post-apply 门禁）。同时定案 `verify_after` 为 **opt-in**：显式 `true` 才验证，规范 §7.1 的「缺省 true」被订正——按缺省 true 装配会让存量计划升级后新增向目标机派发门禁命令的副作用。遗留：未附 `Grader`（故 `Grade` 恒空、不触发 notify/escalate），取消触发的回滚其验证记为 ctx 取消失败 | 无 |
| P2 | ~~run 级快照原语（显式 `scope: run` 或独立顶层 `snapshot {}`）：首批发前**一次性**采集、回滚时恢复~~ **已完成（2026-10-01）** | 采纳"独立原语"而非放宽 §7.1：新增顶层 `snapshot { scope, paths, type }` 块（LE098–LE101），`plan.RunSnapshot` 进 artifact 且**进 plan_hash**（基线决定回滚还原什么，两份只在基线上不同的计划不是同一份计划）。采集在所有锁已持有、零改动时**一次**，恢复在补偿走完之后**一次**（run 的前状态对它覆盖的路径有最终发言权）。**与 step 级刻意不同的一点**：声明了基线却没有采集器时 **fail-closed 拒绝本次 run**，因为 step 级"没装就是 no-op"是未接线行为，而这里是声明——照常执行会改掉一批从未留下前镜像的目标机。`on_failure: manual` 曾被 LE102 拒绝（手动路径不恢复基线），该限制已随手动回滚接上基线恢复而解除 | 无 |
| P0 | ~~完成 D-3 遗漏的链接：CLI 终态矩阵 / proto 注释 / metrics 标签引用 `runstatus`~~ **已定案并落地（2026-09-26）** | 三处副本全部换成引用：CLI 的 `isRollbackableStatus` / `isTerminalStatus` 改调 `runstatus`（前者此前比服务端门禁多放行 `running`，后者漏掉三个终态），帮助文本与报错由 `JoinRollbackAdmitted()` 拼装；`metrics` 标签常量改为别名（只保留非 run 状态的 `created` / `succeeded`）；proto 注释改指 `runstatus` 并由 `TestProtoStatusCommentMatchesGo` 钉定。守门测试去掉 `metrics` 豁免、扫描范围扩到 `cmd/` | 无（已实现） |
| P0 | ~~`recommend` → `Change` 的正式桥~~ **已定案并落地（2026-09-26）** | `internal/conversation/change_bridge.go` 在确认后把 `WorkflowDraft` 交给既有 `ChangeService.CreateChange`，产出 `draft` 变更进入标准治理链；草案先过与 `levee compile` 严格模式相同的解析 + 校验两道门（含 LE097），fail-closed：不通过则一条变更记录都不建，会话留在 `reviewing`。**桥不执行工作流、不跳过审批**。两个入口都接上了：serve 路径用进程内 change 服务，**CLI `levee converse` 用懒打开的本地 store**（只在第一次确认建议时才开，生命周期挂到引擎 `AddCloser` 上，只读命令不碰数据库） | 无（已实现） |
| P1 | `idempotent: true` 的存量 Lint：检出未声明幂等的 undo 步骤并引导作者补齐 | 方案 C 留下的最后一步：从被动门禁变主动信息 | D-3 |
| P1（**②①去莠④⑥③已落地 2026-10-03**） | **8 个未链入包的逐包处置（接线 / 改良 / 退役）** | 2026-09-29 按 `go list -deps ./cmd/levee` 与非测试导入数核查：`compat`、`diagnosis/llm_diag`、`diagnosis/topology`、`notify/chatopsbridge`、`opsmesh`、`recommend/feedback`、`recommend/rag`、`scheduler` 共 8 个包未链入二进制，非测试代码合计 4,691 行，包内测试 90.4%~95.4%——覆盖的是没人调用的代码。已验证的接线前置缺陷：① `compat` 的执行器自述不强制审批与门禁（`compat/executor.go:4-7`），动作表映射 4 个不存在的动作（`compat.go:48-55`），且映射是**状态盲**的（`yum: state=absent` 会被映射成 `pkg.install`）——改映射会连带改 `compat/risk.go` 的风险分级，属于重定义 D-08，需产品定夺；② `chatopsbridge` 两个观察者槽位都是**单槽**（`approval/service.go:258`、`grpc/change_service.go:116`），而 `serve` 已把两槽都给了 Jira 镜像（`cmd_serve.go:740,747`），直接接线会**静默挤掉 Jira**，必须先改成扇出；③ `llm_diag` 的收敛判据取自模型自报的 `Confidence`，而这个值在产品里要映射到审批档位，不能信模型自述；`recommend.NewLLMClient` 的 22 个调用点全在 `llm_test.go`，`internal/config` 也没有 llm/rag 配置面；④ `rag` 的唯一 embedding 实现是 FNV-1a 哈希 + 同余生成器造伪向量（`rag/embedding.go:7-10`），接上等于把检索退化成噪声；⑤ `feedback` 全内存、进程重启即失忆，且它写入的知识库本身在生产侧也没有持久化入口；⑥ `scheduler` 做的是 agent 任务派发（`Schedule([]agent.Task)`），与已接入的 `internal/dispatch` 职责重叠，不是变更时间窗触发器——真正的缺口是 `dsl.Workflow.Window` 无人读取（仅 `parser.go:313` 赋值）、calendar 的 cron 只用于冲突检查。~~建议顺序：先做 ②（110 行），再做 ①的去莠，④⑥按定位退役，③单独连 `ai.llm` 配置面一起做~~——**四项已于 2026-10-03 落地**：② 两个观察者槽位改成扇出（`WithDecisionObserver`/`WithApprovalCreateObserver` 每次安装都保留、不再互相挤掉，nil 清空、逐调用 panic 恢复，approval 与 grpc 两包守卫测试钉住）；① 去莠——`internal/compat` 的模拟执行器与重复的风险评估器删除，导入层与 D-08 映射保留；④ `internal/recommend/rag`（FNV-1a 伪向量）与 ⑥ `internal/scheduler`（与已接入的 dispatch 职责重叠）整包退役删除，未链入包 8→6 个；③ `ai.llm` 配置面——`internal/config` 新增 AI/LLM 节，`serve` 与 `levee converse` 都读它（默认关、不构造客户端，构造失败降级回知识库模式并打 warn）。**剩余三项也已落地（2026-10-03 同日）**：chatopsbridge 接线——`notify.chatops.*` 配置节（enabled + bots[platform/name/webhook_url/secret/timeout]）在 serve 装配 slack/dingtalk/feishu webhook bot（StartAll，退出 StopAll），bridge 装入扇出槽位与 Jira 并存，bot 缺 name/webhook_url 或平台未知时拒绝启动并指名；llm_diag 接线与判据重定义——收敛必须**跨两轮同假设确认**（稳定性门禁，单轮自报不再采信）、`ReasoningResult.ConfidenceSource` 显式标注 `model_self_report`（不当作已验证事实）、死状态 `StatusInconclusive` 撤除；`ai.llm` 开启时 DiagnosisService 用收敛结果富化报告，失败/未收敛保持规则结果；feedback——PatternID 存储缺陷修复（Learn 的落戳曾写进无人能读回的副本）、JSON 快照持久化（重启恢复 records/stats/patterns/incidents 并回填共享 KB）、serve 与推荐引擎**共享同一 KB**（Learn 曾写进引擎看不到的私有目录）、Apply 结论喂入学习闭环（change_service 终态记录 FixOutcome）。**防再犯已落地**：`TestMappingResolvesToRealExecutorActions` 用执行器注册表当外部 oracle 遍历解析器全部输出；D-08 的映射重定义随同完成（状态感知 + fail-closed 拒绝 + 幻影动作清零）。未链入包 8→3（剩 `diagnosis/topology`、`compat` 导入层、`opsmesh`）。**最后三项同日收口**：`diagnosis/topology` 接到诊断管线的**影响半径阶段**（`diagnosis.topology.*`，SkyWalking/Pinpoint 采集器 + `ImpactRadius` 分析，service 类 Findings 进报告，默认关、失败降级）；`compat` 补 CLI 入口 `levee import ansible`（纯翻译，产出经 parse+validate 两道门才落盘；play `name:` 进 Meta.Name 保证 LE002 可编译）；`opsmesh` 接线**结果回传**方向（`opsmesh.*`，告警驱动修复终态 POST /alerts/{id}/resolution；拓扑/指标**拉取方向同日定案并接线**（读平台源码：真实端点是 /api/v1/catalog/topology 与 /api/v1/prometheus/query；host 匹配在客户端 metadata 值包含完成；诊断阶段 provider=opsmesh + levee opsmesh CLI；resolution 端点平台尚未实现，404-降级与不用 ack 顶替的理由如实登记）。**未链入包 8→0**。 | ✅ 8 项处置 2026-10-03 全部落地（含最后三项收口） |
| P2 | `internal/docgen` 的多语言实现（把 `web/src/types/levee.ts` 那张表也生成为 TS） | 现在 `TestWebStatusMirrorMatchesGo` 还是手工比对——它本身也不过是"另一份手写词表"在 Go 里的验证镜 | D-3 |
| P0（已落地 2026-09-29，日历侧一半待接） | **变更窗口强制：`window` 声明此前全链路不生效** | 规范 §4.2 承诺"变更只允许在该窗口内执行"，但 `ChangeWindow` 零读取点（只有 `parser.go:313` 一处赋值），`plan.Plan` 不携带窗口，`calendar.CheckWindowForPlan` 生产零调用；实测 `start: "25:99"` / `timezone: "Mars/Olympus_Mons"` / `days: ["funday"]` 编译通过，声明周日 03:00-04:00 的变更在周二 22:05 走完 plan→approve→apply。已补：`internal/dsl/window.go` 文法+判定、validator 的 V19（LE020/LE021/LE003）、`GeneratePlan` 的窗外阻断（规范口径：plan 阶段阻断、不进审批；回滚按规范豁免，由结构不变量测试钉住）。**剩下的另一半**：组织级变更日历（`calendar` 的窗口/冻结期 + `CheckWindowForPlan`）仍未被 plan 路径调用，`Window.MaxConcurrency` 也仍无人读取（规范 §4.2 字段表里没有这个字段，属实现私有扩展） | D-3 / F08 |
| P1 | **`CompileWarning` 一档没有产生点**：LE033 / LE052 / LE094 / LE095 / LE096 五个警告码在目录表登记，但没有任何代码发出 | `Validator.Validate` 返回的是扁平 `[]ValidationError`，两个消费者（`cmd_compile.go:87` 严格模式、`conversation/change_bridge.go:86` 的 fail-closed 门）都把非空当成致命。因此"缺窗口/缺审批/缺批次"这类**应当只警告**的规则一旦塞进 `Validate`，会让所有未声明的既有工作流在编译期与会话桥同时被拒——是行为破坏而不是补门禁。要做的是先给 `ValidationError` 加 severity 并在两个消费者处按 severity 过滤，再逐条发出警告 | 与 D-3 词表收尾同族 |
| P1 | **`allow_irreversible` 白名单与 LE082 均无产生点（规范 vs 实现的又一处差距）** | 规范把 `allow_irreversible` 列为 workflow 级字段、验证表登记 LE082（irreversible action not in allow_irreversible），但解析器没有该字段、validator 没有该规则、全仓 LE082 零产生点（仅错误目录与故障分类器引用它）。**今天不可逆保护是两层真实机制**：模块内 confirm=yes 硬门（mysql.replica_switch 运行时拒绝无确认切换）与计划侧 irreversible: true 的高危审批路由。接线做成编译期门禁会**改变存量行为**（缺省禁止不可逆动作 → 既有含 irreversible: true 步骤的工作流编译失败），需按窗口强制（LE020/LE021）的先例带迁移说明与演练；已在 spec 字段表与 docs/scenario-cross-region-ops.md §7 如实标注「尚未接线」 | 规范一致性 / 治理门禁 |
| P0 | **`levee new <模板>` 产出的 run 无法 plan：渲染后的工作流没有落库** | 实测（本机临时 store + CLI）：`levee new` 只把 `result.TemplateName` 写进 `run.WorkflowName`，**渲染出的 `result.Content` 只出现在命令输出里、从不持久化**；随后 `levee plan <run>` 报 `wiring: parse inline workflow for change "...": LE001: cannot unmarshal !!str 'outwin' into dsl.yamlWorkflowRaw`——即 plan 阶段拿模板名当工作流解析。`internal/wiring/resolveWorkflow` 同时接受内联 YAML 与文件路径，`CreateChange` 走的是内联，所以会话桥路径是好的，坏的只有模板路径。要么把 Content 写进 run（新增/复用一列，需 store 迁移），要么让模板实例化落一个工作流文件并把路径写进 run；顺带说明：`examples/templates/patch-rolling.yaml` 仍是旧方言（`workflow:` 信封 + `window: "02:00-04:00"` 字符串 + `approval: high` 字符串），修好链路后它照样编不过 | D-2.2.3 / 模板库 |
| P1 | **`approval.level` 词表在 `internal/approval` 侧仍有手写副本** | 本仓已把 DSL 侧三处副本（parser / validator / typechecker）收成 `dsl.ApprovalLevels`；`internal/approval` 里 `service.go:221` 的 switch 与 `levels.go:216,270` 的报错文案还是各自手抄的 `standard, high, emergency`。今天值相同所以没出事，但"审批档位改名"这类改动会在其中一处留下不一致的判定或文案——`runstatus` 那轮的教训（词表收成引用，报错由词表拼装）在这里同样适用 | D-3 同族 |
| P1（**需要定夺**） | 给"state 先初始化"的现网库补一条迁移：`ALTER TABLE cluster_nodes DROP CONSTRAINT IF EXISTS cluster_nodes_address_key` | `cluster_nodes` 的 DDL 已收成 `internal/dbschema` 唯一一份，并按集群路径实际生效的形状去掉了 `UNIQUE (address)`；但 `CREATE TABLE IF NOT EXISTS` 不会改动已存在的表，所以历史上由 `pgschema.sql` 先建库的部署**仍带着这条约束**。它不是闲置：节点身份是 `id`，同一监听地址以新 `id` 重新注册是 failover/takeover 的正常动作，实测含该约束时 `internal/takeover` 有 10 个用例报 `SQLSTATE 23505`。是否现在动现网 schema（以及是否顺带给缺 `capabilities` 的 cluster-first 库补列）是迁移决策，不放在本 PR 里 | 集群 / 迁移链 |
| P1 | **master 端 Agent 注册表的 RPC**：`levee agent list/show/remove` 今天跨进程恒空 / 恒 not-found | 2026-10-04 实测：`AgentRegistry` 只在 `cmd/levee/cmd_agent_support.go` 里以进程内单例存在，既没有落盘入口，`proto/` 里也没有任何 agent 服务定义，`internal/grpc`、`internal/wiring`、`internal/cluster` 三个装配面对 `AgentRegistry` 的引用计数实测均为 0（只有定义它的 `internal/agent` 与 `cmd/levee` 用它）。也就是说注册/心跳/派发只活在 `levee agent start` 那一个进程里，CLI 文档第 20 章写的"master 端"是未实现形态（已在文档加边界说明，并把示例改成不承诺跨进程可见）。`--status` 旗标已按真实词表（`registered/idle/busy/offline`）接线并有测试，但它的可观测性要等这个 RPC 才真正有意义。**前置条件**：新增 RPC 要改 `.proto`，而 CI 用 protoc 27.0 钉住 `internal/grpc/pb/` 并 `git diff --exit-code` 校验，本机 protoc 是 36.2，产物对不上——必须在 CI 同版本工具链下生成 | 交付物 / agent 链路 |
| P1 | 漂移基线的**命名多版本**（`--name` 已从文档删除） | `BaselineManager` 明确以 host 为主键、"一台主机同时只保留一份基线"（`internal/drift/baseline.go:106-108`），`set`/`auto` 都是覆盖写。文档曾写 `--name 自动生成`，读起来像"一台机器可以存多份命名基线"，但那要改存储主键与 `--baseline auto` 的取用规则（现在按 host 取唯一那份），不是加旗标。删除承诺而不是造半成品 | drift / 文档一致性 |
| P2 | `tenant delete` 的**级联清理**（`--force` 已从文档删除） | `Delete` 只做软删除 + 释放配额，`TenantManager` 不持有"该租户名下有哪些 target/run"的反查接口，所以"级联清理租户所有数据"今天无从实现；而它是破坏性操作，接线前要先定"清哪些、能不能回滚、谁有权"。同时 `Delete` 没有"必须先 suspend"的前置检查（文档原来那么写，已订正）——如果产品要这条守卫，那是新增行为而非补文档 | 多租户 / 文档一致性 |
| P2 | `agent remove` 的**在途任务守卫**（`--force` 已从文档删除） | `Deregister` 不检查 `ActiveTasks`，直接删记录，所以没有守卫可供 `--force` 绕过。文档写的"即使 Agent 仍有正在执行的任务"暗示存在一个默认拒绝路径，实际不存在。要做的是先决定"在途时拒绝移除"是不是期望语义（它会让 `remove` 从无条件成功变成有条件失败） | 交付物 / agent 链路 |
| P1 | `cluster_nodes.capabilities` 列仍无人读写 | 合并 DDL 时保留它（否则 cluster-first 环境永久少一列），但写侧与读侧都没用到——节点能力上报要么用起来（配一条 `ADD COLUMN IF NOT EXISTS` 迁移照顾 cluster-first 旧库），要么明确退役；`TestClusterNodesDDLCoversEveryReaderAndWriter` 已把列集合钉成显式清单，加/删列都会强制同步该测试 | D-3 / 集群 |

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
