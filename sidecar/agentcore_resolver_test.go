// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// fakeIdentityAPI records what the resolver asked AgentCore Identity for, so
// tests can assert on the flow and scopes each identity resolves with without
// reaching AWS.
type fakeIdentityAPI struct {
	accessToken string
	// tokenErr, when set, is returned by GetResourceOauth2Token instead of a token.
	tokenErr error

	tokenCalls []*bedrockagentcore.GetResourceOauth2TokenInput
}

func (f *fakeIdentityAPI) GetWorkloadAccessTokenForUserId(context.Context, *bedrockagentcore.GetWorkloadAccessTokenForUserIdInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetWorkloadAccessTokenForUserIdOutput, error) {
	return &bedrockagentcore.GetWorkloadAccessTokenForUserIdOutput{
		WorkloadAccessToken: aws.String("fake-workload-token"),
	}, nil
}

func (f *fakeIdentityAPI) GetResourceOauth2Token(_ context.Context, in *bedrockagentcore.GetResourceOauth2TokenInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetResourceOauth2TokenOutput, error) {
	f.tokenCalls = append(f.tokenCalls, in)
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return &bedrockagentcore.GetResourceOauth2TokenOutput{
		AccessToken: aws.String(f.accessToken),
	}, nil
}

func (f *fakeIdentityAPI) CompleteResourceTokenAuth(context.Context, *bedrockagentcore.CompleteResourceTokenAuthInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CompleteResourceTokenAuthOutput, error) {
	return &bedrockagentcore.CompleteResourceTokenAuthOutput{}, nil
}

func newTestResolver(fake *fakeIdentityAPI) *agentcoreTokenResolver {
	return &agentcoreTokenResolver{
		client: fake,
		cfg:    defaultAgentcoreConfig(),
		cache:  make(map[string]cachedToken),
		logf:   func(string, ...any) {},
	}
}

func TestAgentcoreTokenResolver_CacheHit(t *testing.T) {
	r := newTestResolver(&fakeIdentityAPI{})
	r.cache["user"] = cachedToken{token: "cached-token-value", expiresAt: time.Now().Add(time.Hour)}

	got, err := r.ResolveToken(t.Context(), "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cached-token-value" {
		t.Errorf("got %q, want %q", got, "cached-token-value")
	}
}

func TestAgentcoreTokenResolver_RejectsUnknownIdentity(t *testing.T) {
	r := newTestResolver(&fakeIdentityAPI{})

	if _, err := r.ResolveToken(t.Context(), "admin"); err == nil {
		t.Fatal("expected error for unknown identity, got nil")
	}
}

// The user path must keep using the 3LO browser flow, and must send the
// return URL that the session-binding endpoint is registered under.
func TestAgentcoreTokenResolver_UserUsesUserFederationFlow(t *testing.T) {
	// Force the two scope lists apart, so asserting on one of them actually
	// distinguishes it from the other. Left at their defaults they are equal,
	// and the assertion below would hold even if the user path sent BotScopes.
	t.Setenv("AGENTCORE_EXTRA_SCOPES", "offline_access")

	fake := &fakeIdentityAPI{accessToken: "user-token"}
	r := newTestResolver(fake)
	if slices.Equal(r.cfg.Scopes, r.cfg.BotScopes) {
		t.Fatal("test premise broken: Scopes and BotScopes must differ here")
	}

	got, err := r.ResolveToken(t.Context(), "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "user-token" {
		t.Errorf("token = %q, want %q", got, "user-token")
	}
	if len(fake.tokenCalls) != 1 {
		t.Fatalf("GetResourceOauth2Token called %d times, want 1", len(fake.tokenCalls))
	}
	call := fake.tokenCalls[0]
	if call.Oauth2Flow != types.Oauth2FlowTypeUserFederation {
		t.Errorf("flow = %q, want %q", call.Oauth2Flow, types.Oauth2FlowTypeUserFederation)
	}
	if aws.ToString(call.ResourceOauth2ReturnUrl) != r.cfg.ReturnURL {
		t.Errorf("return url = %q, want %q", aws.ToString(call.ResourceOauth2ReturnUrl), r.cfg.ReturnURL)
	}
	if !slices.Equal(call.Scopes, r.cfg.Scopes) {
		t.Errorf("scopes = %v, want %v", call.Scopes, r.cfg.Scopes)
	}
}

// The bot path mints an app-only token via the M2M (client credentials) flow.
// It carries no return URL, because no browser is involved.
func TestAgentcoreTokenResolver_BotUsesM2MFlowAndBotScopes(t *testing.T) {
	// Same reason as the user test: keep the two lists distinguishable, or
	// sending Scopes instead of BotScopes would pass unnoticed.
	t.Setenv("AGENTCORE_BOT_SCOPES", "im:message")

	fake := &fakeIdentityAPI{accessToken: "bot-token"}
	r := newTestResolver(fake)
	if slices.Equal(r.cfg.Scopes, r.cfg.BotScopes) {
		t.Fatal("test premise broken: Scopes and BotScopes must differ here")
	}

	got, err := r.ResolveToken(t.Context(), "bot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "bot-token" {
		t.Errorf("token = %q, want %q", got, "bot-token")
	}
	if len(fake.tokenCalls) != 1 {
		t.Fatalf("GetResourceOauth2Token called %d times, want 1", len(fake.tokenCalls))
	}
	call := fake.tokenCalls[0]
	if call.Oauth2Flow != types.Oauth2FlowTypeM2m {
		t.Errorf("flow = %q, want %q", call.Oauth2Flow, types.Oauth2FlowTypeM2m)
	}
	if call.ResourceOauth2ReturnUrl != nil {
		t.Errorf("return url = %q, want nil for the M2M flow", aws.ToString(call.ResourceOauth2ReturnUrl))
	}
	if !slices.Equal(call.Scopes, r.cfg.BotScopes) {
		t.Errorf("scopes = %v, want %v", call.Scopes, r.cfg.BotScopes)
	}
}

// When AgentCore rejects the M2M call, the error must keep the original cause
// and point at the two usual culprits: a bot scope the app has not granted under
// app identity, and a stale client secret. AgentCore's own message ("Error
// parsing ClientCredentials response") names neither.
func TestAgentcoreTokenResolver_BotM2MFailureNamesScopesAndSecret(t *testing.T) {
	t.Setenv("AGENTCORE_BOT_SCOPES", "im:message,calendar:calendar")
	cause := errors.New("ValidationException: Error parsing ClientCredentials response")
	r := newTestResolver(&fakeIdentityAPI{tokenErr: cause})

	_, err := r.ResolveToken(t.Context(), "bot")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %q does not wrap the AgentCore cause", err)
	}
	for _, want := range []string{"[im:message calendar:calendar]", "app identity", "AGENTCORE_BOT_SCOPES", "client secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if _, ok := r.cachedToken("bot"); ok {
		t.Error("a failed M2M call must not leave a cached bot token")
	}
}

// A cached user token must never satisfy a bot request: the two identities
// carry different authority and are keyed separately in the vault.
func TestAgentcoreTokenResolver_BotCacheSeparateFromUser(t *testing.T) {
	fake := &fakeIdentityAPI{accessToken: "bot-token"}
	r := newTestResolver(fake)
	r.cache["user"] = cachedToken{token: "user-token", expiresAt: time.Now().Add(time.Hour)}

	got, err := r.ResolveToken(t.Context(), "bot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "user-token" {
		t.Fatal("bot request was served the cached user token")
	}
	if got != "bot-token" {
		t.Errorf("token = %q, want %q", got, "bot-token")
	}
	if len(fake.tokenCalls) != 1 {
		t.Errorf("GetResourceOauth2Token called %d times, want 1", len(fake.tokenCalls))
	}
}

func TestAgentcoreTokenResolver_BotCacheHit(t *testing.T) {
	fake := &fakeIdentityAPI{accessToken: "freshly-minted"}
	r := newTestResolver(fake)
	r.cache["bot"] = cachedToken{token: "cached-bot-token", expiresAt: time.Now().Add(time.Hour)}

	got, err := r.ResolveToken(t.Context(), "bot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cached-bot-token" {
		t.Errorf("token = %q, want %q", got, "cached-bot-token")
	}
	if len(fake.tokenCalls) != 0 {
		t.Errorf("GetResourceOauth2Token called %d times on a cache hit, want 0", len(fake.tokenCalls))
	}
}

// forceExpire backs the refresh-verification endpoint. With two identities
// cached it must clear both, or a stale bot token survives a check meant to
// force re-resolution.
func TestAgentcoreTokenResolver_ForceExpire(t *testing.T) {
	r := newTestResolver(&fakeIdentityAPI{})
	r.cache["user"] = cachedToken{token: "will-be-evicted", expiresAt: time.Now().Add(time.Hour)}
	r.cache["bot"] = cachedToken{token: "also-evicted", expiresAt: time.Now().Add(time.Hour)}
	r.forceExpire()

	r.mu.RLock()
	remaining := len(r.cache)
	r.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("%d cache entries survived forceExpire, want 0", remaining)
	}
}

// offline_access buys a refresh token for the 3LO user flow. It is meaningless
// for app-only tokens, so AGENTCORE_EXTRA_SCOPES must not leak into BotScopes.
func TestBotScopesExcludeExtraUserScopes(t *testing.T) {
	t.Setenv("AGENTCORE_EXTRA_SCOPES", "offline_access")
	cfg := defaultAgentcoreConfig()

	if !slices.Contains(cfg.Scopes, "offline_access") {
		t.Fatal("user Scopes should include offline_access from AGENTCORE_EXTRA_SCOPES")
	}
	if slices.Contains(cfg.BotScopes, "offline_access") {
		t.Errorf("BotScopes = %v, must not contain offline_access", cfg.BotScopes)
	}
}

// Unset AGENTCORE_BOT_SCOPES requests no explicit scope, so Feishu issues a token
// with whatever the app holds under app identity instead of failing on a
// user-identity default the console never granted. The list must still be
// non-nil: the SDK rejects a GetResourceOauth2Token call with nil Scopes.
func TestBotScopesDefaultEmptyNonNil(t *testing.T) {
	for _, raw := range []string{"", " , "} {
		t.Setenv("AGENTCORE_BOT_SCOPES", raw)
		got := defaultAgentcoreConfig().BotScopes
		if got == nil || len(got) != 0 {
			t.Errorf("AGENTCORE_BOT_SCOPES=%q: BotScopes = %#v, want non-nil empty", raw, got)
		}
	}
}

func TestAgentcoreTokenResolver_BotDefaultSendsEmptyScopes(t *testing.T) {
	t.Setenv("AGENTCORE_BOT_SCOPES", "")
	fake := &fakeIdentityAPI{accessToken: "bot-token"}
	r := newTestResolver(fake)

	if _, err := r.ResolveToken(t.Context(), "bot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.tokenCalls) != 1 {
		t.Fatalf("GetResourceOauth2Token called %d times, want 1", len(fake.tokenCalls))
	}
	if s := fake.tokenCalls[0].Scopes; s == nil || len(s) != 0 {
		t.Errorf("scopes = %#v, want non-nil empty", s)
	}
}

func TestBotScopesOverridableByEnv(t *testing.T) {
	t.Setenv("AGENTCORE_BOT_SCOPES", "im:message, contact:user.id:readonly")
	cfg := defaultAgentcoreConfig()

	want := []string{"im:message", "contact:user.id:readonly"}
	if !slices.Equal(cfg.BotScopes, want) {
		t.Errorf("BotScopes = %v, want %v", cfg.BotScopes, want)
	}
}
