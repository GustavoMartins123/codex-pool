package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

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
		return nil
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

// isTrustedProxy returns true if the remote peer IP is a trusted proxy.
func (s ipAccessSettings) isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if s.trustAllProxies {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range s.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	if s.trustSameSubnet && sameSubnetAsSelf(ip) {
		return true
	}
	return false
}

// resolveClientIP attributes a request to its peer unless that peer is a
// trusted proxy. A supplied canonical forwarding header must be valid.
func (s ipAccessSettings) resolveClientIP(r *http.Request) (string, error) {
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
	if !s.isTrustedProxy(peer) {
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
			if !s.isTrustedProxy(ips[i]) || i == 0 {
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

type requestClientIPKey struct{}

func withClientIP(r *http.Request, ip string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestClientIPKey{}, ip))
}

func requestClientIP(r *http.Request) (string, error) {
	if r == nil {
		return "", errors.New("request missing")
	}
	ip, ok := r.Context().Value(requestClientIPKey{}).(string)
	if !ok {
		return "", errors.New("client IP was not resolved at ingress")
	}
	return ip, nil
}

func getClientIP(r *http.Request) string {
	ip, _ := requestClientIP(r)
	return ip
}
