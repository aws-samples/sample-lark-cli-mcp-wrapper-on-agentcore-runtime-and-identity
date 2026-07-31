// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// stateTTL bounds how long a signed customState stays acceptable. AgentCore
// Identity expires each authorization URL and its session URI after 10 minutes,
// so anything older than that could not be redeemed anyway.
const stateTTL = 10 * time.Minute

// signState builds the opaque customState value passed to GetResourceOauth2Token
// and echoed back to the session-binding endpoint as a query parameter.
//
// AWS recommends this explicitly: "we highly recommend that you generate an
// opaque state to include in your API call to GetResourceOAuth2Token. Your
// application should be able to parse this value to ensure it's serving
// requests that were initiated by your agent application."
// (oauth2-authorization-url-session-binding.html)
//
// It matters more than usual here because the session-binding endpoint is a
// Lambda function URL with AuthType NONE — a browser redirect cannot carry
// SigV4, so the URL is reachable by anyone. A state that only this sidecar can
// mint is what lets that endpoint tell a real callback from a stray request.
//
// Format: v1.<unix-seconds>.<base64url HMAC-SHA256(secret, "v1." + seconds)>.
// The timestamp is inside the signed material, so it cannot be moved forward
// without the key.
func signState(secret []byte, now time.Time) string {
	payload := "v1." + strconv.FormatInt(now.Unix(), 10)
	return payload + "." + base64.RawURLEncoding.EncodeToString(hmacSHA256(secret, payload))
}

// verifyState checks a customState produced by signState: correct format, an
// HMAC that validates under secret, and a timestamp inside stateTTL of now.
func verifyState(secret []byte, state string, now time.Time) error {
	parts := strings.Split(state, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return fmt.Errorf("malformed state")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("malformed state signature")
	}
	// Constant-time compare, so a caller cannot recover the expected signature
	// byte by byte from timing differences.
	if !hmac.Equal(sig, hmacSHA256(secret, parts[0]+"."+parts[1])) {
		return fmt.Errorf("state signature mismatch")
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("malformed state timestamp")
	}
	age := now.Sub(time.Unix(issued, 0))
	if age < -time.Minute || age > stateTTL {
		return fmt.Errorf("state expired or not yet valid (age %s)", age.Round(time.Second))
	}
	return nil
}

func hmacSHA256(secret []byte, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// stateSecret returns the shared HMAC key used to sign customState, or nil when
// AGENTCORE_STATE_SECRET is unset. Nil disables state signing entirely, which
// is the right behaviour for the loopback callback flow: that listener is only
// reachable from inside the container, so it needs no CSRF defence. The Lambda
// session-binding deployment sets the same value on both sides.
func stateSecret() []byte {
	if v := os.Getenv("AGENTCORE_STATE_SECRET"); v != "" {
		return []byte(v)
	}
	return nil
}
