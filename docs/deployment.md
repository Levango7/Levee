# LEVEE 生产部署与升级手册

本手册面向生产环境，覆盖 LEVEE 的部署拓扑、安全加固、系统服务化、监控接入、数据备份与版本升级。所有旗标与配置键均与当前代码实现核对，未包含未实现的能力。

> 适用版本：v1.12.0 及以后。命令行细节以 `levee <cmd> --help` 与 [cli-reference.md](cli-reference.md) 为准。

## 1. 部署拓扑

LEVEE 是单二进制程序，按角色拆分为三类常驻进程，可按需组合：

| 进程 | 命令 | 职责 | 默认端口 |
|------|------|------|----------|
| API 服务 | `levee serve` | gRPC 服务 + REST 网关（`/api/v1/`）+ `/healthz` + `/metrics` | gRPC `:9090`、HTTP `:8080` |
| Web UI | `levee web` | 服务内嵌 Vue 3 SPA，并可把 `/api/*` 代理到 API 服务 | `:8080` |
| 告警网关 | `levee alert serve` | 接收 Prometheus / 自定义 webhook 告警，去重/聚合/静默 | `:9095` |

- **单机部署**：一个 `levee serve`（SQLite 存储）+ 一个 `levee web` 即可满足大多数场景；需要告警接入时再加 `levee alert serve`。
- **集群部署**：多个 `levee serve` 节点共享一个 PostgreSQL 存储后端（`--cluster`）。集群协同分四层，均已接线：
  - **持久化成员注册**：节点心跳写入 PG `cluster_nodes` 表（含 stale 检测），**不是进程内状态**（`internal/cluster/pg_registry.go`）。
  - **leader 选举 + 租约式分布式锁**：选举规则为「优先 `role=master` 且 `status=active` 的最小 ID，否则回退到最小 ID 的 active worker」（`internal/cluster/node.go` 的 `ElectLeader`）；锁带租约、过期可抢占。
  - **执行围栏与故障接管**：运行中的变更持执行租约（`--cluster-exec-lease-ttl`），执行节点崩溃后由 leader 接管循环（`--cluster-takeover-interval`）把其运行中变更收敛到 `interrupted` 终态，**不重跑副作用**（审计留痕，需再驱动用 `RetryChange`）。
  - **跨节点调度**：leader 把已批准 run 分派给空闲 worker 节点（`--cluster-dispatch-interval` / `--cluster-dispatch-capacity` / `--cluster-dispatch-claim-timeout`），worker 节点在本机执行。
  后两层是 **leader-only 循环且需要 `--engine-enabled`**；把对应间隔设为 `<= 0` 可单独关闭该循环（关闭接管循环时**执行围栏仍然生效**）。`--cluster` 启动时会在日志中打印一行协同能力摘要（`cluster coordination: shared storage, membership, locking; ...`）。

分布式执行 Agent（`levee agent start`）为独立常驻进程，注册到 master 节点承担任务执行，见 [cli-reference.md 第20章](cli-reference.md)。

> **告警摄入有两条路径，不要混用。** `levee serve` 内部也注册了 gRPC `AlertService`，但它以
> **nil gateway** 构造（`cmd_serve.go` 的 `grpc.NewAlertService(nil, ...)`）：`ReceiveAlert` 仍会接受告警，
> 但只写入**内存 ring** 并广播给订阅者——**不落库、不套用静默规则、进程重启即丢**。
> 需要持久化、去重、聚合与静默，请部署 `levee alert serve`（`:9095`），它构造的是完整的 `AlertGateway`。

## 2. 前置要求

- 操作系统：Linux（推荐）/ macOS / Windows。Linux 下插件沙箱可启用 cgroup v2 硬限制（需 `/sys/fs/cgroup` 可写），不可用时自动降级为墙钟超时并输出 WARN。
- 存储：单机默认 SQLite（内嵌，无外部依赖）；集群需一个可达的 PostgreSQL 实例。
- 网络：API 节点之间、CLI/Web 到 API 节点的连通性；SSH/WinRM 到目标机的连通性。
- 凭据主密码：`LEVEE_MASTER_PASSWORD` 环境变量，运行时注入，**不落盘**。

## 3. 获取二进制

```bash
# 源码构建（需 Go 工具链；前端已预构建并 go:embed，无需 node）
make build

# 跨平台编译
make cross-build
```

构建产物为单一可执行文件 `levee`。生产部署只需分发该二进制 + 配置文件，无需运行时依赖。

> 若修改了 `web/` 前端源码，需先 `make web` 重新构建并更新 `internal/web/dist`，再 `make build`，否则内嵌 UI 仍是旧版。

**发行版与镜像**：

- Release 页提供六平台归档（`levee_<版本>_{linux,darwin,windows}_{amd64,arm64}`）；
- 每个 `v*` tag 同时构建多架构容器镜像 `ghcr.io/levango7/levee:<版本>`（默认私有，拉取需凭据）；
- 仓库自带交付物见 [deploy/README.md](../deploy/README.md)：裸金属一键安装脚本（`deploy/baremetal/install.sh`，幂等）、可直接安装的 systemd 单元（`deploy/systemd/levee.service`）、Helm chart（`deploy/helm/levee`，见 §14）。

## 4. 配置

配置文件查找顺序：

1. `--config` / `-c` 显式指定的路径（必须存在，否则启动失败）
2. 默认位置 `~/.levee/config.yaml`

未找到配置文件时全部使用内置默认值 + 环境变量。完整键位参考 [`config.example.yaml`](../config.example.yaml)（与 `internal/config/config.go` 一一对应）。

**单个配置键的生效优先级**（高 → 低）：

```
环境变量 LEVEE_<SECTION>_<KEY>  >  配置文件值  >  内置默认值
例：LEVEE_DATABASE_PATH=/var/levee/levee.db 覆盖 database.path
    LEVEE_LOG_LEVEL=debug                   覆盖 log.level
```

生产建议：

- `server.data_dir` 指向独立数据盘（如 `/var/lib/levee/data`），SQLite 库自动落在 `<data_dir>/levee.db`。
- `server.log_format: json` + `log.output: /var/log/levee/levee.log`，便于日志采集。
- `channel.ssh.strict_host_check: true` 保持开启（默认即 true），防中间人。
- 需要提权时再用 `channel.ssh.become_method: sudo` + 专用账号，且目标机已配置免密 sudo；默认不提权。

## 5. 安全加固

### 5.1 鉴权（必须）

`levee serve` 设有**启动门禁**：未配置任何 token 且未显式 `--insecure` 时拒绝启动，避免生产环境意外暴露无鉴权 API。

三种注入 token 的方式：

```bash
# 方式一：命令行单令牌
levee serve --token <secret>

# 方式二：环境变量单令牌
LEVEE_TOKEN=<secret> levee serve

# 方式三：命名多令牌（可重复，name 成为认证主体）
levee serve --token <secret> --auth-token alice=<tok-a> --auth-token ci-bot=<tok-ci>
```

- 设置任一 token 后，gRPC 与 REST 网关均要求客户端携带匹配的 `Authorization: Bearer <token>`。
- **命名多令牌**：每个 `--auth-token name=secret` 映射到一个主体（subject）。命名令牌认证后，其主体注入请求上下文并**优先于**客户端自报的 `X-Acting-As`，使审计归属为“被证明的身份”而非“断言”。`--token` 单令牌行为完全向后兼容（不注入主体）。
- 生产环境**不要**使用 `--insecure`。

### 5.2 TLS

```bash
levee serve --addr :9090 --tls-cert /etc/levee/tls/server.crt --tls-key /etc/levee/tls/server.key --token <secret>
```

省略 `--tls-cert` / `--tls-key` 时为明文传输；若由上游负载均衡/反向代理终结 TLS，可让代理到 LEVEE 的内网链路保持明文，但须确保该链路隔离。

### 5.3 `/metrics` 端点鉴权

启用任一 token 后，运维端点 `/metrics` **默认同样要求 Bearer 鉴权**。无法携带凭据的采集器（如某些 Prometheus 部署）可用 `--metrics-public` 显式放开：

```bash
levee serve --token <secret> --metrics-public
```

建议优先让 Prometheus 携带 token 抓取（见第 9 节），而非放开匿名访问。

### 5.4 CORS 与限流

- CORS 默认拒绝所有跨域请求；同源不受影响。确需跨域时用 `--cors-origin` 白名单，避免 `*`。
- REST 网关默认全局限流 200 req/s（令牌桶，突发 400）；触发时返回 429 + `Retry-After`。按容量用 `--rate-limit` / `--rate-burst` 调整。gRPC 原生端口无内置限流，请在 LB/网关侧实施。

### 5.5 凭据主密码

`LEVEE_MASTER_PASSWORD` 用于凭据加密（AES-256-GCM + argon2id），仅在运行时通过环境变量注入，**绝不写入配置文件或镜像层**。

### 5.6 审计哈希链密钥（可选，建议生产启用）

设置 `LEVEE_AUDIT_HMAC_KEY`（≥16 字节口令）后，审计校验和与哈希链摘要由无密钥 SHA-256 升级为 **HMAC-SHA256**：只有持有密钥的一方能够构造出可验证的链，从而提供来源真实性（防"知情者重建整条链销毁篡改痕迹"）。未设置时回退无密钥 SHA-256 并在启动时输出 WARN；短于 16 字节视为无效口令，同样回退并 WARN。

注意事项：
- 与主密码同级管理（环境变量注入、不落盘）；所有读写同一审计库的进程必须配置**相同**的密钥，否则验链失败。
- 密钥一经启用不可随意更换：更换密钥会使存量记录的摘要无法复算，等效于断开历史链。确需轮换时先归档旧审计库，再以新密钥开新链。
- 密钥仅参与摘要计算，不进入审计记录内容，不会出现在 trace/detail 中。

### 5.7 SQLite 持久级别（`state.sqlite_synchronous`）

默认 `normal`：WAL 检查点时批量 fsync，数据库不会损坏，但操作系统整机崩溃时可能丢失最近几秒的 WAL 写入。审计强持久场景（合规要求"已确认记录的审计事件不得因断电丢失"）设 `full`：每事务提交即 fsync，写放大约 1-2%。非法值拒绝启动。环境变量 `LEVEE_STATE_SQLITE_SYNCHRONOUS` 同步可用；集群模式（PostgreSQL）不受此项影响。

## 6. 以系统服务运行（systemd 示例）

`/etc/systemd/system/levee.service`：

```ini
[Unit]
Description=LEVEE API server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=levee
Group=levee
EnvironmentFile=/etc/levee/levee.env
ExecStart=/usr/local/bin/levee serve \
  --config /etc/levee/config.yaml \
  --addr :9090 --http-addr :8080 \
  --tls-cert /etc/levee/tls/server.crt --tls-key /etc/levee/tls/server.key
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=read-only

[Install]
WantedBy=multi-user.target
```

`/etc/levee/levee.env`（权限 600，属主 levee）：

```ini
LEVEE_TOKEN=<api-token>
LEVEE_MASTER_PASSWORD=<master-password>
# 命名多令牌示例（按需追加）：
# LEVEE 暂以命令行 --auth-token 传入；如需环境变量管理，可改写 ExecStart 追加 --auth-token 参数
```

Web UI 单独成服务（`levee-web.service`），`ExecStart=/usr/local/bin/levee web --addr :8081 --api http://127.0.0.1:8080`，或由反向代理统一入口。

启用：

```bash
systemctl daemon-reload
systemctl enable --now levee
systemctl status levee
```

**仓库自带可直接使用的版本**：`deploy/systemd/levee.service`（含最小权限加固：`ProtectSystem=strict`、`ReadWritePaths` 限定数据目录、`$LEVEE_SERVE_EXTRA` 展开集群参数）与 `deploy/baremetal/install.sh`——幂等安装（建用户/装二进制/渲染单元/生成 0600 凭据模板），容器实测可直接重复执行。用法：

```bash
sudo ./deploy/baremetal/install.sh --binary ./levee --start
```

## 7. Web UI

`levee web` 服务内嵌 SPA，并可把 `/api/*` 代理到 API 服务：

```bash
# 与 API 同机，代理到本机网关
levee web --addr :8081 --api http://127.0.0.1:8080
```

生产通常用 Nginx / 反向代理把 `/`（UI）与 `/api/`（网关）收敛到同一域名与 TLS 证书下，避免跨域。

## 8. 健康检查

- `GET /healthz`（位于 `--http-addr`）：服务注册完成后返回 200；未就绪返回 503 `{"status":"unavailable"}`。用于负载均衡健康探测与启动探针。
- 命令行自检：`levee system doctor`（config / database / permission_matrix 三项）。

## 9. 监控接入（Prometheus）

`/metrics` 以 Prometheus text 0.0.4 格式暴露 10 组指标（变更生命周期、批处理耗时、门禁、审批、通道获取、锁、回滚、备份、告警等）。

**携带 token 抓取（推荐）**：

```yaml
scrape_configs:
  - job_name: levee
    metrics_path: /metrics
    static_configs:
      - targets: ["levee-1:8080"]
    bearer_token: "<api-token>"
    # 若 API 启用 TLS：
    # scheme: https
    # tls_config: { insecure_skip_verify: false }
```

若采集器无法携带凭据，才在 `levee serve` 上加 `--metrics-public`，并建议用网络策略限制该端口的访问来源。

链路追踪：`serve` 启动时初始化 OpenTelemetry（`tracing` 配置段：`enabled` / `exporter` / `endpoint`，默认关闭），失败时优雅降级为 noop。

## 10. 数据备份与恢复

使用内置 `levee backup` / `levee restore`（详见 [cli-reference.md 第28章](cli-reference.md)）：

```bash
# SQLite 备份（VACUUM INTO，守护进程运行时亦安全），附带 .sha256 校验和
levee backup --output /backup/levee-$(date +%F).db

# PostgreSQL 备份（纯 Go SQL dump，无需 pg_dump）
levee backup --pg-dsn "$LEVEE_PG_DSN" --output /backup/levee-$(date +%F).sql

# 定期校验已有备份
levee backup --output /backup/levee-2026-08-28.db --verify-only
```

建议用 cron 周期备份并把产物异地留存。恢复见第 11 节升级流程与 cli-reference。

> PostgreSQL 恢复语义（v1.13+）：恢复到**空库**是主路径——恢复前自动重放迁移建齐 schema，再按外键依赖拓扑序回放，旧版本备份同样可用；目标库**仍有数据**时恢复会被拒绝并列出持数表，仅灾难恢复场景可加 `--allow-destructive-restore`（单事务内临时挂起 WORM 触发器、提交前校验恢复）。CI 对 PG 备份/恢复做全链路演练门禁（建库→造数→dump→恢复→逐表指纹对比）。

## 11. 升级流程

LEVEE 为单二进制，升级即“备份 → 替换 → 验证”。推荐流程：

1. **冻结变更**（可选但建议）：升级窗口内避免发起新变更，或用 `levee pause-all --reason "升级"` 全局暂停。
2. **备份当前数据**：
   ```bash
   levee backup --output /backup/pre-upgrade-$(date +%F).db
   ```
3. **记录当前版本**：`levee version`。
4. **替换二进制**：将新版 `levee` 覆盖 `/usr/local/bin/levee`（或先放到旁路路径并原子 `mv`）。
5. **重启服务**：`systemctl restart levee`（及 `levee-web` / `levee alert serve` 等相关服务）。
6. **健康验证**：
   - `curl -fsS http://127.0.0.1:8080/healthz` 返回 200；
   - `levee system doctor` 全部 OK；
   - `levee version` 显示目标版本；
   - 抽查一条变更 `levee list --limit 1` 与审计 `levee audit verify <run-id>`。
7. **恢复服务**：若第 1 步执行了全局暂停，`levee resume-all --reason "升级完成"`。

**回滚**：若新版本异常，用升级前备份恢复后回退二进制：

```bash
systemctl stop levee
levee restore --input /backup/pre-upgrade-<date>.db --yes
# 将旧版二进制 mv 回 /usr/local/bin/levee
systemctl start levee
```

> SQLite 恢复会自动先写 `<db>.pre-restore` 安全快照，误恢复可据此再次回退；恢复前会校验 SHA-256 与 `integrity_check`，校验失败不做任何替换。

**跨版本注意事项**：升级前阅读 [CHANGELOG.md](../CHANGELOG.md) 对应版本的 “变更/安全” 段落，确认是否有旗标默认值或行为变化（例如本版本 `/metrics` 默认鉴权、REST 方法校验收紧）。

## 12. 集群模式（如实说明）

```bash
levee serve --cluster \
  --pg-dsn "postgres://user:pass@pg:5432/levee" \
  --node-id node-1 --node-addr 10.0.0.11:9090 --node-role master \
  --token <secret>
```

当前能力边界：

- **已具备**：
  - 共享 PostgreSQL 存储带来的数据一致性；
  - 持久化成员注册：节点写入 `cluster_nodes` 表并周期心跳（默认 10s 一次，30s 未心跳被对端标记 offline），各节点本地视图从共享表收敛，leader 按确定性策略（active master 最小 ID，缺则 active worker 最小 ID）在各节点独立收敛；
  - 租约式分布式锁（`cluster_locks` 表）：锁带租约过期时间，持有者需周期续租；持有者崩溃或失联后租约到期，其他节点可自动抢占接管，无需人工干预。每次获取/抢占携带单调递增的 fence token，供未来的接管工作做隔离校验。
- **尚未实现**：在途变更的自动故障转移（节点崩溃时其正在执行的变更不会自动由他端续跑）、跨节点任务调度。节点角色（`--node-role master|worker`）当前主要用于标识与 leader 收敛。

因此多节点部署应视为“共享存储 + 共享成员/锁 + 多接入点”，可用性提升依赖外部负载均衡与健康检查（`/healthz`）摘除故障节点，而非内置的在途工作自动切换。启动时的告警日志会重申这一点，请在容量与 SLO 评估中纳入。

## 13. 常见问题

- **服务拒绝启动，提示缺少 token**：这是启动门禁生效。配置 `--token` / `LEVEE_TOKEN` / `--auth-token` 之一；仅本地开发可用 `--insecure`。
- **`/metrics` 返回 401**：启用鉴权后该端点默认需要 Bearer token。让采集器携带 token，或显式 `--metrics-public`。
- **REST 接口返回 405 method not allowed**：本版本起变更类操作强制 POST、`logs`/`trace` 强制 GET；请校正客户端请求方法。
- **Web UI 打开但无数据**：确认 `levee web --api` 指向的网关地址可达，且浏览器请求携带了有效 token（同源代理时由网关鉴权）。
- **SSH 目标机连接失败**：检查 `channel.ssh` 配置（端口/密钥/known_hosts）；`strict_host_check` 开启时首次连接需先建立 known_hosts 记录。

## 14. Kubernetes（Helm）部署

客户有 K8s 时用仓库自带的 chart（`deploy/helm/levee`），现场只需填 `values.yaml`：

```bash
helm lint deploy/helm/levee                                   # 先校验
helm install levee deploy/helm/levee -f your-values.yaml      # 单机（SQLite+PVC）
helm install levee deploy/helm/levee -f your-values.yaml \
  --set mode=cluster --set postgres.dsn='postgres://...?sslmode=require'   # 集群
helm test levee                                               # 校验 /healthz
```

- 两种形态：`mode=single`（SQLite + PVC）与 `mode=cluster`（master 1 副本 + worker N 副本，共享 PG；`--node-id/--node-addr/--node-role` 由 Downward API 注入）。
- 认证、主密码、PG DSN 全部走 Secret（`auth.existingSecret` 可完全接管）；探针用网关 `/healthz`（就绪）与 TCP（存活）。
- 边界如实写在模板注释里：**Service 只指向 master 副本**——`WatchChange` 事件总线是进程内的，多副本前置会让订阅者漏事件；内嵌 PG 仅供演示（单副本无 HA），生产用客户既有 PG/RDS；集群 worker 的快照目录默认 emptyDir，需要跨 Pod 重建保留就配 `cluster.snapshotClaimName` 指向共享 RWX 卷。
- 镜像：`ghcr.io/levango7/levee:<版本>`（多架构；默认私有，配 `imagePullSecrets` 或同步到客户镜像库）。

## 15. 交付检查单（上线前逐项核对）

| # | 检查项 | 判据 |
| --- | --- | --- |
| 1 | 认证为**命名令牌或 OIDC**（非单令牌） | 审计报告里每个审批/执行都有可证明的主体；单令牌模式仅限试用（`docs/security-audit.md` 生产准入清单） |
| 2 | gRPC 端口（默认 9090）**不直接暴露** | 网络策略/防火墙只放行 REST（:8080）或全部置于内网；网关限流覆盖不到 gRPC |
| 3 | `LEVEE_MASTER_PASSWORD` 经 secret 注入 | 环境文件/Secret 0600、不落盘、不进镜像；`install.sh`/Helm 默认即如此 |
| 4 | `levee doctor` 全绿 | 配置合法性、存储连通、通道插件、数据目录可写 |
| 5 | 试点演练完成（见 `docs/scenario-cross-region-ops.md` §5） | 逐台切换演练、故意失败走收敛、窗外 plan 被拒、杀 worker 走接管、审计报告可离线阅读 |
| 6 | 备份/恢复演练跑过一次 | `levee backup` → 空库恢复（PG 主路径；破坏性恢复需显式 opt-in）——见 §10 |
| 7 | 升级路径确认 | §11 的滚动升级步骤在测试环境走过一遍；回滚到上一版本已验证 |
| 8 | 审计链校验通过 | `levee audit verify` 全绿（audit 全局链 + 每 run 的 trace 链） |

## 附录：本地覆盖率联合口径（state sqlite+PG）

`internal/state` 的质量门以 **sqlite+PG 联合口径 ≥75%** 计。两条 `-coverprofile` 分别来自两种后端，需按"同一代码块取 max 命中数"合并——用仓内工具 `scripts/merge-cover.ps1`，否则联合口径不可复现。完整命令序列：

```powershell
# 1. 起与 CI 同参数的 PostgreSQL（或复用 CI 环境）
docker run -d --name levee-ci-pg -e POSTGRES_USER=levee -e POSTGRES_PASSWORD=levee-ci `
  -e POSTGRES_DB=levee_test -p 5432:5432 postgres:16-alpine

# 2. sqlite 口径（不设 DSN，PG 用例自动 skip）
go test ./internal/state/... -count=1 -coverprofile=cover_sqlite.out

# 3. PG 口径（同一二进制内 sqlite 用例照常执行）
$env:LEVEE_PG_TEST_DSN = 'postgres://levee:levee-ci@localhost:5432/levee_test?sslmode=disable'
go test ./internal/state/... ./internal/cluster/... -count=1 -coverprofile=cover_pg.out

# 4. 合并出 state 的联合口径并查看总数
powershell -NoProfile -ExecutionPolicy Bypass -Command `
  "& .\scripts\merge-cover.ps1 -ProfilePaths cover_sqlite.out,cover_pg.out `
   -OutFile cover_merged.out -Filter github.com/nexus/levee/internal/state"
go tool cover -func=cover_merged.out | Select-Object -Last 1
```

注意：state 与 cluster 的测试二进制在该库上**并发**执行（`go test` 多包并行是默认行为），state 侧测试助手只截断自己拥有的表，不要截断 `cluster_nodes`/`cluster_locks`。

## 参考

- [cli-reference.md](cli-reference.md) — 全部命令与旗标
- [config.example.yaml](../config.example.yaml) — 完整配置键
- [security-audit.md](security-audit.md) — 安全审计结论
- [CHANGELOG.md](../CHANGELOG.md) — 版本变更记录
