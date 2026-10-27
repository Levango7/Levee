# tcp-console — 通道插件示例

LEVEE 四类插件里唯一一种**会话型**消费点（`channel`）的最小真示例：把一台
行协议 TCP 控制台（设备、串口桥、终端服务器）变成 LEVEE 可拨的
`channel_type`。协议是 JSON 行（stdin 一行请求、stdout 一行应答），与其他三档
共用同一套宿主实现。

## 它演示了什么

| 协议动词 | 谁调用 | 本示例做什么 |
| --- | --- | --- |
| `meta` | serve 启动时（一次性） | 声明身份（`type: channel`） |
| `init` | 每次会话开始 | 读取 `config.yaml`：连接/读超时、终止符 |
| `connect` | 拨号时 | 按 `host`/`port` 建 TCP 连接；**参数里没有明文凭据**，只有 `credential_ref` 引用名 |
| `exec` | 每个步骤 | 写命令行、读到终止符；超时记 `exit_code 1`（跑了但没答），传输错记 `ok:false`（没跑成） |
| `collect` | 取文件时 | **如实拒绝**：行协议没有文件语义 |
| `disconnect` / `close` | 会话收尾 | 关连接、退出 |

**会话是这一档的全部意义**：宿主在 connect 时启动本进程，之后多次 `exec` 走
同一根管道与同一个 TCP 连接，直到 Close 才收尾。把连接存在变量里就是正确的
写法——每步重连会把传输的会话语义丢掉。

## 装与试

终端 1——五行的玩具控制台（每行都答 `OK <line>`）：

```python
python -c "
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', 9999)); s.listen(4)
while True:
    c,_=s.accept(); f=c.makefile('rwb')
    for line in f: f.write(b'OK '+line); f.flush()
"
```

终端 2：

```bash
go build -o examples/plugins/tcp_console/tcp-console ./examples/plugins/tcp_console
levee plugin install ./examples/plugins/tcp_console
levee plugin enable tcp-console
levee target add console-1 --channel-type tcp-console --host 127.0.0.1 --port 9999
```

workflow 里写：

```yaml
steps:
  - name: show-version
    module: shell
    action: exec
    args:
      cmd: show version
```

## 三条必须知道的边界

1. **明文凭据永不过管道**：插件拿到的 connect 参数只有 `host`/`port`/`type` 和
   `credential_ref`（引用**名**）。要认证就在 `config.yaml` 里放密钥，或在这里
   写登录行——宿主不会把 LEVEE 凭据库里的口令交给插件。
2. **没有 upload 动词**：宿主的运行期通道接口只有 connect/exec/collect/
   disconnect，所以 `file.copy`、`file.template` 这类推文件的步骤在本通道上会
   被按名拒绝（要推文件用内建 ssh/winrm，或写编译期通道插件）。
3. **一次会话一个进程**：每个 target 每次 run 起一个本进程，会话结束后回收，
   没有跨 run 连接池。
