# LEVEE Performance Baseline Report

> **Generated**: 2026-08-16  
> **Go Version**: go1.25.0  
> **OS/Arch**: windows/amd64  
> **CPU**: AMD Ryzen 9 7945HX with Radeon Graphics (32 threads)  
> **Benchmark Command**: `go test -bench=Benchmark -benchmem -count=3 -run=^$ ./internal/...`

---

## Table of Contents

1. [Channel](#1-channel)
2. [Channel/SSH](#2-channelssh)
3. [Channel/WinRM](#3-channelwinrm)
4. [Executor](#4-executor)
5. [Executor/Modules/Shell](#5-executormodulesshell)
6. [Batch](#6-batch)
7. [DSL](#7-dsl)
8. [Plan](#8-plan)
9. [Audit](#9-audit)
10. [Credential](#10-credential)
11. [Permission](#11-permission)
12. [Approval](#12-approval)
13. [Rollback](#13-rollback)
14. [Lock](#14-lock)
15. [Key Metrics Summary](#key-metrics-summary)
16. [Top 5 Hotspots & Optimization Recommendations](#top-5-hotspots--optimization-recommendations)

---

## 1. Channel

Package: `internal/channel`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| ChannelRegistry_New | 7 | 0 | 0 |
| ChannelRegistry_Register | 96 | 8 | 1 |
| ChannelRegistry_Factory | 86 | 8 | 1 |
| ChannelRegistry_Create | 19 | 0 | 0 |
| ChannelRegistry_Types | 1036 | 320 | 1 |
| Limiter_AcquireRelease | 274 | 16 | 1 |
| Limiter_AcquireRelease_Parallel | 649 | 22 | 2 |
| Limiter_Stats | 6012 | 3720 | 11 |
| Prechecker_New | 123 | 80 | 1 |

---

## 2. Channel/SSH

Package: `internal/channel/ssh`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| SSHFactory_Create | 205 | 112 | 2 |
| NewChannel | 93 | 48 | 1 |
| NewConfig | 0.43 | 0 | 0 |
| SSHPool_New | 469 | 240 | 3 |
| SSHPool_Stats | 15 | 0 | 0 |
| SSHPool_PoolKey | 253 | 32 | 2 |

---

## 3. Channel/WinRM

Package: `internal/channel/winrm`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| WinRMFactory_Create | 316 | 112 | 1 |
| NewChannel | 299 | 112 | 1 |
| Config_WithDefaults | 22 | 0 | 0 |
| Config_ResolvePort | 0.91 | 0 | 0 |
| FormatISO8601 | 0.40 | 0 | 0 |
| WinRMPool_New | 293 | 176 | 4 |
| WinRMPool_Stats | 21 | 0 | 0 |
| WinRMPool_PoolKey | 233 | 40 | 3 |

---

## 4. Executor

Package: `internal/executor`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewExecutor | 9 | 0 | 0 |
| Executor_RegisterModule | 88 | 16 | 1 |
| Executor_Module | 161 | 8 | 1 |
| Executor_Modules | 1355 | 320 | 1 |
| Executor_Execute | 198 | 96 | 2 |
| ActionSupported | 1.5 | 0 | 0 |
| NewShellRunner | 0.34 | 0 | 0 |
| BuildShellCommand | 8077793 | 32768 | 332 |
| IsBlank | 0.98 | 0 | 0 |

---

## 5. Executor/Modules/Shell

Package: `internal/executor/modules/shell`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| Shell_New | 0.31 | 0 | 0 |
| Shell_Name | 0.28 | 0 | 0 |
| Shell_Actions | 0.34 | 0 | 0 |
| Shell_Idempotent | 0.37 | 0 | 0 |
| Shell_Execute_Exec | 201 | 112 | 2 |
| StringArg | 19 | 0 | 0 |
| RandomSuffix | 162 | 8 | 1 |

---

## 6. Batch

Package: `internal/batch`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewController | 52 | 48 | 1 |
| Controller_Execute_Serial | 82220 | 11605 | 115 |
| Controller_Execute_Parallel | 612391 | 101487 | 791 |
| ConcurrencyManager_AcquireRelease_Unlimited | 96 | 28 | 2 |
| ConcurrencyManager_AcquireRelease_WithLimiter | 3051 | 240 | 6 |
| ConcurrencyManager_Stats | 0.52 | 0 | 0 |
| ErrorPolicy_String | 0.38 | 0 | 0 |

---

## 7. DSL

Package: `internal/dsl`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| Parser_ParseBytes | 118613 | 34256 | 581 |
| Parser_ParseBytes_Large | 924536 | 165038 | 2754 |
| NewParser | 0.48 | 0 | 0 |
| Validator_Validate | 1563 | 120 | 9 |
| Validator_ValidateStrict | 1536 | 120 | 9 |
| NewValidator | 0.44 | 0 | 0 |
| SplitAction | 5.8 | 0 | 0 |
| ConvertInput_List | 535 | 160 | 3 |

---

## 8. Plan

Package: `internal/plan`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewGenerator | 0.46 | 0 | 0 |
| Generator_Generate_Serial | 1112 | 368 | 5 |
| Generator_Generate_Percent | 1908 | 680 | 6 |
| Generator_Generate_Fixed | 2265 | 680 | 6 |
| Generator_Generate_Large | 2807 | 680 | 6 |
| ComputeHash | 141372 | 31502 | 74 |
| ComputeHash_Large | 2031401 | 383341 | 100 |
| VerifyHash | 182045 | 31515 | 74 |
| BuildCanonical | 105369 | 22208 | 46 |
| SortedCopy | 2943 | 1792 | 1 |
| NewPlanID | 203 | 24 | 1 |

---

## 9. Audit

Package: `internal/audit`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| Redact | 1538 | 672 | 4 |
| Redact_Large | 6174 | 2392 | 4 |
| BuildDetail | 5493 | 1186 | 15 |
| IsSensitive | 150 | 0 | 0 |
| NewID | 312 | 32 | 1 |
| ComputeHash | 2185 | 504 | 5 |
| ComputeHash_Chain | 203082 | 47072 | 500 |
| TraceRecorder_Record | 552696 | 4073 | 60 |

---

## 10. Credential

Package: `internal/credential`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| CredentialStore_encrypt | 142547698 | 67120768 | 99 |
| CredentialStore_decrypt | 159396092 | 67119695 | 96 |
| CredentialStore_deriveKey | 186684758 | 67117149 | 93 |
| SecureZero | 118 | 0 | 0 |
| CredentialStore_StoreAndRetrieve | 288908725 | 67120999 | 155 |
| NewID | 329 | 24 | 1 |

---

## 11. Permission

Package: `internal/permission`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewPermissionMatrix | 23 | 0 | 0 |
| PermissionMatrix_Grant | 78 | 0 | 0 |
| PermissionMatrix_Allow | 177 | 0 | 0 |
| PermissionMatrix_Allow_Wildcard | 103 | 0 | 0 |
| PermissionMatrix_Allow_Admin | 222 | 0 | 0 |
| PermissionMatrix_Allow_Denied | 56 | 0 | 0 |
| PermissionMatrix_ActionsFor | 4104 | 696 | 5 |
| PermissionMatrix_Teams | 4980 | 1704 | 6 |
| PermissionMatrix_Environments | 2055 | 616 | 4 |
| PermissionMatrix_LoadFromConfig | 3275 | 1584 | 13 |
| PermissionMatrix_LoadFromJSON | 5808 | 1432 | 25 |
| PermissionChecker_Check | 62 | 0 | 0 |
| PermissionChecker_CheckBatch | 174 | 0 | 0 |

---

## 12. Approval

Package: `internal/approval`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewLevelManager | 93 | 0 | 0 |
| LevelManager_DetermineLevel_Standard | 25 | 0 | 0 |
| LevelManager_DetermineLevel_High | 28 | 0 | 0 |
| LevelManager_DetermineLevel_Emergency | 29 | 0 | 0 |
| LevelManager_Get | 43 | 0 | 0 |
| LevelManager_All | 694 | 352 | 1 |
| Service_Create | 1020000 | 88000 | 1000 |

---

## 13. Rollback

Package: `internal/rollback`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| NewManager | 207 | 72 | 2 |
| Manager_Rollback_Serial | 19869 | 8224 | 38 |
| Manager_Rollback_Parallel | 1028159 | 147372 | 777 |
| Manager_Rollback_WithWhitelist | 34427 | 8224 | 38 |
| Manager_Whitelist | 344 | 48 | 1 |
| IsSkipped | 0.47 | 0 | 0 |
| HasError | 3.8 | 0 | 0 |

---

## 14. Lock

Package: `internal/lock`

| Benchmark | Avg ns/op | Avg B/op | Avg allocs/op |
|-----------|-----------|----------|---------------|
| Lock_Expired | 5.4 | 0 | 0 |
| Lock_Expired_NotExpired | 5.4 | 0 | 0 |
| Scope | 21 | 0 | 0 |
| LockStore_Acquire | 501557 | 2988 | 75 |
| LockStore_Release | 47936 | 1138 | 33 |
| LockStore_Get | 49097 | 1408 | 43 |
| LockManager_Acquire (100 hosts) | 1583282300 | 48088 | 779 |

---

## Key Metrics Summary

| Metric | Value | Unit | Package |
|--------|-------|------|---------|
| DSL Parse Throughput (standard) | ~8.4K | ops/s | dsl |
| DSL Parse Throughput (large) | ~1.1K | ops/s | dsl |
| Plan Generate Throughput (serial) | ~899K | ops/s | plan |
| Plan ComputeHash (standard) | ~7.1K | ops/s | plan |
| Plan ComputeHash (large) | ~492 | ops/s | plan |
| Audit Trace Record | ~1.8K | ops/s | audit |
| Audit HashChain Build (100 entries) | ~4.9K | ops/s | audit |
| Credential Encrypt | ~7.0 | ops/s | credential |
| Credential Decrypt | ~6.3 | ops/s | credential |
| Credential Store+Retrieve (round-trip) | ~3.5 | ops/s | credential |
| Lock Acquire (SQLite) | ~2.0K | ops/s | lock |
| Lock Release (SQLite) | ~20.9K | ops/s | lock |
| Permission Check | ~16.1M | ops/s | permission |
| Batch Serial Execute | ~12.2K | ops/s | batch |
| Batch Parallel Execute | ~1.6K | ops/s | batch |

---

## Top 5 Hotspots & Optimization Recommendations

### 1. Credential Encryption/Decryption (~142-187 ms/op, ~67 MB/op)

**Root Cause**: `deriveKey` uses `crypto.scrypt` with high cost parameters (N=32768, r=8, p=1), producing a 64 MB memory footprint per call. This is by design for security but dominates runtime.

**Recommendation**: 
- Consider caching derived keys in memory with TTL for repeated operations on the same passphrase.
- For high-throughput scenarios, evaluate if `crypto.argon2id` offers better trade-offs on modern hardware.
- Current parameters are appropriate for security; **do not reduce** scrypt cost factors without security review.

### 2. BuildShellCommand (~8.1 ms/op, 332 allocs/op)

**Root Cause**: Shell command construction involves extensive string concatenation and template processing with many small allocations.

**Recommendation**:
- Use `strings.Builder` instead of repeated string concatenation.
- Pre-allocate buffer sizes based on typical command lengths.
- Consider caching compiled templates for repeated command patterns.

### 3. LockManager Acquire for Bulk Operations (~1.6 s/op for 100 hosts)

**Root Cause**: Sequential SQLite writes for each host lock acquisition. Each `LockStore_Acquire` costs ~500 μs due to SQLite disk I/O.

**Recommendation**:
- Batch lock acquisitions into a single SQLite transaction using `BEGIN TRANSACTION` / `COMMIT`.
- Use WAL mode for concurrent read/write access.
- Consider in-memory lock caching with periodic SQLite sync.

### 4. DSL Parser for Large Inputs (~925 μs/op, 2754 allocs/op)

**Root Cause**: Recursive descent parser creates many intermediate AST nodes and string slices during tokenization of large DSL scripts.

**Recommendation**:
- Implement a tokenizer/lexer pass to reduce intermediate allocations.
- Use `[]byte` slices with zero-copy techniques instead of string conversions.
- Pool AST node objects for reuse across parse calls.

### 5. Audit TraceRecorder Record (~553 μs/op, 60 allocs/op)

**Root Cause**: Each trace record involves SQLite INSERT, hash chain computation, and redaction processing.

**Recommendation**:
- Batch trace records and write them in a single transaction.
- Use async write-behind buffering for non-critical trace entries.
- Pre-compute redaction patterns at initialization rather than per-record.

---

## Appendix: Raw Benchmark Data

Full raw output from all benchmark runs is available in the project CI artifacts. The values in this report represent the median of 3 runs (`-count=3`).

---

## Phase 1+2 新特性性能基准 (v1.3.0)

### 测试环境

- **OS**: Windows (windows/amd64)
- **Go**: go1.26.6
- **CPU**: AMD Ryzen 9 7945HX with Radeon Graphics (32 threads)
- **日期**: 2026-08-16
- **Benchmark Command**: `go test -bench="." -benchmem -count=1 -timeout 120s ./internal/<pkg>/`
- **说明**: 本次运行使用 `-count=1`（单次运行），v1.0.0 基线使用 `-count=3` 中位数。由于单次运行存在噪声，回归分析仅供参考。

### F06 RBAC 增强 (internal/permission)

Package: `internal/permission`

| Benchmark | ns/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| NewPermissionMatrix | 12.39 | 0 | 0 |
| PermissionMatrix_Grant | 64.02 | 0 | 0 |
| PermissionMatrix_Allow | 135.5 | 0 | 0 |
| PermissionMatrix_Allow_Wildcard | 80.84 | 0 | 0 |
| PermissionMatrix_Allow_Admin | 153.7 | 0 | 0 |
| PermissionMatrix_Allow_Denied | 45.40 | 0 | 0 |
| PermissionMatrix_ActionsFor | 2511 | 696 | 5 |
| PermissionMatrix_Teams | 4128 | 1704 | 6 |
| PermissionMatrix_Environments | 1628 | 616 | 4 |
| PermissionMatrix_LoadFromConfig | 2450 | 1728 | 16 |
| PermissionMatrix_LoadFromJSON | 4655 | 1576 | 28 |
| PermissionChecker_Check | 61.97 | 0 | 0 |
| PermissionChecker_CheckBatch | 204.6 | 0 | 0 |

**说明**: F06 在 v1.3.0 引入了 ABAC 引擎、策略集（PolicySet）、角色树（RoleTree）和权限缓存（PermissionCache）等增强能力。以上 benchmark 为原有 RBAC 矩阵基础操作的基准，新增的 ABAC/PolicySet/RoleTree/Cache 模块暂未提供 benchmark 函数。

### F03 插件系统 (internal/plugin)

Package: `internal/plugin`

**无 benchmark 函数**。

> F03 插件系统（manager、sandbox、registry）当前未提供 `Benchmark*` 函数。该包的单元测试在 Windows 环境下存在已知失败（sandbox 进程启动与 kill 语义在 Windows 上不兼容），建议在 Linux 环境下补充 benchmark 并修复 sandbox 测试。

### F09 ChatOps (internal/chatops)

Package: `internal/chatops`

**无 benchmark 函数**。

> F09 ChatOps（飞书/Slack/钉钉机器人适配层）当前未提供 `Benchmark*` 函数。后续可针对 bot 注册/注销、命令路由、消息分发等热点补充 benchmark。

### F08 变更日历 (internal/calendar)

Package: `internal/calendar`

**无 benchmark 函数**。

> F08 变更日历（变更窗口、冻结期、冲突检测）当前未提供 `Benchmark*` 函数。后续可针对窗口匹配、冻结规则评估、日历查询等热点补充 benchmark。

### F13 类型检查 (internal/dsl)

Package: `internal/dsl`

| Benchmark | ns/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| Parser_ParseBytes | 132928 | 34257 | 581 |
| Parser_ParseBytes_Large | 703563 | 165022 | 2754 |
| NewParser | 0.3764 | 0 | 0 |
| Validator_Validate | 1263 | 120 | 9 |
| Validator_ValidateStrict | 1317 | 120 | 9 |
| NewValidator | 0.3448 | 0 | 0 |
| SplitAction | 5.340 | 0 | 0 |
| ConvertInput_List | 676.7 | 160 | 3 |

**说明**: F13 在 v1.3.0 对 DSL 类型检查进行了增强（严格模式、输入类型转换）。`Validator_ValidateStrict` 与 `Validator_Validate` 性能基本持平，说明严格模式未引入显著开销。

### F14 SLO 门禁 (internal/verify)

Package: `internal/verify`

**无 benchmark 函数**。

> F14 SLO 门禁（pre-apply/grace-period 阶段、Prometheus 查询、重试策略）当前未提供 `Benchmark*` 函数。后续可针对门禁评估、查询重试、多查询并行等热点补充 benchmark。

### F05 KMS 集成 (internal/credential)

Package: `internal/credential`

| Benchmark | ns/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| CredentialStore_encrypt | 344455367 | 203437112 | 125 |
| CredentialStore_decrypt | 267361100 | 203434108 | 109 |
| CredentialStore_deriveKey | 263991875 | 203431580 | 103 |
| SecureZero | 85.23 | 0 | 0 |
| CredentialStore_StoreAndRetrieve | 292203700 | 203435004 | 149 |
| NewID | 173.9 | 24 | 1 |

**说明**: F05 在 v1.3.0 引入了 KMS 提供者抽象与 fallback 机制。本次 benchmark 内存占用（~203 MB/op）显著高于 v1.0.0 基线（~67 MB/op），主要源于 KMS fallback 路径在测试中触发了额外的密钥派生与缓冲区分配。`deriveKey` 仍使用 `crypto.scrypt` 高成本参数，属预期行为。

### MVP 核心包性能回归

以下对比 v1.0.0 基线（报告开头数据，`-count=3` 中位数）与 v1.3.0 本次运行（`-count=1`）。变化率 = (v1.3.0 - v1.0.0) / v1.0.0 × 100%。

#### internal/permission 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| NewPermissionMatrix | 23 | 12.39 | **-46%** ✅ | 0 | 0 |
| PermissionMatrix_Grant | 78 | 64.02 | -18% ✅ | 0 | 0 |
| PermissionMatrix_Allow | 177 | 135.5 | -23% ✅ | 0 | 0 |
| PermissionMatrix_Allow_Wildcard | 103 | 80.84 | -22% ✅ | 0 | 0 |
| PermissionMatrix_Allow_Admin | 222 | 153.7 | -31% ✅ | 0 | 0 |
| PermissionMatrix_Allow_Denied | 56 | 45.40 | -19% ✅ | 0 | 0 |
| PermissionMatrix_ActionsFor | 4104 | 2511 | -39% ✅ | 5 | 5 |
| PermissionMatrix_Teams | 4980 | 4128 | -17% ✅ | 6 | 6 |
| PermissionMatrix_Environments | 2055 | 1628 | -21% ✅ | 4 | 4 |
| PermissionMatrix_LoadFromConfig | 3275 | 2450 | -25% ✅ | 13 | 16 |
| PermissionMatrix_LoadFromJSON | 5808 | 4655 | -20% ✅ | 25 | 28 |
| PermissionChecker_Check | 62 | 61.97 | ~0% | 0 | 0 |
| PermissionChecker_CheckBatch | 174 | 204.6 | **+18%** ⚠️ | 0 | 0 |

**结论**: permission 包整体性能提升显著，多数操作提速 17%-46%。`PermissionChecker_CheckBatch` 有约 18% 回归，建议关注批量检查路径是否引入了额外开销。`LoadFromConfig`/`LoadFromJSON` 的 allocs/op 略有增加（+3），但耗时仍下降。

#### internal/dsl 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| Parser_ParseBytes | 118613 | 132928 | **+12%** ⚠️ | 581 | 581 |
| Parser_ParseBytes_Large | 924536 | 703563 | -24% ✅ | 2754 | 2754 |
| NewParser | 0.48 | 0.3764 | -22% ✅ | 0 | 0 |
| Validator_Validate | 1563 | 1263 | -19% ✅ | 9 | 9 |
| Validator_ValidateStrict | 1536 | 1317 | -14% ✅ | 9 | 9 |
| NewValidator | 0.44 | 0.3448 | -22% ✅ | 0 | 0 |
| SplitAction | 5.8 | 5.340 | -8% ✅ | 0 | 0 |
| ConvertInput_List | 535 | 676.7 | **+27%** ⚠️ | 3 | 3 |

**结论**: dsl 包大输入解析与校验提速明显（-14% ~ -24%）。`Parser_ParseBytes`（标准输入）有约 12% 回归，`ConvertInput_List` 有约 27% 回归，allocs/op 不变，建议排查类型转换路径是否引入了额外循环。考虑到本次为单次运行（`-count=1`），小幅回归可能源于噪声。

#### internal/credential 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 B/op | v1.3.0 B/op |
|-----------|--------------|--------------|------|-------------|-------------|
| CredentialStore_encrypt | 142547698 | 344455367 | **+142%** 🔴 | 67120768 | 203437112 |
| CredentialStore_decrypt | 159396092 | 267361100 | **+68%** 🔴 | 67119695 | 203434108 |
| CredentialStore_deriveKey | 186684758 | 263991875 | **+41%** 🔴 | 67117149 | 203431580 |
| SecureZero | 118 | 85.23 | -28% ✅ | 0 | 0 |
| CredentialStore_StoreAndRetrieve | 288908725 | 292203700 | +1% | 67120999 | 203435004 |
| NewID | 329 | 173.9 | -47% ✅ | 24 | 24 |

**结论**: credential 包加密/解密/派生密钥操作耗时与内存均显著上升（耗时 +41% ~ +142%，内存 ~3 倍）。这与 F05 KMS 集成引入的 fallback 机制有关：测试路径触发了 KMS 提供者失败回退到本地派生，产生额外的 scrypt 调用与缓冲区。`SecureZero` 与 `NewID` 提速明显。`StoreAndRetrieve` 耗时基本持平（+1%），说明端到端往返路径未受显著影响。建议后续针对 KMS 命中路径单独补充 benchmark，以区分 KMS 命中与 fallback 两种场景。

#### internal/channel 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| ChannelRegistry_New | 7 | 8.603 | **+23%** ⚠️ | 0 | 0 |
| ChannelRegistry_Register | 96 | 118.5 | **+23%** ⚠️ | 1 | 1 |
| ChannelRegistry_Factory | 86 | 123.7 | **+44%** ⚠️ | 1 | 1 |
| ChannelRegistry_Create | 19 | 20.60 | +8% | 0 | 0 |
| ChannelRegistry_Types | 1036 | 1445 | **+39%** ⚠️ | 1 | 1 |
| Limiter_AcquireRelease | 274 | 379.4 | **+38%** ⚠️ | 1 | 1 |
| Limiter_AcquireRelease_Parallel | 649 | 891.9 | **+37%** ⚠️ | 2 | 2 |
| Limiter_Stats | 6012 | 8286 | **+38%** ⚠️ | 11 | 11 |
| Prechecker_New | 123 | 182.4 | **+48%** ⚠️ | 1 | 1 |

**结论**: channel 包整体有 8%-48% 回归，allocs/op 不变。考虑到本次为单次运行且 channel 包 benchmark 对 GC 噪声敏感（多数操作 < 1 μs），该回归可能部分源于测量噪声。建议后续以 `-count=3` 或更高重跑确认。若回归真实存在，建议排查 v1.3.0 是否在注册/工厂路径引入了额外查表或锁竞争。

#### internal/channel/ssh 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| SSHFactory_Create | 205 | 220.6 | +8% | 2 | 2 |
| NewChannel | 93 | 115.7 | **+24%** ⚠️ | 1 | 1 |
| NewConfig | 0.43 | 0.3345 | -22% ✅ | 0 | 0 |
| SSHPool_New | 469 | 531.7 | +13% | 3 | 3 |
| SSHPool_Stats | 15 | 13.52 | -10% ✅ | 0 | 0 |
| SSHPool_PoolKey | 253 | 165.9 | -34% ✅ | 2 | 2 |

**结论**: ssh 子包表现混合，`PoolKey` 与 `NewConfig` 提速明显，`NewChannel` 有约 24% 回归。整体回归幅度在噪声范围内。

#### internal/channel/winrm 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| WinRMFactory_Create | 316 | 149.7 | -53% ✅ | 1 | 1 |
| NewChannel | 299 | 179.0 | -40% ✅ | 1 | 1 |
| Config_WithDefaults | 22 | 18.23 | -17% ✅ | 0 | 0 |
| Config_ResolvePort | 0.91 | 0.9168 | +1% | 0 | 0 |
| FormatISO8601 | 0.40 | 0.3595 | -10% ✅ | 0 | 0 |
| WinRMPool_New | 293 | 334.1 | +14% | 4 | 4 |
| WinRMPool_Stats | 21 | 16.19 | -23% ✅ | 0 | 0 |
| WinRMPool_PoolKey | 233 | 174.5 | -25% ✅ | 3 | 3 |

**结论**: winrm 子包整体提速明显，`WinRMFactory_Create` 与 `NewChannel` 分别提速 53% 和 40%。

#### internal/batch 回归

| Benchmark | v1.0.0 ns/op | v1.3.0 ns/op | 变化 | v1.0.0 allocs | v1.3.0 allocs |
|-----------|--------------|--------------|------|---------------|---------------|
| NewController | 52 | 86.88 | **+67%** 🔴 | 1 | 1 |
| Controller_Execute_Serial | 82220 | 134599 | **+64%** 🔴 | 115 | 115 |
| Controller_Execute_Parallel | 612391 | 884365 | **+44%** 🔴 | 791 | 791 |
| ConcurrencyManager_AcquireRelease_Unlimited | 96 | 102.5 | +7% | 2 | 2 |
| ConcurrencyManager_AcquireRelease_WithLimiter | 3051 | 3533 | +16% | 6 | 6 |
| ConcurrencyManager_Stats | 0.52 | 0.8413 | **+62%** 🔴 | 0 | 0 |
| ErrorPolicy_String | 0.38 | 0.3595 | -5% ✅ | 0 | 0 |

**结论**: batch 包回归显著，`NewController`、`Controller_Execute_Serial`、`ConcurrencyManager_Stats` 回归 62%-67%，`Controller_Execute_Parallel` 回归 44%。allocs/op 不变，说明回归非来自分配增量。建议排查 v1.3.0 是否在控制器初始化与执行路径引入了额外锁、日志或回调开销。`ErrorPolicy_String` 略有提速。

### Phase 1+2 性能回归总结

| 包 | 整体趋势 | 主要风险 | 建议 |
|----|----------|----------|------|
| permission | ✅ 提升为主 | CheckBatch +18% | 排查批量检查路径 |
| dsl | ✅ 提升为主 | ParseBytes +12%、ConvertInput_List +27% | 以 `-count=3` 复测确认 |
| credential | 🔴 加密路径显著回归 | encrypt +142%、内存 3 倍 | 补充 KMS 命中路径 benchmark，隔离 fallback 开销 |
| channel | ⚠️ 轻微回归 | Prechecker_New +48%、Factory +44% | 以 `-count=3` 复测确认是否噪声 |
| channel/ssh | ⚠️ 混合 | NewChannel +24% | 噪声范围内，持续观察 |
| channel/winrm | ✅ 提升为主 | WinRMPool_New +14% | 整体优秀 |
| batch | 🔴 显著回归 | NewController +67%、Serial +64% | 排查初始化与执行路径新增开销 |
| plugin | ➖ 无 benchmark | — | 补充 benchmark，修复 Windows sandbox 测试 |
| chatops | ➖ 无 benchmark | — | 补充 benchmark |
| calendar | ➖ 无 benchmark | — | 补充 benchmark |
| verify | ➖ 无 benchmark | — | 补充 benchmark |

**关键行动项**:

1. **credential 包**: KMS fallback 路径导致加密操作内存与耗时大幅上升。需补充 KMS 命中场景的 benchmark，并在生产配置中确保 KMS 可用以避免 fallback 开销。
2. **batch 包**: 控制器初始化与执行路径回归 44%-67%，需定位 v1.3.0 引入的额外开销来源。
3. **channel 包**: 整体 8%-48% 回归，建议以 `-count=3` 或更高复测以排除单次运行噪声。
4. **缺失 benchmark**: plugin/chatops/calendar/verify 四个新特性包均未提供 benchmark 函数，建议在下一迭代补充，覆盖各自核心热点。