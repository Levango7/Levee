# SQL 审核链演示（§8.6 第 4 步）

`sql-review-chain.sh` 在一台离线机器上复刻 LEVEE 的数据库变更治理链路：
**高危拦截 → 强制审批 → 事后审计追溯**。不需要网络、不需要 MySQL、不需要
真实主机——`apply` 走 status-only（本构建未接执行引擎），全程只动一个临时
数据目录。

## 运行

```bash
go build -o levee ./cmd/levee   # 或任意已构建的 levee
LEVEE_BIN=$PWD/levee bash examples/demo/sql-review-chain.sh
```

可选环境变量：`LEVEE_ACTOR`（发起人，默认 demo-operator）、
`LEVEE_AUDIT_HMAC_KEY`（审计封链密钥，脚本自带演示值）。脚本用 `mktemp -d`
建临时目录，跑完打印产物路径；断言全过才 exit 0。

## 四幕各自证明什么

| 幕 | 命令面 | 证明 | 代码依据 |
|----|--------|------|----------|
| act 0 | `levee sqlreview` | 规则引擎独立可用：`DELETE FROM orders;` 判 fail 并点名 SR-02，exit 1 | internal/sqlreview/rules.go（SR-02）、cmd/levee/cmd_sqlreview.go |
| act 1 | `levee plan` | fail 级 SQL 在 plan 期被拒，run 停在 draft，审批链从未生成 | internal/wiring/sqlreview_gate.go:35-71（reviewWorkflowSQL）、internal/wiring/plan.go:68 |
| act 2 | `levee new/plan/approve/apply` | warn 级（DROP TABLE → SR-07）plan 放行且结论随 plan_hash 绑定；high 档要 alice+bob 两票（发起人除外）；未集齐前 apply 两次被拒（exit 4）；集齐后 status-only 执行 | internal/approval/levels.go、cmd/levee/cmd_apply.go:39 |
| act 3 | `levee audit report/verify` | HTML 报告同时给出审核结论（warn + SR-07 明细）、plan hash、完整审批链（含 exclude_initiator）；零 trace 的 run 如实说 "no trace records"，验链如实说 "no traces found"，不伪装完好 | cmd/levee/cmd_audit_report.go、cmd/levee/cmd_audit.go |

## 如实态（演示里特意保留的）

- status-only 的 run 停在 `running`：没有执行引擎就没有批次证据，`show` 的
  Evidence 判 `in_flight`、审计报告的哈希链栏写 `no trace records`——空证据
  就说空，不渲染成「链完好」。
- 审计报告页脚注明验链与 `levee audit verify` 同源（同一 ChainVerifier）。

## 演示文案边界

规则文案全为仓库自撰（internal/sqlreview/rules.go 头注已声明）；本演示只复
刻公开选型话术中的「阈值分档」概念——fail 级阻断、warn 级放行但留痕——任
何文字均未抄录外部材料。

## 已踩的坑（写进脚本的）

`LEVEE_AUDIT_HMAC_KEY` 必须从第一次审计写入就设置：中途加 key 会让早期
记录按无钥封存、后续按有钥重读，sealer 报「hash chain exists but is
broken」（2026-11-12 实测）。脚本因此从第一行就导出该变量；换数据目录即可
复现干净链路。
