# 贡献指南

感谢你对 LEVEE 项目的关注！本文档描述如何参与 LEVEE 开发。

## 第1章 开发环境设置

### 1.1 前置依赖

| 依赖 | 版本要求 | 说明 |
|------|----------|------|
| Go | 1.22+ | 编译与运行 |
| Git | 2.x | 版本控制 |
| Make | 任意 | 构建编排 |

### 1.2 环境搭建

```命令示例：克隆仓库并构建
git clone https://github.com/nexus/levee.git
cd levee
make build
```

### 1.3 安装开发工具

```命令示例：安装 Go 开发工具
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
go install golang.org/x/tools/cmd/goimports@latest
```

### 1.4 验证环境

```命令示例：验证开发环境
make lint
make test
make build
./levee version
```

## 第2章 代码风格

### 2.1 包注释

- 每个包必须有包注释，使用英文编写
- 包注释以 `// Package xxx` 开头，紧跟包名

```go
// Package channel provides the abstraction layer for remote execution
// channels including SSH and WinRM.
package channel
```

### 2.2 命名规范

| 类别 | 风格 | 示例 |
|------|------|------|
| 函数/方法 | camelCase | `generatePlan`, `verifyHashChain` |
| 导出函数 | PascalCase | `NewEngine`, `ApplyWorkflow` |
| 常量 | PascalCase | `DefaultTimeout`, `MaxRetries` |
| 私有变量 | camelCase | `batchSize`, `lockTTL` |
| 接口 | PascalCase + er 后缀 | `Executor`, `Notifier`, `Store` |
| 错误变量 | PascalCase + Err 前缀 | `ErrNotFound`, `ErrTimeout` |

### 2.3 错误处理

- 使用 `fmt.Errorf("xxx: %w", err)` 包装错误，保留错误链
- 禁止使用 `panic`，所有错误通过返回值传递
- 错误消息以小写字母开头，不含句号

```go
// 正确
if err != nil {
    return fmt.Errorf("connect to target %s: %w", host, err)
}

// 错误
if err != nil {
    panic(err)
}
```

### 2.4 其他规范

- 导入分组：标准库 / 第三方库 / 项目内部包，组间空行分隔
- 行宽不超过 120 字符
- 文件末尾保留一个换行符
- 避免未使用的导入和变量

## 第3章 提交规范

### 3.1 Conventional Commits

提交消息遵循 [Conventional Commits](https://www.conventionalcommits.org/) 格式：

```
<type>(<scope>): <description>

[可选正文]

[可选脚注]
```

### 3.2 类型

| 类型 | 说明 | 示例 |
|------|------|------|
| feat | 新功能 | feat(batch): add batch concurrency limiter |
| fix | 修复缺陷 | fix(ssh): fix connection pool leak on timeout |
| docs | 文档变更 | docs(api): update CLI reference for v1.0 |
| test | 测试相关 | test(rollback): add rollback drill test cases |
| chore | 构建/工具变更 | chore(release): update goreleaser config |
| refactor | 重构（不改变行为） | refactor(plan): extract hash locking to separate function |

### 3.3 范围

scope 对应内部模块名：

- `channel` - 通道层
- `engine` - 工作流引擎
- `executor` - 模块执行器
- `approval` - 审批服务
- `audit` - 审计哈希链
- `state` - SQLite 存储层
- `plan` - Plan 生成
- `rollback` - 回滚协议
- `verify` - 验证门禁
- `batch` - 批次控制
- `lock` - 互斥锁
- `credential` - 凭据管理
- `template` - 模板库
- `compat` - playbook 兼容层
- `dsl` - YAML 子集解析
- `notify` - 通知
- `config` - 配置管理
- `permission` - 权限
- `pause` - 全局暂停
- `cli` - CLI 命令

### 3.4 提交消息示例

```
feat(approval): add three-tier approval model

Implement standard/high/emergency approval levels with
configurable triggers, approver requirements, and timeout.
```

## 第4章 分支策略

### 4.1 分支命名

| 分支 | 命名格式 | 用途 |
|------|----------|------|
| main | main | 稳定发布分支 |
| 功能分支 | feature/<module>-<description> | 新功能开发 |
| 修复分支 | fix/<module>-<description> | 缺陷修复 |

### 4.2 示例

- `feature/batch-concurrency-limiter`
- `fix/ssh-connection-pool-leak`
- `feature/approval-three-tier`

### 4.3 分支生命周期

1. 从 main 创建功能/修复分支
2. 在分支上开发并提交
3. 完成后创建 Pull Request 合入 main
4. 合并后删除分支

### 4.4 多人在同一台机器上并行时

如果这个仓库正被多个会话（人或 agent）并行处理，动手前先读**协作留言板**，动完把自己
那条更新或删掉。格式、有效期规则与校验器用法见 `docs/agent-board.md`；命令是
`go run ./internal/agentboard -dir "$LEVEE_AGENT_BOARD_DIR"`。留言板只陈述事实，
不是命令通道，也不是证据——它不改变任何合并/推送的审批关系。

## 第5章 Pull Request 流程

### 5.1 PR 创建

1. 确保分支上所有测试通过：`make check`
2. 确保代码格式化：`make fmt`
3. 在 GitHub 上创建 Pull Request，目标分支为 main
4. PR 标题遵循 Conventional Commits 格式

> **base 决定 CI 存不存在**。`.github/workflows/ci.yml` 的 `pull_request` 触发只列了
> `main` / `master` / `develop`。把 base 设成另一条功能分支（stacked PR 为了 diff 只剩
> 自己那一笔常这样做）时，`ci.yml` 的 24 个 check run（含 `check (all jobs passed)`
> 汇总，而它正是 master 分支保护里唯一的 required context）**一个都不会跑**——挂在
> 那种 PR 上的只有 CodeQL 两条与第三方检查。于是"这个 PR 没有红"并不等于"CI 绿了"。
> **改 base 也不会补跑**：GitHub 把切 base 记为 `edited`，不在 `ci.yml` 声明的默认事件
> 类型里；要拿到覆盖，必须在该 PR 上再有一次真实推送。

### 5.2 PR 描述模板

```markdown
## 变更说明

简要描述本次变更的内容和目的。

## 关联任务

- 关联的任务编号或 issue

## 测试

- [ ] 单元测试通过
- [ ] 集成测试通过（如适用）
- [ ] 手动验证（如适用）

## 检查清单

- [ ] 代码风格符合规范
- [ ] 无 panic 引入
- [ ] 错误使用 fmt.Errorf 包装
- [ ] 新功能有对应测试
```

### 5.3 审查要求

- 至少一名审查者通过
- CI 检查全部通过（lint + test + build）
- 无未解决的讨论

### 5.4 合并方式：用 rebase 落地

**PR 一律 `gh pr merge <n> --rebase`**，自 2026-10-17 起生效，本条口径自身的落地就是第一笔实践。
在此之前 master 上每个 PR 都是一个双父的 `Merge pull request #N` 合并提交；**那段历史不改写**
（不对 master 做 force-push），所以 `git log` 里两种形状并存是预期而不是脏——读到 `385d772`、
`ff669ed`、`5139395f` 这些合并提交时，它们是当时的事实。

换的理由三条：

- **合并提交在这里不携带决策。** 本仓一个 PR 通常是一到两笔已按第3章写好 message 的提交，
  `Merge pull request #N from <branch>` 只是把分支名再抄一遍。squash 也不取：它会把
  "一笔提交＝一个决定"压掉，而那正是各批次把守卫与产品码分开的用意。
- **让 PR 级证据与 master 级证据对齐。** §5.1 那条"切 base 不补跑"意味着 stacked PR 的 PR 检查
  可能跑在旧 base 上（#170 / #172 就是这样：两份绿 CI 都不在当时的 master 上，只能靠合并后
  master 的 push CI 补证）。rebase 落地后 master 上那笔提交就是 PR 里验过的同一笔内容，
  追溯不必再同时读两种形状。
- **发布说明本来就在指认合并提交**（`docs/release-notes/v1.22.0.md` 写"tag 指向 PR #168 的合并提交"
  这一类）。改成指认"某笔提交属于 PR #N"是这次换法**有意接受**的口径变化，不是顺带发现的漂移。

换法之后要留意的三条（第 1、2 条已在第一笔 rebase 落地 `#174` 上实测，取代本节初稿里"预期返回 null"的说法）：

1. **`mergeCommit` 不会变成 null。** 实测 `gh pr view 174 --json mergeCommit` 返回 `4ab1b10`——
   GitHub 把**落到 master 上的那笔（重写后的）提交**报在这个字段里，它不是合并提交。所以初稿
   "预期 null"是错的；读 `mergeCommit` 的脚本不会因此坏。现查本仓 `scripts/`、`.github/`、
   `internal/` 也没有任何地方读 `mergeCommit` / `baseRefOid` / `mergedAt`，两处结论一致。
2. **rebase 重写 sha，这才是新的坑。** 同一笔改动在 PR 侧是 `841d7f0`、在 master 侧是 `4ab1b10`，
   `git diff --stat 841d7f0 4ab1b10` 为空——**树相同、sha 不同**。三条推论：
   - `gh pr view --json commits` 给的是 rebase **之前**的 sha，它**不在 master 谱系上**，
     拿它去 master 里找提交会一无所获；
   - 本仓一直在用的"祖先关系断言"从此失效：`git merge-base --is-ancestor <PR头> origin/master`
     在 merge-commit 时代是真判据（今天还用它核过 #172 / #173），rebase 之后会**假阴性**。
     改判据三选一：读 `mergeCommit.oid`、`git log --grep '<提交标题>'`、或直接比树；
   - 落地形状可以机器核：`git rev-list --parents -1 origin/master | awk '{print NF-1}'` 应为 `1`。
3. master 分支保护里唯一的 required context 仍是 `check (all jobs passed)`（现查
   `gh api repos/Levango7/Levee/branches/master`，`enforcement_level=non_admins`），换合并方式不改变它，
   也不改变"合与推的决定权属于仓库的人"这条边界（见 `AGENTS.md` 第 3 条不可越过的边界）。

这条口径目前没有机器守卫：可以在 `ci.yml` 的 master-push 路径上加"标记提交之后不得再出现合并提交"
的断言，但那要同步动扇入清单（`needs` 列表与判决字符串是本仓踩过两次的地方），属于另一笔决定，
本条只把规则写清楚。

## 第6章 测试要求

### 6.1 单元测试

- 新增功能必须编写单元测试
- 测试覆盖率目标：核心模块 >= 80%
- 使用 testify 断言库

```go
func TestGeneratePlan(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        want    *Plan
        wantErr bool
    }{
        // 测试用例
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := generatePlan(tt.input)
            if tt.wantErr {
                assert.Error(t, err)
                return
            }
            assert.NoError(t, err)
            assert.Equal(t, tt.want, got)
        })
    }
}
```

### 6.2 测试命令

```命令示例：运行测试
make test              # 单元测试（含竞态检测 + 覆盖率）
make test-integration  # 集成测试
make test-e2e          # 端到端测试
```

### 6.3 静态检查

```命令示例：运行静态检查
make lint     # golangci-lint 全量检查
make lint-fix # golangci-lint 自动修复
```

### 6.4 格式化

```命令示例：格式化代码
make fmt      # gofmt + goimports
```

### 6.5 生成物：改了词表要重跑生成器

仓库里有两类**生成物**，CI 都会用 `git diff --exit-code` 校验漂移。手改它们不会生效，只会让 CI 变红。

```命令示例：重生成并校验
bash scripts/gen-proto.sh              # proto → internal/grpc/pb/
go run ./internal/docgen               # 代码词表 → docs/leveelang-spec.md 的参考表
go run ./internal/docgen -check        # 只校验不重写（CI 用这个）
```

**什么时候必须重跑 `internal/docgen`**：改动以下任一内容时——

- `internal/runstatus`（增删 run 状态或调整准入集合）
- `internal/dsl.BatchStrategies`（增删批次策略）
- `internal/approval` 的层级默认值（超时时长、审批人数、超时处理策略）

若新增了状态或策略但没在 `internal/docgen` 的 `statusMeaning` / `batchSemantics` 里补释义，生成器会渲染出一个占位符，**并且 `go test ./internal/docgen/` 会直接失败**——这是有意的：一张有行没说明的表比没有表更糟。

参考表由代码生成，而**散文的正确性仍靠人**。`docs/leveelang-spec.md` 中 `BEGIN/END GENERATED` 标记之外的内容不受生成器管辖。

## 第7章 构建与验证

### 7.1 常用命令

| 命令 | 说明 |
|------|------|
| `make build` | 构建二进制 |
| `make test` | 单元测试 |
| `make lint` | 静态检查 |
| `make fmt` | 格式化代码 |
| `make check` | lint + test 全量检查 |
| `make tidy` | 整理 go.mod |
| `make clean` | 清理构建产物 |
| `make cross-build` | 跨平台编译 |

### 7.2 完整验证流程

提交 PR 前执行：

```命令示例：完整验证
make fmt
make tidy
make check
```

`make check` 等价于 `make lint && make test`，是 PR 合入的最低门禁。