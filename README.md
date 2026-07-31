[中文](#中文) | [English](#english)

---

<a name="中文"></a>

# 在 AgentCore Runtime + Identity 上运行的 lark-cli MCP wrapper

把 [lark-cli](https://github.com/larksuite/cli)（飞书/Lark 官方 CLI）封装成一个 MCP
server，部署在 AWS Bedrock AgentCore Runtime 上，飞书 token 通过 AgentCore Identity
的 Token Vault（OAuth 3LO）按用户解析，而不是自己维护一套 token 存储。设计目标是让外部
调用方（例如 AWS DevOps Agent）能像用普通 MCP 工具一样调用它，完全不需要了解飞书的 OAuth
流程——token 解析全部封在这个 server 内部。

本仓库**没有** fork lark-cli。它依赖官方 `github.com/larksuite/cli` 模块的一个固定版本
（用 lark-cli 自带的 `authsidecar` build tag 编译——这个 tag 本来就在 lark-cli 上游
main 分支上，不是这里加的），只复制了一个很小、很少变动的文件
（`internal/larkendpoints/endpoints.go`，一张静态的飞书/Lark 域名表），因为 Go 的
`internal/` 可见性规则让原文件从外部模块真的没法 import。升级 lark-cli 版本后如果想确认这份
副本有没有漂移，见 `internal/larkendpoints/check_drift.sh`。

## 前置条件

需要一个飞书自建应用（App ID / App Secret、重定向 URL、权限、发版），以及 AWS 侧的
AgentCore Identity credential provider 和 workload identity。飞书那侧全都要在开放平台
后台手点，且顺序和 AWS 侧交替——步骤和检查清单见
[`docs/FEISHU_APP_SETUP.md`](docs/FEISHU_APP_SETUP.md)。

## 架构

### 请求路径

```
 DevOps Agent（或任意 MCP 客户端）
     │  MCP over Gateway (SigV4)
     ▼
 AgentCore Gateway
     │
     ▼
╭─ AgentCore Runtime 容器（本仓库的镜像）────────────────────────────────╮
│                                                                      │
│  mcp-wrapper                    0.0.0.0:8000/mcp                     │
│    · MCP 协议适配层，每次工具调用 shell 出 lark-cli                    │
│         │                                                            │
│         ▼  argv                                                      │
│  lark-cli  (-tags authsidecar)                                       │
│    · 沙箱化：只会看到哨兵凭证                                          │
│         │                                                            │
│         ▼  loopback 上的 HMAC 签名 HTTP                               │
│  sidecar-agentcore-identity     127.0.0.1:16385                      │
│    · 唯一能看到真实飞书 token 的进程                                   │
│                                                                      │
╰──────────┬───────────────────────────────────────────────────────────╯
           │  GetWorkloadAccessTokenForUserId
           │  GetResourceOauth2Token
           ▼
 AgentCore Identity —— Token Vault
           │  缓存 / 刷新后的 user_access_token
           ▼
 open.feishu.cn
```

- **lark-cli** 从不持有真实 token。用 `-tags authsidecar` 编译，通过同机 HMAC 签名的
  HTTP 协议（`LARKSUITE_CLI_AUTH_PROXY`/`LARKSUITE_CLI_PROXY_KEY`）和 sidecar 通信，
  只会看到一个哨兵凭证——即使 lark-cli 进程被攻破，也没有真实 token 可偷。
- **sidecar-agentcore-identity** 是唯一见过真实飞书 access token 的进程。它从
  AgentCore Identity 的 Token Vault 里解析出 token，按请求通过 sidecar 协议交给 lark-cli。
- **mcp-wrapper** 是 MCP 协议适配层（Streamable HTTP，
  `initialize`/`tools/list`/`tools/call`），独立的 Go module，每次工具调用 shell 出
  `lark-cli` 二进制。

### 授权路径（3LO）

两次跳转，由不同方托管。把这两个搞混是配错这套部署最常见的原因，所以分开画：

```
  lark_auth_start
       │  GetResourceOauth2Token(ForceAuthentication, customState)
       ▼
  AgentCore Identity ──────── authorization_url ────────▶ 操作者的浏览器
                                                                │
                                                                │ 登录 + 授权
                                                                ▼
                                                        accounts.feishu.cn
                                                                │
   ┌──────── 第一跳：authorizationCode + state ◀────────────────┘
   │         （provider 的 callbackUrl —— AWS 托管，
   │           注册在飞书应用的 redirect URL 里）
   ▼
  AgentCore Identity
   │
   │  第二跳：带 session_id + state 的跳转
   │  （resourceOauth2ReturnUrl —— 即 "session binding URL"，需要你自己托管）
   ▼
  ┌─────────────────────────────┬──────────────────────────────────────┐
  │  没有 session-binding       │  部署了 session-binding endpoint     │
  │  endpoint（默认）           │  （deploy/session-binding/）         │
  ├─────────────────────────────┼──────────────────────────────────────┤
  │  → http://localhost:3000    │  → https://…/<token>（CloudFront）   │
  │    浏览器访问不到；页面      │    校验 state，再由 Lambda 调         │
  │    打不开（预期行为）        │    CompleteResourceTokenAuth         │
  │                             │                                      │
  │  调用方把 session_id 传给    │  绑定已完成，直接调 lark_exec 即可   │
  │  lark_auth_complete          │                                      │
  └─────────────────────────────┴──────────────────────────────────────┘
```

默认方式需要一步手工操作。部署
[`deploy/session-binding/`](deploy/session-binding/README.md) 可以去掉这一步——可选，
清掉两个环境变量就能回退。`lark_auth_start` 会在 `session_binding` 字段里报告当前是哪种
模式，所以驱动这个流程的 agent 无论哪种情况都能拿到正确的下一步指引。

完整设计推导和逐步验证记录见
[`docs/AGENTCORE_IDENTITY_MCP_DESIGN.md`](docs/AGENTCORE_IDENTITY_MCP_DESIGN.md) 和
[`docs/AGENTCORE_IDENTITY_MCP_PLAN.md`](docs/AGENTCORE_IDENTITY_MCP_PLAN.md)。

## 配置

这是一个单租户部署：一个 `userId` 在 Token Vault 里绑定一个飞书账号，通过 sidecar 进程的
环境变量设置一次（不是按请求推导——目前没有可用的调用方身份来做 key；DevOps Agent 调这个
MCP server 用的是不带任何终端用户 claim 的服务级 SigV4 身份。多租户按 `actor_id` 分键的
版本需要什么，见 `docs/AGENTCORE_IDENTITY_MCP_PLAN.md`）。

| 环境变量 | 默认值 | 含义 |
|---|---|---|
| `AGENTCORE_USER_ID` | `bot` | 这套部署所绑定飞书账号在 Token Vault 里的 key |
| `AGENTCORE_REGION` | `us-east-1` | AgentCore Identity 所在的 AWS region |
| `AGENTCORE_WORKLOAD_NAME` | `feishu-mcp-workload` | workload identity 名字 |
| `AGENTCORE_CREDENTIAL_PROVIDER_ID` | `feishu-oauth-provider` | OAuth2 credential provider 名字 |
| `AGENTCORE_RETURN_URL` | `http://localhost:3000/callback` | OAuth2 session binding URL —— 即上面的第二跳。必须在 workload identity 的 `allowedResourceOauth2ReturnUrls` 里。指向一个 [session-binding endpoint](deploy/session-binding/README.md) 可让授权全自动 |
| `AGENTCORE_STATE_SECRET` | *（未设置）* | `customState` CSRF token 的 HMAC 密钥。配 `AGENTCORE_RETURN_URL` 时必需；不设置就不发 state，loopback 监听器不需要它 |
| `AGENTCORE_EXTRA_SCOPES` | *（未设置）* | 额外的飞书 scope，逗号或空格分隔。设成 `offline_access` 可申请 refresh token —— 见下文 |
| `LARKSUITE_CLI_APP_ID` | *（必填）* | 飞书应用 ID（`cli_…`）。必须是给 AgentCore Identity credential provider 提供 client 凭证的那个应用，否则 lark-cli 签名用的应用和 vault 里 token 所属的应用不一致 |
| `LARK_CLI_BIN_PATH` | `/usr/local/bin/lark-cli` | wrapper shell 出的 lark-cli 二进制路径 |

`LARK_CLI_BIN_PATH` 在每次 `exec` 前都会校验：路径必须是绝对路径、是普通文件（不能是
symlink 或目录），而且它自己和它的父目录都不能对 group/other 可写。这里指定的二进制会继承
wrapper 的环境变量——包括 `LARKSUITE_CLI_PROXY_KEY`——所以一个别人能覆盖的路径会被拒绝执行。
带 sticky bit 的 world-writable **目录**仍然接受（`/tmp` 是 `1777`），这样本地开发时把
lark-cli 编译到 `/tmp` 的流程还能用。

Token Vault 的槽位是由 workload 名字、`userId` 和**完整 scope 列表**的组合做 key 的，
不是由调用方的 IAM 身份。在你排查一个意外的 `authorization required` 之前，有两个后果值得
先知道：

- 改动 `AGENTCORE_USER_ID`（或者让它回落到跟之前授权时不同的默认值）会把这套部署指向一个
  *不同的*空槽位。之前那次授权还在，只是没被查到。
- 增加、删除或重排 scope 是同样的效果：这个请求会被当成一次全新的授权。

因为槽位跟 IAM 无关，任何有 `bedrock-agentcore` 权限的操作者都可以从任何地方完成授权，
已部署的容器在下一次调用时就会拿到 token。

## 首次授权（3LO）

需要有人在浏览器里完成一次飞书登录。登录**之后**发生什么，取决于有没有部署
session-binding endpoint。

### 默认：一步手工操作

```
1. lark_auth_start          → authorization_url + session_id
2. 打开 URL 登录             → 最后的跳转打不开（预期行为）
3. lark_auth_complete       → 传入第 1 步的 session_id
```

第 3 步只需要 session URI，而第 1 步已经返回了——不需要从浏览器里读任何东西。第 2 步末尾
的跳转指向 sidecar 的 loopback 监听器，在 Runtime 上浏览器访问不到；这个失败只是观感问题。
要在 ~5 分钟内完成，因为底层的 OAuth authorization code 有效期很短。

### 部署了 session-binding endpoint

部署 [`deploy/session-binding/`](deploy/session-binding/README.md) 后第 3 步就消失了：

```
1. lark_auth_start          → authorization_url
2. 打开 URL 登录             → 落在 "Feishu authorization complete" 页面
                              （endpoint 已经调了 CompleteResourceTokenAuth）
3. lark_exec                → 直接可用
```

`lark_auth_start` 会返回 `session_binding: "automatic"` 或 `"manual"`，让驱动流程的
agent 知道自己面对的是哪一种，而不用猜。

### 从 MCP 客户端驱动

`lark_auth_start` / `lark_auth_complete` 就是普通 MCP 工具，任何客户端都能跑这个流程——
包括聊天界面，而一旦部署到 Runtime、sidecar 的 loopback 端点不可达，这就是唯一的选择。
[`skills/feishu-send-message/SKILL.md`](skills/feishu-send-message/SKILL.md) 是一份
针对 AWS DevOps Agent 的实操示例，包含"模糊搜索找不到这些工具"这个加载坑。

有些传输层（在 AWS DevOps Agent 上观察到）会在模型看到之前把 URL 形状的字符串替换成
`<redacted>`。所以 `lark_auth_start` 同时返回 `authorization_url_without_scheme` 和
裸的 `request_uri`，保证至少有一种可用形式能活下来；补上 `https://` 就能拼回去。

### 或者在本地做

```
curl -X POST http://127.0.0.1:16385/_sidecar/auth/start
```

仅 loopback —— 本地开发，或者 `docker exec` 进容器。这里浏览器**能**访问到 callback
监听器（`--callback-listen`，默认 `127.0.0.1:3000`），所以绑定会自己完成，不需要部署任何
endpoint。

### 为什么总是带 `ForceAuthentication`

两条授权路径调 `GetResourceOauth2Token` 都带 `ForceAuthentication: true`。不带它会稳定
复现出后续 `CompleteResourceTokenAuth` 失败，报 `AccessDeniedException: Invalid or
expired session`，无论重试间隔多长都一样。副作用是 `lark_auth_start` 每次都会签发一个新的
授权 URL，即使 vault 里已经有有效 token。

数据面的 token 获取（`ResolveToken`）调同一个 API 时**不带**这个参数，所以缓存或刷新过的
token 会直接返回，不会再提示用户。

## Token 生命周期与重新授权

飞书的 `user_access_token` 有效期 2 小时。这是否意味着每 2 小时要重新授权一次，取决于一个
scope：

| `AGENTCORE_EXTRA_SCOPES` | 行为 |
|---|---|
| 未设置（默认） | 飞书只返回 access token。2 小时后下一次调用会失败报 `authorization required`，必须重走浏览器流程 |
| `offline_access` | 飞书会同时返回 refresh token，Token Vault 用它静默续期。授权实际上变成一次性的 |

默认不申请 `offline_access`，是因为它需要本仓库之外的配套改动：必须在飞书开放平台给应用
开通这个权限，**并且**在授权 scope 里声明
（[飞书文档](https://open.feishu.cn/document/authentication-management/access-token/refresh-user-access-token)）。
申请一个应用没有的 scope 会导致授权失败，所以做成环境变量 opt-in，而不是内置默认值。

注意 Token Vault 的槽位是按 scope 列表分键的，所以加上 `offline_access` 会让现有授权失效，
需要再登录一次浏览器 —— 见[配置](#配置)一节的说明。

## 构建

AgentCore Runtime 要求 `linux/arm64` 镜像。可以在 CodeBuild 上构建（不需要本地 docker
daemon，而且 arm64 是原生而非模拟），也可以本地用 buildx。

### 在 CodeBuild 上

`deploy/agentcore-runtime/buildspec.yml` 驱动一个开了 `privilegedMode` 的
`ARM_CONTAINER` 项目。它需要 `REGISTRY`、`ECR_REPOSITORY`、`IMAGE_TAG` 这几个环境变量；
源码是 S3 上的一个 zip：

```
zip -qr /tmp/source.zip go.mod go.sum internal sidecar mcp-wrapper deploy
aws s3 cp /tmp/source.zip s3://<source-bucket>/lark-cli-mcp-source.zip
aws codebuild start-build --project-name <project> \
  --environment-variables-override name=IMAGE_TAG,value="$(git rev-parse --short HEAD)",type=PLAINTEXT
```

优先用不可变的 tag（git SHA），不要用会移动的 tag。这里的 Go 构建不是逐字节可复现的，所以
用同一个 tag 重新构建同样的源码会产生**不同的** digest，并且会静默地把 tag 从你验证过的
那个镜像上挪走。

### 本地

```
docker buildx build --platform linux/arm64 --provenance=false \
  -f deploy/agentcore-runtime/Dockerfile -t <tag> .
```

`--provenance=false` 很重要：buildx 默认会附加一个 attestation manifest，这会把推上去的
产物变成 OCI image index，而不是 AgentCore Runtime 从 `containerUri` 期望的单架构 image
manifest。

`LARK_CLI_VERSION`（Dockerfile build arg）固定通过
`go install -tags authsidecar github.com/larksuite/cli@<version>` 安装的官方 lark-cli
版本。

## 部署

把镜像推到 ECR，让一个 AgentCore Runtime 资源指向它，前面用 AgentCore Gateway 面向外部
MCP 客户端：

```
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --role-arn <runtime-role-arn> \
  --network-configuration '{"networkMode":"PUBLIC"}' \
  --protocol-configuration '{"serverProtocol":"MCP"}' \
  --agent-runtime-artifact '{"containerConfiguration":{"containerUri":"<registry>/<repo>:<tag>"}}'
```

每次 update 都会创建一个新的 runtime 版本；轮询 `get-agent-runtime` 直到 `status` 变成
`READY`。上一个版本的镜像 tag 还在 ECR 里，所以回滚就是同一条命令换成旧的 `containerUri`。

这套东西依赖的 OAuth2 credential provider、workload identity 和 IAM role 是怎么创建的，
见 `docs/AGENTCORE_IDENTITY_MCP_PLAN.md` 里的 AWS CLI 命令。

### 可选：session-binding endpoint

要去掉手工调 `lark_auth_complete` 这一步，部署
[`deploy/session-binding/`](deploy/session-binding/README.md) 里的 endpoint。它会创建一个
CloudFront + Lambda 的落地页，AgentCore Identity 把浏览器跳转到那里，由它自己完成绑定。

```sh
cd deploy/session-binding
./deploy.sh
```

创建的是 CloudFront + WAF + Lambda：CloudFront 是唯一公网入口，Lambda 的 function URL 用
origin access control 锁到那一个 distribution，直连返回 403。全部在持有 AgentCore Identity
的同一个账号里，没有跨账号信任。脚本会打印需要设到 runtime 上的 `AGENTCORE_RETURN_URL` 和
`AGENTCORE_STATE_SECRET`；把两个都清掉就回退到手工流程。

## 仓库结构

| 路径 | 内容 |
|---|---|
| `mcp-wrapper/` | MCP 协议适配层（独立 Go module）。暴露 `lark_exec`、`lark_list_skills`、`lark_get_skill`、`lark_auth_start`、`lark_auth_complete` |
| `sidecar/` | 认证 sidecar。从 Token Vault 解析真实飞书 token；唯一持有 token 的进程 |
| `internal/larkendpoints/` | 从 lark-cli 复制的静态飞书/Lark 域名表，以及一个漂移检查脚本 |
| `deploy/agentcore-runtime/` | `linux/arm64` runtime 镜像的 Dockerfile、entrypoint 和 CodeBuild buildspec |
| `deploy/session-binding/` | 可选：CloudFront + Lambda endpoint，自动化 OAuth 第二跳 |
| `skills/feishu-send-message/` | 从 AWS DevOps Agent 驱动这些工具的实操示例，含可直接粘贴的 `PROMPT.md` |
| `docs/` | 飞书应用配置步骤（`FEISHU_APP_SETUP.md`）、设计推导和逐步验证记录 |

---

<a name="english"></a>

# lark-cli MCP wrapper on AgentCore Runtime + Identity

Exposes [lark-cli](https://github.com/larksuite/cli) (the official Lark/Feishu CLI) as an
MCP server, deployed on AWS Bedrock AgentCore Runtime, with per-user Feishu tokens
resolved via AgentCore Identity's Token Vault (OAuth 3LO) instead of a self-managed
token store. Built so external consumers (e.g. AWS DevOps Agent) can call it as a
regular MCP tool without knowing anything about Feishu's OAuth flow — token
resolution is entirely internal to this server.

This repo does **not** fork lark-cli. It depends on the official
`github.com/larksuite/cli` module at a pinned version (built with lark-cli's
existing `authsidecar` build tag — a feature already on lark-cli's upstream
main, not something added here) and copies one small, low-churn file
(`internal/larkendpoints/endpoints.go`, a static Feishu/Lark domain table) since
Go's `internal/` visibility rule makes the original genuinely unimportable
from an external module. See `internal/larkendpoints/check_drift.sh` for how
to detect if that copy has drifted after bumping the pinned lark-cli version.

## Prerequisites

A Feishu custom app (App ID / App Secret, redirect URL, scopes, published
version), plus the AWS-side AgentCore Identity credential provider and workload
identity. Everything on the Feishu side is console-only, and the two sides
interleave — steps and a checklist in
[`docs/FEISHU_APP_SETUP.md`](docs/FEISHU_APP_SETUP.md).

## Architecture

### Request path

```
 DevOps Agent (or any MCP client)
     │  MCP over Gateway (SigV4)
     ▼
 AgentCore Gateway
     │
     ▼
╭─ AgentCore Runtime container (this repo's image) ─────────────────────╮
│                                                                      │
│  mcp-wrapper                    0.0.0.0:8000/mcp                     │
│    · MCP protocol adapter, shells out to lark-cli per tool call       │
│         │                                                            │
│         ▼  argv                                                      │
│  lark-cli  (-tags authsidecar)                                       │
│    · sandboxed: only ever sees a sentinel credential                  │
│         │                                                            │
│         ▼  HMAC-signed HTTP over loopback                            │
│  sidecar-agentcore-identity     127.0.0.1:16385                      │
│    · the ONLY process that sees a real Feishu token                  │
│                                                                      │
╰──────────┬───────────────────────────────────────────────────────────╯
           │  GetWorkloadAccessTokenForUserId
           │  GetResourceOauth2Token
           ▼
 AgentCore Identity — Token Vault
           │  cached / refreshed user_access_token
           ▼
 open.feishu.cn
```

- **lark-cli** never holds a real token. Built with `-tags authsidecar`, it
  talks to the sidecar over a same-host HMAC-signed HTTP protocol
  (`LARKSUITE_CLI_AUTH_PROXY`/`LARKSUITE_CLI_PROXY_KEY`) and only ever sees a
  sentinel credential — even if the lark-cli process were compromised, there's
  no real token to steal from it.
- **sidecar-agentcore-identity** is the only process that ever sees the real
  Feishu access token. It resolves it from AgentCore Identity's Token Vault
  and hands it to lark-cli per-request over the sidecar protocol.
- **mcp-wrapper** is the MCP protocol adapter (Streamable HTTP,
  `initialize`/`tools/list`/`tools/call`), independent Go module, shells out to
  the `lark-cli` binary per tool call.

### Authorization path (3LO)

Two redirect hops, hosted by different parties. Conflating them is the single
most common way to misconfigure this deployment, so they are drawn separately:

```
  lark_auth_start
       │  GetResourceOauth2Token(ForceAuthentication, customState)
       ▼
  AgentCore Identity ──────── authorization_url ────────▶ operator's browser
                                                                │
                                                                │ login + consent
                                                                ▼
                                                        accounts.feishu.cn
                                                                │
   ┌──────────── hop 1: authorizationCode + state ◀─────────────┘
   │             (provider callbackUrl — AWS-hosted, registered
   │              as the redirect URL on the Feishu app)
   ▼
  AgentCore Identity
   │
   │  hop 2: redirect with session_id + state
   │  (resourceOauth2ReturnUrl — the "session binding URL", YOURS to host)
   ▼
  ┌─────────────────────────────┬──────────────────────────────────────┐
  │  WITHOUT session-binding    │  WITH session-binding endpoint       │
  │  endpoint (default)         │  (deploy/session-binding/)           │
  ├─────────────────────────────┼──────────────────────────────────────┤
  │  → http://localhost:3000    │  → https://…/<token>  (CloudFront)   │
  │    unreachable from the     │    verifies state, then a Lambda     │
  │    browser; page fails      │    behind it calls                   │
  │    to load (expected)       │    CompleteResourceTokenAuth         │
  │                             │                                      │
  │  caller passes session_id   │  binding already done; just call     │
  │  to lark_auth_complete      │  lark_exec                           │
  └─────────────────────────────┴──────────────────────────────────────┘
```

The default needs one manual step. Deploying
[`deploy/session-binding/`](deploy/session-binding/README.md) removes it —
optional, and reversible by clearing two environment variables.
`lark_auth_start` reports which mode is active in its `session_binding` field,
so an agent driving the flow is told the right next step either way.

Full design rationale and the step-by-step verification log: see
[`docs/AGENTCORE_IDENTITY_MCP_DESIGN.md`](docs/AGENTCORE_IDENTITY_MCP_DESIGN.md) and
[`docs/AGENTCORE_IDENTITY_MCP_PLAN.md`](docs/AGENTCORE_IDENTITY_MCP_PLAN.md).

## Configuration

This is a single-tenant deployment: one `userId` is bound to one Feishu
account in the Token Vault, set once via env vars on the sidecar process
(not derived per-request — no caller identity is currently available to key
it by; DevOps Agent calls this MCP server with a bare service-level SigV4
identity carrying no end-user claim. See `docs/AGENTCORE_IDENTITY_MCP_PLAN.md`
for what a multi-tenant `actor_id`-keyed version would need).

| Env var | Default | Meaning |
|---|---|---|
| `AGENTCORE_USER_ID` | `bot` | Token Vault key for this deployment's bound Feishu account |
| `AGENTCORE_REGION` | `us-east-1` | AWS region for AgentCore Identity |
| `AGENTCORE_WORKLOAD_NAME` | `feishu-mcp-workload` | Workload identity name |
| `AGENTCORE_CREDENTIAL_PROVIDER_ID` | `feishu-oauth-provider` | OAuth2 credential provider name |
| `AGENTCORE_RETURN_URL` | `http://localhost:3000/callback` | OAuth2 session binding URL — hop 2 above. Must be in the workload identity's `allowedResourceOauth2ReturnUrls`. Set to a [session-binding endpoint](deploy/session-binding/README.md) to make authorization fully automatic |
| `AGENTCORE_STATE_SECRET` | *(unset)* | HMAC key for the `customState` CSRF token. Required with `AGENTCORE_RETURN_URL`; unset means no state is sent, which the loopback listener does not need |
| `AGENTCORE_EXTRA_SCOPES` | *(unset)* | Extra Feishu scopes, comma or space separated. Set to `offline_access` to request a refresh token — see below |
| `LARKSUITE_CLI_APP_ID` | *(required)* | Feishu app ID (`cli_…`). Must be the app whose client credentials back the AgentCore Identity credential provider, or lark-cli signs as a different app than the vaulted token was issued for |
| `LARK_CLI_BIN_PATH` | `/usr/local/bin/lark-cli` | lark-cli binary the wrapper shells out to |

`LARK_CLI_BIN_PATH` is validated before every `exec`: the path must be
absolute, a regular file (not a symlink or directory), and neither it nor its
parent directory may be group- or world-writable. The binary named here
inherits the wrapper's environment — including `LARKSUITE_CLI_PROXY_KEY` — so a
path anyone else on the host could overwrite is rejected rather than executed.
A world-writable *directory* is still accepted if it has the sticky bit (`/tmp`
is `1777`), which keeps the local-dev flow of building lark-cli into `/tmp`
working.

A Token Vault slot is keyed by the **combination** of workload name, `userId`,
and the full scope list — not by the IAM identity of the caller. Two
consequences worth knowing before you debug an unexpected
`authorization required`:

- Changing `AGENTCORE_USER_ID` (or letting it fall back to a different default
  than the one authorized earlier) points the deployment at a *different*,
  empty slot. The previous authorization is still there; it is simply not being
  looked up.
- Adding, removing, or reordering scopes has the same effect: the request is
  treated as an entirely new authorization.

Because slots are IAM-independent, an operator with `bedrock-agentcore`
permissions can complete the authorization from anywhere and the deployed
container will pick up the token on its next call.

## First-time authorization (3LO)

Someone has to complete a Feishu login in a browser once. What happens *after*
that login depends on whether a session-binding endpoint is deployed.

### Default: one manual step

```
1. lark_auth_start          → authorization_url + session_id
2. open the URL, log in     → final redirect fails to load (expected)
3. lark_auth_complete       → pass the session_id from step 1
```

Step 3 needs only the session URI, which step 1 already returned — nothing has
to be read out of the browser. The redirect at the end of step 2 targets the
sidecar's loopback listener, which the browser cannot reach on Runtime; that
failure is cosmetic. Complete within ~5 minutes, since the underlying OAuth
authorization code is short-lived.

### With a session-binding endpoint

Deploy [`deploy/session-binding/`](deploy/session-binding/README.md) and step 3
disappears:

```
1. lark_auth_start          → authorization_url
2. open the URL, log in     → lands on "Feishu authorization complete"
                              (the endpoint called CompleteResourceTokenAuth)
3. lark_exec                → works immediately
```

`lark_auth_start` returns `session_binding: "automatic"` or `"manual"` so a
driving agent knows which of these it is looking at, rather than guessing.

### Driving it from an MCP client

`lark_auth_start` / `lark_auth_complete` are ordinary MCP tools, so any client
can run the flow — including a chat interface, which is the only option once
deployed to Runtime and the sidecar's loopback endpoints are unreachable.
[`skills/feishu-send-message/SKILL.md`](skills/feishu-send-message/SKILL.md) is a
worked example for AWS DevOps Agent, including the tool-loading quirk that
fuzzy search does not find these tools.

Some transports (observed with AWS DevOps Agent) replace URL-shaped strings with
`<redacted>` before the model sees them. `lark_auth_start` therefore also returns
`authorization_url_without_scheme` and the bare `request_uri`, so at least one
usable form survives; prepend `https://` to reassemble.

### Doing it locally instead

```
curl -X POST http://127.0.0.1:16385/_sidecar/auth/start
```

Loopback only — local development, or `docker exec` into the container. Here the
browser *can* reach the callback listener (`--callback-listen`, default
`127.0.0.1:3000`), so the binding completes on its own with no endpoint to
deploy.

### Why `ForceAuthentication` is always set

Both authorization paths call `GetResourceOauth2Token` with
`ForceAuthentication: true`. Omitting it reproducibly makes the subsequent
`CompleteResourceTokenAuth` fail with `AccessDeniedException: Invalid or expired
session`, regardless of retry delay. A consequence is that `lark_auth_start`
always issues a fresh authorization URL, even when a valid token is already
vaulted.

Data-plane token fetches (`ResolveToken`) call the same API **without** it, so a
cached or refreshed token is returned with no user prompt.

## Token lifetime and re-authorization

A Feishu `user_access_token` lasts 2 hours. Whether that means
re-authorizing every 2 hours depends on one scope:

| `AGENTCORE_EXTRA_SCOPES` | Behaviour |
|---|---|
| unset (default) | Feishu returns a bare access token. After 2 hours the next call fails with `authorization required` and the browser flow has to be repeated |
| `offline_access` | Feishu also returns a refresh token, which the Token Vault uses to renew silently. Authorization becomes effectively one-time |

`offline_access` is not requested by default because it needs a matching change
outside this repo: the permission must be enabled for the app in the Feishu
developer console *and* declared in the authorization scope
([Feishu docs](https://open.feishu.cn/document/authentication-management/access-token/refresh-user-access-token)).
Requesting a scope the app does not hold fails the authorization, so it is opt-in
via the env var rather than a built-in default.

Note that a Token Vault slot is keyed by the scope list, so adding
`offline_access` invalidates the existing authorization and requires one more
browser login — see the caveat under [Configuration](#configuration).

## Build

AgentCore Runtime requires a `linux/arm64` image. Either build it on CodeBuild
(no local docker daemon, and arm64 is native rather than emulated) or locally
with buildx.

### On CodeBuild

`deploy/agentcore-runtime/buildspec.yml` drives an `ARM_CONTAINER` project with
`privilegedMode` enabled. It expects `REGISTRY`, `ECR_REPOSITORY`, and
`IMAGE_TAG` as environment variables; the source is a zip in S3:

```
zip -qr /tmp/source.zip go.mod go.sum internal sidecar mcp-wrapper deploy
aws s3 cp /tmp/source.zip s3://<source-bucket>/lark-cli-mcp-source.zip
aws codebuild start-build --project-name <project> \
  --environment-variables-override name=IMAGE_TAG,value="$(git rev-parse --short HEAD)",type=PLAINTEXT
```

Prefer an immutable tag (a git SHA) over a moving one. Go builds here are not
reproducible byte-for-byte, so rebuilding the same source under the same tag
produces a *different* digest and silently repoints the tag away from whatever
was verified.

### Locally

```
docker buildx build --platform linux/arm64 --provenance=false \
  -f deploy/agentcore-runtime/Dockerfile -t <tag> .
```

`--provenance=false` matters: buildx otherwise attaches an attestation
manifest, which turns the pushed artifact into an OCI image index rather than
the single-arch image manifest AgentCore Runtime expects from `containerUri`.

`LARK_CLI_VERSION` (Dockerfile build arg) pins the official lark-cli version
installed via `go install -tags authsidecar github.com/larksuite/cli@<version>`.

## Deploy

Push the image to ECR and point an AgentCore Runtime resource at it, fronted by
an AgentCore Gateway for external MCP clients:

```
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --role-arn <runtime-role-arn> \
  --network-configuration '{"networkMode":"PUBLIC"}' \
  --protocol-configuration '{"serverProtocol":"MCP"}' \
  --agent-runtime-artifact '{"containerConfiguration":{"containerUri":"<registry>/<repo>:<tag>"}}'
```

Each update creates a new runtime version; poll `get-agent-runtime` until
`status` is `READY`. The previous version's image tag stays in ECR, so rolling
back is the same call with the older `containerUri`.

See `docs/AGENTCORE_IDENTITY_MCP_PLAN.md` for the AWS CLI commands used to
provision the OAuth2 credential provider, workload identity, and IAM roles this
depends on.

### Optional: session-binding endpoint

To remove the manual `lark_auth_complete` step, deploy the endpoint in
[`deploy/session-binding/`](deploy/session-binding/README.md). It provisions a
CloudFront-fronted Lambda that AgentCore Identity redirects the browser to, which
completes the binding itself.

```sh
cd deploy/session-binding
./deploy.sh
```

This provisions CloudFront + WAF + Lambda: CloudFront is the only public entry
point, and the Lambda's function URL is locked to that one distribution with
origin access control, so a direct request returns 403. Everything lives in the
same account as AgentCore Identity — no cross-account trust. The script prints
the `AGENTCORE_RETURN_URL` and `AGENTCORE_STATE_SECRET` to set on the runtime;
clearing both reverts to the manual flow.

## Repository layout

| Path | What it is |
|---|---|
| `mcp-wrapper/` | MCP protocol adapter (independent Go module). Exposes `lark_exec`, `lark_list_skills`, `lark_get_skill`, `lark_auth_start`, `lark_auth_complete` |
| `sidecar/` | Auth sidecar. Resolves real Feishu tokens from the Token Vault; the only process that ever holds one |
| `internal/larkendpoints/` | Static Feishu/Lark domain table copied from lark-cli, plus a drift check |
| `deploy/agentcore-runtime/` | Dockerfile, entrypoint, and CodeBuild buildspec for the `linux/arm64` runtime image |
| `deploy/session-binding/` | Optional CloudFront + Lambda endpoint that automates OAuth hop 2 |
| `skills/feishu-send-message/` | Worked example for driving the tools from AWS DevOps Agent, plus a paste-ready `PROMPT.md` |
| `docs/` | Feishu app setup (`FEISHU_APP_SETUP.md`), design rationale, and the step-by-step verification log |
