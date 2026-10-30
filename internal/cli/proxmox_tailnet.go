package cli

import "net"

func proxmoxTailnetIPv4(value string) bool {
	ip := net.ParseIP(value).To4()
	return ip != nil && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}
