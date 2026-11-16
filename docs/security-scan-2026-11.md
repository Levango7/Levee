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
