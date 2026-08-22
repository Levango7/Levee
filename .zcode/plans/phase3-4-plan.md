# Phase 3 + 4 交付计划 — Levee 上线前收尾

## 背景
Phase 1（安全止血）、Phase 2（lint 全面整改）已完成。本阶段聚焦：
1. Phase 3: DSL 规范-实现对齐 + 文档/UI/API 一致性修复
2. Phase 4: Dockerfile 与发布制品补齐
3. 三阶段完成后全局验证并提交

## 发现清单

### Phase 3 问题

**P3-1: tests/integration/ 目录为空**
- Makefile 的 `test-integration` 目标执行 `go test ./tests/integration/...` 报 "matched no packages"
- 需要补至少一个 integration 测试让目标有意义（或改为 conditional）
- 已有 `tests/helpers.go` / `tests/mocks.go` 可用

**P3-2: DSL 规范 vs 实现不一致**
- docs/leveelang-spec.md 定义 `steps/targets/batches/gates/approval/rollback/input/output` 为一等原语
- 需对照 internal/dsl/parser.go 验证所有关键字有对应解析逻辑
- 重点检查：batches 的 `wave_size`/`serial_pct`、gate 的 `pre_apply/post_batch/grace_period` 三段式时序

**P3-3: API 路径 /api/ vs /v1/ 不一致**
- internal/web/server.go 用 `/api/` 作为前缀匹配
- embed.go 中 SPA fallback 只排除 `/api/` 和 `/events/`，前端 `axios` baseURL 是 `/api/v1`（见 dist 里的 JS bundle）
- 需确认 grpc-gateway 路由约定并补齐前缀映射

**P3-4: README/quickstart 过时**
- README 或 quickstart 可能缺少 go 版本声明（当前要求 1.25）
- 需对齐 CI 矩阵中已列出的平台

**P3-5: Validate() gocyclo 51**
- config.go:217 Validate 函数复杂度超 30（gocyclo 默认阈值）
- 需重构为子函数，但风险较大，建议单独处理

### Phase 4 问题

**P4-1: .goreleaser.yml 缺少 darwin**
- builds.goos 只有 linux/windows，缺少 darwin
- 与 Makefile TARGETS (`darwin/amd64 darwin/arm64`) 和 CI build matrix（macos-latest）三方不一致
- 需在 goreleaser 补充 darwin 并在 README 更新发布说明

**P4-2: Dockerfile 缺失**
- 仓库根目录无 Dockerfile
- README 示例使用 `curl | tar xz` 安装，无容器化路径
- 需创建多阶段构建 Dockerfile（alpine + 最终瘦身镜像）

**P4-3: Makefile 构建矩阵不完整**
- TARGETS = linux/amd64 linux/arm64 windows/amd64 darwin/amd64
- CI build matrix 已包含 arm64 全平台
- goreleaser 只 build linux/windows
- 需三方对齐

## 执行策略

### Phase 3 任务拆分

**T3-1: 补 tests/integration 目录**
- 创建 `tests/integration/suite_test.go` 作为包入口（可选）
- 创建 1-2 个集成测试（例：模板实例化 → 审批 → 执行链路）
- 复用 tests/helpers.go 中的 TempDB、MockTarget

**T3-2: DSL 规范对齐**
- 读取 docs/leveelang-spec.md 完整内容
- 对照 internal/dsl/parser.go 的关键字解析
- 产出差异清单，决定是否更新文档或补实现

**T3-3: API 路径前缀对齐**
- 读取 internal/web/server.go 完整理解 /api/ proxy 逻辑
- 读取 internal/grpc/ 下的 gateway 注册
- 确认是否需要加 /v1/ 前缀或调整前端 baseURL 常量

**T3-4: README/quickstart 更新**
- 更新 Go 版本要求（1.25）
- 补充 darwin 支持说明
- 对齐 CI 矩阵的 OS/ARCH 列表

### Phase 4 任务拆分

**T4-1: 补齐 .goreleaser.yml darwin**
- 在 builds.goos 加 darwin
- 验证与 Makefile TARGETS 和 CI matrix 一致

**T4-2: 创建 Dockerfile**
- 多阶段构建（builder + alpine runner）
- 使用 CGO_ENABLED=0
- 暴露端口（如有 web/gRPC）
- 提供 HEALTHCHECK

**T4-3: Makefile TARGETS 三方对齐**
- TARGETS 补充 darwin/arm64（已缺）
- 与 CI matrix + goreleaser 保持一致

## 验证标准
- `go build ./...` 通过
- `go test ./... -count=1` 全绿
- `go vet ./...` 通过
- `golangci-lint run ./...` 退出码 0（lint 噪音按 Phase 2 配置豁免）
- `make check` 通过（lint + test）
- `make test-integration` 通过（不再是 "matched no packages"）
- Dockerfile 能本地构建成功：`docker build -t levee:test .`
- goreleaser dry-run 通过：`goreleaser check` 或 `goreleaser release --snapshot`

## 风险提示
- P3-2 DSL 对齐可能涉及 parser 改动，有回归风险；需保守只补测试用例不修改解析器
- P3-3 API 路径变更可能影响 Web UI（dist 内 JS bundle 已硬编码 `/api/v1`），需同步检查
- P4-2 Dockerfile 需与现有 systemd/nginx 部署假设兼容

## 提交策略
- 每完成一个 T3/T4 任务即 commit（保持可追溯）
- 最终 commit message 格式：`fix(phase3+4): integration tests, DSL/doc/API alignment, Dockerfile, release artifacts`
- 推送到 master
