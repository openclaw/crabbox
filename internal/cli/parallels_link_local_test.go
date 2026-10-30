package cli

import (
	"fmt"
	"testing"
)

func TestParseParallelsVMsAddressSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		network    string
		want       string
	}{
		{name: "IPv4 below range", configured: "169.253.255.255", want: "169.253.255.255"},
		{name: "IPv4 range start", configured: "169.254.0.0"},
		{name: "IPv4 range end", configured: "169.254.255.255"},
		{name: "IPv4 above range", configured: "169.255.0.0", want: "169.255.0.0"},
		{name: "public IPv4", configured: "192.0.2.1", want: "192.0.2.1"},
		{name: "private IPv4", configured: "10.211.55.8", want: "10.211.55.8"},
		{name: "IPv6 below range", configured: "fe7f::1", want: "fe7f::1"},
		{name: "IPv6 range start", configured: "fe80::1"},
		{name: "IPv6 range end", configured: "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
		{name: "IPv6 above range", configured: "fec0::1", want: "fec0::1"},
		{name: "global IPv6", configured: "2001:db8::1", want: "2001:db8::1"},
		{name: "first configured usable", configured: "169.254.2.136 fe80::1 10.211.55.8 10.211.55.9", want: "10.211.55.8"},
		{name: "configured address wins", configured: "10.211.55.8 169.254.2.136", network: `[{"type":"ipv4","ip":"10.211.55.9"}]`, want: "10.211.55.8"},
		{name: "configured IPv6 wins", configured: "2001:db8::1", network: `[{"type":"ipv4","ip":"10.211.55.9"}]`, want: "2001:db8::1"},
		{name: "first network usable", configured: "169.254.2.136", network: `[{"type":"ipv4","ip":"169.254.3.1"},{"type":"ipv4","ip":"10.211.55.8"},{"type":"ipv4","ip":"10.211.55.9"}]`, want: "10.211.55.8"},
		{name: "network IPv6 still ignored", network: `[{"type":"ipv6","ip":"2001:db8::1"},{"type":"ipv4","ip":"10.211.55.8"}]`, want: "10.211.55.8"},
		{name: "network range start", network: `[{"type":"ipv4","ip":"169.254.0.0"}]`},
		{name: "network range end", network: `[{"type":"ipv4","ip":"169.254.255.255"}]`},
		{name: "network below range", network: `[{"type":"ipv4","ip":"169.253.255.255"}]`, want: "169.253.255.255"},
		{name: "network above range", network: `[{"type":"ipv4","ip":"169.255.0.0"}]`, want: "169.255.0.0"},
		{name: "whitespace and placeholders", configured: " \t-\n169.254.2.136\t10.211.55.8 ", want: "10.211.55.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := tc.network
			if network == "" {
				network = "[]"
			}
			vms, err := parseParallelsVMs(fmt.Sprintf(`[{"ip_configured":%q,"Network":{"ipAddresses":%s}}]`, tc.configured, network))
			if err != nil || len(vms) != 1 {
				t.Fatalf("vms=%+v err=%v", vms, err)
			}
			if vms[0].IP != tc.want {
				t.Fatalf("IP=%q, want %q", vms[0].IP, tc.want)
			}
		})
	}
}
