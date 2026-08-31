# IdBridge 自托管部署指南

本文面向把 IdBridge 部署到自有 IDC、私有云或 Kubernetes 集群的使用方。目标是让部署方按步骤完成初始化、反向代理、飞书配置、OIDC 应用接入和上线验证。

## 1. 部署模型

IdBridge 由两个运行单元组成：

| 组件 | 说明 |
|---|---|
| `idbridge` backend | Go API 服务，提供 `/api/*`、`/sapi/*`、OIDC、登录、同步、审计等能力 |
| `idbridge-frontend` web | React + Vite + Ant Design 静态前端，提供 `/admin/*`、`/portal`、`/auth/continue` 等页面 |

推荐生产形态：

```text
https://idbridge.example.com
  /api/*   -> backend:8080
  /sapi/*  -> backend:8080
  /        -> frontend:80
```

如果使用仓库内 `web/Dockerfile` 构建的前端镜像，镜像内 Caddy 已把 `/api*` 和 `/sapi*` 代理到 `http://idbridge:8080`。如果使用自己的 Nginx、Ingress、网关或前端静态托管，必须自行配置上面的路由。

## 2. 前置条件

生产或 IDC 环境至少需要：

- PostgreSQL 13+，并启用 `pgcrypto` 扩展。
- 一个稳定的 HTTPS 域名，例如 `https://idbridge.example.com`。
- 可以访问 Feishu OpenAPI 的出口网络，如果启用飞书登录或通讯录同步。
- 一个 Feishu 企业自建应用，如果启用飞书。
- 可安全存放数据库连接串和 Feishu App Secret 的 Secret 管理方式。

不建议生产使用：

- 明文 HTTP。
- 使用默认管理员密码长期运行。
- 把 `client_secret`、Feishu `app_secret` 写入公开仓库。

## 3. 必填配置

后端通过环境变量配置。

| 变量 | 必填 | 示例 | 说明 |
|---|---:|---|---|
| `DATABASE_URL` | 是 | `postgres://idbridge:***@postgres:5432/idbridge?sslmode=disable` | PostgreSQL 连接串 |
| `DB_POOL_MAX_CONNS` | 建议 | `10` | 单个 backend 副本的最大 PostgreSQL 连接数；环境变量优先于 URL 中的 `pool_max_conns` |
| `DB_POOL_MIN_CONNS` | 建议 | `2` | 单个 backend 副本保持的最小连接数；不得超过最大连接数 |
| `DB_POOL_MAX_CONN_LIFETIME` | 否 | `1h` | 连接最大生命周期，使用 Go duration 格式 |
| `DB_POOL_MAX_CONN_IDLE_TIME` | 否 | `15m` | 空闲连接最大保留时间，使用 Go duration 格式 |
| `DB_POOL_ACQUIRE_TIMEOUT` | 建议 | `2s` | 等待池连接的最大时间；超时后请求快速失败 |
| `DB_BACKGROUND_MAX_CONCURRENCY` | 建议 | `2` | 同步、审计、cleanup 和 webhook recovery 共享的后台数据库并发预算 |
| `IDB_HTTP_ADDR` | 是 | `:8080` | 后端监听地址 |
| `IDB_OIDC_ISSUER` | 是 | `https://idbridge.example.com` | OIDC issuer，必须是用户和外部应用可访问的最终地址 |
| `IDB_WEB_BASE_URL` | 建议 | `https://idbridge.example.com` | Web 基准地址；未显式设置 Feishu 回调时用于生成回调地址 |
| `IDB_FEISHU_REDIRECT_URI` | 飞书必填 | `https://idbridge.example.com/api/auth/feishu/callback` | 飞书 OAuth 回调地址 |
| `IDB_OIDC_KEY_ID` | 建议 | `prod-key-1` | OIDC/JWKS key id |
| `IDB_OIDC_PRIVATE_KEY_PEM` | 生产必填 | 由 Secret 注入 | OIDC RSA 私钥 PEM；必须在所有 backend 副本中保持一致 |
| `IDB_CONFIG_ENCRYPTION_KEY` | 建议 | 由 Secret 注入 | Base64 编码的 32 字节 AES-256 密钥；用于加密新保存的身份源配置，所有 backend 副本必须一致 |
| `IDB_TRUSTED_PROXY_CIDRS` | 反向代理时必填 | `10.42.0.0/16` | 直接连接 backend 的可信路由 Caddy/Ingress 网段，多个 CIDR 用逗号分隔；仅这些来源的 `X-Forwarded-For`/`Forwarded` 会被采信 |
| `IDB_DEFAULT_LOCALE` | 否 | `zh-CN` | `zh-CN` 或 `en-US` |
| `IDB_SESSION_TTL_SECONDS` | 否 | `86400` | 用户会话 TTL |
| `IDB_AUTH_CODE_TTL_SECONDS` | 否 | `300` | OIDC 授权码 TTL |
| `IDB_ACCESS_TOKEN_TTL_SECONDS` | 否 | `900` | access token TTL |
| `IDB_ID_TOKEN_TTL_SECONDS` | 否 | `900` | id token TTL |
| `IDB_REDIS_ENABLED` | 否 | `true` | 是否启用 Redis 读模型/缓存 |
| `IDB_REDIS_URL` | Redis 时必填 | `redis://redis:6379/0` | Redis 连接地址 |
| `IDB_FEISHU_BASE_URL` | 否 | `https://open.feishu.cn` | 飞书 OpenAPI 地址 |

注意：

- `IDB_OIDC_ISSUER` 必须和部署后发现文档中的 issuer 一致。
- `IDB_FEISHU_REDIRECT_URI` 必须在飞书开放平台中配置为 OAuth 回调地址。
- 如果后端在内网 `http://idbridge:8080`，但用户访问域名是 `https://idbridge.example.com`，`IDB_OIDC_ISSUER` 仍然必须写外部可访问地址。
- 生产环境必须通过 Secret 注入 `IDB_OIDC_PRIVATE_KEY_PEM`，不得提交到仓库或写入普通 ConfigMap。未配置时服务会为兼容旧部署临时生成密钥，但进程重启会改变 JWKS，不能作为生产长期方案。
- `IDB_CONFIG_ENCRYPTION_KEY` 未配置时仍可读取和保存旧版明文配置，便于无中断升级；配置后，新建或再次保存的身份源配置会使用带版本标识的密文，旧明文配置仍可读取。
- `IDB_TRUSTED_PROXY_CIDRS` 应填写直接连接 backend 的最后一跳代理网段；使用“IDC Edge Caddy → IdBridge 路由 Caddy → backend”时，应填写路由 Caddy 的网段，而不是 Edge 或终端用户公网网段。未配置时后端会忽略转发头并保留原始直连地址，以保持旧版限流行为；经反向代理的生产部署必须配置该变量，且禁止为了方便填写 `0.0.0.0/0` 或 `::/0`。
- backend 未配置连接池参数时采用明确的安全默认值 `max=10`、`min=2`、最大生命周期 `1h`、最大空闲时间 `15m`，不再按 CPU 核数扩大连接数。也可以在 `DATABASE_URL` 中使用 pgxpool 的 `pool_max_conns`、`pool_min_conns`、`pool_max_conn_lifetime` 和 `pool_max_conn_idle_time`；同一参数同时出现时环境变量优先。
- 每个副本都会设置 PostgreSQL `application_name=idbridge`，除非 `DATABASE_URL` 已显式提供其他值。

### 3.1 PostgreSQL 连接预算

连接预算必须按 PostgreSQL 实例整体分配，不能只看单个 Pod：

```text
DB_POOL_MAX_CONNS * backend 最大副本数
< PostgreSQL max_connections
  - 管理连接预留
  - 监控和迁移连接预留
  - 共享实例中其他业务的连接预算
```

单副本可先使用：

```bash
DB_POOL_MAX_CONNS=10
DB_POOL_MIN_CONNS=2
DB_POOL_MAX_CONN_LIFETIME=1h
DB_POOL_MAX_CONN_IDLE_TIME=15m
DB_POOL_ACQUIRE_TIMEOUT=2s
DB_BACKGROUND_MAX_CONCURRENCY=2
```

后台并发必须小于池上限，以给登录、OIDC 换票和管理请求保留连接。增加副本数时，应先重新计算每副本上限，不得让连接总预算随副本数无界增长。

### 3.2 OIDC 签名密钥上线步骤

现有旧版本使用进程内临时密钥，因此无法在升级后恢复旧私钥。为避免第三方应用在切换期间校验到刚刚失效的短期 token：

1. 在受控环境生成 2048 位 RSA 私钥，并保存在部署 Secret 管理系统：

```bash
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048
```

2. 将完整 PEM 内容作为 `IDB_OIDC_PRIVATE_KEY_PEM` 注入全部 backend 副本，并为本次密钥设置新的 `IDB_OIDC_KEY_ID`。
3. 在一次受控发布窗口内切换全部 backend 副本；旧版本签发的 access/id token 默认最多有效 15 分钟。发布前先等待该窗口结束或让依赖方刷新 token，避免旧公钥缓存造成短暂验签失败。
4. 发布后访问 `/api/.well-known/jwks.json`，确认 `kid` 与配置一致；重启一个副本后再次确认 JWKS 不变。

后续发布保持同一 Secret 和 `kid`，不会影响现有 `client_id`、`client_secret`、回调地址、用户或应用。密钥轮换应作为单独的兼容发布执行，保留旧公钥直到最长 token 有效期和缓存期结束。

### 3.3 身份源配置加密的兼容上线

生成并保管配置加密密钥：

```bash
openssl rand -base64 32
```

现有生产环境建议分两阶段上线：

1. 先备份数据库并部署新版本，暂不设置 `IDB_CONFIG_ENCRYPTION_KEY`，确认现有身份源仍能读取、登录与同步。
2. 回滚观察窗口结束后，把同一密钥注入全部 backend 副本并滚动重启。
3. 之后新建或更新身份源时，该条配置会自动转为密文；未修改的旧配置继续兼容读取，不要求停机批量迁移。

密钥一旦启用不得丢失或在副本间不一致。已有配置被加密后，如需回滚到不支持该密文格式的旧程序，必须同时恢复启用加密前的数据库备份；不能只回滚镜像。

## 4. 数据库初始化

数据库变更位于 `backend/migrations/`，必须按编号依次执行。全新数据库从 `000001_schema_baseline.sql` 开始；现有数据库只执行尚未应用的增量迁移，不得重复导入基线或重建已有表。

迁移不在 backend 进程启动时执行。Kubernetes 必须使用独立 migration Job，并等待 Job 成功后才扩容 backend；不要把迁移命令放入每个 Pod 的启动脚本或 init container。Goose 使用版本表保证已完成迁移不会重复应用，但生产发布仍应只调度一个 migration Job。

执行迁移：

```bash
cd backend
go run github.com/pressly/goose/v3/cmd/goose@v3.22.1 \
  -dir migrations \
  postgres "$DATABASE_URL" up
```

容器镜像内已包含 `goose` 和 `/app/migrations`，Kubernetes Job 可直接执行：

```bash
goose -dir /app/migrations postgres "$DATABASE_URL" up
```

迁移完成后会初始化：

- 默认公司：`Default Enterprise`
- 默认平台管理员：`admin / admin123`
- 基础权限和角色

首次登录后必须立即修改默认管理员密码。管理员账号和普通用户账号完全隔离，初始化管理员不属于业务用户数。

### 4.1 非生产 sandbox bootstrap

受控的非生产 GitOps 环境可在迁移完成后运行：

```bash
/usr/local/bin/idbridge bootstrap sandbox
```

该命令只读取 `DATABASE_URL`、`IDB_DEFAULT_LOCALE` 和以下预投影字段：
`IDBRIDGE_ENTITY_SLUG`、`IDBRIDGE_ENTITY_NAME`、`IDBRIDGE_ADMIN_USERNAME`、
`IDBRIDGE_ADMIN_PASSWORD`、`IDBRIDGE_ADMIN_ROLE`、
`IDBRIDGE_DIRECTORY_READER_APPLICATION_NAME`、
`IDBRIDGE_DIRECTORY_READER_CLIENT_ID`、
`IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET`、`IDBRIDGE_FIXTURE_SOURCE_NAME`、
`IDBRIDGE_FIXTURE_OWNER_USERNAME`、`IDBRIDGE_FIXTURE_OWNER_TROBS_USER_ID`、
`IDBRIDGE_FIXTURE_UNRELATED_USERNAME` 和
`IDBRIDGE_SANDBOX_SINGLE_DEVELOPER`。它不需要服务器签名密钥、管理员
cookie、Kubernetes token 或 secret-manager 写权限，也不会输出密码或 client secret。

命令通过应用事务和审计记录创建缺失资源；现有资源必须精确满足安全契约，否则失败，不会静默轮换 credential 或重绑账号。角色
`isa:pam:requester` 与 `isa:pam:approver` 由显式 sandbox policy 分配；
`trobs_user_id` 只是 opaque 外部引用，不能推导授权。此命令不得用于生产初始化。

## 5. 构建镜像

后端：

```bash
docker build -t idbridge-backend:latest backend/
```

前端：

```bash
docker build -t idbridge-frontend:latest web/
```

前端镜像默认用 Caddy 暴露 80 端口，并把 `/api*`、`/sapi*` 转发到服务名 `idbridge:8080`。如果你的后端服务名不是 `idbridge`，请改 `web/Caddyfile` 或在部署层提供同名 Service。

## 6. Kubernetes 部署顺序

推荐顺序：

1. 创建 namespace。
2. 创建 PostgreSQL Secret。
3. 启动 PostgreSQL 或配置外部 PostgreSQL。
4. 执行 migration Job。
5. 部署 backend。
6. 部署 frontend。
7. 配置 Ingress/TLS。
8. 执行上线验证。

仓库内 OrbStack 示例：

```bash
make k8s-build
make k8s-build-frontend
make k8s-deploy
make k8s-deploy-frontend
```

这些示例面向本地开发，不应原样用于生产。IDC 部署时请至少替换：

- namespace
- 镜像仓库地址
- `DATABASE_URL`
- `IDB_OIDC_ISSUER`
- `IDB_WEB_BASE_URL`
- `IDB_FEISHU_REDIRECT_URI`
- `IDB_TRUSTED_PROXY_CIDRS`
- TLS Secret
- Ingress host

### 6.1 备份与恢复演练

PostgreSQL 是用户、应用、OIDC client、授权状态和审计记录的唯一事实来源。生产上线前应建立自动备份，并至少完成一次非生产恢复演练。

备份示例：

```bash
pg_dump --format=custom --no-owner --file=idbridge-$(date +%F).dump "$DATABASE_URL"
```

恢复必须在隔离的目标数据库上先验证，再安排受控维护窗口：

```bash
pg_restore --clean --if-exists --no-owner --dbname="$DATABASE_URL" idbridge-YYYY-MM-DD.dump
```

恢复后必须验证迁移版本、OIDC discovery、JWKS、管理员登录和一个既有 OIDC 应用的授权码流程。不要通过删除或重建数据库来排查单个应用、用户或同步问题。

### 6.2 现有生产环境升级顺序

本版本的数据库迁移为增量变更，不重建用户、应用、OIDC client 或账号绑定。建议按以下顺序升级：

1. 记录当前镜像版本、迁移版本和所有 Secret，并完成数据库备份。
2. 对备份恢复出的非生产数据库执行 `goose status` 和 `goose up`，验证迁移可重复检查且现有数据可读取。
3. 检查待执行迁移是否包含 `000011_oidc_client_secret_verifiers.sql`。若包含，必须进入维护窗口、停止新 token 请求并先排空全部旧 backend；旧程序只会比较明文，不能读取迁移后的单向 verifier。排空后执行 `goose up`，再一次性发布 verifier-aware backend，全部 `/readyz` 成功后才恢复流量。该迁移不能使用旧/新副本并存的滚动顺序。
4. 不包含 `000011` 的兼容增量迁移可按原滚动顺序执行。任何路径都不要执行 `goose down`，也不要重新导入 `000001_schema_baseline.sql`。
5. 保持原有 client secret、`IDB_OIDC_PRIVATE_KEY_PEM`、`IDB_OIDC_KEY_ID` 和现有域名不变；`000011` 只转换存储 verifier，调用方无需轮换 secret。
6. backend 全部就绪后再发布 frontend 静态资源。
7. 验证一个既有管理员会话、一个既有用户登录、一个既有 OIDC client 的授权码换票，以及一次身份源读取；无需删除或重建任何现有应用。
8. `000011` 的数据转换不可逆；执行后不得回滚到明文比较的旧镜像。故障必须向前修复，或在明确接受迁移后数据丢失的灾难恢复流程中恢复迁移前备份。其他向前兼容迁移仍保留已执行数据库版本。若期间已启用身份源配置加密，则按 3.3 节保留新程序与密钥，或恢复启用前备份。

全量同步的清理语义比旧版本更严格。首次升级后执行全量同步前，应先在恢复出的非生产副本核对源端快照和清理结果；生产环境是否触发同步由部署方在维护窗口决定。

`000007_webhook_recovery.sql` 只给 `sync_jobs` 增加恢复元数据、索引和独立的 source lease 表，不删除或改写用户、应用、绑定和既有同步记录。新 backend 启动后会立即检查未完成的 webhook job，之后默认每 30 秒检查一次；多副本通过数据库 lease 避免同一身份源被同时领取。执行失败的 webhook job 会按 1 分钟、5 分钟、30 分钟、2 小时退避，累计 5 次失败后才进入终态 `failed`，成功消费则进入 `succeeded`。发布期间旧副本仍可按原 SQL 写入，新增列的默认值保证这些记录可由新副本恢复。

`000007` 和 `000009` 使用 PostgreSQL 并发索引并由 goose 以非事务方式逐步执行，避免在历史回填和建索引期间持续阻塞生产写入；迁移步骤可安全重试，不要在外层额外包裹事务。`000008` 会把既有 OIDC client 标记为 `secret_required=false`，保持原换票行为；升级后由新后台创建的 client 才会显式标记为 `true`。`000009` 只增加授权码/token hash 查询索引，不改变凭证数据。

`000011` 使用 `pgcrypto` 把非空 legacy `client_secret_hash` 原地转换成
`sha256:<lowercase-hex>` verifier，并加格式约束。迁移不会把 plaintext
写入临时表或日志；Down 只能移除约束，无法也不得恢复 plaintext。创建和
rotate API 只在当次响应返回新 secret，详情、普通更新和 bootstrap rerun
不再返回它。Sandbox bootstrap 用 projected secret 验证现有 verifier；不匹配
会以 credential drift 失败，绝不会静默轮换。

SN cutover 的迁移编号以本目录为唯一分配源：`000011` 已保留给本次
verifier 转换。其他工作树中尚未发布的 full-sync lease、TROBS provenance
或类似迁移必须先基于该提交重排到下一个可用编号，再进入同一发布序列；不能
直接复制相同编号，也不能把未获批准的 TROBS 授权语义带入本合约。

## 7. 反向代理配置

健康检查约定：`/healthz` 只表示进程存活；`/readyz` 使用 2 秒超时检查 PostgreSQL，数据库不可用时返回 `503`。Kubernetes、负载均衡和发布脚本应把流量检查指向 `/readyz`。

backend 的 `/metrics` 暴露 Prometheus 文本指标，包括池上限、总连接、占用连接、空闲连接、等待连接次数、累计等待时间和 Acquire timeout 次数。该端点应仅在集群内或受监控网络访问，不要通过公网 Ingress 暴露。

本地池使用率告警示例：

```promql
# warning: 持续 5 分钟超过 80%
open_idb_db_pool_acquired_connections
  / open_idb_db_pool_max_connections >= 0.80

# critical: 持续 2 分钟超过 90%
open_idb_db_pool_acquired_connections
  / open_idb_db_pool_max_connections >= 0.90
```

共享 PostgreSQL 的全局 80%/90% 告警必须由 PostgreSQL exporter 或数据库监控基于 `max_connections` 和当前 session 数配置；应用自身只能观测本进程的连接池，不能准确代表共享实例的总连接占用。

### 7.1 Nginx 示例

```nginx
server {
    listen 443 ssl http2;
    server_name idbridge.example.com;

    ssl_certificate     /etc/nginx/tls/tls.crt;
    ssl_certificate_key /etc/nginx/tls/tls.key;

    location /api/ {
        proxy_pass http://idbridge-backend:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    location /sapi/ {
        proxy_pass http://idbridge-backend:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    location / {
        proxy_pass http://idbridge-frontend:80;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

### 7.2 Ingress 路由要求

如果 Ingress 直接分流，而不是让前端 Caddy 代理 API，请配置：

| 路径 | 后端服务 |
|---|---|
| `/api` | `idbridge` backend |
| `/sapi` | `idbridge` backend |
| `/` | frontend |

OIDC 集成推荐使用 `/api` 前缀下的发现文档：

```text
https://idbridge.example.com/api/.well-known/openid-configuration
```

### 7.3 双层 Caddy 的客户端地址

如果链路为“IDC Edge Caddy → 仓库内路由 Caddy → backend”，需要分别建立两段信任，不能把未知公网地址直接加入 backend 的可信列表：

1. Edge Caddy 正确写入 `X-Forwarded-For`。
2. 路由 Caddy 在全局 `servers` 配置中仅信任 Edge Caddy 的实际 CIDR，例如：

```caddyfile
{
	servers {
		trusted_proxies static 192.0.2.10/32 2001:db8::10/128
	}
}
```

3. backend 的 `IDB_TRUSTED_PROXY_CIDRS` 仅填写路由 Caddy 到 backend 使用的实际 CIDR。

如果第 2 步缺失，路由 Caddy 会把 Edge 地址当作直连客户端，backend 无法得到终端用户地址，登录与 token 交换限流可能被错误聚合。示例 CIDR 仅为文档地址，部署时必须替换；不要信任所有公网地址。

## 8. 飞书配置

在飞书开放平台创建企业自建应用，至少准备：

- App ID
- App Secret
- OAuth 回调地址：

```text
https://idbridge.example.com/api/auth/feishu/callback
```

在 IdBridge 后台配置：

1. 登录 `/admin/login`。
2. 进入“身份源”。
3. 添加飞书身份源。
4. 填入 Feishu App ID 和 App Secret。
5. 保存后触发全量同步。
6. 在“同步任务”查看进度和日志。
7. 在“组织架构”检查部门树和人员。

身份源边界：

- 当前版本只开放飞书作为主身份源。
- 同一公司内主身份源互斥。
- 员工登录以飞书 SSO 和飞书工作台 SSO 为主；本地账号只是用户侧登录兜底能力，不作为身份源配置。
- 全量同步成功后必须和飞书当前快照一致：已存在用户保持 ULID 不变，源端缺失的部门和目录用户会被清理，对应受管账号按既有归档规则处理。
- 远端快照获取完成后，完整数据库变更与同步成功状态在同一事务提交；任一数据库写入失败会回滚本次快照，不会暴露半完成结果。

## 9. 管理员和账号

访问：

```text
https://idbridge.example.com/admin/login
```

初始管理员：

```text
admin / admin123
```

首次操作建议：

1. 修改 `admin` 密码。
2. 在“管理员管理”创建真实平台管理员或公司管理员。
3. 禁用或仅保留 `admin` 作为 break-glass 账号。
4. 配置公司信息。
5. 配置身份源。
6. 同步组织架构和目录用户。
7. 配置应用。

管理员账号和普通用户账号是隔离的：

- 管理员：`admin_users`，登录 `/admin/*`，使用 `idb_admin_session`。
- 用户：`users`，登录 `/portal` 或应用登录流，使用 `idb_session`。
- `/`、`/login`、`/auth/continue` 不暴露管理员入口、管理员文案或管理员账号提示。

## 10. 应用接入

在后台进入：

```text
/admin/applications
```

添加 OIDC 应用时，IdBridge 会自动签发：

- `client_id`
- `client_secret`
- 默认授权范围：`openid profile email`
- 授权类型：`authorization_code`
- 响应类型：`code`
- PKCE：启用

如果业务应用需要人员选择、部门选择或组织搜索，在 OIDC 配置中勾选“目录 API”。这会把 `directory:read` 加入应用允许的授权范围：

```text
directory:read
```

IdBridge 会按应用已勾选的授权范围签发 access token；业务应用侧不需要同步维护这份授权范围。

管理员需要填写业务应用回调地址，必须是绝对 URL：

```text
https://app.example.com/auth/oidc/callback
```

保存后应用抽屉会展示：

- Discovery URL
- Authorization Endpoint
- Token Endpoint
- UserInfo Endpoint
- Feishu login template
- Feishu workplace SSO template

业务应用需要自己生成 `state`、`nonce`、PKCE `code_verifier/code_challenge`，不能复用固定值。

## 11. 飞书工作台 SSO

适用场景：用户从飞书工作台点击业务应用，业务应用跳转到 IdBridge 完成飞书上游登录，再回到业务应用。

推荐链路：

1. 飞书工作台入口配置为业务应用自己的地址：

```text
https://app.example.com/sso/start
```

2. 业务应用生成 `state`、`nonce`、PKCE。
3. 业务应用重定向到 IdBridge 授权端点，并带：

```text
idp=feishu&workplace=feishu
```

4. IdBridge 在飞书工作台环境中通过飞书 JS bridge 获取 `auth_code`。
5. IdBridge 用 `auth_code` 建立自身用户会话。
6. IdBridge 继续 OIDC 授权请求，回调业务应用 `redirect_uri?code=...&state=...`。
7. 业务应用用 `code` 换 token 并建立自己的业务会话。

普通浏览器没有飞书 JS bridge，因此本地普通 Chrome 测试工作台入口时，看到“登录码交换失败”是预期现象。真实验证必须在飞书工作台内完成。

## 12. 上线验证

基础健康检查：

```bash
curl -fsS https://idbridge.example.com/api/.well-known/openid-configuration
curl -fsS https://idbridge.example.com/admin/login
```

OIDC authorize 未登录时应返回 302 到登录页：

```bash
curl -i "https://idbridge.example.com/api/oauth2/authorize"
```

后台页面验证：

1. 打开 `/admin/login`。
2. 使用管理员账号登录。
3. 打开“身份源”，确认能加载。
4. 打开“应用管理”，创建一个临时 OIDC 应用。
5. 确认自动生成 `client_id` 和 `client_secret`。
6. 使用生成的授权 URL 检查是否跳到登录页。
7. 删除临时应用。

Feishu 验证：

1. 配置飞书身份源。
2. 触发全量同步。
3. 检查“同步任务”日志。
4. 检查“组织架构”是否显示公司、部门、人员和英文名。
5. 使用飞书登录一个已同步用户。
6. 在飞书工作台真实打开业务应用入口，验证 `workplace=feishu` 链路。

## 13. 常见问题

### 后台登录后页面加载 API 失败

检查 `/sapi/*` 是否代理到 backend。只代理 `/api/*` 不够。

### OIDC discovery 地址返回前端 HTML

说明路由转发错误。推荐使用：

```text
/api/.well-known/openid-configuration
```

并确保 `/api/*` 到 backend。

### 飞书回调失败

检查：

- 飞书开放平台回调地址是否等于 `IDB_FEISHU_REDIRECT_URI`。
- 回调地址是否为 HTTPS。
- `/api/auth/feishu/callback` 是否能从飞书访问到。

### 工作台 SSO 在普通浏览器失败

普通浏览器没有飞书 JS bridge，无法获取飞书工作台 `auth_code`。请在飞书工作台内验证。

### authorize 一直提示登录态缺失

这是未登录时的正常行为。OIDC authorize 会先要求 IdBridge 用户会话，登录成功后继续授权请求。

### token 换取失败

检查：

- `redirect_uri` 是否和后台登记值完全一致。
- PKCE `code_verifier` 是否对应授权请求里的 `code_challenge`。
- `client_secret` 是否放在服务端请求中，且没有泄露到前端。
- `IDB_OIDC_ISSUER` 是否是最终用户可访问域名。

## 14. 发布前检查清单

- [ ] PostgreSQL 已初始化并完成迁移。
- [ ] `admin / admin123` 已修改密码。
- [ ] `IDB_OIDC_ISSUER` 是最终 HTTPS 域名。
- [ ] `/api/*` 和 `/sapi/*` 已正确转发到 backend。
- [ ] Feishu OAuth 回调地址已在飞书开放平台配置。
- [ ] 应用回调 URI 是 HTTPS 绝对地址。
- [ ] OIDC discovery 可访问。
- [ ] 管理后台、身份源、应用管理、同步任务、组织架构页面可加载。
- [ ] 已完成一次飞书全量同步。
- [ ] 已完成至少一个 OIDC 应用授权码登录闭环。
