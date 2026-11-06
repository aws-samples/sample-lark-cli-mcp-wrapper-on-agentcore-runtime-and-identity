[中文](#中文) | [English](#english)

---

<a name="中文"></a>

# 飞书应用配置

部署这套东西之前，需要在[飞书开放平台](https://open.feishu.cn/app)建一个自建应用，并完成
下面 4 项配置。这些都在飞书侧，不在 AWS 侧，也没有 API 可以自动化——都要在后台点。

配置顺序有依赖：第 2 步的 redirect URL 需要先在 AWS 侧创建 credential provider 才能拿到，
所以两边是交替进行的。

---

## 1. 建应用，拿到 App ID 和 App Secret

开放平台 → **创建企业自建应用**。建完在**凭证与基础信息**页拿两个值：

| 值 | 用在哪 |
|---|---|
| **App ID**（`cli_…`） | runtime 环境变量 `LARKSUITE_CLI_APP_ID` |
| **App Secret** | AgentCore Identity credential provider 的 `clientSecret` |

App ID 不是机密——它出现在每一个授权 URL 里。**App Secret 是机密**，只交给
`CreateOauth2CredentialProvider`，AgentCore Identity 会把它存进自己托管的 Secrets Manager
secret，不要写进代码或环境变量。

## 2. 配置 Redirect URL

这一步要先在 AWS 侧建好 credential provider，因为 URL 里带一个它生成的 UUID：

```sh
# AWS 侧：用第 1 步的 App ID / App Secret 创建 provider
aws bedrock-agentcore-control create-oauth2-credential-provider \
  --name feishu-oauth-provider \
  --credential-provider-vendor CustomOauth2 \
  --oauth2-provider-config-input '{
    "customOauth2ProviderConfig": {
      "clientId": "cli_xxxxxxxxxxxxxxxx",
      "clientSecret": "<app secret>",
      "oauthDiscovery": {
        "authorizationServerMetadata": {
          "issuer": "https://accounts.feishu.cn",
          "authorizationEndpoint": "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
          "tokenEndpoint": "https://open.feishu.cn/open-apis/authen/v2/oauth/token",
          "responseTypes": ["code"]
        }
      }
    }
  }'

# 读出它生成的 callbackUrl
aws bedrock-agentcore-control get-oauth2-credential-provider \
  --name feishu-oauth-provider --query callbackUrl --output text
```

`authorizationServerMetadata` 需要手填，因为飞书没有提供标准的
`.well-known/openid-configuration`，`discoveryUrl` 那条路走不通。

拿到的 `callbackUrl` 形如：

```
https://bedrock-agentcore.us-east-1.amazonaws.com/identities/oauth2/callback/<uuid>
```

把它填进开放平台 → **安全设置** → **重定向 URL**（Redirect URLs）。

⚠️ 这个 URL 是 OAuth 流程的**第一跳**，只负责接收飞书发来的 authorization code。它**不能**
同时当第二跳（session binding URL）用——那是另一个地址，细节见
[`deploy/session-binding/README.md`](../deploy/session-binding/README.md)。这两个搞混是
配错这套部署最常见的原因。

## 3. 开通权限（Scope）

开放平台 → **权限管理**，按需勾选，**注意是"用户身份"（user token scope）那一栏**，不是
"应用身份"。默认代码请求的 7 个 scope：

| Scope | 用途 |
|---|---|
| `im:message` | 发消息 |
| `im:chat:readonly` | 读群列表 |
| `calendar:calendar:readonly` | 读日历 |
| `calendar:calendar` | 写日历 |
| `calendar:calendar.event:read` | 读日程 |
| `docx:document` | 读写云文档 |
| `docx:document:create` | 创建云文档 |

改动这份列表要同时改 `sidecar/agentcore_resolver.go` 里的 `Scopes`。注意 Token Vault 的
槽位是按 scope 列表分键的，所以增删 scope 会让已有授权失效，需要重新走一次浏览器授权。

### 要用 `--as bot` 的话：还得勾「应用身份」栏

开放平台里同一个权限分「用户身份」和「应用身份」两栏，**是分别开通的**。上面那 7 个勾在用户
身份栏只让 `--as user` 能用；`--as bot` 走的是 M2M（client credentials）flow，读的是**应用
身份**栏。

默认（不设 `AGENTCORE_BOT_SCOPES`）bot **不指定 scope**：飞书的 client credentials 换 token
时不带 scope，就按应用身份栏已开通的权限全部签发。所以应用身份栏开什么，bot 就能用什么，不会因为
少开了某一个而整个换不到 token。`AGENTCORE_EXTRA_SCOPES` 永远不作用于 bot（`offline_access`
对应用身份没有意义）。

想让 bot 的 token 只带一部分权限，设 `AGENTCORE_BOT_SCOPES`（逗号或空格分隔），比如只发消息的
bot 设 `im:message`。**列出来的每一个都必须在应用身份栏开通并发版**，只要有一个没开，飞书就拒掉
整个请求（`invalid_scope`），AgentCore 报的是 `ValidationException: Error parsing
ClientCredentials response`，看不出是 scope 的问题。

应用身份栏一个都没开就调 `--as bot`，症状是 `GetResourceOauth2Token (M2M) returned no
AccessToken`。同样**改完要发版**。

### 收信人的 open_id 是按应用区分的

同一个人在每个飞书应用下的 `open_id` 都不一样。从别的应用（包括别的 bot）抄来的 `ou_xxx`
发不出去，报 `99992361 open_id cross app`。在本应用下拿 open_id 的办法（都能用 `--as bot`）：

- 收信人是这个应用的 owner：`api GET /open-apis/application/v6/applications/<app_id> --as bot
  --params '{"lang":"zh_cn","user_id_type":"open_id"}' --jq '.data.app.owner.owner_id'`。
  `app_id` 用 `whoami --as bot` 查；`lang` 必填，缺了报 `99992402`。需要应用身份的
  `application:application:self_manage` 权限。
- 收信人给 bot 发过私聊：`api GET /open-apis/im/v1/messages --as bot --params
  '{"container_id_type":"chat","container_id":"<chat_id>","page_size":20}'`，用户消息的
  `sender.id` 就是。
- 知道对方的 `union_id`（同一开发者主体下跨应用不变）：`api POST /open-apis/im/v1/messages
  --as bot --params '{"receive_id_type":"union_id"}' --data '...'`。
  `im +messages-send --user-id` 只收 `ou_` 开头的 open_id。

注意 `lark-cli api` 默认是用户身份，不加 `--as bot` 会要求浏览器授权。

### 要发外部（跨租户）群：额外开 External sharing

外部群只认应用身份 —— 用户身份发消息会被拒（`230027`），换 `--as bot` 才通。除此之外还要在
开放平台的**机器人**配置页开 **External sharing** → 勾「允许机器人加入外部群」，然后发版。
不开的话机器人在客户端「添加机器人」列表里根本不出现。

「允许外部用户发起单聊」是另一件事（管的是*外部用户主动私聊机器人*），发外部群不需要它。

注意跨租户**单聊**是飞书平台禁的，换什么身份都不行，错误码 `230038 cross tenant p2p chat
operate forbid`。要跨租户发消息只能走外部群。

### 可选：`offline_access`（强烈建议开）

不开这个权限，飞书只发 2 小时有效期的 access token，**每 2 小时就要重新走一次浏览器授权**。
开了之后飞书会同时下发 refresh token，Token Vault 自动续期，授权基本变成一次性的。

两步都要做，缺一不可（[飞书文档](https://open.feishu.cn/document/authentication-management/access-token/refresh-user-access-token)）：

1. 权限管理里开通 `offline_access`（在"用户身份"栏，搜索框直接搜这个名字）
2. runtime 上设 `AGENTCORE_EXTRA_SCOPES=offline_access`

只做第 2 步会导致授权失败——申请了应用没有的权限。

## 4. 发布版本

**权限改动必须发版才生效。** 开放平台 → **版本管理与发布** → 创建版本 → 提交发布。企业自建
应用通常需要管理员审核。

漏了这步的典型症状：`lark_exec` 报 `missing_scope`，但你确信权限已经勾上了。

---

## 检查清单

部署前确认：

- [ ] App ID 和 App Secret 已拿到
- [ ] AWS 侧 credential provider 已创建（用了这个 App Secret）
- [ ] provider 的 `callbackUrl` 已填进开放平台的重定向 URL
- [ ] 需要的 scope 已在"用户身份"栏勾选
- [ ] `offline_access` 已开通（如果不想每 2 小时重新授权）
- [ ] **已创建并发布新版本**
- [ ] runtime 上 `LARKSUITE_CLI_APP_ID` 已设成这个 App ID

## 常见问题

| 现象 | 原因 |
|---|---|
| `missing_scope` | 权限没勾，或勾了但没发版（见第 4 步）；也可能勾在了"应用身份"栏而不是"用户身份"栏 |
| `GetResourceOauth2Token (M2M) returned no AccessToken` | `--as bot` 要的权限没在"应用身份"栏勾，或勾了没发版 |
| `GetResourceOauth2Token (M2M) failed ... Error parsing ClientCredentials response` | 飞书拒了 client credentials 请求，AgentCore 看不懂飞书的错误体。多半是 `AGENTCORE_BOT_SCOPES` 里有应用身份栏没开的 scope（`invalid_scope` 20068），或者 credential provider 里的 client secret 不是当前的（`invalid_client` 20002）。复现：`curl -X POST https://open.feishu.cn/open-apis/authen/v2/oauth/token -H 'Content-Type: application/json' -d '{"grant_type":"client_credentials","client_id":"cli_xxx","client_secret":"...","scope":"<同样的 scope>"}'` 看飞书原话 |
| `99992361 open_id cross app` | 收信人的 open_id 是从别的应用抄来的。open_id 按应用区分，见上文「收信人的 open_id 是按应用区分的」 |
| 发外部群报 `230027` | 用了 `--as user`。外部群只认应用身份，换 `--as bot`；另需开 External sharing 并发版 |
| 跨租户单聊报 `230038` | 平台限制，任何身份都不行。改走外部群 |
| 授权页报 redirect URL 不匹配 | 第 2 步的 URL 没填、填错，或 provider 重建过导致 UUID 变了 |
| 每 2 小时就要重新授权 | `offline_access` 没开通，或没设 `AGENTCORE_EXTRA_SCOPES` |
| 授权成功但 `lark_exec` 报 `authorization required` | `LARKSUITE_CLI_APP_ID` 和 provider 里的 `clientId` 不是同一个应用 |
| 浏览器报 `authorizationCode ... must not be null` | 把第 2 步的 callbackUrl 当成 session binding URL 用了，见 [`deploy/session-binding/README.md`](../deploy/session-binding/README.md) |

---

<a name="english"></a>

# Feishu app setup

Four things to configure in the [Feishu Open Platform](https://open.feishu.cn/app)
before deploying. All console-only — no API automates them.

1. **Create a custom app**, note the App ID (`cli_…`, not secret — it appears in
   every authorization URL) and App Secret (secret — hand it only to
   `CreateOauth2CredentialProvider`, which stores it in an AWS-managed Secrets
   Manager secret).

2. **Register the redirect URL.** Create the AgentCore credential provider first,
   then read its generated `callbackUrl` and paste that into the app's redirect
   URL list. `authorizationServerMetadata` must be filled in by hand: Feishu
   publishes no `.well-known/openid-configuration`, so `discoveryUrl` is not an
   option. Note this URL is OAuth **hop 1** only and is *not* interchangeable with
   the session binding URL — see
   [`deploy/session-binding/README.md`](../deploy/session-binding/README.md).

3. **Enable scopes** under the *user token* column (not the tenant/app one). The
   seven the code requests by default are listed in the table above. Strongly
   consider also enabling `offline_access`, then set
   `AGENTCORE_EXTRA_SCOPES=offline_access`: without a refresh token, Feishu's
   2-hour access token means re-running the browser flow every two hours. Both
   steps are required — requesting a scope the app does not hold fails the
   authorization outright.

4. **Publish a version.** Scope changes do not take effect until a new app version
   is released and approved. Skipping this shows up as `missing_scope` errors even
   though the console shows the permission as granted.

Adding or removing scopes re-keys the Token Vault slot, so it invalidates the
existing authorization and needs one more browser login.

**For `--as bot`**, enable the permissions the bot needs under the *app identity*
column — the console grants the two columns independently, and the M2M flow
reads the app one. By default (`AGENTCORE_BOT_SCOPES` unset) the bot requests no
explicit scope, and Feishu issues a token carrying everything the app holds under
app identity, so the default cannot fail on a scope you never enabled.
`AGENTCORE_EXTRA_SCOPES` never applies to the bot. Set `AGENTCORE_BOT_SCOPES`
(e.g. `im:message` for a notify-only bot) to narrow the token; every scope listed
must be granted under app identity, or Feishu rejects the whole grant with
`invalid_scope`, which AgentCore reports only as `ValidationException: Error
parsing ClientCredentials response`. No app permission at all surfaces as
`GetResourceOauth2Token (M2M) returned no AccessToken`.

**open_id is per app.** The same person has a different `open_id` in every Feishu
app; an `ou_xxx` copied from another app (or another bot) fails with `99992361
open_id cross app`. Ways to get it within this app, all with `--as bot`: the app
owner via `api GET /open-apis/application/v6/applications/<app_id> --as bot
--params '{"lang":"zh_cn","user_id_type":"open_id"}' --jq
'.data.app.owner.owner_id'` (`app_id` from `whoami --as bot`; `lang` is required;
needs app-identity `application:application:self_manage`); the `sender.id` of
the user's messages in their DM with the bot; or send by `union_id`, which is
stable across one developer's apps, through `api POST /open-apis/im/v1/messages`
with `receive_id_type=union_id` (`im +messages-send --user-id` takes only
`ou_` ids). `lark-cli api` defaults to user identity; add `--as bot`.

**To post into external (cross-tenant) groups**, bot identity is mandatory: a
user-identity send is refused with `230027`. Also turn on **External sharing →
allow the bot to join external groups** on the bot config page, then publish —
without it the bot never appears in the client's "add bot" list. Cross-tenant
*direct* messages are blocked by the platform for every identity (`230038 cross
tenant p2p chat operate forbid`); an external group is the only route.
