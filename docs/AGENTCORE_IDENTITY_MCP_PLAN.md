# 飞书 lark-cli 接入 AWS AgentCore Identity + MCP Wrapper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 lark-cli（fork 自 `larksuite/cli`）在无浏览器/无本机 Keychain 的环境下也能拿到真实飞书 access token（来源：AWS Bedrock AgentCore Identity 的 Token Vault），并通过一个新的 MCP Wrapper 把它暴露给 AWS DevOps Agent 调用。

**Architecture:** 三个独立进程本地联调（不部署到容器）：(1) `lark-cli` 编译时带 `authsidecar` build tag，运行时只产生哨兵 token；(2) 新的 Go sidecar server（在 `panlm/cli` 仓库内新目录 `sidecar/server-agentcore-identity/`）验证 HMAC 签名后，改用 AWS SDK for Go v2 调 AgentCore Identity 的 `GetWorkloadAccessTokenForUserId`/`GetResourceOauth2Token`/`CompleteResourceTokenAuth` 换取真实飞书 token 并注入转发请求；(3) 新的 Go MCP Wrapper（独立目录 `mcp-wrapper/`，同仓库）讲 MCP Streamable HTTP 协议对外，内部 shell 调 `lark-cli` 二进制执行命令。

**Tech Stack:** Go 1.23（跟仓库 go.mod 一致）、`github.com/aws/aws-sdk-go-v2/service/bedrockagentcore` v1.26.0、`github.com/aws/aws-sdk-go-v2/config`、MCP Wrapper 用 `github.com/modelcontextprotocol/go-sdk`（如该库不可用则退回手写最小 JSON-RPC/HTTP 处理，见 Task 6 的降级方案）。

## Global Constraints

- 不修改 `larksuite/cli` 已有代码（`extension/`、`internal/`、`cmd/` 等目录一律只读，不改动），所有新代码放在新目录下。
- sidecar 与 lark-cli 必须同机（sidecar 协议本身强制校验，`sidecar.ValidateProxyAddr` 只认 loopback/同机别名），本次全部在 `127.0.0.1` 上跑。
- AgentCore Identity 账号 <AWS_ACCOUNT_ID>，region `us-east-1`，复用已建好的 provider `feishu-oauth-provider` 和 workload identity `feishu-mcp-workload`，不重建。
- userId 用固定字符串 `"panlm"`（代表本人），不做多用户隔离。
- 每完成一个 Task 提交一次 git commit。
- 本次范围止于本地三进程联调通过；不做 Docker 化、不做 AgentCore Runtime 实际部署。

---

## Task 0: 环境准备 — 安装 Go 并验证仓库能编译

**Files:** 无代码改动，纯环境验证。

**Interfaces:** 无（这是本地环境的前置条件，不产出代码接口）。

- [ ] **Step 1: 安装 Go 1.23+**

```bash
brew install go
go version
```

Expected: 输出形如 `go version go1.23.x darwin/arm64` 或更高版本（`go.mod` 要求 `go 1.23.0`）。

- [ ] **Step 2: 验证仓库默认目标能编译**

```bash
cd ~/Documents/git/lark-cli
make build
```

Expected: 生成 `./lark-cli` 二进制，无编译错误。

- [ ] **Step 3: 验证 authsidecar build tag 能编译**

```bash
go build -tags authsidecar -o /tmp/lark-cli-sidecar .
ls -la /tmp/lark-cli-sidecar
```

Expected: 生成 `/tmp/lark-cli-sidecar` 二进制，无编译错误（这是官方 Makefile `sidecar-test` 目标里的第一条命令，确认 sidecar 凭证扩展点在当前代码状态下确实能正常编译进去）。

- [ ] **Step 4: 验证官方 sidecar-test 全部通过（确认没有破坏任何已有机制的前提条件）**

```bash
make sidecar-test
```

Expected: 全部测试 PASS（这条命令会跑 `extension/credential/sidecar/`、`extension/transport/sidecar/`、`sidecar/server-demo/` 的测试）。

不提交（本任务无代码变更）。

---

## Task 1: Sidecar Server 骨架 — 复制官方 server-demo 结构，验证原样可编译可跑

**Files:**
- Create: `sidecar/server-agentcore-identity/main.go`
- Create: `sidecar/server-agentcore-identity/handler.go`
- Create: `sidecar/server-agentcore-identity/forward.go`
- Create: `sidecar/server-agentcore-identity/allowlist.go`
- Create: `sidecar/server-agentcore-identity/audit.go`

**Interfaces:**
- Consumes: `github.com/larksuite/cli/sidecar`（wire protocol 常量与函数：`sidecar.HeaderProxyVersion`、`sidecar.HeaderProxyTarget`、`sidecar.HeaderProxyIdentity`、`sidecar.HeaderProxySignature`、`sidecar.HeaderProxyTimestamp`、`sidecar.HeaderBodySHA256`、`sidecar.HeaderProxyAuthHeader`、`sidecar.HeaderMCPUAT`、`sidecar.HeaderMCPTAT`、`sidecar.ProtocolV1`、`sidecar.IdentityUser`、`sidecar.IdentityBot`、`sidecar.CanonicalRequest{}`、`sidecar.Verify(key, canonical, signature) error`、`sidecar.BodySHA256(body []byte) string`）
- Produces: 一个监听 `127.0.0.1:16385`（跟官方 demo 的 16384 区分开，避免同时跑两个 sidecar 冲突）的 HTTP server，行为先与官方 `server-demo` 完全等价（本 Task 只是搬迁验证骨架能跑，凭证换成 AgentCore Identity 是下一个 Task）。

这一步先原样照抄 `sidecar/server-demo/*.go` 的四个非 main 文件内容到新目录，仅改：包名保持 `main`，build tag 从 `authsidecar_demo` 改成 `authsidecar_agentcore_identity`，`main.go` 里默认监听端口从 `sidecar.DefaultListenAddr`（16384）改成硬编码 `127.0.0.1:16385`。这样能先验证"骨架搬迁没搬错"，下一个 Task 再动真正的凭证逻辑。

- [ ] **Step 1: 复制 allowlist.go（原样，只改 build tag）**

```bash
mkdir -p ~/Documents/git/lark-cli/sidecar/server-agentcore-identity
```

写 `sidecar/server-agentcore-identity/allowlist.go`：

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"strings"

	"github.com/larksuite/cli/internal/core"
	"github.com/larksuite/cli/sidecar"
)

// buildAllowedHosts extracts the set of allowed target hostnames from
// multiple brand endpoints so the sidecar can serve both feishu and lark clients.
func buildAllowedHosts(endpoints ...core.Endpoints) map[string]bool {
	hosts := make(map[string]bool)
	for _, ep := range endpoints {
		for _, u := range []string{ep.Open, ep.Accounts, ep.MCP} {
			if idx := strings.Index(u, "://"); idx >= 0 {
				hosts[u[idx+3:]] = true
			}
		}
	}
	return hosts
}

// buildAllowedIdentities returns the set of identities the sidecar is allowed to serve.
// Fixed single-user deployment: always allow both user and bot identities.
func buildAllowedIdentities() map[string]bool {
	return map[string]bool{
		sidecar.IdentityUser: true,
		sidecar.IdentityBot:  true,
	}
}
```

（与官方版本的差异：`buildAllowedIdentities` 不再读 `cfg.SupportedIdentities`，因为本次单人部署不需要 strict-mode 配置链路，固定允许两种身份，简化掉一个依赖。）

- [ ] **Step 2: 复制 forward.go（原样，仅包名/路径不变）**

写 `sidecar/server-agentcore-identity/forward.go`：

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/larksuite/cli/sidecar"
)

// newForwardClient creates an HTTP client for forwarding requests to the
// Lark API. It strips Authorization on cross-host redirects and disables
// proxy to prevent real tokens from leaking through environment proxies.
func newForwardClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // never proxy the trusted hop
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
				req.Header.Del(sidecar.HeaderMCPUAT)
				req.Header.Del(sidecar.HeaderMCPTAT)
			}
			return nil
		},
	}
}

// isProxyHeader returns true for headers specific to the sidecar protocol.
func isProxyHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case http.CanonicalHeaderKey(sidecar.HeaderProxyTarget),
		http.CanonicalHeaderKey(sidecar.HeaderProxyIdentity),
		http.CanonicalHeaderKey(sidecar.HeaderProxySignature),
		http.CanonicalHeaderKey(sidecar.HeaderProxyTimestamp),
		http.CanonicalHeaderKey(sidecar.HeaderBodySHA256),
		http.CanonicalHeaderKey(sidecar.HeaderProxyAuthHeader):
		return true
	}
	return false
}
```

- [ ] **Step 3: 复制 audit.go（原样）**

先读官方文件确认内容：

```bash
cat ~/Documents/git/lark-cli/sidecar/server-demo/audit.go
```

把读到的内容原样写入 `sidecar/server-agentcore-identity/audit.go`，只把文件头的 `//go:build authsidecar_demo` 改成 `//go:build authsidecar_agentcore_identity`。

- [ ] **Step 4: 写 handler.go（本 Task 先保留官方 keychain 逻辑，占位跑通链路；Task 2 再替换）**

写 `sidecar/server-agentcore-identity/handler.go`：

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/larksuite/cli/sidecar"
)

// tokenResolver is the seam this Task's handler depends on. Task 2 replaces
// the implementation passed into proxyHandler with one backed by AgentCore
// Identity; this Task's implementation is a stub returning a fixed error so
// the HTTP plumbing (HMAC verification, forwarding) can be verified independently.
type tokenResolver interface {
	ResolveToken(ctx context.Context, identity string) (string, error)
}

// stubTokenResolver always fails. Used only to prove the request pipeline
// (steps 0-5.5 in ServeHTTP) works before wiring in the real resolver.
type stubTokenResolver struct{}

func (stubTokenResolver) ResolveToken(ctx context.Context, identity string) (string, error) {
	return "", fmt.Errorf("stubTokenResolver: not implemented (Task 1 placeholder)")
}

// proxyHandler handles HTTP requests from sandbox CLI instances.
type proxyHandler struct {
	key          []byte
	resolver     tokenResolver
	logger       *log.Logger
	forwardCl    *http.Client
	allowedHosts map[string]bool
	allowedIDs   map[string]bool
}

var allowedAuthHeaders = map[string]bool{
	"Authorization":      true,
	sidecar.HeaderMCPUAT: true,
	sidecar.HeaderMCPTAT: true,
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	version := r.Header.Get(sidecar.HeaderProxyVersion)
	if version != sidecar.ProtocolV1 {
		http.Error(w, "unsupported "+sidecar.HeaderProxyVersion+": "+version, http.StatusBadRequest)
		return
	}

	ts := r.Header.Get(sidecar.HeaderProxyTimestamp)
	if ts == "" {
		http.Error(w, "missing "+sidecar.HeaderProxyTimestamp, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	claimedSHA := r.Header.Get(sidecar.HeaderBodySHA256)
	if claimedSHA == "" {
		http.Error(w, "missing "+sidecar.HeaderBodySHA256, http.StatusBadRequest)
		return
	}
	actualSHA := sidecar.BodySHA256(body)
	if claimedSHA != actualSHA {
		http.Error(w, "body SHA256 mismatch", http.StatusBadRequest)
		return
	}

	target := r.Header.Get(sidecar.HeaderProxyTarget)
	if target == "" {
		http.Error(w, "missing "+sidecar.HeaderProxyTarget, http.StatusBadRequest)
		return
	}

	pathAndQuery := r.URL.RequestURI()
	targetHost, err := parseTarget(target)
	if err != nil {
		http.Error(w, "invalid "+sidecar.HeaderProxyTarget+": "+err.Error(), http.StatusForbidden)
		h.logger.Printf("REJECT method=%s path=%s reason=%q", r.Method, pathAndQuery, err.Error())
		return
	}

	identity := r.Header.Get(sidecar.HeaderProxyIdentity)
	if identity == "" {
		http.Error(w, "missing "+sidecar.HeaderProxyIdentity, http.StatusBadRequest)
		return
	}
	authHeader := r.Header.Get(sidecar.HeaderProxyAuthHeader)
	if authHeader == "" {
		http.Error(w, "missing "+sidecar.HeaderProxyAuthHeader, http.StatusBadRequest)
		return
	}

	signature := r.Header.Get(sidecar.HeaderProxySignature)
	if err := sidecar.Verify(h.key, sidecar.CanonicalRequest{
		Version:      version,
		Method:       r.Method,
		Host:         targetHost,
		PathAndQuery: pathAndQuery,
		BodySHA256:   claimedSHA,
		Timestamp:    ts,
		Identity:     identity,
		AuthHeader:   authHeader,
	}, signature); err != nil {
		http.Error(w, "HMAC verification failed: "+err.Error(), http.StatusUnauthorized)
		h.logger.Printf("REJECT method=%s path=%s reason=%q", r.Method, pathAndQuery, err.Error())
		return
	}

	if !h.allowedHosts[targetHost] {
		http.Error(w, "target host not allowed: "+targetHost, http.StatusForbidden)
		return
	}
	if !h.allowedIDs[identity] {
		http.Error(w, "identity not allowed: "+identity, http.StatusForbidden)
		return
	}
	if !allowedAuthHeaders[authHeader] {
		http.Error(w, "auth-header not allowed: "+authHeader, http.StatusForbidden)
		return
	}

	token, err := h.resolver.ResolveToken(r.Context(), identity)
	if err != nil {
		http.Error(w, "failed to resolve token: "+err.Error(), http.StatusInternalServerError)
		h.logger.Printf("TOKEN_ERROR method=%s path=%s identity=%s error=%q", r.Method, pathAndQuery, identity, err.Error())
		return
	}

	forwardURL := "https://" + targetHost + pathAndQuery
	forwardReq, err := http.NewRequestWithContext(r.Context(), r.Method, forwardURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to create forward request", http.StatusInternalServerError)
		return
	}

	for k, vs := range r.Header {
		if isProxyHeader(k) {
			continue
		}
		for _, v := range vs {
			forwardReq.Header.Add(k, v)
		}
	}

	forwardReq.Header.Del("Authorization")
	forwardReq.Header.Del(sidecar.HeaderMCPUAT)
	forwardReq.Header.Del(sidecar.HeaderMCPTAT)

	if authHeader == "Authorization" {
		forwardReq.Header.Set("Authorization", "Bearer "+token)
	} else {
		forwardReq.Header.Set(authHeader, token)
	}

	resp, err := h.forwardCl.Do(forwardReq)
	if err != nil {
		http.Error(w, "forward request failed: "+err.Error(), http.StatusBadGateway)
		h.logger.Printf("FORWARD_ERROR method=%s path=%s error=%q", r.Method, pathAndQuery, err.Error())
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	h.logger.Printf("FORWARD method=%s path=%s identity=%s status=%d duration=%s",
		r.Method, pathAndQuery, identity, resp.StatusCode, time.Since(start).Round(time.Millisecond))
}

func parseTarget(target string) (host string, err error) {
	u, perr := url.Parse(target)
	if perr != nil {
		return "", fmt.Errorf("parse: %w", perr)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if u.User != nil {
		return "", fmt.Errorf("userinfo not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("path not allowed (got %q)", u.Path)
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("query not allowed")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("fragment not allowed")
	}
	return u.Host, nil
}
```

（与官方版本的关键差异：`h.cred *credential.CredentialProvider` + `h.appID`/`h.brand` 换成了 `h.resolver tokenResolver` 这个新接口，`credential.TokenSpec`/`TokenType` 相关逻辑全部移到 `tokenResolver` 实现内部——这是 Task 2 要替换的缝。）

- [ ] **Step 5: 写 main.go（先用 stubTokenResolver 占位）**

写 `sidecar/server-agentcore-identity/main.go`：

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

// Command sidecar-agentcore-identity is the auth sidecar proxy that resolves
// real Feishu tokens from AWS Bedrock AgentCore Identity's Token Vault
// instead of the local OS keychain. See superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md and
// superpowers-docs/AGENTCORE_IDENTITY_MCP_PLAN.md for the full design.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/larksuite/cli/internal/core"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:16385", "listen address (host:port)")
	keyFile := flag.String("key-file", defaultKeyFile(), "path to write the HMAC key")
	logFile := flag.String("log-file", "", "audit log file (stderr if empty)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *listen, *keyFile, *logFile); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultKeyFile() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".lark-sidecar-agentcore", "proxy.key")
	}
	return "/tmp/lark-sidecar-agentcore/proxy.key"
}

func run(ctx context.Context, listen, keyFile, logFile string) error {
	if listen == "" {
		return fmt.Errorf("invalid --listen address: empty")
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("failed to generate HMAC key: %v", err)
	}
	keyHex := hex.EncodeToString(keyBytes)

	keyDir := filepath.Dir(keyFile)
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return fmt.Errorf("failed to create key directory: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte(keyHex), 0600); err != nil {
		return fmt.Errorf("failed to write key file: %v", err)
	}

	var auditLogger *log.Logger
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("failed to open log file: %v", err)
		}
		defer f.Close()
		auditLogger = log.New(f, "", log.LstdFlags)
	} else {
		auditLogger = log.New(os.Stderr, "[audit] ", log.LstdFlags)
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", listen, err)
	}
	defer listener.Close()

	allowedHosts := buildAllowedHosts(
		core.ResolveEndpoints(core.BrandFeishu),
		core.ResolveEndpoints(core.BrandLark),
	)
	allowedIDs := buildAllowedIdentities()

	handler := &proxyHandler{
		key:          []byte(keyHex),
		resolver:     stubTokenResolver{}, // Task 2 replaces this
		logger:       auditLogger,
		forwardCl:    newForwardClient(),
		allowedHosts: allowedHosts,
		allowedIDs:   allowedIDs,
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		auditLogger.Println("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			auditLogger.Printf("shutdown error: %v", err)
		}
	}()

	keyPrefix := keyHex
	if len(keyPrefix) > 8 {
		keyPrefix = keyPrefix[:8]
	}
	proxyURL := "http://" + listen
	fmt.Fprintf(os.Stderr, "AgentCore Identity auth sidecar listening on %s\n", proxyURL)
	fmt.Fprintf(os.Stderr, "HMAC key prefix: %s\n", keyPrefix)
	fmt.Fprintf(os.Stderr, "Full key written to %s (mode 0600)\n", keyFile)

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("sidecar server exited unexpectedly: %v", err)
	}
	return nil
}
```

- [ ] **Step 6: 编译验证**

```bash
cd ~/Documents/git/lark-cli
go build -tags authsidecar_agentcore_identity -o /tmp/sidecar-agentcore-identity ./sidecar/server-agentcore-identity/
ls -la /tmp/sidecar-agentcore-identity
```

Expected: 编译成功，无错误。

- [ ] **Step 7: 手动跑一下确认 HTTP 服务器能起来**

```bash
/tmp/sidecar-agentcore-identity --listen 127.0.0.1:16385 &
SIDECAR_PID=$!
sleep 1
curl -sS -o /dev/null -w "%{http_code}\n" http://127.0.0.1:16385/anything
kill $SIDECAR_PID
```

Expected: 打印一个 HTTP 状态码（因为没带任何 sidecar 协议 header，预期是 `400`，说明 `ServeHTTP` 的 header 校验逻辑在跑）。

- [ ] **Step 8: Commit**

```bash
cd ~/Documents/git/lark-cli
git add sidecar/server-agentcore-identity/
git commit -m "feat(sidecar): scaffold AgentCore Identity sidecar server (stub token resolver)"
```

---

## Task 2: AgentCore Identity Token Resolver — 接入真实 AWS API

**Files:**
- Create: `sidecar/server-agentcore-identity/agentcore_resolver.go`
- Create: `sidecar/server-agentcore-identity/agentcore_resolver_test.go`
- Modify: `sidecar/server-agentcore-identity/main.go`（把 `stubTokenResolver{}` 换成 `agentcoreTokenResolver`）
- Modify: `go.mod`（新增依赖）

**Interfaces:**
- Consumes: `tokenResolver` 接口（Task 1 定义：`ResolveToken(ctx context.Context, identity string) (string, error)`）
- Produces: `agentcoreTokenResolver` struct，实现 `tokenResolver`。内部维护一个进程内内存缓存（`sync.RWMutex` 保护的 map，key 是 identity 字符串 `"user"`/`"bot"`，value 是 `{token string, expiresAt time.Time}`），下一 Task（Task 3）的授权管理端点会往这个缓存里触发首次写入。

先安装依赖：

- [ ] **Step 1: 添加 AWS SDK for Go v2 依赖**

```bash
cd ~/Documents/git/lark-cli
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/service/bedrockagentcore@latest
go mod tidy
```

Expected: `go.mod` 新增两行 require，无报错。

- [ ] **Step 2: 写 agentcore_resolver.go**

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// agentcoreConfig holds the fixed identifiers for this single-user deployment.
// These values correspond to resources already created via boto3 in prior
// manual verification (see superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md).
type agentcoreConfig struct {
	Region               string
	WorkloadName         string
	CredentialProviderID string
	UserID               string
	ReturnURL            string
	Scopes               []string
}

func defaultAgentcoreConfig() agentcoreConfig {
	return agentcoreConfig{
		Region:               "us-east-1",
		WorkloadName:         "feishu-mcp-workload",
		CredentialProviderID: "feishu-oauth-provider",
		UserID:               "panlm",
		ReturnURL:            "http://localhost:3000/callback",
		Scopes:               []string{"im:message", "im:chat:readonly", "calendar:calendar"},
	}
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// agentcoreTokenResolver resolves real Feishu tokens via AWS Bedrock
// AgentCore Identity's Token Vault instead of the local OS keychain.
//
// Identity mapping: sidecar wire-protocol identity ("user"/"bot") maps
// 1:1 onto a single cached token slot, since this deployment serves exactly
// one Feishu user (see agentcoreConfig.UserID) and TAT (bot) requests are
// minted independently from AppID+AppSecret — TAT is intentionally NOT
// wired to AgentCore Identity in this design; only UAT (user) goes through
// the 3LO flow. A "bot" identity request returns an error, since this
// deployment's only registered use case is the user-token DevOps Agent path.
type agentcoreTokenResolver struct {
	client *bedrockagentcore.Client
	cfg    agentcoreConfig

	mu    sync.RWMutex
	cache map[string]cachedToken // key: identity ("user")

	logf func(format string, args ...any)
}

func newAgentcoreTokenResolver(ctx context.Context, cfg agentcoreConfig, logf func(string, ...any)) (*agentcoreTokenResolver, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	return &agentcoreTokenResolver{
		client: bedrockagentcore.NewFromConfig(awsCfg),
		cfg:    cfg,
		cache:  make(map[string]cachedToken),
		logf:   logf,
	}, nil
}

// ResolveToken implements tokenResolver. identity is "user" or "bot" from
// the sidecar wire protocol. Only "user" is supported in this deployment.
func (r *agentcoreTokenResolver) ResolveToken(ctx context.Context, identity string) (string, error) {
	if identity != "user" {
		return "", fmt.Errorf("agentcoreTokenResolver: identity %q not supported (only \"user\" is wired to AgentCore Identity)", identity)
	}

	r.mu.RLock()
	cached, ok := r.cache[identity]
	r.mu.RUnlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.token, nil
	}

	// Cache miss or expired: ask AgentCore Identity. If a valid refresh
	// token is stored server-side, this returns AccessToken directly
	// without requiring re-authorization (this is the refresh path we
	// verify in Task 5).
	workloadToken, err := r.getWorkloadAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get workload access token: %w", err)
	}

	out, err := r.client.GetResourceOauth2Token(ctx, &bedrockagentcore.GetResourceOauth2TokenInput{
		WorkloadIdentityToken:          aws.String(workloadToken),
		ResourceCredentialProviderName: aws.String(r.cfg.CredentialProviderID),
		Scopes:                         r.cfg.Scopes,
		Oauth2Flow:                     types.Oauth2FlowTypeUserFederation,
		ResourceOauth2ReturnUrl:        aws.String(r.cfg.ReturnURL),
	})
	if err != nil {
		return "", fmt.Errorf("GetResourceOauth2Token failed: %w", err)
	}

	if out.AccessToken == nil || *out.AccessToken == "" {
		if out.AuthorizationUrl != nil {
			// No cached token in the Vault yet — first-time authorization
			// is required. Task 3's management endpoints handle this flow;
			// surface the URL in the error so it's visible in the sidecar's
			// stderr/audit log even if called outside that flow.
			r.logf("AUTHORIZATION_REQUIRED url=%s session_uri=%s", *out.AuthorizationUrl, aws.ToString(out.SessionUri))
			return "", fmt.Errorf("authorization required: open %s in a browser, then POST the session_id to /_sidecar/auth/complete", *out.AuthorizationUrl)
		}
		return "", fmt.Errorf("GetResourceOauth2Token returned no AccessToken and no AuthorizationUrl")
	}

	// Feishu UAT lifetime is 2 hours; refresh 5 minutes early to avoid
	// races with in-flight requests.
	expiresAt := time.Now().Add(2*time.Hour - 5*time.Minute)
	r.mu.Lock()
	r.cache[identity] = cachedToken{token: *out.AccessToken, expiresAt: expiresAt}
	r.mu.Unlock()

	r.logf("TOKEN_RESOLVED identity=%s expires_at=%s", identity, expiresAt.Format(time.RFC3339))
	return *out.AccessToken, nil
}

func (r *agentcoreTokenResolver) getWorkloadAccessToken(ctx context.Context) (string, error) {
	out, err := r.client.GetWorkloadAccessTokenForUserId(ctx, &bedrockagentcore.GetWorkloadAccessTokenForUserIdInput{
		WorkloadName: aws.String(r.cfg.WorkloadName),
		UserId:       aws.String(r.cfg.UserID),
	})
	if err != nil {
		return "", err
	}
	if out.WorkloadAccessToken == nil {
		return "", fmt.Errorf("GetWorkloadAccessTokenForUserId returned nil token")
	}
	return *out.WorkloadAccessToken, nil
}

// completeAuth calls CompleteResourceTokenAuth to bind a completed 3LO
// browser authorization to this deployment's fixed userID, then evicts any
// stale cache entry so the next ResolveToken call fetches the real token.
func (r *agentcoreTokenResolver) completeAuth(ctx context.Context, sessionURI string) error {
	_, err := r.client.CompleteResourceTokenAuth(ctx, &bedrockagentcore.CompleteResourceTokenAuthInput{
		SessionUri: aws.String(sessionURI),
		UserIdentifier: &types.UserIdentifierMemberUserId{
			Value: r.cfg.UserID,
		},
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.cache, "user")
	r.mu.Unlock()
	return nil
}

// forceExpire evicts the cached token immediately, forcing the next
// ResolveToken call to hit AgentCore Identity. Used by Task 5's refresh
// verification (does NOT delete the underlying stored token/refresh_token
// in the Token Vault — only this process's local cache — so the next call
// exercises the exact same refresh-or-reauth decision AgentCore Identity
// would make after natural 2-hour expiry).
func (r *agentcoreTokenResolver) forceExpire() {
	r.mu.Lock()
	delete(r.cache, "user")
	r.mu.Unlock()
}
```

- [ ] **Step 3: 写单元测试（验证缓存命中逻辑，不打真实 AWS API）**

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"testing"
	"time"
)

func TestAgentcoreTokenResolver_CacheHit(t *testing.T) {
	r := &agentcoreTokenResolver{
		cfg:   defaultAgentcoreConfig(),
		cache: make(map[string]cachedToken),
		logf:  func(string, ...any) {},
	}
	r.cache["user"] = cachedToken{token: "cached-token-value", expiresAt: time.Now().Add(time.Hour)}

	got, err := r.ResolveToken(t.Context(), "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cached-token-value" {
		t.Errorf("got %q, want %q", got, "cached-token-value")
	}
}

func TestAgentcoreTokenResolver_RejectsBotIdentity(t *testing.T) {
	r := &agentcoreTokenResolver{
		cfg:   defaultAgentcoreConfig(),
		cache: make(map[string]cachedToken),
		logf:  func(string, ...any) {},
	}
	_, err := r.ResolveToken(t.Context(), "bot")
	if err == nil {
		t.Fatal("expected error for unsupported bot identity, got nil")
	}
}

func TestAgentcoreTokenResolver_ForceExpire(t *testing.T) {
	r := &agentcoreTokenResolver{
		cfg:   defaultAgentcoreConfig(),
		cache: make(map[string]cachedToken),
		logf:  func(string, ...any) {},
	}
	r.cache["user"] = cachedToken{token: "will-be-evicted", expiresAt: time.Now().Add(time.Hour)}
	r.forceExpire()

	r.mu.RLock()
	_, stillCached := r.cache["user"]
	r.mu.RUnlock()
	if stillCached {
		t.Fatal("expected cache entry to be evicted after forceExpire")
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
cd ~/Documents/git/lark-cli
go test -tags authsidecar_agentcore_identity -run TestAgentcoreTokenResolver -v ./sidecar/server-agentcore-identity/
```

Expected: 三个测试全部 PASS。

- [ ] **Step 5: main.go 接入真实 resolver**

修改 `sidecar/server-agentcore-identity/main.go`，把：

```go
	handler := &proxyHandler{
		key:          []byte(keyHex),
		resolver:     stubTokenResolver{}, // Task 2 replaces this
```

改成：

```go
	agentcoreCfg := defaultAgentcoreConfig()
	resolver, err := newAgentcoreTokenResolver(ctx, agentcoreCfg, auditLogger.Printf)
	if err != nil {
		return fmt.Errorf("failed to init AgentCore Identity resolver: %w", err)
	}

	handler := &proxyHandler{
		key:          []byte(keyHex),
		resolver:     resolver,
```

并删除文件顶部现在无用的 `stubTokenResolver`/`tokenResolver` 定义从 `handler.go` 里搬出来的部分——保持 `tokenResolver` 接口定义仍在 `handler.go`（其它文件消费它），仅删除 `stubTokenResolver` 这个具体实现（它已经不再被任何地方引用）。

在 `handler.go` 中删除：

```go
// stubTokenResolver always fails. Used only to prove the request pipeline
// (steps 0-5.5 in ServeHTTP) works before wiring in the real resolver.
type stubTokenResolver struct{}

func (stubTokenResolver) ResolveToken(ctx context.Context, identity string) (string, error) {
	return "", fmt.Errorf("stubTokenResolver: not implemented (Task 1 placeholder)")
}
```

- [ ] **Step 6: 编译验证**

```bash
cd ~/Documents/git/lark-cli
go build -tags authsidecar_agentcore_identity -o /tmp/sidecar-agentcore-identity ./sidecar/server-agentcore-identity/
```

Expected: 编译成功（这一步不需要真实 AWS 凭证，`config.LoadDefaultConfig` 只在运行时才需要）。

- [ ] **Step 7: Commit**

```bash
cd ~/Documents/git/lark-cli
git add sidecar/server-agentcore-identity/ go.mod go.sum
git commit -m "feat(sidecar): resolve Feishu tokens via AWS Bedrock AgentCore Identity"
```

---

## Task 3: 首次授权管理端点 — `/_sidecar/auth/start` 和 `/_sidecar/auth/complete`

**Files:**
- Create: `sidecar/server-agentcore-identity/auth_mgmt.go`
- Create: `sidecar/server-agentcore-identity/auth_mgmt_test.go`
- Modify: `sidecar/server-agentcore-identity/main.go`（注册管理端点路由）

**Interfaces:**
- Consumes: `agentcoreTokenResolver.completeAuth(ctx, sessionURI string) error`（Task 2 定义）、`bedrockagentcore.Client.GetResourceOauth2Token`（Task 2 已引入依赖）
- Produces: 两个 HTTP endpoint：
  - `POST /_sidecar/auth/start` — body 为空，返回 JSON `{"authorization_url": "...", "session_uri": "..."}`
  - `POST /_sidecar/auth/complete` — body `{"session_id": "..."}`（字段名对应飞书回调 URL 里的查询参数名，之前手工验证时确认过飞书跳转回调用的参数名是 `session_id`），返回 JSON `{"ok": true}` 或 `{"ok": false, "error": "..."}`

这两个端点跑在同一个 sidecar HTTP server 上（同端口 `16385`），本人用 `curl` 手动调用，不做 HMAC 校验（这是本机 loopback-only 的管理操作，且只有本人一个使用者，跟数据面的 HMAC 校验是两个不同的信任模型：数据面校验的是"lark-cli 沙箱侧发来的请求"，管理面校验的是"本人在本机敲的 curl"——同一台机器上敲命令本身就是信任边界，不需要额外签名）。

- [ ] **Step 1: 写 auth_mgmt.go**

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// authMgmt exposes loopback-only management endpoints for the one-time
// first-authorization flow. Unlike the data-plane proxy handler, these
// endpoints are NOT HMAC-verified: they are meant to be called by the
// operator directly via curl on the same machine running the sidecar, which
// is itself the trust boundary (see superpowers-docs/AGENTCORE_IDENTITY_MCP_PLAN.md Task 3).
type authMgmt struct {
	client   *bedrockagentcore.Client
	cfg      agentcoreConfig
	resolver *agentcoreTokenResolver
	logf     func(format string, args ...any)
}

func (a *authMgmt) getWorkloadAccessToken(ctx context.Context) (string, error) {
	out, err := a.client.GetWorkloadAccessTokenForUserId(ctx, &bedrockagentcore.GetWorkloadAccessTokenForUserIdInput{
		WorkloadName: aws.String(a.cfg.WorkloadName),
		UserId:       aws.String(a.cfg.UserID),
	})
	if err != nil {
		return "", err
	}
	if out.WorkloadAccessToken == nil {
		return "", fmt.Errorf("GetWorkloadAccessTokenForUserId returned nil token")
	}
	return *out.WorkloadAccessToken, nil
}

// handleStart initiates a fresh 3LO authorization flow, always returning an
// authorization_url (forceAuthentication semantics via always calling
// GetResourceOauth2Token fresh — AgentCore Identity itself decides whether
// a cached token already satisfies the request; if it does, this endpoint
// still surfaces it so the operator can confirm auth is already valid).
func (a *authMgmt) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonMgmtError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	workloadToken, err := a.getWorkloadAccessToken(r.Context())
	if err != nil {
		jsonMgmtError(w, http.StatusBadGateway, "failed to get workload access token: "+err.Error())
		return
	}

	out, err := a.client.GetResourceOauth2Token(r.Context(), &bedrockagentcore.GetResourceOauth2TokenInput{
		WorkloadIdentityToken:          aws.String(workloadToken),
		ResourceCredentialProviderName: aws.String(a.cfg.CredentialProviderID),
		Scopes:                         a.cfg.Scopes,
		Oauth2Flow:                     types.Oauth2FlowTypeUserFederation,
		ResourceOauth2ReturnUrl:        aws.String(a.cfg.ReturnURL),
	})
	if err != nil {
		jsonMgmtError(w, http.StatusBadGateway, "GetResourceOauth2Token failed: "+err.Error())
		return
	}

	resp := map[string]any{}
	if out.AccessToken != nil && *out.AccessToken != "" {
		resp["already_authorized"] = true
		a.logf("AUTH_START already_authorized=true")
	} else if out.AuthorizationUrl != nil {
		resp["authorization_url"] = *out.AuthorizationUrl
		resp["session_uri"] = aws.ToString(out.SessionUri)
		a.logf("AUTH_START authorization_url=%s session_uri=%s", *out.AuthorizationUrl, aws.ToString(out.SessionUri))
	} else {
		jsonMgmtError(w, http.StatusInternalServerError, "GetResourceOauth2Token returned neither AccessToken nor AuthorizationUrl")
		return
	}
	jsonMgmtOK(w, resp)
}

// handleComplete binds a completed browser authorization (identified by the
// session_id query parameter Feishu appended to the return URL) to this
// deployment's fixed userID, then evicts the resolver's cache so the next
// data-plane request fetches the freshly-bound real token.
func (a *authMgmt) handleComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonMgmtError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonMgmtError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.SessionID == "" {
		jsonMgmtError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	if err := a.resolver.completeAuth(r.Context(), req.SessionID); err != nil {
		jsonMgmtError(w, http.StatusBadGateway, "CompleteResourceTokenAuth failed: "+err.Error())
		a.logf("AUTH_COMPLETE_FAILED session_id=%s error=%q", req.SessionID, err.Error())
		return
	}

	a.logf("AUTH_COMPLETE_OK session_id=%s", req.SessionID)
	jsonMgmtOK(w, map[string]any{"ok": true})
}

func jsonMgmtOK(w http.ResponseWriter, data map[string]any) {
	data["ok"] = true
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonMgmtError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}
```

- [ ] **Step 2: 在 main.go 注册路由**

`net/http.Server{Handler: handler}` 目前把 `proxyHandler`（一个实现 `ServeHTTP` 的 struct）直接当唯一 handler。改用 `http.ServeMux` 分流：数据面走 `proxyHandler`，管理面走新的 `authMgmt`。

修改 `main.go` 里创建 `server` 之前的部分，插入：

```go
	mgmt := &authMgmt{
		client:   resolver.client,
		cfg:      agentcoreCfg,
		resolver: resolver,
		logf:     auditLogger.Printf,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_sidecar/auth/start", mgmt.handleStart)
	mux.HandleFunc("/_sidecar/auth/complete", mgmt.handleComplete)
	mux.Handle("/", handler) // everything else goes through the HMAC-verified proxy handler
```

并把：

```go
	server := &http.Server{
		Handler:           handler,
```

改成：

```go
	server := &http.Server{
		Handler:           mux,
```

- [ ] **Step 3: 写测试验证 handleComplete 的输入校验（不打真实 AWS API）**

```go
// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthMgmt_HandleComplete_RejectsMissingSessionID(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodPost, "/_sidecar/auth/complete", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	a.handleComplete(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAuthMgmt_HandleComplete_RejectsGET(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodGet, "/_sidecar/auth/complete", nil)
	rec := httptest.NewRecorder()

	a.handleComplete(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestAuthMgmt_HandleStart_RejectsGET(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodGet, "/_sidecar/auth/start", nil)
	rec := httptest.NewRecorder()

	a.handleStart(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
```

- [ ] **Step 4: 运行测试**

```bash
cd ~/Documents/git/lark-cli
go test -tags authsidecar_agentcore_identity -run TestAuthMgmt -v ./sidecar/server-agentcore-identity/
```

Expected: 三个测试全部 PASS。

- [ ] **Step 5: 编译验证整体**

```bash
go build -tags authsidecar_agentcore_identity -o /tmp/sidecar-agentcore-identity ./sidecar/server-agentcore-identity/
```

Expected: 编译成功。

- [ ] **Step 6: Commit**

```bash
cd ~/Documents/git/lark-cli
git add sidecar/server-agentcore-identity/
git commit -m "feat(sidecar): add loopback-only /_sidecar/auth/{start,complete} management endpoints"
```

---

## Task 4: 端到端首次授权验证（真实 AWS 调用，需要本人操作浏览器）

**Files:** 无新文件，本 Task 是运行时验证步骤。

**Interfaces:** 无新增。

**前置条件**：本机已配置好可用的 AWS 凭证（`AWS_PROFILE=panlm` 或默认 profile，region `us-east-1`，账号 <AWS_ACCOUNT_ID> 有权限调用 `bedrock-agentcore:*`——此前 boto3 手工验证时已确认当前身份权限够用，不需要额外 IAM 配置）。

- [ ] **Step 1: 启动 sidecar server**

```bash
cd ~/Documents/git/lark-cli
export AWS_PROFILE=panlm
export AWS_REGION=us-east-1
go build -tags authsidecar_agentcore_identity -o /tmp/sidecar-agentcore-identity ./sidecar/server-agentcore-identity/
/tmp/sidecar-agentcore-identity --listen 127.0.0.1:16385 --log-file /tmp/sidecar-agentcore-identity.log &
echo "PID: $!"
sleep 1
```

Expected: stderr 打印 `AgentCore Identity auth sidecar listening on http://127.0.0.1:16385`。

- [ ] **Step 2: 触发首次授权**

```bash
curl -sS -X POST http://127.0.0.1:16385/_sidecar/auth/start | python3 -m json.tool
```

Expected: 返回 JSON，包含 `authorization_url` 字段（如果之前手工验证时创建的 token 还没过期/还在 Vault 里，可能直接返回 `already_authorized: true`——如果是这种情况，跳到 Step 5 验证 `/anything` 代理请求，Step 3-4 可跳过）。

- [ ] **Step 3: 打开浏览器完成飞书授权**

把 Step 2 拿到的 `authorization_url` 复制到浏览器打开，完成飞书登录同意。浏览器最终会跳转到 `http://localhost:3000/callback?session_id=...`（本机没监听 3000 端口会显示连接失败，这是预期的——之前手工验证时确认过），从地址栏复制 `session_id` 参数的值（是一个 URL-encoded 的 `urn:ietf:params:oauth:request_uri:...` 字符串）。

- [ ] **Step 4: 完成 session binding**

```bash
curl -sS -X POST http://127.0.0.1:16385/_sidecar/auth/complete \
  -H "Content-Type: application/json" \
  -d '{"session_id": "<粘贴上一步拿到的 session_id 值，需要 URL-decode 一次>"}' \
  | python3 -m json.tool
```

Expected: 返回 `{"ok": true}`。

- [ ] **Step 5: 验证数据面代理请求真的能拿到有效 token（绕过 lark-cli，直接模拟一个签名请求）**

写一个一次性验证脚本（不进入正式代码，用完删）：

```bash
cat > /tmp/verify_sidecar_proxy.go << 'EOF'
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	keyHex, err := os.ReadFile(os.ExpandEnv("$HOME/.lark-sidecar-agentcore/proxy.key"))
	if err != nil {
		panic(err)
	}
	key, err := hex.DecodeString(string(keyHex))
	if err != nil {
		panic(err)
	}

	method := "GET"
	targetHost := "open.feishu.cn"
	pathAndQuery := "/open-apis/authen/v1/user_info"
	body := []byte{}
	bodySHA := sha256Hex(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	identity := "user"
	authHeader := "Authorization"

	canonical := "v1\n" + method + "\n" + targetHost + "\n" + pathAndQuery + "\n" + bodySHA + "\n" + ts + "\n" + identity + "\n" + authHeader
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(method, "http://127.0.0.1:16385"+pathAndQuery, bytes.NewReader(body))
	req.Header.Set("X-Lark-Proxy-Version", "v1")
	req.Header.Set("X-Lark-Proxy-Target", "https://"+targetHost)
	req.Header.Set("X-Lark-Proxy-Identity", identity)
	req.Header.Set("X-Lark-Proxy-Auth-Header", authHeader)
	req.Header.Set("X-Lark-Proxy-Signature", sig)
	req.Header.Set("X-Lark-Proxy-Timestamp", ts)
	req.Header.Set("X-Lark-Body-SHA256", bodySHA)
	req.Header.Set("Authorization", "Bearer sidecar-managed-uat") // sentinel, sidecar replaces it

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	fmt.Println("status:", resp.StatusCode)
	respBody, _ := io.ReadAll(resp.Body)
	fmt.Println(string(respBody))
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
EOF
go run /tmp/verify_sidecar_proxy.go
```

Expected: 输出 `status: 200`，body 包含飞书返回的真实用户信息 JSON（含 `open_id`、`name` 字段，跟之前 boto3 手工验证时拿到的 `panlm` 账号信息一致）。这就证明了 sidecar 的 HMAC 校验 + AgentCore Identity token 换取 + 转发注入 + 飞书 API 真实调用这条完整链路是通的——且完全不依赖 `lark-cli`（下一个 Task 才把 `lark-cli` 接进来）。

- [ ] **Step 6: 清理**

```bash
kill %1  # 停掉 sidecar server（Step 1 后台启动的进程）
rm /tmp/verify_sidecar_proxy.go
```

本 Task 无代码改动，不提交。

---

## Task 5: Refresh Token 自动续期验证

**Files:** 无新文件，本 Task 是运行时验证步骤，复用 Task 2 已实现的 `forceExpire()`。

**Interfaces:**
- Consumes: `agentcoreTokenResolver.forceExpire()`（Task 2 已实现）

这个 Task 验证设计文档里标记的未决问题：AgentCore Identity 对飞书的 refresh token 自动续期是否真的生效。做法：不等真实 2 小时过期，而是清空本地缓存强制下一次调用重新问 AgentCore Identity——AgentCore Identity 服务端会自己判断"缓存的 access token 是否还有效"，如果它认为已过期就会用 refresh token 换新的；这验证的是 AgentCore Identity 服务端的 refresh 逻辑，跟我们本地缓存过期与否无关（本地缓存只是一个减少 API 调用次数的优化，不是 refresh 机制本身）。

为了真正验证"过期后 refresh"而不是"缓存未过期直接返回"，需要一个额外的调试端点触发强制走一遍完整流程且能看到 AgentCore Identity 侧的响应细节。

- [ ] **Step 1: 加一个调试端点暴露 GetResourceOauth2Token 的完整响应（不只是 token 字符串）**

在 `auth_mgmt.go` 里加一个新方法：

```go
// handleDebugForceRefresh forces a fresh call to GetResourceOauth2Token,
// bypassing this process's local cache, and reports the SessionStatus AWS
// returns. This is used to verify AgentCore Identity's refresh-token
// behavior without waiting for the real 2-hour access-token expiry: if a
// valid refresh_token is stored server-side, AWS returns AccessToken
// directly (no AuthorizationUrl) even though our local cache was evicted —
// proving the refresh path, not just cache-hit, was exercised.
func (a *authMgmt) handleDebugForceRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonMgmtError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	a.resolver.forceExpire()

	workloadToken, err := a.getWorkloadAccessToken(r.Context())
	if err != nil {
		jsonMgmtError(w, http.StatusBadGateway, "failed to get workload access token: "+err.Error())
		return
	}

	out, err := a.client.GetResourceOauth2Token(r.Context(), &bedrockagentcore.GetResourceOauth2TokenInput{
		WorkloadIdentityToken:          aws.String(workloadToken),
		ResourceCredentialProviderName: aws.String(a.cfg.CredentialProviderID),
		Scopes:                         a.cfg.Scopes,
		Oauth2Flow:                     types.Oauth2FlowTypeUserFederation,
		ResourceOauth2ReturnUrl:        aws.String(a.cfg.ReturnURL),
	})
	if err != nil {
		jsonMgmtError(w, http.StatusBadGateway, "GetResourceOauth2Token failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"got_access_token":  out.AccessToken != nil && *out.AccessToken != "",
		"got_authorization_url": out.AuthorizationUrl != nil,
		"session_status":    string(out.SessionStatus),
	}
	a.logf("DEBUG_FORCE_REFRESH got_access_token=%v got_authorization_url=%v session_status=%s",
		resp["got_access_token"], resp["got_authorization_url"], out.SessionStatus)
	jsonMgmtOK(w, resp)
}
```

- [ ] **Step 2: 注册路由**

在 `main.go` 的 `mux` 设置里加一行：

```go
	mux.HandleFunc("/_sidecar/debug/force-refresh", mgmt.handleDebugForceRefresh)
```

- [ ] **Step 3: 编译**

```bash
cd ~/Documents/git/lark-cli
go build -tags authsidecar_agentcore_identity -o /tmp/sidecar-agentcore-identity ./sidecar/server-agentcore-identity/
```

Expected: 编译成功。

- [ ] **Step 4: 跑一次验证（紧接 Task 4 完成首次授权之后进行，此时 Vault 里已有 token）**

```bash
export AWS_PROFILE=panlm
export AWS_REGION=us-east-1
/tmp/sidecar-agentcore-identity --listen 127.0.0.1:16385 --log-file /tmp/sidecar-agentcore-identity.log &
sleep 1
curl -sS -X POST http://127.0.0.1:16385/_sidecar/debug/force-refresh | python3 -m json.tool
kill %1
```

Expected: 返回 `{"got_access_token": true, "got_authorization_url": false, "ok": true, "session_status": "..."}`。`got_access_token: true` 且 `got_authorization_url: false` 就证明了：即使本地缓存被清空（模拟"access token 已过期需要重新拿"的场景），AgentCore Identity 依然靠它服务端存的 refresh token 直接给出了新的有效 access token，而不需要重新走浏览器 3LO 授权——这正是设计文档里悬而未决的"refresh token 自动续期是否真生效"问题的答案。

- [ ] **Step 5: 记录验证结果到设计文档的「未决问题」章节**

编辑 `superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md`，把「未决问题」第 1 条改成已解决状态，追加验证结果（验证时间、`session_status` 实际返回值）。

- [ ] **Step 6: Commit**

```bash
cd ~/Documents/git/lark-cli
git add sidecar/server-agentcore-identity/ superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md
git commit -m "feat(sidecar): add force-refresh debug endpoint; confirm AgentCore Identity refresh-token path works"
```

---

## Task 6: lark-cli 编译 + 手动跑通一次真实命令（通过 sidecar，不经 MCP Wrapper）

**Files:** 无新文件（`lark-cli` 本体不改代码，只是用不同 build tag 编译）。

**Interfaces:** 无新增（消费 Task 1-5 已跑通的 sidecar）。

- [ ] **Step 1: 编译带 authsidecar tag 的 lark-cli**

```bash
cd ~/Documents/git/lark-cli
go build -tags authsidecar -o /tmp/lark-cli-sandbox .
```

Expected: 编译成功。

- [ ] **Step 2: 确认 lark-cli 已有 app 配置（config init，走真实 App ID，不需要 App Secret 因为 sidecar 模式下 AppSecret 是 `NoAppSecret` 占位）**

先确认要用哪个 App ID——复用之前 `~/.mcp.json` 里飞书自建应用的 App ID（同一个已经建好 provider 的应用）：

```bash
/tmp/lark-cli-sandbox config init --app-id cli_a93bd889cfb91cc9 --brand feishu --non-interactive 2>&1 || \
/tmp/lark-cli-sandbox config init
```

（如果 `config init` 要求交互式输入且没有 `--non-interactive`/`--app-id` 这类 flag，改用交互模式手动跑一次，只填 App ID，App Secret 留空/跳过——sidecar 模式下 lark-cli 侧不需要真实 secret，具体交互提示以运行时实际输出为准，照着填即可）

- [ ] **Step 3: 启动 sidecar server（如果 Task 5 验证后已经停掉了）**

```bash
export AWS_PROFILE=panlm
export AWS_REGION=us-east-1
/tmp/sidecar-agentcore-identity --listen 127.0.0.1:16385 --log-file /tmp/sidecar-agentcore-identity.log &
sleep 1
```

- [ ] **Step 4: 用 sidecar 环境变量跑一个真实 lark-cli 命令**

```bash
export LARKSUITE_CLI_AUTH_PROXY="http://127.0.0.1:16385"
export LARKSUITE_CLI_PROXY_KEY="$(cat ~/.lark-sidecar-agentcore/proxy.key)"
export LARKSUITE_CLI_APP_ID="cli_a93bd889cfb91cc9"
export LARKSUITE_CLI_BRAND="feishu"
export LARKSUITE_CLI_DEFAULT_AS="user"

/tmp/lark-cli-sandbox api GET /open-apis/authen/v1/user_info --as user
```

Expected: 返回真实飞书用户信息 JSON（`open_id`、`name` 字段，同 Task 4 Step 5 看到的结果）。这一步证明了 `lark-cli` 本体（未改一行代码，只是编译 tag 不同）通过 sidecar 拿到了 AgentCore Identity 管理的真实 token。

- [ ] **Step 5: 跑一个业务命令验证（发一条 IM 消息给自己）**

先查自己的 `open_id`（Step 4 输出里已经有），然后：

```bash
/tmp/lark-cli-sandbox im +messages-send --as user --receive-id "<Step 4 输出里的 open_id>" --receive-id-type open_id --text "AgentCore Identity sidecar 验证通过 $(date)"
```

Expected: 命令成功返回，飞书客户端上能看到这条自己发给自己的消息（人工确认一次）。

- [ ] **Step 6: 清理**

```bash
kill %1
```

本 Task 无代码改动，不提交。

---

## Task 7: MCP Wrapper — 项目骨架 + `lark_exec` 工具

**Files:**
- Create: `mcp-wrapper/go.mod`
- Create: `mcp-wrapper/main.go`
- Create: `mcp-wrapper/exec_tool.go`
- Create: `mcp-wrapper/exec_tool_test.go`

**Interfaces:**
- Produces: 一个独立 Go module（不属于 `larksuite/cli` 的 module，因为它跟 lark-cli 本体逻辑无关，是纯粹的协议适配层，独立 module 方便单独构建部署），监听 `127.0.0.1:8090`，讲 MCP Streamable HTTP 协议，暴露一个工具 `lark_exec`。
- Consumes: 系统上已编译好的 `lark-cli` 二进制路径（通过环境变量 `LARK_CLI_BIN_PATH` 指定，默认 `/tmp/lark-cli-sandbox`，跟 Task 6 编译产物一致），以及 sidecar 相关的环境变量（`LARKSUITE_CLI_AUTH_PROXY` 等，MCP Wrapper 自己不处理认证，只是把这些环境变量原样传给它 fork 出来的 `lark-cli` 子进程）。

先确认 MCP Go SDK 可用性：

- [ ] **Step 1: 检查官方 MCP Go SDK 是否存在并可安装**

```bash
mkdir -p ~/Documents/git/lark-cli/mcp-wrapper
cd ~/Documents/git/lark-cli/mcp-wrapper
go mod init github.com/panlm/lark-cli-mcp-wrapper
go get github.com/modelcontextprotocol/go-sdk/mcp@latest 2>&1
```

Expected 情况 A（SDK 存在）：`go get` 成功，`go.mod` 新增 require。继续 Step 2A。
Expected 情况 B（SDK 不存在或版本不兼容）：`go get` 报错（如 404 或版本解析失败）。此时改用降级方案 Step 2B：不依赖第三方 MCP SDK，手写最小 JSON-RPC 2.0 over HTTP（MCP Streamable HTTP transport 的核心就是 JSON-RPC 请求/响应，工具数量少、协议面窄，手写完全可控）。

**执行时先跑 Step 1 的命令，根据实际结果选择 Step 2A 或 Step 2B，不要凭空假设。**

- [ ] **Step 2A（如果官方 SDK 可用）: 用官方 SDK 写 main.go**

参考官方 SDK 文档里 server 端最小示例的写法（`mcp.NewServer` + `mcp.AddTool` + `Streamable` transport），把下面 Step 2B 里手写的 `lark_exec` 工具逻辑（`runLarkExec` 函数体不变）套进官方 SDK 的工具注册 API 里。具体 API 调用方式以 `go doc github.com/modelcontextprotocol/go-sdk/mcp` 实际输出为准，照官方 SDK 的工具注册签名改写 `main.go` 的注册部分，`exec_tool.go` 里的 `runLarkExec` 纯业务逻辑函数保持不变（下面 Step 2B 给出的 `exec_tool.go` 内容对两种方案都适用，是复用的）。

- [ ] **Step 2B（降级方案：手写 JSON-RPC，SDK 不可用时执行）: 写 exec_tool.go（业务逻辑，两种方案通用）**

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// larkCLIBinPath returns the path to the lark-cli binary this wrapper shells
// out to. Configurable via LARK_CLI_BIN_PATH so the same wrapper binary can
// point at different lark-cli builds without recompiling.
func larkCLIBinPath() string {
	if p := os.Getenv("LARK_CLI_BIN_PATH"); p != "" {
		return p
	}
	return "/tmp/lark-cli-sandbox"
}

// execResult is the structured outcome of running a lark-cli command,
// returned to the MCP caller as the tool result payload.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// splitCommandLine tokenizes a command line the way a shell would for the
// subset of syntax lark-cli commands need: whitespace separates words, and
// single or double quotes group a word that contains whitespace (e.g.
// --text "hello world"). Inside double quotes, a backslash escapes a
// following backslash or double-quote; single-quoted sections are fully
// literal (no escaping, matching POSIX shell single-quote semantics).
// Mismatched quotes return an error rather than silently truncating input,
// since a malformed --text value must not be silently misparsed.
func splitCommandLine(s string) ([]string, error) {
	var args []string
	var cur bytes.Buffer
	inWord := false
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case c == '"':
			inWord = true
			i++
			for i < n && s[i] != '"' {
				if s[i] == '\\' && i+1 < n && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated double quote in command line")
			}
			i++ // skip closing quote
		case c == '\'':
			inWord = true
			i++
			for i < n && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated single quote in command line")
			}
			i++ // skip closing quote
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// runLarkExec tokenizes argsLine with splitCommandLine (quote-aware, so
// arguments containing spaces — e.g. a --text value — must be wrapped in
// double or single quotes) and executes lark-cli with those arguments,
// inheriting the current process's environment (so sidecar env vars like
// LARKSUITE_CLI_AUTH_PROXY set on the wrapper process flow through to the
// lark-cli child process).
func runLarkExec(ctx context.Context, argsLine string) (execResult, error) {
	args, err := splitCommandLine(argsLine)
	if err != nil {
		return execResult{}, fmt.Errorf("invalid command line: %w", err)
	}
	if len(args) == 0 {
		return execResult{}, fmt.Errorf("empty command")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, larkCLIBinPath(), args...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		return execResult{}, fmt.Errorf("failed to run lark-cli: %w", runErr)
	}

	return execResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}
```

- [ ] **Step 3: 写 exec_tool_test.go（用一个假的可执行脚本代替真实 lark-cli，验证 runLarkExec 的参数拼接和输出捕获逻辑，不依赖真实飞书凭证）**

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeBinary creates a tiny shell script standing in for lark-cli, so
// this test exercises runLarkExec's argument-passing and output-capturing
// logic without needing a real lark-cli build or Feishu credentials.
func writeFakeBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
	return path
}

func TestRunLarkExec_CapturesStdout(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 arg2=$2"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	want := "arg1=hello arg2=world\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_CapturesNonZeroExit(t *testing.T) {
	bin := writeFakeBinary(t, `echo "boom" >&2; exit 3`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), "anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", result.ExitCode)
	}
	if result.Stderr != "boom\n" {
		t.Errorf("stderr = %q, want %q", result.Stderr, "boom\n")
	}
}

func TestRunLarkExec_RejectsEmptyCommand(t *testing.T) {
	_, err := runLarkExec(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestRunLarkExec_DoubleQuotedArgumentKeepsSpaces(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 arg2=$2 argc=$#"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), `--text "hello world" --flag`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "arg1=--text arg2=hello world argc=3\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_SingleQuotedArgumentKeepsSpaces(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 argc=$#"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), `'hello world'`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "arg1=hello world argc=1\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_RejectsUnterminatedQuote(t *testing.T) {
	_, err := runLarkExec(context.Background(), `--text "unterminated`)
	if err == nil {
		t.Fatal("expected error for unterminated quote, got nil")
	}
}
```

- [ ] **Step 4: 运行测试**

```bash
cd ~/Documents/git/lark-cli/mcp-wrapper
go test -v ./...
```

Expected: 三个测试全部 PASS。

- [ ] **Step 5: 写 main.go（取决于 Step 1 的结果选 2A 或 2B 对应的 server 搭建方式；如果走 2B 降级方案，用最小 JSON-RPC）**

2B 降级方案的 `main.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
)

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type larkExecArgs struct {
	Command string `json:"command"`
}

func main() {
	http.HandleFunc("/mcp", handleMCP)
	log.Println("MCP wrapper listening on :8090/mcp")
	if err := http.ListenAndServe("127.0.0.1:8090", nil); err != nil {
		log.Fatal(err)
	}
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo":      map[string]any{"name": "lark-cli-mcp-wrapper", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "lark_exec",
					"description": "Execute a lark-cli command and return its stdout/stderr/exit code. Pass the full command line as it would appear after 'lark-cli '. Wrap any argument containing spaces in double or single quotes, e.g. 'im +messages-send --as user --receive-id ou_xxx --receive-id-type open_id --text \"hello world\"'.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{"type": "string", "description": "Arguments to pass to lark-cli, space-separated"},
						},
						"required": []string{"command"},
					},
				},
			},
		})
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
			return
		}
		if params.Name != "lark_exec" {
			writeJSONRPCError(w, req.ID, -32601, "unknown tool: "+params.Name)
			return
		}
		var args larkExecArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeJSONRPCError(w, req.ID, -32602, "invalid tool arguments: "+err.Error())
			return
		}
		result, err := runLarkExec(context.Background(), args.Command)
		if err != nil {
			writeJSONRPCError(w, req.ID, -32000, err.Error())
			return
		}
		resultJSON, _ := json.Marshal(result)
		writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(resultJSON)},
			},
			"isError": result.ExitCode != 0,
		})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: message}})
}
```

- [ ] **Step 6: 编译**

```bash
cd ~/Documents/git/lark-cli/mcp-wrapper
go build -o /tmp/lark-mcp-wrapper .
```

Expected: 编译成功。

- [ ] **Step 7: 手动跑一次 MCP 协议往返，验证 tools/list 和 tools/call**

```bash
export LARK_CLI_BIN_PATH=/tmp/lark-cli-sandbox
export LARKSUITE_CLI_AUTH_PROXY="http://127.0.0.1:16385"
export LARKSUITE_CLI_PROXY_KEY="$(cat ~/.lark-sidecar-agentcore/proxy.key)"
export LARKSUITE_CLI_APP_ID="cli_a93bd889cfb91cc9"
export LARKSUITE_CLI_BRAND="feishu"
export LARKSUITE_CLI_DEFAULT_AS="user"

/tmp/lark-mcp-wrapper &
sleep 1

# tools/list
curl -sS http://127.0.0.1:8090/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | python3 -m json.tool

# tools/call — 确保 sidecar server 此时也在跑（Task 6 Step 3 那个进程，如果已停需重新启动）
curl -sS http://127.0.0.1:8090/mcp -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lark_exec","arguments":{"command":"api GET /open-apis/authen/v1/user_info --as user"}}}' | python3 -m json.tool

kill %1
```

Expected: `tools/list` 返回包含 `lark_exec` 的工具列表；`tools/call` 返回 `result.content[0].text` 里是一段 JSON 字符串，解开后能看到 `exit_code: 0` 和 `stdout` 里含真实飞书用户信息。

- [ ] **Step 8: Commit**

```bash
cd ~/Documents/git/lark-cli
git add mcp-wrapper/
git commit -m "feat(mcp-wrapper): scaffold MCP server exposing lark_exec tool"
```

---

## Task 8: MCP Wrapper — `lark_list_skills` 和 `lark_get_skill` 工具

**Files:**
- Modify: `mcp-wrapper/main.go`（`tools/list` 里增加两个工具定义，`tools/call` 里增加两个 case）
- Create: `mcp-wrapper/skill_tools.go`
- Create: `mcp-wrapper/skill_tools_test.go`

**Interfaces:**
- Consumes: `runLarkExec(ctx, argsLine string) (execResult, error)`（Task 7 已定义）
- Produces: `runLarkListSkills(ctx) (execResult, error)`、`runLarkGetSkill(ctx, skillName string) (execResult, error)`——两者都是对 `runLarkExec` 的薄封装，固定命令前缀。

先确认 `lark-cli` 真实的 skill 子命令语法（不要凭空猜测参数名）：

- [ ] **Step 1: 查证 lark-cli 的 skill 命令实际语法**

```bash
cd ~/Documents/git/lark-cli
grep -rn "\"skill\"" cmd/root.go cmd/skill/ 2>/dev/null | head -10
/tmp/lark-cli-sandbox skill --help 2>&1
```

根据实际输出确定 `list` 和 `show`/`get`（具体子命令名以 `--help` 真实输出为准）的准确调用方式，下面 Step 2 的代码里 `fmt.Sprintf` 的命令模板要替换成这一步查到的真实语法。

- [ ] **Step 2: 写 skill_tools.go（命令模板占位符 `<VERIFIED_SUBCOMMAND>` 替换为 Step 1 查到的真实子命令名）**

```go
package main

import (
	"context"
	"fmt"
)

// runLarkListSkills lists all available lark-cli Skills.
func runLarkListSkills(ctx context.Context) (execResult, error) {
	return runLarkExec(ctx, "skill <VERIFIED_SUBCOMMAND>")
}

// runLarkGetSkill fetches the detailed orchestration guide for one Skill by name.
func runLarkGetSkill(ctx context.Context, skillName string) (execResult, error) {
	if skillName == "" {
		return execResult{}, fmt.Errorf("skillName is required")
	}
	return runLarkExec(ctx, fmt.Sprintf("skill <VERIFIED_SUBCOMMAND> %s", skillName))
}
```

- [ ] **Step 3: 写 skill_tools_test.go（同样用假二进制，不依赖真实 lark-cli 语法细节——只验证参数拼接和错误处理，不验证真实 skill 命令本身）**

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFakeBinaryForSkills(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
	return path
}

func TestRunLarkListSkills_PassesThroughToExec(t *testing.T) {
	bin := writeFakeBinaryForSkills(t, `echo "got: $@"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkListSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestRunLarkGetSkill_RejectsEmptyName(t *testing.T) {
	_, err := runLarkGetSkill(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty skillName, got nil")
	}
}

func TestRunLarkGetSkill_IncludesNameInCommand(t *testing.T) {
	bin := writeFakeBinaryForSkills(t, `echo "got: $@"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkGetSkill(context.Background(), "calendar-scheduling")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if !contains(result.Stdout, "calendar-scheduling") {
		t.Errorf("stdout %q does not contain skill name", result.Stdout)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
```

- [ ] **Step 4: 运行测试（预期 TestRunLarkListSkills_PassesThroughToExec 和 TestRunLarkGetSkill_IncludesNameInCommand 会因为 Step 2 里的占位符 `<VERIFIED_SUBCOMMAND>` 被当作真实参数传给假二进制而依然 PASS，因为假二进制只是 echo 参数，不校验语义——这两个测试验证的是"参数被正确传递"而不是"lark-cli 真实语法"）**

```bash
cd ~/Documents/git/lark-cli/mcp-wrapper
go test -v ./...
```

Expected: 全部 PASS。

- [ ] **Step 5: 在 main.go 的 tools/list 里补充两个工具定义**

在 `handleMCP` 的 `case "tools/list":` 分支里，把 `tools` 数组从只有 `lark_exec` 一项，扩展成三项：

```go
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "lark_exec",
					"description": "Execute a lark-cli command and return its stdout/stderr/exit code. Pass the full command line as it would appear after 'lark-cli '. Wrap any argument containing spaces in double or single quotes, e.g. 'im +messages-send --as user --receive-id ou_xxx --receive-id-type open_id --text \"hello world\"'.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{"type": "string", "description": "Arguments to pass to lark-cli, space-separated"},
						},
						"required": []string{"command"},
					},
				},
				{
					"name":        "lark_list_skills",
					"description": "List all available lark-cli Skills (structured multi-step operation guides for common Feishu/Lark tasks like scheduling meetings or searching messages).",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
				{
					"name":        "lark_get_skill",
					"description": "Fetch the detailed orchestration guide for one Skill by name (parameter formats, call order, preconditions). Call lark_list_skills first to discover available names.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"skill_name": map[string]any{"type": "string", "description": "Name of the Skill, as returned by lark_list_skills"},
						},
						"required": []string{"skill_name"},
					},
				},
			},
		})
```

- [ ] **Step 6: 在 tools/call 里补充两个 case**

在 `handleMCP` 的 `case "tools/call":` 分支里，把 `if params.Name != "lark_exec"` 这段判断改成一个 switch，分派到三个工具：

```go
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
			return
		}

		var result execResult
		var runErr error

		switch params.Name {
		case "lark_exec":
			var args larkExecArgs
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				writeJSONRPCError(w, req.ID, -32602, "invalid tool arguments: "+err.Error())
				return
			}
			result, runErr = runLarkExec(context.Background(), args.Command)
		case "lark_list_skills":
			result, runErr = runLarkListSkills(context.Background())
		case "lark_get_skill":
			var args struct {
				SkillName string `json:"skill_name"`
			}
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				writeJSONRPCError(w, req.ID, -32602, "invalid tool arguments: "+err.Error())
				return
			}
			result, runErr = runLarkGetSkill(context.Background(), args.SkillName)
		default:
			writeJSONRPCError(w, req.ID, -32601, "unknown tool: "+params.Name)
			return
		}

		if runErr != nil {
			writeJSONRPCError(w, req.ID, -32000, runErr.Error())
			return
		}
		resultJSON, _ := json.Marshal(result)
		writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(resultJSON)},
			},
			"isError": result.ExitCode != 0,
		})
```

- [ ] **Step 7: 编译 + 手动验证三个工具都能通过 tools/list 看到**

```bash
cd ~/Documents/git/lark-cli/mcp-wrapper
go build -o /tmp/lark-mcp-wrapper .
/tmp/lark-mcp-wrapper &
sleep 1
curl -sS http://127.0.0.1:8090/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | python3 -c "import json,sys; d=json.load(sys.stdin); print([t['name'] for t in d['result']['tools']])"
kill %1
```

Expected: 输出 `['lark_exec', 'lark_list_skills', 'lark_get_skill']`。

- [ ] **Step 8: Commit**

```bash
cd ~/Documents/git/lark-cli
git add mcp-wrapper/
git commit -m "feat(mcp-wrapper): add lark_list_skills and lark_get_skill tools"
```

---

## Task 9: 端到端验收 — 三进程联调 + 接入 DevOps Agent

**Files:** 无新文件。本 Task 是最终验收，对齐设计文档「验收标准」章节。

**Interfaces:** 无新增，是前面所有 Task 产出的组装验证。

- [ ] **Step 1: 依次启动三个进程**

```bash
# Terminal/background 1: sidecar server
export AWS_PROFILE=panlm
export AWS_REGION=us-east-1
/tmp/sidecar-agentcore-identity --listen 127.0.0.1:16385 --log-file /tmp/sidecar-agentcore-identity.log &
sleep 1

# Terminal/background 2: MCP wrapper
export LARK_CLI_BIN_PATH=/tmp/lark-cli-sandbox
export LARKSUITE_CLI_AUTH_PROXY="http://127.0.0.1:16385"
export LARKSUITE_CLI_PROXY_KEY="$(cat ~/.lark-sidecar-agentcore/proxy.key)"
export LARKSUITE_CLI_APP_ID="cli_a93bd889cfb91cc9"
export LARKSUITE_CLI_BRAND="feishu"
export LARKSUITE_CLI_DEFAULT_AS="user"
/tmp/lark-mcp-wrapper &
sleep 1

curl -sS http://127.0.0.1:8090/mcp -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | python3 -m json.tool
```

Expected: 返回 `serverInfo.name: "lark-cli-mcp-wrapper"`，确认 MCP Wrapper 活着且能应答标准 MCP `initialize` 方法。

- [ ] **Step 2: 把 MCP Wrapper 加进 `~/.mcp.json`，作为本机 Claude Code 的一个新 MCP server（用于本地验证 MCP 协议兼容性，不是最终给 DevOps Agent 用的方式——DevOps Agent 走 AWS 官方"Connecting MCP Servers"注册流程，本 Task 用 Claude Code 作为验证用的 MCP client 更快）**

编辑 `~/.mcp.json`，在 `mcpServers` 里新增一项（先读取现有文件内容，用 Edit 工具在现有 JSON 结构基础上插入，不要整份重写）：

```json
    "lark-cli-agentcore-verify": {
      "type": "http",
      "url": "http://127.0.0.1:8090/mcp"
    }
```

- [ ] **Step 3: 重启 Claude Code 会话，确认新 MCP server 已连接**

重启后，用工具列表确认能看到 `lark_exec`、`lark_list_skills`、`lark_get_skill` 三个工具来自 `lark-cli-agentcore-verify` server。

- [ ] **Step 4: 通过这个 MCP 连接，实际调用一次 lark_exec 发送一条飞书消息**

在 Claude Code 对话里请求："用 lark_exec 工具，通过 lark-cli 给我自己发一条飞书消息，command 参数要按 `im +messages-send --as user --receive-id <open_id> --receive-id-type open_id --text \"MCP Wrapper end to end verified\"` 这样写（`--text` 的值必须用双引号包起来，因为里面有空格）"，观察工具调用结果。

Expected: 工具返回 `exit_code: 0`，飞书客户端上收到这条消息（人工确认）。

- [ ] **Step 5: 验证「至少一个飞书操作」这条验收标准的第二个例子——读取日历（如果 Step 4 已经用了发消息，这一步换成日历验证覆盖面更广）**

```bash
curl -sS http://127.0.0.1:8090/mcp -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lark_exec","arguments":{"command":"calendar +agenda --as user"}}}' | python3 -m json.tool
```

Expected: 返回 `exit_code: 0`，`stdout` 包含今日日程信息（即使是空日程，只要命令成功执行、没有认证错误，就算通过）。

- [ ] **Step 6: 清理验证用的 MCP server 配置（避免长期占用本机端口/进程）**

从 `~/.mcp.json` 里移除 Step 2 新增的 `lark-cli-agentcore-verify` 项（这是临时验证配置，不是最终交付形态——最终交付形态是部署到 AgentCore Runtime 后由 DevOps Agent 走官方注册流程连接，不在本次范围）。

停掉后台进程：

```bash
kill %1 %2
```

- [ ] **Step 7: 更新设计文档，把验收标准三条全部标记为已验证通过，附上验证日期**

编辑 `superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md` 的「验收标准」章节，在每条后面加 `✅ 已验证（<验证日期>）` 标注。

- [ ] **Step 8: Commit**

```bash
cd ~/Documents/git/lark-cli
git add superpowers-docs/AGENTCORE_IDENTITY_MCP_DESIGN.md
git commit -m "docs: mark acceptance criteria as verified end-to-end"
```

---

## Self-Review Notes（写计划时的自查记录）

- **Spec 覆盖检查**：设计文档「验收标准」三条 → Task 4/6（三进程本地跑通）、Task 9（DevOps Agent 场景验证，用 Claude Code 代替实际 DevOps Agent 走一遍协议兼容性）、Task 5（refresh 验证）全部覆盖。「未决问题」四条 → 第1条(refresh) Task 5 解决；第2条(session_id回传) Task 3 解决（管理端点方案）；第3条(MCP工具集) Task 7-8 解决（3个工具）；第4条(Docker化/部署) 明确不在本计划范围，未做任何相关 Task，符合设计文档的范围边界。
- **占位符扫描**：Task 8 Step 1 里的 `<VERIFIED_SUBCOMMAND>` 不是遗留占位符，是有意设计的「先查证真实语法再替换」步骤（因为 `lark-cli skill` 子命令的准确名称在写计划时未经验证，编造一个可能是错的，所以明确要求执行者先跑 `--help` 查证——这是唯一允许存在的"留白"，且给出了查证方法和替换指引，不是空洞的 TODO）。已在该 Task 内说明原因，不是本文档遗漏。
- **类型一致性检查**：`tokenResolver` 接口在 Task 1 定义为 `ResolveToken(ctx context.Context, identity string) (string, error)`，Task 2 的 `agentcoreTokenResolver.ResolveToken` 签名一致；`execResult` struct 在 Task 7 定义后，Task 8/9 均复用同一结构，字段名 `ExitCode`/`Stdout`/`Stderr` 全文一致；`runLarkExec` 签名 `(ctx context.Context, argsLine string) (execResult, error)` 在 Task 7 定义后被 Task 8 的 `runLarkListSkills`/`runLarkGetSkill` 正确复用。
