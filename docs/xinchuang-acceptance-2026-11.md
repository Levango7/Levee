# 信创阶段性验收报告（2026-11-13）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 阶段性验收（**非最终签收**） |
| 基线 | `origin/master` @ `5d2bd0c`（#365 合并后） |
| 撰写 | agent `levee-opencode` |
| 取证口径 | 每条结论附 ISO sha256、虚机 SSH 端口、变更 run-id 或仓库脚本输出；**不采信「看起来对」** |
| 适用范围 | 面向下一位接手本仓的 agent、仓库 owner、以及投标/客户对话中的「已验证什么、没验证什么」 |
| 复核 | `python scripts/verify_xinchuang_evidence.py [--skip-hash|--skip-vm]`（hash 约需数分钟） |

---

## 1. 一句话结论

**LEVEE 在「麒麟 / 统信」两族的 x86_64 服务器版上，以虚拟机口径完成了完整治理链取证（4/4 绿）；数据库只有金仓 V9 PG 模式可用（已验证）、达梦 DM8 明确不支持（已实测）；最终验收仍缺 §4 的 6 项条件，本文件是阶段性收尾，不是签收。**

---

## 2. 验收口径与证据分级

**环境基线**：宿主 = Windows 11 家庭版（build 26300，`HypervisorPresent=True`，无 Hyper-V 角色，AMD Ryzen 9 7945HX，31.2 GB RAM）；虚机 = QEMU 11.1.3 ＋ WHPX 硬件加速；每台 4 vCPU / 4G；ISO 原始文件默认在 `G:\levee-vm\`，**例外**：`uos-server-25-2510-amd64.iso` 于 2026-11-13 移交至 `H:\Downloads\doubaobrowser\`（与已取证副本 sha256/md5 一致，该校验脚本会两个目录都找）。

**分级定义**（本报告只引用 A 级）：

| 级别 | 含义 | 本仓实例 |
| --- | --- | --- |
| **A** | 完整治理链实跑：doctor → 变更 → 审批 → apply（含快照）→ 回滚 → `audit/verify` | 四个 OS 虚机、金仓 PG 模式 |
| **B** | 部分验证：静态链接 + doctor 跑通，无完整变更 | 麒麟 WSL2 用户态（部分环节） |
| **C** | 模拟/外推：QEMU 用户态、交叉编译的 arm64 容器 | arm64 镜像 |

**WSL2 与虚机的区别（必须记住）**：WSL2 是「用户态复用 Windows 内核」，没有真 systemd；虚机里内核、initrd、systemd、包管理、XFS/LVM 全部是真的——**强一档，但都不是物理裸机**。

---

## 3. OS 证据总表（全部 A 级）

| OS | ISO（sha256） | 内核 | systemd | glibc | doctor | 变更（审批→apply→回滚） | audit/verify | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 麒麟 V10 SP3（Halberd）| `Kylin-Server-V10-SP3-2403-Release-20240426-X86_64.iso`（4.37GB，`039681842f…`）| `4.19.90-89.11.v2401.ky10` | 243 | 2.28 | healthy | `run-c82b2dceaab5d8b7`：approved→completed(v1→v2)→rolled_back(v2→v1)，快照 `snap-552851cb…` | `valid=true`、entriesVerified=3 | SELinux **disabled**；`is-system-running`=degraded（kylin-activation-check 未激活 ＋ rngd 无硬件 RNG）；内核命令行 `audit=0` |
| 麒麟 V11（Swan25）| `Kylin-Server-V11-2503-Release-General-20250715-X86_64.iso`（4.26GB，`05569c9a…`）| `6.6.0-32.7.v2505.ky11` | 255 | 2.38 | healthy | `run-635fbe17401cddd1`，快照 `snap-ddb080e437…` | `valid=true`、entriesVerified=3、unsealed=0 | **必须 `-cpu host`**（glibc 2.38 要 x86-64-v2）；`running`（0 failed units）；`apparmor=0` |
| 统信 UOS Server 20（1050e）| `uniontechos-server-20-1050e-amd64.iso`（5.77GB，`614ae5a7…`）| `4.19.90-2201.4.0.0135.up1.uel20` | 243 | 2.28 | healthy | `run-11484ccfa0fc3574`，快照 `snap-8a70e85e60d…` | `valid=true`、entriesVerified=3、unsealed=0 | **必须用默认 qemu64**（`-cpu host` 会让 UEL20 内核 dracut 全 segfault）；sshd 预登录横幅，`scp` 必须 `-O`；degraded（仅 rngd）；内核命令行无 `audit=0` |
| 统信 UOS Server V25（2510）| `uos-server-25-2510-amd64.iso`（5.53GB，`825ec55f…`）| `6.6.0-25.02.2510.029.uos25` | 255 | 2.38 | healthy | `run-6c202ebeb2362b42`，快照 `snap-1663a524667…` | `valid=true`、entriesVerified=3、unsealed=0 | **必须 `-cpu host`**（与 UOS 20 相反）；anaconda 36（PowerShell `Out-File -Encoding utf8` 带 BOM 会被当未知命令退出）；`running`（0 failed units）；`apparmor=0`；sshd 无横幅 |

补充两格（同为已验证、口径较弱）：**麒麟 V10（Sword）× WSL2 用户态**（B 级边缘：doctor healthy、完整变更 + `audit/verify` valid、entriesVerified=7，但非独立 OS）；**arm64 容器**（C 级：buildx 构建 + 容器内 `version`/`doctor`/`/healthz` 通过，Docker Desktop QEMU 模拟，**非 ARM 真机**）。

复跑端口：V10 SP3＝`22222`、V11＝`22223`、UOS 20＝`22224`、UOS V25＝`22225`（宿主转发）。

---

## 4. 缺口与最终验收条件（缺什么，才能说「最终」）

1. **物理裸机 × amd64**（麒麟 + 统信）：虚机证据不含固件/驱动/真硬件故障域/等保基线差异。需要一台可委派的信创整机（或 IDC 租机，**按 5 天计 ~¥250–350**，下手前核对：x86_64 机型、目标地域有麒麟/统信镜像、公网 SSH）。步骤清单 `docs/xinchuang-baremetal-drill.md`，照抄即可。
2. **ARM 真机**（鲲鹏/飞腾）：owner 判定「没办法」。arm64 容器证据只覆盖构建+可启动，不能替代鲲鹏/飞腾实机。
3. **统信桌面专业版 V25（2500）**：官网 CDN 无公开直链（API `desktop` 段为空）；需联合统信账号下载后另开一台虚机取证。桌面 OS 对 LEVEE 的服务器变更治理定位加分不大，除非客户点名。
4. **达梦 DM8 完整支持**：当前是「不支持（已实测）」。要支持需单独立项：DM Go 驱动选型（素材已有 `jdbc-20260923.zip` / `dmpython` wheel，Go 驱动缺）→ SQL 方言层（`TIMESTAMPTZ`、`TEXT` 主键、`ON CONFLICT` 均不被 DM8 接受，`COMPATIBLE_MODE=7` 也无济于事）→ CLOB 主键替代方案 → 全量回归。
5. **License/激活形态**：麒麟的 `kylin-activation-check.service`（本实验未激活，即 degraded 的一部分）、UOS 的「免费使用授权 vs 正式授权」路径——均需真实采购/试用后验证，未在此覆盖。
6. **性能与稳定性**：本报告全是「单实例、单变更」。没有并发压测、长跑泄漏、大库（审计链到 10⁵ 条以上）基准；`docs/benchmark-report.md` 是另一条线，未覆盖信创组合。

任一项的状态由「缺口」变为「已补」时，回到对应矩阵行升级即可；**本文件在满足 1 之前不得引用为「裸机已验证」**。

---

## 5. 限定与对外口径（照引用）

- 对外只能说「**麒麟 V10 SP3 / V11、统信 UOS Server 20 / V25（2510）× amd64，虚拟机口径，完整治理链已验证**」；裸机一律说「未验证」。
- 「已在 WSL2 上验证」只对麒麟 V10（Sword）成立，且必须带「WSL2 用户态」限定。
- 麒麟 V11 与 UOS V25 必须 `-cpu host`；UOS 20 必须默认 qemu64；**不要混**。
- SELinux 在四台实验机上均为 **disabled**（镜像默认）；V11/UOS V25 内核命令行 `apparmor=0`、kylin 含 `audit=0`。
- `is-system-running`：V10 SP3=degraded（kylin-activation-check + rngd）、UOS 20=degraded（rngd）、**V11=running、UOS V25=running**。degraded ≠ 安装失败。
- 快照回滚已验证（每台都跑过 `rolled_back` 且 v2→v1 还原），不是「设计上有回滚」。

---

## 6. 复核方法

```powershell
# 四个 ISO 的 sha256 + 四台虚机的 SSH 可达性（hash 需要数分钟）
python scripts/verify_xinchuang_evidence.py --iso-dir G:\levee-vm
# 只洗 hash：python scripts/verify_xinchuang_evidence.py --skip-vm
# 只看虚拟机：python scripts/verify_xinchuang_evidence.py --skip-hash
```

复跑演练本体：每台 guest 里同一套三步（`drill-setup.sh` → `drill-lifecycle.sh`），详见 `docs/xinchuang-vm-drill.md` §3 与 §7–§9 的分册差异；guest 内 `levee` 静态构建见该文档；ISO 的 16 分段并发下载法见 §8。

---

## 7. 相关变更记录

| PR | 内容 | merge |
| --- | --- | --- |
| #352 | 信创 DB 取证：金仓 V9 PG 全绿（`internal/state` 61.7s、`internal/cluster` 11.8s、变更 `run-8eecbcd833193a74`、`audit/verify`）；达梦 DM8 实测不支持 | `0b99bcc` |
| #353 | 裸机取证清单 `docs/xinchuang-baremetal-drill.md`（仍缺场地，未执行） | `ae7386d` |
| #354 | release.yml 多架构出包是发布时能力，不进 PR 门 | `19ab577` |
| #355 | k8spolicy S1 策略集 11→19 条（凭据与运行属性族） | `0c6875b` |
| #358 | 麒麟 V10 SP3 虚机取证 | `687df30` |
| #361 | 麒麟 V11（2503）虚机取证 | `63ff87b` |
| #362 | 统信 UOS Server 20（1050e）虚机取证 | `d9798c5` |
| #365 | 统信 UOS Server V25（2510）虚机取证 | `5d2bd0c` |

板面：`F:\Nexus\agent-board\levee\opencode-xinchuang-vm-os-drill.md`（本会话取证记录）。
矩阵：`docs/platform-support-matrix.md` §1/§5/§6（活文档，逐格）。
