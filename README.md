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

## Architecture

```
DevOps Agent (or any MCP client)
    │ MCP over Gateway (SigV4)
    ▼
AgentCore Gateway
    │
    ▼
AgentCore Runtime container (this repo's image)
    ├── mcp-wrapper       (0.0.0.0:8000/mcp — MCP protocol, shells out to lark-cli)
    ├── lark-cli           (-tags authsidecar — sandboxed, only ever sees sentinel tokens)
    └── sidecar-agentcore-identity (loopback-only — resolves REAL Feishu tokens)
              │ GetWorkloadAccessTokenForUserId / GetResourceOauth2Token /
              │ CompleteResourceTokenAuth
              ▼
        AWS Bedrock AgentCore Identity — Token Vault (3LO)
              │
              ▼
        open.feishu.cn / accounts.feishu.cn
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

Full design rationale and the step-by-step verification log: see
[`docs/AGENTCORE_IDENTITY_MCP_DESIGN.md`](docs/AGENTCORE_IDENTITY_MCP_DESIGN.md) and
[`docs/AGENTCORE_IDENTITY_MCP_PLAN.md`](docs/AGENTCORE_IDENTITY_MCP_PLAN.md).

## First-time authorization (3LO)

This is a single fixed-user deployment (`userId` is a config constant, not
derived per-request — see `docs/AGENTCORE_IDENTITY_MCP_PLAN.md` for what a
multi-tenant `actor_id`-keyed version would need). Authorization is a one-time
manual step:

```
curl -X POST http://127.0.0.1:16385/_sidecar/auth/start
```

Open the returned `authorization_url` in a browser, log in to Feishu, and the
browser is redirected automatically to the sidecar's dedicated callback
listener (`--callback-listen`, default `127.0.0.1:3000`) — no manual
copy-pasting of `session_id`. `GetResourceOauth2Token` is called with
`ForceAuthentication: true` here; omitting it reproducibly makes the
subsequent `CompleteResourceTokenAuth` fail with `AccessDeniedException:
Invalid or expired session`, regardless of retry delay.

Subsequent token fetches (the sidecar's data-plane path, `ResolveToken`) call
the same API **without** `ForceAuthentication`, so a cached/refreshed token is
returned without re-prompting the user.

## Build

```
docker buildx build --platform linux/arm64 -f deploy/agentcore-runtime/Dockerfile -t <tag> .
```

`LARK_CLI_VERSION` (Dockerfile build arg) pins the official lark-cli version
installed via `go install -tags authsidecar github.com/larksuite/cli@<version>`.

## Deploy

Push the image to ECR and point an AgentCore Runtime resource at it
(`create-agent-runtime` / `update-agent-runtime`), fronted by an AgentCore
Gateway for external MCP clients. See `docs/AGENTCORE_IDENTITY_MCP_PLAN.md` for
the exact AWS CLI commands used to provision the OAuth2 credential provider,
workload identity, and IAM roles this depends on.
