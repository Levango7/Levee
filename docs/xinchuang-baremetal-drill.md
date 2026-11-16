# 信创裸机取证清单（麒麟 / 统信 × amd64）

状态：**清单已脚本化；脚本本身已在 Linux amd64 自指口径实跑通过（2026-11-16）；信创裸机仍未执行**（需先租到机器）。
owner 判断（2026-11-12）：ARM 真机无门路；麒麟/统信只能租机器。
本清单把 `docs/platform-support-matrix.md` §1/§5 里「麒麟 / 统信 × amd64（裸机）」那一格，从「未验证」变成**可照做的步骤**——照做完就能拿到证据、就能改那一行。

> **先跑脚本，再读本文**：`scripts/xinchuang-baremetal-drill.sh` 把本文的 7 步做成一次命令，
> 并把每一步的原始输出落盘（不是让人手贴）。本文保留为「脚本为什么这么写」的依据与
> 「脚本没覆盖的部分」的清单。
>
> **已有的相邻证据（不要混为一谈）**：麒麟 V10 x86_64 **WSL2 用户态**已跑通（2026-11-12，见矩阵 §1/§6）；本清单要补的是**裸机/整机**那一格。脚本的自指演练实跑通过（Linux x86_64）证明的是**脚本与契约正确**，不是**信创裸机已验证**。

## 0. 一键驱动（首选）

```bash
# 在目标机上执行（levee 已在 PATH）；默认 key 认证、目标机自指
./scripts/xinchuang-baremetal-drill.sh --user root --addr 127.0.0.1

# 常用覆盖
LEVEE_DRILL_OUTDIR=/tmp/drill LEVEE_DRILL_USER=root ./scripts/xinchuang-baremetal-drill.sh
./scripts/xinchuang-baremetal-drill.sh --dry-run          # 只跑到拿到 run id
./scripts/xinchuang-baremetal-drill.sh --ssh-auth password --password '<pwd>'
```

脚本自己完成：环境事实 → `ssh-keyscan` 准备 `known_hosts` → 生成演练钥匙并写进
`authorized_keys` → 生成整份 config → `system doctor` → `template create` /
`secret add` / `target import` / `new` → 起 `serve --engine-enabled` →
REST 走 `plan → approve → apply → rollback` → `audit/verify` → 打印一页纸摘要。

产物落在 outdir（默认 `/tmp/levee-baremetal-drill`）：`00-environment.txt`、
`01-doctor.txt`、`02-connectivity.txt`、`03-metadata.txt`、`04-file-v1.txt`、
`05-file-v2.txt`、`06-file-restored.txt`、`07-audit-verify.json`、`summary.txt`。

**判定看 `summary.txt` 的三行**（脚本不替人下结论）：

```
file_v1=v1
file_v2=v2
file_restored=v1
```

`v1 → v2 → v1` 三段都对上、且 `audit_verify` 的 `valid=true`，才说明这次演练**真的**
走完了治理链。任何一段停在 `v1`，都说明 apply/rollback 没发生——那是一次**空的取证**。

## 1. 租机规格（脚本之外的准备）

| 项 | 要求 | 备注 |
|---|---|---|
| 架构 | **x86_64 (amd64)** | 租到 arm64 机型就按 arm64 记，且**不能**替代信创整机 |
| 规格 | 2 核 4G / 40G 磁盘 | 单机即可；变更目标可以用本机 sshd 自指 |
| OS | 麒麟 V10 SP3 (Sword) 服务器版 **或者** 统信 UOS Server 20（1050/1060） | 官方镜像通常要账号下载；记录**完整版本号** |
| 网络 | SSH（22）可达 | 若禁 root 密码登录，脚本默认走 key，另备一台机器当变更目标亦可 |

把 levee 二进制带上（`scripts/xinchuang-baremetal-drill.sh` 会调 `levee`）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o levee ./cmd/levee
sha256sum levee   # 记进证据列
```

> 需要带 Web 前端时按 `Dockerfile` 的多阶段流程走（前端产物内嵌进二进制）；或直接取 `release.yml` 的 amd64 产物。

开完机先留底这些事实（脚本自动采进 `00-environment.txt`，此处是人工核对用）：

```bash
cat /etc/os-release; uname -a; ldd --version | head -1; systemctl --version | head -1
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
3. **SSH 通道与凭据**：目标写进 inventory（`channel_type: ssh` + **`credential_ref`**），凭据用**结构化** JSON
   （`internal/channel.CredentialRef` 的形状，key 与 password 两种都在里面）：
   ```bash
   # key（默认）：key_path 指向**私钥**，由 levee 进程读取
   levee -c <cfg> secret add --name ssh-drill --type ssh_key \
     --value '{"username":"root","key_path":"/abs/path/to/private-key"}'
   # password（目标机开了密码登录时）
   levee -c <cfg> secret add --name ssh-drill --type ssh_password \
     --value '{"username":"root","password":"<pwd>"}'
   levee -c <cfg> target import --file inventory.yaml   # 目标行里要带 credential_ref: ssh-drill
   ```
   > **踩过的坑**：只填裸密码会让 `CredentialRef.Username` 为空，认证直接失败——username 必须在 JSON 里。
   > 凭据库是加密的：`secret add` 与 serve 解析凭据都要 `LEVEE_MASTER_PASSWORD`，**不设置则第一步就失败**。
   > `channel.ssh.auth_method` 只接受 `key`/`password`（`internal/config` 的校验）；选 `key` 时 `key_path` 必填。
   > `strict_host_check` 默认 **true**，所以 `known_hosts` 要先用 `ssh-keyscan` 准备好，否则必然连不上。
4. **起一条变更**：`template create` → `new` → `plan`（`levee plan <run> --targets <ip>`）。
5. **带引擎起服务**（回滚靠它）：
   ```bash
   levee -c <cfg> serve --engine-enabled \
     --engine-snapshot-dir /opt/levee/snapshots \
     --auth-token initiator=<tok> --auth-token drill-approver=<tok>
   ```
6. **跑完整条治理链**（REST，起服务后另开一个 shell）。**顺序不能变**——
   `plan → approve → apply → rollback`：
   ```bash
   ID=<run-id>
   # plan 的 body 必须有 JSON（空体会 400 "invalid body: EOF"），targetHosts 显式点名目标机
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/plan"    -H "Authorization: Bearer <init-tok>"   -H 'Content-Type: application/json' -d '{"targetHosts":["<ip>"]}'
   # 审批人取自**认证主体**（named token），body 里写 approver 反而会被拒（与主体不一致即 PermissionDenied）
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/approve" -H "Authorization: Bearer <approver-tok>" -H 'Content-Type: application/json' -d '{"comment":"baremetal drill"}'
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/apply"   -H "Authorization: Bearer <init-tok>"   -H 'Content-Type: application/json' -d '{}'
   curl -sX POST "http://127.0.0.1:8080/api/v1/changes/$ID/rollback" -H "Authorization: Bearer <init-tok>" -H 'Content-Type: application/json' -d '{}'
   curl -s  "http://127.0.0.1:8080/api/v1/audit/verify?changeId=$ID" -H "Authorization: Bearer <approver-tok>"
   ```
   期望：状态 `approved → completed → rolled_back`，目标文件 **v1 → v2 → v1**，`audit/verify` 返回 `valid=true`。
   > **2026-11-16 实跑时漏了 plan**，于是一连串错误各自指向别的方向：approve 回
   > `has no persisted plan to approve`、apply 回 `is in draft state`、rollback 回
   > `cannot ... current status draft`。三个错误合起来才是同一个缺失——这也是把它做成脚本的原因。
   > 就绪探针也要带 token：serve 开了 `--auth-token` 后 `/system/version` 要鉴权，
   > 不带 token 的探针会把「已经起来」读成「没起来」。
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
