// Copyright (c) 2026 panlm
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
		h.logger.Printf("REJECT method=%s path=%s reason=%q", r.Method, sanitizePath(pathAndQuery), sanitizeError(err))
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
		h.logger.Printf("REJECT method=%s path=%s reason=%q", r.Method, sanitizePath(pathAndQuery), sanitizeError(err))
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
		h.logger.Printf("TOKEN_ERROR method=%s path=%s identity=%s error=%q", r.Method, sanitizePath(pathAndQuery), identity, sanitizeError(err))
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
		h.logger.Printf("FORWARD_ERROR method=%s path=%s error=%q", r.Method, sanitizePath(pathAndQuery), sanitizeError(err))
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
		r.Method, sanitizePath(pathAndQuery), identity, resp.StatusCode, time.Since(start).Round(time.Millisecond))
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
