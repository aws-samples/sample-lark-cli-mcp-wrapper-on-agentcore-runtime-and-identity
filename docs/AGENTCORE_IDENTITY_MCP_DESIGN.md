# 飞书 lark-cli 接入 AWS AgentCore Runtime 设计文档

## 背景

目标：让 AWS DevOps Agent（团队内部一个基于 AWS Bedrock AgentCore 的 agent，agent_space_id `<AGENT_SPACE_ID>`，账号 <AWS_ACCOUNT_ID>，us-east-1）能调用飞书（Feishu/Lark）能力，作为 MCP 工具挂载。

### 为什么不用官方 `lark-openapi-mcp`

`larksuite/lark-openapi-mcp` 是飞书官方发布的 MCP server，本身就讲 MCP 协议、有 Streamable HTTP transport，理论上是最直接的路径。但：

- 该仓库最后一次提交是 2025-08-14，已停更近一年。
- 覆盖能力有限（约 21 个工具，preset 机制），飞书真正维护的高覆盖工具（200+ 命令，26 个 Skill）都在另一个活跃仓库里。

### 为什么改用 `larksuite/cli`

`larksuite/cli`（npm 包 `@larksuite/cli`，命令 `lark-cli`）是飞书官方持续活跃维护的 Go CLI 工具（撰写本文档当天仍有多个 PR 合并），覆盖 18 个业务域、200+ 命令、26 个 AI Agent Skill，专为 AI Agent 场景设计（结构化错误、简洁参数、smart defaults）。

但它是**命令行工具**，不是 MCP server，不能被 DevOps Agent 直接连接（DevOps Agent 只认 Streamable HTTP 协议的 MCP server，见其官方文档 "Connecting MCP Servers" 的 Requirements）。所以中间必须有一层 MCP Wrapper 做协议翻译。

### 为什么要接 AgentCore Identity

`lark-cli` 的默认认证方式是 OAuth Device Flow + 本机 OS Keychain 存储 token（`internal/auth/token_store.go` 走 `internal/keychain`）。这套在**开发者自己电脑**上没问题，但要塞进 AWS AgentCore Runtime（无状态、容器化、随时可能被回收重建）里跑，会遇到跟之前调研 `lark-openapi-mcp`/keytar 时一样的问题：容器没有持久 Keychain，一重启 token 就丢，得重新走一遍浏览器授权。

之前已经用 boto3 手工验证过完整链路：AgentCore Identity 的 Token Vault + 自定义 OAuth2CredentialProvider（`authorizationServerMetadata` 手填飞书端点，因为飞书没有标准 `.well-known/openid-configuration`）可以托管飞书的 3LO 授权，首次授权后 token 落在 AWS 托管的 Token Vault 里，不依赖容器本地存储，重启不丢，理论上能自动 refresh（这点还没做长时间验证，见「未决问题」）。详细验证记录见 basic-memory 笔记《飞书MCP接入AgentCore Identity验证记录》。

### 关键发现：`lark-cli` 已经预留了对应的扩展点

`lark-cli` 源码里有一套官方设计好的「sidecar 凭证模式」（`extension/credential/sidecar/`，编译时用 `authsidecar` build tag 激活），专门解决"CLI 跑在不可信沙箱环境、真实凭证要放在可信边界外"的问题：

- CLI 进程本身**永远不持有真实 token**，只有哨兵值（`sidecar-managed-uat` / `sidecar-managed-tat`）。
- 请求发出前，`extension/transport/sidecar/interceptor.go` 把哨兵值替换成真实凭证注入点，实际由一个**同机运行的独立 sidecar 进程**完成真正的 HMAC 签名校验和 token 注入转发。
- 官方给了两个参考实现：`sidecar/server-demo/`（单租户，凭证来自本机 keychain）和 `sidecar/server-multi-tenant-demo/`（多租户，含 Device Flow 登录管理端点、client→user 映射）。两者 README 都明确写着"这是 demo，生产环境请自己实现，可以从密钥管理服务取真实凭证"。

这正是我们要的切入点：**不改 `lark-cli` 一行代码**，自己写一个新的 sidecar server，把 demo 里"读本机 keychain"的部分换成"调用 AgentCore Identity API"，两边协议完全不用动。

## 整体架构

三个进程跑在同一个 AgentCore Runtime 容器里：

```
┌─────────────────────────────────────────────────────────────┐
│                  AgentCore Runtime 容器                       │
│                                                               │
│  ┌──────────────┐     ┌──────────────────┐                  │
│  │  MCP Wrapper  │────▶│    lark-cli       │                  │
│  │ (对外 MCP/    │ 子  │ (authsidecar tag  │                  │
│  │  Streamable   │进程 │  编译, 无真实token)│                  │
│  │  HTTP, 给     │     └─────────┬─────────┘                  │
│  │  DevOps Agent │               │ HMAC 签名请求               │
│  │  连接)        │               ▼ (env: LARKSUITE_CLI_       │
│  └──────────────┘        ┌───────────────────┐  AUTH_PROXY)  │
│                          │  Sidecar Server    │               │
│                          │ (仿 server-demo,    │               │
│                          │  凭证源=AgentCore   │               │
│                          │  Identity 而非      │               │
│                          │  本机keychain)      │               │
│                          └─────────┬───────────┘               │
└────────────────────────────────────┼─────────────────────────┘
                                     │ boto3/AWS SDK
                                     ▼
                    AWS Bedrock AgentCore Identity
                    (Token Vault, 已验证通)
                                     │
                                     ▼
                            飞书 OpenAPI (open.feishu.cn)
```

### 组件职责

1. **MCP Wrapper**（新代码）：讲 MCP 协议（Streamable HTTP transport）对外，给 DevOps Agent 连接。收到工具调用请求后，内部 shell 执行 `lark-cli` 命令，把 stdout/stderr 包装成 MCP 工具结果格式返回。**与认证完全无关**，不知道 token 从哪来。
2. **lark-cli**（fork 自官方，不改代码，只是用特定 build tag 编译）：实际执行飞书业务操作的二进制。编译时带 `authsidecar` tag，运行时设置 `LARKSUITE_CLI_AUTH_PROXY` 等环境变量指向 sidecar，本身不持有任何真实凭证。
3. **Sidecar Server**（新代码，仿官方 `server-demo` 改）：负责认证。验证 lark-cli 发来的 HMAC 签名请求后，向 AgentCore Identity 换取真实飞书 access token，注入到转发给飞书 OpenAPI 的请求里。**与 MCP/DevOps Agent 完全无关**，不知道请求是被谁触发的。

三者解耦，各自单一职责，符合"能不能脱离读内部实现就理解它是干什么的"这条设计准则。

### 代码组织

- `~/Documents/git/lark-cli`（fork 自 `larksuite/cli` → `panlm/cli`）：sidecar server 新代码放在这个仓库下的新目录（比如 `sidecar/server-agentcore-identity/`），跟官方 `server-demo`/`server-multi-tenant-demo` 平行，方便以后 `git fetch upstream` 同步官方更新不冲突。
- MCP Wrapper：独立新目录/新仓库（待定，不影响本设计的架构正确性，纯粹是代码摆放位置的选择，可以在实施计划阶段再定）。

## 认证设计细节

### userId / 多用户支持

当前只有一个人（本人）会用这套工具链，通过 `~/.mcp.json` 里现有的 `aws-devops-agent` 和 `lark-mcp` 配置也能看出，两处都是单人固定配置，没有为多用户设计过任何身份识别机制。

因此：**AgentCore Identity 的 userId 用一个硬编码的固定字符串**（代表本人），不构建多用户隔离、不做 client→user 映射（即不需要官方 `server-multi-tenant-demo` 里那套 `client_id → feishuOpenId` 映射逻辑，那是给"多个不同的人共用一套 sidecar 基础设施"场景设计的，我们不需要）。

以后如果真的需要给别人也接上，再参考 multi-tenant demo 的映射设计加，不在本次范围内。

### 首次 3LO 授权流程

AgentCore Runtime 容器是无头环境，没有浏览器。设计为：sidecar server 在需要首次授权时（Token Vault 里还没有对应 userId 的 token），调用 `GetResourceOauth2Token` 拿到 `authorizationUrl`，通过某种方式（暴露一个管理端点，或者写进容器日志）让这个 URL 能被人看到；本人在自己电脑浏览器打开这个 URL，完成飞书登录同意；之后飞书会跳转到一个 `resourceOauth2ReturnUrl`，地址栏带 `session_id` 参数（之前手工验证时确认过，即使这个回调地址本身连不上也没关系，只需要读出 URL 里的参数）；需要把这个 `session_id` 传回 sidecar，让它调用 `CompleteResourceTokenAuth` 完成 session binding，之后 Token Vault 里就有真实 token 了，后续调用直接从缓存拿。

**这一步"session_id 怎么传回 sidecar"的具体交互方式（新增一个 MCP 工具 vs 本地小脚本 vs 其它方式）留给实施计划阶段设计**，本文档只确定这个环节存在、且发生在 sidecar 层，不需要 MCP Wrapper 或 lark-cli 参与。

### 已有的 AWS 资源（复用，不重建）

之前手工验证时已经建好，直接复用：

- AgentCore Identity OAuth2 credential provider：`feishu-oauth-provider`（`CustomOauth2`，`authorizationServerMetadata` 手填飞书端点）
- Workload identity：`feishu-mcp-workload`
- 账号 <AWS_ACCOUNT_ID>，region us-east-1

## 验收标准（本次范围）

以下场景端到端跑通即算完成：

1. ~~`lark-cli`（authsidecar 编译）+ 自建 sidecar server（接 AgentCore Identity）+ MCP Wrapper 三者本地跑通~~ ✅ **已验证（2026-07-18）**：三进程本地联调通过。`lark-cli`（authsidecar tag 编译，源码零改动）→ sidecar server（HMAC 校验 + AgentCore Identity token 换取）→ MCP Wrapper（官方 MCP Go SDK，Streamable HTTP，暴露 `lark_exec`/`lark_list_skills`/`lark_get_skill` 三个工具）。用真实 MCP 协议握手（`initialize` → `notifications/initialized` → `tools/list`）验证协议兼容性，`serverInfo.name` 正确返回 `lark-cli-mcp-wrapper`。
2. ~~首次通过浏览器完成一次飞书 3LO 授权后，DevOps Agent 通过 MCP Wrapper 成功调用至少一个飞书操作~~ ✅ **已验证并超出预期（2026-07-18）**：完成一次浏览器 3LO 授权后，通过 MCP `tools/call` 真实验证了三类飞书操作：(a) 发送 IM 消息（`im +messages-send`，真实 `message_id`，人工确认飞书客户端收到）；(b) 读取日历（`calendar +agenda`，`ok:true`）；(c) 创建并写入飞书文档（`docs +create` + `docs +update --command append`，真实生成 `https://my.feishu.cn/docx/Mpaud1bmNo5a1sxmnlHcT8y0nYg`，人工确认标题和内容都正确写入）。过程中遇到并解决了两处问题：一次探测性请求中看到过 AgentCore 400（后续对 `calendar:calendar` 这个 scope 单独和与其他 scope 组合多次独立复现均未再出现，更可能是那次探测本身的一次性工具/网络异常，而不是这个 scope 字符串格式有问题），以及飞书应用后台未开通 3 个日历权限（Task 9 期间通过后台手动开通并重新发布解决）。
3. ~~确认 token 过期后自动 refresh 有效~~ ✅ 已验证（2026-07-18，见下方「未决问题」第 1 条）。

Docker 化 + 实际部署到 AgentCore Runtime（`create-agent-runtime` + Gateway + associate 到 DevOps Agent agent1）**不在本次范围**，留作后续步骤。

## 未决问题（不阻塞本次设计，留给实施/后续验证）

1. ~~**Refresh token 自动续期是否真的生效**~~ ✅ **已验证（2026-07-18）**：加了一个调试端点 `/_sidecar/debug/force-refresh`（不等真实 2 小时，强制清空本地缓存后立即重新调用 `GetResourceOauth2Token`，观察 AgentCore Identity 服务端的真实响应）。结果：`{"got_access_token": true, "got_authorization_url": false, "session_status": ""}`——即使本地缓存被清空，AgentCore Identity 依然凭它服务端存的 refresh token 直接给出新的有效 access token，完全不需要重新走浏览器 3LO 授权。飞书虽然不在官方列出的 vendor 白名单里，但 refresh 机制照样生效，问题已解决。
2. **首次授权 session_id 回传的具体交互方式**：留给实施计划阶段设计（见上文「首次 3LO 授权流程」小节末尾）。
3. **MCP Wrapper 的具体工具集设计**：确定了"少量通用工具、仿 Meta Tool 模式"的方向（比如 `lark_exec` 接命令字符串执行，而不是给 200+ 命令逐个建 schema），但具体工具名、参数形状、要不要做 `lark_list_skills`/`lark_discover` 之类的按需发现机制，留给实施计划阶段设计。
4. **Docker 化与 AgentCore Runtime 实际部署步骤**：不在本次范围，本次只做到本地三进程联调通过。

## 参考资料

- `larksuite/cli` 源码（本次调研版本，2026-07-17 前后）：
  - `extension/credential/sidecar/provider.go` — sidecar 凭证 provider（沙箱侧）
  - `extension/transport/sidecar/interceptor.go` — 请求拦截替换哨兵 token
  - `sidecar/protocol.go` — HMAC 签名 wire protocol 定义
  - `sidecar/server-demo/` — 单租户参考实现（本次设计的直接基础）
  - `sidecar/server-multi-tenant-demo/` — 多租户参考实现（含 Device Flow 登录管理端点设计，本次不需要多租户但可参考其登录管理端点的实现方式）
  - `internal/auth/device_flow.go`、`internal/auth/token_store.go` — 官方默认认证机制（本次不使用，仅供理解对比）
- basic-memory 笔记《飞书MCP接入AgentCore Identity验证记录》（project `main`）：AgentCore Identity + 飞书自定义 OAuth2 provider 的完整 boto3 手工验证记录，本次 sidecar server 里调 AWS API 的部分直接复用这套已验证的调用序列。
- AWS 官方文档：`identity-add-oauth-client-custom.html`、`oauth2-authorization-url-session-binding.html`、`get-workload-access-token.html`、`scope-credential-provider-access.html`
- fork 仓库：
  - `https://github.com/panlm/cli`（本地 `~/Documents/git/lark-cli`，fork 自 `larksuite/cli`，本次改造的主仓库）
  - `https://github.com/panlm/lark-openapi-mcp`（本地 `~/Documents/git/lark-openapi-mcp`，fork 自 `larksuite/lark-openapi-mcp`，评估后放弃使用，保留 fork 以防后续需要参考对比）
