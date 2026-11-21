# 签单非代码资产证据包（2026-11-20，预登记/备查）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | **应答素材预备**，不是任何标书的应答文件 |
| 前置状态 | `F:\Nexus\agent-board\levee\opencode-tender-registry.md` 的 **R 区仍为空**（尚未收到标书原文/要点）；本文只能按 M 区「市场惯例」做对照 |
| 铁律 | R-xxx 一律以标书原文为准；未确认的猜测不得进 R 区。本文所有条目均为「预登记锚点」，不得当作已确认的招标要求 |
| 引用限定 | 每条信创证据必须带「**虚拟机，非物理裸机**」；桌面 V25 另须披露「已切允许任意应用、drill 走用户态 sshd」；性能/供应链结论**只作研发侧相对比较**，不作招投标门槛值、不引用为认证 |

---

## 1. 市场惯例 → 本仓证据对照（M-xx 预登记）

| 编号 | 市场常见条款 | 本仓可引用证据 | 诚实缺口（必须同时给出） |
| --- | --- | --- | --- |
| M-01 | 鲲鹏/飞腾 CPU＋麒麟/统信 UOS；达梦/金仓等国产 DB | `docs/platform-support-matrix.md` §1/§2：麒麟 V10 SP3 / V11、统信 UOS Server 20 / V25、UOS 桌面 V25 × amd64 全链路走通（虚拟机）；金仓 V9 PG 模式可用 | **物理裸机、ARM 真机未验证**；达梦/openGauss 实测不兼容；桌面 V25 的两条限定见上 |
| M-02 | 审计留存 ≥6 个月、防篡改、可导出 | `docs/platform-support-matrix.md` §1：审计链 WORM＋哈希链＋`audit verify/export`；各 OS 取证均 `valid=true`；SM3 迁移设计见 `docs/audit-chain-mode-migration-design.md`（设计完成、代码未动） | 留存期声明与容量策略未核查；`LEVEE_AUDIT_HMAC_KEY` 未设时摘要为无口令 SHA-256（tamper-evident，**非 tamper-proof**）；审计链**尚未支持 SM3** |
| M-03 | 双因素、三权分立、最小权限 | `auth.oidc`/`auth.github`、RBAC/ABAC/tenant、发起/审批/管理角色 | MFA 双因素未核查；无真实 IdP 联调 |
| M-04 | 国密 SM2/SM3/SM4、密评 | `internal/gmcrypto` 提供 SM3（GM/T 0004）与 SM4（GM/T 0002）纯 Go 包装 + 自检；凭据存储可选 `credential.encryption: sm4-gcm`（LEV2 算法标记 blob，AES/SM4 双向可读）；默认仍为 aes256-gcm | **仅算法级增量，不等于密评**：TLCP/国密 TLS、SM2 签名、审计链 SM3 迁移（D-8 前置）、硬件密码模块/国密 RNG、密评报告均**未做**；命中即专项立项 |
| M-05 | 4A、操作录像回放 | 账号/审批/审计链具备 | LEVEE 是变更治理、非堡垒机；**无会话录像**（无代理设计），须讲清边界 |
| M-06 | 对接 ITSM/CMDB/监控/工单、开放 API | gRPC/REST/ChatOps 已备，`docs/levee-api.md` | webhook HMAC 签名（E-2）与结构化导出仍按需；API 对接说明待整理 |
| M-07 | 集群/主备、故障切换、容灾演练 | D-03：容器靶场三场景 S1/S2/S3 实跑全绿；集群故障接管已具备 | 缺多节点生产浸泡数据（A-5） |
| M-08 | 并发用户/纳管规模/响应时间 ≥99.9% | `docs/benchmark-report.md`（2026-11-20 复测） | **仅单实例、研发侧相对比较**；无并发压测/长跑/大库容量承诺，不作 SLA 值 |
| M-09 | 内网离线部署、容器/K8s、信创 | `deploy/baremetal/install.sh` 幂等、`deploy/helm/levee` chart、D-02 断网策略四段 | **离线安装包本身未验证**；信创限定同上 |
| M-10 | 7×24 应急、驻场、培训、SLA、质保 | 商务侧（非代码） | 需商务口径，技术侧不承诺 |
| M-11 | 软著/检测报告/ISO27001/CMMI/案例 | 商务侧；demo 脚本＋容器靶场可演示；渗透测试采购预备见 `docs/pentest-sow-2026-11.md`（SOW/询价草案） | 案例/品牌素材缺（D-2）；**第三方渗透测试未采购未执行**，报告不可声称 |
| M-12 | 国产浏览器兼容（奇安信/360 等） | CI 跑 chrome/firefox 四矩阵 | **国产浏览器未测** |
| M-13 | SQL 审核平台：规则库/分级/审批/回滚/追踪/多数据源 | v1.2 定位「数据库变更发布与治理核」；`levee sqlreview` 自撰规则；审批→执行→快照回滚→审计链闭环保真；PG 方言可读性预研见 `docs/pg-dialect-readability-spike-2026-11.md`（oliphant 10/10 样本解析成功，待产品决策） | 多方言审核**未落地**（仅预研 + 测试专用 import，生产未接入）；仅 MySQL 语法层可审；对外不得说「PG 变更能审」 |

---

## 2. 非代码资产清单（可直接引用/待补）

| 资产 | 路径 | 状态 |
| --- | --- | --- |
| 平台适配矩阵（逐格三态） | `docs/platform-support-matrix.md` §1/§2/§5/§6 | 已交付、活文档 |
| 信创阶段性验收报告 | `docs/xinchuang-acceptance-2026-11.md` | 已交付；**标题含「阶段性（非最终签收）」**，缺项见 §4 |
| 第三方评估复核完善版结论 | `F:\Agent\opencode\workspace\Levee-第三方评估报告-复核完善版.md` | 已交付；§8 是唯一许可的对外话术清单 |
| 性能基线报告 | `docs/benchmark-report.md` | 已交付（2026-11-20 复测）；**只作研发相对比较** |
| 供应链扫描报告 | `docs/security-scan-2026-11.md` | 已交付（§8 为 11-20 复扫）；**自证基线，非认证** |
| 模块级 SBOM（CycloneDX 1.5） | `G:\levee-vm\evidence\sbom-2026-11-20.json` | 已生成，159 组件；镜像层 SBOM 仍由 CI trivy 承担 |
| UOS 桌面 V25 取证原始证据 | `G:\levee-vm\evidence\`（bundle/audit/setup/life/serve log） | 已留档，`run-2946297cc8777610` |
| Demo 脚本与容器靶场 | 仓内 demo 脚本 + `deploy/lab/container-lab` | 已交付；录像/品牌素材缺（D-2） |
| 部署交付检查单 | `docs/deployment.md` §15 | 已交付 |
| 审计导出样例 | `levee audit export` + 各 OS 取证的 `audit/verify` 输出 | 可现场导出；留存期策略待商务/运维确认 |
| 审计链 SM3 迁移设计（D-8） | `docs/audit-chain-mode-migration-design.md` | 设计完成（v12 加 digest_mode + 双口径验链 + 混合链策略）；**代码未动、需产品裁决** |
| PG 方言可读性预研（D-6） | `docs/pg-dialect-readability-spike-2026-11.md` + `internal/sqlreview/pg_oliphant_spike_test.go` | 预研通过（10/10 样本）；**生产未接入**，测试专用 import 不进发布二进制 |
| 渗透测试 SOW/询价 | `docs/pentest-sow-2026-11.md` | 采购预备；**未采购未执行**，不得作为已过渗透测试的证据 |
| 本地渗透自测报告 | `docs/pentest-self-assessment-2026-11.md` | 研发侧自测（隔离靶场 + sqlmap，3 项低/中改进点）；**不是第三方报告** |
| 密评差距自查清单 | `docs/miping-gap-checklist-2026-11.md` | 对照 GM/T 0054 控制面的改造面自查；**不是密评报告** |

---

## 3. 仍需外部输入才能补齐（不要自行编造）

1. **标书原文/评分表** → 填 R 区，否则 M 区对照只能作预判。
2. **带 CMA/CNAS 资质的第三方渗透测试报告** → 预算量级 ¥5–10 万/次、SOW/询价清单见 `docs/pentest-sow-2026-11.md`；本轮未采购未执行。已有 `docs/pentest-self-assessment-2026-11.md` 是**研发侧自测**，不得替代第三方报告。
3. **国密改造（M-04）** → 已完成算法级增量（SM3/SM4 + `sm4-gcm` 凭据选项）；审计链 SM3 迁移已有设计（`docs/audit-chain-mode-migration-design.md`，未实现、需产品裁决）；TLCP/国密 TLS、SM2 签名、硬件密码模块与**密评报告**仍是专项，需外部机构/预算。密评改造面自查见 `docs/miping-gap-checklist-2026-11.md`（自查，非报告）。
4. **物理裸机 / ARM 真机（M-01）** → 无门路/需租机，仍按未验证。
5. **MFA、国产浏览器、离线包、案例素材、7×24 服务承诺** → 分别对应 A-2/D-2/M-12/M-09/M-10，需产品/商务排期。

---

## 4. 边界

本文是**应答素材预备**，不构成对任何具体标书的响应；所有能力描述必须连同对应文档里的限定一起引用。对外口径以 `Levee-第三方评估报告-复核完善版.md` §8 为准，尤其不得说「裸机已验证」「已过渗透测试」「安全认证」「PG 变更完全能审/完全不能审」。
