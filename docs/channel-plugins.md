# LEVEE 通道插件开发指南

> 适用版本：v1.14.0 及以后。本文面向要为 LEVEE 接入新执行通道（传输类型）的开发者。

LEVEE 的通道抽象层（CAL，`internal/channel`）把"如何连到目标并执行动作"与"编排/审批/回滚"完全解耦。内置通道：`ssh`、`winrm`、`local`（CI/沙箱用）。任何新传输——NETCONF、Redfish、堡垒机跳板、云厂商 API——都以插件形式接入，**不需要改动 LEVEE 核心代码**。

## 1. 你要实现什么

一个通道插件 = 一个 `channel.ChannelFactory` + 一个 `channel.Channel` 实现 + 一次 init 注册。

```go
package netconf

import (
    "github.com/nexus/levee/internal/channel"
)

// 编译期断言：接口实现错误在编译时暴露，不是运行时。
var (
    _ channel.Channel        = (*NetconfChannel)(nil)
    _ channel.ChannelFactory = (*Factory)(nil)
)

// Target 实现 channel.Target，Type() 返回你的注册键。
type Target struct{ Hostname string }

func (t Target) Host() string                      { return t.Hostname }
func (t Target) Port() int                         { return 830 }
func (t Target) Type() string                      { return "netconf" }
func (t Target) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// Factory 满足 channel.ChannelFactory。
type Factory struct{}

func (f *Factory) Create(t channel.Target) (channel.Channel, error) {
    return &NetconfChannel{target: t}, nil
}

// init 注册到进程级 registry——这一行就是接入的全部仪式。
func init() {
    channel.DefaultRegistry().Register("netconf", &Factory{})
}
```

然后让 LEVEE 的组合根 import 你的包（`import _ "yourmodule/netconf"`），inventory 里 `channel_type: netconf` 的 target 就会解析到你的通道。

## 2. Channel 接口的契约

实现 `internal/channel/channel.go` 的 `Channel` 接口，遵守以下不变量（都有测试锚定）：

| 方法 | 契约 |
|---|---|
| `Connect(ctx)` | 幂等（已连接再调返回 nil）；ctx 取消返回 `ctx.Err()` |
| `Exec(ctx, cmd)` | 非交互执行；阻塞到命令结束或 ctx 取消；返回完整 `ExecResult{ExitCode, Stdout, Stderr, Duration}` |
| `Upload(ctx, path, reader)` / `Download(ctx, path)` | 流式传输；Download 返回的 reader 的 Close 释放传输资源 |
| `Close()` | 幂等 |
| `IsConnected()` | best-effort；false ⇒ 必须先 Connect |

**关键约束**：

1. **一切方法尊重 ctx**——返回 `context.Canceled` / `context.DeadlineExceeded` 原样错误
2. **并发安全**——一个 Channel 实例可能被多 goroutine 同时 Exec（SSH 有连接池；你的实现要么无状态要么自己加锁）
3. **结构化证据**——ExecResult 的四个字段是审计链的证据来源，必须如实填充（ExitCode 拿不到时用非 0 哨兵，绝不把失败伪装成 0）

## 3. 安全底线（强约束）

LEVEE 的红线 R2/R3/R4 传导到每个通道：

- **凭据零泄露**：明文凭据只存在于内存 `CredentialRef`；不进日志、不进 ExecResult、不进 argv（SSH 用环境变量/选项文件，WinRM 用内存协议头——你的通道用传输原生的凭据机制）
- **fail-closed 默认**：任何配置缺失（known_hosts、证书、白名单）时拒绝连接而不是降级。参考 `internal/channel/ssh`：`StrictHostCheck` 默认 true，文件缺失直接报错并附修复指引（ssh-keyscan），绝不静默降级
- **命令注入面收敛**：如果通道把结构化参数拼成命令行（如 local 通道的 `os/exec`），必须有白名单/转义门禁（参考 `internal/channel/local`：程序允许列表 + 参数策略 + 沙箱根目录三重门）
- **Windows 特例**：如果你的通道涉及本地进程执行，注意 `local` 通道在 windows 上拒绝 Exec 的原因（os/exec 解析 cmd 内建/PATHEXT 无法白名单化）

## 4. 参考实现（按复杂度排序）

| 通道 | 学习点 | 位置 |
|---|---|---|
| `local` | 最小实现 + 安全门禁（允许列表/沙箱根/平台门） | `internal/channel/local/local.go` |
| `winrm` | HTTP 传输、内存凭据、非流式文件的适配 | `internal/channel/winrm/winrm.go` |
| `ssh` | 全功能参考：known_hosts、密码/密钥双认证、sudo 提权、cat 管道传输、连接池 | `internal/channel/ssh/ssh.go` |

每个包的测试（`*_test.go`）展示了如何用 mock channel 驱动你的实现——先写测试再写实现是这个项目的惯例。

## 5. 注册后会发生什么

- `levee target add <host> --channel-type netconf` 的 target 入库
- plan 阶段对 target 做 precheck（`internal/channel/precheck.go`）
- apply 阶段 `channel.DefaultRegistry().Create(target)` → 你的工厂 → Connect → 模块（shell/pkg/mysql/...）经你的通道驱动目标
- 执行证据（stdout/stderr/exit/duration）自动进入 trace 审计链与 WORM 哈希链——**你的通道不需要做任何审计动作**，闭合在引擎层

## 6. 测试要求

CI 门禁（覆盖率 70%、三 OS race、lint v2.13）对插件同等生效。最低要求：

1. 接口契约测试（Connect 幂等 / Close 幂等 / ctx 取消 / 并发 Exec）
2. 安全门禁测试（每个 fail-closed 路径一个用例，参考 `local_test.go`）
3. 真实语义测试（至少一个端到端 Exec 往返断言）

## 7. 两种接入方式（编译期 / 运行期）

本文档描述的是**编译期**通道插件：一个 Go 包 + `init()` 注册，随 LEVEE 二进制一起构建。
适合自有传输、需要与核心同步演进的情况。

**运行期**通道插件（2026-10-27 起可用）走另一条路：`internal/plugin` 的运行期插件系统
（`levee plugin install/enable`，`type: channel`）——serve 启动时把每个**启用**的 channel 型
插件注册进同一张 `channel.DefaultRegistry()`，注册键就是**插件名**，此后 inventory 里
`channel_type: <插件名>` 的 target 就解析到它。**不需要重新编译 LEVEE**（这正是本节早先版本
写作"V2 backlog"的那项能力，现已落地）。

两条路的差别：

| | 编译期（本文档） | 运行期（`levee plugin`） |
|---|---|---|
| 进程形态 | 与宿主同进程函数调用 | 常驻子进程，JSON 行协议（`internal/plugin/session.go`） |
| 会话 | 自选（ssh 用连接池） | 每 target 每次 run 一个会话进程，connect 一次、exec 多次 |
| 凭据 | 经 `Target.Credentials()` 拿扩写后的 `CredentialRef` | **只拿引用名**（`credential_ref`），明文永不过管道；密钥走插件自己的 `config.yaml` |
| 能力 | 接口全集（含 Upload/Download） | connect/exec/collect；**无 upload 动词** → `file.copy` 这类推文件的步骤在运行期通道上会按名拒绝 |
| 装卸 | 重新编译 | 安装/启用即注册；同名撞已有类型（ssh/winrm/local…）**拒绝启动** |

## 8. 已知限制

- `Target.Type()` 的返回值就是注册键，拼错不会报错而是静默查不到工厂（`Create` 返回 `no factory registered`）——集成测试必须覆盖 target→factory→channel 全链
- inventory/gRPC 层的 `channel_type` 字段**不做枚举校验**（存什么传什么），只有到 `Create` 时未知类型才暴露——新通道的 target 数据校验靠你自己的集成测试兜底
- 通道能力差异（如 local 无凭据、winrm 不支持流式大文件、运行期通道无 upload）由模块层在运行时感知，通道接口本身没有能力协商机制——模块文档需写明支持哪些通道
- 运行期通道的一个插件只提供**一种**传输类型（类型名 = 插件名）；要提供多种类型就装多个插件
- 运行期通道的会话按"每 target 每 run 一个子进程"建立，没有跨 run 的连接池——大批量目标会产生等量的子进程
