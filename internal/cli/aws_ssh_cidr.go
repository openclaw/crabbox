package cli

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

func ensureAWSSSHCIDRs(ctx context.Context, cfg *Config) {
	if cfg.Provider != "aws" || len(cfg.AWSSSHCIDRs) > 0 {
		return
	}
	cidr, err := detectOutboundIPv4CIDR(ctx)
	if err != nil || cidr == "" {
		return
	}
	cfg.AWSSSHCIDRs = []string{cidr}
}

func detectOutboundIPv4CIDR(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 128))
	if err != nil {
		return "", err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return "", err
	}
	if !addr.Is4() {
		return "", nil
	}
	return netip.PrefixFrom(addr, 32).String(), nil
}
