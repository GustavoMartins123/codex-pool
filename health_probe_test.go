package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthAndLivenessProbes(t *testing.T) {
	handler := &proxyHandler{
		startTime: time.Now().Add(-5 * time.Minute),
	}

	tests := []struct {
		path       string
		wantStatus int
		wantField  string
		wantValue  string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantField: "status", wantValue: "ok"},
		{path: "/healthz/", wantStatus: http.StatusOK, wantField: "status", wantValue: "ok"},
		{path: "/livez", wantStatus: http.StatusOK, wantField: "status", wantValue: "ok"},
		{path: "/livez/", wantStatus: http.StatusOK, wantField: "status", wantValue: "ok"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d for %s, got %d", tc.wantStatus, tc.path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("expected application/json, got %q", ct)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid json response: %v", err)
			}
			if got, ok := body[tc.wantField].(string); !ok || got != tc.wantValue {
				t.Fatalf("expected %s=%q, got %v", tc.wantField, tc.wantValue, body[tc.wantField])
			}
		})
	}
}

func TestReadinessProbe(t *testing.T) {
	// 1. Not ready when pool is uninitialized
	unreadyHandler := &proxyHandler{
		pool: nil,
	}

	reqUnready := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recUnready := httptest.NewRecorder()
	unreadyHandler.ServeHTTP(recUnready, reqUnready)

	if recUnready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for uninitialized pool, got %d", recUnready.Code)
	}

	var unreadyBody map[string]any
	if err := json.Unmarshal(recUnready.Body.Bytes(), &unreadyBody); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if unreadyBody["status"] != "not_ready" {
		t.Fatalf("expected status 'not_ready', got %v", unreadyBody["status"])
	}

	// 2. Ready when pool is initialized
	readyHandler := &proxyHandler{
		pool: newPoolState([]*Account{}, false),
	}

	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady := httptest.NewRecorder()
	readyHandler.ServeHTTP(recReady, reqReady)

	if recReady.Code != http.StatusOK {
		t.Fatalf("expected 200 for initialized pool, got %d", recReady.Code)
	}

	var readyBody map[string]any
	if err := json.Unmarshal(recReady.Body.Bytes(), &readyBody); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if readyBody["status"] != "ready" {
		t.Fatalf("expected status 'ready', got %v", readyBody["status"])
	}
}
