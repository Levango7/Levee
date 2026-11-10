# LEVEE 演示库（demo library）—— 场景索引

这份文档把「客户要看的四个场景」映射到「可以直接跑的工作流文件」。每个场景回答三件事：

1. **它证明什么能力**（不是「跑通了」，是「哪条治理语义被看见了」）
2. **跑之前要准备什么**（少一条就会跑到一半失败）
3. **验收看哪条证据**（屏幕上出现什么才算演成了）

与 [README.md](README.md) 的分工：README 是**安装与运维手册**（怎么把实验室搭起来），
本文是**演示脚本**（怎么在客户面前演、演完拿什么当证据）。两者不重复。

> **仓库守卫**：`internal/dsl/shipped_assets_guard_test.go` 的
> `TestLabDemoIndexMatchesLabWorkflows` 强制本文与 `deploy/lab/workflows/`
> 双向对齐——索引里漏掉一个工作流文件、或索引指向不存在的文件，CI 直接红。
> 索引与实物不会各走各的。

## 0. 前置条件

实验室怎么搭见 [README.md](README.md) §1–§3（bootstrap、清单导入、凭据）。演示额外要这几样：

| 前置 | 为什么 | 缺了会怎样 |
| --- | --- | --- |
| `levee serve --engine-enabled` | 执行引擎：没有它 `apply` 只改状态不改机器 | apply 报 `FailedPrecondition`（status-only 模式） |
| `levee serve --engine-enabled --engine-snapshot-dir <目录>` | 快照补偿的基线目录 | 声明了 `strategy: snapshot` 的步骤在执行期被直接拒绝（fail-closed，`internal/wiring/run.go`） |
| 清单里至少 2 台 `role=web` 的机器（`.15` / `.16`） | A2 金丝雀灰度要两台才切得出批 | `min_count: 2` 在计划期就拒（LE105） |
| 目标机上装好 `nginx`（A2）与 `lab-app` 单元（A3） | 场景的验证门禁要跑真命令 | 门禁红 → 回滚 → 演示变成"回滚演示" |
| MySQL 8 主从 + 目标机的 MySQL 账号（A1） | `mysql.replica_switch` 在**目标机上**跑 mysql 客户端（经 ssh 通道下发），用的是目标机自己的账号与复制账号 `rpl_user` | 预检步骤失败 |
| PostgreSQL + `--cluster` 集群形态（A4） | 接管只在集群模式发生 | 单机 SQLite 没有 worker 可杀 |
| 控制端本地的两份配置文件：`lab-site.conf`（A2）、`lab-app.conf`（A3） | `file.copy` 的 `src` 是**控制端**本地路径 | 读不到文件 |

A2/A3/A4 用的是覆盖全天的窗口（`00:00`–`23:59` UTC），现场随时能跑；
"窗外拒绝"由 B2 单独承担。

## 1. 三步运行法

每个场景都是这三步，只有参数不同。

```sh
# ① 注册模板：把工作流文件登记成模板，并声明参数。
#    参数名必须与工作流里的 input: 一致（模板层与语言层各声明一次同一个名字）。
levee -c /etc/levee/config.yaml template create \
  --name lab-nginx-canary \
  --content "$(cat deploy/lab/workflows/lab-nginx-canary.yaml)" \
  --params '[{"name":"site_config_src","type":"string","required":true}]'

# ② 建 run：参数在这里传。
levee -c /etc/levee/config.yaml new lab-nginx-canary \
  --params site_config_src=./lab-site.conf        # → 打印 run-id

# ③ 看计划：--targets 必填，且顺序决定批次切分（见下方"目标从哪来"）。
levee -c /etc/levee/config.yaml plan <run-id> \
  --targets 192.168.56.15,192.168.56.16 --dry-run
```

确认无误后去掉 `--dry-run` 真跑一遍，再审批、执行：

```sh
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.15,192.168.56.16
levee -c /etc/levee/config.yaml approve <run-id> --comment "lab drill"
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled
levee -c /etc/levee/config.yaml audit verify <run-id>
```

**目标从哪来**：`plan --targets` 收的是**清单里的主机名**（导入时主机名就是地址，
`inventory.example.yaml` 没有单独的 hostname 字段）。工作流里的 `target.query`
目前**不参与选目标**——它只是给人看的选择说明；计划期真正切批的是 `--targets`
给出的顺序，`min_count`/`max_count` 用来卡这个集合的大小
（`internal/plan/targetcaps.go`）。所以流程是：

```sh
levee -c /etc/levee/config.yaml target list     # 看清单与标签
# 人工挑出 env=lab AND svc=web 的两台，按你想要的顺序传给 --targets
```

**审批人数**：`standard` 档 1 人；`high` 档要 **2 个不同身份**且**发起人不能投**
（`internal/approval/levels.go`）。两个要点：

1. **工作流必须声明 `approval.approvers`**。不声明的话计划期就被拒——实测报错
   `approval under-provisioned: high-level change needs 2 independent approver(s)
   but the workflow declares 0 votable ([])`。本目录里 A1/A2 的工作流已经声明了
   `approvers: [alice, bob]`（换成你们实验室的两个人即可）。
2. **发起人不能是审批人之一**：发起人从可投票名单里被剔除，所以建 run 的身份要
   另换一个（下面的 `carol`）。

```sh
levee --as carol new lab-replica-switch --params new_primary=192.168.56.12:3306  # 发起人
levee --as alice approve <run-id> --comment "lab drill"
levee --as bob   approve <run-id> --comment "lab drill"   # 第二票到齐 → run 进入 approved
```

在 PowerShell 上读文件进参数用 `Get-Content -Raw`：

```powershell
$content = Get-Content deploy/lab/workflows/lab-nginx-canary.yaml -Raw
levee -c C:\levee\config.yaml template create --name lab-nginx-canary --content $content --params '[{"name":"site_config_src","type":"string","required":true}]'
```

## 2. 场景索引

| 场景 | 证明什么 | 工作流 | 前置 | 验收看哪条证据 |
| --- | --- | --- | --- | --- |
| A1 | MySQL 主从切换：不可逆动作要编译期白名单（LE082）+ 运行期 confirm + 两人审批 + 逐台切换后核复制状态 | deploy/lab/workflows/lab-replica-switch.yaml | 两台副本 + MySQL 凭据 | 计划里 `irreversible` 标记与批次=逐台；审计链里 `confirm=yes` 与两票批准 |
| A2 | nginx 灰度：**分批 + 批间门禁**；配置正确性由 `nginx -t` 判定而不是"reload 没报错"；改前快照、失败按快照还原 | deploy/lab/workflows/lab-nginx-canary.yaml | 两台 web + nginx + 快照目录 | `plan --dry-run` 输出两批（先 1 台后 2 台）；第二批被批间探针阻断时的门禁记录 |
| A3 | web 配置回滚：失败被门禁**观测到**（不是忽略）；回滚是**按改动前的快照还原**而不是重发旧包；回滚后再验证一次 | deploy/lab/workflows/lab-web-config-rollback.yaml | 一台 web + `lab-app` 单元 + 快照目录 | 回滚账本里这一步是「已补偿」；回滚后复核（`verify_after: true`）的通过记录 |
| A4 | 集群接管：执行节点中途死掉 / 失联，变更**收敛到 interrupted** 而不是永远卡 running；账本 exactly-once（鬼影写被围栏拦、恢复后每步恰一行）；目标机副作用物理 at-least-once 如实记录 | deploy/lab/workflows/lab-cluster-takeover.yaml | PostgreSQL + `--cluster` 集群形态 | 接管事件（REST `GET /system/takeover-events`）；run 状态 `interrupted`；围栏 trace `evidence_discarded`；恢复后每步恰一行 success |
| B1 | 分批重启 + 故意失败：回滚账本如实区分「未执行」与「已补偿」 | deploy/lab/workflows/lab-batch-restart.yaml | 两台 `role=app` + 一个被 mask 的 `lab-app` | 账本两列的差别；`on_failure: auto` 自动触发补偿 |
| B2 | 变更窗口强制：窗外 plan 被拒、窗内放行；时钟/时区写错在编译期就被拒 | deploy/lab/workflows/lab-window-test.yaml | 无（执行体只是 `uptime`，零副作用） | 两次 plan 的相反结果 |

### A1 · MySQL 主从切换

```sh
levee -c /etc/levee/config.yaml template create \
  --name lab-replica-switch \
  --content "$(cat deploy/lab/workflows/lab-replica-switch.yaml)" \
  --params '[{"name":"new_primary","type":"string","required":true}]'

levee --as carol new lab-replica-switch --params new_primary=192.168.56.12:3306
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.13
levee --as alice approve <run-id> --comment "lab drill"
levee --as bob   approve <run-id> --comment "lab drill"
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled
```

讲什么：

- **不可逆要先授权**：`mysql.replica_switch` 在工作流的 `allow_irreversible` 里。
  把那一行删掉再 `levee compile`，编译期就被 LE082 拒——白名单缺席等于什么都没授权。
- **运行期还要再确认一次**：`confirm: "yes"` 改成别的值，执行期被确认硬门拒。
- **值进计划哈希**：`new_primary` 是语言层 input，取值在**计划期**从参数袋解析，
  解析后的值进 plan hash。换一台主库就是另一个计划，需要重新审批
  （`docs/leveelang-spec.md` §2.1）。
- **逐台推进**：`one-per-target` 每批一台，每台切换后用 verify 门禁核对复制状态
  才继续。

### A2 · nginx 灰度

```sh
levee -c /etc/levee/config.yaml template create \
  --name lab-nginx-canary \
  --content "$(cat deploy/lab/workflows/lab-nginx-canary.yaml)" \
  --params '[{"name":"site_config_src","type":"string","required":true}]'

levee -c /etc/levee/config.yaml new lab-nginx-canary --params site_config_src=./lab-site.conf
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.15,192.168.56.16 --dry-run
# A2 是 high 档：两票 + 发起人另算（见 §1「审批人数」）
levee --as alice approve <run-id> --comment "lab drill"
levee --as bob   approve <run-id> --comment "lab drill"
```

讲什么：

- **灰度是分批 + 批间门禁**：第一批（金丝雀）跑完必须过 HTTP 探针门禁，
  不过就阻断第二批并触发回滚（`docs/gates.md` 的 post_batch 语义）。
- **配置正确性由语法判定**：`nginx -t` 是独立的一步，配置写坏了这一步就红，
  而不是等 nginx 起不来才发现。
- **改坏了能还原**：改配置前先快照该文件，失败时按快照还原（不是"重装一遍"）。
- **金丝雀是哪台**：由批次策略按 `--targets` 的顺序切出（`percent 50/100` →
  先 1 台后 2 台）。`by-tag` 批次策略已从词表移除（`docs/leveelang-spec.md` §593），
  清单里的 `tier` 标签只是给人看的标注。**以 `plan --dry-run` 输出的每批目标为准**。

### A3 · web 配置回滚

```sh
# 故意写坏一份配置（少一个分号），重启就会失败
cp lab-app.conf lab-app-broken.conf

levee -c /etc/levee/config.yaml template create \
  --name lab-web-config-rollback \
  --content "$(cat deploy/lab/workflows/lab-web-config-rollback.yaml)" \
  --params '[{"name":"app_config_src","type":"string","required":true}]'

levee -c /etc/levee/config.yaml new lab-web-config-rollback --params app_config_src=./lab-app-broken.conf
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.16
levee -c /etc/levee/config.yaml approve <run-id> --comment "lab drill"
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled
```

讲什么：

- **失败是被观测到的**：配置写坏 → 重启失败 → verify 门禁红 → 触发
  `on_failure: auto` 的补偿。
- **回滚是按快照还原**：快照在 apply 前采集，补偿挂在它撤销的那一步上
  （LE097：补偿必须归属到具体的前向步骤）。不是"再部署一次旧版本"。
- **回滚后再验证一次**（`verify_after: true`）：账本说的"已补偿"要能被复核，
  不靠记账自证。
- 换成正常的 `lab-app.conf` 再跑一遍，对比"没出事"的账本。

### A4 · 集群接管

```sh
# 集群形态：核心机（master）+ worker 节点，见 README §4
levee -c /etc/levee/config.yaml serve --engine-enabled \
  --cluster --pg-dsn "$PG_DSN" --node-id node-a --node-addr 192.168.56.10:9090 --node-role master
# worker 节点同理（--node-role worker）

levee -c /etc/levee/config.yaml template create \
  --name lab-cluster-takeover \
  --content "$(cat deploy/lab/workflows/lab-cluster-takeover.yaml)" \
  --params '[]'

levee -c /etc/levee/config.yaml new lab-cluster-takeover
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.10
levee -c /etc/levee/config.yaml approve <run-id> --comment "lab drill"
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled
# —— 在 hold 步骤执行期间，kill -9 执行节点 ——
levee -c /etc/levee/config.yaml cluster status --pg-dsn "$PG_DSN"
# 恢复必须人驱动；且 interrupted 不在 CLI retry 的词表里
# （isRetryableStatus 只收 failed/paused），走 REST：
curl -X POST -H "Authorization: Bearer $LEVEE_TOKEN" \
  "$LEVEE_BASE/changes/<run-id>/retry" \
  -d '{"replan": false, "targetHosts": ["192.168.56.10"]}'
```

讲什么：

- **不会永远卡 running**：leader 的接管循环把在途变更收敛到 `interrupted`
  终态，并留一条接管事件（`docs/deployment.md` 的"执行围栏与故障接管"；
  REST `GET /system/takeover-events` 可读，`levee cluster status` 的
  Active Assignments 是同一事实的状态面）。
- **接管收敛状态，不伪造证据**：步级账本只在 closure 结束一次性落盘，
  硬杀在途轮次是零行——「已完成步骤保持 completed、正在执行的记为
  unknown」只适用于 kill 恰好落在批次边界之后的情况；本工作流（单批
  两步）实测是被 kill 一轮账本零行。
- **恢复是物理 at-least-once + 账本 exactly-once**：`retry` 后每步恰一行
  success、无鬼影行；目标机副作用可能重跑（标记文件 mtime 推后），这是
  设计事实，如实记录而不是写成"没重跑"。
- **愈合后旧执行者的晚期落盘被围栏拦下**：租约 CAS 失败，落
  `evidence_discarded` trace（专线中断 / DB 重启两场景实测）。
- **恢复执行要人来驱动**：这是设计取舍——半执行的变更不该被另一台机器当作
  可续的账（`docs/design-cluster-failover.md` §7.5 Q1）。CLI `levee retry`
  只收 failed / paused，对 interrupted 是错指令；集群态恢复必须走
  `POST /changes/:id/retry`（`replan=false` + 显式 `targetHosts`）。
- 接管**不做远端补偿**（快照在死者本地盘），所以工作流声明 `on_failure: manual`。
- **恢复时延上界 = 目标机锁 TTL**：硬杀后目标机锁成孤儿（owner 是引擎临时
  执行 id，接管不碰它），TTL 内 retry 必被 `ErrLockHeld` 挡——这是防物理
  双跑的安全网（锁过期 ⇒ 旧执行无在途步骤），不提前强抢。

本机容器靶场的三场景变体（S1 硬杀 / S2 专线中断 / S3 DB 重启）已实跑，
跑法与机器断言见 `deploy/lab/container-lab/README.md` §7。

### B1 · 分批重启 + 故意失败

```sh
# 在其中一台目标上制造失败：把单元 mask 掉，restart 必然失败
ssh root@192.168.56.11 systemctl mask lab-app

levee -c /etc/levee/config.yaml new lab-batch-restart
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.11,192.168.56.14
levee -c /etc/levee/config.yaml approve <run-id> --comment "lab drill"
levee -c /etc/levee/config.yaml apply <run-id> --engine-enabled
```

讲什么：回滚账本如实区分「未执行」（NotExecuted）与「已补偿」（Compensated）；
`on_failure: auto` 触发补偿而不是把机器留在改坏的状态等人来救。

### B2 · 变更窗口强制

```sh
# 把窗口改成「当前时间 +10 分钟」到「+20 分钟」（UTC）
vi deploy/lab/workflows/lab-window-test.yaml
levee -c /etc/levee/config.yaml compile deploy/lab/workflows/lab-window-test.yaml
levee -c /etc/levee/config.yaml new lab-window-test
levee -c /etc/levee/config.yaml plan <run-id> --targets 192.168.56.10    # 到点前：被窗外阻断拒绝
# 进入窗口后再 plan —— 通过
```

讲什么：窗口是**计划期**的硬门（窗外 plan 直接被拒，不进审批）；时钟/时区写错
（如 `25:00`）在编译期就被 LE020/LE021 拒。

## 3. 证据怎么收

每个场景演完，收这几样，它们合起来就是第一份试点案例材料：

| 证据 | 命令 | 说明 |
| --- | --- | --- |
| 计划输出 | `levee plan <run-id> --targets ... --dry-run` | 批次切分、目标、风险等级——证明"分批"是真的 |
| 计划哈希 | run 详情里的 `plan_hash` | 换参数 → 换哈希 → 证明"审批绑定的是这一个计划" |
| 审计链校验 | `levee audit verify <run-id>` | 哈希链完整性的结论 |
| 可离线阅读的报告 | `levee audit report --output lab-audit.html` | 审批链、哈希链校验结论、回滚记录都在里面 |
| 回滚账本 | `levee logs <run-id>` / 报告里的补偿段 | 「未执行」与「已补偿」的差别 |
| 接管事件 | REST `GET /system/takeover-events`（`levee cluster status --pg-dsn ...` 的 Active Assignments 是同一事实的状态面） | A4 专用 |

把 `lab-audit.html` 拷给一位"内审同事"离线打开——他能自己读出审批链与回滚记录，
这是这份材料最值钱的地方。

## 4. 诚实边界

这一节写的是**本仓库交付时实际验证到哪一步**，不是话术：

- **执行期已在「本地容器靶场」实跑，真 VM 实验室仍未实跑**：
  靶场见 `deploy/lab/container-lab/`（3 台 alpine+sshd+nginx 容器顶 VM）。
  已实测：① 成功路径 8 个步骤（`svc.start` / `file.copy` / `shell.exec` /
  `svc.reload` × 2 台）全部 success，输出是目标机真实回显；② `percent 50/100`
  切出 2 批各 1 台；③ 步骤级 cmd 门禁与批间 HTTP 探针门禁均通过；
  ④ 失败路径（退出码 1）触发自动回滚，`undo-action` 补偿步真把目标机 nginx
  停掉（目标机实测 `status: stopped`）；⑤ 快照路径（`levee serve
  --engine-snapshot-dir` + REST gateway）实测：正向写入 `listen 8091` →
  故意失败 → 快照还原 → 目标机文件回到 `listen 8090`，快照库落地
  `meta.json` / `paths.json` / 载荷三件套。
  ⑥ **两条失败路径的终态都是 `rolled_back_partial`，不是 `rolled_back`**：
  故意失败的那一步自己没声明补偿契约，它的副作用没被撤销 ⇒ 回滚没完全收敛，
  引擎于是不声称"干净回滚"（`internal/engine/closure.go:96-101`；引擎自己的回归
  测试 `internal/wiring/exec_run_test.go:351-357` 钉的就是这一条）。
  快照那条的还原步是 `success`、目标机文件确实回到 `listen 8090`，但 run 终态
  仍是 `rolled_back_partial`——**"文件回来了"不等于"回滚干净了"**，这两件事要分开说。
  **仍未实跑**：A1（MySQL 主从切换，无 MySQL 集群）、A4 的 VM 实验室形态
  （容器靶场的三场景变体已实跑，见 `deploy/lab/container-lab/README.md` §7；
  VM 形态仍无 PG 集群与第二节点）、以及真 VM 上的 systemd 服务语义（靶机走的是
  openrc 分支）。
  另外，计划层的老结论仍然成立：真二进制 + 真实 SQLite 下
  `template create → new --params → plan` 全链可行，换参数后 plan_hash 变化。
  **不要把"容器靶场跑通"说成"客户现场跑通"。**
- **A1/B1/B2 的执行路径沿用既有 lab 演练**，同样没有在本机实跑；
  它们的价值在于把既有手册的流程固化成可编译、可校验的文件。
- **命令策略是本笔新增的门禁**：已发布工作流里原先有 7 处 `shell.exec` 命令
  含 `&&`、引号或重定向——它们**编译期一律放行**（编译层不校验命令体），
  但运行期会被 shell 模块的白名单直接拒（`internal/executor/modules/shell/shell.go`）。
  这些命令已改为「拆成多步」或「改用 `shell.script`」，并由
  `TestShippedWorkflowCommandsMatchRuntimePolicy` 守住。
- **审批人声明同样是本笔修掉的**：高危档工作流若不声明 `approval.approvers`，
  计划期就被拒（实测 `approval under-provisioned`）。已发布工作流里原先有 5 处
  命中，已补声明并由 `TestShippedWorkflowsDeclareApproversForHighTier` 守住。
- **`target.query` 不参与选目标**：见 §1"目标从哪来"。工作流里写查询串是给人看的，
  真正生效的是 `--targets`。这是当前实现的边界，不是本文的简化。
- **A4 需要集群形态**：单机 SQLite 没有 worker 可杀，接管循环也不启动。

### 4.1 实跑才知道的操作事实

这五条是在容器靶场实跑时撞出来的，每条都有本机实测支撑，
写在这里是为了让下一个人不用再踩一遍（细节与复现步骤见
`deploy/lab/container-lab/README.md`）：

- **CLI 本地路径不读 `channel.ssh.*` 配置**：`ssh.SetDefaultConfig(...)` 全仓
  只有 `cmd/levee/cmd_serve.go:954` 一处调用，所以 `levee plan` /
  `levee apply --engine-enabled` 一律按包默认值走——**严格主机密钥校验**、
  读 `~/.ssh/known_hosts`。配置里写 `strict_host_check: false` 对本地路径
  **无效**。要在本地连没登记过的机器，先把主机密钥写进 `~/.ssh/known_hosts`
  （非 22 端口写 `[host]:port` 形式）。
- **快照补偿在 CLI 本地 apply 上不可用**：`--engine-snapshot-dir` 只有
  `levee serve` 有。本地 apply 遇到声明 `rollback.strategy: snapshot` 的计划
  会在动任何目标之前 fail-closed 拒绝：
  `declares snapshot rollback but no snapshot store is configured
  (--engine-snapshot-dir)`。要实跑快照补偿，走 serve。
- **`file.copy` 的 `src` 不能是绝对路径**：文件模块只读**控制端工作目录内**
  的源文件（防"任意文件读"），绝对路径与 `../` 逃逸都被拒；要读别处，把该
  目录加进 `LEVEE_FILE_MODULE_EXTRA_DIRS`。相对 `src` 是相对**执行 levee
  那个进程的工作目录**，不是相对配置文件。
- **别用 `pgrep` 做存活探测**：Alpine 的 busybox 把 `pgrep` 实现成了
  `pkill` 的别名，`pgrep nginx` 是"按名字杀进程"（实测把执行它的 shell 一起
  杀了）。用 `service nginx status`（systemd 与 openrc 语义一致）。
- **`levee target check` 在 CLI 本地模式下探不了**：本地路径不装通道工厂，
  恒返回 `no channel available (neither ch nor factory configured)`。
  连通性探测要在 serve 下做，或直接看 apply 的步骤证据。
