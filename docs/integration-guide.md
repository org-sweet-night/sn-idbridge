# IdBridge 接入与集成指南

本文说明两类集成：

1. IdBridge 接入企业身份源，例如飞书。
2. 业务应用接入 IdBridge，使用 OIDC 完成统一登录。

当前版本的部署和路由边界：

| 范围 | 路径 |
|---|---|
| 管理后台 | `/admin/*` |
| 用户门户 | `/portal` |
| 用户与登录 API | `/api/*` |
| 管理 API | `/sapi/*` |

管理员账号和普通用户账号隔离：

- 管理员账号在 `admin_users`，只能通过 `/admin/login` 或 `/t/{entity}/admin/login` 登录。
- 普通用户在 `users`，通过 `/portal`、`/login`、`/auth/continue` 或应用登录流进入。
- `/` 不暴露管理员入口。用户登录和企业后台管理员登录是两套独立账号体系。

## 1. 身份源接入

### 1.1 当前支持状态

| 身份源 | 状态 | 说明 |
|---|---|---|
| 飞书 | 可用 | 支持登录、目录同步、组织架构、工作台 SSO |
| 企业微信 | 规划 | 产品边界保留，当前版本未开放配置 |
| 钉钉 | 规划 | 产品边界保留，当前版本未开放配置 |
| LDAP | 规划 | 产品边界保留，当前版本未开放配置 |
| 本地账号 | 兜底 | 只作为登录兜底，不作为主身份源 |

同一公司内主身份源互斥。当前版本只开放飞书。

### 1.2 飞书准备

在飞书开放平台创建企业自建应用，准备：

- App ID
- App Secret
- OAuth 回调地址

回调地址必须指向 IdBridge 后端 API：

```text
https://idbridge.example.com/api/auth/feishu/callback
```

对应后端环境变量：

```bash
IDB_WEB_BASE_URL='https://idbridge.example.com'
IDB_FEISHU_REDIRECT_URI='https://idbridge.example.com/api/auth/feishu/callback'
```

如果配置了 `IDB_WEB_BASE_URL` 且没有显式设置 `IDB_FEISHU_REDIRECT_URI`，后端会自动使用：

```text
<IDB_WEB_BASE_URL>/api/auth/feishu/callback
```

### 1.3 在后台配置飞书身份源

1. 登录 `/admin/login`。
2. 进入“身份源”。
3. 添加飞书身份源。
4. 填入 App ID 和 App Secret。
5. 保存。
6. 触发全量同步。
7. 在“同步任务”查看日志。
8. 在“组织架构”确认部门和员工。

同步后的数据边界：

- “组织架构”显示公司、部门树和同步来的员工。
- “账号管理”显示 IdBridge 内可登录、可授权、可分配角色和应用访问的账号。
- 通讯录档案和账号对象不是同一个概念。
- 全量同步按飞书快照收敛：匹配到既有用户时保留 IdBridge ULID，源端缺失的部门和目录用户会被清理；对应受管账号会按既有规则归档并从活动 `users` 中移除。
- 远端快照拉取成功后，本次全量同步的数据库变更与成功状态原子提交；任一写入失败会整体回滚，不会留下半完成快照。
- 飞书 webhook 先持久化为同步任务；相同身份源下重复的 `event_id` 只保留一条。服务重启或副本切换后，后台 worker 会恢复未完成任务并执行增量同步；临时失败按持久化退避重试，成功后任务状态为 `succeeded`，达到重试上限后为 `failed`。

## 2. 业务应用接入 IdBridge

推荐使用 OIDC Authorization Code + PKCE。

### 2.1 创建 OIDC 应用

在后台进入：

```text
/admin/applications
```

点击“创建应用”，选择 OIDC 客户端，填写：

- 应用名称
- 回调 URI，例如 `https://app.example.com/auth/oidc/callback`

回调 URI 必须是 `http://` 或 `https://` 绝对地址。生产环境应使用 HTTPS。

保存后 IdBridge 自动签发：

- `client_id`
- `client_secret`
- `scope`: `openid profile email`
- `grant_type`: `authorization_code`
- `response_type`: `code`
- PKCE: enabled

创建响应会显示一次 `client_secret`；调用方必须当场写入自己的 secret
manager。IdBridge 之后只保存单向 verifier，应用详情、普通更新、审计和
后续读取都不会再次返回该 secret：

- 应用基本信息与状态
- `client_id`，以及仅创建响应中的一次性 `client_secret`
- 回调 URI、scope、grant type、response type 与 PKCE 配置
- 已配置的工作台提供方字段

Discovery、Authorization、Token 和 UserInfo 地址按下节的公开端点配置，不会作为派生字段写入应用详情 JSON。

### 2.2 推荐端点

业务应用应使用 `/api` 前缀端点，便于单域部署和反向代理：

| 端点 | URL |
|---|---|
| Discovery | `https://idbridge.example.com/api/.well-known/openid-configuration` |
| Authorization | `https://idbridge.example.com/api/oauth2/authorize` |
| Token | `https://idbridge.example.com/api/oauth2/token` |
| UserInfo | `https://idbridge.example.com/api/oauth2/userinfo` |

### 2.3 授权码流程

业务应用启动登录时：

1. 生成 `state`。
2. 生成 `nonce`。
3. 生成 PKCE `code_verifier` 和 `code_challenge`。
4. 重定向到 IdBridge 授权端点。

示例：

```text
https://idbridge.example.com/api/oauth2/authorize?response_type=code&client_id=<client_id>&redirect_uri=https%3A%2F%2Fapp.example.com%2Fauth%2Foidc%2Fcallback&scope=openid%20profile%20email&state=<state>&nonce=<nonce>&code_challenge=<challenge>&code_challenge_method=S256
```

用户完成登录后，IdBridge 回调：

```text
https://app.example.com/auth/oidc/callback?code=<code>&state=<state>
```

业务应用后端可以在表单中提交 client 凭据换 token：

```http
POST /api/oauth2/token HTTP/1.1
Host: idbridge.example.com
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code&
code=<code>&
redirect_uri=https%3A%2F%2Fapp.example.com%2Fauth%2Foidc%2Fcallback&
client_id=<client_id>&
client_secret=<client_secret>&
code_verifier=<code_verifier>
```

也可以使用标准 HTTP Basic 认证，此时表单不必重复提交 `client_id` 和 `client_secret`：

```http
Authorization: Basic <base64(client_id:client_secret)>
```

`entity_id` 和 `X-IDB-Entity-ID` 仍为兼容参数；标准授权码换票可由一次性授权码定位所属实体，不要求第三方应用额外传递租户头。

业务应用必须校验：

- `state`
- `id_token` 签名
- `iss`
- `aud`
- `exp`
- `nonce`

### 2.4 强制使用飞书登录

如果业务应用希望用户优先使用飞书登录，在 authorize 请求中追加：

```text
idp=feishu
```

示例：

```text
https://idbridge.example.com/api/oauth2/authorize?response_type=code&client_id=<client_id>&redirect_uri=<encoded_callback>&scope=openid%20profile%20email&state=<state>&nonce=<nonce>&code_challenge=<challenge>&code_challenge_method=S256&idp=feishu
```

IdBridge 会解析 `return_to` 中的 `client_id`，识别对应公司和应用，并进入飞书登录链路。

## 3. 飞书工作台 SSO

飞书工作台入口不要直接配置成 IdBridge 登录页，推荐配置成业务应用自己的启动入口：

```text
https://app.example.com/sso/start
```

业务应用 `/sso/start` 负责：

1. 生成 `state`、`nonce`、PKCE。
2. 生成 IdBridge authorize URL。
3. 在 authorize URL 中追加：

```text
idp=feishu&workplace=feishu
```

完整示例：

```text
https://idbridge.example.com/api/oauth2/authorize?response_type=code&client_id=<client_id>&redirect_uri=<encoded_callback>&scope=openid%20profile%20email&state=<state>&nonce=<nonce>&code_challenge=<challenge>&code_challenge_method=S256&idp=feishu&workplace=feishu
```

工作台链路：

1. 用户在飞书工作台点击业务应用。
2. 业务应用跳转到 IdBridge authorize。
3. IdBridge 登录页识别 `workplace=feishu`。
4. 在飞书工作台环境中，IdBridge 通过飞书 JS bridge 获取 `auth_code`。
5. IdBridge 调用后端 `/api/auth/feishu/exchange` 建立 IdBridge 用户会话。
6. IdBridge 继续 OIDC authorize。
7. 业务应用收到 `code` 并换取 token。

普通浏览器没有飞书 JS bridge，所以用普通 Chrome 打开带 `workplace=feishu` 的 URL 时，可能显示“登录码交换失败”。这不是生产工作台链路失败，真实验证必须在飞书工作台内完成。

## 4. 用户门户

用户直接登录 IdBridge 时进入：

```text
/portal
```

门户首页显示该用户可访问的应用。用户从业务应用发起登录时，IdBridge 登录完成后会回到业务应用，不进入门户。

## 5. OIDC 应用通讯录 API

OIDC 应用如果需要做人员选择、部门选择或组织内搜索，管理员在 IdBridge 应用配置中勾选“目录 API”即可。IdBridge 会按应用已勾选的授权范围签发 access token，业务应用不需要维护第二份 scope 配置。

请求必须带上：

- `Authorization: Bearer <access_token>`
- `X-IDB-Entity-ID: <company_entity_id>`

可用接口：

```text
GET /api/directory/organization-tree/root
GET /api/directory/organization-tree/children?id=<node_id>&kind=company|organization|department
GET /api/directory/organization-tree/search?q=<keyword>
GET /api/directory/users/<managed_oidc_subject_id>
```

这些接口返回已同步的公司、部门和目录用户节点，用于业务应用内的人员选择和查找。用户节点的 `roles` 数组只投影 IdBridge 中该托管用户的权威角色分配（例如 `isa:pam:requester` 和 `isa:pam:approver`）。外部引用（包括 `trobs_user_id`）只是受限、opaque 的关联元数据，绝不能推导或授予角色；接口不会返回原始飞书档案。

精确用户接口不读取组织树缓存，并返回顶层 `version`。该 opaque
`sha256:<lowercase-hex>` 值只覆盖托管用户 active lifecycle、当前绑定的
source/directory-user、目录状态和排序后的 IdBridge 角色集合。IAM 应使用
`id` 作为 IdBridge subject、使用 `roles` 做当前 membership，并把 `version`
作为 membership proof revision；不得以 `updated_at` 代替，因为等价同步也会
推进该显示/同步时间戳。inactive、deleted 或 unbound subject 返回 `404`。

管理员需要先在 `/admin/applications` 的 OIDC 配置中允许 `directory:read`，否则 token 不会获得该 scope，调用目录 API 会返回 `insufficient_scope`。

## 6. 管理 API 和服务 API

当前边界：

- `/api/*`: 用户登录、OIDC、Feishu 回调、用户侧接口。
- `/sapi/*`: 管理后台 API 和 service-to-service 管理 API。

鉴权方式：

- Admin console: `idb_admin_session`
- 用户会话: `idb_session`
- Service-to-service: service token，按具体管理 API 设计使用

开源部署方通常不需要直接调用 `/sapi/*` 创建应用，优先使用管理后台。管理后台会自动处理 OIDC Client 签发和配置展示。

## 7. 排查清单

### 7.1 应用登录提示 client 或 redirect_uri 错误

检查：

- 应用是否在 `/admin/applications` 中启用。
- 回调 URI 是否和业务应用请求中的 `redirect_uri` 完全一致。
- 回调 URI 是否包含 `https://`。
- `client_id` 是否复制正确。

### 7.2 Discovery 返回前端 HTML

说明反向代理错误。请使用并检查：

```text
https://idbridge.example.com/api/.well-known/openid-configuration
```

`/api/*` 必须转发到 backend。

### 6.3 后台页面加载失败

检查 `/sapi/*` 是否转发到 backend。后台登录和管理 API 不走 `/api/*`。

### 6.4 飞书登录回调失败

检查：

- 飞书开放平台回调地址。
- `IDB_FEISHU_REDIRECT_URI`。
- `IDB_WEB_BASE_URL`。
- 后端是否能访问飞书 OpenAPI。

### 6.5 工作台 SSO 失败

检查：

- 是否在飞书工作台内真实打开。
- authorize URL 是否包含 `idp=feishu&workplace=feishu`。
- IdBridge 是否能根据 `client_id` 识别应用和公司。
- 飞书身份源是否已配置并启用。

## 7. 相关文档

- [自托管部署指南](deployment.md)
- [开发与本地联调](development.md)
- [存储架构](architecture/storage.md)
