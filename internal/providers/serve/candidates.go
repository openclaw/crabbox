package serve

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

const maxAddressCandidates = 4

type addressCandidate struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
}
type candidateInterface struct {
	Name      string
	Flags     net.Flags
	Addresses []net.Addr
}

func localCandidateInterfaces() ([]candidateInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]candidateInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		result = append(result, candidateInterface{iface.Name, iface.Flags, addrs})
	}
	return result, nil
}

func detectCandidates(explicit []string, noLAN bool, interfaces []candidateInterface) ([]addressCandidate, error) {
	var result []addressCandidate
	seen := map[string]bool{}
	add := func(ip net.IP, kind string) {
		address := ip.String()
		if !seen[address] && len(result) < maxAddressCandidates {
			result = append(result, addressCandidate{address, kind})
			seen[address] = true
		}
	}
	if len(explicit) > 0 {
		if len(explicit) > maxAddressCandidates {
			return nil, fmt.Errorf("at most four --advertise-address values are allowed")
		}
		for _, address := range explicit {
			ip := net.ParseIP(address)
			if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
				return nil, fmt.Errorf("--advertise-address must be IPv4 and not loopback, multicast, or unspecified: %q", address)
			}
			if seen[ip.String()] {
				return nil, fmt.Errorf("duplicate --advertise-address: %s", address)
			}
			add(ip, "explicit")
		}
		return result, nil
	}
	if interfaces == nil {
		var err error
		interfaces, err = localCandidateInterfaces()
		if err != nil {
			return nil, err
		}
	}
	// Stable ordering prevents harmless OS enumeration changes from changing the route.
	interfaces = append([]candidateInterface(nil), interfaces...)
	sort.SliceStable(interfaces, func(i, j int) bool { return interfaces[i].Name < interfaces[j].Name })
	for _, kind := range []string{"tailnet", "lan"} {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			if kind == "lan" && (noLAN || iface.Flags&net.FlagBroadcast == 0 || virtualLANInterface(iface.Name)) {
				continue
			}
			var ips []net.IP
			for _, addr := range iface.Addresses {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil || ip.To4() == nil {
					continue
				}
				ip = ip.To4()
				if kind == "tailnet" && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 || kind == "lan" && ip.IsPrivate() {
					ips = append(ips, ip)
				}
			}
			sort.Slice(ips, func(i, j int) bool { return ips[i].String() < ips[j].String() })
			for _, ip := range ips {
				add(ip, kind)
			}
			if kind == "tailnet" && len(result) > 0 {
				break
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no tailnet or private LAN IPv4 found; set --advertise-address to an address clients can reach")
	}
	return result, nil
}

func virtualLANInterface(name string) bool {
	for _, prefix := range []string{"utun", "bridge", "vmnet", "docker", "br-", "veth", "llw", "awdl", "anpi"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
