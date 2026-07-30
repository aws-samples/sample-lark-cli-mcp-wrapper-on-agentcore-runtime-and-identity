[English](README.md) | [中文](README_CN.md)

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

Authorization is a manual step, available two ways.

**Via the MCP tools** (works from any MCP client, including a chat interface —
this is the only option when the sidecar's loopback endpoints aren't reachable,
e.g. once deployed to AgentCore Runtime):

1. Call `lark_auth_start` (no arguments). It returns an `authorization_url`
   and a `session_id`.
2. Open the URL in a browser and log in to Feishu.
3. Call `lark_auth_complete` with that `session_id`.

Step 3 needs only the session URI, so the browser does **not** have to reach
the configured return URL — a redirect that fails to load is fine, and the
`session_id` query parameter can also be copied out of the address bar.
Complete within ~5 minutes: the underlying OAuth authorization code is
short-lived.

Note for clients that filter tool output: some transports (observed with AWS
DevOps Agent) replace URL-shaped strings with `<redacted>` before the model
sees them. `lark_auth_start` therefore also returns
`authorization_url_without_scheme` and the bare `request_uri`, so a usable form
survives; prepend `https://` to reassemble.

**Via the sidecar directly** (loopback only — local development, or `docker
exec` into the container):

```
curl -X POST http://127.0.0.1:16385/_sidecar/auth/start
```

Here the browser is redirected automatically to the sidecar's dedicated
callback listener (`--callback-listen`, default `127.0.0.1:3000`), which
completes the binding with no copy-pasting.

Both paths call `GetResourceOauth2Token` with `ForceAuthentication: true`;
omitting it reproducibly makes the subsequent `CompleteResourceTokenAuth` fail
with `AccessDeniedException: Invalid or expired session`, regardless of retry
delay. A consequence is that `lark_auth_start` always issues a fresh
authorization URL, even when a valid token is already vaulted.

Subsequent token fetches (the sidecar's data-plane path, `ResolveToken`) call
the same API **without** `ForceAuthentication`, so a cached/refreshed token is
returned without re-prompting the user.

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
