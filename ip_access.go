package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

type ipAccessPolicy struct {
	mu       sync.RWMutex
	settings ipAccessSettings
}

type ipAccessSettings struct {
	allow, deny     []*net.IPNet
	trustedProxies  []*net.IPNet
	trustAllProxies bool
	trustSameSubnet bool
}

func parseIPNetList(field string, entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("%s: empty IP or CIDR entry", field)
		}
		var ipNet *net.IPNet
		if strings.Contains(entry, "/") {
			if _, parsed, err := net.ParseCIDR(entry); err == nil && parsed != nil {
				ipNet = parsed
			}
		} else if ip := net.ParseIP(entry); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				ipNet = &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
			} else {
				ipNet = &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
			}
		}
		if ipNet == nil {
			return nil, fmt.Errorf("%s: invalid IP or CIDR entry %q", field, entry)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

func parseIPAccessSettings(allow, deny, proxies []string, sameSubnet bool) (ipAccessSettings, error) {
	allowNets, err := parseIPNetList("PROXY_IP_ALLOW", allow)
	if err != nil {
		return ipAccessSettings{}, err
	}
	denyNets, err := parseIPNetList("PROXY_IP_DENY", deny)
	if err != nil {
		return ipAccessSettings{}, err
	}
	proxyNets, trustAll, err := parseTrustedProxies(proxies)
	if err != nil {
		return ipAccessSettings{}, err
	}
	return ipAccessSettings{
		allow: allowNets, deny: denyNets, trustedProxies: proxyNets,
		trustAllProxies: trustAll, trustSameSubnet: sameSubnet,
	}, nil
}

func (p *ipAccessPolicy) install(settings ipAccessSettings) {
	p.mu.Lock()
	p.settings = settings
	p.mu.Unlock()
}

func (p *ipAccessPolicy) snapshot() ipAccessSettings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.settings
}

func (s ipAccessSettings) restricted() bool {
	return len(s.allow) > 0 || len(s.deny) > 0
}

func (s ipAccessSettings) permitted(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return !s.restricted()
	}
	for _, n := range s.deny {
		if n.Contains(ip) {
			return false
		}
	}
	if len(s.allow) == 0 {
		return true
	}
	for _, n := range s.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
