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
| 容器镜像 | arm64 | **已验证**（本地单架构构建＋运行，2026-11-12） | 本机 `docker build --platform linux/arm64` 整份 Dockerfile 39 步全过（qemu-aarch64 模拟，宿主 amd64）；产物 `docker image inspect` = `linux/arm64`、镜像内 `uname -m` = `aarch64`、`/usr/local/bin/levee` ELF machine = `0xb7`；`levee version` = `go1.26.11`，`serve --insecure` 起 `/healthz`=200 且 `/system/version` 应答。**范围如实**：本机无 docker buildx 插件，做的是**单架构 arm64 构建**，不是 release 的 `buildx --platform amd64,arm64` 多架构 manifest。 |
| 麒麟 / 统信 UOS | amd64 | **未验证** | 无对应腿；`deploy/baremetal/install.sh`（幂等安装器）与 `deploy/systemd/levee.service` 是**有实现**，未在目标 OS 上实跑 |
| 麒麟 / 统信 UOS | arm64（鲲鹏/飞腾） | **未验证** | 同上；ARM 腿跑在 ubuntu/macos/windows 上，不构成信创 OS 证据 |

## 2. 数据库（信创路线的真正未知）

| 引擎 | 现状 | 证据 |
|---|---|---|
| SQLite | **已验证**（默认） | `internal/state/sqlite.go`；`database.driver` 默认 sqlite（`internal/state/driver.go`） |
| PostgreSQL（原生） | **已验证** | `internal/state/pgstore.go`；CI `integration & e2e & postgres` 腿 |
| 达梦 DM8 | **不支持** | 无驱动；全仓零提及 |
| 人大金仓 KingBase | **不支持** | 同上 |
| 国产 DB 的 PG 兼容模式 | **未验证** | —— |

**为什么这一格最关键**：治理链整体压在 DB 上——审计链（WORM 触发器）、租约/epoch CAS、方言差异（`internal/state` 的 sqlite/pg 双实现）。信创栈上"PG 位"通常是达梦/金仓的**兼容模式**，**差异面比 OS 大**：能连上不等于审计触发器、WORM、并发语义都对得上。取证结论可能是"要改 SQL 方言"——那属于**先做 DB 适配**的独立工作，不是扩云原生的前置。

## 3. 部署形态

| 形态 | 现状 | 证据 |
|---|---|---|
| 裸金属安装 | **有实现，未验证** | `deploy/baremetal/install.sh`（幂等）、`deploy/systemd/levee.service` |
| 容器镜像 | **已验证**（amd64） | 同 §1 |
| Helm / Kubernetes 部署 | **已验证（渲染与打包门）** | `deploy/helm/levee` 完整 chart（deployment / cluster-worker / postgres / pvc / ingress / secret / configmap / service）；CI `delivery (helm lint + render + package)` 腿 |
| 多节点集群与故障接管 | **已验证**（容器靶场） | `deploy/lab/container-lab` 灾备三场景（S1 硬杀 / S2 分区 / S3 PG 重启）实测，见 `docs/design-disconnect-policy.md` §0 |

## 4. 云原生：作为**被治理的变更目标**

| 能力 | 现状 | 证据 |
|---|---|---|
| LEVEE 自己跑在 k8s 里 | **已验证**（Helm 渲染门） | `deploy/helm/levee` |
| 把 k8s 工作负载当变更目标 | **已验证**（S1＋S2，master `f122a10`/`6c0f273`） | S1 策略门禁 `internal/k8spolicy`（11 条）＋ `internal/verify.K8sPolicyGate`（critical 阻断 / warning 报出 / 解析失败 fail-closed）；S2 通道插件 `examples/plugins/k8s`（`type: channel`，`channel_types: [k8s, kubernetes]`，**核心零行改动**）＋ 契约单测 22 例 ＋ kind 实测 `TestK8sChannelDrill`。**边界**：插件通道仅在 **serve 启动时装入**，CLI `apply --engine-enabled` 不装；门禁的**注册与 DSL 接线**待协调；`target check` 的 `echo ok` 探针集群答不了；exec 无 stdin，manifest 走 upload（见设计文档 S1/S2 段） |
| k8s 侧变更的审批·回滚·举证 | **已验证**（serve 三场景验收） | 审批拒绝→不执行 / 批准→`kubectl scale` 真实执行 / rollback→按快照还原（剥 resourceVersion/uid/status）/ 审计链 `Valid: true`；机制与边界见 `docs/design-k8s-change-governance.md` S2 段，验收记录见 agent 板 `opencode-cloudnative-track.md` |

设计与分片见 `docs/design-k8s-change-governance.md`。

## 5. 待补证据（谁做、怎么算完）

| 格 | 取证方式 | 前置 |
|---|---|---|
| 麒麟/统信 × amd64/arm64 | 装 `levee` → `levee doctor` → 起一条 change 走完 审批→执行→回滚→审计 | 一台（或一台容器化）目标机 |
| 达梦/金仓 PG 兼容模式 | 在兼容模式实例上跑 `internal/state` 的 PG 路径：schema 迁移 + 审计链 + 租约 CAS | 一个 DB 实例；结论可能是"要改方言" |
| S1 门禁的注册 / DSL 接线 | 把 `internal/verify.K8sPolicyGate` 注册进 gate 家族并接进 DSL 与 serve 装配 | 与 `internal/dsl`/`cmd/levee` 认领者协调 |

> **2026-11-12 消除两格**：**arm64 容器镜像**（见 §1）与 **k8s 变更面 S2**（见 §4）已由「未验证/不支持」转为「已验证」，不再列本表。

## 6. 诚实边界

- 本表只登记**本仓自己跑过**的结论；厂商认证报告、第三方兼容性认证不在此表内。
- 「未验证」格在拿到证据前，对外只能说"需评估"，不能说"支持"。
- 本表会随 CI 与实跑更新；改了结论要同步 `docs/blocked-items.md` 的对应行（同一结论写两遍就会漏改一半）。
