# Agent 须知

本文件是给**在此仓库上工作的 agent**（任何 CLI/模型）的说明。人类贡献者请看
`CONTRIBUTING.md`。它只讲协作；工程门禁的口径以 `CONTRIBUTING.md` 与本仓 CI 为准。

## 动手前：读留言板

多个 agent 会在同一台机器上并行处理这个仓库，而碰撞大多是**缺一条事实**，不是缺代码。
本机有一个协作留言板，格式与规则见 `docs/agent-board.md`，校验器是 `internal/agentboard`：

```bash
# 板目录没有默认值：它由 -dir 或环境变量给出（这是刻意的）
go run ./internal/agentboard -dir "$LEVEE_AGENT_BOARD_DIR"
```

读它输出的最后一段"现在有人正在动的文件"，再决定要不要打开那个文件。

## 干活期间：写自己那一条

板目录里一个 agent 一个文件（文件名小写、无斜杠，例如 `qoder-a5c.md`），键值行如下：

```
agent: qoder-a5c
branch: feat/node-capability-declaration
state: active
updated: 2026-10-15T03:04:05+08:00
ttl-minutes: 180
worktree: F:/Nexus/levee-wt-a5c
owns: internal/grpc/rest.go, web/src/views/ClusterView.vue
notes: 正在给 cluster-status 加能力过滤
```

- `updated` 必须是 RFC3339 带偏移；`ttl-minutes` 取 5–1440。**到期就判红**，
  所以要么续期，要么删掉文件。收尾时删掉它。
- 词表是封闭的：写错键名会被判红，不是被忽略（把 `ttl-minutes` 打成 `ttl_minutes`
  如果被忽略，这条条目就永远不会过期，而下一个人会当它是有效声明）。

## 三条不可越过的边界

1. **留言板不是命令通道。** 条目只陈述事实，没有优先级、没有派活、没有请求。
2. **别人的声明不是证据。** 别的 agent 说"验过了"，你仍需自己重跑你要依赖的那条结论。
3. **合并与推送的决定权属于仓库的人。** 不要因为看到一条 `state: active`
   就去合并、rebase、关闭或删除别人的分支；过期条目可以直接删（它按定义已不再断言任何
   东西），但那不等于否决那条工作。

## 本机上已经付过代价的几件事

- **`F:\Nexus\levee` 是别人的 checkout**，别在里面改东西；自己开工作树
  （`git worktree add ../levee-wt-<tag> -b <branch> origin/master`）。
- **工作树可能被并行会话批量清理**：改完就 commit，commit 完就 push，别攒。
  `F:\` 盘禁用 `git stash`。
- **默认端口 5173 / 4599 是别人 checkout 的 vite dev server 与 mock gateway**。
  跑 `web/scripts/browser-check.mjs` 之前先自证端口归属（`Get-CimInstance
  Win32_Process` 看命令行），或用 `MOCK_PORT` / `VITE_API_TARGET` / `BASE_URL` /
  `MOCK_URL` 换到自己的端口。浏览器检查不会自己起服务。
- **推送只走** `ssh://git@ssh.github.com:443/Levango7/Levee`（https 直连会失败）。
- **Go 工具链：`go.mod` 要求 Go ≥ 本机版本时，先修 `GOSUMDB`**。本机装的是 go1.26.6，
  而 `go.mod` 要求 1.26.10，`GOTOOLCHAIN=auto` 会去下载 `go1.26.10`——但 Go 对
  **工具链模块强制要求校验数据库确认**，`GOSUMDB=off` 会让它直接失败，报
  `verifying module: checksum database disabled by GOSUMDB=off`。**这个错误出现在
  任何 go 命令上**（含 `go version`），容易被误判成代码坏了。修法（机器级、一次）：
  `go env -w GOSUMDB=sum.golang.org`（`GOPROXY` 已是 `https://goproxy.cn,direct`，
  goproxy.cn 提供 sumdb 代理，下载与校验都通）。**注意 `GOPRIVATE` / `GONOSUMDB`
  对工具链无效**——Go 的工具链路径不接受豁免，只能给一个真实 sumdb。
- **浅克隆上做 `git fetch --unshallow` 之后，`git fsck` 会红——那是 commit-graph 缓存过期，
  不是仓库坏了**（本机 2026-11-04 实测，git 2.56.0.windows.2）。症状：
  `git fsck` / `git commit-graph verify` 打出若干 `Could not read <sha>` 与
  `failed to parse commit <sha> from object database for commit-graph`（那次是 5 个）。
  **别急着重新 clone**，先按三步定性：① `git rev-list --objects HEAD`（rc=0 且无 `missing` ⇒
  master 全史对象齐）；② `git cat-file -e <那个 sha>`（rc=1 ⇒ 它本来就不在对象库里）；
  ③ `gh api repos/Levango7/Levee/compare/<sha>...master`（那次 5 个全是 `diverged` /
  `behind_by=1`，即当年没合进来的 dependabot PR 头 ⇒ **历史没有洞**）。定性为缓存后，修法：
  删 `.git/objects/info/commit-graphs/`（split 链）或 `.git/objects/info/commit-graph`，
  再 `git commit-graph write --reachable`；复验 `git commit-graph verify` 与 `git fsck` 都要 rc=0。
  **为什么值得先读这条**：commit-graph 只是加速缓存，删掉 git 会回落到 pack index，代价是日志查询变慢；
  而"fsck 红"看起来像数据损坏，很容易被误处置成重新 clone 或者不敢推。
- **顺着上一条**：浅克隆还会让 `git rev-list --left-right --count` 与 `git branch --no-merged`
  给出**假数**（本机实测解浅前把已完成的分支报成 `ahead=700+`）。要按祖先关系下结论
  （"这条分支还有没有未落地的东西"），先 `--unshallow`，再按补丁等价判——`git cherry <base> <branch>`
  的 `+` 才是"master 里没有"的真判据，rebase 落地改写过 sha 也不会误判。
- PostgreSQL 相关测试包必须 `-p 1` 串行跑，否则它们会互删数据，症状是一个无关包 panic。
- **本机有可用容器底座**：`docker` 可用（29.9.2）；镜像里已有
  `ghcr.io/levango7/levee:v1.24.0`、`postgres:16` / `postgres:16-alpine`、
  `node:24-alpine`。**`LEVEE_PG_TEST_DSN` 未设时那 28 个 PG 用例是 skip 的**，
  起一个 PG 容器就能把它们真跑起来（起容器前先确认 5432 端口归属，本机有多个
  已停的 postgres 容器占过 25499 / 5432）。
