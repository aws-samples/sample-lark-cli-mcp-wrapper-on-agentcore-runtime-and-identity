[中文](#中文) | [English](#english)

---

<a name="中文"></a>

# 部署到 AWS 中国区

这套东西在 AWS 中国区（`cn-north-1` 北京 / `cn-northwest-1` 宁夏）能完整跑起来，**源码一行
不用改**。本文记录跑通的过程、和全球区的差异，以及几个只在中国区才会撞上的坑。

已实测：`cn-northwest-1`，bot 身份（M2M）和 user 身份（3LO）两条 OAuth2 flow 都走通。
`cn-north-1` 只验证了控制面 API 有响应，没做端到端。

## 为什么代码不用改

三件事凑在一起，让分区差异自己消失了：

1. **sidecar 调 AgentCore Identity 用的是「名字」，不是 ARN** —— workload identity 名、
   credential provider 名。所以 `arn:aws` 和 `arn:aws-cn` 的区别根本没进代码。
2. **region 走 `AGENTCORE_REGION` 环境变量**（见 `sidecar/agentcore_resolver.go`），
   AWS SDK for Go v2 自己把它解析成 `bedrock-agentcore.<region>.amazonaws.com.cn`。
3. **飞书域名本来就是 `feishu.cn` 那一套**（`internal/larkendpoints/`）。中国区在这一点上
   反而比全球区顺——Token Vault 做 token 交换时要出网打 `open.feishu.cn`，从中国区打过去是
   本地流量。

所有要变的东西都在环境变量和 AWS 资源定义里，没有一处在 Go 代码里。

## 和全球区的差异

| | 全球区 | 中国区 |
|---|---|---|
| ARN 分区 | `arn:aws:` | `arn:aws-cn:` |
| 服务端点后缀 | `amazonaws.com` | `amazonaws.com.cn` |
| ECR registry | `<ACCOUNT_ID>.dkr.ecr.<region>.amazonaws.com` | `<ACCOUNT_ID>.dkr.ecr.<region>.amazonaws.com.cn` |
| IAM trust 的 service principal | `bedrock-agentcore.amazonaws.com` | **同样是** `bedrock-agentcore.amazonaws.com`（中国区接受全球写法，别改成 `.com.cn`） |
| runtime 环境变量 | `AGENTCORE_REGION` 可省（默认 `us-east-1`） | **必须显式设** `AGENTCORE_REGION=<cn-region>` |

`AGENTCORE_REGION` 那行是唯一一个"不设就错但不报错"的地方：不设的话 sidecar 会拿默认的
`us-east-1` 去找 Token Vault，而中国区凭证在全球区端点上无效，最后表现成 token 取不到。

## 部署步骤

下面假设目标区是 `cn-northwest-1`。凡是 `<...>` 都要换成你自己的值。

### 1. 把镜像搬到中国区 ECR

**不用重新构建。** 镜像是 `linux/arm64`，跨分区 pull / retag / push 就行，digest 完全一致：

```bash
aws ecr create-repository --repository-name lark-cli-mcp --region cn-northwest-1

# 登录两个 registry（两个不同分区的凭证）
aws ecr get-login-password --region us-east-1 --profile <global-profile> \
  | docker login --username AWS --password-stdin \
    <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com
aws ecr get-login-password --region cn-northwest-1 --profile <cn-profile> \
  | docker login --username AWS --password-stdin \
    <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn

docker pull --platform linux/arm64 \
  <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com/lark-cli-mcp:<tag>
docker tag \
  <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com/lark-cli-mcp:<tag> \
  <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>
docker push <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>
```

从源码构建也可以，`deploy/agentcore-runtime/buildspec.yml` 在中国区 CodeBuild 的
ARM_CONTAINER 上一样能用。只是从零构建要拉 Go module 和 Debian 基础镜像，跨境网络那一段
不如直接搬镜像省事。

### 2. Runtime 的 IAM role

和全球区同构，把所有 ARN 换成 `arn:aws-cn:`。trust policy：

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "bedrock-agentcore.amazonaws.com" },
    "Action": "sts:AssumeRole",
    "Condition": { "StringEquals": { "aws:SourceAccount": "<CN_ACCOUNT_ID>" } }
  }]
}
```

权限 policy 的 Resource 换分区即可，语句结构不变：AgentCore Identity 四个 action、
ECR pull、CloudWatch Logs、X-Ray、以及 credential provider 那个托管 secret 的
`secretsmanager:GetSecretValue`。完整清单见 README 的部署一节。

### 3. Credential provider 和 workload identity

```bash
aws bedrock-agentcore-control create-oauth2-credential-provider \
  --name feishu-oauth-provider \
  --credential-provider-vendor CustomOauth2 \
  --oauth2-provider-config-input file://provider.json \
  --region cn-northwest-1
```

`provider.json` 里的 `oauthDiscovery` 和全球区**完全一样**（`accounts.feishu.cn` /
`open.feishu.cn`），只有 `clientId` / `clientSecret` 是你自己应用的。

返回的 `callbackUrl` 是中国区形式：

```
https://bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/identities/oauth2/callback/<provider-uuid>
```

**这串要填进飞书开放平台的重定向 URL**（见 [`FEISHU_APP_SETUP.md`](FEISHU_APP_SETUP.md)）。
注意它绑的是 provider 的 UUID，不是 clientId——所以后面换应用凭证时这串不变，不用回控制台
重新加；但改 provider **名字**会生成新的 UUID，就得重新加了。

bot 身份（M2M / client credentials）不经过重定向，所以只想跑 bot 那条路的话，这一步的控制台
配置可以跳过。

workload identity 照常建，`allowedResourceOauth2ReturnUrls` 的取值规则和全球区一致。

### 4. 建 Runtime

```bash
aws bedrock-agentcore-control create-agent-runtime \
  --agent-runtime-name lark_cli_mcp_cn \
  --agent-runtime-artifact '{"containerConfiguration":{"containerUri":"<CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>"}}' \
  --role-arn arn:aws-cn:iam::<CN_ACCOUNT_ID>:role/<runtime-role> \
  --network-configuration '{"networkMode":"PUBLIC"}' \
  --protocol-configuration '{"serverProtocol":"MCP"}' \
  --environment-variables 'LARKSUITE_CLI_APP_ID=cli_xxxxxxxxxxxxxxxx,AGENTCORE_REGION=cn-northwest-1,AGENTCORE_CREDENTIAL_PROVIDER_ID=feishu-oauth-provider' \
  --region cn-northwest-1
```

### 5.（可选）前面挂一个 Gateway

Gateway 的 target 用 `mcp.mcpServer`，endpoint 指向 runtime 的 invocation URL——URL 里那个
ARN 要整体 percent-encode，且分区是 `aws-cn`：

```json
{
  "mcp": {
    "mcpServer": {
      "endpoint": "https://bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/runtimes/arn%3Aaws-cn%3Abedrock-agentcore%3Acn-northwest-1%3A<CN_ACCOUNT_ID>%3Aruntime%2F<runtime-id>/invocations?qualifier=DEFAULT"
    }
  }
}
```

凭证配置用 `GATEWAY_IAM_ROLE`，里面的 `iamCredentialProvider.region` 也要写中国区。
Gateway 的 role 只需要对目标 runtime 的 `bedrock-agentcore:InvokeAgentRuntime`。

### 6. 客户端接入

Gateway URL 形如：

```
https://<gateway-id>.gateway.bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/mcp
```

用 `mcp-proxy-for-aws` 接的话，**必须显式传 `--service bedrock-agentcore`**（原因见下面的坑
3）：

```json
{
  "mcpServers": {
    "lark-cli-mcp-agentcore-cn": {
      "type": "stdio",
      "command": "uvx",
      "args": [
        "mcp-proxy-for-aws@latest",
        "https://<gateway-id>.gateway.bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/mcp",
        "--service", "bedrock-agentcore",
        "--region", "cn-northwest-1",
        "--profile", "<cn-profile>"
      ],
      "env": {
        "no_proxy": ".amazonaws.com.cn",
        "NO_PROXY": ".amazonaws.com.cn"
      }
    }
  }
}
```

## 四个坑

这几个都表现成别的样子，排查时容易跑偏。

### 1. `InvalidClientTokenId` —— 其实是 region 被环境变量顶掉了

```
An error occurred (InvalidClientTokenId) when calling the GetCallerIdentity operation:
The security token included in the request is invalid.
```

凭证没问题。环境变量 `AWS_REGION` / `AWS_DEFAULT_REGION` 的优先级**高于** `~/.aws/config` 里
profile 的 `region`。如果 shell 里留着一个全球区的 `AWS_REGION`，CLI 会把请求打到全球分区端点
（IAM 这种"全球服务"最明显，会去 `iam.amazonaws.com`），中国区的 access key 在那边当然无效。

判别：`aws configure list` 看 `region` 那行的 TYPE 是不是 `env`。

解法：每条命令显式 `--region <cn-region>`，或在子 shell 里覆盖掉那两个环境变量。

### 2. `docker login` 报 EOF —— proxy 的 NO_PROXY 后缀匹配不到 `.com.cn`

```
Error response from daemon: Get "https://<...>.amazonaws.com.cn/v2/": EOF
```

很多开发环境（Colima、Docker Desktop、公司代理）的 `NO_PROXY` 里写的是 `.amazonaws.com`。
`xxx.amazonaws.com.cn` 结尾是 `.com.cn`，**后缀匹配不上**，于是走了代理，而代理往往到不了
中国区。

解法：docker daemon 和 shell 两侧的 `NO_PROXY` / `no_proxy` 都补上 `.amazonaws.com.cn`。
Colima 的话这个值在 VM 内 `/etc/systemd/system/docker.service.d/override.conf`，
改完 `systemctl daemon-reload && systemctl restart docker`；注意这文件由 Colima 启动时生成，
`colima start` 可能重置，重启后要重打。

### 3. `mcp-proxy-for-aws` 401 —— 上游没适配中国分区

`mcp-proxy-for-aws`（实测 1.7.0）从端点 URL 推断 SigV4 签名用的 service 名，
`utils.py` 里的模式是：

```python
case [*_, 'bedrock-agentcore', region, 'amazonaws', 'com']:
    return 'bedrock-agentcore', region
```

`...amazonaws.com.cn` 多出一段 `cn`，匹配不上，落到 fallback 分支「取 hostname 第一段当
service 名」——于是拿 gateway ID 去签名，网关回 `401 Unauthorized`。

解法：显式传 `--service bedrock-agentcore`。region 只要也显式传就不会踩到同一个函数的
region 推断。

### 4. `{"message":"Invalid request"}` —— PAR request_uri 是一次性且短命的

`lark_auth_start` 返回的 authorization URL 里带一个 OAuth 2.0 Pushed Authorization Request
的 `request_uri`。它**只能用一次**，而且有效期很短。用 curl 探一下、或者放一会儿再点，
再打开就是：

```json
{"fieldList":null,"message":"Invalid request","reason":null}
```

这不是配置错。重新调一次 `lark_auth_start` 拿新的，拿到就立刻用。

顺带一个相邻的坑：如果飞书报的是 **错误码 20029 `Invalid redirect URL`**，那才是配置问题——
说明发出去的 `redirect_uri` 和开放平台里注册的不一致。最常见的原因是 callback 加到了**另一个
应用**上。核对时把浏览器地址栏里的 `client_id` 和 runtime 的 `LARKSUITE_CLI_APP_ID` 对一遍。

## 还没覆盖的

`deploy/session-binding/`（那个把 3LO 变成一步的 Lambda + CloudFront）**没有**在中国区部署。
中国区 CloudFront 要求自定义域名已备案，所以这一套需要改造——换成 ALB，或者直接用 API Gateway
自带的域名。

不部署它也能用，只是 3LO 要手工两步（`lark_auth_start` → 浏览器登录 → `lark_auth_complete`），
详见 README 的「首次授权」一节。bot 身份完全不受影响。

---

<a name="english"></a>

# Deploying to AWS China regions

This stack runs end to end in the AWS China partition (`cn-north-1` Beijing /
`cn-northwest-1` Ningxia) with **no source changes at all**. This page records what it took,
how the China partition differs, and the handful of traps that only show up there.

Verified on `cn-northwest-1`, with both OAuth2 flows working: bot identity (M2M) and user
identity (3LO). On `cn-north-1` only the control-plane APIs were confirmed to respond; no
end-to-end run.

## Why no code changes are needed

Three properties combine to make the partition difference disappear on its own:

1. **The sidecar addresses AgentCore Identity by *name*, not by ARN** — workload identity
   name, credential provider name. So the `arn:aws` vs `arn:aws-cn` split never reaches the
   code.
2. **The region comes from the `AGENTCORE_REGION` environment variable**
   (see `sidecar/agentcore_resolver.go`), and the AWS SDK for Go v2 resolves that into
   `bedrock-agentcore.<region>.amazonaws.com.cn` by itself.
3. **The Feishu endpoints are the `feishu.cn` set to begin with**
   (`internal/larkendpoints/`). China is arguably the easier side here: the Token Vault has to
   reach `open.feishu.cn` to do the token exchange, and from a China region that is local
   traffic.

Everything that changes lives in environment variables and AWS resource definitions. Nothing
lives in the Go code.

## What differs from a global region

| | Global | China |
|---|---|---|
| ARN partition | `arn:aws:` | `arn:aws-cn:` |
| Service endpoint suffix | `amazonaws.com` | `amazonaws.com.cn` |
| ECR registry | `<ACCOUNT_ID>.dkr.ecr.<region>.amazonaws.com` | `<ACCOUNT_ID>.dkr.ecr.<region>.amazonaws.com.cn` |
| IAM trust service principal | `bedrock-agentcore.amazonaws.com` | **also** `bedrock-agentcore.amazonaws.com` (China accepts the global form — do not change it to `.com.cn`) |
| Runtime env var | `AGENTCORE_REGION` optional (defaults to `us-east-1`) | `AGENTCORE_REGION=<cn-region>` **required** |

That last row is the one place where omitting something fails without telling you: left unset,
the sidecar goes looking for a Token Vault in `us-east-1`, China credentials are invalid
against a global endpoint, and the symptom surfaces as "cannot resolve a token".

## Deployment

Examples target `cn-northwest-1`. Replace everything in `<...>`.

### 1. Move the image into a China ECR repository

**No rebuild required.** The image is `linux/arm64`; pull / retag / push across partitions and
the digest is identical:

```bash
aws ecr create-repository --repository-name lark-cli-mcp --region cn-northwest-1

# Log in to both registries (credentials from two different partitions)
aws ecr get-login-password --region us-east-1 --profile <global-profile> \
  | docker login --username AWS --password-stdin \
    <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com
aws ecr get-login-password --region cn-northwest-1 --profile <cn-profile> \
  | docker login --username AWS --password-stdin \
    <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn

docker pull --platform linux/arm64 \
  <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com/lark-cli-mcp:<tag>
docker tag \
  <GLOBAL_ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com/lark-cli-mcp:<tag> \
  <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>
docker push <CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>
```

Building from source works too — `deploy/agentcore-runtime/buildspec.yml` runs unchanged on an
ARM_CONTAINER CodeBuild project in China. It just means pulling Go modules and a Debian base
image across the border, which the image copy avoids entirely.

### 2. Runtime IAM role

Structurally identical to the global one; swap every ARN to `arn:aws-cn:`. Trust policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "bedrock-agentcore.amazonaws.com" },
    "Action": "sts:AssumeRole",
    "Condition": { "StringEquals": { "aws:SourceAccount": "<CN_ACCOUNT_ID>" } }
  }]
}
```

The permissions policy needs the same statements with repartitioned Resources: the four
AgentCore Identity actions, ECR pull, CloudWatch Logs, X-Ray, and
`secretsmanager:GetSecretValue` on the credential provider's managed secret. The full list is
in the README's deployment section.

### 3. Credential provider and workload identity

```bash
aws bedrock-agentcore-control create-oauth2-credential-provider \
  --name feishu-oauth-provider \
  --credential-provider-vendor CustomOauth2 \
  --oauth2-provider-config-input file://provider.json \
  --region cn-northwest-1
```

The `oauthDiscovery` block in `provider.json` is **byte-for-byte the same** as the global one
(`accounts.feishu.cn` / `open.feishu.cn`); only `clientId` / `clientSecret` are yours.

The returned `callbackUrl` takes the China form:

```
https://bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/identities/oauth2/callback/<provider-uuid>
```

**Register that string as a redirect URL on the Feishu app** (see
[`FEISHU_APP_SETUP.md`](FEISHU_APP_SETUP.md)). Note it is keyed to the provider's UUID, not to
the clientId — so swapping the app credentials later leaves it unchanged and needs no console
visit, whereas renaming the provider mints a new UUID and does.

Bot identity (M2M / client credentials) never goes through a redirect, so if you only care
about that path you can skip this console step.

Create the workload identity as usual; the rules for `allowedResourceOauth2ReturnUrls` are the
same as in a global region.

### 4. Create the Runtime

```bash
aws bedrock-agentcore-control create-agent-runtime \
  --agent-runtime-name lark_cli_mcp_cn \
  --agent-runtime-artifact '{"containerConfiguration":{"containerUri":"<CN_ACCOUNT_ID>.dkr.ecr.cn-northwest-1.amazonaws.com.cn/lark-cli-mcp:<tag>"}}' \
  --role-arn arn:aws-cn:iam::<CN_ACCOUNT_ID>:role/<runtime-role> \
  --network-configuration '{"networkMode":"PUBLIC"}' \
  --protocol-configuration '{"serverProtocol":"MCP"}' \
  --environment-variables 'LARKSUITE_CLI_APP_ID=cli_xxxxxxxxxxxxxxxx,AGENTCORE_REGION=cn-northwest-1,AGENTCORE_CREDENTIAL_PROVIDER_ID=feishu-oauth-provider' \
  --region cn-northwest-1
```

### 5. (Optional) Front it with a Gateway

The Gateway target is an `mcp.mcpServer` whose endpoint is the runtime invocation URL. The ARN
inside that URL is percent-encoded as a whole, and its partition is `aws-cn`:

```json
{
  "mcp": {
    "mcpServer": {
      "endpoint": "https://bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/runtimes/arn%3Aaws-cn%3Abedrock-agentcore%3Acn-northwest-1%3A<CN_ACCOUNT_ID>%3Aruntime%2F<runtime-id>/invocations?qualifier=DEFAULT"
    }
  }
}
```

Use `GATEWAY_IAM_ROLE` for credentials, with `iamCredentialProvider.region` set to the China
region as well. The Gateway's role only needs `bedrock-agentcore:InvokeAgentRuntime` on the
target runtime.

### 6. Client wiring

The Gateway URL looks like:

```
https://<gateway-id>.gateway.bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/mcp
```

If you connect through `mcp-proxy-for-aws`, you **must** pass `--service bedrock-agentcore`
explicitly (trap 3 below explains why):

```json
{
  "mcpServers": {
    "lark-cli-mcp-agentcore-cn": {
      "type": "stdio",
      "command": "uvx",
      "args": [
        "mcp-proxy-for-aws@latest",
        "https://<gateway-id>.gateway.bedrock-agentcore.cn-northwest-1.amazonaws.com.cn/mcp",
        "--service", "bedrock-agentcore",
        "--region", "cn-northwest-1",
        "--profile", "<cn-profile>"
      ],
      "env": {
        "no_proxy": ".amazonaws.com.cn",
        "NO_PROXY": ".amazonaws.com.cn"
      }
    }
  }
}
```

## Four traps

Each of these presents as something other than what it is, which is what makes them expensive.

### 1. `InvalidClientTokenId` — really an environment variable overriding your region

```
An error occurred (InvalidClientTokenId) when calling the GetCallerIdentity operation:
The security token included in the request is invalid.
```

The credentials are fine. `AWS_REGION` / `AWS_DEFAULT_REGION` in the environment take
**precedence over** the profile's `region` in `~/.aws/config`. Leave a global-region
`AWS_REGION` in your shell and the CLI sends requests to global-partition endpoints — most
visibly for "global" services like IAM, which go to `iam.amazonaws.com` — where a China access
key is, of course, invalid.

Diagnostic: run `aws configure list` and check whether the TYPE column for `region` says `env`.

Fix: pass `--region <cn-region>` explicitly on every call, or override both variables in a
subshell.

### 2. `docker login` returns EOF — `NO_PROXY` suffix matching misses `.com.cn`

```
Error response from daemon: Get "https://<...>.amazonaws.com.cn/v2/": EOF
```

Many development setups (Colima, Docker Desktop, corporate proxies) list `.amazonaws.com` in
`NO_PROXY`. A host like `xxx.amazonaws.com.cn` ends in `.com.cn`, so **the suffix does not
match**, the request goes through the proxy, and the proxy typically cannot reach China.

Fix: add `.amazonaws.com.cn` to `NO_PROXY` / `no_proxy` for both the docker daemon and your
shell. On Colima the daemon's copy lives inside the VM at
`/etc/systemd/system/docker.service.d/override.conf`; after editing run
`systemctl daemon-reload && systemctl restart docker`. Note Colima generates that file at
startup, so `colima start` may reset it and you will have to reapply.

### 3. `mcp-proxy-for-aws` returns 401 — upstream has no China-partition support

`mcp-proxy-for-aws` (observed on 1.7.0) infers the SigV4 signing service name from the
endpoint URL. The pattern in `utils.py` is:

```python
case [*_, 'bedrock-agentcore', region, 'amazonaws', 'com']:
    return 'bedrock-agentcore', region
```

`...amazonaws.com.cn` has an extra `cn` segment, so it does not match and falls through to the
"first hostname segment is the service name" branch — signing with the gateway ID, which the
gateway answers with `401 Unauthorized`.

Fix: pass `--service bedrock-agentcore` explicitly. Passing `--region` explicitly as well
keeps you clear of the same function's region inference.

### 4. `{"message":"Invalid request"}` — the PAR request_uri is single-use and short-lived

The authorization URL returned by `lark_auth_start` carries an OAuth 2.0 Pushed Authorization
Request `request_uri`. It is good for **one** use and expires quickly. Probe it with curl, or
sit on it for a few minutes, and the next open gives you:

```json
{"fieldList":null,"message":"Invalid request","reason":null}
```

This is not a misconfiguration. Call `lark_auth_start` again for a fresh one and use it
immediately.

One adjacent trap worth separating out: if Feishu reports **error code 20029
`Invalid redirect URL`**, that *is* a configuration problem — the `redirect_uri` being sent
does not match what is registered on the open platform. The most common cause is the callback
having been added to a **different app**. Compare the `client_id` in the browser's address bar
against the runtime's `LARKSUITE_CLI_APP_ID`.

## Not covered

`deploy/session-binding/` — the Lambda + CloudFront pair that collapses 3LO into a single step
— has **not** been deployed in a China region. CloudFront there requires a custom domain with a
completed ICP filing, so that piece needs reworking: an ALB, or API Gateway's own domain.

You do not need it. Without it, 3LO is the manual two-step flow (`lark_auth_start` → browser
login → `lark_auth_complete`) described in the README's first-authorization section. Bot
identity is unaffected either way.
