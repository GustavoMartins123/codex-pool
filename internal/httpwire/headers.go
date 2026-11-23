// Package httpwire provides HTTP header and request body plumbing for the proxy.
package httpwire

import (
	"net/http"
	"net/textproto"
	"sort"
	"strings"
)

func CloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		cpy := make([]string, len(vv))
		copy(cpy, vv)
		out[k] = cpy
	}
	return out
}

func DebugHeaderSummary(h http.Header) []string {
	headers := make([]string, 0, len(h))
	for name, values := range h {
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		if IsSensitiveHeader(name) {
			value = "<redacted>"
		} else if len(value) > 80 {
			value = value[:80]
		}
		headers = append(headers, name+"="+value)
	}
	sort.Strings(headers)
	return headers
}

func IsSensitiveHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"x-api-key", "api-key", "x-goog-api-key", "x-oai-attestation":
		return true
	}
	// Providers invent their own credential header names, so an exact list
	// never stays complete. Treat any header that announces a secret by name
	// as sensitive.
	for _, marker := range []string{"token", "secret", "password", "session", "apikey", "api-key", "api_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.HasSuffix(lower, "-key")
}

func CopyHeader(dst, src http.Header) {
	for k, vv := range src {
		dst.Del(k)
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// RemoveHopByHopHeaders strips headers that must not be forwarded by proxies.
func RemoveHopByHopHeaders(h http.Header) {
	// Strip any headers listed in the Connection header first.
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(textproto.CanonicalMIMEHeaderKey(f))
			}
		}
	}

	// Standard hop-by-hop headers.
	for _, k := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(k)
	}
}

func HeaderContainsToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func IsWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return HeaderContainsToken(r.Header, "Connection", "Upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}
