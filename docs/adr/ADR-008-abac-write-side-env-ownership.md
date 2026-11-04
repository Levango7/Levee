# ADR-008: ABAC 清单侧 env 归属的**写侧**强制

| 字段 | 内容 |
|------|------|
| 编号 | ADR-008 |
| 标题 | 写入路径是否必须服从 `target-ownership.yaml` 的 env 归属 |
| 状态 | **已定案并实现**（方案 D，P1；见文末"裁决与实现"） |
| 日期 | 2026-11-03（材料）/ 2026-11-04（裁决） |
| 关联 | `docs/product-roadmap.md` 的 ABAC 服务层准入行（"仍待做"那一半） |
| 依据版本 | 材料基于 `origin/master @ 9974b44`；实现基于 `e848f79` |

## 上下文

`env` 这个标签在本仓有**两个身份**（`internal/grpc/resource_authz.go:46-50` 自己写明"one label serves both"）：既是"策略谈的是哪些主机"，也是"谁能碰它们"。归属有两个可能的真值来源：主机库存行的 `labels["env"]`（**调用方自报**），与 `target-ownership.yaml` 注册表（**平台自己的事实**）。

读侧的优先级已经定死并落地：注册表压过 labels（`internal/grpc/resource_authz.go:86`、`:90`、`:101` 三处都先 `ownership.Lookup`），不一致时在 serve 启动打 WARN 并报出两侧值（`internal/inventory/ownership.go:170` 及其冲突清单）。

**没做的是写侧**：`AddTarget` 与 `ImportTargets` 都按请求/文件**自报的** env 判定门禁，从不问注册表这台机器到底属于哪个环境。

## 实测事实（这些决定定级，不看注释看代码）

1. `AddTarget` 的门禁输入是自报值：`internal/grpc/target_service.go:133`
   `authorizeResource(ctx, s.authz, req.Labels[targetEnvLabelKey], ActionAdmin, "AddTarget")`。
   它自己的注释（`:120-132`）已经把残余写清楚了：*"a caller may still assert an environment for an address the platform has never held"*。
2. `ImportTargets` 已经吃掉了**最危险的那一半**：除了判文件声明的每个 env（`internal/grpc/inventory_service.go:132-140`），它还先预览再判"**把主机从哪个 env 拿走**"（`:155-165`，需要那个 env 的 admin），并保证首个写入前就拒绝（不留半应用状态）。
3. **不构成权限逃逸**：条件策略层拿到的 labels 是用"被判定的那个 env"现造的——`internal/authz/authz.go:435` `Labels: map[string]string{"env": env, "target.env": env}`——**不读库存行**。所以把一台注册表认定为 prod 的主机写成 `env=dev`，并不会让它获得 dev 侧的额外许可；随后任何真去操作它的请求都按注册表解析成 prod 被判。
4. 那失真疼在哪里（三条都是实测可指的）：
   - **门禁判错了对象**：dev 的 admin 能为一台 prod 主机建/改清单行，因为他被按 `dev` 授权（事实 1）。这是"能不能在这个 env 里 claim 主机"这个判定本身用错了 env。
   - **运维视图与筛选失真**：按 env 过滤主机、控制台列表、以及任何以 `labels["env"]` 为输入的下钻都读到假值；而 boot-time WARN 只在**启动那一刻**响一次，写入之后没人再提醒。
   - **注册表未覆盖的主机仍由自报 label 单独决定**（设计如此，`resource_authz.go:93` 的 `envOfTarget` 回落）：所以"注册表就是权威"只在它声明过的主机上成立。写侧强制的作用面因此**不大于**注册表的覆盖面——这一点必须写进任何方案的验收里，否则会误以为补完就封死了。

**结论**：这条不是"prod 机器能被 dev 接管"（实测不成立，事实 3），而是"**清单可以对着平台自己的权威事实说谎，而且说谎的那次写入是按假的 env 拿到许可的**"。P0 还留不留，是要 owner 明确表态的第一件事（见"需要回答"第 1 条）。

## 候选方案

### A. 写入即拒绝（fail-closed，无豁免）

注册表认识这台主机、且其 env 与请求声明不一致 ⇒ `FailedPrecondition`，错误串点名两个事实源与两侧值（沿用 `ownership.go:170` 那句冲突措辞）。

- 改动点：`TargetService` / `InventoryService` 各加一个注册表入口（照 `WithAuthorizer` 的既有姿态，`inventory_service.go:34`），`cmd/levee/cmd_serve.go:1650`/`:1657` 与 `cmd/levee/service_factory.go:137` 三处接线；`AddTarget` 与 `ImportTargets` 各一段判定。
- 爆炸半径：存量部署里**已经冲突**的行不会消失（读侧照旧 registry-first），但此后任何一次对该主机的再写入都会失败——包括运维用 `import` 做例行同步。也就是说 A 把一个历史失真变成"今天开始改不动"的阻塞。
- 测试面：两服务各 2 例（冲突拒绝 / 一致零行为变化）+ 1 例"注册表不认识这台主机时行为不变"（防过度收紧）+ serve 接线守卫（证明真把注册表交进去，不是又造一个装饰字段）。
- 与本仓惯例的契合度：高（fail-closed、错误点名事实源、拒绝可解释）。

### B. 按注册表纠正 label（静默改写调用方的声明）

不推荐，且理由要写下来：**它让 `labels["env"]` 变成派生值**，于是一台主机的 env 同时存在于两处（行内 label 与注册表），而读侧仍要 registry-first——两份真值的合并规则会从"读侧一条"扩散成"读写各一条"。这正是本仓今年反复出现、并已被 `scripts/test_doc_anchors.py` 之外的一整类守卫盯着的"平行词表"缺陷。另外它会**静默丢弃调用方声明**，审计里看不出借口。

### C. 只在写入时告警（可观测优先，不改判定）

`AddTarget` / `ImportTargets` 成功写入后，若注册表与 label 不一致就打一条 WARN（含主机、两侧值、来源 RPC），不改判定。

- 成本最低、零阻塞，等于把 boot-time 那一次提醒搬到每次写入。
- 弱点：门禁仍然判的是假 env（事实 4 第一条没解决），台账失真照样进库；它只是让失真**更早可见**。
- 适合作为 A 的第一步（先取得真实冲突面有多大，再决定要不要硬拒），不适合当终态。

### D. 拒绝 + 显式豁免（推荐）

A 的判定 + 一条明确的逃生门：默认拒绝，`ImportTargetsRequest` 增加 `allow_env_conflict`（gRPC 字段，REST 同名键，CLI `--allow-env-conflict`），豁免必须**调用方点名**才生效，且豁免生效时写一条 audit/trace 说明"按调用方声明入库、与注册表不一致"。

- 为什么推荐：既解决"门禁判错对象"（默认路径不再接受假 label），又不把例行 `import` 变成阻塞；豁免是显式决定而不是代码里偷偷放宽，与本仓既有姿态同源（`ImportTargets` 的 stored-env 检查、`RemoveAgent` 的 refusal、E-3 的 `allow_irreversible` 白名单都是"默认拒 + 点名豁免"）。
- 代价：`proto/levee_extra.proto` + 重新生成的 `pb` 要动（一次 proto 变更，`proto regenerate check` 那道门会要求同步），且新增字段必须被消费（否则就是本仓点名的"假字段"）。
- 未定的子问题（要 owner 表态）：豁免是**每次调用**给，还是允许 serve 配置里长期打开？后者的正确回答大概是"不许长期打开"，因为长期打开等于没有判定。

## 需要 owner 回答

1. 定级：这条继续挂 **P0** 还是降 **P1**？依据是本文"实测事实 3"——不构成权限逃逸，但构成门禁输入失真。定级影响它排在谁前面，不影响它要不要做。
2. 选 A 还是 D（拒绝 / 拒绝+豁免）？C 只作为 D 的第一跳是否接受？
3. **存量冲突行的处置**：只挡新写（A/D 的默认），还是同时提供一次普查报数（"这个库里有多少行的 label 与注册表矛盾"）？后者是新读路径，不属于本 ADR 的最小面。
4. 长期真值来源：注册表是否为 env 的唯一权威？若是，`labels["env"]` 是否应降级为派生显示值（那是**数据模型变更**，本 ADR 明确不主张，但要表态否）。
5. 豁免能否作用于 `AddTarget`（单主机登记）还是只给 `ImportTargets`（批量同步）？前者打开等于门禁可被绕过，倾向只给后者。

## 未核清单（本文没有证明的事）

- 没核注册表在真实部署里的**覆盖率**：写侧强制的作用面以它声明过的主机为上界，覆盖率低 ⇒ A/D 的实际收口很窄（这条必须由部署侧给数据）。
- 没核控制台/前端是否还有以 `labels["env"]` 为输入的其他筛选或聚合（只核了服务端判定链）。
- 没核 `levee rbac check --labels`（`cmd/levee/cmd_rbac.go:390`）那条路径——它的 labels 由调用方手输，与库存无关，因此不受本议题影响，但我没有逐行证明。
- 没跑存量库普查：本机没有真实 inventory 数据，冲突量级是空的。
- 没核 REST 侧映射：新错误码/字段若要透出，`rest_endpoint_table_test.go` 那张路由表需要同步（本文不改动它）。

## 裁决与实现（2026-11-04）

owner 的五条回答，按顺序：

1. **定级：P1。** 依据是"实测事实 3"——不构成权限逃逸，但构成门禁输入失真。P1 不改变"要做"，只改变排序。
2. **选 D。** 默认拒绝 + 显式豁免；C 不作为独立终态。
3. **只挡新写。** 存量冲突行的普查是另一条读路径，本笔不提供。启动时的冲突 WARN 仍然保留（`loadTargetOwnershipRegistry`）。
4. **注册表是它声明过的主机的唯一权威。** `labels["env"]` 不降级为派生值（那是数据模型变更，本 ADR 不主张）；对注册表**未声明**的主机，labels/默认 env 继续是唯一事实。因此写侧强制的作用面上界就是注册表的覆盖率。
5. **豁免只给 `ImportTargets`。** `AddTarget` 没有豁免字段，也没有 flag；单主机登记若与注册表冲突，只能改对或先改注册表。

实现的落点（与上面的候选方案对应）：

| 面 | 改动 |
|----|------|
| 判定 | `internal/inventory/ownership.go`：`EnvConflict`、`(*Importer).EnvConflicts`。判定的是**写入后会留下的** env（对已存在的行走 `mergedLabels`，所以"不带 labels 的再导入"不能把旧值保留成假事实），不是只看文件字面声明。 |
| `AddTarget` | `internal/grpc/target_service.go`：`WithOwnershipRegistry`；授权之后、写库之前判冲突，`codes.FailedPrecondition`。无豁免。 |
| `ImportTargets` | `internal/grpc/inventory_service.go`：`WithOwnershipRegistry`；在 stored-env 检查之后判冲突。`allow_env_conflict` 为假则拒绝，为真则导入并在成功后写一条 `audit.action=import_env_conflict`（`Target` 是冲突主机清单，最多列 8 个）。豁免**不绕过**原 stored-env admin 检查。 |
| proto | `proto/levee_extra.proto`：`bool allow_env_conflict = 3`（`ImportTargetsRequest`），重新生成 `pb`。 |
| REST | `internal/grpc/rest_inventory.go`：同名 JSON 键 `allow_env_conflict`；`FailedPrecondition` 经既有映射是 HTTP 412。 |
| CLI | `cmd/levee/cmd_target.go`：`--allow-env-conflict`。本地导入直连 importer，必须同一判定，否则它就是绕过口；豁免同样写审计（`audit-import-*`）。 |
| serve | `cmd/levee/cmd_serve.go`：把已经加载的 `ownershipReg` 交给 `targetSvc` / `inventorySvc`；`cmd_serve_ownership_test.go` 钉住这条接线。 |
| 审计词表 | `internal/state/store.go` 的 action 常量 + 两份 schema 的注释清单（`audit_vocabulary_test.go` 强制同步）。 |
| 测试 | `internal/inventory/ownership_write_test.go`、`internal/grpc/ownership_write_test.go`、`internal/grpc/rest_inventory_test.go`、`cmd/levee/cmd_target_ownership_test.go`；每个拒绝用例都配一个"注册表未声明/无注册表时行为不变"的控制组。 |

仍未覆盖（与"未核清单"一致，不是回归）：真实部署的注册表覆盖率、控制台其余以 `labels["env"]` 为输入的下钻、存量冲突行普查。写侧强制只对注册表声明过的主机生效。

## 边界

本文的材料部分（候选 A–D）写于 `#272` 在飞期间，当时的建议是"先做判定函数 + 两服务入口，接线等 rebase"。实现时 `#272` 已合并（`e848f79`），三处线头不再冲突，因此判定、两个服务入口与 serve 接线在同一笔里落地。

`service_factory.go` 的本地 `TargetService` 没有接线：它没有生产调用方（CLI 的本地导入走 `cmd_target.go` 直连 importer，已加同一判定），给它加一个永远不会被读到的注册表字段，就是本文反对的那种"装饰字段"。若将来有本地服务调用方，再加接线并补一条与 serve 同形的守卫。
