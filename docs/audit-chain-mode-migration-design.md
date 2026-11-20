# 审计链摘要模式迁移设计（D-8，2026-11-21，设计输入，**未实现**）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 设计输入；落地需产品决策（回填/归档策略属 product call） |
| 基线 | `master@8fc1135`；既有结论见 `docs/deployment.md` §5.6、`docs/blocked-items.md` D-8、`internal/audit/hmackey_switch_test.go` |
| 状态 | **设计完成，代码未动** |

---

## 1. 为什么不能"直接换成 SM3"

`internal/audit/canonical.go:98-106` 的 `digest()` 是全仓审计摘要唯一漏斗（配 key → HMAC-SHA256，未配 → SHA-256）。`audit`/`trace` 两表只有 `prev_hash`/`curr_hash` 两列，**没有记录每行用的是哪种算法**。既有实测（`docs/deployment.md:152-154`）已经证明：仅更换 HMAC key 就会让存量记录 `ErrHashMismatch`、`VerifyTraces valid=false`，而验链器**分不清"换了 key"与"被篡改"**——算法切换同理且更彻底（摘要长度都是 64 hex，连编码层都看不出差异）。

`internal/audit/hmackey_switch_test.go` 是一份**ratchet 测试**：它把"换 key 会断链"钉成期望行为；将来引入 per-record mode marker 或双口径验链器时，**这份测试必须同 commit 更新**，否则会转红。

## 2. 设计目标

1. 新增 SM3（GM/T 0004-2012）摘要模式作为**可选项**，默认 `sha256` 不变。
2. 存量链**不重写、不回填摘要值**——历史行保留其原模式，新行用新模式；同一条链允许混合模式。
3. 验链器按**每行的模式**重算摘要，跨行 `prev_hash` 仍是前一行的实际摘要字符串（算法无关）。
4. HMAC key 轮换与摘要模式切换**解耦**为两个 epoch 维度，避免重演"验链器分不清"的问题。

## 3. 数据模型（建议 migration v12）

`internal/state/migrate.go` 现有最新为 v11。新增一步：

```sql
ALTER TABLE audit ADD COLUMN digest_mode TEXT NOT NULL DEFAULT 'sha256';
ALTER TABLE trace ADD COLUMN digest_mode TEXT NOT NULL DEFAULT 'sha256';
```

配套：
- `schema.sql` / `pgschema.sql` / `legacy_v1_test.go` 的建表语句同步加列（保持 `TestMigrate_FreshAndUpgraded_SchemaShapesMatch` 的形状一致）。
- **WORM 触发器**：`worm_prevent_audit_update`（`migrate.go:208-220`）当前逐列比较内容字段；`digest_mode` 应作为**受保护字段**加入 WHEN 列表（改模式等同于改内容），迁移中需 `DROP TRIGGER` + 重建。
- 索引 `idx_audit_chain (timestamp, id)` 不变。

## 4. 摘要与验链

- `internal/audit/canonical.go`
  - `digest()` 增 `mode` 参数：`sha256 | sm3`（HMAC 口径分别为 HMAC-SHA256 / HMAC-SM3）。
  - 复用 `internal/gmcrypto` 的 `SM3Sum` / `NewSM3`（2026-11-21 已合入，PR #419），避免第二套国密依赖。
  - 对外编码仍是 hex；SM3 为 32B → 64 hex，与 SHA-256 同宽，无需改列宽或 JSON 契约。
- `internal/audit/hashchain.go:307-334`、`worm.go:151-180`、`verify.go:142-244`
  - 写入：每行固化 `digest_mode`；`prev_hash` 取前一行 `curr_hash` 原值。
  - 校验：逐行读取 `digest_mode` 决定重算算法；**不存在算法级"回退"**（V1/V2 编码回退保留，见 `verify.go:231`）。
- `internal/grpc/rest.go:1710-1729` `/audit/chain-mode`
  - 现返回部署级 `"SHA-256"` / `"HMAC-SHA256"`；扩展为 `current_mode` + `row_modes`（实际出现的模式集合），并注明这是**部署当前状态**、不是每行属性（保持与 `docs/levee-api.md:1082-1084` 一致）。

## 5. 两个 epoch 维度

| 维度 | 载体 | 切换方式 |
| --- | --- | --- |
| 摘要算法 | 每行 `digest_mode` | 新行生效；历史行不变；混合链可验 |
| HMAC key | `LEVEE_AUDIT_HMAC_KEY`（进程级） | **仍不支持就地轮换**（既有结论）；换 key 必须归档旧库 + 开新链 |

明确：本设计**不改变** HMAC key 的归档重开链要求，只解决"算法切换导致存量不可验"这一半。

## 6. 回填与过渡策略（三选一，需产品裁决）

| 方案 | 做法 | 优点 | 代价 |
| --- | --- | --- | --- |
| **A 混合链（推荐）** | 历史行保持 sha256，新行 sm3，验链器双口径 | 零回填、零停机、历史可验 | 验链器复杂度上升；长期存在混合链 |
| B 归档重开链 | 沿用 `docs/deployment.md` 既有路径：归档旧库，以 sm3 开新链 | 单一模式，验链器简单 | 历史链物理分离，跨库查询/举证需并查 |
| C 全量回填 | 重写所有历史行摘要为 sm3 | 单一库单一模式 | 改 WORM 保护内容、需关闭触发器并重算全链；与 tamper-evident 语义冲突，**不建议** |

## 7. 测试与守卫

- 更新 `hmackey_switch_test.go` ratchet 文案（同 commit）。
- 新增：混合模式链（sha256 行 → sm3 行）`VerifyTraces valid=true`；篡改任一模式字段被 WORM 拦；unknown `digest_mode` 硬失败。
- `internal/state` 迁移测试：fresh/upgraded schema 形状一致；旧库升级后历史行 `digest_mode='sha256'` 且仍可验。
- `internal/grpc/rest_chain_mode_test.go`:69-117 的 `"SHA-256"` 硬断言扩展而非替换。
- 命令面：`cmd/levee/cmd_audit.go:53-64` verify/export 输出增加模式信息；`cmd_archive.go:142,149` 归档补盖 checksum 需带模式。

## 8. 影响面清单（落地前核对）

- `internal/state/{migrate.go,schema.sql,pgschema.sql,audit_vocabulary_test.go,legacy_v1_test.go}`
- `internal/audit/{canonical.go,hashchain.go,worm.go,verify.go,auditchain.go,*_test.go,hmackey_switch_test.go}`
- `internal/grpc/{rest.go,rest_chain_mode_test.go}`
- `internal/gmcrypto`（已就绪）
- `config.example.yaml` / `configs/config.yaml` 的 `audit` 段新增 `digest_mode`（默认 sha256）
- `docs/{deployment.md,levee-api.md,blocked-items.md}` 同步

## 9. 边界

- 本设计**不改变**默认行为；在当前落地前，`/audit/chain-mode` 仍报 SHA-256/HMAC-SHA256。
- 摘要模式不等于"国密合规"；它只解决审计链算法可迁移性。TLCP/SM2/密评仍是独立专项（见 `docs/tender-evidence-pack-2026-11.md` M-04）。
- 对外口径在落地前不得说"审计链已支持 SM3"。
