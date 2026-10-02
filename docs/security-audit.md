# LEVEE 安全审计报告

## 审计范围

本次审计覆盖 LEVEE MVP 阶段的三个安全关键模块：

| 模块 | 文件 | 行数 | 职责 |
|------|------|------|------|
| `internal/credential/` | `store.go`, `provider.go` | 637 | AES-GCM 加密存储 + argon2id 密钥派生 + 按需获取 |
| `internal/permission/` | `matrix.go`, `checker.go` | 677 | 团队×环境权限矩阵 + 权限校验 |
| `internal/audit/` | `trace.go`, `hashchain.go`, `worm.go`, `verify.go`, `auditchain.go`, `record.go` | 1119 | 审计 trace + SHA-256 哈希链（trace 按 run、audit 全局）+ WORM 存储 + 校验 |

同时审查了支撑层：`internal/state/`（SQLite 持久化）、`internal/log/`（日志）。

审计日期：2026-08-16
审计方法：静态代码审查，逐文件逐函数分析

---

## 审计发现

### CRITICAL（严重）

#### [SA-001] WORM 存储可被底层 SQLite 绕过——trace 表仍暴露 Update/Delete 接口 [已修复 v1.0.0]

**位置**：`internal/state/store.go:205-207`，`internal/state/sqlite.go:511-581`

**描述**：`WORMStore` 仅在应用层封装了 append-only 语义（不暴露 Update/Delete 方法），但底层 `state.Store` 接口和 `SQLiteStore` 实现仍然提供了 `UpdateTrace` 和 `DeleteTrace` 方法。任何持有原始 `state.Store` 引用的代码（包括同一进程中的其他模块）都可以直接调用 `store.UpdateTrace()` 或 `store.DeleteTrace()` 来篡改或删除审计记录，完全绕过 WORM 保护。

**证据**：
- `state.Store` 接口定义了 `UpdateTrace` 和 `DeleteTrace`（`store.go:205-207`）
- `SQLiteStore` 实现了这两个方法（`sqlite.go:511-527`, `sqlite.go:574-581`）
- `WORMStore` 的注释也承认了这一点（`worm.go:45-46`）："The underlying store may still allow them (e.g. for administrative recovery)"

**风险**：内部攻击者或被入侵的模块可静默篡改审计记录而不被 WORM 层检测。

**修复建议**：
1. 为 WORM 场景创建独立的 `WORMStore` 接口，不包含 Update/Delete 方法
2. 在 SQLite 层使用触发器（`CREATE TRIGGER ... BEFORE UPDATE ON trace ... RAISE`）强制阻止 UPDATE/DELETE
3. 或将 trace 表的数据库文件设为只读追加（append-only filesystem flag）

---

#### [SA-002] 哈希链可被 Build 重建——篡改后重建链将销毁证据 [已修复 v1.0.0]

**位置**：`internal/audit/hashchain.go:64-78`（`Build` 方法）

**描述**：`HashChainBuilder.Build()` 会覆盖所有 trace 记录的 `PrevHash` 和 `CurrHash`。如果攻击者先篡改 trace 内容再调用 `Build`，篡改将被"合法化"——新链完全基于篡改后的内容重新计算，校验将通过。`Build` 没有检查现有链是否已存在且完整，也没有要求提供前一次的 tail hash 作为锚点。

**证据**：
- `buildChainWithPrev` 直接设置 `t.PrevHash = current` 和 `t.CurrHash = ComputeHash(t, current)`（`hashchain.go:138-139`）
- 测试 `TestBuild_TamperDetectedViaRebuild` 验证了重建后链"正确"（`hashchain_test.go:322-357`），但这恰恰说明重建会覆盖篡改痕迹

**风险**：攻击者可以"先篡改、再重建"来消除所有篡改证据，使审计链完全失效。

**修复建议**：
1. `Build` 应在执行前先调用 `Verify`，如果链已存在且完整则拒绝重建
2. 保存每次 Build 的 tail hash 到受信任的外部存储（如签名文件），Build 时验证锚点
3. 引入不可逆的链锚定机制（如定期将 tail hash 写入外部不可篡改系统）

---

#### [SA-003] 凭据主密钥无轮换机制——主密码泄露将导致所有凭据暴露 [已修复 v1.0.0]

**位置**：`internal/credential/store.go:83-89`

**描述**：`CredentialStore` 将主密码保存在内存中，用于所有凭据的密钥派生。但没有任何主密码轮换机制：一旦主密码泄露，攻击者可以解密所有已存储的凭据密文（因为 salt 存储在密文 blob 中）。`Rotate` 方法（`store.go:338-371`）仅轮换单个凭据的明文，不涉及主密码轮换。

**证据**：
- `masterPassword` 字段在 `NewCredentialStore` 时设置后不再变更（`store.go:120-121`）
- `Rotate` 方法仅替换 `EncryptedData`，不重新派生密钥（`store.go:357-367`）
- 无任何方法可以更换 `masterPassword` 而不重新加密所有凭据

**风险**：主密码泄露 = 全部凭据泄露，且无法通过轮换主密码来缓解。

**修复建议**：
1. 实现 `RotateMasterPassword(oldPW, newPW)` 方法，遍历所有凭据用旧密码解密、用新密码重新加密
2. 考虑使用硬件安全模块（HSM）或操作系统密钥链（如 OS keychain）保护主密码
3. 支持主密码的定期轮换策略

---

### HIGH（高危）

#### [SA-004] SecureZero 可能被编译器优化掉 [已修复 v1.11.0]

**位置**：`internal/credential/store.go:216-220`

**描述**：`SecureZero` 使用简单的 for 循环将字节清零。Go 编译器的优化器（特别是启用 `-O2` 时）可能识别出清零后的数据不再被读取，从而将整个清零操作优化掉。虽然 `provider.go:218` 中对 `cred.Plaintext` 调用了 `runtime.KeepAlive`，但 `store.go:151` 和 `store.go:189` 中对派生密钥 `key` 的 `SecureZero` 调用没有 `KeepAlive` 保护。

**证据**：
- `SecureZero` 注释已承认此风险（`store.go:213-215`）："Go's escape analysis and GC may copy slice data, so this is a best-effort wipe"
- `encrypt` 方法中 `defer SecureZero(key)` 后 `key` 不再被引用（`store.go:151`），编译器可能优化掉
- `decrypt` 方法同理（`store.go:189`）

**修复建议**：
1. 使用 `crypto/subtle` 包或内联汇编实现不可优化的清零
2. 在 `SecureZero` 末尾添加 `runtime.KeepAlive(b)` 防止优化
3. 考虑使用 Go 1.24+ 的 `crypto/mlkem` 风格的清零模式

---

#### [SA-005] argon2id 参数偏低——OWASP 最低推荐不足以应对 GPU 攻击 [已修复 v1.11.0]

**位置**：`internal/credential/store.go:46-50`

**描述**：当前 argon2id 参数为 `time=3, memory=64MiB, parallelism=4`。虽然注释声称符合 OWASP 推荐最低值，但 OWASP 2024 年推荐已更新为 `time=3, memory=194MiB (65536 KiB × 3), parallelism=4`。64MiB 的内存成本在现代 GPU（如 RTX 4090 有 24GB VRAM）面前偏低，攻击者可并行运行数百个 argon2 实例进行暴力破解。

**证据**：
- `defaultMemoryCost = 64 * 1024`（`store.go:48`）= 64 MiB
- OWASP 2024 推荐 memory ≥ 194 MiB（即 3 × 64 MiB）
- 当前参数对高端 GPU 攻击的防御力不足

**修复建议**：
1. 将 `defaultMemoryCost` 提升至至少 `194 * 1024`（194 MiB）
2. 允许通过配置文件覆盖 argon2 参数，以便生产环境使用更强的参数
3. 考虑 `time=4, memory=256MiB, parallelism=4` 作为生产默认值

---

#### [SA-006] 权限矩阵非线程安全——并发读写可导致数据竞争 [已修复 v1.11.0]

**位置**：`internal/permission/matrix.go:83-90`

**描述**：`PermissionMatrix` 的 `grants` 和 `revokes` 字典没有任何互斥保护。`Grant`/`Revoke`/`LoadFromConfig` 会修改这些字典，而 `Allow`/`ActionsFor`/`Teams`/`Environments` 会读取它们。如果配置热加载（调用 `LoadFromConfig`）与权限检查并发执行，将产生数据竞争（Go race condition），可能导致权限检查返回错误结果或程序崩溃。

**证据**：
- `PermissionChecker` 的注释声明"A PermissionChecker is safe for concurrent use as long as the underlying PermissionMatrix is not mutated after construction"（`checker.go:45-46`），但这只是约定，非强制
- `LoadFromConfig` 重置整个 grants/revokes 字典（`matrix.go:133-134`），与并发读取不兼容
- Go 的 map 并发读写会直接 panic

**修复建议**：
1. 在 `PermissionMatrix` 中添加 `sync.RWMutex`，`Allow` 等读操作用 `RLock`，`Grant`/`Revoke`/`LoadFromConfig` 用 `Lock`
2. 或将 `PermissionMatrix` 设计为不可变——`LoadFromConfig` 返回新实例而非修改现有实例
3. 在文档中明确标注线程安全保证

---

#### [SA-007] 权限校验缺少操作级审计——拒绝决策未自动记录审计 trace [部分修复+边界]

**位置**：`internal/permission/checker.go:130-161`

**描述**：`PermissionChecker.Check` 在权限被拒绝时返回 `PermissionDeniedError`，但自身不记录任何审计 trace。审计 trace 的记录完全依赖调用方。如果调用方忘记或选择不记录拒绝事件，权限拒绝将无审计痕迹，违反安全合规要求（"deny by default + audit all denials"）。

**证据**：
- `Check` 方法仅返回错误，不调用 `audit.TraceRecorder`（`checker.go:154-160`）
- `PermissionDeniedError` 包含完整的审计信息（actor, team, env, action），但这些信息仅存在于返回值中
- 无任何机制保证拒绝事件被记录

**修复建议**：
1. 在 `PermissionChecker` 中注入 `audit.TraceRecorder`，`Check` 在拒绝时自动记录审计 trace
2. 或提供 `CheckWithAudit` 方法，同时执行权限检查和审计记录
3. 至少在文档中强制要求调用方记录所有拒绝事件

---

#### [SA-008] 哈希链排序依赖时间戳——相同时间戳的记录顺序不确定 [已修复 v1.11.0]

**位置**：`internal/audit/hashchain.go:69`，`internal/state/sqlite.go:548`

**描述**：哈希链的构建依赖 `ListTraces` 返回的顺序，而排序依据是 `timestamp ASC`（`sqlite.go:548`）。如果两条 trace 记录具有相同的时间戳（SQLite 的 DATETIME 精度为秒级），它们的顺序是不确定的。顺序不同会导致完全不同的哈希链，使校验结果不可预测。

**证据**：
- `ListTraces` 的 SQL 为 `ORDER BY timestamp ASC`（`sqlite.go:548`），无二级排序
- 测试中通过 `time.Sleep(2 * time.Millisecond)` 避免此问题（`hashchain_test.go:49-51`），但生产环境可能在高并发下产生相同时间戳
- `Record` 使用 `time.Now().UTC()`（`trace.go:138`），高频调用可能返回相同时间

**修复建议**：
1. 将 `ORDER BY` 改为 `ORDER BY timestamp ASC, id ASC`，确保确定性排序
2. 或在 trace 记录中添加单调递增的序列号作为二级排序键

---

### MEDIUM（中危）

#### [SA-009] 敏感字段脱敏列表不完整——可能遗漏自定义敏感字段 [已修复 v1.13.0]

**位置**：`internal/audit/trace.go:51-60`

**描述**：`sensitiveFields` 列表包含 8 个常见敏感字段名（password, passwd, key, token, secret, credential, private_key, api_key），但无法覆盖所有可能的敏感字段。例如 `ssh_key`、`passphrase`、`auth_code`、`refresh_token`、`access_token`、`connection_string` 等常见敏感字段未被包含。此外，`key` 字段过于宽泛，可能误脱敏非敏感的 `key` 字段（如 `sort_key`、`primary_key`）。

**证据**：
- `sensitiveFields` 硬编码了 8 个字段名（`trace.go:51-60`）
- 无配置化扩展机制
- `key` 匹配过于宽泛，`isSensitive` 使用 `strings.ToLower`（`trace.go:274`）

**修复建议**：
1. 扩展敏感字段列表，增加 `passphrase`、`auth_code`、`refresh_token`、`access_token`、`connection_string`、`ssh_key`、`cert`、`certificate` 等
2. 将 `key` 改为更精确的模式匹配（如 `private_key`、`secret_key`、`encryption_key`），避免误脱敏
3. 支持通过配置文件自定义敏感字段列表

---

#### [SA-010] 脱敏仅覆盖 map[string]any——结构体中的敏感字段不受保护 [已修复 v1.13.0]

**位置**：`internal/audit/trace.go:260-270`

**描述**：`Redact` 函数仅递归处理 `map[string]any` 类型。如果 `Input`/`Output` 中包含结构体、切片或其他非 map 类型中的敏感字段，这些字段不会被脱敏。例如 `Input: map[string]any{"config": someStruct{Password: "secret"}}` 中的 `Password` 字段将原样进入审计 trace。

**证据**：
- `redactValueForKey` 仅对 `map[string]any` 递归（`trace.go:264-267`）
- 其他类型（struct、slice）直接返回原始值（`trace.go:268`）
- JSON 序列化后结构体字段会出现在 Detail 中

**修复建议**：
1. 对 JSON 序列化后的字符串执行正则脱敏，匹配常见敏感模式
2. 或要求调用方在传入前自行脱敏，并在文档中明确说明限制
3. 考虑使用反射遍历结构体字段进行脱敏

---

#### [SA-011] 凭据明文在 Retrieve 后的生命周期不受控 [部分修复+边界]

**位置**：`internal/credential/store.go:275-296`

**描述**：`CredentialStore.Retrieve` 返回凭据明文 `[]byte`，但调用方没有义务调用 `SecureZero` 清除。与 `CredentialProvider.Clear` 的"用完即弃"语义不同，直接使用 `Retrieve` 的调用方可能长期持有明文引用，导致凭据在内存中驻留过久。

**证据**：
- `Retrieve` 注释仅建议"use SecureZero on it as soon as it is no longer needed"（`store.go:269-270`），但无强制机制
- 测试中手动调用 `SecureZero(got)`（`store_test.go:94`），说明需要调用方配合
- 与 `Provider.Clear` 的自动清零形成对比

**修复建议**：
1. 提供 `RetrieveWithCallback` 方法，接受一个回调函数，在回调执行后自动清零明文
2. 或将 `Retrieve` 标记为内部方法，外部调用统一走 `CredentialProvider`
3. 在文档中用 MUST 级别强调调用方清零义务

---

#### [SA-012] newID 在 rand.Read 失败时降级为时间戳——可预测性风险 [已修复 v1.13.0]

**位置**：`internal/credential/store.go:379-385`

**描述**：`newID` 在 `crypto/rand.Read` 失败时降级为 `fmt.Sprintf("cred-%d", time.Now().UnixNano())`。时间戳 ID 是可预测的，且在纳秒精度下仍可能碰撞（高并发场景）。如果攻击者能触发 rand 失败（如耗尽文件描述符），可预测的 ID 可能被用于凭据枚举攻击。

**证据**：
- 降级逻辑在 `store.go:381-383`
- `audit/trace.go:303-308` 的 `newID` 在 rand 失败时直接返回错误，不降级——两种策略不一致

**修复建议**：
1. 与 `audit/trace.go` 保持一致：rand 失败时返回错误而非降级
2. 或使用 `uuid.New()` 作为降级方案（仍有随机性保证）

---

#### [SA-013] 权限矩阵的通配符 "admin" 超集可能意外扩大权限 [已修复 v1.13.0]

**位置**：`internal/permission/matrix.go:199-204`

**描述**：当团队在某个环境上拥有 `admin` 权限时，`Allow` 自动允许该团队在该环境上执行所有非 `admin` 操作。如果配置文件中意外授予了 `admin`（如通配符环境 `*` + `admin`），将导致该团队在所有环境上拥有所有权限，构成提权风险。

**证据**：
- `Allow` 的步骤 3（`matrix.go:199-204`）：`if action != ActionAdmin && m.lookup(m.grants, team, env, ActionAdmin) { return true }`
- 示例配置中 `security` 团队在 `*` 环境上有 `admin`（`matrix_test.go:33-37`），意味着 security 团队在所有环境上拥有所有权限
- 虽然 `Revoke` 可以覆盖 admin 超集，但需要显式配置

**修复建议**：
1. 在 `LoadFromConfig` 中对 `admin` + 通配符环境的组合发出警告
2. 考虑将 admin 超集限制为仅扩展预定义的子集（而非 AllActions）
3. 在文档中明确说明 admin 超集的语义和风险

---

#### [SA-014] WORM Append 存在 TOCTOU 竞态——存在性检查与写入非原子 [已修复 v1.13.0；台账降档 LOW]

**位置**：`internal/audit/worm.go:82-98`

**描述**：`Append` 先调用 `GetTrace` 检查 ID 是否已存在（`worm.go:82-88`），然后调用 `CreateTrace` 写入（`worm.go:95`）。在并发场景下，两个协程可能同时通过存在性检查，然后都尝试写入相同 ID。虽然 SQLite 的 UNIQUE 约束会在第二个写入时返回错误，但该错误不是 `ErrAlreadyExists`，而是底层的 SQL 约束违反错误，调用方无法正确识别为 WORM 重复写入。

**证据**：
- `existing, err := w.store.GetTrace(ctx, trace.ID)`（`worm.go:82`）
- `w.store.CreateTrace(ctx, trace)`（`worm.go:95`）
- 两步操作之间无事务保护

**修复建议**：
1. 将存在性检查和写入包裹在单个数据库事务中
2. 或在 `CreateTrace` 返回 UNIQUE 约束错误时，将其转换为 `ErrAlreadyExists`
3. 使用 `INSERT OR IGNORE` + 检查 `RowsAffected()` 实现原子性

---

### LOW（低危）

#### [SA-015] 凭据密文 blob 格式无版本标识——未来算法迁移困难 [已修复 v1.13.0]

**位置**：`internal/credential/store.go:143-176`

**描述**：加密 blob 格式为 `salt(16) || nonce(12) || ciphertext`，没有版本前缀。如果未来需要更换加密算法（如从 AES-256-GCM 迁移到 XChaCha20-Poly1305），`decrypt` 函数无法区分旧格式和新格式，需要破坏性迁移。

**证据**：
- `encrypt` 生成的 blob 无版本前缀（`store.go:171-175`）
- `decrypt` 假设固定偏移量解析 blob（`store.go:184-186`）

**修复建议**：
1. 在 blob 开头添加 1 字节版本号：`version(1) || salt(16) || nonce(12) || ciphertext`
2. `decrypt` 根据版本号选择解密路径

---

#### [SA-016] 权限矩阵不验证 action 名称——任意字符串均可作为权限 [已修复 v1.13.0]

**位置**：`internal/permission/matrix.go:368-385`

**描述**：`Grant` 和 `Revoke` 接受任意字符串作为 action，不验证是否为 `AllActions` 中的已知 action。这意味着拼写错误（如 `"aply"` 代替 `"apply"`）会静默创建无效权限，且无法被检测。

**证据**：
- `Grant` 无 action 验证（`matrix.go:368-373`）
- `LoadFromConfig` 也不验证 action（`matrix.go:144-149`）
- `AllActions` 定义了 11 个合法 action（`matrix.go:46-58`），但不强制使用

**修复建议**：
1. 在 `Grant` 中添加 action 白名单验证，未知 action 返回错误
2. 或在 `LoadFromConfig` 中对未知 action 发出警告
3. 提供严格模式选项，拒绝未知 action

---

#### [SA-017] 哈希链使用 SHA-256 而非 HMAC——无法证明链的来源真实性 [已修复 v1.11.0（opt-in：`LEVEE_AUDIT_HMAC_KEY`）]

**位置**：`internal/audit/hashchain.go:156-171`

**描述**：`ComputeHash` 使用纯 SHA-256 哈希构建链，没有密钥参与。这意味着任何知道 trace 内容的人都可以重新计算正确的哈希链。虽然链的完整性（防篡改）得到了保证，但来源真实性（证明链由 LEVEE 系统生成）无法保证。攻击者可以构造一条完整的伪造链。

**证据**：
- `ComputeHash` 使用 `sha256.Sum256([]byte(payload))`（`hashchain.go:169`）
- 无密钥或签名参与

**修复建议**：
1. 考虑使用 HMAC-SHA256 替代纯 SHA-256，密钥由受信任源管理
2. 或对链的 tail hash 进行数字签名
3. 当前方案对内部防篡改已足够，但对外部证明力不足

---

#### [SA-018] 凭据 Tags 字段未持久化——可能包含安全元数据 [已修复 v1.13.0]

**位置**：`internal/credential/store.go:98`

**描述**：`CredentialSpec.Tags` 注释为"当前未持久化，保留以供未来扩展"。如果 Tags 中包含安全相关元数据（如 `env=prod`、`classification=confidential`），这些信息在存储后会丢失，无法在后续查询中使用。

**证据**：
- `Tags` 字段注释（`store.go:98`）
- `Store` 方法不保存 Tags（`store.go:249-255`）

**修复建议**：
1. 如果 Tags 包含安全元数据，应持久化到数据库
2. 或在文档中明确 Tags 的用途限制

---

#### [SA-019] SQLite synchronous=NORMAL——极端情况下可能丢失最近写入 [已修复 v1.13.0]

**位置**：`internal/state/sqlite.go:56`

**描述**：`PRAGMA synchronous=NORMAL` 在 WAL 模式下是安全的（不会损坏数据库），但在操作系统崩溃（非 SQLite 进程崩溃）时，可能丢失最近几秒的 WAL 写入。对于审计 trace，这意味着最近记录的审计事件可能在系统崩溃后丢失。

**证据**：
- `synchronous=NORMAL` 设置（`sqlite.go:56`）
- SQLite 文档：NORMAL 在 WAL 模式下安全，但崩溃时可能丢失最近的 WAL 帧

**修复建议**：
1. 对审计关键写入使用 `PRAGMA synchronous=FULL`（性能代价约 1-2%）
2. 或在每次关键审计写入后执行显式 checkpoint

---

### INFO（信息）

#### [SA-020] AES-256-GCM nonce 使用 crypto/rand——实现正确 [无需修复：实现正确]

**位置**：`internal/credential/store.go:162-165`

**描述**：每次加密都使用 `crypto/rand.Read` 生成 12 字节随机 nonce，且每条凭据使用独立的 salt 派生密钥。即使 nonce 碰撞（概率极低，约 2^-96），由于密钥不同也不会导致 AES-GCM 的灾难性 nonce 重用问题。实现正确。

---

#### [SA-021] argon2id 使用 per-credential salt——实现正确 [无需修复：实现正确]

**位置**：`internal/credential/store.go:145-148`

**描述**：每条凭据使用 16 字节随机 salt 派生独立密钥。即使两条凭据明文相同，由于 salt 不同，密文也不同（测试 `TestCiphertextIndependence` 验证了这一点）。实现正确。

---

#### [SA-022] 日志不记录凭据明文——实现正确 [无需修复：实现正确]

**位置**：`internal/credential/store.go:260-264`, `internal/credential/provider.go:170-171`

**描述**：所有日志仅记录凭据名称和类型，不记录明文或密文。`credential` 包不调用 `audit.TraceRecorder`。实现正确。

---

#### [SA-023] 权限矩阵默认拒绝——实现正确 [无需修复：实现正确]

**位置**：`internal/permission/matrix.go:184-207`

**描述**：`Allow` 在无匹配 grant 时返回 `false`（步骤 4）。空 team/env/action 也返回 `false`。admin 超集可以被显式 `Revoke` 覆盖。默认拒绝语义正确。

---

#### [SA-024] 审计 trace 的 Input/Output 自动脱敏——实现正确 [无需修复：实现正确]

**位置**：`internal/audit/trace.go:282-298`

**描述**：`buildDetail` 在序列化前对 Input/Output 调用 `Redact`，敏感字段被替换为 `[REDACTED]`。递归脱敏支持嵌套 map。大小写不敏感匹配。实现正确。

---

#### [SA-025] WORM 校验机制能检测内容篡改——实现正确 [无需修复：实现正确]

**位置**：`internal/audit/worm.go:146-175`

**描述**：`computeChecksum` 覆盖所有 trace 内容字段（ID, RunID, Event, Actor, Detail, Timestamp），`verifyChecksum` 在每次读取时重新计算并比对。篡改任何字段都会被检出。实现正确。

---

#### [SA-026] ChainVerifier 能检测多种篡改类型——实现正确 [无需修复：实现正确]

**位置**：`internal/audit/verify.go:172-214`

**描述**：`checkTrace` 按优先级检测三种篡改：空哈希（未构建链）、PrevHash 断裂（插入/删除记录）、CurrHash 不匹配（内容篡改）。实现正确。

---

#### [SA-027] audit 动作日志无防篡改——既无 WORM 触发器也无哈希链 [已修复 v1.14.0]

**位置**：`internal/state/schema.sql:251-269`（WORM 触发器），`internal/state/migrate.go:186-227`（v7 迁移步），`internal/audit/auditchain.go`（链构建与校验），`internal/audit/record.go`（写入即封链），`internal/grpc/rest.go:1307-1431`（`/audit/verify`）

**描述**：`trace` 表自 v1.0.0 起即受 WORM 触发器与按 run 哈希链保护（SA-001 / SA-002），但 `audit` 表——即 `GET /audit/log` 实际服务的"谁对哪个目标做了什么"高层动作日志——两者皆无。它此前只是**因为 Store 接口上恰好没有 update 方法**才表现为 append-only，而 DBA 没有义务尊重一个 Go 接口。

**修复要点**：

1. schema v7（PG v6）为 `audit` 增加 `prev_hash` / `curr_hash` 与 `idx_audit_chain (timestamp, id)`，并加 UPDATE / DELETE 触发器。`tenant_id` 纳入不可变列——否则一次 UPDATE 就能把审计记录搬进别的租户而链毫无反应。
2. 链是**全局**而非按 run：`run_id` 为空的 audit 行恰是 login / config / credential 这类安全相关性最高的记录，按 run 建链结构上覆盖不到。多租户开启时链的范围即 Store 暴露的范围（`TenantStore` 注入租户谓词），表现为每租户一条链——范围收窄，不是漏洞。
3. 链按存储的 `(timestamp, id)` 升序**重算**，而非写入时向链尾追加。原因是 audit id 为 8 字节随机 hex（`internal/grpc/change_service.go:169`），同毫秒写入的两行按 id 排序本质随机；追加式封链会把后插入的行挂到先插入的行上，直到验证时才以"顺序相反"暴露。重算式封链是幂等的：两个并发 `Seal` 对重叠行必然导出相同哈希，因此不会分叉（`TestAuditChain_ConcurrentSealIsSafe` 覆盖）。
4. **写入路径真正封链**：`audit.Record` 在 `CreateAudit` 之后立即 `Seal`，10 处生产调用点全部改走它（`change_service` / `rest_gate` / `lock` / `pause`×2 / `template` / `cmd`×4）；非测试代码中的裸 `store.CreateAudit` 已清零。这一步是必需的：只加列和触发器而不封链，验证会永远报 `empty_hash`，或者更糟——被改成"忽略空哈希"，那恰恰是留给 DBA 的盲区。
5. `GET /audit/verify` 响应新增 `auditChain` 成员并折叠进顶层 `valid`。**未改 proto**：`levee.proto` 的 `VerifyHashChainResponse` 字段全是 run 维度的，且本机无 `protoc` 无法重新生成，硬塞进 `RunVerification` 会误报其覆盖范围。

**已知边界**：

- 链能检出篡改，**挡不住删除**——删掉中间一行后，后一行指向的哈希已无来源。所以 WORM 触发器是配套而非冗余（`TestAuditChain_DetectsDeletion` 先摘触发器再删，验证必须报 `prev_hash_mismatch`）。
- `Seal` 每次全量读取可见的 audit 行（无 LIMIT），稳态下只写新增行，但读成本随日志增长。超大审计库需要改成分批封链。
- **trace 链的构建器在生产中仍然零接线**：`HashChainBuilder` 的全部调用点都在 `tests/integration/*` 与 `*_test.go` 内，生产代码从未调用 `Build` / `BuildBatch` / `BuildForce`——即按 run 的 trace 哈希链至今没有在生产中构建过任何一条。这与 SA-007 同属"机制存在但生产零接线"，本轮**未修**（改动会触及 trace 写入路径，超出本次范围），登记为已知限制而非宣称已闭环。
- PG 侧（plpgsql 版触发器、占位符编号、v6 迁移步）本机无实例，**未执行验证**，仅经编译期检查与逐行比对。

---

## 总结

| 严重级别 | 数量 | 编号 |
|----------|------|------|
| CRITICAL | 3 | SA-001, SA-002, SA-003 |
| HIGH | 5 | SA-004, SA-005, SA-006, SA-007, SA-008 |
| MEDIUM | 6 | SA-009, SA-010, SA-011, SA-012, SA-013, SA-014 |
| LOW | 5 | SA-015, SA-016, SA-017, SA-018, SA-019 |
| INFO | 7 | SA-020 ~ SA-026 |
| **总计** | **26** | |

### 整体评估

LEVEE 的三个安全模块在密码学选型（AES-256-GCM + argon2id）和基础安全架构（默认拒绝、凭据不进日志/trace、审计脱敏）上做得较好。但存在三个严重问题：

1. **WORM 不可篡改性仅停留在应用层**（SA-001），底层 SQLite 仍可被绕过
2. **哈希链可被重建销毁篡改证据**（SA-002），缺乏锚定机制
3. **主密码无轮换机制**（SA-003），一旦泄露全盘崩溃

这三个 CRITICAL 问题应优先修复。此外，`SecureZero` 可能被优化掉（SA-004）和 argon2 参数偏低（SA-005）也应在下一个迭代中解决。权限矩阵的线程安全（SA-006）和审计记录缺失（SA-007）是生产化前的必要修复项。

#### 修复摘要（26 项全量终态，2026-09-06 更新）

| 编号 | 级别（原→终态） | 状态 | 修复方式 |
|------|----------------|------|----------|
| SA-001 | CRITICAL | 已修复（v1.0.0） | 硬编码 SQLite 触发器阻止 UPDATE/DELETE + WORMStore 接口 |
| SA-002 | CRITICAL | 已修复（v1.0.0） | Build 前先 Verify + 拒绝重建已存在链 + BuildForce 管理恢复 |
| SA-003 | CRITICAL | 已修复（v1.0.0） | RotateMasterPassword 三阶段原子轮换 + SecureZero 清理 |
| SA-004 | HIGH | 已修复（v1.11.0） | SecureZero 末尾 runtime.KeepAlive 防止编译器优化 |
| SA-005 | HIGH | 已修复（v1.11.0） | argon2id memory cost 提升至 194MiB（OWASP 2024）；提参造成的存量断代（v1.11/12 为 194MiB 代、v1.10 前为 64MiB 代）由 v1.13.0 的 blob 版本化收口（见 SA-015） |
| SA-006 | HIGH | 已修复（v1.11.0） | sync.RWMutex 保护并发读写；本轮另修正 checker.go 中过时的"构造后不得变更"注释 |
| SA-007 | HIGH | **部分修复+边界** | v1.11.0 交付的拒绝审计机制核查为**生产零接线**（CHANGELOG"自动记录审计 trace"行文系虚假锚点，已加勘误）；本轮（v1.13.0）CLI 全局暂停/恢复拒绝路径接线落审计表（`permission.denied` 行）；剩余边界：serve 路径不构造 PermissionMatrix（LoadFrom* 仅 cmd_user/cmd_rbac 调用），gRPC 侧接线待生产装配 |
| SA-008 | HIGH | 已修复（v1.11.0） | SQLite/PG 双后端 `ORDER BY timestamp, id` 二级排序键 |
| SA-009 | MEDIUM | 已修复（v1.13.0） | 词表 8→16（新增 passphrase/auth_code/refresh_token/access_token/ssh_key/cert/certificate/connection_string）；`_`/`-` 词边界后缀匹配（`db_password` 命中、`sort_key` 不误伤，裸 `key` 仅全等）；`security.sensitive_fields` 配置扩展 |
| SA-010 | MEDIUM | 已修复（v1.13.0） | reflect 遍历结构体/嵌入/切片/指针（可见性口径 = json.Marshal 可见字段，known-leaf 短路 + 深度上限 8，无命中子树原值透传）；已文档化边界：值内嵌明文（如 error 文本 `secret=…`）无键上下文，键名规则不可覆盖 |
| SA-011 | MEDIUM | **部分修复+边界** | RetrieveInto 回调式读取（defer 清零含 panic 展开路径）+ Retrieve 文档清零义务升为 MUST；残留（已标注、非本轮修复）：serve 凭据解析器的裸密码路径返回 `string`——`CredentialRef.Password` 为 string 类型不可清零，改造波及 ssh/winrm/grpc 通道，登记为已知残留 |
| SA-012 | MEDIUM | 已修复（v1.13.0） | 身份类标识（凭据 ID、deeplink 一次性 token、审批/锁/租户/通知等）rand 失败一律硬失败并逐点传播；纯观测类保留降级并以注释标注分类；change_service 的 panic+recovery 策略评审豁免 |
| SA-013 | MEDIUM | 已修复（v1.13.0） | `admin` + 环境通配 `*` 组合在装载后汇总 WARN 一次并列出受影响团队；config.example.yaml 权限段说明风险 |
| SA-014 | MEDIUM→LOW | 已修复（v1.13.0） | CreateTrace 按后端驱动错误类型（SQLite 约束码集合不含误分类 FK / PG 23505）映射 `ErrTraceExists`，WORM Append 转为 `ErrAlreadyExists`，GetTrace 预检降为快速路径；并发约束测试双后端固化；因竞态仅剩错误语义问题，台账降档 LOW |
| SA-015 | LOW | 已修复（v1.13.0） | blob 前缀版本 + 自描述 KDF 参数：旧密文按自带参数解密、新写入用当前参数，算法/参数迁移不再破坏性 |
| SA-016 | LOW | 已修复（v1.13.0） | Grant/Revoke 未知动作默认 WARN 后仍记录（兼容存量）；`StrictActions` 严格模式整批拒绝、装载原子生效 |
| SA-017 | LOW | 已修复（v1.11.0，opt-in） | V2 canonical 化 + HMAC-SHA256 密钥摘要（`LEVEE_AUDIT_HMAC_KEY` ≥16 字节；未设置时回退无密钥 SHA-256 并 WARN）。实现随 035967a 落入 v1.11.0，当时台账未标注，本轮核查回填；部署文档已补 env 说明 |
| SA-018 | LOW | 已修复（v1.13.0） | `credentials.tags` 经 schema v2 迁移步持久化为 JSON map（schema.sql 全量形状与迁移步双轨、SQLite/PG 一致，v1→v2 升级以手工构建的旧库文件实测）；Rotate/RotateMasterPassword 保留 tags；无 tag 行与历史行不可区分（`''`） |
| SA-019 | LOW | 已修复（v1.13.0） | 新增 `state.sqlite_synchronous = normal|full`（默认 normal；full 每提交 fsync，审计强持久场景，写放大约 1-2%）；非法值启动即拒 |
| SA-020 | INFO | 无需修复 | AES-GCM 随机 nonce + per-credential salt，实现正确 |
| SA-021 | INFO | 无需修复 | argon2id per-credential salt，实现正确 |
| SA-022 | INFO | 无需修复 | 凭据明文不进日志，实现正确 |
| SA-023 | INFO | 无需修复 | 权限矩阵默认拒绝，实现正确 |
| SA-024 | INFO | 无需修复 | trace Input/Output 自动脱敏，实现正确（覆盖面已由 SA-009/010 增强） |
| SA-025 | INFO | 无需修复 | WORM checksum 覆盖全部内容字段，实现正确 |
| SA-026 | INFO | 无需修复 | ChainVerifier 三类篡改检出，实现正确 |
| SA-027 | LOW | 已修复（v1.14.0） | audit 表补 WORM 触发器 + 全局哈希链（schema v7 / PG v6）；链按存储 `(timestamp, id)` 重算封链（audit id 为随机 hex，追加式封链在同毫秒下顺序不可靠），`audit.Record` 写入即封链并覆盖全部 10 处生产调用点；`GET /audit/verify` 增 `auditChain`。**残留**：trace 链构建器生产零接线（仅测试调用）、`Seal` 全量扫描、PG 侧未实测 |

统计（2026-09-06 终态，2026-10-02 增补 SA-027）：26 项中 已修复 17（v1.0.0 ×3、v1.11.0 ×5、v1.13.0 ×9）、部分修复+边界 2（SA-007、SA-011）、无需修复 7（SA-020~026）、未闭环 0；另新增 SA-027（v1.14.0，已修复+残留边界）。

### 2026-09-06 核查记录

本轮对上述 26 项逐条复核（方法与完整设计见 `docs/quality-hardening-2026-09.md`），要点：

- **方法**：每项状态判定由独立核查代理在**当前代码库 + git 历史**上重演，不接受台账或 CHANGELOG 行文作为证据；"已修复"必须指向当前代码证据（文件:行为）或可定位的提交锚点，二者皆无则判"仍有效"。
- **虚假锚点点名**：CHANGELOG v1.11.0"[SA-007] 权限校验拒绝时自动记录审计 trace"与代码事实不符——机制（recorder 注入点）存在但生产路径零接线，已在 CHANGELOG 原处追加勘误（不改写历史行文）。
- **漏标回填**：SA-017 实际已随 v1.11.0 落地（`LEVEE_AUDIT_HMAC_KEY` opt-in HMAC 摘要），台账此前未标注，本轮回填。
- **断代确认**：SA-005 提参确认于 v1.11.0 发布（2026-08-27），其副作用（存量密文 KDF 参数断代，即 SA-015）由本轮 blob 版本化收口。
- **前提证伪**：SA-011 设计稿所称"Retrieve 存在额外明文 string 拷贝"经设计基线（0e8d74f）核对不成立；剩余暴露仅为 `CredentialRef.Password` 的 string 类型残留，按诚实原则登记为已知残留而非宣称修复。
- **降档**：SA-014 经 C-7 消除错误语义问题后，竞态本身仅剩"预检后被约束兜底"的良性路径，台账降档 LOW。
- **本轮修复的测试环境限制**：并发正确性测试以 `-count=5` 重复压力替代 `-race`（该机无 C 工具链，race detector 不可用），CI（linux）仍以 `-race` 为准。

## 部署安全声明（2026-08-22，2026-09-06 修订）

### 已加固项

| 项目 | 说明 |
|------|------|
| 认证启动门禁 | `levee serve` 无 token（`--token` 或 `LEVEE_TOKEN`）拒绝启动；`--insecure` 为显式开发逃生口；无任何凭据源（含 OIDC/GitHub SSO）仍拒绝启动 |
| CORS 默认拒绝 | 空 origins 列表拒绝所有跨域；白名单经 `--cors-origin` 显式配置，通配需显式 `*` |
| gRPC 健康探针 | 标准 `grpc.health.v1.Health` 已注册，免鉴权供编排系统探活 |
| 密码传递 | user 模块密码经通道文件传输（SFTP/SCP 临时文件 + 即时删除），明文不进命令行/sshd 日志/审计 |
| token 比较 | gRPC 与 REST 网关统一使用 `crypto/subtle.ConstantTimeCompare` |
| TLS 明文告警 | 无证书启动时输出 WARN（不强制，兼容 sidecar TLS 终结部署） |
| 权限拒绝审计 | 审计写入失败时输出 ERROR 日志，不再完全静默 |
| REST 方法校验（v1.13.0） | 状态变更路由（plan/apply/approve/…/archive）强制 POST、查询路由（logs/trace）强制 GET——爬虫/预取无法误触状态变更 |
| SSH become_user 注入（v1.13.0） | `buildExecCommand` 对 `become_user` 做 POSIX shell 引用，阻断来自配置值的 `sudo -u` 注入 |
| /metrics 默认鉴权（v1.13.0） | 网关运维端点在配置任一 token 时默认要求 Bearer；`--metrics-public` 显式放开（供无法携带凭据的采集器） |

### 已知限制

- **多租户隔离：已接线，默认关闭**：请求级租户传播已落地——租户取自**已验签的凭据**（命名令牌 `--auth-token name=secret,tenant`、OIDC `auth.oidc.tenant_claim`），经认证拦截器注入上下文，`internal/tenant.TenantStore` 对 10 张租户表施加 SQL 级谓词，76 个 Store 方法全部覆盖（`var _ state.Store` 编译期断言；方法数可用 `awk '/^type Store interface/,/^}/' internal/state/store.go | grep -cE '^\s+[A-Z][A-Za-z0-9]*\('` 复核）。`config.example.yaml` 的 `tenant.enabled` **默认为 false**，关闭时行为与引入该功能前逐字节一致。
  开启前必须知道的三件事：① **fail-closed**——上下文无租户的请求被拒绝而非回落到 default，因此**所有**凭据都必须绑定租户，否则 serve 拒绝启动；② **GitHub SSO 拿不到租户**（OAuth 只证明身份不证明归属），多租户部署须改用命名令牌或 OIDC；③ 后台接管/派发循环（takeover / dispatch）刻意使用未包裹的 store——它们无可用请求上下文，其写操作目前全是**不写 tenant_id** 的窄状态更新；将来若在这些循环中新增 `Create*` 调用，会写出空租户行，需要配套守护测试。
  未覆盖的边角：`ListCredentials` 与 `ListInventoryGroups` 的基础方法签名不带 filter，无法在 SQL 层加谓词，由 `TenantStore` 在内存中按租户过滤（凭据密文为 AES-GCM，过滤发生在任何调用方拿到指针之前）。
- **trace 哈希链在生产中从未构建**：`HashChainBuilder`（`internal/audit/hashchain.go`）的 `Build` / `BuildBatch` / `BuildForce` 全部调用点都位于 `tests/integration/*` 与 `*_test.go`，生产代码零调用——即 SA-002 声称修复的"按 run 哈希链"至今没有在生产中封过一条。`/audit/verify` 校验的 trace 部分因此恒为"无链可验"。与 SA-007 同属"机制存在但生产零接线"，**本轮未修**（改动会触及 trace 写入路径）。相比之下 audit 表的链已真正接线（见 SA-027），因为 audit 的写入是分散在十处的独立调用，链的封口点可以收敛到一个 `audit.Record` 助手，而 trace 的写入走的是 `TraceRecorder` 且涉及 checksum 与链两套字段的先后顺序，需要单独设计。
- **沙箱内存限制**：Unix 平台子进程内存不受限（`setrlimit` 仅作用于宿主进程，见 `internal/plugin/sandbox_unix.go`）；依赖墙钟超时兜底。需要强隔离时请在容器/cgroup 层面限制。
- **速率限制**：REST 网关内置全局限流（令牌桶，`--rate-limit` / `--rate-burst`，429 + `Retry-After`）；**gRPC 原生端口无内置限流**，请在 LB/网关侧实施。
- **审计 Actor 为声明式身份（单令牌模式）/可证明身份（命名令牌或 SSO）**：审计记录中的 Actor 在共享单 token（`--token`/`LEVEE_TOKEN`）模式下来自客户端自报（CLI 端取 `LEVEE_ACTOR` 环境变量，缺省 `cli-user`；服务端从请求元数据读取，缺省 `grpc-user`），是**断言（asserted）而非可证明（proven）**——任何持有 token 的调用方都可自称任意身份。启用命名多令牌（`--auth-token name=secret`）、OIDC 或 GitHub SSO 后，认证主体注入请求上下文并**优先于**自报的 `X-Acting-As`，Actor 成为可证明身份。需要不可抵赖性时须启用上述凭据源之一或 mTLS。
- **Vault 出站 TLS 校验可配置关闭**：Vault Provider 提供 `Insecure` 配置项（`insecure_vault`），用于自签名证书的内网环境。**生产环境必须保持证书校验开启**（`Insecure=false`）；开启即放弃对中间人攻击的防护。
- **file 模块本地读取路径围栏**：`internal/executor/modules/file` 的 copy/template 动作已限制 `src` 只能取进程工作目录内的相对路径；绝对路径或越出工作目录的路径会被拒绝，除非目标目录列入 `LEVEE_FILE_MODULE_EXTRA_DIRS` 允许列表（`os.PathListSeparator` 分隔）。请保持该列表最小化，并通过 RBAC 限制 file 模块的使用面。
- **执行引擎凭据面（v1.13.0）**：`serve --engine-enabled` 的目标通道凭据解析依赖 `LEVEE_MASTER_PASSWORD`（未设置时匿名拨号并输出警告，与目标探测同口径）。SA-011 的已知残留在此路径上同样成立：解析结果经 `CredentialRef.Password`（string）传递、不可清零。执行日志中的命令输出可能包含目标主机回显的敏感内容——审计侧有 `security.sensitive_fields` 脱敏兜底，但目标侧回显的治理（如命令模板避免打印机密）属使用方责任。


**风险评级**（2026-09-08 修订）：3 个 CRITICAL 已在 v1.0.0 修复；5 个 HIGH 中 SA-004/005/006/008 已修复（v1.11.0），SA-007 机制与 CLI 拒绝路径已接线、剩余边界见修复摘要。26 项台账全部终态、无未闭环 HIGH 及以上项（原 Unreleased 批次的 9 项修复已随 **v1.13.0** 发布）。生产准入决策改以上方"已知限制"为约束清单（重点：gRPC 原生端口无限流、单令牌模式 Actor 不可证明、执行引擎凭据面依赖 `LEVEE_MASTER_PASSWORD`）。多租户隔离已接线但默认关闭，开启前的约束见上方对应条目。