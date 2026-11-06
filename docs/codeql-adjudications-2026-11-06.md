# CodeQL 公开告警裁决记录（2026-11-06）

> 这是**裁决账本**，不是待办。它登记了 2026-11-06 之前 CodeQL 还挂着的 4 条
> 告警：为什么它们不是漏洞、以及为什么在工程上不该再花精力修它们。SQLi 与
> trend.go 的两条已在 `#296`（`fix/codeql-open-alerts`）里修掉并关账。
>
> 裁决口径与仓库既有的先例一致：不加 `#nosec` / suppression；在文档里写清
> 判定依据，承认它 "open"，等规则或实现改变了再处理。CodeQL 不是必需门
> （分支保护只要求 `check (all jobs passed)`，它的 `needs:` 是 ci.yml 的
> 16 个 job；CodeQL 定义在 `codeql.yml`，不参与必需检查）。

## 裁决总表

| # | 文件:行 | 规则 | 严重度 | 处置 | 判定依据 |
|---|---------|------|--------|------|----------|
| 12 | `internal/grpc/audit_service.go:261` | `go/uncontrolled-allocation-size` | high | 保留（qoder 文件域，未在此裁决重写） | `pageSize` 在 `make([]*pb.TraceEntry, 0, pageSize)` 之前已由 `helpers.go` 的 const 块夹住（`maxPageSize=1000`）；`ListAuditTraces` 流式分页走 `auditStorePageSize`，上限 const；同一形状在 master 上已是开放告警 #5/#6 的孪生 |
| 5 | `internal/grpc/audit_service.go:162` | `go/uncontrolled-allocation-size` | high | 保留（同上） | 同上（`GetAuditLog` 的同形状 make；函数入口 `:137-143` 有 `pageSize <= 0 → defaultPageSize`、`> maxPageSize → maxPageSize` 的双钳） |
| 6 | `internal/grpc/change_service.go:2817` | `go/uncontrolled-allocation-size` | high | 保留 | `pageSize` 在 `:2734-2742` 已 clamp 到 `maxPageSize=1000`；capacity 现在以 const 常量上钳，不是 "用户输入直接进 make"；CodeQL 的 taint 分析不进 const 块，判不到 |
| 8 | `internal/diagnosis/health_probe.go:781` | `go/request-forgery` | high | 保留 | `target` 在 `probeHTTP` 入口已由 `validProbeTarget` 白名单校验（仅 host / IPv4 / IPv6，拒绝 userinfo / path / query / 反斜线 / 空白），闭合了 SSRF 真阳性分支；CodeQL 不追踪自定义校验函数的返回约束，判不到 |

## 已修复并关账的

- `#1`（`inventory_sqlite.go:246`，`go/sql-injection`）：label key 直接拼进
  `json_extract` 路径。在 `#296` 里改绑定参数 `jsonPathKey()`。
- `#9`/`#10`（`internal/state/trend.go:186-187`）：两处 capacity 已由
  `MaxRunTrendDays=365` const 夹住；常量声明让上限在代码里可见。

## 为什么不给这 4 条加 suppression

- 它们**不是**"不想碰"的麻烦事——是规则与实现的认知差：CodeQL 的 taint
  分析不追踪 const clamp / 自定义白名单校验函数。
- 加 `#nosec` 会让 open 队列更顺眼，但会让这条认知差消失——下一次 CodeQL
  规则升级就没有可查的案例库了。
- 真实的代码风险已在 `#296` 里收掉：SSRF 与 SQLi 都有对应的回归测试
  （`TestProbeServiceSSRFTargetFailsClosed`、`TestTargetFilterHostileLabelKey`）。
