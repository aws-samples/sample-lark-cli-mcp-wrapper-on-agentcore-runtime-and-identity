// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/larksuite/cli/sidecar"
)

// getenvOr returns the environment variable's value, or fallback if unset/empty.
func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultLoopbackReturnURL is the session-binding URL used when
// AGENTCORE_RETURN_URL is unset: the sidecar's own loopback callback listener
// (see main.go's --callback-listen). Reachable only from the same machine, so
// it completes the flow automatically during local development and falls back
// to manual session_id entry once deployed.
const defaultLoopbackReturnURL = "http://localhost:3000/callback"

// selfHostedSessionBinding reports whether this deployment has an external
// session-binding endpoint configured, i.e. whether the browser can be expected
// to land somewhere that completes the binding on its own. Drives the guidance
// lark_auth_start returns, so the caller is told to wait for the redirect
// rather than to collect a session_id (or the reverse) based on what is
// actually deployed.
func selfHostedSessionBinding(cfg agentcoreConfig) bool {
	return cfg.ReturnURL != defaultLoopbackReturnURL
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
	// BotScopes are requested for the app-only ("bot") token instead of
	// Scopes. They are a separate list because Feishu grants app-identity and
	// user-identity permissions independently in the developer console, and
	// because user-only scopes (offline_access) are meaningless for a token
	// that is minted from client credentials on every expiry.
	BotScopes []string
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
		// The OAuth2 "session binding URL" — the *second* redirect hop, where
		// AgentCore Identity sends the browser after it has exchanged Feishu's
		// authorization code. Whatever value is used must appear in the
		// workload identity's allowedResourceOauth2ReturnUrls allowlist.
		//
		// Not interchangeable with the credential provider's own callbackUrl
		// (the one registered as the Feishu app's redirect URL). That is the
		// *first* hop's entry point: it expects Feishu to arrive carrying
		// authorizationCode and state, and rejects a bare browser visit with
		// "2 validation errors detected: Value at 'authorizationCode' failed to
		// satisfy constraint: Member must not be null; Value at 'state' failed
		// to satisfy constraint: Member must not be null". Verified 2026-07-31
		// by pointing ReturnURL at it — the API accepts the URL once it is
		// allowlisted, so the mistake only surfaces when a human actually
		// completes a login.
		//
		// The default is the loopback listener started in main.go, which only
		// works when the browser and the sidecar share a machine — true for
		// local development, false on AgentCore Runtime. Deployments there set
		// AGENTCORE_RETURN_URL to a publicly reachable endpoint that calls
		// CompleteResourceTokenAuth itself; see deploy/session-binding/.
		// Without it the flow still completes, just manually: the redirect
		// fails to load and the operator hands session_id to
		// lark_auth_complete. AWS documents this split — on Runtime "your web
		// application ... must host a publicly accessible HTTPS callback
		// endpoint itself", while the local CLI "hosts the callback endpoint
		// and calls the CompleteResourceTokenAuth API on your behalf".
		//
		// Plain HTTP is safe for the loopback default specifically: it is a
		// same-machine redirect target, not a network hop, so TLS would add no
		// protection an on-machine attacker doesn't already have. Any override
		// is expected to be HTTPS.
		// nosemgrep: use-tls
		ReturnURL: getenvOr("AGENTCORE_RETURN_URL", defaultLoopbackReturnURL),
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
		//
		// Deliberately excludes offline_access, which is what would make
		// AgentCore Identity's refresh path work: Feishu only returns a
		// refresh_token when the app has that permission enabled in the
		// developer console AND declares it in scope (see
		// https://open.feishu.cn/document/authentication-management/
		// access-token/refresh-user-access-token). Without it every token is a
		// bare 2-hour one and re-authorization is needed after it expires,
		// which is what the audit log showed for 07-19 through 07-30. Left out
		// for now on purpose, to keep exercising the interactive flow while the
		// session-binding path is still being validated; add it back once that
		// is settled. Set AGENTCORE_EXTRA_SCOPES=offline_access to opt in
		// without a rebuild.
		Scopes:    append(extraScopes(), baseScopes()...),
		BotScopes: botScopes(),
	}
}

// baseScopes is the permission set both identities start from. Returned fresh
// on every call so callers can append to it without sharing a backing array.
func baseScopes() []string {
	return []string{
		"im:message", "im:chat:readonly",
		"calendar:calendar:readonly", "calendar:calendar", "calendar:calendar.event:read",
		"docx:document", "docx:document:create",
	}
}

// botScopes returns the scopes requested for the app-only token. Defaults to
// baseScopes without AGENTCORE_EXTRA_SCOPES applied: offline_access, the scope
// that env var exists for, only affects the 3LO refresh path and is rejected
// as unknown authority for a client-credentials grant. Override the whole list
// with AGENTCORE_BOT_SCOPES (comma or space separated) when the app's
// app-identity permissions differ from its user-identity ones — which is the
// common case, since the developer console grants the two independently.
func botScopes() []string {
	if raw := os.Getenv("AGENTCORE_BOT_SCOPES"); raw != "" {
		return splitScopes(raw)
	}
	return baseScopes()
}

// extraScopes returns the scopes named in AGENTCORE_EXTRA_SCOPES (comma or
// space separated), so a deployment can request additional Feishu permissions
// — offline_access in particular — without rebuilding the image.
func extraScopes() []string {
	return splitScopes(os.Getenv("AGENTCORE_EXTRA_SCOPES"))
}

// splitScopes parses a comma or space separated scope list, dropping empties.
func splitScopes(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// agentcoreIdentityAPI is the slice of AgentCore Identity this resolver calls,
// named as an interface so tests can assert which OAuth2 flow and scopes each
// wire-protocol identity resolves with without reaching AWS.
// *bedrockagentcore.Client satisfies it.
type agentcoreIdentityAPI interface {
	GetWorkloadAccessTokenForUserId(context.Context, *bedrockagentcore.GetWorkloadAccessTokenForUserIdInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetWorkloadAccessTokenForUserIdOutput, error)
	GetResourceOauth2Token(context.Context, *bedrockagentcore.GetResourceOauth2TokenInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetResourceOauth2TokenOutput, error)
	CompleteResourceTokenAuth(context.Context, *bedrockagentcore.CompleteResourceTokenAuthInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CompleteResourceTokenAuthOutput, error)
}

// agentcoreTokenResolver resolves real Feishu tokens via AWS Bedrock
// AgentCore Identity's Token Vault instead of the local OS keychain.
//
// Identity mapping: both sidecar wire-protocol identities go through the Token
// Vault, each with its own cache slot and its own OAuth2 flow.
//
//   - "user" resolves a user token over USER_FEDERATION (3LO). Authority is one
//     specific person's, granted in a browser, and expiry sends the operator
//     back to that browser unless offline_access is in scope.
//   - "bot" resolves an app-only token over M2M, the client-credentials grant,
//     using the client_id/client_secret already held in the credential
//     provider. No browser, no user consent, renewable unattended.
//
// The two are not interchangeable. The bot token carries tenant-wide app
// authority rather than a person's, so any caller able to reach this sidecar
// acts as the app. It exists because Feishu gates some surfaces on app
// identity: user tokens are refused on external (cross-tenant) chats with
// error 230027, while the same send over an app-only token succeeds.
//
// Earlier revisions rejected "bot" outright, on the reasoning that a tenant
// token is minted from AppID+AppSecret and so has no business in a vault built
// for 3LO. That turned out to be wrong on the mechanics: Feishu's OAuth2 token
// endpoint accepts grant_type=client_credentials, so AgentCore Identity can
// mint the app-only token itself and the app secret never has to leave the
// vault.
type agentcoreTokenResolver struct {
	client agentcoreIdentityAPI
	cfg    agentcoreConfig

	mu    sync.RWMutex
	cache map[string]cachedToken // key: identity ("user" or "bot")

	logf func(format string, args ...any)
}

func newAgentcoreTokenResolver(ctx context.Context, cfg agentcoreConfig, logf func(string, ...any)) (*agentcoreTokenResolver, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	logf("RETURN_URL url=%s", cfg.ReturnURL)
	return &agentcoreTokenResolver{
		client: bedrockagentcore.NewFromConfig(awsCfg),
		cfg:    cfg,
		cache:  make(map[string]cachedToken),
		logf:   logf,
	}, nil
}

// ResolveToken implements tokenResolver. identity is "user" or "bot" from the
// sidecar wire protocol; anything else is rejected rather than guessed at.
func (r *agentcoreTokenResolver) ResolveToken(ctx context.Context, identity string) (string, error) {
	switch identity {
	case sidecar.IdentityUser:
		return r.resolveUserToken(ctx)
	case sidecar.IdentityBot:
		return r.resolveBotToken(ctx)
	default:
		return "", fmt.Errorf("agentcoreTokenResolver: identity %q not supported (want %q or %q)", identity, sidecar.IdentityUser, sidecar.IdentityBot)
	}
}

// cachedToken returns the live cached token for identity, if any.
func (r *agentcoreTokenResolver) cachedToken(identity string) (string, bool) {
	r.mu.RLock()
	cached, ok := r.cache[identity]
	r.mu.RUnlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.token, true
	}
	return "", false
}

// storeToken caches a token for identity, expiring it `early` before its real
// lifetime runs out so an in-flight request cannot race the expiry.
func (r *agentcoreTokenResolver) storeToken(identity, token string, lifetime, early time.Duration) time.Time {
	expiresAt := time.Now().Add(lifetime - early)
	r.mu.Lock()
	r.cache[identity] = cachedToken{token: token, expiresAt: expiresAt}
	r.mu.Unlock()
	return expiresAt
}

// resolveBotToken mints an app-only token over the M2M (client-credentials)
// grant. AgentCore Identity holds the client_id/client_secret, so no app secret
// passes through this process. No return URL is sent: nothing here is
// interactive, and an unauthorized caller gets an error rather than a login URL.
func (r *agentcoreTokenResolver) resolveBotToken(ctx context.Context) (string, error) {
	if token, ok := r.cachedToken(sidecar.IdentityBot); ok {
		return token, nil
	}

	workloadToken, err := r.getWorkloadAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get workload access token: %w", err)
	}

	out, err := r.client.GetResourceOauth2Token(ctx, &bedrockagentcore.GetResourceOauth2TokenInput{
		WorkloadIdentityToken:          aws.String(workloadToken),
		ResourceCredentialProviderName: aws.String(r.cfg.CredentialProviderID),
		Scopes:                         r.cfg.BotScopes,
		Oauth2Flow:                     types.Oauth2FlowTypeM2m,
	})
	if err != nil {
		return "", fmt.Errorf("GetResourceOauth2Token (M2M) failed: %w", err)
	}
	if out.AccessToken == nil || *out.AccessToken == "" {
		// There is no authorization-URL fallback for M2M: a failure here means
		// the app lacks the requested app-identity permissions, or the
		// permission change was never published to a new app version.
		return "", fmt.Errorf("GetResourceOauth2Token (M2M) returned no AccessToken; check that the app grants %v under app identity and that a version was published", r.cfg.BotScopes)
	}

	// Feishu app-only tokens live 2 hours, same as user tokens.
	expiresAt := r.storeToken(sidecar.IdentityBot, *out.AccessToken, 2*time.Hour, 5*time.Minute)
	r.logf("TOKEN_RESOLVED identity=%s flow=M2M expires_at=%s", sidecar.IdentityBot, expiresAt.Format(time.RFC3339))
	return *out.AccessToken, nil
}

// resolveUserToken resolves the 3LO user token, driving the browser
// authorization flow when the vault has nothing valid stored.
func (r *agentcoreTokenResolver) resolveUserToken(ctx context.Context) (string, error) {
	if token, ok := r.cachedToken(sidecar.IdentityUser); ok {
		return token, nil
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
	expiresAt := r.storeToken(sidecar.IdentityUser, *out.AccessToken, 2*time.Hour, 5*time.Minute)
	r.logf("TOKEN_RESOLVED identity=%s flow=USER_FEDERATION expires_at=%s", sidecar.IdentityUser, expiresAt.Format(time.RFC3339))
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
	// Only the user slot: completing a 3LO authorization says nothing about
	// the app-only token, which is minted from client credentials.
	r.mu.Lock()
	delete(r.cache, sidecar.IdentityUser)
	r.mu.Unlock()
	return nil
}

// forceExpire evicts every cached token immediately, forcing the next
// ResolveToken call to hit AgentCore Identity. Used by Task 5's refresh
// verification (does NOT delete the underlying stored token/refresh_token
// in the Token Vault — only this process's local cache — so the next call
// exercises the exact same refresh-or-reauth decision AgentCore Identity
// would make after natural 2-hour expiry).
//
// Clears both identities: a check meant to force re-resolution that quietly
// left a stale app-only token in place would report on the user flow while
// bot traffic kept serving from cache.
func (r *agentcoreTokenResolver) forceExpire() {
	r.mu.Lock()
	clear(r.cache)
	r.mu.Unlock()
}
