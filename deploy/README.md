# deploy —— LEVEE 交付物（裸金属 / 容器）

三条交付路径，按客户环境选一条：

| 路径 | 适用 | 入口 |
| --- | --- | --- |
| **裸金属 / VM（推荐主线）** | 物理机房、无 K8s、等保要求最小依赖 | `deploy/baremetal/install.sh` + `deploy/systemd/levee.service` + [deployment.md](../docs/deployment.md) |
| **Kubernetes（Helm）** | 有 K8s 的客户，一配置即部署 | `deploy/helm/levee/`（`helm install levee ./levee -f your-values.yaml`） |
| **容器（无 K8s）** | docker/podman 直接跑 | `docker run ghcr.io/levango7/levee:<版本> serve ...`（仓库根 Dockerfile 构建） |
| **实验室（试点/验证）** | 在 4~5 台 VM 小集群上跑通试点演练、产出案例证据 | [lab/](lab/README.md)（`bootstrap.sh` 一键安装 + 三条 CI 守护的演练工作流） |

## 镜像来源

- 每个 `v*` tag 的 Release 会构建并推送多架构镜像到 **GHCR**：
  `ghcr.io/levango7/levee:<版本>` 与 `:latest`（linux/amd64 + linux/arm64）。
- GHCR 包默认**私有**：客户现场拉取需要把镜像同步到客户镜像库，或配置
  `imagePullSecrets`（Helm 的 `imagePullSecrets` / 裸金属的 `docker login`）。
- 离线/气隙现场：`docker save` 导出后 `docker load`，或直接用二进制包
  （Release 的 `levee_<版本>_linux_<arch>.tar.gz`）。
  注意二进制内已 `go:embed` 前端产物，**不需要**单独发前端文件。

## 模板与真实旗标的一致性

Helm 模板里的每个参数都对应 `levee serve --help` 的真实旗标。**这条一致性现在有两道机器门禁，改模板不必再靠人核对**：

```bash
# 1) 每个旗标对着活的 cobra 树解析（含 systemd 单元的 ExecStart）
go test ./cmd/levee/ -run TestDeliveryArtifactsPassRegisteredServeFlags

# 2) 渲染 + lint + 打包：模板语法、.Values 路径、<no value> 落进 args
bash scripts/validate_delivery.sh
```

两者都在 CI 的 `test` 与 `delivery` job 里跑。为什么需要两道：第 1 道能发现"旗标名不存在"，但它只读模板文本，看不见模板渲染失败；第 2 道能发现渲染问题，但 helm 不知道 `levee` 有哪些旗标。

历史上这里只写了"改模板前手工核对 `levee serve --help`"，而 chart 带着一个不存在的 `--cluster-dispatch-worker-capacity` 发了版——每次 `helm install --set mode=cluster` 起来的 Pod 都以 "unknown flag" 退出。手册式核对在无人复述流程时等于没有核对。

同族但上面两道检查都覆盖不到的一层是**镜像引用本身**：旗标名与模板渲染都能过，而 `image.tag` 指向一个 registry 里不存在的 tag，`helm install` 照样起不来（ImagePullBackOff）。真实事故：`release.yml` 推的镜像 tag 就是 git tag 名（带 `v`），而 chart 里写的是不带 `v` 的版本号，v1.18.0 起如此——`values.image.tag` 与 `Chart.appVersion` 相等、git tag 也存在，所以旧的版本门禁一路判绿。现在 `scripts/check_release_versions.py` 规则②要求 `image.tag` **逐字**出现在 tag 集合里，渲染出的那串字符才是被校验的对象。**从旧 tag 部署时的自救**：`helm install … --set image.tag=vX.Y.Z`（X.Y.Z 是你实际要跑的那个发布，v1.19.0 的镜像是 `ghcr.io/levango7/levee:v1.19.0`）。

留作备查的手工动作（例如临时排查某个旗标是否存在）：

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
