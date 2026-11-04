# LEVEE 项目现状与下一步规划（2026-11-01）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 现状盘点 + 评估 + 规划 |
| 基线 | `origin/master` @ `482af47`（v1.24.0 已切版，2026-10-31） |
| 撰写 | agent `mcode-status`（留言板条目 `mcode-status.md`，工作树 `F:/Nexus/levee-wt-ms1`） |
| 取证口径 | 每条结论附 `file:line` 或本机实测输出。**不采信台账行文本身**——本轮复核出的 5 处漂移全部是台账写错、代码已做的方向 |
| 适用范围 | 面向下一个接手本仓的 agent 与仓库的人 |

---

## 1. 一句话结论

**代码层对既定定位已经闭环；现在卡住项目的是签单，不是功能。** 剩余待办里只有一项是中量代码工作（D-1c 插件一插件多类型），其余要么等外部凭据、要么等复现、要么等产品决定、要么要真人录素材。

---

## 2. 仓库现状

### 2.1 谱系

| 项 | 事实 |
| --- | --- |
| 上游 | `origin/master` @ `482af47`（v1.24.0 已切版：tag `v1.24.0`、GitHub Release 7 个资产、镜像 `ghcr.io/levango7/levee:v1.24.0` 多架构） |
| `F:\Nexus\levee` | 原来是**别人的 checkout**，停在 `docs/restore-213-changelog`（2026-10-26）。该分支的 CHANGELOG 恢复条目**已在 master 的 v1.23.0 段**（`origin/master:CHANGELOG.md:270`，落在 `## [v1.23.0]`（209 行）与 `## [v1.22.0]`（618 行）之间）——**本轮已回收**：切回 master、删本地与远端同名分支 |
| 推送通道 | https 直连 github **不通**（实测 `Recv failure: Connection was reset`）；必须走 `ssh://git@ssh.github.com:443/Levango7/Levee`。ssh 也时通时断（实测一次 21s 超时） |
| 本地是浅克隆 | `.git/shallow` 存在 |

### 2.1b 本机构建现状（一个会挡住所有人的坑）

`origin/master` 的 `go.mod` 要求 **`go 1.26.10`**，而本机装的是 **go1.26.6**。`GOTOOLCHAIN=auto` 会去下载 `go1.26.10`，但本机 `go env` 里 **`GOSUMDB=off`**——而工具链下载**必须**经校验数据库确认，`GOSUMDB=off` 会让它直接失败：

```
go: golang.org/toolchain@v0.0.1-go1.26.10.windows-amd64: verifying module: checksum database disabled by GOSUMDB=off
```

**现象是任何 `go` 命令都报这一行**，包括 `go version`（在仓库目录里）。**解法**（本轮实测可用，并已**在本机修好**）：

```powershell
go env -w GOSUMDB=sum.golang.org     # 机器级、一次；改完 go version 直接报 1.26.10
```

`GOPROXY` 已是 `https://goproxy.cn,direct`，goproxy.cn 的 sumdb 代理可用，工具链能下下来并缓存。**`GOPRIVATE` / `GONOSUMDB` 对工具链无效**——Go 的工具链路径不接受豁免（两者都实测过，仍报同一错），只能给一个真实 sumdb。

附带的好处：`GOSUMDB=off` 原本意味着普通依赖**只信 go.sum、不查校验库**；改成 `sum.golang.org` 后依赖也纳入校验库验证（经 goproxy.cn 的 sumdb 代理），这是一次安全侧的加固而不是纯粹绕过。

**已同步写入 `AGENTS.md` 的「本机上已经付过代价的几件事」**，让别的机器上的 agent 也少踩一次（同一笔提交 `ec06e30`）。

> 补一句来由：这条**原本**是打算只写进本文、不动 `AGENTS.md` 的——理由是 `AGENTS.md` 是给所有 agent 看的公共契约，改动宜由仓库的人拍板。后来仓库的人明确让「解决一下配置」，就顺势补进去了；`AGENTS.md` 的两段写法与本文一致（机器级 `go env -w`，不是每次设环境变量），免得下一个 agent 以为自己得记得设变量。

### 2.2 两个会误导人的信号（先知道再用这些命令）

1. **浅克隆让 `git merge-base` 在边界处失效。** 本地 `git rev-list --count origin/master..HEAD` 报「领先 716」，`HEAD..origin/master` 报「落后 17」，且 `git merge-base` 返回空——这是**克隆假象，不是真分叉**。判定谱系前先 `git fetch --unshallow`，否则任何基于祖先关系的结论都不成立。
2. **`git branch --no-merged` 在本仓是假阴性判据。** 本仓自 2026-10-17 起一律 `gh pr merge --rebase` 落地（`CONTRIBUTING.md` §5.4），rebase 会重写 sha，PR 侧的 sha 不在 master 谱系上。`CONTRIBUTING.md:259-264` 自己就写了这条：「rebase 重写 sha，这才是新的坑…SHA 判据在 merge-commit 时代是真判据，rebase 之后会**假阴性**」。本轮实测：**24 条 `origin/*` 远端分支全部显示"未合并"，其中绝大多数是已落地但 sha 不同的重复品**（另 8 条 `ssh/*` 是同名远端的重复引用）。

**⇒ 判断「有没有落地」的唯一可靠口径：PR 状态 + CHANGELOG + `docs/blocked-items.md` + 内容比对（`git diff <旧sha> <master上的对应sha> -- <该笔触及的文件>`），不是分支是否 merged。**

---

## 3. 留言板现状

板目录：`F:\Nexus\agent-board\levee`（由 `-dir` / `LEVEE_AGENT_BOARD_DIR` 给出，无默认值——刻意如此）。校验器 `internal/agentboard`。

### 3.1 本轮开始时的声明

| 条目 | 分支 | 校验器判定 | 复核结论 |
| --- | --- | --- | --- |
| `qoder-a26` | `fix/release-gate-g06-exit-status` | **有效**（至 16:54） | 工作树今早 08:59 仍在动；PR #262 已开、**CI 29 条 check 全绿**（另 1 条 Macroscope 为 `skipping`） |
| `qoder-cn-drift` | `feat/drift-baseline-named-versions` | **过期 6h18m** | 工作树今早 08:03 仍在动，**但该分支内容已 100% 落地 master**（`25e2e64` ≡ `82b57d8`、`4b22e22` ≡ `10fec0f`；限定两笔触及的文件比对，唯一差异是 master 更新的 v1.24.0 切版状态行）→ 它是已落地工作的重复品，不需要 PR |

另有一笔**板上无声明**的在途工作：PR #263（`fix/alert-sse-route-guard-and-ledger`，2026-11-01 04:51 开），worktree 已不在 `git worktree list`（`AGENTS.md` 记的"工作树被并行会话批量清理"确实发生过），但 PR 在远端、CI 正常跑着，工作没丢。

### 3.2 本轮做的板卫生

- 删掉过期条目 `qoder-cn-drift.md`（`docs/agent-board.md` 明确：过期条目按定义不再断言任何东西，删除不需要征得原作者同意）。
- 移除工作树 `F:/Nexus/levee-wt-qcn-drift`（**删前实测**：工作树干净、无 stash、tip 已是落地内容）。
- 删除本地与远端分支 `feat/drift-baseline-named-versions`（远端推送删除已回 `[deleted]`）。
- 校验器复跑：**`board is honest`，exit 0**（此前 exit 1，报那条过期声明）。

### 3.3 板卫生的一条观察

**板子能报出冲突，但不能自动发现「有人没写声明」。** PR #263 就是板上无声明的在途工作。板机制对"两条都声明了但重叠"有效，对"只有一条声明"和"谁都没声明"无效——后者只能靠人核对 PR 列表。若要补齐，只能靠约定（开 PR 前必须先写板条目）或机制（PR 模板要求填板条目路径），本轮未做。

---

## 4. 定位与市场定位

### 4.1 当前定案（`docs/product-positioning.md` v1.1）

> **LEVEE 是一套高危变更治理系统**：把一个「改一台主机/数据库/网络设备/中间件」从手工命令升级为**计划 → 审批 → 分批执行 → 验证门禁 → 自动回滚 → 审计留痕** 的受治理闭环，默认无代理、CLI 优先。

| 维度 | 归属 |
| --- | --- |
| 是 | 面向生产变更的**治理控制平面**：状态机 + 版本绑定的审批 + 执行证据账本 + 补偿幂等性 + 不可变审计链 + 集群故障接管 |
| 不是 | generic workflow / DAG 调度器（workflow 编排只是内部子系统，约占 `internal/` 三成） |
| 不是 | CMDB / 日志平台 / 告警引擎 / Agent 常驻纳管平台——**那些归 OpsMesh** |
| 不是 | ArgoCD / Flux 的替代：集群内声明式交付归它们，集群外命令式高危变更归 LEVEE |

一句话：**workflow 是 LEVEE 的内部实现，Change 才是它的公开主语**（60 个 RPC 里 23 个挂在 `ChangeService`，十个已注册服务中最大——数字已于 2026-11-01 按当前 `proto/` 订正，见 §7 第 5 条）。

### 4.2 对定位的评估

**定位本身是清楚、自洽、且有边界的**——"它不是什么"写得比"它是什么"更具体，这与本仓一贯的"把边界写死"的风格一致；`levee schedule` 定档不做（定位明言"不是调度器"，触发归平台，治理那一半即 `window` + `calendar`）就是这条定位的一次实战。

**两处值得补强的地方**：

1. **定位与"签单"之间的那一步还没走完。** 定位讲的是产品是什么，没有讲**它替代谁、成本差在哪、谁先买**。`docs/product-roadmap.md` 的运营层三项（demo 库、角色视频、品牌资产）正是在补这一步，但三项目前都需要人手——这恰恰是 §6 结论的根因。
2. **与 OpsMesh 的边界是口头约定，不是产品机制。** 文档写"日常自动化走 OpsMesh，动生产数据的变更走 LEVEE"。但两者的**分工点在数据上**：谁来判断一次动作算"日常"还是"高危"？目前这由操作员在选工具时人工划分，没有任何一方在运行时拒绝越界的动作。若目标客户同时买这两套，这是最可能被现场问到的问题。

---

## 5. 规划现状

### 5.1 四层路线图（`docs/product-roadmap.md`）

代码层 / 可视化层 / 交互层三层的条目**几乎全部标 ✅**（本轮按"行首是否带 ✅"扫过全表）。剩下的多是已落地后的收尾行与少量 P2。运营层三项**全未开工**（要人手）。

### 5.2 搁置项六类（`docs/blocked-items.md`，2026-10-26 定版 / 10-28 对账 / 10-29 增设 F 节）

| 类 | 项 | 谁能解 |
| --- | --- | --- |
| A 外部凭据/环境 | A-1 ChatOps 真机验收、A-2 SSO/OIDC 真实启用、A-3 移动端原生推送、A-4 L0 四环境安装演练、A-5 租约锁生产浸泡 | **部署侧给资源**，代码零阻碍 |
| B 工作所有权 | **当前无**（原 B-1 Firefox 矩阵已随 PR #208 合入） | — |
| C 未复现/无数据 | C-1 Raft/真故障转移（等 A-5）、C-2 windows calendar `0xc0000005`、C-3 browser-check windows 模式 A | **等触发** |
| D 工作量/立项 | D-1c 插件一插件多类型（中）、D-2 运营层资产（需人手，**账本自称"面向客户签单的最大缺口"**） | 代码侧 / 人手 |
| E 产品决策 | E-1 双语（定档不做）、E-2 按需项（等真实消费方）、E-4 `levee schedule`（定档不做） | 明确等触发 |
| F 上游发布 | F-1 TypeScript 5.9→7（上游 `vue-tsc` 无 4.x，本仓已钉 `^5.9.3`） | 不是本仓工作量 |

**唯一真正未开工的代码项是 D-1c**（中量，插件通道一插件多类型）。

---

## 6. 评估：约束已经不在代码上

把上面的清单按"谁能让它动"排一遍，结论是清楚的：

| 剩余项 | 性质 | 缺的是什么 |
| --- | --- | --- |
| A 组 5 项 | **部署侧** | secret / 云账号 / 真实多节点环境 |
| D-2 运营层资产 | **人** | 真人录制与设计 |
| C 组 3 项 | **时间** | 复现或浸泡数据 |
| E 组 3 项 | **决策（已定档不做）** | — |
| F-1 | **上游** | `vue-tsc` 4.x |
| D-1c | **代码（中）** | 真消费方出现再排 |

**⇒ 继续加代码功能的边际收益，低于把上面第一、二行补上。** 现在最值得投入的是「让这套东西能被签出去」，而不是「让它多一个功能」。

需要说清的边界：本仓自己的门禁仍很强（本轮在 `origin/master` @ `482af47` 上实跑：`go build ./...` exit 0 · `go vet ./...` exit 0 · `go test ./... -count=1` **72 包（68 ok / 4 个无测试文件）0 FAIL** · `golangci-lint run ./...` **0 issues** 且 rc=0；CI 侧另有 govulncheck / gosec / trivy / CodeQL / 4 腿 browser-check / `landing shape` 全绿），**这不是"功能做完了所以该去做市场"的局面，而是"代码侧确实到了这一轮的边界"**——两者不能混为一谈。

**本轮还把一直挂着的 PG 盲区补上了**（见 §9）：本机 `postgres:16-alpine` 容器起在 `localhost:25433`，设 `LEVEE_PG_TEST_DSN` 后 `-p 1` 串行跑，**`internal/state` 412 条用例全 PASS、0 SKIP**（此前无 DSN 时 28 条 PG 用例是 skip 的），`internal/cluster` / `internal/takeover` / `internal/calendar` / `internal/backup` / `cmd/levee` / `tests/integration` 六个包也全绿——其中 `internal/state` 的 `TestPGStore_AuditChainSealAndWORM` 与 `TestPGStore_ListAuditChainPage_OrderCursorAndTenant` 是**在真实 PostgreSQL 16.15 上**跑的 audit WORM 触发器与全局链。

---

## 7. 本轮复核出的 5 处台账漂移（已修）

台账说"未做"而代码已做，下一个 agent 会照着它排期去做重复劳动——这与 SA-007「虚假锚点」是同款失败模式，而 `docs/product-roadmap.md` 自己就写着「**一行一主题**的账本里，同一个结论写两遍就会漏改一半」。本轮逐条 grep 复核出 5 处：

| # | 位置 | 台账原话 | 事实 | 修法 |
| --- | --- | --- | --- | --- |
| 1 | `product-roadmap.md` | ABAC 行标题「阶段 0–2 已完成，**阶段 3+ 待做**」 | 正文显示阶段 3/4/5/6/7 全落地（阶段 7 告警接入 2026-10-19，例外表随之清空） | 标题改为「阶段 0–7 已完成；残留见本行末」 |
| 2 | `product-roadmap.md` | 「清单与 labels 不一致…**未加一致性审计**——那是下一笔的候选」 | `internal/inventory/ownership.go:157` 的 `inventory.LabelConflicts` 逐行比对 registry 与存储 label；`cmd/levee/cmd_serve.go:1370` 在 serve 启动时对每处冲突 WARN，冲突时 registry 压过 labels | 改写为已落地并给出 file:line；**同时把真正残留写准**：写入路径（`ImportTargets` / `AddTarget`）仍只看 labels、不查 registry，故不一致只能被报出来、不能被拒掉 |
| 3 | `product-roadmap.md` | 「`human` 门禁的 approver 传输**仍未实现**（改为 plan 期拒绝）」 | `internal/approval/gate_approver.go` 实现 `verify.HumanApprover`；`cmd/levee/cmd_serve.go:1236` 经 `wiring.WithGateApprover` 装进 `buildEngineServeOptions`，其生产调用点 `cmd/levee/cmd_serve.go:1473` | 标题改为「✅ 已落地（v1.20.0，#84）」；正文的「全仓实现数 = 0」标注为登记当时的事实并给出订正 |
| 4 | `product-roadmap.md` | 「组织级变更日历至今**整条链路无人消费**」及其 ①②③④ 四小项 | ① `cmd/levee/cmd_serve.go:51` import `internal/calendar`，`:1203` 的 `calendarFor` 按 store 类型选方言，`:1207` 装 `wiring.WithChangeCalendar`；② 日历表已有 PG 形状；③ 判定函数的生产调用点在 `internal/wiring/calendar_gate.go:95/107/162`，`internal/wiring/calendar_gate_test.go:340` 的 `TestCalendarGateIsForwardOnly` 钉住回滚豁免；④ 冻结语义进了 `docs/leveelang-spec.md` §4.2b | 四小项逐条改写为已落地并附证据 |
| 5 | `product-positioning.md` | 「53 个 RPC（`levee.proto` 40 个 + `levee_extra.proto` 13 个）…**22 个**挂在 `ChangeService`，是**九个**已注册服务中最大的一个」 | 实测 `proto/`：**60 个**（`levee.proto` **41** + `levee_extra.proto` **19**），`ChangeService` **23 个**，服务 **10 个**（`AgentService` 是之后加的）；生产注册点 10 处（`internal/grpc/server.go:292/305/306/307/308` 五个 + `cmd/levee/cmd_serve.go:949` 一个 + `internal/grpc/extra_services.go:60/66/72/78` 四个） | 三个数字全部按实测订正，并标注订正日期与来源 |

`docs/security-audit.md` 与 `docs/blocked-items.md` 无同类陈述——漂移集中在 `product-roadmap.md`（4 处）与 `product-positioning.md`（1 处）。

**第 3 处是这批里最刺眼的一条**：同一行的正文写着「已落地（2026-10-07）」「该项已落地（v1.20.0，#84）…原登记的「仍未做」是过期信息，别再照着它排期」，而**标题还写着「仍未实现」**。台账自己已经警告过这种写法，结果自己踩了一遍。

另记一条编号撞车（未重命名，只消歧）：**「D-2」指两件事**——`docs/blocked-items.md` 的 D-2 是「运营层资产（需人手）」，`CHANGELOG.md` `[Unreleased]` 的 D-2 是「目标组边界检查」（`feat/d2-target-caps`）。已在 `docs/blocked-items.md` 的 D 节加消歧注记，**没有改名**——「D-2 是最大签单缺口」这句话是仓库的人正在用的引用，改名会打断它。

---

## 8. 下一步任务规划

### 8.1 立刻可做，不需要新决策

| 项 | 内容 | 备注 |
| --- | --- | --- |
| PR #262 | `qoder-a26` 的 G-06 修复 → **rebase 到新 master 后合入** | 原 PR 与 master 冲突（双方都在 `CHANGELOG.md` 的 `[Unreleased]` 顶部追加 `###` 小节）；解法是**两侧小节全保留**、G-06 置顶。它的板声明 16:54 到期；合入权在仓库的人手里 |
| PR #263 | 告警 SSE 路由守卫 → **评审并合入** | 板上无声明——合入前顺手问一句作者。它能干净地 rebase 到本 PR 之上（已实测） |
| 台账 5 处漂移 | 本轮已改（§7） | 与本文同笔 PR |
| 回收 checkout | `F:\Nexus\levee` 停在已落地内容上 → **本轮已回收**：切回 master、删本地与远端陈旧分支 | 删前实测：工作树干净、无 stash、无进程占用、分支内容逐行已在 master |

### 8.2 需要人（**这是当前最大的杠杆**）

| 项 | 内容 | 依据 |
| --- | --- | --- |
| **D-2 运营层资产** | demo 库（MySQL 主从切换 / nginx 灰度 / web 配置回滚 / 集群接管）、角色视频（审批人 90s / 发起人 120s / 补救 90s）、品牌资产（堤坝符号 + 状态色板 + GIF） | `docs/blocked-items.md` 自称「面向客户签单的最大缺口，建议优先安排人手」；`docs/product-roadmap.md` 运营层三项全未开工 |
| A-1 / A-2 | ChatOps 平台 secret、真实 IdP / GitHub OAuth App | 代码全链路已备，填配置即可端到端 |
| A-4 / A-5 | 云账号或信创硬件；一个真实多节点部署跑 4–6 周 | A-5 是 C-1（Raft）的输入 |

> **指针（2026-11-01 后续推进，不改写本快照的结论）**：D-2 里**可由 agent 完成的那一半**
> ——demo 库的**场景脚本**——已经落地：四个场景各有可跑工作流与演示索引
> （`deploy/lab/DEMOS.md` + `deploy/lab/workflows/`），并由 CI 守卫强制索引与文件双向对齐。
> 上面这一行里**仍然需要人手的**是录像与品牌资产（拍摄与设计），不是脚本。
> 上表依据列里「运营层三项全未开工」是快照原文——现在 demo 库一项已落地，另两项仍未开工。
>
> **实测边界订正（2026-11-02）**：上一段原写的「执行路径与快照回滚未在本机实跑，只验证到计划层」
> **已不成立**——执行路径与快照采集/还原**已在本地容器靶场实跑**（3 台 alpine+sshd+nginx 容器
> 顶 VM，配方与证据见 `deploy/lab/container-lab/`）。**仍未实跑**的是：真 VM 实验室的
> systemd 服务语义、MySQL 主从切换（A1）、PG 集群接管（A4）。
> 口径与实跑才知道的操作事实见 `deploy/lab/DEMOS.md` §4 与 §4.1。


### 8.3 需要产品决定

| 项 | 问题 |
| --- | --- |
| `service_factory` 接线还是退役 | `cmd/levee/service_factory.go` 609 行，非测试代码零调用、只有 `cmd/levee/grpc_client_test.go` 在用——**测试在维护一条没人走的路径**。接线还是退役属决定（`docs/product-roadmap.md` P2 行、`docs/blocked-items.md`「维持现状」） |
| 清单写侧是否按 registry 强制 | 现在 registry 只压过**读侧**判定，写入路径不看它；不一致只被启动 WARN 报出来。改成写侧强制，需要先定"registry 与 labels 冲突时谁说了算"的处置口径（拒绝改行？还是以 registry 为准改写行？） |
| 与 OpsMesh 的运行时边界 | §4.2 的问题：谁来在运行时拒绝越界动作 |
| `docs/levee-api.md` §6 的 drift 命令 | 文档写 `levee drift scan` / `levee drift show`，实际注册的是 `drift detect` / `drift baseline show`。**已登记为「不修，改法需单独定夺」**（API 参考文档的口径问题，不是本笔键控改动）；本轮未改，因为 drift 文档此刻正在被别的分支改动 |

### 8.4 机制建议（可选，非当前必需）

- **把「台账声称未做/已做」变成可机器校验的东西。** 本轮 5 处漂移全部是同一种形状，且**没有任何 CI 能发现它**——CI 能校验代码，不能校验"账本说的事与代码一致"。一个低成本的形态：给 `docs/product-roadmap.md` 的每条"已落地/仍未做"断言配一个 `file:line` 锚点，由一个测试检查锚点存在且该处含关键字。**本轮未做**——它要动的是 CI 与全表，属于独立立项。
- **板机制加一条"开 PR 前必须先有板条目"的约定。** §3.3 的观察：板能报重叠，不能发现漏声明。

---

## 9. 本文的验证盲区（照实登记）

0. **本轮实跑过的**（`origin/master` @ `482af47`，需先按 §2.1b 设 `GOSUMDB`）：`go build ./...` exit 0 · `go vet ./...` exit 0 · `go test ./... -count=1` 72 包 0 FAIL（68 ok / 4 无测试文件）· `golangci-lint run ./...` **0 issues** 且 rc=0 · `python scripts/test_changelog_structure.py` 7 tests OK · `python scripts/test_landing_shape_step.py` 11 tests OK · 留言板校验器实跑两次（清理前 exit 1、清理后 exit 0）· `git worktree`/分支/远端删除的每一步都有回显。**§7 的五处漂移是读代码 + grep 复核**，不是行为复现。
1. **PostgreSQL 侧本轮已实跑，不再是盲区。** 起 `postgres:16-alpine` 容器（`localhost:25433`，凭据 `levee`/`levee-ci`/库 `levee_test`），设 `LEVEE_PG_TEST_DSN` 后按 `AGENTS.md` 的要求 `-p 1` 串行跑：`internal/state` **412 条 PASS / 0 SKIP**，`internal/cluster` / `internal/takeover` / `internal/calendar` / `internal/backup` / `cmd/levee` / `tests/integration` 全绿。**§7 第 2、4 项涉及 PG 行为的部分（calendar 的 PG 方言、audit 链的 PG 触发器）因此有了真机依据**——但其余 CI 专有的组合（PG 与集群、PG 与备份的联合腿）仍只由 CI 承担。
2. **§7 的五处漂移仍是「读代码 + grep 复核」**（除第 1 条 PG 部分已实跑外）。第 2、4 项虽在本机 PG 上有相关用例通过，但那些用例验的是 WORM 触发器与链的**行为**，不是"台账那句话现在对不对"——后者只能靠比对代码与文档，仍未变。
3. **PR #263 的 CI 在本文写作时仍在跑**，结论只到"已开、CI 正常触发"，未到"全绿"。
4. **本文基于 `origin/master` @ `482af47`**，master 后续前进后需重新核对——尤其 §5.1 的"几乎全部 ✅"是按该 commit 数的。**注意本文自己就是一次教训**：写作时我按十月的认知描述了 audit 链的封链行为，而上游早已把它改成 keyset 分页（`internal/audit/auditchain.go:137-139`）、`Seal` 增加"内容被改即拒绝重链"（`:184`/`:226` 的 `ErrChainBroken`）、并留了 `Rebuild` 管理口（`:247`）。**本文正文并没有断言封链语义**（唯一涉及 audit 链的表述是 §6 与 §7 第 4 项提到的 PG WORM 触发器用例名），所以正文无需改动——但这一条要记下来：**写作时的认知会过期，本文引用 `482af47` 的每处行号都需要按该 commit 复核**。顺带记一条**上游尚未解决**的问题：`walkChain` 的读放大仍在（`auditchain.go:138` 自述"sealing still walks every visible row, and Record calls it per write"），分页没有去掉"每次写入都全量走一遍"的成本。
5. **本地是浅克隆**，本文未做 `git fetch --unshallow`（会拉全量历史、耗时不可控）；§2.2 的谱系判断依据是内容比对与 `CONTRIBUTING.md` 自述，不是祖先关系。

---

## 10. 2026-11-03 复核（追加，不改写上面任何一节）

**基线推进**：本文正文的基线是 `origin/master @ 482af47`（§9.4 自己写了"master 后续前进后需重新核对"）。本轮复核时的 master 是 `9974b44`，**前进 21 笔**。

> 本节所有数字都是在 `9974b44` 上跑出来的。本 PR 在 rebase 时 master 又前进一笔（`e6f6f34`，`--remote` 帮助文本单一来源化 + 一条新守卫测试），**那一笔之后没有重跑**：它不改包数、不改门禁项、也不动本节引用的任何 `file:line`，所以数字仍属 `9974b44`；下一轮复核应以 `e6f6f34` 为新基线重测。上面各节凡是按 `482af47` 写的结论，本轮重新核了一遍，核出的两处需要限定的表述见"要限定/收回的表述"。

### 复测结果（命令 + 实测，不是引用）

| 项 | 命令 | 实测 | 对本文的影响 |
| --- | --- | --- | --- |
| 全量测试 | `go test -count=1 ./...`（`GOSUMDB=sum.golang.org`，见 §2.1b） | rc=**0**；**72** 个包 = 68 `ok` + 4 `[no test files]`；`^FAIL` 行 **0** | §9.0 的自报数字**仍成立**，无需订正 |
| 墙钟大头 | 同上输出按包耗时排序 | `internal/grpc` **98.9s**、`cmd/levee` **82.7s**，其余均在 9s 以内 | 新增事实：全量的等待几乎全部来自这两个包，估 CI 排队时间时别按平均值摊 |
| RPC 计数 | `grep -c '^\s*rpc ' proto/levee.proto proto/levee_extra.proto` | **60** 个 RPC = `levee.proto` **41** + `levee_extra.proto` **19**；`^service` **10** 个；`ChangeService` **23** 个 | §7 第 5 条（那三个数字的订正）**独立复算一致**，仍成立 |
| 浅克隆 | `git rev-parse --is-shallow-repository` | 主检出与本工作树都是 **true** | §2.2 与 §9.5 的谱系类判断**前提未变** |
| 搁置项分类行数 | 逐类数 `docs/blocked-items.md` | A **5**、B **0**、C **3**、D 活跃 **2**（D-1c / D-2）、E **3**（三条都已定档）、F **1** | §5.2 那张表**仍准确** |
| 静态检查 | `golangci-lint run --timeout 9m ./...`（本机 v2.13.2，CI 钉 `version: v2.13`；私有 `TMP` 与 `GOLANGCI_LINT_CACHE` 避开同机并发锁） | rc=**0**、`0 issues.`（一次跑完，`--timeout` 未触发） | §9.0 的 lint 自报**复现**；注意本机补丁版 ≠ CI 钉版，结论仍以 CI 的 `lint` job 为权威 |
| PostgreSQL 腿 | `LEVEE_PG_TEST_DSN=… go test -count=1 -v ./internal/state/... ./internal/cluster/...`，容器 `qcn-recheck-pg`（`postgres:16-alpine`，`127.0.0.1:55442`） | rc=**0**；`internal/state` **单独** 412 个 PASS（顶层 217 + 子用例 195）、**0 SKIP**、0 FAIL，44.6s；再带 `internal/cluster` 时合计 462 个 PASS（顶层 267 + 子 195），cluster 9.5s | §9.1 那句"412 PASS / 0 SKIP"**逐位复现**（含它的计数口径：顶层 + 子用例），所以那条从"自报"升级成本轮实测 |

PG 这一趟特意**另起一个容器**而不是复用 §9.1 那台 `localhost:25433`：本机此刻跑着别人的一批容器（kind 集群、`levee-lab-1..3`），共用端口或共用库都会把别人的状态卷进我的结论。代价是这只是一份**空的新库**，覆盖的是同一套测试代码、不是同一份数据。

### 要限定/收回的表述

1. **§8.1 的 PR #262 与 #263 都已合入**，不再是待办：#262 → `03849c9`（G-06 判定修复）、#263 → `3386213`（告警 SSE 路由守卫）。该表两行保留原文，状态以本节为准。
2. **§5.2 末句"唯一真正未开工的代码项是 D-1c"要限定范围**。它在 `docs/blocked-items.md` 的 D 类里成立，但作为**全局陈述不成立**：`docs/product-roadmap.md` 第 27 行那条 P0（ABAC **写侧** env 归属强制）从未开工，而 `#272` 的 `--remote` 系列正在推进同一批文件。准确的措辞是"搁置项（blocked-items）里唯一未开工的代码项"。

### 本轮新增的可认领项（都是"要人表态"，不是"要人写码"）

- **ADR-008**（`docs/adr/ADR-008-abac-write-side-env-ownership.md`）把 §8.3 那一问「清单写侧是否按 registry 强制」写成四个候选 + 五个问题，并**收回了它的定级基础**：条件策略层的 labels 是用**被判定的那个 env** 现造的（`internal/authz/authz.go:435`），不读库存行 ⇒ 假 label 换不到额外许可，"dev 能接走 prod 机器"这种逃逸不成立；真实失真面是"门禁按自报 env 判定"（`internal/grpc/target_service.go:133`）与"清单行可对平台自己的权威事实说谎"。**所以这条还要不要挂 P0，是 owner 的决定，不是代码的决定。**
- §8.3 的 `docs/levee-api.md` 第 6 章那一问已进 `docs/product-roadmap.md`（P2，登记不修）：文档写的 `drift scan` / `drift show <report-id>` 实际不存在，而改法有两个相反方向（把文档改成现实 vs 真做这两条接口），需要定夺。

### 本轮的验证盲区（照实登记）

- 没跑前端腿（`web/` 的 vitest 与 `browser-check`），本轮零前端改动。
- 没跑全量 `-race`；只在 `internal/drift` 上跑过 `-race -count=2`（那是上一笔改动的验收，不在本轮）。
- PG 复测用的是新建空容器，与 §9.1 那台的凭据与库名不同：结论是"这套 PG 用例在 `9974b44` 上仍通过"，不是"那台库仍然好"。
- 浅克隆未 `--unshallow`，谱系类判断仍只能靠内容比对。

### 一条机制建议（不新增门禁，只提写法）

带日期的快照文档要**只追加、不重写**：每轮复核另起一节，写明"基线 sha + 复测日期 + 命令"，上面旧条目原文保留，失准处单列"要限定/收回的表述"。本节的写法就是这条建议的第一个样本——反过来做（直接改旧条目）会让"当时的判断"消失，而后一轮的人最需要的往往正是那个。

## 11. 2026-11-04 追加：`docs/levee-api.md` 第 6 章 drift 文档项收口

- **结论**：§8.3 与 §10 提到的「第 6 章写 `drift scan` / `drift show`」已在 2026-11-01 的文档订正中改为实际注册的 `drift detect` / `drift report`；2026-11-04 复核确认正文、选项表与 `cmd/levee/cmd_drift.go` 的 `Use:` 清单一致（`detect` :101、`report` :754）。
- **本次补的收尾**：`docs/review-report.md` 2026-08-15 的两处旧口径加注为历史记录；`docs/product-roadmap.md` 第 36 行从「登记不修」改为「已修」；`docs/mvp-plus1-roadmap.md` 的 F10 行把 `drift scan` 改为 `drift detect` / `drift report`；`docs/levee-api.md` 第 6 章补了 `drift report` 的 `--host` / `--days` 说明，并把 `drift baseline` / `drift schedule` 的完整旗标指向 `docs/cli-reference.md` 第 22 章（避免 API 参考与 CLI 参考各抄一份）。
- **仍未做**：`drift scan` 与 `drift show <report-id>` 没有实现；若将来要新增这两个接口，需要新的报告 ID 存储与读路径，另开产品/接口项，不在本次文档订正范围内。
- **验证**：`go test ./cmd/levee/ -run TestDocumentedFlagsAreRegistered -count=1`、`python scripts/check_doc_anchors.py`、`python scripts/test_changelog_structure.py` 均通过。

---

## 12. 2026-11-05 复核（追加，不改写 §1–§11）

**为什么又有这一节**：§10 结尾我自己写了"下一轮复核应以 `e6f6f34` 为新基线重测"。去兑的时候 master 已经是 `e848f79`——自 §10 的基线 `9974b44` 以来前进 **10** 笔（其中三笔是 §10 自己那批：`65bdf8e` / `4a35a4d` / `274fcda`；另有 `1d8e511` 把 API 参考纳入 CLI 示例守卫并订正 20 处假命令、`e848f79` 收口 `levee-api` 第 6 章并加注 §11）。所以本轮基线是 **`e848f79`**（本笔最终落在 `4c0b42db` 之上 = 自 `9974b44` 起 **15** 笔；测量两次：`8ab41428` 时 11 笔、`4c0b42db` 时 15 笔，`git rev-list --count`）。

> 和 §10 一模一样的情形又发生了一次，而且这次连发生了**两轮**：本 PR 第一次 rebase 时 master 前进一笔（`8ab41428`，pi 的旗标守卫扩面），第二次 rebase 时又前进五笔（base = `4c0b42db`，含 ADR-008 写侧实现与 #280 落地），**这两轮之后我都没有重跑全量 / lint / PG**——下面那张测量表里的 72 包、412+50 个 PG PASS、60 个 RPC、lint `0 issues.` 仍属 `e848f79`，下一轮复核请以当时的 live master 为准，别拿本节当"最新"。
>
> 但**指向别人的坐标必须按 base 复算**，否则一落地就是假话：推送前我在 `8ab41428` 上逐条重读了本节引用的 `file:line` 与 PR 状态，改四处——① 旗标守卫的行号 `:49`/`:65` → `:54`/`:73`；② "identity-deployment.md 正在另一条线上接" → 已落在 base；③ "`#272` 还没进 master" → **错了**，`74653e18` 是本基线的祖先（不过它新增 RPC 为 0，计数不受影响）；④ 板面成员与派单号那两条按复读时刻改写。这四条都不是墙钟类测量，重读即可判定，所以不受上一句"没有重跑"的限制。

### 第二轮 rebase（base `4c0b42db`）后的复算：可机器判的那批全部未变

第二轮把 base 又推了 4 笔（`91a7509d` 测试选址、`7c191fc7` = PR #280、`3be8895` ADR-008 写侧实现、`4c0b42db` 其 CHANGELOG 补节），**这批改动碰了产品代码与 proto**，所以本节所有可机器复算的计数在新 base 上重跑一遍：

| 计数 | `e848f79` | `4c0b42db` | 判据 |
| --- | --- | --- | --- |
| `proto/levee.proto` RPC | 41 | **41** | `grep -c '^\s*rpc '`（`3be8895` 只动 `levee_extra.proto`） |
| `proto/levee_extra.proto` RPC | 19 | **19** | 同上——它 +6 行是 `allow_env_conflict` 的 message 字段，不是新 RPC |
| `^service` | 5 + 5 = 10 | **5 + 5 = 10** | 同上 |
| `blocked-items` 分类 | A5 / B0 / C3 / D3 / E3 / F1 | **同** | 按 `^| [A-F]-\d` 行计数 |
| roadmap `^\| P` 行 | 47 | **47** | 本节"未决行数"那张表的分母 |
| roadmap 被引行号 | :26/:28/:29/:30/:31/:32/:36/:43/:46/:80/:96–:98 | **逐行回读一致** | 每条按内容核对（删除线 / 优先级格 `✅` / 尾列 `✅` / 「已修」/ 两列表） |
| 旗标守卫锚点 | :54/:55/:61/:73/:80 | **同** | `git diff --name-only 8ab4142 4c0b42d` 不含 `cli_reference_flags_test.go` |

墙钟类（72 包 / 412+50 PG PASS / lint `0 issues.`）**没有**重跑——那要四十分钟以上，而本笔零产品代码改动，它们的归属仍按上面那条按语算作 `e848f79`。

### 复测（命令 + 实测，本轮全部重跑）

| 项 | 命令 | 实测 | 与 §10 的差 |
| --- | --- | --- | --- |
| 全量测试 | `go test -count=1 ./...` | rc=**0**；**72** 个包 = 68 `ok` + 4 `[no test files]`；`^FAIL` 行 **0** | 包数与结论**不变**（§10 也是 68 ok / 0 FAIL） |
| 静态检查 | `golangci-lint run --timeout 9m ./...`（v2.13.2，私有 `TMP`/`GOLANGCI_LINT_CACHE`） | rc=**0**、`0 issues.` | 不变 |
| PostgreSQL 腿 | 新建容器 `qcn12-pg`（`postgres:16-alpine`，`127.0.0.1:55443`），`LEVEE_PG_TEST_DSN` 指向它 | `internal/state`：**412** 个 PASS（顶层 217 + 子用例 195）、**0 SKIP**、0 FAIL，48.1s；`internal/cluster`：50 个 PASS、0 SKIP、0 FAIL，8.2s ⇒ 合计 **462** | 与 §10 逐项相同 ⇒ state/cluster 的用例数这半月**一格未动**（这本身值得记：这两块没被测面扩张） |
| proto 计数 | `grep -c '^\s*rpc ' proto/*.proto` | **60** = `levee.proto` **41** + `levee_extra.proto` **19**；`^service` **10** | 不变。**收回本节初稿的一条**：初稿写"`#272` 那条链在扩 proto，但还没进 master"——按 base 复算，#272 的合并 commit `74653e18`（2026-11-03T23:13Z）**已是 `e848f79` 的祖先**，"还没进"是错的；但它对 `proto/levee.proto` 的 54 行改动**新增 RPC 数为 0**（`git show 74653e18 -- proto/levee.proto | grep -c '^+.*rpc '` = 0，改的是 message 字段），所以 41/19/60 这三个数不受它影响，也不因它而过期 |
| 搁置项分类 | 数 `docs/blocked-items.md` 各节表行 | A **5**、B **0**、C **3**、D **3**（其中 D-1b 已交付 ⇒ 活跃 2）、E **3**（全部已定档）、F **1** | 不变 |
| 守卫扫描面 | 读 `cmd/levee/cli_reference_flags_test.go`（**按本笔 base `8ab41428` 重读**） | 扫描面 **5** 本文档：`docs/cli-reference.md`（:54）、`quickstart.md`（:55）、`deployment.md`（:61）、`docs/levee-api.md`（:73）、`docs/identity-deployment.md`（:80） | **比 §10 多两本**。本节初稿写的是 `e848f79` 上的 `:49`/`:65` 与"`identity-deployment.md` 正在另一条线上接"——推送前按 base 复算时两处都已过期（行号漂到 :54/:73，那一笔已落在 base）。这是 §7 那类"文档说假话"从被查得到查不到的边界 |

### 一条挡住本轮落地的环境事实：macOS **arm64** 池容量（本轮实测，非转述）

- **现象**：#282 自己那笔 run `41874663901` 与 master 的 `41885083482` 都变红，但**除聚合器外只有一条 macos 腿不是 success**——前者 22 条 success + `build (macos-latest/arm64)` cancelled + `check (all jobs passed)` failure，后者同一形态（`test (macos-latest)` cancelled）。
- **根因有出处**：run `41885083482` 的 job `126767273532` 是 `conclusion=cancelled` 且 **`runner=null`**（从未被任何 runner 领取），注解原文 `The job was not acquired by Runner of type hosted even after multiple attempts`，同批还有 GitHub 自己的公告 `Due to capacity constraints, jobs targeting macOS arm64 runners may experience longer queue times.` ⇒ **不是内容红，是池子没接活**；聚合器把 cancelled 计成 failure，所以一条基础设施抖动会伪装成门禁红。
- **恢复判据**：本轮挂一个只读看守，每 600s 采最近若干笔已完成 run 的 macos 腿，判据是"有 macos 腿且零 cancelled"。实测 18:57Z 那一轮 8 条腿里 3 条 cancelled；21:53Z 最近 3 笔的 9 条腿全 success ⇒ 判恢复后才推送。
- **省事的办法不是重跑整条流水线**：先取证（`runner` 是不是 null、注解原文说什么），再决定**等池子缓过来**还是用 `gh run rerun --failed` 只重放失败腿——上一批 #274 那笔就是靠后者从 attempt 3 转绿的（run `41481819152`，`attempt=3`、`conclusion=success` 实测）。本轮没有需要重放的内容红，所以等恢复后一次性推送。

### 要收回的一条：§10 的"墙钟大头"排法本身是错的

§10 的"墙钟大头"那行（`docs/project-status-2026-11.md:245`）实测格写的是：「`internal/grpc` **98.9s**、`cmd/levee` **82.7s**，其余均在 9s 以内」，并据此下了结论「全量的等待几乎全部来自这两个包」。**两个数值是真的，"其余均在 9s 以内"是我排序方法错的产物**：我用 `sort -r` 对 `go test` 输出的时长列做**字符串**倒序，于是 `"9.4s"` 会排在 `"98.9s"` 前面、`"8.6s"` 又能排到最顶上。按数值排序的真实前三是：

- `cmd/levee` **118.4s**
- `internal/grpc` **113.0s**
- `internal/credential` **66.4s** ← 这条在 §10 的说法里根本不该存在，而它单独占了全量墙钟（各包之和 **664.4s**）的十分之一

它慢有来由：该包的密钥派生走 argon2id（`internal/credential/store.go:7`），测试要付真实 KDF 成本——但这条我只到"包级 66.4s + 代码里是 argon2id"为止，**没有逐用例计时**，要把成本归到具体测试得再跑一次带计时的 `-v`。**订正只落在本节**：§10 原文不改，按本文的追加式口径，收回原因写在这里——教训是"给计数文档定排名时，排序必须按数值，且断言'其余都在 X 以内'要能自己复算"。

### 一个新发现：`roadmap` 的"未决行数"当前不可机器复算

同一份 `docs/product-roadmap.md`（**47** 行表），三种都能跑通的判据给出三个数：

| 判据 | 判为未决 |
| --- | --- |
| A：优先级格含闭合标记（`✅`/已落地/已完成/已定案/不做/维持现状/登记不修/已修…） | **11** |
| B：任一格含上述标记 | **1** |
| C：B 再加"标题整条 `~~划掉~~` 视为已闭" | **1** |
| 人工逐行回读 | **6** |

差异不是理解问题，是**闭合状态有四种互不兼容的写法**：`roadmap.md:32` 把 `✅ 已落地（2026-10-12，PR #137）` 写在优先级格；`:43`/`:46` 把 `✅ 已落地` 写在**尾列**；`:26`/`:28`/`:29`/`:30`/`:31`/`:80` 只在标题上打删除线（部分同时在他格带标记）；`:36` 用「已修」——而「已修」不在任何词表里（它是 §11 刚把"登记不修"改过来的）。**最严重的一条不是判据分歧，是结构性漏项**：运营层那张表的行首根本不是 `| P<n>`（`:96`–`:98` 是 `| 项 | 目标 |` 两列），所以任何 `^| P` 统计**整类看不见这三行未决项**。本轮我自己就先按"6 行"报了，人工回读才发现其中三行从来不在统计面上。

按本文的追加口径，这条只登记事实，不动 `roadmap.md`（它此刻在别的线的 owns 里）。要做成可机器判的东西，最小形状是：闭合状态收成**一个显式单元格**（例如固定第 N 列取值 `open` / `closed` / `blocked-on:<谁>`），词表单一来源，加一条脚本门禁数出 `open` 行数并钉住"每行必须有这个格"——顺带把运营层表也纳入同一形状。**它需要的文件正在被并行线改**，所以排到那几条落地之后。

### 分工与我自己那两条的状态

- 板面（**按本笔 base `4c0b42db` 于 2026-11-05T06:31+08:00 复读**；初稿那次是 02:58，中间隔了 3.4 小时）：`pi` 一条（`test/changelog-subsection-uniqueness` = 派单 `PI-LEVEE-006`，给 `scripts/test_changelog_structure.py` 补 `[Unreleased]` 内 `###` 小节标题去重 + 粘行断言）、`qoder` 一条（`feat/remote-read-audit-group`）、本线一条。初稿里的 `opencode` 两条**都已落地并撤了声明**：ADR-008 写侧实现 = `3be8895`(02:50+08) + `4c0b42d`(03:54+08)，`system status --remote` = PR **#280**（MERGED 2026-11-04T20:30Z）；`pi` 的另一条（旗标守卫扩面）落在上一版 base `8ab41428`。**这份清单三小时内换了一轮，所以它只承担"本轮复核时刻谁在动"，不要当实时状态读。**
- **ADR-008 已落地，不再是"实现中"**：owner 五问答完（降 **P1**、方案 **D**、只挡新写、注册表为其声明过的主机的唯一权威、豁免只给 `ImportTargets`），ADR 的状态行现已是「**已定案并实现**」（`docs/adr/ADR-008-abac-write-side-env-ownership.md:7`，落点表在文末 `:81` 起）。⇒ §8.3 那一问走完"待定夺 → 材料 → 实现"三段；权威口径以那份 ADR 为准，本笔不改 `blocked-items.md`。我这条线在 #274 只交付裁决材料，实现由另一条线完成——**这条链是本仓少见的完整闭环：一个 P0/P1 议题从登记到落地用了两天。**
- 我在 roadmap 登记过的 `levee-api` 第 6 章缺陷**已由 §11 收口**，本轮复核确认：正文已改成实际注册的 `drift detect` / `drift report`，`drift scan` / `drift show` 只作为被取代的历史口径留在订正注里。
- **协调面的一条观察**：`PI-LEVEE-004` / `PI-LEVEE-005` 这类派单号**不在仓里、也不在板目录里**（`/f/Nexus/agent-board/levee/` 里只有各线自己的声明文件，那两个号只作为引用出现在声明正文；板成员随时间变，本条只依赖"那两个号从不作为独立声明文件存在"这一点）。结果是：**看板上没有待办 ≠ 项目没有待办**——只靠板找活的人会得出错结论。这是 §8.4 那条"板机制能报重叠、不能发现漏声明"的同一类缺口的另一面。

### 本轮验证盲区

- 没跑前端腿（`web/` vitest、`browser-check`），本轮零前端改动。
- 没跑全量 `-race`。
- PG 用的是新建空容器（跑完已 `docker rm`），与 §9.1 / §10 那两台共享库无关：覆盖同一套测试代码，不是同一份数据。
- 浅克隆仍未 unshallow（`git rev-parse --is-shallow-repository` = true），谱系类判断继续只靠内容比对。
- "真未决 6 行"是**人工判读**，不是机器判据产物——上面那三种机器判据没有一种能复现它，这正是本节要登记的问题本身。
