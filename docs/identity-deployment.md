# 身份与授权：部署手册

> 这份文档回答一个问题：**把 LEVEE 交付到一个企业里，身份这件事实话说到什么程度。**
> 结论先说：三层都在，且都是接线的真实实现，不是占位——
>
> | 层 | 是什么 | 在哪里 |
> | --- | --- | --- |
> | 认证（你是谁） | OIDC JWT 验签 / GitHub OAuth / 命名令牌 | `internal/auth`（oidc.go / github.go / session.go） |
> | 会话（浏览器怎么登录） | PKCE 授权码流 + 回调换票 | `web/src/api/sso.ts`、`/system/auth-info`、`/login/callback` |
> | 授权（你能干什么） | 矩阵 × 角色树 × ABAC 策略，三层合成 | `internal/permission`、`internal/authz` |
>
> 本文按"先选认证来源 → 再填三张表 → 最后逐条验证"的顺序写，所有键名与命令都可直接照抄执行。

---

## 1. 先决定认证来源：三种，按部署对象选

LEVEE 支持四种凭据，**按 `AuthTokens.Resolve` 的真实顺序逐个尝试，先中者得**（`internal/grpc/auth.go`）：

1. `Legacy` —— 单一共享令牌（`--token`），不带主体，向后兼容旧部署；
2. `Named` —— 命名令牌（`--auth-token name=secret`），命中即注入主体与租户；
3. `Sessions` —— LEVEE 本地登录票（SSO 流程签发的会话令牌，今天是 GitHub 登录）；
4. `OIDC` —— 形如 JWT 的 bearer token，用 IdP 的 JWKS 验签。

三者可同时开启；静态令牌与 OIDC 同时配置时，一个恰好长得像 JWT 的静态令牌会先被 Named 命中——**这是特性不是漏洞**（静态令牌是你自己配的，优先级最高），但别把静态令牌设成 JWT 形状。

### 1.1 客户已有 IdP（最常见，推荐）

用 **OIDC**。任何符合标准的提供商都能用，不需要改代码：Keycloak、Zitadel、Entra ID（Azure AD）、Okta。

```yaml
auth:
  oidc:
    enabled: true
    issuer_url: "https://idp.example.com/realms/levee"   # 发现端点 = <issuer>/.well-known/openid-configuration
    client_id: "levee-console"
    audience: ""                # 缺省等于 client_id；IdP 发的 access token 有独立 aud 时才填
    username_claim: "preferred_username"   # 主体名；回落 email → sub
    role_claim: "groups"        # 携带角色/组的 claim，接受 string / []string / 逗号分隔串
    role_map:                   # 把 IdP 的组名映射成 LEVEE 角色名，未列出的原样通过
      levee-admins: sre
      levee-operators: operator
      levee-viewers: viewer
    tenant_claim: ""            # 多租户部署才填；空 = 单租户
```

**两个必须知道的约束：**

- **多租户开启时必须配 `tenant_claim`**。`serve` 启动时若发现 OIDC 启用、多租户开启而该 claim 为空，会告警并要求二选一——因为 token 里没有租户声明时，下游只能拒绝而不是猜。
- **浏览器登录需要 IdP 允许跨域**。控制台是 SPA，用 PKCE 授权码流在浏览器侧直接换 token（`web/src/api/sso.ts`），所以 IdP 必须把这个来源加进允许的 Web 来源。换成"后端代换 token"的 BFF 形态可以免掉 CORS，但那是一次产品决定，当前未做。

### 1.2 客户只有 LDAP / AD，没有 OIDC 端点

**不要为它手写 LDAP 对接。** 标准做法是在 LEVEE 前面加一层 Keycloak：LEVEE ↔ Keycloak 走 OIDC，Keycloak ↔ AD 走 LDAP/Kerberos。对 LEVEE 而言什么都没变，对客户而言登录仍是他们原来的账号体系。信创环境里这一层通常还要考虑国密算法，那是 Keycloak 侧的选型问题。

### 1.3 没有 IdP：命名令牌 + 显式主体

```bash
levee serve --token <共享密钥> \
  --auth-token alice=<tok-a> \
  --auth-token ci-bot=<tok-ci>
```

命名令牌认证后，**其主体注入请求上下文并优先于客户端自报的 `X-Acting-As`**——审计归属记的是"被证明的身份"，不是"客户端断言的身份"。`--token` 单令牌不留主体，行为与旧版完全一致。

> **启动门禁**：未配置任何凭据、又没显式 `--insecure` 时，`serve` 拒绝启动。这不是严格，是防止一次忘记配 token 的部署把整个 API 敞开。

---

## 2. 再填三张表：主体 → 团队角色 → 环境动作

授权是三个文件，都在数据目录下，**缺省拒绝**。

```
<dataDir>/users.yaml        主体登记：这个人属于哪个团队、担任何种角色
<dataDir>/permissions.yaml  矩阵：哪个团队在哪个环境能做什么（WHERE）
<dataDir>/roles.yaml        角色树：这个角色被授予哪些权限（WHAT）
<dataDir>/policies.yaml     ABAC 策略：带标签条件的 allow/deny（可选）
<dataDir>/alert-sources.yaml 告警入口身份：哪个监控源在哪个环境、凭据是什么（可选）
```

`alert-sources.yaml` 与前三张表不同：它登记的不是人，而是机器（监控源）。凭据证明“哪个源在说话”，登记项决定“它的告警属于哪个环境”，因此告警流的可见性（`view`）有可判的输入。缺文件 = 告警入口退回只认证的旧姿态：

```yaml
sources:
  - name: prometheus-prod
    environment: prod
    secret: <per-source-token>
```

### 2.1 `users.yaml`——先让人存在

```yaml
users:
  - name: liwg
    team: platform
    role: sre
  - name: zhangs
    team: dba
    role: operator
  - name: ops-bot
    team: platform
    role: automation
```

文件缺失是合法状态（"还没登记任何人"），**但配了矩阵之后，未登记主体一律拒绝**——"我不认识你"必须落在拒绝那一侧，否则配矩阵等于没配。

### 2.2 `permissions.yaml`——矩阵说"在哪里能动手"

```yaml
teams:
  - name: platform
    environments:
      - name: "*"            # 生产、预发、测试全部
        actions: [view, plan, apply, rollback, pause, resume, cancel]
  - name: dba
    environments:
      - name: prod
        actions: [view, approve]        # 生产上只有审批权，apply 归平台组
      - name: "*"
        actions: [view, plan, apply, rollback]   # 其他环境全流程
```

动作词表（`internal/permission/matrix.go`）：`plan` `apply` `approve` `rollback` `pause` `resume` `pause_all` `resume_all` `cancel` `view` `admin`。`admin` 是超集，覆盖其余全部。

两条硬规则，加载时就执行：

- **显式 revoke 压过一切**，包括 admin 超集本身。给某个团队某个环境 revoke 掉 `apply`，再给它的角色授 `apply` 也不会生效——这是唯一能让"角色授权"和"环境隔离"同时成立的口子。
- **给 `*` 环境授 `admin` 会告警**：那让这个团队在当前和未来所有环境里全能，几乎总是配置事故。是 WARN 不是 ERROR——单运维的部署真心想要时可以被允许。

### 2.3 `roles.yaml`——角色树说"能做什么"

```yaml
roles:
  - name: viewer
    permissions: [view]
  - name: operator
    parent: viewer
    permissions: [plan, apply, rollback]
  - name: sre
    parent: operator
    permissions: [approve, pause, resume, cancel]
  - name: automation
    parent: operator
    permissions: []          # 只继承父级，不额外授权
```

### 2.4 两轴为什么必须分开

矩阵和角色树不是重复设计，拆开是有原因的：

- **只用矩阵**：每个"团队×环境"组合都要把每个动作拼一遍，新增一个动作要改每一行。
- **只用角色**：角色授权跟着人走，于是"operator"在 dev 是 operator，在 prod 还是 operator——**这个产品卖的环境隔离就没了**。

合成规则（`internal/authz/authz.go`，一行）：

```
allowed = 矩阵放行(团队, 环境, 动作)
       || (角色授予该动作
           && 该团队在这个环境至少有任意一个动作授权     ← 边界：角色不能把团队带进它进不去的环境
           && 该动作未被显式 revoke)
```

最后一条不是装饰：矩阵文档明确 revoke 压过一切，合成时忽略它，角色就能悄悄重新授权一条运维特意禁掉的动作。

---

## 3. 列表读路径怎么收窄（这是最容易被问住的一点）

**读不被拒绝，被收窄。** `view` 命中后，`ListChanges`/`ListTargets`/`GetAuditLog` 等返回的是"这个主体可见环境"的子集，而不是 403——拒绝整个页面会把该看的行也藏了，把权限问题变成一个空看板。

聚合数同样收窄：`/system/status` 里的 `activeRuns` 计的是**可见环境内**的运行数（"你的 prod 里此刻有 2 个变更在跑"是关于别人变更的信息）。不可归属的调用者（共享令牌、无可证身份）读放行、写拒绝——这个豁免是继承来的：给同一个令牌的所有持有者逐行过滤，什么也藏不住，只会把控制台变暗。

---

## 4. 验收：三条命令，逐条可复现

配完之后不要凭感觉，用 CLI 问出和 RPC 完全相同的答案：

```bash
# 1. 当前策略的总体状态（是否强制、登记了哪些主体）
levee authz status --json

# 2. 逐条问诊：这个主体在这个环境能不能做这个动作
levee authz explain --subject liwg --env prod --action apply
levee authz explain --subject zhangs --env prod --action apply     # 预期：拒绝，原因写明团队在 prod 无 apply 授权
levee authz explain --subject liwg --env prod --action cancel
```

拒绝的报错文案本身是可操作的，会直接把自助命令带上：

```
approve denied for subject "zhangs" on env "prod" action "approve":
  <原因> (see `levee authz explain --subject zhangs --env prod --action approve`)
```

**拒绝有留痕（SA-007）**：每次被拒的请求在返回前写一条 audit 行，`action = permission.denied`，`target` 形如 `<被拒动作>@<环境>`，`result = denied`。查法：

```sql
-- 直接查库（sqlite / pg 同构）
SELECT actor, target, result, timestamp FROM audit
 WHERE action = 'permission.denied' ORDER BY timestamp DESC;
```

> 如实记一笔：**`levee audit` 的四个子命令（`verify` / `export` / `list <run-id>` / `show <trace-id>`）不支持按 action 过滤**，所以上面这条要么走 SQL、要么 `export` 出来再筛。CLI 不过滤是已知缺口，不是本文档漏写。

拒绝的报错文案本身是可操作的，会直接把自助命令带上：

```
approve denied for subject "zhangs" on env "prod" action "approve":
  <原因> (see `levee authz explain --subject zhangs --env prod --action approve`)
```

---

## 5. 一份可直接用的最小交付样例

假设客户要三种人：平台组全流程、DBA 只在生产审批、外部审计只读。

```yaml
users:
  - { name: liwg,   team: platform, role: sre }
  - { name: zhangs, team: dba,      role: operator }
  - { name: audit,  team: audit,    role: viewer }
```

```yaml
teams:
  - name: platform
    environments:
      - name: "*"
        actions: [view, plan, apply, rollback, pause, resume, cancel, approve]
  - name: dba
    environments:
      - name: prod
        actions: [view, approve]
      - name: "*"
        actions: [view, plan, apply, rollback]
  - name: audit
    environments:
      - name: "*"
        actions: [view]
```

```yaml
roles:
  - { name: viewer,    permissions: [view] }
  - { name: operator,  parent: viewer, permissions: [plan, apply, rollback] }
  - { name: sre,       parent: operator, permissions: [approve, pause, resume, cancel] }
```

验收预期：`zhangs` 在 prod 能 approve 不能 apply；`zhangs` 在 staging 能 apply；`audit` 在任何环境都只读；一个没登记的用户被一切写动作拒绝。

---

## 6. 交付时如实说明的边界

写进交付说明，不要等客户问：

1. **审批人身份以认证主体为准，不接受自报。** `ApproveChange` 用 `requireSubject(ctx)` 取身份；请求体里带的 `approver` 若与认证主体不一致，直接 PermissionDenied，而不是悄悄覆盖——客户端无法把"用了我的身份"误读成"我的身份被忽略"。
2. **未配矩阵 = 不强制。** 没有任何矩阵时所有放行，`serve` 启动时会把这个姿态打出来，不逐请求悄悄决定。**投产必须配矩阵**，否则身份只是登录，不是授权。
3. **`users.yaml` 是本地文件，不进 IdP。** 主体→团队→角色的对应目前在数据目录里维护；IdP 只提供"你是谁、属于哪些组"。把它做成从 IdP 组同步是一次产品决定（已列待办），当前状态是手工维护。
4. **Audit 链能查出篡改，挡不住删除**——删中间一行会让后一行的前驱哈希无来源。WORM 触发器是配套而非冗余（`TestAuditChain_DetectsDeletion` 先摘触发器再删，验证必须报 mismatch）。
5. **超大审计库的封链读成本**：`Seal` 每次全量读可见 audit 行（无 LIMIT），稳态只写增量，但读成本随日志增长。百万行以上的部署需要分批判封，这是已知限制。
6. **告警入口有身份、告警流按归属判可见性。** `ReceiveAlert` 在部署了 `alert-sources.yaml` 时要求每源凭据（gRPC metadata `x-levee-alert-source` + `x-levee-alert-token`，REST 同名头），凭据证明“哪个监控在说话”，登记项决定“它的告警属于哪个环境”——归属来自注册表，不接受载荷自报；未登记 / 错凭据 / 无凭据一律 Unauthenticated，且被拒的告警不进 ring、不进流。`GetAlertStatus` / `SubscribeAlerts` 按告警**自己的**环境要 `view`（流按可见环境收窄而非整条拒绝）。为什么不由矩阵直接判载荷：`AlertMessage` 没有 team/env/可解析 target，两条捷径都在说谎（按 `permission.default_env` 判等于把监控源归给恰拥有默认环境的那支队；新增 `alert` 动作 token 在每份既有矩阵上 fail-open）。未配 `alert-sources.yaml` 时保持历史姿态（只认证、启动 WARN），配置了却写坏则拒绝启动（与矩阵/策略同口径）。例外表因此清空——`AlertService` 已归入 `cmd/levee/serve_policy_wiring_test.go` 的受控集。
7. **没有环境的请求面按默认环境判，而且不是唯一一次判定。** 对话面的四个入口（gRPC `SendMessage` / `SubscribeConversation`，REST `/conversation/sessions*`）的请求里都不带环境，于是按 `permission.default_env` 判——这是运维在配置里刻意选的那个回退，不是猜出来的。它管的是"这个主体能不能驱动这场对话、能不能花这份 LLM 预算、能不能订阅回执"；对话唯一能触发的变更动作（`CreateChangeDraft` → `ChangeService.CreateChange`）会带着**变更自己的环境**再判一次，所以这里的放行永远替代不了真实环境上的授权。
8. **有环境的资源按它自己声明的环境判，未登记的直接拒。** 诊断是在目标机上用平台凭据执行采集，所以 `Diagnose` 按 `labels["env"]` 解析出的那个环境要 `apply`，`GetDiagnosis` 按报告目标的环境要 `view`；不在清单里的主机**拒绝**（没有可归属的环境，就没有在它上面执行许可以），已登记但没打标签的落回默认环境（配置里的答案）。解析器没装上时返回 `FailedPrecondition` 而不是退回默认环境——半装的接线要被点名，不能悄悄按别人没问过的环境判出结论。**代价也说明白**：`GetDiagnosis` 必须先解析 id 才知道该判哪个环境，于是判定发生在查找之后，调用方能区分"没有这份报告"与"不是你能读的"——报告 id 因此是一个弱存在预言；它们是引擎自造而非可枚举的，这是两个错里较轻的那一个。
