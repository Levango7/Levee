# 审计链摘要模式迁移设计（D-8，2026-11-21 设计 / 2026-11-23 实现）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 设计 + 实现记录（算法级能力，非密评） |
| 基线 | 设计 `master@8fc1135`；实现见文末 §10 |
| 状态 | **已实现（2026-11-23）**：采用**哈希值前缀**方案（非加列），历史行天然兼容。下文 §3–§8 的原"加列 + migration v12"设计保留为决策取证，实现改用更低风险的等价机制（零 schema 迁移、零 WORM 触发器改动、历史链不回填即可验） |

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
- 对外口径在落地前不得说"审计链已支持 SM3"（本节为落地前原文；落地后口径见 §10）。

---

## 10. 实现记录（2026-11-23）

### 10.1 实际方案：哈希值前缀（等价机制，替换原"加列"设计）

完整调用面勘察后发现原设计的加列方案牵涉 50+ 文件（两套 DB 迁移 × 4 个旧版本 fixture × 90 处写入点 × 两张 WORM 触发器），任一漏改即导致历史链整链 `ErrHashMismatch`。故改用**哈希值前缀**——与 `internal/plan/hash.go` 的 `v2:` 同范式，但把算法族编码进摘要本身：

| 配置 | 输出 | 算法 |
|---|---|---|
| 默认 / 历史（无前缀 64 hex） | `ab12…` | SHA-256 或 HMAC-SHA256（按 LEVEE_AUDIT_HMAC_KEY） |
| `audit.digest_mode: sm3` | `sm3:ab12…` | SM3（GM/T 0004-2012） |
| sm3 + 有 key | `hmac-sm3:ab12…` | HMAC-SM3 |

SM3 与 SHA-256 同为 32B→64 hex，无需改列宽/JSON 契约。**历史行无前缀 → 验算时走原 SHA-256 口径，零回填即可验**；新行带前缀 → 验算时按前缀选算法。混合链逐行各按各的算法验。

### 10.2 正确性要点（对应勘察标出的 3 个风险）

- 前缀**不进 canonical 字段集**，只是摘要值的显示前缀——历史行的摘要字节不变（风险 A 规避）。
- `legacyHash`/`legacyChecksum`（V1 管道编码）**固定 SHA-256**，不随当前模式漂移——历史 V1 行不会被误判（风险 B 规避）。
- **零 schema / 触发器改动**——没有 digest_mode 列，两套 DB 迁移、4 个旧版本 fixture、WORM 触发器全部不动（风险 C 规避）。

### 10.3 验算分流（行驱动，非配置驱动）

- `internal/audit/canonical.go`：`digest()`（历史 sha256，签名不变）+ `digestWithMode()`（按模式选 hash + 前缀）+ `recomputeCanonical()`（按存储前缀选算法，未知前缀 fail-closed）+ `verifyTraceHash`/`verifyAuditHash`/`verifyWORMChecksum`。
- `verify.go` / `hashchain.go` / `worm.go` / `auditchain.go` 的验链路径全部改为按**每行存储 hash 的前缀**重算，而不是按进程配置模式——这是混合链可验的关键。
- 写入路径 `ComputeChecksum`/`ComputeHashV2`/`ComputeAuditHash` 改用 `digestForCurrent()`（配置模式），新行据此带前缀。

### 10.4 HMAC key 与算法模式正交

4 组合（SHA-256/HMAC-SHA256/SM3/HMAC-SM3）均可产出与验算。`hmackey_switch_test.go` 的 ratchet（加 key 断历史链）**仍成立**——key epoch 仍无 per-row 标记（D-8 原本就只解决算法维度，不解决 key 轮换）。测试注释订正为"无 key epoch marker"。

### 10.5 配置与可观测性

- `audit.digest_mode: sha256|sm3`（默认 sha256，行为不变）；`LEVEE_AUDIT_DIGEST_MODE` 环境变量绑定；非法值启动即拒。
- `serve` 启动时 `audit.SetDigestMode(cfg.Audit.DigestMode)` 固定进程模式（一次性，类 HMAC key）。
- `GET /audit/chain-mode` 保留原三字段（`mode`/`algorithm`/`keyed`），新增 `digest_mode` + `digest_algorithm`（SM3/HMAC-SM3）——扩展非替换，前端旧消费方不受影响。

### 10.6 验证

- 新增 `internal/audit/sm3_migration_test.go`：摘要格式、keyed 正交、前缀解析 fail-closed、**混合链验真（在另一种配置模式下验）**、SM3 篡改检测、audit 链混合验真、SM3 已知答案。
- `internal/grpc/rest_chain_mode_test.go` 扩展 digest_mode 断言（unkeyed/keyed × sha256/sm3）。
- `go test ./...` 全绿；全部历史审计测试（含 ratchet、V1 回退、WORM）保持通过。

### 10.7 边界

- 这是**算法可迁移性**，不是密评合规；TLCP/国密 TLS、SM2、硬件密码模块仍是专项（见 `docs/tender-evidence-pack-2026-11.md` M-04）。
- 对外可说"审计链支持 SM3 摘要模式、历史链可验"，不得说"国密合规 / 已过密评"。

---

## 11. 第二维实现记录：HMAC key epoch（2026-11-23）

### 11.1 问题（D-8 原本登记的缺口）
keyed 摘要由部署级 `LEVEE_AUDIT_HMAC_KEY` 算出，链记录不带 key 标识。运维换 key 后，历史 keyed 行无法复算，验链器一律报 `ErrHashMismatch`——**分不清"换了 key"与"被篡改"**，这在审计产品里是签字前必被追问的一问。

### 11.2 方案：key 指纹前缀（自描述 epoch）
新写入的 keyed 摘要嵌入 key 的短指纹（key 字节的 SHA-256 前 8 hex）：
- keyed SHA-256：`k<8hex>:<64hex>`
- keyed SM3：`hmac-sm3:k<8hex>:<64hex>`（算法前缀在前，兼容 SM3 测试与历史 `hmac-sm3:<hex>`）
- unkeyed 摘要不变（无 epoch 可标）

验算按存储值自带的 epoch 前缀比对当前 key 指纹：不符 → `ErrKeyEpochMismatch` / `FailureKeyEpochMismatch`（字符串 `key_epoch_mismatch`）；相符后再验内容，不过 → `FailureHashMismatch`（真篡改）。**两者在 REST `/audit/verify` 的失败类型里明确区分**。

### 11.3 边界（must state）
- `hmackey_switch_test.go` ratchet 仍成立：它测的是"无 key 历史行 + 事后加 key"——无 epoch 标记可依，仍报 mismatch。这是**迁移前数据的固有限制**，不是回归。
- 带 epoch 的新行由 `key_epoch_test.go` 覆盖（换 key → key_epoch_mismatch、key 移除 → key_epoch_mismatch、SM3 keyed 往返、chain 内失败类型）。
- 对存量无标记数据，归档旧库 + 新 key 开新链仍是唯一受支持路径（`docs/deployment.md` §5.6）。
- key epoch 与算法模式正交：`sm3:k<fp>:<hex>` 同时携带两种前缀。

### 11.4 可观测性
`GET /audit/chain-mode` 新增 `key_epoch` 字段（当前 key 的 8-hex 指纹，unkeyed 为空）——控制台可显示"新行正以哪个 key epoch 写入"；历史行的 epoch 从各自的前缀读取。

### 11.5 验证
`go test ./...` 全绿：新增 key_epoch_test.go，sm3_migration_test.go 的 keyed 断言更新为带 epoch 格式，ratchet/V1 回退/WORM 全部历史测试保持通过。proto 注释与 docs/levee-api.md 失败类型列表同步（type 3 = key_epoch_mismatch）。
- 算法切换后长期存在混合链（历史 sha256 + 新 sm3），属设计预期（方案 A），验链器逐行处理。
