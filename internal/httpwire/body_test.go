package httpwire

import (
	"net/http"
	"testing"
)

func TestShouldStreamBodyUnknownLength(t *testing.T) {
	req := &http.Request{ContentLength: -1}
	if !ShouldStreamBody(req, 1024) {
		t.Fatalf("expected streaming for unknown length")
	}
}

func TestShouldStreamBodyThreshold(t *testing.T) {
	req := &http.Request{ContentLength: 1024}
	if ShouldStreamBody(req, 1024) {
		t.Fatalf("expected no streaming at threshold")
	}
	req.ContentLength = 2048
	if !ShouldStreamBody(req, 1024) {
		t.Fatalf("expected streaming above threshold")
	}
}
