package applecontainer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// inspectContainer mirrors the JSON returned by `container inspect <id>` and
// `container ls --format json`. Apple's runtime documents the network shape
// (a `networks` array carrying an IPv4 address in CIDR form) and a
// `configuration` object that holds the container id/image. The label location
// is not fully
// documented; we look for labels in the most CLI-consistent places
// (`configuration.labels` and a top-level `labels`) and tolerate either.
//
// Reference: https://github.com/apple/container/blob/main/docs/how-to.md
type inspectContainer struct {
	Status        string               `json:"status"`
	Configuration inspectConfiguration `json:"configuration"`
	Networks      []inspectNetwork     `json:"networks"`
	Labels        map[string]string    `json:"labels,omitempty"`
}

type inspectConfiguration struct {
	ID       string            `json:"id"`
	Image    inspectImage      `json:"image"`
	Hostname string            `json:"hostname,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	DNS      inspectDNS        `json:"dns,omitempty"`
}

// inspectImage tolerates both an object ({"reference":"..."}) and a bare
// string image form, since the documented surface does not pin this down.
type inspectImage struct {
	Reference string `json:"reference,omitempty"`
}

func (i *inspectImage) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		i.Reference = s
		return nil
	}
	type alias inspectImage
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*i = inspectImage(a)
	return nil
}

type inspectNetwork struct {
	Address     string `json:"address"`
	IPv4Address string `json:"ipv4Address,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	IPv4Gateway string `json:"ipv4Gateway,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Network     string `json:"network,omitempty"`
}

type inspectDNS struct {
	Nameservers   []string `json:"nameservers,omitempty"`
	SearchDomains []string `json:"searchDomains,omitempty"`
	Options       []string `json:"options,omitempty"`
}

func decodeInspect(data []byte) ([]inspectContainer, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	var containers []inspectContainer
	if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
		// `container inspect <id>` may return a single object in some
		// versions; fall back to a single-element decode.
		var single inspectContainer
		if singleErr := json.Unmarshal([]byte(trimmed), &single); singleErr != nil {
			return nil, fmt.Errorf("decode container inspect: %w", err)
		}
		return []inspectContainer{single}, nil
	}
	return containers, nil
}

func (c inspectContainer) id() string {
	return strings.TrimSpace(c.Configuration.ID)
}

func (c inspectContainer) labels() map[string]string {
	out := map[string]string{}
	for k, v := range c.Configuration.Labels {
		out[k] = v
	}
	for k, v := range c.Labels {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

func (c inspectContainer) image() string {
	return strings.TrimSpace(c.Configuration.Image.Reference)
}

func (c inspectContainer) status() string {
	return strings.ToLower(strings.TrimSpace(c.Status))
}

// ip returns the container's first network address without its CIDR suffix.
func (c inspectContainer) ip() string {
	for _, n := range c.Networks {
		addr := firstNonBlank(n.Address, n.IPv4Address)
		if addr == "" {
			continue
		}
		if idx := strings.IndexByte(addr, '/'); idx >= 0 {
			addr = addr[:idx]
		}
		if addr != "" {
			return addr
		}
	}
	return ""
}

func (c inspectContainer) running() bool {
	switch c.status() {
	case "running", "ready":
		return true
	default:
		return false
	}
}
