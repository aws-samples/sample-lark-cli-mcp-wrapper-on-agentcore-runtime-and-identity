// Copyright (c) 2026 panlm
// SPDX-License-Identifier: MIT

//go:build authsidecar_agentcore_identity

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthMgmt_HandleComplete_RejectsMissingSessionID(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodPost, "/_sidecar/auth/complete", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	a.handleComplete(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAuthMgmt_HandleComplete_RejectsGET(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodGet, "/_sidecar/auth/complete", nil)
	rec := httptest.NewRecorder()

	a.handleComplete(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestAuthMgmt_HandleStart_RejectsGET(t *testing.T) {
	a := &authMgmt{logf: func(string, ...any) {}}
	req := httptest.NewRequest(http.MethodGet, "/_sidecar/auth/start", nil)
	rec := httptest.NewRecorder()

	a.handleStart(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
