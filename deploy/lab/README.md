# LEVEE 实验室（lab）—— 4~5 台 VM 跑通试点演练

目的：在本机虚拟机小集群上把 [docs/scenario-cross-region-ops.md](../../docs/scenario-cross-region-ops.md) §5
的五项演练走一遍，产出第一份可以拿给客户看的证据（审计链校验输出、自包含 HTML 报告、
回滚账本截图）。

**实验室 ≠ 生产**：跑完实验室只说明控制流与治理语义通了；接入生产前仍须逐项过
[docs/deployment.md](../../docs/deployment.md) §15 交付检查单（命名令牌、gRPC 不外露、备份恢复演练等）。

## 0. 拓扑与前置

| 项 | 建议 |
| --- | --- |
| VM 数量 | 4~5 台 Linux x86_64（2C4G 足够）：1 台"核心" + 2~4 台"区域" |
| 网络隔离 | 每台 VM 一块 host-only / 内部网卡模拟区域隔离；你的工作站可 SSH 到全部 |
| 系统 | 任意带 systemd 的发行版（Debian/Ubuntu/Rocky 均可） |
| 可选 | 其中 2 台装 MySQL 8 并配好主从（演练 1 用；`role=replica` 的两台） |
| 本机 | `make build` 出二进制（或 release 归档解出 `levee`） |

IP 约定（`inventory.example.yaml` 里的示例值，按实际改）：
`192.168.56.10` 核心 · `.11/.12` DC-A · `.13/.14` DC-B；`.12/.13` 是测试 MySQL 副本。

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

## 3. 凭据（演练 1 前置）

`mysql.replica_switch` 走 mysql 通道，需要凭据：

```sh
levee -c /etc/levee/config.yaml secret add   # 按 --help 录入 MySQL 凭据
# 把返回的引用名填进 inventory 的 credential_ref（.12/.13 两台），重新 import
```

## 4. 五项演练（对照 scenario §5 验收表）

所有工作流先本地编译再看执行——`deploy/lab/workflows/` 三个文件都被 CI 强制编译
（`TestShippedLabWorkflowsCompile`），你 clone 下来的就是能编过的现行方言。

**演练 1 · 副本切换**（验收：confirm 门、审批流、切换后 verify、审计链）

```sh
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-replica-switch.yaml
# 反向验证（各做一次，都要被拒）：
#   a) 删掉 allow_irreversible 段 → compile 被 LE082 拒（V14 编译期门禁）
#   b) 把 confirm: "yes" 改成别的 → 运行期被 confirm 硬门拒
levee -c /etc/levee/config.yaml plan   <run-id>      # 传入 --new-primary 等 input
levee -c /etc/levee/config.yaml approve <run-id> --comment "lab drill"   # 高危需两人
levee -c /etc/levee/config.yaml apply   <run-id>
levee -c /etc/levee/config.yaml audit verify
```

**演练 2 · 分批重启 + 故意失败**（验收：失败策略、回滚账本区分「未执行/已补偿」）

```sh
# 先在一台 role=app 的目标上制造失败：systemctl stop lab-app（单元不存在即可）
levee -c /etc/levee/config.yaml plan   <run-id>
levee -c /etc/levee/config.yaml approve <run-id>
levee -c /etc/levee/config.yaml apply   <run-id>     # 观察 auto 回滚与账本
```

**演练 3 · 窗口强制**（验收：窗外拒绝、窗内放行）

```sh
vi deploy/lab/workflows/lab-window-test.yaml   # window 改成 now+10min ~ +20min（UTC）
levee -c /etc/levee/config.yaml plan <run-id>  # 到点前：被窗外阻断拒绝
# 进入窗口后再 plan —— 通过。时钟/时区写错则 compile 期被 LE020/LE021 拒
```

**演练 4 · 杀进程接管**（验收：接管收敛到 interrupted、Retry 不重跑已完成步骤）
需要**集群形态**（单机 SQLite 没有 worker 可杀）。实验室内最简做法：核心机起 PostgreSQL，
两台 VM 以 `--cluster --pg-dsn … --node-role master/worker` 加入（参数模板见
`/etc/levee/levee.env` 注释与 `config.example.yaml` 的 cluster 节），kill -9 worker 后观察
`levee cluster_status` 与 `levee retry <run-id>`。

**演练 5 · 审计出证**

```sh
levee -c /etc/levee/config.yaml audit report --output lab-audit.html
# 把 lab-audit.html 拷给一位"内审同事"离线打开：审批链、哈希链校验结论、回滚记录都在
```

## 5. 收尾

把五项演练的证据（audit verify 输出、HTML 报告、账本截图）归档——这就是第一份
试点案例材料。下一步才是按 [docs/deployment.md](../../docs/deployment.md) §15
在真实前哨环境重复这套流程。
