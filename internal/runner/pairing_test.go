package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func TestAwaitPairingWaitsAfterPendingResponse(t *testing.T) {
	var readyAt time.Time
	var calls int
	id := "b7ca2b0b-0099-490d-a49e-420ec07b720c"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			time.Sleep(250 * time.Millisecond)
			readyAt = time.Now().Add(3 * time.Second)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if time.Now().Before(readyAt) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(runnerproto.RunnerCredential{RunnerID: id, Token: "pool-runner-" + id + "." + strings.Repeat("s", 43)})
	}))
	defer server.Close()
	p := runnerproto.PairingStart{ID: strings.Repeat("i", 43), DeviceSecret: strings.Repeat("d", 43), Interval: 3, ExpiresAt: time.Now().Add(10 * time.Second)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := AwaitPairing(ctx, server.Client(), server.URL, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerID != id || calls != 2 {
		t.Fatal("invalid authorization completion")
	}
}
