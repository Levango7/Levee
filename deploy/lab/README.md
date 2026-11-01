# LEVEE 实验室（lab）—— 4~5 台 VM 跑通试点演练

目的：在本机虚拟机小集群上把 [docs/scenario-cross-region-ops.md](../../docs/scenario-cross-region-ops.md) §5
的五项演练走一遍，产出第一份可以拿给客户看的证据（审计链校验输出、自包含 HTML 报告、
回滚账本截图）。

**实验室 ≠ 生产**：跑完实验室只说明控制流与治理语义通了；接入生产前仍须逐项过
[docs/deployment.md](../../docs/deployment.md) §15 交付检查单（命名令牌、gRPC 不外露、备份恢复演练等）。

**本文是安装与运维手册**；「每个场景证明什么能力、怎么在客户面前演、验收看哪条证据」
在 [DEMOS.md](DEMOS.md)（演示库索引）。两者不重复：本文讲怎么把实验室搭起来，
DEMOS.md 讲怎么演。

## 0. 拓扑与前置

| 项 | 建议 |
| --- | --- |
| VM 数量 | 4~5 台 Linux x86_64（2C4G 足够）：1 台"核心" + 2~4 台"区域" |
| 网络隔离 | 每台 VM 一块 host-only / 内部网卡模拟区域隔离；你的工作站可 SSH 到全部 |
| 系统 | 任意带 systemd 的发行版（Debian/Ubuntu/Rocky 均可） |
| 可选 | 其中 2 台装 MySQL 8 并配好主从（演练 1 用；`role=replica` 的两台） |
| 可选 | 再加 2 台装 nginx 与 `lab-app` 单元（演练 6/7 用；`role=web` 的两台） |
| 本机 | `make build` 出二进制（或 release 归档解出 `levee`） |

IP 约定（`inventory.example.yaml` 里的示例值，按实际改）：
`192.168.56.10` 核心 · `.11/.12` DC-A · `.13/.14` DC-B；`.12/.13` 是测试 MySQL 副本；
`.15/.16` 是 web 前端层（可选，只服务演练 6/7）。

**执行期还要两样**（缺了会跑到一半才失败）：

- `levee serve --engine-enabled`——不接执行引擎时 `apply` 只改状态、不碰目标机。
- `levee serve --engine-enabled --engine-snapshot-dir <目录>`——快照补偿是
  **fail-closed** 的：声明了 `strategy: snapshot` 的步骤在没有这个目录时于执行期被直接拒绝
  （`internal/wiring/run.go`）。


## 1. 一键安装（幂等）

在工作站（Git Bash / WSL，需 `ssh`/`scp`/`openssl`）：

```sh
make build   # 或解开 release 归档
LEVEE_BIN=./levee ./deploy/lab/bootstrap.sh \
    root@192.168.56.10 root@192.168.56.11 root@192.168.56.12 \
    root@192.168.56.13 root@192.168.56.14
```

脚本内部调用 `deploy/baremetal/install.sh`（与生产同一套语义：建系统用户、装二进制、
渲染加固 systemd 单元、0600 凭据文件），然后生成**命名令牌**（`--auth-token admin=…`）
与引擎主密码并启动。令牌只在首次生成时打印一次，**立即保存**——审计里要靠它证明"谁做的"。

逐台验收：

```sh
curl -sf http://192.168.56.10:8080/healthz
ssh root@192.168.56.10 /usr/local/bin/levee doctor
```

重复执行 bootstrap 是安全的（更新二进制、不覆盖既有配置）。

## 2. 清单导入与体检

把 `inventory.example.yaml` 的地址改成实际值，在核心机上：

```sh
levee -c /etc/levee/config.yaml target import --file inventory.example.yaml
levee -c /etc/levee/config.yaml target list
levee -c /etc/levee/config.yaml doctor
```

导入幂等（按 address upsert）；labels 是演练工作流查询选择的依据。

**清单里没有单独的 hostname 字段**——导入时主机名就是地址，所以后面
`levee plan --targets` 收的也是地址（`192.168.56.12`）。工作流里的
`target.query` 目前**不参与选目标**（它只是给人看的选择说明）；计划期真正切批的是
`--targets` 给出的顺序，`min_count`/`max_count` 用来卡这个集合的大小
（`internal/plan/targetcaps.go`）。

## 3. 凭据（演练 1 前置）

**要存的是 SSH 凭据，不是 MySQL 凭据**：mysql 模块是在**目标机上**跑 `mysql` 客户端，
经由 ssh 通道下发（`internal/executor/modules/mysql/mysql.go`）。它用的是目标机自己的
MySQL 账号与复制账号 `rpl_user` 配置，不从 LEVEE 取 MySQL 密码——`levee secret add`
的类型只有 `ssh_key` / `ssh_password` / `winrm_password` / `api_token`。

实验室的 VM 用密码登录时：

```sh
levee -c /etc/levee/config.yaml secret add \
  --name lab-ssh --type ssh_password --value '<VM 的 root 密码>'
# 把返回的引用名填进 inventory 的 credential_ref（.12/.13 两台），重新 import
levee -c /etc/levee/config.yaml target import --file inventory.example.yaml
```

用密钥登录就不用存凭据（ssh agent / 默认密钥）。

## 4. 五项演练（对照 scenario §5 验收表）

所有工作流先本地编译再看执行——`deploy/lab/workflows/` 下的文件都被 CI 强制编译
（`TestShippedLabWorkflowsCompile`），你 clone 下来的就是能编过的现行方言。
另外还有 7 条针对已发布资产的守卫（取值层、input 双向、补偿键、可执行集、命令策略、
索引对齐、审批人），见 `internal/dsl/shipped_assets_guard_test.go`。

### 4.0 通用流程：编译 → 注册 → 建 run → 计划 → 审批 → 执行

`levee compile` **只做检查，不建 run**；参数也不在 `plan` 上传——`levee plan`
没有 `--params` 旗标，参数在 `levee new --params` 传。一次演练的完整命令序列：

```sh
# ① 编译检查（不建 run）
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-replica-switch.yaml

# ② 注册成模板。参数名必须与工作流里的 input: 一致。
levee -c /etc/levee/config.yaml template create \
  --name lab-replica-switch \
  --content "$(cat deploy/lab/workflows/lab-replica-switch.yaml)" \
  --params '[{"name":"new_primary","type":"string","required":true}]'

# ③ 建 run（参数在这里传）→ 打印 run-id
levee -c /etc/levee/config.yaml new lab-replica-switch \
  --params new_primary=192.168.56.12:3306

# ④ 看计划。--targets 必填（收清单里的主机名，即地址）。
#    --dry-run 只看不落盘；真跑要去掉它。
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.13 --dry-run

# ⑤ 审批。高危档要 2 个不同身份、且发起人不能投（用 --as 换身份）。
#    注意：高危档工作流必须声明 approval.approvers，否则计划期就被拒
#    （实测 "approval under-provisioned: ... declares 0 votable ([])"）。
levee --as alice approve <run-id> --comment "lab drill"
levee --as bob   approve <run-id> --comment "lab drill"

# ⑥ 执行（--engine-enabled 才会真的动目标机）
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled

# ⑦ 取证
levee -c /etc/levee/config.yaml audit verify <run-id>
levee -c /etc/levee/config.yaml audit report --output lab-audit.html
```

> **窗口是计划期的硬门**：演练 1 的窗口是 `01:00–05:00 Asia/Shanghai`，窗外
> `plan` 直接被拒（不进审批）。演练 6/7/8 用的是覆盖全天的窗口，现场随时能跑。

### 4.1 演练 1 · 副本切换（验收：confirm 门、审批流、切换后 verify、审计链）

```sh
# 反向验证（各做一次，都要被拒）：
#   a) 删掉 allow_irreversible 段 → compile 被 LE082 拒（V14 编译期门禁）
#   b) 把 confirm: "yes" 改成别的 → 运行期被 confirm 硬门拒
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-replica-switch.yaml
# 之后按 §4.0 的 ②~⑦ 走，参数 new_primary=192.168.56.12:3306，目标 .13
```

### 4.2 演练 2 · 分批重启 + 故意失败（验收：失败策略、回滚账本区分「未执行/已补偿」）

```sh
# 先在一台 role=app 的目标上制造失败：把单元 mask 掉，restart 必然失败
ssh root@192.168.56.11 systemctl mask lab-app

levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-batch-restart.yaml
# 之后按 §4.0 的 ②~⑦ 走，目标 .11,.14
# apply 时观察 auto 回滚与账本
```

### 4.3 演练 3 · 窗口强制（验收：窗外拒绝、窗内放行）

```sh
vi deploy/lab/workflows/lab-window-test.yaml   # window 改成 now+10min ~ +20min（UTC）
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-window-test.yaml
# 之后按 §4.0 的 ②~⑦ 走：到点前 plan 被窗外阻断拒绝，进入窗口后再 plan —— 通过
# 时钟/时区写错则 compile 期被 LE020/LE021 拒
```

### 4.4 演练 4 · 杀进程接管（验收：接管收敛到 interrupted、Retry 不重跑已完成步骤）

需要**集群形态**（单机 SQLite 没有 worker 可杀）。实验室内最简做法：核心机起 PostgreSQL，
两台 VM 以 `--cluster --pg-dsn … --node-role master/worker` 加入（参数模板见
`/etc/levee/levee.env` 注释与 `config.example.yaml` 的 cluster 节），然后：

```sh
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-cluster-takeover.yaml
# 之后按 §4.0 的 ②~⑦ 走；apply 起来后，在 hold 步骤执行期间 kill -9 执行节点
levee -c /etc/levee/config.yaml cluster status --pg-dsn "$PG_DSN"   # 看接管事件
levee -c /etc/levee/config.yaml retry <run-id>                      # 已完成步骤不重跑
```

### 4.5 演练 5 · 审计出证

```sh
levee -c /etc/levee/config.yaml audit report --output lab-audit.html
# 把 lab-audit.html 拷给一位"内审同事"离线打开：审批链、哈希链校验结论、回滚记录都在
```

### 4.6 演练 6/7/8 · 演示场景（nginx 灰度 / web 配置回滚 / 集群接管）

这三个是面向客户的演示场景，脚本、前置与验收证据见 [DEMOS.md](DEMOS.md) §2。

## 5. 收尾

把五项演练的证据（audit verify 输出、HTML 报告、账本截图）归档——这就是第一份
试点案例材料。下一步才是按 [docs/deployment.md](../../docs/deployment.md) §15
在真实前哨环境重复这套流程。

要给客户演示时，按 [DEMOS.md](DEMOS.md) 走：那里有四个场景的脚本（证明什么能力、
前置、怎么跑、验收看哪条证据）与一节**诚实边界**（哪些环节本仓库实测过、哪些没有）。

## 6. 没有 VM 的时候：本地容器靶场

[container-lab/](container-lab/) 给出用 Docker 容器顶掉实验室 VM 的完整配方
（目标机镜像、三条可跑的工作流、清单与凭据要点，以及实跑中踩到的操作事实）。

它证明的是**引擎的执行期路径**：SSH 认证、模块分发、门禁判定、批次切分、
失败回滚、快照采集与还原、证据落库——这些在真 VM 到位前就能拿到实测证据。

它**不**证明真 VM 的 systemd 服务语义、MySQL 主从切换与集群接管：那三件
仍然要按 §0 的实验室环境做。
