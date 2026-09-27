package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"sort"
	"strings"
	"sync"
)

func randomID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

func safeText(b []byte) string {
	s := string(b)
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return redactSecrets(s)
}

var (
	trustedProxiesMu     sync.RWMutex
	trustedProxyNets     []*net.IPNet
	trustedProxyTrustAll bool
)

func init() {
	initTrustedProxiesFromEnv()
}

// splitCommaEntries splits a comma-separated env value into trimmed entries.
func splitCommaEntries(raw string) []string {
	var list []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			list = append(list, part)
		}
	}
	return list
}

func initTrustedProxiesFromEnv() {
	raw := os.Getenv("PROXY_TRUSTED_PROXIES")
	if raw == "" {
		raw = os.Getenv("TRUSTED_PROXIES")
	}
	if raw != "" {
		var list []string
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				list = append(list, part)
			}
		}
		setTrustedProxies(list)
	} else {
		// By default, only loopback addresses are trusted proxies.
		setTrustedProxies(nil)
	}
}

// setTrustedProxies configures the list of trusted proxy CIDRs/IPs.
// Passing "*" or "all" trusts all peer connections.
// If empty, only loopback addresses (127.0.0.1, ::1) are trusted.
func setTrustedProxies(entries []string) {
	trustedProxiesMu.Lock()
	defer trustedProxiesMu.Unlock()

	trustedProxyTrustAll = false
	trustedProxyNets = nil

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" || strings.EqualFold(entry, "all") {
			trustedProxyTrustAll = true
			continue
		}
		if strings.Contains(entry, "/") {
			_, ipNet, err := net.ParseCIDR(entry)
			if err == nil && ipNet != nil {
				trustedProxyNets = append(trustedProxyNets, ipNet)
			}
			continue
		}
		ip := net.ParseIP(entry)
		if ip != nil {
			if v4 := ip.To4(); v4 != nil {
				trustedProxyNets = append(trustedProxyNets, &net.IPNet{
					IP:   v4,
					Mask: net.CIDRMask(32, 32),
				})
			} else {
				trustedProxyNets = append(trustedProxyNets, &net.IPNet{
					IP:   ip,
					Mask: net.CIDRMask(128, 128),
				})
			}
		}
	}
}

// isTrustedProxy returns true if the remote peer IP is a trusted proxy.
func isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	trustedProxiesMu.RLock()
	defer trustedProxiesMu.RUnlock()

	if trustedProxyTrustAll {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// getClientIP extracts the client IP from the request.
// Proxy headers (CF-Connecting-IP, X-Forwarded-For, X-Real-IP) are ONLY trusted
// when the immediate connecting peer (r.RemoteAddr) is a verified trusted proxy
// (e.g. loopback or configured trusted proxy CIDRs). Direct untrusted connections will always
// use the IP from RemoteAddr to prevent header spoofing.
func getClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = strings.TrimSpace(r.RemoteAddr)
	}
	remoteHost = strings.Trim(remoteHost, "[]")
	peerIP := net.ParseIP(remoteHost)

	// If remote peer is not a trusted proxy, do not trust proxy headers.
	if peerIP == nil || !isTrustedProxy(peerIP) {
		if peerIP != nil {
			return peerIP.String()
		}
		return remoteHost
	}

	// Peer is trusted, so inspect proxy headers.
	// Check CF-Connecting-IP first (Cloudflare, most reliable when present).
	if cfip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfip != "" {
		if ip := net.ParseIP(cfip); ip != nil {
			return ip.String()
		}
	}
	// Check X-Forwarded-For (may contain multiple IPs, take first valid one).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			part = strings.TrimSpace(part)
			if ip := net.ParseIP(part); ip != nil {
				return ip.String()
			}
		}
	}
	// Check X-Real-IP.
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return ip.String()
		}
	}
	// Fall back to RemoteAddr.
	if peerIP != nil {
		return peerIP.String()
	}
	return remoteHost
}

// secureSecretEquals compares two secret strings without leaking match
// length or prefix information. Both sides are hashed to a fixed length
// first so the constant-time comparison never reveals how much of the
// raw secret agreed.
func secureSecretEquals(a, b string) bool {
	ah := hashToken(a)
	bh := hashToken(b)
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func poolHashSalt(legacySalt string) string {
	legacySalt = strings.TrimSpace(legacySalt)
	if legacySalt != "" {
		return legacySalt
	}
	return "codex-pool"
}

func hashRequestOrigin(r *http.Request, salt string) string {
	if r == nil {
		return ""
	}
	ip := getClientIP(r)
	if ip == "" {
		return ""
	}
	return "ip_" + hashUserIP(ip, salt)
}

func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func respondJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// shouldStreamBody returns true when the request body should be streamed directly
// instead of fully buffered in memory.
func shouldStreamBody(r *http.Request, maxInMem int64) bool {
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

var errReplayBodyTooLarge = errors.New("request body exceeds replay buffer limit")

// readBodyForReplay reads a bounded body for paths that need retries or JSON
// rewriting. Reading without a limit lets one client reserve the entire host.
func readBodyForReplay(body io.ReadCloser, maxBytes int64, wantSample bool, sampleLimit int64) (full []byte, sample []byte, err error) {
	if body == nil {
		return nil, nil, nil
	}
	defer body.Close()
	if maxBytes <= 0 {
		return nil, nil, errReplayBodyTooLarge
	}
	full, err = io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(full)) > maxBytes {
		return nil, nil, errReplayBodyTooLarge
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

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		cpy := make([]string, len(vv))
		copy(cpy, vv)
		out[k] = cpy
	}
	return out
}

func debugHeaderSummary(h http.Header) []string {
	headers := make([]string, 0, len(h))
	for name, values := range h {
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		if isSensitiveHeader(name) {
			value = "<redacted>"
		} else if len(value) > 80 {
			value = value[:80]
		}
		headers = append(headers, name+"="+value)
	}
	sort.Strings(headers)
	return headers
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"x-api-key", "api-key", "x-goog-api-key", "x-oai-attestation":
		return true
	default:
		return false
	}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		dst.Del(k)
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// removeHopByHopHeaders strips headers that must not be forwarded by proxies.
func removeHopByHopHeaders(h http.Header) {
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

func headerContainsToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return headerContainsToken(r.Header, "Connection", "Upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
