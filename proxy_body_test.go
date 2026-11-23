package main

import (
	"net/http"
	"testing"

	"codex-pool-proxy/internal/httpwire"
)

func TestShouldStreamBodyUnknownLength(t *testing.T) {
	req := &http.Request{ContentLength: -1}
	if !httpwire.ShouldStreamBody(req, 1024) {
		t.Fatalf("expected streaming for unknown length")
	}
}

func TestShouldStreamBodyThreshold(t *testing.T) {
	req := &http.Request{ContentLength: 1024}
	if httpwire.ShouldStreamBody(req, 1024) {
		t.Fatalf("expected no streaming at threshold")
	}
	req.ContentLength = 2048
	if !httpwire.ShouldStreamBody(req, 1024) {
		t.Fatalf("expected streaming above threshold")
	}
}
