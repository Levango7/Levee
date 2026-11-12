# SQL 审核链演示（高危拦截 · 强制审批 · 事后审计追溯）

本文档是 §8.6 第 4 步的交付面：一个可以在一台离线机器上复跑的演示脚本，
复刻数据库变更发布楔子的三个演示阈值——**高危拦截、强制审批、事后审计
追溯**（阈值概念按市场选型口径复刻，全部文案自撰）。

与 `deploy/lab/DEMOS.md` 的分工：DEMOS.md 是面向客户的场景演示库（A1–A4，
需要真 target 与实验室拓扑）；本文档是**SQL 审核治理链**的专项演示，不需要
任何外部数据库。

## 1. 前置

| 项 | 要求 |
| --- | --- |
| 二进制 | `make build`（或 `go build -o levee ./cmd/levee`），设为 `LEVEE_BIN` |
| 数据库 | 无。演示数据落在 `mktemp -d` 的 throwaway 目录，跑完即弃 |
| 网络 | 无。主机清单是虚构的 `db-01.prod`，全程不联系任何机器 |
| 环境变量 | 脚本自建 `LEVEE_SERVER_DATA_DIR` / `LEVEE_AUDIT_HMAC_KEY`（演示 key，仅供本脚本） |

脚本位置：`examples/demo/sql-review-chain.sh`（该目录当前未随版本库提交，
归属在 agent-board 登记中；复跑请向维护者取该文件）。

## 2. 跑法

```sh
LEVEE_BIN=$PWD/levee bash examples/demo/sql-review-chain.sh
```

期望：四幕全部 `PASS:`，末行 `all acts passed`，退出码 0。
每幕产物在脚本打印的临时目录里（act0–act3 文本 + HTML 报告）。

## 3. 四幕：看到什么、证明什么、验收哪条证据

| 幕 | 屏幕上看到 | 证明的治理语义 | 验收证据 |
| --- | --- | --- | --- |
| act 0 规则引擎 | `DELETE FROM orders;` 当场被判 fail，规则 SR-02 具名，退出码 1 | 审核面独立于工作流存在：不给机会进审批池，先过规则 | `act0-sqlreview.txt` 的 `verdict: fail (1 fail, 0 warn)` |
| act 1 高危拦截 | `levee plan` 以 `FailedPrecondition` 拒绝，报错点名 SR-02 与步骤 | 失败裁决卡死在计划生成前：审批流根本不会被拉起 | `act1-plan.txt` 的 `sql review failed: step "purge-orders" ... rule SR-02` |
| act 2 强制审批 | 警告级 SQL 过审、计划落库并绑定哈希；只投一票时 `apply` 被拒（退出码 4）；两票齐后执行（status-only） | 警告不拦但留痕；高级别变更双人签字、发起人除外；未达法定人数不放行 | `act2-plan.txt` 的 `plan persisted and hash-bound`；`act2-apply-early.txt` 退出码 4；`act2-approve-bob.txt` 的 `approved by bob` |
| act 3 事后审计追溯 | HTML 报告同页给出审核结论、计划哈希、两名审批人、发起人排除标记；空链如实报空 | 审计报告＝结论＋哈希＋审批链三件套，且不断链装链 | `report.html` 含 `SR-07` / `v2:` / `alice` / `bob` / `exclude_initiator`；`audit verify` 对空链报 `no traces found` |

## 4. 演示话术（三句，自撰）

- **高危拦截**：一条没有 WHERE 的删除语句，连计划都生成不了——它到不了
  审批池，更到不了生产库。
- **强制审批**：警告级的结构变更放行前，必须有两名互斥审批人签字；发起人
  不能投自己，差一票都不行。
- **事后审计追溯**：事后打开报告，这次变更的审核结论、计划哈希、谁批的，
  一页说完；审计链断了就如实说断了，不粉饰。

## 5. 与九步链路的映射（如实，不夸大）

市场扫描 §5 给的完整链路是：提交 SQL 工单 → 规则审核（高危拦截）→ 分级
审批 → 计划生成 → plan_hash 绑定 → 分批执行 → 验证门禁 → 失败自动回滚 →
哈希链审计报告。本演示覆盖前五步与最后一步；**分批执行、验证门禁、失败
自动回滚不在离线演示面内**——它们需要 `serve --engine-enabled` 与真实
target（实验室拓扑见 `deploy/lab/README.md`）。act 2 的 apply 是
status-only：只改状态不动机器，输出里带 `status-only` 标记，不冒充执行。

## 6. 边界（如实）

- 空 `k8spolicy: {}`、空 trace 链等空态一律如实报空，不补假数据。
- act 2 用 `--as alice` / `--as bob` 本地模式审批，会有一条既有
  WARN（本地模式无权限矩阵可判）——演示画面里保留它，不截掉。
- 规则面 SR-01..SR-20 共 20 条，文案自撰（`internal/sqlreview` 有专测反查
  第三方工具词汇与薄文案）；阈值概念可复刻，措辞不抄任何外部选型文。
- 演示 HMAC key 仅供本脚本；生产环境的密钥管理不在本演示面。
