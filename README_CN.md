[English](README.md) | [中文](README_CN.md)

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

## 架构

```
DevOps Agent（或任意 MCP 客户端）
    │ MCP over Gateway (SigV4)
    ▼
AgentCore Gateway
    │
    ▼
AgentCore Runtime 容器（本仓库的镜像）
    ├── mcp-wrapper       (0.0.0.0:8000/mcp —— MCP 协议，按需 shell 调 lark-cli)
    ├── lark-cli           (-tags authsidecar —— 沙箱化，只会看到哨兵 token)
    └── sidecar-agentcore-identity (仅 loopback —— 解析真实飞书 token)
              │ GetWorkloadAccessTokenForUserId / GetResourceOauth2Token /
              │ CompleteResourceTokenAuth
              ▼
        AWS Bedrock AgentCore Identity —— Token Vault (3LO)
              │
              ▼
        open.feishu.cn / accounts.feishu.cn
```

- **lark-cli** 永远不持有真实 token。用 `-tags authsidecar` 编译后，它通过同机
  HMAC 签名的 HTTP 协议（`LARKSUITE_CLI_AUTH_PROXY`/`LARKSUITE_CLI_PROXY_KEY`）
  跟 sidecar 通信，只会看到一个哨兵凭证——即使 lark-cli 进程被攻破，里面也没有真实
  token 可偷。
- **sidecar-agentcore-identity** 是唯一见得到真实飞书 access token 的进程。它从
  AgentCore Identity 的 Token Vault 解析出 token，再按请求通过 sidecar 协议交给
  lark-cli。
- **mcp-wrapper** 是 MCP 协议适配层（Streamable HTTP，
  `initialize`/`tools/list`/`tools/call`），独立的 Go module，每次工具调用 shell 执行
  一次 `lark-cli` 二进制。

完整设计推演和逐步验证记录见
[`docs/AGENTCORE_IDENTITY_MCP_DESIGN.md`](docs/AGENTCORE_IDENTITY_MCP_DESIGN.md) 和
[`docs/AGENTCORE_IDENTITY_MCP_PLAN.md`](docs/AGENTCORE_IDENTITY_MCP_PLAN.md)。

## 配置

这是**单租户**部署：一个 `userId` 在 Token Vault 里绑定一个飞书账号，通过 sidecar 进程上的
环境变量设置一次（不是按请求推导出来的——目前拿不到调用方身份可以作为 key；DevOps Agent
调用这个 MCP server 时用的是一个纯服务级 SigV4 身份，不携带任何终端用户信息。想做成多租户
`actor_id`-keyed 版本需要什么，见 `docs/AGENTCORE_IDENTITY_MCP_PLAN.md`）。

| 环境变量 | 默认值 | 含义 |
|---|---|---|
| `AGENTCORE_USER_ID` | `bot` | 本部署所绑定飞书账号在 Token Vault 里的 key |
| `AGENTCORE_REGION` | `us-east-1` | AgentCore Identity 所在 AWS region |
| `AGENTCORE_WORKLOAD_NAME` | `feishu-mcp-workload` | Workload identity 名称 |
| `AGENTCORE_CREDENTIAL_PROVIDER_ID` | `feishu-oauth-provider` | OAuth2 credential provider 名称 |
| `LARK_CLI_BIN_PATH` | `/usr/local/bin/lark-cli` | wrapper 要 shell 调用的 lark-cli 二进制 |

`LARK_CLI_BIN_PATH` 在每次 `exec` 之前都会被校验：必须是绝对路径、必须是普通文件（不能是
符号链接或目录），且它本身和它的父目录都不能被 group 或 other 写。这里指定的二进制会继承
wrapper 的环境变量——**包括 `LARKSUITE_CLI_PROXY_KEY`**——所以一个本机其他人能覆写的路径
会被直接拒绝，而不是被执行。world-writable 的**目录**如果带 sticky bit（`/tmp` 是 `1777`）
仍然接受，这样本地开发时把 lark-cli 编译到 `/tmp` 的流程还能用。

Token Vault 的槽位是由 workload 名称、`userId` 和**完整 scope 列表**三者的**组合**决定的，
**不是**由调用方的 IAM 身份决定。排查意外出现的 `authorization required` 之前，有两个后果
值得先知道：

- 改动 `AGENTCORE_USER_ID`（或者让它回落到一个跟当初授权时不同的默认值）会让本部署指向
  **另一个空槽位**。之前的授权还在，只是根本没被查到。
- 增加、删除或调整 scope 顺序会产生同样的效果：这次请求会被当成一次全新的授权。

因为槽位与 IAM 无关，任何拥有 `bedrock-agentcore` 权限的操作者都可以从任何地方完成授权，
部署好的容器在下一次调用时就能拿到 token。

## 首次授权（3LO）

授权是一个手工步骤，有两种做法。

**通过 MCP 工具**（任何 MCP 客户端都可以，包括聊天界面——部署到 AgentCore Runtime 之后
sidecar 的 loopback 端点从外面够不着，这是唯一可用的方式）：

1. 调用 `lark_auth_start`（不需要参数）。它会返回一个 `authorization_url` 和一个
   `session_id`。
2. 在浏览器里打开这个 URL，完成飞书登录。
3. 用拿到的 `session_id` 调用 `lark_auth_complete`。

第 3 步只需要 session URI，所以浏览器**不需要**真的访问得到配置里那个 return URL——
跳转过去加载失败是正常的，`session_id` 这个 query 参数也可以直接从地址栏里复制出来。
请在 5 分钟左右内完成：底层的 OAuth authorization code 生命周期很短。

对会过滤工具输出的客户端有一点提示：某些传输层（在 AWS DevOps Agent 上观察到）会在模型
看到之前把长得像 URL 的字符串替换成 `<redacted>`。所以 `lark_auth_start` 还会额外返回
`authorization_url_without_scheme` 和裸的 `request_uri`，保证至少有一种形式能活下来；
自己拼上 `https://` 就能还原。

**直接通过 sidecar**（仅 loopback——本地开发，或者 `docker exec` 进容器）：

```
curl -X POST http://127.0.0.1:16385/_sidecar/auth/start
```

这条路径下，浏览器会被自动重定向到 sidecar 专门的 callback listener
（`--callback-listen`，默认 `127.0.0.1:3000`），它会自动完成绑定，不需要复制粘贴。

两条路径都会带 `ForceAuthentication: true` 调 `GetResourceOauth2Token`；不带这个参数会
稳定复现后续 `CompleteResourceTokenAuth` 失败并报
`AccessDeniedException: Invalid or expired session`，重试多久都一样。这么做的一个副作用
是：即使 vault 里已经有一个有效 token，`lark_auth_start` 仍然会签发一个新的授权 URL。

后续的 token 获取（sidecar 的数据面路径 `ResolveToken`）调用同一个 API 但**不带**
`ForceAuthentication`，所以会直接返回已缓存/已刷新的 token，不会再次要求用户授权。

## 构建

AgentCore Runtime 要求 `linux/arm64` 镜像。可以用 CodeBuild 构建（不需要本地 docker
daemon，而且 arm64 是原生编译而非模拟），也可以本地用 buildx。

### 用 CodeBuild

`deploy/agentcore-runtime/buildspec.yml` 驱动一个开了 `privilegedMode` 的
`ARM_CONTAINER` project。它需要 `REGISTRY`、`ECR_REPOSITORY`、`IMAGE_TAG` 三个环境
变量；源码是 S3 上的一个 zip：

```
zip -qr /tmp/source.zip go.mod go.sum internal sidecar mcp-wrapper deploy
aws s3 cp /tmp/source.zip s3://<source-bucket>/lark-cli-mcp-source.zip
aws codebuild start-build --project-name <project> \
  --environment-variables-override name=IMAGE_TAG,value="$(git rev-parse --short HEAD)",type=PLAINTEXT
```

优先用不可变 tag（git SHA），别用会移动的 tag。这里的 Go 构建不是 byte-for-byte 可复现的，
所以同一份源码用同一个 tag 重新构建会产生**不同的 digest**，于是这个 tag 会悄悄指向一个
你没验证过的镜像。

### 本地构建

```
docker buildx build --platform linux/arm64 --provenance=false \
  -f deploy/agentcore-runtime/Dockerfile -t <tag> .
```

`--provenance=false` 很重要：不加的话 buildx 会附加一个 attestation manifest，推上去的
产物就变成 OCI image index，而不是 AgentCore Runtime 解析 `containerUri` 时期望的单架构
image manifest。

`LARK_CLI_VERSION`（Dockerfile build arg）固定了通过
`go install -tags authsidecar github.com/larksuite/cli@<version>` 安装的官方 lark-cli
版本。

## 部署

把镜像推到 ECR，让一个 AgentCore Runtime 资源指向它，前面再挂一个 AgentCore Gateway 给
外部 MCP 客户端用：

```
aws bedrock-agentcore-control update-agent-runtime \
  --agent-runtime-id <runtime-id> \
  --role-arn <runtime-role-arn> \
  --network-configuration '{"networkMode":"PUBLIC"}' \
  --protocol-configuration '{"serverProtocol":"MCP"}' \
  --agent-runtime-artifact '{"containerConfiguration":{"containerUri":"<registry>/<repo>:<tag>"}}'
```

每次 update 都会产生一个新的 runtime version；轮询 `get-agent-runtime` 直到 `status`
变成 `READY`。上一个 version 用的镜像 tag 还在 ECR 里，所以回滚就是拿旧的 `containerUri`
再调一次同样的命令。

本方案依赖的 OAuth2 credential provider、workload identity 和 IAM 角色，具体用哪些
AWS CLI 命令创建，见 `docs/AGENTCORE_IDENTITY_MCP_PLAN.md`。
