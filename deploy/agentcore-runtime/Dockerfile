# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT
#
# AgentCore Runtime container for the lark-cli MCP wrapper.
#
# Bundles three Go binaries:
#   - lark-cli: `go install`ed straight from the official upstream module
#     (github.com/larksuite/cli) at a pinned version, built with -tags
#     authsidecar so it produces only sentinel tokens; shelled out to per
#     MCP tool call. Not vendored/forked — this repo only depends on the
#     upstream module and its public sidecar wire-protocol package.
#   - sidecar-agentcore-identity (this module, -tags authsidecar_agentcore_identity):
#     resolves real Feishu tokens from AWS Bedrock AgentCore Identity's Token
#     Vault, loopback-only.
#   - mcp-wrapper (independent module in mcp-wrapper/): speaks MCP Streamable
#     HTTP on 0.0.0.0:8000/mcp per AgentCore Runtime's container contract.
#
# Build from the repo root:
#   docker buildx build --platform linux/arm64 -f deploy/agentcore-runtime/Dockerfile -t <tag> .

FROM golang:1.26-bookworm AS build

ARG LARK_CLI_VERSION=v1.0.72

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOBIN=/out \
    go install -tags authsidecar "github.com/larksuite/cli@${LARK_CLI_VERSION}" \
    && mv /out/cli /out/lark-cli

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY internal/ internal/
COPY sidecar/ sidecar/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -tags authsidecar_agentcore_identity \
      -o /out/sidecar-agentcore-identity ./sidecar/

WORKDIR /src/mcp-wrapper
COPY mcp-wrapper/go.mod mcp-wrapper/go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY mcp-wrapper/ ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -o /out/mcp-wrapper .

FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl \
  && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/lark-cli /usr/local/bin/lark-cli
COPY --from=build /out/sidecar-agentcore-identity /usr/local/bin/sidecar-agentcore-identity
COPY --from=build /out/mcp-wrapper /usr/local/bin/mcp-wrapper
COPY deploy/agentcore-runtime/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENV AGENTCORE_RUNTIME=1

EXPOSE 8000

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
