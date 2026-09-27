package main

import (
	"net"
	"strings"
	"sync"
)

// ipAccessPolicy gates requests by client IP/CIDR. An empty allow list means
// every address is a candidate; deny always wins over allow. Loopback is
// always permitted so container health checks and local admin tooling keep
// working regardless of the configured policy.
type ipAccessPolicy struct {
	mu    sync.RWMutex
	allow []*net.IPNet
	deny  []*net.IPNet
}

// globalIPAccess is wired once at startup from config/env, mirroring the
// trusted-proxies pattern in utils.go.
var globalIPAccess = &ipAccessPolicy{}

// parseIPNetList converts "ip" and "ip/prefix" entries into CIDR networks.
// Invalid entries are skipped silently, matching setTrustedProxies.
func parseIPNetList(entries []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, ipNet, err := net.ParseCIDR(entry); err == nil && ipNet != nil {
				nets = append(nets, ipNet)
			}
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				nets = append(nets, &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)})
			} else {
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
			}
		}
	}
	return nets
}

func (p *ipAccessPolicy) configure(allow, deny []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allow = parseIPNetList(allow)
	p.deny = parseIPNetList(deny)
}

func (p *ipAccessPolicy) restricted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.allow) > 0 || len(p.deny) > 0
}

// permitted reports whether the client IP may reach the pool. Unparseable
// peers are refused only when an explicit allow list exists.
func (p *ipAccessPolicy) permitted(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return len(p.allow) == 0
	}
	if ip.IsLoopback() {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, n := range p.deny {
		if n.Contains(ip) {
			return false
		}
	}
	if len(p.allow) == 0 {
		return true
	}
	for _, n := range p.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
