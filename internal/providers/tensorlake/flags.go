package tensorlake

import "flag"

type tensorlakeFlagValues struct {
	APIURL         *string
	CLIPath        *string
	Image          *string
	Snapshot       *string
	OrganizationID *string
	ProjectID      *string
	Namespace      *string
	Workdir        *string
	CPUs           *float64
	MemoryMB       *int
	DiskMB         *int
	TimeoutSecs    *int
	NoInternet     *bool
}

func RegisterTensorlakeProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return tensorlakeFlagValues{
		APIURL:         fs.String("tensorlake-api-url", defaults.Tensorlake.APIURL, "Tensorlake API base URL"),
		CLIPath:        fs.String("tensorlake-cli", defaults.Tensorlake.CLIPath, "Path to the tensorlake CLI binary"),
		Image:          fs.String("tensorlake-image", defaults.Tensorlake.Image, "Tensorlake sandbox image name"),
		Snapshot:       fs.String("tensorlake-snapshot", defaults.Tensorlake.Snapshot, "Tensorlake snapshot ID to restore from"),
		OrganizationID: fs.String("tensorlake-organization-id", defaults.Tensorlake.OrganizationID, "Tensorlake organization ID"),
		ProjectID:      fs.String("tensorlake-project-id", defaults.Tensorlake.ProjectID, "Tensorlake project ID"),
		Namespace:      fs.String("tensorlake-namespace", defaults.Tensorlake.Namespace, "Tensorlake namespace"),
		Workdir:        fs.String("tensorlake-workdir", defaults.Tensorlake.Workdir, "Absolute working directory inside the sandbox (also used as sync target)"),
		CPUs:           fs.Float64("tensorlake-cpus", defaults.Tensorlake.CPUs, "Tensorlake sandbox CPU count"),
		MemoryMB:       fs.Int("tensorlake-memory-mb", defaults.Tensorlake.MemoryMB, "Tensorlake sandbox memory in MB"),
		DiskMB:         fs.Int("tensorlake-disk-mb", defaults.Tensorlake.DiskMB, "Tensorlake sandbox root disk in MB"),
		TimeoutSecs:    fs.Int("tensorlake-timeout-secs", defaults.Tensorlake.TimeoutSecs, "Tensorlake sandbox lifetime timeout in seconds"),
		NoInternet:     fs.Bool("tensorlake-no-internet", defaults.Tensorlake.NoInternet, "Block outbound internet from the sandbox"),
	}
}

func ApplyTensorlakeProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(tensorlakeFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "tensorlake-api-url") {
		cfg.Tensorlake.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "tensorlake-cli") {
		cfg.Tensorlake.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "tensorlake-image") {
		cfg.Tensorlake.Image = *v.Image
	}
	if flagWasSet(fs, "tensorlake-snapshot") {
		cfg.Tensorlake.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "tensorlake-organization-id") {
		cfg.Tensorlake.OrganizationID = *v.OrganizationID
	}
	if flagWasSet(fs, "tensorlake-project-id") {
		cfg.Tensorlake.ProjectID = *v.ProjectID
	}
	if flagWasSet(fs, "tensorlake-namespace") {
		cfg.Tensorlake.Namespace = *v.Namespace
	}
	if flagWasSet(fs, "tensorlake-workdir") {
		cfg.Tensorlake.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "tensorlake-cpus") {
		cfg.Tensorlake.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "tensorlake-memory-mb") {
		cfg.Tensorlake.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "tensorlake-disk-mb") {
		cfg.Tensorlake.DiskMB = *v.DiskMB
	}
	if flagWasSet(fs, "tensorlake-timeout-secs") {
		cfg.Tensorlake.TimeoutSecs = *v.TimeoutSecs
	}
	if flagWasSet(fs, "tensorlake-no-internet") {
		cfg.Tensorlake.NoInternet = *v.NoInternet
	}
	return nil
}
