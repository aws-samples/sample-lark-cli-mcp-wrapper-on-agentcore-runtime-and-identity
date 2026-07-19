// Copyright (c) 2026 panlm
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
// is itself the trust boundary (see docs/AGENTCORE_IDENTITY_MCP_PLAN.md Task 3).
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
// authorization_url. ForceAuthentication is required here: the AWS reference
// sample (agentcore-samples/06-workshops/03-AgentCore-identity/
// 09-Outbound_Auth_Self_Hosted/agent.py) sets it true for exactly this
// call — omitting it was reproduced to make CompleteResourceTokenAuth fail
// with AccessDeniedException "Invalid or expired session" on every attempt
// for this session, regardless of how long the caller waits before retrying;
// the identical call with ForceAuthentication set succeeded on the first try.
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
		ForceAuthentication:            aws.Bool(true),
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

// handleCallback is the browser-facing counterpart of handleStart: it's the
// address configured as ResourceOauth2ReturnUrl, so the browser lands here
// automatically after the user completes Feishu login — no manual copy-paste
// of session_id out of the address bar. It extracts session_id from the query
// string, completes the binding via the same path as handleComplete, and
// renders a plain-text human-readable result instead of JSON (this endpoint
// is viewed in a browser, not called by a script).
func (a *authMgmt) handleCallback(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if sessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "missing session_id query parameter")
		return
	}
	if err := a.resolver.completeAuth(r.Context(), sessionID); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "authorization failed: %v\n", err)
		a.logf("AUTH_CALLBACK_FAILED session_id=%s error=%q", sessionID, err.Error())
		return
	}
	a.logf("AUTH_CALLBACK_OK session_id=%s", sessionID)
	fmt.Fprintln(w, "Feishu authorization complete. You can close this tab.")
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
		"got_access_token":      out.AccessToken != nil && *out.AccessToken != "",
		"got_authorization_url": out.AuthorizationUrl != nil,
		"session_status":        string(out.SessionStatus),
	}
	a.logf("DEBUG_FORCE_REFRESH got_access_token=%v got_authorization_url=%v session_status=%s",
		resp["got_access_token"], resp["got_authorization_url"], out.SessionStatus)
	jsonMgmtOK(w, resp)
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
