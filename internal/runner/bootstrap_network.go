package runner

import (
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"

	"codex-pool-proxy/internal/runnerproto"
)

// BootstrapInterface is the part of a network interface that decides
// whether a phone on the LAN can reach the runner through it.
type BootstrapInterface struct {
	Name     string
	Flags    net.Flags
	Hardware bool
	Addrs    []netip.Addr
}

// Bridges, tunnels and VPNs rarely lead to the phone. Names only rank and
// filter candidates; the QR secret and certificate pin protect the channel.
var virtualInterfacePrefixes = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "utun", "tun", "tap", "wg", "zt", "tailscale", "lxc", "lxd", "cni", "flannel", "podman"}

func virtualInterface(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// bootstrapCandidates picks at most BootstrapMaxEndpoints private IPv4
// addresses of interfaces that are up, physical first.
func bootstrapCandidates(interfaces []BootstrapInterface) []netip.Addr {
	type candidate struct {
		addr     netip.Addr
		hardware bool
		order    int
	}
	var found []candidate
	seen := map[netip.Addr]bool{}
	for i, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagRunning == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 || virtualInterface(iface.Name) {
			continue
		}
		for _, addr := range iface.Addrs {
			addr = addr.Unmap()
			if !runnerproto.BootstrapAddressAllowed(addr) || seen[addr] {
				continue
			}
			seen[addr] = true
			found = append(found, candidate{addr, iface.Hardware, i})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].hardware != found[j].hardware {
			return found[i].hardware
		}
		return found[i].order < found[j].order
	})
	addrs := []netip.Addr{}
	for _, c := range found {
		if len(addrs) == runnerproto.BootstrapMaxEndpoints {
			break
		}
		addrs = append(addrs, c.addr)
	}
	return addrs
}

func localBootstrapInterfaces() ([]BootstrapInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.New("network interfaces unavailable")
	}
	result := make([]BootstrapInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		entry := BootstrapInterface{Name: iface.Name, Flags: iface.Flags, Hardware: len(iface.HardwareAddr) == 6}
		for _, addr := range addrs {
			if prefix, err := netip.ParsePrefix(addr.String()); err == nil {
				entry.Addrs = append(entry.Addrs, prefix.Addr())
			}
		}
		result = append(result, entry)
	}
	return result, nil
}

var errNoBootstrapAddress = errors.New("no private IPv4 LAN address found: connect this computer to the phone's network (IPv6-only networks are not supported yet)")
