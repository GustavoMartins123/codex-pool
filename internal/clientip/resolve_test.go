package clientip

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testResolvedClientIP(t *testing.T, policy Settings, r *http.Request) string {
	t.Helper()
	ip, err := policy.ResolveClientIP(r)
	if err != nil {
		t.Fatal(err)
	}
	return ip
}

func TestIPSharesSubnetWithAny(t *testing.T) {
	eth0 := &net.IPNet{IP: net.ParseIP("172.18.0.5").To4(), Mask: net.CIDRMask(16, 32)}
	if !ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), []*net.IPNet{eth0}) {
		t.Fatal("same /16 subnet peer must match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.19.0.9"), []*net.IPNet{eth0}) {
		t.Fatal("peer outside the /16 must not match")
	}
	if ipSharesSubnetWithAny(nil, []*net.IPNet{eth0}) {
		t.Fatal("nil IP must not match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), nil) {
		t.Fatal("no subnets must never match")
	}
	ula := &net.IPNet{IP: net.ParseIP("fd00::1"), Mask: net.CIDRMask(64, 128)}
	if !ipSharesSubnetWithAny(net.ParseIP("fd00::42"), []*net.IPNet{ula}) {
		t.Fatal("same IPv6 /64 peer must match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), []*net.IPNet{ula}) {
		t.Fatal("IPv4 peer must not match an IPv6 subnet")
	}
}

// TestSecuritySameSubnetProxyTrust verifies the generic container-network
// proxy trust: a reverse proxy sharing a subnet with this process is trusted
// (so its X-Forwarded-For is honored) while unrelated peers stay untrusted,
// and the mode is strictly opt-in via PROXY_TRUST_SAME_SUBNET.
func TestSecuritySameSubnetProxyTrust(t *testing.T) {
	var policy Settings

	var subnet *net.IPNet
	for _, candidate := range interfaceSubnets() {
		if candidate.IP.To4() != nil {
			subnet = candidate
			break
		}
	}
	if subnet == nil {
		t.Skip("no IPv4 interface subnet available")
	}
	peer := append(net.IP(nil), subnet.IP.To4()...)
	peer[3]++
	if !subnet.Contains(peer) {
		t.Skip("interface subnet too small to derive a neighbor IP")
	}
	outside := make(net.IP, 4)
	binary.BigEndian.PutUint32(outside, binary.BigEndian.Uint32(subnet.IP.To4())^0xFFFFFFFF)

	// Opt-in disabled (default): same-subnet peer headers are ignored.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = net.JoinHostPort(peer.String(), "40000")
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	if ip := testResolvedClientIP(t, policy, req); ip != peer.String() {
		t.Fatalf("same-subnet peer must not be trusted by default, got %s", ip)
	}

	policy.TrustSameSubnet = true

	if ip := testResolvedClientIP(t, policy, req); ip != "198.51.100.77" {
		t.Fatalf("same-subnet proxy XFF should be respected, got %s", ip)
	}

	outsider := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	outsider.RemoteAddr = net.JoinHostPort(outside.String(), "40000")
	outsider.Header.Set("X-Forwarded-For", "198.51.100.77")
	if ip := testResolvedClientIP(t, policy, outsider); ip != outside.String() {
		t.Fatalf("peer outside local subnets must stay untrusted, got %s", ip)
	}
}
