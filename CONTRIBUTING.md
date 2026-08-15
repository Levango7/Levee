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

## 第5章 Pull Request 流程

### 5.1 PR 创建

1. 确保分支上所有测试通过：`make check`
2. 确保代码格式化：`make fmt`
3. 在 GitHub 上创建 Pull Request，目标分支为 main
4. PR 标题遵循 Conventional Commits 格式

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