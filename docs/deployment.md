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
- **集群部署**：多个 `levee serve` 节点共享一个 PostgreSQL 存储后端（`--cluster`）。**当前集群协同仅限共享存储层**：数据一致性 + 咨询锁（advisory lock）已具备；节点成员注册为进程内状态，**尚无自动故障转移与跨节点调度**。`--cluster` 启动时会输出告警提示，请据此评估是否满足你的可用性要求。

分布式执行 Agent（`levee agent start`）为独立常驻进程，注册到 master 节点承担任务执行，见 [cli-reference.md 第20章](cli-reference.md)。

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
- REST 网关默认全局限流 200 req/s（令牌桶，突发 400）；触发时返回 429 + `Retry-After`。按容量用 `--rate-limit` / `--rate-burst` 调整。

### 5.5 凭据主密码

`LEVEE_MASTER_PASSWORD` 用于凭据加密（AES-256-GCM + argon2id），仅在运行时通过环境变量注入，**绝不写入配置文件或镜像层**。

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

## 参考

- [cli-reference.md](cli-reference.md) — 全部命令与旗标
- [config.example.yaml](../config.example.yaml) — 完整配置键
- [security-audit.md](security-audit.md) — 安全审计结论
- [CHANGELOG.md](../CHANGELOG.md) — 版本变更记录
