package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

func bodyForInspection(r *http.Request, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	enc := ""
	if r != nil {
		enc = strings.ToLower(r.Header.Get("Content-Encoding"))
	}
	looksGzip := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if strings.Contains(enc, "gzip") || looksGzip {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer gr.Close()
		decoded, err := io.ReadAll(io.LimitReader(gr, 512*1024))
		if err != nil || len(decoded) == 0 {
			return body
		}
		return decoded
	}
	return body
}

// decodeRequestBody makes compressed JSON routable and rewritable. We must not
// forward encoded bytes after a model-based provider override: the selected
// provider needs the rewritten JSON, not the original Codex wire payload.
func decodeRequestBody(contentEncoding string, body []byte, maxBytes int64) ([]byte, bool, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	if len(body) == 0 || encoding == "" || encoding == "identity" {
		return body, false, nil
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}

	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false, fmt.Errorf("decode gzip request body: %w", err)
		}
		reader = gzipReader
	case "zstd":
		zstdReader, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false, fmt.Errorf("decode zstd request body: %w", err)
		}
		reader = zstdReader.IOReadCloser()
	default:
		return body, false, nil
	}
	defer reader.Close()

	decoded, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("decode %s request body: %w", encoding, err)
	}
	if int64(len(decoded)) > maxBytes {
		return nil, false, fmt.Errorf("decoded request body exceeds %d byte limit", maxBytes)
	}
	return decoded, true, nil
}

// clientOrDefaultTimeout picks the request timeout. If the client sent X-Stainless-Timeout
// (Anthropic SDK), use that. Otherwise fall back to streaming vs non-streaming defaults.
func clientOrDefaultTimeout(r *http.Request, reqTimeout, streamTimeout time.Duration, body []byte) time.Duration {
	var obj any
	if json.Unmarshal(body, &obj) != nil {
		obj = nil
	}
	request, _ := obj.(map[string]any)
	stream, _ := request["stream"].(bool)
	return timeoutForRequestIntent(r, reqTimeout, streamTimeout, requestIntent{
		stream: stream,
		image:  valueHasImageGenerationTool(obj),
	})
}

type requestIntent struct {
	stream bool
	image  bool
}

func timeoutForRequestIntent(r *http.Request, reqTimeout, streamTimeout time.Duration, intent requestIntent) time.Duration {
	const codexExpectedStreamTimeout = 5 * time.Minute

	isStreaming := intent.stream || strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
	isImageGeneration := intent.image

	// Streaming requests can run for a long time. Use the configured stream
	// timeout only; a zero value means no hard cap.
	if isStreaming {
		return streamTimeout
	}

	const maxClientRequestTimeout = 10 * time.Minute

	// Honour SDK/client-requested timeouts for non-streaming requests, but do
	// not let short 60s/120s client defaults cut off image generation before
	// Codex's expected 300s window. Bound against arbitrary client values to prevent
	// lingering connection denial-of-service.
	if v := r.Header.Get("X-Stainless-Timeout"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			requested := time.Duration(secs * float64(time.Second))
			if requested > maxClientRequestTimeout {
				requested = maxClientRequestTimeout
			}
			if isImageGeneration && requested < codexExpectedStreamTimeout {
				return codexExpectedStreamTimeout
			}
			return requested
		}
	}
	if isImageGeneration && reqTimeout > 0 && reqTimeout < codexExpectedStreamTimeout {
		return codexExpectedStreamTimeout
	}
	return reqTimeout
}
