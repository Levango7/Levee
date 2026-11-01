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
> 实测边界（执行路径与快照回滚未在本机实跑，只验证到计划层）见 `deploy/lab/DEMOS.md` §4。


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
