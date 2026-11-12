# 平台适配矩阵（信创 / ARM / 云原生）

状态：**活文档**。每格只记本仓自己跑过的结论，标「已验证 / 未验证 / 不支持」并给取证方式。
用途：投标应答的实施前置检查；**对外话术只能引用「已验证」格**。

## 0. 读法

- **已验证**：本仓 CI 腿或本机实跑过（给证据位置）。
- **未验证**：**没有证据**——不代表不行，但对外不得承诺。
- **不支持**：代码面明确没有（不是"没试过"，是"没有这条路径"）。

> **两个轴别混**：ARM **架构**已进 CI 矩阵 ≠ **信创 OS** 已验证。前者是 `GOARCH`，后者是操作系统。

## 1. 运行时（OS × 架构）

| 目标 | 架构 | 现状 | 证据 |
|---|---|---|---|
| Linux 二进制 | amd64 | **已验证** | CI `build (ubuntu-latest/amd64)`（`GOARCH` 矩阵） |
| Linux 二进制 | arm64 | **已验证** | CI `build (ubuntu-latest/arm64)`；鲲鹏/飞腾即 aarch64，同一批产物 |
| macOS 二进制 | amd64 / arm64 | **已验证** | CI `build (macos-latest/{amd64,arm64})` |
| Windows 二进制 | amd64 / arm64 | **已验证** | CI `build (windows-latest/{amd64,arm64})` |
| 容器镜像（alpine 基础） | amd64 | **已验证** | `Dockerfile`（多阶段、`CGO_ENABLED=0 GOOS=linux`）；CI `trivy` 腿 `docker build` + 扫描 |
| 容器镜像 | arm64 | **已验证（QEMU 模拟，非 ARM 真机）** | 2026-11-12 本机 `docker buildx build --platform linux/arm64 --load .` 构建 `Dockerfile` 全链路成功（node 前端 + Go 交叉编译 + alpine 运行时）；`docker run --platform linux/arm64` 实跑 `uname -m`=aarch64、`levee version`、`levee system doctor`=healthy、`serve` 的 `/healthz` 返回 `{"status":"ok"}`。宿主为 x86_64 + Docker Desktop QEMU，不构成 ARM 真机证据，限制见 §6 |
| 麒麟 V10（Sword） | amd64 | **已验证（WSL2 用户态，非裸机）** | 2026-11-12 实跑：Kylin V10 (Sword) WSL2（内核 `6.6.87.2-microsoft-standard-WSL2`、glibc 2.28、x86_64）装 `levee` linux/amd64（`CGO_ENABLED=0`），`levee system doctor` healthy；SQLite 存储；SSH 通道到 `127.0.0.1:2222`；一条 change 走完 审批→执行（apply 前快照 `files=1`）→回滚→审计：`approved → completed → rolled_back`，目标文件 `v1 → v2 → v1`，`GET /audit/verify` 返回 `valid=true`、`entriesVerified=7`。限制见 §6 |
| 麒麟 / 统信 UOS | amd64（裸机） | **未验证** | WSL2 证据不等同裸机：内核、systemd、安全基线、驱动与 `deploy/baremetal/install.sh` 均未在信创整机上实跑 |
| 麒麟 / 统信 UOS | arm64（鲲鹏/飞腾） | **未验证** | 同上；ARM 腿跑在 ubuntu/macos/windows 上，不构成信创 OS 证据 |

## 2. 数据库（信创路线的真正未知）

| 引擎 | 现状 | 证据 |
|---|---|---|
| SQLite | **已验证**（默认） | `internal/state/sqlite.go`；`database.driver` 默认 sqlite（`internal/state/driver.go`） |
| PostgreSQL（原生） | **已验证** | `internal/state/pgstore.go`；CI `integration & e2e & postgres` 腿 |
| 达梦 DM8 | **不支持（已实测，2026-11-12）** | 镜像 `dm8_single:dm8_20241022_rev244896_x86_rh6_64`（DM Database Server 64 V8，构建 `03134284294-20241009-244896-20119`）。`pgx` 直连在握手阶段即 `unexpected EOF`——DM8 **不实现 PostgreSQL 线协议**，`COMPATIBLE_MODE` 只是语法开关。把 `dm.ini` 的 `COMPATIBLE_MODE` 改为 `7`（该文件注释即 `7:PG`）并重启后仍不兼容：`TIMESTAMPTZ` 报 `Invalid base class name`、`TEXT` 建主键报「CLOB 不能建索引」、`ON CONFLICT` 与 `TRUNCATE … RESTART IDENTITY CASCADE` 语法错误（`JSONB`、`NOW()` 可用）。要支持需**新增 DM 驱动 + SQL 方言层**，不是改配置 |
| 人大金仓 KingBase | **已验证（V9R1C10，PG 模式，2026-11-12）** | 镜像 `kingbase_v009r001c010b0004_single_x86:v1`，以 `DB_MODE=pg` 初始化单实例。`LEVEE_PG_TEST_DSN` 指向该实例后，`go test ./internal/state`（schema 迁移、审计链封链、WORM 触发器、租约/epoch CAS）与 `go test ./internal/cluster` 全绿；`levee serve` 日志 `driver=postgres location=postgres@…/levee_kb`；一条真实 change `run-8eecbcd833193a74` 走完 `approved → completed`（文件 v1→v2）→ `rolled_back`（v2→v1），`GET /audit/verify` 返回 `valid=true`、`entriesVerified=3`（run 维度 1）。限制见 §6 |
| 国产 DB 的 PG 兼容模式（通用结论） | **金仓可用；达梦不可用** | 金仓 V9 的 PG 模式与 `pgx`/LEVEE 的 PG 路径兼容；达梦的 `COMPATIBLE_MODE=7` 只覆盖部分语法且**不提供 PG 线协议**。两者不能按同一类「PG 位」处理 |

**为什么这一格最关键**：治理链整体压在 DB 上——审计链（WORM 触发器）、租约/epoch CAS、方言差异（`internal/state` 的 sqlite/pg 双实现）。信创栈上"PG 位"通常是达梦/金仓的**兼容模式**，**差异面比 OS 大**：能连上不等于审计触发器、WORM、并发语义都对得上。2026-11-12 的取证结论：**金仓 V9 的 PG 模式可直接跑通**（无需改方言）；**达梦 DM8 不能当 PG 位**（无 PG 线协议，PG 语法模式也不覆盖 LEVEE schema），要支持得单独立项做 DM 驱动 + 方言层——那不是扩云原生的前置。

## 3. 部署形态

| 形态 | 现状 | 证据 |
|---|---|---|
| 裸金属安装 | **有实现，未验证** | `deploy/baremetal/install.sh`（幂等）、`deploy/systemd/levee.service` |
| 容器镜像 | **已验证**（amd64 / arm64） | 同 §1；arm64 为 QEMU 模拟构建与运行 |
| Helm / Kubernetes 部署 | **已验证（渲染与打包门）** | `deploy/helm/levee` 完整 chart（deployment / cluster-worker / postgres / pvc / ingress / secret / configmap / service）；CI `delivery (helm lint + render + package)` 腿 |
| 多节点集群与故障接管 | **已验证**（容器靶场） | `deploy/lab/container-lab` 灾备三场景（S1 硬杀 / S2 分区 / S3 PG 重启）实测，见 `docs/design-disconnect-policy.md` §0 |

## 4. 云原生：作为**被治理的变更目标**

| 能力 | 现状 | 证据 |
|---|---|---|
| LEVEE 自己跑在 k8s 里 | **已验证**（Helm 渲染门） | `deploy/helm/levee` |
| 把 k8s 工作负载当变更目标 | **已验证（运行期通道插件，kind 实跑）** | `examples/plugins/k8s`（`type: channel`，`channel_types: [k8s, kubernetes]`，核心零行改动）；`plugin install` + `enable` 后由 `serve` 启动装载（日志 `plugin channel transports installed count=2`）；kind 集群上执行 `kubectl scale` 实测通过。边界：仅 `serve` 装载插件通道，CLI `apply` 路径不装载；`exec` 无 stdin；`target check` 的探针命令集群答不了 |
| k8s 侧变更的审批·回滚·举证 | **已验证（运行期通道插件，kind 实跑）** | 一条 change：`approved → completed`（Deployment 副本 1→3）→ `POST /rollback` → `rolled_back`（副本 3→1，按变更前快照 `kubectl get -o json` 还原）→ `GET /audit/verify` `valid=true`；run `run-205b77d4bce89594`（2026-11-12） |

设计与分片见 `docs/design-k8s-change-governance.md`。

## 5. 待补证据（谁做、怎么算完）

| 格 | 取证方式 | 前置 |
|---|---|---|
| 麒麟/统信 × amd64/arm64 | 装 `levee` → `levee doctor` → 起一条 change 走完 审批→执行→回滚→审计 | **amd64 WSL2 用户态已跑通**（2026-11-12，见 §1）；裸机 amd64、arm64 真机仍缺 |
| 达梦/金仓 PG 兼容模式 | 在兼容模式实例上跑 `internal/state` 的 PG 路径：schema 迁移 + 审计链 + 租约 CAS | **金仓已跑通（2026-11-12）**：`internal/state` + `internal/cluster` 全绿，另有一条 change 的全链路与 `audit/verify`；达梦实测**不适用**（无 PG 线协议；PG 语法模式仍不认 `TIMESTAMPTZ`、`TEXT` 主键、`ON CONFLICT`），要支持需 DM 驱动 + 方言层 |
| arm64 容器镜像 | buildx 多架构构建 + 在 ARM 容器里跑关键用例（qemu 可用与否记实） | **已跑通（QEMU 模拟，2026-11-12，见 §1）**：buildx 构建 + 容器内 `version` / `system doctor` / `serve` 的 `/healthz`；ARM 真机仍缺 |
| k8s 变更面（S2） | 本地 kind 集群三场景：改 Deployment 副本数 → 审批拒绝则不发生 → 允许则发生 → 回滚还原 → 审计链留行 | **已跑通**（#348 + #349；kind `levee-s2`，run `run-205b77d4bce89594`） |

## 6. 诚实边界

- 本表只登记**本仓自己跑过**的结论；厂商认证报告、第三方兼容性认证不在此表内。
- 「未验证」格在拿到证据前，对外只能说"需评估"，不能说"支持"。
- **WSL2 证据的边界**：2026-11-12 的麒麟 V10 证据来自 WSL2 **用户态**（Microsoft 内核），它证明「LEVEE 在麒麟 V10 用户态 / glibc 2.28 上可安装并跑通治理链」，**不能**替代裸机或整机认证。对外引用必须带这个限定；裸机 amd64 与 arm64 仍按「未验证」处理。该次实验的 OpenSSH 8.0p1 由 CentOS 8.5 vault 仓库补装（麒麟镜像未带 sshd），同属实验环境限定。
- **QEMU 证据的边界**：arm64 容器镜像证据来自 x86_64 宿主上的 Docker Desktop QEMU 模拟（`docker buildx --platform linux/arm64`），证明 Dockerfile 能交叉构建、镜像在 aarch64 用户态可启动并 serving，**不能**替代鲲鹏/飞腾真机；对外引用必须注明「QEMU 模拟」。
- **金仓证据的边界**：2026-11-12 的 KingBase 证据来自 Docker **单实例**（x86_64、`DB_MODE=pg`、V9R1C10），验证的是「金仓 V9 PG 兼容模式 + LEVEE 的 PG 路径」。**未**覆盖 KES 主备/共享存储集群、Oracle 模式、国密/等保配置、备份恢复与生产参数调优；对外引用必须写「金仓 V9 PG 模式（单实例）」。
- **达梦证据的边界**：2026-11-12 实测的是「DM8 不能作为 PG 位直接替换」，不是「达梦永远不支持」。若未来做 DM 适配，需单独立项（DM 驱动 + SQL 方言层 + `TEXT`/CLOB 主键替代方案）；完成前对外只能说「需评估 / 需适配」。
- 本表会随 CI 与实跑更新；改了结论要同步 `docs/blocked-items.md` 的对应行（同一结论写两遍就会漏改一半）。
