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

## 下一步

- 阅读 [CLI 参考文档](cli-reference.md) 了解全部命令
- 配置权限矩阵和团队管理
- 使用 `levee secret` 管理加密凭据
- 通过 `levee audit export` 导出合规审计数据