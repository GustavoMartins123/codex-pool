package clientip

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

type Policy struct {
	mu       sync.RWMutex
	settings Settings
	subnets  subnetCache
}

type Settings struct {
	allow, deny     []*net.IPNet
	trustedProxies  []*net.IPNet
	trustAllProxies bool
	TrustSameSubnet bool
	subnets         *subnetCache
}

func ParseNetList(field string, entries []string) ([]*net.IPNet, error) {
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

func ParseSettings(allow, deny, proxies []string, sameSubnet bool) (Settings, error) {
	allowNets, err := ParseNetList("PROXY_IP_ALLOW", allow)
	if err != nil {
		return Settings{}, err
	}
	denyNets, err := ParseNetList("PROXY_IP_DENY", deny)
	if err != nil {
		return Settings{}, err
	}
	proxyNets, trustAll, err := ParseTrustedProxies(proxies)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		allow: allowNets, deny: denyNets, trustedProxies: proxyNets,
		trustAllProxies: trustAll, TrustSameSubnet: sameSubnet,
	}, nil
}

func (p *Policy) Install(settings Settings) {
	p.mu.Lock()
	p.settings = settings
	p.mu.Unlock()
}

func (p *Policy) Snapshot() Settings {
	p.mu.RLock()
	settings := p.settings
	p.mu.RUnlock()
	settings.subnets = &p.subnets
	return settings
}

func (s Settings) Restricted() bool {
	return len(s.allow) > 0 || len(s.deny) > 0
}

func (s Settings) Permitted(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return !s.Restricted()
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

// localHealthProbe permits only read-only health routes from a direct local
// peer whose forwarded client identity is also local.
func localHealthProbe(r *http.Request, clientIP string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/healthz/", "/livez", "/livez/", "/readyz", "/readyz/":
	default:
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, client := net.ParseIP(host), net.ParseIP(clientIP)
	return peer != nil && client != nil && peer.IsLoopback() && client.IsLoopback()
}

func (p *Policy) AuthorizeRequest(w http.ResponseWriter, r *http.Request) *http.Request {
	settings := p.Snapshot()
	clientIP, err := settings.ResolveClientIP(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	if settings.Restricted() && !localHealthProbe(r, clientIP) && !settings.Permitted(clientIP) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil
	}
	return With(r, clientIP)
}
