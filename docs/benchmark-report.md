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

> 与 `docs/audit-chain-scale-design.md` §2 的规模观察同档测量。
> **本节两栏均于 2026-11-17 在修复后重测**（两栏是同一条链——全局 audit 链，可直接对照）。
> 修复前的那组数（2026-11-16/17 测于 OR 形式的 keyset 谓词）保留在 §17.2 的「修复前后对照」表里，
> 不删——**归因过程与修复前后的数放在一起才构成证据**，单独留一组数说明不了任何事。
> 根因、证据链与后续杠杆见 §17.2 与 `docs/audit-chain-scale-design.md` §8。

### 17.1 Seal（写入侧：插入 + 封链）

> **测量方法**：`LEVEE_BENCH_EXTENDED=1 go test ./internal/audit/ -run '^$'
> -bench '(BenchmarkAuditChainSealSteadyState|BenchmarkAuditRecordSealsWholeChain)/sealed_rows=(100|1000|5000|25000)$'
> -benchmem -benchtime=20x -count=3`，**取 3 次中位数**。100 000 行另用**同进程交错计时**
> （一次跑里依次测封链 / 插入 / Record，宿主漂移对三者同向影响）。
> 宿主：2026-11-17 本机，**非严格空载**；驱动 `modernc.org/sqlite v1.62.0`（内嵌 SQLite 3.54.1）。

| 已封行数 | `AuditRecordSealsWholeChain`（插入+封链） | `AuditChainSealSteadyState`（仅封链） | 估算 seal 常数（µs/行） | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 100 | 0.48 ms | 0.24 ms | 2.4 | 137 KB / 2 443 | 2 443 |
| 1 000 | 2.47 ms | 2.52 ms | 2.5 | 1.29 MB / 24 091 | 24 091 |
| 5 000 | 12.23 ms | 13.41 ms | 2.7 | 6.46 MB / 120 303 | 120 303 |
| 25 000 | 68.6 ms | 73.9 ms | 3.0 | 32.4 MB / 603 200 | 603 200 |
| 100 000 | **≈275 ms**（交错同进程） | **≈276 ms**（交错同进程） | 2.8 | 129.5 MB / 2 412 709 | 2 412 709 |

> **100 000 行为什么不给单一中位数**：本机在一小时内的绝对值漂移可达 2×（同一段代码、同一份数据：
> 封链 0.34 s–0.76 s，11 个样本里还有 1 个 4.6 s 的离群值）。**跨轮的中位数因此不能互相比较**，
> 也不该被当成一个数引用。上表 100 000 行给的是**同进程交错计时**的数（本机唯一自洽的写法）：
> 封链 **276 ms**、单条插入 **< 1 ms**、Record **275 ms**。
>
> **一个诚实的负面结论**：100 000 行时 `Record`（插入+封链）**并不比** `Seal`（仅封链）贵——
> 插入是亚毫秒级的，**写路径的成本就是那次全链走读**。此前跨轮测出的「Record 是 Seal 的两倍」
> 是宿主漂移，不是算法差异。

> **大规格是环境变量门控的**（`LEVEE_BENCH_EXTENDED=1` 才跑到 25 000/100 000）：CI 的 benchmark
> 腿是 `-benchtime=1x -timeout 300s` 的**存活门**（不是测量），播种 10 万行在 runner 上要 ~25 s，
> 不能默认加进去。

### 17.2 Verify（读取侧：只读校验，全链重算）

`AuditChainBuilder.Verify` 走一遍整条链并**重算每一行的摘要**（与 Seal 的修补分支不同，它
是纯读路径）。这是审计员会问、投标附录会引的那个数。

> **测量方法**：`go test ./internal/audit/ -run '^$' -bench 'BenchmarkAuditChainVerifySteadyState'
> -benchmem -benchtime=20x -count=3`，**取 3 次中位数**（本文件末尾「使用注意」要求的口径）；
> 25 000 / 100 000 两格另采了 5 次（见下方样本说明）。
> 测量 commit：本节所在的分支头（master `5c553aa` 之上）；被测文件内容与 master 一致。
> 宿主：2026-11-17 本机，**非严格空载**（另有 agent 会话），绝对值仅供相对比较。
> 驱动：`modernc.org/sqlite v1.62.0`（内嵌 **SQLite 3.54.1**）。

| 已封行数 | `AuditChainVerifySteadyState`（整链重算） | 估算 verify 常数（µs/行） | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| 100 | 0.271 ms | 2.71 | 137 KB / 2 444 | 2 444 |
| 1 000 | 2.22 ms | 2.22 | 1.29 MB / 24 092 | 24 092 |
| 5 000 | 12.16 ms | 2.43 | 6.46 MB / 120 304 | 120 304 |
| 25 000 | 69.17 ms | 2.77 | 32.4 MB / 603 201 | 603 201 |
| 100 000 | **293.83 ms** | 2.94 | 129.5 MB / 2 412 709 | 2 412 709 |

> **样本与漂移**：100 000 行的整链校验共采了 **16 个样本**（两轮基准 × 两个驱动版本，SQLite 引擎
> 同为 3.54.1）：**284–393 ms**，中位数 **296 ms**；25 000 行 11 个样本：67.6–145.2 ms，中位数 72.3 ms。
> 表内取**样本最多那一轮**（5 次）的中位数。本机一小时内的绝对值漂移可达 2×，
> 因此对外引用应写「**0.28–0.39 s**」或「中位数约 0.30 s」，不要写成某个精确到毫秒的值。

#### 归因：此前那 7.6 倍的「每行成本随规模上升」是 O(N²) 的分页重扫，不是摘要计算

上一版本节写的是「每行成本从 2.5 µs/行涨到 19.0 µs/行（10 万行比 1 千行高 7.6 倍），
按常理应来自存储读取路径与分配压力，**但本文件不把它当已证实的结论**」。2026-11-17 做了归因实测，
结论是：**它是一个具体的 bug，不是「常理」。**

**症状**（2026-11-17 实测，10 万行链）：

- 逐页耗时随页号**线性增长**：第 0 页 2 ms → 第 50 页 20 ms → 第 90 页 36 ms；
- 每页都要从索引最左端重新扫：两种查询形状的 `EXPLAIN QUERY PLAN` **都是**
  `SCAN audit USING INDEX idx_audit_chain`——带游标的那条**没有变成区间定位**；
- 整链走读 1.944 s = **19.4 µs/行**，而摘要计算本身约 1 µs/行。

**根因**：keyset 分页谓词写成了 OR 形式

```sql
(timestamp > ? OR (timestamp = ? AND id > ?))
```

SQLite **不能把 OR 当作索引区间**（OR 的两臂各自可走索引，但合起来不是一条区间），
于是该谓词被当作**过滤条件**施加在一次全索引扫描上；而全扫描意味着**每一页都从最左端重来**，
走读的总量是 O(N²)。

**证据链**（每一步都可复现）：

| 证据 | 手段 | 结果 |
| --- | --- | --- |
| 每页从头重扫 | 逐页计时 | 2 ms → 36 ms（线性） |
| 谓词不是区间 | `EXPLAIN QUERY PLAN`（修复前后各一次） | `SCAN audit USING INDEX …` → `SEARCH audit USING INDEX idx_audit_chain ((timestamp,id)>(?,?))` |
| 耗时在 SQLite 内部 | 显式 `runtime/pprof` 剖析（排除播种与封链） | `ListAuditChainPage` cum 86.53%；`_sqlite3VdbeExec` 82%；`_applyNumericAffinity` 34.41%；`_sqlite3AtoF` 24.45%；`_moveToLeftmost` 1.58 s |
| 同进程 A/B | 同一份数据上跑两种谓词 | OR 形式 1.937 s（19.37 µs/行）→ row-value 形式 **235 ms（2.35 µs/行）**，8.2× |

**修法**：谓词改成 **row-value 比较** `(timestamp, id) > (?, ?)`——SQLite 能把它用作
`idx_audit_chain (timestamp, id)` 上的区间定位；PostgreSQL 侧同形改写（它的 planner 本来就能
用 OR 的两臂走索引，不是二次方，但**两个后端读链的方式一致**这件事本身值得守住）。
语义等价性有守卫：`internal/state/audit_chain_plan_test.go` 同时钉住
**计划形状**（必须是 `SEARCH`、不许 `SCAN`）、**row-value ≡ OR 的逐页对账**、**参数记账**，
并对 PostgreSQL 侧钉同一条（`LEVEE_PG_TEST_DSN` 门控，CI 的 postgres 腿跑）。

> **为什么谓词形状值得一个守卫测试**：OR 形式与 row-value 形式**返回同一批行**，
> 只是一个贵一个便宜——只断言结果的测试**永远抓不到这个退化**。所以守卫断言的是**计划形状**。

#### 修复前后对照（同一天、同一台宿主、同一口径：`-benchtime=20x -count=3` 取中位数）

| 已封行数 | 修复前 | 修复后 | 提速 | 修复前 µs/行 | 修复后 µs/行 |
| --- | --- | --- | --- | --- | --- |
| 100 | 0.28 ms | 0.271 ms | — | 2.80 | 2.71 |
| 1 000 | 2.51 ms | 2.22 ms | — | 2.51 | 2.22 |
| 5 000 | 15.80 ms | 12.16 ms | 1.30× | 3.16 | 2.43 |
| 25 000 | 149.6 ms | 69.17 ms | **2.16×** | 5.98 | 2.77 |
| 100 000 | **1.90 s** | **293.83 ms** | **6.47×** | 19.03 | 2.94 |

三件事值得记下来：

1. **每行成本现在持平了**：2.71 → 2.94 µs/行（100 → 100 000 行），此前是 2.80 → 19.03 µs/行。
   **这才是这次修复的成果**——不是「1.9 s 变成 0.29 s」这一个数，而是**成本不再随链长恶化**。
   前两行（100 / 1 000 行）**按构造**不受影响：链不足两页时根本不会用到游标谓词，
   两栏的差异在该噪声量级内（修复前 2.51 vs 修复后 2.22 µs/行即噪声）。
2. **Verify 比 Seal 便宜**，且便宜得合理：Seal 要写链（`UpdateAuditChain` 逐行落盘），
   Verify 只读不写。25 000 行上 69.17 ms vs 73.9 ms。**审计校验不是写路径的瓶颈**。
3. **每行一次分配**（allocs/op ≈ 行数），1.29 MB/1 000 行、129.5 MB/100 000 行——与 Seal 同量级，
   说明链的内存画像由「每行一份规范化载荷」主导，与写读无关。修复顺带把 100 000 行的
   分配数从 2 485 037 降到 2 412 709（−2.9%）：OR 形式会把扫过又丢掉的行也分配出来。

#### 下一个杠杆（已量化，未做）

`audit.timestamp` 列声明为 `DATETIME`——**在 SQLite 里 DATETIME 的 affinity 是 NUMERIC**，
而列里存的是 RFC3339 文本，于是每一行都要尝试一次「文本→浮点」转换（剖析里的
`_applyNumericAffinity` 34.41% / `_sqlite3AtoF` 24.45% 就是它）。同进程 A/B 实测：

| timestamp 列声明 | OR 谓词 | row-value 谓词 |
| --- | --- | --- |
| `DATETIME`（现状，NUMERIC affinity） | 1.937 s | 235 ms |
| `TEXT`（TEXT affinity） | 1.055 s | **166 ms** |

也就是说，改成 TEXT affinity 还有 **1.4×**（235 → 166 ms）。**本笔不做**：
把列类型改掉要**重建 audit 表**（连带 WORM 触发器与迁移路径），而收益是 1.4×——这个交换不划算，
且 WORM 邻域的迁移风险与「读路径快 1.4×」不成比例。方案与触发条件见
`docs/audit-chain-scale-design.md` §8。

> 同 §17.1：**本组不进入 CI 门控**（CI 上跑这个只会测出 runner 的体型），**也不构成任何
> 招投标门槛值**。对外引用须在干净宿主上按本文件末尾的口径复跑并登记机器型号。

---

## 18. 历史（v1.3.0 Phase 1+2，2026-08-16）

> 仅作历史存档；**该数据仅在 `-count=1` 与 `-count=3` 混跑**，且基线 go1.25（v1.3.0）。
> 本节**不被任何当前决策引用**；收录是因为本文件此前的正文就是这组数据，git 历史里找不到更好的「为什么这些数字不可比」的例子。
> 详见 git 历史 `docs/benchmark-report.md` @ 2026-08-16。

---

> **使用注意**：本报告**不构成新系统的承诺值**；任何对外基准声明（tender/SLA/招投标）须在干净宿主（`go test -run='^$' -bench Benchmark -benchmem -count=3 ./internal/...`）复跑取 3 次中位数，并登记 `verify (xinchuang)` 或目标机器型。

---

## 19. 2026-11-20 复测（P2-A 收口）

> **口径**：`master@6d51d9f`，`go1.26.11 windows/amd64`（基线 §1 为 go1.26.6，跨工具链不可逐项比较），
> 同一台 Ryzen 9 7945HX / 32 threads / 31.2 GB，命令同 §1（`-count=1`）。
> **宿主状态**：复测期间另有多个 agent 会话，`govulncheck` 也在并行跑——**非空载**，
> 实测值整体偏慢，且已知本机一小时漂移可达 2×（§17.2）。本轮**只作同轮横截面**，
> **不与上表逐项比大小**，更不把差异解读为性能回归。

`go test -bench=Benchmark -benchmem -count=1 -run='^$' ./internal/...` → **126 条 / 14 包**
（基线记 128 条；差异为本轮未跑需要环境变量门控的扩展项），退出码 0。

本轮横截面（ops/s，四舍五入；仅用于同轮内部相对量级）：

| 项 | 本轮 ops/s | 包 |
| --- | --- | --- |
| DSL Parse（standard / large） | ~6.8K / ~1.6K | dsl |
| Plan Generate（serial / large） | ~86.8K / ~9.2K | plan |
| Plan ComputeHash（standard / large） | ~6.3K / ~0.6K | plan |
| Plan VerifyHash | ~3.8K | plan |
| Audit TraceRecorder Record | ~126 | audit |
| Audit Seal steady-state（100 行） | ~3.5K | audit |
| Audit Verify steady-state（100 行） | ~2.8K | audit |
| Credential encrypt / decrypt | ~1.9 / ~1.7 | credential |
| LockStore Acquire / Release | ~2.8K / ~39.9K | lock |
| Permission Check | ~15.1M | permission |

原始输出留档 `C:\Users\winge\AppData\Local\Temp\opencode\bench-rerun.txt`（16.7 MB）。

**审计链大规格再确认（同轮、非空载）**：`Seal steady-state` 5 000 行 20.2 ms、
`Verify` 5 000 行 20.4 ms、`Verify` 100 000 行 **0.588 s**（§17.2 基线 0.294 s， ratio≈2×，
正落在已登记的漂移区间，且本轮有并行负载）。**结论不是「变慢了」而是「本轮不能用来对绝对值下结论」**——
与 §17.2 的诚实负面结论一致：`Record` 与 `Seal` 同量级，写路径成本是全链走读，
每行成本不随链长恶化（修复后的 row-value 谓词守卫仍在 `internal/state/audit_chain_plan_test.go`）。

**收口判定**：本轮未发现超出既有漂移解释范围的新热点；`docs/benchmark-report.md` 继续定位为
**研发侧相对比较**。对外仍按文末「使用注意」——干净宿主、`-count=3` 取中位数、登记机器型，
**不作招投标门槛值/SLA**。
