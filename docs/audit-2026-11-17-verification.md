# 2026-11-17 第三方整体审计的逐条核实与处置

**对象**：外部（豆包）对 `F:\Nexus\levee` 的《LEVEE 代码级整体审计报告》（787 行）+ 证据包（12 张截图、可复现命令）。
**复核人**：本线（ZCode）。**复核方式**：代码级判据 + 真机实测，不复述报告。
**基线**：复核时 master 已多次前进（`d209e9c` 起），报告观测点为 `5efc3db`/`09319c8`。**"已由 master 修掉"必须逐项单列**，否则会把别人的成果再修一遍。

## 一句话

报告的**方向是对的**（契约保真、临界宽度、错误/空态、mock 漂移），后端七条全部成立；前端两条（F-01/F-02）的**机理描述与实测不符**，按实测修。**最重要的教训是报告的 §7**：手写 mock 比真后端"更完整"，导致门禁全绿而页面错误——这一类缺口我们自己的审计确实漏得过。

## 逐条核实

| ID | 报告结论 | 我的判据 | 定性 | 处置 |
| --- | --- | --- | --- | --- |
| **B-01** | 模板实例化 label/team 不落库 | `state.Run` 无此二列；`template_service.go:425/434` 回显请求值；`runToPB` 由 workflow 名派生 label、**没有 Team 映射**。且同文件 `Priority` 的注释记录的就是同一缺陷类（当时只修了 priority） | **真缺口（P0）** | **本批修复**：v11 双梯加列 + 三条创建路径写入 + `labelOf()` 读映射 + 7 例测试 |
| **B-02** | 无引擎模式启动日志称 fully functional | `cmd_serve.go:1528` 原文即此；无引擎时 plan 空、approve 412 | 真缺口 | 本批修：日志措辞 + quickstart 注记 |
| **B-03** | 空 trace 显示绿「哈希链完整」，CLI 同状态 exit 1 | 报告时属实 | **已由 master 修掉**（他线实现更完整：`audit.VerifyTraces` 真重算摘要、`FailureEmptyHash` 区分「未封口≠篡改」、消息前缀契约） | **撤掉我先前的重复实现**（`hash_chain_state` 三态字段），保留 master 版本 |
| **B-04** | `/system/config` 返回 base64，前端当 YAML 渲染 | `writeProto` → protojson 对 `bytes` 的规范编码；`SystemView.vue:272` 直接渲染；mock 却发明文 | 真缺口 | 本批修：视图解码 + **mock 按真实 wire 发 base64** |
| **B-05** | 未注册 target 返回 500 | `wiring/plan.go:333` 原文 | **已由 master 修掉**（`internal/wiring/errors.go` 哨兵带码 + `planStatus`/`engineStatus` + 专测） | **撤掉我先前的重复实现**（`planFailureStatus`），保留 master 版本 |
| **B-06** | README 称 `--insecure` 仍按操作者记录，与实现 401 冲突 | 文档原文 vs 实现 | 真缺口 | 本批修：README 改为「只放宽传输，不放宽审批主体」 |
| **B-07** | 空列表返回 `{}` | 旧代码实测 changes/templates/targets/audit-log/audit-traces 均丢集合键；根因=protojson 默认省略空 repeated | **真缺口（实测确认）** | **后续批已修**：仅顶层空集合显式为 `[]`；agents/groups 实测原本就是 `[]`，未误改 |
| **T-01** | mock 手写 fixture 比真后端更完整，掩盖真 bug | `/system/status`、`/system/config`、conversation、alerts 均实证缺路由或字段漂移 | **真缺口（结构性）** | 本批修：按真实 wire 形状改 mock + 补 conversation/alerts 路由。**生成式 fixture 仍未做**（见下方残留） |
| **F-01/F-02** | 1280 下关键列被压到 <60px | CDP 实测：创建时间列 **168px**；真因是**列宽合计 1100/1132 > 容器 950** | **真问题，但报告机理描述不准** | 按实测修：<1400 隐藏次要列，而不是调那一列 |
| **F-03** | 趋势图固定 640，右半空白 | `trend__svg` **640** in parent **918**（CDP 实测）；`TrendChart` 默认 width=640 且调用方从不传 width | 真缺口 | 本批修：ResizeObserver 按容器宽传参 |
| **F-04** | 对话页错误块在两栏 grid 外、视口外 | 实测 `state--error` bottom=946 > 视口 900；且 mock 无路由必现 | 真缺口 | 本批修：会话列表失败整块替换 grid；动作失败留在 grid 内 |
| **F-05** | 同 B-03 | 同 B-03 | 已由 master 修掉 | 同上 |
| **F-06** | 顶栏/对话页硬编码 `operator` | `App.vue:308`、`ConversationView.vue:10` | 真缺口 | 本批修：统一 `currentSubject()`，头像字母同源 |
| **F-07** | 「转授权」必然 403/412 | `delegate()` 用 `approve({approver: target})` 冒充目标人投票，服务端按认证主体拒绝 | 真缺口 | 本批修：解释型按钮（写明无端点、旧路径会被拒），不再冒充投票 |
| **F-08** | 审批人可编辑 ⇒ 自造 403 | `el-input v-model="approver"` 确在 | 真缺口 | 本批修：只读展示当前主体 |
| **F-09** | 集群接管行 830px 空档 | 1440 实测：行 1110、run 弹性列 684、真实列间距 **12px**；空白在弹性列内部，不是布局断裂 | **非缺陷（实测后不改）** | 保持流式布局；不把宽屏弹性空白当裁切修 |
| **F-10** | 模板页 CTA 文案不一致 | 同页实测按钮为「新建模板」「创建模板」 | 真缺口 | **后续批已修**：顶部、空态、创建对话框统一「创建模板」 |
| **F-11** | 1024 告警表环境列被切 | 1024 实测表右缘 **975**、环境右缘 **1031**、触发时间右缘 **1161**，且不可横滚 | **真缺口（实测确认）** | **后续批已修**：<1240 隐藏来源/环境；修复后触发时间右缘=表右缘 **975**，1280 全列保留 |
| **F-12** | 无最小宽度/断点策略，768 文字竖排 | 768 实测四页仅审批标签被压成 **12×56px**，其余页无竖排/横溢；根因是长快捷键提示挤压，不是全站缺最小宽度 | **真缺口（局部，实测确认）** | **后续批已修**：<1400 隐藏快捷键提示 + 标签 flex:none/nowrap；UI 蓝图写明降级边界 |
| **F-13** | 390 toast 覆盖标题、申请人空（mock 字段漂移） | mock 用 `creator`，页面读 `createdBy` | 真缺口 | **后续批已修**：mock 全部 change fixture 改 `createdBy` 并补 `workflowFile`，由 mock-wire 护栏钉住 |
| **F-14** | 顶栏「文档」指向 `github.com/nexus/levee` | `App.vue:126` 原文，真实远端为 Levango7/Levee | 真缺口 | 本批修：改真实仓库地址 |
| **F-15** | 详情页暂停/取消/归档无 loading | `transition()` 无 acting 守卫 | 真缺口 | **后续批已修**：单一在途动词 + 四按钮 loading/disabled + finally 清理 + 等待刷新 |
| **P-01** | vendor chunk 1.21MB 无分割 | 「无分割」不准确：18 条路由全部动态 import；仅首屏 vendor 合并。三组拆分实验均未减总量，反把 preload 增至 16–20 个 | **实测后不拆** | 保留现设计；不为消除告警制造更多首屏请求 |
| **P-02** | 审计表耗时列对缺失值渲染 ` ms` | `AuditView.vue:251-254` 无条件插值；protojson 省略未设置的 durationMs | 真缺口 | **后续批已修**：null/undefined 显示「—」，0 ms 仍作为真实测量值展示 |
| **T-02** | WIP 的 schema_snapshot 未提交、测试未覆盖 | 当时属实 | **已交付** | 已随 #405 合入（`mysql/pg.schema_snapshot` + `sqlguard.TableList` 全测 + DSL 词表 ratchet） |

## 我复核时另外发现的两个缺口（报告未列）

1. **`internal/dsl/actionSignatures` 里没有 `pg.query`** —— 未列出的动作是**编译期跳过**而不是报错，于是 PG 步骤的参数从没有被类型检查（`sql: 42` 能过编译）。同时表里躺着两个**没有任何模块执行**的幽灵键（`shell.command`、`file.write`）。已随 #405 修掉并加双向 ratchet。
2. **`shipped_assets_guard_test.go` 的 G5 可执行集漏 import pg** —— 将来一旦有 pg 工作流进仓，G5 会把平台说成「不支持 pg.query」。已补。

## 残留（明确未做，不冒充已闭环）

- **生成式 mock**：本批只把 mock 改回真实形状，**没有**做「从 proto/wire 快照生成 fixture」这一结构性解法。没有它，同类漂移还会再来。
- **browser-check 的内容断言**：本机 Chrome + CDP 的探针是手写的（`F:/tmp/cdp-probe.mjs`，不入仓），仓内 `browser-check.mjs` 仍只断言 h1 + 几何位置。把「system 页必须出现 storeType/uptimeSeconds」「对话错误块必须在视口内」「发布单必须有七段」这类内容断言写进仓内门禁，是下一件该做的事。
- F-09/F-10/F-11/F-12/F-13/F-15、P-01/P-02、B-07 未动（逐条原因见上表）。

## 后续批次（2026-11-19）：mock↔wire 契约守卫 + 浏览器内容断言

针对上面两条结构性残留，本批交付（分支 `feat/mock-wire-guard-browser-assertions`）：

1. **`web/scripts/mock-wire-check.mjs`（新增）** —— mock 是 wire 契约的**第三个消费方**，此前 `wire_snapshot_test.go`（Go 生成 wire.json）与 `wireContract.spec.ts`（DTO 双向校验）都不看它，漂移由此漏网。护栏把 mock 实际发出的键集对到同一份 wire.json + DTO 接口上：
   - **ceiling（不许发明字段）**：字段全集用 DTO 声明 ∪ wire 观测键 ∪ 带理由的 proto 豁免，**不**用 wire.json 观测样本当全集——因为 protojson 会省略零值（`team/environment/priority` 是真字段、快照里为空被丢），拿样本当全集会误伤。
   - **floor（不许丢「必发且必填」的键）**：wire.json 里有、且 DTO 里非可选的键，mock 不得丢。protojson 省略零值的键**不**强制——护栏不能比真后端更严。
   - **forbid（无 PascalCase 泄漏）**：用 wire.json 的 forbid 表。
   接进 `package.json` 的 `test:mock-wire` 与 CI `frontend` job。
2. **护栏首跑即抓住并修复 6 处真漂移**（144/144 转绿）：
   - `/system/version`：mock 发明 `commit`/`builtAt`、漏 `gitCommit`/`buildDate`/`goVersion`（报告未列，版本面板在生产会空白）；
   - `/changes` 行 + `/changes/run-1000` detail + `run-1001` + `run-fleet`：`creator`→`createdBy`（F-13 本尊），并补 `workflowFile`；
   - `/system/batch-status`：去掉发明的 `run_id`（真后端不回显）。
   `planHash` 是 proto `Change.plan_hash=9` 真字段（runToPB 会发、页面不读），列入带理由豁免而非误判。
3. **browser-check 补 `[1d]`**：system 页「存储后端=sqlite / 运行时长非空 / 提交=gitCommit / 工具链=goVersion」内容断言，轮询等数据就位（同 [1b] 徽章等法）。本地实跑 `browser-check: ALL PASS`。
4. **诚实登记的覆盖缺口**（护栏输出，未冒充闭环）：`dbchange-ticket`（mock 无 ticket 路由，故「发布单七段」断言尚无法落地）、`change-with-plan`（mock 未建模 `includePlan`）、`audit-traces`/`agents`/`groups`（mock 未服务）；`takeover-events`/`rollback` mock 有服务但不在本护栏键校验范围（无 DTO read-set / POST 由 browser-check 直接驱动）。

验证：`mock-wire-check` 144/144；`browser-check` 本地 ALL PASS（含 [1d] 四条）；`vitest` 48 文件/383 用例（forks 池）；`vue-tsc + vite build` 通过。本批只改 `web/scripts` 与 `web/package.json`，未碰 `web/src`，`internal/web/dist` 无需重打。

仍未动：F-09/F-10/F-11/F-12（需 CDP 实测或产品口径）、P-01、B-07。「发布单七段」断言由下一批补齐。

## 后续批次（2026-11-20）：F-15 在途守卫 + P-02 缺失耗时

- **F-15**：`ChangeDetailView.transition()` 以当前生命周期动词做单 flight；第二次点击在确认框之前返回，四个互斥动作按钮统一 disabled、仅当前动作显示 loading，成功或失败均在 finally 解锁，并等待详情刷新完成再清除在途态。
- **P-02**：审计表对 protojson 省略的 `durationMs` 显示「—」而非裸 ` ms` / `undefined ms`；保留 `0 ms`，不把真实零值误作缺失。
- **钉测与变异**：新增 2 例结构钉测，定向 20/20、全量 vitest 48 文件 / 385 用例全绿。三项真实变异（恢复旧耗时插值、去掉在途守卫、去掉 finally）分别让对应判据转红；恢复后两源文件 sha256 与变异前逐字一致。
- **构建**：vue-tsc + vite build 通过，`web/dist` 与 `internal/web/dist` 已重打并用 `diff -r` 确认一致。

## 后续批次（2026-11-20）：ticket mock + 发布单七段内容断言

- **完整 fixture**：为已有的 completed 变更 `run-1003` 增加平铺七段 ticket（change/sql/rollback/window/approval/execution/audit），七段均有真实内容；int64 时间戳按 protojson 用字符串，int32 计数保持数字，不再让「无 ticket 路由」成为浏览器盲区。
- **契约校验**：`dbchange-ticket` 纳入 `mock-wire-check`，顶层与七个 section 分别按 DTO/wire 校验；总数由 144 增至 **168/168 全绿**。剩余未服务端点诚实登记为 change-with-plan、audit-traces、agents、groups。
- **浏览器断言**：新增 `[1e]`，先断言七个 tab 的名称与顺序，再逐一点击并只读取可见 pane 的专属证据（SQL、快照路径、窗口、审批理由、执行说明、审计 verdict），防止「七个空 tab」假绿。本地完整 `browser-check: ALL PASS`。
- 本批只改 mock 与检查脚本，未碰 `web/src`，无需重打 dist。

## 后续批次（2026-11-20）：F-09/F-10/F-11 实测分诊

- **F-09 不改**：真实 Chrome 在 1440 下测得接管行 1110px、run 弹性列 684px、相邻列间距 12px；报告所述 830px 不是列间断层，是宽屏弹性列内部空白。保持流式布局，不把观感偏好伪装成缺陷修复。
- **F-10 已修**：顶部、空态与创建对话框统一「创建模板」；源码钉测 + 浏览器点击对话框双重验证。
- **F-11 已按数字修**：修复前 1024 表右缘 975，而环境/触发时间右缘分别 1031/1161 且不可横滚；全列最小预算 880px、面板宽=视口−330，故 <1240 隐藏来源/环境两个次要列。修复后 1024 触发时间右缘=975，1280 六列全部保留且右缘=1231。
- **门禁**：browser-check 新增 `[1f]` 双宽度几何断言与 `[1g]` 文案/对话框断言；完整 ALL PASS。vitest 48 文件 / 386 用例、vue-tsc + vite build 全过，dist 重打同步。

## 后续批次（2026-11-20）：B-07 空集合信封

- **先红后绿**：真实 REST 回归证明 changes/templates/targets/audit-log/audit-traces 空结果均为 `{}`、没有集合键；修复后五者分别为 `{changes:[]}`、`{templates:[]}`、`{targets:[]}`、`{entries:[]}`。`/agents`、`/groups` 实测原本就返回裸 `[]`，只加钉测、不改实现。
- **最小规则**：`marshalProtoWithEmptyCollections` 只补顶层缺失的 repeated 字段为 `[]`，不使用全局 EmitDefaults；嵌套 plan 的空 batches/targetHosts 与 totalSize 等零标量仍按原契约省略，预览/单对象响应不被顺带改写。
- **契约同步**：wire 快照仅新增 `/system/status` 的 `warnings`（空数组）一项；mock 同步 `warnings:[]`，mock-wire 168/168、DTO wireContract 6/6。
- **验证**：新增 2 例（7 个端点子路径 + 嵌套省略保持），全量 vitest 386、build 通过。全量 Go 验证另发现 master 既有 Windows 日志句柄红腿，已由独立 hotfix 处理，未混入本批。

## 后续批次（2026-11-21）：mock 覆盖缺口归零

- **change-with-plan**：`/changes/run-1000?includePlan=true` 现在附带真实存量 Plan（changeId/targetHosts/batches/impactSummary），默认请求仍不带 plan；护栏新增嵌套 `plan.batches` 数组校验，浏览器断言影响面与第二批次目标确实到达详情页。
- **audit-traces**：按真实 `targetHost` 字段与分页信封返回；`/agents`、`/groups` 按裸数组返回完整 DTO 记录，不再由 404 兜底。
- **护栏升级**：支持 bare-array `rootDto` 与点路径 `objectArrays`，mock-wire 从 168 增至 **197/197**，四个覆盖缺口全部清零；仍如实保留 takeover-events/rollback 为「服务但不在此键校验」的既有边界。
- **验证**：完整本地 browser-check ALL PASS（含新增 stored-plan 内容断言）；未改 `web/src`，无需重打 dist。

## 后续批次（2026-11-21）：F-12 局部响应式 + P-01 拆分否决

- **F-12 已修**：768 实测四页只有审批标签被长快捷键提示挤成 12×56px，其他页没有所谓整站竖排。<1400 隐藏提示，标签 flex:none/nowrap；1600 恢复提示。UI 蓝图新增「有序降级而非强制最小宽度」边界，browser-check `[1h]` 双宽度钉住。
- **P-01 实测不拆**：现有 18 条路由全部动态 import，单体仅为首屏 vendor。① 按包改名后 Element Plus 仍 1.134MB；② 450KB 上限产生约 15 个 preload；③ 900KB 与严格顺序实验产生 16–20 个 preload，还拆出额外应用共享块。总下载量未下降、请求数反升，故恢复原配置，不为告警拆包。
- **验证**：完整 browser-check ALL PASS；vitest 48 文件 / 386 用例（限 4 workers 避免本机启动超时）；vue-tsc + vite build 通过，dist 重打同步。

## 验证口径（本批交付的事实）

- `go test ./...` 全绿；新增 state 迁移 3 例 + grpc 归属 4 例（label/team 的存取与只读路径）。
- 前端 `vue-tsc` 0 错误；`vitest` 43 文件 / 350 用例全绿（本机 threads 池起不来属环境问题，`--pool=forks` 全过）；`vite build` 通过；`internal/web/dist` 重打并入库（CI 逐字节比对）。
- 四类 Python 门禁（doc-anchors / roadmap-status / changelog-structure / blocked-items）全过。
- **CDP 实测数字**（1280 视口）：审批表列宽合计 **1100**、变更表 **1132**，容器 **950**；趋势图 **640/918**；对话错误块 **bottom 946 > 视口 900**。探针自证：若无数字即不成立。
