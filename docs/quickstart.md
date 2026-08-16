# LEVEE 快速开始

## 安装

### 预编译二进制

从 GitHub Releases 下载对应平台的二进制文件：

| 平台 | 架构 | 文件名 |
|------|------|--------|
| Linux | amd64 | `levee_x.y.z_linux_amd64.tar.gz` |
| Linux | arm64 | `levee_x.y.z_linux_arm64.tar.gz` |
| Windows | amd64 | `levee_x.y.z_windows_amd64.zip` |
| Windows | arm64 | `levee_x.y.z_windows_arm64.zip` |

```命令示例：下载并解压 Linux amd64 二进制
curl -sL https://github.com/nexus/levee/releases/latest/download/levee_0.1.0_linux_amd64.tar.gz | tar xz -C /usr/local/bin levee
```

```命令示例：Windows 下解压并加入 PATH
Expand-Archive levee_0.1.0_windows_amd64.zip -DestinationPath C:\Tools\levee
$env:PATH += ";C:\Tools\levee"
```

验证安装：

```命令示例：验证版本
levee version
```

### 源码编译

需要 Go 1.21+。

```命令示例：从源码编译安装
git clone https://github.com/nexus/levee.git
cd levee
go build -o levee ./cmd/levee/
```

如需注入版本信息：

```命令示例：带 ldflags 编译
go build -ldflags "-s -w -X main.version=$(git describe --tags) -X main.commitHash=$(git rev-parse HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o levee ./cmd/levee/
```

## 初始化

首次使用前运行诊断检查，确认环境就绪：

```命令示例：运行系统诊断
levee system doctor
```

输出示例：

```text
LEVEE Doctor: healthy

  config               OK
  database             OK
  permission_matrix    OK
    teams: 1
    environments: 1
```

如果诊断结果为 `unhealthy`，请根据提示修复对应项。常见问题：

- 配置文件缺失：创建 `~/.levee/config.yaml`
- 数据库不可达：检查 `database.path` 配置
- 权限矩阵未加载：创建 `permissions.yaml`

## 第一个 Workflow

### 1. 创建模板

```命令示例：创建一个 Nginx 重载模板
levee template create --name nginx-reload --content "name: nginx-reload
steps:
  - name: reload-nginx
    action: shell
    command: systemctl reload nginx
    rollback: systemctl restart nginx" --params '[{"name":"target","description":"Target host"}]'
```

### 2. 实例化 Workflow

```命令示例：从模板创建 workflow
levee new nginx-reload --params target=web01.prod
```

输出中将包含 `run_id`，后续操作均以此 ID 为准。

### 3. 查看 Workflow 详情

```命令示例：查看 run 详情
levee show <run-id>
```

## Dry-Run 预览

在正式执行前，使用 dry-run 模式预览变更计划：

```命令示例：dry-run 预览
levee plan --dry-run
```

dry-run 会展示以下信息而不实际执行：

- 涉及的目标主机列表
- 每个步骤的 action 和 rollback 命令
- 分批执行计划（batch 划分）
- 预计影响范围

## 审批与执行

### 审批

```命令示例：审批通过
levee approve <run-id>
```

可指定审批级别：

```命令示例：指定高级别审批
levee approve <run-id> --level high --comment "已确认变更窗口"
```

如需驳回：

```命令示例：驳回 run
levee reject <run-id> --reason "变更窗口已关闭"
```

### 执行

审批通过后触发执行：

```命令示例：触发 apply
levee apply <run-id>
```

紧急情况下可跳过审批强制执行：

```命令示例：强制执行（跳过审批）
levee apply <run-id> --force
```

## 查看结果

### 查看详情

```命令示例：查看 run 完整详情
levee show <run-id>
```

输出包含 run 状态、batch 执行进度、step 状态和审计 trace。

### 查看日志

```命令示例：查看日志
levee logs <run-id>
```

按主机过滤：

```命令示例：按主机过滤日志
levee logs <run-id> --target web01.prod
```

实时跟踪日志流：

```命令示例：实时跟踪日志
levee logs <run-id> -f
```

### 查看审计链

```命令示例：查看审计 trace 并验证哈希链
levee trace <run-id> --verify
```

哈希链验证通过时退出码为 0，验证失败时退出码为 6。

## 编译 Workflow

在执行前对 LEVEELang workflow 文件进行编译期类型检查，提前发现类型错误：

```命令示例：类型检查
levee compile deploy.yml
```

宽松模式下类型错误降级为警告，并输出中间表示（IR）：

```命令示例：宽松模式并输出 IR
levee compile deploy.yml --lenient --ir
```

仅做类型检查、不生成 IR：

```命令示例：仅类型检查
levee compile deploy.yml --check-only
```

## 变更日历

管理变更窗口与冻结期，避免在冻结期触发变更。

创建一个发版窗口：

```命令示例：创建变更窗口
levee calendar create --name "发版窗口" --start 2026-08-16T10:00:00Z --end 2026-08-16T12:00:00Z --targets web,batch
```

创建月末冻结期并设置 cron 重复规则：

```命令示例：创建冻结期
levee calendar create --name "月末冻结" --start 2026-08-31T00:00:00Z --end 2026-08-31T23:59:59Z --targets prod --frozen --cron "0 0 1 * *"
```

检查目标集当前是否处于冻结期：

```命令示例：检查冻结状态
levee calendar check --targets web,batch
```

列出所有窗口：

```命令示例：列出变更窗口
levee calendar list
```

## KMS 配置

查看外部密钥管理系统（HashiCorp Vault、AWS KMS）集成状态：

```命令示例：查看 KMS 状态
levee kms status
```

查看 KMS 配置（启用的 Provider、默认 Provider、降级开关）：

```命令示例：查看 KMS 配置
levee kms config
```

测试 KMS 连通性，并可选执行指定凭据的完整 GetSecret 往返：

```命令示例：测试 KMS 连通性
levee kms test

命令示例：测试凭据往返
levee kms test --name prod-ssh
```

## RBAC 配置

管理角色继承树与细粒度权限策略。

添加角色并建立继承关系：

```命令示例：添加角色
levee rbac role add --name operator --parent viewer
```

添加 `Resource × Action × Condition` 权限策略：

```命令示例：添加策略
levee rbac policy add --id p001 --resource "change:*" --action apply --effect allow --description "允许应用变更"
```

权限检查（支持基于标签的 ABAC）：

```命令示例：权限检查
levee rbac check --user zhangsan --action apply --resource change:run-001 --label env=production --verbose
```

查看角色继承树：

```命令示例：查看继承树
levee rbac tree
```

## 插件安装

安装插件（Channel/Gate/Module/Notifier 四类接口）：

```命令示例：安装插件
levee plugin install ./plugins/http-probe
```

安装并校验二进制签名：

```命令示例：安装并校验签名
levee plugin install ./plugins/http-probe --verify-signature
```

启用 / 禁用 / 查看插件：

```命令示例：启用插件
levee plugin enable http-probe

命令示例：查看插件详情
levee plugin info http-probe

命令示例：列出插件
levee plugin list
```

## ChatOps 启动

启动飞书 / 钉钉 / Slack 机器人，监听 LEVEE 事件并推送卡片消息：

```命令示例：启动飞书机器人
levee chatops start --platform feishu --config bot.json
```

通过机器人发送消息：

```命令示例：发送消息
levee chatops send --platform feishu --config bot.json --channel oc_123 --message "变更 run-001 已完成"
```

通过 ChatOps 一键审批 / 驳回：

```命令示例：一键审批
levee chatops approve --id run-abc123

命令示例：一键驳回
levee chatops reject --id run-abc123 --reason "变更窗口已关闭"
```

## Web UI 启动

启动 LEVEE Web UI（Vue 3 SPA），默认监听 8080 端口：

```命令示例：启动 Web UI
levee web
```

指定端口并代理 `/api/*` 到 gRPC-gateway 后端：

```命令示例：指定端口并代理后端
levee web --port 8080 --api http://localhost:9090
```

开发模式（代理到 Vite dev server 支持热更新）：

```命令示例：开发模式
levee web --dev
```

## gRPC 服务启动

在当前进程运行 LEVEE gRPC 服务器，暴露全部五个服务：

```命令示例：启动 gRPC 服务
levee serve
```

启用 TLS 与 Bearer token 鉴权：

```命令示例：启用 TLS 与鉴权
levee serve --addr :9090 --tls-cert server.crt --tls-key server.key --token s3cret
```

## 下一步

- 阅读 [CLI 参考文档](cli-reference.md) 了解全部命令
- 配置权限矩阵和团队管理
- 使用 `levee secret` 管理加密凭据
- 通过 `levee audit export` 导出合规审计数据

## 分布式执行 (F04)

启动 master 节点和 Agent，将变更任务分发到多台 Agent 执行：

```命令示例：启动 master 节点
levee serve --addr :9090
```

```命令示例：启动 Agent 并注册到 master
levee agent start --addr :9091 --master localhost:9090 --caps shell,file,pkg
```

查看已注册 Agent：

```命令示例：列出已注册 Agent
levee agent list
```

查看 Agent 详情与状态：

```命令示例：查看 Agent 状态
levee agent status

命令示例：查看 Agent 详情
levee agent show <agent-id>
```

移除 Agent：

```命令示例：移除 Agent
levee agent remove <agent-id>
```

## 多租户 (F07)

创建和管理租户，实现租户隔离与资源配额：

```命令示例：创建租户
levee tenant create --name acme --display "ACME Corp" --max-targets 100 --max-changes 10
```

查看租户列表与详情：

```命令示例：列出租户
levee tenant list

命令示例：查看租户详情
levee tenant show <tenant-id>
```

查看租户使用量与配额：

```命令示例：查看使用量
levee tenant usage <tenant-id>

命令示例：调整配额
levee tenant quota <tenant-id> --max-targets 200 --max-changes 20
```

暂停与恢复租户：

```命令示例：暂停租户
levee tenant suspend <tenant-id> --reason "合规审查"

命令示例：恢复租户
levee tenant resume <tenant-id>
```

## 配置漂移检测 (F10)

检测和管理配置漂移，定期巡检目标机配置一致性：

```命令示例：从最近 apply 自动生成基线
levee drift baseline auto --host web-01
```

检测漂移：

```命令示例：检测漂移
levee drift detect --host web-01 --baseline auto
```

添加定期巡检调度：

```命令示例：添加每日凌晨巡检
levee drift schedule add --name daily-check --cron "0 2 * * *" --hosts web-01,web-02
```

立即触发一次巡检：

```命令示例：立即触发巡检
levee drift schedule run <schedule-id>
```

查看漂移报告与趋势：

```命令示例：查看 30 天漂移报告
levee drift report --host web-01 --days 30
```

## 移动端审批 (F12)

配置推送通知和移动端审批，支持 iOS（APNs）与 Android（FCM）：

```命令示例：配置 APNs
levee push config --platform ios --key AuthKey.p8 --key-id ABC123 --team-id TEAM456
```

```命令示例：配置 FCM
levee push config --platform android --project my-project --service-account sa.json
```

注册移动设备：

```命令示例：注册 iOS 设备
levee push register --user alice --token <device-token> --platform ios
```

发送审批推送通知：

```命令示例：发送审批推送
levee push send --user alice --title "审批请求" --body "变更 run-123 待审批" --deep-link "levee://approval/run-123"
```

测试推送配置：

```命令示例：测试推送
levee push test --user alice
```

列出用户已注册设备：

```命令示例：列出用户设备
levee push devices --user alice
```