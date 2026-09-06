# Changelog

本文件记录 LEVEE 项目所有重要变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 修复

- **ApplyChange 状态机竞态（P1）**：`ApplyChange` 读-判-写状态流转此前非原子——两个并发请求可同时通过状态检查并互相覆盖终态（双跑/状态翻转/审计污染）。新增 `state.Store.UpdateRunStatusIf`（`WHERE id=? AND status=?` 的 compare-and-set，SQLite/PG 双实现），gRPC `ApplyChange` 与 CLI `apply` 均改用 CAS 抢占 `approved/pending/draft → running`；失败方返回 `FailedPrecondition` 并携带最新状态（SQLite/PG 各新增 CAS 测试 + gRPC 并发双跑互斥测试 `TestApplyChange_ConcurrentDoubleApplyIsSerialised`）。
- **回滚状态语义丢失（P2）**：`EngineAdapter.Run` 此前只返回 success bool，引擎 `PhaseRolledBack` 在 `ApplyChange` 中被压平为 `failed`，回滚成功与彻底失败不可区分（而 `RetryChange` 依赖 `rolled_back` 状态却永远等不到）。`EngineAdapter.Run` 新增 `phase` 返回值，`phase=="rolled_back"` 时 run 落库 `rolled_back`（新增 `TestApplyChange_EngineRolledBackPersistsStatus`）。
- **无引擎 apply 假成功（P0 级体验缺陷）**：serve/网关未接线执行引擎时 `ApplyChange` 此前返回 `Success:true` 并把 run 置 `running` 后永久卡死。现改为 `FailedPrecondition`（"no engine wired ... status-only mode"）且不做状态流转；`serve` 启动时输出引擎未接线的 WARN 日志；CLI `apply` 保留状态流转但帮助文本如实描述为 status-only、非 JSON 输出追加 WARNING 行、JSON 输出新增 `engine_wired:false`。
- **`drift schedule add` 恒 panic**：`DriftScheduler.AddJob` 按值接收 job，调度器生成的 ID/NextRun 只存在内部副本，调用方持有的 `job.ID` 始终为空——CLI 以空 ID 回查 `GetJob("")` 得到 nil 且错误被丢弃，随后 `jobToMap(nil)` 空指针崩溃（该命令自诞生起从未成功过）。`AddJob` 改为返回存储副本 `(*DriftJob, error)`，CLI 与磁盘装载路径改用返回值；`internal/drift` 无外部调用方，API 变更完全收敛。
- **`drift baseline delete` 静默无效**：删除后保存只写不删，磁盘上的孤儿 JSON 文件被后续任意 drift 命令重新装载，被删基线"复活"。新增 `syncJSONDir` 目录全量对账（写入期望集 + 删除不在期望集中的 `*.json`），`baseline delete` 与 `schedule remove`（同类问题）同时修复；对账顺带拒绝含路径分隔符的 host/ID 文件名，封死以主机名/作业 ID 逃逸数据目录的路径穿越面。
- **`drift report` 缺 `--host` 打印空主机趋势**：不再输出空 host 的伪趋势表，改为 `no host specified (use --host)` 并以 exit=2 退出。
- **`system config set` Windows 路径 panic**：定位配置目录时按 `'/'` 做 `LastIndex`，Windows 反斜杠路径返回 -1 导致切片越界 panic；改用 `filepath.Dir`。
- **gRPC 服务器发布竞态（CI `-race` 捕获，生产代码）**：`Server.Start` 在 `mu` 临界区外写 `s.listener`，与 `Stop`/`GracefulStop`/`Addr` 的锁内读构成数据竞态——`Addr()` 可能读到半发布状态或读到与关闭操作不一致的 listener。`Start` 的 listener 发布移入锁内，`Stop`/`GracefulStop` 锁内快照、锁外关闭，`Addr` 补锁读取。
- **ShellRunner 输出缓冲竞态（CI `-race` 捕获，生产代码）**：命令超时路径上主 goroutine 读取 stdout/stderr 汇总时，`exec` 的 io 拷贝 goroutine 仍在向同一 `bytes.Buffer` 写入。输出缓冲改为带互斥锁的 `syncBuffer`（`io.Writer` 接口不变），超时截断与正常完成两条路径统一消竞态。
- **变更事件总线发布竞态（CI `-race` 捕获，生产代码）**：`publishEvent` 先在 `bus.mu` 锁内取出订阅者 map 引用、解锁后再遍历，而 `WatchChange` 退出时的 `unsubscribe` 并发向同一 map 删除键——race 检测器报 `WARNING: DATA RACE`，且 Go runtime 对"边遍历边写 map"可直接 fatal 崩溃。改为 fan-out 全程持锁：投递本身是非阻塞 `select/default`，锁持有时间有界。
- **插件沙箱 `Stop` 竞态 + 必然空等宽限期（CI `-race` 捕获，生产代码）**：`Stop` 在 `s.mu` 临界区内等待 `doneCh`，而 monitor 必须先拿到 `s.mu` 记录 waitErr 才能关闭 `doneCh`——等待永远失败、宽限期每次都耗满，然后读 `cmd.ProcessState` 判断进程是否已被回收；该字段由 monitor 协程里的 `cmd.Wait()` 写入，无任何同步边，构成真竞态（`TestManager*`/`TestSandbox*` 六测试在 linux/macOS race 腿全红）。重构为：锁内快照 cmd/doneCh、锁外等宽限；升级 kill 不再读 `ProcessState`（kill 到已退出的进程本就无害，以 `doneCh` 关闭作为进程消亡的唯一证据）。附带效果：`internal/plugin` 包测试 18.9s → 1.3s，不再每测固定空等宽限期。
- **gRPC `GracefulStop` 超时回退死锁（CI 10 分钟超时元凶，生产代码）**：宽限期耗尽后回退调用 grpc 的 `Stop()`，与仍在进行的 `GracefulStop()` 并发——grpc 文档明示二者互斥；v1.83.1 实现中 graceful 路径持 server 互斥等待 `handlersWG`，并发 `Stop()` 与之死锁，`Serve` 的退出等待（`<-s.done`）连带挂起整个关停序列（ubuntu race 腿 `TestGracefulStopTimeoutFallsBackToHardStop` 挂 9m22s 拖死整包测试二进制）。新增连接追踪监听器：graceful 排空进行中触发硬停时不再触碰 grpc `Stop()`，改为关闭监听器与全部已跟踪连接——HTTP/2 传输层被强断、客户端收到连接错误、grpc 在传输层关闭时取消所有活动流的 context，尊重 ctx 的 handler 得以退出（忽略 ctx 的 handler 与 grpc 自身 `Stop()` 行为一致，只能等其自然返回）。`gateStore` 测试网关同步改为感知 ctx。
- **`unflattenPath` Unix 下返回双斜杠**：Unix 的 flatten 把前导 `/` 编码为第一个 `_`，unflatten 剥掉 `root` 标记后替换出的路径已自带前导分隔符，再手动前插一个分隔符得到 `//etc/nginx/nginx.conf`（Linux/macOS `TestUnflattenPathAbsolute` 红）；现仅当重建结果不以分隔符开头（Windows 盘符形态）才前插。
- **Dockerfile 构建基底标签不存在（trivy 作业红灯）**：`golang:1.25-alpine3.20` 从未发布（Go 1.25 镜像从未跟踪 alpine 3.20），`docker build` 直接拉取失败。builder 改用官方为每个受支持 Go 系列必发的无后缀别名 `golang:1.25-alpine`；运行时基底升 `alpine:3.22`（`ALPINE_VERSION` 自此只约束运行时阶段）。
- **Linux 构建上下文安全/风格残留（本地工具盲区补漏）**：Windows 本地 gosec/golangci-lint 看不见 `_linux.go` 文件，ubuntu 腿暴露 `sandbox_linux.go` 三处——cgroup 目录 `0o755`→`0o750`（G301）、控制文件打开模式 `0o644`→`0o600` 并补 `Close` 错误处理（G302/errcheck）、`formatCpuMax`→`formatCPUMax`（revive）。本地复验流程同步固化 `set GOOS=linux` gosec/lint/vet 三件套。
- **分布式锁并发冲突错误误分类（windows 腿红灯暴露的生产缺陷）**：`internal/lock` 的 `Acquire` 在"查重后插入"窗口被并发对手抢占时，底层 `CreateLock` 的数据库 UNIQUE 约束错误被当作 `lock: create` 通用错误抛出——调用方无法把"锁已被并发持有"与"存储故障"区分开，`ForceAcquire` 抢占竞态同样裸抛。新增 `isUniqueViolation`（按 SQLite `UNIQUE constraint failed` / PostgreSQL `duplicate key value violates unique constraint` 文案匹配，与 registry、inventory 既有惯例口径一致）：`Acquire` 输家归一为 `ErrLockHeld` 哨兵，`ForceAcquire` 输家自动重试。`TestManager_ConcurrentAcquire_SingleWinner` 断言同步收紧为恰好 `1` 个成功、其余全部 `ErrLockHeld`（旧界限恰好容忍了该缺陷，测试此前常绿、本次调度才暴露）。
- **PostgreSQL 并发建表目录竞态（integration&postgres 腿红灯，生产多节点同样暴露）**：`CREATE TABLE IF NOT EXISTS` 的存在性检查与系统目录插入非原子——多个连接并发创建同名表会在 `pg_type_typname_nsp_index` 唯一索引上相撞，输家事务整体以 `duplicate key value` 中止。CI 里 state 与 cluster 两个测试二进制并发创建 `cluster_nodes` 触发（state 的 pgschema.sql 与 cluster 的自建 DDL 都含该表）；生产上多节点同时对同一库首次启动同样会失败。`pgMigrate`（internal/state）与 `ensureClusterSchema`（internal/cluster）改在**专用连接**（`db.Conn`）上以共享 key 的 `pg_advisory_lock` 串行化 DDL——DDL 全程持有同一连接，完成经 `context.WithoutCancel` 解锁、`conn.Close()` 作为兜底释放（即使 DSN 配 `MaxOpenConns=1` 也不会自锁死）；`dbExecutor` 接口加 `QueryRowContext` 以同时接受 `*sql.DB`/`*sql.Tx`/`*sql.Conn`。新增回归 `TestClusterPGConcurrentEnsureSchemaSerialisesDDL`（6 goroutine 并发建 schema）。
- **运行镜像基底快照自带 HIGH CVE（trivy 门禁拦截）**：`alpine:3.22`（3.22.5）基底快照携带 openssl `libcrypto3`/`libssl3` 3.5.7-r0，CVE-2026-14456（HIGH，QUIC 服务器无界内存增长 DoS）在 3.5.8-r0 已修复——基底镜像发布后不再滚动更新补丁，`--ignore-unfixed` 下照样命中门禁。运行时阶段构建时 `apk upgrade --no-cache` 全量升到仓库当前发布版（本地实测 3.5.7-r0→3.5.8-r0），此后任何基底包已修复 CVE 不再需要改 Dockerfile。

### 测试

- **CLI 命令族 e2e（E-2）**：新增 12 个测试文件覆盖 drift / calendar / target / secret / pause / retry / audit / system / push / plugin / chatops / 共享 helper——每条命令走真实 `rootCmd.Execute()`，SQLite 临时库 + `--config` 隔离（push 族此前会写入真实用户目录，一并修复为隔离夹具）。harness 以"复位全部选项变量 + 遍历命令树清 pflag `Changed`"模拟 fresh process，消除 cobra 进程内全局状态跨用例泄漏（曾致 no-flag `drift detect` 误检、calendar 局部更新误报必填）。`cmd/levee`（剔除 serve）行覆盖率 45.9% → **66.8%**（质量方案目标 ≥60% 达成；最差的 `cmd_drift.go` 33.7% → 78.9%）。audit 族含 WORM 端到端篡改检测（临时库 DROP 触发器后裸 UPDATE 篡改 detail，`verify` 报 tampered / exit=6）。
- **前端单元测试基线（E-3）**：`web/` 引入 vitest + jsdom（38 用例全绿）——`api/client`（token 三态存取、Bearer 请求拦截、`AxiosError → ApiError` 归一化的 401/5xx/网络/预请求四类路径、401 清 token 与统一文案、403 不清 token，传输层在 axios adapter 处替换，拦截器与 baseURL 走真实逻辑）；`api/sso`（OIDC PKCE 与 GitHub 两条登录流的 state/verifier 持久化、CSRF state 校验、令牌交换请求体、JWT access_token 优先 / id_token 回退的选牌逻辑、登录后一次性凭据清理、开放重定向防护）；`utils/format`（时间戳/时长/运行时长格式化与边界、状态/优先级标签色表一致性）。jsdom 的 `window.location` 不可伪造（unforgeable），401 重定向与 SSO 跳转以可观测副作用（token 清除、storage 簿记）断言并在配置中定点 origin、过滤 jsdom 导航噪音；Node ≥25 原生 webstorage 全局遮蔽 jsdom Storage，`vitest.setup.ts` 以内存实现顶替。CI `frontend` 作业纳入 `npm run test`（vitest → vue-tsc → vite build 三连）。

- **集成套件语义对账（tests/integration）**：b12eafc 改变 apply 语义（有引擎同步完成至 `completed`、无引擎 `FailedPrecondition` 拒绝）后，lifecycle 套件的旧断言仍停留在"apply 后 running、可 pause"时代。重写：生命周期主用例改为 创建→计划→批准→apply→`completed`（run_id 非空、审计哈希链有效），终态 `CancelChange` 断言拒绝（`FailedPrecondition`）且状态不变；新增回归 `TestApplyChange_NoEngineRefused`（无引擎 apply 拒绝且状态停留 `approved`）；pause/resume 自终态非法、跨服务审计计数、哈希链完整性用例同步对账。
- **diagnosis 窗口用例 Linux 抖动修复**：`TestCollect_InvalidWindow` 原以秒级截断构造 `Start > End`，Linux 纳秒精度单调钟下不总成立（CI 偶发假阴/假阳）；改为同一时间戳显式构造非法窗口。
- **权限矩阵并发一致性用例改确定性启动同步（windows 腿红灯修复）**：`TestConcurrent_GrantThenReadConsistency` 断言"读者在写者并发期间至少观测到 1 次授权"，但该重叠纯靠调度碰运气——windows runner 上读者 1 万次扫描可全部跑完而写者一次 Grant 都未开始（`observed==0` 假红）。改为启动同步：写者首笔 Grant 后关闭信号通道、读者等通道再扫描；通道关闭顺带建立 happens-before，断言从概率性变确定性（其余 49+1 遍扫描仍与写者真并发）。
- **file 模块绝对路径拒绝用例按平台拆分（linux/macOS 腿红灯修复）**：`TestResolveLocalSrcAbsoluteRejected` 用 `C:\Windows\...` 作第二个绝对路径样本，但 Unix 上盘符路径是普通相对文件名（策略上合法），断言必然假红。改为按 `runtime.GOOS` 分发：Windows 验盘符绝对路径，Unix 验经 `filepath.Clean` 的 `..` 存活的根路径形态（两平台都保有双重样本）。
- **集群 leader 可见性断言改确定性收敛（windows/PG 腿红灯修复）**：`TestClusterPGTwoNodeVisibility` 在"发现对端 active"的 Eventually 之后立即裸读 `GetLeader`，但各 manager 的内存视图要等下一轮心跳才刷新——node-b 侧 leader 可能尚未收敛，断言纯赌时序。改为 `require.Eventually` 等双侧都产出 leader 再断言 ID 一致。
- **trivy 作业可见性重构（CI）**：门禁步 `format: sarif` + `exit-code: 1` 失败时 stdout 零输出，sarif 只被其后的上传步消费——而上传步没有 `if: always()`，门禁一红即被跳过，红灯完全盲视（上一轮 trivy 失败只能看到 exit 1）。门禁前新增"scan (print findings)"步（`format: table`、CRITICAL/HIGH、`exit-code: 0`）先打印明细，两个上传步改 `if: always() && hashFiles('trivy-results.sarif') != ''`。
- **trivy 门禁语义修正 + Code Scanning 权限补齐（CI，可见性重构揭出的两层潜伏问题）**：① trivy-action 在 `format: sarif` 下强制按全等级扫描（其日志自述 "Building SARIF report with all severities"，`severity` 输入不参与退出码判定），门禁步 `exit-code: 1` 实际会把可修复的 MEDIUM/LOW 一并拦红（x/crypto v0.55.0 的两条中低危即触发）——与门禁声明的"仅拦可修复 CRITICAL/HIGH"语义不符。sarif 步改 `exit-code: 0` + `limit-severities-for-sarif: true`（报告仍按 CRITICAL/HIGH 过滤），门禁移到两个上传步之后：显式 `jq` 统计过滤后 sarif 的 `runs[].results` 计数并打印 CVE 清单，红灯运行不再牺牲报告与 Code Scanning 告警。② `upload-sarif` 步历史上从未真正执行过（门禁先红即被跳过），`if: always()` 补上后立刻暴露 trivy 作业缺 `security-events: write`（`Resource not accessible by integration`），作业级 `permissions` 补齐。
- **check 聚合作业恒红修复（CI，首次全绿运行暴露）**：聚合脚本循环体里的 `${{ needs[job].result }}` 从未生效——Actions 表达式引擎在 bash 启动前渲染 `${{ }}`，引擎只见到字面 `job`（bash 循环变量对其不可见），查无此 needs 条目渲染为空串，`[[ "" != "success" ]]` 首个迭代必炸；此前从未暴露是因为运行总在叶子作业就红了，第 6 轮 16 个叶子全绿才把聚合作业自身打到红灯。改为把各 needs 结果以字面 `name:value` 对传入 bash 逐项检查，顺带把"遇首个失败即 exit"改为汇总报告所有未通过作业（错误信息携带具体 result）。首版修复还踩中次生坑：`run:` 块标量里的 YAML 注释是字符串内容而非注释，注释文字里的字面 `${{ }}` 被当作（非法）表达式解析，workflow 编译失败、零作业即红——注释改写为不含 `${{` 字面序列后消除。
- **KMS 回退测试夹具吞错超时修复（macOS race 腿红灯定位）**：`storeInFallbackForTest` 向本地回退库写种子凭据时以 5s 超时 ctx 调用 `Store` 且错误被 `_, _ =` 丢弃——满载共享 runner 的 `-race` 腿下 argon2id KDF 主导的 `Store` 实测可达 ~9s（同 run 日志相邻两条 `credential stored` 间隔即 8-9s），超时静默过期后测试以 `credential: not found` 假象收场（子测试 5.54s ≈ 5s 超时 + 探测开销），纯时序赌博、此前绿全靠运气。夹具改为 60s 超时且 `require.NoError` 大声失败（存储失败直接指向 Store 而非误导性的 GetCredential not found），并入唯一调用方、删除中间层；同子测试里一段从未生效的死脚手架（5 字节 `pt` 与被 `_ = err` 吞掉的首次调用）一并清除。

### 前端

- **401 统一处理（UX）**：`web/src/api/client.ts` 响应拦截器对 401 统一清除本地 token 并跳转 `/login`（登录/回调页豁免防死循环；并发 401 只触发一次重定向），错误文案统一为「登录已过期，请重新登录」。`internal/web/dist` 同步重新构建。

### 文档

- 修正 `internal/grpc/rest.go` 中 `SetExtraRoute` 鉴权语义的矛盾注释（实际行为：配置 token 时挂载端点默认要求 Bearer 鉴权，CORS/限流不适用）。
- 补注 `closure.go` 回滚路径刻意使用 `context.Background()` 的意图（回滚触发即不可取消，依赖逐步执行超时控制运行时长）。
- 修正 `.golangci.yml` 中 `misspell` 启用的过时注释（原文误标 disabled）。

### 安全修复

- **x/crypto 升级 v0.56.0（连带 go1.26 工具链升代）**：收口 trivy 台账上最后两条依赖类遗留（CVE-2026-56855 MEDIUM、CVE-2026-78662 LOW）。v0.56.0 声明 go 1.26.0，故模块 go 指令 1.25.0→1.26.0，`ci.yml`（7 处 setup-go）、`release.yml`、`Dockerfile` `GO_VERSION`、README/quickstart 同步升代。go1.26 弃用 `httputil.ReverseProxy.Director`（staticcheck SA1019 即拦），`internal/web` API 代理迁移到等价的 `Rewrite` 形态（`SetURL`+`SetXForwarded`+固定出站 Host），`TestServer_ApiProxy` 断言收紧为锁定"完整路径透传 + Host 钉到上游"两条行为契约。
- **REST 方法校验（P1）**：`/changes/{id}/{plan,apply,approve,reject,pause,resume,cancel,retry,rollback,archive}` 现强制 `POST`、`/changes/{id}/{logs,trace}` 强制 `GET`；此前任意 HTTP 方法（含 GET）即可触发状态变更，爬虫/预取可误暂停变更。`pause/resume/archive` 不再吞掉请求体解析错误：空体合法、畸形 JSON 返回 400。
- **SSH BecomeUser 注入（P2）**：`buildExecCommand` 现对 `become_user` 也做 POSIX shell 引用（此前仅引用命令本体），阻断来自配置值的 `sudo -u` 注入面。
- **/metrics 默认鉴权（P2）**：网关 `SetExtraRoute` 挂载的运维端点（`/metrics`）在配置了任一 token 时默认要求 Bearer 鉴权；新增 `--metrics-public` / `ServeGatewayConfig.MetricsPublic` 显式放开（供无法携带凭据的采集器）。
- **凭据 blob 版本化（SA-005/SA-015）**：加密 blob 添加版本前缀 + 自描述 KDF 参数——存量密文按写入时的参数解密、新写入用当前参数。收口 v1.11.0 argon2id 提参至 194MiB 造成的存量密文断代（v1.11/12 为 194MiB 代、v1.10 前为 64MiB 代），并为未来算法/参数迁移留出演进路径。
- **随机 ID 生成失败统一硬失败（SA-012）**：身份类标识（凭据 ID、deeplink 一次性授权 token、审批/运行/锁/快照/租户/通知/计划/盘点导入等）在 `crypto/rand` 失败时一律返回错误并逐点传播，deeplink 不再降级为可猜测的 `fallback-<nano>` 时间戳；纯观测类 ID（请求关联标签、临时文件名、展示标签）保留降级并以注释标注分类；`change_service` 的 panic+recovery 策略经评审豁免。
- **审计脱敏增强（SA-009/SA-010）**：内置敏感词表 8→16（新增 passphrase/auth_code/refresh_token/access_token/ssh_key/cert/certificate/connection_string）；匹配升级为 `_`/`-` 词边界后缀命中（`db_password` 命中、`sort_key` 不误伤、裸 `key` 仅全等匹配）；新增 `security.sensitive_fields` 自定义词表（进程级注册、热路径无锁）；脱敏支持结构体/嵌入/切片/指针的 reflect 遍历（可见性口径 = `json.Marshal` 可见字段，无命中子树原值透传零漂移）。已文档化边界：值内嵌明文（如 error 文本中的 `secret=…`）无键上下文，键名规则不可覆盖。
- **权限矩阵加固（SA-013/SA-016）**：`Grant`/`Revoke` 未知 action 默认 WARN 后仍记录（兼容存量拼写），新增 `StrictActions` 严格模式整批拒绝且装载原子生效；`admin` + 环境通配 `*` 授予组合在装载完成后汇总 WARN 一次并列出受影响团队。
- **权限拒绝审计接线（SA-007，部分修复）**：CLI 全局暂停/恢复的权限拒绝现落审计表一行（`permission.denied`，含 Actor 与权限串；选择 audit 表因 CLI 拒绝发生在选定任何 run 之前）。剩余边界已文档化：serve 路径不构造 PermissionMatrix（`LoadFrom*` 仅 cmd_user/cmd_rbac 调用），gRPC 侧权限检查接线待生产装配。
- **WORM 重复写入错误语义收口（SA-014，台账降档 LOW）**：`state.CreateTrace` 将驱动 UNIQUE 约束错误映射为 `ErrTraceExists` 哨兵（SQLite 按约束错误码集合判定、不误分类 FK 违反；PG 按 SQLSTATE 23505），`WORMStore.Append` 转译为 `ErrAlreadyExists`——TOCTOU 竞态输家不再收到不透明 SQL 错误；双后端并发双写约束测试固化"恰好一方成功"。
- **凭据 Tags 持久化（SA-018）**：新增 `credentials.tags` 列（schema v2 迁移步，SQLite/PG 形状一致，v1→v2 升级以手工构建的旧库文件实测），以 JSON map 存储；无 tag 行以 `''` 存储与历史行不可区分；`Rotate`/`RotateMasterPassword` 保留 tags。
- **SQLite 持久级别可配置（SA-019）**：新增 `state.sqlite_synchronous = normal|full`（默认 `normal`；`full` 每提交 fsync，供审计强持久场景，写放大约 1-2%；非法值拒绝启动），环境变量 `LEVEE_STATE_SQLITE_SYNCHRONOUS` 同步可用。
- **RetrieveInto 回调式凭据读取（SA-011，部分修复）**：`CredentialStore.RetrieveInto` 在回调返回（含 panic 展开）后必然 `SecureZero` 明文；`Retrieve` 文档清零义务升为 MUST 并推荐新代码走 RetrieveInto。已知残留（代码注释标注、非本轮修复）：serve 凭据解析器裸密码路径经 `string` 返回（`CredentialRef.Password` 为 string 类型不可清零，类型改造波及 ssh/winrm/grpc 通道）。

### 新增

- **GitHub OAuth 单点登录（可选）**：GitHub 不是 OIDC 提供方（access token 不透明、token 端点无浏览器 CORS），故 code 交换在服务端完成——浏览器 POST `{"code","state"}` 到公开端点 `/auth/github`，网关持 client_secret 与 GitHub 换取 token、调 `/user`+`/user/teams` 完成身份/成员校验后**丢弃 GitHub token**，签发 LEVEE 本地会话令牌（HS256 HMAC、12h TTL、`internal/auth/session.go`）返回浏览器；后续请求走本地会话令牌验证，**不逐请求访问 api.github.com**（无限流/可用性耦合），client_secret 全程不出进程。凭据解析升为三源：静态（constant-time）→ LEVEE 会话令牌 → OIDC JWT，gRPC 与 REST 一致；审计主体 = GitHub login（注入 actorKey，覆盖 X-Acting-As），`team_role_map`（`"org/team-slug"` 键）映射团队为角色，`org` 限制仅组织成员可登录（内部部署必须设置）。`session_secret` 为空时进程内随机密钥（重启失效、多节点不共享，启动时告警；多节点须配 ≥32 字符共享密钥）。配置 `auth.github` 段（`LEVEE_AUTH_GITHUB_*` 同步可用；`team_role_map` 仅文件）。安全门同步扩展：无任何凭据源（含 GitHub）仍拒绝启动。前端：登录页"通过 GitHub 登录"按钮（auth-info 探测），`/login/callback` 按提供方分发（GitHub → POST 网关交换；OIDC → 浏览器直连 IdP），state 防 CSRF 两种流程共用。
- **OIDC 单点登录（可选）**：`serve` 支持 OpenID Connect JWT 作为静态 Bearer 令牌之外的附加凭据源，与 IdP 无关（Keycloak / Zitadel / Entra ID / Okta 等标准提供方均可）。新增 `internal/auth` 包（`coreos/go-oidc/v3`）：启动时对 `auth.oidc.issuer_url` 做发现（10s 超时，失败拒绝启动——fail-fast，防止误配置导致令牌永远验不过却无告警），验证签名（JWKS）、issuer、audience、expiry；主体取 `username_claim`（默认 `preferred_username`，回退 `email`、`sub`），`role_claim` 声明经 `role_map` 映射后注入上下文（v1 仅审计/预留，无授权消费）。凭据解析顺序：静态令牌（constant-time）→ JWT 形态令牌走 OIDC 验证；启用 OIDC 不影响既有静态令牌，gRPC 拦截器与 REST 中间件行为一致。配置经 `auth.oidc` 段（`LEVEE_AUTH_OIDC_*` 环境变量同步可用，`role_map` 仅文件可配）。安全门同步扩展：无 `--token`、无 `--auth-token` 且未启用 OIDC 时仍拒绝启动。前端：登录页经公开描述符 `GET /system/auth-info`（免 Bearer）探测 SSO，展示“通过 SSO 登录”按钮，走 authorization code + PKCE（公共客户端，Web Crypto 生成 verifier/challenge，state 防 CSRF）；新增 `/login/callback` 在浏览器直接与 IdP token 端点交换令牌（要求 IdP 允许本站点 CORS），存 access_token（JWT 形态）或回退 id_token；静态令牌登录路径完全保留。
- **多令牌身份认证**：`serve` 新增可重复 `--auth-token name=secret`，每个命名令牌映射到一个主体（subject）；gRPC 拦截器与 REST 中间件均支持 `AuthTokens`（`Legacy` + `Named`）。命名令牌认证后，其主体注入请求上下文并**优先于**客户端自报的 `X-Acting-As`，使审计归属为“被证明的身份”而非“断言”。单令牌（`--token`/`LEVEE_TOKEN`）行为完全向后兼容。
- **serve 装配 AI 引擎**：`levee serve` 现装配真实的诊断引擎（日志管线 + 健康探针，本地执行器）与对话引擎（内置推荐引擎），`Diagnose`/`SendMessage` RPC 不再返回 `Unimplemented`；告警服务保持独立环形存储（完整网关仍用 `levee alert serve`）。
- **前端 CI 门禁**：新增 `frontend` 作业（`vue-tsc --noEmit` + `vite build`），并纳入 `check` 聚合门禁。
- **发布工作流**：新增 `.github/workflows/release.yml`——推送 `v*` tag 时自动触发 goreleaser，按既有 `.goreleaser.yml` 构建 linux/darwin/windows × amd64/arm64 产物并发布 Release；此前仅有 goreleaser 配置、无触发工作流，发布依赖手工构建。
- **集群成员持久化与租约锁**：集群模式（`serve --cluster`）新增两项协调能力。其一，持久化节点注册：节点写入 `cluster_nodes` 表并周期心跳（健康循环每 10s 刷新、30s 未心跳被标记 offline），各节点本地注册表从共享表收敛，leader 按确定性策略（active master 最小 ID，缺则 active worker 最小 ID）独立收敛——此前节点注册仅为进程内，节点之间互不可见。其二，租约式分布式锁（`cluster_locks` 表）取代原 PG advisory lock：锁带租约过期时间，持有者周期续租，持有者崩溃/失联后租约到期即可以被其他节点自动抢占（advisory lock 永不失效，挂死但连接存活的持有者会永久阻塞锁键）；每次获取/抢占携带单调递增的 fence token 供接管隔离。两张表由 cluster 包首次使用时自建（幂等 DDL），不依赖 state 包 schema 版本。在途变更的自动故障转移与跨节点调度仍未实现，启动告警文案同步更新。
- **protobuf 契约补源与双轨再生**：新增 `proto/levee_extra.proto`，为手写的 AlertService / DiagnosisService / ConversationService / InventoryService 四个服务补齐 .proto 定义（共 24 个消息，字段编号与既有手写 pb 的 struct tag 逐一比对对齐）；原内嵌于 `proto/levee.proto` 尾部的 extra 定义拆分至该文件（主文件仅保留核心域消息，尾部留有指引注释）。新增 `scripts/gen-proto.sh`（锁定 protoc v27.0 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2，与已检入生成代码的版本头一致）。手写文件（`levee_extra*.pb.go`、`inventory_extra*.pb.go`）标注 `//go:build !proto_regenerate`，默认构建行为不变；protoc 再生输出以 `levee_extra*.regen.pb.go` 落盘并标注 `//go:build proto_regenerate`，作为可再生的验证轨道（两轨字段/服务契约已逐字段比对一致，双轨全量测试均通过）。新增 CI `proto` 作业：用锁定版本工具链再生后与检入文件做字节级漂移检查（git diff），并在 proto_regenerate 轨道执行 build + 全量测试；已纳入 `check` 聚合门禁。

### 变更

- **CI 工具链与口径对齐**：`.gitattributes` 统一行尾；CI golangci-lint 升至 v2.13 与本地同配置（本轮清零存量 34 处告警）；CI 覆盖率统计剔除生成代码。E 轮测试补齐后 **CI 覆盖率地板 60% → 70%** 落地（state sqlite+PG 联合口径 ≥75%、CLI 剔 serve ≥60%、web vitest 纳入 CI）。
- **CI 行动项修复（master 转绿）**：地板抬升触发的那次 CI 运行暴露 8 个红作业——其中 lint、trivy、gosec 等为存量债务（master 自 2026-08-28 前即持续红色，与本轮提交无关），逐项修复：`golangci-lint-action` v6→v7（v6 无法解析 v2.x 工具版本串）；`trivy-action` v0.30.0→v0.36.0（v0.30.0 的组合实现按已被上游删除的 `setup-trivy@v0.2.2` tag 引用，v0.36.0 已改为 commit SHA 钉定）；Windows 测试步骤给 `-coverprofile=coverage.out` 加引号（pwsh 默认 shell 会把未加引号的路径拆成 `-coverprofile=coverage` + 裸包名 `.out` 导致 setup 失败）；其余为上文两个 `-race` 生产竞态、diagnosis 窗口用例与集成套件语义对账。
- **gosec 全量分诊（60 → 0）**：默认流水线口径（剔 pb）下 60 条发现逐条处置——28 处权限收紧（目录 0o755→0o750、文件 0o644→0o600，快照/备份/盘点库/模板库/插件沙箱等落盘路径）；32 处附逐条理由的 `#nosec` 标注（每条先读代码核实：如快照恢复的 Walk 遍历的是操作员自有存储目录、calendar 动态 WHERE 全部为 `?` 占位参数拼接、SSH 弱主机密钥校验是配置显式 opt-in 且严格模式永不降级、`system config set` 写的是操作员 `--config` 自指路径且无服务端可达路径）；G304（40 条，"按变量路径加载配置/模板/插件/基线"即产品形态）与 G115（17 条，protobuf 分页/计数的有界 int↔int32 转换）两类经全量人工审读后在 CI 参数整体排除并注释理由。此后 gosec 新增任何一条发现都是净新信号。
- **Trivy 阻断合并**：`trivy` 作业对可修复的 CRITICAL/HIGH CVE 设 `exit-code: 1`，由“仅上报”改为“阻断”。
- **集群模式如实标注**：`serve --cluster` 启动时输出告警，说明当前集群协同仅限共享 PostgreSQL 存储（数据一致性 + 咨询锁），节点注册为进程内、尚无自动故障转移/跨节点调度；README 特性描述同步收敛。
- **前端产物清理**：`internal/web/dist` 重新构建，移除历史遗留的多代哈希资产，仅保留当前一代。
- **手写服务 pb 全量切换为生成代码，REST 序列化统一 protojson**：AlertService / DiagnosisService / ConversationService / InventoryService 的 pb 绑定现为 protoc 生成代码——手写文件与 `proto_regenerate` build tag 双轨脚手架移除（上一条目中的 `levee_extra*.regen.pb.go` 提升为正式文件），`scripts/gen-proto.sh` 直接产出全部四个绑定文件，CI `proto` 作业相应简化为工具链再生 + 字节级漂移检查 + 构建。`/api/v1/{AlertService,DiagnosisService,ConversationService}` 五个非流式端点由手写反射镜像切换为 protojson（`rest.go` 移除 `readProtoJSON`/`writeProtoJSON` 及约 270 行 protoLike* 镜像机制），与其他全部端点统一遵循 13.5 约定。**客户端可见变化**：这五个端点的响应中，proto3 标量零值不再显式输出（此前 `0`/`""`/`false` 会出现，现按规范省略），int64 由 JSON 数字改为 JSON 字符串；请求体仍同时接受 camelCase 与 snake_case 拼写。仓库前端未消费这五个端点（已全量检索确认），新增契约测试 `TestExtraServicesProtoJSONContract` 固化零值省略 / int64 字符串 / 双拼写兼容三项行为。

### 文档

- README 版本号由过时的 v1.10.0 更正为 v1.12.0。
- 补充 v1.5.0 跳号说明（该号未发布）。
- 修正 `.gitignore` 中 `.env"coverfunc.txt"` 的粘连/引号错误。
- 新增生产部署与升级手册（docs/deployment.md）。
- 补全 CLI 参考缺失命令（alert/backup/restore/group/converse/diagnose 等）。
- `docs/levee-api.md` 新增 13.5 节，声明 REST JSON 字段约定：protojson 序列化采用 lowerCamelCase 键名、**省略 proto3 零值字段**（客户端须将缺失字段视为零值）、int64 输出为 JSON 字符串；并如实标注 AlertService/DiagnosisService/ConversationService 五个端点的手写镜像序列化差异（标量零值显式输出、int64 为数字）。同步修正 13.1 与实现不符的描述：REST 成功响应为 proto 消息直接序列化（无 `data`/`meta`/`error` 包装），HTTP 状态码映射改按 `writeGRPCError` 实际实现（移除不存在的 422，补 412/429/501）。

### 修复

- **Target.status 序列化丢失**：`pb.Target` 的 `Status` 字段（v1.11.0 引入，af62981）当时以手工编辑方式加入生成文件 `internal/grpc/pb/levee.pb.go` 的 Go 结构体，未同步更新文件内嵌的 protobuf 描述符（rawDesc）。protobuf-go 的序列化完全由描述符驱动，该字段对 protojson 与二进制 wire 编码均不可见——“REST/gRPC 暴露库存生命周期状态”实际静默失效：字段在进程内可读写，但 REST 响应与 gRPC 传输永远丢弃它（实测 wire 字节与 protojson 输出均无 field 8）。本次以锁定工具链从当前 `proto/levee.proto` 再生该文件（与手工版本的差异仅为 field 8 的描述符字节与一行注释），`status` 现按 proto3 语义正常序列化（非空时输出，空值省略）。
- **移动一键审批 401**：`/changes/deeplink/approve` 与 `/changes/deeplink/reject` 现接受请求体内的一次性 token 作为认证凭据，不再要求 Bearer 头——此前启用鉴权后，无法携带 Bearer 的移动设备点击审批深链必然 401。一次性 token 为 32 字节随机数、30 分钟 TTL、单次消费并绑定 (run-id, 用户, 动作)；无效/过期 token 现返回 401（此前被不透明地映射为 500）。豁免仅覆盖这两个端点，其余端点的 Bearer 校验不受影响。
- **告警订阅流测试竞态**：`SubscribeAlerts` 广播为尽力而为（best-effort），订阅注册完成前发布的告警会被丢弃；相关流式测试此前以固定 `time.Sleep` 等待注册，在高负载下（如完整 `go test ./...`）可能竞态导致 `RecvMsg` 永久阻塞（触发 10 分钟单测超时）。新增 `AlertService.SubscriberCount()` 观测方法，全部 5 个订阅流式测试改为 `require.Eventually` 等待注册完成后再发布告警，消除挂起。

## v1.12.0 - 2026-08-27

### Added

- **Self-metrics endpoint** (`internal/metrics/`): lightweight atomic-counter collector exposing 10 metric families (change lifecycle, batch duration, gates, approvals, channel acquisition, locks, rollbacks, backups, alerts) in Prometheus text 0.0.4 format; mounted at `/metrics` on the REST gateway; coverage 98.1%, 19 tests
- **Data backup/restore** (`internal/backup/` + `levee backup` / `levee restore` CLI): SQLite backups via `VACUUM INTO` + `integrity_check` + `.sha256` checksum; pure-Go PostgreSQL SQL dump via pgx (no `pg_dump` dependency); restore performs an automatic `.pre-restore` safety backup, atomic replacement, and stale-WAL cleanup; flags: `backup [--output] [--verify-only] [--pg-dsn]`, `restore --input <path> [--yes]`; coverage 86.8%, 60 tests
- **OpenTelemetry tracing** (`internal/tracing/`): Tracer interface + stdouttrace exporter + W3C `traceparent` parse/format utilities; initialized on `serve` startup with graceful noop fallback on failure; new `tracing` config section (`enabled` / `exporter` / `endpoint`, disabled by default); OTel v1.44.0; coverage 95.3%, 17 tests
- **Dependabot** (`.github/dependabot.yml`): scheduled dependency updates for gomod / github-actions / docker ecosystems
- **CodeQL** (`.github/workflows/codeql.yml`): security analysis on push / pull request / weekly schedule
- **`config.example.yaml`**: complete example of all 52 config keys, cross-checked one-by-one against `internal/config/config.go` (zero fabricated keys), with default values and scenario examples
- **`CODE_OF_CONDUCT.md`**: Contributor Covenant 2.1 full text

### Changed

- `internal/grpc/rest.go`: added `SetExtraRoute` generic extension point (~30 lines) so external handlers can be mounted on the gateway mux (used for `/metrics`); no impact on existing behavior

详细说明见 [docs/release-notes/v1.12.0.md](docs/release-notes/v1.12.0.md)。

## v1.11.0 - 2026-08-27

### 安全加固

- **P0 网关接线修复**：`levee serve` 此前会启动未挂载任何服务的 REST 网关；现改为启动配置的服务实例。`/healthz` 在服务注册完成前返回 503 `{"status":"unavailable"}`（服务随 serve 自动注册，正常运行时为 200）
- user 模块命令行参数统一引号转义，防止参数注入
- WinRM 通道 PowerShell 路径转义修复
- SSH 主机密钥校验默认开启（strict-by-default，known_hosts 路径与豁免开关可配）
- 审批 / 归档状态机守卫：拒绝非法状态迁移并留痕
- WORM 级联删除防护：SQLite 开启 `PRAGMA recursive_triggers=ON`，外键 `ON DELETE CASCADE` 触发的删除同样命中审计保护触发器
- [SA-004] SecureZero 添加 runtime.KeepAlive 防止编译器优化
- [SA-005] argon2id memory cost 提升至 194MiB（OWASP 2024 推荐）
- [SA-006] 权限矩阵添加 sync.RWMutex 保证线程安全
- [SA-007] 权限校验拒绝时自动记录审计 trace
- [SA-008] 哈希链排序添加二级排序键确保确定性

> **勘误（2026-09-06 追加，不改写上文历史行文）**：上文 SA-007 条目与当时实际落地不符——v1.11.0 交付的是拒绝审计**机制**（recorder 注入点），生产路径（CLI/serve）并未接线，权限拒绝不会自动落审计记录，"自动记录审计 trace"属过度声明（核查证据见 `docs/security-audit.md` "2026-09-06 核查记录"）。实际接线（CLI 全局暂停/恢复拒绝落审计表）于 Unreleased 落地。

### Bug 修复

- macOS/BSD 插件沙箱构建修复（sandbox unix 构建约束整理）
- 列表分页 `offset` 参数生效（SQLite / PostgreSQL 存储层）

### Web UI

- 前后端 API 契约对齐；新增登录页

### 文档

- CLI 文档与实现对齐：`new <template> --params k=v`（移除虚构的 `--file/--template/--dry-run/--label/--priority`）、删除不存在的 `levee plan --dry-run` 步骤、push config / tenant create（配额默认 0 = 不限，存储单位 MB，补 `--max-api-rate`）/ drift schedule add（去 `--baseline`，补 `--alert/--enabled`）/ drift report（无 `--format`）/ agent start（默认值修正，补 `--max-concurrent`）/ serve 补 `--http-addr`

### 工程修复

- vet/gofmt 清理：`cmd_serve.go` IPv6 安全的 host:port 拼接（`net.JoinHostPort`），3 个文件格式对齐
- 添加 Apache-2.0 LICENSE 文件

## v1.10.0 - 2026-08-23

### 安全加固（审计修复）

#### 认证与访问控制
- **auth 启动门禁**：`levee serve` 无 token 时拒绝启动，除非显式传 `--insecure`；token 可经 `--token` 或 `LEVEE_TOKEN` 环境变量提供
- **CORS 默认拒绝**：origin 列表为空不再隐含通配，需显式配置 `"*"`
- Bearer token 校验改用 `crypto/subtle.ConstantTimeCompare`
- 无 TLS 启动时输出明文传输警告日志

#### 凭据处理
- user 模块密码不再出现在命令行参数中：凭据以临时文件上传后 `chpasswd < file` 消费，避免泄露到 argv / SSH 日志

#### Linux 插件沙箱
- 基于 cgroup v2 的硬性资源限制：`memory.max` + `cpu.max`
- 插件进程挂入独立 `levee-plugin-{pid}` cgroup
- cgroup 不可用时优雅降级（保留墙钟超时兜底）
- 新增 `sandbox_linux_test.go` 验证测试（无 cgroup 写权限时跳过）

### REST 网关

- RESTful 路由完善：`/api/v1/changes/{id}` 等 `/:id` 路径正确解析（修复前导斜杠导致的 400）
- HTTP 端点 token 认证中间件
- 全局令牌桶限流：`--rate-limit` / `--rate-burst`，429 + Retry-After
- 请求 ID 追踪：`X-Request-Id` 响应头 + gRPC metadata 透传
- 移动审批 deeplink 端点
- 注册标准 `grpc.health.v1` 健康服务（Start/Stop 联动 SERVING/NOT_SERVING）

### Bug 修复

- GetLogs 现在生效 `levels` 过滤（stdout→INFO，stderr→ERROR）
- GetTrace 显式 `run_id` 优先于 change 级默认值
- ArchiveChange purge 对 WORM trace 的保留行为改为显式设计并文档化
- REST 网关限流与请求 ID 中间件

### CI/CD

- 新增 gosec 静态安全扫描 job（JSON 报告上传为 workflow artifact `gosec-report`）
- 新增 trivy 容器镜像扫描（SARIF 上传 GitHub Code Scanning；CRITICAL/HIGH 阻断）
- `check` 聚合门禁 job 覆盖 vet/lint/test/build/gosec/trivy

### 测试覆盖率

| 包 | 之前 | 之后 |
|---|---|---|
| internal/grpc/ | ~39% | 80.5% |
| internal/state/ | 34.9% | 55.5% |
| internal/tenant/ | — | 90.5% |

### 已知限制

- 多租户隔离在 store 层有契约测试，但 daemon 主路径未接线（MVP 范围决策，V2 再评估）
- cgroup 沙箱要求 `/sys/fs/cgroup` 可写（root 或授权容器）

详细说明见 [docs/release-notes/v1.10.0.md](docs/release-notes/v1.10.0.md)。

## v1.9.0 - 2026-08-18

### Phase D: 高级诊断

#### D1: SkyWalking/Pinpoint 拓扑分析
- 新增 `internal/diagnosis/topology/` 包
- 统一 `Collector` 接口 + `Topology`/`Node`/`Edge` 类型
- SkyWalking GraphQL API 客户端
- Pinpoint REST API 客户端
- 覆盖率 93.0%

#### D2: Zabbix/Nagios 告警适配器
- 新增 `internal/alert/adapter_zabbix.go` + `adapter_nagios.go`
- Zabbix webhook JSON payload 解析（支持单对象和数组）
- Nagios HTTP webhook JSON payload 解析（支持单对象和数组）
- 哨兵错误 `ErrInvalidPayload` / `ErrMissingField`
- 覆盖率 93.2%

#### D3: LLM 对话式诊断
- 新增 `internal/diagnosis/llm_diag/` 包
- 多轮推理引擎 `ReasoningEngine`
- 推理上下文 `ReasoningContext` + `Turn` + `ReasoningStatus`
- 收敛检测 + 最大轮次控制
- 覆盖率 95.0%

#### D4: RAG 知识库增强
- 新增 `internal/recommend/rag/` 包
- `EmbeddingProvider` 接口 + `MockEmbeddingProvider`（FNV-1a 哈希确定性嵌入）
- `VectorStore` 接口 + `InMemoryVectorStore`（余弦相似度，线程安全）
- `Retriever` + `AugmentPrompt` RAG pipeline
- 覆盖率 94.4%

#### D5: 修复效果学习
- 新增 `internal/recommend/feedback/` 包
- `FeedbackLearner`：Record / Learn / RecordAndLearn / GetStats
- 反馈循环：成功→创建新 FixPattern + HistoricalIncident 添加到 KB
- 线程安全（sync.RWMutex）
- 覆盖率 93.5%

## [1.8.0] - 2026-08-18

### Added — Phase C: 自动执行 + OpsMesh 集成

- **C1 AutoPlanner** (`internal/autoplanner/`): 自动任务拆分引擎，将 AI 推荐（Recommendation）转换为可执行的 LEVEELang workflow，包含风险评估和批次划分
  - `AutoPlanner.Plan()` — Recommendation → Workflow 转换
  - `RiskAssessor.Assess()` — 风险→审批级别映射（低危→标准/中高危→高危/紧急→紧急）
  
- **C2 AutoExecutor** (`internal/autoplanner/auto_executor.go`): 全自动执行模式
  - 低危（RiskLow）→ 自动执行（LevelStandard）
  - 中高危（RiskMedium/High）→ 需人工确认（LevelHigh）
  - 紧急（RiskCritical）→ 紧急审批（LevelEmergency）
  - 失败自动回滚，回滚也失败则升级告警
  - 三种执行模式：ModeDryRun / ModeAuto / ModeForce
  
- **C3 PostReport** (`internal/autoplanner/post_report.go`): 事后审计报告生成
  - 修复摘要 + 指标对比（MetricsBefore/After/Delta）+ 审计链验证
  - `ToText()` 纯文本格式 + `ToJSON()` JSON 格式
  
- **C4 OpsMesh Client** (`internal/opsmesh/`): OpsMesh 平台集成客户端
  - `ReportResult()` — 回传修复结果给 OpsMesh
  - `GetTopology()` — 获取服务拓扑
  - `GetMetrics()` — 获取监控指标
  - `Ping()` — 健康检查
  - HTTP Bearer 认证 + 重试 + 限速
  
- **C5 gRPC Services** (`internal/grpc/`): 3 个新 gRPC 服务
  - `AlertService`: ReceiveAlert / GetAlertStatus / SubscribeAlerts（流式）
  - `DiagnosisService`: Diagnose / GetDiagnosis
  - `ConversationService`: SendMessage / SubscribeConversation（流式）
  - 手动编写 pb 代码（protoc 不可用）

### Changed

- `proto/levee.proto`: 追加 AlertService / DiagnosisService / ConversationService 定义
- `internal/grpc/pb/`: 新增 levee_extra.pb.go + levee_extra_grpc.pb.go（手动编写）

### Test Coverage

| 包 | 覆盖率 |
|----|--------|
| internal/autoplanner (C1) | 93.2% |
| internal/autoplanner (C2) | 96.77% |
| internal/autoplanner (C3) | 100% |
| internal/opsmesh (C4) | 90.3% |
| internal/grpc (C5) | 28 tests pass |

## [v1.7.0] - 2026-08-16

### Phase B — AI 建议 + 对话引擎

#### 新增特性
- **B1 知识库框架**：历史故障/Runbook/FixPattern 匹配引擎，Jaccard + 症状 + 根因三维评分
- **B2 LLM 集成**：OpenAI/Ollama adapter + Mock client，8 条内置脱敏规则（IP/密码/API key/DB连接/JWT/AWS key/邮箱/手机号）
- **B3 修复方案生成**：RecommendEngine 集成知识库+LLM+脱敏+优雅降级，WorkflowGenerator 生成 LEVEELang YAML 草稿
- **B4 对话引擎**：ConversationEngine 多轮对话状态机（Idle→Diagnosing→Recommending→Reviewing→Executing→Done/Failed）
- **B5 IM 对话扩展**：IMAdapter 桥接飞书/钉钉/Slack → ConversationEngine，审批卡片交互
- **B6 Web UI 对话框**：WebSocket Hub 实时对话通道，WSRequest/WSResponse JSON 协议
- **B7 CLI 对话命令**：`levee converse` 单次+交互模式，支持 /help /state /history /sessions /new

#### 新增文件
- `internal/recommend/` — AI 建议引擎（11 files: types, knowledge_base, defaults, llm, sanitizer, engine, workflow_gen + tests）
- `internal/conversation/` — 对话引擎（7 files: session, engine, im_adapter, web_hub + tests）
- `cmd/levee/cmd_converse.go` — CLI 对话命令

#### 测试覆盖率
- `internal/recommend/`: 91.8%
- `internal/conversation/`: 94.9%
- `cmd/levee/cmd_converse.go`: 98.88%

#### 依赖
- 无新增外部依赖

## v1.6.0 - 2026-08-16

### Phase A: 智能运维闭环引擎 - 告警接入 + 基础诊断

#### 新增功能
- **告警网关** (internal/alert/): 统一告警模型 + HTTP 网关 + Prometheus Alertmanager 适配器 + 自研平台适配器 + 去重/聚合/抑制
- **日志采集器** (internal/diagnosis/log_collector.go): SSH/Agent 远程日志拉取 + 多源并发采集 + syslog/journald/eventlog/app 四类源
- **日志分析器** (internal/diagnosis/log_analyzer.go): 8 种内置错误模式匹配 + 错误聚类 + 根因定位 + 置信度评分
- **健康探针** (internal/diagnosis/health_probe.go): 网络(ping/DNS/TCP) + 节点(CPU/内存/磁盘/负载) + 服务(进程/端口/HTTP) + 数据(DB/复制延迟) 四类探针
- **诊断引擎** (internal/diagnosis/engine.go): 并发执行日志分析+健康探针 + 综合诊断报告 + 告警触发诊断 + 多目标诊断
- **CLI 命令**: `levee alert serve/list/show/silence` + `levee diagnose <target>`

#### 技术指标
- 新增代码: ~5,500 行 Go 代码 + ~2,000 行测试代码
- 测试覆盖率: alert 90.8%, diagnosis 95.4%
- 新增包: internal/alert, internal/diagnosis (扩展)
- 新增 CLI 命令组: alert, diagnose

> **说明**：不存在 v1.5.0 —— 版本号从 v1.4.0 直接跳到 v1.6.0（该号被跳过、未发布）。

## [1.4.0] - 2026-08-16 — Phase 3: 生态扩展

### Added

- **F04 分布式执行**: Agent 常驻进程 + 心跳 + 注册/注销 + 任务执行 + 结果回传 + 多节点调度器（任务分片 + 负载均衡）+ `levee agent` CLI
- **F07 多租户**: 租户隔离（行级数据隔离 + 命名空间）+ 资源配额（目标机数/并发数/存储空间）+ 租户管理 CLI + 审计隔离
- **F10 配置漂移检测增强**: 定期巡检调度（cron）+ 漂移基线自动生成 + 漂移告警 + 趋势报告 + `levee drift` CLI
- **F12 移动端审批**: APNs/FCM 推送通知 + 深度链接 + 一键审批/驳回 + 响应式 Web UI 适配 + `levee push` CLI

## [1.3.0] - 2026-08-16 — Phase 2: 平台化

### Added

- **F01 Web UI**: Vue 3 + Vite + TypeScript 前端，7 个核心页面（变更看板/审批/监控/模板/目标/审计/系统），gRPC-Web API 客户端，go:embed 嵌入静态文件，`levee web` CLI 命令
- **F03 插件系统**: 四类插件接口（Channel/Gate/Module/Notifier），子进程沙箱（资源限制+崩溃恢复），SQLite 注册表，`levee plugin` CLI 命令，HTTP 探针示例插件
- **F06 RBAC 增强**: 角色继承树，细粒度权限策略（Resource×Action×Condition），ABAC 基于标签的访问控制，权限缓存（TTL 5min），`levee rbac` CLI 命令
- **F09 ChatOps**: 飞书/钉钉/Slack 机器人适配层，交互卡片消息，一键审批/驳回，变更通知推送，命令路由，`levee chatops` CLI 命令

## [1.2.0] - 2026-08-16 — Phase 1: 核心增强

### Added

- **F05 外部 KMS 集成**: HashiCorp Vault Provider（AppRole+KV v2+租约管理）+ AWS KMS Provider（信封加密）+ 降级策略 + `levee kms` CLI
- **F08 变更日历**: 冻结期 + 冲突检测（倒排索引）+ cron 重复规则（5 字段 POSIX）+ `levee calendar` CLI（6 个子命令）
- **F13 LEVEELang 类型检查**: 8 种/种基础类型 + 别名 + 枚举 + 类型注册表 + IR 生成 + `levee compile` 命令
- **F14 SLO 门禁三阶段时序**: PreApplySLOGate（基线检查）+ GracePeriodGate（延迟回归检测）+ PhaseGracePeriod

## [1.0.0] - 2026-08-16

### Added

#### 通道层

- SSH 通道实现（golang.org/x/crypto/ssh），支持密码/密钥认证 + 文件传输（scp）
- SSH 连接池 + 多路复用（ControlMaster 等价），单目标机并发上限可配
- WinRM 通道最小子集（masterzen/winrm），Negotiate 认证 + 命令执行
- WinRM 连接池，单连接单命令策略，并发上限可配
- 通道限速与背压：全局并发上限 + 单通道 + 单目标机三级限速，背压排队 + 超时
- 目标可达性预检：apply 前对每台目标机 noop 探测，失败剔除并产出预检报告

#### 工作流核心

- LEVEELang YAML 子集解析器：解析 input / target / window / batches / step / rollback / approval，产出 AST
- LEVEELang 基础校验：必填字段校验 + 类型基础校验 + 批次声明合法性
- Plan 生成器：目标解析 + 批次划分 + 步骤编排，产出执行计划结构体
- 影响面分析：直接受影响目标集 + 间接影响标注，产出影响面报告
- Plan 哈希锁定：canonical 化 + sha256 计算，plan_hash = hash(workflow + 目标集 + 参数 + 批次 + 影响面)
- 批次控制器：分批执行 + 批间串行 + 批内并发，批次边界显式，批间等待可配
- 批次并发限速：批内并发受通道限速约束，超限排队
- 验证门禁框架：GateManager 接口 + 门禁注册，pre_apply / post_batch / post_apply 三时机
- 命令门禁：在目标机执行检查命令，期望 exit_code / stdout 匹配，重试 + 超时
- SLO 门禁 post_batch：查询 Prometheus 指标，阈值比对，重试 + 超时

#### 变更闭环

- 审批服务框架：ApprovalService 接口 + 状态机（待审批 / 通过 / 驳回 / 超时）
- 审批三级分级：标准 / 高危 / 紧急，触发条件 + 审批人要求 + 超时配置
- 审批模板库：高危规则模板（删库 / 主从切换 / 防火墙全量），可配置
- 不可逆操作标记：irreversible: true 声明 + 白名单校验 + 强制升高审批级别
- 回滚协议框架：RollbackManager 接口 + 白名单校验 + 按批逆序调度
- 快照管理：apply 前快照创建（文件 / 配置备份）+ 快照存储 + 快照恢复
- 回滚后验证：回滚完成后强制跑 verify，失败按回滚失败处理
- 回滚失败分级：成功 / 部分回滚 / 回滚失败三档，对应通知 + 升级动作
- 失败语义五档模型：retryable / manual_retry / rollback / escalate / fatal
- 互斥锁 + TTL：目标机级锁 + TTL 默认 1h + 锁过期抢占 + 抢占前状态检查
- 全局暂停 / 恢复：pause-all / resume-all + 单 run pause / resume，留痕 + 权限校验

#### 审计安全

- 审计 trace 记录：每个动作记录输入 / 输出 / 耗时 / 目标机上下文
- 哈希链构建：每个动作 hash 包含前一动作 hash，链式结构，分批分片
- WORM 存储模拟：SQLite 模拟 WORM（追加只写 + 校验和），不可篡改
- 哈希链校验：任意 run 的 trace 可独立校验，篡改可检出并报错
- 凭据本地 AES-GCM 加密存储：argon2 密钥派生，凭据不落盘明文 / 不进 trace / 不进日志
- 凭据按需获取：apply 时按目标机获取凭据，用完即弃
- 权限 v0 框架：团队 x 环境二维权限矩阵，配置文件定义
- 权限校验集成：plan / apply / approval / rollback 操作前权限校验
- 通知框架：Notifier 接口 + 触发点注册，对象分级（发起人 / 审批人 / oncall / 订阅人）
- Webhook 通知渠道：webhook 发送 + 签名校验 + 重试
- 回滚通知独立：回滚触发 / 结果独立通知，不与 apply 合并

#### 兼容体验

- Playbook 兼容层框架：CompatLayer 接口 + playbook 导入解析，独立模块不引入核心依赖（R8）
- Playbook 最小子集执行：支持 shell / command / file / copy / template 模块，包审批 / 门禁 / 审计
- 兼容层风险评估：静态分析 shell / command 非幂等 + ignore_errors + 无 rollback，命中标记高危
- 裸 shell 直跑：`levee run --shell "cmd"` 单命令直跑，不走 workflow
- Dry-run 预览：`levee plan --dry-run` 产出目标集 / 批次 / 影响面 / 预估耗时 / 潜在冲突
- 变更克隆：`levee clone <run-id>` 生成可编辑副本，保留原参数与批次结构
- 模板库管理：模板存储 + 列表 + show，模板带参数占位
- 模板实例化：`levee new <template> --params key=val,...` 参数填充 + 完整性校验

#### CLI 命令全集

- 变更管理：new / clone / show / list / diff
- 审批控制：approve / reject
- 执行控制：apply / pause / resume / pause-all / resume-all / cancel / retry / retry-host / rollback
- 观察性：logs / trace / archive / link
- 模板管理：template list / show / create / delete
- 目标管理：target list / import / check
- 审计管理：audit verify / export / list / show
- 凭据管理：secret list / add / rotate / revoke / show
- 用户与团队：user list / add / team list / add
- 系统管理：system version / status / config get / config set / doctor / version

#### 发布工具

- GoReleaser 跨平台打包：linux amd64/arm64 + windows amd64，含 checksum
- 10 分钟测试门禁：单元测试 + 集成测试 + Lint，10 分钟内完成
- 发布门禁检查：G-01 至 G-07 全部通过

#### 内置模块

- shell 模块：shell / command 动作，幂等契约声明
- file 模块：copy / template 动作，文件分发 + 模板渲染 + 校验
- pkg 模块：install / remove / upgrade 动作，包管理器抽象（apt/yum/dnf）
- svc 模块：start / stop / restart / enable / disable / reload 动作，服务管理器抽象（systemd/sysvinit）
- user 模块：add / remove / modify 动作，用户/组管理 + SSH 公钥分发

### Security

- 凭据 AES-GCM 加密存储，argon2 密钥派生，凭据不落盘明文 / 不进 trace / 不进日志
- WORM 审计存储，追加只写 + 校验和，不可篡改
- 哈希链校验，任意 run 的 trace 可独立校验，篡改可检出
- 三级审批模型，高危变更强制人工审批，审批人不能是发起人
- 不可逆操作白名单 + 强制升高审批级别
- 团队 x 环境二维权限矩阵，操作前权限校验
- [SA-001] WORM 存储硬编码 SQLite 触发器，阻止 trace 表内容字段 UPDATE/DELETE，创建 WORMStore 接口限制审计层只读访问
- [SA-002] 哈希链 Build 前先 Verify 链完整性，拒绝重建已存在链（ErrChainAlreadyBuilt/ErrChainBroken），新增 BuildForce 用于管理恢复
- [SA-003] 实现 RotateMasterPassword 三阶段原子轮换（旧密码解密→新密码加密→更新内存），所有错误路径 SecureZero 清理