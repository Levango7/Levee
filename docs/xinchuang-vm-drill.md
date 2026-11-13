# 信创虚机取证（麒麟 V10 SP3 × amd64）——已跑通的做法

状态：**已执行**（2026-11-12/13）。产出证据登记在 `docs/platform-support-matrix.md` §1/§5/§6。
物理裸机那一格仍缺，取证步骤见 `docs/xinchuang-baremetal-drill.md`。

## 0. 为什么先走虚机，以及它的口径

- 本机是 **Windows 11 家庭版**：没有 Hyper-V 角色（`Get-WindowsOptionalFeature` 需要管理员且该版本不提供 Hyper-V Manager），WSL2 只是用户态——**没有真 systemd**。
- 虚机给的是**真内核 + 真 initrd + 真 systemd + 真包管理 + 真文件系统（XFS/LVM）**，比 WSL2 强一档，代价是口径必须写清：**机器是虚拟的**。
- 成本 **¥0**：ISO 免费下载、QEMU 便携版免管理员安装。
- 所以矩阵里它是**新增一格**「麒麟 V10 SP3（Halberd）× amd64（虚拟机，非物理裸机）」，而不是把「裸机」那格改成已验证。

## 1. 准备（全部在本机完成，不需要管理员）

### 1.1 ISO：麒麟官方镜像中心

双源飞流下载中心 `sl.ygwid.cn` 的目录数据在它的 `js/main.js` 里（`kylinDownloadData`）；服务器版文件名形如
`Kylin-Server-V10-SP3-2403-Release-20240426-X86_64.iso`，直链形如
`https://iso.kylinos.cn/web_pungi/download/cdn/<token>/<文件名>`，同一份目录数据里带 **sha256**。

下载后**必须核对 sha256**（本次 `Kylin-Server-V10-SP3-2403-Release-20240426-X86_64.iso` = 4.37GB，
sha256 `039681842fc354789ed4471f9debdf0204a4e64607db59a9d8fecb37c4ed77a3`，核对通过）。

> 统信 UOS 的 ISO 不在这个站（需官网注册账号），所以统信那格仍是「未验证」。

### 1.2 QEMU for Windows：本机装不了的那条路，怎么绕过去

- 官方 Windows 安装包镜像（`qemu.weilnetz.de`）实测只有 **6.7 KB/s**（197MB 要 8 小时）——不可用。
- Windows 安装还要管理员权限装驱动——本轮不想引入 UAC 交互。
- **可行解**：MSYS2 便携版 ＋ TUNA 镜像装 UCRT64 版 QEMU：
  ```bash
  # msys2-base-*.sfx.exe 是自解压包，任意目录，免管理员
  # etc/pacman.d/mirrorlist.* 指向 TUNA（注意 mingw 系仓库要带 /mingw/）
  Server = https://mirrors.tuna.tsinghua.edu.cn/msys2/mingw/$repo/    # ucrt64 / mingw64 / clang64 …
  Server = https://mirrors.tuna.tsinghua.edu.cn/msys2/$repo/           # msys
  pacman -S --needed mingw-w64-ucrt-x86_64-qemu
  ```
  结果：QEMU 11.1.3，`qemu-system-x86_64 -accel help` 含 **whpx**（Windows Hypervisor Platform）。
- **宿主能力前提**：Windows 11 家庭版 + 已在跑 WSL2/VBS ⇒ WHPX 可用（本次 `-accel whpx` 生效，
  装完 4.4GB 的麒麟 ISO 全程约 **5 分钟**，虚拟化性能不是瓶颈）。

### 1.3 宿主侧 HTTP 服务（喂 kickstart 用）

QEMU user 模式网络的 `10.0.2.2` 就是**宿主 loopback**，所以 guest 能直接取宿主上的服务。
本轮用一个 30 行的 Go 静态服务绑 `127.0.0.1:8000` 伺服 `ks.cfg`（无需管理员、无需额外依赖）。

## 2. 无人值守安装（anaconda kickstart）

麒麟 V10 SP3 是 anaconda（RHEL 系）安装器，装法：

1. 从 ISO 取引导件：Windows 侧 `Mount-DiskImage` 挂载后直接拷 `/isolinux/vmlinuz` 与 `/isolinux/initrd.img`。
2. kickstart（`ks.cfg`）要点：
   - `rootpw --plaintext <口令>`、`network --bootproto=dhcp --device=link --onboot=on --activate`
   - `autopart` ＋ `clearpart --all --initlabel`（自动分区）
   - `%packages`：`@core`、`openssh-server`、`curl`、`wget`
   - `%post`：放开 `PermitRootLogin yes` / `PasswordAuthentication yes`（默认禁 root 口令登录，**不改这条就进不去**），
     `systemctl enable sshd`、`systemctl enable NetworkManager`、`systemctl set-default multi-user.target`
   - 末尾 `poweroff`（配 QEMU 的 `-no-reboot`，装完自动退出）
   - SELinux 不在 ks 里开（镜像默认 disabled，**照登**而不是硬开）
3. 启动参数（`console=ttyS0` 与 `inst.text` **必须有**，否则无显示设备时安装器卡在图形界面）：
   ```bash
   qemu-system-x86_64 -accel whpx -m 4096 -smp 4 \
     -cdrom <ISO> -drive file=<disk>.qcow2,if=virtio,format=qcow2,cache=writeback \
     -kernel <vmlinuz> -initrd <initrd.img> \
     -append "inst.ks=http://10.0.2.2:8000/ks.cfg console=ttyS0 inst.text" \
     -netdev "user,id=n0,hostfwd=tcp::22222-:22,hostfwd=tcp::18083-:18083" \
     -device e1000,netdev=n0 -nographic -no-reboot -rtc base=utc
   ```
   串口输出重定向到文件即安装进度日志。装完关机后，**再不带 `-kernel/-initrd` 起一次**，从磁盘启动。
   （`-cdrom` 与 `-kernel` 同时给时安装源仍是 cdrom，不需要 `inst.repo`。）

## 3. 在 guest 里跑 LEVEE（演练本体）

```bash
# 交叉编译（静态，不依赖目标机 glibc）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=dev+19ab577 -X main.commitHash=19ab577 -X main.buildTime=<ts>" \
  -o levee ./cmd/levee
```
把它 scp 进 guest，然后 guest 内四步（与 SQLite 版的 S3 演练同形，差别只是 OS）：

1. `levee -c <cfg> system doctor` → 期望 **healthy**
2. `levee -c <cfg> secret add --name ssh-lab --type ssh_password --value '{"username":"root","password":"…"}'`
   （**结构化 JSON 必须带 username**——裸口令会让 `CredentialRef.Username` 为空而认证失败）
3. `levee -c <cfg> target import --file inventory.yaml` → `template create` → `new` → `plan <run> --targets 127.0.0.1`
4. `levee -c <cfg> serve --engine-enabled --engine-snapshot-dir <dir> --auth-token initiator=… --auth-token drill-approver=…`
   然后 REST 走 `approve → apply → rollback`，最后 `GET /api/v1/audit/verify?changeId=<id>`。

宿主侧免交互口令登录（Windows OpenSSH）：
`SSH_ASKPASS=<脚本>` + `SSH_ASKPASS_REQUIRE=force` + `DISPLAY=dummy`，脚本内容 `@echo <口令>`。

## 4. 本次结果（照登，不修饰）

| 项 | 值 |
|---|---|
| OS | Kylin Linux Advanced Server **V10 (Halberd)**，x86_64（ISO 为 SP3 2403 构建） |
| 内核 | `4.19.90-89.11.v2401.ky10.x86_64`（内核命令行含 `audit=0`） |
| init | **systemd 243**（v243-67.p05.ky10）；`sshd` active |
| glibc | 2.28 |
| 文件系统 | XFS + LVM：`/boot` 1G、`klas-root` 35G（`/`）、`klas-swap` 4G |
| 资源 | 4 vCPU / 约 2.9G 可用内存（QEMU 给 4G） |
| SELinux | **disabled**（镜像默认） |
| 系统状态 | `systemctl is-system-running` = **degraded**：`kylin-activation-check.service`（企业授权未激活）、`rngd.service`（虚机无硬件 RNG） |
| levee | `dev+19ab577`，静态链接（`not a dynamic executable`），`system doctor` = **healthy**（config / database / permission_matrix 全 OK） |
| 变更 | `run-c82b2dceaab5d8b7`：`approved → completed`（`/tmp/levee-vm/state.txt` v1→v2）→ `rolled_back`（v2→v1） |
| 审计 | `GET /audit/verify` → `valid=true`、`entriesVerified=3`、`unsealed=0`；快照目录 `snap-552851cb0abaeef0` 落盘并按快照还原 |
| 存储 | SQLite（`/root/vm/data/levee.db`）；库内 `runs` 1 行（rolled_back）、`audit` 3 行 |

## 5. 踩坑清单（都实测过）

- **qemu.weilnetz.de 的 Windows 包不要用**：6.7KB/s，197MB 要 8 小时。走 MSYS2 + TUNA（22MB/s）。
- **TUNA 的 msys2 镜像路径**：mingw 系仓库要带 `/mingw/`（`msys2/mingw/ucrt64/`），写成本轮第一版的
  `msys2/$repo/` 会 404；`msys.db` 404 与 ucrt64 包无关，可忽略。
- **PowerShell 传参**：`Start-Process -ArgumentList` 传数组时，`-append "a b c"` 会被拆成三个参数，
  QEMU 报 `console=ttyS0: Could not open`——要用**整串参数**。
- **`scp` 的端口旗标是大写 `-P`**，`-p` 是保留时间；用错会把端口号当成本地文件名。
- **Windows OpenSSH 的免交互口令**：`SSH_ASKPASS` + `SSH_ASKPASS_REQUIRE=force` + `DISPLAY=dummy`，口令脚本回显即可。
- **degraded 不等于安装失败**：授权检查与硬件 RNG 两个单元失败是虚机环境的必然结果，照登即可。
- **镜像默认 SELinux disabled**：要开得在 ks 里显式写 `selinux --enforcing`，且要确认该镜像支持，否则安装器会拒绝。

## 6. 还没做的

- **统信 UOS**：**服务器版两格已取证**（第 8 节 20-1050e、第 9 节 V25-2510）。桌面 V25（2500）仍无公开直链（官网下载页需登录联合统信账号；CDN 目录数据里只有 20 系的文件名）。
- **物理裸机**：仍需租 IDC 机（步骤见 `docs/xinchuang-baremetal-drill.md`）。
- **arm64**：owner 已判定无门路，不做。

## 7. 2026-11-13 追加：麒麟 V11（2503）同口径复跑

结论先行：**V11 也能跑通，而且比 V10 还干净**——`systemctl is-system-running` = **running**（0 failed units）。整条链路（doctor healthy → 审批 → apply v1→v2 → rollback v2→v1 → `audit/verify valid=true` entriesVerified=3）与 V10 SP3 一致；变更 `run-635fbe17401cddd1`，快照 `snap-ddb080e437ef63ac`，ISO 为 `Kylin-Server-V11-2503-Release-General-20250715-X86_64.iso`（sha256 `05569c9ac23ded1f6768ccedc60a344e0c24cec5075fd54670e3e7a7a4428d7e`，与官方目录数据一致），guest 内核 `6.6.0-32.7.v2505.ky11.x86_64`、systemd **255**、glibc **2.38**、内核命令行含 `audit=0 apparmor=0`。

**V11 相对 V10 的实测差异（都踩过）**：

1. **必须 `-cpu host`**：V11 glibc 2.38 要求 **x86-64-v2**；QEMU 默认 `qemu64` 型号只暴露基础 x86-64 → 内核 init 直接 `Fatal glibc error: CPU does not support x86-64-v2` 然后 kernel panic。V10 的 4.19 内核 + glibc 2.28 无此要求，所以 V10 那次没加。
2. **新 anaconda 删除了 `install` 指令**：kickstart 第一行 `install` 直接报错 "The following problem occurred… `install` has been removed" 并终止安装。V11 的 ks 去掉该行即可（本仓 `ks/ks-v11.cfg`）；同时 **`@core` 不含 `wget`**（V10 无人值守靠 wget，V11 只留 `curl`）。
3. 资源与 V10 一致：4 vCPU / 4G，XFS+LVM（`klas-root` 35G / `klas-swap` 4G），宿主同一台 Windows 11。

口径不变：**这仍是虚拟机（QEMU+WHPX），不是裸机**；镜像默认 SELinux disabled；对外引用必须写「麒麟 V11 × amd64（虚拟机，非物理裸机）」。实验主机：SSH `22223`（V10 SP3 虚机占 `22222`）。

## 8. 2026-11-13 追加：统信 UOS Server 20（1050e）同口径复跑

结论先行：**统信服务器版也跑通了**——变更 `run-11484ccfa0fc3574`，`approved → completed`（v1→v2）→ `rolled_back`（v2→v1），`audit/verify` `valid=true`、`entriesVerified=3`、`unsealed=0`，快照 `snap-8a70e85e60d24b7c`；`system doctor` = **healthy**。guest 内核 `4.19.90-2201.4.0.0135.up1.uel20.x86_64`、systemd **243**（UEL20 build）、glibc **2.28**、XFS+LVM（`uos-root` 36G）、SELinux disabled、firewalld public zone 仅 ssh/mdns/dhcpv6-client；`is-system-running` = **degraded**（仅 `rngd.service`，虚机无硬件 RNG）。

**获取镜像的过程（绕开 CDN 限速，全程 hash 兜底）**

- 官网下载 API（`/api/v1/download/iso_info`）公布：`uniontechos-server-20-1050e-amd64.iso`，sha256 `614ae5a7cfaf4f7750a5e5b09a6bcc7d78ac99fbffd78a3eb593229d4a88c185`，大小 5.77 GB。
- 直链：`https://cdimage-download.chinauos.com/uniontechos-server-20-1050e-amd64.iso`。实测单流限速约 **0.7–0.9 MB/s**，下到 2.42 GB 后杀掉；华为云镜像（`mirrors.huaweicloud.com/uniontech/`）返回的只是 SPA 首页（HTTP 200 但 `Content-Type: text/html`，不是 ISO）。
- 做法：16 个并发分段 `curl -r s-e` 补齐剩余 3.34 GB（约 4 分钟），按序 `copy /b` 拼接，`Get-FileHash` 与官网 sha256 **完全一致**才启用。
- 桌面 V25（2500）**没有公开直链**（API 的 `desktop` 段为空数组，CDN 文件名探测全 404/SPA），需注册联合统信账号下载；本节只覆盖**服务器版**。

**安装的实测坑（都实测过）**

1. **`-cpu host` 会让 UEL20 内核全灭**：dracut 的 bash hooks（`dracut-dmsquash`、`dracut-livenet`、`dracut-rootfs`）全部 segfault（`error 14 in bash`，`ip=0`）。**换默认 qemu64 CPU，segfault = 0**，kickstart 一次读入、7 分钟装完。原因：4.19 UEL 内核不认识 7945HX 暴露出的全部新指令集/状态位。（这与 V11 相反——V11 必须 `-cpu host`。）
2. **anaconda 版本是 33（UEL20 包线），不是麒麟那套**：仍是 RHEL 系（`Packages/` + `repodata`，含 `@core` 组：minimal-environment → `core` ＋ `license-cui`）。**kickstart 直接复用麒麟那份**（省略已被移除的 `install` 关键字）。引导内核/initrd 在 `isolinux/vmlinuz` 与 `isolinux/initrd.img`。
3. **sshd 预登录横幅 + SFTP 静默失败**：sshd 打印 `UnionTech OS Server 20 1050e`；`scp`（OpenSSH≥9 默认走 SFTP）**静默失败、无错误码**，`-O`（旧 scp 协议）正常。Windows 免交互口令同样用 `SSH_ASKPASS` + `SSH_ASKPASS_REQUIRE=force`。
4. `%post` 要点同麒麟：UEL20 的 sshd 默认**禁止 root 口令登录**，必须同样 sed 放开 `PermitRootLogin` / `PasswordAuthentication`，否则进不去。

资源与前两台一致：4 vCPU / 4G，宿主 Windows 11，WHPX。口径不变：**这仍是虚拟机，不是裸机**；对外引用必须写「统信 UOS Server 20（1050e）× amd64（虚拟机，非物理裸机）」。SSH 端口 `22224`（V10 SP3 占 22222，V11 占 22223）。License 形态（免费使用授权 vs 正式授权）未在此验证。

## 9. 2026-11-13 追加：统信 UOS Server V25（2510）同口径复跑

结论先行：**统信最新服务器版 V25 也跑通了**——变更 `run-6c202ebeb2362b42`，`approved → completed`（v1→v2）→ `rolled_back`（v2→v1），`audit/verify` `valid=true`、`entriesVerified=3`、`unsealed=0`，快照 `snap-1663a5246678724b`；`system doctor` = **healthy**；`is-system-running` = **running**（0 failed units）。guest ＝ **UOS Server V25**（`VERSION_ID="25"`、`VERSION_CODENAME=qianlai`、`PLATFORM_ID=platform:uos25`），内核 `6.6.0-25.02.2510.029.uos25.x86_64`、systemd **255**、glibc **2.38**、XFS+LVM（`uos-root` 36G）、SELinux disabled、内核命令行含 `apparmor=0`、firewalld public zone 仅 ssh/mdns/dhcpv6-client、sshd 无预登录横幅。

**获取镜像**

- 官方服务器版下载页 `https://www.chinauos.com/resource/download-server` 列出的最新版 `uos-server-25-2510-amd64.iso`（5.53 GB），页面公布 **sha256 `825ec55f76166584b2bd923d41f9938e18ed380272e7315a57b587e2d89b17b2`**（md5 `ea3e28e9af214e7c8ea86b61edf9f589`）。该文件不在 `iso_info` API 的公开直链列表里，且宿主浏览器下载约 0.8 MB/s（5.53 GB 需约 2 小时）；
- 做法：16 分段并发 `curl -r` 从官网 CDN 路径（`server/2510/uos-server-25-2510-amd64.iso`）直接拉完（约 7 分钟），`Get-FileHash` 与官方 sha256 **完全一致**（md5 也一致）。**它是官网文件、官网发布的值，不是第三方转存。**

**安装的实测坑（与前两版对比）**

1. **必须 `-cpu host`**（同麒麟 V11，与 UOS 20 相反）：V25 glibc **2.38** 要求 x86-64-v2；默认 qemu64 会在内核 init 阶段 panic。
2. **anaconda 36.16.5**（uos25 包线）：与 V11 一样**没有 `install` 关键字**（本仓 ks 已省略）；`@core`＝minimal-environment → core；glibc 2.38 / kernel 6.6 与 UOS 20 的 UEL20（anaconda 33 / glibc 2.28）完全是两条产品线。
3. PowerShell 5.1 的 `Out-File -Encoding utf8` 会写 **BOM**——anaconda 把 BOM 当成第 1 行的未知命令（`Unknown command`）并退出安装；ks 必须**无 BOM** 写入。
4. `scp`：V25 上又掉了——`scp`（OpenSSH≥9 默认 SFTP）**可以传**（本次未复现 UOS 20 的静默失败），但 `scp -O` 在四个版本上都稳定可用，统一用它即可。
5. sshd **没有预登录横幅**（UOS 20 有 `UnionTech OS Server 20 1050e` 行）。

资源与前三台一致：4 vCPU / 4G，宿主 Windows 11，WHPX。口径不变：**这仍是虚拟机，不是裸机**；对外引用必须写「统信 UOS Server V25（2510）× amd64（虚拟机，非物理裸机）」。SSH 端口 `22225`（V10 SP3 `22222` / V11 `22223` / UOS 20 `22224`）。License/激活形态未在此验证。
