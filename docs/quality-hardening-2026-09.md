# LEVEE 质量还债与可信度收口方案（2026-09）

> 状态：评审通过（修订稿）
> 评审：2026-09-06 独立评审代理，结论 APPROVE_WITH_CHANGES；P0×2（C-1 漏 194MiB 存量断代、C-6
> "pause 天然有 run"前提与代码相反）、P1×7（C-8 迁移机制不成立、C-5 清零承诺不可兑现、C-3 反射边界、
> 覆盖率验收口径机械化、提交自洽定义、验收夹具不全、风险表缺行）已全部按评审意见修订入本稿。
> 输入：2026-09-06 全局质量评估（全量测试/覆盖率/lint 实测 + 三个只读核查代理对安全台账的逐条代码核对）
> 关联：docs/security-audit.md、docs/mvp-plus1-roadmap.md

## 第1章 背景与目标

项目工程流程（CI/测试/文档/诚实化文化）处于第一梯队，但存在三类"宣称与兑现的差距"：

1. **安全台账失更**：26 项发现中 16 项（SA-004~019）状态空白，修复声明无法对账；
2. **门禁口径失真**：本地 golangci-lint 报 34 处问题而 CI lint 绿（版本漂移 + Windows CRLF），
   覆盖率门禁把 3126 行 0% 的生成代码计入分母；
3. **测试结构性缺口**：internal/state 50.9%（PG 半边 0%）、CLI 44.9%、前端零测试。

目标：本轮全部收敛以上差距，并把核查中发现的真实缺陷（含一个**已发生的**发布级兼容性事故）修复掉。

## 第2章 范围

### 2.1 做

| 编号 | 工作流 | 一句话 |
|---|---|---|
| A | 工具链对齐 | .gitattributes、CI lint 版本对齐、覆盖率门禁剔除生成代码 |
| B | lint 清零 | 34 处问题逐类修复，CI 与本地口径一致 |
| C | 安全修复 | 核查确认仍有效的 12 项 SA 发现 + 1 个已发生的凭据存量断代事故 |
| D | 台账闭环 | security-audit.md 逐条标注终态 + 修正"部署安全声明"中与代码脱节的条目 + 修正 CHANGELOG 虚假锚点 |
| E | 测试补强 | state（含本地 docker PG 联合覆盖率）/ CLI / 前端 vitest 逻辑层 |

### 2.2 不做（及理由）

- **集群在途故障转移/跨节点调度**：4-6 周量级的特性工程，混入还债轮必然烂尾；保持 README 的如实标注不动。
- **serve 侧 RBAC 全量接线**（token auth → permission checker 全链路）：架构级改造，与多租户 per-request
  传播同属一类，作为独立设计立项；本轮 SA-007 只做"CLI pause 路径拒绝落 audit 表"的部分接线（见 C-6）。
- **AI 链路评估集**：依赖试点数据，无数据则评估集无意义；随试点启动。
- **前端组件级测试（@vue/test-utils）**：先立 vitest 基座测逻辑层，组件测试列为后续项。
- **Windows CI race**：维持 CI 现状（Windows 不跑 -race），本地对并发敏感包跑 -race。
- **trace.run_id 可空 schema 迁移**：NOT NULL→NULL 在 SQLite 要重建表，风险超收益；无 run 上下文的拒绝
  审计改走 audit 表（无 FK，见 C-6），trace 路线列后续项。

## 第3章 分析结论（事实基线）

### 3.1 lint 34 处的构成与根因

| 类别 | 数量 | 根因 | 处置 |
|---|---|---|---|
| gofmt | 4 | Windows 工作副本 CRLF（autocrlf=true），仓库内是 LF，CI 看不到 | .gitattributes 定死 `*.go text eol=lf` |
| goimports | 2 | 真问题（inventory_service.go / importer.go import 分组） | 修 |
| bodyclose | 6 | 真问题（rest_gateway_extra_test.go 测试里响应体未关） | 修（补 close，不用豁免——测试同样该守规矩） |
| errcheck | 8 | os.Remove ×5、rows.Close ×3，均为 best-effort 语义 | `_ =` 显式丢弃 + 注释 |
| gocyclo | 4 | runServe(41)、probe_gate.applyParams(41)、gateFromCheck(32)、Importer.Import(31) | 函数拆分重构（见 4.B.5） |
| unused | 6 | protojson 单轨化重构遗留死代码 + audit/canonical.go 两个死符号 | 删除（逐个确认无引用） |
| revive/staticcheck/unparam | 4 | fixtureIdP 命名、S1008、未用参数 | 机械修 |

版本漂移：CI pin v2.12、本地 2.13.2 → CI 升到 v2.13 并在 Makefile lint 目标注释基准版本。

### 3.2 安全台账核查结论（代理逐条核对，证据略——见实现提交信息）

| 编号 | 核查结论 | 本轮动作 |
|---|---|---|
| SA-004 SecureZero | 已修复（KeepAlive 在函数体内） | 台账标注 FIXED |
| SA-005 argon2 194MiB | 提参**已随 v1.11.0（2026-08-27）发布**（CHANGELOG.md:91）——存量断代已经发生，不止是隐患：v1.11.0/v1.12.0 上 Store/Rotate 过的凭据是"无 magic + 194MiB"blob，v1.10 前存量是"无 magic + 64MiB"blob，两代存量都需要救 | 修复 C-1（发布级事故收口） |
| SA-006 矩阵线程安全 | 已修复 | 台账标注 + 修 checker.go 过时注释 |
| SA-007 拒绝审计 | PARTIAL：机制齐备但生产零接线；空 RunID 被 trace FK 拒后静默丢。CHANGELOG v1.11.0 声称"权限校验拒绝时自动记录审计 trace"为**虚假锚点**，需点名修正 | C-6 走 audit 表接线 + D 章修正锚点 |
| SA-008 链排序 | 已修复（双后端 timestamp,id 二级键） | 台账标注 FIXED |
| SA-009 脱敏列表 | 仍有效（8 项、无扩展机制） | C-3 修复 |
| SA-010 结构体脱敏 | PARTIAL（map/slice 已覆盖，struct 未覆盖） | C-3 修复 |
| SA-011 Retrieve 明文 | PARTIAL（3 个调用点合规、约束靠纪律；cmd_serve.go:603 把明文复制进 string——但该 string 落在 channel.CredentialRef 的 string 字段上，见 C-5 的诚实边界） | C-5 修复 + 边界文档化 |
| SA-012 newID 降级 | 仍有效且扩散：**push/deeplink 一次性审批 token 也走时间戳降级**（最危险点）；全库清点 18 处降级点、三种策略并存 | C-2 统一策略 |
| SA-013 admin+`*` 超集 | 仍有效 | C-4 加载告警 + 文档 |
| SA-014 WORM TOCTOU | PARTIAL：PK+触发器已兜住数据完整性，剩错误语义 | C-7 约束错误映射，台账降档 LOW |
| SA-015 blob 无版本 | 仍有效 | 并入 C-1 |
| SA-016 action 不校验 | 仍有效 | C-4 |
| SA-017 无 HMAC | **实际已修复**（canonical.go LEVEE_AUDIT_HMAC_KEY opt-in，台账未标） | 台账标注 FIXED(opt-in) + 部署文档补 env 说明 |
| SA-018 Tags 不持久化 | 仍有效 | C-8 持久化（前置：迁移机制重构） |
| SA-019 synchronous=NORMAL | 仍有效 | C-8 配置项 |
| SA-020~026 | 实现正确（INFO） | 台账标注保持 |

"部署安全声明（2026-08-22）"两条与代码脱节：REST 网关已内置限流（gRPC 原生端口仍无限流，措辞收窄）；
Actor 身份在命名 token/OIDC 模式下已可证明（仅 legacy 共享 token 仍为断言，加范围限定）。KMS TLS 跳过半句
在 kms_helpers.go 已无对应代码，删除该半句（Vault 侧 Insecure 项仍在，保留）。

### 3.3 覆盖率缺口事实

- state 50.9%：缺口几乎全部在 pgstore.go/inventory_pg.go（PG 测试在 CI 的 postgres job 里跑，本地与门禁口径都不计）；
  另有 sqlite 真漏网：DeleteLockByIDAndOwner、GetInventoryGroupByName。
- CLI 44.9%：未覆盖大头 cmd_drift(337)/calendar(184)/serve(175)/push(124)/target(108)/secret(107)/retry(83) 语句。
  serve 的组装路径由 e2e 兜底，不计入本轮 CLI 目标。
- 前端：无 test 脚本，CI 只 type-check+build。
- 门禁现值 68.4%（含 pb 0% 的 3126 语句分母）；剔除生成代码后约 71%（不含 PG 半边贡献）。

## 第4章 设计

### A. 工具链对齐

1. 新增 `.gitattributes`：`* text=auto`、`*.go text eol=lf`、`*.sh text eol=lf`、`*.bat text eol=crlf`、
  生成物/二进制标 binary。
2. ci.yml lint job 的 golangci-lint-action version 升 v2.13（与本地一致）；Makefile lint 目标加注释写明基准版本。
3. 覆盖率门禁：计算 total 时剔除 `internal/grpc/pb`（生成代码）。实现：门禁脚本先 `grep -v '/internal/grpc/pb/'`
   过滤 profile 再 `go tool cover -func`。**本轮 CI 地板维持 60 不下调**（过滤后 CI 基线约 71，CI 侧 PG 测试不计
   覆盖率故取保守值）；第 5 章验收 3 的 70% 是本地人工核对口径；"CI 地板抬到 70"写入 CHANGELOG 作为下轮动作。
4. 顺手删除本次评估产生的 lint_report.txt/cover_scoped.out（*.out 已 ignore；lint_report.txt 不入库）。

### B. lint 清零

- B.1 gofmt：对 4 个 CRLF 文件跑 `gofmt -w`（在 .gitattributes 生效后语义不变）。
- B.2 goimports：修 2 处 import 分组（local-prefixes 规则）。
- B.3 bodyclose：rest_gateway_extra_test.go 的 post() 辅助返回前统一收口——测试辅助里对非 2xx 路径也补
  `resp.Body.Close()`（改辅助函数一处，六个调用点自动受益；若辅助结构不支持则逐点补）。
- B.4 errcheck：8 处改 `_ = os.Remove(...)` / `_ = rows.Close()`，保留/补充既有 best-effort 注释。
- B.5 gocyclo 拆分（不改行为，拆分后各段 <30）：
  - `runServe` → 按装配阶段拆 `wireStores/wireEngines/wireGateway/wireCluster` 私有函数；
  - `applyParams` → 参数解析按参数组拆 `applyXxx` 小函数；
  - `gateFromCheck` → 类型 switch 拆表驱动；
  - `Importer.Import` → 拆 parse/validate/commit 三段。
  每个拆分保持同文件内私有函数，不动导出 API；以现有测试全绿为重构正确性判据。
- B.6 unused：删 canonicalVersion、hmacKeyRead、maxAuditFetchRows、matchLabelSelector、cloneTarget、
  sortTargetsByID（每个先全库 grep 复核确实零引用；canonical.go 若注释引用了版本号，把说明移入注释文本）。
- B.7 revive/staticcheck/unparam：fixtureIdP→fixtureIDP、S1008 化简、handleSystemAuthInfo 未用参数改 `_`。

### C. 安全修复

**C-1 凭据 blob 版本化 + 两代存量兼容（SA-005+SA-015，发布级事故收口）**

评审修正：194MiB 提参已随 v1.11.0 发布（CHANGELOG.md:91），**断代已发生**——存量有两代无头 blob：
v1.10 前（64MiB, t=3, par=4）与 v1.11/v1.12（194MiB, t=3, par=4）。解密必须按候选参数链尝试，
GCM 认证成功者胜出。

新格式自描述，KDF 参数随 blob 走，任何参数演进不再破坏存量：

```
header = "LEV1"(4B magic) || ver(1B=0x01) || time(1B) || memKiB(4B BE) || par(1B) || salt(16) || nonce(12)
cipher = header || GCM-sealed(plaintext)      // header 共 38B，密文最小总长 38+16=54B
```

- encrypt：总是写 v1 头 + 当前默认参数；参数约束 `time ≤ 255`（1B 编码域），超出直接报错
  （当前 NewCredentialStoreWithParams 允许任意 uint32，加校验并在格式说明写明）。memKiB 用 4B：
  argon2 memoryCost 定义域即 uint32 KiB，不过度。
- decrypt 候选参数链：
  1. `magic 命中 && ver==0x01 && len ≥ 54` → v1 路径，用头内参数派生。**此路径失败一律硬报错**
     （损坏就是损坏，不回退——避免污染诊断语义、白烧一轮 argon2）；
  2. 否则按旧格式（salt||nonce||ct）以**当前默认参数（194MiB, t=3, par=4）**派生（覆盖 v1.11/v1.12 存量）；
  3. 仍 GCM 拒绝 → 以 **legacy 参数常量（64MiB, t=3, par=4）**派生（覆盖 v1.10 前存量）。
     两常量组命名 `headlessCurrentParams`（从 store.go 现默认引用）/ `legacyParams`，来源在注释标注。
- magic 碰撞处理确定性化：magic 命中但 `ver!=0x01` 视为碰撞的旧 blob（概率 2^-32×255/256），走 2→3 链；
  不再对"v1 路径内失败"做任何回退。
- 命中 2/3 旧路径时打一条 INFO 日志（"legacy credential blob decrypted, rotate master password to
  upgrade"）；legacy 路径声明删除窗口：v1 格式发布后两个大版本评估移除。
- RotateMasterPassword 成功后自然全量升 v1。
- 测试矩阵：v1.10 前格式（64MiB）blob 解密成功；**v1.11.0 格式（194MiB 无头）blob 解密成功**（golden
  夹具，两代各一，离线生成入库）；v1 round-trip；v1 内嵌非常规参数按头解密；篡改各段失败；
  magic+ver!=0x01 碰撞走旧链；magic+ver==0x01 损坏 blob 硬报错不回退；len<54 拒绝。
  除 golden 夹具外所有加解密单测走 `NewCredentialStoreWithParams` 降参构造，避免 argon2 194MiB×N 拖慢测试。

**C-2 随机 ID 失败策略统一（SA-012）**

统一策略：**身份类标识生成失败一律硬失败返回错误**（对齐 audit/trace.go 先例）；纯观测类允许降级并注释
说明。全库清点 18 处降级点，PR 描述附逐点分类清单（身份类→硬失败 / 观测类→降级 / 豁免），分类标准以
"该 ID 是否被用作唯一性/授权凭据"为判据。

- 身份类（硬失败，签名改 `(string, error)` 或错误上抛）：credential.newID、push/deeplink GenerateToken
  （仓内调用点仅 deeplink.go:170 一处 + 2 测试，波及面小）、approval/engine closure/lock/pause/plan/
  rollback snapshot/tenant/notify/drift×2/verify probe_gate auditID/inventory importer、CLI cancel、
  retry、rollback 审计 ID、cmd_group 吞错点。
- 观测类（保留降级+注释）：`req-unknown` 有**两处**——internal/grpc/interceptor.go:82 与
  internal/grpc/rest.go:2271（评审补充第二处），均允许降级。
- 豁免（评审补充）：internal/grpc/change_service.go:98-103 的"故意 panic 靠 recovery interceptor 兜底"
  策略维持现状，在分类清单中标注豁免，不在本轮收编。
- 测试：可注入点做失败注入（credential.newID 接受 io.Reader；deeplink GenerateToken 同理）；
  被改签名的 CLI 命令逐个补 exit-code 断言（现有 harness 支持）。

**C-3 脱敏增强（SA-009+010）**

- 词表扩至：password/passwd/secret/token/credential/private_key/api_key + passphrase/auth_code/
  refresh_token/access_token/ssh_key/cert/certificate/connection_string（新增 8 项），保留裸 `key` 全等匹配。
- 匹配规则升级：精确命中 或 `_`/`-` 词边界后缀命中（`db_password`✓、`sort_key`✗、`access-token`✓）。
- 配置扩展：`security.sensitive_fields: []` 追加自定义字段（合并内置表，config.Validate 不做存在性校验）。
- 结构体覆盖：redactComposite 增加 reflect 遍历。实现约定（评审 P1 补充，防实现踩坑）：
  - **可见性口径 = json.Marshal 可见字段**：未导出字段本就不出现在 Detail 的 JSON 里，遍历时
    `!CanInterface()` 直接跳过，脱敏副本以"Marshal 可见字段"为键构造 map——与原序列化形状一致，
    不引入兼容性变化；json tag 为 `-` 的字段同样跳过（与 Marshal 语义对齐）。
  - **known-leaf 短路表**：time.Time、json.RawMessage、error、数组（如 [16]byte）与全部基础类型不再
    递归，直接原值保留——防止烧穿深度、防止数组展开成 16 个元素。
  - **深度上限 8 为主防护**；visited 集只对 map/指针有效（值语义无稳定地址），仅收集 map/ptr，
    裸指针环由深度上限兜底。此口径写入代码注释。
  - 仅当子树内**存在命中字段**时才构造脱敏副本，否则原值返回——避免所有 struct 都变成 map 的形状漂移。
- 已知边界（写进台账与代码注释）：值内嵌明文（如 trace.go:325 `d.Error = record.Error.Error()`，
  "auth failed for secret=hunter2"）键名规则永远覆盖不到，属 SA-011 精神范围，本轮不修、只文档化。
- 性能护栏：trace 包已有 benchmark_test.go，加一条含嵌套 struct 的 Redact 用例，防热路径回退。
- 测试：边界矩阵（后缀命中/误伤反例/配置扩展/嵌套结构体/未导出字段跳过/known-leaf 短路/循环引用/深度上限/
  仅命中才转 map）。

**C-4 权限矩阵加固（SA-013+016）**

- LoadFromConfig：检测到 `admin` grant 且 env 为通配 `*` 时输出 WARN（列出 team）；文档在
  config.example.yaml 权限段注释风险。WARN 去重（评审补充）：LoadFromConfig 直接写 grants 表不经 Grant
  （matrix.go:135-170），双发只可能来自调用方自行循环 Grant（cmd_rbac.go:562 路径），Grant 内告警按
  "每次 Grant 调用一条"记录，不做跨调用去重——调用方日志天然成组。
- Grant/Revoke/LoadFromConfig：action ∉ AllActions 时 WARN（"unknown permission action"）；新增
  `PermissionMatrix.StrictActions bool`（NewPermissionMatrix 选项或导出字段），严格模式下 Grant 返回错误、
  LoadFromConfig 整体失败。默认 warn 不破坏存量配置。
- 测试：警告可断言（捕获日志或返回收集列表）+ 严格模式拒绝。

**C-5 凭据明文生命周期（SA-011，诚实化修复）**

评审修正：cmd_serve.go:603 的明文最终 `json.Unmarshal` 进 channel.CredentialRef 的 **string 字段**
（Password/KeyPassphrase，channel.go:69-74）。Go string 不可清零， CredentialRef 改 []byte 波及
ssh/winrm/grpc 全线，超出还债轮范围。本轮走诚实路线：

- 消除 cmd_serve.go:603-606 的**额外**中间明文副本：直接把解密产物 []byte 交给反序列化路径，不再先复制
  进局部 string 变量再传递；对无法避免的"落库到 string 字段"这一事实，在 SA-011 台账行如实标注为
  已知残留（channel 配置类型为 string，副本不可清零），不开"已修复"空头支票。
- 新增 `CredentialStore.RetrieveInto(name string, fn func(secret []byte) error) error`：回调返回后统一
  SecureZero（即便 panic 也经 defer 清零）；文档将 Retrieve 的清零义务升级为 MUST。
- 存量 3 个合规调用点不强制迁移（回归风险>收益），在 Provider.Get/KMS 注释指向 RetrieveInto 为新代码首选。

**C-6 拒绝审计接线（SA-007，改走 audit 表）**

评审推翻了原设计的前提：CLI pause 路径的唯一权限拒绝点是 PauseAll/ResumeAll（pause.go:265、281），
拒绝发生在挑选任何 run **之前**，手里没有任何 run ID——"pause 天然有 run 上下文"与代码相反；
checker.go:228-236 的占位 run_id 恰会被 trace FK（schema.sql:80-87：trace.run_id NOT NULL + FK）拒掉。

- 接线改走 **audit 表**（schema.sql:166-168：`run_id TEXT NOT NULL DEFAULT ''`，无 FK，天然可挂无 run 事件）：
  pause 服务层拒绝时 `CreateAudit(action="permission.denied", result="denied", actor=<拒绝主体>,
  detail=<env/action>)`。recorder 从 cmd_pause 的 newCLIPermissionChecker 构造点注入（CLI 已有 state
  store）；实现取"pause 层拒绝时记录"路线——pause.PermissionChecker 接口不动，SimplePermissionChecker
  增加可选 audit 记录回调字段（不引入 permission 包的 WithRecorder 依赖，避免 pause→permission 包间耦合）。
- 无 store 的场景（纯内存/测试）：维持 ERROR 日志（现有行为），注释说明。
- trace 表路线（run_id 可空迁移）明确列后续项，写入台账边界。
- permission checker 生产构造点（若有新增）一律 WithRecorder。
- 台账：PARTIAL，剩余边界写明 serve 侧 RBAC 接线为独立立项。

**C-7 WORM 重复写错误语义（SA-014）**

- state 层 CreateTrace 识别 UNIQUE 冲突并返回 `ErrTraceExists`（state 包哨兵），识别方式锁定
  （评审 P2 具体化，避免脆的错误串匹配）：
  - SQLite：`errors.As` 到 `modernc.org/sqlite` 导出的 `*sqlite3.Error` 且 `Code&0xFF == 19`
    （SQLITE_CONSTRAINT；go.mod 锁 v1.56.0 该类型可用）；错误串 "UNIQUE constraint failed" 仅作兜底。
    现有 sqlite.go:520 用 `%w` 包装，errors.As 可穿透。
  - PG：`errors.As` 到 `*pgconn.PgError` 且 `Code == "23505"`。
- audit/worm.go Append 将其映射为 ErrAlreadyExists；worm.go:79-85 的 GetTrace 预检保留（快路径），
  C-7 覆盖其 TOCTOU 残差。
- 测试：并发双写同 ID 只有一个成功、另一个拿到 ErrAlreadyExists（双后端）。

**C-8 小项（含前置重构）**

- **前置（评审 P1）：迁移机制重构**。现 migrate.go 是"currentSchemaVersion=1 + 整体重放 schema.sql"
  （migrate.go:22、46-63），ALTER TABLE 不幂等无法挂载；PG 侧 pgMigrate（pgstore_support.go:69）同构。
  先把 Migrate 重构为**按版本递增的迁移步表**（`migrations []migrationStep{version, stmts}`，读
  appliedSchemaVersion 后逐段执行、逐段落版本），schema.sql 保持新库一次建齐；SQLite/PG 两份对称改造，
  各自补"v1 存量库 → v2"的迁移测试。
- SA-018：credentials 表加 `tags TEXT NOT NULL DEFAULT ''`（JSON map 序列化）作为迁移步 v2（SQLite
  ALTER TABLE ADD COLUMN，PG 同；新库 CREATE TABLE 同步含列）；CredentialSpec/结构体与 List 路径透传。
  测试：存/取/空 tags 兼容旧行 + v1→v2 迁移测试。
- SA-019：config `state.sqlite_synchronous: normal|full`（默认 normal 保持现状），sqlite 打开时按配置设置
  PRAGMA；部署文档说明"审计强持久场景用 full（约 1-2% 写放大）"。

### D. 台账闭环（纯文档）

- 每个 SA 小节标题追加状态：`[已修复 v1.0.0] / [已修复 Unreleased] / [已修复 本方案后 Unreleased] /
  [部分修复+边界] / [不修复：理由]`；修复摘要表重写为 26 行全量终态。
- 修正"部署安全声明"：限流条目收窄到 gRPC 原生端口；Actor 条目加命名 token/OIDC 范围限定；删 KMS TLS
  跳过半句；沙箱条目文件路径指向 internal/plugin。
- **修正 CHANGELOG 虚假锚点**：v1.11.0 小节"[SA-007] 权限校验拒绝时自动记录审计 trace"与"生产零接线"
  的核查结论冲突，加勘误注（不改写历史行文，追加勘误段），防止再次形成虚假锚点。
- 部署文档补 `LEVEE_AUDIT_HMAC_KEY` env 说明（SA-017 已实现未文档化的部分）。
- 追加"2026-09-06 核查记录"小节说明本轮状态判定的方法（核查代理 + 证据标准 + 本轮评审修正记录）。
- CHANGELOG 补 Unreleased 条目（安全修复逐条 + "下轮把 CI 覆盖率地板抬到 70"）。

### E. 测试补强

**E-1 state（目标：sqlite+PG 联合口径 ≥75%）**

- 本地跑法：docker 起 postgres:16-alpine（levee/levee-ci@:5432/levee_test，同 CI 参数）→
  `LEVEE_PG_TEST_DSN=... go test ./internal/state/... ./internal/cluster/... -coverprofile=cover_pg.out`。
- 覆盖率合并：`scripts/merge-cover.ps1`（按块取 max(count) 的小工具，**入库**，评审 P1：不入库则联合
  口径不可复现）；README-deployment 记录完整命令序列。
- 补 sqlite 漏网：DeleteLockByIDAndOwner、GetInventoryGroupByName 直接补用例。
- pgstore 无 DSN 时保持 skip（现状），CI postgres job 不变。

**E-2 CLI（目标：44.9% → ≥60%，剔 serve）**

优先级按未覆盖语句量：cmd_drift(337)、calendar(184)、push(124)、target(108)、secret(107)、plugin(100)、
chatops(94)、system(90)、audit(84)、pause(84)、retry(83)、helpers(61)。方法：沿用 cmd_compile_test/
cmd_converse_test 的既有 harness（findSub、临时 SQLite、捕获输出、JSON 输出断言），表驱动。serve 组装路径
排除（e2e 覆盖）；目标按排除 serve 后计算，硬线 60%。
**cmd_drift 单列子任务**（占缺口约 1/5，且依赖调度器/时钟）：先给 drift 命令建时钟/rand 注入点再测；
若投入超预算允许降级达标线：CLI 整体 ≥58% 且 cmd_drift 自身 ≥40%，在提交信息中如实标注。

**E-3 前端 vitest（逻辑层关键路径）**

- devDeps 加 vitest + jsdom；`vite.config.ts` 加 test 配置（environment: jsdom）；scripts 加
  `test: vitest run`、`test:watch`。
- 用例：api/client（token 三态存取、请求拦截器 Bearer 注入、401→清 token+重定向+并发去重+登录页豁免、
  normalizeError 五种形状）——通过替换 axios 实例 defaults.adapter 注入 canned 响应，零新增 mock 依赖；
  utils/format、api/sso 的纯函数；
- CI frontend job 在 build 前加 `npm run test`。

## 第5章 验收标准（全局核验清单）

1. `go vet ./...` 无输出；`golangci-lint run` **0 issues**（本地 v2.13.2，与 CI 同版本）。
2. `go test ./... -count=1` 全绿；并发敏感包（permission/audit/credential/cluster/state）另跑 `-race`
   全绿（本机不支持 race 时以 CI race job 绿为准）。
3. 覆盖率（人工核对口径，本地合并 profile 后用 scripts/merge-cover.ps1 + `go tool cover -func` 计算）：
   total 剔 pb ≥70%；`internal/state` sqlite+PG 联合 ≥75%；`cmd/levee` 剔 serve ≥60%（或 E-2 降级线）。
   CI 门禁本轮维持 60（含 pb），下轮抬 70（CHANGELOG 记录）。
4. C-1 迁移测试：golden 夹具两代——64MiB 旧参数 blob 与 v1.11.0 的 194MiB 无头 blob——在新代码下解密
   成功，并可在轮换主密码后升 v1；v1 损坏 blob 硬报错。
5. `cd web && npm run type-check && npm run test && npm run build` 全绿；dist 无 UI 变更不重嵌入。
6. postgres job 语义本地复现：docker PG + DSN，state/cluster 测试全绿。
7. 台账 26 项全部有终态标注；部署声明与代码事实一致（自审：每条限制都能给出当前代码 file:line 或"已移除"）；
   CHANGELOG v1.11.0 SA-007 勘误落笔。
8. lint_report.txt、cover_scoped.out、docker 容器清理（scripts/merge-cover.ps1 入库保留）。
9. 分逻辑提交（约 10 个）；**提交点自洽定义**（评审 P1）：每个提交点上 `go build ./...` + 受影响包
   `go test` 全绿 **且 `golangci-lint run` 0 issues**（提交前跑，非仅 HEAD 断言）；不 push（等用户确认）。

## 第6章 提交切分

1. `build: .gitattributes 统一行尾 + CI golangci-lint 版本对齐 + 覆盖率门禁剔除生成代码`
2. `chore(lint): 清零 34 处告警（errcheck/bodyclose/gocyclo 拆分/unused 死代码/命名）`
3. `fix(credential): 凭据 blob 版本化自描述 KDF 参数 + 两代无头存量参数回退链（SA-005/015）`
4. `fix(security): 随机 ID 生成失败统一硬失败，deeplink 一次性 token 不再降级时间戳（SA-012）`
5. `feat(security): 脱敏词表扩展+词边界后缀+配置扩展+结构体反射遍历（SA-009/010）`
6. `fix(permission): admin+通配符告警、action 白名单校验（严格模式）、矩阵注释修正；pause 拒绝审计走 audit 表（SA-006/007/013/016）`
7. `fix(audit): trace UNIQUE 冲突映射 ErrAlreadyExists（SA-014）`
8. `refactor(state): 迁移机制改按版本递增步表（C-8 前置）`
9. `feat(state): 凭据 tags 持久化 + sqlite_synchronous 配置项（SA-018/019）`
10. `fix(credential): cmd_serve 明文副本消除 + RetrieveInto 回调（SA-011 诚实化）`
11. `docs: 安全台账 26 项全量闭环 + 部署安全声明对齐 + CHANGELOG SA-007 勘误`
12. `test: state 联合覆盖补强 / CLI 表驱动补强 / web vitest 基座`（拆 3 个提交，merge-cover.ps1 随 state 测试提交入库）

## 第7章 风险与回滚

| 风险 | 缓解 |
|---|---|
| C-1 改加密格式引入新回归 | 三级候选链仅对无头 blob 生效；v1 路径失败硬报错不掩盖损坏；golden 夹具两代先行 |
| **v1.11.0/v1.12.0 存量 194MiB 无头 blob 断代**（评审 P0） | 候选链第 2 级即当前默认参数解密；golden 夹具锁定；升级说明写入 CHANGELOG |
| gocyclo 拆分改行为 | 只搬代码不改逻辑，现有测试全绿为准；每函数拆分独立可 revert |
| newID 硬失败改变错误路径 / CLI exit code 漂移 | 仅身份类 ID；调用点全部显式传播；被改 CLI 命令逐个补 exit-code 断言 |
| 脱敏词边界误伤 | 反例测试锁定（sort_key/primary_key 不命中）；`key` 行为不变 |
| C-3 反射进入 Record 热路径性能回退 | trace 包 benchmark 加 Redact 用例作护栏；仅命中才转 map、known-leaf 短路 |
| 迁移机制重构本身出错 | 按版本步表逐段落版、失败即启动报错不静默；v1→v2 存量迁移测试双后端 |
| vitest 引入破坏前端构建链 | test 与 build 独立脚本；CI 顺序 test→build，build 失败可整体 revert E-3 |
| CLI 表驱动测试不稳定 | 全部临时 SQLite + 注入时钟/rand，无网络无端口依赖 |
| WARN 日志噪声 | 已核查：serve 路径不构造 PermissionMatrix（LoadFrom* 仅 cmd_user/cmd_rbac 调用），告警是一次性非每请求；Grant 告警按调用粒度不做跨调用去重 |
| legacy 解密路径永久保留（现为三条路径）的复杂度 | 旧路径命中打 INFO 提示轮换；删除窗口=v1 发布后两个大版本，写入注释 |
