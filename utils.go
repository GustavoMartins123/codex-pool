package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"time"
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
	trustedProxiesMu       sync.RWMutex
	trustedProxyNets       []*net.IPNet
	trustedProxyTrustAll   bool
	trustedProxySameSubnet bool
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
	if err := setTrustedProxies(effectiveTrustedProxies(nil)); err != nil {
		log.Fatalf("invalid trusted proxies: %v", err)
	}
	setTrustSameSubnet(parseBoolEnv("PROXY_TRUST_SAME_SUBNET", false))
}

func setTrustSameSubnet(enabled bool) {
	trustedProxiesMu.Lock()
	defer trustedProxiesMu.Unlock()
	trustedProxySameSubnet = enabled
}

// localSubnetCache caches the subnets of this process's own network
// interfaces so same-subnet trust checks stay cheap. Container networks do
// not change while the process runs; a short TTL covers interface changes.
var localSubnetCache struct {
	mu   sync.Mutex
	at   time.Time
	nets []*net.IPNet
}

const localSubnetCacheTTL = 30 * time.Second

func localInterfaceSubnets() []*net.IPNet {
	localSubnetCache.mu.Lock()
	defer localSubnetCache.mu.Unlock()
	if time.Since(localSubnetCache.at) < localSubnetCacheTTL && localSubnetCache.nets != nil {
		return localSubnetCache.nets
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return localSubnetCache.nets
	}
	nets := make([]*net.IPNet, 0, len(interfaces))
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsLinkLocalMulticast() {
				continue
			}
			nets = append(nets, ipNet)
		}
	}
	localSubnetCache.nets = nets
	localSubnetCache.at = time.Now()
	return nets
}

// ipSharesSubnetWithAny reports whether ip belongs to one of the given
// interface subnets. Interface IPNets carry the live subnet mask, so the
// comparison is family-aware through IPNet.Contains.
func ipSharesSubnetWithAny(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, subnet := range nets {
		if subnet == nil {
			continue
		}
		localIP := subnet.IP.To4()
		candidate := ip.To4()
		if (localIP == nil) != (candidate == nil) {
			continue
		}
		if localIP != nil {
			masked := &net.IPNet{IP: localIP, Mask: subnet.Mask}
			if masked.Contains(candidate) {
				return true
			}
			continue
		}
		if subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// sameSubnetAsSelf reports whether the peer IP shares a subnet with one of
// this process's own non-loopback interfaces. This is what makes reverse
// proxy containers (Traefik, nginx, Caddy) on the same Docker network trusted
// without hardcoding their dynamic container IPs.
func sameSubnetAsSelf(ip net.IP) bool {
	return ipSharesSubnetWithAny(ip, localInterfaceSubnets())
}

// parseTrustedProxies rejects malformed entries before replacing any policy.
func parseTrustedProxies(entries []string) ([]*net.IPNet, bool, error) {
	var nets []*net.IPNet
	trustAll := false
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "*" {
			trustAll = true
			continue
		}
		parsed, err := parseIPNetList("trusted_proxies", []string{entry})
		if err != nil {
			return nil, false, err
		}
		nets = append(nets, parsed...)
	}
	return nets, trustAll, nil
}

// If empty, only loopback peers are trusted. "*" explicitly trusts all peers.
func setTrustedProxies(entries []string) error {
	nets, trustAll, err := parseTrustedProxies(entries)
	if err != nil {
		return err
	}
	installTrustedProxies(nets, trustAll)
	return nil
}
func installTrustedProxies(nets []*net.IPNet, trustAll bool) {
	trustedProxiesMu.Lock()
	defer trustedProxiesMu.Unlock()
	trustedProxyTrustAll = trustAll
	trustedProxyNets = nets
}

// isTrustedProxy returns true if the remote peer IP is a trusted proxy.
func isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	trustedProxiesMu.RLock()
	trustAll := trustedProxyTrustAll
	trustSameSubnet := trustedProxySameSubnet
	nets := trustedProxyNets
	trustedProxiesMu.RUnlock()

	if trustAll {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	if trustSameSubnet && sameSubnetAsSelf(ip) {
		return true
	}
	return false
}

// requestClientIP attributes a request to its peer unless that peer is a
// trusted proxy. A supplied canonical forwarding header must be valid.
func requestClientIP(r *http.Request) (string, error) {
	if r == nil {
		return "", errors.New("request missing")
	}
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = strings.Trim(strings.TrimSpace(r.RemoteAddr), "[]")
	}
	peer := net.ParseIP(remoteHost)
	if peer == nil {
		return "", errors.New("invalid remote peer IP")
	}
	if !isTrustedProxy(peer) {
		return peer.String(), nil
	}
	single := func(name, raw string) (string, error) {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			return "", fmt.Errorf("invalid %s", name)
		}
		return ip.String(), nil
	}
	if raw, present := r.Header[http.CanonicalHeaderKey("CF-Connecting-IP")]; present {
		if len(raw) != 1 {
			return "", errors.New("invalid CF-Connecting-IP")
		}
		return single("CF-Connecting-IP", raw[0])
	}
	if raw, present := r.Header[http.CanonicalHeaderKey("X-Forwarded-For")]; present {
		parts := strings.Split(strings.Join(raw, ","), ",")
		ips := make([]net.IP, len(parts))
		for i, part := range parts {
			ips[i] = net.ParseIP(strings.TrimSpace(part))
			if ips[i] == nil {
				return "", errors.New("invalid X-Forwarded-For")
			}
		}
		// Walk from the immediate proxy toward the client. Any addresses to
		// the left of the first untrusted hop may have been supplied by it.
		for i := len(ips) - 1; i >= 0; i-- {
			if !isTrustedProxy(ips[i]) || i == 0 {
				return ips[i].String(), nil
			}
		}
	}
	if raw, present := r.Header[http.CanonicalHeaderKey("X-Real-IP")]; present {
		if len(raw) != 1 {
			return "", errors.New("invalid X-Real-IP")
		}
		return single("X-Real-IP", raw[0])
	}
	return peer.String(), nil
}
func getClientIP(r *http.Request) string {
	ip, _ := requestClientIP(r)
	return ip
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
