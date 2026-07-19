// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

// Package larkendpoints resolves the Feishu/Lark brand's API domains.
//
// This is a small, self-contained copy of the equivalent table in
// github.com/larksuite/cli/internal/core (an unexported package this
// module cannot import). The domain list is public, static configuration —
// not application logic — so vendoring it here avoids depending on
// lark-cli's internal package.
package larkendpoints

import "strings"

// Brand represents the Lark platform brand.
// "feishu" targets China-mainland, "lark" targets international.
type Brand string

const (
	BrandFeishu Brand = "feishu"
	BrandLark   Brand = "lark"
)

// ParseBrand normalizes a brand string (case-insensitive, whitespace-tolerant);
// anything other than "lark" normalizes to BrandFeishu.
func ParseBrand(value string) Brand {
	if strings.ToLower(strings.TrimSpace(value)) == "lark" {
		return BrandLark
	}
	return BrandFeishu
}

// Endpoints holds resolved endpoint URLs for different Lark services.
type Endpoints struct {
	Open     string // e.g. "https://open.feishu.cn"
	Accounts string // e.g. "https://accounts.feishu.cn"
	MCP      string // e.g. "https://mcp.feishu.cn"
	AppLink  string // e.g. "https://applink.feishu.cn"
}

// Resolve resolves endpoint URLs for the brand, normalizing its input so
// stored values with unusual casing still resolve correctly.
func Resolve(brand Brand) Endpoints {
	switch ParseBrand(string(brand)) {
	case BrandLark:
		return Endpoints{
			Open:     "https://open.larksuite.com",
			Accounts: "https://accounts.larksuite.com",
			MCP:      "https://mcp.larksuite.com",
			AppLink:  "https://applink.larksuite.com",
		}
	default:
		return Endpoints{
			Open:     "https://open.feishu.cn",
			Accounts: "https://accounts.feishu.cn",
			MCP:      "https://mcp.feishu.cn",
			AppLink:  "https://applink.feishu.cn",
		}
	}
}
