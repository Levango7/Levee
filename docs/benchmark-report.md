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