package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// newTokenRefreshRequest builds the POST that exchanges a refresh token at an
// OAuth token endpoint.
func newTokenRefreshRequest(ctx context.Context, endpoint, contentType, userAgent string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// tokenRefreshFailure reports a token endpoint status with up to 8 KiB of the
// response body. The body carries the codes isPermanentRefreshTokenError
// looks for.
func tokenRefreshFailure(resp *http.Response, label string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if len(bytes.TrimSpace(msg)) > 0 {
		return fmt.Errorf("%s: %s: %s", label, resp.Status, safeText(msg))
	}
	return fmt.Errorf("%s: %s", label, resp.Status)
}
