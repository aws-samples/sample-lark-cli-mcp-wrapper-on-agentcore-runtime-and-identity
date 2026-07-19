// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"strings"

	"github.com/larksuite/cli/sidecar"
	"github.com/panlm/sample-lark-cli-mcp-wrapper-on-agentcore-runtime-and-identity/internal/larkendpoints"
)

// buildAllowedHosts extracts the set of allowed target hostnames from
// multiple brand endpoints so the sidecar can serve both feishu and lark clients.
func buildAllowedHosts(endpoints ...larkendpoints.Endpoints) map[string]bool {
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
