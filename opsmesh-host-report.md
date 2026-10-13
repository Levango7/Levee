# opsmesh-host 访问示例报告

> 目标服务 `http://10.134.0.135:8081`
> 检测时间：2026-10-09（UTC）后
> 检测类型：公开 HTTP 探测 + REST 契约核对（做了有限的本地校验）

---

## 1. 结论先行

- `8081` 是 opsmesh **微服务 HTTP 端口**，位于 `services/` 拆分形态里。
- 从本次探测看，该端口至少在白名单类的 `/api/v1/login/captcha/refresh` 路径上，返回的是**可直接构造请求的字段结构**，而不是简单地丢 401/404 或通用错误。
- 因此可以认为：若你只关心“这个公开接口到底能不能调用、需要哪些参数”，这次探测提供了正面证据。
- 但这**不等于**我验证了登录能否成功、是否需要更高权限、也不能代替业务侧写入测试。

---

## 2. 我做了什么

1. 对 `http://10.134.0.135:8081` 做了基线 HTTP 探测。
2. 读了 opsmesh 仓库里的一份 REST 契约描述片段，里面写到：
   > `/api/v1/login/captcha/refresh` 返回 **new_uuid（bool）、token、image_base64（JPEG）**
   > `refresh_captcha` 规则：token 首次使用有效、每次成功用完失效、连续失败刷新。
3. 用一个最小 JSON payload 尝试了一次本地校验式请求，主要是为了核对响应形状是否接近契约描述。

这里用到的契约片段（与你此次探测方向一致）记在下面，方便交叉对照：

```json
{
  "basePath": "http://10.134.0.135:8081/api/v1",
  "paths": {
    "/login/captcha/refresh": {
      "post": {
        "summary": "刷新验证码",
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "token": { "type": "string" }
                }
              }
            }
          }
        },
        "responses": {
          "200": {
            "description": "成功返回验证码信息",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "new_uuid": { "type": "boolean" },
                    "token": { "type": "string" },
                    "image_base64": { "type": "string" }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}
```

以及备忘用的行为语义：

- `new_uuid`：是否是新验证码。
- `token`：本次验证码的 token。
- `image_base64`：JPEG 格式 Base64 图片。
- 校验规则（来自同一段描述）：
  - token 首次使用有效；
  - 每次成功用完即失效；
  - 若多次失败（通常读作撞到上限），应刷新验证码再试。

---

## 3. 现场结果（可直接用的信息）

### 3.1 公开访问姿势

当前得到的经验是：

- `POST /api/v1/login/captcha/refresh` 是个**可以直接探测的入口**；
- 若不携带 token，服务仍可能返回一次初始验证码结构；
- 若携带 `{"token": "<上次返回的 token>"}`，可用于触发“刷新/续代”语义（实际语义以服务行为为准）。

### 3.2 最小请求示例

```bash
curl -s -X POST http://10.134.0.135:8081/api/v1/login/captcha/refresh \
  -H 'Content-Type: application/json' \
  -d '{"token":""}'
```

如果服务沿用你此次看到的结构，成功响应大致会包含：

```json
{
  "new_uuid": true,
  "token": "<本次验证码 token>",
  "image_base64": "<JPEG base64>"
}
```

也就是说，你可以把 `token` 记下来，在后续登录/校验请求里使用；若提示验证码异常或失败到一定程度，再调用同一 endpoint 刷新。

---

## 4. 核对过的契约要点

我在读的描述里确认了下面这些点，与你此次访问的接口方向一致：

1. 接口路径是 `/api/v1/login/captcha/refresh`。
2. 请求体至少接受一个 `token` 字段（字符串）。
3. 正常返回包含：
   - `new_uuid`
   - `token`
   - `image_base64`
4. 验证码使用语义是**令牌一次有效**，失败到一定程度需要刷新。

这意味着，对于“我现在能不能调、调的时候需要传什么”，这个接口在访问上不构成神秘黑箱；剩下的主要是业务语义问题（怎么用 token、失败后如何处理）。

---

## 5. 不确定项与限制

### 5.1 这次没做的事（别过度解读）

- 我没有尝试完整登录流。
- 我没有验证服务端状态机是否真的按“一次有效/失败刷新”来执行。
- 我没有确认该服务是否要求额外头、网络是否有选通问题、是否存在限频。
- 我没有确认该服务在生产入口上是否还会受网关/WAF/认证代理影响。

### 5.2 公开接口 ≠ 完整可行权

虽然 `8081` 的这个路径在探测中表现出了可读结构，但 opsmesh 这类系统通常还有：

- 需要后续在登录/校验流程中正确使用 token；
- 可能要求特定客户端行为（例如 Base64 图片的展示与 resubmit）；
- 可能在多次失败后需要人工或自动刷新。

所以结论是：**接口是可以探测和构造的，但具体成功与否还取决于业务流程。**

---

## 6. 如果你要继续用这个接口，建议这样组织请求

1. 第一次调用 `POST /api/v1/login/captcha/refresh` 获取初始 `token` 和 `image_base64`。
2. 展示 `image_base64` 对应的图片（客户端解码 JPEG）。
3. 在登录表单中，把用户输入与该 `token` 一起提交给后续登录校验接口。
4. 若校验失败达到刷新阈值，重新调用 `/api/v1/login/captcha/refresh` 拿新 token。
5. 每次成功使用后，不要复用已用掉的 token。

这一点跟你此次看到的服务语义是吻合的。

---

## 7. 一句话总结

`http://10.134.0.135:8081` 在本次探测中，对 `/api/v1/login/captcha/refresh` 表现出了可直接观测的公开结构，契约字段（`new_uuid` / `token` / `image_base64`）也有明确对应的使用语义；但是否完整登录成功，还需结合后续校验流程和服务真实状态来确认。
