package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

type ipAccessPolicy struct {
	mu    sync.RWMutex
	allow []*net.IPNet
	deny  []*net.IPNet
}

var globalIPAccess = &ipAccessPolicy{}

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

// configure installs an access policy atomically: on any invalid entry it
// returns an error and keeps the previously installed policy untouched.
func (p *ipAccessPolicy) configure(allow, deny []string) error {
	allowNets, err := parseIPNetList("PROXY_IP_ALLOW", allow)
	if err != nil {
		return err
	}
	denyNets, err := parseIPNetList("PROXY_IP_DENY", deny)
	if err != nil {
		return err
	}
	p.install(allowNets, denyNets)
	return nil
}
func (p *ipAccessPolicy) install(allowNets, denyNets []*net.IPNet) {
	p.mu.Lock()
	p.allow = allowNets
	p.deny = denyNets
	p.mu.Unlock()
}

func (p *ipAccessPolicy) restricted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.allow) > 0 || len(p.deny) > 0
}

func (p *ipAccessPolicy) permitted(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return len(p.allow) == 0 && len(p.deny) == 0
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
