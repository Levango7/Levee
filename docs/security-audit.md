# LEVEE 安全审计报告

## 审计范围

本次审计覆盖 LEVEE MVP 阶段的三个安全关键模块：

| 模块 | 文件 | 行数 | 职责 |
|------|------|------|------|
| `internal/credential/` | `store.go`, `provider.go` | 637 | AES-GCM 加密存储 + argon2id 密钥派生 + 按需获取 |
| `internal/permission/` | `matrix.go`, `checker.go` | 677 | 团队×环境权限矩阵 + 权限校验 |
| `internal/audit/` | `trace.go`, `hashchain.go`, `worm.go`, `verify.go` | 908 | 审计 trace + SHA-256 哈希链 + WORM 存储 + 校验 |

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

#### [SA-004] SecureZero 可能被编译器优化掉

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

#### [SA-005] argon2id 参数偏低——OWASP 最低推荐不足以应对 GPU 攻击

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

#### [SA-006] 权限矩阵非线程安全——并发读写可导致数据竞争

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

#### [SA-007] 权限校验缺少操作级审计——拒绝决策未自动记录审计 trace

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

#### [SA-008] 哈希链排序依赖时间戳——相同时间戳的记录顺序不确定

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

#### [SA-009] 敏感字段脱敏列表不完整——可能遗漏自定义敏感字段

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

#### [SA-010] 脱敏仅覆盖 map[string]any——结构体中的敏感字段不受保护

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

#### [SA-011] 凭据明文在 Retrieve 后的生命周期不受控

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

#### [SA-012] newID 在 rand.Read 失败时降级为时间戳——可预测性风险

**位置**：`internal/credential/store.go:379-385`

**描述**：`newID` 在 `crypto/rand.Read` 失败时降级为 `fmt.Sprintf("cred-%d", time.Now().UnixNano())`。时间戳 ID 是可预测的，且在纳秒精度下仍可能碰撞（高并发场景）。如果攻击者能触发 rand 失败（如耗尽文件描述符），可预测的 ID 可能被用于凭据枚举攻击。

**证据**：
- 降级逻辑在 `store.go:381-383`
- `audit/trace.go:303-308` 的 `newID` 在 rand 失败时直接返回错误，不降级——两种策略不一致

**修复建议**：
1. 与 `audit/trace.go` 保持一致：rand 失败时返回错误而非降级
2. 或使用 `uuid.New()` 作为降级方案（仍有随机性保证）

---

#### [SA-013] 权限矩阵的通配符 "admin" 超集可能意外扩大权限

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

#### [SA-014] WORM Append 存在 TOCTOU 竞态——存在性检查与写入非原子

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

#### [SA-015] 凭据密文 blob 格式无版本标识——未来算法迁移困难

**位置**：`internal/credential/store.go:143-176`

**描述**：加密 blob 格式为 `salt(16) || nonce(12) || ciphertext`，没有版本前缀。如果未来需要更换加密算法（如从 AES-256-GCM 迁移到 XChaCha20-Poly1305），`decrypt` 函数无法区分旧格式和新格式，需要破坏性迁移。

**证据**：
- `encrypt` 生成的 blob 无版本前缀（`store.go:171-175`）
- `decrypt` 假设固定偏移量解析 blob（`store.go:184-186`）

**修复建议**：
1. 在 blob 开头添加 1 字节版本号：`version(1) || salt(16) || nonce(12) || ciphertext`
2. `decrypt` 根据版本号选择解密路径

---

#### [SA-016] 权限矩阵不验证 action 名称——任意字符串均可作为权限

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

#### [SA-017] 哈希链使用 SHA-256 而非 HMAC——无法证明链的来源真实性

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

#### [SA-018] 凭据 Tags 字段未持久化——可能包含安全元数据

**位置**：`internal/credential/store.go:98`

**描述**：`CredentialSpec.Tags` 注释为"当前未持久化，保留以供未来扩展"。如果 Tags 中包含安全相关元数据（如 `env=prod`、`classification=confidential`），这些信息在存储后会丢失，无法在后续查询中使用。

**证据**：
- `Tags` 字段注释（`store.go:98`）
- `Store` 方法不保存 Tags（`store.go:249-255`）

**修复建议**：
1. 如果 Tags 包含安全元数据，应持久化到数据库
2. 或在文档中明确 Tags 的用途限制

---

#### [SA-019] SQLite synchronous=NORMAL——极端情况下可能丢失最近写入

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

#### [SA-020] AES-256-GCM nonce 使用 crypto/rand——实现正确

**位置**：`internal/credential/store.go:162-165`

**描述**：每次加密都使用 `crypto/rand.Read` 生成 12 字节随机 nonce，且每条凭据使用独立的 salt 派生密钥。即使 nonce 碰撞（概率极低，约 2^-96），由于密钥不同也不会导致 AES-GCM 的灾难性 nonce 重用问题。实现正确。

---

#### [SA-021] argon2id 使用 per-credential salt——实现正确

**位置**：`internal/credential/store.go:145-148`

**描述**：每条凭据使用 16 字节随机 salt 派生独立密钥。即使两条凭据明文相同，由于 salt 不同，密文也不同（测试 `TestCiphertextIndependence` 验证了这一点）。实现正确。

---

#### [SA-022] 日志不记录凭据明文——实现正确

**位置**：`internal/credential/store.go:260-264`, `internal/credential/provider.go:170-171`

**描述**：所有日志仅记录凭据名称和类型，不记录明文或密文。`credential` 包不调用 `audit.TraceRecorder`。实现正确。

---

#### [SA-023] 权限矩阵默认拒绝——实现正确

**位置**：`internal/permission/matrix.go:184-207`

**描述**：`Allow` 在无匹配 grant 时返回 `false`（步骤 4）。空 team/env/action 也返回 `false`。admin 超集可以被显式 `Revoke` 覆盖。默认拒绝语义正确。

---

#### [SA-024] 审计 trace 的 Input/Output 自动脱敏——实现正确

**位置**：`internal/audit/trace.go:282-298`

**描述**：`buildDetail` 在序列化前对 Input/Output 调用 `Redact`，敏感字段被替换为 `[REDACTED]`。递归脱敏支持嵌套 map。大小写不敏感匹配。实现正确。

---

#### [SA-025] WORM 校验机制能检测内容篡改——实现正确

**位置**：`internal/audit/worm.go:146-175`

**描述**：`computeChecksum` 覆盖所有 trace 内容字段（ID, RunID, Event, Actor, Detail, Timestamp），`verifyChecksum` 在每次读取时重新计算并比对。篡改任何字段都会被检出。实现正确。

---

#### [SA-026] ChainVerifier 能检测多种篡改类型——实现正确

**位置**：`internal/audit/verify.go:172-214`

**描述**：`checkTrace` 按优先级检测三种篡改：空哈希（未构建链）、PrevHash 断裂（插入/删除记录）、CurrHash 不匹配（内容篡改）。实现正确。

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

#### 修复摘要

| 编号 | 级别 | 问题摘要 | 修复版本 | 修复方式 |
|------|------|----------|----------|----------|
| SA-001 | CRITICAL | WORM 存储可被底层 SQLite 绕过 | v1.0.0 | 硬编码 SQLite 触发器阻止 UPDATE/DELETE + WORMStore 接口 |
| SA-002 | CRITICAL | 哈希链可被 Build 重建销毁证据 | v1.0.0 | Build 前先 Verify + 拒绝重建已存在链 + BuildForce 管理恢复 |
| SA-003 | CRITICAL | 主密码无轮换机制 | v1.0.0 | RotateMasterPassword 三阶段原子轮换 + SecureZero 清理 |
| SA-004 | HIGH | SecureZero 可能被编译器优化掉 | Unreleased | runtime.KeepAlive 防止优化 |
| SA-005 | HIGH | argon2id 参数偏低 | Unreleased | memory cost 提升至 194MiB（OWASP 2024） |
| SA-006 | HIGH | 权限矩阵非线程安全 | Unreleased | sync.RWMutex 保护并发读写 |
| SA-007 | HIGH | 权限校验缺少操作级审计 | Unreleased | 拒绝时自动记录审计 trace |
| SA-008 | HIGH | 哈希链排序依赖时间戳 | Unreleased | 添加二级排序键确保确定性 |

## 部署安全声明（2026-08-22）

### 已加固项

| 项目 | 说明 |
|------|------|
| 认证启动门禁 | `levee serve` 无 token（`--token` 或 `LEVEE_TOKEN`）拒绝启动；`--insecure` 为显式开发逃生口 |
| CORS 默认拒绝 | 空 origins 列表拒绝所有跨域；白名单经 `--cors-origin` 显式配置，通配需显式 `*` |
| gRPC 健康探针 | 标准 `grpc.health.v1.Health` 已注册，免鉴权供编排系统探活 |
| 密码传递 | user 模块密码经通道文件传输（SFTP/SCP 临时文件 + 即时删除），明文不进命令行/sshd 日志/审计 |
| token 比较 | gRPC 与 REST 网关统一使用 `crypto/subtle.ConstantTimeCompare` |
| TLS 明文告警 | 无证书启动时输出 WARN（不强制，兼容 sidecar TLS 终结部署） |
| 权限拒绝审计 | 审计写入失败时输出 ERROR 日志，不再完全静默 |

### 已知限制

- **多租户隔离未接线**：`internal/tenant.IsolatedStore` 已实现且测试完备，但 daemon 服务路径（`levee serve`）当前为单租户运行，未按请求接入租户上下文。多租户部署前必须完成 per-request 租户传播的架构改造；在此之前请勿将 `--tenant` 相关能力视为生产可用。
- **沙箱内存限制**：Unix 平台子进程内存不受限（`setrlimit` 仅作用于宿主进程，见 sandbox_unix.go）；依赖墙钟超时兜底。需要强隔离时请在容器/cgroup 层面限制。
- **速率限制**：gRPC 与 REST 网关暂无内置限流，请在 LB/网关侧实施。
- **审计 Actor 为声明式身份**：审计记录中的 Actor 来自客户端自报的身份（CLI 端取 `LEVEE_ACTOR` 环境变量，缺省 `cli-user`；服务端从请求上下文/元数据读取，缺省 `grpc-user`）。在共享 token 鉴权模式下，Actor 是**断言（asserted）而非可证明（proven）**——任何持有 token 的调用方都可自称任意身份。需要不可抵赖性时必须引入每用户独立凭证或 mTLS/签名身份。
- **KMS/Vault 出站 TLS 校验可配置关闭**：Vault Provider 提供 `Insecure` 配置项、KMS 集成提供跳过证书校验的传输构造（`kms_helpers.go`），用于自签名证书的内网环境。**生产环境必须保持证书校验开启**（`Insecure=false`）；开启即放弃对中间人攻击的防护。
- **file 模块本地读取路径围栏**：`internal/executor/modules/file` 的 copy/template 动作已限制 `src` 只能取进程工作目录内的相对路径；绝对路径或越出工作目录的路径会被拒绝，除非目标目录列入 `LEVEE_FILE_MODULE_EXTRA_DIRS` 允许列表（`os.PathListSeparator` 分隔）。请保持该列表最小化，并通过 RBAC 限制 file 模块的使用面。


**风险评级**：3 个 CRITICAL 已在 v1.0.0 修复，5 个 HIGH 正在修复中。建议在修复所有 HIGH 问题后再进入生产阶段。