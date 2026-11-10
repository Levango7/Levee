# LEVEE 本地容器靶场

`deploy/lab/DEMOS.md` 里的四个客户场景面向**真 VM 实验室**（4~7 台）。
本目录是它的**本地替身**：用 Docker 容器顶掉 VM，把「执行期路径」在没有
集群的机器上也实跑一遍。

它证明什么、不证明什么，见最后一节。

## 0. 前置

- Docker（镜像 `alpine:3.24` 能拉下来即可）
- levee 二进制：`go build -o levee.exe ./cmd/levee`
- 本机有 `~/.ssh/known_hosts` 的写权限（SSH 默认严格校验主机密钥，见 §4.1）

## 1. 起三台靶机

```bash
docker build -t levee-lab-ssh:local container-lab/
docker rm -f levee-lab-1 levee-lab-2 levee-lab-3
docker run -d --name levee-lab-1 -p 2201:22 -p 127.0.0.11:80:80 levee-lab-ssh:local
docker run -d --name levee-lab-2 -p 2202:22 -p 127.0.0.12:80:80 levee-lab-ssh:local
docker run -d --name levee-lab-3 -p 2203:22                 levee-lab-ssh:local
```

三台用**不同的 loopback 地址**（127.0.0.11/.12/.13）：LEVEE 的目标按
hostname 唯一索引（`internal/wiring/exec.go` 的 `byHost` 映射），同一个
hostname 出现两次只有第一台会被选中。

端口 80 也按 IP 绑定，这样批间 HTTP 探针门禁能按
`http://{target}/nginx-status` 逐台打到各自的 nginx。

## 2. 录主机密钥

容器每次重建都会换主机密钥，而 SSH 默认严格校验：

```bash
for c in 1 2 3; do
  docker exec levee-lab-$c cat /etc/ssh/ssh_host_ed25519_key.pub
done
```

把三行按 `[127.0.0.1N]:PORT ssh-ed25519 AAAA...` 的格式写进
`~/.ssh/known_hosts`（非 22 端口要带方括号）。

> `ssh-keyscan` 在老版本 Windows 上握手不了新 KEX
> （`choose_kex: unsupported KEX method sntrup761x25519-sha512@openssh.com`），
> 直接从容器里取公钥更省事。

## 3. 配 levee

本目录自带跑起来要用的全部文件（**除二进制外**）：

| 文件 | 作用 |
| --- | --- |
| `run/config.template.yaml` | 配置模板。`{{LAB_DIR}}` 是占位符 |
| `run/config.yaml` | **生成物**（不入库）：首次运行时由 `labdrive.py` 从模板生成，占位符换成本目录的绝对路径 |
| `run/inventory.yaml` | 目标清单：三台容器都用 `credential_ref: ssh-lab` |
| `run/credential-value.json` | 靶场凭据值（一次性口令，与 `Dockerfile` 里的 `root:rootpass` 相同） |
| `labdrive.py` | 驱动脚本，见 §3.1 |

`run/config.yaml` 关键三处：

```yaml
server:
  data_dir: "<靶场目录>/run/data"
database:
  driver: sqlite
  path: "<靶场目录>/run/data/levee.db"
channel:
  ssh:
    auth_method: password
    strict_host_check: false   # 注意：本地路径读不到这一项，见 §4.1
```

### 3.1 为什么有 `labdrive.py`

PowerShell 上有两个坑，两条都足以让一次"看起来能跑"的复现变成玄学：

1. **多行参数被按行拆散**——`levee template create --content <整份 YAML>`
   在 PowerShell 里会被拆成一个参数一行，原生程序收到的是一堆碎片；
2. **内嵌双引号被剥掉**——`--value '{"username":"root","password":"rootpass"}'`
   到程序手里变成 `{username:root,password:rootpass}`，**不报错**；解析回退后
   整段被当成口令，最终表现成一次莫名其妙的认证失败。

`labdrive.py` 用 Python 的 `subprocess.run([...])` 列表形式传参（Windows 下走
`list2cmdline`，引号与换行原样送达），并固定三件事：工作目录 = 靶场根目录
（`file.copy` 的相对 `src` 相对的就是它）、`LEVEE_FILE_MODULE_EXTRA_DIRS` =
靶场根目录、`LEVEE_MASTER_PASSWORD` = 本地靶场口令。

## 4. 实跑时踩到的真事实

这几条都是**实测**出来的，写在这里省得下一个人再踩一遍。

### 4.1 CLI 本地路径不读 `channel.ssh.*` 配置

`ssh.SetDefaultConfig(...)` 全仓只有一处调用：`cmd/levee/cmd_serve.go:954`。
也就是说 `levee plan` / `levee apply --engine-enabled` 这条本地路径**不会**
把配置里的 `strict_host_check` / `known_hosts` / `become_method` 接到 SSH
传输层上，一律按包默认值走：**严格主机密钥校验 + `~/.ssh/known_hosts`**。
配置里写 `strict_host_check: false` 对本地路径无效。

### 4.2 快照补偿在 CLI 本地 apply 上不可用

`--engine-snapshot-dir` 只有 `levee serve` 有。本地 apply 遇到声明
`rollback.strategy: snapshot` 的计划会**在动任何目标之前**直接拒绝：

```
engine: wiring: plan "plan-xxx" declares snapshot rollback but no snapshot
store is configured (--engine-snapshot-dir); refusing to execute
(fail-closed, D-2 v2)
```

要实跑快照补偿，走 serve：

```bash
levee serve --insecure --engine-enabled --engine-snapshot-dir ./snapshots \
  --addr 127.0.0.1:9099 --http-addr 127.0.0.1:8088
curl -X POST http://127.0.0.1:8088/api/v1/changes/<run-id>/apply \
  -H 'content-type: application/json' -d '{"autoApprove":true}'
```

（`serve` 没有 `--token` 会拒绝启动；本地实验用 `--insecure`。）

### 4.3 `file.copy` 的 src 不能是绝对路径

文件模块只允许读**控制端工作目录内**的源文件（防"任意文件读"）：
绝对路径被拒、`../` 逃逸被拒；要读别处就把它所在目录加进
`LEVEE_FILE_MODULE_EXTRA_DIRS`（`os.PathListSeparator` 分隔）。

```
file.copy: src "F:/.../lab-site.conf" must not be an absolute path: the file
module only reads inside the control-node working directory (or
LEVEE_FILE_MODULE_EXTRA_DIRS); ...
```

相对 src 是相对**执行 levee 那个进程的工作目录**，不是相对配置文件。

### 4.4 别用 `pgrep` 做存活探测

Alpine 的 busybox 把 `pgrep` 实现成了 `pkill` 的别名：

```
$ pgrep nginx
Usage: pkill [-l|-SIGNAL] [-xfvnoe] ...
```

`pgrep nginx` 会**按名字杀进程**（实测把执行它的 shell 一起杀了）。
用 `service nginx status`（systemd 与 openrc 上语义一致：active 时退出码 0）。

### 4.5 凭据值必须是 JSON，且回退路径会把整段当口令

`internal/credential` 的解析器先按 JSON 解 `CredentialRef`；解不出就把
**整个明文当密码**。所以一个被 PowerShell 吃掉双引号的 JSON 不会报
"格式错误"，而是变成一次静默的认证失败。

### 4.6 `levee target check` 在本地模式下探不了

本地路径不装通道工厂，`target check` 恒返回
`no channel available (neither ch nor factory configured)`。
连通性探测要在 serve 下做，或者直接看 apply 的步骤证据。

### 4.7 复跑时最容易误判成"跑歪了"的两处

- **`file.copy` 可能显示 `already in sync (sha256=...)`，那不是没下发**：
  文件模块先比本地与目标机的 sha256，一致就跳过上传（幂等）。靶机上已经
  是同一份 `lab-site.conf` 时就会出现这句话——第一次跑（镜像预置的是
  `listen 8080`）会看到 `uploaded 132 bytes to '/etc/nginx/http.d/lab-site.conf'`，
  之后复跑就是 `already in sync`。想看真实上传，改载荷内容（或重建容器）。
- **每条 svc 步骤的 stderr 里都有 `mkdir: can't create directory
  '/sys/fs/cgroup/openrc.nginx': Read-only file system`**：容器里没有可写的
  cgroup，openrc 建不了服务 cgroup 就报这句。**退出码仍是 0**，判定不受影响
  ——别把它当成失败。

## 5. 本目录里跑什么

`workflows/` 下三个工作流分别对应三条路径：

| 文件 | 证明什么 | 实测终态 |
| --- | --- | --- |
| `lab-smoke.yaml` | 成功路径：svc / file / shell 三模块 + 步骤级门禁 + 批间 HTTP 探针门禁 + 分批 | `completed`（2 批各 1 台） |
| `lab-smoke-fail.yaml` | 失败路径：退出码非 0 → 自动回滚 → undo-action 补偿 | `rolled_back_partial`，目标机 nginx 被停掉 |
| `lab-smoke-snap.yaml` | 快照路径：serve --engine-snapshot-dir 下采集 + 还原 | `rolled_back_partial`，目标机文件回到改动前内容 |
| `lab-cluster-takeover.yaml` | 集群故障接管的执行体（write-marker + hold 120s，给 kill / 分区 / DB 重启制造窗口） | 不在本表跑——由 §7 的 `clusterdrive.py` 三场景驱动，实测终态见 §7 |

> **为什么失败路径是 `rolled_back_partial` 而不是 `rolled_back`**：故意失败的那一步
> （`deliberate-failure`）**自己没声明补偿契约**，它的副作用没被撤销 ⇒ 回滚没有完全收敛。
> 引擎于是判 `rolled_back_partial` 而不是 `rolled_back`——这正是 D-2 v2 的设计意图
> （`internal/engine/closure.go:96-101`：「声称干净回滚是证据不支持的说法」，
> 引擎自己的回归测试 `internal/wiring/exec_run_test.go:351-357` 钉的就是这一条）。
> 想要 `rolled_back`，得让**每一步**都有补偿声明。

### 5.1 命令序列

```bash
# 0. 二进制（本机实测用的是从仓库根目录 build 出来的 levee.exe）
go build -o levee.exe ./cmd/levee
cp levee.exe deploy/lab/container-lab/          # 或用 LEVEE_BIN 指向它

cd deploy/lab/container-lab

# 1. 凭据（值从文件读，不走命令行——见 §3.1）
python labdrive.py secret reset ssh-lab

# 2. 目标清单（幂等）
python labdrive.py target

# 3. 模板 → run → plan → apply
python labdrive.py template workflows/lab-smoke.yaml lab-smoke
python labdrive.py new lab-smoke                # 输出 RUN_ID=<id>
python labdrive.py plan <run-id> 127.0.0.11,127.0.0.12
python labdrive.py apply <run-id> --force
```

`--force` = 跳过审批检查（引擎路径下按 auto-approve 处理）——靶场没有真实审批人。
`--targets` 收的是**清单里的主机名**（就是地址），`min_count` 卡集合大小。
`target.query` 不参与选目标，写在工作流里只是给人看的说明。

失败路径与快照路径同理，换成 `lab-smoke-fail.yaml` / `lab-smoke-snap.yaml` 即可；
快照路径必须走 serve（见 §4.2）。

证据在本地库里：`steps` / `batches` 两张表逐条记录了每一步的动作、
退出码、stdout/stderr 与耗时；快照库目录里有 `meta.json` / `paths.json`
和目标文件的原始载荷。

两份站点配置是被下发的载荷：`lab-site.conf`（成功路径用，端口 8090）与
`lab-site-snap.conf`（快照路径用，端口 8091——**故意与前者不同**，
这样"还原成功"才和"什么都没发生"区分得开）。镜像里预置的是 `listen 8080`，
所以成功路径那一步是真的把文件改掉了。

## 6. 边界

本靶场证明的是**引擎的执行期路径**：SSH 认证、模块分发、门禁判定、
批次切分、失败回滚、快照采集与还原、证据落库。

它**不**证明：
- 真 VM 上的 systemd 服务语义（靶机走的是 openrc 的 sysvinit 分支）；
- MySQL 主从切换（没有 MySQL 集群）；
- 真实 nginx 灰度的流量效果（探针是固定 URL，没有真实负载）。

这些仍然要在 `DEMOS.md` 描述的实验室环境里做。

集群接管本身由 §7 证明（容器内 PG + master/worker 两个 serve 节点）；§7
也不证明真 PG 集群的副本提升——S3 是单 PG 进程级重启、数据在命名卷上，
这是容器靶场对「DB 切换」的诚实近似，不是副本切换。

## 7. 集群故障接管演练（clusterdrive.py）

§5 的三条 smoke 路径证明引擎的执行期路径；本节证明**集群态下的故障收敛**：
执行节点死掉 / 失联 / 数据库重启后，run 不卡 `running`，账本不出现鬼影行，
恢复由人显式驱动。

### 7.1 前置

```bash
# 宿主机二进制（CLI 侧操作）与节点二进制（容器内 serve）都从当前源码构建
go build -o levee.exe ./cmd/levee
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o levee-linux ./cmd/levee
```

`clusterdrive.py` 默认用本目录的 `levee.exe` / `levee-linux`（`LEVEE_BIN`
可覆盖宿主机 CLI）。镜像每次无条件重建：`Dockerfile.node` 只 COPY
`levee-linux`，二进制一更新旧镜像就是陈旧版本（围栏那类修只落在新二进制上，
容器不重建 = 修没生效）。

### 7.2 三个场景

| 场景 | 制造的事故 | 机器断言（全部通过才 PASS） |
| --- | --- | --- |
| `python clusterdrive.py s1` | `docker kill -s KILL` worker 容器（等价 OOM / 断电） | run 收敛 `interrupted`；接管事件可读且属本 run；在途轮次账本零行；死者无晚期写（标记文件 mtime 未变）；孤儿锁 owner 是引擎临时执行 id（≠ run_id）等 TTL 自然过期；REST retry 后 `completed`，每步恰一行 success（账本 exactly-once），标记文件 mtime 推后（物理 at-least-once） |
| `python clusterdrive.py s2` | `docker network disconnect` worker（进程活、与 PG / 目标真分区） | 分区期间 worker 进程活着（`ps` 为证，区别于 S1 的 kill -9）；run 收敛 `interrupted`；愈合后旧 worker 的晚期落盘被围栏拦下（trace `evidence_discarded` ≥ 1、steps 仍零行）；retry 后 `completed`，每步恰一行 |
| `python clusterdrive.py s3` | `docker stop/start` PG（命名卷上的进程级重启） | 短 outage（< 租约 TTL 30s）run 保持 `running` 无误判；长 outage（> TTL）收敛 `interrupted`；活 worker 醒来时租约已换主、晚期写被围栏拦下（`evidence_discarded` ≥ 1）；retry 后 `completed`，每步恰一行 |

驱动打印 `VERDICT: PASS (n/n)` 才是一轮收口；任何一条断言不过即非零退出。

### 7.3 跑法与证据

```bash
python clusterdrive.py s1     # 或 s2 / s3；每个场景跑前 ensure_infra 清卷重建 PG + 两节点
python clusterdrive.py status # 当前集群面（nodes / stuck / load / assignments）
python clusterdrive.py down   # 拆控制面容器（目标与卷保留）
```

证据落 `run/cluster/evidence/s{1,2,3}/`（`.gitignore` 已覆盖 `run/`）：
每步快照（run JSON、cluster status、locks 表、trace 表、takeover-events
REST 响应、worker / master 日志）。

### 7.4 诚实契约（这套靶场不夸大）

- **硬杀 / 分区丢在途证据是设计事实**：步级行只在 closure 结束一次性落盘
  （`persistClosureResults` 单 `now` 回算），死者没机会写、也不该补写。
- **物理 at-least-once + 账本 exactly-once**：目标机副作用可能重跑（标记
  文件 mtime 推后），账本每步恰一行 success、无鬼影行。
- **围栏拦鬼影写**：愈合 / DB 恢复后旧执行者的晚期落盘被租约 CAS 拒绝，落
  `evidence_discarded` trace。
- **恢复必须人驱动**：`POST /changes/:id/retry`（`replan=false` + 显式
  `targetHosts`）。CLI `levee retry` 只收 failed / paused，对 interrupted
  是错指令。
- **恢复时延上界 = 目标机锁 TTL**（300s = exec_timeout）：硬杀后锁成孤儿
  （owner 是引擎临时执行 id，接管不碰它），TTL 内 retry 必被 `ErrLockHeld`
  挡——这是防物理双跑的安全网（锁过期 ⇒ 旧执行无在途步骤），不提前强抢。
