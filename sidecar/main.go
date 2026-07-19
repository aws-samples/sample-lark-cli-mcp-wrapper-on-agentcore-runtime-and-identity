// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

// Command sidecar-agentcore-identity is the auth sidecar proxy that resolves
// real Feishu tokens from AWS Bedrock AgentCore Identity's Token Vault
// instead of the local OS keychain. See docs/AGENTCORE_IDENTITY_MCP_DESIGN.md and
// docs/AGENTCORE_IDENTITY_MCP_PLAN.md for the full design.
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

	"github.com/panlm/sample-lark-cli-mcp-wrapper-on-agentcore-runtime-and-identity/internal/larkendpoints"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:16385", "listen address (host:port)")
	callbackListen := flag.String("callback-listen", "127.0.0.1:3000", "listen address for the Feishu OAuth callback (must match agentcoreConfig.ReturnURL and the workload identity's allowedResourceOauth2ReturnUrls allowlist)")
	keyFile := flag.String("key-file", defaultKeyFile(), "path to write the HMAC key")
	logFile := flag.String("log-file", "", "audit log file (stderr if empty)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *listen, *callbackListen, *keyFile, *logFile); err != nil {
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

// validateLoopbackListen rejects any --listen address whose host is not
// loopback. The three management endpoints (auth/start, auth/complete,
// debug/force-refresh) carry no authentication of their own — they rely
// entirely on the sidecar being reachable only from the local machine. This
// is a hard rejection, not a warning: silently downgrading a non-loopback
// address would defeat the purpose of the check.
func validateLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid --listen address %q: %w", listen, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("invalid --listen address %q: host must be loopback "+
		"(127.0.0.1, ::1, or localhost) — the unauthenticated management "+
		"endpoints (/_sidecar/auth/*, /_sidecar/debug/*) rely on loopback-only "+
		"binding as their sole access control", listen)
}

func run(ctx context.Context, listen, callbackListen, keyFile, logFile string) error {
	if listen == "" {
		return fmt.Errorf("invalid --listen address: empty")
	}
	if err := validateLoopbackListen(listen); err != nil {
		return err
	}
	if callbackListen != "" {
		if err := validateLoopbackListen(callbackListen); err != nil {
			return err
		}
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
		larkendpoints.Resolve(larkendpoints.BrandFeishu),
		larkendpoints.Resolve(larkendpoints.BrandLark),
	)
	allowedIDs := buildAllowedIdentities()

	agentcoreCfg := defaultAgentcoreConfig()
	resolver, err := newAgentcoreTokenResolver(ctx, agentcoreCfg, auditLogger.Printf)
	if err != nil {
		return fmt.Errorf("failed to init AgentCore Identity resolver: %w", err)
	}

	handler := &proxyHandler{
		key:          []byte(keyHex),
		resolver:     resolver,
		logger:       auditLogger,
		forwardCl:    newForwardClient(),
		allowedHosts: allowedHosts,
		allowedIDs:   allowedIDs,
	}

	mgmt := &authMgmt{
		client:   resolver.client,
		cfg:      agentcoreCfg,
		resolver: resolver,
		logf:     auditLogger.Printf,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_sidecar/auth/start", mgmt.handleStart)
	mux.HandleFunc("/_sidecar/auth/complete", mgmt.handleComplete)
	mux.HandleFunc("/_sidecar/debug/force-refresh", mgmt.handleDebugForceRefresh)
	mux.Handle("/", handler) // everything else goes through the HMAC-verified proxy handler

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Dedicated callback listener on a separate port/mux, so its route isn't
	// reachable through the main listener (whose "/" catches everything else
	// via the HMAC-verified proxy handler) and its address can independently
	// match whatever URL is already on the workload identity's return-URL
	// allowlist without touching that AWS-side config.
	var callbackServer *http.Server
	var callbackListener net.Listener
	if callbackListen != "" {
		var err error
		callbackListener, err = net.Listen("tcp", callbackListen)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %v", callbackListen, err)
		}
		defer callbackListener.Close()

		callbackMux := http.NewServeMux()
		callbackMux.HandleFunc("/callback", mgmt.handleCallback)
		callbackServer = &http.Server{
			Handler:           callbackMux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
	}

	go func() {
		<-ctx.Done()
		auditLogger.Println("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			auditLogger.Printf("shutdown error: %v", err)
		}
		if callbackServer != nil {
			if err := callbackServer.Shutdown(shutdownCtx); err != nil {
				auditLogger.Printf("callback server shutdown error: %v", err)
			}
		}
	}()

	if callbackServer != nil {
		go func() {
			if err := callbackServer.Serve(callbackListener); err != nil && err != http.ErrServerClosed {
				auditLogger.Printf("callback server exited unexpectedly: %v", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "Feishu OAuth callback listening on http://%s/callback\n", callbackListen)
	}

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
