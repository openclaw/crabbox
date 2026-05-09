package semaphore

import (
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "semaphore"

type flagValues struct {
	Host        *string
	Project     *string
	Machine     *string
	OSImage     *string
	IdleTimeout *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) flagValues {
	sem := defaults.Semaphore
	return flagValues{
		Host:        fs.String("semaphore-host", sem.Host, "Semaphore host (e.g. myorg.semaphoreci.com)"),
		Project:     fs.String("semaphore-project", sem.Project, "Semaphore project name"),
		Machine:     fs.String("semaphore-machine", withDefault(sem.Machine, "f1-standard-2"), "Machine type"),
		OSImage:     fs.String("semaphore-os-image", withDefault(sem.OSImage, "ubuntu2204"), "OS image"),
		IdleTimeout: fs.String("semaphore-idle-timeout", withDefault(sem.IdleTimeout, "30m"), "Idle timeout"),
	}
}

func applyFlagOverrides(cfg *core.Config, fs *flag.FlagSet, v flagValues) {
	if wasSet(fs, "semaphore-host") {
		cfg.Semaphore.Host = *v.Host
	}
	if wasSet(fs, "semaphore-project") {
		cfg.Semaphore.Project = *v.Project
	}
	if wasSet(fs, "semaphore-machine") {
		cfg.Semaphore.Machine = *v.Machine
	}
	if wasSet(fs, "semaphore-os-image") {
		cfg.Semaphore.OSImage = *v.OSImage
	}
	if wasSet(fs, "semaphore-idle-timeout") {
		cfg.Semaphore.IdleTimeout = *v.IdleTimeout
	}
}

func wasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func withDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

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
		if u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("semaphore host %q must be a host name, not an API URL", value)
		}
		raw = u.Host
	}
	host := strings.TrimRight(raw, "/")
	if strings.ContainsAny(host, "/?#") {
		return "", fmt.Errorf("semaphore host %q must be a host name, not an API URL", value)
	}
	return host, nil
}

func idleTimeout(cfg core.Config) (time.Duration, error) {
	if cfg.Semaphore.IdleTimeout != "" {
		d, err := time.ParseDuration(cfg.Semaphore.IdleTimeout)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("invalid semaphore idle timeout %q", cfg.Semaphore.IdleTimeout)
		}
		return d, nil
	}
	return 30 * time.Minute, nil
}

func isCrabboxJobName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "crabbox testbox")
}
