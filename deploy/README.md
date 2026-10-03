# deploy —— LEVEE 交付物（裸金属 / 容器）

三条交付路径，按客户环境选一条：

| 路径 | 适用 | 入口 |
| --- | --- | --- |
| **裸金属 / VM（推荐主线）** | 物理机房、无 K8s、等保要求最小依赖 | `deploy/baremetal/install.sh` + `deploy/systemd/levee.service` + [deployment.md](../docs/deployment.md) |
| **Kubernetes（Helm）** | 有 K8s 的客户，一配置即部署 | `deploy/helm/levee/`（`helm install levee ./levee -f your-values.yaml`） |
| **容器（无 K8s）** | docker/podman 直接跑 | `docker run ghcr.io/levango7/levee:<版本> serve ...`（仓库根 Dockerfile 构建） |

## 镜像来源

- 每个 `v*` tag 的 Release 会构建并推送多架构镜像到 **GHCR**：
  `ghcr.io/levango7/levee:<版本>` 与 `:latest`（linux/amd64 + linux/arm64）。
- GHCR 包默认**私有**：客户现场拉取需要把镜像同步到客户镜像库，或配置
  `imagePullSecrets`（Helm 的 `imagePullSecrets` / 裸金属的 `docker login`）。
- 离线/气隙现场：`docker save` 导出后 `docker load`，或直接用二进制包
  （Release 的 `levee_<版本>_linux_<arch>.tar.gz`）。
  注意二进制内已 `go:embed` 前端产物，**不需要**单独发前端文件。

## 模板与真实旗标的一致性

Helm 模板里的每个参数都对应 `levee serve --help` 的真实旗标；改动模板前先核对：

```bash
levee serve --help
```

模板的验证方式（仓库维护者）：

```bash
helm lint deploy/helm/levee
helm template levee deploy/helm/levee                                    # 单机
helm template levee deploy/helm/levee --set mode=cluster \
  --set postgres.enabled=true --set ingress.enabled=true \
  --set auth.namedTokens[0].name=alice --set "auth.namedTokens[0].secret=x"
```

## 部署前必读（安全约束，来自 docs/security-audit.md 的生产准入清单）

1. **认证必须用命名令牌或 OIDC**——单令牌模式无法在审计里证明"谁做的"；
2. **gRPC 端口（9090）不要直接暴露**：网关限流只覆盖 REST；gRPC 端口置于
   内网/VPN/网络策略之后；
3. `LEVEE_MASTER_PASSWORD` 走 secret 注入，不落盘、不进镜像；
4. `levee doctor` 通过后再接入生产目标机；先在测试机群跑通一次完整
   计划 → 审批 → 执行 → 回滚演练。
