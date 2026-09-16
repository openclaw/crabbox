package semaphore

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "semaphore"

func normalizeSemaphoreHost(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("invalid semaphore host %q", value)
		}
		if u.Scheme != "https" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("semaphore host %q must be a host name, not an API URL", value)
		}
		raw = u.Host
	}
	host := strings.TrimRight(raw, "/")
	if strings.ContainsAny(host, "/?#@") {
		return "", fmt.Errorf("semaphore host %q must be a host name, not an API URL", value)
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host == "" || u.Host != host || u.User != nil || u.Hostname() == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid semaphore host %q", value)
	}
	return host, nil
}

func idleTimeout(cfg core.Config) (time.Duration, error) {
	value := core.Blank(cfg.Semaphore.IdleTimeout, core.SemaphoreConfigFlagFallbackIdleTimeout)
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid semaphore idle timeout %q", value)
	}
	return d, nil
}

func isCrabboxJobName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "crabbox testbox")
}
