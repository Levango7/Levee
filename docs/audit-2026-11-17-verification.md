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
| **B-07** | 空列表返回 `{}` | 与既有「空集合统一为 []」的批次有关，需逐端点再核 | 暂不动 | 留待逐端点枚举后再定（不在本批冒进） |
| **T-01** | mock 手写 fixture 比真后端更完整，掩盖真 bug | `/system/status`、`/system/config`、conversation、alerts 均实证缺路由或字段漂移 | **真缺口（结构性）** | 本批修：按真实 wire 形状改 mock + 补 conversation/alerts 路由。**生成式 fixture 仍未做**（见下方残留） |
| **F-01/F-02** | 1280 下关键列被压到 <60px | CDP 实测：创建时间列 **168px**；真因是**列宽合计 1100/1132 > 容器 950** | **真问题，但报告机理描述不准** | 按实测修：<1400 隐藏次要列，而不是调那一列 |
| **F-03** | 趋势图固定 640，右半空白 | `trend__svg` **640** in parent **918**（CDP 实测）；`TrendChart` 默认 width=640 且调用方从不传 width | 真缺口 | 本批修：ResizeObserver 按容器宽传参 |
| **F-04** | 对话页错误块在两栏 grid 外、视口外 | 实测 `state--error` bottom=946 > 视口 900；且 mock 无路由必现 | 真缺口 | 本批修：会话列表失败整块替换 grid；动作失败留在 grid 内 |
| **F-05** | 同 B-03 | 同 B-03 | 已由 master 修掉 | 同上 |
| **F-06** | 顶栏/对话页硬编码 `operator` | `App.vue:308`、`ConversationView.vue:10` | 真缺口 | 本批修：统一 `currentSubject()`，头像字母同源 |
| **F-07** | 「转授权」必然 403/412 | `delegate()` 用 `approve({approver: target})` 冒充目标人投票，服务端按认证主体拒绝 | 真缺口 | 本批修：解释型按钮（写明无端点、旧路径会被拒），不再冒充投票 |
| **F-08** | 审批人可编辑 ⇒ 自造 403 | `el-input v-model="approver"` 确在 | 真缺口 | 本批修：只读展示当前主体 |
| **F-09** | 集群接管行 830px 空档 | 属观感问题，本批未实测 | 未动 | 残留（见下） |
| **F-10** | 模板页 CTA 文案不一致 | 未实测 | 未动 | 残留 |
| **F-11** | 1024 告警表环境列被切 | 与 F-01/F-02 同类（列预算超容器） | 未动 | 残留：告警页按同一规则补次要列隐藏即可，但**未实测前不批量改** |
| **F-12** | 无最小宽度/断点策略，768 文字竖排 | 属产品口径（响应式范围本已声明限移动审批） | 未动 | 建议在 UI blueprint 声明「桌面控制台最低 1440×900」而非全面响应式化 |
| **F-13** | 390 toast 覆盖标题、申请人空（mock 字段漂移） | mock 用 `creator`，页面读 `createdBy` | 未动 | 残留：mock 字段名统一一遍（属 T-01 的下一半） |
| **F-14** | 顶栏「文档」指向 `github.com/nexus/levee` | `App.vue:126` 原文，真实远端为 Levango7/Levee | 真缺口 | 本批修：改真实仓库地址 |
| **F-15** | 详情页暂停/取消/归档无 loading | `transition()` 无 acting 守卫 | 未动 | 残留 |
| **P-01** | vendor chunk 1.21MB 无分割 | 构建告警属实 | 未动 | 残留：路由级 code-split |
| **P-02** | 审计表耗时列对缺失值渲染 ` ms` | `AuditView.vue:251-254` | 未动 | 残留 |
| **T-02** | WIP 的 schema_snapshot 未提交、测试未覆盖 | 当时属实 | **已交付** | 已随 #405 合入（`mysql/pg.schema_snapshot` + `sqlguard.TableList` 全测 + DSL 词表 ratchet） |

## 我复核时另外发现的两个缺口（报告未列）

1. **`internal/dsl/actionSignatures` 里没有 `pg.query`** —— 未列出的动作是**编译期跳过**而不是报错，于是 PG 步骤的参数从没有被类型检查（`sql: 42` 能过编译）。同时表里躺着两个**没有任何模块执行**的幽灵键（`shell.command`、`file.write`）。已随 #405 修掉并加双向 ratchet。
2. **`shipped_assets_guard_test.go` 的 G5 可执行集漏 import pg** —— 将来一旦有 pg 工作流进仓，G5 会把平台说成「不支持 pg.query」。已补。

## 残留（明确未做，不冒充已闭环）

- **生成式 mock**：本批只把 mock 改回真实形状，**没有**做「从 proto/wire 快照生成 fixture」这一结构性解法。没有它，同类漂移还会再来。
- **browser-check 的内容断言**：本机 Chrome + CDP 的探针是手写的（`F:/tmp/cdp-probe.mjs`，不入仓），仓内 `browser-check.mjs` 仍只断言 h1 + 几何位置。把「system 页必须出现 storeType/uptimeSeconds」「对话错误块必须在视口内」「发布单必须有七段」这类内容断言写进仓内门禁，是下一件该做的事。
- F-09/F-10/F-11/F-12/F-13/F-15、P-01/P-02、B-07 未动（逐条原因见上表）。

## 验证口径（本批交付的事实）

- `go test ./...` 全绿；新增 state 迁移 3 例 + grpc 归属 4 例（label/team 的存取与只读路径）。
- 前端 `vue-tsc` 0 错误；`vitest` 43 文件 / 350 用例全绿（本机 threads 池起不来属环境问题，`--pool=forks` 全过）；`vite build` 通过；`internal/web/dist` 重打并入库（CI 逐字节比对）。
- 四类 Python 门禁（doc-anchors / roadmap-status / changelog-structure / blocked-items）全过。
- **CDP 实测数字**（1280 视口）：审批表列宽合计 **1100**、变更表 **1132**，容器 **950**；趋势图 **640/918**；对话错误块 **bottom 946 > 视口 900**。探针自证：若无数字即不成立。
