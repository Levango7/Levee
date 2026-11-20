# LEVEE 供应链安全扫描报告（2026-11-16）

| 元信息项 | 内容 |
| --- | --- |
| 文档类型 | 证据报告（安全扫描复核，**不构成合规证书**） |
| 基线 | `origin/master` @ `2108093`（`go1.26.6`，`go.mod` 声明 `go 1.26.11`——见下文 §工具链） |
| 撰写 | agent `levee-opencode` |
| 命令与工具版本 | `govulncheck` v1.9.0（pinned）；`gosec` v2.28.0（pinned，**与 CI 完全同一命令**）；`npm audit`（官方 `registry.npmjs.org`） |

---

## 1. 一句话结论

**截至 2026-11-16 20:18，LEVEE master（`2108093`）没有已知可利用的高危/严重漏洞：Go 模块 `govulncheck` 报 0；`gosec`（CI 同款命令、同款排除）报 0；npm 依赖 `npm audit` 报 0。CodeQL 由 CI 腿跑（`.github/workflows/codeql.yml`），本机未重跑；Docker 守护进程当时未运行，trivy 镜像扫描由发布门（`release.yml` + `release-gate` 腿）承担——本报告不代替它。** 任何对外承诺仍须带「工具版本 + 日期 + 路径」限定。

---

## 2. 命令与结果

### 2.1 Go 依赖（`govulncheck ./...`）

```text
$ E:\dev-data\go\bin\govulncheck.exe ./...
No vulnerabilities found.
```

govulncheck 只报告 **可达调用路径** 上的漏洞（比 nancy/snyk 严格得多），结果：**0**。

工具链核对：`go version = go1.26.6 windows/amd64`；`go.mod` 有 `go 1.26.11` 声明——本次 govulncheck 运行时工具链满足（`GOTOOLCHAIN=auto`，`go version` 输出已包含）。**不要求**安装/指定更高的 Go——扫描按 `go.mod` 允许的版本运行即合规。

### 2.2 Go 源代码（`gosec`，CI 同款命令）

```text
$ gosec -exclude=G104,G115,G304,G307,G501,G505,G601 -exclude-dir=internal/grpc/pb -fmt=text ./...
Results:  Files: 375  Issues: 0
```

对照 CI（`.github/workflows/ci.yml:592`）：同命令、同排除、同 `-exclude-dir=internal/grpc/pb`。首次运行不带 pb 排除时会命中 8 条 **G103**（全部 `internal/grpc/pb/*.pb.go` 的 protobuf 生成代码里 `unsafe`，属生成物，CI 亦排除）；加上 CI 口径后 **0**。`go.mod` pinned：`github.com/securego/gosec/v2/cmd/gosec@v2.28.0`。

### 2.3 npm 依赖（`web/`）

首次 `npm audit --json`（指向 `registry.npmmirror.com`）报错 `NOT_IMPLEMENTED /-/npm/v1/security/*`——**npmmirror 镜像不实现 audit 接口**。改用官方镜像复核：

```text
$ npm audit --json --registry=https://registry.npmjs.org
metadata.dependencies.total: 214（prod 130 / dev 58 / optional 27 / peer 0）
metadata.vulnerabilities: { info: 0, low: 0, moderate: 0, high: 0, critical: 0, total: 0 }
```

结果：**0**。node_modules 使用 `web/package-lock.json` 锁定的版本（不再 `npm ci` 全新安装；新装与锁文件不一致会改表）。

### 2.4 CodeQL 与 trivy

- **CodeQL**：`.github/workflows/codeql.yml` 腿作为门控跑的（`/Security` 仪表盘 → Code Scanning），本机不重跑——口径：指引用仓库 CI 门控记录，不引入本机未跑过的口令。
- **trivy**：本机 Docker 守护进程在扫描时未运行，未能 `docker build` + trivy。**诚实记录**：镜像层漏洞由 CI 发布腿（tag 触发）扫描。若指令 "就本机 trivy 强制复跑"，需先启动 Docker Desktop。

---

## 3. 附带：已验证的本机复核（非投标门槛）

- `docker` 进程不可用：不会自动启动，**本报告不宣称镜像已重新扫描**。
- `gosec` 运行与 CI 同一台 Windows 宿主（go1.26.6 Win11 中文版 build 26300），CI 运行在 Ubuntu/macOS/Windows 三架马 runner，两端都应绿。

---

## 4. 什么不在本报告范围

1. **镜像里拉过的第三方依赖**：`Dockerfile` 装入的 `ca-certificates`/`alpine` 基底——由 trivy + CI release 门控兜底。
2. **Windows 代码页/中文文件名**：本次扫描在中文 Windows 上，文件名含中文的 path 解析均正常（`－exclude-dir=internal/grpc/pb` 成功生效）。
3. **生成代码**（`internal/grpc/pb/*.pb.go`，G103/`unsafe`）：超出应用层代码扫描范围；`protoc` 生成物由上游负责。
4. **私有依赖/自建包**：本仓无。

---

## 5. 复核命令（照抄即可）

```powershell
cd F:\Nexus\levee-wt-eng
$env:GOPROXY='https://goproxy.cn,direct'
govulncheck ./...
gosec '-exclude=G104,G115,G304,G307,G501,G505,G601' '-exclude-dir=internal/grpc/pb' '-fmt=text' './...'

cd web
npm audit --json --registry=https://registry.npmjs.org
```

---

## 6. 结论的边界

- 本扫描是 `master@2108093` 的**横截面**，不是持续保证；CI 每次 push 都会跑 `govulncheck` + `gosec` + CodeQL + trivy，**合并门控不等同因本报告跳过 CI**。
- 报告没发现漏洞 ≠ 产品无漏洞。`govulncheck` 只看「可达调用路径」；`gosec` 是基于模式匹配；`npm audit` 需要 npm advisory DB 同步（本报告用官方镜像，数据源是 npm 官方 advisory feed）。
- 对外可引用版本口径：**「2026-11-16 master，无已知可利用高危/严重漏洞（govulncheck 0 / gosec 0 / npm audit 0）」**。不可引用为"通过等保/红字联测"。

---

## 7. 修复记录（本会话）

无——本次扫描没有命中任何需修复的条目。首轮不带 pb 排除时命中 8 条 G103（protobuf 生成 `unsafe`，生成物），已按 CI 口径排除，无代码变动。

---

## 8. 2026-11-20 复扫（P2-B 收口）＋ 第三方渗透测试摸底

### 8.1 复扫结果

基线 `origin/master` @ `6d51d9f`，工具链 `go1.26.11 windows/amd64`，`GOPROXY=https://goproxy.cn,direct`。

| 项 | 结果 | 来源 |
| --- | --- | --- |
| Go 依赖 govulncheck | **No vulnerabilities found**（`golang.org/x/vuln` v1.10.0，本机 `go run ...@latest ./...`） | 本机 2026-11-20 实跑 |
| Go 源码 gosec | **0 issues** | CI 腿（PR #411/#413 checks，同排除口径） |
| Go 漏洞库 | **pass** | CI 腿 `govulncheck` |
| 容器镜像 trivy | **pass** | CI 腿 `trivy` |
| CodeQL | **pass** | CI 腿 `CodeQL` / `analyze (go)` |
| npm audit | **0**（沿用 §2.3） | `web/package-lock.json` 自 `2108093` 至今 **0 变更**（`git log 2108093..HEAD -- web/package-lock.json` 为空），故 11-16 结论对本基线仍成立；本轮未重跑 |

期间 `go.mod` 有多笔依赖 bump（`aws-sdk-go-v2/config`、`x/crypto`、`pgx/v5 5.12→5.13`、`modernc.org/sqlite 1.61.1→1.62.0` 等），均已包含在本轮 govulncheck 复扫内。

### 8.2 SBOM

`go list -m -json all` → CycloneDX 1.5，**159 个组件（不含主模块）**，落档 `G:\levee-vm\evidence\sbom-2026-11-20.json`。这是模块级 SBOM，**不是容器镜像层 SBOM**（后者仍由 CI 发布腿 trivy 承担）。

### 8.3 第三方渗透测试：渠道与预算摸底（公开资料，非正式报价）

公开可查的渠道分四类：① 国家级权威检测机构；② 上市安全厂商安全服务团队（腾讯云、奇安信补天众测、绿盟、安恒、漏洞盒子等）；③ 专业第三方测评机构；④ 区域型机构。**本次未采购、未执行任何第三方渗透测试**——以下只是选型与预算输入。

腾讯云 PTS 公开标价（2026-05 公开文章，**单次、含三次免费复测**）：

| 类型 | 档位 | 价格（元/次） |
| --- | --- | --- |
| Web | I 展示型 / II 功能型 / III 交易型 | 31,800 / 53,000 / 84,800 |
| 小程序 | I 功能型 / II 交易型 | 31,800 / 53,000 |
| App | 客户端 I/II | 31,800 / 42,400 |
| App | 服务端 I/II | 21,200 / 31,800 |
| 二进制 | 客户端 I/II | 31,800 / 42,400 |
| 二进制 | 服务端 I/II | 21,200 / 31,800 |

国际参考（Ostorlab 2026 综述）：人工应用渗透测试 $3,000–$50,000+，公开标价 AI 渗透测试 $499–$8,000；Invadel 公开价 web app $5,200 / API $4,000 / external $4,200 / cloud $6,800。

**对 LEVEE 的建议范围与预算口径**：产品同时暴露 Web UI、REST/gRPC API 与静态二进制，最小有代表性的组合是 **Web II 类（53,000）＋ 二进制服务端 I 类（21,200）≈ ¥7.4 万/次**；若要覆盖 API 单独计价需向厂商确认是否含在 Web 类内。量级结论：**¥5–10 万/次**，不含等保/密评专项。**若用于投标加分，必须先向机构确认报告是否带 CMA/CNAS 资质**——普通商业渗透报告不等同于检测报告。上述均为公开文章标价，正式预算须走询价/合同。

### 8.4 边界

- 本轮没有第三方渗透测试，**对外不得说「已过渗透测试 / 安全认证」**（与 `Levee-第三方评估报告-复核完善版.md` §8 一致）。
- SBOM 与扫描结果是研发侧自证基线，**不作招投标门槛值、不引用为认证**。
- 工具版本、日期、基线 commit 必须随引用一起给出。
