# ADR-005: playbook 兼容层独立模块

| 字段 | 内容 |
|------|------|
| 编号 | ADR-005 |
| 标题 | playbook 兼容层独立模块（R8 约束：不引入核心包依赖） |
| 状态 | 已采纳 |
| 日期 | 2026-08-16 |

## 上下文

LEVEE 的设计红线 R8 要求"兼容层不能污染核心"：兼容逃生舱独立模块，不引入核心路径依赖，可独立卸载。

playbook 兼容层的目标是让现有 Ansible playbook 可导入 LEVEE 执行，包审批 / 门禁 / 审计，确保"第一天能干活"。但兼容层本身是过渡方案：

- Ansible playbook 语义与 LEVEELang 语义不完全对齐（幂等契约 / 批次 / 门禁）
- 兼容层需要处理 Ansible 特有概念（play / role / handler / variable precedence），复杂度高
- 兼容层可能引入安全风险（shell / command 非幂等、ignore_errors 绕过门禁）
- 长期目标是用户迁移到 LEVEELang，兼容层应可独立卸载

可选方案：

1. **独立模块（internal/compat）**：兼容层作为独立包，仅依赖 executor / plan 接口，不引入核心路径
2. **核心内嵌**：将兼容逻辑嵌入 engine / dsl 包，复用核心代码但引入耦合
3. **外部转换工具**：独立 CLI 工具将 playbook 转换为 LEVEELang，但不支持直接执行
4. **不做兼容层**：强制用户迁移到 LEVEELang，但失去"第一天能干活"的价值

## 决策

选择 **独立模块（internal/compat）** 方案，严格遵循 R8 红线。

模块边界：

1. **compat 包仅依赖接口层**：CompatLayer 接口仅依赖 Executor 和 Planner 接口，不直接依赖 engine / batch / verify 等核心包
2. **单向依赖**：compat -> executor / plan（接口），核心包不反向依赖 compat
3. **独立配置**：compat 的配置项（支持的模块列表 / 风险评估规则）在独立配置段，不影响核心配置
4. **可独立卸载**：编译时可通过 build tag 排除 compat 包，核心功能不受影响

兼容层功能范围：

1. **playbook 解析**：解析 Ansible playbook YAML 结构，映射到 LEVEE 内部执行模型
2. **模块映射**：shell / command -> shell 模块，file / copy / template -> file 模块
3. **风险评估**：静态分析 shell / command 非幂等 + ignore_errors + 无 rollback，命中标记高危
4. **审批包裹**：导入的 playbook 自动包裹审批 / 门禁 / 审计，不绕过闭环

## 后果

### 正面

- 严格遵循 R8 红线，兼容层不污染核心代码路径
- 兼容层可独立卸载（build tag），不需要兼容层的用户无需承担额外二进制体积
- 核心包的测试和演进不受兼容层影响，降低维护负担
- 风险评估在导入阶段完成，高危 playbook 自动升级审批级别

### 负面

- 兼容层仅支持 Ansible playbook 最小子集（shell / command / file / copy / template），不支持的模块需用户自行迁移
- 模块映射存在语义差异（如 Ansible 的 shell 默认非幂等，LEVEE 的 shell 需显式声明幂等契约）
- 兼容层独立维护成本：Ansible 模块生态庞大，完整兼容需要持续投入
- 风险评估为静态分析，无法覆盖运行时风险（如 shell 命令中的动态内容）

### 缓解

- 兼容层文档明确列出支持的模块子集，不支持的模块给出迁移建议
- shell / command 模块默认标记为非幂等 + 高危，用户需显式确认或补充幂等契约
- V1 阶段根据用户反馈扩展兼容层模块覆盖范围
- 长期引导用户从 playbook 迁移到 LEVEELang，兼容层作为过渡桥梁