# ADR-004: 哈希链审计追溯

| 字段 | 内容 |
|------|------|
| 编号 | ADR-004 |
| 标题 | 哈希链审计追溯（防篡改 trace 链式校验） |
| 状态 | 已采纳 |
| 日期 | 2026-08-16 |

## 上下文

LEVEE 的设计红线 R3 要求"变更必须可审计"：每次 apply 产出 trace + 哈希链，写入 WORM 存储，不可篡改。审计追溯需要满足：

- 每次 apply 的所有动作（建连 / 执行 / 收证 / 门禁 / 回滚）必须记录 trace
- trace 记录不可篡改，篡改可检出
- 任意 run 的 trace 可独立校验，无需依赖外部状态
- 审计记录满足合规要求（金融 / 电信 / 政企场景的审计留痕要求）

可选方案：

1. **哈希链（区块链式）**：每条 trace 的 hash 包含前一条 trace 的 hash，链式结构，篡改可检出
2. **数字签名**：每条 trace 用私钥签名，校验时用公钥验证，但密钥管理复杂
3. **WORM 存储 + 校验和**：追加只写 + 内容校验和，防篡改但无法检出中间删除
4. **外部审计系统**：trace 写入外部审计系统（如 ELK），但引入外部依赖

## 决策

采用 **哈希链 + WORM 存储 + 校验和** 组合方案。

哈希链构建规则：

1. 每个 run 的第一条 trace：`curr_hash = sha256(trace_data + genesis_seed)`
2. 后续 trace：`curr_hash = sha256(trace_data + prev_hash)`
3. `trace_data` 包含：run_id / batch_id / step_id / event / actor / detail / timestamp
4. 哈希链按 run 维度构建，不同 run 的链独立
5. 批次内 trace 按执行顺序链接，批次间通过 batch_id 关联

WORM 存储模拟：

1. SQLite 追加只写：trace 表 INSERT ONLY，无 UPDATE / DELETE 权限
2. 每条记录附带内容校验和（sha256），写入时计算，读取时校验
3. 归档（archive）操作将 trace 追加到 WORM 文件，不可修改

校验流程：

1. 按 run_id 查询所有 trace，按 timestamp 排序
2. 从第一条开始，逐条计算 `expected_hash = sha256(trace_data + prev_hash)`
3. 比较 `expected_hash` 与存储的 `curr_hash`，不一致则检出篡改
4. 校验失败返回退出码 6，输出被篡改的记录信息

## 后果

### 正面

- 哈希链提供防篡改保证：篡改任意一条 trace 会导致后续所有 hash 不匹配
- 独立校验：任意 run 的 trace 可独立校验，无需外部状态或密钥
- WORM 存储模拟追加只写，满足合规审计要求
- 实现简单，仅依赖标准库 crypto/sha256，无外部依赖
- 发布门禁 G-07 可验证：任意 run 的 trace 哈希链可独立校验通过

### 负面

- 哈希链校验为事后校验，无法防止实时篡改（需配合 WORM 存储）
- 批量 trace 校验时需全量读取，超长 run（万步以上）校验耗时可观
- SQLite 模拟 WORM 依赖应用层权限控制，非数据库级强制（有权限的应用可绕过）
- 哈希链不支持分支（同一 run 的 trace 必须是线性链），并行批次需按顺序链接

### 缓解

- WORM 存储通过应用层禁止 UPDATE / DELETE，V1 可引入真正的 WORM 文件系统
- 并行批次的 trace 按完成时间排序链接，确保链的线性结构
- 校验性能通过批量读取 + 流式计算优化，万步 trace 校验可在秒级完成
- 归档操作将 trace 写入只追加文件，作为 SQLite 之外的防篡改第二层