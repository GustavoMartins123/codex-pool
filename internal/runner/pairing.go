package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

// PairingLink builds the QR payload. commandKey travels only through the QR,
// from the runner's screen to the device that scans it, never to the pool.
func PairingLink(origin, id, commandKey string) (string, error) {
	if err := ValidateOrigin(origin); err != nil {
		return "", err
	}
	if !validPairingID(id) {
		return "", errors.New("invalid pairing identifier")
	}
	if _, err := decodeCommandKey(commandKey); err != nil {
		return "", err
	}
	return "codexpool://pair?pool=" + url.QueryEscape(origin) + "&request=" + id + "&key=" + commandKey, nil
}

func validPairingID(id string) bool {
	return len(id) == 43 && !strings.ContainsAny(id, "/?#&=+% \r\n")
}

func BeginPairing(ctx context.Context, client *http.Client, origin string, machine runnerproto.PairingRequest, rotationToken string) (*runnerproto.PairingStart, error) {
	if err := ValidateOrigin(origin); err != nil {
		return nil, err
	}
	var result runnerproto.PairingStart
	status, err := pairingRequest(ctx, client, origin+"/api/runner/pairings", rotationToken, machine, &result)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("pairing rejected (HTTP %d)", status)
	}
	if len(result.ID) != 43 || len(result.DeviceSecret) != 43 || len(result.UserCode) != 8 || result.Interval < 3 || result.Interval > 30 || !result.ExpiresAt.After(time.Now()) || result.ExpiresAt.After(time.Now().Add(10*time.Minute)) {
		return nil, errors.New("invalid pairing response")
	}
	if !validPairingID(result.ID) {
		return nil, errors.New("invalid pairing identifier")
	}
	return &result, nil
}

func CompletePairing(ctx context.Context, client *http.Client, origin string, p runnerproto.PairingStart) (Config, bool, error) {
	var result runnerproto.RunnerCredential
	if err := ValidateOrigin(origin); err != nil {
		return Config{}, false, err
	}
	if !validPairingID(p.ID) {
		return Config{}, false, errors.New("invalid pairing identifier")
	}
	status, err := pairingRequest(ctx, client, origin+"/api/runner/pairings/"+p.ID, p.DeviceSecret, struct{}{}, &result)
	if err != nil {
		return Config{}, false, err
	}
	if status == 202 {
		return Config{}, false, nil
	}
	if status != 200 {
		return Config{}, false, fmt.Errorf("pairing rejected (HTTP %d); create a new request", status)
	}
	if len(result.RunnerID) != 36 || len(result.Token) != len("pool-runner-")+36+1+43 || !strings.HasPrefix(result.Token, "pool-runner-"+result.RunnerID+".") {
		return Config{}, false, errors.New("invalid pairing credential")
	}
	return Config{PoolURL: origin, RunnerID: result.RunnerID, Token: result.Token}, true, nil
}

func AwaitPairing(ctx context.Context, client *http.Client, origin string, p runnerproto.PairingStart) (Config, error) {
	ctx, cancel := context.WithDeadline(ctx, p.ExpiresAt)
	defer cancel()
	timer := time.NewTimer(time.Duration(p.Interval) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Config{}, errors.New("pairing expired or cancelled; create a new request")
		case <-timer.C:
			cfg, done, err := CompletePairing(ctx, client, origin, p)
			if err != nil {
				return Config{}, err
			}
			if done {
				return cfg, nil
			}
			// Measure the interval after the response: a fixed ticker can send
			// consecutive polls too close together when request latency varies.
			timer.Reset(time.Duration(p.Interval) * time.Second)
		}
	}
}

func pairingRequest(ctx context.Context, client *http.Client, endpoint, token string, body, result any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := httpClient(client).Do(r)
	if err != nil {
		return 0, errors.New("pairing transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode == 200 {
		if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(result); err != nil {
			return 0, errors.New("invalid pairing response")
		}
	}
	return response.StatusCode, nil
}
