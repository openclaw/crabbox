package replicate

import "flag"

type replicateFlagValues struct {
	APIURL           *string
	Deployment       *string
	Version          *string
	Workdir          *string
	WaitSecs         *int
	PollIntervalSecs *int
	ExecTimeoutSecs  *int
	CancelAfterSecs  *int
	MaxArchiveBytes  *int64
}

func RegisterReplicateProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return replicateFlagValues{
		APIURL:           fs.String("replicate-api-url", defaults.Replicate.APIURL, "Replicate API base URL"),
		Deployment:       fs.String("replicate-deployment", defaults.Replicate.Deployment, "Replicate deployment name or owner/name identifier"),
		Version:          fs.String("replicate-version", defaults.Replicate.Version, "Replicate model version identifier"),
		Workdir:          fs.String("replicate-workdir", defaults.Replicate.Workdir, "Absolute working directory inside the Replicate runner"),
		WaitSecs:         fs.Int("replicate-wait-secs", defaults.Replicate.WaitSecs, "Replicate prediction wait window in seconds (0 = provider default)"),
		PollIntervalSecs: fs.Int("replicate-poll-interval-secs", defaults.Replicate.PollIntervalSecs, "Replicate prediction polling interval in seconds"),
		ExecTimeoutSecs:  fs.Int("replicate-exec-timeout-secs", defaults.Replicate.ExecTimeoutSecs, "Replicate command timeout in seconds"),
		CancelAfterSecs:  fs.Int("replicate-cancel-after-secs", defaults.Replicate.CancelAfterSecs, "Cancel Replicate prediction after this many seconds (0 = disabled)"),
		MaxArchiveBytes:  fs.Int64("replicate-max-archive-bytes", defaults.Replicate.MaxArchiveBytes, "Maximum archive size for Replicate data URL sync"),
	}
}

func ApplyReplicateProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=replicate")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=replicate")
		}
	}
	v, ok := values.(replicateFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "replicate-api-url") {
		cfg.Replicate.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "replicate-deployment") {
		cfg.Replicate.Deployment = *v.Deployment
	}
	if flagWasSet(fs, "replicate-version") {
		cfg.Replicate.Version = *v.Version
	}
	if flagWasSet(fs, "replicate-workdir") {
		cfg.Replicate.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "replicate-wait-secs") {
		cfg.Replicate.WaitSecs = *v.WaitSecs
	}
	if flagWasSet(fs, "replicate-poll-interval-secs") {
		cfg.Replicate.PollIntervalSecs = *v.PollIntervalSecs
	}
	if flagWasSet(fs, "replicate-exec-timeout-secs") {
		cfg.Replicate.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "replicate-cancel-after-secs") {
		cfg.Replicate.CancelAfterSecs = *v.CancelAfterSecs
	}
	if flagWasSet(fs, "replicate-max-archive-bytes") {
		cfg.Replicate.MaxArchiveBytes = *v.MaxArchiveBytes
	}
	return nil
}
