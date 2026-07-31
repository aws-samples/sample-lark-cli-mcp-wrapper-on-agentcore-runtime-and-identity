[中文](#中文) | [English](#english)

---

<a name="中文"></a>

# Session-binding endpoint（可选）

去掉首次飞书授权里那一步手工操作。不部署也能用——只是需要操作者把 `session_id` 交给
`lark_auth_complete`。想让浏览器跳转自己把活干完，就部署这个。

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

[`awslabs/agentcore-samples`](https://github.com/awslabs/agentcore-samples) 里的每一个
示例都遵循同样的分工：provider 的 `callbackUrl` 注册给 IdP，而
`allowedResourceOauth2ReturnUrls` 永远指向一个客户自己托管的 server。

## 流程

```
                  ┌─ 操作者的浏览器 ─────────────────────────────┐
                  │                                              │
     lark_auth_start                                             │
          │  authorization_url（带签名 state）                    │
          ▼                                                      ▼
   AgentCore Identity ──────────────────────────────▶  accounts.feishu.cn
   （agent 账号）                 authorize                （登录 + 授权）
          ▲                                                      │
          │  第一跳：authorizationCode + state                     │
          └──────────────────────────────────────────────────────┘
          │
          │  第二跳：带 session_id + state 的跳转
          ▼
   Lambda function URL（operator 账号）
          │  1. path token 不匹配就拒
          │  2. state HMAC 校验不过、或超过 10 分钟就拒
          │  3. sts:AssumeRole ──▶ LarkCliMcpSessionBindingRole（agent 账号）
          │  4. CompleteResourceTokenAuth(sessionUri, userId)
          ▼
   "Feishu authorization complete" 页面

   token 已进 vault；下一次 lark_exec 直接解析出来，不再提示。
```

飞书从不跟这个 endpoint 通信。它只会跳到第一跳。

## 跨账号布局

这套部署故意把两个账号拆开：AgentCore 相关资源放在管控较严的公司账号里，而公开 endpoint 放在
操作者自己的账号里——在那里暴露一个 function URL 不是个合规问题。

```
OPERATOR 账号                            AGENT 账号
├── lark-session-binding  (Lambda)      ├── feishu-mcp-workload   (workload identity)
├── function URL, AuthType NONE         ├── feishu-oauth-provider (credential provider)
├── LarkSessionBindingLambdaRole        ├── LarkCliMcpSessionBindingRole
│     • sts:AssumeRole 只到那一个 role   │     • 只信 operator 的 Lambda role
│     • 只写自己账号的 log group         │       + ExternalId
└── /aws/lambda/lark-session-binding    │     • CompleteResourceTokenAuth（收窄）
                                        │     • 只能读 provider 那一个 secret
                                        └── AgentCore runtime
```

trust policy 是两个账号之间唯一的联系。两边都不往对方写日志：Lambda role 的 `logs:*` 授权
写死了自己那个 log group 的 ARN，所以跨账号写是被 IAM 拒掉的，不只是"没配"。

跨账号 role 是必需的，不是风格选择——token vault 不支持 resource policy，所以
`CompleteResourceTokenAuth` 没法跨账号直接授权。把 Lambda 直接部署到 agent 账号里就能完全
去掉 trust 关系和 `AssumeRole` 这一跳；handler 代码不用改，只要去掉
`AGENTCORE_ASSUME_ROLE_ARN`/`AGENTCORE_EXTERNAL_ID`，把那两个权限直接给执行角色即可。

## 安全模型

给浏览器跳转用的 function URL 必须是 `AuthType: NONE`——跳转带不了 SigV4 签名。下面几层都是
为了补偿这一点。

| 层 | 挡住什么 |
|---|---|
| 不可猜的 path 段 | 对 `lambda-url.*.on.aws` 命名空间的扫描；path 不对只回一个什么都不透露的 404 |
| HMAC 签名的 `customState` | 不是本部署发起的请求。这是 AWS [明确推荐](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html)的做法：只有 sidecar 持有密钥 |
| state 10 分钟 TTL | 旧跳转的重放。时间戳在签名内容里，改不了 |
| 最小权限的 assumed role | 即使 Lambda 被完全攻破，也读不到 token、发不起授权——只能绑定一个真实登录已经产生的 session |
| trust policy 上的 `ExternalId` | 知道 role ARN 但不知道这个共享值的人 |

**故意没实现的部分。** AWS 示例会校验"发起授权的用户 == 完成授权的用户"，方式是读浏览器
session cookie。这是一个单租户部署，只有一个固定 `userId`，没有 web session 可查，所以那道
校验用签名 state 替代了，而不是实现了。多租户部署应该实现它：示例代码见上面那篇
session-binding 文档。

`AGENTCORE_STATE_SECRET` 以明文存在 Lambda 和 runtime 两侧的环境变量里，所以能读到任一份
配置的人都能伪造 state。但要利用它仍然需要一个未过期的 `session_id`，也就是得先完成一次飞书
登录。在意这点的部署可以把两边都换成 Secrets Manager（secret 支持跨账号 resource policy），
代价是两边各多一次查询。

## 部署

`./deploy.sh` 会创建全部资源，并打印需要设到 runtime 上的两个值。脚本是幂等的——改完 policy
重跑是安全的。

```sh
export OPERATOR_PROFILE=self           # 托管 Lambda 的账号
export AGENT_PROFILE=default           # 有 AgentCore Identity 的账号
./deploy.sh
```

然后把打印出来的环境变量设到 runtime 上：

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --environment-variables "AGENTCORE_RETURN_URL=<打印值>,AGENTCORE_STATE_SECRET=<打印值>" \
  ... # 以及 update-agent-runtime 其余必填参数
```

两个都必须设。只设 `AGENTCORE_RETURN_URL` 会让浏览器跳到 Lambda 但没有 state 可验，每次
回调都会被拒。

验证方式：调 `lark_auth_start` 并完成登录，浏览器最后应该停在 "Feishu authorization
complete"，然后 `lark_exec` 直接可用，不需要调 `lark_auth_complete`。

## 回退

清掉那两个环境变量，runtime 就指回 loopback 监听器。手工流程立即恢复，别的都不用改，Lambda
留着或删掉都行。

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> --environment-variables "{}" ...
```

## 文件

| 文件 | 用途 |
|---|---|
| `handler.py` | Lambda 本体：校验请求、assume 跨账号 role、调 `CompleteResourceTokenAuth`、渲染结果页 |
| `deploy.sh` | 创建两个账号的 IAM、Lambda、function URL，以及 return-URL 白名单条目 |

IAM policy 是写在 `deploy.sh` 里的 heredoc，不是单独的 json 文件，因为里面要插账号 ID、
region 和 secret ARN。`grep -n 'Sid' deploy.sh` 可以快速扫出四段 policy。

## 故障排查

失败信息落在 Lambda 自己的 log group（operator 账号下的
`/aws/lambda/lark-session-binding`）。浏览器只会看到一句笼统的提示。

| 现象 | 原因 |
|---|---|
| function URL 所有请求都返回 403 | 2025 年 10 月起，`AuthType: NONE` 的 URL 需要在 resource policy 里**同时**有 `lambda:InvokeFunctionUrl` 和 `lambda:InvokeFunction`。`deploy.sh` 两条都加了 |
| `state rejected: state signature mismatch` | runtime 和 Lambda 两边的 `AGENTCORE_STATE_SECRET` 不一致 |
| `state rejected: malformed state` | runtime 上没设 `AGENTCORE_STATE_SECRET`，所以压根没发 state |
| `sts:AssumeRole` 报 `AccessDenied` | trust policy 或 `ExternalId` 不匹配；重跑 `deploy.sh` |
| `AccessDeniedException ... secretsmanager:GetSecretValue` | assumed role 读不到 provider 的 client secret，而 `CompleteResourceTokenAuth` 内部需要它去兑换 code |
| `AccessDeniedException: Invalid or expired session` | session URI 已被消费过，或超过 10 分钟。用 `lark_auth_start` 重来 |
| 浏览器显示 `authorizationCode ... must not be null` | `AGENTCORE_RETURN_URL` 指向了第一跳的 callback，而不是这个 endpoint |

---

<a name="english"></a>

# Session-binding endpoint (optional)

Removes the manual step from first-time Feishu authorization. Without it the
flow still works — the operator hands `session_id` to `lark_auth_complete`
instead. Deploy this when you want the browser redirect to finish the job by
itself.

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

Every sample in [`awslabs/agentcore-samples`](https://github.com/awslabs/agentcore-samples)
follows the same split: the provider `callbackUrl` is registered with the IdP,
and `allowedResourceOauth2ReturnUrls` always points at a customer-hosted server.

## Flow

```
                  ┌─ operator's browser ─────────────────────────┐
                  │                                              │
     lark_auth_start                                             │
          │  authorization_url (+ signed state)                  │
          ▼                                                      ▼
   AgentCore Identity ──────────────────────────────▶  accounts.feishu.cn
   (agent account)               authorize                  (login + consent)
          ▲                                                      │
          │  hop 1: authorizationCode + state                     │
          └──────────────────────────────────────────────────────┘
          │
          │  hop 2: redirect with session_id + state
          ▼
   Lambda function URL  (operator account)
          │  1. reject unless path token matches
          │  2. reject unless state HMAC validates and is < 10 min old
          │  3. sts:AssumeRole ──▶ LarkCliMcpSessionBindingRole (agent acct)
          │  4. CompleteResourceTokenAuth(sessionUri, userId)
          ▼
   "Feishu authorization complete" page

   Token now in the vault; the next lark_exec resolves it with no prompt.
```

Feishu never talks to this endpoint. It only ever redirects to hop 1.

## Cross-account layout

This deployment splits the two accounts deliberately: the AgentCore resources
live in a company account with tighter controls, while the public endpoint lives
in the operator's own account, where exposing a function URL is not a
governance question.

```
OPERATOR account                        AGENT account
├── lark-session-binding  (Lambda)      ├── feishu-mcp-workload   (workload identity)
├── function URL, AuthType NONE         ├── feishu-oauth-provider (credential provider)
├── LarkSessionBindingLambdaRole        ├── LarkCliMcpSessionBindingRole
│     • sts:AssumeRole → that role only │     • trusts the operator Lambda role
│     • logs to its OWN log group only  │       + ExternalId
└── /aws/lambda/lark-session-binding    │     • CompleteResourceTokenAuth (scoped)
                                        │     • GetSecretValue on the provider
                                        │       secret only
                                        └── the AgentCore runtime
```

The trust policy is the only link between the accounts. Neither writes logs into
the other: the Lambda role's `logs:*` grant names its own log group ARN, so a
cross-account write is denied by IAM rather than merely unconfigured.

A cross-account role is required, not a style choice — token vaults carry no
resource policy, so `CompleteResourceTokenAuth` cannot be granted across
accounts directly. Deploying the Lambda into the agent account instead removes
the trust relationship and the `AssumeRole` hop entirely; the handler works
unchanged, just drop `AGENTCORE_ASSUME_ROLE_ARN`/`AGENTCORE_EXTERNAL_ID` and
grant the two permissions to the execution role.

## Security model

A function URL serving browser redirects must be `AuthType: NONE` — a redirect
cannot carry a SigV4 signature. Everything below compensates for that.

| Layer | What it stops |
|---|---|
| Unguessable path segment | Scans of the `lambda-url.*.on.aws` namespace; wrong path returns a bare 404 that reveals nothing |
| HMAC-signed `customState` | Requests this deployment did not initiate. AWS [recommends this explicitly](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/oauth2-authorization-url-session-binding.html): only the sidecar holds the key |
| 10-minute state TTL | Replay of an old redirect. The timestamp is inside the signed material, so it cannot be advanced |
| Least-privilege assumed role | A full compromise of the Lambda still cannot read a token or start an authorization — only bind a session that a real login already produced |
| `ExternalId` on the trust policy | Anyone who learns the role ARN but not the shared value |

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
to Secrets Manager (the secret supports a cross-account resource policy) at the
cost of a lookup on each side.

## Deploy

`./deploy.sh` provisions everything and prints the two values to set on the
runtime. It is idempotent — safe to re-run after changing a policy.

```sh
export OPERATOR_PROFILE=self           # account hosting the Lambda
export AGENT_PROFILE=default           # account with AgentCore Identity
./deploy.sh
```

Then apply the printed environment variables to the runtime:

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --environment-variables "AGENTCORE_RETURN_URL=<printed>,AGENTCORE_STATE_SECRET=<printed>" \
  ... # plus the other required update-agent-runtime arguments
```

Both must be set. `AGENTCORE_RETURN_URL` alone sends the browser to the Lambda
with no state to verify, and every callback is rejected.

To verify, call `lark_auth_start` and complete the login: the browser should end
on "Feishu authorization complete", and `lark_exec` should work without any call
to `lark_auth_complete`.

## Rollback

Point the runtime back at the loopback listener by clearing the two variables.
The manual flow resumes immediately; nothing else needs to change, and the
Lambda can be left in place or deleted.

```sh
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> --environment-variables "{}" ...
```

## Files

| File | Purpose |
|---|---|
| `handler.py` | The Lambda: verifies the request, assumes the cross-account role, calls `CompleteResourceTokenAuth`, renders the result page |
| `deploy.sh` | Provisions both accounts' IAM, the Lambda, its function URL, and the return-URL allowlist entry |

The IAM policies live as heredocs inside `deploy.sh` rather than as separate JSON
files, since they interpolate account IDs, the region, and the provider's secret
ARN. `grep -n 'Sid' deploy.sh` lists all four.

## Troubleshooting

Failures land in the Lambda's own log group (`/aws/lambda/lark-session-binding`
in the operator account). The browser only ever sees a generic message.

| Symptom | Cause |
|---|---|
| Function URL returns 403 for every request | Since October 2025 an `AuthType: NONE` URL needs **both** `lambda:InvokeFunctionUrl` and `lambda:InvokeFunction` in the resource policy. `deploy.sh` adds both |
| `state rejected: state signature mismatch` | `AGENTCORE_STATE_SECRET` differs between the runtime and the Lambda |
| `state rejected: malformed state` | The runtime has no `AGENTCORE_STATE_SECRET`, so it is not sending a state at all |
| `AccessDenied` on `sts:AssumeRole` | The trust policy or `ExternalId` does not match; re-run `deploy.sh` |
| `AccessDeniedException ... secretsmanager:GetSecretValue` | The assumed role cannot read the provider's client secret, which `CompleteResourceTokenAuth` needs internally to redeem the code |
| `AccessDeniedException: Invalid or expired session` | The session URI was already consumed or is older than 10 minutes. Start over with `lark_auth_start` |
| Browser shows `authorizationCode ... must not be null` | `AGENTCORE_RETURN_URL` is pointing at hop 1's callback instead of this endpoint |
