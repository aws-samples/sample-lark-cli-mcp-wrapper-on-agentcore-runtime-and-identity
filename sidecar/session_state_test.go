// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"strings"
	"testing"
	"time"
)

func TestSignState_RoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Unix(1785500000, 0)

	if err := verifyState(secret, signState(secret, now), now); err != nil {
		t.Fatalf("freshly signed state rejected: %v", err)
	}
}

// The signature is what stops a public session-binding endpoint from accepting
// requests it did not initiate, so a state signed under a different key must
// never validate.
func TestVerifyState_RejectsWrongSecret(t *testing.T) {
	now := time.Unix(1785500000, 0)
	state := signState([]byte("real-secret"), now)

	if err := verifyState([]byte("attacker-secret"), state, now); err == nil {
		t.Fatal("expected state signed with a different secret to be rejected")
	}
}

// The timestamp is inside the signed material, so an expired state cannot be
// refreshed by editing it — this checks the TTL is actually enforced.
func TestVerifyState_RejectsExpired(t *testing.T) {
	secret := []byte("test-secret")
	issued := time.Unix(1785500000, 0)
	state := signState(secret, issued)

	if err := verifyState(secret, state, issued.Add(stateTTL+time.Minute)); err == nil {
		t.Fatal("expected state older than stateTTL to be rejected")
	}
}

func TestVerifyState_RejectsFutureTimestamp(t *testing.T) {
	secret := []byte("test-secret")
	issued := time.Unix(1785500000, 0)
	state := signState(secret, issued)

	if err := verifyState(secret, state, issued.Add(-10*time.Minute)); err == nil {
		t.Fatal("expected state issued well in the future to be rejected")
	}
}

func TestVerifyState_RejectsMalformed(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Unix(1785500000, 0)
	valid := signState(secret, now)

	for name, state := range map[string]string{
		"empty":            "",
		"no separators":    "notastate",
		"too few parts":    "v1.1785500000",
		"wrong version":    strings.Replace(valid, "v1.", "v2.", 1),
		"bad base64":       "v1.1785500000.!!!not-base64!!!",
		"non-numeric time": "v1.abc." + strings.SplitN(valid, ".", 3)[2],
	} {
		if err := verifyState(secret, state, now); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// AGENTCORE_RETURN_URL is the single switch between the two deployment shapes:
// unset means the loopback listener and manual session_id entry, set means an
// external endpoint that completes the binding on its own. lark_auth_start's
// guidance keys off this, so getting it backwards would send the caller down
// the wrong path.
func TestSelfHostedSessionBinding(t *testing.T) {
	t.Run("unset falls back to loopback", func(t *testing.T) {
		t.Setenv("AGENTCORE_RETURN_URL", "")
		cfg := defaultAgentcoreConfig()
		if cfg.ReturnURL != defaultLoopbackReturnURL {
			t.Errorf("ReturnURL = %q, want %q", cfg.ReturnURL, defaultLoopbackReturnURL)
		}
		if selfHostedSessionBinding(cfg) {
			t.Error("loopback default should not report self-hosted session binding")
		}
	})

	t.Run("override reports self-hosted", func(t *testing.T) {
		t.Setenv("AGENTCORE_RETURN_URL", "https://example.lambda-url.us-east-1.on.aws/tok")
		cfg := defaultAgentcoreConfig()
		if !selfHostedSessionBinding(cfg) {
			t.Error("explicit return URL should report self-hosted session binding")
		}
	})
}

func TestStateSecret(t *testing.T) {
	t.Run("unset disables signing", func(t *testing.T) {
		t.Setenv("AGENTCORE_STATE_SECRET", "")
		if stateSecret() != nil {
			t.Error("expected nil secret when AGENTCORE_STATE_SECRET is unset")
		}
	})

	t.Run("set enables signing", func(t *testing.T) {
		t.Setenv("AGENTCORE_STATE_SECRET", "s3cret")
		if got := string(stateSecret()); got != "s3cret" {
			t.Errorf("got %q, want %q", got, "s3cret")
		}
	})
}

func TestExtraScopes(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want []string
	}{
		"empty":            {"", nil},
		"single":           {"offline_access", []string{"offline_access"}},
		"comma separated":  {"offline_access,im:message", []string{"offline_access", "im:message"}},
		"space separated":  {"offline_access im:message", []string{"offline_access", "im:message"}},
		"surrounding junk": {" offline_access , ", []string{"offline_access"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCORE_EXTRA_SCOPES", tc.raw)
			got := extraScopes()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// offline_access is what makes AgentCore Identity's refresh path work, and it
// has to be requestable without rebuilding the image.
func TestDefaultConfig_ExtraScopesPrepended(t *testing.T) {
	t.Setenv("AGENTCORE_EXTRA_SCOPES", "offline_access")
	scopes := defaultAgentcoreConfig().Scopes

	if len(scopes) == 0 || scopes[0] != "offline_access" {
		t.Fatalf("expected offline_access first, got %v", scopes)
	}
	if len(scopes) != 8 {
		t.Errorf("expected 8 scopes (1 extra + 7 base), got %d: %v", len(scopes), scopes)
	}
}
