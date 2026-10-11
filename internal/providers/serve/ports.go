package serve

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

// portPool reserves ports between assignment and the next runtime inventory.
// The runtime owns the final bind; another process winning that race fails creation.
type portPool struct {
	ports []int
	host  string
	used  map[string]int
}

func newPortPool(spec, host string) (*portPool, error) {
	if spec == "" {
		return nil, nil
	}
	ports, err := parseSSHPorts(spec)
	if err != nil {
		return nil, err
	}
	return &portPool{ports: ports, host: host, used: map[string]int{}}, nil
}
func parseSSHPorts(spec string) ([]int, error) {
	invalid := func() ([]int, error) {
		return nil, fmt.Errorf("invalid --ssh-ports: use ports/ranges in 1-65535, at most 1024 ports")
	}
	var ports []int
	seen := map[int]bool{}
	for _, entry := range strings.Split(spec, ",") {
		if entry == "" {
			return invalid()
		}
		bounds := strings.Split(entry, "-")
		if len(bounds) > 2 {
			return invalid()
		}
		numbers := make([]int, len(bounds))
		for i, text := range bounds {
			if text == "" || strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return invalid()
			}
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 || n > 65535 {
				return invalid()
			}
			numbers[i] = n
		}
		first, last := numbers[0], numbers[len(numbers)-1]
		if first > last || last-first+1 > 1024 {
			return invalid()
		}
		for n := first; n <= last; n++ {
			if !seen[n] {
				ports = append(ports, n)
				seen[n] = true
			}
			if len(ports) > 1024 {
				return invalid()
			}
		}
	}
	return ports, nil
}
func (p *portPool) reconcile(containers []localcontainer.ServedContainer) {
	p.used = map[string]int{}
	for _, c := range containers {
		if port, err := strconv.Atoi(c.Port); err == nil {
			p.used[c.LeaseID] = port
		}
	}
}
func (p *portPool) free(port int) bool {
	for _, used := range p.used {
		if port == used {
			return false
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(p.host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
func (p *portPool) capacity() int {
	count := len(p.used)
	for _, port := range p.ports {
		if p.free(port) {
			count++
		}
	}
	return count
}
func (p *portPool) acquire(id string) (string, error) {
	if port, ok := p.used[id]; ok {
		return strconv.Itoa(port), nil
	}
	for _, port := range p.ports {
		if p.free(port) {
			p.used[id] = port
			return strconv.Itoa(port), nil
		}
	}
	return "", fmt.Errorf("serve_ssh_ports_exhausted: no free port in --ssh-ports pool")
}
func (p *portPool) release(id string) { delete(p.used, id) }
