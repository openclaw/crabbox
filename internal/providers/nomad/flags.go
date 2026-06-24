package nomad

import (
	"flag"
	"strings"
	"time"
)

type flagValues struct {
	Address           *string
	Region            *string
	Namespace         *string
	TokenEnv          *string
	CACert            *string
	CAPath            *string
	ClientCert        *string
	ClientKey         *string
	TLSServerName     *string
	SkipVerify        *bool
	Task              *string
	Driver            *string
	Image             *string
	Workdir           *string
	JobSpecTemplate   *string
	NodePool          *string
	Datacenters       *string
	CPU               *int
	MemoryMB          *int
	DiskMB            *int
	AllocReadyTimeout *string
	EvalTimeout       *string
	ExecTimeoutSecs   *int
}

func RegisterNomadProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		Address:           fs.String("nomad-address", defaults.Nomad.Address, "Nomad HTTP API address (or NOMAD_ADDR)"),
		Region:            fs.String("nomad-region", defaults.Nomad.Region, "Nomad region (or NOMAD_REGION)"),
		Namespace:         fs.String("nomad-namespace", defaults.Nomad.Namespace, "Nomad namespace (or NOMAD_NAMESPACE)"),
		TokenEnv:          fs.String("nomad-token-env", defaults.Nomad.TokenEnv, "environment variable containing the Nomad ACL token"),
		CACert:            fs.String("nomad-ca-cert", defaults.Nomad.CACert, "Nomad TLS CA certificate path (or NOMAD_CACERT)"),
		CAPath:            fs.String("nomad-ca-path", defaults.Nomad.CAPath, "Nomad TLS CA directory path (or NOMAD_CAPATH)"),
		ClientCert:        fs.String("nomad-client-cert", defaults.Nomad.ClientCert, "Nomad TLS client certificate path"),
		ClientKey:         fs.String("nomad-client-key", defaults.Nomad.ClientKey, "Nomad TLS client key path"),
		TLSServerName:     fs.String("nomad-tls-server-name", defaults.Nomad.TLSServerName, "Nomad TLS server name override"),
		SkipVerify:        fs.Bool("nomad-skip-verify", defaults.Nomad.SkipVerify, "skip Nomad TLS verification"),
		Task:              fs.String("nomad-task", defaults.Nomad.Task, "Nomad task name for later delegated runs"),
		Driver:            fs.String("nomad-driver", defaults.Nomad.Driver, "Nomad task driver for later delegated runs"),
		Image:             fs.String("nomad-image", defaults.Nomad.Image, "Nomad task image for later delegated runs"),
		Workdir:           fs.String("nomad-workdir", defaults.Nomad.Workdir, "absolute workdir inside later Nomad tasks"),
		JobSpecTemplate:   fs.String("nomad-jobspec-template", defaults.Nomad.JobSpecTemplate, "optional Nomad job spec template path"),
		NodePool:          fs.String("nomad-node-pool", defaults.Nomad.NodePool, "optional Nomad node pool for later delegated runs"),
		Datacenters:       fs.String("nomad-datacenters", strings.Join(defaults.Nomad.Datacenters, ","), "comma-separated Nomad datacenters for later delegated runs"),
		CPU:               fs.Int("nomad-cpu", defaults.Nomad.CPU, "CPU MHz for later Nomad tasks"),
		MemoryMB:          fs.Int("nomad-memory-mb", defaults.Nomad.MemoryMB, "memory MB for later Nomad tasks"),
		DiskMB:            fs.Int("nomad-disk-mb", defaults.Nomad.DiskMB, "ephemeral disk MB for later Nomad tasks"),
		AllocReadyTimeout: fs.String("nomad-alloc-ready-timeout", defaults.Nomad.AllocReadyTimeout.String(), "allocation readiness timeout"),
		EvalTimeout:       fs.String("nomad-eval-timeout", defaults.Nomad.EvalTimeout.String(), "evaluation readiness timeout"),
		ExecTimeoutSecs:   fs.Int("nomad-exec-timeout-secs", defaults.Nomad.ExecTimeoutSecs, "later task command timeout in seconds"),
	}
}

func ApplyNomadProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=nomad; use --nomad-cpu/--nomad-memory-mb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=nomad; use --nomad-image")
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "nomad-address") {
		cfg.Nomad.Address = *v.Address
	}
	if flagWasSet(fs, "nomad-region") {
		cfg.Nomad.Region = *v.Region
	}
	if flagWasSet(fs, "nomad-namespace") {
		cfg.Nomad.Namespace = *v.Namespace
	}
	if flagWasSet(fs, "nomad-token-env") {
		cfg.Nomad.TokenEnv = *v.TokenEnv
	}
	if flagWasSet(fs, "nomad-ca-cert") {
		cfg.Nomad.CACert = *v.CACert
	}
	if flagWasSet(fs, "nomad-ca-path") {
		cfg.Nomad.CAPath = *v.CAPath
	}
	if flagWasSet(fs, "nomad-client-cert") {
		cfg.Nomad.ClientCert = *v.ClientCert
	}
	if flagWasSet(fs, "nomad-client-key") {
		cfg.Nomad.ClientKey = *v.ClientKey
	}
	if flagWasSet(fs, "nomad-tls-server-name") {
		cfg.Nomad.TLSServerName = *v.TLSServerName
	}
	if flagWasSet(fs, "nomad-skip-verify") {
		cfg.Nomad.SkipVerify = *v.SkipVerify
	}
	if flagWasSet(fs, "nomad-task") {
		cfg.Nomad.Task = *v.Task
	}
	if flagWasSet(fs, "nomad-driver") {
		cfg.Nomad.Driver = *v.Driver
	}
	if flagWasSet(fs, "nomad-image") {
		cfg.Nomad.Image = *v.Image
	}
	if flagWasSet(fs, "nomad-workdir") {
		cfg.Nomad.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "nomad-jobspec-template") {
		cfg.Nomad.JobSpecTemplate = *v.JobSpecTemplate
	}
	if flagWasSet(fs, "nomad-node-pool") {
		cfg.Nomad.NodePool = *v.NodePool
	}
	if flagWasSet(fs, "nomad-datacenters") {
		cfg.Nomad.Datacenters = splitList(*v.Datacenters)
	}
	if flagWasSet(fs, "nomad-cpu") {
		cfg.Nomad.CPU = *v.CPU
	}
	if flagWasSet(fs, "nomad-memory-mb") {
		cfg.Nomad.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "nomad-disk-mb") {
		cfg.Nomad.DiskMB = *v.DiskMB
	}
	if flagWasSet(fs, "nomad-alloc-ready-timeout") {
		parsed, err := parsePositiveDuration(*v.AllocReadyTimeout, "nomad alloc ready timeout")
		if err != nil {
			return err
		}
		cfg.Nomad.AllocReadyTimeout = parsed
	}
	if flagWasSet(fs, "nomad-eval-timeout") {
		parsed, err := parsePositiveDuration(*v.EvalTimeout, "nomad eval timeout")
		if err != nil {
			return err
		}
		cfg.Nomad.EvalTimeout = parsed
	}
	if flagWasSet(fs, "nomad-exec-timeout-secs") {
		cfg.Nomad.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	return validateConfig(*cfg)
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parsePositiveDuration(value, name string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, exit(2, "%s must be a positive duration", name)
	}
	return parsed, nil
}
