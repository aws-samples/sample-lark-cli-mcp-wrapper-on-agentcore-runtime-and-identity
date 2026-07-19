// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import "testing"

func TestValidateLoopbackListen(t *testing.T) {
	valid := []string{
		"127.0.0.1:16385",
		"127.0.0.1:0",
		"[::1]:16385",
		"localhost:16385",
	}
	for _, addr := range valid {
		if err := validateLoopbackListen(addr); err != nil {
			t.Errorf("validateLoopbackListen(%q) unexpected error: %v", addr, err)
		}
	}

	invalid := []string{
		"0.0.0.0:16385",
		"192.168.1.5:16385",
		"example.com:16385",
		"[::]:16385",
		"not-a-valid-address",
		"127.0.0.1",
	}
	for _, addr := range invalid {
		if err := validateLoopbackListen(addr); err == nil {
			t.Errorf("validateLoopbackListen(%q) expected error, got nil", addr)
		}
	}
}
