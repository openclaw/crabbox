package semaphore

import (
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "semaphore"

type flagValues struct {
	Host        *string
	Token       *string
	Project     *string
	Machine     *string
	OSImage     *string
	IdleTimeout *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) flagValues {
	sem := defaults.Semaphore
	return flagValues{
		Host:        fs.String("semaphore-host", sem.Host, "Semaphore host (e.g. myorg.semaphoreci.com)"),
		Token:       fs.String("semaphore-token", sem.Token, "Semaphore API token"),
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
	if wasSet(fs, "semaphore-token") {
		cfg.Semaphore.Token = *v.Token
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

func idleTimeout(cfg core.Config) time.Duration {
	if cfg.Semaphore.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.Semaphore.IdleTimeout); err == nil {
			return d
		}
	}
	return 30 * time.Minute
}
