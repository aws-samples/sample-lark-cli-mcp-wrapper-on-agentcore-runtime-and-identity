// Copyright (c) 2026 panlm
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
