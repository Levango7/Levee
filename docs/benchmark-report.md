# LEVEE Performance Baseline Report

> **Generated**: 2026-11-16（本次全量重跑；2026-08-16 版仅作历史参考，数据见 git 历史）
> **增补**: 2026-11-17 补 §17.2（Audit Verify 规模，`-count=3` 取中位数）；方法与 commit 见该节，
> 与 2026-11-16 的全量重跑**不是同一次测量**，两者的绝对值不可直接混用。
> **Go Version**: go1.26.6
> **OS/Arch**: Windows 11 家庭版 build 26300，windows/amd64
> **CPU**: AMD Ryzen 9 7945HX with Radeon Graphics（32 threads）
> **RAM**: 31.2 GB
> **Host state**: 宿主上另有若干 agent 会话在跑（本次数据**不是空载**，仅供相对比较——单看绝对值会偏乐观/偏悲观，不可作招投标的门槛数据）
> **Benchmark Command**: `go test -bench=Benchmark -benchmem -count=1 -run='^$' ./internal/...`
> **测试基准 commit**: `bd53f01`（`docs/benchmark-2026-11` 分支，master 同位）
> **输出**：128 条 benchmark，14 个包，全量耗时约 3–5 分钟

---

## Table of Contents

1. [Approval](#1-approval)
2. [Audit](#2-audit)
3. [Batch](#3-batch)
4. [Channel](#4-channel)
5. [Channel/SSH](#5-channelssh)
6. [Channel/WinRM](#6-channelwinrm)
7. [Credential](#7-credential)
8. [DSL](#8-dsl)
9. [Executor](#9-executor)
10. [Executor/Modules/Shell](#10-executormodulesshell)
11. [Lock](#11-lock)
12. [Permission](#12-permission)
13. [Plan](#13-plan)
14. [Rollback](#14-rollback)
15. [Key Metrics Summary](#key-metrics-summary)
16. [Hotspots & Observations](#hotspots--observations)
17. [Audit Seal / Verify 规模（steady-state）](#17-audit-seal--verify-规模steady-state)
18. [历史（v1.3.0 Phase 1+2）](#18-历史v130-phase-12)

---

## 1. Approval

Package: `internal/approval`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewLevelManager | 22 040 833 | 62.59 | 0 | 0 |
| LevelManager_DetermineLevel_Standard | 68 279 623 | 18.06 | 0 | 0 |
| LevelManager_DetermineLevel_High | 69 166 656 | 17.45 | 0 | 0 |
| LevelManager_DetermineLevel_Emergency | 80 042 155 | 16.70 | 0 | 0 |
| LevelManager_Get | 57 224 332 | 25.56 | 0 | 0 |
| LevelManager_All | 5 680 058 | 281.5 | 352 | 1 |
| Service_Create（10x） | 10 | 11 350 | 2 309 | 10 |
| Service_Approve（10x） | 10 | 2 610 | 1 036 | 8 |
| Service_Reject（10x） | 10 | 23 700 | 3 024 | 21 |
| Service_Get（10x） | 10 | 470.0 | 256 | 2 |
| CanTransition | 1e9 | 0.2624 | 0 | 0 |
| ValidLevel | 1e9 | 0.9593 | 0 | 0 |
| NewID | 8 368 404 | 164.0 | 32 | 1 |

> `Service_Create/Approve/Reject/Get` 系列带 INFO 日志，`-benchtime=10x` 实测；
> 全套全量 `-run=^$` 时不输出这些日志位，直接以 `-bench XXX -benchtime 10x -v` 拿数。

---

## 2. Audit

Package: `internal/audit`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| AuditRecordSealsWholeChain/Sealed 100 | 969 | 3 835 869 | 810 395 | 14 147 |
| AuditRecordSealsWholeChain/Sealed 1 000 | 198 | 7 534 008 | 1 498 572 | 26 548 |
| AuditRecordSealsWholeChain/Sealed 5 000 | 42 | 33 550 293 | 6 809 062 | 120 902 |
| AuditChainSealSteadyState/Sealed 100 | 2 420 | 564 161 | 136 712 | 2 443 |
| AuditChainSealSteadyState/Sealed 1 000 | 226 | 4 800 194 | 1 355 187 | 24 095 |
| AuditChainSealSteadyState/Sealed 5 000 | 32 | 36 472 009 | 6 771 290 | 120 323 |
| Redact | 537 128 | 2 196 | 672 | 4 |
| Redact_Large | 43 962 | 26 803 | 2 392 | 4 |
| BuildDetail | 232 222 | 6 219 | 1 186 | 15 |
| Redact_NestedStructs/SecretFree | 76 260 | 13 657 | 2 160 | 31 |
| Redact_NestedStructs/WithHit | 80 304 | 14 361 | 2 496 | 34 |
| IsSensitive | 1e6 | 1 311 | 0 | 0 |
| NewID | 5 457 921 | 204.1 | 32 | 1 |
| ComputeHash | 508 254 | 2 185 | 1 120 | 10 |
| ComputeHash_Chain | 5 190 | 214 720 | 110 016 | 1 001 |
| TraceRecorder_Record | 993 | 4 304 995 | 982 027 | 14 042 |

> 注：`AuditRecordSealsWholeChain` 与 `AuditChainSealSteadyState` 的 sealed_rows=5000 在 go1.26.6 下
> 数字**高于** 2026-11-02（Record 4.31→33.5ms，Seal 17.5→36.5ms）。当日宿主有其它会话压测中，未排除干扰；
> 该趋势不能用来反推 "封链插入变慢"。稳定对比见 §17。

---

## 3. Batch

Package: `internal/batch`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewController | 22 668 144 | 60.78 | 48 | 1 |
| Controller_Execute_Serial | 13 395 | 93 556 | 12 085 | 115 |
| Controller_Execute_Parallel | 2 121 | 604 267 | 105 539 | 791 |
| ConcurrencyManager_AcquireRelease_Unlimited | 17 968 778 | 78.31 | 28 | 2 |
| ConcurrencyManager_AcquireRelease_WithLimiter | 339 171 | 3 000 | 240 | 6 |
| ConcurrencyManager_Stats | 1e9 | 0.528 | 0 | 0 |
| ErrorPolicy_String | 1e9 | 0.343 | 0 | 0 |

---

## 4. Channel

Package: `internal/channel`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| ChannelRegistry_New | 152 031 327 | 8.193 | 0 | 0 |
| ChannelRegistry_Register | 12 749 694 | 89.31 | 8 | 1 |
| ChannelRegistry_Factory | 12 450 121 | 93.27 | 8 | 1 |
| ChannelRegistry_Create | 64 114 550 | 21.53 | 0 | 0 |
| ChannelRegistry_Types | 1 227 032 | 1 034 | 320 | 1 |
| Limiter_AcquireRelease | 4 057 448 | 327.8 | 16 | 1 |
| Limiter_AcquireRelease_Parallel | 1 423 596 | 832.8 | 22 | 2 |
| Limiter_Stats | 180 216 | 7 946 | 3 720 | 11 |
| Prechecker_New | 6 793 574 | 158.1 | 80 | 1 |

---

## 5. Channel/SSH

Package: `internal/channel/ssh`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| SSHFactory_Create | 1 390 081 | 751.4 | 440 | 7 |
| NewChannel | 20 402 656 | 55.85 | 48 | 1 |
| NewConfig | 1 840 653 | 697.1 | 392 | 6 |
| SSHPool_New | 3 381 206 | 401.4 | 240 | 3 |
| SSHPool_Stats | 81 723 826 | 14.16 | 0 | 0 |
| SSHPool_PoolKey | 7 964 834 | 153.0 | 32 | 2 |

---

## 6. Channel/WinRM

Package: `internal/channel/winrm`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| WinRMFactory_Create | 9 538 374 | 141.7 | 128 | 1 |
| NewChannel | 10 567 538 | 135.9 | 128 | 1 |
| Config_WithDefaults | 61 508 810 | 16.62 | 0 | 0 |
| Config_ResolvePort | 1e9 | 0.862 | 0 | 0 |
| FormatISO8601 | 1e9 | 0.245 | 0 | 0 |
| WinRMPool_New | 6 302 100 | 226.4 | 176 | 4 |
| WinRMPool_Stats | 82 064 187 | 14.10 | 0 | 0 |
| WinRMPool_PoolKey | 5 855 637 | 183.3 | 64 | 4 |

---

## 7. Credential

Package: `internal/credential`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| CredentialStore_encrypt | 4 | 345 484 800 | 203 438 836 | 130 |
| CredentialStore_decrypt | 3 | 353 946 700 | 203 437 496 | 139 |
| CredentialStore_deriveKey | 3 | 348 389 667 | 203 433 410 | 131 |
| CredentialStore_StoreAndRetrieve（2x） | 2 | 297 029 550 | 203 439 116 | 211 |
| SecureZero | 18 692 170 | 74.55 | 0 | 0 |
| NewID | 7 058 316 | 168.2 | 32 | 2 |

> `CredentialStore_*` 3-4 次迭代 / 203 MB/op 的水平与 2026-08-16（go1.25，KMS 集成后）一致，
> 高内存**是预期行为**（scrypt 派生 + fallback 路径），不是 bug。

---

## 8. DSL

Package: `internal/dsl`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| Parser_ParseBytes | 11 833 | 96 780 | 34 274 | 581 |
| Parser_ParseBytes_Large | 3 271 | 436 886 | 165 090 | 2 754 |
| NewParser | 1e9 | 0.240 | 0 | 0 |
| Validator_Validate | 619 395 | 1 778 | 681 | 18 |
| Validator_ValidateStrict | 584 020 | 1 921 | 681 | 18 |
| NewValidator | 6 409 101 | 185.1 | 144 | 3 |
| SplitAction | 262 683 853 | 4.397 | 0 | 0 |
| ConvertInput_List | 2 688 394 | 379.4 | 160 | 3 |

---

## 9. Executor

Package: `internal/executor`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewExecutor | 165 585 256 | 7.660 | 0 | 0 |
| Executor_RegisterModule | 24 161 154 | 46.80 | 16 | 1 |
| Executor_Module | 14 190 032 | 91.21 | 8 | 1 |
| Executor_Modules | 1 528 330 | 751.0 | 320 | 1 |
| Executor_Execute | 9 752 814 | 137.6 | 96 | 2 |
| ActionSupported | 1e9 | 1.135 | 0 | 0 |
| NewShellRunner | 1e9 | 0.244 | 0 | 0 |
| BuildShellCommand | 2 299 | 591 372 | 17 272 | 119 |
| IsBlank | 1e9 | 0.757 | 0 | 0 |

---

## 10. Executor/Modules/Shell

Package: `internal/executor/modules/shell`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| Shell_New | 1e9 | 0.283 | 0 | 0 |
| Shell_Name | 1e9 | 0.276 | 0 | 0 |
| Shell_Actions | 1e9 | 0.255 | 0 | 0 |
| Shell_Idempotent | 1e9 | 0.262 | 0 | 0 |
| Shell_Execute_Exec | 8 668 634 | 134.2 | 112 | 2 |
| StringArg | 1e8 | 11.13 | 0 | 0 |
| RandomSuffix | 10 762 186 | 108.6 | 8 | 1 |

---

## 11. Lock

Package: `internal/lock`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| Lock_Expired | 293 347 035 | 4.508 | 0 | 0 |
| Lock_Expired_NotExpired | 267 769 758 | 4.531 | 0 | 0 |
| Scope | 79 641 084 | 14.20 | 0 | 0 |
| LockStore_Acquire | 3 726 | 286 524 | 3 140 | 74 |
| LockStore_Release | 55 801 | 18 336 | 1 135 | 32 |
| LockStore_Get | 69 634 | 19 690 | 1 440 | 43 |
| LockManager_Acquire（2x，100 hosts） | 2 | 3 548 200 | 13 728 | 194 |
| LockManager_Release（2x，100 hosts） | 2 | 4 397 800 | 4 684 | 118 |

---

## 12. Permission

Package: `internal/permission`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewPermissionMatrix | 80 294 949 | 14.72 | 0 | 0 |
| PermissionMatrix_Grant | 15 946 270 | 86.41 | 0 | 0 |
| PermissionMatrix_Allow | 10 373 972 | 117.7 | 0 | 0 |
| PermissionMatrix_Allow_Wildcard | 18 650 608 | 65.59 | 0 | 0 |
| PermissionMatrix_Allow_Admin | 9 148 258 | 127.1 | 0 | 0 |
| PermissionMatrix_Allow_Denied | 32 419 376 | 39.53 | 0 | 0 |
| PermissionMatrix_ActionsFor | 575 037 | 1 813 | 696 | 5 |
| PermissionMatrix_Teams | 404 318 | 3 286 | 1 704 | 6 |
| PermissionMatrix_Environments | 787 173 | 1 346 | 616 | 4 |
| PermissionMatrix_LoadFromConfig | 741 206 | 2 863 | 1 744 | 16 |
| PermissionMatrix_LoadFromJSON | 266 578 | 4 673 | 1 592 | 28 |
| PermissionChecker_Check | 20 962 968 | 60.97 | 0 | 0 |
| PermissionChecker_CheckBatch | 6 784 888 | 197.6 | 0 | 0 |

---

## 13. Plan

Package: `internal/plan`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewGenerator | 890 642 | 1 208 | 744 | 15 |
| Generator_Generate_Serial | 239 551 | 6 796 | 4 543 | 20 |
| Generator_Generate_Percent | 101 307 | 10 244 | 4 872 | 21 |
| Generator_Generate_Fixed | 125 767 | 8 566 | 4 872 | 21 |
| Generator_Generate_Large | 18 950 | 67 008 | 56 075 | 23 |
| ComputeHash | 13 568 | 84 398 | 39 327 | 109 |
| ComputeHash_Large | 1 657 | 730 631 | 401 657 | 134 |
| VerifyHash | 16 365 | 72 839 | 39 332 | 109 |
| BuildCanonical | 31 218 | 38 298 | 22 208 | 46 |
| SortedCopy | 747 666 | 1 456 | 1 792 | 1 |
| NewPlanID | 9 524 836 | 138.0 | 24 | 1 |

---

## 14. Rollback

Package: `internal/rollback`

| Benchmark | Iters | ns/op | B/op | allocs/op |
|-----------|-------|-------|------|-----------|
| NewManager | 12 174 607 | 109.0 | 112 | 2 |
| Manager_Rollback_Serial | 193 224 | 6 227 | 8 272 | 38 |
| Manager_Rollback_Parallel | 3 128 | 515 499 | 151 466 | 777 |
| Manager_Rollback_WithWhitelist | 89 259 | 12 855 | 8 272 | 38 |
| Manager_Whitelist | 10 221 882 | 118.8 | 48 | 1 |
| IsSkipped | 1e9 | 0.244 | 0 | 0 |
| HasError | 652 338 769 | 1.918 | 0 | 0 |

---

## Key Metrics Summary

| Metric | Value | Unit | Package |
|--------|-------|------|---------|
| DSL Parse Throughput (standard) | ~10.3K | ops/s | dsl |
| DSL Parse Throughput (large) | ~2.3K | ops/s | dsl |
| Plan Generate Throughput (serial) | ~147K | ops/s | plan |
| Plan Generate Throughput (large) | ~14.9K | ops/s | plan |
| Plan ComputeHash (standard) | ~11.8K | ops/s | plan |
| Plan ComputeHash (large) | ~1.4K | ops/s | plan |
| Audit TraceRecorder Record | ~232 | ops/s | audit |
| Audit Seal steady-state (100 rows) | ~1.8K | ops/s | audit |
| Credential Encrypt | ~2.9 | ops/s | credential |
| Credential Decrypt | ~2.8 | ops/s | credential |
| Credential Store+Retrieve (round-trip) | ~3.4 | ops/s | credential |
| LockStore Acquire (SQLite) | ~3.5K | ops/s | lock |
| LockStore Release (SQLite) | ~54.6K | ops/s | lock |
| Permission Check | ~16.4M | ops/s | permission |
| Batch Serial Execute | ~10.7K | ops/s | batch |
| Batch Parallel Execute | ~1.7K | ops/s | batch |
| Plan VerifyHash | ~13.7K | ops/s | plan |

---

## Hotspots & Observations

### 1. Credential Encryption/Decryption（固有高成本）

`CredentialStore_encrypt/decrypt/deriveKey` 3 次迭代量级：`~345–354 ms/op`，`~203 MB/op`。
与 2026-08-16 基线同属 KMS fallback 路径触发后的数值，高成本来自 `scrypt`
(N=32768) + KMS 回退分配，**属预期行为**。KMS 命中路径仍未单独建基准（F05 待补）。

### 2. Audit chain Seal（已量化）

Record ≈ Seal × 2。`sealed_rows=5 000` 时 Record=33.5 ms、Seal=36.5 ms（2026-11-16 本机、宿主有其它会话）。
2026-11-02（宿主干净）同档：Record=18.34 ms、Seal=17.45 ms。**比例关系（Seal 占 Record 的 90%+）稳定成立**，绝对值受宿主负载显著影响。§17 详述。

### 3. Batch Parallel vs Serial

`Controller_Execute_Parallel`（604 267 ns/op，791 allocs）≈ Serial（93 556 ns/op，115 allocs）× 6.5。
小批量并行没有优势，与 2026-08-16 一致。

### 4. Plan Large 输入

`Generator_Generate_Large` 67 008 ns/op（v1.0.0 同量级路径）；`ComputeHash_Large` 730 631 ns/op、402 KB/op。
大输入的 SHA256（canon）主导耗时，稳定。

### 5. DSL 解析器

Parser 微调过：standard 96 780 ns（v1.0.0 118 613 → **+18.5% 提速**）；Large 436 886 ns（v1.0.0 924 536 → **+53% 提速**），allocs 全持平。
CHANGELOG 的优化见 v1.x 系列的 release notes。

---

## 17. Audit Seal / Verify 规模（steady-state）

> 与 `docs/audit-chain-scale-design.md` §2 的规模观察同档测量，宿主 **非空载**（2026-11-16 本机）。
> 17.1（Seal）测于 2026-11-16，17.2（Verify）测于 2026-11-17，**两条链是同一条**（全局 audit 链），
> 所以两栏可以直接对照。

### 17.1 Seal（写入侧：插入 + 封链）

| 已封行数 | `AuditRecordSealsWholeChain`（插入+封链） | `AuditChainSealSteadyState`（仅封链） | 估算 seal 常数（µs/行） | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 100 | 3.84 ms | 0.56 ms | 5.6 | 137 KB / 2443 | 2 443 |
| 1 000 | 7.53 ms | 4.80 ms | 4.8 | 1.36 MB / 24 095 | 24 095 |
| 5 000（首测，宿主有并发） | 33.55 ms | 36.47 ms | 7.3 | 6.77 MB / 120 323 | 120 323 |
| 5 000（稳态 20x，宿主空载复测） | — | **26.3 ms** | 5.3 | 6.77 MB / 120 324 | 120 324 |
| 25 000 | 339 ms | 198 ms | 7.9 | 33.9 MB / 603 276 | 603 276 |
| 100 000 | — | — | ≈ 38 | 139 MB / 2 485 036 | 2 485 036 |

> 2026-11-16 稳态复测（`-benchtime=20x`，宿主空载）：100 行 0.46 ms、1 000 行 4.55 ms、5 000 行 **26.3 ms**——首测的 Seal@5000 = 36.5 ms 含宿主并发开销。

> 2026-11-16 早些时候并行测量（`-benchtime=5x`，当时宿主正跑后台全量基准、负载重）得到：
> 25 000 行 Record ≈ 339 ms，Seal ≈ 198 ms；
> 100 000 行 Seal ≈ **3 829 114 ns/op**（3.8 s，139 MB/op，2.48M allocs/op）。
> 该测量**不是空载**，偏高 ~4–10 倍；规模外推请以 §2（docs/audit-chain-scale-design.md）为准。
> 本组不进入 CI 门控——CI 上跑这个只会测出 runner 的体型。

### 17.2 Verify（读取侧：只读校验，全链重算）

`AuditChainBuilder.Verify` 走一遍整条链并**重算每一行的摘要**（与 Seal 的修补分支不同，它
是纯读路径）。这是审计员会问、投标附录会引的那个数。

> **测量方法**：`go test ./internal/audit/ -run '^$' -bench 'BenchmarkAuditChainVerifySteadyState'
> -benchmem -benchtime=20x -count=3`，**取 3 次中位数**（本文件末尾「使用注意」要求的口径）。
> 测量 commit：`2daef09`（PR #402 的分支头，合并后 master 为 `9cd4a99`，被测文件内容一致）。
> 宿主：2026-11-17 本机，**非严格空载**（另有 agent 会话），绝对值仅供相对比较。

| 已封行数 | `AuditChainVerifySteadyState`（整链重算） | 估算 verify 常数（µs/行） | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| 100 | 0.28 ms | 2.80 | 137 KB / 2 444 | 2 444 |
| 1 000 | 2.51 ms | 2.51 | 1.36 MB / 24 096 | 24 096 |
| 5 000 | 15.80 ms | 3.16 | 6.77 MB / 120 324 | 120 324 |
| 25 000 | 149.6 ms | 5.98 | 33.9 MB / 603 274 | 603 274 |
| 100 000 | **1.90 s** | 19.03 | 139 MB / 2 485 037 | 2 485 037 |

三件事值得记下来：

1. **Verify 比 Seal 便宜**，且便宜得合理：Seal 要写链（`UpdateAuditChain` 逐行落盘），
   Verify 只读不写。5 000 行上 15.8 ms vs 26.3 ms，100 000 行上 1.90 s vs ≈3.8 s（Seal 那一格
   是非空载测得，偏保守）。**审计校验不是写路径的瓶颈**。
2. **每行成本随规模上升**（2.5 → 19.0 µs/行，10 万行比 1 千行高 7.6 倍）。哈希本身每行 O(1)，
   所以这 7 倍不在摘要计算上——按常理应来自存储读取路径（索引扫描随表深度与缓存命中下降）
   与分配压力，但本文件**不把它当已证实的结论**：需要归因时另做一次带 pprof 的测量。
   可确认的**工程结论**只有一个：**10 万行的整链校验约 1.9 s**，这个量级要进设计取舍
   （比如大链是否分片校验），而不是当作常数。
3. **每行一次分配**（allocs/op ≈ 行数），1.36 MB/1 000 行、139 MB/100 000 行——与 Seal 同量级，
   说明链的内存画像由「每行一份规范化载荷」主导，与写读无关。

> 同 §17.1：**本组不进入 CI 门控**（CI 上跑这个只会测出 runner 的体型），**也不构成任何
> 招投标门槛值**。对外引用须在干净宿主上按本文件末尾的口径复跑并登记机器型号。

---

## 18. 历史（v1.3.0 Phase 1+2，2026-08-16）

> 仅作历史存档；**该数据仅在 `-count=1` 与 `-count=3` 混跑**，且基线 go1.25（v1.3.0）。
> 本节**不被任何当前决策引用**；收录是因为本文件此前的正文就是这组数据，git 历史里找不到更好的「为什么这些数字不可比」的例子。
> 详见 git 历史 `docs/benchmark-report.md` @ 2026-08-16。

---

> **使用注意**：本报告**不构成新系统的承诺值**；任何对外基准声明（tender/SLA/招投标）须在干净宿主（`go test -run='^$' -bench Benchmark -benchmem -count=3 ./internal/...`）复跑取 3 次中位数，并登记 `verify (xinchuang)` 或目标机器型。
