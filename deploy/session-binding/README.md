[中文](#中文) | [English](#english)

---

<a name="中文"></a>

# Session-binding endpoint（可选）

去掉首次飞书授权里那一步手工操作。不部署也能用——只是 agent 需要多调一次
`lark_auth_complete`。部署了之后，浏览器登录完就自动绑定。

## 为什么需要一个独立的 endpoint

AgentCore Identity 的 authorization-code 流程有**两次**跳转，很容易搞混：

| 跳 | 谁跳转 | 落到哪 | 带的参数 | 谁托管 |
|---|---|---|---|---|
| 1 | 飞书 | credential provider 的 `callbackUrl` | `authorizationCode`、`state` | AWS |
| 2 | AgentCore Identity | `resourceOauth2ReturnUrl`（即 *session binding URL*） | `session_id`、`state` | **你** |

第一跳已经配好了：`GetOauth2CredentialProvider` 返回的 `callbackUrl` 里带一个 per-provider
的 UUID，那个 URL 注册在飞书应用的 redirect URL 里。这里的东西不碰它。

第二跳才是本目录实现的。AWS 明确说了，一旦 agent 跑在 AgentCore Runtime 上，这就是客户的
责任：

> when deploying your agent code to AgentCore Runtime, your web application that
> connects to the agent runtime must host a publicly accessible HTTPS callback
> endpoint itself, the callback endpoint must be registered against the workload
> identity as an `AllowedResourceOAuth2ReturnUrl` by calling
> `UpdateWorkloadIdentity` [...] and then call the `CompleteResourceTokenAuth`
> API
>
> —— [OAuth 2.0 authorization URL session binding](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html)

同一页还提到 `agentcore dev` 会在**本地**替你托管这个 endpoint——这就是 sidecar 为什么带一个
loopback 监听器（`--callback-listen`，默认 `127.0.0.1:3000`）：本地开发好用，在 Runtime 上
完全不行，因为那时浏览器和容器不在同一台机器上。

**这两个 URL 不能互换。** 把 `resourceOauth2ReturnUrl` 指向第一跳那个地址是能通过校验的
——加进白名单后 API 就接受了——然后只有在真的有人完成登录时才会失败，因为那个 endpoint 要求
`authorizationCode` 和 `state`：

```
2 validation errors detected: Value at 'authorizationCode' failed to satisfy
constraint: Member must not be null; Value at 'state' failed to satisfy
constraint: Member must not be null
```

## 流程

```
                  ┌─ 操作者的浏览器 ─────────────────────────────┐
                  │                                              │
     lark_auth_start                                             │
          │  authorization_url（带签名 state）                    │
          ▼                                                      ▼
   AgentCore Identity ──────────────────────────────▶  accounts.feishu.cn
                                authorize                （登录 + 授权）
          ▲                                                      │
          │  第一跳：authorizationCode + state                     │
          └──────────────────────────────────────────────────────┘
          │
          │  第二跳：带 session_id + state 的跳转
          ▼
   CloudFront + WAF  ──── OAC 签名（SigV4）────▶  Lambda function URL
   （唯一的公网入口）                              （AuthType AWS_IAM，
          │                                        直连返回 403）
          │  1. path token 不匹配就拒（404）
          │  2. state HMAC 校验不过、或超过 10 分钟就拒（400）
          │  3. CompleteResourceTokenAuth(sessionUri, userId)
          ▼
   "Feishu authorization complete" 页面

   token 已进 vault；下一次 lark_exec 直接解析出来，不再提示。
```

飞书从不跟这个 endpoint 通信。它只会跳到第一跳。

## 资源布局

全部在持有 AgentCore Identity 的**同一个账号**里，没有跨账号信任：

```
├── CloudFront distribution      ← 唯一公网入口，挂 WAF
│     └── origin access control  ← 用 SigV4 签名到 origin 的请求
├── WAF WebACL                   ← rate limit + AWS 托管规则集
├── lark-session-binding (Lambda)
│     └── function URL           ← AuthType AWS_IAM，resource policy 只允许
│                                   cloudfront.amazonaws.com + 这个 distribution
├── LarkSessionBindingLambdaRole
│     • CompleteResourceTokenAuth（限定到一个 workload identity + 一个 provider）
│     • GetSecretValue（只能读 provider 那一个 secret）
│     • 只写自己那个 log group
└── feishu-mcp-workload 的 allowedResourceOauth2ReturnUrls 加上 CloudFront 域名
```

**为什么不跨账号**：早先的版本把 Lambda 放在操作者自己的账号里，通过跨账号 role 去调
`CompleteResourceTokenAuth`（token vault 不支持 resource policy，跨账号只能这么做）。那个
trust policy 会被内部安全扫描判定为"允许组织外主体 assume"。同账号部署把这个问题整个去掉，
也少了 `AssumeRole`、`ExternalId` 和两个环境变量。

如果确实需要 Lambda 和 Token Vault 分账号，`handler.py` 里改一处即可（用 `sts:AssumeRole`
拿临时凭证再建 client），但要预期那条 trust policy 会被扫描告警。

## 安全模型

浏览器跳转带不了 SigV4，所以最外层那一跳必须是未认证的。下面几层是为此补偿的。

| 层 | 挡住什么 |
|---|---|
| CloudFront + OAC | 直连 Lambda function URL 返回 403。resource policy 里没有 wildcard principal，只有 `cloudfront.amazonaws.com` + `AWS:SourceArn` 锁到那一个 distribution |
| WAF | rate limit（100 req/5min per IP）+ AWS 托管的 CommonRuleSet 和 KnownBadInputs |
| 不可猜的 path 段 | 对 distribution 的扫描；path 不对只回一个什么都不透露的 404 |
| HMAC 签名的 `customState` | 不是本部署发起的请求。这是 AWS [明确推荐](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html)的做法：只有 sidecar 持有密钥 |
| state 10 分钟 TTL | 旧跳转的重放。时间戳在签名内容里，改不了 |
| 最小权限执行角色 | 即使 Lambda 被完全攻破，也读不到 token、发不起授权——只能绑定一个真实登录已经产生的 session |

CloudFront + OAC 这个组合是 AWS 认可的标准形态，Security Hub 有对应的 control
（[CloudFront.16](https://docs.aws.amazon.com/securityhub/latest/userguide/cloudfront-controls.html)）
**要求**给 Lambda function URL origin 配 OAC。

**故意没实现的部分。** AWS 示例会校验"发起授权的用户 == 完成授权的用户"，方式是读浏览器
session cookie。这是一个单租户部署，只有一个固定 `userId`，没有 web session 可查，所以那道
校验用签名 state 替代了，而不是实现了。多租户部署应该实现它：示例代码见上面那篇
session-binding 文档。

`AGENTCORE_STATE_SECRET` 以明文存在 Lambda 和 runtime 两侧的环境变量里，所以能读到任一份
配置的人都能伪造 state。但要利用它仍然需要一个未过期的 `session_id`，也就是得先完成一次飞书
登录。在意这点的部署可以把两边都换成 Secrets Manager，代价是两边各多一次查询。

## 部署

`./deploy.sh` 会创建全部资源，并打印需要设到 runtime 上的两个值。脚本是幂等的——改完
policy 或 handler 重跑是安全的。

```sh
cd deploy/session-binding
./deploy.sh                # 用当前 AWS profile；AWS_REGION 默认 us-east-1
```

新建的 distribution 要 5-10 分钟才会变成 `Deployed`。之后把打印出来的环境变量设到 runtime：

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --environment-variables "AGENTCORE_RETURN_URL=<打印值>,AGENTCORE_STATE_SECRET=<打印值>" \
  ... # 以及 update-agent-runtime 其余必填参数
```

两个都必须设。只设 `AGENTCORE_RETURN_URL` 会让浏览器跳到 CloudFront 但没有 state 可验，
每次回调都会被拒。

验证方式：调 `lark_auth_start` 并完成登录，浏览器最后应该停在 "Feishu authorization
complete"，然后 `lark_exec` 直接可用，不需要调 `lark_auth_complete`。同时直连 Lambda
function URL 应该返回 403。

## 回退

清掉那两个环境变量，runtime 就指回 loopback 监听器。手工流程立即恢复，别的都不用改。

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> --environment-variables "{}" ...
```

## 文件

| 文件 | 用途 |
|---|---|
| `handler.py` | Lambda 本体：校验请求、调 `CompleteResourceTokenAuth`、渲染结果页 |
| `deploy.sh` | 创建 IAM、Lambda、function URL、OAC、WAF、CloudFront，以及 return-URL 白名单条目 |

IAM policy 和 WAF 规则是写在 `deploy.sh` 里的 heredoc，不是单独的 json 文件，因为里面要插
账号 ID、region 和 secret ARN。`grep -n 'Sid' deploy.sh` 可以快速扫出各段 policy。

## 故障排查

失败信息落在 Lambda 自己的 log group（`/aws/lambda/lark-session-binding`）。浏览器只会看到
一句笼统的提示。

| 现象 | 原因 |
|---|---|
| 直连 function URL 返回 403 | **这是预期行为。** OAC 生效后只有 CloudFront 能调 |
| 走 CloudFront 也返回 403 | resource policy 里的 `AWS:SourceArn` 和实际 distribution 不匹配，或 function URL 的 `AuthType` 不是 `AWS_IAM`；重跑 `deploy.sh` |
| CloudFront 返回 502/503 | distribution 还在 `InProgress`，或 origin request policy 不是 `AllViewerExceptHostHeader`（origin 收到 CloudFront 的 Host 头会拒签名） |
| `state rejected: state signature mismatch` | runtime 和 Lambda 两边的 `AGENTCORE_STATE_SECRET` 不一致 |
| `state rejected: malformed state` | runtime 上没设 `AGENTCORE_STATE_SECRET`，所以压根没发 state |
| `AccessDeniedException ... secretsmanager:GetSecretValue` | 执行角色读不到 provider 的 client secret，而 `CompleteResourceTokenAuth` 内部需要它去兑换 code |
| `AccessDeniedException: Invalid or expired session` | session URI 已被消费过，或超过 10 分钟。用 `lark_auth_start` 重来 |
| 浏览器显示 `authorizationCode ... must not be null` | `AGENTCORE_RETURN_URL` 指向了第一跳的 callback，而不是这个 endpoint |
| 回调页面被缓存 | cache policy 必须是 `CachingDisabled`；`session_id` 是一次性的，缓存会导致重放失败 |

---

<a name="english"></a>

# Session-binding endpoint (optional)

Removes the manual step from first-time Feishu authorization. Without it the flow
still works — the agent just makes one extra `lark_auth_complete` call. With it,
completing the browser login binds the session on its own.

## Why a separate endpoint is needed

AgentCore Identity's authorization-code flow has **two** redirect hops, and they
are easy to conflate:

| Hop | Who redirects | Where it lands | Query params | Who hosts it |
|---|---|---|---|---|
| 1 | Feishu | the credential provider's `callbackUrl` | `authorizationCode`, `state` | AWS |
| 2 | AgentCore Identity | `resourceOauth2ReturnUrl` (the *session binding URL*) | `session_id`, `state` | **you** |

Hop 1 is already configured: `GetOauth2CredentialProvider` returns a
`callbackUrl` containing a per-provider UUID, and that URL is registered as the
redirect URL on the Feishu app. Nothing here changes it.

Hop 2 is the one this directory implements. AWS is explicit that it is the
customer's responsibility once the agent runs on AgentCore Runtime:

> when deploying your agent code to AgentCore Runtime, your web application that
> connects to the agent runtime must host a publicly accessible HTTPS callback
> endpoint itself, the callback endpoint must be registered against the workload
> identity as an `AllowedResourceOAuth2ReturnUrl` by calling
> `UpdateWorkloadIdentity` [...] and then call the `CompleteResourceTokenAuth`
> API
>
> — [OAuth 2.0 authorization URL session binding](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html)

The same page notes that `agentcore dev` hosts this endpoint for you *locally*,
which is why the sidecar ships a loopback listener (`--callback-listen`, default
`127.0.0.1:3000`) that works fine during development and not at all on Runtime,
where the browser and the container are on different machines.

**The two URLs are not interchangeable.** Pointing `resourceOauth2ReturnUrl` at
hop 1's URL passes validation — the API accepts it once allowlisted — and then
fails only when a human completes a login, because that endpoint requires
`authorizationCode` and `state`:

```
2 validation errors detected: Value at 'authorizationCode' failed to satisfy
constraint: Member must not be null; Value at 'state' failed to satisfy
constraint: Member must not be null
```

## Flow

```
                  ┌─ operator's browser ─────────────────────────┐
                  │                                              │
     lark_auth_start                                             │
          │  authorization_url (+ signed state)                  │
          ▼                                                      ▼
   AgentCore Identity ──────────────────────────────▶  accounts.feishu.cn
                                authorize                  (login + consent)
          ▲                                                      │
          │  hop 1: authorizationCode + state                     │
          └──────────────────────────────────────────────────────┘
          │
          │  hop 2: redirect with session_id + state
          ▼
   CloudFront + WAF ──── OAC-signed (SigV4) ────▶  Lambda function URL
   (the only public entry)                          (AuthType AWS_IAM;
          │                                          direct access → 403)
          │  1. reject unless the path token matches (404)
          │  2. reject unless the state HMAC validates, < 10 min old (400)
          │  3. CompleteResourceTokenAuth(sessionUri, userId)
          ▼
   "Feishu authorization complete" page

   Token now in the vault; the next lark_exec resolves it with no prompt.
```

Feishu never talks to this endpoint. It only ever redirects to hop 1.

## Resource layout

Everything sits in the **same account** as AgentCore Identity — no cross-account
trust:

```
├── CloudFront distribution      ← the only public entry point, fronted by WAF
│     └── origin access control  ← signs requests to the origin with SigV4
├── WAF WebACL                   ← rate limiting + AWS managed rule sets
├── lark-session-binding (Lambda)
│     └── function URL           ← AuthType AWS_IAM; resource policy allows only
│                                  cloudfront.amazonaws.com for this distribution
├── LarkSessionBindingLambdaRole
│     • CompleteResourceTokenAuth (one workload identity, one provider)
│     • GetSecretValue (that provider's secret only)
│     • writes to its own log group only
└── the CloudFront domain added to feishu-mcp-workload's
    allowedResourceOauth2ReturnUrls
```

**Why single-account.** An earlier revision put the Lambda in the operator's own
account and reached `CompleteResourceTokenAuth` through a cross-account role
(token vaults carry no resource policy, so that is the only route). Internal
security scanners flag that trust policy as allowing assumption by a principal
outside the organization. Deploying in one account removes the problem entirely,
along with `AssumeRole`, `ExternalId`, and two environment variables.

If the Lambda genuinely has to live apart from the Token Vault, one change in
`handler.py` covers it — assume a scoped role and build the client from those
credentials — but expect the trust policy to be flagged.

## Security model

A browser redirect cannot carry SigV4, so the outermost hop has to be
unauthenticated. These layers stand in for it.

| Layer | What it stops |
|---|---|
| CloudFront + OAC | A direct request to the function URL returns 403. The resource policy has no wildcard principal — only `cloudfront.amazonaws.com` with `AWS:SourceArn` pinned to one distribution |
| WAF | Rate limiting (100 req/5 min per IP) plus the AWS managed CommonRuleSet and KnownBadInputs |
| Unguessable path segment | Scans of the distribution; anything else returns a bare 404 that reveals nothing |
| HMAC-signed `customState` | Requests this deployment did not initiate. AWS [recommends this explicitly](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html): only the sidecar holds the key |
| 10-minute state TTL | Replay of an old redirect. The timestamp is inside the signed material, so it cannot be advanced |
| Least-privilege execution role | A full compromise of the Lambda still cannot read a token or start an authorization — only bind a session a real login already produced |

CloudFront in front of a Lambda function URL with OAC is the shape AWS itself
prescribes: Security Hub control
[CloudFront.16](https://docs.aws.amazon.com/securityhub/latest/userguide/cloudfront-controls.html)
*requires* OAC for Lambda function URL origins.

**What is deliberately not implemented.** The AWS sample verifies that the user
who *started* the authorization is the user *completing* it, by reading a browser
session cookie. This is a single-tenant deployment with one fixed `userId` and no
web session to inspect, so that check is replaced by the signed state rather than
implemented. For a multi-tenant deployment, implement it: see the sample code in
the session-binding doc linked above.

`AGENTCORE_STATE_SECRET` is stored in plaintext in both the Lambda's and the
runtime's environment variables, so anyone who can read either configuration can
forge a state. Exploiting that still requires an unexpired `session_id`, which
means completing a Feishu login first. Deployments that care can move both sides
to Secrets Manager at the cost of a lookup on each side.

## Deploy

`./deploy.sh` provisions everything and prints the two values to set on the
runtime. It is idempotent — safe to re-run after changing a policy or the handler.

```sh
cd deploy/session-binding
./deploy.sh                # uses the current AWS profile; AWS_REGION defaults to us-east-1
```

A new distribution takes 5-10 minutes to reach `Deployed`. Then apply the printed
environment variables to the runtime:

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --environment-variables "AGENTCORE_RETURN_URL=<printed>,AGENTCORE_STATE_SECRET=<printed>" \
  ... # plus the other required update-agent-runtime arguments
```

Both must be set. `AGENTCORE_RETURN_URL` alone sends the browser to CloudFront
with no state to verify, and every callback is rejected.

To verify, call `lark_auth_start` and complete the login: the browser should end
on "Feishu authorization complete", `lark_exec` should work without any call to
`lark_auth_complete`, and a direct request to the function URL should return 403.

## Rollback

Point the runtime back at the loopback listener by clearing the two variables.
The manual flow resumes immediately; nothing else needs to change.

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> --environment-variables "{}" ...
```

## Files

| File | Purpose |
|---|---|
| `handler.py` | The Lambda: verifies the request, calls `CompleteResourceTokenAuth`, renders the result page |
| `deploy.sh` | Provisions the IAM role, Lambda, function URL, OAC, WAF, CloudFront, and the return-URL allowlist entry |

The IAM policies and WAF rules live as heredocs inside `deploy.sh` rather than as
separate JSON files, since they interpolate the account ID, region, and the
provider's secret ARN. `grep -n 'Sid' deploy.sh` lists them.

## Troubleshooting

Failures land in the Lambda's own log group
(`/aws/lambda/lark-session-binding`). The browser only ever sees a generic
message.

| Symptom | Cause |
|---|---|
| Direct request to the function URL returns 403 | **Expected.** With OAC in place only CloudFront can invoke it |
| Requests *through* CloudFront also return 403 | The `AWS:SourceArn` in the resource policy does not match the distribution, or the function URL's `AuthType` is not `AWS_IAM`; re-run `deploy.sh` |
| CloudFront returns 502/503 | The distribution is still `InProgress`, or the origin request policy is not `AllViewerExceptHostHeader` (the origin rejects the signature if it receives CloudFront's Host header) |
| `state rejected: state signature mismatch` | `AGENTCORE_STATE_SECRET` differs between the runtime and the Lambda |
| `state rejected: malformed state` | The runtime has no `AGENTCORE_STATE_SECRET`, so it is not sending a state at all |
| `AccessDeniedException ... secretsmanager:GetSecretValue` | The execution role cannot read the provider's client secret, which `CompleteResourceTokenAuth` needs internally to redeem the code |
| `AccessDeniedException: Invalid or expired session` | The session URI was already consumed or is older than 10 minutes. Start over with `lark_auth_start` |
| Browser shows `authorizationCode ... must not be null` | `AGENTCORE_RETURN_URL` is pointing at hop 1's callback instead of this endpoint |
| The callback page gets cached | The cache policy must be `CachingDisabled`; `session_id` is single-use, and a cached response makes the replay fail |
