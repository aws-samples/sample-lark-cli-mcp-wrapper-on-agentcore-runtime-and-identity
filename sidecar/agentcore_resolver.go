// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// getenvOr returns the environment variable's value, or fallback if unset/empty.
func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// agentcoreConfig holds the fixed identifiers for this single-user deployment.
// These values correspond to resources already created via boto3 in prior
// manual verification (see docs/AGENTCORE_IDENTITY_MCP_DESIGN.md).
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
		Region:               getenvOr("AGENTCORE_REGION", "us-east-1"),
		WorkloadName:         getenvOr("AGENTCORE_WORKLOAD_NAME", "feishu-mcp-workload"),
		CredentialProviderID: getenvOr("AGENTCORE_CREDENTIAL_PROVIDER_ID", "feishu-oauth-provider"),
		// No per-request caller identity is currently available to key this
		// by (see docs/AGENTCORE_IDENTITY_MCP_DESIGN.md) — DevOps Agent calls
		// this MCP server with a bare service-level SigV4 identity, carrying
		// no end-user claim. "bot" is a deliberately generic default, not a
		// real person's name; override via AGENTCORE_USER_ID for any other
		// single-tenant deployment.
		UserID: getenvOr("AGENTCORE_USER_ID", "bot"),
		// Must match both the workload identity's allowedResourceOauth2ReturnUrls
		// allowlist (an AWS-side config already granted for this exact URL) and
		// the dedicated callback listener started in main.go (default
		// 127.0.0.1:3000): the browser is redirected here automatically after
		// Feishu login, so handleCallback can complete the binding without a
		// human copying session_id out of the address bar.
		// Plain HTTP to localhost only. This is a loopback redirect target on
		// the same machine, not a network hop; TLS would add no protection
		// against an on-machine attacker that plain HTTP doesn't already lack,
		// and the return-URL allowlist is an exact-match AWS-side config
		// already granted for this string.
		// nosemgrep: use-tls
		ReturnURL: "http://localhost:3000/callback",
		// Full scope set for Task 9's calendar/docs verification. Each
		// scope was independently confirmed to authorize cleanly (302 to
		// Feishu's login page, verified via direct curl against the
		// authorization_url) before being combined here. The earlier
		// single combined-request 400 seen while probing turned out to
		// be reproducible neither per-scope nor for the full 7-scope set
		// when re-tested individually and then together via curl — most
		// likely a transient/tooling artifact in that one probe, not a
		// property of any scope string. calendar:* scopes obtained from
		// `calendar +agenda`'s missing_scope error; docx:* scopes
		// obtained from `docs +create`'s missing_scope error.
		Scopes: []string{
			"im:message", "im:chat:readonly",
			"calendar:calendar:readonly", "calendar:calendar", "calendar:calendar.event:read",
			"docx:document", "docx:document:create",
		},
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
