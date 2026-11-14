# 设计提案：云原生变更治理面（k8s 上的"谁批、谁回滚、谁举证"）

状态：**提案（待 owner 拍板 S1–S3 的先后与范围）**。本文档是策略/设计层提案，**不改任何代码**；事实基线逐条给证据（CI 腿、文件路径）。

## 0. 一句话

云原生环境里，**声明式交付**有 ArgoCD/Flux、**观测**有 Prometheus/Zabbix/ELK/Loki、**作业**有平台；**"高危动作级变更的审批·回滚·举证"没人管**——那正是 LEVEE 的位置。本提案把已有的治理内核（审批 / 门禁 / 分批 / 证据 / 回滚 / 围栏接管 / 断网策略）**延伸到 k8s 工作负载**，**不新增通用运维能力**。

## 1. 事实基线（可复核）

> 注：本节是 2026-11-09 提案时的基线快照。S1/S2 落地后部分行已变化（k8s 已可作变更目标、金仓 PG 模式已验证、达梦 DM8 实测不兼容）；**现状以 `docs/platform-support-matrix.md` 为准**。

| 事实 | 证据 | 结论 |
|---|---|---|
| ARM64 已在构建矩阵里 | `ci.yml` 的 `build` 矩阵：ubuntu / macos / windows × amd64 / arm64 共 6 腿 | ✅ ARM（含鲲鹏/飞腾 aarch64）**不需要新增能力**，只缺运行验证 |
| LEVEE 可作为 K8s 工作负载部署 | `deploy/helm/levee/templates/`：deployment、cluster-worker、postgres、pvc、ingress、secret、configmap、service；CI `delivery (helm lint + render + package)` 腿 | ✅ 部署面已具备（含多节点 worker 形态） |
| K8s 工作负载**不能**当变更目标 | 执行通道只有 `internal/channel/{local,ssh,winrm}`；全仓无 kubectl / k8s 客户端代码 | ❌ 今天改 k8s 工作负载**无法**走审批-门禁-回滚-审计链<br>（**提案基线，已被 S2 推翻**：k8s 变更通道已作为运行期通道插件交付，见 `docs/platform-support-matrix.md` 与 CHANGELOG「feat(k8s): S2 k8s 变更通道」） |
| 数据库只有原生 SQLite + PostgreSQL | `internal/state/{sqlite,pgstore}.go`、`driver.go` | ⚠️ 信创栈的"PG 位"通常是达梦/金仓**兼容模式**，**未验证** |
| 通用运维面薄 | 执行模块仅 6 个（`internal/executor/modules/{file,mysql,pkg,shell,svc,user}`）；无监控服务、无日志管道；inventory 只有 target/group/label/file | ⚠️ 薄，但**本文不打算补它**（§2） |
| 治理内核厚 | `internal/{approval,audit,rollback,evidence,calendar,dispatch,takeover,cluster,drift}` ＋ 断网策略四段（`docs/design-disconnect-policy.md`）＋ 灾备三场景实测 | ✅ 延伸的底座在 |

逐格的信创/ARM/k8s 结论见 `docs/platform-support-matrix.md`（该表只登记本仓跑过的结论）。

## 2. 为什么是"治理面延伸"，而不是"补通用运维面"

| 路线 | 能拿到什么 | 代价 | 判断 |
|---|---|---|---|
| **A** 补通用运维面（监控 / 日志 / CMDB / 作业 / Ansible 模块广度） | 一个"什么都有一点"的平台 | 与 `docs/product-positioning.md` §2 的"不是 CMDB / 日志平台 / 告警引擎"**直接矛盾**；对手是 Zabbix / ELK / Ansible 本身，赢不了；模块广度差一个数量级（6 vs 上千） | ❌ |
| **B** 治理面延伸到云原生（本文） | 在 k8s / 信创 / 裸金属上，**高危变更仍受审批·门禁·回滚·审计·举证约束** | 需要新增 k8s 执行面（S2）与 k8s 特有的回滚语义 | ✅ **推荐** |

**判据（可验收的一句话）**：
- B："在 k8s 上改一个 Deployment，必须经人审批；出事能一键回到上一版本；事后能拿出完整链路证据。"
- A："我们的监控比 Zabbix 好"——比不了。

## 3. 楔子与边界（对外话术）

| 场景 | 归谁 | LEVEE 的角色 |
|---|---|---|
| 声明式交付（GitOps、持续同步） | ArgoCD / Flux | **不碰** |
| 观测（指标 / 日志 / 告警） | Prometheus / Zabbix / ELK / Loki | **不碰**（只在变更发生时把证据取出来） |
| **动作级高危变更**（重启 / 回滚 / 改配置 / 改副本 / 灰度） | **LEVEE** | 审批 · 分批 · 门禁 · 回滚 · 审计 · 举证 |
| 数据库变更 | LEVEE（v1.2 第一身份） | 审核 → 计划 → 审批 → 执行 → 审计 |

与 `docs/product-positioning.md` §2「不是…」表的区别：那条表说的是"LEVEE **不是**这些产品"；本文说的是"这些场景里**哪一段**归 LEVEE"。两者不冲突。

## 4. 分片（每片都能在本地验收）

### S1 — k8s 工作负载**策略门禁**（纯函数，不需集群）

> **状态：已实现**（`internal/k8spolicy` 策略引擎 ＋ `internal/verify` 的 `K8sPolicyGate`；**19 条策略**、解析失败 fail-closed、拒绝必带字段定位与补救文案；门禁的**注册**已在引擎侧接好（`internal/engine/step_gates.go` 的 `case "k8spolicy"`），**DSL/parser 接线**归 `internal/dsl` 认领者，待其落地后 YAML 才能声明该门禁）。
> 策略覆盖两族：**权限与卫生**（privileged / allowPrivilegeEscalation / hostPath / hostNamespace / hostPort / runAsUser=0 / add capabilities / 未固定镜像 / 缺 resources.limits / 未声明 runAsNonRoot / 自动挂载 SA token）＋ **凭据与运行属性**（`/proc` 未遮蔽 / 默认 ServiceAccount / 凭据以字面量写进 env / 根文件系统可写 / 未 drop ALL capabilities / 缺 liveness / 缺 readiness / 明文 HTTP 镜像仓库）。后一族是 2026-11-12 补的：前一族管「容器要了太多权限」，后一族管「这个容器能不能安全地跑起来、出了事能不能被发现」。

- **做什么**：一组"提交前"判定：privileged / hostPath / 未声明 `resources.limits` / `image: latest` 配 `imagePullPolicy: Always` / 缺 `runAsNonRoot` 等 → 拒绝或告警，**必须给出可读的理由**（不静默拒绝）。
- **形态**：接入既有 gate 家族（`internal/verify`，含 pre_apply / post_batch 位置），与既有"拒绝要带原因"的约定一致。
- **验收**：纯函数单测（本地 `go test`，**不依赖集群**）；样本取自常见基线（含信创/安全基线里的高危项）。
- **边界**：不引 K8s 依赖、不做完整 schema 解析、不做策略引擎（OPA 之类）。

### S2 — k8s 变更通道（执行面）

> **状态：已实现**（`examples/plugins/k8s`，通道插件 `channel_types: [k8s, kubernetes]`；**核心零行改动**）。机制：核心快照器（`internal/wiring/snapshotter.go`）对通道只说两句话——`Exec("base64 <路径>")` 抓状态、`Upload(路径, 内容)` 写回；本插件把"路径"解释成**资源引用**（`base64` → `kubectl get -o json` 并剥掉服务器记账字段 resourceVersion/uid/generation/creationTimestamp/managedFields/status；`Upload` → `kubectl apply -f -`），**"回滚＝按快照还原（不是重放一条反向命令）"在零核心改动下成立**——恰好绕开本仓里`internal/wiring`、`cmd/levee`、`internal/dsl` 三处他人认领区。
> 验证：契约单测 22 例（fake kubectl 断言**参数向量**：钉死的 kubeconfig/context 前置、动词白名单、改写目标旗标拒绝、抓取剥字段、还原逐字节透传、协议环回）＋ **kind 实测**（`TestK8sChannelDrill`，env 门控，真集群：抓取→scale→按快照还原断言）。
> **已知边界（如实）**：① 插件通道只在 **serve 启动时装入**（`installPluginChannels`），CLI 一次性路径 `levee apply --engine-enabled` 不装——三场景的引擎半边（审批拒绝→不执行、审计链）是引擎既有行为且传输无关（`internal/grpc` 的状态守卫与审批链测试覆盖），走 serve 即得完整链路；② `levee target check` 的探针是 `echo ok`，集群答不了（只影响那条诊断命令，plan/apply 不做该探针）；③ 通道 exec 无 stdin，manifest 应用走 upload（即快照还原的通道），`kubectl apply -f -` 在 exec 形态下读到 EOF 即失败——有意为之，不给宿主侧任意文件读取留口子。
- **做什么**：让"改 k8s 工作负载"成为一个受治理的 target：
  1. 变更前**快照**（副本数、镜像 digest、关键 annotation）；
  2. 执行（受审批与门禁约束）；
  3. **验证**（rollout 状态）；
  4. **回滚**＝按快照还原（不是"重放一条反向命令"）。
- **形态**：优先走**插件通道**的缝（`channel_types` 机制，PR #308 已留），把 k8s 放在 `examples/plugins/`，**不把 k8s 写死在核心里**。
- **验收**：本地 kind 集群三场景——改 Deployment 副本数 → 审批拒绝则**确实没发生** → 允许则发生 → 回滚还原 → 审计链留行。
- **边界**：不做 Operator、不做声明式同步、不做 GitOps。

### S3 — 信创 / ARM **适配矩阵**（把"信创硬件"四个字变成可投标的清单）

- **做什么**：按 `docs/platform-support-matrix.md` 逐格补证据：OS（alpine / 麒麟 / 统信）× arch（amd64 / arm64）× DB（SQLite / 原生 PG / 达梦 / 金仓兼容模式）的"能装 / 能跑 / 能备份恢复"。
- **验收**：buildx 多架构构建 ＋ 在 ARM 容器里跑关键用例（qemu 可用与否**如实记**）；国产 DB 的 PG 兼容模式**实测**结论（能 / 不能 / 需要改什么）。
- **边界**：只登记实测结论，**不做"假支持"的适配层**。

## 5. 风险，以及"这条路线不成立"的判据

1. 若真实标书里出现"必须自带监控 / 日志采集 agent"或"必须提供 Operator"→ A 路线的诉求变成真的，届时重估。
2. 若客户现场的 k8s 高危变更**全部**由 GitOps 平台承担（没有"动作级变更"这回事）→ S2 无落点，先做 S1 / S3。
3. 若信创栈的 DB 兼容模式差异大到治理链跑不通 → **先解决 DB 适配**，不要先扩云原生（顺序错了会把两个风险叠在一起）。

## 6. 与账本 / 路线图的落点（待拍板后执行）

- `docs/blocked-items.md` **A-4**（信创环境）：解锁后动作补一句"按 `docs/platform-support-matrix.md` 逐格取证"。
- 新增一条 D 类（云原生变更治理面），owner 拍板范围与先后。
- roadmap：S1 / S2 / S3 各一行（状态 open）。

## 7. 诚实边界

- 本文档没有写一行代码。S2 的实现细节（快照语义、"物理副作用不可逆"时回滚能做什么不能做什么）需在动手前再设计一次——**不要照搬 SSH 通道那套"反向命令补偿"的假设到 k8s 上**。
- 本文对"市场会不会买"的判断来自本仓定位文档与产品结构，**不是**来自真实标书；真实标书到位后 §5 的判据必须复核。
