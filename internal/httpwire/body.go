package httpwire

import (
	"errors"
	"io"
	"net/http"
)

// ShouldStreamBody returns true when the request body should be streamed directly
// instead of fully buffered in memory.
func ShouldStreamBody(r *http.Request, maxInMem int64) bool {
	if r == nil {
		return false
	}
	if r.ContentLength < 0 {
		return true
	}
	if maxInMem <= 0 {
		return false
	}
	return r.ContentLength > maxInMem
}

var ErrReplayBodyTooLarge = errors.New("request body exceeds replay buffer limit")

// ReadBodyForReplay reads a bounded body for paths that need retries or JSON
// rewriting. Reading without a limit lets one client reserve the entire host.
func ReadBodyForReplay(body io.ReadCloser, maxBytes int64, wantSample bool, sampleLimit int64) (full []byte, sample []byte, err error) {
	if body == nil {
		return nil, nil, nil
	}
	defer body.Close()
	if maxBytes <= 0 {
		return nil, nil, ErrReplayBodyTooLarge
	}
	full, err = io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(full)) > maxBytes {
		return nil, nil, ErrReplayBodyTooLarge
	}
	if wantSample && sampleLimit > 0 {
		if int64(len(full)) > sampleLimit {
			sample = full[:sampleLimit]
		} else {
			sample = full
		}
	}
	return full, sample, nil
}
