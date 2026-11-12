# 信创裸机取证清单（麒麟 / 统信 × amd64）

状态：**待执行**（需先租到云服务器）。owner 判断（2026-11-12）：ARM 真机无门路；麒麟/统信只能租云服务器。
本清单把 `docs/platform-support-matrix.md` §1/§5 里「麒麟 / 统信 × amd64（裸机）」那一格，从「未验证」变成**可照做的步骤**——照做完就能拿到证据、就能改那一行。

> 已有的相邻证据（**不要混为一谈**）：麒麟 V10 x86_64 **WSL2 用户态**已跑通（2026-11-12，见矩阵 §1/§6）；本清单要补的是**裸机/整机**那一格。

## 0. 租机规格

| 项 | 要求 | 备注 |
|---|---|---|
| 架构 | **x86_64 (amd64)** | 租到 arm64 机型就按 arm64 记，且**不能**替代信创整机 |
| 规格 | 2 核 4G / 40G 磁盘 | 单机即可；变更目标可以用本机 sshd 自指 |
| OS | 麒麟 V10 SP3 (Sword) 服务器版 **或** 统信 UOS Server 20（1050/1060） | 官方镜像通常要账号下载；记录**完整版本号** |
| 网络 | 公网 SSH（22） | 若禁 root 密码登录，另备一台机器当变更目标 |

开完机先留底这些事实（后面要贴进矩阵证据列）：

```bash
cat /etc/os-release
uname -a
ldd --version | head -1
systemctl --version | head -1
```

## 1. 带过去的二进制

在开发机交叉编译（静态链接，不依赖目标机 glibc）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o levee ./cmd/levee
sha256sum levee
```

> 需要带 Web 前端时按 `Dockerfile` 的多阶段流程走（前端产物内嵌进二进制）；或直接取 `release.yml` 的 amd64 产物。

## 2. 落地步骤（每步留证）

1. **安装与自检**
   ```bash
   scp levee root@<host>:/usr/local/bin/levee
   ssh root@<host> 'chmod +x /usr/local/bin/levee && /usr/local/bin/levee version'
   ```
   然后 `levee system doctor`：**每项检查都要 OK**（配置 / 数据库 / 权限矩阵等，以实际输出为准），**把原始输出贴回来**。
2. **配置**：`server.data_dir` 指向一个可写目录（如 `/opt/levee/data`），`database.driver` 先用默认 sqlite；配置文件路径后续每条命令都用 `levee -c <cfg>` 指同一份。
3. **SSH 通道与凭据**：目标写进 inventory（`channel_type: ssh`），凭据用**结构化** JSON：
   ```bash
   levee -c <cfg> secret add --name ssh-lab --type ssh_password \
     --value '{"username":"root","password":"<pwd>"}'
   levee -c <cfg> target import --file inventory.yaml
   ```
   > **踩过的坑**：只填裸密码会让 `CredentialRef.Username` 为空，认证直接失败——username 必须在 JSON 里。
4. **起一条变更**：`template create` → `new` → `plan`（`levee plan <run> --targets <ip>`）。
5. **带引擎起服务**（回滚靠它）：
   ```bash
   levee -c <cfg> serve --engine-enabled \
     --engine-snapshot-dir /opt/levee/snapshots \
     --auth-token initiator=<tok> --auth-token drill-approver=<tok>
   ```
6. **跑完整条治理链**（REST，起服务后另开一个 shell）：
   ```bash
   ID=<run-id>
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/approve" -H "Authorization: Bearer <drill-tok>" -H 'Content-Type: application/json' -d '{"comment":"baremetal drill"}'
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/apply"   -H "Authorization: Bearer <drill-tok>" -H 'Content-Type: application/json' -d '{}'
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/rollback" -H "Authorization: Bearer <drill-tok>" -H 'Content-Type: application/json' -d '{}'
   curl -s  "http://127.0.0.1:8080/api/v1/audit/verify?changeId=$ID" -H "Authorization: Bearer <drill-tok>"
   ```
   期望：状态 `approved → completed → rolled_back`，目标文件 **v1 → v2 → v1**，`audit/verify` 返回 `valid=true`。
7. **（可选，覆盖"裸机安装"这一格）systemd 化**：`deploy/baremetal/install.sh` + `deploy/systemd/levee.service`，留 `systemctl status levee` 输出。

## 3. 证据要贴回哪里

- 矩阵 §1 对应行：`麒麟/统信 × amd64（裸机）` 由「未验证」改「已验证（裸机）」，证据列写清：OS 版本 / 内核 / glibc、`run-id`、`audit/verify` 结果、`doctor` 结论。
- 矩阵 §6 追加**边界**：租来的云主机**不等于客户现场**（机型、内核、安全基线可能不同）；若租到的是 arm64 机型，仍单列 arm64，不得替代信创整机。
- `docs/blocked-items.md` A-4（安装演练）同步：四 profile 演练完成到哪一格。

## 4. 风险与已知坑

- **glibc**：静态构建可绕开动态库依赖；但 `doctor` 仍会探测环境版本，如实记录报出的值。
- **安全基线**：麒麟/统信默认的强制访问控制可能拦 `install.sh` 与 sshd 自启；**保留错误原文**，别绕过成"看起来成功"。
- **SSH**：密码认证可能被禁；备好密钥方式。
- **快照目录是服务端的**：`--engine-snapshot-dir` 指 levee 进程所在机器的目录；目标机上的文件由通道读取后存到那里。
- **CLI `apply` 不装载插件通道、也不做快照回滚**：回滚验证必须走 `serve --engine-enabled` + REST（`levee apply` 路径只做一次性执行）。
- **别把租机当验收**：矩阵格子的措辞是「裸机已验证」，不是「已通过客户环境验收」。
