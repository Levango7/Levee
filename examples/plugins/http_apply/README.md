# http-apply — 模块插件示例

LEVEE 四类插件中最深的一类（`module`）的最小真示例：把一次「对厂商 REST API 的
幂等变更」暴露成 workflow 里可书写的执行模块。协议是 JSON 行（stdin 一行请求、
stdout 一行应答），与 `plugin test`、notifier、gate 三档共用同一套宿主实现
（`internal/plugin/protocol.go`）。

## 它演示了什么

| 协议动词 | 谁调用 | 本示例做什么 |
| --- | --- | --- |
| `meta` | serve 启动时（一次性） | 声明身份 + 模块面：`details.actions: ["apply","destroy"]`（必填）、`details.idempotent: true` |
| `init` | 每次执行的第一步 | 读取 `plugin install --config` 存下的 `timeout_seconds` / `auth_token` |
| `validate` | 每次执行，**execute 之前** | 拒绝缺 `args.url`、拒绝非 http(s) 绝对地址、拒绝未声明的动作 |
| `execute` | 每次执行 | `apply`→PUT、`destroy`→DELETE；2xx 记 `exit_code 0`，非 2xx 记 `exit_code 1` |
| `close` | 每次执行的最后 | 干净退出 |

## 三种结果的分工（照抄这条边界，别混）

- **`ok:false`** = 动作根本没能跑（建请求失败、连接失败、配置缺失）。宿主把它
  当步骤错误，**不会**当成「执行了但失败」。
- **`ok:true` + `exit_code != 0`** = 动作跑了并失败（厂商 API 返回 4xx/5xx）。步骤
  同样失败，但审计里留下的是退出码与 stderr，而不是「没跑成」。
- **`ok:true` + 无 `module` 结果** = 无 verdict。宿主的适配器**判失败**
  （空成功不是成功）。

## 装与试

插件目录里的 `config.yaml` 是约定名：`plugin install` 会把它**原样**存进注册表
（没有 `--config` 旗标），之后每次执行的 `init` 请求把这份配置交给插件。
`entry_point` 指向目录里的二进制文件名，所以先构建到那个名字：

```bash
go build -o examples/plugins/http_apply/http-apply ./examples/plugins/http_apply
levee plugin install ./examples/plugins/http_apply
levee plugin enable http-apply
levee plugin test http-apply        # 活的协议握手，确认 meta/close 都答得上来
```

workflow 里写：

```yaml
steps:
  - name: provision-bucket
    module: http-apply
    action: apply
    args:
      url: https://api.example.com/v1/buckets/demo
```

## 两条必须知道的边界

1. **模块自持传输**：插件模块声明 channel-free，宿主**不为它拨 SSH/WinRM**，
   库存行只作为寻址数据传入（`params.target:{host,port,type}`）。所以 inventory
   里那台机器连不上，也不影响本模块执行——这是它存在的理由，不是漏洞。
2. **不可逆要靠作者声明**：插件动作不在编译期「固有不可逆」词表里（那张表是
   静态的）。真正破坏性的插件动作（比如 `destroy`）必须在 step 上写
   `irreversible: true`，否则不会触发不可逆级别的审批要求。