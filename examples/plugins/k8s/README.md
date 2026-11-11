# k8s — 通道插件（Kubernetes 变更通道）

LEVEE 四类插件里 `channel`（会话型）档位的第二个真示例，也是 D-4
（`docs/design-k8s-change-governance.md`）S2 的**执行面**：把一个 Kubernetes
集群变成 LEVEE 可拨的 `channel_type`，让"改一个工作负载"走与 SSH 主机变更
完全相同的治理链——审批、快照回滚、WORM 审计。

协议是 JSON 行（stdin 一行请求、stdout 一行应答），与其他三档共用同一套宿主
实现。**核心零行改动**：传输注册、快照回滚、审批、审计全部走既有机制。

## 它演示了什么

| 协议动词 | 本插件做什么 |
| --- | --- |
| `meta` | 声明身份（`type: channel`） |
| `init` | 读取 `config.yaml`：kubeconfig 路径、允许的 context、动词下限、超时 |
| `connect` | 把会话绑定到一个 kube **context**（来自 target 行的 address，必须在 `allowed_contexts` 里）；**明文凭据不过管道**——kubeconfig 在插件自己的 config 里，宿主只传 `credential_ref` 名字 |
| `exec` | 只认两种命令形态（见下）；kubectl 以**参数向量**直跑，**不过 shell** |
| `collect` | 读一个资源的 JSON（宿主的 Download） |
| `upload` | 把一个 manifest 用 `kubectl apply -f -` 应用上去（宿主的 Upload）——这也是快照的**还原半边** |
| `disconnect` / `close` | 无会话状态可拆（kubectl 每次一发即弃） |

## exec 的两种命令形态

```text
kubectl <verb> [args...]     # 一次工作负载变更（动词须在允许列表内）
base64 <resource>            # 宿主的快照抓取惯用语（见下）
```

**为什么快照抓取长这样**：LEVEE 核心的快照器（`internal/wiring/snapshotter.go`）
对任何通道只说两句话——`Exec("base64 <path>")` 抓状态、`Upload(path, 内容)`
写回。对 k8s 目标，"path" 就解释成**资源引用**（`deployment/web`）：

- 抓取 = `kubectl get <ref> -o json`，**剥掉服务器记账字段**
  （resourceVersion / uid / generation / creationTimestamp / managedFields /
  status）后 base64——重放这些字段会让还原撞冲突而不是还原；
- 还原 = `kubectl apply -f -`（内容就是抓到的字节）。

于是 **"回滚＝按快照还原（不是重放一条反向命令）"在核心零改动下成立**：
工作流步骤声明 `rollback: {strategy: snapshot, snapshot_paths: ["deployment/web"]}`
即可，核心照常走它的快照器，本插件负责把那两句话翻译成 kubectl。

## 安全下限（传输自身的地板）

1. **目标钉死**：kubeconfig、context 来自插件 config 与 target 行；调用方
   在命令里带 `--kubeconfig` / `--context` / `--server` / `--namespace` /
   `-n` / `--token` / `--as` 等改写目标的旗标一律**拒绝**（不是静默覆盖）。
2. **动词白名单**：默认只放行改/读已声明工作负载的动词。会把通道变成逃生口
   的动词**不在默认集里**：`exec`/`attach`/`debug`/`run`/`cp`（集群内任意代码
   执行或文件外带）、`port-forward`/`proxy`（绕过上面所有门的隧道）、
   `auth`/`create token`/`certificate`（凭据铸造）、`config`/`edit`（改操作者
   自己的 kubeconfig）。要放行是运维在 config.yaml 里的显式决定，审计可见。
3. **无 shell**：命令按引号感知的 tokenizer 切成 argv 直跑；shell 元字符
   （`; | & $ `` ` `` > < 换行）直接拒绝——插件与工作流对命令含义的理解
   不允许出现分歧。
4. **明文凭据不过管道**：kubeconfig 路径在插件自己的 config.yaml 里。

## 装与试

```bash
# POSIX（entry_point 是 k8s）
go build -o examples/plugins/k8s/k8s ./examples/plugins/k8s
# Windows（产物须与 entry_point 同名，Windows 不给 -o 补 .exe）
go build -o examples/plugins/k8s/k8s.exe ./examples/plugins/k8s
#   并把 plugin.yaml 的 entry_point 临时改成 k8s.exe（安装期按文件名 stat）
```

然后（先编辑 `config.yaml` 把集群 context 加进 `allowed_contexts`，**空列表
拒绝一切**）：

```bash
levee plugin install ./examples/plugins/k8s
levee plugin enable k8s
levee target add kind-levee --channel-type k8s --address kind-levee-s2
```

workflow 里写（工作流示例，与 `docs/design-k8s-change-governance.md` S2 验收
场景一致）：

```yaml
steps:
  - name: scale-web
    module: shell
    action: exec
    args:
      cmd: kubectl scale deployment web --replicas=3
    approval:
      level: high
      approvers: [sre-oncall]
    rollback:
      strategy: snapshot
      snapshot_paths:
        - deployment/web
```

> 插件通道在 **serve 启动时装入**（`installPluginChannels`）；`levee serve
> --engine-enabled` 之后经 REST/gRPC 走 create→plan→approve→apply。
> CLI 一次性路径（`levee apply --engine-enabled`）目前不装插件通道——见
> 设计文档的"已知边界"。

## 三条必须知道的边界

1. **exec 无 stdin**：通道协议的 exec 只带命令串。要"应用一份 manifest"，
   走 `upload`（宿主的 Upload，即快照还原的通道）；`kubectl apply -f -`
   在 exec 形态里会立刻读到 EOF 而失败——这是有意的（不给宿主侧任意文件
   读取留口子）。
2. **`levee target check` 对 k8s 目标会报不可达**：核心探针是 `echo ok`，
   集群没有 shell 可答。这只影响那条诊断命令；plan/apply 不做该探针。
3. **一个 context 一个命名空间**：命名空间来自 kubeconfig 的 context
   （运维放凭据的地方本就该管它），调用方不可改。多命名空间 = 多条
   target 行 + 多个 context 条目。
